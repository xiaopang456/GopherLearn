package agent

// The write path and the read path, on a real database.
//
// The unit tests prove what the generator returns. This test proves that the
// answer reaches the database and comes back readable: the real producer
// writes the segment and the board in one transaction, the real session
// repository reads them back, and the real renderer puts the value and the
// message it came from into the block the final request reads.
//
// The session is synthetic, like the one in the recovery tests: a weekly
// morning-reading plan set at 3 times a week. The rewrite is broken JSON, so
// the test also proves that the first usable answer survives it.
//
// The MySQL form is opt-in like the other integration tests: it needs
// GOPHERAI_MYSQL_TEST_DSN and skips without it. The SQLite form always runs
// and uses the same producer, repository and renderer, so the chain is
// exercised even when no MySQL is reachable.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	mysqldriver "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	sessiondao "GopherAI/dao/session"
	"GopherAI/internal/testdb"
	"GopherAI/model"
)

func TestTheSummaryAndTheBoardSurviveTheRealWriteAndRead(t *testing.T) {
	dsn := testdb.RequireMySQLDSN(t)
	db, err := gorm.Open(mysqldriver.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent), TranslateError: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	runSummaryWriteReadChain(t, db)
}

func TestTheSummaryAndTheBoardSurviveTheRealWriteAndReadOnSQLite(t *testing.T) {
	dsn := fmt.Sprintf("file:summary-chain-%s?mode=memory&cache=shared", uuid.NewString())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent), TranslateError: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	runSummaryWriteReadChain(t, db)
}

// runSummaryWriteReadChain is the whole chain: migrate, write through the
// real producer and repository, read back, render.
func runSummaryWriteReadChain(t *testing.T, db *gorm.DB) {
	t.Helper()
	if err := db.AutoMigrate(&model.User{}, &model.Session{}, &model.Message{},
		&model.SessionWhiteboard{}, &model.SessionSummarySegment{}); err != nil {
		t.Fatal(err)
	}
	suffix := uuid.NewString()
	user := model.User{Username: fmt.Sprintf("summary-chain-%s", suffix), Password: "secret"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	session := model.Session{
		ID: uuid.NewString(), UserID: user.ID, UserName: user.Username,
		Mode: model.SessionModeAgent, Title: "summary chain",
	}
	if err := db.Create(&session).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Unscoped().Where("session_id = ?", session.ID).Delete(&model.SessionWhiteboard{})
		db.Unscoped().Where("session_id = ?", session.ID).Delete(&model.SessionSummarySegment{})
		db.Unscoped().Where("session_id = ?", session.ID).Delete(&model.Message{})
		db.Unscoped().Delete(&session)
		db.Unscoped().Delete(&user)
	})

	// The database may hold other rows. The batch starts above the highest
	// message id so the evidence ids name the messages this test inserted
	// and nothing else.
	var highest uint
	if err := db.Model(&model.Message{}).Select("COALESCE(MAX(id), 0)").Scan(&highest).Error; err != nil {
		t.Fatal(err)
	}
	base := highest + 1000
	messages := syntheticBatchMessagesAt(base, 31, syntheticTurnsInitial)
	for index := range messages {
		messages[index].SessionID = session.ID
		messages[index].UserName = user.Username
	}
	if err := db.Create(&messages).Error; err != nil {
		t.Fatal(err)
	}
	startID := base + 1
	endID := base + uint(2*len(syntheticTurnsInitial))
	valueID := base + 5 // the initial value's user message

	answer := syntheticEvidenceAnswer(t, base, 31, syntheticTurnsInitial,
		"我把每周固定的晨读安排定名叫“晨读”，每周做 3 次。", "每周 3 次", "morning_reading_freq", valueID)
	stub := &recordingSegmentModel{answers: []string{answer, "this is not the JSON the generator asked for"}}
	generator, err := NewModelSegmentGenerator(stub, 8000, 2000)
	if err != nil {
		t.Fatal(err)
	}
	generator.UseAssistantContent(true)

	repository := sessiondao.NewRepository(db)
	producer, err := NewModelSummarySegmentProducer(repository, repository, generator,
		DefaultSummarySegmentProducerConfig())
	if err != nil {
		t.Fatal(err)
	}
	job := &model.SessionSummarySegmentJob{
		UserID: user.ID, SessionID: session.ID,
		StartMessageID: startID, BatchMessageID: endID, DesiredMessageID: endID,
	}
	outcome, err := producer.ProduceSegment(context.Background(), job)
	if err != nil {
		t.Fatalf("the real write path lost the batch: %v", err)
	}
	if outcome.SegmentSeq != 1 {
		t.Fatalf("segment seq = %d, want 1", outcome.SegmentSeq)
	}
	if stub.calls != 2 {
		t.Fatalf("the model was asked %d times, want 2", stub.calls)
	}

	// The read path starts here: the rows come back from the repository, not
	// from the generator's result.
	stored, err := repository.GetLatestSummarySegment(context.Background(), user.ID, session.ID)
	if err != nil {
		t.Fatalf("the stored segment cannot be read: %v", err)
	}
	if stored.StartMessageID != startID || stored.EndMessageID != endID {
		t.Fatalf("the stored range = %d-%d, want %d-%d",
			stored.StartMessageID, stored.EndMessageID, startID, endID)
	}
	content, err := model.DecodeSessionSummarySegmentContent(stored.Content)
	if err != nil {
		t.Fatalf("the stored segment body cannot be decoded: %v", err)
	}
	if !segmentContentCarriesValue(content, "3 次", valueID) {
		t.Fatalf("the stored segment does not carry the initial value and its message: %+v", content.Points)
	}

	board, err := repository.GetWhiteboard(context.Background(), user.ID, session.ID)
	if err != nil {
		t.Fatalf("the stored board cannot be read: %v", err)
	}
	boardDocument, err := model.DecodeSessionWhiteboardDocument(board.Content)
	if err != nil {
		t.Fatalf("the stored board body cannot be decoded: %v", err)
	}
	if !whiteboardCarriesValue(boardDocument, "3 次", valueID) {
		t.Fatalf("the stored board does not carry the initial value and its message: %+v",
			boardDocument.Constraints)
	}

	// The renderer is the one the context builder calls. Both the value and
	// the message it came from have to be in the block.
	block := renderSummarySegments([]model.SessionSummarySegment{*stored}, boardDocument,
		1000, DefaultAssistantSegmentTokens, true)
	if !strings.Contains(block.Text, "3 次") || !strings.Contains(block.Text,
		fmt.Sprintf("[message_ids=%d]", valueID)) {
		t.Fatalf("the final block does not carry the value and its message: %q", block.Text)
	}
	boardBlock := renderWhiteboardBlock(boardDocument, 1000)
	if !strings.Contains(boardBlock.Text, "3 次") ||
		!strings.Contains(boardBlock.Text, fmt.Sprintf("%d", valueID)) {
		t.Fatalf("the board block does not carry the value and its message: %q", boardBlock.Text)
	}
}

// segmentContentCarriesValue reports whether one of the points says the value
// and names the message it came from.
func segmentContentCarriesValue(content model.SessionSummarySegmentContent, value string, messageID uint) bool {
	for _, point := range content.Points {
		if !strings.Contains(point.Text, value) {
			continue
		}
		for _, id := range point.EvidenceMessageIDs {
			if id == messageID {
				return true
			}
		}
	}
	return false
}

// whiteboardCarriesValue reports whether one of the board's entries says the
// value and names the message it came from.
func whiteboardCarriesValue(document model.SessionWhiteboardDocument, value string, messageID uint) bool {
	entries := make([]model.SessionWhiteboardEntry, 0,
		len(document.Constraints)+len(document.Plan)+len(document.OpenItems))
	entries = append(entries, document.Constraints...)
	entries = append(entries, document.Plan...)
	entries = append(entries, document.OpenItems...)
	for _, entry := range entries {
		if !strings.Contains(entry.Text, value) && !strings.Contains(entry.Value, value) {
			continue
		}
		for _, id := range entry.SourceMessageIDs {
			if id == messageID {
				return true
			}
		}
	}
	return false
}
