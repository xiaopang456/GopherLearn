package memory

import (
	redisCommon "GopherAI/common/redis"
	"GopherAI/model"
	"context"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	redisCli "github.com/redis/go-redis/v9"
)

// memoryRow is one active topic memory. Content is the same for both users on
// purpose: the owner is the only field that may separate the two search results.
func memoryRow(id string, userID int64) IndexedMemory {
	vector := make([]float32, redisCommon.MemoryVectorDimension)
	vector[0] = 1
	return IndexedMemory{
		Memory: model.LongTermMemory{
			ID: id, UserID: userID, Scope: model.MemoryScopeTopic,
			Subject: "reading archive tag", Content: "the reading archive tag is decided once",
			ContentHash: strings.Repeat("0", 64), TopicKey: "reading-archive",
			Status: model.LongTermMemoryActive, Confidence: 1,
		},
		Vector: vector,
	}
}

func idsOf(results []MemorySearchResult) []string {
	ids := make([]string, 0, len(results))
	for _, result := range results {
		ids = append(ids, result.MemoryID)
	}
	sort.Strings(ids)
	return ids
}

// TestRedisMemoryIndexUserIsolation is the retrieval side of the isolation the
// evaluation identity relies on: with two users holding identical rows in the
// same shared index, every search path must return only the requesting user's
// rows, and clearing one user must leave the other intact.
//
// It is gated on a disposable Redis Stack because CI has none; the skip is
// recorded in the change description rather than hidden.
func TestRedisMemoryIndexUserIsolation(t *testing.T) {
	address := os.Getenv("GOPHERAI_REDIS_TEST_ADDR")
	if address == "" {
		t.Skip("set GOPHERAI_REDIS_TEST_ADDR to run Redis Stack integration test")
	}
	client := redisCli.NewClient(&redisCli.Options{Addr: address, Password: os.Getenv("GOPHERAI_REDIS_TEST_PASSWORD"), Protocol: 2})
	t.Cleanup(func() { _ = client.Close() })
	ctx := context.Background()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	index, err := NewRedisMemoryIndex(client, redisCommon.MemoryVectorDimension)
	if err != nil {
		t.Fatal(err)
	}
	if err = index.EnsureIndex(ctx); err != nil {
		t.Fatal(err)
	}
	base := time.Now().UnixNano() & 0x3fffffffffffffff
	left, right := base, base+1
	suffix := strconv.FormatInt(base, 10)
	rows := []IndexedMemory{memoryRow("left-a-"+suffix, left), memoryRow("left-b-"+suffix, left), memoryRow("right-a-"+suffix, right), memoryRow("right-b-"+suffix, right)}
	if err = index.Store(ctx, rows); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		quiet := context.Background()
		_ = index.ResetUser(quiet, left)
		_ = index.ResetUser(quiet, right)
	})
	deadline := time.Now().Add(3 * time.Second)
	var vector []MemorySearchResult
	for time.Now().Before(deadline) {
		vector, err = index.SearchByVector(ctx, left, rows[0].Vector, 20)
		if err != nil || len(vector) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"left-a-" + suffix, "left-b-" + suffix}
	if got := idsOf(vector); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("vector search for the left user returned %v, want %v", got, want)
	}
	if foreign, err := index.SearchByVector(ctx, right, rows[0].Vector, 20); err != nil {
		t.Fatal(err)
	} else if got := idsOf(foreign); strings.Join(got, ",") != strings.Join([]string{"right-a-" + suffix, "right-b-" + suffix}, ",") {
		t.Fatalf("vector search for the right user returned %v", got)
	}
	lexical, err := index.SearchByText(ctx, left, []string{"archive"}, 20)
	if err != nil {
		t.Fatal(err)
	}
	if got := idsOf(lexical); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("text search for the left user returned %v, want %v", got, want)
	}
	if counts, err := index.CountUser(ctx, left); err != nil || counts != 2 {
		t.Fatalf("count left = %d, %v want 2", counts, err)
	}
	if counts, err := index.CountUser(ctx, right); err != nil || counts != 2 {
		t.Fatalf("count right = %d, %v want 2", counts, err)
	}
	if err := index.ResetUser(ctx, left); err != nil {
		t.Fatal(err)
	}
	if counts, err := index.CountUser(ctx, left); err != nil || counts != 0 {
		t.Fatalf("after reset left = %d, %v want 0", counts, err)
	}
	if counts, err := index.CountUser(ctx, right); err != nil || counts != 2 {
		t.Fatalf("the other user was damaged: %d, %v", counts, err)
	}
	if empty, err := index.SearchByVector(ctx, left, rows[0].Vector, 20); err != nil || len(empty) != 0 {
		t.Fatalf("the reset user still searches: %v, %v", empty, err)
	}
}
