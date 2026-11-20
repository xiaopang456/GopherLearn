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

// The keyword search returns the NEWEST matching messages first.
//
// A conversation settles its arrangements at its end, so the newest match is
// the one that answers "what did we finally agree on" - and the oldest
// matches never reach the end of a long conversation at all.
func TestTheOwnedMessageSearchReturnsTheNewestMatchesFirst(t *testing.T) {
	dsn := testdb.RequireMySQLDSN(t)
	db, err := gorm.Open(mysqldriver.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.User{}, &model.Session{}, &model.Message{}); err != nil {
		t.Fatal(err)
	}

	suffix := uuid.NewString()
	user := model.User{Username: fmt.Sprintf("search-%s", suffix[:8]), Password: "secret"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	session := model.Session{ID: uuid.NewString(), UserID: user.ID, UserName: user.Username,
		Mode: model.SessionModeAgent, Title: "search test"}
	if err := db.Create(&session).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Unscoped().Where("session_id = ?", session.ID).Delete(&model.Message{})
		db.Unscoped().Delete(&session)
		db.Unscoped().Delete(&user)
	})
	// Five messages that all match the keyword, and one that does not.
	for i := 1; i <= 5; i++ {
		row := model.Message{SessionID: session.ID, UserName: user.Username,
			Content: fmt.Sprintf("第 %d 轮 每次 37 轮", i), IsUser: i%2 == 1}
		if err := db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
	}
	quiet := model.Message{SessionID: session.ID, UserName: user.Username,
		Content: "这一条不含关键词", IsUser: true}
	if err := db.Create(&quiet).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Unscoped().Delete(&quiet) })

	got, err := NewRepository(db).SearchOwnedMessagesBySession(context.Background(),
		user.ID, session.ID, "37", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("the search returned %d messages, want 3", len(got))
	}
	// The newest three of the five matches, newest first.
	if got[0].ID <= got[1].ID || got[1].ID <= got[2].ID {
		t.Fatalf("the matches are not newest first: %d, %d, %d", got[0].ID, got[1].ID, got[2].ID)
	}
	for _, row := range got {
		if row.Content == quiet.Content {
			t.Fatalf("a message that does not match was returned: %+v", row)
		}
	}
	// Another owner's session is not reachable through the same keyword.
	foreign, err := NewRepository(db).SearchOwnedMessagesBySession(context.Background(),
		user.ID+1, session.ID, "37", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(foreign) != 0 {
		t.Fatalf("another owner reached %d messages", len(foreign))
	}
}
