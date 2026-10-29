package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/xiaopang456/GopherLearn/internal/app"
	"github.com/xiaopang456/GopherLearn/internal/knowledge"
	"github.com/xiaopang456/GopherLearn/internal/retrieval"
)

// The binary is where a retrieval mode becomes real, so these three cases are the ones an
// operator's typo actually hits. They never open a socket: the point of the default branch
// is that it does not need to, and the point of the semantic branch is that it refuses
// before it could answer from a half-built stack.

func TestOpenRetrievalDefaultModeNeedsNothingOutsideTheProcess(t *testing.T) {
	store := knowledge.NewMemoryStore()
	assembly, _, err := openRetrieval(context.Background(), store, env(map[string]string{}))
	if err != nil {
		t.Fatalf("openRetrieval with no configuration: %v", err)
	}
	defer func() { _ = assembly.Close() }()
	if assembly.Store != knowledge.Store(store) {
		t.Fatalf("default mode replaced the store with %T", assembly.Store)
	}
	if _, ok := assembly.Retriever.(*retrieval.Pipeline); !ok {
		t.Fatalf("default retriever is %T, want the deterministic pipeline", assembly.Retriever)
	}
	if !strings.HasPrefix(assembly.StartupLine, "retrieval=deterministic") {
		t.Fatalf("startup line = %q", assembly.StartupLine)
	}
}

func TestOpenRetrievalRefusesAHalfConfiguredSemanticMode(t *testing.T) {
	// Every case is the same request with one required variable removed. The refusal must
	// name the variable, because the alternative - starting in the default mode - is the
	// silent downgrade this whole configuration exists to make impossible.
	complete := map[string]string{
		app.EnvRetrievalMode:       string(app.RetrievalModeSemantic),
		app.EnvEmbeddingBaseURL:    "http://127.0.0.1:18099/v1",
		app.EnvEmbeddingModel:      "bge-m3-test",
		app.EnvEmbeddingDimensions: "4",
		app.EnvRedisAddress:        "127.0.0.1:16380",
		app.EnvRerankEndpoint:      "http://127.0.0.1:18083",
		app.EnvVectorMinSimilarity: "0.75",
	}
	for _, missing := range []string{
		app.EnvEmbeddingBaseURL, app.EnvEmbeddingModel, app.EnvEmbeddingDimensions,
		app.EnvRedisAddress, app.EnvRerankEndpoint, app.EnvVectorMinSimilarity,
	} {
		values := map[string]string{}
		for name, value := range complete {
			values[name] = value
		}
		delete(values, missing)
		_, _, err := openRetrieval(context.Background(), knowledge.NewMemoryStore(), env(values))
		if !errors.Is(err, app.ErrRetrievalConfig) {
			t.Fatalf("missing %s: error = %v, want ErrRetrievalConfig", missing, err)
		}
		if !strings.Contains(err.Error(), missing) {
			t.Errorf("missing %s: message %q must name it", missing, err.Error())
		}
	}
}

func TestOpenRetrievalRefusesSemanticVariablesWhileDeterministic(t *testing.T) {
	// The other direction: an operator who typed the endpoint but not the mode must not get
	// a deterministic process they believe is semantic.
	_, _, err := openRetrieval(context.Background(), knowledge.NewMemoryStore(), env(map[string]string{
		app.EnvEmbeddingModel: "bge-m3-test",
	}))
	if !errors.Is(err, app.ErrRetrievalConfig) || !strings.Contains(err.Error(), app.EnvEmbeddingModel) {
		t.Fatalf("error = %v, want a refusal naming %s", err, app.EnvEmbeddingModel)
	}
}

func TestStartupLineCarriesNoCredential(t *testing.T) {
	// The semantic startup line is the one retrieval string that reaches a log file, and a
	// log is the least protected thing in a deployment.
	assembly, err := app.AssembleRetrieval(knowledge.NewMemoryStore(), mustRetrievalConfig(t, map[string]string{
		app.EnvRetrievalMode:       string(app.RetrievalModeSemantic),
		app.EnvEmbeddingBaseURL:    "http://127.0.0.1:18099/v1",
		app.EnvEmbeddingModel:      "bge-m3-test",
		app.EnvEmbeddingDimensions: "4",
		app.EnvRedisAddress:        "127.0.0.1:16380",
		app.EnvRedisPassword:       "redis-secret-must-not-be-logged",
		app.EnvEmbeddingAPIKey:     "embedding-key-must-not-be-logged",
		app.EnvRerankEndpoint:      "http://127.0.0.1:18083",
		app.EnvRerankAuthToken:     strings.Repeat("rerank-token-must-not-be-logged-", 2),
		app.EnvVectorMinSimilarity: "0.75",
	}))
	if err != nil {
		t.Fatalf("AssembleRetrieval: %v", err)
	}
	defer func() { _ = assembly.Close() }()
	for _, secret := range []string{
		"embedding-key-must-not-be-logged", "redis-secret-must-not-be-logged",
		"rerank-token-must-not-be-logged",
	} {
		if strings.Contains(assembly.StartupLine, secret) {
			t.Fatalf("startup line leaked a credential: %q", assembly.StartupLine)
		}
	}
	// What it must say instead: the mode, the model names, the index and its host.
	for _, want := range []string{"retrieval=semantic_redis", "embedder=bge-m3-test:4", "index=", "rerank="} {
		if !strings.Contains(assembly.StartupLine, want) {
			t.Errorf("startup line %q does not record %q", assembly.StartupLine, want)
		}
	}
}

func mustRetrievalConfig(t *testing.T, values map[string]string) app.RetrievalConfig {
	t.Helper()
	config, err := app.RetrievalConfigFromEnvironment(env(values))
	if err != nil {
		t.Fatalf("RetrievalConfigFromEnvironment: %v", err)
	}
	return config
}
