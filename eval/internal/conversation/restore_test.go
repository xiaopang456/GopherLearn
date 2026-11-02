package conversation

import (
	"GopherAI/model"
	"context"
	"errors"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"testing"
)

func TestRestoreRequiresEmptyTablesAndIsAtomic(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.AutoMigrate(&model.User{}, &model.Session{}, &model.Message{}, &model.SessionSummary{}); err != nil {
		t.Fatal(err)
	}
	if err = db.Create(&model.User{ID: 7, Username: "learner"}).Error; err != nil {
		t.Fatal(err)
	}
	s := validSnapshot()
	if err = restoreIntoEmpty(context.Background(), db, s); err != nil {
		t.Fatal(err)
	}
	got, err := Capture(context.Background(), db, s.Session.UserID, s.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Messages[0].ID != 1 || got.Summary.CoveredMessageID != 2 {
		t.Fatal("message ids changed")
	}
	if err = restoreIntoEmpty(context.Background(), db, s); err == nil {
		t.Fatal("accepted a nonempty target")
	}
	var count int64
	if err = db.Model(&model.Message{}).Count(&count).Error; err != nil || count != 2 {
		t.Fatalf("count=%d err=%v", count, err)
	}
}

func TestRestoreRejectsDifferentUser(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.AutoMigrate(&model.User{}, &model.Session{}, &model.Message{}, &model.SessionSummary{}); err != nil {
		t.Fatal(err)
	}
	if err = db.Create(&model.User{ID: 7, Username: "other"}).Error; err != nil {
		t.Fatal(err)
	}
	if err = restoreIntoEmpty(context.Background(), db, validSnapshot()); err == nil {
		t.Fatal("accepted wrong user")
	}
	var count int64
	if err = db.Model(&model.Session{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("count=%d error=%v", count, err)
	}
}

func TestRestoreRollsBackOnInsertFailure(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.AutoMigrate(&model.User{}, &model.Session{}, &model.Message{}, &model.SessionSummary{}); err != nil {
		t.Fatal(err)
	}
	if err = db.Create(&model.User{ID: 7, Username: "learner"}).Error; err != nil {
		t.Fatal(err)
	}
	if err = db.Callback().Create().Before("gorm:create").Register("fail-messages", func(tx *gorm.DB) {
		if tx.Statement.Table == "messages" {
			tx.AddError(errors.New("injected"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err = restoreIntoEmpty(context.Background(), db, validSnapshot()); err == nil {
		t.Fatal("expected injected error")
	}
	var count int64
	if err = db.Model(&model.Session{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("count=%d error=%v", count, err)
	}
}
