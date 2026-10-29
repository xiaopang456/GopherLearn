package episode

import (
	redisCommon "GopherAI/common/redis"
	"GopherAI/model"
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	redisCli "github.com/redis/go-redis/v9"
)

func TestRedisEpisodeIndexIntegration(t *testing.T) {
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
	index, err := NewRedisEpisodeIndex(client, redisCommon.EpisodeVectorDimension)
	if err != nil {
		t.Fatal(err)
	}
	if err = index.EnsureIndex(ctx); err != nil {
		t.Fatal(err)
	}
	userID := time.Now().UnixNano() & 0x3fffffffffffffff
	suffix := strconv.FormatInt(userID, 10)
	current := "current-" + suffix
	vector := make([]float32, redisCommon.EpisodeVectorDimension)
	vector[0] = 1
	rows := []model.EpisodeMemory{
		{ID: "exact-" + suffix, UserID: userID, SessionID: "old-a", Title: "RPC 粘包", SearchText: "RPC 粘包最终决定使用长度字段", TopicKeys: model.StringList{"rpc-粘包"}, Status: model.EpisodeStatusActive, LastMessageAt: time.Now().Add(-time.Hour)},
		{ID: "current-" + suffix, UserID: userID, SessionID: current, Title: "RPC", SearchText: "RPC 粘包", TopicKeys: model.StringList{"rpc-粘包"}, Status: model.EpisodeStatusActive, LastMessageAt: time.Now()},
		{ID: "other-" + suffix, UserID: userID + 1, SessionID: "old-b", Title: "RPC", SearchText: "RPC 粘包", TopicKeys: model.StringList{"rpc-粘包"}, Status: model.EpisodeStatusActive, LastMessageAt: time.Now()},
	}
	for _, row := range rows {
		if err = index.Store(ctx, IndexedEpisode{Episode: row, Vector: vector}); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_ = index.ResetUser(context.Background(), userID)
		_ = index.ResetUser(context.Background(), userID+1)
	})
	var exact []EpisodeSearchResult
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		exact, err = index.SearchExactTopics(ctx, userID, current, []string{"rpc-粘包"}, 20)
		if err != nil || len(exact) > 0 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if err != nil || len(exact) != 1 || exact[0].EpisodeID != rows[0].ID {
		t.Fatalf("exact=%+v err=%v", exact, err)
	}
	lexical, err := index.SearchByText(ctx, userID, current, []string{"RPC"}, 20)
	if err != nil || len(lexical) != 1 || lexical[0].EpisodeID != rows[0].ID {
		t.Fatalf("lexical=%+v err=%v", lexical, err)
	}
	semantic, err := index.SearchByVector(ctx, userID, current, vector, 20)
	if err != nil || len(semantic) != 1 || semantic[0].EpisodeID != rows[0].ID {
		t.Fatalf("semantic=%+v err=%v", semantic, err)
	}
}
