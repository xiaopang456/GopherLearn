package isolated

import (
	"GopherAI/common/rag"
	"GopherAI/config"
	"GopherAI/model"
	"GopherAI/utils"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/BurntSushi/toml"
	"github.com/cloudwego/eino/components/embedding"
	modelcomponent "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/glebarez/sqlite"
	redisclient "github.com/redis/go-redis/v9"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

type capturedRequest struct {
	Messages []*schema.Message
	Tools    []map[string]any
}
type captureModel struct {
	inner     *scriptedChatModel
	mu        sync.Mutex
	infos     []*schema.ToolInfo
	chunks    *testChunks
	requests  []capturedRequest
	collector *captureModel
}

func (m *captureModel) record(input []*schema.Message) {
	root := m.root()
	root.mu.Lock()
	defer root.mu.Unlock()
	r := capturedRequest{Messages: append([]*schema.Message(nil), input...)}
	for _, info := range m.infos {
		j, err := info.ParamsOneOf.ToJSONSchema()
		if err != nil {
			panic(err)
		}
		r.Tools = append(r.Tools, map[string]any{"name": info.Name, "description": info.Desc, "schema": j})
	}
	root.requests = append(root.requests, r)
}
func (m *captureModel) root() *captureModel {
	if m.collector != nil {
		return m.collector
	}
	return m
}
func (m *captureModel) WithTools(infos []*schema.ToolInfo) (modelcomponent.ToolCallingChatModel, error) {
	_, err := m.inner.WithTools(infos)
	return &captureModel{inner: m.inner, infos: append([]*schema.ToolInfo(nil), infos...), chunks: m.chunks, collector: m.root()}, err
}
func (m *captureModel) Generate(ctx context.Context, input []*schema.Message, opts ...modelcomponent.Option) (*schema.Message, error) {
	m.record(input)
	return m.inner.Generate(ctx, input, opts...)
}
func (m *captureModel) Stream(ctx context.Context, input []*schema.Message, opts ...modelcomponent.Option) (*schema.StreamReader[*schema.Message], error) {
	m.record(input)
	return m.inner.Stream(ctx, input, opts...)
}
func (m *captureModel) all() []capturedRequest {
	root := m.root()
	root.mu.Lock()
	defer root.mu.Unlock()
	return append([]capturedRequest(nil), root.requests...)
}

// Only vector/embedding transport is substituted. SQL business repositories,
// source files, ContextBuilder, grants, Runner, workers and controllers are real.
type testChunks struct{ rows []rag.RedisChunkSearchResult }

func (*testChunks) EnsureIndex(context.Context) error                { return nil }
func (*testChunks) Store(context.Context, []rag.EmbeddedChunk) error { return nil }
func (*testChunks) LoadDocumentVersionEmbeddings(context.Context, int64, string, int) ([]rag.StoredChunkEmbedding, error) {
	return nil, nil
}
func (f *testChunks) SearchActiveByUserVector(context.Context, int64, []float32, int, map[string]int) ([]rag.RedisChunkSearchResult, error) {
	return f.rows, nil
}
func (f *testChunks) SearchActiveByUserText(context.Context, int64, string, int, map[string]int) ([]rag.RedisChunkSearchResult, error) {
	return f.rows, nil
}
func (*testChunks) DeleteDocumentVersion(context.Context, int64, string, int) error { return nil }

type redisTransport struct{}

func (redisTransport) DialHook(redisclient.DialHook) redisclient.DialHook {
	return func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("offline test forbids Redis dialing")
	}
}
func (redisTransport) ProcessHook(redisclient.ProcessHook) redisclient.ProcessHook {
	return func(_ context.Context, cmd redisclient.Cmder) error {
		if cmd.Name() == "ft.info" {
			err := errors.New("unknown index name")
			cmd.SetErr(err)
			return err
		}
		switch c := cmd.(type) {
		case *redisclient.StatusCmd:
			c.SetVal("OK")
		case *redisclient.Cmd:
			c.SetVal([]any{})
		case *redisclient.StringSliceCmd:
			c.SetVal([]string{})
		default:
			err := fmt.Errorf("offline transport unsupported %T %s", cmd, cmd.Name())
			cmd.SetErr(err)
			return err
		}
		return nil
	}
}
func (h redisTransport) ProcessPipelineHook(redisclient.ProcessPipelineHook) redisclient.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redisclient.Cmder) error {
		for _, cmd := range cmds {
			if err := h.ProcessHook(nil)(ctx, cmd); err != nil {
				return err
			}
		}
		return nil
	}
}

func testSettings(t *testing.T) *Settings {
	t.Helper()
	data, err := os.ReadFile("../../../tutorial/v2-entry/isolated.example.toml")
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ReplaceAll(string(data), `key = ""`, `key = "synthetic-isolated-test-key-at-least-32-characters"`)
	text = strings.ReplaceAll(text, `chatKey = ""`, `chatKey = "synthetic-provider-never-sent"`)
	text = strings.ReplaceAll(text, `embeddingKey = ""`, `embeddingKey = "synthetic-embedding-never-sent"`)
	path := filepath.Join(t.TempDir(), "isolated.toml")
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Config.KnowledgeConfig.StorageDir = t.TempDir()
	return s
}
func sqliteDatabase(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "v2-test.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Warn)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	err = db.AutoMigrate(&model.User{}, &model.Session{}, &model.Message{}, &model.AgentRun{}, &model.AgentStep{}, &model.SessionSummary{}, &model.KnowledgeDocument{}, &model.LearningSession{}, &model.LearningQuestion{}, &model.LearningAttempt{}, &model.TopicProgress{}, &model.LongTermMemory{}, &model.MemoryDraft{}, &model.MemoryRevision{}, &model.MemoryCandidate{}, &model.MemoryExtractionJob{}, &model.MemoryDreamRun{}, &model.EpisodeMemory{}, &model.EpisodeProjectionJob{}, &model.EpisodeAssertionState{}, &model.EpisodeAssertionRelation{}, &model.EpisodeRelationProposal{})
	if err != nil {
		sqlDB.Close()
		t.Fatal(err)
	}
	if err = db.Exec("ALTER TABLE users ADD COLUMN email_normalized TEXT GENERATED ALWAYS AS (NULLIF(LOWER(TRIM(email)), '')) STORED").Error; err != nil {
		sqlDB.Close()
		t.Fatal(err)
	}
	return db
}
func dependencies(t *testing.T, db *gorm.DB, chat *captureModel, chunks *testChunks, migrations *int) Dependencies {
	return Dependencies{Clock: func() time.Time { return time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC) }, Infrastructure: Infrastructure{OpenDatabase: func(context.Context, *config.Config) (*gorm.DB, error) { return db, nil }, InitializeDatabase: func(*gorm.DB) error { *migrations++; return nil }, OpenRedis: func(*config.Config) (*redisclient.Client, error) {
		client := redisclient.NewClient(&redisclient.Options{Addr: "offline.invalid:6379", MaxRetries: -1})
		client.AddHook(redisTransport{})
		return client, nil
	}}, ChatModel: func(context.Context, *Settings) (modelcomponent.ToolCallingChatModel, error) { return chat, nil }, Embedder: func(context.Context, *config.Config) (embedding.Embedder, error) {
		return &retrievalFakeEmbedder{dimension: 1024}, nil
	}, Chunks: func(*redisclient.Client, int) (ChunkStore, error) { return chunks, nil }}
}
func newFixture(t *testing.T, s *Settings, respond func(context.Context, int) (*schema.Message, error)) (*Application, *gorm.DB, *captureModel, string) {
	t.Helper()
	db := sqliteDatabase(t)
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
func postJSON(t *testing.T, h http.Handler, path, body, token string, result any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("%s HTTP %d: %s", path, w.Code, w.Body.String())
	}
	if result != nil {
		if err := json.Unmarshal(w.Body.Bytes(), result); err != nil {
			t.Fatalf("%s: %v %s", path, err, w.Body.String())
		}
	}
}
func createSession(t *testing.T, app *Application, token, key string) string {
	var result struct {
		SessionID string `json:"sessionId"`
	}
	postJSON(t, app.Handler, "/api/v1/AI/chat/session", fmt.Sprintf(`{"modelType":"agent","requestId":%q}`, key), token, &result)
	if result.SessionID == "" {
		t.Fatal("session not created")
	}
	return result.SessionID
}
func runHTTP(t *testing.T, app *Application, token, session, key, question string) map[string]any {
	data, _ := json.Marshal(map[string]any{"sessionId": session, "requestId": key, "question": question, "knowledgePolicy": "knowledge_preferred"})
	var result map[string]any
	postJSON(t, app.Handler, "/api/v1/AI/agent/run", string(data), token, &result)
	return result
}
func TestNativeEntryCapturesDefaultAndDeploymentTools(t *testing.T) {
	for _, recall := range []string{"shadow_only", "inject_enabled"} {
		t.Run(recall, func(t *testing.T) {
			s := testSettings(t)
			if recall == "inject_enabled" {
				if _, err := toml.DecodeFile(filepath.Join("..", "..", "..", "tutorial", "v2-entry", "deployment-agent-selectors.toml"), s.Config); err != nil {
					t.Fatal(err)
				}
				if s.Config.AgentConfig.MemoryReadMode != "legacy_v2" || s.Config.AgentConfig.EpisodeRecallMode != recall {
					t.Fatal("deployment selector drift")
				}
			}
			app, db, chat, token := newFixture(t, s, func(context.Context, int) (*schema.Message, error) {
				return schema.AssistantMessage("你好", nil), nil
			})
			session := createSession(t, app, token, "create-default")
			result := runHTTP(t, app, token, session, "run-default", "你好")
			if result["answer"] != "你好" {
				t.Fatalf("native run: %+v", result)
			}
			requests := chat.all()
			if len(requests) == 0 {
				t.Fatal("no actual model request")
			}
			var names []string
			for _, info := range requests[0].Tools {
				names = append(names, info["name"].(string))
				if info["schema"] == nil {
					t.Fatal("missing actual schema")
				}
			}
			sort.Strings(names)
			want := []string{"document_read", "knowledge_search"}
			if !reflect.DeepEqual(names, want) {
				t.Fatalf("tools %v want %v", names, want)
			}
			var messages int64
			if err := db.Model(&model.Message{}).Where("session_id = ?", session).Count(&messages).Error; err != nil || messages != 2 {
				t.Fatalf("messages=%d err=%v", messages, err)
			}
			var steps int64
			db.Model(&model.AgentStep{}).Count(&steps)
			if steps == 0 {
				t.Fatal("original AgentStep DAO not used")
			}
		})
	}
}
func TestOriginalDefaultsRemainDistinct(t *testing.T) {
	s := testSettings(t)
	if s.Config.AgentConfig.MemoryReadMode != "legacy_v2" || s.Config.AgentConfig.EpisodeRecallMode != "shadow_only" || s.Config.AgentConfig.ContextInputTargetTokens != 28000 {
		t.Fatal(s.Config.AgentConfig)
	}
	if s.Config.AgentConfig.EpisodeRelationMode != "off" {
		t.Fatal("relation auto enabled")
	}
}
func TestNativeEntryWithoutTools(t *testing.T) {
	s := testSettings(t)
	s.Config.AgentConfig.MaxToolCalls = 0
	app, _, chat, token := newFixture(t, s, func(context.Context, int) (*schema.Message, error) {
		return schema.AssistantMessage("你好", nil), nil
	})
	session := createSession(t, app, token, "create-no-tools")
	runHTTP(t, app, token, session, "run-no-tools", "你好")
	if requests := chat.all(); len(requests) != 1 || len(requests[0].Tools) != 0 {
		t.Fatalf("unexpected tools: %+v", requests)
	}
}
func TestPartialRealWorkerStartRollsBackDatabase(t *testing.T) {
	s := testSettings(t)
	db := sqliteDatabase(t)
	if err := db.Migrator().DropTable(&model.EpisodeProjectionJob{}); err != nil {
		t.Fatal(err)
	}
	migrations := 0
	_, err := Assemble(context.Background(), s, dependencies(t, db, &captureModel{inner: &scriptedChatModel{}}, &testChunks{}, &migrations))
	if err == nil {
		t.Fatal("missing episode schema accepted")
	}
	sqlDB, _ := db.DB()
	if err = sqlDB.Ping(); err == nil {
		t.Fatal("database not closed after worker start failure")
	}
}
func TestDatabaseInitializationIsExplicitAndOffline(t *testing.T) {
	for _, migrate := range []bool{false, true} {
		t.Run(fmt.Sprint(migrate), func(t *testing.T) {
			s := testSettings(t)
			s.Entry.Migrate = migrate
			db := sqliteDatabase(t)
			calls := 0
			ctx, l := newLifecycle(context.Background())
			infra := dependencies(t, db, nil, nil, &calls).Infrastructure
			if err := openResources(ctx, s, infra, l); err != nil {
				t.Fatal(err)
			}
			if err := l.Close(); err != nil {
				t.Fatal(err)
			}
			want := 0
			if migrate {
				want = 1
			}
			if calls != want {
				t.Fatalf("initialization calls=%d want=%d", calls, want)
			}
		})
	}
}
