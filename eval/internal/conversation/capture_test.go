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
	if err = db.Create(&s.Session).Error; err != nil {
		t.Fatal(err)
	}
	if err = db.Create(&s.Messages).Error; err != nil {
		t.Fatal(err)
	}
	if err = db.Omit("Session").Create(s.Summary).Error; err != nil {
		t.Fatal(err)
	}
	got, err := Capture(context.Background(), db, 7, s.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 2 || got.Summary == nil || got.Summary.CoveredMessageID != 2 {
		t.Fatalf("bad capture: %+v", got)
	}
	if _, err = Capture(context.Background(), db, 8, s.Session.ID); err == nil {
		t.Fatal("foreign user accepted")
	}
}
