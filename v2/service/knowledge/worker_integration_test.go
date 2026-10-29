package knowledge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"GopherAI/common/rag"
	redisKeys "GopherAI/common/redis"
	knowledgeDAO "GopherAI/dao/knowledge"
	"GopherAI/internal/testdb"
	"GopherAI/model"

	"github.com/cloudwego/eino/components/embedding"
	redisCli "github.com/redis/go-redis/v9"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// TestWorkerPersistentIntegration exercises the real durable chain without
// calling a paid embedding service: local source -> parser/chunker -> fake
// vectors -> Redis Stack, with lifecycle state stored in MySQL.
func TestWorkerPersistentIntegration(t *testing.T) {
	redisAddress := os.Getenv("GOPHERAI_REDIS_TEST_ADDR")
	if redisAddress == "" {
		t.Skip("set GOPHERAI_REDIS_TEST_ADDR to run persistent worker integration")
	}
	dsn := testdb.RequireMySQLDSN(t)
	ctx := context.Background()
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.User{}, &model.KnowledgeDocument{}); err != nil {
		t.Fatal(err)
	}
	redisClient := redisCli.NewClient(&redisCli.Options{
		Addr: redisAddress, Password: os.Getenv("GOPHERAI_REDIS_TEST_PASSWORD"), Protocol: 2,
	})
	if err := redisClient.Ping(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = redisClient.Close() })

	userID := time.Now().UnixNano() & 0x3fffffffffffffff
	user := model.User{ID: userID, Username: fmt.Sprintf("worker-integration-%d", userID)}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	repository := knowledgeDAO.NewRepository(db)
	storage, err := NewLocalFileStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := NewDocumentService(repository, storage, testKnowledgeLimits())
	document, err := service.CreateDocument(ctx, userID, "redis-worker.md", bytes.NewBufferString(
		"# Redis\n\n## TTL\nRedis 的键可以设置过期时间。\n\n```go\nrdb.Set(ctx, key, value, time.Minute)\n```\n\n## 持久化\nRDB 快照与 AOF 日志各有取舍。\n\n## 主从\n主从复制是异步的，主库不等从库确认。\n",
	))
	if err != nil {
		t.Fatal(err)
	}
	prefix := redisKeys.GenerateKnowledgeDocumentChunkPrefix(userID, document.ID, document.Version)
	documentPrefix := redisKeys.KnowledgeChunkKeyPrefix + fmt.Sprintf("%d:%s:", userID, document.ID)
	t.Cleanup(func() {
		keys, _ := redisClient.Keys(context.Background(), documentPrefix+"*").Result()
		if len(keys) > 0 {
			_ = redisClient.Del(context.Background(), keys...).Err()
		}
		db.Unscoped().Where("user_id = ?", userID).Delete(&model.KnowledgeDocument{})
		db.Unscoped().Delete(&model.User{}, userID)
	})

	chunker, err := rag.NewChunker(rag.DefaultChunkerConfig())
	if err != nil {
		t.Fatal(err)
	}
	vectorRepository, err := rag.NewRedisChunkRepository(redisClient, redisKeys.KnowledgeChunkVectorDimension)
	if err != nil {
		t.Fatal(err)
	}
	embedder := &integrationEmbedder{dimension: redisKeys.KnowledgeChunkVectorDimension}
	indexer, err := rag.NewKnowledgeChunkIndexer(
		embedder,
		vectorRepository,
		rag.ChunkIndexConfig{
			Dimension: redisKeys.KnowledgeChunkVectorDimension, BatchSize: 10,
			EmbeddingModel: "integration-embedder", EmbeddingContentVersion: rag.EmbeddingContentVersion,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	ingestor, err := NewDocumentIngestor(storage, chunker, indexer, 5*1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := NewIndexWorkerPool(repository, ingestor, WorkerConfig{WorkerCount: 2, QueueSize: 10, ScanInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.Start(ctx); err != nil {
		t.Fatal(err)
	}
	cleanupWorker, err := NewCleanupWorkerPool(repository, vectorRepository, storage, WorkerConfig{WorkerCount: 1, QueueSize: 10, ScanInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := cleanupWorker.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = worker.Stop(stopCtx)
		_ = cleanupWorker.Stop(stopCtx)
	})

	stored := waitPersistentDocument(t, db, document.ID, model.KnowledgeStatusReady)
	keys, err := redisClient.Keys(ctx, prefix+"*").Result()
	if err != nil {
		t.Fatal(err)
	}
	if stored.ChunkCount <= 0 || len(keys) != stored.ChunkCount || stored.ActiveVersion != stored.Version {
		t.Fatalf("ready metadata/redis keys = chunks %d, keys %d, active version %d", stored.ChunkCount, len(keys), stored.ActiveVersion)
	}

	// Everything indexed so far was a first upload, so it had no reuse source.
	firstUploadTexts := embedder.texts.Load()
	if firstUploadTexts != int64(stored.ChunkCount) {
		t.Fatalf("first upload embedded %d texts for %d chunks", firstUploadTexts, stored.ChunkCount)
	}

	updated, unchanged, err := service.UpdateDocument(ctx, userID, document.ID, "redis-worker.md", bytes.NewBufferString(
		"# Redis\n\n## TTL\n更新后的个人笔记解释。\n\n```go\nrdb.Expire(ctx, key, time.Hour)\n```\n\n## 持久化\nRDB 快照与 AOF 日志各有取舍。\n\n## 主从\n主从复制是异步的，主库不等从库确认。\n",
	))
	if err != nil || unchanged || updated.Version != 2 || updated.ActiveVersion != 1 {
		t.Fatalf("stage persistent update = (%+v, %v, %v)", updated, unchanged, err)
	}
	stored = waitPersistentDocument(t, db, document.ID, model.KnowledgeStatusReady)
	v1Keys, _ := redisClient.Keys(ctx, prefix+"*").Result()
	v2Prefix := redisKeys.GenerateKnowledgeDocumentChunkPrefix(userID, document.ID, 2)
	v2Keys, _ := redisClient.Keys(ctx, v2Prefix+"*").Result()
	if stored.ActiveVersion != 2 || len(v1Keys) != 0 || len(v2Keys) != stored.ChunkCount {
		t.Fatalf("version cleanup = active %d, v1 keys %d, v2 keys %d, chunks %d", stored.ActiveVersion, len(v1Keys), len(v2Keys), stored.ChunkCount)
	}
	// The update changed the TTL paragraph and the code block but left the
	// document heading chunk alone, so the whole version must not be recomputed.
	// Only the TTL section changed; the persistence and replication sections are
	// byte-identical, so exactly one chunk may be recomputed.
	updateTexts := embedder.texts.Load() - firstUploadTexts
	if stored.ChunkCount != 3 || updateTexts != 1 {
		t.Fatalf("update embedded %d texts for a %d-chunk version; want 1 of 3", updateTexts, stored.ChunkCount)
	}

	if err := service.DeleteDocument(ctx, userID, document.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.GetByIDAndUserID(ctx, document.ID, userID); !errors.Is(err, knowledgeDAO.ErrDocumentNotFound) {
		t.Fatalf("logical delete must immediately hide document: %v", err)
	}
	stored = waitPersistentDocument(t, db, document.ID, model.KnowledgeStatusDeleted)
	remaining, _ := redisClient.Keys(ctx, documentPrefix+"*").Result()
	if len(remaining) != 0 || stored.ActiveVersion != 0 || stored.ChunkCount != 0 {
		t.Fatalf("physical delete left state: keys=%d document=%+v", len(remaining), stored)
	}
}

func waitPersistentDocument(t *testing.T, db *gorm.DB, documentID string, status model.KnowledgeDocumentStatus) model.KnowledgeDocument {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var stored model.KnowledgeDocument
		if err := db.Where("id = ?", documentID).First(&stored).Error; err != nil {
			t.Fatal(err)
		}
		if stored.IndexStatus == status {
			return stored
		}
		if stored.IndexStatus == model.KnowledgeStatusFailed {
			t.Fatalf("worker marked integration document failed: %s", stored.ErrorMessage)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("document %s did not become %s before timeout", documentID, status)
	return model.KnowledgeDocument{}
}

type integrationEmbedder struct {
	dimension int
	texts     atomic.Int64
}

func (e *integrationEmbedder) EmbedStrings(_ context.Context, texts []string, _ ...embedding.Option) ([][]float64, error) {
	e.texts.Add(int64(len(texts)))
	vectors := make([][]float64, len(texts))
	for textIndex := range vectors {
		vectors[textIndex] = make([]float64, e.dimension)
		for dimension := range vectors[textIndex] {
			vectors[textIndex][dimension] = float64(textIndex + dimension + 1)
		}
	}
	return vectors, nil
}
