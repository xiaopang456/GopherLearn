package hybrid

import (
	messageDAO "GopherAI/dao/message"
	sessionDAO "GopherAI/dao/session"
	"GopherAI/model"
	"context"
	"errors"
	"github.com/xiaopang456/GopherLearn/conversation"
	"gorm.io/gorm"
	"testing"
	"time"
)

func touchFixture(t *testing.T) (*Store, *gorm.DB, time.Time) {
	t.Helper()
	db := sqliteDatabase(t)
	t.Cleanup(func() {
		sqlDB, err := db.DB()
		if err != nil {
			t.Error(err)
			return
		}
		if err = sqlDB.Close(); err != nil {
			t.Error(err)
		}
	})
	fixed := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	if err := db.Create(&model.Session{ID: "touch-session", UserID: 1, UserName: "learner", Mode: model.SessionModeNormal, CreatedAt: fixed, UpdatedAt: fixed}).Error; err != nil {
		t.Fatal(err)
	}
	return &Store{db: db}, db, fixed
}
func rawMessageCount(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	var count int64
	if err := db.Model(&model.Message{}).Where("session_id = ?", "touch-session").Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	return count
}

// SQLite does not emulate MySQL changed-row reporting. This callback injects
// only the zero count AFTER the real SQL update, leaving all original DAOs real.
func TestAppendMessageUnchangedTouchWithZeroChangedRowsCommits(t *testing.T) {
	store, db, fixed := touchFixture(t)
	touches := 0
	if err := db.Callback().Update().After("gorm:update").Register("test:zero-changed-touch", func(tx *gorm.DB) {
		if tx.Statement.Table == "sessions" && tx.Error == nil {
			touches++
			tx.RowsAffected = 0
		}
	}); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"user", "assistant"} {
		got, err := store.AppendMessage(context.Background(), "1", "touch-session", &conversation.Message{Role: role, Content: role, CreatedAt: fixed})
		if err != nil || got == nil || got.ID == "" {
			t.Fatalf("unchanged timestamp must commit: %+v %v", got, err)
		}
	}
	page, err := messageDAO.NewRepository(db).ListOwnedHistoryPage(context.Background(), 1, "touch-session", 0, 100)
	if err != nil || len(page.Messages) != 2 || !page.Messages[0].IsUser || page.Messages[1].IsUser || touches != 2 {
		t.Fatalf("original DAO records=%+v touches=%d err=%v", page, touches, err)
	}
	session, err := sessionDAO.NewRepository(db).GetByIDAndUserID(context.Background(), "touch-session", 1)
	if err != nil || !session.UpdatedAt.Equal(fixed) {
		t.Fatalf("timestamp changed: %+v %v", session, err)
	}
}

func TestAppendMessageInvalidSessionLeavesNoMessage(t *testing.T) {
	for _, when := range []string{"before-append", "after-message-before-touch"} {
		for _, state := range []string{"missing", "deleted", "foreign", "mode"} {
			t.Run(when+"/"+state, func(t *testing.T) {
				store, db, fixed := touchFixture(t)
				invalidate := func(tx *gorm.DB) error {
					switch state {
					case "missing":
						return tx.Unscoped().Where("id = ?", "touch-session").Delete(&model.Session{}).Error
					case "deleted":
						return tx.Where("id = ?", "touch-session").Delete(&model.Session{}).Error
					case "foreign":
						return tx.Model(&model.Session{}).Where("id = ?", "touch-session").UpdateColumn("user_id", 2).Error
					default:
						return tx.Model(&model.Session{}).Where("id = ?", "touch-session").UpdateColumn("mode", model.SessionModeAgent).Error
					}
				}
				touchCalls := 0
				if when == "before-append" {
					if err := invalidate(db); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := db.Callback().Create().After("gorm:create").Register("test:invalidate-after-message", func(tx *gorm.DB) {
						if _, ok := tx.Statement.Dest.(*model.Message); ok && tx.Error == nil {
							// The injected state change belongs to the SAME append transaction.
							tx.AddError(invalidate(tx.Session(&gorm.Session{NewDB: true})))
						}
					}); err != nil {
						t.Fatal(err)
					}
				}
				if err := db.Callback().Update().After("gorm:update").Register("test:observe-invalid-touch", func(tx *gorm.DB) {
					if tx.Statement.Table == "sessions" && tx.Statement.SQL.String() != "" && tx.Statement.Dest != nil {
						if values, ok := tx.Statement.Dest.(map[string]interface{}); ok {
							if _, ok := values["updated_at"]; ok {
								touchCalls++
								if tx.RowsAffected != 0 {
									t.Errorf("invalid touch affected %d rows", tx.RowsAffected)
								}
							}
						}
					}
				}); err != nil {
					t.Fatal(err)
				}
				got, err := store.AppendMessage(context.Background(), "1", "touch-session", &conversation.Message{Role: "user", Content: "must roll back", CreatedAt: fixed})
				want := conversation.ErrNotFound
				if state == "mode" {
					want = conversation.ErrInvalidArgument
				}
				if got != nil || !errors.Is(err, want) || rawMessageCount(t, db) != 0 {
					t.Fatalf("invalid session accepted/residue: result=%+v err=%v count=%d", got, err, rawMessageCount(t, db))
				}
				if when == "after-message-before-touch" {
					if touchCalls != 1 {
						t.Fatalf("zero update branch was not exercised: %d", touchCalls)
					}
					row, err := sessionDAO.NewRepository(db).GetByIDAndUserID(context.Background(), "touch-session", 1)
					if err != nil || row.Mode != model.SessionModeNormal {
						t.Fatalf("same transaction state not rolled back: %+v %v", row, err)
					}
				}
			})
		}
	}
}

func TestAppendMessageTouchWriteErrorRollsBackMessage(t *testing.T) {
	store, db, fixed := touchFixture(t)
	failure := errors.New("synthetic timestamp write error")
	if err := db.Callback().Update().Before("gorm:update").Register("test:touch-error", func(tx *gorm.DB) {
		if tx.Statement.Table == "sessions" {
			tx.AddError(failure)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if result, err := store.AppendMessage(context.Background(), "1", "touch-session", &conversation.Message{Role: "user", Content: "must roll back", CreatedAt: fixed.Add(time.Second)}); result != nil || !errors.Is(err, failure) {
		t.Fatalf("touch error identity lost: %+v %v", result, err)
	}
	row, err := sessionDAO.NewRepository(db).GetByIDAndUserID(context.Background(), "touch-session", 1)
	if err != nil || !row.UpdatedAt.Equal(fixed) || rawMessageCount(t, db) != 0 {
		t.Fatalf("touch error left facts: %+v %v count=%d", row, err, rawMessageCount(t, db))
	}
}
