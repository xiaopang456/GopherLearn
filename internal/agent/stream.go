package agent

import (
	"context"
	"fmt"

	"github.com/xiaopang456/GopherLearn/internal/model"
	"github.com/xiaopang456/GopherLearn/internal/responder"
)

// publisher decides what a client may see of one model call's public text. It has two modes,
// because there are two honest answers to "can this text be shown yet?".
//
// In blocks mode - a turn whose client is watching progress - one model call's text is one
// numbered block, and a fragment goes out the moment it arrives. The text does not have to be
// known to be an answer first: the block's ending frame says which of three things it turned
// out to be (a process note, a candidate answer, a failed round), and a candidate becomes the
// learner's answer only when message.completed names it. That is what lets a provider that
// narrates and then asks for a tool keep working: the narration stays on screen as the note it
// was, and nothing has to be retracted.
//
// In buffered mode - the turn with no progress channel, whose frames are the four message.*
// ones this contract has always had - the same narration cannot be told apart from an answer,
// so a call's text is held until the call has ended and published only if it asked for
// nothing. That is the price of a channel with no block frames on it; the alternative, putting
// out text the turn then disowns, is what this answer path has always refused.
//
// What both modes keep is the invariant the SSE contract rests on: the bytes a client was
// given as the answer are exactly the bytes message.completed carries and the repository
// stored. A nil publisher is a turn nobody is reading, and every method then does nothing.
type publisher struct {
	// report is present in blocks mode and is what a block frame is sent through. It is the
	// same sink as the turn's step events, so a text frame can never overtake the step frame
	// it belongs beside.
	report responder.StepSink
	// emit is present in buffered mode alone: it is the answer channel, and a blocks-mode
	// publisher holds nil so that a text frame cannot go out twice under two names.
	emit func(string) error
	// round and blockID name the model call currently being read. The id is derived from the
	// round rather than drawn at random so that one call has one block, and a client can
	// order what it folded away without being told what it said.
	round   int
	blockID string
	// buffer holds the current call's text until the call has finished.
	buffer []string
	// askedForTool is sticky for one call: after a tool request appears, the call's text is
	// a decision - in blocks mode that changes what the ending frame says, never what the
	// client already got to read.
	askedForTool bool
	// blockStarted records that this call announced a block, which is also the only case in
	// which it has an ending frame to send and an answer block to name.
	blockStarted bool
}

func newPublisher(emit func(string) error) *publisher {
	if emit == nil {
		return nil
	}
	return &publisher{emit: emit}
}

// newBlockPublisher returns the publisher of a turn that reports progress: text leaves as
// block frames, and the answer channel stays unused.
func newBlockPublisher(report responder.StepSink) *publisher {
	if report == nil {
		return nil
	}
	return &publisher{report: report}
}

// beginCall starts a fresh publication decision for one model call. Whether a call's text was
// a decision or an answer is a fact about that call, never about the turn: a round that asked
// for a tool must not be able to suppress the answer that comes after it.
func (p *publisher) beginCall(round int) {
	if p == nil {
		return
	}
	p.round, p.blockID = round, fmt.Sprintf("blk-%d", round)
	p.buffer, p.askedForTool, p.blockStarted = nil, false, false
}

// content hands over one fragment of the current call.
func (p *publisher) content(delta string) error {
	if p == nil || delta == "" {
		return nil
	}
	if p.report == nil {
		if p.askedForTool {
			return nil
		}
		p.buffer = append(p.buffer, delta)
		return nil
	}
	if !p.blockStarted {
		p.blockStarted = true
		if err := p.send(responder.Step{Phase: responder.TextStarted, BlockID: p.blockID, Round: p.round}); err != nil {
			p.blockStarted = false
			return err
		}
	}
	return p.send(responder.Step{Phase: responder.TextDelta, BlockID: p.blockID, Content: delta})
}

// toolCall records that the current call asked for a tool. Buffered mode drops the round's
// text, because it has not been shown to anybody. Blocks mode keeps showing it and closes it
// as a process note: a client that watched a sentence arrive is not served by a server that
// afterwards pretends the sentence was never said.
func (p *publisher) toolCall() {
	if p == nil {
		return
	}
	p.askedForTool = true
	if p.report == nil {
		p.buffer = nil
	}
}

// end closes a call that finished. A block is classified here and only here, because the
// ending of the stream is the first moment the loop knows whether this call asked for a tool.
func (p *publisher) end() error {
	if p == nil {
		return nil
	}
	if p.report != nil {
		if !p.blockStarted {
			return nil
		}
		kind := responder.TextKindCandidate
		if p.askedForTool {
			kind = responder.TextKindProcess
		}
		return p.send(responder.Step{Phase: responder.TextEnded, BlockID: p.blockID, Kind: kind})
	}
	if p.askedForTool {
		p.buffer = nil
		return nil
	}
	fragments := p.buffer
	p.buffer = nil
	for _, fragment := range fragments {
		if err := p.emit(fragment); err != nil {
			return err
		}
	}
	return nil
}

// fail closes a call whose stream broke mid-text. The fragments already on the screen stay
// there, and the ending frame is what tells a client they are not an answer. The delivery
// error of this frame is not the failure the loop is already returning, so a caller keeps the
// provider's own error as the one it reports.
func (p *publisher) fail() {
	if p == nil || p.report == nil || !p.blockStarted {
		return
	}
	_ = p.send(responder.Step{Phase: responder.TextEnded, BlockID: p.blockID, Kind: responder.TextKindFailed})
	p.blockStarted = false
}

// answerBlock names the block this call published as the turn's answer, and reports "" for
// every call that is not a blocks-mode answer round: a turn whose text arrived through
// message.delta has no block to point at, and saying so is better than inventing one.
func (p *publisher) answerBlock() string {
	if p == nil || p.report == nil || !p.blockStarted || p.askedForTool {
		return ""
	}
	return p.blockID
}

func (p *publisher) send(step responder.Step) error {
	if p == nil || p.report == nil {
		return nil
	}
	if err := p.report(step); err != nil {
		return fmt.Errorf("%w: %w", ErrStepDelivery, err)
	}
	return nil
}

// decide asks the model one thing, and publishes what that one call produced. The round is
// the loop's own count of model calls, so a block id names its call without a second counter
// that could disagree with the budget.
//
// A model that cannot stream a tool turn is not asked twice to make an answer look
// incremental: it is asked once, and its finished text is published as the one block it is.
func (h *Harness) decide(ctx context.Context, request model.Request, round int, out *publisher) (model.Completion, error) {
	streamer, canStream := h.chat.(model.ToolStreamChatModel)
	if out == nil || !canStream {
		completion, err := h.chat.CompleteWithTools(ctx, request)
		if out == nil {
			return completion, err
		}
		if err != nil {
			return model.Completion{}, err
		}
		// A turn being read as blocks gets the same three frames it would have got from a
		// streaming provider - opened, filled, closed - just filled in one piece. A client
		// cannot tell the two apart, and neither can the answer's bytes.
		out.beginCall(round)
		if err := out.content(completion.Content); err != nil {
			return model.Completion{}, err
		}
		if len(completion.ToolCalls) > 0 {
			out.toolCall()
		}
		return completion, out.end()
	}
	out.beginCall(round)
	completion, err := streamer.StreamWithTools(ctx, request, func(delta model.StreamDelta) error {
		if delta.ToolCall {
			out.toolCall()
			return nil
		}
		return out.content(delta.Content)
	})
	if err != nil {
		out.fail()
		return model.Completion{}, err
	}
	if err := out.end(); err != nil {
		return model.Completion{}, err
	}
	return completion, nil
}
