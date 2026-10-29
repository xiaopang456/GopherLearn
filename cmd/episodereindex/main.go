// Command episodereindex rebuilds the episodic retrieval view for one or more
// users. MySQL (or the memory store) remains authoritative: the view is a
// derived cache, and this tool is the repair path for a view that fell behind -
// an outage while indexing, a failed embed, an embedder change.
//
// Usage:
//
//	episodereindex <userID> [userID...]
//
// Configuration is the same environment the server reads: GOPHERLEARN_STORAGE
// and its DSN, the retrieval mode with its embedding and Redis settings. The
// deterministic mode rebuilds into a process-local view, which is only useful
// as a consistency check (the view dies with this process); the real repair
// target is semantic_redis, whose view persists.
//
// The tool computes the same embedder identity the server computes
// (retrieval.VectorIdentity over service.EpisodeContentVersion), so a rebuilt
// view is visible to recall instead of being silently filtered out.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/xiaopang456/GopherLearn/internal/app"
	"github.com/xiaopang456/GopherLearn/internal/repository/gormmysql"
	"github.com/xiaopang456/GopherLearn/internal/repository/redisindex"
	"github.com/xiaopang456/GopherLearn/internal/retrieval"
	"github.com/xiaopang456/GopherLearn/internal/service"
	"github.com/xiaopang456/GopherLearn/internal/storage"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	users := os.Args[1:]
	if len(users) == 0 {
		log.Fatal("usage: episodereindex <userID> [userID...]")
	}

	retrievalConfig, err := app.RetrievalConfigFromEnvironment(os.Getenv)
	if err != nil {
		log.Fatalf("retrieval configuration failed: %v", err)
	}
	storageConfig, err := storage.ConfigFromEnvironment(os.Getenv)
	if err != nil {
		log.Fatalf("storage configuration failed: %v", err)
	}
	stores, err := storage.Connect(ctx, storageConfig, storage.Options{
		NewMySQL: func(ctx context.Context, config storage.Config) (storage.Stores, error) {
			connected, err := gormmysql.Connect(ctx, config.MySQLDSN, gormmysql.Options{ConnectTimeout: config.ConnectTimeout})
			if err != nil {
				return storage.Stores{}, err
			}
			return storage.Stores{Repository: connected.Repository(), Knowledge: connected.Knowledge()}, nil
		},
	})
	if err != nil {
		log.Fatalf("storage startup failed: %v", err)
	}

	embedder, view, closeView, err := buildView(retrievalConfig)
	if err != nil {
		log.Fatalf("view startup failed: %v", err)
	}
	defer func() { _ = closeView() }()
	indexer, err := service.NewEpisodeIndexer(view, service.NewRepositoryEpisodeIndexSource(stores.Repository),
		embedder, retrieval.VectorIdentity(embedder, service.EpisodeContentVersion))
	if err != nil {
		log.Fatalf("indexer startup failed: %v", err)
	}

	for _, user := range users {
		count, err := indexer.Reindex(ctx, user)
		if err != nil {
			log.Fatalf("reindex user %s failed: %v", user, err)
		}
		fmt.Printf("user %s: rebuilt the retrieval view for %d episode(s)\n", user, count)
	}
}

func buildView(config app.RetrievalConfig) (retrieval.Embedder, service.EpisodeVectorIndex, func() error, error) {
	if config.Mode == app.RetrievalModeDeterministic {
		return retrieval.NewDeterministicEmbedder(0), service.NewMemoryEpisodeVectorIndex(),
			func() error { return nil }, nil
	}
	embedder, err := retrieval.NewHTTPEmbedder(retrieval.EmbeddingsOptions{
		BaseURL: config.Semantic.Embedding.BaseURL, APIKey: config.Semantic.Embedding.APIKey,
		Model: config.Semantic.Embedding.Model, Dimensions: config.Semantic.Embedding.Dimensions,
		Timeout: config.Semantic.Embedding.Timeout, BatchSize: config.Semantic.Embedding.BatchSize,
	})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("embedding configuration is invalid: %v", err)
	}
	index, err := redisindex.NewEpisodeIndex(redisindex.Options{
		Address: config.Semantic.Redis.Address, Password: config.Semantic.Redis.Password,
		Database: config.Semantic.Redis.Database,
		// The episode view always uses its own index name; see the server wiring.
		Dimensions: embedder.Dimensions(),
	})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("redis configuration is invalid: %v", err)
	}
	if err := index.EnsureIndex(context.Background()); err != nil {
		_ = index.Close()
		return nil, nil, nil, fmt.Errorf("the episode index could not be prepared: %v", err)
	}
	return embedder, app.NewRedisEpisodeView(index), index.Close, nil
}
