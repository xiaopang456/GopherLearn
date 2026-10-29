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

// ReadOwnedMessagesBySession exists because a read-back grant names a session as
// well as an id: summary evidence lives in the session being answered, episode
// evidence in the session the episode describes. Owner-plus-id is a wider door
// than that, and every one of the rows behind it belongs to the same learner, so
// this is the test that proves the query binds both.
//
// It needs a real MySQL and skips without one, like the rest of this package. The
// boundaries it checks cannot be checked against an in-memory stand-in, because
// the thing under test is the SQL.
func TestReadOwnedMessagesBySessionBindsOwnerAndSession(t *testing.T) {
	dsn := testdb.RequireMySQLDSN(t)
	db, err := gorm.Open(mysqldriver.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.User{}, &model.Session{}, &model.Message{}); err != nil {
		t.Fatal(err)
	}

	suffix := uuid.NewString()
	owner := model.User{Username: fmt.Sprintf("own-%s", suffix[:8]), Password: "secret"}
	if err := db.Create(&owner).Error; err != nil {
		t.Fatal(err)
	}
	stranger := model.User{Username: fmt.Sprintf("str-%s", suffix[:8]), Password: "secret"}
	if err := db.Create(&stranger).Error; err != nil {
		t.Fatal(err)
	}
	newSession := func(user *model.User, title string) model.Session {
		session := model.Session{ID: uuid.NewString(), UserID: user.ID, UserName: user.Username,
			Mode: model.SessionModeAgent, Title: title}
		if err := db.Create(&session).Error; err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			db.Unscoped().Where("session_id = ?", session.ID).Delete(&model.Message{})
			db.Unscoped().Delete(&session)
		})
		return session
	}
	write := func(session model.Session, user *model.User, content string, isUser bool) model.Message {
		message := model.Message{SessionID: session.ID, UserName: user.Username, Content: content, IsUser: isUser}
		if err := db.Create(&message).Error; err != nil {
			t.Fatal(err)
		}
		return message
	}

	// Two sessions of one account, plus a row in another account's session that
	// the same ids could otherwise reach.
	current := newSession(&owner, "current")
	earlier := newSession(&owner, "earlier")
	foreign := newSession(&stranger, "foreign")
	defer func() {
		db.Unscoped().Where("session_id = ?", foreign.ID).Delete(&model.Message{})
		db.Unscoped().Delete(&foreign)
		db.Unscoped().Delete(&owner)
		db.Unscoped().Delete(&stranger)
	}()

	currentMessage := write(current, &owner, "本轮会话里的原话", true)
	earlierMessage := write(earlier, &owner, "上一个会话里的原话", true)
	assistantRow := write(current, &owner, "助手的回答", false)
	foreignMessage := write(foreign, &stranger, "别人的原话", true)

	repo := NewRepository(db)

	// One call, two sessions: each id comes back from the session that granted it.
	mixed, err := repo.ReadOwnedMessagesBySession(context.Background(), owner.ID, map[string][]uint{
		current.ID: {currentMessage.ID}, earlier.ID: {earlierMessage.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(mixed) != 2 || mixed[0].ID != currentMessage.ID || mixed[1].ID != earlierMessage.ID {
		t.Fatalf("mixed read = %#v", mixed)
	}
	for _, row := range mixed {
		if !row.IsUser {
			t.Fatalf("assistant row %d came back as learner words", row.ID)
		}
	}

	// The same id asked of the wrong session finds nothing. This is the case an
	// owner-only query would have served, because both rows are the same account's.
	miscounted, err := repo.ReadOwnedMessagesBySession(context.Background(), owner.ID, map[string][]uint{
		earlier.ID: {currentMessage.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(miscounted) != 0 {
		t.Fatalf("cross-session read returned %#v", miscounted)
	}

	// A stranger's session belongs to another account, so it yields nothing even
	// though the id is correct and the query is well-formed.
	crossAccount, err := repo.ReadOwnedMessagesBySession(context.Background(), owner.ID, map[string][]uint{
		foreign.ID: {foreignMessage.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(crossAccount) != 0 {
		t.Fatalf("cross-account read returned %#v", crossAccount)
	}
	// The owner's own two ids plus a foreign one still resolve to their own rows
	// only, and in a stable order.
	together, err := repo.ReadOwnedMessagesBySession(context.Background(), owner.ID, map[string][]uint{
		current.ID: {currentMessage.ID, assistantRow.ID}, foreign.ID: {foreignMessage.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(together) != 2 || together[0].ID > together[1].ID {
		t.Fatalf("combined read = %#v want two of the owner's rows in id order", together)
	}

	for name, scopes := range map[string]map[string][]uint{
		"empty map":      {},
		"blank session":  {"": {currentMessage.ID}},
		"empty id group": {current.ID: {}},
	} {
		if _, err := repo.ReadOwnedMessagesBySession(context.Background(), owner.ID, scopes); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
	if _, err := repo.ReadOwnedMessagesBySession(context.Background(), 0,
		map[string][]uint{current.ID: {currentMessage.ID}}); err == nil {
		t.Fatal("an unowned read was accepted")
	}
	// A session that does not exist is not a malformed request; it is a request
	// that matches nothing, and the join is what says so.
	unknown, err := repo.ReadOwnedMessagesBySession(context.Background(), owner.ID,
		map[string][]uint{uuid.NewString(): {currentMessage.ID}})
	if err != nil || len(unknown) != 0 {
		t.Fatalf("unknown session = %#v error=%v", unknown, err)
	}
}
