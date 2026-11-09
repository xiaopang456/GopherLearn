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
	"strconv"
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

type redisTransport struct {
	// sets is the part of Redis the offline transport keeps: the sorted sets
	// the summary segment's recent-use record lives in. The record is what
	// keeps a segment the learner reached for in front of them on the next
	// turn, so an entry-level test cannot be written without it: the entry
	// writes the record through Redis and reads it back through the same
	// client.
	sets *redisSortedSets
}

// redisSortedSets is an in-memory stand-in for the sorted sets the offline
// transport serves.
//
// It is not a Redis implementation: it keeps what the recent-use record
// needs and nothing else. The ordering is the one Redis uses, because the
// record's meaning depends on it: the newest use is the first member.
type redisSortedSets struct {
	mu   sync.Mutex
	sets map[string][]redisSortedEntry
}

type redisSortedEntry struct {
	member string
	score  float64
}

func newRedisSortedSets() *redisSortedSets {
	return &redisSortedSets{sets: map[string][]redisSortedEntry{}}
}

// add puts a member into a set with a score, the way ZADD does: a member that
// is already there takes the new score.
func (s *redisSortedSets) add(key, member string, score float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries := s.sets[key]
	for index := range entries {
		if entries[index].member == member {
			entries[index].score = score
			s.sets[key] = entries
			return
		}
	}
	entries = append(entries, redisSortedEntry{member: member, score: score})
	s.sets[key] = entries
	s.sort(entries)
}

// members returns the members of a set in the order Redis returns them in:
// by score, newest first.
func (s *redisSortedSets) members(key string) []redisSortedEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries := s.sets[key]
	s.sort(entries)
	return entries
}

// removeByRank drops the members in a rank range and reports how many went.
//
// A negative rank counts from the end, the way Redis counts it.
func (s *redisSortedSets) removeByRank(key string, start, stop int64) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries := s.sets[key]
	s.sort(entries)
	first, last, ok := rankRange(int64(len(entries)), start, stop)
	if !ok {
		return 0
	}
	s.sets[key] = append(entries[:first], entries[last+1:]...)
	return last - first + 1
}

// sort puts the entries in the order Redis returns them in: by score, newest
// first, and by member when two members share a score.
func (s *redisSortedSets) sort(entries []redisSortedEntry) {
	sort.SliceStable(entries, func(left, right int) bool {
		if entries[left].score != entries[right].score {
			return entries[left].score > entries[right].score
		}
		return entries[left].member > entries[right].member
	})
}

// rankRange turns a rank range into the indexes it covers.
//
// A negative rank counts from the end, and an empty range covers nothing.
func rankRange(length, start, stop int64) (int64, int64, bool) {
	if start < 0 {
		start += length
	}
	if stop < 0 {
		stop += length
	}
	if start < 0 {
		start = 0
	}
	if stop >= length {
		stop = length - 1
	}
	if stop < start || length == 0 {
		return 0, 0, false
	}
	return start, stop, true
}

func (redisTransport) DialHook(redisclient.DialHook) redisclient.DialHook {
	return func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("offline test forbids Redis dialing")
	}
}
func (h redisTransport) ProcessHook(redisclient.ProcessHook) redisclient.ProcessHook {
	return func(_ context.Context, cmd redisclient.Cmder) error {
		if cmd.Name() == "ft.info" {
			err := errors.New("unknown index name")
			cmd.SetErr(err)
			return err
		}
		if h.sets != nil {
			if handled, err := h.sets.serve(cmd); handled {
				return err
			}
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

// stringArgs renders a command's arguments as the strings Redis would have
// received.
//
// The client writes a score as a float and a rank as an integer, so the
// arguments are not all strings by the time they arrive here.
func stringArgs(raw []any) ([]string, bool) {
	args := make([]string, 0, len(raw))
	for _, value := range raw {
		switch typed := value.(type) {
		case string:
			args = append(args, typed)
		case int64:
			args = append(args, strconv.FormatInt(typed, 10))
		case int:
			args = append(args, strconv.Itoa(typed))
		case float64:
			args = append(args, strconv.FormatFloat(typed, 'f', -1, 64))
		default:
			return nil, false
		}
	}
	return args, true
}

// serve answers the sorted-set commands the recent-use record uses.
//
// It reports whether the command was one of them: a command it does not know
// belongs to the caller, not to it.
func (s *redisSortedSets) serve(cmd redisclient.Cmder) (bool, error) {
	// The command name decides first: only the sorted-set commands are this
	// transport's business, and the others take arguments of kinds it does
	// not have to read.
	switch cmd.Name() {
	case "zadd", "zremrangebyrank", "zrevrange":
	case "multi":
		// A transaction opens with MULTI. The offline transport has no
		// transaction to speak of: every command it answers stands on its
		// own, and the record it keeps is a sorted set, so a transaction
		// over it changes nothing.
		if c, ok := cmd.(*redisclient.StatusCmd); ok {
			c.SetVal("OK")
		}
		return true, nil
	case "exec":
		// A transaction ends with EXEC, whose answer is the list of the
		// replies. The commands' replies were already set one by one.
		if c, ok := cmd.(*redisclient.SliceCmd); ok {
			c.SetVal([]any{})
		}
		return true, nil
	default:
		return false, nil
	}
	args, ok := stringArgs(cmd.Args())
	if !ok {
		return true, fmt.Errorf("offline transport: %s takes strings", cmd.Name())
	}
	if len(args) < 2 {
		return true, fmt.Errorf("offline transport: %s needs a key", cmd.Name())
	}
	key := args[1]
	switch cmd.Name() {
	case "zadd":
		if len(args) < 4 {
			return true, fmt.Errorf("offline transport: zadd needs a score and a member")
		}
		score, err := strconv.ParseFloat(args[2], 64)
		if err != nil {
			return true, fmt.Errorf("offline transport: zadd score: %v", err)
		}
		s.add(key, args[3], score)
		if c, ok := cmd.(*redisclient.IntCmd); ok {
			c.SetVal(1)
		}
		return true, nil
	case "zremrangebyrank":
		if len(args) < 4 {
			return true, fmt.Errorf("offline transport: zremrangebyrank needs a range")
		}
		start, startErr := strconv.ParseInt(args[2], 10, 64)
		stop, stopErr := strconv.ParseInt(args[3], 10, 64)
		if startErr != nil || stopErr != nil {
			return true, fmt.Errorf("offline transport: zremrangebyrank range")
		}
		removed := s.removeByRank(key, start, stop)
		if c, ok := cmd.(*redisclient.IntCmd); ok {
			c.SetVal(removed)
		}
		return true, nil
	case "zrevrange":
		if len(args) < 4 {
			return true, fmt.Errorf("offline transport: zrevrange needs a range")
		}
		start, startErr := strconv.ParseInt(args[2], 10, 64)
		stop, stopErr := strconv.ParseInt(args[3], 10, 64)
		if startErr != nil || stopErr != nil {
			return true, fmt.Errorf("offline transport: zrevrange range")
		}
		entries := s.members(key)
		first, last, ok := rankRange(int64(len(entries)), start, stop)
		members := []string{}
		if ok {
			for _, entry := range entries[first : last+1] {
				members = append(members, entry.member)
			}
		}
		if c, ok := cmd.(*redisclient.StringSliceCmd); ok {
			c.SetVal(members)
		}
		return true, nil
	}
	return false, nil
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
	text = strings.ReplaceAll(text, `embeddingApiKey = ""`, `embeddingApiKey = "synthetic-embedding-never-sent"`)
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
	err = db.AutoMigrate(&model.User{}, &model.Session{}, &model.Message{}, &model.AgentRun{}, &model.AgentStep{}, &model.SessionSummary{}, &model.SessionWhiteboard{}, &model.SessionSummarySegment{}, &model.SessionSummarySegmentJob{}, &model.KnowledgeDocument{}, &model.LearningSession{}, &model.LearningQuestion{}, &model.LearningAttempt{}, &model.TopicProgress{}, &model.LongTermMemory{}, &model.MemoryDraft{}, &model.MemoryRevision{}, &model.MemoryCandidate{}, &model.MemoryExtractionJob{}, &model.MemoryDreamRun{}, &model.EpisodeMemory{}, &model.EpisodeProjectionJob{}, &model.EpisodeAssertionState{}, &model.EpisodeAssertionRelation{}, &model.EpisodeRelationProposal{})
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
		client.AddHook(redisTransport{sets: newRedisSortedSets()})
		return client, nil
	}}, ChatModel: func(context.Context, *Settings) (modelcomponent.ToolCallingChatModel, error) { return chat, nil }, JSONChatModel: func(context.Context, *Settings) (modelcomponent.ToolCallingChatModel, error) { return chat, nil }, Embedder: func(context.Context, *config.Config) (embedding.Embedder, error) {
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
			// The draft tool is on the table every turn now. Whether the model
			// is told to use it is decided by the question, not by whether it
			// is there. The segment browse tool is on the table because the
			// layered summary is on by default, and it only exists in that
			// mode: there are no segments to browse in the other two.
			want := []string{"browse_summary_segments", "document_read", "knowledge_search", "propose_memory"}
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
