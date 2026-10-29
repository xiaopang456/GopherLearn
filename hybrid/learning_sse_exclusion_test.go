package hybrid

import (
	"GopherAI/entry"
	"GopherAI/model"
	"GopherAI/utils"
	"context"
	"fmt"
	modelcomponent "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/xiaopang456/GopherLearn/conversation"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLearningSSEExcludedFromActualAssemblies(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		t.Run(fmt.Sprintf("hybrid=%t", mixed), func(t *testing.T) {
			db := sqliteDatabase(t)
			settings := testSettings(t)
			settings.Config.KnowledgeConfig.ScanIntervalSeconds = 3600
			script := &scriptedChatModel{respond: func(context.Context, int) (*schema.Message, error) {
				return schema.AssistantMessage("offline", nil), nil
			}}
			generation := &learningHTTPModel{scriptedChatModel: script, state: &learningModelState{}}
			migrations := 0
			deps := dependencies(t, db, script, &testChunks{}, &migrations)
			deps.ChatModel = func(context.Context, *entry.Settings) (modelcomponent.ToolCallingChatModel, error) {
				return generation, nil
			}
			var handler http.Handler
			if mixed {
				app, err := Assemble(context.Background(), settings, deps, &ordinaryModel{}, conversation.Options{})
				if err != nil {
					t.Fatal(err)
				}
				handler = app.Handler
				t.Cleanup(func() {
					if err := app.Close(); err != nil {
						t.Error(err)
					}
				})
			} else {
				app, err := entry.Assemble(context.Background(), settings, deps)
				if err != nil {
					t.Fatal(err)
				}
				handler = app.Handler
				t.Cleanup(func() {
					if err := app.Close(); err != nil {
						t.Error(err)
					}
				})
			}
			if migrations != 0 {
				t.Fatal("unexpected business migration")
			}
			password, err := utils.HashPassword("synthetic-password")
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Create(&model.User{ID: 1, Username: "learner", Name: "learner", Email: "learner@invalid.example", Password: password}).Error; err != nil {
				t.Fatal(err)
			}
			status, login, _ := requestHTTP(t, handler, "POST", "/api/v1/user/login", `{"username":"learner","password":"synthetic-password"}`, "")
			token, ok := login["token"].(string)
			if status != http.StatusOK || !ok || token == "" {
				t.Fatalf("native login: %d %v", status, login)
			}
			_, created, _ := requestHTTP(t, handler, "POST", "/api/v1/AI/chat/session", `{"modelType":"agent","requestId":"exclude-learning-sse"}`, token)
			sid, ok := created["sessionId"].(string)
			if !ok || sid == "" {
				t.Fatal(created)
			}
			body := learningJSON(map[string]string{"sessionId": sid, "topic": "Redis"})
			for _, auth := range []string{token, "", "invalid-token"} {
				req := httptest.NewRequest(http.MethodPost, learningPath+"start-stream", strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				if auth != "" {
					req.Header.Set("Authorization", "Bearer "+auth)
				}
				writer := httptest.NewRecorder()
				handler.ServeHTTP(writer, req)
				if writer.Code != http.StatusNotFound || strings.Contains(writer.Header().Get("Content-Type"), "text/event-stream") || writer.Flushed || writer.Body.Len() != 0 {
					t.Fatalf("excluded route: status=%d headers=%v flushed=%v body=%q", writer.Code, writer.Header(), writer.Flushed, writer.Body.String())
				}
				t.Logf("authenticated=%t status=%d body empty, no SSE/Flush", auth == token, writer.Code)
				for _, table := range []any{&model.LearningSession{}, &model.LearningQuestion{}, &model.LearningAttempt{}, &model.TopicProgress{}} {
					var count int64
					if err := db.Model(table).Count(&count).Error; err != nil || count != 0 {
						t.Fatalf("learning side effect %T: %d %v", table, count, err)
					}
				}
				generation.state.mu.Lock()
				calls := generation.state.generationCalls + generation.state.gradingCalls
				generation.state.mu.Unlock()
				if calls != 0 || script.CallCount() != 0 {
					t.Fatal("excluded route called model")
				}
			}
			// Non-streaming current still passes original auth and reaches the
			// original controller; it is not intercepted by the exclusion.
			status, current, _ := requestHTTP(t, handler, "GET", learningPath+"current?sessionId="+sid, "", token)
			if status != http.StatusOK || current["status_code"] == nil {
				t.Fatalf("current route lost: %d %v", status, current)
			}
		})
	}
}
