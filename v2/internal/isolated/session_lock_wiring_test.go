package isolated

import (
	"context"
	"encoding/json"
	"os"
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
	lockFixtureSegments(t, db, userID, userName, sessionID, title, topic, 1, point)
}

// lockFixtureSegments is an earlier conversation with SEVERAL summary
// segments.
//
// The count matters: the locked evidence is the conversation's whole summary,
// and one segment is too small to push the evidence past the block's cap.
func lockFixtureSegments(t *testing.T, db *gorm.DB, userID int64, userName, sessionID, title, topic string,
	segments int, point string) {
	t.Helper()
	earlier := model.Session{ID: sessionID, UserID: userID, UserName: userName,
		Mode: model.SessionModeAgent, Title: title}
	if err := db.Create(&earlier).Error; err != nil {
		t.Fatalf("write the earlier session: %v", err)
	}
	for seq := 1; seq <= segments; seq++ {
		content, err := json.Marshal(model.SessionSummarySegmentContent{
			SchemaVersion: model.SessionSummarySegmentSchemaV1,
			Topic:         topic,
			Points: []model.SessionSummarySegmentPoint{
				{Text: point, EvidenceMessageIDs: []uint{uint(seq)}},
			},
		})
		if err != nil {
			t.Fatalf("encode the segment: %v", err)
		}
		segment := model.SessionSummarySegment{
			UserID: userID, SessionID: sessionID, Seq: seq,
			StartMessageID: uint(seq), EndMessageID: uint(seq) + 1,
			Content: string(content), WhiteboardChanges: "null",
			IndexStatus: model.SessionSummarySegmentIndexPending, TokenEstimate: 40,
		}
		if err := db.Create(&segment).Error; err != nil {
			t.Fatalf("write the segment: %v", err)
		}
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

// The evidence block that does not fit still scopes the turn.
//
// The evidence is dropped and the clarification block stays - the model then
// reads the conversation with the tools - and that read is bounded by the
// lock the same way. A turn whose evidence did not fit must not become a turn
// that may read any conversation of the learner's.
func TestTheLockHoldsWhenTheEvidenceBlockDoesNotFit(t *testing.T) {
	s := thinCardSettings(t)
	// The shipped window, and a record long enough that its evidence does
	// not fit the block's own cap.
	//
	// The evidence is capped at 4000 runes when it is read, and the block is
	// capped at 2000 tokens: a Chinese record of that length is more than
	// 2000 tokens, so the block cannot fit and the turn falls back to the
	// clarification block.
	app, db, _, token := newFixture(t, s, func(context.Context, int) (*schema.Message, error) {
		return schema.AssistantMessage("你好", nil), nil
	})
	session := createSession(t, app, token, "create-lock-over-budget")
	var row model.Session
	if err := db.Where("id = ?", session).First(&row).Error; err != nil {
		t.Fatalf("read the session: %v", err)
	}
	// A long enough record that its evidence cannot fit the block.
	//
	// The point is at the schema's own limit (500 runes) and there are 24
	// segments, so the conversation's summary is far past the reader's 4000
	// runes and far past the block's 2000 tokens.
	long := strings.Repeat("这是锁定那一次会话里的很长一段记录。", 22)
	lockFixtureSegments(t, db, row.UserID, row.UserName, "l4r2-2026am-a1-03", "洄漪", "三道题", 24, long)
	askFixture(t, db, row.UserID, row.UserName, session,
		"上次你给我排的那三道题里，第二道练的是什么？", "l4r2-2026am-a1-03")

	window, err := app.Context.Build(context.Background(), agentService.RunContext{
		UserID: row.UserID, SessionID: session, Question: "l4r2-2026am-a1-03",
	})
	if err != nil {
		t.Fatalf("build the window: %v", err)
	}
	// The lock is there whether or not the evidence fit.
	if window.SessionLock == nil {
		t.Fatal("the turn locked no conversation")
	}
	if window.SessionLock.SessionID != "l4r2-2026am-a1-03" {
		t.Fatalf("the lock is %q", window.SessionLock.SessionID)
	}
	// The evidence did not fit, so it is not in the window - and the
	// clarification block is.
	if window.Budget.LockedSession != 0 {
		t.Fatalf("the evidence block cost %d tokens, want none", window.Budget.LockedSession)
	}
	if window.Budget.Clarification == 0 {
		t.Fatal("the clarification block was dropped too: the turn would not know what the learner meant")
	}
	carried := false
	for _, message := range window.Messages {
		if message != nil && strings.Contains(message.Content, "上次你给我排的那三道题") {
			carried = true
		}
	}
	if !carried {
		t.Fatal("the clarification did not reach the window")
	}
}

// b3LockFixture is the frozen B3 sample: the p2c-06 summary and the two words
// after it, copied verbatim from the evaluation run's snapshot.
type b3LockFixture struct {
	TargetSession struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	} `json:"target_session"`
	Segment struct {
		Seq            int      `json:"seq"`
		StartMessageID uint     `json:"start_message_id"`
		EndMessageID   uint     `json:"end_message_id"`
		Topic          string   `json:"topic"`
		Points         []string `json:"points"`
	} `json:"segment"`
	Tail []struct {
		ID      uint   `json:"id"`
		IsUser  bool   `json:"is_user"`
		Content string `json:"content"`
	} `json:"tail"`
	Question     string `json:"question"`
	OtherSession struct {
		ID    string `json:"id"`
		Title string `json:"title"`
		Point string `json:"point"`
		Tail  string `json:"tail"`
	} `json:"other_session"`
}

func loadB3LockFixture(t *testing.T) b3LockFixture {
	t.Helper()
	raw, err := os.ReadFile("testdata/b3-p2c-06-lock.json")
	if err != nil {
		t.Fatalf("read the frozen B3 sample: %v", err)
	}
	var fixture b3LockFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("decode the frozen B3 sample: %v", err)
	}
	return fixture
}

// The frozen B3 sample: the answer after the summary watermark reaches the
// actual model input.
//
// B3 locked p2c-06 and the lock's evidence carried 374 tokens of summary, but
// the answer ("依赖裁剪") is message 720, two messages after the summary's last
// covered message 718. The window the runner sends must now carry those words
// as well - the summary first, then the tail in message order - and it must
// stay inside the one cap the two parts share. Another conversation's tail
// must not be merged: the read is bounded by the lock, not by the learner.
func TestTheFrozenB3SampleCarriesTheWordAfterTheSummaryWatermark(t *testing.T) {
	fixture := loadB3LockFixture(t)
	s := thinCardSettings(t)
	app, db, _, token := newFixture(t, s, func(context.Context, int) (*schema.Message, error) {
		return schema.AssistantMessage("你好", nil), nil
	})
	session := createSession(t, app, token, "create-lock-b3-frozen")
	var current model.Session
	if err := db.Where("id = ?", session).First(&current).Error; err != nil {
		t.Fatalf("read the session: %v", err)
	}

	target := model.Session{ID: fixture.TargetSession.ID, UserID: current.UserID,
		UserName: current.UserName, Mode: model.SessionModeAgent, Title: fixture.TargetSession.Title}
	if err := db.Create(&target).Error; err != nil {
		t.Fatalf("write the frozen conversation: %v", err)
	}
	points := make([]model.SessionSummarySegmentPoint, 0, len(fixture.Segment.Points))
	for index, text := range fixture.Segment.Points {
		points = append(points, model.SessionSummarySegmentPoint{
			Text: text, EvidenceMessageIDs: []uint{fixture.Segment.StartMessageID + uint(index)},
		})
	}
	encoded, err := json.Marshal(model.SessionSummarySegmentContent{
		SchemaVersion: model.SessionSummarySegmentSchemaV2,
		Topic:         fixture.Segment.Topic,
		Points:        points,
	})
	if err != nil {
		t.Fatalf("encode the frozen summary: %v", err)
	}
	segment := model.SessionSummarySegment{
		UserID: current.UserID, SessionID: fixture.TargetSession.ID, Seq: fixture.Segment.Seq,
		StartMessageID: fixture.Segment.StartMessageID, EndMessageID: fixture.Segment.EndMessageID,
		Topic: fixture.Segment.Topic, Content: string(encoded), WhiteboardChanges: "null",
		IndexStatus: model.SessionSummarySegmentIndexPending, TokenEstimate: 100,
	}
	if err := db.Create(&segment).Error; err != nil {
		t.Fatalf("write the frozen summary: %v", err)
	}
	for _, word := range fixture.Tail {
		message := model.Message{ID: word.ID, SessionID: fixture.TargetSession.ID,
			UserName: current.UserName, Content: word.Content, IsUser: word.IsUser}
		if err := db.Create(&message).Error; err != nil {
			t.Fatalf("write the frozen word %d: %v", word.ID, err)
		}
	}
	// Another conversation with a tail of its own. It is never named by the
	// learner, so its words must never be merged into the locked evidence.
	other := model.Session{ID: fixture.OtherSession.ID, UserID: current.UserID,
		UserName: current.UserName, Mode: model.SessionModeAgent, Title: fixture.OtherSession.Title}
	if err := db.Create(&other).Error; err != nil {
		t.Fatalf("write the other conversation: %v", err)
	}
	otherPoint, err := json.Marshal(model.SessionSummarySegmentContent{
		SchemaVersion: model.SessionSummarySegmentSchemaV2,
		Topic:         fixture.OtherSession.Point,
		Points:        []model.SessionSummarySegmentPoint{{Text: fixture.OtherSession.Point}},
	})
	if err != nil {
		t.Fatalf("encode the other summary: %v", err)
	}
	if err := db.Create(&model.SessionSummarySegment{
		UserID: current.UserID, SessionID: fixture.OtherSession.ID, Seq: 1,
		StartMessageID: 400, EndMessageID: 500, Topic: fixture.OtherSession.Point,
		Content: string(otherPoint), WhiteboardChanges: "null",
		IndexStatus: model.SessionSummarySegmentIndexPending,
	}).Error; err != nil {
		t.Fatalf("write the other summary: %v", err)
	}
	if err := db.Create(&model.Message{ID: 501, SessionID: fixture.OtherSession.ID,
		UserName: current.UserName, Content: fixture.OtherSession.Tail}).Error; err != nil {
		t.Fatalf("write the other tail: %v", err)
	}

	askFixture(t, db, current.UserID, current.UserName, session, fixture.Question)

	window, err := app.Context.Build(context.Background(), agentService.RunContext{
		UserID: current.UserID, SessionID: session, Question: fixture.TargetSession.ID,
	})
	if err != nil {
		t.Fatalf("build the window: %v", err)
	}
	if window.SessionLock == nil || window.SessionLock.SessionID != fixture.TargetSession.ID {
		t.Fatalf("the frozen conversation was not locked: %+v", window.SessionLock)
	}
	if window.SessionLock.TailMessages != len(fixture.Tail) {
		t.Fatalf("the lock carries %d words after the summary, want %d",
			window.SessionLock.TailMessages, len(fixture.Tail))
	}

	var joined strings.Builder
	lockedBlock := ""
	for _, message := range window.Messages {
		if message == nil {
			continue
		}
		joined.WriteString(message.Content)
		joined.WriteString("\n")
		if strings.Contains(message.Content, "The learner told you which conversation they meant") {
			lockedBlock = message.Content
		}
	}
	all := joined.String()
	if !strings.Contains(all, "怀疑是全量重建") {
		t.Fatal("the summary did not reach the window")
	}
	if !strings.Contains(all, "依赖裁剪") {
		t.Fatal("message 720's words did not reach the actual model input")
	}
	if !strings.Contains(all, "「蓁葭」三道") {
		t.Fatal("the merged word is not message 720's")
	}
	// The lock's own block carries summary first, then the tail in message order.
	if lockedBlock == "" {
		t.Fatal("the locked evidence block did not reach the window")
	}
	firstWordAt := strings.Index(lockedBlock, "给我三个练习吧")
	answerAt := strings.Index(lockedBlock, "依赖裁剪")
	summaryAt := strings.Index(lockedBlock, "怀疑是全量重建")
	if summaryAt < 0 || firstWordAt < 0 || answerAt < 0 || !(summaryAt < firstWordAt && firstWordAt < answerAt) {
		t.Fatalf("the locked block is not summary-then-tail in message order: %q", lockedBlock)
	}
	// The summary and the tail share the one cap.
	if window.Budget.LockedSession <= 0 || window.Budget.LockedSession > 2000 {
		t.Fatalf("the locked block cost %d tokens, want it inside the 2000 cap", window.Budget.LockedSession)
	}
	// The other conversation's tail is not merged.
	if strings.Contains(all, fixture.OtherSession.Tail) {
		t.Fatal("another conversation's tail reached the locked evidence")
	}
}

// A tail longer than the shared cap is cut; the summary stays.
//
// The tail is read in message order and bounded, and the summary keeps its
// place first: a long tail must not push the whole block over the cap and
// cost the conversation the summary it already had.
func TestALongTailIsCutToFitAndTheSummaryStays(t *testing.T) {
	s := thinCardSettings(t)
	app, db, _, token := newFixture(t, s, func(context.Context, int) (*schema.Message, error) {
		return schema.AssistantMessage("你好", nil), nil
	})
	session := createSession(t, app, token, "create-lock-long-tail")
	var current model.Session
	if err := db.Where("id = ?", session).First(&current).Error; err != nil {
		t.Fatalf("read the session: %v", err)
	}

	long := model.Session{ID: "long-tail-session", UserID: current.UserID,
		UserName: current.UserName, Mode: model.SessionModeAgent, Title: "长尾会话"}
	if err := db.Create(&long).Error; err != nil {
		t.Fatalf("write the earlier session: %v", err)
	}
	encoded, err := json.Marshal(model.SessionSummarySegmentContent{
		SchemaVersion: model.SessionSummarySegmentSchemaV2,
		Topic:         "摘要",
		Points:        []model.SessionSummarySegmentPoint{{Text: "摘要里的结论要保留", EvidenceMessageIDs: []uint{100}}},
	})
	if err != nil {
		t.Fatalf("encode the summary: %v", err)
	}
	if err := db.Create(&model.SessionSummarySegment{
		UserID: current.UserID, SessionID: long.ID, Seq: 1,
		StartMessageID: 1, EndMessageID: 100, Topic: "摘要",
		Content: string(encoded), WhiteboardChanges: "null",
		IndexStatus: model.SessionSummarySegmentIndexPending, TokenEstimate: 20,
	}).Error; err != nil {
		t.Fatalf("write the summary: %v", err)
	}
	const lastReadWord = 140
	for id := uint(101); id <= lastReadWord; id++ {
		content := strings.Repeat("尾部很长的原话内容", 18)
		if id == lastReadWord {
			content = "最后一条被读到的尾部原话，肯定放不下"
		}
		if err := db.Create(&model.Message{ID: id, SessionID: long.ID,
			UserName: current.UserName, Content: content}).Error; err != nil {
			t.Fatalf("write the tail word %d: %v", id, err)
		}
	}
	askFixture(t, db, current.UserID, current.UserName, session, "上次那三道题里第二道练的是什么？")

	window, err := app.Context.Build(context.Background(), agentService.RunContext{
		UserID: current.UserID, SessionID: session, Question: long.ID,
	})
	if err != nil {
		t.Fatalf("build the window: %v", err)
	}
	if window.SessionLock == nil || window.SessionLock.TailMessages != 40 {
		t.Fatalf("the lock is %+v, want the bounded tail read", window.SessionLock)
	}
	if window.Budget.LockedSession <= 0 || window.Budget.LockedSession > 2000 {
		t.Fatalf("the locked block cost %d tokens, want it inside the 2000 cap", window.Budget.LockedSession)
	}
	var joined strings.Builder
	for _, message := range window.Messages {
		if message != nil {
			joined.WriteString(message.Content)
			joined.WriteString("\n")
		}
	}
	all := joined.String()
	if !strings.Contains(all, "摘要里的结论要保留") {
		t.Fatal("the summary did not survive the long tail")
	}
	if !strings.Contains(all, "尾部很长的原话内容") {
		t.Fatal("the first tail words that fit were dropped")
	}
	if strings.Contains(all, "最后一条被读到的尾部原话") {
		t.Fatal("a tail word that does not fit was kept")
	}
}
