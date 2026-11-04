package conversation

import (
	rediscommon "GopherAI/common/redis"
	"context"
	redis "github.com/redis/go-redis/v9"
	"os"
	"strconv"
	"testing"
	"time"
)

func TestEmptyUserIndexesIntegration(t *testing.T) {
	addr := os.Getenv("GOPHERAI_REDIS_TEST_ADDR")
	if addr == "" {
		t.Skip("requires disposable Redis")
	}
	ctx := context.Background()
	client := redis.NewClient(&redis.Options{Addr: addr})
	defer client.Close()
	id := time.Now().UnixNano() & 0x3fffffffffffffff
	env := RealEnvironment{RedisAddress: addr, UserID: id}
	if err := requireEmptyUserIndexes(ctx, env); err != nil {
		t.Fatal(err)
	}
	keys := []string{rediscommon.EpisodeKeyPrefix + strconv.FormatInt(id, 10) + ":test", rediscommon.GenerateMemoryVectorKey(id, "test")}
	defer client.Del(ctx, keys...)
	for _, key := range keys {
		if err := client.HSet(ctx, key, "test", "value").Err(); err != nil {
			t.Fatal(err)
		}
		if err := requireEmptyUserIndexes(ctx, env); err == nil {
			t.Fatal("dirty user accepted")
		}
		if err := client.Del(ctx, key).Err(); err != nil {
			t.Fatal(err)
		}
	}
}
