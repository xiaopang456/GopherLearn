package hybrid

import (
	messageDAO "GopherAI/dao/message"
	"GopherAI/model"
	"GopherAI/utils"
	"context"
	"encoding/json"
	"fmt"
	"github.com/cloudwego/eino/schema"
	"gorm.io/gorm"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func fixture(t *testing.T) (*Application, *gorm.DB, *scriptedChatModel) {
	t.Helper()
	db := sqliteDatabase(t)
	agent := &scriptedChatModel{respond: func(context.Context, int) (*schema.Message, error) {
		return schema.AssistantMessage("agent response", nil), nil
	}}
	migrations := 0
	app, err := Assemble(context.Background(), testSettings(t), dependencies(t, db, agent, &testChunks{}, &migrations))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Error(err)
		}
	})
	if migrations != 0 {
		t.Fatal("business migration executed")
	}
	hash, err := utils.HashPassword("synthetic-password")
	if err != nil {
		t.Fatal(err)
	}
	for _, user := range []model.User{{ID: 1, Username: "learner", Name: "learner", Password: hash}, {ID: 2, Username: "other", Name: "other"}} {
		if err := db.Create(&user).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Create(&model.Session{ID: "ordinary", UserID: 1, UserName: "learner", Mode: model.SessionModeNormal, Title: "ordinary"}).Error; err != nil {
		t.Fatal(err)
	}
	return app, db, agent
}

// TestAssembleRequiresExplicitDependenciesAndPreservesNativeAgentRoute pins the
// two halves of the assembly: the native routes still answer, and the entry
// keeps serving them itself rather than handing the whole surface to the
// Agent's own handler.
func TestAssembleRequiresExplicitDependenciesAndPreservesNativeAgentRoute(t *testing.T) {
	app, db, agent := fixture(t)
	post := func(path, body, token string) map[string]any {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		app.Agent.Handler.ServeHTTP(w, req)
		var result map[string]any
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
			t.Fatalf("native %s: %d %s", path, w.Code, w.Body.String())
		}
		return result
	}
	login := post("/api/v1/user/login", `{"username":"learner","password":"synthetic-password"}`, "")
	token, _ := login["token"].(string)
	if token == "" {
		t.Fatalf("original User authentication: %v", login)
	}
	created := post("/api/v1/AI/chat/session", `{"modelType":"agent","requestId":"hybrid-create-agent"}`, token)
	sid, _ := created["sessionId"].(string)
	if sid == "" {
		t.Fatalf("original Session creation: %v", created)
	}
	result := post("/api/v1/AI/agent/run", fmt.Sprintf(`{"sessionId":%q,"requestId":"hybrid-agent-run","question":"你好","knowledgePolicy":"knowledge_preferred"}`, sid), token)
	if result["answer"] != "agent response" {
		t.Fatalf("original Agent execution: %v", result)
	}
	page, err := messageDAO.NewRepository(db).ListOwnedHistoryPage(context.Background(), 1, sid, 0, 100)
	if err != nil || len(page.Messages) != 2 || agent.CallCount() == 0 {
		t.Fatalf("shared V2 storage/isolated execution: %+v err=%v", page, err)
	}
}
