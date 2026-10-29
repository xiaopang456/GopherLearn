package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/xiaopang456/GopherLearn/internal/knowledge"
	"github.com/xiaopang456/GopherLearn/internal/repository/redisindex"
	"github.com/xiaopang456/GopherLearn/internal/retrieval"
)

func environment(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func semanticEnvironment(overrides map[string]string) map[string]string {
	values := map[string]string{
		EnvRetrievalMode:       string(RetrievalModeSemantic),
		EnvEmbeddingBaseURL:    "http://127.0.0.1:18099/v1",
		EnvEmbeddingModel:      "bge-m3-test",
		EnvEmbeddingDimensions: "4",
		EnvRedisAddress:        "127.0.0.1:16380",
		EnvRerankEndpoint:      "http://127.0.0.1:18083",
		// Required in this mode: the cut belongs to the model in use, so a deployment
		// states it rather than inheriting the placeholder's measured number.
		EnvVectorMinSimilarity: "0.75",
	}
	for name, value := range overrides {
		if value == "" {
			delete(values, name)
			continue
		}
		values[name] = value
	}
	return values
}

func TestRetrievalDefaultsToDeterministicWithoutAnyConfiguration(t *testing.T) {
	config, err := RetrievalConfigFromEnvironment(environment(map[string]string{}))
	if err != nil {
		t.Fatalf("empty environment: %v", err)
	}
	if config.Mode != RetrievalModeDeterministic {
		t.Fatalf("mode = %q, want %q", config.Mode, RetrievalModeDeterministic)
	}
	store := knowledge.NewMemoryStore()
	assembly, err := AssembleRetrieval(store, config)
	if err != nil {
		t.Fatalf("AssembleRetrieval: %v", err)
	}
	// The default store must be the store that was handed in: a decorator appearing by
	// accident would change what every write does in the mode that promises no change.
	if assembly.Store != knowledge.Store(store) {
		t.Fatalf("default assembly replaced the store with %T", assembly.Store)
	}
	if _, ok := assembly.Retriever.(*retrieval.Pipeline); !ok {
		t.Fatalf("default retriever is %T, want the *retrieval.Pipeline the process has always built", assembly.Retriever)
	}
	if !strings.HasPrefix(assembly.StartupLine, "retrieval=deterministic") {
		t.Fatalf("startup line = %q", assembly.StartupLine)
	}
	if err := assembly.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Deterministic mode has no index to prepare, but it still refuses a nil context: the
	// store on the other side of this call treats that as a programming error, and a
	// method that quietly accepted it would let a caller forget the deadline.
	if err := assembly.EnsureIndex(nil); err == nil {
		t.Fatal("EnsureIndex accepted a nil context")
	}
	if err := assembly.EnsureIndex(context.Background()); err != nil {
		t.Fatalf("EnsureIndex in deterministic mode: %v", err)
	}
}

func TestRetrievalRefusesAnUnknownMode(t *testing.T) {
	_, err := RetrievalConfigFromEnvironment(environment(map[string]string{EnvRetrievalMode: "vectorish"}))
	if !errors.Is(err, ErrRetrievalConfig) {
		t.Fatalf("error = %v, want ErrRetrievalConfig", err)
	}
	text := err.Error()
	for _, want := range []string{string(RetrievalModeDeterministic), string(RetrievalModeSemantic)} {
		if !strings.Contains(text, want) {
			t.Errorf("the refusal does not name the closed set: %q missing from %q", want, text)
		}
	}
}

func TestRetrievalDemandsEverySemanticInput(t *testing.T) {
	for _, missing := range []string{
		EnvEmbeddingBaseURL, EnvEmbeddingModel, EnvEmbeddingDimensions, EnvRedisAddress,
	} {
		_, err := RetrievalConfigFromEnvironment(environment(semanticEnvironment(map[string]string{missing: ""})))
		if !errors.Is(err, ErrRetrievalConfig) {
			t.Fatalf("%s unset: error = %v, want a refusal", missing, err)
		}
		// The refusal must name the variable: "semantic mode failed" is not actionable.
		if !strings.Contains(err.Error(), missing) {
			t.Errorf("%s unset: message %q must name it", missing, err.Error())
		}
	}
}

func TestRetrievalRefusesSilentFallbackWhenSemanticIsNotAskedFor(t *testing.T) {
	// An operator who set the embedding variables clearly meant semantic. Starting
	// deterministically anyway would leave them believing notes were being read by a
	// model, which is the failure this whole configuration exists to prevent.
	for _, name := range []string{
		EnvEmbeddingBaseURL, EnvEmbeddingModel, EnvEmbeddingDimensions, EnvRedisAddress,
		EnvRerankEndpoint, EnvVectorMinSimilarity,
	} {
		_, err := RetrievalConfigFromEnvironment(environment(map[string]string{
			EnvRetrievalMode: string(RetrievalModeDeterministic), name: "set-by-accident",
		}))
		if !errors.Is(err, ErrRetrievalConfig) {
			t.Fatalf("%s set without the mode: error = %v, want a refusal", name, err)
		}
		if !strings.Contains(err.Error(), name) {
			t.Errorf("%s set without the mode: message %q must name it", name, err.Error())
		}
	}
}

func TestRetrievalRejectsMalformedSemanticValues(t *testing.T) {
	cases := []struct {
		name      string
		overrides map[string]string
	}{
		{"dimensions are not a number", map[string]string{EnvEmbeddingDimensions: "wide"}},
		{"dimensions are zero", map[string]string{EnvEmbeddingDimensions: "0"}},
		{"dimensions are negative", map[string]string{EnvEmbeddingDimensions: "-8"}},
		{"batch is zero", map[string]string{EnvEmbeddingBatchSize: "0"}},
		{"embedding timeout is not a duration", map[string]string{EnvEmbeddingTimeout: "30"}},
		{"redis database is negative", map[string]string{EnvRedisDatabase: "-1"}},
		{"redis database is not zero", map[string]string{EnvRedisDatabase: "3"}},
		{"similarity cut is zero", map[string]string{EnvVectorMinSimilarity: "0"}},
		{"similarity cut above one", map[string]string{EnvVectorMinSimilarity: "1.5"}},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			overrides := map[string]string{}
			for name, value := range item.overrides {
				overrides[name] = value
			}
			_, err := RetrievalConfigFromEnvironment(environment(semanticEnvironment(overrides)))
			if !errors.Is(err, ErrRetrievalConfig) {
				t.Fatalf("error = %v, want ErrRetrievalConfig", err)
			}
		})
	}
}

func TestAssembleRetrievalSemanticWiresTheSynchronizingStore(t *testing.T) {
	config, err := RetrievalConfigFromEnvironment(environment(semanticEnvironment(map[string]string{
		EnvEmbeddingAPIKey: "embedding-key-must-not-be-logged",
		EnvRedisPassword:   "redis-password-must-not-be-logged",
		EnvRerankEndpoint:  "http://127.0.0.1:18081",
	})))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	store := knowledge.NewMemoryStore()
	assembly, err := AssembleRetrieval(store, config)
	if err != nil {
		t.Fatalf("AssembleRetrieval: %v", err)
	}
	defer func() { _ = assembly.Close() }()
	if _, ok := assembly.Store.(*redisindex.Synchronizer); !ok {
		t.Fatalf("semantic store is %T, want the synchronizing decorator", assembly.Store)
	}
	if assembly.Retriever == nil {
		t.Fatal("semantic retriever is missing")
	}
	if !strings.Contains(assembly.StartupLine, "retrieval=semantic_redis") {
		t.Fatalf("startup line = %q", assembly.StartupLine)
	}
	// The second stage is part of this mode, so the log names the model that provides it
	// rather than just claiming something is wired.
	if !strings.Contains(assembly.StartupLine, "rerank="+retrieval.RerankModel) {
		t.Fatalf("startup line should name the cross-encoder: %q", assembly.StartupLine)
	}
	// The startup line is the one retrieval string that reaches a log. Neither credential
	// may appear in it, in any form.
	for _, secret := range []string{"embedding-key-must-not-be-logged", "redis-password-must-not-be-logged"} {
		if strings.Contains(assembly.StartupLine, secret) {
			t.Fatalf("startup line leaked a credential: %q", assembly.StartupLine)
		}
	}
}

func TestAssembleRetrievalPrintsTheIndexItActuallyUses(t *testing.T) {
	// The startup line is the only place an operator can see which index a semantic boot
	// picked. Printing a constant there would let an overridden index name silently read a
	// different one than the log claims.
	config, err := RetrievalConfigFromEnvironment(environment(semanticEnvironment(map[string]string{
		EnvRedisIndex: "gopherlearn_11a_probe_idx",
	})))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	assembly, err := AssembleRetrieval(knowledge.NewMemoryStore(), config)
	if err != nil {
		t.Fatalf("AssembleRetrieval: %v", err)
	}
	defer func() { _ = assembly.Close() }()
	if !strings.Contains(assembly.StartupLine, "index=gopherlearn_11a_probe_idx@127.0.0.1:16380") {
		t.Fatalf("startup line names the wrong index: %q", assembly.StartupLine)
	}
	if strings.Contains(assembly.StartupLine, "gopherlearn_knowledge_chunks_v1_idx") {
		t.Fatalf("startup line still prints the built-in default: %q", assembly.StartupLine)
	}
}

func TestAssembleRetrievalRefusesARerankerItShouldNotReach(t *testing.T) {
	// The reranker is a local model server by design. A public host is far more likely to
	// be a mistake that starts sending document text outward than an intention.
	config, err := RetrievalConfigFromEnvironment(environment(semanticEnvironment(map[string]string{
		EnvRerankEndpoint: "https://rerank.example.invalid",
	})))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	if _, err := AssembleRetrieval(knowledge.NewMemoryStore(), config); !errors.Is(err, ErrRetrievalConfig) {
		t.Fatalf("error = %v, want the host allowlist to refuse at startup", err)
	}
}

func TestAssembleRetrievalNeedsAStore(t *testing.T) {
	if _, err := AssembleRetrieval(nil, RetrievalConfig{Mode: RetrievalModeDeterministic}); !errors.Is(err, ErrNoKnowledgeStore) {
		t.Fatalf("error = %v, want ErrNoKnowledgeStore", err)
	}
	if _, err := AssembleRetrieval(knowledge.NewMemoryStore(), RetrievalConfig{Mode: "hybrid"}); !errors.Is(err, ErrRetrievalConfig) {
		t.Fatalf("error = %v, want ErrRetrievalConfig for an unresolved mode", err)
	}
}

func TestAssembleRetrievalRefusesAnUnreachableEmbeddingEndpoint(t *testing.T) {
	// Dimensions must agree with the index before anything is written, and an unusable
	// base url is a startup refusal rather than a first-request surprise.
	config, err := RetrievalConfigFromEnvironment(environment(semanticEnvironment(map[string]string{
		EnvEmbeddingBaseURL: "127.0.0.1:18099/v1",
	})))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	if _, err := AssembleRetrieval(knowledge.NewMemoryStore(), config); !errors.Is(err, ErrRetrievalConfig) {
		t.Fatalf("error = %v, want ErrRetrievalConfig", err)
	}
}
