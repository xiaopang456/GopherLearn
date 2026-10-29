package hybrid

import (
	"GopherAI/common/rag"
	"GopherAI/config"
	"GopherAI/entry"
	"GopherAI/model"
	"context"
	"errors"
	"fmt"
	"github.com/cloudwego/eino/components/embedding"
	modelcomponent "github.com/cloudwego/eino/components/model"
	"github.com/glebarez/sqlite"
	redisclient "github.com/redis/go-redis/v9"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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

func testSettings(t *testing.T) *entry.Settings {
	t.Helper()
	data, err := os.ReadFile("../tutorial/v2-entry/isolated.example.toml")
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
	s, err := entry.Load(path)
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
func dependencies(t *testing.T, db *gorm.DB, chat *scriptedChatModel, chunks *testChunks, migrations *int) entry.Dependencies {
	return entry.Dependencies{Clock: func() time.Time { return time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC) }, Infrastructure: entry.Infrastructure{OpenDatabase: func(context.Context, *config.Config) (*gorm.DB, error) { return db, nil }, InitializeDatabase: func(*gorm.DB) error { *migrations++; return nil }, OpenRedis: func(*config.Config) (*redisclient.Client, error) {
		client := redisclient.NewClient(&redisclient.Options{Addr: "offline.invalid:6379", MaxRetries: -1})
		client.AddHook(redisTransport{})
		return client, nil
	}}, ChatModel: func(context.Context, *entry.Settings) (modelcomponent.ToolCallingChatModel, error) { return chat, nil }, Embedder: func(context.Context, *config.Config) (embedding.Embedder, error) {
		return &retrievalFakeEmbedder{dimension: 1024}, nil
	}, Chunks: func(*redisclient.Client, int) (entry.ChunkStore, error) { return chunks, nil }}
}
