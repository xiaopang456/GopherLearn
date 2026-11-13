package agent

// The claim contract and the rewrite path, together.
//
// The board change makes the learner show the words behind an entry, and the
// summary change writes the summary again when it does not fit. The two meet
// in one place: the answer the model gave.
//
// This test is the whole path with both in it. The model answers with a
// summary that does not fit and a board entry that carries the learner's
// words, the summary is asked for again, and the board the batch produced is
// the one the engine accepted from the first answer - with the learner's own
// value on it.
//
// It lives here because the two changes meet here: the board change has no
// path that writes a summary again, and the summary change has no claim to
// carry. The branch it lives on holds both, and the order they were merged
// in is the order they were reviewed in: the board change first, the summary
// change on top of it.

import (
	"context"
	"strings"
	"testing"

	"GopherAI/model"
)

// claimPathInput is the batch the test above is written against.
//
// The learner says the value changed, in their own words, and the words are
// in the batch: the claim quotes the message, and the program checks the
// quote against it.
func claimPathInput() SegmentGenerationInput {
	return SegmentGenerationInput{
		Messages: []model.Message{
			{ID: 30, IsUser: true, Content: "阅读计划以后每次读41页"},
			{ID: 31, IsUser: false, Content: "往后按新的页数走。"},
			{ID: 32, IsUser: true, Content: "给我三道动态规划的练习"},
		},
		FirstMessageID: 30,
		LastMessageID:  32,
	}
}

// The whole path with the claim contract in it.
//
// The learner says the value changed, the model writes the entry down as the
// assistant's and shows the learner's words behind it, and the summary does
// not fit so it is written again. The board the batch produced carries the
// learner's new value, and the segment carries the summary that fitted.
func TestTheClaimContractSurvivesTheRewritePath(t *testing.T) {
	long := `{"schema_version":1,"segment":{"topic":"很长的一个话题","points":[` +
		`{"text":"` + strings.Repeat("学习者把这一条说清楚了", 4) + `","evidence_message_ids":[30]},` +
		`{"text":"` + strings.Repeat("学习者把这一条说清楚了", 4) + `","evidence_message_ids":[30]},` +
		`{"text":"` + strings.Repeat("学习者把这一条说清楚了", 4) + `","evidence_message_ids":[30]},` +
		`{"text":"` + strings.Repeat("学习者把这一条说清楚了", 4) + `","evidence_message_ids":[30]},` +
		`{"text":"` + strings.Repeat("学习者把这一条说清楚了", 4) + `","evidence_message_ids":[30]},` +
		`{"text":"` + strings.Repeat("学习者把这一条说清楚了", 4) + `","evidence_message_ids":[30]},` +
		`{"text":"` + strings.Repeat("学习者把这一条说清楚了", 4) + `","evidence_message_ids":[30]},` +
		`{"text":"` + strings.Repeat("学习者把这一条说清楚了", 4) + `","evidence_message_ids":[30]},` +
		`{"text":"` + strings.Repeat("学习者把这一条说清楚了", 4) + `","evidence_message_ids":[30]},` +
		`{"text":"` + strings.Repeat("学习者把这一条说清楚了", 4) + `","evidence_message_ids":[30]}]},` +
		`"whiteboard":{"schema_version":1,"constraints":[` +
		`{"key":"pages","text":"阅读计划每次读 41 页","value":"41 页/次",` +
		`"source_message_ids":[30],"origin":"assistant_accepted",` +
		`"learner_claim":{"message_id":30,"quote":"阅读计划以后每次读41页","subject":"阅读计划",` +
		`"attribute":"页","value":"41页","stance":"decided","update":"yes"}}]}}`
	short := `{"schema_version":1,"segment":{"topic":"t","points":[` +
		`{"text":"压短之后的要点","evidence_message_ids":[30]}]}}`
	stub := &recordingSegmentModel{answers: []string{long, short}}
	generator := rewritingGenerator(t, stub)
	result, err := generator.GenerateSegment(context.Background(), claimPathInput())
	if err != nil {
		t.Fatalf("generate segment: %v", err)
	}
	// The path really was the rewrite path: the summary was asked for
	// again and the shorter one is the one that was kept.
	if stub.calls != 2 {
		t.Fatalf("the model was asked %d times, want 2", stub.calls)
	}
	if len(result.Segment.Points) != 1 {
		t.Fatalf("points = %d, want the shorter summary: %+v", len(result.Segment.Points), result.Segment.Points)
	}
	// The board the batch produced carries the learner's own value, because
	// the claim behind it held: the quote is the learner's own wording, the
	// value is in it, and the subject and the attribute are what the entry
	// is about.
	if len(result.Whiteboard.Constraints) != 1 {
		t.Fatalf("the board = %+v", result.Whiteboard.Constraints)
	}
	entry := result.Whiteboard.Constraints[0]
	if entry.Value != "41 页/次" {
		t.Fatalf("the board entry = %+v", entry)
	}
	if entry.EntryOrigin() != model.SessionWhiteboardOriginUser {
		t.Fatalf("the entry's origin = %q, want the learner's: the claim held and the label was wrong",
			entry.EntryOrigin())
	}
	if len(entry.SourceMessageIDs) != 1 || entry.SourceMessageIDs[0] != 30 {
		t.Fatalf("the entry's citations = %v, want the learner's message", entry.SourceMessageIDs)
	}
}

// The same path, with a claim that does not hold.
//
// The model quotes words the learner did not say. The board change refuses
// the entry, and the rewrite path does not bring it back: the summary is
// written again, the board is not touched by that, and what is left on the
// board is what the board held before the batch.
func TestAClaimThatDoesNotHoldDoesNotReachTheBoardThroughTheRewritePath(t *testing.T) {
	long := `{"schema_version":1,"segment":{"topic":"很长的一个话题","points":[` +
		`{"text":"` + strings.Repeat("学习者把这一条说清楚了", 4) + `","evidence_message_ids":[30]},` +
		`{"text":"` + strings.Repeat("学习者把这一条说清楚了", 4) + `","evidence_message_ids":[30]},` +
		`{"text":"` + strings.Repeat("学习者把这一条说清楚了", 4) + `","evidence_message_ids":[30]},` +
		`{"text":"` + strings.Repeat("学习者把这一条说清楚了", 4) + `","evidence_message_ids":[30]},` +
		`{"text":"` + strings.Repeat("学习者把这一条说清楚了", 4) + `","evidence_message_ids":[30]},` +
		`{"text":"` + strings.Repeat("学习者把这一条说清楚了", 4) + `","evidence_message_ids":[30]},` +
		`{"text":"` + strings.Repeat("学习者把这一条说清楚了", 4) + `","evidence_message_ids":[30]},` +
		`{"text":"` + strings.Repeat("学习者把这一条说清楚了", 4) + `","evidence_message_ids":[30]},` +
		`{"text":"` + strings.Repeat("学习者把这一条说清楚了", 4) + `","evidence_message_ids":[30]},` +
		`{"text":"` + strings.Repeat("学习者把这一条说清楚了", 4) + `","evidence_message_ids":[30]}]},` +
		`"whiteboard":{"schema_version":1,"constraints":[` +
		`{"key":"pages","text":"阅读计划每次读 41 页","value":"41 页/次",` +
		`"source_message_ids":[30],"origin":"assistant_accepted",` +
		`"learner_claim":{"message_id":30,"quote":"阅读计划以后每次读55页","subject":"阅读计划",` +
		`"attribute":"页","value":"41页","stance":"decided","update":"yes"}}]}}`
	short := `{"schema_version":1,"segment":{"topic":"t","points":[` +
		`{"text":"压短之后的要点","evidence_message_ids":[30]}]}}`
	stub := &recordingSegmentModel{answers: []string{long, short}}
	generator := rewritingGenerator(t, stub)
	result, err := generator.GenerateSegment(context.Background(), claimPathInput())
	if err != nil {
		t.Fatalf("generate segment: %v", err)
	}
	if stub.calls != 2 {
		t.Fatalf("the model was asked %d times, want 2", stub.calls)
	}
	if len(result.Whiteboard.Constraints) != 0 {
		t.Fatalf("the board = %+v, want nothing: the quote is not the learner's own wording",
			result.Whiteboard.Constraints)
	}
	if result.Diagnostics.RejectedClaims != 1 {
		t.Fatalf("rejected claims = %d, want 1", result.Diagnostics.RejectedClaims)
	}
	if result.Diagnostics.VerifiedClaims != 0 {
		t.Fatalf("verified claims = %d, want 0", result.Diagnostics.VerifiedClaims)
	}
}
