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
