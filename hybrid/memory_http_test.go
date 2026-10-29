package hybrid

import (
	memoryDAO "GopherAI/dao/memory"
	"GopherAI/model"
	"context"
	"encoding/json"
	"github.com/cloudwego/eino/schema"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

const nativeMemoryPath = "/api/v1/AI/agent/memories"
const nativeDraftPath = "/api/v1/AI/agent/memory-drafts/"

func nativeMemoryHTTP(t *testing.T, h http.Handler, method, path, body, token string) map[string]any {
	t.Helper()
	status, result, _ := requestHTTP(t, h, method, path, body, token)
	if status != 200 || result["status_code"] != float64(1000) {
		t.Fatalf("native %s %s: %d %v", method, path, status, result)
	}
	return result
}

func TestMemoryNativeHTTPOriginalLifecycleAndTombstone(t *testing.T) {
	app, db, _, _, token, other := httpFixture(t)
	body := `{"scope":"topic","topic_key":"redis","subject":"个人理解","content":"我用队列类比 Redis","durability":"stable","reason":"用户明确填写","idempotency_key":"manual-memory-001"}`
	created := nativeMemoryHTTP(t, app.Handler, "POST", nativeMemoryPath, body, token)
	row := created["memory"].(map[string]any)
	id := row["id"].(string)
	if row["scope"] != "topic" || row["topic_key"] != "redis" || row["status"] != "active" || row["user_confirmed"] != true {
		t.Fatalf("original handwritten state %v", row)
	}
	replay := nativeMemoryHTTP(t, app.Handler, "POST", nativeMemoryPath, body, token)
	if replay["replay"] != true || replay["memory"].(map[string]any)["id"] != id {
		t.Fatal("explicit replay changed original identity")
	}
	list := nativeMemoryHTTP(t, app.Handler, "GET", nativeMemoryPath+"?scope=topic&status=active", "", token)
	if len(list["memories"].([]any)) != 1 {
		t.Fatal(list)
	}
	foreign := nativeMemoryHTTP(t, app.Handler, "GET", nativeMemoryPath, "", other)
	if len(foreign["memories"].([]any)) != 0 {
		t.Fatal("cross-user list")
	}
	for _, op := range []struct{ method, path, body string }{{"PATCH", "", `{"subject":"理解","content":"修改","tags":["redis"],"reason":"修正"}`}, {"POST", "/archive", `{"reason":"归档"}`}, {"DELETE", "", `{"reason":"删除"}`}} {
		status, _, _ := requestHTTP(t, app.Handler, op.method, nativeMemoryPath+"/"+id+op.path, op.body, other)
		if status != 404 {
			t.Fatalf("cross-user %s %d", op.method, status)
		}
	}
	patched := nativeMemoryHTTP(t, app.Handler, "PATCH", nativeMemoryPath+"/"+id, `{"subject":"个人理解","content":"我用任务队列类比 Redis","tags":["redis"],"reason":"修正比喻"}`, token)
	if patched["memory"].(map[string]any)["content"] != "我用任务队列类比 Redis" {
		t.Fatal(patched)
	}
	revisions := nativeMemoryHTTP(t, app.Handler, "GET", nativeMemoryPath+"/"+id+"/revisions", "", token)
	if len(revisions["revisions"].([]any)) != 2 {
		t.Fatal(revisions)
	}
	archived := nativeMemoryHTTP(t, app.Handler, "POST", nativeMemoryPath+"/"+id+"/archive", `{"reason":"暂不使用"}`, token)
	if archived["memory"].(map[string]any)["status"] != "archived" {
		t.Fatal(archived)
	}
	status, _, _ := requestHTTP(t, app.Handler, "POST", nativeMemoryPath+"/"+id+"/archive", `{"reason":"再次归档"}`, token)
	if status != 404 {
		t.Fatalf("original inactive archive %d", status)
	}
	nativeMemoryHTTP(t, app.Handler, "DELETE", nativeMemoryPath+"/"+id, `{"reason":"不再保留"}`, token)
	nativeMemoryHTTP(t, app.Handler, "DELETE", nativeMemoryPath+"/"+id, `{"reason":"明确重复删除"}`, token)
	var stored model.LongTermMemory
	if err := db.Where("id = ?", id).First(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Status != model.LongTermMemoryDeleted || stored.Content != "" || stored.Subject != "" || stored.TombstoneHash == "" || stored.DeletedAt == nil {
		t.Fatal("original tombstone lost")
	}
	deleted, err := memoryDAO.NewRepository(db).HasDeletedHash(context.Background(), 1, model.MemoryScopeTopic, stored.TombstoneHash)
	if err != nil || !deleted {
		t.Fatalf("original resurrection filter %v %v", deleted, err)
	}
	for _, bad := range []struct {
		body   string
		status int
	}{
		// A body that does not bind is a client error: the fields it was
		// missing are named by the request, not by the server's state. The
		// answer used to be 500, which sent a caller looking for a server
		// problem while the request was simply incomplete.
		{`{"scope":"topic","topic_key":"redis","content":"clean-only body"}`, 400},
		{`{"scope":"topic","topic_key":"redis","subject":"过期","content":"过期目标","durability":"time_bound","valid_until":"2000-01-01T00:00:00Z","reason":"用户填写","idempotency_key":"expired-input"}`, 400},
	} {
		status, _, _ := requestHTTP(t, app.Handler, "POST", nativeMemoryPath, bad.body, token)
		if status != bad.status {
			t.Fatalf("invalid native write %d", status)
		}
	}
}

func TestMemoryNativeHTTPAgentDraftReplayAndDecision(t *testing.T) {
	for _, action := range []string{"add", "cancel"} {
		t.Run(action, func(t *testing.T) {
			app, db, _, agent, token, other := httpFixture(t)
			agent.respond = func(_ context.Context, n int) (*schema.Message, error) {
				if n == 1 {
					return &schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "actual-draft", Type: "function", Function: schema.FunctionCall{Name: "propose_memory", Arguments: `{"scope":"topic","topic_key":"redis","subject":"个人理解","content":"我用队列理解 Redis","durability":"stable"}`}}}}, nil
				}
				return schema.AssistantMessage("已生成待确认草稿", nil), nil
			}
			sid := createAgentHTTP(t, app, token, "create-native-memory-001", "Agent")
			path := "/api/v1/sessions/" + sid + "/messages"
			body := agentBody("send-native-memory-001", "请记住我用队列理解 Redis")
			result := dataHTTP(t, app.Handler, "POST", path, body, token)
			card := result["memoryDraft"].(map[string]any)
			id := card["draftId"].(string)
			var draft model.MemoryDraft
			if err := db.Where("id = ?", id).First(&draft).Error; err != nil || draft.Status != model.MemoryDraftPending {
				t.Fatalf("actual draft %v %v", draft, err)
			}
			calls := agent.CallCount()
			repeated := dataHTTP(t, app.Handler, "POST", path, body, token)
			if repeated["replayed"] != true || agent.CallCount() != calls {
				t.Fatal("Agent replay generated a new draft")
			}
			resolve := nativeDraftPath + id + "/resolve"
			statusMissing, _, _ := requestHTTP(t, app.Handler, "POST", nativeDraftPath+"missing/resolve", `{"action":"add","reason":"明确确认"}`, token)
			if statusMissing != 404 {
				t.Fatalf("missing draft %d", statusMissing)
			}
			conflictStatus, _, _ := requestHTTP(t, app.Handler, "POST", resolve, `{"action":"replace","target_memory_id":"not-authorized","reason":"冲突操作"}`, token)
			if conflictStatus != 409 {
				t.Fatalf("unapproved replacement target %d", conflictStatus)
			}
			decision, _ := json.Marshal(map[string]string{"action": action, "reason": "用户明确决定"})
			status, _, _ := requestHTTP(t, app.Handler, "POST", resolve, string(decision), other)
			if status != 404 {
				t.Fatalf("foreign draft %d", status)
			}
			decided := nativeMemoryHTTP(t, app.Handler, "POST", resolve, string(decision), token)
			wanted := "confirmed"
			if action == "cancel" {
				wanted = "cancelled"
			}
			if decided["draft"].(map[string]any)["status"] != wanted {
				t.Fatal(decided)
			}
			list := nativeMemoryHTTP(t, app.Handler, "GET", nativeMemoryPath, "", token)
			count := len(list["memories"].([]any))
			if (action == "add" && count != 1) || (action == "cancel" && count != 0) {
				t.Fatal(list)
			}
			// An old cancel card can receive confirmed/replay. Do not infer the state from the clicked action.
			again := nativeMemoryHTTP(t, app.Handler, "POST", resolve, `{"action":"cancel","reason":"旧卡明确操作"}`, token)
			if again["draft"].(map[string]any)["status"] != wanted {
				t.Fatal("old card changed decided state")
			}
			if action == "add" && again["replay"] != true {
				t.Fatal("original confirmation replay lost")
			}
			if agent.CallCount() != calls {
				t.Fatal("governance reran Agent")
			}
			if action == "add" {
				memoryID := decided["memory"].(map[string]any)["id"].(string)
				nativeMemoryHTTP(t, app.Handler, "DELETE", nativeMemoryPath+"/"+memoryID, `{"reason":"用户删除"}`, token)
				old := nativeMemoryHTTP(t, app.Handler, "POST", resolve, `{"action":"add","reason":"旧卡明确重放"}`, token)
				if old["replay"] != true || old["memory"].(map[string]any)["status"] != "deleted" {
					t.Fatal("old confirmed card resurrected memory")
				}
			}
		})
	}
}

func TestMemoryNativeHTTPGlobalLockRemainsExternalBoundary(t *testing.T) {
	app, db, _, _, token, _ := httpFixture(t)
	status, _, _ := requestHTTP(t, app.Handler, "POST", nativeMemoryPath, `{"scope":"global","subject":"偏好","content":"喜欢简短讲解","durability":"stable","reason":"明确填写","idempotency_key":"global-lock-boundary"}`, token)
	if status != 500 {
		t.Fatalf("SQLite must not emulate native MySQL GET_LOCK: %d", status)
	}
	var count int64
	db.Model(&model.LongTermMemory{}).Count(&count)
	if count != 0 {
		t.Fatal("failed original lock left memory")
	}
}

func TestMemoryNativeFrontendAPIThroughSocket(t *testing.T) {
	app, _, _, _, _, _ := httpFixture(t)
	server := httptest.NewServer(app.Handler)
	defer server.Close()
	script, err := filepath.Abs("../web/test/helpers/hybridMemoryClient.mjs")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("node", script)
	cmd.Env = append(os.Environ(), "HYBRID_MEMORY_URL="+server.URL)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("actual frontend native API: %v\n%s", err, output)
	}
}

func TestMemoryNativeHTTPExpiredDraftStaysPending(t *testing.T) {
	app, db, _, agent, token, _ := httpFixture(t)
	agent.respond = func(_ context.Context, n int) (*schema.Message, error) {
		if n == 1 {
			return &schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "expiring-draft", Type: "function", Function: schema.FunctionCall{Name: "propose_memory", Arguments: `{"scope":"topic","topic_key":"redis","subject":"个人目标","content":"我计划练习 Redis 队列","durability":"time_bound","valid_until":"2030-01-01T00:00:00Z"}`}}}}, nil
		}
		return schema.AssistantMessage("已生成待确认草稿", nil), nil
	}
	sid := createAgentHTTP(t, app, token, "create-expiring-draft-001", "Agent")
	result := dataHTTP(t, app.Handler, "POST", "/api/v1/sessions/"+sid+"/messages", agentBody("send-expiring-draft-001", "请记住我计划练习 Redis 队列"), token)
	id := result["memoryDraft"].(map[string]any)["draftId"].(string)
	// Offline time-expiry injection on the actual original tool-created draft,
	// not a manually constructed draft/history or a new confirmation rule.
	expired := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := db.Model(&model.MemoryDraft{}).Where("id = ?", id).Updates(map[string]any{"valid_until": expired, "created_at": expired.Add(-time.Hour)}).Error; err != nil {
		t.Fatal(err)
	}
	status, _, _ := requestHTTP(t, app.Handler, "POST", nativeDraftPath+id+"/resolve", `{"action":"add","reason":"旧卡明确确认"}`, token)
	// Original topic confirmation reaches Validate in DAO and returns an
	// untyped validation error through the native 500 envelope. Preserve it.
	if status != 500 {
		t.Fatalf("original expired topic draft %d", status)
	}
	var draft model.MemoryDraft
	db.Where("id = ?", id).First(&draft)
	var count int64
	db.Model(&model.LongTermMemory{}).Count(&count)
	if draft.Status != model.MemoryDraftPending || count != 0 {
		t.Fatal("expiry failure changed draft or wrote memory")
	}
}
