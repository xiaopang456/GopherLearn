package message

import (
	"context"
	"fmt"
	"testing"

	"GopherAI/internal/testdb"
	"GopherAI/model"

	"github.com/google/uuid"
	mysqldriver "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// The words-after read is bounded, ordered, and owner scoped.
//
// It is the read of the stretch a summary never covered, so it starts after a
// message id and walks forward; and it carries the same ownership boundary as
// the rest of the repository, because a tail is still somebody's conversation.
func TestTheOwnedWordsAfterReadIsBoundedOrderedAndOwnerScoped(t *testing.T) {
	dsn := testdb.RequireMySQLDSN(t)
	db, err := gorm.Open(mysqldriver.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.User{}, &model.Session{}, &model.Message{}); err != nil {
		t.Fatal(err)
	}

	suffix := uuid.NewString()
	user := model.User{Username: fmt.Sprintf("after-%s", suffix[:8]), Password: "secret"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	session := model.Session{ID: uuid.NewString(), UserID: user.ID, UserName: user.Username,
		Mode: model.SessionModeAgent, Title: "after test"}
	if err := db.Create(&session).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Unscoped().Where("session_id = ?", session.ID).Delete(&model.Message{})
		db.Unscoped().Delete(&session)
		db.Unscoped().Delete(&user)
	})

	ids := make([]uint, 0, 5)
	for index := 1; index <= 5; index++ {
		row := model.Message{SessionID: session.ID, UserName: user.Username,
			Content: fmt.Sprintf("第 %d 条原话", index), IsUser: index%2 == 1}
		if err := db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
		ids = append(ids, row.ID)
	}

	got, err := NewRepository(db).ListOwnedWordsAfter(context.Background(), user.ID, session.ID, ids[1], 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("the read returned %d words, want the bounded 2", len(got))
	}
	if got[0].ID != ids[2] || got[1].ID != ids[3] {
		t.Fatalf("the read returned %d,%d, want %d,%d after %d",
			got[0].ID, got[1].ID, ids[2], ids[3], ids[1])
	}

	// Another account cannot read this conversation's tail.
	foreign, err := NewRepository(db).ListOwnedWordsAfter(context.Background(), user.ID+1, session.ID, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(foreign) != 0 {
		t.Fatalf("another owner read %d words of this conversation", len(foreign))
	}
	// The owner cannot read another conversation's tail by naming its id.
	otherSession := model.Session{ID: uuid.NewString(), UserID: user.ID, UserName: user.Username,
		Mode: model.SessionModeAgent, Title: "other"}
	if err := db.Create(&otherSession).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Unscoped().Delete(&otherSession) })
	other := model.Message{SessionID: otherSession.ID, UserName: user.Username, Content: "别的会话"}
	if err := db.Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Unscoped().Delete(&other) })
	mixed, err := NewRepository(db).ListOwnedWordsAfter(context.Background(), user.ID, otherSession.ID, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(mixed) != 1 || mixed[0].Content != "别的会话" {
		t.Fatalf("the read crossed conversations: %+v", mixed)
	}
}
