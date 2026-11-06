package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"GopherAI/model"
	agent "GopherAI/service/agent"
)

// stubLayeredProvider is a read side that answers with whatever it is given.
type stubLayeredProvider struct {
	snapshot agent.LayeredSummarySnapshot
	err      error
	calls    int
}

func (p *stubLayeredProvider) LayeredSummary(context.Context, agent.RunContext) (agent.LayeredSummarySnapshot, error) {
	p.calls++
	return p.snapshot, p.err
}

func testSegmentSeqs(seqs ...int) []model.SessionSummarySegment {
	out := make([]model.SessionSummarySegment, 0, len(seqs))
	for _, seq := range seqs {
		out = append(out, model.SessionSummarySegment{Seq: seq})
	}
	return out
}

// The preselector's answer has to be recorded separately from the block's.
//
// The two can differ - the renderer walks the selection from the newest
// backwards and keeps what fits - and when they do, only the pair says which
// half went wrong: a selection that was empty, or a selection the block could
// not hold. One column cannot carry both, which is why the report has two.
func TestThePreselectionIsRecordedSeparatelyFromTheBlock(t *testing.T) {
	inner := &stubLayeredProvider{snapshot: agent.LayeredSummarySnapshot{
		Retrieved: testSegmentSeqs(2, 5, 7),
	}}
	provider := &recordingLayeredProvider{inner: inner}

	if _, err := provider.LayeredSummary(context.Background(), agent.RunContext{Question: "读多少页"}); err != nil {
		t.Fatalf("read: %v", err)
	}
	if inner.calls != 1 {
		t.Fatalf("the inner provider was read %d times, want once", inner.calls)
	}
	if len(provider.retrieved) != 3 || provider.retrieved[0] != 2 || provider.retrieved[2] != 7 {
		t.Fatalf("the preselection was recorded as %v, want the segments the reader returned", provider.retrieved)
	}

	read := LayeredRead{RetrievedSegments: []int{7}}
	read.PreselectedSegments = append([]int{}, provider.retrieved...)
	if len(read.PreselectedSegments) == len(read.RetrievedSegments) {
		t.Fatal("the fixture cannot tell the two columns apart")
	}
	encoded, err := json.Marshal(read)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var columns map[string]any
	if err := json.Unmarshal(encoded, &columns); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := columns["preselected_segments"]; !ok {
		t.Fatalf("the report has no preselected_segments column: %s", encoded)
	}
	if _, ok := columns["retrieved_segments"]; !ok {
		t.Fatalf("the report has no retrieved_segments column: %s", encoded)
	}
}

// A read that failed records no preselection, and the report still says so.
func TestAFailedReadRecordsNoPreselection(t *testing.T) {
	provider := &recordingLayeredProvider{inner: &stubLayeredProvider{err: errors.New("reader down")}}
	if _, err := provider.LayeredSummary(context.Background(), agent.RunContext{}); err == nil {
		t.Fatal("the failure was swallowed")
	}
	if len(provider.retrieved) != 0 {
		t.Fatalf("a failed read recorded %v as a preselection", provider.retrieved)
	}
}
