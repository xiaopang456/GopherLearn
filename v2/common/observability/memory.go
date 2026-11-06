package observability

import (
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"
)

// MemorySnapshot is an in-process, metadata-only view of the long-term-memory
// pipeline. Keys are bounded labels chosen by the server; user text and memory
// bodies are never accepted by this package.
type MemorySnapshot struct {
	Counters map[string]uint64
	Totals   map[string]int64
	// Mode is the layered summary's mode at the time of the snapshot. The
	// counters carry it too, so a snapshot can be read without knowing which
	// mode produced it.
	Mode string
}

// The layered summary's modes.
//
// Off writes nothing. Shadow writes the segments and the board but leaves the
// context to the old path, so the write side can be watched without changing
// what the model sees. On reads the new state back.
const (
	MemoryModeOff    = "off"
	MemoryModeShadow = "shadow"
	MemoryModeOn     = "on"
)

// MemoryRecorder keeps low-cardinality counters and emits the same values as
// structured log lines. The snapshot is useful to focused diagnostics and
// tests, while logs remain available in ordinary Docker deployments without a
// separate metrics backend.
type MemoryRecorder struct {
	mu       sync.RWMutex
	counters map[string]uint64
	totals   map[string]int64
	mode     string
	logf     func(string, ...any)
}

func NewMemoryRecorder(logf func(string, ...any)) *MemoryRecorder {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &MemoryRecorder{counters: map[string]uint64{}, totals: map[string]int64{}, mode: MemoryModeOff, logf: logf}
}

var Memory = NewMemoryRecorder(log.Printf)

// SetMode records which mode the layered summary is running in.
//
// The mode is a property of the deployment rather than of a request, so it
// lives on the recorder instead of being passed to every counter. The
// counters carry it as a label because a shadow run and an applied run answer
// different questions: a shadow run is asked how well the write side works,
// an applied run is asked how well the whole thing works.
func (r *MemoryRecorder) SetMode(mode string) {
	mode = boundedLabel(mode, MemoryModeOff, MemoryModeShadow, MemoryModeOn)
	r.mu.Lock()
	r.mode = mode
	r.mu.Unlock()
}

// Mode reports the mode the recorder is counting in.
func (r *MemoryRecorder) Mode() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.mode
}

// modeLabel returns the mode as a metric label.
func (r *MemoryRecorder) modeLabel() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.mode == "" {
		return MemoryModeOff
	}
	return r.mode
}

func (r *MemoryRecorder) Projection(status, errorCode string, duration, lag time.Duration) {
	status = boundedLabel(status, "completed", "failed")
	errorCode = boundedLabel(errorCode, "none", "projection_timeout", "projection_cancelled", "sensitive_content", "source_boundary", "invalid_model_output", "source_not_found", "projection_failed")
	r.add("episode_projection_jobs_total{status="+status+",error_code="+errorCode+"}", 1)
	r.addTotal("episode_projection_duration_ms", duration.Milliseconds())
	r.addTotal("episode_projection_lag_seconds", int64(nonNegative(lag).Seconds()))
	r.logf("memory_metric name=episode_projection status=%s error_code=%s duration_ms=%d lag_seconds=%d", status, errorCode, duration.Milliseconds(), int64(nonNegative(lag).Seconds()))
}

func (r *MemoryRecorder) Index(status string, duration, lag time.Duration) {
	status = boundedLabel(status, "indexed", "failed", "deleted")
	r.add("episode_index_total{status="+status+"}", 1)
	r.addTotal("episode_index_duration_ms", duration.Milliseconds())
	r.addTotal("episode_index_lag_seconds", int64(nonNegative(lag).Seconds()))
	if status == "failed" {
		r.add("episode_index_lag_total", 1)
	}
	r.logf("memory_metric name=episode_index status=%s duration_ms=%d lag_seconds=%d", status, duration.Milliseconds(), int64(nonNegative(lag).Seconds()))
}

func (r *MemoryRecorder) Retrieval(intent string, channels []string, selected int, abstainReason string, duration time.Duration) {
	intent = boundedLabel(intent, "none", "last_session", "time_range", "topic_history", "decision_history")
	abstainReason = boundedLabel(abstainReason, "none", "empty_query", "current_session_has_history", "no_history_intent", "exact_topic_unavailable", "exact_topic_not_found", "redis_unavailable", "no_eligible_candidate", "provider_error", "no_selected_episode")
	r.add("episode_retrieval_total{intent="+intent+"}", 1)
	unique := map[string]struct{}{}
	for _, channel := range channels {
		for _, part := range strings.Split(channel, "+") {
			part = boundedLabel(part, "exact", "lexical", "vector", "mysql_time", "explicit_topic")
			if part != "other" {
				unique[part] = struct{}{}
			}
		}
	}
	ordered := make([]string, 0, len(unique))
	for channel := range unique {
		ordered = append(ordered, channel)
	}
	sort.Strings(ordered)
	for _, channel := range ordered {
		r.add("episode_candidates_total{channel="+channel+"}", 1)
	}
	if selected > 0 {
		abstainReason = "none"
		r.add("episode_selected_total", uint64(selected))
	} else {
		if abstainReason == "none" {
			abstainReason = "no_selected_episode"
		}
		r.add("episode_abstained_total{reason="+abstainReason+"}", 1)
	}
	r.addTotal("episode_retrieval_duration_ms", duration.Milliseconds())
	r.logf("memory_metric name=episode_retrieval intent=%s channels=%s selected=%d abstain_reason=%s duration_ms=%d", intent, strings.Join(ordered, ","), selected, abstainReason, duration.Milliseconds())
}

func (r *MemoryRecorder) Context(projected, inputTarget, episode, global, topic int) {
	r.add("episode_context_tokens", uint64(max(episode, 0)))
	r.add("user_memory_context_tokens{scope=global}", uint64(max(global, 0)))
	r.add("user_memory_context_tokens{scope=topic}", uint64(max(topic, 0)))
	r.addTotal("context_projected_input_tokens", int64(max(projected, 0)))
	r.logf("memory_metric name=context_tokens projected_input=%d input_target=%d episode=%d user_memory_global=%d user_memory_topic=%d", projected, inputTarget, episode, global, topic)
}

func (r *MemoryRecorder) ProviderDegraded(provider string) {
	provider = boundedLabel(provider, "mastery", "episode", "user_memory", "session_summary", "layered_summary", "message_history", "redis", "embedding", "projector", "mysql", "topic_bootstrap")
	r.add("memory_provider_degraded_total{provider="+provider+"}", 1)
	r.logf("memory_metric name=provider_degraded provider=%s", provider)
}

func (r *MemoryRecorder) KnowledgeRetrieval(status, reason string, duration time.Duration) {
	status = boundedLabel(status, "complete", "no_evidence", "unavailable", "failed")
	reason = boundedLabel(reason, "none", "no_candidate", "embedding_unavailable", "vector_unavailable", "lexical_unavailable", "index_unavailable", "document_metadata_unavailable", "invalid_result")
	r.add("knowledge_retrieval_total{status="+status+",reason="+reason+"}", 1)
	r.addTotal("knowledge_retrieval_duration_ms", duration.Milliseconds())
	r.logf("memory_metric name=knowledge_retrieval status=%s reason=%s duration_ms=%d", status, reason, duration.Milliseconds())
}

func (r *MemoryRecorder) KnowledgeCandidate(matchKind string) {
	if strings.TrimSpace(matchKind) == "vector+lexical" {
		matchKind = "hybrid"
	}
	matchKind = boundedLabel(matchKind, "vector", "lexical", "hybrid")
	r.add("knowledge_candidate_total{match_kind="+matchKind+"}", 1)
}

func (r *MemoryRecorder) KnowledgeRerank(status string, duration time.Duration) {
	status = boundedLabel(status, "applied", "fallback", "failed", "bypassed", "retried")
	r.add("knowledge_rerank_total{status="+status+"}", 1)
	r.addTotal("knowledge_rerank_duration_ms", duration.Milliseconds())
	r.logf("memory_metric name=knowledge_rerank status=%s duration_ms=%d", status, duration.Milliseconds())
}

func (r *MemoryRecorder) KnowledgeAnswer(basis, status, reason string) {
	basis = boundedLabel(basis, "conversation", "personal_knowledge", "general_knowledge", "refused", "legacy_unspecified")
	status = boundedLabel(status, "not_required", "grounded", "no_evidence", "unavailable", "invalid_grounding", "legacy_unspecified")
	reason = boundedLabel(reason, "none", "search_required", "no_active_document", "no_candidate", "model_abstained", "embedding_unavailable", "vector_unavailable", "lexical_unavailable", "index_unavailable", "document_metadata_unavailable", "invalid_citation", "missing_citation", "fallback_citation", "search_not_performed")
	r.add("knowledge_answer_total{basis="+basis+",status="+status+"}", 1)
	if basis == "general_knowledge" {
		r.add("knowledge_fallback_total{reason="+reason+"}", 1)
	}
	if status == "invalid_grounding" {
		r.add("knowledge_grounding_rejected_total{reason="+reason+"}", 1)
	}
	r.logf("memory_metric name=knowledge_answer basis=%s status=%s reason=%s", basis, status, reason)
}

func (r *MemoryRecorder) KnowledgeFallbackDuration(duration time.Duration) {
	r.addTotal("knowledge_fallback_duration_ms", duration.Milliseconds())
	r.logf("memory_metric name=knowledge_fallback duration_ms=%d", duration.Milliseconds())
}

// Whiteboard records what one batch did to a session board.
//
// Every count here is an intervention the server had to make, so a rising
// number names the part of the generator that is drifting. Nothing in this
// call carries board text: the label set is fixed and the rest are counts.
func (r *MemoryRecorder) Whiteboard(applied, changed, fabricated, suspiciousDelete, missedUpdate, truncated, dropped, droppedDone int) {
	mode := r.modeLabel()
	r.addTotal("whiteboard_applied_total", int64(applied))
	r.addTotal("whiteboard_changed_total", int64(changed))
	r.addTotal("whiteboard_fabricated_total", int64(fabricated))
	r.addTotal("whiteboard_suspicious_delete_total", int64(suspiciousDelete))
	r.addTotal("whiteboard_missed_update_total", int64(missedUpdate))
	r.addTotal("whiteboard_truncated_total", int64(truncated))
	r.addTotal("whiteboard_dropped_total", int64(dropped))
	r.addTotal("whiteboard_dropped_done_total", int64(droppedDone))
	if fabricated > 0 {
		r.add("whiteboard_fabricated_batches_total{mode="+mode+"}", 1)
	}
	if suspiciousDelete > 0 {
		r.add("whiteboard_suspicious_delete_batches_total{mode="+mode+"}", 1)
	}
	if missedUpdate > 0 {
		r.add("whiteboard_missed_update_batches_total{mode="+mode+"}", 1)
	}
	r.logf("memory_metric name=whiteboard mode=%s applied=%d changed=%d fabricated=%d suspicious_delete=%d missed_update=%d truncated=%d dropped=%d dropped_done=%d",
		mode, applied, changed, fabricated, suspiciousDelete, missedUpdate, truncated, dropped, droppedDone)
}

// SummarySegment records what one segment generation produced or why it did
// not produce one.
//
// The outcome label is bounded, so a new failure reason has to be named here
// before it can be reported; an unnamed one is counted as "other" rather than
// stored as free text. The counts alongside it are what the generator had to
// drop on the way to a storable segment.
func (r *MemoryRecorder) SummarySegment(outcome, reason string, points, pointsDropped int) {
	mode := r.modeLabel()
	outcome = boundedLabel(outcome, "generated", "failed")
	reason = boundedLabel(reason, "none", "invalid_output", "unknown_schema_version", "point_without_source",
		"input_too_large", "output_too_large", "model_error", "empty_output")
	r.add("summary_segment_total{mode="+mode+",outcome="+outcome+",reason="+reason+"}", 1)
	if outcome == "failed" {
		r.add("summary_segment_failed_total{mode="+mode+",reason="+reason+"}", 1)
	}
	r.addTotal("summary_segment_points_total", int64(points))
	if pointsDropped > 0 {
		r.addTotal("summary_segment_points_dropped_total", int64(pointsDropped))
	}
	r.logf("memory_metric name=summary_segment mode=%s outcome=%s reason=%s points=%d points_dropped=%d",
		mode, outcome, reason, points, pointsDropped)
}

// SummarySegmentJob records what one background segment attempt did.
//
// The status label is bounded, and the error code is a fixed set, so neither
// becomes free text in a metric name. The duration is the cost of the
// attempt, which is the number that says whether the background is keeping up.
func (r *MemoryRecorder) SummarySegmentJob(status, errorCode string, duration time.Duration) {
	mode := r.modeLabel()
	status = boundedLabel(status, "completed", "failed", "empty")
	errorCode = boundedLabel(errorCode, "none", "task_timeout", "task_cancelled", "whiteboard_conflict",
		"source_not_found", "invalid_model_output", "model_error", "generation_failed")
	if status == "completed" || status == "empty" {
		// An attempt that wrote a segment and an attempt that had no turns to
		// write both ended without a failure, so there is no error to name.
		errorCode = "none"
	}
	r.add("summary_segment_job_total{mode="+mode+",status="+status+",error_code="+errorCode+"}", 1)
	r.addTotal("summary_segment_job_duration_ms", duration.Milliseconds())
	r.logf("memory_metric name=summary_segment_job mode=%s status=%s error_code=%s duration_ms=%d",
		mode, status, errorCode, duration.Milliseconds())
}

// SummarySegmentFallback records that the raw conversation was too big to
// send and a segment was generated before the answer went out.
//
// The reason says which of the three happened: it was not needed, it ran, or
// it failed and the raw conversation was truncated instead. The count is what
// the raw conversation was worth, so a fallback that keeps happening can be
// traced to the conversation that caused it.
func (r *MemoryRecorder) SummarySegmentFallback(reason string, unsummarizedTokens int) {
	mode := r.modeLabel()
	reason = boundedLabel(reason, "not_needed", "generated_before_answer", "generation_failed",
		"generator_unavailable", "background_queued", "background_running")
	r.add("summary_segment_fallback_total{mode="+mode+",reason="+reason+"}", 1)
	r.addTotal("summary_segment_fallback_tokens", int64(max(unsummarizedTokens, 0)))
	r.logf("memory_metric name=summary_segment_fallback mode=%s reason=%s unsummarized_tokens=%d", mode, reason, unsummarizedTokens)
}

func (r *MemoryRecorder) Snapshot() MemorySnapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := MemorySnapshot{Counters: make(map[string]uint64, len(r.counters)), Totals: make(map[string]int64, len(r.totals))}
	if r.mode == "" {
		result.Mode = MemoryModeOff
	} else {
		result.Mode = r.mode
	}
	for key, value := range r.counters {
		result.Counters[key] = value
	}
	for key, value := range r.totals {
		result.Totals[key] = value
	}
	return result
}

func (r *MemoryRecorder) add(key string, value uint64) {
	r.mu.Lock()
	r.counters[key] += value
	r.mu.Unlock()
}

func (r *MemoryRecorder) addTotal(key string, value int64) {
	r.mu.Lock()
	r.totals[key] += value
	r.mu.Unlock()
}

func label(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "none"
	}
	var out strings.Builder
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '_' || char == '-' {
			out.WriteRune(char)
		}
	}
	if out.Len() == 0 {
		return "other"
	}
	return out.String()
}

func boundedLabel(value string, allowed ...string) string {
	value = label(value)
	for _, candidate := range allowed {
		if value == candidate {
			return value
		}
	}
	return "other"
}

func nonNegative(value time.Duration) time.Duration {
	if value < 0 {
		return 0
	}
	return value
}

func max(left, right int) int {
	if left > right {
		return left
	}
	return right
}

func (s MemorySnapshot) String() string {
	return fmt.Sprintf("counters=%d totals=%d", len(s.Counters), len(s.Totals))
}
