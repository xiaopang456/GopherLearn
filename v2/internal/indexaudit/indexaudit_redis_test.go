package indexaudit

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	redisCommon "GopherAI/common/redis"

	redisCli "github.com/redis/go-redis/v9"
)

// TestKnowledgeIndexCheckerAgainstRedisStack is the reading that the
// per-document comparison cannot make: the blocks are all in Redis, exactly as
// many as the record says, and the search index over them is gone. Counting
// keys alone calls that consistent, and retrieval answers with nothing.
//
// It creates an index only when none is there to begin with, and drops only
// the index it created, so it must not be pointed at a serving Redis - it
// writes HASH keys under the production knowledge prefix.
func TestKnowledgeIndexCheckerAgainstRedisStack(t *testing.T) {
	address := os.Getenv("GOPHERAI_REDIS_TEST_ADDR")
	if address == "" {
		t.Skip("set GOPHERAI_REDIS_TEST_ADDR to run Redis Stack index audit test")
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
	if _, err := client.FTInfo(ctx, redisCommon.KnowledgeChunkIndexName).Result(); err == nil {
		t.Skip("the knowledge search index already exists on this server; this test may only create and drop an index of its own")
	}

	suffix := strconv.FormatInt(time.Now().UnixNano(), 10)
	userID := time.Now().UnixNano() & 0x3fffffffffffffff
	docID := "indexaudit-" + suffix
	version := 2
	identity := Identity{UserID: userID, DocumentID: docID, Version: version}
	blocks := []string{"block-1", "block-2"}
	keys := make([]string, 0, len(blocks))
	for _, block := range blocks {
		keys = append(keys, redisCommon.GenerateKnowledgeChunkKey(userID, docID, version, block))
	}
	t.Cleanup(func() {
		removal := context.Background()
		for _, key := range keys {
			_ = client.Del(removal, key).Err()
		}
	})
	for index, key := range keys {
		if err := client.HSet(ctx, key,
			"user_id", strconv.FormatInt(userID, 10),
			"doc_id", docID,
			"doc_version", strconv.Itoa(version),
			"chunk_id", blocks[index],
		).Err(); err != nil {
			t.Fatalf("write %s: %v", key, err)
		}
	}

	counter := RedisCounter{Client: client}
	counts, blockers, err := counter.Counts(ctx)
	if err != nil {
		t.Fatalf("count knowledge blocks: %v", err)
	}
	if len(blockers) != 0 {
		t.Fatalf("the test keys were not countable: %v", blockers)
	}
	if counts[identity] != len(keys) {
		t.Fatalf("the blocks were not counted for this document: counts=%v want %d for %#v", counts[identity], len(keys), identity)
	}

	// The blocks are all there, so the per-document comparison has nothing to
	// report. The index check is what sees the missing index.
	problems, err := KnowledgeIndexChecker{Client: client}.CheckIndexes(ctx)
	if err != nil {
		t.Fatalf("check the knowledge index: %v", err)
	}
	if len(problems) == 0 || !strings.Contains(problems[0], "does not exist") {
		t.Fatalf("an absent search index was not reported: %v", problems)
	}
	if !strings.Contains(problems[0], redisCommon.KnowledgeChunkIndexName) {
		t.Fatalf("the report does not name the index that is absent: %v", problems)
	}

	// Creating the index is the repair, and it is the product's own creation
	// path. Afterwards the same reading over the same keys has to be silent:
	// a check that always complained would prove nothing with the case above.
	if err := redisCommon.EnsureKnowledgeChunkIndex(ctx, client, redisCommon.KnowledgeChunkVectorDimension); err != nil {
		t.Fatalf("create the knowledge index: %v", err)
	}
	// Only this test created the index, so only this test drops it.
	t.Cleanup(func() {
		_ = client.FTDropIndex(context.Background(), redisCommon.KnowledgeChunkIndexName).Err()
	})
	deadline := time.Now().Add(30 * time.Second)
	for {
		problems, err = KnowledgeIndexChecker{Client: client}.CheckIndexes(ctx)
		if err != nil {
			t.Fatalf("check the knowledge index after creating it: %v", err)
		}
		if len(problems) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the created index never became sound: %v", problems)
		}
		time.Sleep(200 * time.Millisecond)
	}
	counts, blockers, err = counter.Counts(ctx)
	if err != nil {
		t.Fatalf("recount knowledge blocks: %v", err)
	}
	if len(blockers) != 0 || counts[identity] != len(keys) {
		t.Fatalf("creating the index changed the count: counts=%v blockers=%v", counts[identity], blockers)
	}
}
