package observability

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestMemoryRecorderUsesMetadataOnlyBoundedLabels(t *testing.T) {
	secret := "private-memory-body"
	var logs []string
	recorder := NewMemoryRecorder(func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) })
	recorder.Projection("completed", "", 25*time.Millisecond, 2*time.Second)
	recorder.Index("failed", 8*time.Millisecond, 3*time.Second)
	recorder.Retrieval("decision_history", []string{"exact+lexical", secret}, 0, secret, 4*time.Millisecond)
	recorder.Retrieval("last_session", []string{"mysql_time"}, 1, secret, time.Millisecond)
	recorder.Context(900, 1000, 90, 80, 70)
	recorder.ProviderDegraded(secret)
	recorder.KnowledgeRetrieval("unavailable", "embedding_unavailable", 3*time.Millisecond)
	recorder.KnowledgeCandidate("vector+lexical")
	recorder.KnowledgeRerank("applied", 7*time.Millisecond)
	recorder.KnowledgeRerank("fallback", 11*time.Millisecond)
	recorder.KnowledgeAnswer("general_knowledge", "no_evidence", secret)
	recorder.KnowledgeFallbackDuration(7 * time.Millisecond)

	snapshot := recorder.Snapshot()
	if snapshot.Counters["episode_projection_jobs_total{status=completed,error_code=none}"] != 1 ||
		snapshot.Counters["episode_index_lag_total"] != 1 ||
		snapshot.Counters["episode_candidates_total{channel=exact}"] != 1 ||
		snapshot.Counters["episode_abstained_total{reason=other}"] != 1 ||
		snapshot.Counters["memory_provider_degraded_total{provider=other}"] != 1 {
		t.Fatalf("unexpected snapshot: %#v", snapshot)
	}
	if snapshot.Counters["knowledge_retrieval_total{status=unavailable,reason=embedding_unavailable}"] != 1 ||
		snapshot.Counters["knowledge_candidate_total{match_kind=hybrid}"] != 1 ||
		snapshot.Counters["knowledge_rerank_total{status=applied}"] != 1 ||
		snapshot.Counters["knowledge_rerank_total{status=fallback}"] != 1 ||
		snapshot.Counters["knowledge_fallback_total{reason=other}"] != 1 {
		t.Fatalf("unexpected knowledge snapshot: %#v", snapshot)
	}
	if snapshot.Totals["knowledge_fallback_duration_ms"] != 7 || snapshot.Totals["knowledge_rerank_duration_ms"] != 18 {
		t.Fatalf("fallback duration = %#v", snapshot.Totals)
	}
	if snapshot.Counters["episode_selected_total"] != 1 || snapshot.Counters["episode_abstained_total{reason=none}"] != 0 {
		t.Fatalf("selected retrieval used abstain reason: %#v", snapshot)
	}
	for _, line := range logs {
		if strings.Contains(line, secret) {
			t.Fatalf("log format leaked caller text: %q", line)
		}
	}
}

// The board and segment counters have to stay bounded: a label that comes
// from a model or from free text would otherwise become a metric name.
func TestMemoryRecorderBoundsTheWhiteboardAndSegmentLabels(t *testing.T) {
	secret := "private-board-body"
	var logs []string
	recorder := NewMemoryRecorder(func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) })
	recorder.Whiteboard(1, 2, 3, 4, 5, 6, 7, 8, 9, 10)
	recorder.SummarySegment("failed", secret, 0, 0)
	recorder.SummarySegment("generated", "none", 4, 2)
	recorder.SummarySegmentAssistant(3, 1)

	snapshot := recorder.Snapshot()
	if snapshot.Totals["whiteboard_applied_total"] != 1 ||
		snapshot.Totals["whiteboard_changed_total"] != 2 ||
		snapshot.Totals["whiteboard_fabricated_total"] != 3 ||
		snapshot.Totals["whiteboard_suspicious_delete_total"] != 4 ||
		snapshot.Totals["whiteboard_missed_update_total"] != 5 ||
		snapshot.Totals["whiteboard_truncated_total"] != 6 ||
		snapshot.Totals["whiteboard_dropped_total"] != 7 ||
		snapshot.Totals["whiteboard_dropped_done_total"] != 8 ||
		snapshot.Totals["whiteboard_unproven_acceptance_total"] != 9 ||
		snapshot.Totals["whiteboard_unknown_origin_total"] != 10 {
		t.Fatalf("unexpected whiteboard totals: %#v", snapshot.Totals)
	}
	if snapshot.Totals["summary_segment_assistant_products_total"] != 3 ||
		snapshot.Totals["summary_segment_assistant_accepted_total"] != 1 {
		t.Fatalf("unexpected assistant product totals: %#v", snapshot.Totals)
	}
	if snapshot.Counters["whiteboard_fabricated_batches_total{mode=off}"] != 1 ||
		snapshot.Counters["whiteboard_suspicious_delete_batches_total{mode=off}"] != 1 ||
		snapshot.Counters["whiteboard_missed_update_batches_total{mode=off}"] != 1 {
		t.Fatalf("unexpected whiteboard batch counters: %#v", snapshot.Counters)
	}
	if snapshot.Counters["summary_segment_total{mode=off,outcome=failed,reason=other}"] != 1 {
		t.Fatalf("an unnamed failure reason was not bounded: %#v", snapshot.Counters)
	}
	if snapshot.Counters["summary_segment_total{mode=off,outcome=generated,reason=none}"] != 1 {
		t.Fatalf("unexpected segment counters: %#v", snapshot.Counters)
	}
	if snapshot.Counters["summary_segment_failed_total{mode=off,reason=other}"] != 1 {
		t.Fatalf("unexpected segment failure counters: %#v", snapshot.Counters)
	}
	if snapshot.Totals["summary_segment_points_total"] != 4 ||
		snapshot.Totals["summary_segment_points_dropped_total"] != 2 {
		t.Fatalf("unexpected segment totals: %#v", snapshot.Totals)
	}
	for _, line := range logs {
		if strings.Contains(line, secret) {
			t.Fatalf("log format leaked caller text: %q", line)
		}
	}
}

// The counters carry the mode, because a shadow run and an applied run answer
// different questions: a shadow run is asked how well the write side works,
// an applied run is asked how well the whole thing works. Numbers that cannot
// be told apart are numbers that cannot be read.
func TestMemoryRecorderLabelsTheCountersWithTheMode(t *testing.T) {
	recorder := NewMemoryRecorder(nil)
	recorder.SetMode(MemoryModeShadow)
	recorder.SummarySegmentJob("failed", "model_error", 0)
	recorder.SummarySegmentFallback("generation_failed", 100)

	snapshot := recorder.Snapshot()
	if snapshot.Mode != MemoryModeShadow {
		t.Fatalf("the snapshot reports mode %q", snapshot.Mode)
	}
	if snapshot.Counters["summary_segment_job_total{mode=shadow,status=failed,error_code=model_error}"] != 1 {
		t.Fatalf("the job counter is not labelled with the mode: %#v", snapshot.Counters)
	}
	if snapshot.Counters["summary_segment_fallback_total{mode=shadow,reason=generation_failed}"] != 1 {
		t.Fatalf("the fallback counter is not labelled with the mode: %#v", snapshot.Counters)
	}
	// The same counters under a different mode are separate numbers.
	recorder.SetMode(MemoryModeOn)
	recorder.SummarySegmentJob("completed", "", 0)
	snapshot = recorder.Snapshot()
	if snapshot.Counters["summary_segment_job_total{mode=on,status=completed,error_code=none}"] != 1 {
		t.Fatalf("the applied mode is not counted separately: %#v", snapshot.Counters)
	}
	if snapshot.Counters["summary_segment_job_total{mode=shadow,status=failed,error_code=model_error}"] != 1 {
		t.Fatalf("the shadow count was lost: %#v", snapshot.Counters)
	}
}

// A batch that held no turns is not a failure, so it carries no error code.
func TestTheEmptyBatchCarriesNoErrorCode(t *testing.T) {
	recorder := NewMemoryRecorder(nil)
	recorder.SummarySegmentJob("empty", "", 0)
	snapshot := recorder.Snapshot()
	if snapshot.Counters["summary_segment_job_total{mode=off,status=empty,error_code=none}"] != 1 {
		t.Fatalf("the empty batch was not counted: %#v", snapshot.Counters)
	}
}

// An unknown mode is not a free-text label.
func TestMemoryRecorderBoundsTheModeLabel(t *testing.T) {
	recorder := NewMemoryRecorder(nil)
	recorder.SetMode("something else")
	if recorder.Mode() != "other" {
		t.Fatalf("the mode is %q", recorder.Mode())
	}
}
