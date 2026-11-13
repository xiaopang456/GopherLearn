package agent

// A summary that does not fit is written again.
//
// The segment is read under a budget, and a summary that costs more than
// that budget is not read at all: the block is dropped and the learner
// loses a stretch of their own session without being told. Making it fit by
// cutting the words is not an option - a cut point says something else, and
// the thing it says is often the opposite of what was said.
//
// So the question goes back to the model that wrote it. The same turns are
// sent again with a request to write it shorter, and what has to survive
// the shortening is named: the facts that still matter, the negations and
// the conditions, the order of the versions, and the difference between the
// learner's words and the assistant's.
//
// The request is made at most twice. A summary that does not fit after that
// is not stored at all: a batch that wrote nothing is a hole in the
// session's memory, and a hole that is counted is a hole somebody can see.

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
		return "the summary does not fit the budget"
	}
	return ""
}

// fitSegmentToBudget makes a segment the window can carry.
//
// A segment that already fits is returned as it is. A segment that does not
// fit is asked for again - at most segmentCompressLimit times - with the
// same turns and a request to write it shorter. A segment that still does
// not fit is a failure: the batch wrote nothing, and the reason is counted.
//
// The board is not touched by the compression. The board of the batch is
// already what the calls wrote, and applying a second board on top of it
// would write the same changes twice.
func (g *ModelSegmentGenerator) fitSegmentToBudget(ctx context.Context, ledger *segmentCallLedger,
	input SegmentGenerationInput,
	content model.SessionSummarySegmentContent) (model.SessionSummarySegmentContent, int, error) {
	if segmentNotAcceptable(content, input.FirstMessageID, input.LastMessageID, g.segmentBudget()) == "" {
		return content, 0, nil
	}
	// The first answer came back and it did not fit. The call was spent on
	// a summary that was refused, and the record says so: what became of
	// the answer is a second thing from the fact that the call was made.
	ledger.refuse(segmentPurposeSummary, segmentReasonOverBudget)
	compressed := 0
	for attempt := 0; attempt < segmentCompressLimit; attempt++ {
		shorter, err := g.compressSegment(ctx, ledger, input)
		if err != nil {
			return content, compressed, err
		}
		compressed++
		content = shorter
		if segmentNotAcceptable(shorter, input.FirstMessageID, input.LastMessageID, g.segmentBudget()) == "" {
			return content, compressed, nil
		}
		// The answer came back and it did not fit. The call was spent on
		// a summary that was refused, and the record says so: the outcome
		// of the call is replaced, not added to, so one call is one
		// record whatever became of its answer.
		ledger.refuse(segmentPurposeRewrite, segmentReasonOverBudget)
	}
	return content, compressed, segmentFailure{reason: segmentReasonOverBudget,
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
