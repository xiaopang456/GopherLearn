package isolated

import (
	"GopherAI/model"
	agentService "GopherAI/service/agent"
	"context"
	"encoding/json"
	"fmt"
	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"
	"gorm.io/gorm"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func assertToolContract(t *testing.T, name string, actual []map[string]any) {
	t.Helper()
	data, err := json.Marshal(actual)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("ACTUAL_TOOL_CONTRACT %s %s", name, data)
	// The snapshot is captured from a real native HTTP run through Assemble.
	// It contains full original descriptions and JSON Schemas, not just names.
	path := filepath.Join("testdata", name+".json")
	expected, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var a, b any
	if json.Unmarshal(data, &a) != nil || json.Unmarshal(expected, &b) != nil || !reflect.DeepEqual(a, b) {
		t.Fatalf("complete tool contract differs from %s: %s", name, data)
	}
}
func TestActualToolSchemasAndOriginalWriteIntentCondition(t *testing.T) {
	for _, mode := range []string{"legacy_v2", "agent_memory_source_priority_v2"} {
		for _, intent := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/write=%v", mode, intent), func(t *testing.T) {
				s := testSettings(t)
				if mode != "legacy_v2" {
					toolMode(s)
				}
				app, _, chat, token := newFixture(t, s, func(context.Context, int) (*schema.Message, error) {
					return schema.AssistantMessage("你好", nil), nil
				})
				session := createSession(t, app, token, "create-contract")
				question := "你好"
				if intent {
					question = "你好，请记住我喜欢简短讲解"
				}
				runHTTP(t, app, token, session, "run-contract", question)
				requests := chat.all()
				if len(requests) == 0 {
					t.Fatal("model was not called")
				}
				name := mode
				if intent {
					name += "-write"
				}
				assertToolContract(t, name, requests[0].Tools)
			})
		}
	}
}
func TestDraftToolUsesOriginalGovernanceAndRemainsPending(t *testing.T) {
	s := testSettings(t)
	app, db, chat, token := newFixture(t, s, func(_ context.Context, n int) (*schema.Message, error) {
		if n == 1 {
			return toolCall("propose_memory", `{"scope":"global","subject":"讲解偏好","content":"我喜欢简短讲解","durability":"stable"}`), nil
		}
		return schema.AssistantMessage("已生成待确认的记忆草稿", nil), nil
	})
	session := createSession(t, app, token, "create-draft")
	result := runHTTP(t, app, token, session, "run-draft", "请记住我喜欢简短讲解")
	if result["memoryDraft"] == nil {
		t.Fatalf("no confirmation card: %+v %s", result, requestText(chat))
	}
	var drafts []model.MemoryDraft
	if err := db.Find(&drafts).Error; err != nil {
		t.Fatal(err)
	}
	if len(drafts) != 1 || drafts[0].Status != model.MemoryDraftPending {
		t.Fatalf("original pending draft absent: %+v", drafts)
	}
	var active int64
	db.Model(&model.LongTermMemory{}).Count(&active)
	if active != 0 {
		t.Fatal("draft silently wrote active memory")
	}
}
func TestOriginalV3BudgetAssemblyConflictFailsClosed(t *testing.T) {
	s := testSettings(t)
	toolMode(s)
	s.Config.AgentConfig.MemoryReadMode = "agent_memory_source_priority_v3"
	s.Config.AgentConfig.TopicConsentSecret = ""
	s.Config.AgentConfig.MaxToolCalls = 3
	s.Config.AgentConfig.MaxModelCalls = 4
	s.Config.AgentConfig.MaxGraphSteps = 10
	s.Config.AgentConfig.MemoryToolMaxCalls = 0
	s.Config.AgentConfig.MemoryToolTopK = 0
	s.Config.AgentConfig.MemoryToolCandidateDepth = 0
	s.Config.AgentConfig.MemoryToolMinConfidence = 0
	s.Config.AgentConfig.MemoryToolTimeoutMS = 0
	db := sqliteDatabase(t)
	calls := 0
	_, err := Assemble(context.Background(), s, dependencies(t, db, &captureModel{inner: &scriptedChatModel{}}, &testChunks{}, &calls))
	if err == nil || !strings.Contains(err.Error(), "forcing the memory draft first") {
		t.Fatalf("source V3 budget conflict: %v", err)
	}
	t.Logf("original V3 startup rejection: %v", err)
	sqlDB, _ := db.DB()
	if sqlDB.Ping() == nil {
		t.Fatal("failed assembly leaked database")
	}
}

// These are negative unit tests of the guard, separate from HTTP assembly acceptance.
func TestRegistryRejectsUnknownDuplicateAndCapabilityAliases(t *testing.T) {
	for _, names := range [][]string{{"unknown_tool"}, {"knowledge_search", "knowledge_search"}, {"knowledge_search", "search_uploaded_documents"}} {
		var items []tool.BaseTool
		for _, name := range names {
			item, err := toolutils.InferTool(name, "guard fixture", func(context.Context, *struct {
				Query string `json:"query"`
			}) (string, error) {
				return "", nil
			})
			if err != nil {
				t.Fatal(err)
			}
			items = append(items, item)
		}
		_, err := (checkedRegistry{inner: agentService.StaticToolRegistry{Registered: items}}).Tools(context.Background(), agentService.RunContext{})
		if err == nil {
			t.Fatalf("accepted %v", names)
		}
	}
}
func TestOriginalGlobalMemoryRecallAndLearningRoute(t *testing.T) {
	s := testSettings(t)
	app, db, chat, token := newFixture(t, s, func(context.Context, int) (*schema.Message, error) {
		return schema.AssistantMessage("你好", nil), nil
	})
	// An initial memory fixture is read by the original SQL DAO/retriever.
	// It is not an assertion that MySQL-only confirmation succeeds on SQLite.
	memory := model.LongTermMemory{ID: "offline-global-memory", UserID: 1, Scope: model.MemoryScopeGlobal, Subject: "讲解偏好", Content: "我喜欢简短讲解", ContentHash: model.MemoryContentHash("我喜欢简短讲解"), StructuredJSON: "{}", Tags: model.StringList{}, SourceRefs: model.StringList{}, Confidence: 1, Importance: 3, Status: model.LongTermMemoryActive, UserConfirmed: true, Durability: model.MemoryDurabilityStable}
	if err := db.Create(&memory).Error; err != nil {
		t.Fatal(err)
	}
	session := createSession(t, app, token, "create-global-recall")
	runHTTP(t, app, token, session, "run-global-recall", "你好")
	if !strings.Contains(requestText(chat), "我喜欢简短讲解") {
		t.Fatal("original global retriever was not injected")
	}
	seedLearningFixture(t, db, session)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/AI/agent/learning/current?sessionId="+session, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	recorder := httptest.NewRecorder()
	app.Handler.ServeHTTP(recorder, req)
	var current map[string]any
	if json.Unmarshal(recorder.Body.Bytes(), &current) != nil || current["status_code"] != float64(1000) {
		t.Fatalf("learning route not assembled: %s", recorder.Body.String())
	}
}

func seedLearningFixture(t *testing.T, db *gorm.DB, session string) {
	t.Helper()
	activeKey := model.LearningActiveKey(1, session)
	learning := model.LearningSession{ID: "offline-learning", UserID: 1, SessionID: session, AgentRunID: "offline-learning-run", Topic: "TCP", TopicKey: "tcp", Status: model.LearningSessionCreated, CurrentQuestion: 1, TotalQuestions: 3, Version: 1, ActiveKey: &activeKey, ExpiresAt: time.Now().Add(time.Hour)}
	if err := learning.ValidateForCreate(); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&learning).Error; err != nil {
		t.Fatal(err)
	}
}
