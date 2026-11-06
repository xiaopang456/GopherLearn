package indexaudit

import (
	"context"
	"fmt"
	"strings"

	"GopherAI/config"
)

// Mode selects what an index audit does with what it finds.
type Mode string

const (
	// ModeOff does not audit at all.
	ModeOff = Mode(config.StartupIndexAuditModeOff)
	// ModeAudit reports the documents whose index is short and changes
	// nothing.
	ModeAudit = Mode(config.StartupIndexAuditModeAudit)
	// ModeRebuild reports them and sends the first ones back to the indexer.
	ModeRebuild = Mode(config.StartupIndexAuditModeRebuild)
)

// DefaultRebuildLimit is the number of documents one start may send back to
// the indexer when the configuration does not say.
//
// The limit exists because a rebuild is not free: every document that goes
// back is parsed, chunked and embedded again. A start that rebuilds a thousand
// documents would spend a thousand embeddings without anybody asking for it.
const DefaultRebuildLimit = config.DefaultStartupIndexAuditRebuildLimit

// ParseMode reads the mode out of a configuration value.
//
// An empty value means audit. A deployment that never heard of the audit
// still gets the report, because an index that is thinner than the record
// says is silent: the retrieval simply answers less.
func ParseMode(value string) (Mode, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", string(ModeAudit):
		return ModeAudit, nil
	case string(ModeOff):
		return ModeOff, nil
	case string(ModeRebuild):
		return ModeRebuild, nil
	default:
		return "", fmt.Errorf("index audit mode must be %q, %q or %q", ModeOff, ModeAudit, ModeRebuild)
	}
}

// ReindexRepository is the part of the document repository a rebuild needs.
type ReindexRepository interface {
	Reindex(ctx context.Context, id string, userID int64) error
}

// Report is the result of one comparison.
type Report struct {
	// Compared is the number of document versions the record knows.
	Compared int
	// Blocks is the number of blocks the index holds for those versions.
	Blocks int
	// Missing are the document versions whose index holds fewer blocks than
	// the record says.
	Missing []Document
	// Extra are the document versions whose index holds more blocks than the
	// record says.
	Extra []Document
	// Blockers are the reasons the comparison could not be completed. They
	// are categories, never document text.
	Blockers []string
	// IndexProblems are the reasons the search index does not agree with the
	// keys it is supposed to index: an index that is absent, one that reports
	// a different number of documents than the keys under its own prefix, one
	// that is still working, or one that failed to index documents.
	//
	// They are kept apart from Blockers because they answer a different
	// question. A blocker says the count could not be trusted; an index
	// problem says the count was trustworthy and the index over those counted
	// blocks is not usable. Both are reported, and neither is a reason to
	// refuse a start or to send a document back to the indexer.
	IndexProblems []string
}

// Result is what one audit found and did.
type Result struct {
	Mode Mode
	Report
	// Rebuilt is the number of documents sent back to the indexer.
	Rebuilt int
	// Skipped is the number of documents that were short but left alone,
	// because the rebuild limit was reached.
	Skipped int
	// Failed is the number of documents that could not be sent back.
	Failed int
}

// Auditor compares the record with the index and, in rebuild mode, sends the
// documents that are short back to the indexer.
//
// It never refuses the start and never deletes anything: a thin index is a
// retrieval that answers less, not a service that cannot run.
type Auditor struct {
	Source  Source
	Counter Counter
	// Index is the part that looks at the search index instead of at the keys
	// under it. It is not optional: a comparison of block counts alone calls
	// a dropped index consistent, because a dropped index removes nothing.
	Index   IndexChecker
	Reindex ReindexRepository
	// Logf writes the report. It is never nil after New.
	Logf func(string, ...any)
}

// NewAuditor builds an auditor over the given parts.
func NewAuditor(source Source, counter Counter, index IndexChecker, reindex ReindexRepository, logf func(string, ...any)) *Auditor {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Auditor{Source: source, Counter: counter, Index: index, Reindex: reindex, Logf: logf}
}

// Audit performs the comparison and reports it. It changes nothing.
func (a *Auditor) Audit(ctx context.Context) (Report, error) {
	if a.Source == nil || a.Counter == nil || a.Index == nil {
		return Report{}, fmt.Errorf("index audit requires a source, a counter and an index checker")
	}
	expected, err := a.Source.Source(ctx)
	if err != nil {
		return Report{}, err
	}
	actual, blockers, err := a.Counter.Counts(ctx)
	if err != nil {
		return Report{}, err
	}
	indexProblems, err := a.Index.CheckIndexes(ctx)
	if err != nil {
		return Report{}, err
	}
	comparison := Reconcile(expected, actual)
	report := Report{
		Compared:      comparison.Recorded,
		Blocks:        comparison.Kept,
		Missing:       comparison.Missing,
		Extra:         comparison.Extra,
		Blockers:      append(append([]string(nil), blockers...), comparison.Blockers...),
		IndexProblems: append([]string(nil), indexProblems...),
	}
	return report, nil
}

// Run audits and, in rebuild mode, sends the documents that are short back to
// the indexer.
//
// The rebuild is bounded by limit: at most that many documents are sent back,
// and the rest are only reported. A document that goes back is reindexed by
// the ordinary worker - the audit only returns it to the queue.
//
// An index problem does not send anything back. A document whose blocks are
// all present is not short, and a dropped index is repaired by creating the
// index, which the indexing path and the retrieval service each do on their
// own; requeuing documents to fix index metadata would re-embed blocks that
// are already in Redis for nothing.
func (a *Auditor) Run(ctx context.Context, mode Mode, limit int) (Result, error) {
	if mode == ModeOff {
		return Result{Mode: ModeOff}, nil
	}
	report, err := a.Audit(ctx)
	if err != nil {
		return Result{Mode: mode}, err
	}
	result := Result{Mode: mode, Report: report}
	a.report(report, mode)
	if mode != ModeRebuild {
		return result, nil
	}
	if limit <= 0 {
		limit = DefaultRebuildLimit
	}
	if a.Reindex == nil {
		return result, fmt.Errorf("index audit rebuild requires a document repository")
	}
	for index, document := range report.Missing {
		if index >= limit {
			result.Skipped++
			a.Logf("index_audit document=%s user=%d version=%d expected=%d actual=%d status=skipped reason=rebuild_limit",
				document.DocumentID, document.UserID, document.Version, document.Expected, document.Actual)
			continue
		}
		// A document that cannot be sent back is reported and the rest are
		// still tried: one document that is busy must not keep the others
		// from being repaired.
		if err := a.Reindex.Reindex(ctx, document.DocumentID, document.UserID); err != nil {
			result.Failed++
			a.Logf("index_audit document=%s user=%d version=%d expected=%d actual=%d status=requeue_failed reason=%q",
				document.DocumentID, document.UserID, document.Version, document.Expected, document.Actual, err.Error())
			continue
		}
		result.Rebuilt++
		a.Logf("index_audit document=%s user=%d version=%d expected=%d actual=%d status=requeued",
			document.DocumentID, document.UserID, document.Version, document.Expected, document.Actual)
	}
	if result.Skipped > 0 {
		a.Logf("index_audit status=partial reason=rebuild_limit skipped=%d limit=%d", result.Skipped, limit)
	}
	if result.Failed > 0 {
		a.Logf("index_audit status=partial reason=requeue_failed failed=%d", result.Failed)
	}
	return result, nil
}

// Status is the one word an audit result is recorded under.
//
// "incomplete" covers both ways this start could not vouch for the index: a
// count it could not trust, and an index that does not agree with the keys
// under it. The log tells those apart; the metric does not, because for an
// operator they are the same finding - the audit finished without proving the
// index sound.
func (r Result) Status() string {
	switch {
	case len(r.Blockers) > 0 || len(r.IndexProblems) > 0:
		return "incomplete"
	case r.Skipped > 0 || r.Failed > 0:
		return "partial"
	case len(r.Missing) > 0 || len(r.Extra) > 0:
		return "inconsistent"
	default:
		return "consistent"
	}
}

// report writes what the audit found: one summary line, one line per
// document that is short - with the command that rebuilds it -, one line per
// reason the comparison could not be completed, and one line per reason the
// index itself does not agree with the keys under it.
func (a *Auditor) report(report Report, mode Mode) {
	a.Logf("index_audit status=%s compared=%d blocks=%d missing=%d extra=%d index_problems=%d",
		mode, report.Compared, report.Blocks, len(report.Missing), len(report.Extra), len(report.IndexProblems))
	for _, blocker := range report.Blockers {
		a.Logf("index_audit status=incomplete blocker=%q", blocker)
	}
	for _, problem := range report.IndexProblems {
		a.Logf("index_audit index_problem=%q", problem)
	}
	for _, document := range report.Missing {
		a.Logf("index_audit document=%s user=%d version=%d expected=%d actual=%d status=short rebuild_command=%q",
			document.DocumentID, document.UserID, document.Version, document.Expected, document.Actual, RebuildCommand(document.DocumentID))
	}
}

// RebuildCommand is the command that sends one document back to the indexer.
//
// It is the product's own per-document endpoint: the document's owner (or the
// operator acting for them) asks the service to index it again, and the
// ordinary worker does the work.
func RebuildCommand(documentID string) string {
	return fmt.Sprintf("POST /api/v1/knowledge/documents/%s/reindex", documentID)
}
