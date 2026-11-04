package conversation

import (
	rediscommon "GopherAI/common/redis"
	episodestore "GopherAI/dao/episode"
	memorystore "GopherAI/dao/memory"
	"context"
	"fmt"
	redis "github.com/redis/go-redis/v9"
)

// Reject stale derived views instead of deleting data or letting one repeat
// alter the initial memory state of the next repeat.
func requireEmptyUserIndexes(ctx context.Context, env RealEnvironment) error {
	client := redis.NewClient(&redis.Options{Addr: env.RedisAddress, Password: env.RedisPassword, Protocol: 2})
	defer client.Close()
	episodes, err := episodestore.NewRedisEpisodeIndex(client, rediscommon.EpisodeVectorDimension)
	if err != nil {
		return err
	}
	memories, err := memorystore.NewRedisMemoryIndex(client, rediscommon.MemoryVectorDimension)
	if err != nil {
		return err
	}
	a, err := episodes.CountUser(ctx, env.UserID)
	if err != nil {
		return err
	}
	b, err := memories.CountUser(ctx, env.UserID)
	if err != nil {
		return err
	}
	if a != 0 || b != 0 {
		return fmt.Errorf("evaluation user has nonempty derived memory indexes: episodes=%d memories=%d", a, b)
	}
	return nil
}
