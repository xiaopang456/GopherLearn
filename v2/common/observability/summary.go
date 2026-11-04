package observability

// The counters of the rolling summary. They live in their own file because
// they are about the summary and not about the memory pipeline, and because a
// summary that outgrew its budget is an event worth counting on its own: a
// session that is trimmed is a session that lost something, and a session
// that failed to update is a session that lost everything.

// SummaryOutcome counts one compaction that did not produce a summary.
//
// The reason separates the failures that are worth fixing (the model did not
// answer, the body could not be read) from the ones that are not (the
// cooldown held the compaction back, the request had no room left).
func (r *MemoryRecorder) SummaryOutcome(reason string) {
	reason = boundedLabel(reason, "none", "model_call", "parse", "structure", "length", "timeout", "storage", "empty_output", "cooldown", "skipped")
	r.add("memory_summary_outcome_total{reason="+reason+"}", 1)
	r.logf("memory_metric name=summary_outcome reason=%s", reason)
}

// SummaryTruncated counts one cut the compression budget made.
//
// The group says which part of the summary was cut and the kind says what was
// counted: items of a group, or the message ids those items cite.
func (r *MemoryRecorder) SummaryTruncated(group, kind string, kept, dropped int) {
	group = boundedLabel(group, "confirmed_decisions", "open_loops", "user_corrections", "completed_items", "key_errors", "references")
	kind = boundedLabel(kind, "items", "evidence_ids")
	r.add("memory_summary_truncated_total{group="+group+",kind="+kind+"}", 1)
	r.addTotal("memory_summary_truncated_items_total", int64(dropped))
	r.logf("memory_metric name=summary_truncated group=%s kind=%s kept=%d dropped=%d", group, kind, kept, dropped)
}
