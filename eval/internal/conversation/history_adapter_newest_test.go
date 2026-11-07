package conversation

import (
	"context"
	"fmt"
	"testing"

	messagedao "GopherAI/dao/message"
	"GopherAI/model"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// The harness reads the conversation back through the adapter, and the
// adapter has to reach the store's newest-first fetch.
//
// Pinning the method the adapter calls is not enough on its own: the
// direction is only worth anything if the method returns what it promises.
// This test goes through the real repository, with a conversation longer
// than the fetch limit, because that is the case where the two directions
// disagree: the chronological prefix returns the oldest slice above the
// watermark, and the newest-first fetch returns the newest slice.
//
// A window assembled from the oldest slice does not fail loudly. It carries
// the beginning of the conversation, drops the newest turns, and answers the
// question from the wrong end.

// daoHistoryStore is the guarded store's shape over the real repository.
type daoHistoryStore struct{ db *gorm.DB }

func (s daoHistoryStore) ListAfter(ctx context.Context, session string, id uint, limit int) ([]model.Message, error) {
	return messagedao.NewRepository(s.db).ListAfter(ctx, session, id, limit)
}

func (s daoHistoryStore) ListRecentAfter(ctx context.Context, session string, id uint, limit int) ([]model.Message, error) {
	return messagedao.NewRepository(s.db).ListRecentAfter(ctx, session, id, limit)
}

func TestTheAdapterReturnsTheNewestMessagesWhenTheTailIsLongerThanTheLimit(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.AutoMigrate(&model.Session{}, &model.Message{}); err != nil {
		t.Fatal(err)
	}
	const (
		session    = "session-1"
		messages   = 200
		fetchLimit = 128
	)
	if err = db.Create(&model.Session{ID: session, UserID: 7, UserName: "learner", Mode: model.SessionModeAgent}).Error; err != nil {
		t.Fatal(err)
	}
	for index := 1; index <= messages; index++ {
		row := model.Message{SessionID: session, UserName: "learner", IsUser: index%2 == 1, Content: fmt.Sprintf("turn %d", index)}
		if err = db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
	}

	adapter := historyAdapter{store: daoHistoryStore{db: db}, sessionID: session}
	returned, err := adapter.ListRecentAfter(context.Background(), session, 0, fetchLimit)
	if err != nil {
		t.Fatalf("ListRecentAfter: %v", err)
	}
	if len(returned) != fetchLimit {
		t.Fatalf("returned %d messages, want %d", len(returned), fetchLimit)
	}
	// The newest slice above the watermark, oldest-first within it.
	if returned[0].ID != messages-fetchLimit+1 || returned[len(returned)-1].ID != messages {
		t.Fatalf("returned ids %d..%d, want %d..%d", returned[0].ID, returned[len(returned)-1].ID,
			messages-fetchLimit+1, messages)
	}
	// The last turn is the one a question about the latest state needs, and
	// the chronological prefix would have dropped it.
	if !containsContent(returned, "turn 200") {
		t.Fatal("the newest turn did not reach the window")
	}
	if containsContent(returned, "turn 1") {
		t.Fatal("the oldest turn reached the window, so the fetch is the prefix")
	}
}

func containsContent(rows []model.Message, want string) bool {
	for _, row := range rows {
		if row.Content == want {
			return true
		}
	}
	return false
}
