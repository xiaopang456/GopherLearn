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
	Reindex ReindexRepository
	// Logf writes the report. It is never nil after New.
	Logf func(string, ...any)
}

// NewAuditor builds an auditor over the given parts.
func NewAuditor(source Source, counter Counter, reindex ReindexRepository, logf func(string, ...any)) *Auditor {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Auditor{Source: source, Counter: counter, Reindex: reindex, Logf: logf}
}

// Audit performs the comparison and reports it. It changes nothing.
func (a *Auditor) Audit(ctx context.Context) (Report, error) {
	if a.Source == nil || a.Counter == nil {
		return Report{}, fmt.Errorf("index audit requires a source and a counter")
	}
	expected, err := a.Source.Source(ctx)
	if err != nil {
		return Report{}, err
	}
	actual, blockers, err := a.Counter.Counts(ctx)
	if err != nil {
		return Report{}, err
	}
	comparison := Reconcile(expected, actual)
	report := Report{
		Compared: comparison.Recorded,
		Blocks:   comparison.Kept,
		Missing:  comparison.Missing,
		Extra:    comparison.Extra,
		Blockers: append(append([]string(nil), blockers...), comparison.Blockers...),
	}
	return report, nil
}

// Run audits and, in rebuild mode, sends the documents that are short back to
// the indexer.
//
// The rebuild is bounded by limit: at most that many documents are sent back,
// and the rest are only reported. A document that goes back is reindexed by
// the ordinary worker - the audit only returns it to the queue.
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

// report writes what the audit found: one summary line, one line per
// document that is short - with the command that rebuilds it - and one line
// per reason the comparison could not be completed.
func (a *Auditor) report(report Report, mode Mode) {
	a.Logf("index_audit status=%s compared=%d blocks=%d missing=%d extra=%d",
		mode, report.Compared, report.Blocks, len(report.Missing), len(report.Extra))
	for _, blocker := range report.Blockers {
		a.Logf("index_audit status=incomplete blocker=%q", blocker)
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
