package session

import (
	"GopherAI/internal/testdb"
	"GopherAI/model"
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	mysqldriver "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestSummaryRepositoryOptimisticUpdateOwnershipAndCascade(t *testing.T) {
	dsn := testdb.RequireMySQLDSN(t)
	db, err := gorm.Open(mysqldriver.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent), TranslateError: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.User{}, &model.Session{}, &model.SessionSummary{}); err != nil {
		t.Fatal(err)
	}
	suffix := uuid.NewString()
	user := model.User{Username: fmt.Sprintf("summary-%s", suffix), Password: "secret"}
	other := model.User{Username: fmt.Sprintf("summary-other-%s", suffix), Password: "secret"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	session := model.Session{ID: uuid.NewString(), UserID: user.ID, UserName: user.Username, Mode: model.SessionModeAgent, Title: "summary"}
	if err := db.Create(&session).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Unscoped().Where("session_id = ?", session.ID).Delete(&model.SessionSummary{})
		db.Unscoped().Delete(&session)
		db.Unscoped().Delete(&other)
		db.Unscoped().Delete(&user)
	})

	repository := NewRepository(db)
	foreign := &model.SessionSummary{UserID: other.ID, SessionID: session.ID, Content: "cross-user", CoveredMessageID: 1}
	if err := repository.SaveSummary(context.Background(), foreign, 0); !errors.Is(err, ErrSummaryNotFound) {
		t.Fatalf("cross-user create error=%v", err)
	}
	first := &model.SessionSummary{UserID: user.ID, SessionID: session.ID, Content: `{"current_goal":"first"}`, CoveredMessageID: 10, TokenEstimate: 10}
	if err := repository.SaveSummary(context.Background(), first, 0); err != nil {
		t.Fatal(err)
	}
	if first.Version != 1 {
		t.Fatalf("initial version=%d", first.Version)
	}
	stale := &model.SessionSummary{UserID: user.ID, SessionID: session.ID, Content: "stale", CoveredMessageID: 11, TokenEstimate: 1}
	if err := repository.SaveSummary(context.Background(), stale, 0); !errors.Is(err, ErrSummaryConflict) {
		t.Fatalf("duplicate create error=%v", err)
	}
	updated := &model.SessionSummary{UserID: user.ID, SessionID: session.ID, Content: `{"current_goal":"updated"}`, CoveredMessageID: 20, TokenEstimate: 12}
	if err := repository.SaveSummary(context.Background(), updated, 1); err != nil {
		t.Fatal(err)
	}
	if err := repository.SaveSummary(context.Background(), &model.SessionSummary{UserID: user.ID, SessionID: session.ID, Content: "older", CoveredMessageID: 15}, 2); !errors.Is(err, ErrSummaryConflict) {
		t.Fatalf("non-monotonic error=%v", err)
	}
	if _, err := repository.GetSummary(context.Background(), other.ID, session.ID); !errors.Is(err, ErrSummaryNotFound) {
		t.Fatalf("cross-user read error=%v", err)
	}
	if err := db.Unscoped().Delete(&session).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := repository.GetSummary(context.Background(), user.ID, session.ID); !errors.Is(err, ErrSummaryNotFound) {
		t.Fatalf("hard-deleted session left summary: %v", err)
	}
}
