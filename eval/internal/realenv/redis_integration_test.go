package realenv

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	redisCli "github.com/redis/go-redis/v9"

	"GopherAI/common/rag"

	"github.com/xiaopang456/GopherLearn/eval/internal/safety"
)

const integrationUser = int64(987654)

// TestRedisGuardIntegration runs the safety guard against a real Redis. It is
// skipped unless GOPHERAI_REDIS_TEST_ADDR is set, and skips again when database
// 0 holds data that is not an evaluation index, so it never touches a database
// it does not own.
func TestRedisGuardIntegration(t *testing.T) {
	addr := os.Getenv("GOPHERAI_REDIS_TEST_ADDR")
	if addr == "" {
		t.Skip("set GOPHERAI_REDIS_TEST_ADDR to run the Redis guard integration test")
	}
	client := redisCli.NewClient(&redisCli.Options{Addr: addr, Password: os.Getenv("GOPHERAI_REDIS_TEST_PASSWORD"), Protocol: 2})
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	kv := &redisKV{client: client}
	if size, _ := kv.DBSize(ctx); size != 0 {
		if _, found, _ := kv.Get(ctx, safety.FingerprintKey); !found {
			t.Skip("database 0 is not empty and is not an evaluation index; the test will not touch it")
		}
	}
	const foreignKey = "eval-integration-test:foreign"
	t.Cleanup(func() {
		_ = kv.DeleteEvalData(context.Background(), integrationUser)
		_ = client.Del(context.Background(), foreignKey, "gopherai:knowledge:chunk:987654:doc:1:c1").Err()
	})
	if err := kv.DeleteEvalData(ctx, integrationUser); err != nil {
		t.Fatal(err)
	}

	fp := safety.NewFingerprint("corpus", "model", 1024, rag.DefaultChunkerConfig(), integrationUser)
	if d, err := safety.Check(ctx, kv, fp, false); err != nil || d != safety.Build {
		t.Fatalf("empty database: %v, %v", d, err)
	}
	if err := safety.Begin(ctx, kv, fp); err != nil {
		t.Fatal(err)
	}
	if _, err := safety.Check(ctx, kv, fp, false); !errors.Is(err, safety.ErrStaleIndex) {
		t.Fatalf("an unfinished build must not be reusable: %v", err)
	}
	if err := safety.Commit(ctx, kv, fp); err != nil {
		t.Fatal(err)
	}
	if d, err := safety.Check(ctx, kv, fp, false); err != nil || d != safety.Reuse {
		t.Fatalf("committed index: %v, %v", d, err)
	}

	// -rebuild removes this user's chunks and the fingerprint, and only those.
	if err := client.Set(ctx, "gopherai:knowledge:chunk:987654:doc:1:c1", "x", 0).Err(); err != nil {
		t.Fatal(err)
	}
	if err := client.Set(ctx, foreignKey, "x", 0).Err(); err != nil {
		t.Fatal(err)
	}
	other := fp
	other.CorpusHash = "changed"
	if d, err := safety.Check(ctx, kv, other, true); err != nil || d != safety.Build {
		t.Fatalf("rebuild: %v, %v", d, err)
	}
	if n, _ := client.Exists(ctx, "gopherai:knowledge:chunk:987654:doc:1:c1", safety.FingerprintKey).Result(); n != 0 {
		t.Fatal("-rebuild left the evaluation's own data behind")
	}
	if n, _ := client.Exists(ctx, foreignKey).Result(); n != 1 {
		t.Fatal("-rebuild deleted a key that is not the evaluation's")
	}
	// What remains is data the evaluation does not own.
	if _, err := safety.Check(ctx, kv, fp, true); !errors.Is(err, safety.ErrForeignDatabase) {
		t.Fatalf("foreign data must be refused even with -rebuild: %v", err)
	}
}
