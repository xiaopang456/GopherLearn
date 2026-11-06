package conversation

import (
	"context"
	"testing"

	"GopherAI/common/observability"
	"GopherAI/model"
)

// The counters are global, so a run reads the difference between the state
// before it and the state after it. These tests pin that difference down:
// a counter that was already there before the run is not the run's number.
func TestTheCounterDifferenceOnlyKeepsWhatTheRunMoved(t *testing.T) {
	before := map[string]uint64{"a": 3, "b": 1}
	after := map[string]uint64{"a": 5, "b": 1, "c": 2}
	delta := counterDelta(before, after)
	if delta["a"] != 2 {
		t.Fatalf("a = %d, want 2", delta["a"])
	}
	if _, moved := delta["b"]; moved {
		t.Fatal("a counter that did not move is not the run's business")
	}
	if delta["c"] != 2 {
		t.Fatalf("c = %d, want 2", delta["c"])
	}
}

func TestTheCounterDifferenceOfARunThatMovedNothingIsEmpty(t *testing.T) {
	if delta := counterDelta(map[string]uint64{"a": 1}, map[string]uint64{"a": 1}); delta != nil {
		t.Fatalf("delta = %v, want nothing", delta)
	}
	if delta := totalDelta(map[string]int64{"a": 1}, map[string]int64{"a": 1}); delta != nil {
		t.Fatalf("delta = %v, want nothing", delta)
	}
}

// The coverage of a run is what says whether the summary kept up. A series
// that goes back is a summary that stopped, and the report has to be able to
// say so.
func TestTheCoverageSaysWhetherTheSummaryStopped(t *testing.T) {
	growing := coverageOf(30, 60, []uint{10, 20, 30})
	if !growing.Monotonic || growing.Stalled {
		t.Fatalf("a growing series is not a stalled one: %+v", growing)
	}
	if growing.Ratio != 0.5 {
		t.Fatalf("ratio = %v, want 0.5", growing.Ratio)
	}
	stalled := coverageOf(20, 60, []uint{10, 20, 15})
	if stalled.Monotonic || !stalled.Stalled {
		t.Fatalf("a series that went back is a stalled one: %+v", stalled)
	}
	if stalled.Explanation == "" {
		t.Fatal("a stalled coverage has to say what happened")
	}
}

// The modes are the three the product knows. A run that asked for a fourth
// one has to fail before it spends anything.
func TestTheLayeredModesAreTheOnesTheProductKnows(t *testing.T) {
	for _, mode := range LayeredModes {
		if !layeredModeKnown(mode) {
			t.Fatalf("%q is not known", mode)
		}
	}
	if layeredModeKnown("partial") {
		t.Fatal("a mode the product does not know is not a mode")
	}
}

// The recorder is global and the run reads a difference, so a run that is
// measured against a recorder that already moved must not report the older
// numbers.
func TestTheRecorderKeepsTheModeItWasSetTo(t *testing.T) {
	recorder := observability.NewMemoryRecorder(nil)
	recorder.SetMode(observability.MemoryModeOn)
	if recorder.Mode() != observability.MemoryModeOn {
		t.Fatalf("mode = %q, want on", recorder.Mode())
	}
	recorder.SummarySegment("failed", "invalid_output", 0, 0)
	snapshot := recorder.Snapshot()
	if snapshot.Counters["summary_segment_failed_total{mode=on,reason=invalid_output}"] != 1 {
		t.Fatalf("the failed segment was not counted: %+v", snapshot.Counters)
	}
	recorder.SetMode(observability.MemoryModeOff)
	if recorder.Mode() != observability.MemoryModeOff {
		t.Fatalf("mode = %q, want off", recorder.Mode())
	}
}

// The evidence of a question is the turn the answer comes from. The turn is
// two messages, and the segment that covers both of them is the one the
// answer can be read from.
func TestTheEvidenceOfATurnIsTheSegmentThatCoversIt(t *testing.T) {
	segments := []LayeredSegment{
		{Seq: 1, StartMessageID: 1, EndMessageID: 8},
		{Seq: 2, StartMessageID: 9, EndMessageID: 16},
	}
	messages := make([]model.Message, 0, 10)
	for i := 0; i < 10; i++ {
		messages = append(messages, model.Message{ID: uint(i + 1)})
	}
	retrieval := runRetrieval(context.Background(), "s1", "什么时间", []int{5}, messages, segments)
	if len(retrieval.Evidence) != 1 || retrieval.Evidence[0] != 2 {
		t.Fatalf("evidence = %v, want the second segment", retrieval.Evidence)
	}
	if retrieval.Hit {
		t.Fatal("nothing was selected, so nothing was hit")
	}
}
