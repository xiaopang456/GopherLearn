package summaryconvert

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"GopherAI/model"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const (
	currentContent = `{"schema_version":2,"current_goal":"复习归并排序","confirmed_decisions":[{"text":"用中文讲解","evidence_message_ids":[7]}],"completed_items":["讲完一轮"],"open_loops":[],"user_corrections":[],"key_errors":[],"references":[]}`
	legacyContent  = `{"schema_version":2,"current_goal":"复习归并排序","confirmed_decisions":[{"text":"用中文讲解","evidence_message_ids":["7"]}],"completed_items":["讲完一轮"],"open_loops":[],"user_corrections":[],"key_errors":[],"references":[],"legacy_text":"更早的散文：归并排序是稳定的。"}`
	proseContent   = "用户想复习归并排序，已经讲完一轮。\n\n还要出练习题。"
	damagedContent = `{"schema_version":2,"current_goal":"复习","mood":"good"}`
)

func conversionDatabase(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "summaryconvert.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Warn)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.Session{}, &model.SessionSummary{}); err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

func seedRow(t *testing.T, db *gorm.DB, sessionID string, content string, watermark uint, version uint64) {
	t.Helper()
	if err := db.Create(&model.Session{ID: sessionID, UserID: 1, UserName: "learner", Mode: model.SessionModeNormal, CreatedAt: time.Unix(1, 0), UpdatedAt: time.Unix(1, 0)}).Error; err != nil {
		t.Fatalf("seed session: %v", err)
	}
	row := &model.SessionSummary{UserID: 1, SessionID: sessionID, Content: content, CoveredMessageID: watermark, Version: version, UpdatedAt: time.Unix(1, 0)}
	if err := db.Create(row).Error; err != nil {
		t.Fatalf("seed summary: %v", err)
	}
}

func readRow(t *testing.T, db *gorm.DB, sessionID string) model.SessionSummary {
	t.Helper()
	var row model.SessionSummary
	if err := db.Where("session_id = ?", sessionID).First(&row).Error; err != nil {
		t.Fatalf("read back %s: %v", sessionID, err)
	}
	return row
}

func TestRunPreviewWritesNothing(t *testing.T) {
	db := conversionDatabase(t)
	seedRow(t, db, "s-legacy", legacyContent, 7, 3)
	seedRow(t, db, "s-prose", proseContent, 9, 2)
	seedRow(t, db, "s-current", currentContent, 7, 5)
	seedRow(t, db, "s-damaged", damagedContent, 7, 1)

	report, err := NewRunner(NewStore(db), Options{Apply: false, Limit: 64, Budget: model.DefaultSummaryBudget()}).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	counts := report.CountByShape()
	if counts[ShapeLegacyJSON] != 1 || counts[ShapeProse] != 1 || counts[ShapeCurrent] != 1 || counts[ShapeDamaged] != 1 {
		t.Fatalf("shape counts=%v, want one of each shape", counts)
	}
	for _, sessionID := range []string{"s-legacy", "s-prose", "s-current", "s-damaged"} {
		stored := readRow(t, db, sessionID)
		want := map[string]string{"s-legacy": legacyContent, "s-prose": proseContent, "s-current": currentContent, "s-damaged": damagedContent}[sessionID]
		if stored.Content != want {
			t.Fatalf("%s content changed in a preview: %q", sessionID, stored.Content)
		}
	}
	if stored := readRow(t, db, "s-legacy"); stored.Version != 3 {
		t.Fatalf("version moved in a preview: %d", stored.Version)
	}
	for _, outcome := range report.Outcomes {
		switch outcome.Shape {
		case ShapeLegacyJSON, ShapeProse:
			if outcome.Action != ActionWouldConvert {
				t.Fatalf("%s row action=%q, want a preview of a write", outcome.Shape, outcome.Action)
			}
		default:
			if outcome.Action != ActionUnchanged {
				t.Fatalf("row %s should not move, action=%q", outcome.Row.SessionID, outcome.Action)
			}
		}
	}
}

func TestRunApplyConvertsLegacyWithoutMovingTheWatermark(t *testing.T) {
	db := conversionDatabase(t)
	seedRow(t, db, "s-legacy", legacyContent, 7, 3)

	runner := NewRunner(NewStore(db), Options{Apply: true, Limit: 8, Budget: model.DefaultSummaryBudget()})
	if _, err := runner.Run(context.Background()); err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	stored := readRow(t, db, "s-legacy")
	if stored.CoveredMessageID != 7 {
		t.Fatalf("watermark moved to %d: a conversion covers the same messages it did before", stored.CoveredMessageID)
	}
	if stored.Version != 4 {
		t.Fatalf("version=%d, want the row to move forward by one", stored.Version)
	}
	document, err := model.DecodeSessionSummaryDocument(stored.Content)
	if err != nil {
		t.Fatalf("the stored row is still not readable: %v\n%s", err, stored.Content)
	}
	if got := document.EvidenceMessageIDs(); len(got) != 1 || got[0] != 7 {
		t.Fatalf("ids=%v, want the string citation carried over as 7", got)
	}
	if stored.TokenEstimate <= 0 {
		t.Fatalf("token estimate=%d, want the new content measured", stored.TokenEstimate)
	}
	if joined := strings.Join(document.CompletedItems, "\n"); !strings.Contains(joined, "更早的散文：归并排序是稳定的。") {
		t.Fatalf("legacy_text was not carried into completed_items: %v", document.CompletedItems)
	}
}

func TestRunApplyConvertsProseWithoutACall(t *testing.T) {
	db := conversionDatabase(t)
	seedRow(t, db, "s-prose", proseContent, 9, 2)

	if _, err := NewRunner(NewStore(db), Options{Apply: true, Limit: 8, Budget: model.DefaultSummaryBudget()}).Run(context.Background()); err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	stored := readRow(t, db, "s-prose")
	if stored.CoveredMessageID != 9 || stored.Version != 3 {
		t.Fatalf("row moved wrongly: watermark=%d version=%d", stored.CoveredMessageID, stored.Version)
	}
	document, err := model.DecodeSessionSummaryDocument(stored.Content)
	if err != nil {
		t.Fatalf("converted prose is not readable: %v", err)
	}
	if joined := strings.Join(document.CompletedItems, ""); joined != proseContent {
		t.Fatalf("the prose was not carried over:\n got: %q\nwant: %q", joined, proseContent)
	}
}

func TestRunApplyLeavesReadableAndUnreadableRowsAlone(t *testing.T) {
	db := conversionDatabase(t)
	seedRow(t, db, "s-current", currentContent, 7, 5)
	seedRow(t, db, "s-damaged", damagedContent, 7, 1)
	seedRow(t, db, "s-empty", "", 0, 1)

	runner := NewRunner(NewStore(db), Options{Apply: true, Limit: 8, Budget: model.DefaultSummaryBudget()})
	report, err := runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if report.CountByAction()[ActionConverted] != 0 {
		t.Fatalf("a readable or unreadable row was rewritten: %#v", report.Outcomes)
	}
	if stored := readRow(t, db, "s-current"); stored.Content != currentContent || stored.Version != 5 {
		t.Fatalf("the current row moved: version=%d", stored.Version)
	}
	if stored := readRow(t, db, "s-damaged"); stored.Content != damagedContent || stored.Version != 1 {
		t.Fatalf("the damaged row was touched: version=%d", stored.Version)
	}
}

// TestRunRefusesAnUnexpressibleCitationAndKeepsTheRow: a citation this schema
// cannot carry refuses the conversion, and the row keeps its old bytes. The
// refusal is in the report, with the id named.
func TestRunRefusesAnUnexpressibleCitationAndKeepsTheRow(t *testing.T) {
	db := conversionDatabase(t)
	content := `{"schema_version":2,"current_goal":"复习","confirmed_decisions":[{"text":"用中文讲解","evidence_message_ids":["msg-7"]}],"completed_items":[],"open_loops":[],"user_corrections":[],"key_errors":[],"references":[]}`
	seedRow(t, db, "s-legacy", content, 7, 2)

	report, err := NewRunner(NewStore(db), Options{Apply: true, Limit: 8, Budget: model.DefaultSummaryBudget()}).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if report.CountByAction()[ActionConverted] != 0 {
		t.Fatalf("a row with an unexpressible citation was converted: %#v", report.Outcomes)
	}
	if len(report.Outcomes) != 1 || !strings.Contains(report.Outcomes[0].Reason, "msg-7") {
		t.Fatalf("the report does not name the citation: %#v", report.Outcomes)
	}
	if stored := readRow(t, db, "s-legacy"); stored.Content != content || stored.Version != 2 {
		t.Fatalf("the refused row was modified: version=%d", stored.Version)
	}
}

// TestRunRefusesADocumentOverTheBudget: a narrowed budget refuses the row
// rather than trimming the user's summary.
func TestRunRefusesADocumentOverTheBudget(t *testing.T) {
	db := conversionDatabase(t)
	content := `{"schema_version":2,"current_goal":"复习","confirmed_decisions":[{"text":"一","evidence_message_ids":["7"]},{"text":"二","evidence_message_ids":["8"]},{"text":"三","evidence_message_ids":["9"]}],"completed_items":[],"open_loops":[],"user_corrections":[],"key_errors":[],"references":[]}`
	seedRow(t, db, "s-legacy", content, 9, 1)

	tight := model.SummaryBudget{GroupItemLimit: 2, OpenLoopsItemLimit: 2, ItemEvidenceLimit: model.SessionSummaryEvidenceLimit, EvidenceIDLimit: 64}
	report, err := NewRunner(NewStore(db), Options{Apply: true, Limit: 8, Budget: tight}).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if report.CountByAction()[ActionConverted] != 0 {
		t.Fatalf("a document over the budget was written")
	}
	if stored := readRow(t, db, "s-legacy"); stored.Content != content {
		t.Fatalf("the refused row was rewritten: %q", stored.Content)
	}
}

func TestWriteIsRefusedOnceTheRowMoved(t *testing.T) {
	db := conversionDatabase(t)
	seedRow(t, db, "s-legacy", legacyContent, 7, 3)
	store := NewStore(db)

	rows, err := store.List(context.Background(), 8)
	if err != nil || len(rows) != 1 {
		t.Fatalf("List()=%v err=%v", rows, err)
	}
	if err := store.Write(context.Background(), rows[0], currentContent); err != nil {
		t.Fatalf("first write: %v", err)
	}
	// The same stale row a second time: the version has moved, so the guarded
	// write must refuse rather than write the same content twice.
	if err := store.Write(context.Background(), rows[0], currentContent); err == nil {
		t.Fatal("a stale write landed")
	}
	// A write that would move the watermark is refused even with the right
	// version, because the predicate pins it.
	fresh := rows[0]
	fresh.Version++
	fresh.CoveredMessageID = 8
	if err := store.Write(context.Background(), fresh, currentContent); err == nil {
		t.Fatal("a write that moved the watermark landed")
	}
	if stored := readRow(t, db, "s-legacy"); stored.CoveredMessageID != 7 {
		t.Fatalf("watermark moved to %d", stored.CoveredMessageID)
	}
}

func TestRunReportsRowsWhoseSessionIsGone(t *testing.T) {
	db := conversionDatabase(t)
	// A summary row with no session at all: the join must not hide it, or the
	// count of old shapes in a database would read lower than the truth.
	if err := db.Create(&model.SessionSummary{UserID: 1, SessionID: "s-orphan", Content: legacyContent, CoveredMessageID: 7, Version: 1, UpdatedAt: time.Unix(1, 0)}).Error; err != nil {
		t.Fatalf("seed orphan: %v", err)
	}

	rows, err := NewStore(db).List(context.Background(), 8)
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}
	if len(rows) != 1 || rows[0].SessionID != "s-orphan" {
		t.Fatalf("rows=%v, want the orphan row still listed", rows)
	}
	if rows[0].Mode != "" {
		t.Fatalf("mode=%q, want no mode for a row whose session is gone", rows[0].Mode)
	}

	report, err := NewRunner(NewStore(db), Options{Apply: true, Limit: 8, Budget: model.DefaultSummaryBudget()}).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if report.CountByAction()[ActionConverted] != 1 {
		t.Fatalf("the orphan row was not converted: %#v", report.Outcomes)
	}
	if stored := readRow(t, db, "s-orphan"); stored.CoveredMessageID != 7 {
		t.Fatalf("watermark moved on an orphan: %d", stored.CoveredMessageID)
	}
}

func TestRunReportsRowsLeftBeyondTheLimit(t *testing.T) {
	db := conversionDatabase(t)
	seedRow(t, db, "s-a", legacyContent, 7, 1)
	seedRow(t, db, "s-b", legacyContent, 7, 1)

	report, err := NewRunner(NewStore(db), Options{Apply: false, Limit: 1, Budget: model.DefaultSummaryBudget()}).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if report.RowsRead != 1 || !report.RowsWithMore {
		t.Fatalf("read=%d more=%t, want one row and a warning that more exist", report.RowsRead, report.RowsWithMore)
	}
}
