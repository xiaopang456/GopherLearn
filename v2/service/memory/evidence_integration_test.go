package memory

import (
	"GopherAI/internal/testdb"
	"GopherAI/model"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	mysqldriver "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestDatabaseEvidenceLoaderRejectsMalformedSummaryBeforeExtraction(t *testing.T) {
	dsn := testdb.RequireMySQLDSN(t)
	db, err := gorm.Open(mysqldriver.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.Session{}); err != nil {
		t.Fatal(err)
	}
	session := &model.Session{
		ID: uuid.NewString(), UserID: time.Now().UnixNano(), UserName: "summary-evidence-test",
		Mode: model.SessionModeAgent, Title: "summary evidence",
	}
	if err := db.Create(session).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Unscoped().Where("id = ?", session.ID).Delete(&model.Session{})
	})
	loader, err := NewDatabaseEvidenceLoader(db)
	if err != nil {
		t.Fatal(err)
	}
	job := &model.MemoryExtractionJob{
		UserID: session.UserID, EventType: model.MemoryEventSummaryCompacted,
		SourceID: session.ID + ":v1", SourceSessionID: session.ID,
		EventPayload: `{"current_goal":"temporary",`,
	}
	if evidence, err := loader.LoadEvidence(context.Background(), job); !errors.Is(err, ErrInvalidSummaryEvidence) || evidence.Content != "" {
		t.Fatalf("malformed evidence=%+v err=%v", evidence, err)
	}
	job.EventPayload = `{"current_goal":"temporary","open_loops":["later"],"confirmed_decisions":["Body 使用 Protobuf"],"user_corrections":[],"key_errors":[],"references":["message:1"]}`
	evidence, err := loader.LoadEvidence(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(evidence.Content, "current_goal") || strings.Contains(evidence.Content, "open_loops") || !strings.Contains(evidence.Content, "Body 使用 Protobuf") {
		t.Fatalf("sanitized evidence=%s", evidence.Content)
	}
}
