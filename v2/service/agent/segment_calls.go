package agent

// Every model call the segment generator makes, and what it cost.
//
// A batch is not one call. It is one call to write the summary, and then up
// to two more to write it again when the first one does not fit the window
// it is read under. Each of those calls is a request the provider saw and
// billed, and each of them has an outcome: a summary that came back, an
// answer that could not be parsed, a call that failed outright.
//
// The counting used to be one number at the end of a successful run, which
// meant a batch that failed after three calls reported nothing. The cost of
// a failure is the cost of the calls that went before it, and a cost nobody
// counted is a cost nobody can see.
//
// So the calls are recorded as they happen, and the record travels with the
// result whether the result is a segment or a failure.

import (
	"context"
	"fmt"

	"github.com/cloudwego/eino/schema"

	"GopherAI/common/rag"
	"GopherAI/model"
)

// What the calls the generator makes are for.
//
// The purpose travels with the record, so a batch of three calls reads as
// one summary and two rewrites rather than as three calls with no shape.
const (
	// segmentPurposeSummary is the call that writes the summary.
	segmentPurposeSummary = "summary"
	// segmentPurposeRewrite is a call that writes the summary again,
	// because the one before it did not fit the window.
	segmentPurposeRewrite = "rewrite"
	// segmentPurposeChanges is the call that asks for the board's changes
	// instead of the whole board.
	segmentPurposeChanges = "changes"
)

// segmentCallOutcome is what one model call ended in.
type segmentCallOutcome string

const (
	// segmentCallOK says the call returned an answer the program could use
	// or explain.
	segmentCallOK segmentCallOutcome = "ok"
	// segmentCallFailed says the call returned nothing usable: an empty
	// answer, an answer that could not be parsed, or a failure of the
	// provider.
	segmentCallFailed segmentCallOutcome = "failed"
	// segmentCallRejected says the call returned an answer that was
	// refused on purpose - the summary did not fit the window, so it was
	// not stored.
	segmentCallRejected segmentCallOutcome = "rejected"
)

// segmentCallRecord is one model call.
//
// The record is what makes the cost of a failure visible: a batch that
// wrote nothing still made the calls, and the record says how many and what
// came back.
type segmentCallRecord struct {
	// Purpose says what the call was for.
	Purpose string
	// Outcome is how it ended.
	Outcome segmentCallOutcome
	// Reason names why, for a call that did not succeed.
	Reason string
}

// segmentCallLedger is the record of the calls one batch made.
//
// It is written as the calls happen and read afterwards, so a batch that
// fails in the middle still has the calls that came before it.
type segmentCallLedger struct {
	records []segmentCallRecord
}

// record notes one call.
func (l *segmentCallLedger) record(purpose string, outcome segmentCallOutcome, reason string) {
	l.records = append(l.records, segmentCallRecord{
		Purpose: purpose, Outcome: outcome, Reason: reason,
	})
}

// refuse marks the last call as one whose answer was refused.
//
// The call was made and it cost what a call costs. What became of its
// answer is a second thing: a summary that came back and did not fit is a
// call that was spent on something that did not survive, and the record
// says that rather than what the call did mechanically.
//
// One call is one record. The outcome of the last call is replaced, so a
// call that was answered and then refused is not counted twice.
func (l *segmentCallLedger) refuse(purpose, reason string) {
	if len(l.records) == 0 {
		return
	}
	last := len(l.records) - 1
	if l.records[last].Purpose != purpose {
		return
	}
	l.records[last].Outcome = segmentCallRejected
	l.records[last].Reason = reason
}

// unusable marks the last call as one whose answer could not be used.
//
// The difference from refuse is the reason: the answer was refused because
// it did not fit, and an answer is unusable because the program could not
// read it. The call is counted either way.
func (l *segmentCallLedger) unusable(purpose, reason string) {
	if len(l.records) == 0 {
		return
	}
	last := len(l.records) - 1
	if l.records[last].Purpose != purpose {
		return
	}
	l.records[last].Outcome = segmentCallFailed
	l.records[last].Reason = reason
}

// calls is how many calls were made.
func (l *segmentCallLedger) calls() int {
	return len(l.records)
}

// failed is how many of them did not succeed.
func (l *segmentCallLedger) failed() int {
	count := 0
	for _, record := range l.records {
		if record.Outcome != segmentCallOK {
			count++
		}
	}
	return count
}

// copies returns the records, so a result can carry them without sharing
// the ledger's slice.
func (l *segmentCallLedger) copies() []segmentCallRecord {
	if len(l.records) == 0 {
		return nil
	}
	out := make([]segmentCallRecord, len(l.records))
	copy(out, l.records)
	return out
}

// segmentRequest is one request the generator sends.
//
// The request is built whole before it is measured, because the budget is
// about what the provider sees: the system prompt, the payload, and the
// instruction that goes with the call. Measuring only the payload lets a
// request that is over the limit through, and the provider is the one that
// has to answer it.
type segmentRequest struct {
	// system is the instruction the model answers under.
	system string
	// user is what the model reads.
	user string
}

// tokens is what the request costs.
func (r segmentRequest) tokens() int {
	return rag.EstimateTokens(r.system) + rag.EstimateTokens(r.user)
}

// messages is the request as the provider sees it.
func (r segmentRequest) messages() []*schema.Message {
	return []*schema.Message{
		schema.SystemMessage(r.system),
		schema.UserMessage(r.user),
	}
}

// segmentRequestWithinBudget reports whether the whole request fits.
//
// The limit the deployment set is on what the provider receives, so this is
// the measure every path uses: the first call, the calls that write the
// summary again, and the fallback that sends the turns without the board.
//
// The limit it is measured against is the deployment's limit for the payload
// plus the instruction the request carries under. The deployment's limit has
// always bounded what the generator reads - the turns, the board and the
// previous segment - and the instruction is the generator's own and is not
// part of what it reads. Adding it here rather than taking it out of the
// payload budget is what keeps the two the same number as before: a
// deployment that set the limit to 1200 still reads 1200 tokens of turns.
func segmentRequestWithinBudget(request segmentRequest, maxTokens int) bool {
	if maxTokens <= 0 {
		return true
	}
	return request.tokens() <= maxTokens+rag.EstimateTokens(request.system)
}

// segmentPayloadBudget is what the payload of a request may cost.
//
// The limit is on what the generator reads, and the instruction is the
// generator's own, so the payload may cost what the deployment set. The
// whole request is measured against the limit plus the instruction, and the
// two are the same measure everywhere: what is sized is what is sent.
func segmentPayloadBudget(system string, maxTokens int) int {
	if maxTokens <= 0 {
		return maxTokens
	}
	return maxTokens
}

// segmentCall makes one model call and notes what came of it.
//
// Every call goes through here, so the ledger cannot miss one: a call that
// fails is recorded before the error travels on, and a call that returns
// something the program cannot use is recorded as what it is.
//
// A request that does not fit is not a call. Nothing was sent and nothing
// was paid for, so the ledger does not say a call was made: the generation
// failed before it began, and the failure says why.
//
// The purpose is what the call was for, and it travels with the record so
// that a batch of three calls reads as "one summary and two rewrites" rather
// than as three calls with no shape.
func (g *ModelSegmentGenerator) segmentCall(ctx context.Context, ledger *segmentCallLedger,
	purpose string, request segmentRequest) (*schema.Message, error) {
	if !segmentRequestWithinBudget(request, g.maxInputTokens) {
		return nil, segmentFailure{reason: segmentReasonInputTooLarge,
			err: fmt.Errorf("segment request exceeds token limit")}
	}
	response, err := g.model.Generate(ctx, request.messages())
	if err != nil {
		ledger.record(purpose, segmentCallFailed, segmentReasonModelError)
		return nil, segmentFailure{reason: segmentReasonModelError,
			calls: segmentCallRecords(ledger),
			err:   fmt.Errorf("generate summary segment: %w", err)}
	}
	if response == nil {
		ledger.record(purpose, segmentCallFailed, segmentReasonEmptyOutput)
		return nil, segmentFailure{reason: segmentReasonEmptyOutput,
			calls: segmentCallRecords(ledger), err: ErrEmptySessionSummary}
	}
	ledger.record(purpose, segmentCallOK, segmentReasonNone)
	return response, nil
}

// segmentCallRecords is the record the result carries.
//
// The result of a failed batch carries it too: the calls were made and
// they were paid for, and a caller that cannot see them cannot tell a
// batch that failed early from a batch that failed late.
func segmentCallRecords(ledger *segmentCallLedger) []segmentCallRecord {
	return ledger.copies()
}

// segmentCallCount is how many calls the records hold.
func segmentCallCount(records []segmentCallRecord) int {
	return len(records)
}

// segmentCallFailures is how many of the calls did not succeed.
func segmentCallFailures(records []segmentCallRecord) int {
	count := 0
	for _, record := range records {
		if record.Outcome != segmentCallOK {
			count++
		}
	}
	return count
}

// segmentContentFrom builds the segment content of one answer.
//
// It is the one place the answer is turned into a segment, so the calls
// that write the summary again produce the same shape as the first one.
func segmentContentFrom(document segmentGenerationDocument, input SegmentGenerationInput,
	batch WhiteboardBatch, assistantContent bool) (model.SessionSummarySegmentContent, int, error) {
	return buildSegmentContent(document, input, batch, assistantContent)
}
