package app

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/knowledge"
	"github.com/xiaopang456/GopherLearn/internal/repository/redisindex"
	"github.com/xiaopang456/GopherLearn/internal/retrieval"
)

// Retrieval has two modes and exactly one switch between them.
//
// deterministic is the default and is what every existing test, every deployment and
// the HTTP contract see: BM25 plus a feature-hash vector scorer, fused in process. It
// needs no configuration and reaches no network.
//
// semantic asks a real embedder for vectors, keeps them in Redis Stack / RediSearch,
// recalls neighbours from that index and may reorder the fused head with a local
// cross-encoder. It is opt-in by environment variable, and it fails at startup when any
// part of its configuration is missing. Falling back to deterministic instead would be
// the worse outcome: the deployment would keep answering, look healthy, and retrieve by
// surface-token overlap while an operator believes a model is reading their notes.
//
// What stays outside this switch on purpose is everything the retrieval path does not
// own: citations are still built by the server from the chunks it published, and no
// mode lets a model name a source it was not shown.

// Environment variable names owned by the retrieval stack.
const (
	EnvRetrievalMode = "GOPHERLEARN_RETRIEVAL_MODE"

	EnvEmbeddingBaseURL    = "GOPHERLEARN_EMBEDDING_BASE_URL"
	EnvEmbeddingAPIKey     = "GOPHERLEARN_EMBEDDING_API_KEY"
	EnvEmbeddingModel      = "GOPHERLEARN_EMBEDDING_MODEL"
	EnvEmbeddingDimensions = "GOPHERLEARN_EMBEDDING_DIMENSIONS"
	EnvEmbeddingTimeout    = "GOPHERLEARN_EMBEDDING_TIMEOUT"
	EnvEmbeddingBatchSize  = "GOPHERLEARN_EMBEDDING_BATCH_SIZE"

	EnvRedisAddress  = "GOPHERLEARN_REDIS_ADDRESS"
	EnvRedisPassword = "GOPHERLEARN_REDIS_PASSWORD"
	EnvRedisDatabase = "GOPHERLEARN_REDIS_DATABASE"
	EnvRedisIndex    = "GOPHERLEARN_REDIS_INDEX_NAME"

	EnvRerankEndpoint  = "GOPHERLEARN_RERANK_ENDPOINT"
	EnvRerankAuthToken = "GOPHERLEARN_RERANK_AUTH_TOKEN"

	EnvVectorMinSimilarity = "GOPHERLEARN_VECTOR_MIN_SIMILARITY"
)

// chunkContentVersion names how chunk text is produced today. It is part of the vector
// identity, so a change to the chunker invalidates every stored vector instead of
// letting an index mix two incompatible readings of the same document. Bump it together
// with any change to internal/document chunking or to retrieval.EmbeddingInput.
const chunkContentVersion = "document-chunks-v1"

// RetrievalMode is the closed set of retrieval backends this build can start with.
type RetrievalMode string

const (
	RetrievalModeDeterministic RetrievalMode = "deterministic"
	// RetrievalModeSemantic is named after the thing it cannot work without, because an
	// operator reading `semantic` in a startup log has no way to tell "vectors are being
	// computed" from "vectors are being stored and recalled from Redis". They are not the
	// same failure to notice.
	RetrievalModeSemantic RetrievalMode = "semantic_redis"
)

// semanticRequiredVariables are demanded by name in semantic mode. The cross-encoder is on
// the list because this mode's contract is embed -> recall -> re-rank -> server-built
// citation; a deployment that forgot the third stage would still answer, and would look
// like it had a second stage.
//
// GOPHERLEARN_VECTOR_MIN_SIMILARITY is required here rather than defaulted because the only
// number this repository has measured a cut against belongs to the placeholder embedder, and
// a real model needs a different one. Running a semantic deployment at the placeholder's 0.25
// was measured to cite every chunk of a document for a question that shares nothing with it.
// A cut chosen for one model cannot be inherited by the next, so the deployment has to state
// it.
var semanticRequiredVariables = []string{
	EnvEmbeddingBaseURL, EnvEmbeddingModel, EnvEmbeddingDimensions, EnvRedisAddress,
	EnvRerankEndpoint, EnvVectorMinSimilarity,
}

// semanticOnlyVariables is everything the semantic stack reads, which is every retrieval
// variable except the mode itself. Any one of them set while the mode is deterministic stops
// startup, so a half-typed configuration can never look like a working semantic deployment.
var semanticOnlyVariables = append(append([]string{}, semanticRequiredVariables...),
	EnvEmbeddingAPIKey, EnvEmbeddingTimeout, EnvEmbeddingBatchSize,
	EnvRedisPassword, EnvRedisDatabase, EnvRedisIndex,
	EnvRerankAuthToken, EnvVectorMinSimilarity)

var (
	// ErrRetrievalConfig reports retrieval configuration that cannot be honoured. The
	// message always names the variable at fault, because "semantic mode with a typo in
	// one name" is otherwise indistinguishable from a broken embedder.
	ErrRetrievalConfig = errors.New("invalid " + EnvRetrievalMode + " configuration")
	// ErrNoKnowledgeStore reports an assembly attempt with nothing to read from.
	ErrNoKnowledgeStore = errors.New("a knowledge store is required")
)

// EmbeddingSettings configures the HTTP embedder.
type EmbeddingSettings struct {
	BaseURL    string
	APIKey     string
	Model      string
	Dimensions int
	Timeout    time.Duration
	BatchSize  int
}

// RedisSettings configures the vector index connection.
type RedisSettings struct {
	Address  string
	Password string
	Database int
	Index    string
}

// RerankSettings configures the optional cross-encoder. An empty endpoint means the
// fused order stands.
type RerankSettings struct {
	Endpoint  string
	AuthToken string
}

// RetrievalConfig is the resolved retrieval mode and everything semantic mode needs.
type RetrievalConfig struct {
	Mode RetrievalMode
	// Semantic is only read in semantic mode; deterministic mode keeps the pipeline it
	// always built so a half-filled struct cannot change anything.
	Semantic struct {
		Embedding     EmbeddingSettings
		Redis         RedisSettings
		Rerank        RerankSettings
		MinSimilarity float64
	}
}

// RetrievalConfigFromEnvironment resolves the mode and validates it. Required values are
// demanded by name; nothing here falls back to another mode.
func RetrievalConfigFromEnvironment(lookup func(string) string) (RetrievalConfig, error) {
	if lookup == nil {
		lookup = func(string) string { return "" }
	}
	mode := RetrievalMode(strings.ToLower(strings.TrimSpace(lookup(EnvRetrievalMode))))
	switch mode {
	case "", RetrievalModeDeterministic:
		if leftover := semanticVariableLeftBehind(lookup); leftover != "" {
			return RetrievalConfig{}, fmt.Errorf("%w: %s is set while %s=%s; name the mode or drop the value",
				ErrRetrievalConfig, leftover, EnvRetrievalMode, RetrievalModeDeterministic)
		}
		return RetrievalConfig{Mode: RetrievalModeDeterministic}, nil
	case RetrievalModeSemantic:
	default:
		return RetrievalConfig{}, fmt.Errorf("%w: %q is not %s or %s",
			ErrRetrievalConfig, strings.TrimSpace(lookup(EnvRetrievalMode)), RetrievalModeDeterministic, RetrievalModeSemantic)
	}

	config := RetrievalConfig{Mode: RetrievalModeSemantic}
	missing := make([]string, 0, len(semanticRequiredVariables))
	for _, name := range semanticRequiredVariables {
		if strings.TrimSpace(lookup(name)) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return RetrievalConfig{}, fmt.Errorf("%w: %s needs %s", ErrRetrievalConfig, EnvRetrievalMode, strings.Join(missing, ", "))
	}
	config.Semantic.Embedding.BaseURL = strings.TrimSpace(lookup(EnvEmbeddingBaseURL))
	config.Semantic.Embedding.APIKey = strings.TrimSpace(lookup(EnvEmbeddingAPIKey))
	config.Semantic.Embedding.Model = strings.TrimSpace(lookup(EnvEmbeddingModel))
	dimensions, err := strconv.Atoi(strings.TrimSpace(lookup(EnvEmbeddingDimensions)))
	if err != nil || dimensions <= 0 {
		return RetrievalConfig{}, fmt.Errorf("%w: %s must be a positive integer", ErrRetrievalConfig, EnvEmbeddingDimensions)
	}
	config.Semantic.Embedding.Dimensions = dimensions
	if raw := strings.TrimSpace(lookup(EnvEmbeddingTimeout)); raw != "" {
		timeout, err := time.ParseDuration(raw)
		if err != nil || timeout <= 0 {
			return RetrievalConfig{}, fmt.Errorf("%w: %s must be a Go duration such as 30s", ErrRetrievalConfig, EnvEmbeddingTimeout)
		}
		config.Semantic.Embedding.Timeout = timeout
	}
	if raw := strings.TrimSpace(lookup(EnvEmbeddingBatchSize)); raw != "" {
		batch, err := strconv.Atoi(raw)
		if err != nil || batch <= 0 {
			return RetrievalConfig{}, fmt.Errorf("%w: %s must be a positive integer", ErrRetrievalConfig, EnvEmbeddingBatchSize)
		}
		config.Semantic.Embedding.BatchSize = batch
	}
	config.Semantic.Redis.Address = strings.TrimSpace(lookup(EnvRedisAddress))
	config.Semantic.Redis.Password = strings.TrimSpace(lookup(EnvRedisPassword))
	config.Semantic.Redis.Index = strings.TrimSpace(lookup(EnvRedisIndex))
	if raw := strings.TrimSpace(lookup(EnvRedisDatabase)); raw != "" {
		database, err := strconv.Atoi(raw)
		// Measured against a real server: FT.CREATE fails with "Cannot create index on
		// db != 0", so every other selection is a startup that would look healthy and
		// then break on the first upload. Naming it here keeps the reason next to the
		// variable that caused it.
		if err != nil || database != 0 {
			return RetrievalConfig{}, fmt.Errorf("%w: %s must be 0, because the search module only indexes database 0", ErrRetrievalConfig, EnvRedisDatabase)
		}
		config.Semantic.Redis.Database = database
	}
	config.Semantic.Rerank.Endpoint = strings.TrimSpace(lookup(EnvRerankEndpoint))
	config.Semantic.Rerank.AuthToken = strings.TrimSpace(lookup(EnvRerankAuthToken))
	if raw := strings.TrimSpace(lookup(EnvVectorMinSimilarity)); raw != "" {
		value, err := strconv.ParseFloat(raw, 64)
		if err != nil || !(value > 0) || value > 1 {
			return RetrievalConfig{}, fmt.Errorf("%w: %s must be a finite number in (0, 1]", ErrRetrievalConfig, EnvVectorMinSimilarity)
		}
		config.Semantic.MinSimilarity = value
	}
	return config, nil
}

// semanticVariableLeftBehind names the first semantic-only variable an operator set
// without switching the mode on. Silently ignoring it would leave them believing the
// deployment was semantic.
func semanticVariableLeftBehind(lookup func(string) string) string {
	for _, name := range semanticOnlyVariables {
		if strings.TrimSpace(lookup(name)) != "" {
			return name
		}
	}
	return ""
}

// RetrievalAssembly is what the process needs from the retrieval stack: the store to
// hand every consumer, the retriever to answer from it, one line describing the mode,
// and a way to release the index.
type RetrievalAssembly struct {
	// Store is the knowledge store the retriever reads. In semantic mode it is the
	// synchronizing decorator, and callers must use this value rather than the store
	// they passed in - a KnowledgeService built over the undecorated store would accept
	// documents that never reach the index.
	Store     knowledge.Store
	Retriever retrieval.Retriever
	// StartupLine describes retrieval in the log, without a credential or a URL path.
	StartupLine string
	// Close releases the index connection. It is always safe to call.
	Close func() error
}

// AssembleRetrieval builds the stack the configuration asked for.
//
// The deterministic branch calls the same constructor the process has always called, so
// "default behaviour unchanged" is a code path rather than an intention.
func AssembleRetrieval(store knowledge.Store, config RetrievalConfig) (RetrievalAssembly, error) {
	if store == nil {
		return RetrievalAssembly{}, ErrNoKnowledgeStore
	}
	switch config.Mode {
	case RetrievalModeDeterministic:
		return RetrievalAssembly{
			Store: store, Retriever: NewRetrievalPipeline(store),
			StartupLine: "retrieval=deterministic embedder=" + retrieval.NewDeterministicEmbedder(0).Model() +
				" index=none rerank=off",
			Close: func() error { return nil },
		}, nil
	case RetrievalModeSemantic:
	default:
		return RetrievalAssembly{}, fmt.Errorf("%w: unknown mode %q", ErrRetrievalConfig, config.Mode)
	}

	embedder, err := retrieval.NewHTTPEmbedder(retrieval.EmbeddingsOptions{
		BaseURL: config.Semantic.Embedding.BaseURL, APIKey: config.Semantic.Embedding.APIKey,
		Model: config.Semantic.Embedding.Model, Dimensions: config.Semantic.Embedding.Dimensions,
		Timeout: config.Semantic.Embedding.Timeout, BatchSize: config.Semantic.Embedding.BatchSize,
	})
	if err != nil {
		return RetrievalAssembly{}, fmt.Errorf("%w: %s: %v", ErrRetrievalConfig, EnvEmbeddingBaseURL, err)
	}
	index, err := redisindex.New(redisindex.Options{
		Address: config.Semantic.Redis.Address, Password: config.Semantic.Redis.Password,
		Database: config.Semantic.Redis.Database, IndexName: config.Semantic.Redis.Index,
		Dimensions: embedder.Dimensions(),
	})
	if err != nil {
		return RetrievalAssembly{}, fmt.Errorf("%w: %s: %v", ErrRetrievalConfig, EnvRedisAddress, err)
	}
	synchronized, err := redisindex.NewSynchronizer(store, index, embedder, chunkContentVersion)
	if err != nil {
		_ = index.Close()
		return RetrievalAssembly{}, fmt.Errorf("%w: knowledge store and index cannot be wired: %v", ErrRetrievalConfig, err)
	}
	source := knowledge.NewChunkSource(synchronized)
	// The cross-encoder is part of this mode, not an extra bolted on afterwards: a semantic
	// retrieval that recalls by vector distance and never re-reads the candidates is how a
	// wrong chunk ends up cited to a learner. NewHTTPReranker also enforces the local-host
	// allowlist here, so a mistyped public endpoint stops the process instead of shipping
	// document text outward.
	reranker, err := retrieval.NewHTTPReranker(retrieval.RerankerOptions{
		Endpoint: config.Semantic.Rerank.Endpoint, AuthToken: config.Semantic.Rerank.AuthToken,
	})
	if err != nil {
		_ = index.Close()
		return RetrievalAssembly{}, fmt.Errorf("%w: %s: %v", ErrRetrievalConfig, EnvRerankEndpoint, err)
	}
	hybrid, err := retrieval.NewHybrid(retrieval.HybridOptions{
		Source: source, Embedder: embedder, Index: index, Reranker: reranker,
		MinSimilarity: config.Semantic.MinSimilarity, Identity: synchronized.Identity(),
	})
	if err != nil {
		_ = index.Close()
		return RetrievalAssembly{}, fmt.Errorf("%w: %v", ErrRetrievalConfig, err)
	}
	return RetrievalAssembly{
		Store: synchronized, Retriever: hybrid,
		StartupLine: "retrieval=" + string(RetrievalModeSemantic) + " embedder=" + embedder.Model() +
			" index=" + index.Name() + "@" + redisAddressForLog(config.Semantic.Redis) +
			" rerank=" + reranker.Model() +
			" vector_cut=" + strconv.FormatFloat(hybridVectorCut(config), 'g', -1, 64) +
			" identity=" + shortIdentity(synchronized.Identity()),
		Close: func() error { return index.Close() },
	}, nil
}

// EnsureIndex makes the vector index exist before the first write. It is a separate call
// from assembly because startup wants to fail before serving, while a test that only
// reads does not need it.
func (a RetrievalAssembly) EnsureIndex(ctx context.Context) error {
	if ctx == nil {
		return errors.New("app: context is required")
	}
	if a.Store == nil {
		return ErrNoKnowledgeStore
	}
	synchronizer, ok := a.Store.(*redisindex.Synchronizer)
	if !ok {
		// Deterministic mode has no index to prepare, and this is the check that keeps
		// that statement true rather than a comment.
		return nil
	}
	return synchronizer.EnsureIndex(ctx)
}

func redisAddressForLog(settings RedisSettings) string {
	address := strings.TrimSpace(settings.Address)
	if at := strings.LastIndex(address, "@"); at >= 0 {
		// A redis:// URL that carried credentials was refused at construction; trimming
		// here is defence in depth against a log line that would otherwise repeat them.
		address = address[at+1:]
	}
	return fmt.Sprintf("%s/db%d", address, settings.Database)
}

func shortIdentity(identity string) string {
	if len(identity) <= 12 {
		return identity
	}
	return identity[:12]
}

func hybridVectorCut(config RetrievalConfig) float64 {
	if config.Semantic.MinSimilarity > 0 {
		return config.Semantic.MinSimilarity
	}
	return retrieval.DefaultMinCosineSimilarity
}
