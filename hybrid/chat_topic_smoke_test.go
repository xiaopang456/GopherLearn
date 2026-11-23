package hybrid

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"GopherAI/model"
	agentService "GopherAI/service/agent"
	"GopherAI/utils"

	"github.com/cloudwego/eino/schema"
)

// TestChatTopicFlowThroughRealFrontendClient runs the real chat page chain -
// the actual useChat and useTopicSelection composables over the actual
// HTTP/SSE routes and context assembly - against a scripted model, and reads
// the prompts that model received to confirm the three behaviours:
//
//  1. selecting one topic loads only that topic's records and the answer
//     carries the receipt;
//  2. skipping still asks, with no topic personalisation;
//  3. switching A -> B -> A does not resurrect the panel or send the retired
//     question, and a later unconfirmed turn loads nothing.
//
// Zero paid calls: the model is a scripted stub that records what reached it.
func TestChatTopicFlowThroughRealFrontendClient(t *testing.T) {
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

	server := httptest.NewServer(app.Handler)
	t.Cleanup(server.Close)
	node := exec.Command("node", "../web/test/helpers/hybridChatTopicClient.mjs")
	node.Env = append(os.Environ(), "HYBRID_CHAT_TOPIC_URL="+server.URL)
	output, err := node.CombinedOutput()
	if err != nil {
		t.Fatalf("real chat page chain: %v\n%s", err, output)
	}
	var smoke struct {
		SelectedReceipt   []string `json:"selectedReceipt"`
		SkipReceipt       []string `json:"skipReceipt"`
		PanelAfterSwitch  bool     `json:"panelAfterSwitch"`
		StaleQuestionSent bool     `json:"staleQuestionSent"`
		FreshReceipt      []string `json:"freshReceipt"`
	}
	line := strings.TrimSpace(string(output))
	if index := strings.LastIndex(line, "\n"); index >= 0 {
		line = line[index+1:]
	}
	if err := json.Unmarshal([]byte(line), &smoke); err != nil {
		t.Fatalf("smoke summary %v\n%s", err, output)
	}
	if len(smoke.SelectedReceipt) != 1 || smoke.SelectedReceipt[0] != "jwt-auth" {
		t.Fatalf("selected receipt %v", smoke.SelectedReceipt)
	}
	if len(smoke.SkipReceipt) != 0 {
		t.Fatalf("skip receiver %v", smoke.SkipReceipt)
	}
	if smoke.PanelAfterSwitch || smoke.StaleQuestionSent {
		t.Fatalf("session switch leaked panel=%v question=%v", smoke.PanelAfterSwitch, smoke.StaleQuestionSent)
	}
	if len(smoke.FreshReceipt) != 0 {
		t.Fatalf("fresh turn used records %v", smoke.FreshReceipt)
	}

	// The prompts the scripted model actually received, one per answered turn.
	requests := agent.Requests()
	if len(requests) < 3 {
		t.Fatalf("model requests %d, want at least 3", len(requests))
	}
	prompt := func(index int) string {
		var builder strings.Builder
		for _, message := range requests[index] {
			builder.WriteString(message.Content)
			builder.WriteString("\n")
		}
		return builder.String()
	}
	findPrompt := func(question string) string {
		for index := range requests {
			if strings.Contains(prompt(index), question) {
				return prompt(index)
			}
		}
		t.Fatalf("no model request carried the question %q", question)
		return ""
	}
	selected := findPrompt("请讲讲鉴权里的过期时间校验")
	for _, want := range []string{
		"Topic context the learner explicitly confirmed for this question",
		"我正在学习鉴权过程",
		"过期时间校验",
	} {
		if !strings.Contains(selected, want) {
			t.Fatalf("selected turn prompt misses %q\n%s", want, selected)
		}
	}
	if strings.Contains(selected, "我把 Redis 队列想成排队") || strings.Contains(selected, "持久化配置") {
		t.Fatalf("unselected topic leaked into the selected turn\n%s", selected)
	}
	skipped := findPrompt("鉴权 token 过期怎么处理")
	if strings.Contains(skipped, "Topic context the learner explicitly confirmed for this question") ||
		strings.Contains(skipped, "我正在学习鉴权过程") {
		t.Fatalf("skipped turn loaded topic personal state\n%s", skipped)
	}
	fresh := findPrompt("鉴权里的签名校验怎么做")
	if strings.Contains(fresh, "Topic context the learner explicitly confirmed for this question") ||
		strings.Contains(fresh, "我正在学习鉴权过程") {
		t.Fatalf("post-switch turn loaded topic personal state\n%s", fresh)
	}
}

// TestTopicCapabilityHidesEntryOnDefaultMode proves the entry-side probe: on
// the shipped default read mode the confirmed flow is not wired, so the
// capability answer is false and the page hides the switch instead of
// promising "nothing is read before you choose" while legacy topic matching
// still loads from the question.
func TestTopicCapabilityHidesEntryOnDefaultMode(t *testing.T) {
	// The default suggestion service is process-global and another test in
	// this package may have assembled a confirmed deployment first; clear it
	// so this test observes the default-mode answer it is about.
	agentService.SetDefaultTopicSuggestionService(nil)
	t.Cleanup(func() { agentService.SetDefaultTopicSuggestionService(nil) })
	s := testSettings(t)
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
	token := loginHTTP(t, app.Handler, "learner@invalid.example")
	status, native, _ := requestHTTP(t, app.Handler, "GET", "/api/v1/AI/agent/topic-consent/capability", "", token)
	if status != 200 || native["status_code"] != float64(1000) {
		t.Fatalf("capability response %d %v", status, native)
	}
	if native["supported"] == true {
		t.Fatalf("default mode must not advertise the confirmed flow: %v", native)
	}
}
