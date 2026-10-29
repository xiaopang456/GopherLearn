//go:build v2_entry_mysql

package isolated

import (
	"GopherAI/model"
	"GopherAI/utils"
	"context"
	"encoding/json"
	"flag"
	"github.com/cloudwego/eino/schema"
	"gorm.io/gorm"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// This gate retains the original success assertions. It needs native GET_LOCK;
// the SQLite fixture cannot stand in for MySQL's cross-instance write boundary.
// No business migration is performed. Provider/Redis transports remain offline.
var mysqlEntryConfig = flag.String("v2-entry-mysql-config", "", "explicit independent preinitialized disposable MySQL test config")

func newMySQLFixture(t *testing.T, s *Settings, respond func(context.Context, int) (*schema.Message, error)) (*Application, *gorm.DB, *captureModel, string) {
	t.Helper()
	if !strings.HasPrefix(s.Config.MysqlDatabaseName, "v2_entry_test_") || s.Entry.Migrate {
		t.Fatal("explicit preinitialized disposable v2_entry_test_* schema required; migration must be false")
	}
	db, err := ProductionInfrastructure().OpenDatabase(context.Background(), s.Config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if sqlDB, e := db.DB(); e == nil {
			sqlDB.Close()
		}
	})
	var existing int64
	for _, table := range []string{"users", "sessions", "messages", "long_term_memories", "agent_runs", "episode_memories"} {
		if err := db.Table(table).Count(&existing).Error; err != nil || existing != 0 {
			t.Fatalf("disposable schema must be initialized and empty: %s count=%d error=%v", table, existing, err)
		}
	}
	chat := &captureModel{inner: &scriptedChatModel{respond: respond}}
	migrations := 0
	chunks := &testChunks{}
	chat.chunks = chunks
	app, err := Assemble(context.Background(), s, dependencies(t, db, chat, chunks, &migrations))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Error(err)
		}
	})
	if migrations != 0 {
		t.Fatal("implicit business migration")
	}
	hash, err := utils.HashPassword("synthetic-password")
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Create(&model.User{ID: 1, Username: "offline-user", Name: "offline-user", Password: hash, Email: "user@invalid.example"}).Error; err != nil {
		t.Fatal(err)
	}
	var login struct {
		Code  int    `json:"code"`
		Token string `json:"token"`
	}
	postJSON(t, app.Handler, "/api/v1/user/login", `{"username":"offline-user","password":"synthetic-password"}`, "", &login)
	if login.Token == "" {
		t.Fatalf("native login did not issue token: %+v", login)
	}
	return app, db, chat, login.Token
}

func TestNativeGovernanceInjectionAndLearningRoute(t *testing.T) {
	s, err := Load(*mysqlEntryConfig)
	if err != nil {
		t.Fatal(err)
	}
	app, db, chat, token := newMySQLFixture(t, s, func(context.Context, int) (*schema.Message, error) {
		return schema.AssistantMessage("你好", nil), nil
	})
	var created map[string]any
	postJSON(t, app.Handler, "/api/v1/AI/agent/memories", `{"scope":"global","subject":"讲解偏好","content":"我喜欢简短讲解","durability":"stable","idempotency_key":"offline-manual-memory","reason":"用户明确输入"}`, token, &created)
	if created["memory"] == nil {
		t.Fatalf("original governance failed: %+v", created)
	}
	session := createSession(t, app, token, "create-governance")
	runHTTP(t, app, token, session, "run-governance", "你好")
	if !strings.Contains(requestText(chat), "我喜欢简短讲解") {
		t.Fatal("original global retriever was not injected")
	}
	var count int64
	db.Model(&model.LongTermMemory{}).Count(&count)
	if count != 1 {
		t.Fatal("original memory DAO was not used")
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
