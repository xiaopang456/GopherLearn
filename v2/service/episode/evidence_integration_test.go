package episode

import (
	"GopherAI/internal/testdb"
	"GopherAI/model"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	mysqldriver "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func openEvidenceTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := testdb.RequireMySQLDSN(t)
	db, err := gorm.Open(mysqldriver.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent), TranslateError: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.User{}, &model.Session{}, &model.Message{}, &model.SessionSummary{}, &model.EpisodeMemory{}, &model.EpisodeProjectionJob{}); err != nil {
		t.Fatal(err)
	}
	return db
}

func createEvidenceSession(t *testing.T, db *gorm.DB) (model.User, model.Session) {
	t.Helper()
	suffix := uuid.NewString()
	user := model.User{Username: fmt.Sprintf("ev-%s", suffix[:12]), Password: "secret"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	session := model.Session{ID: uuid.NewString(), UserID: user.ID, UserName: user.Username, Mode: model.SessionModeAgent, Title: "evidence"}
	if err := db.Create(&session).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Unscoped().Where("session_id = ?", session.ID).Delete(&model.EpisodeProjectionJob{})
		db.Unscoped().Where("session_id = ?", session.ID).Delete(&model.EpisodeMemory{})
		db.Unscoped().Where("session_id = ?", session.ID).Delete(&model.SessionSummary{})
		db.Unscoped().Where("session_id = ?", session.ID).Delete(&model.Message{})
		db.Unscoped().Delete(&session)
		db.Unscoped().Delete(&user)
	})
	return user, session
}

func createEvidenceMessages(t *testing.T, db *gorm.DB, session model.Session, count int) []model.Message {
	t.Helper()
	messages := make([]model.Message, 0, count)
	for index := 0; index < count; index++ {
		message := model.Message{SessionID: session.ID, UserName: session.UserName, IsUser: index%2 == 0, Content: fmt.Sprintf("message-%d", index+1), CreatedAt: time.Now().Add(time.Duration(index) * time.Millisecond)}
		if err := db.Create(&message).Error; err != nil {
			t.Fatal(err)
		}
		messages = append(messages, message)
	}
	return messages
}

func TestDatabaseEvidenceLoaderShortAndSummarizedSessions(t *testing.T) {
	db := openEvidenceTestDB(t)
	user, session := createEvidenceSession(t, db)
	messages := createEvidenceMessages(t, db, session, 4)
	loader, err := NewDatabaseEvidenceLoader(db, 128)
	if err != nil {
		t.Fatal(err)
	}

	short, err := loader.Load(context.Background(), user.ID, session.ID, messages[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	if short.Summary != nil || len(short.Messages) != 2 || short.Messages[1].ID != messages[1].ID || short.TargetMessageAt.IsZero() {
		t.Fatalf("short=%#v", short)
	}

	summary := model.SessionSummary{UserID: user.ID, SessionID: session.ID, Content: `{"current_goal":"排查 RPC"}`, CoveredMessageID: messages[1].ID, TokenEstimate: 20, Version: 2}
	if err := db.Create(&summary).Error; err != nil {
		t.Fatal(err)
	}
	long, err := loader.Load(context.Background(), user.ID, session.ID, messages[3].ID)
	if err != nil {
		t.Fatal(err)
	}
	if long.Summary == nil || long.Summary.Version != 2 || len(long.Messages) != 2 || long.Messages[0].ID != messages[2].ID || long.Messages[1].ID != messages[3].ID {
		t.Fatalf("long=%#v", long)
	}

	if err := db.Model(&model.SessionSummary{}).Where("id = ?", summary.ID).Update("content", fmt.Sprintf(`{"schema_version":2,"confirmed_decisions":[{"text":"使用长度前缀","evidence_message_ids":[%d]}]}`, messages[0].ID)).Error; err != nil {
		t.Fatal(err)
	}
	withEvidence, err := loader.Load(context.Background(), user.ID, session.ID, messages[3].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(withEvidence.SummaryMessages) != 1 || withEvidence.SummaryMessages[0].ID != messages[0].ID || !withEvidence.SummaryMessages[0].IsUser {
		t.Fatalf("summary messages=%#v", withEvidence.SummaryMessages)
	}

	// A summary newer than the worker's frozen target must not leak future
	// messages into this projection. The loader falls back to complete rows.
	older, err := loader.Load(context.Background(), user.ID, session.ID, messages[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if older.Summary != nil || len(older.Messages) != 1 || older.Messages[0].ID != messages[0].ID {
		t.Fatalf("older=%#v", older)
	}
}
