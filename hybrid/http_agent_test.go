package hybrid

import (
	agentDAO "GopherAI/dao/agent"
	messageDAO "GopherAI/dao/message"
	sessionDAO "GopherAI/dao/session"
	"GopherAI/model"
	"GopherAI/utils"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/cloudwego/eino/schema"
	"github.com/xiaopang456/GopherLearn/conversation"
	"gorm.io/gorm"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func createAgentHTTP(t *testing.T, app *Application, token, key, title string) string {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"mode": "learning_agent", "title": title, "requestId": key})
	return dataHTTP(t, app.Handler, "POST", "/api/v1/sessions", string(b), token)["id"].(string)
}
func agentBody(key, question string) string {
	b, _ := json.Marshal(map[string]string{"content": question, "requestId": key})
	return string(b)
}
func assertAgentError(t *testing.T, status int, e map[string]any, wantStatus int, key string, native float64) {
	t.Helper()
	if status != wantStatus || e["error"] == nil {
		t.Fatalf("Agent error %d %v", status, e)
	}
	err := e["error"].(map[string]any)
	if err["code"] != key || (native != 0 && err["native_code"] != native) {
		t.Fatalf("Agent classification %v", err)
	}
}
func agentRows(t *testing.T, db *gorm.DB, sid string) []model.Message {
	t.Helper()
	p, err := messageDAO.NewRepository(db).ListOwnedHistoryPage(context.Background(), 1, sid, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	return p.Messages
}

func TestHTTPAgentCreationOriginalIdempotencyTitleAndQuota(t *testing.T) {
	app, db, _, agent, token, other := httpFixture(t)
	title := "  " + strings.Repeat("学", 105) + "  "
	sid := createAgentHTTP(t, app, token, "create-agent-001", title)
	if replay := createAgentHTTP(t, app, token, "create-agent-001", title); replay != sid {
		t.Fatal("creation replay changed identity")
	}
	row, err := sessionDAO.NewRepository(db).GetByIDAndUserID(context.Background(), sid, 1)
	if err != nil || row.Mode != model.SessionModeAgent || row.Title != strings.Repeat("学", 100) || row.CreationRequestKey == nil || *row.CreationRequestKey != "create-agent-001" {
		t.Fatalf("original creation %v %v", row, err)
	}
	status, e, _ := requestHTTP(t, app.Handler, "POST", "/api/v1/sessions", `{"mode":"learning_agent","title":"different","requestId":"create-agent-001"}`, token)
	assertAgentError(t, status, e, 409, "agent_request_conflict", 0)
	for _, key := range []string{"", "short", "bad key!", strings.Repeat("a", 65)} {
		b, _ := json.Marshal(map[string]string{"mode": "learning_agent", "requestId": key})
		status, e, _ = requestHTTP(t, app.Handler, "POST", "/api/v1/sessions", string(b), token)
		assertAgentError(t, status, e, 400, "invalid_request", 0)
	}
	if second := createAgentHTTP(t, app, token, "create-agent-002", "new"); second == sid {
		t.Fatal("new operation reused session")
	}
	if foreign := createAgentHTTP(t, app, other, "create-agent-001", title); foreign == sid {
		t.Fatal("request key crossed users")
	}
	var count int64
	db.Model(&model.Session{}).Where("user_id = ?", 1).Count(&count)
	var fill []model.Session
	for i := int(count); i < model.MaxSessionsPerUser; i++ {
		fill = append(fill, model.Session{ID: fmt.Sprintf("quota-agent-%d", i), UserID: 1, UserName: "learner", Mode: model.SessionModeNormal})
	}
	if len(fill) > 0 {
		if err := db.Create(&fill).Error; err != nil {
			t.Fatal(err)
		}
	}
	if createAgentHTTP(t, app, token, "create-agent-001", title) != sid {
		t.Fatal("replay checked quota before key")
	}
	status, e, _ = requestHTTP(t, app.Handler, "POST", "/api/v1/sessions", `{"mode":"learning_agent","requestId":"create-quota-new"}`, token)
	assertAgentError(t, status, e, 409, "session_limit_reached", 0)
	if agent.CallCount() != 0 {
		t.Fatal("creation ran model")
	}
}

func TestHTTPAgentRunOriginalMessagesResultAndReplay(t *testing.T) {
	app, db, ordinary, agent, token, _ := httpFixture(t)
	sid := createAgentHTTP(t, app, token, "create-run-001", "Agent")
	path := "/api/v1/sessions/" + sid + "/messages"
	body := agentBody("request-run-001", "你好")
	result := dataHTTP(t, app.Handler, "POST", path, body, token)
	if result["runId"] == "" || result["answer"] != "agent response" || agent.CallCount() == 0 || len(ordinary.all()) != 0 {
		t.Fatalf("actual Runner %v calls=%d", result, agent.CallCount())
	}
	run, err := agentDAO.NewRepository(db).GetRun(context.Background(), 1, sid, result["runId"].(string))
	if err != nil || run.Status != model.AgentRunCompleted {
		t.Fatalf("run %v %v", run, err)
	}
	rows := agentRows(t, db, sid)
	if len(rows) != 2 {
		t.Fatalf("messages %v", rows)
	}
	for i, row := range rows {
		role := "user"
		if i == 1 {
			role = "assistant"
		}
		if row.AgentRunID == nil || *row.AgentRunID != run.ID || row.AgentRunRole == nil || *row.AgentRunRole != role {
			t.Fatalf("original run association %v", row)
		}
	}
	var stored map[string]any
	if err := json.Unmarshal([]byte(run.ResultJSON), &stored); err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(result)
	b, _ := json.Marshal(stored)
	if string(a) != string(b) {
		t.Fatalf("RunResult fields lost\n%s\n%s", a, b)
	}
	calls := agent.CallCount()
	replay := dataHTTP(t, app.Handler, "POST", path, body, token)
	if replay["replayed"] != true || replay["runId"] != run.ID || replay["answer"] != result["answer"] || agent.CallCount() != calls || len(agentRows(t, db, sid)) != 2 {
		t.Fatalf("replay repeated side effects %v", replay)
	}
	history := dataHTTP(t, app.Handler, "GET", path, "", token)["items"].([]any)
	if len(history) != 2 || history[0].(map[string]any)["content"] != "你好" || history[1].(map[string]any)["id"] != fmt.Sprint(rows[1].ID) {
		t.Fatalf("history %v", history)
	}
	for _, changed := range []string{agentBody("request-run-001", "different"), `{"content":"你好","requestId":"request-run-001","knowledgePolicy":"knowledge_only"}`} {
		status, e, _ := requestHTTP(t, app.Handler, "POST", path, changed, token)
		assertAgentError(t, status, e, 409, "agent_request_conflict", 10002)
		if e["data"].(map[string]any)["runId"] != run.ID {
			t.Fatal("conflict lost original run ID")
		}
	}
	if agent.CallCount() != calls || len(agentRows(t, db, sid)) != 2 {
		t.Fatal("conflict side effects")
	}
	// No new compatibility message pair or guessed IDs is provided.
	if _, ok := result["assistant_message"]; ok {
		t.Fatal("fabricated message pair")
	}
}

func TestHTTPAgentRunRejectsIdentityModeRequestAndKnowledgePolicy(t *testing.T) {
	app, db, ordinary, agent, token, other := httpFixture(t)
	sid := createAgentHTTP(t, app, token, "create-reject-001", "Agent")
	path := "/api/v1/sessions/" + sid + "/messages"
	for _, tc := range []struct {
		body, token string
		status      int
		key         string
	}{
		{agentBody("request-other-001", "你好"), other, 404, "not_found"},
		{`{"content":"你好"}`, token, 400, "invalid_request"},
		{agentBody("short", "你好"), token, 400, "invalid_request"},
		{agentBody("request-empty-001", " "), token, 400, "invalid_request"},
		{`{"content":"你好","requestId":"request-user-001","userID":2}`, token, 400, "invalid_request"},
		{`{"content":"你好","requestId":"request-name-001","userName":"other"}`, token, 400, "invalid_request"},
		{`{"content":"你好","requestId":"request-mode-001","mode":"chat"}`, token, 400, "invalid_request"},
		{`{"content":"你好","requestId":"request-policy-001","knowledgePolicy":"unknown"}`, token, 400, "invalid_request"},
		{`{"content":"你好","requestId":"request-qa-001","knowledge":{"enabled":true}}`, token, 400, "invalid_request"},
	} {
		status, e, _ := requestHTTP(t, app.Handler, "POST", path, tc.body, tc.token)
		assertAgentError(t, status, e, tc.status, tc.key, 0)
	}
	if err := db.Create(&model.Session{ID: "agent-unknown-mode", UserID: 1, UserName: "learner", Mode: model.SessionMode("unknown")}).Error; err != nil {
		t.Fatal(err)
	}
	status, e, _ := requestHTTP(t, app.Handler, "POST", "/api/v1/sessions/agent-unknown-mode/messages", agentBody("request-mode-002", "你好"), token)
	assertAgentError(t, status, e, 400, "invalid_request", 0)
	chat := createHTTP(t, app.Handler, token, "Chat")
	status, e, _ = requestHTTP(t, app.Handler, "POST", "/api/v1/sessions/"+chat+"/messages", agentBody("request-agent-in-chat", "你好"), token)
	assertAgentError(t, status, e, 400, "invalid_request", 0)
	if agent.CallCount() != 0 || len(ordinary.all()) != 0 || len(agentRows(t, db, sid)) != 0 {
		t.Fatal("rejected request effects")
	}
	// Original optional invalid consent degrades to no personalization, not a new error.
	result := dataHTTP(t, app.Handler, "POST", path, `{"content":"你好","requestId":"request-consent-001","knowledgePolicy":"knowledge_preferred","topicConsent":{"ticket":"invalid-ticket","selectedTopicKeys":["foreign"]}}`, token)
	if result["usedTopicKeys"] != nil {
		t.Fatalf("unauthorised consent used %v", result)
	}
	status, e, _ = requestHTTP(t, app.Handler, "POST", "/api/v1/sessions/"+sid+"/stream", `{"content":"你好"}`, token)
	assertAgentError(t, status, e, 400, "invalid_request", 0)
}

func TestHTTPAgentRunInProgressUsesOriginalAcquisition(t *testing.T) {
	app, db, _, agent, token, _ := httpFixture(t)
	sid := createAgentHTTP(t, app, token, "create-busy-001", "Agent")
	path := "/api/v1/sessions/" + sid + "/messages"
	body := agentBody("request-busy-001", "你好")
	entered, release := make(chan struct{}), make(chan struct{})
	agent.respond = func(ctx context.Context, _ int) (*schema.Message, error) {
		close(entered)
		select {
		case <-release:
			return schema.AssistantMessage("agent response", nil), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { app.Handler.ServeHTTP(w, req); close(done) }()
	defer func() {
		close(release)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Agent HTTP did not return")
		}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("real Runner never called model")
	}
	for _, b := range []string{body, agentBody("request-busy-002", "你好")} {
		status, e, _ := requestHTTP(t, app.Handler, "POST", path, b, token)
		assertAgentError(t, status, e, 409, "agent_run_busy", 10001)
		if e["data"].(map[string]any)["runId"] == "" {
			t.Fatal("busy lost original run identity")
		}
	}
	if agent.CallCount() != 1 || len(agentRows(t, db, sid)) != 1 {
		t.Fatal("busy executed new request")
	}
}

func TestHTTPAgentFailureTerminalAndPersistenceRemainOriginal(t *testing.T) {
	for _, fault := range []string{"model", "run-create", "user-message", "assistant-message"} {
		t.Run(fault, func(t *testing.T) {
			app, db, ordinary, agent, token, _ := httpFixture(t)
			sid := createAgentHTTP(t, app, token, "create-failure-001", "Agent")
			path := "/api/v1/sessions/" + sid + "/messages"
			body := agentBody("request-failure-001", "你好")
			if fault == "model" {
				agent.respond = func(context.Context, int) (*schema.Message, error) {
					return nil, errors.New("synthetic Agent transport error")
				}
			}
			if fault != "model" {
				if err := db.Callback().Create().Before("gorm:create").Register("test:agent-http-failure", func(tx *gorm.DB) {
					if fault == "run-create" && tx.Statement.Table == "agent_runs" {
						tx.AddError(errors.New("synthetic run insert failure"))
					}
					if m, ok := tx.Statement.Dest.(*model.Message); ok && m.AgentRunID != nil && ((fault == "user-message" && m.IsUser) || (fault == "assistant-message" && !m.IsUser)) {
						tx.AddError(errors.New("synthetic Agent message failure"))
					}
				}); err != nil {
					t.Fatal(err)
				}
			}
			status, e, _ := requestHTTP(t, app.Handler, "POST", path, body, token)
			wantStatus, wantKey, native := 500, "agent_model_failed", float64(5003)
			if fault == "run-create" {
				wantStatus, wantKey, native = 503, "internal_error", 4001
			}
			assertAgentError(t, status, e, wantStatus, wantKey, native)
			rows := agentRows(t, db, sid)
			wantMessages := 1
			if fault == "user-message" || fault == "run-create" {
				wantMessages = 0
			}
			if len(rows) != wantMessages || len(ordinary.all()) != 0 {
				t.Fatalf("fault persistence %s %v", fault, rows)
			}
			if fault == "run-create" {
				var n int64
				db.Model(&model.AgentRun{}).Count(&n)
				if n != 0 || agent.CallCount() != 0 {
					t.Fatal("failed acquisition effects")
				}
				return
			}
			runID := e["data"].(map[string]any)["runId"].(string)
			run, err := agentDAO.NewRepository(db).GetRun(context.Background(), 1, sid, runID)
			wantRunStatus := model.AgentRunFailed
			if fault == "assistant-message" {
				wantRunStatus = model.AgentRunRunning
			}
			if err != nil || run.Status != wantRunStatus {
				t.Fatalf("original terminal %v %v", run, err)
			}
			calls := agent.CallCount()
			status, e, _ = requestHTTP(t, app.Handler, "POST", path, body, token)
			if fault == "assistant-message" {
				assertAgentError(t, status, e, 409, "agent_run_busy", 10001)
			} else {
				assertAgentError(t, status, e, 409, "agent_request_finished", 10003)
			}
			if e["data"].(map[string]any)["runId"] != runID || agent.CallCount() != calls || len(agentRows(t, db, sid)) != wantMessages {
				t.Fatal("finished request repeated effects")
			}
		})
	}
}

func TestHTTPAgentDraftResultAndReplayKeepOriginalGovernance(t *testing.T) {
	app, db, ordinary, agent, token, _ := httpFixture(t)
	agent.respond = func(_ context.Context, n int) (*schema.Message, error) {
		if n == 1 {
			return &schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "draft-call-001", Type: "function", Function: schema.FunctionCall{Name: "propose_memory", Arguments: `{"scope":"global","subject":"讲解偏好","content":"我喜欢简短讲解","durability":"stable"}`}}}}, nil
		}
		return schema.AssistantMessage("已生成待确认的记忆草稿", nil), nil
	}
	sid := createAgentHTTP(t, app, token, "create-draft-001", "Agent")
	path := "/api/v1/sessions/" + sid + "/messages"
	body := agentBody("request-draft-001", "请记住我喜欢简短讲解")
	result := dataHTTP(t, app.Handler, "POST", path, body, token)
	if result["memoryDraft"] == nil || result["toolCalls"].(float64) < 1 || len(ordinary.all()) != 0 {
		t.Fatalf("original draft result lost %v", result)
	}
	var drafts []model.MemoryDraft
	if err := db.Where("session_id = ?", sid).Find(&drafts).Error; err != nil || len(drafts) != 1 || drafts[0].Status != model.MemoryDraftPending {
		t.Fatalf("original pending draft %v %v", drafts, err)
	}
	var active int64
	db.Model(&model.LongTermMemory{}).Count(&active)
	if active != 0 {
		t.Fatal("confirmation silently wrote active memory")
	}
	before, _ := json.Marshal(result["memoryDraft"])
	calls := agent.CallCount()
	replayed := dataHTTP(t, app.Handler, "POST", path, body, token)
	after, _ := json.Marshal(replayed["memoryDraft"])
	if string(before) != string(after) || replayed["replayed"] != true || agent.CallCount() != calls || len(agentRows(t, db, sid)) != 2 {
		t.Fatalf("draft replay changed %v", replayed)
	}
	var count int64
	db.Model(&model.MemoryDraft{}).Count(&count)
	if count != 1 {
		t.Fatal("draft replay duplicated draft")
	}
}

func TestHTTPAgentConsentFromOriginalIssuerReachesOriginalContext(t *testing.T) {
	s := testSettings(t)
	c := &s.Config.AgentConfig
	c.MemoryReadMode = "agent_memory_user_confirmed_v1"
	c.TopicConsentSecret = "synthetic-confirmed-topic-secret-at-least-32-chars"
	c.MaxToolCalls, c.MaxModelCalls, c.MaxGraphSteps = 6, 7, 16
	c.MemoryToolMaxCalls, c.MemoryToolTopK, c.MemoryToolCandidateDepth = 2, 4, 40
	c.MemoryToolMinConfidence, c.MemoryToolTimeoutMS = .5, 2000
	db := sqliteDatabase(t)
	agent := &scriptedChatModel{respond: func(context.Context, int) (*schema.Message, error) {
		return schema.AssistantMessage("你好", nil), nil
	}}
	ordinary := &ordinaryModel{}
	migrations := 0
	app, err := Assemble(context.Background(), s, dependencies(t, db, agent, &testChunks{}, &migrations), ordinary, conversation.Options{})
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
	// This fixture is a durable original topic row; the ticket itself is issued
	// by the original authenticated HTTP route, never manually signed or granted.
	if err := db.Create(&model.LongTermMemory{ID: "confirmed-topic-memory", UserID: 1, Scope: model.MemoryScopeTopic, Subject: "鉴权", Content: "我正在学习鉴权", TopicKey: "jwt-auth", StructuredJSON: "{}", Status: model.LongTermMemoryStatus("active"), UserConfirmed: true, Confidence: 1, Durability: model.MemoryDurability("stable")}).Error; err != nil {
		t.Fatal(err)
	}
	token := loginHTTP(t, app.Handler, "learner@invalid.example")
	sid := createAgentHTTP(t, app, token, "create-consent-001", "Agent")
	path := "/api/v1/sessions/" + sid + "/messages"
	without := dataHTTP(t, app.Handler, "POST", path, agentBody("request-no-consent-001", "你好"), token)
	if without["usedTopicKeys"] != nil {
		t.Fatal("unconfirmed topic injected")
	}
	issue, _ := json.Marshal(map[string]any{"sessionId": sid, "requestId": "request-consented-001", "question": "你好", "selectedTopicKeys": []string{"jwt-auth"}})
	status, native, _ := requestHTTP(t, app.Handler, "POST", "/api/v1/AI/agent/topic-consent", string(issue), token)
	if status != 200 || native["status_code"] != float64(1000) || native["ticket"] == "" {
		t.Fatalf("original consent issue %d %v", status, native)
	}
	body, _ := json.Marshal(map[string]any{"content": "你好", "requestId": "request-consented-001", "topicConsent": map[string]any{"ticket": native["ticket"], "selectedTopicKeys": []string{"jwt-auth"}}})
	with := dataHTTP(t, app.Handler, "POST", path, string(body), token)
	keys, ok := with["usedTopicKeys"].([]any)
	if !ok || len(keys) != 1 || keys[0] != "jwt-auth" || len(ordinary.all()) != 0 || migrations != 0 {
		t.Fatalf("original consent lost in HTTP translation %v", with)
	}
	// A new request cannot reuse the ticket bound to the prior question/request.
	body, _ = json.Marshal(map[string]any{"content": "你好", "requestId": "request-consent-reuse-001", "topicConsent": map[string]any{"ticket": native["ticket"], "selectedTopicKeys": []string{"jwt-auth"}}})
	invalid := dataHTTP(t, app.Handler, "POST", path, string(body), token)
	if invalid["usedTopicKeys"] != nil {
		t.Fatal("cross-request consent accepted")
	}
}
