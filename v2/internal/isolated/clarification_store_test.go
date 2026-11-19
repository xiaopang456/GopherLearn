package isolated

import (
	"context"
	"testing"

	"GopherAI/dao/message"
	"GopherAI/model"

	"github.com/cloudwego/eino/schema"
)

// The clarification travels through the message store.
//
// It has to: the learner answers the clarification in their NEXT message,
// and the answer is resolved against the candidates that were offered. If
// the payload did not survive the write and the read, the answer would be a
// bare word with nothing to resolve it against, and the turn after a
// clarification would silently become an ordinary one.
//
// The test goes through the REAL GORM write path - the same repository the
// product writes messages with - and reads back with the same repository the
// context builder reads history with.
func TestTheClarificationTravelsThroughTheMessageStore(t *testing.T) {
	s := testSettings(t)
	app, db, _, token := newFixture(t, s, func(context.Context, int) (*schema.Message, error) {
		return schema.AssistantMessage("你好", nil), nil
	})
	session := createSession(t, app, token, "create-clarification")

	var row model.Session
	if err := db.Where("id = ?", session).First(&row).Error; err != nil {
		t.Fatalf("read the session: %v", err)
	}
	clarification := &model.SessionClarification{
		ClarificationID: "clarification-1",
		Question:        "上次你给我排的那三道题里，第二道练的是什么？",
		Prompt:          "点一个，或者直接告诉我时间、标题或主题。",
		AllowFreeText:   true,
		Candidates: []model.SessionClarificationCandidate{
			{SessionID: "l4r2-2026am-a1-03", Title: "洄漪", Topics: []string{"慢日志与锁"}},
			{SessionID: "l4r2-2026am-a1-04", Title: "浔漳"},
		},
	}
	assistant := &model.Message{
		SessionID: session, UserName: row.UserName, Content: "你在问哪一次？", IsUser: false,
		Clarification: clarification,
	}
	repository := message.NewRepository(db)
	written, err := repository.Create(context.Background(), assistant)
	if err != nil {
		t.Fatalf("write the assistant message: %v", err)
	}
	if written.ID == 0 {
		t.Fatal("the assistant message was not written")
	}

	// Read it back the way the next turn reads the history.
	rows, err := repository.ListRecentAfter(context.Background(), session, 0, 10)
	if err != nil {
		t.Fatalf("read the history back: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("the history holds %d messages, want the one that was written", len(rows))
	}
	read := rows[0]
	if read.Clarification == nil {
		t.Fatalf("the clarification did not survive the store: %+v", read)
	}
	if read.Clarification.Question != clarification.Question {
		t.Fatalf("the question came back as %q, want %q",
			read.Clarification.Question, clarification.Question)
	}
	if len(read.Clarification.Candidates) != 2 {
		t.Fatalf("the candidates came back as %+v", read.Clarification.Candidates)
	}
	if read.Clarification.Candidates[0].SessionID != "l4r2-2026am-a1-03" ||
		read.Clarification.Candidates[0].Title != "洄漪" {
		t.Fatalf("the first candidate came back as %+v", read.Clarification.Candidates[0])
	}
	if len(read.Clarification.Candidates[0].Topics) != 1 ||
		read.Clarification.Candidates[0].Topics[0] != "慢日志与锁" {
		t.Fatalf("the topics came back as %+v", read.Clarification.Candidates[0].Topics)
	}
	if !read.Clarification.AllowFreeText {
		t.Fatal("the free-text permission did not survive the store")
	}
}

// A message written BEFORE the column existed still reads.
//
// The column is added to a table that is already full of rows, and those
// rows have nothing in it. A row whose column is NULL is a message with no
// clarification, and reading it must not be an error.
func TestAMessageWithoutAClarificationStillReads(t *testing.T) {
	s := testSettings(t)
	app, db, _, token := newFixture(t, s, func(context.Context, int) (*schema.Message, error) {
		return schema.AssistantMessage("你好", nil), nil
	})
	session := createSession(t, app, token, "create-no-clarification")

	var row model.Session
	if err := db.Where("id = ?", session).First(&row).Error; err != nil {
		t.Fatalf("read the session: %v", err)
	}
	older := &model.Message{
		SessionID: session, UserName: row.UserName, Content: "一句没有澄清的老消息", IsUser: false,
	}
	repository := message.NewRepository(db)
	if _, err := repository.Create(context.Background(), older); err != nil {
		t.Fatalf("write the old message: %v", err)
	}
	rows, err := repository.ListRecentAfter(context.Background(), session, 0, 10)
	if err != nil {
		t.Fatalf("read the old message back: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("the history holds %d messages", len(rows))
	}
	if rows[0].Clarification != nil {
		t.Fatalf("a message with no clarification carries one: %+v", rows[0].Clarification)
	}
	if rows[0].Content != "一句没有澄清的老消息" {
		t.Fatalf("the old message came back as %q", rows[0].Content)
	}
}
