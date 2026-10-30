package agent

import (
	"GopherAI/internal/testdb"
	"GopherAI/model"
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	mysqldriver "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestListRunsBySessionKeepsTheNewestRunsAndRefusesAnotherOwner(t *testing.T) {
	dsn := testdb.RequireMySQLDSN(t)
	db, err := gorm.Open(mysqldriver.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.Session{}, &model.AgentRun{}, &model.AgentStep{}, &model.Message{}); err != nil {
		t.Fatal(err)
	}
	repository := NewRepository(db)
	ctx := context.Background()
	session := &model.Session{ID: uuid.NewString(), UserID: time.Now().UnixNano(), UserName: "run-read", Mode: model.SessionModeAgent}
	if err := db.Create(session).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Where("session_id = ?", session.ID).Delete(&model.Message{})
		db.Where("run_id IN (?)", db.Model(&model.AgentRun{}).Select("id").Where("session_id = ?", session.ID)).Delete(&model.AgentStep{})
		db.Where("session_id = ?", session.ID).Delete(&model.AgentRun{})
		db.Unscoped().Where("id = ?", session.ID).Delete(&model.Session{})
	})

	base := time.Now().Add(-2 * time.Hour)
	runIDs := make([]string, 0, MaxRunsPerSessionRead+1)
	for index := 0; index < MaxRunsPerSessionRead+1; index++ {
		run := &model.AgentRun{
			ID: uuid.NewString(), UserID: session.UserID, SessionID: session.ID,
			ExecutionKind: model.AgentRunExecutionChat, Status: model.AgentRunCompleted,
			Version: 1, StartedAt: base.Add(time.Duration(index) * time.Minute),
		}
		if err := db.Create(run).Error; err != nil {
			t.Fatal(err)
		}
		runIDs = append(runIDs, run.ID)
		if index < 2 {
			if err := db.Create(&model.AgentStep{
				UserID: session.UserID, RunID: run.ID, Sequence: 1, Type: model.AgentStepTool,
				ToolName: "knowledge_search", DurationMillis: int64(index + 1),
			}).Error; err != nil {
				t.Fatal(err)
			}
		}
	}

	runs, truncated, err := repository.ListRunsBySession(ctx, session.UserID, session.ID, MaxRunsPerSessionRead)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated {
		t.Fatal("a list that was cut did not say so")
	}
	if len(runs) != MaxRunsPerSessionRead {
		t.Fatalf("a cut list answered %d runs", len(runs))
	}
	// The repository answers newest first: the run that started last is first.
	if runs[0].ID != runIDs[len(runIDs)-1] {
		t.Fatalf("the newest run is not first: %s", runs[0].ID)
	}
	if runs[len(runs)-1].ID != runIDs[1] {
		t.Fatalf("the oldest run did not fit: %s", runs[len(runs)-1].ID)
	}

	// Another account sees nothing, and is not told that the runs exist.
	foreign, foreignTruncated, err := repository.ListRunsBySession(ctx, session.UserID+1, session.ID, MaxRunsPerSessionRead)
	if err != nil {
		t.Fatal(err)
	}
	if len(foreign) != 0 || foreignTruncated {
		t.Fatalf("another account read %d runs", len(foreign))
	}

	// Another account's steps are absent too, even with the run id in hand.
	steps, err := repository.ListStepsByRuns(ctx, session.UserID+1, runIDs[:2])
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 0 {
		t.Fatalf("another account read %d steps", len(steps))
	}
	own, err := repository.ListStepsByRuns(ctx, session.UserID, runIDs[:2])
	if err != nil {
		t.Fatal(err)
	}
	if len(own) != 2 || own[0].ToolName != "knowledge_search" || own[0].UserID != session.UserID {
		t.Fatalf("own steps %+v", own)
	}
}

func TestListRunsBySessionAnswersAnEmptySessionWithNothing(t *testing.T) {
	dsn := testdb.RequireMySQLDSN(t)
	db, err := gorm.Open(mysqldriver.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.Session{}, &model.AgentRun{}, &model.AgentStep{}, &model.Message{}); err != nil {
		t.Fatal(err)
	}
	repository := NewRepository(db)
	ctx := context.Background()
	session := &model.Session{ID: uuid.NewString(), UserID: time.Now().UnixNano(), UserName: "run-read-empty", Mode: model.SessionModeAgent}
	if err := db.Create(session).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Where("session_id = ?", session.ID).Delete(&model.AgentRun{})
		db.Unscoped().Where("id = ?", session.ID).Delete(&model.Session{})
	})

	runs, truncated, err := repository.ListRunsBySession(ctx, session.UserID, session.ID, MaxRunsPerSessionRead)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 || truncated {
		t.Fatalf("an empty session answered %d runs (truncated %v)", len(runs), truncated)
	}
	if _, err := repository.ListStepsByRuns(ctx, session.UserID, nil); err != nil {
		t.Fatalf("an empty step read failed: %v", err)
	}
	if _, _, err := repository.ListRunsBySession(ctx, 0, session.ID, MaxRunsPerSessionRead); err == nil {
		t.Fatal("a read without an owner was accepted")
	}
	if _, _, err := repository.ListRunsBySession(ctx, session.UserID, " ", MaxRunsPerSessionRead); err == nil {
		t.Fatal("a read without a session was accepted")
	}
}
