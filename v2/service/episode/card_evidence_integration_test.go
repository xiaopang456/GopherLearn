package episode

import (
	"GopherAI/internal/testdb"
	"GopherAI/model"
	"context"
	"fmt"
	"testing"
	"time"

	mysqldriver "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func openCardTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := testdb.RequireMySQLDSN(t)
	db, err := gorm.Open(mysqldriver.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent), TranslateError: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.User{}, &model.Session{}, &model.Message{},
		&model.SessionWhiteboard{}, &model.SessionSummarySegment{},
		&model.EpisodeMemory{}, &model.EpisodeProjectionJob{}); err != nil {
		t.Fatal(err)
	}
	return db
}

func createCardSession(t *testing.T, db *gorm.DB, suffix string) (model.User, model.Session) {
	t.Helper()
	user := model.User{Username: fmt.Sprintf("card-%s", suffix), Password: "secret"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	session := model.Session{ID: fmt.Sprintf("card-session-%s", suffix), UserID: user.ID,
		UserName: user.Username, Mode: model.SessionModeAgent, Title: "并发练习"}
	if err := db.Create(&session).Error; err != nil {
		t.Fatal(err)
	}
	return user, session
}

func createCardMessages(t *testing.T, db *gorm.DB, sessionID string, count int) []model.Message {
	t.Helper()
	now := time.Now()
	var rows []model.Message
	for index := 1; index <= count; index++ {
		row := model.Message{SessionID: sessionID, IsUser: index%2 == 1,
			Content: fmt.Sprintf("第%d句原话", index), CreatedAt: now.Add(time.Duration(index) * time.Second)}
		if err := db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
		rows = append(rows, row)
	}
	return rows
}

// The summary lags the conversation: the segments cover the first stretch and
// the tail is still raw. The card has to see both, and the part the summary
// missed must not be skipped.
func TestCardEvidenceCarriesTheTailTheSummaryHasNotCovered(t *testing.T) {
	db := openCardTestDB(t)
	user, session := createCardSession(t, db, "lag")
	messages := createCardMessages(t, db, session.ID, 5)
	segmentContent, err := model.MarshalSessionSummarySegmentContent(model.SessionSummarySegmentContent{
		SchemaVersion: model.SessionSummarySegmentSchemaV1, Topic: "题量",
		Points: []model.SessionSummarySegmentPoint{{Text: "每轮9道题", EvidenceMessageIDs: []uint{messages[0].ID}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	segment := model.SessionSummarySegment{
		UserID: user.ID, SessionID: session.ID, Seq: 1,
		StartMessageID: messages[0].ID, EndMessageID: messages[2].ID,
		StartAt: messages[0].CreatedAt, EndAt: messages[2].CreatedAt,
		Topic: "题量", Content: segmentContent, TokenEstimate: 100,
	}
	if err := db.Create(&segment).Error; err != nil {
		t.Fatal(err)
	}
	loader, err := NewDatabaseCardEvidenceLoader(db, 128)
	if err != nil {
		t.Fatal(err)
	}
	target := messages[4].ID
	evidence, err := loader.LoadCardEvidence(context.Background(), user.ID, session.ID, target)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.SummaryWatermark != messages[2].ID {
		t.Fatalf("the summary reaches %d, want %d", evidence.SummaryWatermark, messages[2].ID)
	}
	if len(evidence.Segments) != 1 {
		t.Fatalf("the loader saw %d segments", len(evidence.Segments))
	}
	if len(evidence.Messages) != 2 {
		t.Fatalf("the loader carried %d raw messages, want the two the summary missed", len(evidence.Messages))
	}
	if evidence.Messages[0].ID != messages[3].ID {
		t.Fatalf("the tail starts at %d", evidence.Messages[0].ID)
	}
	known := evidence.KnownMessageIDs()
	if _, exists := known[messages[0].ID]; !exists {
		t.Fatal("the message the segment was compressed from is not citable")
	}
	if _, exists := known[messages[4].ID]; !exists {
		t.Fatal("the raw tail is not citable")
	}
}

// A segment that cannot be decoded is a gap in the compressed state.
//
// The watermark says how far the compressed state has been read. Raising it
// past a segment that could not be read would make the card claim a coverage
// it does not have, and the tail of raw messages would start above the gap
// and skip it entirely - so the stretch would be neither summarised nor
// shown.
func TestCardEvidenceStopsTheWatermarkAtAnUnreadableSegment(t *testing.T) {
	db := openCardTestDB(t)
	user, session := createCardSession(t, db, "unreadable")
	messages := createCardMessages(t, db, session.ID, 5)
	goodContent, err := model.MarshalSessionSummarySegmentContent(model.SessionSummarySegmentContent{
		SchemaVersion: model.SessionSummarySegmentSchemaV1, Topic: "第一段",
		Points: []model.SessionSummarySegmentPoint{{Text: "每轮9道题", EvidenceMessageIDs: []uint{messages[0].ID}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	good := model.SessionSummarySegment{
		UserID: user.ID, SessionID: session.ID, Seq: 1,
		StartMessageID: messages[0].ID, EndMessageID: messages[0].ID,
		StartAt: messages[0].CreatedAt, EndAt: messages[0].CreatedAt,
		Topic: "第一段", Content: goodContent, TokenEstimate: 100,
	}
	broken := model.SessionSummarySegment{
		UserID: user.ID, SessionID: session.ID, Seq: 2,
		StartMessageID: messages[1].ID, EndMessageID: messages[2].ID,
		StartAt: messages[1].CreatedAt, EndAt: messages[2].CreatedAt,
		Topic: "读不出来的一段", Content: "{not json", TokenEstimate: 100,
	}
	if err := db.Create(&good).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&broken).Error; err != nil {
		t.Fatal(err)
	}
	loader, err := NewDatabaseCardEvidenceLoader(db, 128)
	if err != nil {
		t.Fatal(err)
	}
	target := messages[4].ID
	evidence, err := loader.LoadCardEvidence(context.Background(), user.ID, session.ID, target)
	if err != nil {
		t.Fatal(err)
	}
	// The watermark stops at the segment that could be read.
	if evidence.SummaryWatermark != messages[0].ID {
		t.Fatalf("the summary reaches %d, want %d", evidence.SummaryWatermark, messages[0].ID)
	}
	// And the raw messages take over from there, so the stretch the broken
	// segment covered is read as original words rather than skipped.
	if len(evidence.Messages) == 0 {
		t.Fatal("the gap was left unread")
	}
	if evidence.Messages[0].ID != messages[1].ID {
		t.Fatalf("the raw tail starts at %d, want %d", evidence.Messages[0].ID, messages[1].ID)
	}
	if evidence.Messages[len(evidence.Messages)-1].ID != target {
		t.Fatalf("the raw tail ends at %d, want %d",
			evidence.Messages[len(evidence.Messages)-1].ID, target)
	}
}

// A board written after the target describes a state the card may not cover.
func TestCardEvidenceIgnoresABoardThatReachesPastTheTarget(t *testing.T) {
	db := openCardTestDB(t)
	user, session := createCardSession(t, db, "board")
	messages := createCardMessages(t, db, session.ID, 4)
	boardContent, err := model.MarshalSessionWhiteboardDocument(model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV1,
		Goal:          &model.SessionWhiteboardGoal{Text: "更晚的目标", SourceMessageIDs: []uint{messages[3].ID}},
	})
	if err != nil {
		t.Fatal(err)
	}
	board := model.SessionWhiteboard{
		UserID: user.ID, SessionID: session.ID, Content: boardContent,
		CoveredMessageID: messages[3].ID, TokenEstimate: 80, Version: 2,
	}
	if err := db.Create(&board).Error; err != nil {
		t.Fatal(err)
	}
	loader, err := NewDatabaseCardEvidenceLoader(db, 128)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := loader.LoadCardEvidence(context.Background(), user.ID, session.ID, messages[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Whiteboard != nil {
		t.Fatal("a board that reaches past the target was used")
	}
	if evidence.SummaryWatermark != 0 {
		t.Fatalf("the summary reaches %d, want zero", evidence.SummaryWatermark)
	}
}

// Another learner's session is not reachable, and neither is another session
// of this learner's that this card is not about.
func TestCardEvidenceIsBoundToItsOwnerAndSession(t *testing.T) {
	db := openCardTestDB(t)
	user, session := createCardSession(t, db, "owner")
	other, otherSession := createCardSession(t, db, "other")
	messages := createCardMessages(t, db, session.ID, 2)
	loader, err := NewDatabaseCardEvidenceLoader(db, 128)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loader.LoadCardEvidence(context.Background(), user.ID, otherSession.ID, messages[0].ID); err == nil {
		t.Fatal("another learner's session was loaded")
	}
	if _, err := loader.LoadCardEvidence(context.Background(), other.ID, session.ID, messages[0].ID); err == nil {
		t.Fatal("this learner's session was loaded for another owner")
	}
}

// The card a session already has is the starting point of the next one.
func TestCardEvidenceCarriesThePreviousCardAndItsWatermark(t *testing.T) {
	db := openCardTestDB(t)
	user, session := createCardSession(t, db, "previous")
	messages := createCardMessages(t, db, session.ID, 3)
	previous := &model.EpisodeMemory{
		ID: "card-previous", UserID: user.ID, SessionID: session.ID,
		ContentKind: model.EpisodeContentSessionCard,
		Title:       "上一张", Card: validCardDocument(),
		SearchText: "上一张", SourceLastMessageID: messages[0].ID,
		LastMessageAt: messages[0].CreatedAt, Version: 1,
		Status: model.EpisodeStatusActive, IndexStatus: model.EpisodeIndexIndexed,
	}
	if err := db.Create(previous).Error; err != nil {
		t.Fatal(err)
	}
	loader, err := NewDatabaseCardEvidenceLoader(db, 128)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := loader.LoadCardEvidence(context.Background(), user.ID, session.ID, messages[2].ID)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Existing == nil || evidence.Existing.ID != "card-previous" {
		t.Fatalf("the previous card was not found: %+v", evidence.Existing)
	}
	if evidence.PreviousWatermark != messages[0].ID {
		t.Fatalf("the previous watermark is %d, want %d", evidence.PreviousWatermark, messages[0].ID)
	}
}

// One session may hold an Episode and a card at the same time, and neither
// overwrites the other.
func TestEpisodeAndCardRowsCoexistForOneSession(t *testing.T) {
	db := openCardTestDB(t)
	user, session := createCardSession(t, db, "coexist")
	messages := createCardMessages(t, db, session.ID, 1)
	episode := &model.EpisodeMemory{
		ID: "episode-row", UserID: user.ID, SessionID: session.ID,
		ContentKind: model.EpisodeContentEpisode,
		Title:       "一个 Episode",
		Document: model.EpisodeDocument{
			Synopsis: "一段概述", Topics: []model.EpisodeTopic{},
			SourceHandles: []string{"message:1"},
		},
		SearchText: "一段概述", TopicKeys: model.StringList{},
		SourceLastMessageID: messages[0].ID, LastMessageAt: messages[0].CreatedAt,
		Version: 1, Status: model.EpisodeStatusActive, IndexStatus: model.EpisodeIndexIndexed,
	}
	if err := db.Create(episode).Error; err != nil {
		t.Fatal(err)
	}
	card := &model.EpisodeMemory{
		ID: "card-row", UserID: user.ID, SessionID: session.ID,
		ContentKind: model.EpisodeContentSessionCard,
		Title:       "一张卡片", Card: validCardDocument(),
		SearchText: "一张卡片", TopicKeys: model.StringList{},
		SourceLastMessageID: messages[0].ID, LastMessageAt: messages[0].CreatedAt,
		Version: 1, Status: model.EpisodeStatusActive, IndexStatus: model.EpisodeIndexIndexed,
	}
	if err := db.Create(card).Error; err != nil {
		t.Fatalf("the card row could not be written beside the episode row: %v", err)
	}
	var stored []model.EpisodeMemory
	if err := db.Where("user_id = ? AND session_id = ?", user.ID, session.ID).Find(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if len(stored) != 2 {
		t.Fatalf("the session holds %d rows, want the episode and the card", len(stored))
	}
}
