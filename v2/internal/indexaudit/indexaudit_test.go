package indexaudit

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"GopherAI/model"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// fakeSource is a record that the test writes by hand.
type fakeSource struct {
	counts map[Identity]int
	err    error
}

func (s fakeSource) Source(context.Context) (map[Identity]int, error) {
	if s.err != nil {
		return nil, s.err
	}
	result := make(map[Identity]int, len(s.counts))
	for identity, count := range s.counts {
		result[identity] = count
	}
	return result, nil
}

// fakeCounter is an index that the test writes by hand.
type fakeCounter struct {
	counts   map[Identity]int
	blockers []string
	err      error
}

func (c fakeCounter) Counts(context.Context) (map[Identity]int, []string, error) {
	if c.err != nil {
		return nil, nil, c.err
	}
	result := make(map[Identity]int, len(c.counts))
	for identity, count := range c.counts {
		result[identity] = count
	}
	return result, c.blockers, nil
}

// fakeRepository records the documents that were sent back to the indexer.
type fakeRepository struct {
	rebuilt []string
	// failFor is the document that the repository refuses.
	failFor string
	err     error
}

func (r *fakeRepository) Reindex(_ context.Context, id string, _ int64) error {
	if r.err != nil {
		return r.err
	}
	if r.failFor != "" && id == r.failFor {
		return errors.New("document is busy")
	}
	r.rebuilt = append(r.rebuilt, id)
	return nil
}

func TestReconcileReportsShortAndOverfullDocuments(t *testing.T) {
	t.Parallel()
	recorded := map[Identity]int{
		{UserID: 1, DocumentID: "short", Version: 2}: 4,
		{UserID: 1, DocumentID: "exact", Version: 1}: 3,
		{UserID: 1, DocumentID: "over", Version: 1}:  1,
	}
	observed := map[Identity]int{
		{UserID: 1, DocumentID: "short", Version: 2}: 2,
		{UserID: 1, DocumentID: "exact", Version: 1}: 3,
		{UserID: 1, DocumentID: "over", Version: 1}:  5,
	}
	result := Reconcile(recorded, observed)
	if result.Recorded != 3 || result.Expected != 8 || result.Kept != 10 {
		t.Fatalf("unexpected totals: %#v", result)
	}
	if len(result.Missing) != 1 || result.Missing[0].DocumentID != "short" || result.Missing[0].Expected != 4 || result.Missing[0].Actual != 2 {
		t.Fatalf("unexpected missing documents: %#v", result.Missing)
	}
	if len(result.Extra) != 1 || result.Extra[0].DocumentID != "over" {
		t.Fatalf("unexpected extra documents: %#v", result.Extra)
	}
	if len(result.Blockers) != 2 {
		t.Fatalf("a count mismatch did not block: %#v", result.Blockers)
	}
}

func TestReconcileKeepsQuietWhenTheCountsAgree(t *testing.T) {
	t.Parallel()
	identity := Identity{UserID: 7, DocumentID: "doc", Version: 1}
	result := Reconcile(map[Identity]int{identity: 4}, map[Identity]int{identity: 4})
	if len(result.Missing) != 0 || len(result.Extra) != 0 || len(result.Blockers) != 0 {
		t.Fatalf("an equal comparison reported a difference: %#v", result)
	}
}

func TestParseKnowledgeKeyReadsTheDocumentIdentity(t *testing.T) {
	t.Parallel()
	prefix := "gopherai:knowledge:chunk:"
	userID, identity, problem := ParseKnowledgeKey(prefix, prefix+"42:doc:3:chunk")
	if problem != "" || userID != 42 || identity != (Identity{UserID: 42, DocumentID: "doc", Version: 3}) {
		t.Fatalf("unexpected parse: %d %#v %q", userID, identity, problem)
	}
	for _, key := range []string{prefix + "0:doc:1:chunk", prefix + "1::1:chunk", prefix + "1:doc:0:chunk", prefix + "1:doc:1"} {
		if _, _, problem := ParseKnowledgeKey(prefix, key); problem == "" {
			t.Fatalf("invalid key accepted: %s", key)
		}
	}
}

func TestParseModeDefaultsToAuditAndRefusesUnknownModes(t *testing.T) {
	t.Parallel()
	mode, err := ParseMode("")
	if err != nil || mode != ModeAudit {
		t.Fatalf("unexpected default mode: %q %v", mode, err)
	}
	if mode, err = ParseMode(" REBUILD "); err != nil || mode != ModeRebuild {
		t.Fatalf("unexpected rebuild mode: %q %v", mode, err)
	}
	if mode, err = ParseMode("off"); err != nil || mode != ModeOff {
		t.Fatalf("unexpected off mode: %q %v", mode, err)
	}
	if _, err = ParseMode("delete-everything"); err == nil {
		t.Fatal("an unknown mode was accepted")
	}
}

func TestAuditReportsTheShortDocumentsAndTheRebuildCommand(t *testing.T) {
	t.Parallel()
	short := Identity{UserID: 1, DocumentID: "short", Version: 1}
	exact := Identity{UserID: 1, DocumentID: "exact", Version: 1}
	var lines []string
	auditor := NewAuditor(
		fakeSource{counts: map[Identity]int{short: 3, exact: 2}},
		fakeCounter{counts: map[Identity]int{short: 1, exact: 2}},
		nil,
		func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) },
	)
	result, err := auditor.Run(t.Context(), ModeAudit, 0)
	if err != nil {
		t.Fatal(err)
	}
	report := result.Report
	if report.Compared != 2 || len(report.Missing) != 1 || report.Missing[0].DocumentID != "short" {
		t.Fatalf("unexpected report: %#v", report)
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "document=short") || !strings.Contains(joined, "POST /api/v1/knowledge/documents/short/reindex") {
		t.Fatalf("the short document and its rebuild command were not reported: %q", joined)
	}
}

func TestAuditOfAnEmptyIndexReportsEveryDocument(t *testing.T) {
	t.Parallel()
	auditor := NewAuditor(
		fakeSource{counts: map[Identity]int{
			{UserID: 1, DocumentID: "first", Version: 1}:  2,
			{UserID: 2, DocumentID: "second", Version: 1}: 1,
		}},
		fakeCounter{counts: map[Identity]int{}},
		nil,
		func(string, ...any) {},
	)
	report, err := auditor.Audit(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Missing) != 2 || report.Blocks != 0 {
		t.Fatalf("an empty index did not report every document: %#v", report)
	}
}

func TestAuditKeepsTheBlockersOfAnUnreadableIndex(t *testing.T) {
	t.Parallel()
	auditor := NewAuditor(
		fakeSource{counts: map[Identity]int{{UserID: 1, DocumentID: "doc", Version: 1}: 1}},
		fakeCounter{counts: map[Identity]int{}, blockers: []string{"managed prefix contains a non-HASH key"}},
		nil,
		func(string, ...any) {},
	)
	report, err := auditor.Audit(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !slicesContains(report.Blockers, "managed prefix contains a non-HASH key") {
		t.Fatalf("the blocker was lost: %#v", report.Blockers)
	}
}

func slicesContains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func TestRunInOffModeChangesNothing(t *testing.T) {
	t.Parallel()
	repository := &fakeRepository{}
	auditor := NewAuditor(
		fakeSource{counts: map[Identity]int{{UserID: 1, DocumentID: "doc", Version: 1}: 2}},
		fakeCounter{counts: map[Identity]int{}},
		repository,
		func(string, ...any) {},
	)
	result, err := auditor.Run(t.Context(), ModeOff, 5)
	if err != nil {
		t.Fatal(err)
	}
	if result.Compared != 0 || len(repository.rebuilt) != 0 {
		t.Fatalf("the off mode did something: %#v %#v", result, repository.rebuilt)
	}
}

func TestRunInAuditModeRequeuesNothing(t *testing.T) {
	t.Parallel()
	repository := &fakeRepository{}
	auditor := NewAuditor(
		fakeSource{counts: map[Identity]int{{UserID: 1, DocumentID: "doc", Version: 1}: 2}},
		fakeCounter{counts: map[Identity]int{}},
		repository,
		func(string, ...any) {},
	)
	result, err := auditor.Run(t.Context(), ModeAudit, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Missing) != 1 || result.Rebuilt != 0 || len(repository.rebuilt) != 0 {
		t.Fatalf("the audit mode rebuilt something: %#v %#v", result, repository.rebuilt)
	}
}

func TestRunInRebuildModeRequeuesTheShortDocuments(t *testing.T) {
	t.Parallel()
	repository := &fakeRepository{}
	auditor := NewAuditor(
		fakeSource{counts: map[Identity]int{
			{UserID: 1, DocumentID: "short", Version: 1}: 2,
			{UserID: 1, DocumentID: "exact", Version: 1}: 2,
		}},
		fakeCounter{counts: map[Identity]int{
			{UserID: 1, DocumentID: "short", Version: 1}: 1,
			{UserID: 1, DocumentID: "exact", Version: 1}: 2,
		}},
		repository,
		func(string, ...any) {},
	)
	result, err := auditor.Run(t.Context(), ModeRebuild, 5)
	if err != nil {
		t.Fatal(err)
	}
	if result.Rebuilt != 1 || len(repository.rebuilt) != 1 || repository.rebuilt[0] != "short" {
		t.Fatalf("the short document was not requeued: %#v %#v", result, repository.rebuilt)
	}
}

func TestRunInRebuildModeStopsAtTheLimitAndReportsTheRest(t *testing.T) {
	t.Parallel()
	repository := &fakeRepository{}
	recorded := map[Identity]int{}
	observed := map[Identity]int{}
	for _, id := range []string{"first", "second", "third"} {
		recorded[Identity{UserID: 1, DocumentID: id, Version: 1}] = 2
		observed[Identity{UserID: 1, DocumentID: id, Version: 1}] = 1
	}
	auditor := NewAuditor(
		fakeSource{counts: recorded},
		fakeCounter{counts: observed},
		repository,
		func(string, ...any) {},
	)
	result, err := auditor.Run(t.Context(), ModeRebuild, 2)
	if err != nil {
		t.Fatal(err)
	}
	if result.Rebuilt != 2 || result.Skipped != 1 || len(repository.rebuilt) != 2 {
		t.Fatalf("the rebuild limit was not respected: %#v %#v", result, repository.rebuilt)
	}
}

// One document that cannot be sent back must not keep the others from being
// repaired: the audit reports it and moves on.
func TestRunInRebuildModeKeepsGoingWhenOneDocumentIsBusy(t *testing.T) {
	t.Parallel()
	repository := &fakeRepository{failFor: "busy"}
	auditor := NewAuditor(
		fakeSource{counts: map[Identity]int{
			{UserID: 1, DocumentID: "busy", Version: 1}: 2,
			{UserID: 1, DocumentID: "free", Version: 1}: 2,
		}},
		fakeCounter{counts: map[Identity]int{
			{UserID: 1, DocumentID: "busy", Version: 1}: 1,
			{UserID: 1, DocumentID: "free", Version: 1}: 1,
		}},
		repository,
		func(string, ...any) {},
	)
	result, err := auditor.Run(t.Context(), ModeRebuild, 5)
	if err != nil {
		t.Fatal(err)
	}
	if result.Failed != 1 || result.Rebuilt != 1 || len(repository.rebuilt) != 1 || repository.rebuilt[0] != "free" {
		t.Fatalf("a busy document stopped the others: %#v %#v", result, repository.rebuilt)
	}
}

func TestRunInRebuildModeWithoutARepositoryRefusesToRebuild(t *testing.T) {
	t.Parallel()
	auditor := NewAuditor(
		fakeSource{counts: map[Identity]int{{UserID: 1, DocumentID: "doc", Version: 1}: 2}},
		fakeCounter{counts: map[Identity]int{}},
		nil,
		func(string, ...any) {},
	)
	if _, err := auditor.Run(t.Context(), ModeRebuild, 5); err == nil {
		t.Fatal("the rebuild mode accepted a missing repository")
	}
}

func TestMySQLSourceReadsTheActiveVersionsAndLeavesTheInFlightDocumentsOut(t *testing.T) {
	t.Parallel()
	db := knowledgeDatabase(t)
	ready := document("ready", 1, model.KnowledgeStatusReady, 4)
	pending := document("pending", 2, model.KnowledgeStatusPending, 4)
	indexing := document("indexing", 3, model.KnowledgeStatusIndexing, 4)
	failed := document("failed", 4, model.KnowledgeStatusFailed, 2)
	for _, entry := range []*model.KnowledgeDocument{ready, pending, indexing, failed} {
		if err := db.Create(entry).Error; err != nil {
			t.Fatal(err)
		}
	}

	source := MySQLSource{DB: db}
	counts, err := source.Source(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(counts) != 4 {
		t.Fatalf("the collector source changed: %#v", counts)
	}

	auditSource := MySQLSource{DB: db, ExcludeInFlight: true}
	counts, err = auditSource.Source(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(counts) != 2 {
		t.Fatalf("the in-flight documents were not excluded: %#v", counts)
	}
	if _, ok := counts[Identity{UserID: 1, DocumentID: "ready", Version: 1}]; !ok {
		t.Fatalf("the ready document was left out: %#v", counts)
	}
	if _, ok := counts[Identity{UserID: 1, DocumentID: "failed", Version: 1}]; !ok {
		t.Fatalf("the failed document was left out: %#v", counts)
	}
	if _, ok := counts[Identity{UserID: 1, DocumentID: "pending", Version: 1}]; ok {
		t.Fatalf("a pending document was compared: %#v", counts)
	}
}

func knowledgeDatabase(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "indexaudit.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Warn)})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.AutoMigrate(&model.KnowledgeDocument{}); err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	return db
}

func document(id string, version int, status model.KnowledgeDocumentStatus, chunks int) *model.KnowledgeDocument {
	name := id + ".md"
	hash := id + "-hash"
	return &model.KnowledgeDocument{
		ID:            id,
		UserID:        1,
		OriginalName:  name,
		ActiveName:    &name,
		ActiveHash:    &hash,
		StoragePath:   "data/knowledge/" + id,
		FileType:      "md",
		ContentHash:   hash,
		FileSize:      10,
		Version:       version,
		ActiveVersion: 1,
		ChunkCount:    chunks,
		IndexStatus:   status,
	}
}
