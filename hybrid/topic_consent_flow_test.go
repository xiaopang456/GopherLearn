package hybrid

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"GopherAI/model"
	"GopherAI/utils"

	"github.com/cloudwego/eino/schema"
)

// TestTopicConsentFlowLoadsOnlyTheSelectedTopic proves the three read-side
// cases of the confirmed flow through the real HTTP routes and the real
// context assembly:
//
//  1. a ticket for one topic loads exactly that topic's understanding and
//     mastery, and nothing from another topic the learner did not select;
//  2. a turn sent without a ticket still asks and loads no topic personal
//     state at all;
//  3. a ticket signed for one session is rejected on another session, so a
//     selection never leaks across a session switch.
//
// The scripted model records what actually reached it, so the assertions are
// about the assembled prompt rather than about what the server meant to send.
func TestTopicConsentFlowLoadsOnlyTheSelectedTopic(t *testing.T) {
	s := testSettings(t)
	c := &s.Config.AgentConfig
	c.MemoryReadMode = "agent_memory_user_confirmed_v1"
	c.TopicConsentSecret = "synthetic-confirmed-topic-secret-at-least-32-chars"
	c.MaxToolCalls, c.MaxModelCalls, c.MaxGraphSteps = 6, 7, 16
	c.DeadlineSeconds, c.DeadlineThinkingSeconds = 45, 45
	c.MemoryToolMaxCalls, c.MemoryToolTopK, c.MemoryToolCandidateDepth = 2, 4, 40
	c.MemoryToolMinConfidence, c.MemoryToolTimeoutMS = .5, 2000
	db := sqliteDatabase(t)
	agent := &scriptedChatModel{respond: func(context.Context, int) (*schema.Message, error) {
		return schema.AssistantMessage("你好", nil), nil
	}}
	migrations := 0
	app, err := Assemble(context.Background(), s, dependencies(t, db, agent, &testChunks{}, &migrations))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Error(err)
		}
	})
	hash, err := utils.HashPassword("synthetic-password")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.User{ID: 1, Username: "learner", Email: "learner@invalid.example", Password: hash}).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	// Two confirmed topics, each with a note and a graded mastery row. Only
	// one of them will ever be selected.
	for _, note := range []struct{ subject, content, topic string }{
		{"鉴权", "我正在学习鉴权过程", "jwt-auth"},
		{"队列", "我把 Redis 队列想成排队", "redis-basic"},
	} {
		if err := db.Create(&model.LongTermMemory{
			ID: "note-" + note.topic, UserID: 1, Scope: model.MemoryScopeTopic, Subject: note.subject,
			Content: note.content, TopicKey: note.topic, StructuredJSON: "{}", Status: model.LongTermMemoryActive,
			UserConfirmed: true, Confidence: 1, Durability: model.MemoryDurabilityStable,
			CreatedAt: now, UpdatedAt: now,
		}).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, progress := range []struct {
		topic, display string
		score          int
		weak           []string
	}{
		{"jwt-auth", "鉴权", 80, []string{"过期时间校验"}},
		{"redis-basic", "Redis 队列", 55, []string{"持久化配置"}},
	} {
		if err := db.Create(&model.TopicProgress{
			UserID: 1, TopicKey: progress.topic, Topic: progress.display, MasteryScore: progress.score,
			Confidence: 0.8, ValidAttempts: 6, Level: model.TopicProgressDeveloping,
			WeakPoints: progress.weak, LastAttemptAt: &now, CreatedAt: now, UpdatedAt: now,
		}).Error; err != nil {
			t.Fatal(err)
		}
	}
	token := loginHTTP(t, app.Handler, "learner@invalid.example")
	sessionA := createAgentHTTP(t, app, token, "create-topic-flow-a", "Agent")
	sessionB := createAgentHTTP(t, app, token, "create-topic-flow-b", "Agent")

	promptOfLastRequest := func() string {
		requests := agent.Requests()
		if len(requests) == 0 {
			t.Fatal("the scripted model saw no request")
		}
		var builder strings.Builder
		for _, message := range requests[len(requests)-1] {
			builder.WriteString(message.Content)
			builder.WriteString("\n")
		}
		return builder.String()
	}
	askWith := func(sessionID, requestID string, consent map[string]any) map[string]any {
		body := map[string]any{"content": "请讲讲这个概念", "requestId": requestID}
		if consent != nil {
			body["topicConsent"] = consent
		}
		encoded, _ := json.Marshal(body)
		return dataHTTP(t, app.Handler, "POST", "/api/v1/sessions/"+sessionID+"/messages", string(encoded), token)
	}
	issue := func(sessionID, requestID string, keys []string) map[string]any {
		payload, _ := json.Marshal(map[string]any{
			"sessionId": sessionID, "requestId": requestID, "question": "请讲讲这个概念", "selectedTopicKeys": keys,
		})
		status, native, _ := requestHTTP(t, app.Handler, "POST", "/api/v1/AI/agent/topic-consent", string(payload), token)
		if status != 200 || native["ticket"] == "" {
			t.Fatalf("consent issue %d %v", status, native)
		}
		return native
	}

	// Case 1: select only jwt-auth. Its note and mastery instruction reach the
	// prompt; the other topic's note and weak point do not.
	ticket := issue(sessionA, "request-selected-001", []string{"jwt-auth"})
	selected := askWith(sessionA, "request-selected-001", map[string]any{
		"ticket": ticket["ticket"], "selectedTopicKeys": []string{"jwt-auth"},
	})
	keys, ok := selected["usedTopicKeys"].([]any)
	if !ok || len(keys) != 1 || keys[0] != "jwt-auth" {
		t.Fatalf("selected run receipt %v", selected["usedTopicKeys"])
	}
	prompt := promptOfLastRequest()
	for _, want := range []string{
		"Topic context the learner explicitly confirmed for this question",
		"我正在学习鉴权过程", // the selected topic's understanding
		"过期时间校验",    // the selected topic's mastery weak point
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("selected run prompt misses %q\n%s", want, prompt)
		}
	}
	for _, unwanted := range []string{
		"我把 Redis 队列想成排队", // an unselected topic's understanding
		"持久化配置",           // an unselected topic's weak point
	} {
		if strings.Contains(prompt, unwanted) {
			t.Fatalf("unselected topic leaked %q\n%s", unwanted, prompt)
		}
	}

	// Case 2: no ticket. The question is still answered, and no topic personal
	// state is loaded.
	unselected := askWith(sessionA, "request-no-consent-002", nil)
	if unselected["usedTopicKeys"] != nil {
		t.Fatalf("unconfirmed run used topics %v", unselected["usedTopicKeys"])
	}
	prompt = promptOfLastRequest()
	if strings.Contains(prompt, "Topic context the learner explicitly confirmed for this question") ||
		strings.Contains(prompt, "我正在学习鉴权过程") ||
		strings.Contains(prompt, "我把 Redis 队列想成排队") {
		t.Fatalf("unconfirmed run loaded topic personal state\n%s", prompt)
	}

	// Case 3: a ticket signed for session A is rejected on session B, so a
	// selection cannot cross a session switch.
	crossTicket := issue(sessionA, "request-cross-003", []string{"jwt-auth"})
	crossed := askWith(sessionB, "request-cross-003", map[string]any{
		"ticket": crossTicket["ticket"], "selectedTopicKeys": []string{"jwt-auth"},
	})
	if crossed["usedTopicKeys"] != nil {
		t.Fatalf("cross-session consent accepted %v", crossed["usedTopicKeys"])
	}
	prompt = promptOfLastRequest()
	if strings.Contains(prompt, "Topic context the learner explicitly confirmed for this question") ||
		strings.Contains(prompt, "我正在学习鉴权过程") {
		t.Fatalf("cross-session run loaded topic personal state\n%s", prompt)
	}
}
