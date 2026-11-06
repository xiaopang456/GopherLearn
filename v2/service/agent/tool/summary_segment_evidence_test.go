package agenttool

import (
	"context"
	"testing"

	"GopherAI/model"
	serviceagent "GopherAI/service/agent"
	"github.com/cloudwego/eino/components/tool"
)

// A summary segment is written once and never rewritten, so a citation into
// one has no version to check. What it has to check is that the row under
// that sequence number is still the segment the grant was drawn from.
//
// The whole range is needed, not just the end: a deleted segment and a later
// one that took its number can end at the same message while starting
// somewhere else, and a check that only looked at the end would accept the
// replacement - which reads back a stretch of turns the grant never referred
// to.

// fakeSegmentEvidence is the store the verifier reads.
type fakeSegmentEvidence struct {
	facts SummarySegmentEvidenceFacts
}

func (f fakeSegmentEvidence) LoadSummarySegmentEvidence(_ context.Context, _ int64, _ string, _ int) (SummarySegmentEvidenceFacts, error) {
	return f.facts, nil
}

// segmentGrant is a citation drawn from a segment's range.
func segmentGrant(id uint, sessionID string, seq int, start, end uint) serviceagent.HistoryMessageGrant {
	return serviceagent.HistoryMessageGrant{
		MessageID: id, SourceKind: serviceagent.HistorySourceSummarySegment, SourceSessionID: sessionID,
		SegmentSeq: seq, SegmentStartMessageID: start, SegmentEndMessageID: end,
	}
}

// segmentTools builds the read-back tool with a segment verifier.
func segmentTools(t *testing.T, reader HistoryMessageReader, run serviceagent.RunContext,
	facts SummarySegmentEvidenceFacts) []tool.BaseTool {
	t.Helper()
	registry, err := NewHistoryMessageRegistry(reader)
	if err != nil {
		t.Fatal(err)
	}
	registry.UseSummarySegmentEvidenceReader(fakeSegmentEvidence{facts: facts})
	tools, err := registry.Tools(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	return tools
}

// The segment the grant was drawn from is still there: the read goes through.
func TestHistoryReadAcceptsASegmentThatIsStillThere(t *testing.T) {
	reader := &fakeHistoryReader{rows: map[uint]model.Message{
		11: userMessage(11, historyRunSession, "我一开始说的是三十分钟"),
	}}
	run := historyRun(segmentGrant(11, historyRunSession, 1, 10, 30))
	tools := segmentTools(t, reader, run, SummarySegmentEvidenceFacts{
		Seq: 1, UserID: 7, SessionID: historyRunSession, StartMessageID: 10, EndMessageID: 30,
	})
	out := invokeRead(t, tools, 11)
	if len(out.Messages) != 1 || out.Messages[0].MessageID != 11 {
		t.Fatalf("messages=%+v refused=%v", out.Messages, out.Refused)
	}
}

// The same sequence number and the same end, but a different start: this is a
// different segment, and the read is refused.
func TestHistoryReadRefusesASegmentWhoseStartMoved(t *testing.T) {
	reader := &fakeHistoryReader{rows: map[uint]model.Message{
		11: userMessage(11, historyRunSession, "我一开始说的是三十分钟"),
	}}
	run := historyRun(segmentGrant(11, historyRunSession, 1, 10, 30))
	tools := segmentTools(t, reader, run, SummarySegmentEvidenceFacts{
		Seq: 1, UserID: 7, SessionID: historyRunSession, StartMessageID: 20, EndMessageID: 30,
	})
	out := invokeRead(t, tools, 11)
	if len(out.Messages) != 0 {
		t.Fatalf("the read went through: %+v", out.Messages)
	}
	if len(out.Refused) != 1 || out.Refused[0] != 11 {
		t.Fatalf("refused=%v want the replaced segment's id reported", out.Refused)
	}
	if len(out.RefusalReasons) != 1 || out.RefusalReasons[0].Reason != refusalSegmentReordered {
		t.Fatalf("reasons=%+v want the reordered reason", out.RefusalReasons)
	}
}

// A verifier that was never wired refuses the citation rather than trusting
// the render.
func TestHistoryReadRefusesASegmentWhenNoVerifierIsWired(t *testing.T) {
	reader := &fakeHistoryReader{rows: map[uint]model.Message{
		11: userMessage(11, historyRunSession, "我一开始说的是三十分钟"),
	}}
	run := historyRun(segmentGrant(11, historyRunSession, 1, 10, 30))
	registry, err := NewHistoryMessageRegistry(reader)
	if err != nil {
		t.Fatal(err)
	}
	tools, err := registry.Tools(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	out := invokeRead(t, tools, 11)
	if len(out.Messages) != 0 {
		t.Fatalf("the read went through without a verifier: %+v", out.Messages)
	}
	if len(out.RefusalReasons) != 1 || out.RefusalReasons[0].Reason != refusalSegmentVerifierAbsent {
		t.Fatalf("reasons=%+v want the absent verifier reason", out.RefusalReasons)
	}
}
