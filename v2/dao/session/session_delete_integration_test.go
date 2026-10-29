package session

import (
	"GopherAI/internal/testdb"
	"GopherAI/model"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	mysqldriver "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestDeleteOwnedRemovesConversationAggregateAndRejectsForeignOwner(t *testing.T) {
	dsn := testdb.RequireMySQLDSN(t)
	db, err := gorm.Open(mysqldriver.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent), TranslateError: true})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.AutoMigrate(
		&model.User{}, &model.Session{}, &model.Message{}, &model.AgentRun{}, &model.AgentStep{},
		&model.SessionSummary{}, &model.EpisodeMemory{}, &model.EpisodeProjectionJob{},
		&model.MemoryExtractionJob{}, &model.MemoryCandidate{}, &model.MemoryDreamRun{},
		&model.MemoryDraft{}, &model.LongTermMemory{},
	); err != nil {
		t.Fatal(err)
	}
	ctx, now := context.Background(), time.Now()
	user := model.User{Username: "del-" + uuid.NewString()[:12], Password: "secret"}
	if err = db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	sessionID, runID, episodeID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	t.Cleanup(func() {
		for _, target := range []any{
			&model.MemoryDreamRun{}, &model.MemoryCandidate{}, &model.MemoryExtractionJob{}, &model.MemoryDraft{},
			&model.LongTermMemory{}, &model.AgentStep{}, &model.AgentRun{}, &model.EpisodeProjectionJob{},
			&model.EpisodeMemory{}, &model.SessionSummary{}, &model.Session{},
		} {
			db.Unscoped().Where("user_id = ?", user.ID).Delete(target)
		}
		db.Unscoped().Where("session_id = ?", sessionID).Delete(&model.Message{})
		db.Unscoped().Where("id = ?", user.ID).Delete(&model.User{})
	})
	source := "session:" + sessionID
	rows := []any{
		&model.Session{ID: sessionID, UserID: user.ID, UserName: user.Username, Mode: model.SessionModeAgent, Title: "delete"},
		&model.Message{SessionID: sessionID, UserName: user.Username, Content: "source", IsUser: true},
		&model.AgentRun{ID: runID, UserID: user.ID, SessionID: sessionID, Status: model.AgentRunCompleted, Version: 2, StartedAt: now, FinishedAt: &now},
		&model.AgentStep{UserID: user.ID, RunID: runID, Sequence: 1, Type: model.AgentStepAnswer, ResultSummary: "done"},
		&model.SessionSummary{UserID: user.ID, SessionID: sessionID, Content: "summary", CoveredMessageID: 1, TokenEstimate: 1, Version: 1},
		&model.EpisodeMemory{ID: episodeID, UserID: user.ID, SessionID: sessionID, Title: "topic", Document: model.EpisodeDocument{Synopsis: "synopsis", Topics: []model.EpisodeTopic{{Key: "topic", Label: "topic"}}, SourceHandles: []string{source}}, SearchText: "topic synopsis", TopicKeys: model.StringList{"topic"}, SourceLastMessageID: 1, LastMessageAt: now, Version: 1, Status: model.EpisodeStatusActive, IndexStatus: model.EpisodeIndexIndexed},
		&model.EpisodeProjectionJob{ID: uuid.NewString(), UserID: user.ID, SessionID: sessionID, DesiredMessageID: 1, Status: model.EpisodeProjectionPending, AvailableAt: now},
	}
	for _, row := range rows {
		if err = db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	jobID, candidateID, dreamID, draftID, retainedMemoryID := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	otherJobID := uuid.NewString()
	memoryRows := []any{
		&model.MemoryExtractionJob{ID: jobID, UserID: user.ID, EventType: model.MemoryEventAgentRun, SourceID: runID, SourceSessionID: sessionID, SourceRunID: runID, Status: model.MemoryJobProcessing, AvailableAt: now},
		&model.MemoryCandidate{ID: candidateID, JobID: jobID, UserID: user.ID, Scope: model.MemoryScopeGlobal, Subject: "pending", ProposedContent: "derived from deleted session", ContentHash: model.MemoryContentHash("derived from deleted session"), StructuredJSON: `{}`, Tags: model.StringList{}, Durability: model.MemoryDurabilityStable, SourceRefs: model.StringList{"agent_run:" + runID}, Confidence: .9, Importance: 7, Sensitivity: model.MemorySensitivitySafe, Status: model.MemoryCandidateProcessing, DreamRunID: dreamID},
		&model.MemoryDreamRun{ID: dreamID, UserID: user.ID, CandidateID: candidateID, Status: model.MemoryDreamRunProcessing, StartedAt: now},
		&model.MemoryDraft{ID: draftID, UserID: user.ID, SessionID: sessionID, RunID: runID, IdempotencyKey: "delete-draft:" + draftID, Scope: model.MemoryScopeGlobal, Subject: "draft", Content: "pending confirmation", ContentHash: model.MemoryContentHash("pending confirmation"), Tags: model.StringList{}, Durability: model.MemoryDurabilityStable, ConflictIDs: model.StringList{}, Status: model.MemoryDraftPending, CreatedAt: now, UpdatedAt: now},
		&model.LongTermMemory{ID: retainedMemoryID, UserID: user.ID, Scope: model.MemoryScopeGlobal, Subject: "confirmed", Content: "independent user memory", ContentHash: model.MemoryContentHash("independent user memory"), StructuredJSON: `{}`, Tags: model.StringList{}, Durability: model.MemoryDurabilityStable, SourceRefs: model.StringList{"user_api:test"}, Confidence: 1, Importance: 8, Status: model.LongTermMemoryActive, UserConfirmed: true, CreatedAt: now, UpdatedAt: now},
		&model.MemoryExtractionJob{ID: otherJobID, UserID: user.ID, EventType: model.MemoryEventSummaryCompacted, SourceID: "other-session:v1", SourceSessionID: "other-session", Status: model.MemoryJobPending, AvailableAt: now},
	}
	for _, row := range memoryRows {
		if err = db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	repository := NewRepository(db)
	if _, err = repository.DeleteOwned(ctx, user.ID+1, sessionID); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("foreign delete error=%v", err)
	}
	ids, err := repository.DeleteOwned(ctx, user.ID, sessionID)
	if err != nil || len(ids) != 1 || ids[0] != episodeID {
		t.Fatalf("ids=%v err=%v", ids, err)
	}
	for name, target := range map[string]any{
		"session": &model.Session{}, "message": &model.Message{}, "run": &model.AgentRun{}, "step": &model.AgentStep{},
		"summary": &model.SessionSummary{}, "episode": &model.EpisodeMemory{}, "job": &model.EpisodeProjectionJob{},
		"memory_job": &model.MemoryExtractionJob{}, "candidate": &model.MemoryCandidate{}, "dream": &model.MemoryDreamRun{}, "draft": &model.MemoryDraft{},
	} {
		var count int64
		query := db.Unscoped().Model(target)
		switch name {
		case "session":
			query = query.Where("id = ?", sessionID)
		case "step":
			query = query.Where("run_id = ?", runID)
		case "run":
			query = query.Where("id = ?", runID)
		case "memory_job":
			query = query.Where("id = ?", jobID)
		case "candidate":
			query = query.Where("id = ?", candidateID)
		case "dream":
			query = query.Where("id = ?", dreamID)
		case "draft":
			query = query.Where("id = ?", draftID)
		default:
			query = query.Where("session_id = ?", sessionID)
		}
		if err = query.Count(&count).Error; err != nil || count != 0 {
			t.Fatalf("%s survived delete: count=%d err=%v", name, count, err)
		}
	}
	var retained, otherJob int64
	if err = db.Model(&model.LongTermMemory{}).Where("id = ? AND user_id = ?", retainedMemoryID, user.ID).Count(&retained).Error; err != nil || retained != 1 {
		t.Fatalf("independent confirmed memory was deleted: count=%d err=%v", retained, err)
	}
	if err = db.Model(&model.MemoryExtractionJob{}).Where("id = ? AND user_id = ?", otherJobID, user.ID).Count(&otherJob).Error; err != nil || otherJob != 1 {
		t.Fatalf("another session's memory job was deleted: count=%d err=%v", otherJob, err)
	}
}
