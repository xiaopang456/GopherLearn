package hybrid

import (
	"GopherAI/common/rag"
	"GopherAI/config"
	messageDAO "GopherAI/dao/message"
	sessionDAO "GopherAI/dao/session"
	"GopherAI/model"
	"GopherAI/utils"
	"context"
	"encoding/json"
	"fmt"
	"github.com/cloudwego/eino/schema"
	"github.com/xiaopang456/GopherLearn/conversation"
	"gorm.io/gorm"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func requestHTTP(t *testing.T, h http.Handler, method, path, body, token string) (int, map[string]any, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var envelope map[string]any
	if strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
			t.Fatalf("%s: %s", path, w.Body.String())
		}
	}
	return w.Code, envelope, w.Body.String()
}
func dataHTTP(t *testing.T, h http.Handler, method, path, body, token string) map[string]any {
	t.Helper()
	status, envelope, _ := requestHTTP(t, h, method, path, body, token)
	if status != 200 || envelope["error"] != nil {
		t.Fatalf("%s %s: %d %v", method, path, status, envelope)
	}
	data, ok := envelope["data"].(map[string]any)
	if !ok {
		t.Fatalf("%s data: %v", path, envelope)
	}
	return data
}
func loginHTTP(t *testing.T, h http.Handler, email string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"email": email, "password": "synthetic-password"})
	return dataHTTP(t, h, "POST", "/api/v1/auth/login", string(body), "")["token"].(string)
}
func createHTTP(t *testing.T, h http.Handler, token, title string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"mode": "chat", "title": title})
	return dataHTTP(t, h, "POST", "/api/v1/sessions", string(body), token)["id"].(string)
}
func httpFixture(t *testing.T) (*Application, *gorm.DB, *ordinaryModel, *scriptedChatModel, string, string) {
	return httpFixtureWithModel(t, nil)
}
func httpFixtureWithModel(t *testing.T, factory func(*ordinaryModel) conversation.Model, transport ...*httpRedisTransport) (*Application, *gorm.DB, *ordinaryModel, *scriptedChatModel, string, string) {
	t.Helper()
	db := sqliteDatabase(t)
	m := &ordinaryModel{}
	agent := &scriptedChatModel{respond: func(context.Context, int) (*schema.Message, error) {
		return schema.AssistantMessage("agent response", nil), nil
	}}
	var chat conversation.Model = m
	if factory != nil {
		chat = factory(m)
	}
	migrations := 0
	deps := dependencies(t, db, agent, &testChunks{}, &migrations)
	if len(transport) > 0 {
		deps.Infrastructure.OpenRedis = transport[0].open
	}
	app, err := Assemble(context.Background(), testSettings(t), deps, chat, conversation.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Error(err)
		}
	})
	if migrations != 0 {
		t.Fatal("business migration")
	}
	hash, err := utils.HashPassword("synthetic-password")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []model.User{{ID: 1, Username: "learner", Name: "learner", Email: "learner@invalid.example", Password: hash}, {ID: 2, Username: "other", Name: "other", Email: "other@invalid.example", Password: hash}} {
		if err := db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
	}
	return app, db, m, agent, loginHTTP(t, app.Handler, "learner@invalid.example"), loginHTTP(t, app.Handler, "other@invalid.example")
}

func TestHTTPLoginMeOrdinarySendStreamAndOriginalRecords(t *testing.T) {
	app, db, m, agent, token, _ := httpFixture(t)
	me := dataHTTP(t, app.Handler, "GET", "/api/v1/me", "", token)
	if me["id"] != "1" || me["email"] != "learner@invalid.example" {
		t.Fatalf("original identity %v", me)
	}
	sid := createHTTP(t, app.Handler, token, "真实 HTTP")
	session, err := sessionDAO.NewRepository(db).GetByIDAndUserID(context.Background(), sid, 1)
	if err != nil || session.Mode != model.SessionModeNormal || session.Title != "真实 HTTP" {
		t.Fatalf("original Session %v %v", session, err)
	}
	sent := dataHTTP(t, app.Handler, "POST", "/api/v1/sessions/"+sid+"/messages", `{"content":"第一问"}`, token)
	status, _, stream := requestHTTP(t, app.Handler, "POST", "/api/v1/sessions/"+sid+"/stream", `{"content":"第二问"}`, token)
	if status != 200 || !strings.Contains(stream, "event: message.completed\n") || strings.Contains(stream, "final_block_id") {
		t.Fatalf("ordinary SSE %d %s", status, stream)
	}
	checkHistory(t, m.all()[1], "第一问", "第二问")
	page, err := messageDAO.NewRepository(db).ListOwnedHistoryPage(context.Background(), 1, sid, 0, 100)
	if err != nil || len(page.Messages) != 4 {
		t.Fatalf("original messages %v %v", page, err)
	}
	if sent["user_message"].(map[string]any)["id"] != fmt.Sprint(page.Messages[0].ID) || sent["assistant_message"].(map[string]any)["id"] != fmt.Sprint(page.Messages[1].ID) {
		t.Fatal("HTTP IDs differ from original DAO")
	}
	history := dataHTTP(t, app.Handler, "GET", "/api/v1/sessions/"+sid+"/messages", "", token)["items"].([]any)
	for i, item := range history {
		row := item.(map[string]any)
		if row["id"] != fmt.Sprint(page.Messages[i].ID) || row["content"] != page.Messages[i].Content {
			t.Fatalf("HTTP history differs %v", row)
		}
	}
	if agent.CallCount() != 0 {
		t.Fatal("ordinary HTTP reached Runner")
	}
	for _, r := range m.all() {
		if len(r.Tools) != 0 {
			t.Fatal("ordinary HTTP tools")
		}
		for _, msg := range r.Messages {
			if strings.Contains(msg.Content, "跨会话经历") || strings.Contains(msg.Content, "已确认记忆") {
				t.Fatal("Agent memory injected")
			}
		}
	}
	listed := dataHTTP(t, app.Handler, "GET", "/api/v1/sessions", "", token)["items"].([]any)
	found := false
	for _, item := range listed {
		if row := item.(map[string]any); row["id"] == sid {
			found = row["mode"] == "chat" && row["user_id"] == "1"
		}
	}
	if !found {
		t.Fatal("created session not listed with explicit mode")
	}
	// The same combined HTTP entry still routes the original V2 Agent unchanged.
	var native struct {
		SessionID string `json:"sessionId"`
	}
	st, _, body := requestHTTP(t, app.Handler, "POST", "/api/v1/AI/chat/session", `{"modelType":"agent","requestId":"http-native-create"}`, token)
	if st != 200 || json.Unmarshal([]byte(body), &native) != nil || native.SessionID == "" {
		t.Fatalf("native creation %s", body)
	}
	st, _, body = requestHTTP(t, app.Handler, "POST", "/api/v1/AI/agent/run", fmt.Sprintf(`{"sessionId":%q,"requestId":"http-native-run","question":"你好","knowledgePolicy":"knowledge_preferred"}`, native.SessionID), token)
	if st != 200 || !strings.Contains(body, `"answer":"agent response"`) || agent.CallCount() == 0 {
		t.Fatalf("native Agent %s", body)
	}
}

func TestHTTPRejectsUntrustedIdentityTokenModeAndUnconnectedCapabilities(t *testing.T) {
	app, db, m, _, token, other := httpFixture(t)
	sid := createHTTP(t, app.Handler, token, "protected")
	for _, tc := range []struct {
		method, path, body, token string
		status                    int
		code                      string
	}{
		{"GET", "/api/v1/me", "", "invalid", 401, "unauthorized"},
		{"GET", "/api/v1/me", "", "", 401, "unauthorized"},
		{"GET", "/api/v1/sessions/" + sid + "/messages", "", other, 404, "not_found"},
		{"POST", "/api/v1/sessions/" + sid + "/messages", `{"content":"x"}`, other, 404, "not_found"},
		{"POST", "/api/v1/sessions/" + sid + "/stream", `{"content":"x"}`, other, 404, "not_found"},
		{"DELETE", "/api/v1/sessions/" + sid, "", other, 404, "not_found"},
		{"GET", "/api/v1/me?user_id=2", "", token, 400, "invalid_request"},
		{"POST", "/api/v1/sessions", `{"mode":"normal"}`, token, 400, "invalid_request"},
		{"POST", "/api/v1/sessions", `{"mode":"learning_agent"}`, token, 400, "invalid_request"},
		{"POST", "/api/v1/sessions", `{"mode":"unknown"}`, token, 400, "invalid_request"},
		{"POST", "/api/v1/sessions", `{"mode":"chat","user_id":"2"}`, token, 400, "invalid_request"},
		{"POST", "/api/v1/sessions/" + sid + "/messages", `{"content":"x","user_id":"2"}`, token, 400, "invalid_request"},
		{"POST", "/api/v1/sessions/" + sid + "/messages", `{"content":"x","mode":"learning_agent"}`, token, 400, "invalid_request"},
		{"POST", "/api/v1/sessions/" + sid + "/stream", `{"content":"x","stream_steps":true}`, token, 400, "invalid_request"},
		{"POST", "/api/v1/sessions/" + sid + "/messages", `{"content":"x","knowledge":{"enabled":true,"collection_ids":["anything"]}}`, token, 400, "invalid_request"},
		{"POST", "/api/v1/auth/login", `{"email":"learner@invalid.example","password":"wrong"}`, "", 401, "invalid_credentials"},
		{"POST", "/api/v1/auth/register", `{"email":"new@invalid.example","password":"synthetic-password","display_name":"new"}`, "", 501, "registration_not_adapted"},
	} {
		t.Run(tc.path+tc.body, func(t *testing.T) {
			status, e, _ := requestHTTP(t, app.Handler, tc.method, tc.path, tc.body, tc.token)
			if status != tc.status || e["error"].(map[string]any)["code"] != tc.code {
				t.Fatalf("reject %d %v", status, e)
			}
		})
	}
	if err := db.Create(&model.Session{ID: "http-agent", UserID: 1, UserName: "learner", Mode: model.SessionModeAgent}).Error; err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"/messages", "/stream"} {
		status, e, _ := requestHTTP(t, app.Handler, "POST", "/api/v1/sessions/http-agent"+suffix, `{"content":"cannot switch"}`, token)
		if status != 400 || e["error"] == nil {
			t.Fatalf("stored mode bypass %d %v", status, e)
		}
	}
	if len(m.all()) != 0 {
		t.Fatal("invalid input reached model")
	}
	var count int64
	db.Model(&model.Message{}).Count(&count)
	if count != 0 {
		t.Fatal("rejections persisted messages")
	}
	// Expired token comes from a real original Login HTTP call, no claims fabricated.
	conf := config.GetConfig()
	old := conf.JwtConfig.ExpireDuration
	conf.JwtConfig.ExpireDuration = -1
	_, _, native := requestHTTP(t, app.Handler, "POST", "/api/v1/user/login", `{"username":"learner","password":"synthetic-password"}`, "")
	conf.JwtConfig.ExpireDuration = old
	var logged map[string]any
	if err := json.Unmarshal([]byte(native), &logged); err != nil {
		t.Fatal(err)
	}
	expired, ok := logged["token"].(string)
	if !ok {
		t.Fatalf("original expired Login token missing %s", native)
	}
	status, e, _ := requestHTTP(t, app.Handler, "GET", "/api/v1/me", "", expired)
	if status != 401 || e["error"].(map[string]any)["code"] != "unauthorized" {
		t.Fatalf("expired token %d %v", status, e)
	}
	if err := db.Where("id = ?", 1).Delete(&model.User{}).Error; err != nil {
		t.Fatal(err)
	}
	status, e, _ = requestHTTP(t, app.Handler, "GET", "/api/v1/me", "", token)
	if status != 401 || e["error"].(map[string]any)["code"] != "unauthorized" {
		t.Fatalf("missing original User %d %v", status, e)
	}
}

func TestHTTPFullNumericHistoryReferencesAndLargeIdentity(t *testing.T) {
	app, db, _, _, token, _ := httpFixture(t)
	sid := createHTTP(t, app.Handler, token, "all pages")
	for i := 0; i < 51; i++ {
		dataHTTP(t, app.Handler, "POST", "/api/v1/sessions/"+sid+"/messages", fmt.Sprintf(`{"content":"q%d"}`, i), token)
	}
	// Citation fixture lives in the original Message row, never a fabricated History.
	source := rag.Source{Citation: 7, DocID: "original-doc", DocVersion: 3, FileName: "原文.md", TitlePath: []string{"章", "节"}, StartLine: 4, EndLine: 9, ChunkIndex: 2, Snippet: "原证据", Score: .8}
	var first model.Message
	if err := db.Where("session_id = ? AND is_user = ?", sid, false).Order("id ASC").First(&first).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&first).UpdateColumn("sources", model.MessageSources{source}).Error; err != nil {
		t.Fatal(err)
	}
	history := dataHTTP(t, app.Handler, "GET", "/api/v1/sessions/"+sid+"/messages", "", token)["items"].([]any)
	if len(history) != 102 {
		t.Fatalf("first-page truncation: %d", len(history))
	}
	for i, item := range history {
		row := item.(map[string]any)
		want := fmt.Sprintf("q%d", i/2)
		if i%2 == 1 {
			want = "回答：" + want
		}
		if row["content"] != want {
			t.Fatalf("numeric history %d: %v", i, row)
		}
	}
	reference := history[1].(map[string]any)["sources"].([]any)[0].(map[string]any)
	if reference["source_id"] != "7" || reference["document_id"] != "original-doc" || reference["snippet"] != "原证据" || reference["original_v2"].(map[string]any)["start_line"] != float64(4) {
		t.Fatalf("reference lost: %v", reference)
	}
	const big = int64(9007199254740993)
	if err := db.Model(&model.User{}).Where("id = ?", 1).UpdateColumn("id", big).Error; err != nil {
		t.Fatal(err)
	}
	bigToken := loginHTTP(t, app.Handler, "learner@invalid.example")
	me := dataHTTP(t, app.Handler, "GET", "/api/v1/me", "", bigToken)
	if me["id"] != "9007199254740993" {
		t.Fatalf("identity precision lost: %v", me)
	}
	bigSID := createHTTP(t, app.Handler, bigToken, "big identity")
	if err := db.Exec("UPDATE sqlite_sequence SET seq = ? WHERE name = 'messages'", big).Error; err != nil {
		t.Fatal(err)
	}
	got := dataHTTP(t, app.Handler, "POST", "/api/v1/sessions/"+bigSID+"/messages", `{"content":"big"}`, bigToken)
	if id, ok := got["assistant_message"].(map[string]any)["id"].(string); !ok || id != "9007199254740995" {
		t.Fatal("message ID is numeric JSON")
	}
}
