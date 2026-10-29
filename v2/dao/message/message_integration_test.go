package message

import (
	"GopherAI/internal/testdb"
	"GopherAI/model"
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	mysqldriver "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestRepositoryListRecentIsBoundedAndChronological(t *testing.T) {
	dsn := testdb.RequireMySQLDSN(t)
	db, err := gorm.Open(mysqldriver.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.User{}, &model.Session{}, &model.Message{}); err != nil {
		t.Fatal(err)
	}

	suffix := uuid.NewString()
	user := model.User{Username: fmt.Sprintf("ctx-%s", suffix[:8]), Password: "secret"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	session := model.Session{ID: uuid.NewString(), UserID: user.ID, UserName: user.Username, Mode: model.SessionModeAgent, Title: "context test"}
	if err := db.Create(&session).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Unscoped().Where("session_id = ?", session.ID).Delete(&model.Message{})
		db.Unscoped().Delete(&session)
		db.Unscoped().Delete(&user)
	})
	for i := 1; i <= 5; i++ {
		message := model.Message{SessionID: session.ID, UserName: user.Username, Content: fmt.Sprintf("message-%d", i), IsUser: i%2 == 1}
		if err := db.Create(&message).Error; err != nil {
			t.Fatal(err)
		}
	}
	other := model.Message{SessionID: uuid.NewString(), UserName: user.Username, Content: "other-session", IsUser: true}
	if err := db.Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Unscoped().Delete(&other) })

	got, err := NewRepository(db).ListRecent(context.Background(), session.ID, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Content != "message-3" || got[1].Content != "message-4" || got[2].Content != "message-5" {
		t.Fatalf("recent messages = %#v", got)
	}
	after, err := NewRepository(db).ListRecentAfter(context.Background(), session.ID, got[0].ID, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 2 || after[0].Content != "message-4" || after[1].Content != "message-5" {
		t.Fatalf("recent-after messages = %#v", after)
	}
	prefix, err := NewRepository(db).ListAfter(context.Background(), session.ID, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(prefix) != 2 || prefix[0].Content != "message-1" || prefix[1].Content != "message-2" {
		t.Fatalf("chronological prefix = %#v", prefix)
	}
	recent, count, err := NewRepository(db).ListRecentWithCount(context.Background(), session.ID, 3)
	if err != nil || count != 5 || len(recent) != 3 {
		t.Fatalf("recent with count messages=%#v count=%d error=%v", recent, count, err)
	}
	page1, err := NewRepository(db).ListOwnedHistoryPage(context.Background(), user.ID, session.ID, 0, 2)
	if err != nil || !page1.HasMore || page1.NextBeforeID != page1.Messages[0].ID || page1.Messages[0].Content != "message-4" || page1.Messages[1].Content != "message-5" {
		t.Fatalf("history page 1 = %#v error=%v", page1, err)
	}
	page2, err := NewRepository(db).ListOwnedHistoryPage(context.Background(), user.ID, session.ID, page1.NextBeforeID, 2)
	if err != nil || !page2.HasMore || page2.Messages[0].Content != "message-2" || page2.Messages[1].Content != "message-3" {
		t.Fatalf("history page 2 = %#v error=%v", page2, err)
	}
	foreignPage, err := NewRepository(db).ListOwnedHistoryPage(context.Background(), user.ID+1, session.ID, 0, 2)
	if err != nil || len(foreignPage.Messages) != 0 {
		t.Fatalf("foreign history = %#v error=%v", foreignPage, err)
	}

	persistenceKey := uuid.NewString()
	firstDelivery := model.Message{PersistenceKey: &persistenceKey, SessionID: session.ID, UserName: user.Username, Content: "once", IsUser: true}
	if err := NewRepository(db).CreateIdempotent(context.Background(), &firstDelivery); err != nil {
		t.Fatal(err)
	}
	redelivery := model.Message{PersistenceKey: &persistenceKey, SessionID: session.ID, UserName: user.Username, Content: "must-not-overwrite", IsUser: true}
	if err := NewRepository(db).CreateIdempotent(context.Background(), &redelivery); err != nil {
		t.Fatal(err)
	}
	var idempotentRows []model.Message
	if err := db.Where("persistence_key = ?", persistenceKey).Find(&idempotentRows).Error; err != nil {
		t.Fatal(err)
	}
	if len(idempotentRows) != 1 || idempotentRows[0].Content != "once" {
		t.Fatalf("idempotent rows = %#v", idempotentRows)
	}
}
