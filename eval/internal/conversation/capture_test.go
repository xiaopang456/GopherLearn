package conversation

import (
	"GopherAI/model"
	"context"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"testing"
)

func TestCaptureReadsOnlyOwnedSession(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.AutoMigrate(&model.Session{}, &model.Message{}, &model.SessionSummary{}); err != nil {
		t.Fatal(err)
	}
	s := validSnapshot()
	if err = db.Create(&s.Sessions[0].Session).Error; err != nil {
		t.Fatal(err)
	}
	if err = db.Create(&s.Sessions[0].Messages).Error; err != nil {
		t.Fatal(err)
	}
	if err = db.Omit("Session").Create(s.Sessions[0].Summary).Error; err != nil {
		t.Fatal(err)
	}
	got, err := Capture(context.Background(), db, 7, s.Sessions[0].Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Sessions[0].Messages) != 2 || got.Sessions[0].Summary == nil || got.Sessions[0].Summary.CoveredMessageID != 2 {
		t.Fatalf("bad capture: %+v", got)
	}
	if _, err = Capture(context.Background(), db, 8, s.Sessions[0].Session.ID); err == nil {
		t.Fatal("foreign user accepted")
	}
}
