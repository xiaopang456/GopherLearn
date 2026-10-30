// Package realenv wires a run to the real services: the hosted embedding model,
// the hosted reranker and a Redis Stack. It only constructs; nothing here
// contacts a service until the runner does, and only after its own checks pass.
//
// The wiring follows the product's (v2/main.go and internal/isolated), so the
// evaluation talks to the services the way the application does. It is not
// exercised by the offline tests: the Redis adapter has an integration test that
// is skipped unless GOPHERAI_REDIS_TEST_ADDR is set, and the model clients have
// no test at all, because a test would spend money.
package realenv

import (
	"context"
	"fmt"
	"strconv"

	embeddingArk "github.com/cloudwego/eino-ext/components/embedding/ark"
	redisCli "github.com/redis/go-redis/v9"

	"GopherAI/common/rag"
	redisCommon "GopherAI/common/redis"

	"github.com/xiaopang456/GopherLearn/eval/internal/config"
	"github.com/xiaopang456/GopherLearn/eval/internal/runner"
	"github.com/xiaopang456/GopherLearn/eval/internal/safety"
)

// New builds the real environment from the product configuration. The returned
// function closes the Redis connection.
func New(ctx context.Context, product *config.Product) (runner.Env, func() error, error) {
	app := product.App
	rcfg := app.RagModelConfig
	if rcfg.RagDimension != redisCommon.KnowledgeChunkVectorDimension {
		return runner.Env{}, nil, fmt.Errorf("ragModelConfig.dimension is %d, but the product's chunk index is fixed at %d",
			rcfg.RagDimension, redisCommon.KnowledgeChunkVectorDimension)
	}
	if app.RedisDb != 0 {
		return runner.Env{}, nil, fmt.Errorf("redisConfig.db is %d, but the RediSearch chunk index lives in database 0", app.RedisDb)
	}

	embedder, err := embeddingArk.NewEmbedder(ctx, &embeddingArk.EmbeddingConfig{
		BaseURL: rcfg.RagEmbeddingBaseURL, Model: rcfg.RagEmbeddingModel, APIKey: product.EmbeddingKey,
	})
	if err != nil {
		return runner.Env{}, nil, fmt.Errorf("create the embedding client: %w", err)
	}

	client := redisCli.NewClient(&redisCli.Options{
		Addr:     app.RedisHost + ":" + strconv.Itoa(app.RedisPort),
		Password: app.RedisPassword,
		DB:       app.RedisDb,
		Protocol: 2,
	})
	repository, err := rag.NewRedisChunkRepository(client, rcfg.RagDimension)
	if err != nil {
		_ = client.Close()
		return runner.Env{}, nil, err
	}

	env := runner.Env{
		Kind: runner.EnvReal, Paid: true,
		Embedder: embedder, Repository: repository, Searcher: repository,
		Guard:     &redisGuard{kv: &redisKV{client: client}},
		Dimension: rcfg.RagDimension, EmbeddingModel: rcfg.RagEmbeddingModel,
		Chunker: rag.DefaultChunkerConfig(), MaxSourceBytes: app.KnowledgeConfig.MaxFileSizeBytes,
	}
	if rcfg.RagRerankEnabled {
		reranker, err := rag.NewReranker(rag.RerankerOptions{
			Provider: rcfg.RagRerankProvider, Endpoint: rcfg.RagRerankEndpoint, Model: rcfg.RagRerankModel,
			Token: rcfg.RagRerankToken, Timeout: rcfg.RagRerankTimeout(),
		})
		if err != nil {
			_ = client.Close()
			return runner.Env{}, nil, fmt.Errorf("create the reranker client: %w", err)
		}
		env.Reranker = reranker
		env.RerankModel = rcfg.RagRerankProvider + "/" + rcfg.RagRerankModel
	}
	return env, client.Close, nil
}

// redisGuard adapts the safety package to the runner's Guard.
type redisGuard struct{ kv safety.KV }

func (g *redisGuard) Check(ctx context.Context, fp safety.Fingerprint, rebuild bool) (safety.Decision, error) {
	return safety.Check(ctx, g.kv, fp, rebuild)
}
func (g *redisGuard) Begin(ctx context.Context, fp safety.Fingerprint) error {
	return safety.Begin(ctx, g.kv, fp)
}
func (g *redisGuard) Commit(ctx context.Context, fp safety.Fingerprint) error {
	return safety.Commit(ctx, g.kv, fp)
}

// redisKV is safety.KV over a go-redis client.
type redisKV struct{ client *redisCli.Client }

func (k *redisKV) DBSize(ctx context.Context) (int64, error) { return k.client.DBSize(ctx).Result() }

func (k *redisKV) Get(ctx context.Context, key string) (string, bool, error) {
	value, err := k.client.Get(ctx, key).Result()
	if err == redisCli.Nil {
		return "", false, nil
	}
	return value, err == nil, err
}

func (k *redisKV) Set(ctx context.Context, key, value string) error {
	return k.client.Set(ctx, key, value, 0).Err()
}

// DeleteEvalData removes the evaluation user's chunks and the fingerprint key.
// It matches the chunk key prefix of that one user, so nothing else in the
// database is touched.
func (k *redisKV) DeleteEvalData(ctx context.Context, userID int64) error {
	pattern := redisCommon.KnowledgeChunkKeyPrefix + strconv.FormatInt(userID, 10) + ":*"
	var cursor uint64
	for {
		keys, next, err := k.client.Scan(ctx, cursor, pattern, 500).Result()
		if err != nil {
			return err
		}
		if len(keys) > 0 {
			if err := k.client.Del(ctx, keys...).Err(); err != nil {
				return err
			}
		}
		if cursor = next; cursor == 0 {
			break
		}
	}
	return k.client.Del(ctx, safety.FingerprintKey).Err()
}
