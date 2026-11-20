package isolated

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"GopherAI/common/observability"
	"GopherAI/model"
	agentService "GopherAI/service/agent"

	"github.com/cloudwego/eino/schema"
	"gorm.io/gorm"
)

// clarificationRecords counts the records of one stage.
//
// The four stages are recorded separately because the question they answer
// is "where did it break": the conversation was locked, the evidence came
// back, the answer went out, or a tool failed. One counter for all four
// cannot say which of them happened.
func clarificationRecords(stage, outcome string) uint64 {
	snapshot := observability.Memory.Snapshot()
	total := uint64(0)
	for key, value := range snapshot.Counters {
		if !strings.Contains(key, "clarification_session_total{") {
			continue
		}
		if strings.Contains(key, "stage="+stage+",") && strings.Contains(key, "outcome="+outcome+"}") {
			total += value
		}
	}
	return total
}

// The cross-session lock, through the product's own entry.
//
// The claim it defends is the one the learner's click rests on: after they
// have said which conversation they meant, the server locks THAT conversation
// and puts its own record in front of the model, so the original question is
// answered from it.
//
// It is asserted on the window the runner would send, and on the window's own
// diagnosis of what it did.

// lockFixture is an earlier conversation with a summary of its own.
func lockFixture(t *testing.T, db *gorm.DB, userID int64, userName, sessionID, title, topic, point string) {
	t.Helper()
	earlier := model.Session{ID: sessionID, UserID: userID, UserName: userName,
		Mode: model.SessionModeAgent, Title: title}
	if err := db.Create(&earlier).Error; err != nil {
		t.Fatalf("write the earlier session: %v", err)
	}
	content, err := json.Marshal(model.SessionSummarySegmentContent{
		SchemaVersion: model.SessionSummarySegmentSchemaV1,
		Topic:         topic,
		Points: []model.SessionSummarySegmentPoint{
			{Text: point, EvidenceMessageIDs: []uint{1}},
		},
	})
	if err != nil {
		t.Fatalf("encode the segment: %v", err)
	}
	segment := model.SessionSummarySegment{
		UserID: userID, SessionID: sessionID, Seq: 1, StartMessageID: 1, EndMessageID: 2,
		Content: string(content), WhiteboardChanges: "null",
		IndexStatus: model.SessionSummarySegmentIndexPending, TokenEstimate: 40,
	}
	if err := db.Create(&segment).Error; err != nil {
		t.Fatalf("write the segment: %v", err)
	}
}

// askFixture is the assistant message that asked the question.
func askFixture(t *testing.T, db *gorm.DB, userID int64, userName, sessionID string,
	question string, candidates ...string) {
	t.Helper()
	clarification := &model.SessionClarification{
		ClarificationID:    "clarification-1",
		Question:           question,
		Prompt:             "你在问哪一次？",
		AllowFreeText:      true,
		AllowHistoryBrowse: true,
	}
	for _, candidate := range candidates {
		clarification.Candidates = append(clarification.Candidates,
			model.SessionClarificationCandidate{SessionID: candidate, Title: candidate})
	}
	message := &model.Message{SessionID: sessionID, UserName: userName,
		Content: "你在问哪一次？", IsUser: false, Clarification: clarification}
	if err := db.Create(message).Error; err != nil {
		t.Fatalf("write the question: %v", err)
	}
}

// A click locks the conversation, and the window carries its evidence with
// the original question.
//
// The evidence is the answer's input: the model is not asked to find the
// conversation, and it is not asked to look for another one.
func TestAClickLocksTheConversationAndTheWindowCarriesItsEvidence(t *testing.T) {
	s := thinCardSettings(t)
	app, db, _, token := newFixture(t, s, func(context.Context, int) (*schema.Message, error) {
		return schema.AssistantMessage("你好", nil), nil
	})
	session := createSession(t, app, token, "create-lock")
	var row model.Session
	if err := db.Where("id = ?", session).First(&row).Error; err != nil {
		t.Fatalf("read the session: %v", err)
	}
	lockFixture(t, db, row.UserID, row.UserName, "l4r2-2026am-a1-03", "洄漪", "三道题", "第二道练的是读写拆分")
	askFixture(t, db, row.UserID, row.UserName, session,
		"上次你给我排的那三道题里，第二道练的是什么？", "l4r2-2026am-a1-03")

	lockedBefore := clarificationRecords("session_locked", "ok")
	evidenceBefore := clarificationRecords("evidence_returned", "ok")
	window, err := app.Context.Build(context.Background(), agentService.RunContext{
		UserID: row.UserID, SessionID: session, Question: "l4r2-2026am-a1-03",
	})
	if err != nil {
		t.Fatalf("build the window: %v", err)
	}
	if clarificationRecords("session_locked", "ok") != lockedBefore+1 {
		t.Fatal("the lock was not recorded")
	}
	if clarificationRecords("evidence_returned", "ok") != evidenceBefore+1 {
		t.Fatal("the evidence was not recorded")
	}
	if window.SessionLock == nil {
		t.Fatal("the click locked no conversation")
	}
	if window.SessionLock.SessionID != "l4r2-2026am-a1-03" {
		t.Fatalf("the lock is %q", window.SessionLock.SessionID)
	}
	if window.Budget.LockedSession == 0 {
		t.Fatal("the locked conversation cost nothing: the evidence did not reach the window")
	}
	carried := false
	for _, message := range window.Messages {
		if message == nil {
			continue
		}
		content := message.Content
		if strings.Contains(content, "读写拆分") {
			carried = true
		}
		if !strings.Contains(content, "上次你给我排的那三道题") {
			continue
		}
	}
	if !carried {
		t.Fatal("the locked conversation's evidence did not reach the window")
	}
	// The original question is what the answer has to be about.
	found := false
	for _, message := range window.Messages {
		if message != nil && strings.Contains(message.Content, "上次你给我排的那三道题") {
			found = true
		}
	}
	if !found {
		t.Fatal("the original question did not reach the window")
	}
}

// A conversation of somebody else is not locked, and nothing of it is read.
//
// The ownership check comes first: a conversation that is not the learner's
// is refused before a single word of it is looked at.
func TestAClickOnAConversationOfSomebodyElseLocksNothing(t *testing.T) {
	s := thinCardSettings(t)
	app, db, _, token := newFixture(t, s, func(context.Context, int) (*schema.Message, error) {
		return schema.AssistantMessage("你好", nil), nil
	})
	session := createSession(t, app, token, "create-lock-not-owned")
	var row model.Session
	if err := db.Where("id = ?", session).First(&row).Error; err != nil {
		t.Fatalf("read the session: %v", err)
	}
	// Somebody else's conversation, with its own summary.
	other := model.User{Username: "somebody-else", Password: "secret"}
	if err := db.Create(&other).Error; err != nil {
		t.Fatalf("write the other user: %v", err)
	}
	lockFixture(t, db, other.ID, other.Username, "session-not-mine", "别人的会话", "不该被读到", "不该被读到的内容")
	askFixture(t, db, row.UserID, row.UserName, session, "上次你给我排的那三道题里，第二道练的是什么？")

	failureBefore := clarificationRecords("tool_failure", "failed")
	window, err := app.Context.Build(context.Background(), agentService.RunContext{
		UserID: row.UserID, SessionID: session, Question: "session-not-mine",
	})
	if err != nil {
		t.Fatalf("build the window: %v", err)
	}
	if clarificationRecords("tool_failure", "failed") != failureBefore+1 {
		t.Fatal("the failed lock was not recorded as a tool failure")
	}
	if window.SessionLock != nil {
		t.Fatalf("a conversation of somebody else was locked: %+v", window.SessionLock)
	}
	for _, message := range window.Messages {
		if message != nil && strings.Contains(message.Content, "不该被读到的内容") {
			t.Fatal("the evidence of a conversation of somebody else reached the window")
		}
	}
	// The question is asked again, and the learner's own list stays open.
	if window.ClarificationReask == nil {
		t.Fatal("the question was not asked again")
	}
	if !window.ClarificationReask.AllowHistoryBrowse {
		t.Fatal("the question asked again does not offer the learner's own conversation list")
	}
}

// A clarification the learner has not answered yet changes nothing.
//
// The lock is the answer's continuation, and a turn that has no answer to
// continue has nothing to lock.
func TestATurnWithoutAClarificationLocksNothing(t *testing.T) {
	s := thinCardSettings(t)
	app, db, _, token := newFixture(t, s, func(context.Context, int) (*schema.Message, error) {
		return schema.AssistantMessage("你好", nil), nil
	})
	session := createSession(t, app, token, "create-no-clarification")
	var row model.Session
	if err := db.Where("id = ?", session).First(&row).Error; err != nil {
		t.Fatalf("read the session: %v", err)
	}
	window, err := app.Context.Build(context.Background(), agentService.RunContext{
		UserID: row.UserID, SessionID: session, Question: "我最初每天读多少页",
	})
	if err != nil {
		t.Fatalf("build the window: %v", err)
	}
	if window.SessionLock != nil {
		t.Fatalf("a turn without a clarification locked a conversation: %+v", window.SessionLock)
	}
	if window.ClarificationReask != nil {
		t.Fatal("a turn without a clarification asked one again")
	}
}
