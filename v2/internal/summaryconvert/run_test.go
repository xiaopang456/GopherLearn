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
	currentContent  = `{"schema_version":2,"current_goal":"复习归并排序","confirmed_decisions":[{"text":"用中文讲解","evidence_message_ids":[7]}],"completed_items":["讲完一轮"],"open_loops":[],"user_corrections":[],"key_errors":[],"references":[]}`
	legacyContent   = `{"schema_version":2,"current_goal":"复习归并排序","confirmed_decisions":[{"text":"用中文讲解","evidence_message_ids":["7"]}],"completed_items":["讲完一轮"],"open_loops":[],"user_corrections":[],"key_errors":[],"references":[],"legacy_text":"更早的散文"}`
	proseContent    = "用户想复习归并排序，已经讲完一轮，还要出练习题。"
	damagedContent  = `{"schema_version":2,"current_goal":"复习","mood":"good"}`
	unreadableShape = ShapeDamaged
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

// fakeCoveredMessages answers with a fixed covered range and records what it was
// asked for, so a test can prove the conversion cited messages it was shown.
type fakeCoveredMessages struct {
	messages []model.Message
	more     bool
	err      error
	askedFor []uint
	limit    int
}

func (f *fakeCoveredMessages) Covered(_ context.Context, _ int64, _ string, watermark uint, limit int) ([]model.Message, bool, error) {
	f.askedFor = append(f.askedFor, watermark)
	f.limit = limit
	return f.messages, f.more, f.err
}

// fakeProse returns a prepared answer and counts calls, so a preview run can be
// proven to have spent nothing.
type fakeProse struct {
	answer string
	err    error
	calls  int
	seen   []string
}

func (f *fakeProse) GenerateFromProse(_ context.Context, prose string, _ []model.Message) (string, error) {
	f.calls++
	f.seen = append(f.seen, prose)
	return f.answer, f.err
}

func coveredRange() []model.Message {
	return []model.Message{
		{ID: 7, SessionID: "s-prose", Content: "帮我复习归并排序", IsUser: true},
		{ID: 8, SessionID: "s-prose", Content: "用中文讲解", IsUser: true},
		{ID: 9, SessionID: "s-prose", Content: "再出两道练习题", IsUser: true},
	}
}

func TestRunPreviewWritesNothingAndSpendsNoCall(t *testing.T) {
	db := conversionDatabase(t)
	seedRow(t, db, "s-legacy", legacyContent, 7, 3)
	seedRow(t, db, "s-prose", proseContent, 9, 2)
	seedRow(t, db, "s-current", currentContent, 7, 5)
	seedRow(t, db, "s-damaged", damagedContent, 7, 1)

	prose := &fakeProse{answer: currentContent}
	runner := NewRunnerWithMessages(NewStore(db), &fakeCoveredMessages{messages: coveredRange()}, Options{
		Apply: false, Limit: 64, Budget: model.DefaultSummaryBudget(), Prose: prose,
	})

	report, err := runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if prose.calls != 0 {
		t.Fatalf("a preview spent %d model calls, want 0", prose.calls)
	}
	counts := report.CountByShape()
	if counts[ShapeLegacyJSON] != 1 || counts[ShapeProse] != 1 || counts[ShapeCurrent] != 1 || counts[unreadableShape] != 1 {
		t.Fatalf("shape counts=%v, want one of each of the four readable shapes", counts)
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
		case ShapeLegacyJSON:
			if outcome.Action != ActionWouldConvert {
				t.Fatalf("legacy row action=%q, want a preview of a write", outcome.Action)
			}
		case ShapeProse:
			if outcome.Action != ActionUnchanged || !strings.Contains(outcome.Reason, "model") {
				t.Fatalf("prose row action=%q reason=%q, want it named as the arm that costs a call", outcome.Action, outcome.Reason)
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

	runner := NewRunnerWithMessages(NewStore(db), &fakeCoveredMessages{}, Options{Apply: true, Limit: 8, Budget: model.DefaultSummaryBudget()})
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
}

func TestRunApplyLeavesReadableAndUnreadableRowsAlone(t *testing.T) {
	db := conversionDatabase(t)
	seedRow(t, db, "s-current", currentContent, 7, 5)
	seedRow(t, db, "s-damaged", damagedContent, 7, 1)
	seedRow(t, db, "s-empty", "", 0, 1)

	runner := NewRunnerWithMessages(NewStore(db), &fakeCoveredMessages{}, Options{Apply: true, Limit: 8, Budget: model.DefaultSummaryBudget()})
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

func TestProseArmCitesOnlyWhatItWasShown(t *testing.T) {
	db := conversionDatabase(t)
	seedRow(t, db, "s-prose", proseContent, 9, 2)
	// The model cites message 8, which is inside the covered range.
	answer := `{"schema_version":2,"current_goal":"复习归并排序","confirmed_decisions":[{"text":"用中文讲解","evidence_message_ids":[8]}],"completed_items":[],"open_loops":[{"text":"再出两道练习题","evidence_message_ids":[9]}],"user_corrections":[],"key_errors":[],"references":[]}`
	prose := &fakeProse{answer: answer}
	messages := fakeCoveredMessages{messages: coveredRange()}

	runner := NewRunnerWithMessages(NewStore(db), &messages, Options{Apply: true, Limit: 8, MessageLimit: 512, Budget: model.DefaultSummaryBudget(), Prose: prose})
	if _, err := runner.Run(context.Background()); err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if prose.calls != 1 {
		t.Fatalf("calls=%d, want one per prose row", prose.calls)
	}
	if prose.seen[0] != proseContent {
		t.Fatalf("the model was given %q, want the stored prose itself", prose.seen[0])
	}
	if messages.askedFor[0] != 9 {
		t.Fatalf("the covered read was asked for watermark %d, want the row's own watermark", messages.askedFor[0])
	}
	stored := readRow(t, db, "s-prose")
	if stored.CoveredMessageID != 9 || stored.Version != 3 {
		t.Fatalf("row moved wrongly: watermark=%d version=%d", stored.CoveredMessageID, stored.Version)
	}
	if _, err := model.DecodeSessionSummaryDocument(stored.Content); err != nil {
		t.Fatalf("converted prose is not readable: %v", err)
	}
}

func TestProseArmRefusesAFabricatedCitation(t *testing.T) {
	db := conversionDatabase(t)
	seedRow(t, db, "s-prose", proseContent, 9, 2)
	// Message 42 was not supplied: the citation names a message this covered
	// range does not hold.
	answer := `{"schema_version":2,"current_goal":"复习","confirmed_decisions":[{"text":"用中文讲解","evidence_message_ids":[42]}],"completed_items":[],"open_loops":[],"user_corrections":[],"key_errors":[],"references":[]}`
	prose := &fakeProse{answer: answer}

	report, err := NewRunnerWithMessages(NewStore(db), &fakeCoveredMessages{messages: coveredRange()}, Options{
		Apply: true, Limit: 8, Budget: model.DefaultSummaryBudget(), Prose: prose,
	}).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if report.CountByAction()[ActionConverted] != 0 {
		t.Fatalf("a fabricated citation was stored: %#v", report.Outcomes)
	}
	if stored := readRow(t, db, "s-prose"); stored.Content != proseContent || stored.Version != 2 {
		t.Fatalf("the refused row was modified: version=%d", stored.Version)
	}
	if prose.calls != 1 {
		t.Fatalf("calls=%d, want the call to have been made and its answer rejected", prose.calls)
	}
}

func TestProseArmRefusesAnUnbackedItemButKeepsProseText(t *testing.T) {
	db := conversionDatabase(t)
	seedRow(t, db, "s-prose", proseContent, 9, 2)
	// An item with no citations that is not the prose's own wording: nothing in
	// this row backs it, so the whole answer is refused.
	answer := `{"schema_version":2,"current_goal":"复习","confirmed_decisions":[{"text":"用户已经学会","evidence_message_ids":[]}],"completed_items":[],"open_loops":[],"user_corrections":[],"key_errors":[],"references":[]}`

	report, err := NewRunnerWithMessages(NewStore(db), &fakeCoveredMessages{messages: coveredRange()}, Options{
		Apply: true, Limit: 8, Budget: model.DefaultSummaryBudget(), Prose: &fakeProse{answer: answer},
	}).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if report.CountByAction()[ActionConverted] != 0 {
		t.Fatalf("an item backed by nothing was stored")
	}
	if stored := readRow(t, db, "s-prose"); stored.Content != proseContent {
		t.Fatalf("the prose was replaced: %q", stored.Content)
	}
}

func TestProseArmCarriesAnIdLessItemThatIsTheProseOwnText(t *testing.T) {
	db := conversionDatabase(t)
	seedRow(t, db, "s-prose", proseContent, 9, 2)
	// The prose says 还要出练习题; an item with no citations may carry that
	// wording because the stored row is itself the evidence for it.
	answer := `{"schema_version":2,"current_goal":"复习归并排序","confirmed_decisions":[{"text":"还要出练习题","evidence_message_ids":[]}],"completed_items":[],"open_loops":[],"user_corrections":[],"key_errors":[],"references":[]}`

	if _, err := NewRunnerWithMessages(NewStore(db), &fakeCoveredMessages{messages: coveredRange()}, Options{
		Apply: true, Limit: 8, Budget: model.DefaultSummaryBudget(), Prose: &fakeProse{answer: answer},
	}).Run(context.Background()); err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	stored := readRow(t, db, "s-prose")
	if stored.Version != 3 {
		t.Fatalf("version=%d, want the row converted", stored.Version)
	}
	document, err := model.DecodeSessionSummaryDocument(stored.Content)
	if err != nil {
		t.Fatalf("converted row unreadable: %v", err)
	}
	if len(document.ConfirmedDecisions) != 1 || len(document.ConfirmedDecisions[0].EvidenceMessageIDs) != 0 {
		t.Fatalf("the carried item changed shape: %v", document.ConfirmedDecisions)
	}
}

func TestProseArmStopsWhenTheCoveredRangeIsTooLong(t *testing.T) {
	db := conversionDatabase(t)
	seedRow(t, db, "s-prose", proseContent, 9, 2)
	prose := &fakeProse{answer: currentContent}
	messages := &fakeCoveredMessages{messages: coveredRange(), more: true}

	report, err := NewRunnerWithMessages(NewStore(db), messages, Options{
		Apply: true, Limit: 8, MessageLimit: 2, Budget: model.DefaultSummaryBudget(), Prose: prose,
	}).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if messages.limit != 2 {
		t.Fatalf("the covered read was asked for %d messages, want the configured limit", messages.limit)
	}
	if prose.calls != 0 {
		t.Fatalf("a half-read covered range still cost a model call: %d", prose.calls)
	}
	if report.CountByAction()[ActionConverted] != 0 {
		t.Fatalf("a partly-read session was converted")
	}
	if stored := readRow(t, db, "s-prose"); stored.Content != proseContent {
		t.Fatalf("the row was rewritten: %q", stored.Content)
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

	report, err := NewRunnerWithMessages(NewStore(db), &fakeCoveredMessages{}, Options{Apply: true, Limit: 8, Budget: model.DefaultSummaryBudget()}).Run(context.Background())
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

	report, err := NewRunnerWithMessages(NewStore(db), &fakeCoveredMessages{}, Options{Apply: false, Limit: 1, Budget: model.DefaultSummaryBudget()}).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if report.RowsRead != 1 || !report.RowsWithMore {
		t.Fatalf("read=%d more=%t, want one row and a warning that more exist", report.RowsRead, report.RowsWithMore)
	}
}
