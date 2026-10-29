package hybrid

import (
	messageDAO "GopherAI/dao/message"
	"GopherAI/entry"
	"GopherAI/model"
	"GopherAI/utils"
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	modelcomponent "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/xiaopang456/GopherLearn/conversation"
	"gorm.io/gorm"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type streamPlan struct {
	parts            []*schema.Message
	entered, release chan struct{}
	failure          error
	omitFinish       bool
}
type fragmentAgentModel struct {
	*scriptedChatModel
	lock  sync.Mutex
	count int
	plan  func(int, []*schema.Message) streamPlan
}

func (m *fragmentAgentModel) CallCount() int { m.lock.Lock(); defer m.lock.Unlock(); return m.count }
func (m *fragmentAgentModel) WithTools(tools []*schema.ToolInfo) (modelcomponent.ToolCallingChatModel, error) {
	_, err := m.scriptedChatModel.WithTools(tools)
	return m, err
}
func (m *fragmentAgentModel) Stream(ctx context.Context, input []*schema.Message, _ ...modelcomponent.Option) (*schema.StreamReader[*schema.Message], error) {
	m.lock.Lock()
	m.count++
	n := m.count
	m.lock.Unlock()
	plan := m.plan(n, input)
	r, w := schema.Pipe[*schema.Message](1)
	go func() {
		defer w.Close()
		for i, part := range plan.parts {
			if w.Send(part, nil) {
				return
			}
			if i == 0 && plan.entered != nil {
				close(plan.entered)
				select {
				case <-plan.release:
				case <-ctx.Done():
					w.Send(nil, ctx.Err())
					return
				}
			}
		}
		if plan.failure != nil {
			w.Send(nil, plan.failure)
			return
		}
		if !plan.omitFinish {
			reason := "stop"
			for _, part := range plan.parts {
				if len(part.ToolCalls) > 0 {
					reason = "tool_calls"
				}
			}
			w.Send(&schema.Message{Role: schema.Assistant, ResponseMeta: &schema.ResponseMeta{FinishReason: reason}}, nil)
		}
	}()
	return r, nil
}
func pieces(text ...string) []*schema.Message {
	out := make([]*schema.Message, 0, len(text))
	for _, s := range text {
		out = append(out, schema.AssistantMessage(s, nil))
	}
	return out
}
func publicStreamBody(key, q string) string {
	b, _ := json.Marshal(map[string]any{"content": q, "requestId": key, "stream_steps": true})
	return string(b)
}
func sseFixture(t *testing.T, plan func(int, []*schema.Message) streamPlan) (*Application, *gorm.DB, *fragmentAgentModel, *ordinaryModel, string, string) {
	t.Helper()
	db := sqliteDatabase(t)
	ordinary := &ordinaryModel{}
	base := &scriptedChatModel{}
	m := &fragmentAgentModel{scriptedChatModel: base, plan: plan}
	migrations := 0
	deps := dependencies(t, db, base, &testChunks{}, &migrations)
	deps.ChatModel = func(context.Context, *entry.Settings) (modelcomponent.ToolCallingChatModel, error) { return m, nil }
	app, err := Assemble(context.Background(), testSettings(t), deps, ordinary, conversation.Options{})
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
	for _, u := range []model.User{{ID: 1, Username: "learner", Email: "learner@invalid.example", Password: hash}, {ID: 2, Username: "other", Email: "other@invalid.example", Password: hash}} {
		if err := db.Create(&u).Error; err != nil {
			t.Fatal(err)
		}
	}
	if migrations != 0 {
		t.Fatal("business migration")
	}
	return app, db, m, ordinary, loginHTTP(t, app.Handler, "learner@invalid.example"), loginHTTP(t, app.Handler, "other@invalid.example")
}

type wireFrame struct {
	name string
	data map[string]any
}
type cancelPublicWriter struct {
	*httptest.ResponseRecorder
	cancel context.CancelFunc
}

func (w *cancelPublicWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseRecorder.Write(p)
	if strings.Contains(string(p), "event: agent.text.delta\n") {
		w.cancel()
	}
	return n, err
}
func (w *cancelPublicWriter) WriteString(s string) (int, error) { return w.Write([]byte(s)) }
func frames(t *testing.T, body string) []wireFrame {
	t.Helper()
	var out []wireFrame
	for _, raw := range strings.Split(body, "\n\n") {
		if raw == "" {
			continue
		}
		lines := strings.Split(raw, "\n")
		if len(lines) != 2 {
			t.Fatalf("frame %q", raw)
		}
		var d map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(lines[1], "data: ")), &d); err != nil {
			t.Fatal(err)
		}
		out = append(out, wireFrame{strings.TrimPrefix(lines[0], "event: "), d})
	}
	return out
}
func named(all []wireFrame, name string) []wireFrame {
	var out []wireFrame
	for _, e := range all {
		if e.name == name {
			out = append(out, e)
		}
	}
	return out
}
func streamAgentHTTP(t *testing.T, app *Application, token, sid, key, q string) []wireFrame {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"content": q, "requestId": key, "stream_steps": true})
	status, _, text := requestHTTP(t, app.Handler, "POST", "/api/v1/sessions/"+sid+"/stream", string(body), token)
	if status != 200 {
		t.Fatalf("Agent SSE HTTP %d %s", status, text)
	}
	return frames(t, text)
}

func TestAgentSSERealFragmentsFlushBeforeModelEnds(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	app, db, m, ordinary, token, _ := sseFixture(t, func(int, []*schema.Message) streamPlan {
		return streamPlan{parts: pieces("你好", "，公开片段"), entered: entered, release: release}
	})
	sid := createAgentHTTP(t, app, token, "create-live-001", "Agent")
	server := httptest.NewServer(app.Handler)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", server.URL+"/api/v1/sessions/"+sid+"/stream", strings.NewReader(publicStreamBody("request-live-001", "你好")))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var first strings.Builder
	reader := bufio.NewReader(response.Body)
	for !strings.Contains(first.String(), "event: agent.text.delta\n") || !strings.HasSuffix(first.String(), "\n\n") {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		first.WriteString(line)
	}
	if !strings.Contains(first.String(), "你好") || strings.Contains(first.String(), "message.completed") {
		t.Fatalf("pre-completion wire %s", first.String())
	}
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if len(agentRows(t, db, sid)) != 1 {
		t.Fatal("candidate was committed early")
	}
	close(release)
	rest, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	all := frames(t, first.String()+string(rest))
	delta := named(all, "agent.text.delta")
	end := named(all, "agent.text.ended")
	done := named(all, "message.completed")
	if len(delta) != 2 || delta[0].data["content"] != "你好" || delta[1].data["content"] != "，公开片段" || len(done) != 1 || done[0].data["content"] != "你好，公开片段" || done[0].data["final_block_id"] != end[0].data["block_id"] || m.CallCount() != 1 || len(ordinary.all()) != 0 {
		t.Fatalf("actual fragment/confirmation %v", all)
	}
	rows := agentRows(t, db, sid)
	if len(rows) != 2 || done[0].data["message_id"] != fmt.Sprint(rows[1].ID) {
		t.Fatal("completion not bound to stored assistant")
	}
}

func TestAgentSSEToolRoundIsProcessAndCandidateOnlyConfirmedAtCompletion(t *testing.T) {
	app, db, m, _, token, _ := sseFixture(t, func(n int, _ []*schema.Message) streamPlan {
		if n == 1 {
			parts := []*schema.Message{{Role: schema.Assistant, Content: "我先生成草稿。", ToolCalls: []schema.ToolCall{{ID: "PRIVATE_TOOL_CALL", Type: "function", Function: schema.FunctionCall{Name: "propose_memory", Arguments: `{"scope":"global","subject":"讲解偏好","content":"我喜欢简短讲解","durability":"stable"}`}}}}}
			return streamPlan{parts: parts}
		}
		return streamPlan{parts: pieces("已生成", "待确认草稿")}
	})
	sid := createAgentHTTP(t, app, token, "create-tool-001", "Agent")
	all := streamAgentHTTP(t, app, token, sid, "request-tool-001", "请记住我喜欢简短讲解")
	ends := named(all, "agent.text.ended")
	done := named(all, "message.completed")
	if len(ends) != 2 || ends[0].data["kind"] != "process" || ends[1].data["kind"] != "answer_candidate" || len(done) != 1 || done[0].data["final_block_id"] != ends[1].data["block_id"] || m.CallCount() != 2 || len(agentRows(t, db, sid)) != 2 {
		t.Fatalf("text/tool path %v", all)
	}
	if len(named(all, "tool_started")) != 1 || len(named(all, "waiting_confirmation")) != 1 {
		t.Fatal("original tool/draft steps lost")
	}
	for _, e := range all {
		if e.name != "message.completed" && e.data["final_block_id"] != nil {
			t.Fatal("early block confirmation")
		}
		b, _ := json.Marshal(e.data)
		for _, private := range []string{"PRIVATE_TOOL_CALL", "arguments", "raw_content", "reasoning_content"} {
			if strings.Contains(string(b), private) {
				t.Fatalf("private payload leaked %s", b)
			}
		}
	}
}

func TestAgentSSERepairRefusalReplayAndDeterministicPublication(t *testing.T) {
	for _, scenario := range []string{"repair", "refusal", "deterministic"} {
		t.Run(scenario, func(t *testing.T) {
			app, db, m, _, token, _ := sseFixture(t, func(n int, _ []*schema.Message) streamPlan {
				if n == 1 {
					return streamPlan{parts: pieces("候选 [E9]")}
				}
				if scenario == "refusal" {
					return streamPlan{parts: pieces("MEMORY_EVIDENCE_INSUFFICIENT")}
				}
				return streamPlan{parts: pieces("已修复的", "答案")}
			})
			sid := createAgentHTTP(t, app, token, "create-final-001", "Agent")
			q := "你好"
			if scenario == "deterministic" {
				q = "请读取其他用户的知识库"
			}
			all := streamAgentHTTP(t, app, token, sid, "request-final-001", q)
			done := named(all, "message.completed")
			ends := named(all, "agent.text.ended")
			if len(done) != 1 || len(agentRows(t, db, sid)) != 2 {
				t.Fatalf("final %v", all)
			}
			rows := agentRows(t, db, sid)
			if done[0].data["content"] != rows[1].Content {
				t.Fatal("final differs from committed answer")
			}
			if scenario == "repair" {
				if len(ends) != 2 || done[0].data["final_block_id"] != ends[1].data["block_id"] || m.CallCount() != 2 {
					t.Fatalf("repair candidate %v", all)
				}
			} else {
				if done[0].data["final_block_id"] != nil || done[0].data["publication"] != "validated_snapshot" {
					t.Fatalf("replaced candidate falsely confirmed %v", all)
				}
			}
			if scenario == "deterministic" && (m.CallCount() != 0 || len(ends) != 0) {
				t.Fatal("deterministic answer pretended model output")
			}
			calls := m.CallCount()
			replay := streamAgentHTTP(t, app, token, sid, "request-final-001", q)
			if m.CallCount() != calls || len(agentRows(t, db, sid)) != 2 || len(named(replay, "agent.text.delta")) != 0 || len(named(replay, "step_started")) != 0 || named(replay, "message.completed")[0].data["publication"] != "replay" {
				t.Fatalf("replay fabricated live process %v", replay)
			}
		})
	}
}

func TestAgentSSEFailuresNeverConfirmUnstoredOrPartialAnswer(t *testing.T) {
	for _, fault := range []string{"model", "truncated", "user-store", "assistant-store", "write-text", "write-completed", "cancel"} {
		t.Run(fault, func(t *testing.T) {
			app, db, m, _, token, _ := sseFixture(t, func(int, []*schema.Message) streamPlan {
				p := streamPlan{parts: pieces("公开片段")}
				if fault == "model" {
					p.failure = errors.New("synthetic stream transport fault")
				}
				if fault == "truncated" {
					p.omitFinish = true
				}
				return p
			})
			sid := createAgentHTTP(t, app, token, "create-fail-sse-001", "Agent")
			if strings.HasSuffix(fault, "store") {
				if err := db.Callback().Create().Before("gorm:create").Register("test:sse-store-failure", func(tx *gorm.DB) {
					if m, ok := tx.Statement.Dest.(*model.Message); ok && ((fault == "user-store" && m.IsUser) || (fault == "assistant-store" && !m.IsUser)) {
						tx.AddError(errors.New("synthetic SSE persistence fault"))
					}
				}); err != nil {
					t.Fatal(err)
				}
			}
			req := httptest.NewRequest("POST", "/api/v1/sessions/"+sid+"/stream", strings.NewReader(publicStreamBody("request-fail-sse-001", "你好")))
			req.Header.Set("Authorization", "Bearer "+token)
			var w http.ResponseWriter
			rec := httptest.NewRecorder()
			w = rec
			if fault == "write-text" || fault == "write-completed" {
				name := "agent.text.delta"
				if fault == "write-completed" {
					name = "message.completed"
				}
				w = &failedStreamWriter{ResponseRecorder: rec, failEvent: name}
			}
			if fault == "cancel" {
				ctx, cancel := context.WithCancel(req.Context())
				defer cancel()
				req = req.WithContext(ctx)
				w = &cancelPublicWriter{ResponseRecorder: rec, cancel: cancel}
			}
			app.Handler.ServeHTTP(w, req)
			all := frames(t, rec.Body.String())
			if len(named(all, "message.completed")) != 0 {
				t.Fatalf("failed turn completed %v", all)
			}
			expected := 1
			if fault == "user-store" {
				expected = 0
			}
			if fault == "write-completed" {
				expected = 2
			}
			if len(agentRows(t, db, sid)) != expected {
				t.Fatalf("original persistence fault=%s rows=%v", fault, agentRows(t, db, sid))
			}
			if fault == "write-completed" || fault == "model" || fault == "truncated" {
				calls := m.CallCount()
				again := streamAgentHTTP(t, app, token, sid, "request-fail-sse-001", "你好")
				if m.CallCount() != calls || len(agentRows(t, db, sid)) != expected {
					t.Fatal("retry repeated side effects")
				}
				if fault == "write-completed" {
					if len(named(again, "message.completed")) != 1 || named(again, "message.completed")[0].data["publication"] != "replay" {
						t.Fatal("persisted but unwritten completion did not replay")
					}
				} else {
					failed := named(again, "message.failed")
					if len(failed) != 1 || failed[0].data["native_code"] != float64(10003) {
						t.Fatalf("finished request accepted %v", again)
					}
				}
			}
		})
	}
}

// This records a source limitation, not successful late-tool execution: the
// frozen Eino checker chooses its branch at the first nonempty Content chunk.
func TestAgentSSELateToolSourceLimitationMatchesNonStreaming(t *testing.T) {
	app, db, m, _, token, _ := sseFixture(t, func(int, []*schema.Message) streamPlan {
		parts := pieces("工具前的文字")
		parts = append(parts, &schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "late-private", Type: "function", Function: schema.FunctionCall{Name: "propose_memory", Arguments: `{"scope":"global","subject":"讲解偏好","content":"我喜欢简短讲解","durability":"stable"}`}}}})
		return streamPlan{parts: parts}
	})
	sid := createAgentHTTP(t, app, token, "create-late-001", "Agent")
	native := dataHTTP(t, app.Handler, "POST", "/api/v1/sessions/"+sid+"/messages", agentBody("request-late-native", "请记住我喜欢简短讲解"), token)
	all := streamAgentHTTP(t, app, token, sid, "request-late-stream", "请记住我喜欢简短讲解")
	done, ends := named(all, "message.completed"), named(all, "agent.text.ended")
	if len(done) != 1 || len(ends) != 1 || ends[0].data["kind"] != "process" || done[0].data["final_block_id"] != nil || len(named(all, "tool_started")) != 0 || m.CallCount() != 2 {
		t.Fatalf("frozen late tool behavior changed %v", all)
	}
	if native["answer"] != done[0].data["content"] || native["toolCalls"] != float64(0) || len(agentRows(t, db, sid)) != 4 {
		t.Fatal("source parity differs")
	}
}

func TestAgentSSEConcurrentIdentityBusyAndConflictUseOriginalAcquisition(t *testing.T) {
	entered := []chan struct{}{make(chan struct{}), make(chan struct{})}
	release := []chan struct{}{make(chan struct{}), make(chan struct{})}
	app, db, m, ordinary, token, other := sseFixture(t, func(n int, _ []*schema.Message) streamPlan {
		return streamPlan{parts: pieces(fmt.Sprintf("user-public-%d", n)), entered: entered[n-1], release: release[n-1]}
	})
	sid := createAgentHTTP(t, app, token, "create-concurrent-001", "Agent")
	otherSID := createAgentHTTP(t, app, other, "create-concurrent-002", "Other")
	rec := []*httptest.ResponseRecorder{httptest.NewRecorder(), httptest.NewRecorder()}
	finished := []chan struct{}{make(chan struct{}), make(chan struct{})}
	contexts := make([]context.CancelFunc, 2)
	for i, item := range []struct{ token, sid, key string }{{token, sid, "request-concurrent-001"}, {other, otherSID, "request-concurrent-002"}} {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		contexts[i] = cancel
		defer cancel()
		req := httptest.NewRequest("POST", "/api/v1/sessions/"+item.sid+"/stream", strings.NewReader(publicStreamBody(item.key, "你好"))).WithContext(ctx)
		req.Header.Set("Authorization", "Bearer "+item.token)
		go func(i int) { app.Handler.ServeHTTP(rec[i], req); close(finished[i]) }(i)
		select {
		case <-entered[i]:
		case <-ctx.Done():
			t.Fatal("actual model never entered")
		}
	}
	defer func() {
		for _, cancel := range contexts {
			cancel()
		}
		for _, done := range finished {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("request did not terminate")
			}
		}
	}()
	busy := streamAgentHTTP(t, app, token, sid, "request-concurrent-001", "你好")
	failure := named(busy, "message.failed")
	if len(failure) != 1 || failure[0].data["native_code"] != float64(10001) || len(named(busy, "agent.text.delta")) != 0 || m.CallCount() != 2 {
		t.Fatalf("busy semantics %v", busy)
	}
	status, _, _ := requestHTTP(t, app.Handler, "POST", "/api/v1/sessions/"+sid+"/stream", agentBody("request-cross-001", "你好"), other)
	if status != 404 {
		t.Fatalf("cross-user status %d", status)
	}
	// Keep requests overlapping, but avoid making SQLite a MySQL concurrency test.
	close(release[1])
	<-finished[1]
	close(release[0])
	<-finished[0]
	var runIDs []any
	for i, r := range rec {
		all := frames(t, r.Body.String())
		delta, done := named(all, "agent.text.delta"), named(all, "message.completed")
		if len(delta) != 1 || len(done) != 1 || delta[0].data["content"] != fmt.Sprintf("user-public-%d", i+1) || done[0].data["content"] != delta[0].data["content"] {
			t.Fatalf("text crossed requests %v", all)
		}
		runIDs = append(runIDs, done[0].data["run_id"])
		for _, e := range all {
			if e.data["run_id"] != runIDs[i] {
				t.Fatal("run crossed request")
			}
		}
	}
	otherRows, otherErr := messageDAO.NewRepository(db).ListOwnedHistoryPage(context.Background(), 2, otherSID, 0, 100)
	if runIDs[0] == runIDs[1] || len(agentRows(t, db, sid)) != 2 || otherErr != nil || len(otherRows.Messages) != 2 || len(ordinary.all()) != 0 {
		t.Fatal("isolation/persistence")
	}
	conflict := streamAgentHTTP(t, app, token, sid, "request-concurrent-001", "不同输入")
	fail := named(conflict, "message.failed")
	if len(fail) != 1 || fail[0].data["native_code"] != float64(10002) || m.CallCount() != 2 {
		t.Fatalf("conflict semantics %v", conflict)
	}
}

func TestAgentSSEPreflightUsesOriginalValidationAndRejectsClientIdentity(t *testing.T) {
	app, _, m, _, token, _ := sseFixture(t, func(int, []*schema.Message) streamPlan { return streamPlan{parts: pieces("answer")} })
	sid := createAgentHTTP(t, app, token, "create-preflight-001", "Agent")
	path := "/api/v1/sessions/" + sid + "/stream"
	for _, item := range []struct{ path, body string }{{path, `{"content":"question"}`}, {path, `{"content":"question","requestId":"bad"}`}, {path, `{"content":"","requestId":"request-preflight-001"}`}, {path, `{"content":"question","requestId":"request-preflight-001","userID":2}`}, {path + "?userID=2", agentBody("request-preflight-001", "你好")}, {path, `{"content":"question","requestId":"request-preflight-001","knowledgePolicy":"unknown"}`}} {
		status, _, body := requestHTTP(t, app.Handler, "POST", item.path, item.body, token)
		if status != 400 || strings.Contains(body, "agent.text") {
			t.Fatalf("malformed preflight %d %s", status, body)
		}
	}
	if m.CallCount() != 0 {
		t.Fatal("preflight called model")
	}
	status, _, body := requestHTTP(t, app.Handler, "POST", path, agentBody("request-preflight-001", "你好"), token)
	if status != 200 {
		t.Fatal(body)
	}
	all := frames(t, body)
	if len(named(all, "step_started")) != 0 || len(named(all, "agent.text.delta")) != 0 || len(named(all, "message.delta")) != 1 || named(all, "message.delta")[0].data["publication"] != "validated_snapshot" || len(named(all, "message.completed")) != 1 {
		t.Fatal("explicit step gate changed public text")
	}
}

type shortPublicWriter struct{ *httptest.ResponseRecorder }

func (w *shortPublicWriter) Write(p []byte) (int, error)       { return len(p) - 1, nil }
func (w *shortPublicWriter) WriteString(s string) (int, error) { return w.Write([]byte(s)) }
func TestAgentSSEShortWriteCancelsWithoutCompletion(t *testing.T) {
	app, db, m, _, token, _ := sseFixture(t, func(int, []*schema.Message) streamPlan { return streamPlan{parts: pieces("answer")} })
	sid := createAgentHTTP(t, app, token, "create-short-001", "Agent")
	req := httptest.NewRequest("POST", "/api/v1/sessions/"+sid+"/stream", strings.NewReader(agentBody("request-short-001", "你好")))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	app.Handler.ServeHTTP(&shortPublicWriter{rec}, req)
	if rec.Body.Len() != 0 || m.CallCount() != 0 || len(agentRows(t, db, sid)) != 1 {
		t.Fatal("short write did not follow original started-write failure")
	}
}
