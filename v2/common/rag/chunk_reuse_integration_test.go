package rag

import (
	"context"
	"os"
	"reflect"
	"strconv"
	"testing"
	"time"

	redisCommon "GopherAI/common/redis"
	redisCli "github.com/redis/go-redis/v9"
)

// TestRedisChunkReuseSourceIntegration checks the half of the reuse contract
// that unit tests cannot: that the vector survives the Redis round trip
// unchanged, that legacy chunks read back as "not reusable" instead of as an
// error, and that the loader never crosses a user, document, or version.
func TestRedisChunkReuseSourceIntegration(t *testing.T) {
	address := os.Getenv("GOPHERAI_REDIS_TEST_ADDR")
	if address == "" {
		t.Skip("set GOPHERAI_REDIS_TEST_ADDR to run Redis Stack reuse-source integration test")
	}
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

	const dimension = redisCommon.KnowledgeChunkVectorDimension
	repository, err := NewRedisChunkRepository(client, dimension)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.EnsureIndex(ctx); err != nil {
		t.Fatal(err)
	}

	suffix := strconv.FormatInt(time.Now().UnixNano(), 10)
	userA := time.Now().UnixNano() & 0x3fffffffffffffff
	userB := userA + 1
	docA, docB := "reuse-a-"+suffix, "reuse-b-"+suffix
	for _, cleanup := range []struct {
		userID  int64
		docID   string
		version int
	}{{userA, docA, 1}, {userA, docA, 2}, {userB, docB, 1}} {
		cleanup := cleanup
		t.Cleanup(func() {
			_ = repository.DeleteDocumentVersion(context.Background(), cleanup.userID, cleanup.docID, cleanup.version)
		})
	}

	fingerprint := EmbeddingFingerprint("integration-model", dimension, EmbeddingContentVersion)
	chunksA := integrationEmbeddedChunks(t, userA, docA, 3, dimension, 1)
	for index := range chunksA {
		chunksA[index].EmbeddingInputHash = EmbeddingInputHash(chunksA[index].Chunk.EmbeddingContent)
		chunksA[index].EmbeddingFingerprint = fingerprint
	}
	chunksB := integrationEmbeddedChunks(t, userB, docB, 2, dimension, 1)
	for index := range chunksB {
		chunksB[index].EmbeddingInputHash = EmbeddingInputHash(chunksB[index].Chunk.EmbeddingContent)
		chunksB[index].EmbeddingFingerprint = fingerprint
	}
	if err := repository.Store(ctx, chunksA); err != nil {
		t.Fatal(err)
	}
	if err := repository.Store(ctx, chunksB); err != nil {
		t.Fatal(err)
	}

	loaded, err := repository.LoadDocumentVersionEmbeddings(ctx, userA, docA, 1)
	if err != nil {
		t.Fatalf("LoadDocumentVersionEmbeddings() error: %v", err)
	}
	if len(loaded) != len(chunksA) {
		t.Fatalf("loaded %d records, want %d", len(loaded), len(chunksA))
	}
	byChunkID := make(map[string]StoredChunkEmbedding, len(loaded))
	for _, record := range loaded {
		if record.UserID != userA || record.DocID != docA || record.DocVersion != 1 {
			t.Fatalf("loader returned a foreign record: user %d doc %q version %d", record.UserID, record.DocID, record.DocVersion)
		}
		byChunkID[record.ChunkID] = record
	}
	for _, written := range chunksA {
		record, found := byChunkID[written.Chunk.ID]
		if !found {
			t.Fatalf("chunk %q did not come back from the reuse source", written.Chunk.ID)
		}
		if !reflect.DeepEqual(record.Vector, written.Vector) {
			t.Fatalf("chunk %q vector changed across the Redis round trip", written.Chunk.ID)
		}
		if record.EmbeddingContent != written.Chunk.EmbeddingContent ||
			record.EmbeddingInputHash != written.EmbeddingInputHash ||
			record.EmbeddingFingerprint != written.EmbeddingFingerprint {
			t.Fatalf("chunk %q reuse metadata changed across the Redis round trip", written.Chunk.ID)
		}
	}

	// Deterministic order lets the same update make the same reuse decisions on
	// every run, regardless of how SCAN happens to walk the keyspace.
	second, err := repository.LoadDocumentVersionEmbeddings(ctx, userA, docA, 1)
	if err != nil || !reflect.DeepEqual(loaded, second) {
		t.Fatalf("reuse source order is not deterministic: %v", err)
	}

	// A version that was never written is an empty reuse source, not an error.
	if missing, err := repository.LoadDocumentVersionEmbeddings(ctx, userA, docA, 2); err != nil || len(missing) != 0 {
		t.Fatalf("unwritten version = %d records, %v", len(missing), err)
	}
	// Neither is another user's document, even though the key prefix differs
	// only by the user segment.
	if crossed, err := repository.LoadDocumentVersionEmbeddings(ctx, userA, docB, 1); err != nil || len(crossed) != 0 {
		t.Fatalf("cross-document load = %d records, %v", len(crossed), err)
	}

	// A chunk written before this feature existed keeps its vector but has no
	// identity fields. It must be skipped silently, not reported as damage.
	legacyKey := redisCommon.GenerateKnowledgeChunkKey(userA, docA, 1, chunksA[0].Chunk.ID)
	if err := client.HDel(ctx, legacyKey,
		redisCommon.KnowledgeFieldEmbeddingInputHash, redisCommon.KnowledgeFieldEmbeddingFingerprint).Err(); err != nil {
		t.Fatal(err)
	}
	afterLegacy, err := repository.LoadDocumentVersionEmbeddings(ctx, userA, docA, 1)
	if err != nil || len(afterLegacy) != len(chunksA)-1 {
		t.Fatalf("legacy chunk load = %d records, %v", len(afterLegacy), err)
	}

	// A truncated vector is unusable, and again not a load failure: only that
	// one chunk loses its reuse candidate.
	damagedKey := redisCommon.GenerateKnowledgeChunkKey(userA, docA, 1, chunksA[1].Chunk.ID)
	if err := client.HSet(ctx, damagedKey, redisCommon.KnowledgeFieldVector, []byte{1, 2, 3}).Err(); err != nil {
		t.Fatal(err)
	}
	afterDamage, err := repository.LoadDocumentVersionEmbeddings(ctx, userA, docA, 1)
	if err != nil || len(afterDamage) != len(chunksA)-2 {
		t.Fatalf("damaged chunk load = %d records, %v", len(afterDamage), err)
	}
}
