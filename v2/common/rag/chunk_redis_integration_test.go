package rag

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	redisCommon "GopherAI/common/redis"
	redisCli "github.com/redis/go-redis/v9"
)

func TestRedisChunkRepositoryIntegration(t *testing.T) {
	address := os.Getenv("GOPHERAI_REDIS_TEST_ADDR")
	if address == "" {
		t.Skip("set GOPHERAI_REDIS_TEST_ADDR to run Redis Stack integration test")
	}
	// A password is optional so an unauthenticated local Redis Stack still works,
	// but a hardened one can be used too — the perf stack requires auth, and
	// without this the test could only ever run against a wide-open server.
	client := redisCli.NewClient(&redisCli.Options{
		Addr:     address,
		Password: os.Getenv("GOPHERAI_REDIS_TEST_PASSWORD"),
		Protocol: 2,
	})
	t.Cleanup(func() { _ = client.Close() })
	ctx := context.Background()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatalf("ping Redis Stack: %v", err)
	}

	const dimension = 1024
	repository, err := NewRedisChunkRepository(client, dimension)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.EnsureIndex(ctx); err != nil {
		t.Fatal(err)
	}
	// Repeated creation must be harmless for concurrent/restarted services.
	if err := repository.EnsureIndex(ctx); err != nil {
		t.Fatalf("EnsureIndex() second call: %v", err)
	}

	suffix := strconv.FormatInt(time.Now().UnixNano(), 10)
	userA := time.Now().UnixNano() & 0x3fffffffffffffff
	userB := userA + 1
	docA1, docA2, docB := "redis-a1-"+suffix, "redis-a2-"+suffix, "redis-b-"+suffix
	docFailure := "redis-failure-" + suffix
	for _, cleanup := range []struct {
		userID  int64
		docID   string
		version int
	}{
		{userA, docA1, 1}, {userA, docA1, 2}, {userA, docA2, 1}, {userB, docB, 1}, {userA, docFailure, 1},
	} {
		cleanup := cleanup
		t.Cleanup(func() {
			_ = repository.DeleteDocumentVersion(context.Background(), cleanup.userID, cleanup.docID, cleanup.version)
		})
	}

	chunksA1 := integrationEmbeddedChunks(t, userA, docA1, 3, dimension, 1)
	chunksA2 := integrationEmbeddedChunks(t, userA, docA2, 2, dimension, 1)
	chunksB := integrationEmbeddedChunks(t, userB, docB, 2, dimension, 1)
	for _, batch := range [][]EmbeddedChunk{chunksA1, chunksA2, chunksB} {
		if err := repository.Store(ctx, batch); err != nil {
			t.Fatalf("Store() batch: %v", err)
		}
	}

	if err := repository.Store(ctx, chunksA1); err != nil {
		t.Fatalf("idempotent Store(): %v", err)
	}
	if count, err := repository.CountDocumentVersion(ctx, userA, docA1, 1); err != nil || count != 3 {
		t.Fatalf("idempotent count = %d, %v; want 3", count, err)
	}

	queryVector := make([]float32, dimension)
	queryVector[0] = 1
	resultsA := waitForUserSearch(t, repository, userA, queryVector, 10, 5)
	for _, result := range resultsA {
		if result.DocID != docA1 && result.DocID != docA2 {
			t.Fatalf("user A search leaked doc %q", result.DocID)
		}
	}
	resultsB := waitForUserSearch(t, repository, userB, queryVector, 10, 2)
	for _, result := range resultsB {
		if result.DocID != docB {
			t.Fatalf("user B search leaked doc %q", result.DocID)
		}
	}

	// Both versions physically coexist, but the query-level allow-list ranks
	// only the active version and still searches across sibling documents.
	chunksA1V2 := integrationEmbeddedChunks(t, userA, docA1, 2, dimension, 2)
	if err := repository.Store(ctx, chunksA1V2); err != nil {
		t.Fatal(err)
	}
	activeResults, err := repository.SearchActiveByUserVector(ctx, userA, queryVector, 10, map[string]int{docA1: 2, docA2: 1})
	if err != nil {
		t.Fatal(err)
	}
	seenDocuments := map[string]bool{}
	for _, result := range activeResults {
		if result.DocID == docA1 && result.DocVersion != 2 {
			t.Fatalf("stale version entered active KNN results: %+v", result)
		}
		if result.DocID != docA1 && result.DocID != docA2 {
			t.Fatalf("active search leaked document %q", result.DocID)
		}
		seenDocuments[result.DocID] = true
	}
	if !seenDocuments[docA1] || !seenDocuments[docA2] {
		t.Fatalf("active cross-document search = %+v", activeResults)
	}

	// The v2 index uses the Chinese analyzer and indexes weighted title,
	// symbols and raw content. Exact mixed code/config terms must still obey
	// the same user and active-version boundary as vector search.
	lexicalResults := waitForTextSearch(t, repository, userA,
		"GetOrCreateAIHelper 为什么不能只加 sync.RWMutex，事件空窗在 config.toml 的 rabbitmqConfig",
		10, map[string]int{docA1: 2, docA2: 1}, 1)
	for _, result := range lexicalResults {
		if result.DocID != docA2 || result.DocVersion != 1 || result.LexicalScore <= 0 {
			t.Fatalf("lexical search escaped active boundary or lost score: %+v", result)
		}
	}
	privateLexical, err := repository.SearchActiveByUserText(ctx, userB, "GetOrCreateAIHelper", 10, map[string]int{docB: 1})
	if err != nil || len(privateLexical) != 0 {
		t.Fatalf("user B lexical search leaked user A content: %+v, %v", privateLexical, err)
	}

	// Exercise the application-lifecycle service against real Redis Stack,
	// not only the repository method used underneath it.
	retrievalService, err := NewRAGService(ctx, &retrievalFakeEmbedder{dimension: dimension}, repository, func(_ context.Context, userID int64) ([]ActiveDocument, error) {
		switch userID {
		case userA:
			return []ActiveDocument{{DocID: docA1, Version: 2, OriginalName: "current.md"}, {DocID: docA2, Version: 1, OriginalName: "redis.md"}}, nil
		case userB:
			return []ActiveDocument{{DocID: docB, Version: 1, OriginalName: "private.md"}}, nil
		default:
			return []ActiveDocument{}, nil
		}
	}, dimension)
	if err != nil {
		t.Fatal(err)
	}
	serviceResults, err := retrievalService.Retrieve(ctx, userA, "cross document question", 10)
	if err != nil {
		t.Fatal(err)
	}
	serviceDocuments := map[string]bool{}
	for _, result := range serviceResults {
		if result.DocID == docB || (result.DocID == docA1 && result.DocVersion != 2) {
			t.Fatalf("shared service returned isolated or stale chunk: %+v", result)
		}
		serviceDocuments[result.DocID] = true
	}
	if !serviceDocuments[docA1] || !serviceDocuments[docA2] {
		t.Fatalf("shared service did not retrieve across active documents: %+v", serviceResults)
	}

	// Replacing one document version removes stale IDs after the new set is
	// committed, while other documents remain untouched.
	if err := repository.Store(ctx, chunksA1[:2]); err != nil {
		t.Fatalf("replace document chunks: %v", err)
	}
	if count, _ := repository.CountDocumentVersion(ctx, userA, docA1, 1); count != 2 {
		t.Fatalf("stale cleanup count = %d, want 2", count)
	}
	if count, _ := repository.CountDocumentVersion(ctx, userA, docA2, 1); count != 2 {
		t.Fatalf("stale cleanup affected another document, count %d", count)
	}

	if err := repository.DeleteDocumentVersion(ctx, userA, docA1, 1); err != nil {
		t.Fatal(err)
	}
	if count, _ := repository.CountDocumentVersion(ctx, userA, docA1, 1); count != 0 {
		t.Fatalf("delete count = %d, want 0", count)
	}
	if count, _ := repository.CountDocumentVersion(ctx, userA, docA2, 1); count != 2 {
		t.Fatalf("delete affected sibling document, count %d", count)
	}

	// A pre-existing wrong-type key forces one HSET in EXEC to fail. Any other
	// newly written key from that transaction must be compensated.
	failureChunks := integrationEmbeddedChunks(t, userA, docFailure, 2, dimension, 1)
	wrongTypeKey := redisCommon.GenerateKnowledgeChunkKey(userA, docFailure, 1, failureChunks[0].Chunk.ID)
	newKey := redisCommon.GenerateKnowledgeChunkKey(userA, docFailure, 1, failureChunks[1].Chunk.ID)
	if err := client.RPush(ctx, wrongTypeKey, "collision").Err(); err != nil {
		t.Fatal(err)
	}
	if err := repository.Store(ctx, failureChunks); err == nil || !strings.Contains(strings.ToLower(err.Error()), "wrongtype") {
		t.Fatalf("Store() partial failure = %v, want WRONGTYPE", err)
	}
	if exists, err := client.Exists(ctx, newKey).Result(); err != nil || exists != 0 {
		t.Fatalf("new key survived failed transaction: exists=%d err=%v", exists, err)
	}
}

func integrationEmbeddedChunks(t *testing.T, userID int64, docID string, count, dimension int, version int) []EmbeddedChunk {
	t.Helper()
	markdown := "# Redis Integration\n\n"
	for index := 0; index < count; index++ {
		markdown += fmt.Sprintf("## Section %d\nDocument %s content %d\n", index, docID, index)
		if strings.Contains(docID, "redis-a2-") && index == 0 {
			markdown += "GetOrCreateAIHelper 必须使用 sync.RWMutex 的 Lock，不能只加 RLock。\n"
			markdown += "服务发现的 Get 和 Watch 之间存在事件空窗，配置位于 config.toml 的 rabbitmqConfig。\n"
		}
	}
	chunker := mustChunker(t, DefaultChunkerConfig())
	chunks, err := chunker.Chunk(ChunkInput{DocID: docID, DocVersion: version, SourceFile: docID + ".md", Blocks: ParseMarkdown(markdown)})
	if err != nil {
		t.Fatal(err)
	}
	result := make([]EmbeddedChunk, len(chunks))
	for index, chunk := range chunks {
		vector := make([]float32, dimension)
		vector[0] = 1
		vector[1] = float32(index) / 10
		result[index] = EmbeddedChunk{UserID: userID, Chunk: chunk, Vector: vector}
	}
	return result
}

func waitForTextSearch(t *testing.T, repository *RedisChunkRepository, userID int64, query string, topK int, active map[string]int, minimum int) []RedisChunkSearchResult {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		results, err := repository.SearchActiveByUserText(context.Background(), userID, query, topK, active)
		if err == nil && len(results) >= minimum {
			return results
		}
		if time.Now().After(deadline) {
			t.Fatalf("text search user %d did not reach %d results: got %d, err %v", userID, minimum, len(results), err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func waitForUserSearch(t *testing.T, repository *RedisChunkRepository, userID int64, vector []float32, topK, minimum int) []RedisChunkSearchResult {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		results, err := repository.SearchByUserVector(context.Background(), userID, vector, topK)
		if err == nil && len(results) >= minimum {
			return results
		}
		if time.Now().After(deadline) {
			t.Fatalf("search user %d did not reach %d results: got %d, err %v", userID, minimum, len(results), err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
