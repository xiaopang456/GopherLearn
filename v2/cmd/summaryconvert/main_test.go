package main

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"GopherAI/config"
	"GopherAI/internal/summaryconvert"
	"GopherAI/model"
	agentService "GopherAI/service/agent"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// The command has to judge a row the way the running deployment does. Its
// budget is the deployment's, read from the configuration it loaded, not a
// constant: a deployment that narrowed the group limit must refuse a document
// that overflows it, and a deployment that widened it must not refuse a
// document it can hold.

func deployment(groupItemLimit, openLoopsItemLimit, evidenceIDLimit int) *config.Config {
	return &config.Config{AgentConfig: config.AgentConfig{
		SummaryGroupItemLimit:     groupItemLimit,
		SummaryOpenLoopsItemLimit: openLoopsItemLimit,
		SummaryEvidenceIDLimit:    evidenceIDLimit,
	}}
}

func TestSummaryOptionsCarryTheDeploymentBudget(t *testing.T) {
	options := summaryOptions(deployment(2, 3, 50), true, 7)

	if !options.Apply || options.Limit != 7 {
		t.Fatalf("options=%+v, want the flags passed through", options)
	}
	want := agentService.SummaryBudgetOf(2, 3, 50)
	if options.Budget != want {
		t.Fatalf("budget=%+v, want the deployment's %+v", options.Budget, want)
	}
	if options.Budget.ItemEvidenceLimit != model.SessionSummaryEvidenceLimit {
		t.Fatalf("item evidence limit=%d, want the runtime's %d", options.Budget.ItemEvidenceLimit, model.SessionSummaryEvidenceLimit)
	}
}

func TestTheCommandRefusesWhatTheDeploymentBudgetNarrows(t *testing.T) {
	db := commandDatabase(t)
	seedSummary(t, db, "s-three", legacyWithItems(3))

	report, err := summaryconvert.NewRunner(summaryconvert.NewStore(db),
		summaryOptions(deployment(2, 2, 64), false, 8)).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if report.CountByAction()[summaryconvert.ActionWouldConvert] != 0 {
		t.Fatalf("a document over the configured budget was accepted: %#v", report.Outcomes)
	}
	if len(report.Outcomes) != 1 || !strings.Contains(report.Outcomes[0].Reason, "budget") {
		t.Fatalf("the report does not say the budget refused it: %#v", report.Outcomes)
	}
}

func TestTheCommandAcceptsWhatTheDeploymentBudgetWidens(t *testing.T) {
	db := commandDatabase(t)
	seedSummary(t, db, "s-three", legacyWithItems(3))

	report, err := summaryconvert.NewRunner(summaryconvert.NewStore(db),
		summaryOptions(deployment(3, 3, 64), false, 8)).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if report.CountByAction()[summaryconvert.ActionWouldConvert] != 1 {
		t.Fatalf("a document the deployment can hold was refused: %#v", report.Outcomes)
	}
}

// TestTheCommandAcceptsADocumentTheDefaultBudgetWouldRefuse: the default
// budget is a fallback, not the rule. A deployment that widened the limit
// holds more items than the default does, and the command has to convert them.
func TestTheCommandAcceptsADocumentTheDefaultBudgetWouldRefuse(t *testing.T) {
	db := commandDatabase(t)
	seedSummary(t, db, "s-many", legacyWithItems(model.DefaultSummaryBudget().GroupItemLimit+1))

	report, err := summaryconvert.NewRunner(summaryconvert.NewStore(db),
		summaryOptions(deployment(model.DefaultSummaryBudget().GroupItemLimit+2, 64, 200), false, 8)).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if report.CountByAction()[summaryconvert.ActionWouldConvert] != 1 {
		t.Fatalf("a widened deployment refused a document it can hold: %#v", report.Outcomes)
	}
}

func commandDatabase(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "summaryconvert-command.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Warn)})
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

func seedSummary(t *testing.T, db *gorm.DB, sessionID, content string) {
	t.Helper()
	if err := db.Create(&model.Session{ID: sessionID, UserID: 1, UserName: "learner", Mode: model.SessionModeNormal, CreatedAt: time.Unix(1, 0), UpdatedAt: time.Unix(1, 0)}).Error; err != nil {
		t.Fatalf("seed session: %v", err)
	}
	row := &model.SessionSummary{UserID: 1, SessionID: sessionID, Content: content, CoveredMessageID: 7, Version: 1, UpdatedAt: time.Unix(1, 0)}
	if err := db.Create(row).Error; err != nil {
		t.Fatalf("seed summary: %v", err)
	}
}

// legacyWithItems builds a legacy document with the given number of evidence
// items, each citing its own message.
func legacyWithItems(count int) string {
	items := make([]string, 0, count)
	for index := 0; index < count; index++ {
		items = append(items, `{"text":"一条结论","evidence_message_ids":["`+strconv.Itoa(index+1)+`"]}`)
	}
	return `{"schema_version":2,"current_goal":"复习","confirmed_decisions":[` + strings.Join(items, ",") +
		`],"completed_items":[],"open_loops":[],"user_corrections":[],"key_errors":[],"references":[]}`
}
