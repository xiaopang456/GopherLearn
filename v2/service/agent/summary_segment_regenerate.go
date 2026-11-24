package agent

// A summary that does not fit is written again.
//
// The segment is read under a budget, and a summary that costs more than
// that budget is not read in full: the renderer leaves out the points that
// do not fit, whole, and names the messages they came from, so the facts
// stay one read away. Making it fit by cutting the words is not an option -
// a cut point says something else, and the thing it says is often the
// opposite of what was said.
//
// So the question goes back to the model that wrote it. The same turns are
// sent again with a request to write it shorter, and what has to survive
// the shortening is named: the facts that still matter, the negations and
// the conditions, the order of the versions, and the difference between the
// learner's words and the assistant's.
//
// The request is made at most twice. A summary that still does not fit is
// stored as it came back. The words are not cut, and the batch is not
// thrown away: a stored summary the window trims is a stretch of the
// session the reader can still reach, while a failed batch is a hole in the
// session's memory. The B2 eval is why the rule changed - the model's
// rewrites did not get under the budget, and every failed batch lost its
// segment and its board together.

import (
	"context"
	"fmt"

	"GopherAI/common/rag"
	"GopherAI/model"
)

// segmentCompressLimit is how many times a summary that does not fit is
// asked again.
//
// Two is enough to tell a model that it wrote too much, and not enough to
// spend the batch's whole budget on retries. A summary that is still too
// long after that is a summary the model cannot make short, and asking a
// third time would not change that.
const segmentCompressLimit = 2

// segmentCompressRequest is what the compression call is asked for.
//
// It names what a shorter summary has to keep, because those are the parts
// that go first when a summary is made shorter: the facts that still hold,
// what the learner ruled out, what is still open, the order in which the
// versions came, and whose words each fact is.
const segmentCompressRequest = `

The summary of these turns does not fit the window it is read under. Write it again, shorter, from the same turns.

Keep: the facts that still matter, the negations and the conditions (what the learner ruled out, what is still open), the order in which the versions came (the old value and the new one), and the difference between what the learner said and what you produced.
Drop: the explanation, the repetition, and the wording that carries no fact.

Output the same JSON object as before, and nothing else.`

// segmentFitsBudget reports whether a segment is what the window can carry.
//
// The measure is the rendered block, because that is what the window pays
// for: the stored body is JSON and it runs to about a fifth more than the
// block the model reads.
func segmentFitsBudget(content model.SessionSummarySegmentContent,
	firstMessageID, lastMessageID uint, budget int) bool {
	if budget <= 0 {
		return true
	}
	return SegmentContentTokens(content, firstMessageID, lastMessageID) <= budget
}

// segmentBudgetReason is the one reason a summary can be refused for that a
// rewrite can fix. The others are about the structure of the answer, and a
// second answer with the same structure is no better than the first.
const segmentBudgetReason = "the summary does not fit the budget"

// segmentNotAcceptable says why a segment is not what the window can carry.
//
// The reasons are the ones the program can check on its own: the size of the
// rendered block, a point without words, a point that names no message, and
// a point whose source the program does not know. The meaning of a point is
// not one of them - the program cannot read what a sentence says, and the
// model is the one that reports it.
func segmentNotAcceptable(content model.SessionSummarySegmentContent,
	firstMessageID, lastMessageID uint, budget int) string {
	for _, point := range content.Points {
		if len([]rune(point.Text)) == 0 {
			return "a point has no text"
		}
		if len(point.EvidenceMessageIDs) == 0 {
			return "a point names no message"
		}
		switch point.PointSource() {
		case model.SessionSummarySegmentSourceUser,
			model.SessionSummarySegmentSourceAssistant,
			model.SessionSummarySegmentSourceAssistantAccepted:
		default:
			return "a point has a source the program does not know"
		}
	}
	if !segmentFitsBudget(content, firstMessageID, lastMessageID, budget) {
		return segmentBudgetReason
	}
	return ""
}

// fitSegmentToBudget makes a segment the window can carry.
//
// A segment that already fits is returned as it is. A segment that does not
// fit is asked for again - at most segmentCompressLimit times - with the
// same turns and a request to write it shorter. The most recent answer that
// passed the structure and evidence checks is kept while the rewrites are
// asked for: a rewrite that comes back unreadable must not lose a summary
// that was already usable. A summary that still does not fit is stored as
// it came back - the window trims a segment it cannot carry when it renders
// it, and a summary that is stored is a stretch of the session the reader
// can still reach. A batch that never produced a usable answer still fails.
//
// The board is not touched by the compression. The board of the batch is
// already what the calls wrote, and applying a second board on top of it
// would write the same changes twice.
func (g *ModelSegmentGenerator) fitSegmentToBudget(ctx context.Context, ledger *segmentCallLedger,
	input SegmentGenerationInput,
	content model.SessionSummarySegmentContent) (model.SessionSummarySegmentContent, int, error) {
	// valid is the most recent answer that can be stored. The first answer
	// is kept before any rewrite is asked for: it passed the build's
	// structure and evidence checks, and a rewrite that fails must not take
	// it down with it.
	valid := content
	hasValid := true
	reason := segmentNotAcceptable(valid, input.FirstMessageID, input.LastMessageID, g.segmentBudget())
	if reason == "" {
		return valid, 0, nil
	}
	// The first answer came back and it did not fit. The call was spent on
	// a summary that was refused, and the record says so: what became of
	// the answer is a second thing from the fact that the call was made.
	if reason == segmentBudgetReason {
		ledger.refuse(segmentPurposeSummary, segmentReasonOverBudget)
	} else {
		// The first answer cannot be stored. It is not the fallback; a
		// rewrite may still produce one.
		ledger.unusable(segmentPurposeSummary, segmentReasonInvalidOutput)
		hasValid = false
	}
	compressed := 0
	for attempt := 0; attempt < segmentCompressLimit; attempt++ {
		shorter, err := g.compressSegment(ctx, ledger, input)
		compressed++
		if err != nil {
			// The rewrite did not produce a summary. The most recent usable
			// answer is still a summary of the batch; storing it keeps the
			// turns readable. Without one, the rewrite's failure is the
			// batch's failure.
			if hasValid {
				return valid, compressed, nil
			}
			return valid, compressed, err
		}
		reason = segmentNotAcceptable(shorter, input.FirstMessageID, input.LastMessageID, g.segmentBudget())
		if reason == "" {
			return shorter, compressed, nil
		}
		if reason != segmentBudgetReason {
			// The answer is not a summary the program can store, and asking
			// again would not make it one: the checks are about the shape of
			// the answer, not its length.
			ledger.unusable(segmentPurposeRewrite, segmentReasonInvalidOutput)
			if hasValid {
				return valid, compressed, nil
			}
			return shorter, compressed, segmentFailure{reason: segmentReasonInvalidOutput,
				err: fmt.Errorf("summary segment content: %s", reason)}
		}
		// The answer is usable but does not fit. It is the most recent
		// summary that can be stored, so it becomes the fallback. The call
		// was spent on a summary that was refused, and the record says so:
		// the outcome of the call is replaced, not added to, so one call is
		// one record whatever became of its answer.
		valid = shorter
		hasValid = true
		ledger.refuse(segmentPurposeRewrite, segmentReasonOverBudget)
	}
	if hasValid {
		// The model could not make it fit. The summary is still a summary
		// of the batch, and the renderer trims a segment it cannot carry:
		// the points that do not fit are left out whole and the block names
		// the messages they came from, so the facts stay one read away.
		// Failing the batch would lose the segment and the board with it,
		// and the B2 eval showed that this is what happened: the model's
		// rewrites did not get under the budget, and every failed batch
		// became a hole in the session.
		//
		// So the most recent usable answer is stored. The words are not
		// cut, and the calls that were refused stay counted as refused.
		return valid, compressed, nil
	}
	return valid, compressed, segmentFailure{reason: segmentReasonOverBudget,
		err: fmt.Errorf("summary segment does not fit the budget it is read under")}
}

// compressSegment asks the model for a shorter summary of the same turns.
//
// The input is the one the batch was summarized from. A summary that does
// not fit is not a summary of a different stretch - it is the same stretch
// written too long - so the turns are the same and only the length changes.
//
// The board and the previous segment are sent when they fit. A batch that
// was split into several calls does not fit with them, and the turns alone
// are what the summary is written from anyway.
//
// The request is built whole - the system prompt, the payload and the
// instruction that goes with it - and it is the whole request that is
// measured against the input limit. Measuring only the payload would let a
// request through that the provider cannot accept.
func (g *ModelSegmentGenerator) compressSegment(ctx context.Context, ledger *segmentCallLedger,
	input SegmentGenerationInput) (model.SessionSummarySegmentContent, error) {
	// The request is built whole and measured whole.
	//
	// The instruction that goes with this call is part of the request, so
	// the payload may cost what is left of the budget after it. A request
	// that is measured against a budget it was not sized for is a request
	// that does not fit, and the call that would not fit is not made.
	budget := g.payloadBudget() - rag.EstimateTokens(segmentCompressRequest)
	if budget < 0 {
		budget = 0
	}
	payload := renderSegmentInput(input, budget, g.assistantContent)
	if rag.EstimateTokens(payload) > budget {
		payload = renderSegmentInputWithExcerpts("", "", input.Messages, budget,
			g.assistantContent, nil)
	}
	request := segmentRequest{
		system: g.segmentPrompt(),
		user:   payload + segmentCompressRequest,
	}
	response, err := g.segmentCall(ctx, ledger, segmentPurposeRewrite, request)
	if err != nil {
		return model.SessionSummarySegmentContent{}, err
	}
	document, err := decodeSegmentGenerationDocument(response.Content)
	if err != nil {
		ledger.unusable(segmentPurposeRewrite, segmentFailureReason(err))
		return model.SessionSummarySegmentContent{}, err
	}
	batch := whiteboardBatchFor(input, g.assistantContent)
	content, _, err := buildSegmentContent(document, input, batch, g.assistantContent)
	if err != nil {
		return model.SessionSummarySegmentContent{}, err
	}
	return content, nil
}
