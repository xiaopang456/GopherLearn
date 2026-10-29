package isolated

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	myemail "GopherAI/common/email"
	"GopherAI/common/memoryrecall"
	"GopherAI/common/mysql"
	"GopherAI/common/rag"
	"GopherAI/common/redis"
	"GopherAI/config"
	agentDAO "GopherAI/dao/agent"
	episodeDAO "GopherAI/dao/episode"
	episodeassertdao "GopherAI/dao/episodeassert"
	episoderelationdao "GopherAI/dao/episoderelation"
	knowledgeDAO "GopherAI/dao/knowledge"
	learningDAO "GopherAI/dao/learning"
	memoryDAO "GopherAI/dao/memory"
	"GopherAI/dao/message"
	sessionDAO "GopherAI/dao/session"
	"GopherAI/model"
	agentService "GopherAI/service/agent"
	agentTool "GopherAI/service/agent/tool"
	"GopherAI/service/agent/topicconsent"
	episodeService "GopherAI/service/episode"
	"GopherAI/service/episodeassert"
	episodeassertService "GopherAI/service/episodeassert"
	episoderelationService "GopherAI/service/episoderelation"
	knowledgeService "GopherAI/service/knowledge"
	learningService "GopherAI/service/learning"
	memoryService "GopherAI/service/memory"
	"GopherAI/service/memorysearch"
	userService "GopherAI/service/user"

	"github.com/cloudwego/eino/components/embedding"
)

// knowledgeRetriever is the one method the agent's knowledge tool and the learning evidence
// provider need. *rag.RAGService satisfies it.
type knowledgeRetriever interface {
	Retrieve(ctx context.Context, userID int64, query string, topK int) ([]rag.RetrievalResult, error)
}

type knowledgeRuntime struct {
	// index is the in-process indexing worker. It parses, chunks, embeds and stores a
	// document inside this process; there is no remote executor in this build.
	index     *knowledgeService.IndexWorkerPool
	cleanup   *knowledgeService.CleanupWorkerPool
	retrieval knowledgeRetriever
	documents *knowledgeService.DocumentService
	// embedder is shared with long-term memory. AGENT-020 calibrated its
	// cosine floor against vectors from this model, so memory must not build
	// its own embedder: two models would put the two sides in different spaces
	// and make the threshold meaningless.
	embedder embedding.Embedder
}

func buildKnowledgeRuntime(ctx context.Context, conf *config.Config, deps Dependencies) (*knowledgeRuntime, error) {

	storage, err := knowledgeService.NewLocalFileStorage(conf.KnowledgeConfig.StorageDir)
	if err != nil {
		return nil, fmt.Errorf("initialize knowledge storage: %w", err)
	}
	chunker, err := rag.NewChunker(rag.DefaultChunkerConfig())
	if err != nil {
		return nil, fmt.Errorf("initialize knowledge chunker: %w", err)
	}
	embedder, err := deps.Embedder(ctx, conf)
	if err != nil {
		return nil, fmt.Errorf("initialize knowledge embedder: %w", err)
	}
	chunkRepository, err := deps.Chunks(redis.Rdb, conf.RagModelConfig.RagDimension)
	if err != nil {
		return nil, err
	}
	indexer, err := rag.NewKnowledgeChunkIndexer(embedder, chunkRepository, rag.ChunkIndexConfig{
		Dimension: conf.RagModelConfig.RagDimension,
		BatchSize: 10,
		// The vector cache identity comes from startup configuration, so
		// changing the model or dimension invalidates old vectors instead of
		// silently reusing them.
		EmbeddingModel:          conf.RagModelConfig.RagEmbeddingModel,
		EmbeddingContentVersion: rag.EmbeddingContentVersion,
	})
	if err != nil {
		return nil, err
	}
	ingestor, err := knowledgeService.NewDocumentIngestor(storage, chunker, indexer, conf.KnowledgeConfig.MaxFileSizeBytes)
	if err != nil {
		return nil, err
	}
	documentRepository := knowledgeDAO.NewRepository(mysql.DB)
	documentService := knowledgeService.NewDocumentService(documentRepository, storage, conf.KnowledgeConfig)
	workerConfig := knowledgeService.WorkerConfig{
		WorkerCount:  conf.KnowledgeConfig.WorkerCount,
		QueueSize:    conf.KnowledgeConfig.QueueSize,
		ScanInterval: time.Duration(conf.KnowledgeConfig.ScanIntervalSeconds) * time.Second,
		TaskTimeout:  time.Duration(conf.KnowledgeConfig.IndexTimeoutSeconds) * time.Second,
	}
	cleanupWorker, err := knowledgeService.NewCleanupWorkerPool(documentRepository, chunkRepository, storage, workerConfig)
	if err != nil {
		return nil, err
	}
	indexWorker, err := knowledgeService.NewIndexWorkerPool(
		documentRepository,
		ingestor,
		workerConfig,
	)
	if err != nil {
		return nil, err
	}
	retrievalService, err := rag.NewRAGService(
		ctx,
		embedder,
		chunkRepository,
		func(ctx context.Context, userID int64) ([]rag.ActiveDocument, error) {
			documents, err := documentRepository.ListActiveVersionsByUserID(ctx, userID)
			if err != nil {
				return nil, err
			}
			active := make([]rag.ActiveDocument, 0, len(documents))
			for _, document := range documents {
				active = append(active, rag.ActiveDocument{
					DocID:        document.ID,
					Version:      document.ActiveVersion,
					OriginalName: document.OriginalName,
				})
			}
			return active, nil
		},
		conf.RagModelConfig.RagDimension,
	)
	if err != nil {
		return nil, fmt.Errorf("initialize shared RAG retrieval service: %w", err)
	}
	if conf.RagModelConfig.RagRerankEnabled {
		reranker, rerankErr := rag.NewLocalReranker(
			conf.RagModelConfig.RagRerankEndpoint,
			conf.RagModelConfig.RagRerankToken,
			5*time.Second,
		)
		if rerankErr != nil {
			return nil, fmt.Errorf("initialize local RAG reranker: %w", rerankErr)
		}
		if rerankErr = retrievalService.UseReranker(ctx, reranker); rerankErr != nil {
			return nil, fmt.Errorf("verify local RAG reranker: %w", rerankErr)
		}
	}
	return &knowledgeRuntime{index: indexWorker, cleanup: cleanupWorker, retrieval: retrievalService, documents: documentService, embedder: embedder}, nil
}

// buildMemoryIndex keeps the old re-buildable topic index synchronized for
// diagnostics and rollback. AGENT-027 deliberately does not attach it to the
// production Retriever: Topic UserMemory requires a strong server-derived key,
// so Query Expansion and vector-only selection are outside the read path.
func buildMemoryIndex(repository *memoryDAO.Repository, embedder embedding.Embedder) *memoryService.Indexer {
	if embedder == nil {
		log.Printf("memory similarity recall disabled: no embedder available")
		return nil
	}
	index, err := memoryDAO.NewRedisMemoryIndex(redis.Rdb, redis.MemoryVectorDimension)
	if err != nil {
		log.Printf("memory similarity recall disabled: %v", err)
		return nil
	}
	if err := index.EnsureIndex(context.Background()); err != nil {
		log.Printf("memory similarity recall disabled: ensure index: %v", err)
		return nil
	}
	indexer, err := memoryService.NewIndexer(index, embedder, repository, redis.MemoryVectorDimension)
	if err != nil {
		log.Printf("memory index writer disabled: %v", err)
		return nil
	}
	return indexer
}

// buildMemorySearchRegistry wires the Agent's memory_search tool.
//
// Only agent_memory_tool_v1 reaches this function, and it either returns a
// working registry or an error. There is no degraded form: this mode injects no
// topic memory and no episode of its own, so a tool that failed to build would
// leave every turn with global memory alone while the mode's name still claimed
// otherwise — the silent-wrong-mode failure STABILIZE-031 spent a blind run
// discovering.
// agentRunnerConfig is the run budget for one mode.
//
// Agent memory-tool modes carry per-capability limits. Other modes keep a
// single total, which is what their measurements were taken under; adding
// sub-limits there would change behaviour outside this task.
func agentRunnerConfig(mode memoryrecall.ReadMode, agentConf config.AgentConfig) agentService.RunnerConfig {
	runnerConfig := agentService.RunnerConfig{
		MaxToolCalls: agentConf.MaxToolCalls, MaxModelCalls: agentConf.MaxModelCalls,
		MaxGraphSteps: agentConf.MaxGraphSteps,
		Deadline:      time.Duration(agentConf.DeadlineSeconds) * time.Second,
	}
	if mode.UsesMemorySearchTool() {
		runnerConfig.PerTool = agentService.AgentMemoryToolRunnerConfig().PerTool
		runnerConfig.PerTool.MemorySearch = agentConf.MemoryToolMaxCalls
		// A mode that registers memory_search but is not studying the model's
		// first tool choice keeps the server's knowledge-first rule
		// (STABILIZE-056). The four agent_memory_tool identities are studying
		// exactly that, so they keep deciding it themselves.
		runnerConfig.ForcesKnowledgeSearchWithMemoryTool = !mode.DefersFirstToolChoiceToModel()
	}
	if mode.UsesTopicMemoryReadTool() {
		topicConfig := agentService.AgentMemoryToolV3RunnerConfig()
		if mode.UsesTopicMemoryReadObservationV4() {
			topicConfig = agentService.AgentMemoryToolV4RunnerConfig()
		}
		runnerConfig.PerTool.TopicMemoryRead = topicConfig.PerTool.TopicMemoryRead
	}
	// read_history_message gains a per-capability ceiling only where this round
	// widened what it can reach: two calls, which is one readout of the summary's
	// evidence and one of an episode's. Every older mode keeps the budget its
	// published measurements were taken under - bounded by the whole-run total
	// alone - so the new capability stays the only variable. It is clamped to that
	// total because RunnerConfig.Validate rejects a per-capability limit above it.
	// v2 reads the summary's evidence back; v3 reads the summary's and an episode's.
	// Neither v1's nor v2's answer moves: the predicate is exactly the identities that
	// may read evidence back, and both get the same ceiling.
	if mode.ReadsEvidenceBack() {
		runnerConfig.PerTool.HistoryRead = min(agentService.DefaultHistoryReadCalls, runnerConfig.MaxToolCalls)
	}
	// STABILIZE-061. The surface and the ordering rule travel together with the
	// mode rather than with the deployment's tuning knobs: a config file that
	// could rename the model's tools without changing the mode would make the
	// mode's identity stop describing what the model saw.
	if mode.UsesExplicitMemoryActionNames() {
		runnerConfig.ToolSurface = agentService.SurfaceExplicitActions
		if mode.UsesExplicitKnowledgeSourceName() {
			runnerConfig.ToolSurface = agentService.SurfaceExplicitSources
		}
	}
	if mode.OrdersMemoryDraftBeforeMemoryRead() {
		runnerConfig.ForcesMemoryDraftBeforeMemoryRead = true
	}
	return runnerConfig
}

// buildTopicMemoryReadRegistry keeps the application wiring under the same
// mode predicate the ContextBuilder and runner budget use. STABILIZE-040's
// evaluator constructed this registry directly; without the application-side
// branch a configured v3/v4 process would advertise the tool in its prompt
// while never registering it.
func buildTopicMemoryReadRegistry(mode memoryrecall.ReadMode,
	provider agentService.AuthoritativeTopicMemoryProvider,
	snapshotProvider agentService.AuthoritativeTopicMemorySnapshotProvider) (agentService.ToolRegistry, error) {
	if !mode.UsesTopicMemoryReadTool() {
		return nil, nil
	}
	if mode.UsesTopicMemoryReadObservationV4() {
		return agentTool.NewTopicMemoryReadV4Registry(snapshotProvider, agentTool.DefaultTopicMemoryReadConfig())
	}
	return agentTool.NewTopicMemoryReadRegistry(provider, agentTool.DefaultTopicMemoryReadConfig())
}

func buildMemorySearchRegistry(mode memoryrecall.ReadMode, agentConf config.AgentConfig,
	memoryRepository *memoryDAO.Repository, episodeRepository *episodeDAO.Repository,
	episodeIndex *episodeDAO.RedisEpisodeIndex,
	embedder embedding.Embedder) (*agentTool.MemorySearchRegistry, error) {
	if embedder == nil {
		return nil, fmt.Errorf("agent_memory_tool_v1 needs an embedder")
	}
	if episodeIndex == nil {
		return nil, fmt.Errorf("agent_memory_tool_v1 needs the episode index")
	}
	memoryIndex, err := memoryDAO.NewRedisMemoryIndex(redis.Rdb, redis.MemoryVectorDimension)
	if err != nil {
		return nil, fmt.Errorf("memory index: %w", err)
	}
	if err := memoryIndex.EnsureIndex(context.Background()); err != nil {
		return nil, fmt.Errorf("ensure memory index: %w", err)
	}
	queryEmbedder, err := memoryService.NewEinoQueryEmbedder(embedder, redis.MemoryVectorDimension)
	if err != nil {
		return nil, fmt.Errorf("query embedder: %w", err)
	}
	searchConfig := memorysearch.DefaultConfig()
	searchConfig.CandidateDepth = agentConf.MemoryToolCandidateDepth
	searchConfig.MinConfidence = agentConf.MemoryToolMinConfidence
	searchConfig.MaxResults = agentConf.MemoryToolTopK
	if searchConfig.MaxPerKind > searchConfig.MaxResults {
		searchConfig.MaxPerKind = searchConfig.MaxResults
	}
	searcher, err := memorysearch.NewSearcher(memoryIndex, memoryRepository, episodeIndex,
		episodeRepository, queryEmbedder, searchConfig)
	if err != nil {
		return nil, err
	}
	toolConfig := agentTool.DefaultMemorySearchConfig()
	toolConfig.MaxCalls = agentConf.MemoryToolMaxCalls
	toolConfig.DefaultTopK = agentConf.MemoryToolTopK
	toolConfig.MaxTopK = agentConf.MemoryToolTopK
	toolConfig.Timeout = time.Duration(agentConf.MemoryToolTimeoutMS) * time.Millisecond
	// An empty setting is the shipped window, not an absent one: a deployment
	// that never heard of the experiment reads exactly what it read before.
	if policy := memoryrecall.WindowPolicy(strings.TrimSpace(agentConf.MemoryToolWindowPolicy)); policy != "" {
		toolConfig.WindowPolicy = policy
	}
	registry, err := agentTool.NewMemorySearchRegistry(searcher, message.DefaultRepository(), toolConfig)
	if err != nil {
		return nil, err
	}
	// Assertion time governance is built only for the mode that asks for it,
	// and its absence is a startup failure rather than a runtime degradation.
	// A mode named "governed" that quietly returned ungoverned cards because a
	// DAO was missing would be the worst of both: the freeze would say governed
	// and the answers would not be.
	if mode.GovernsAssertionTime() {
		if mysql.DB == nil {
			return nil, fmt.Errorf("%s needs the assertion state store", mode)
		}
		stateRepository, err := episodeassertdao.NewRepository(mysql.DB)
		if err != nil {
			return nil, fmt.Errorf("assertion state repository: %w", err)
		}
		governor, err := episodeassert.NewGovernor(stateRepository)
		if err != nil {
			return nil, fmt.Errorf("assertion governor: %w", err)
		}
		registry.UseAssertionGovernance(governor)
		// The relation reader is attached to the same governed mode, and
		// deliberately not to the write-side mode. Whether this deployment
		// classifies new relations is a separate decision from whether it may
		// read the ones already stored: tying them together would mean a
		// shadow run's rows could never be looked at without turning writing
		// back on, and the whole point of shadow is to look without writing
		// state.
		//
		// An empty relation table simply annotates nothing, so this is inert
		// until something has written to it.
		relationReader, err := episoderelationdao.NewRepository(mysql.DB)
		if err != nil {
			return nil, fmt.Errorf("assertion relation repository: %w", err)
		}
		registry.UseAssertionRelations(relationReader)
	}
	return registry, nil
}

// buildTopicResolver wires the TopicKey resolver the configured mode calls for.
//
// The two lower modes get nil: legacy_v2 and episode_v3 must not merely skip
// the resolver, they must not have one. episode_v3_lexical constructs no
// semantic dependency at all, so its semantic layer cannot run even by
// mistake. Only episode_v3_semantic builds the full thing, and only with the
// gate numbers supplied explicitly by configuration.
func buildTopicResolver(mode memoryrecall.ReadMode, agentConf config.AgentConfig, repository *memoryDAO.Repository, embedder embedding.Embedder) *memoryService.TopicResolver {
	if !mode.UsesTopicResolver() {
		return nil
	}
	resolverConfig := memoryService.DefaultTopicResolverConfig()
	if !mode.UsesSemanticTopicLayer() {
		resolver, err := memoryService.NewLexicalTopicResolver(repository, resolverConfig)
		if err != nil {
			log.Printf("memory topic resolver disabled: %v", err)
			return nil
		}
		return resolver
	}
	resolverConfig.MinStandout = agentConf.MemoryTopicStandout
	resolverConfig.MinMargin = agentConf.MemoryTopicMargin
	if embedder == nil {
		log.Printf("memory topic resolver disabled: %s needs an embedder", mode)
		return nil
	}
	index, err := memoryDAO.NewRedisMemoryIndex(redis.Rdb, redis.MemoryVectorDimension)
	if err != nil {
		log.Printf("memory topic resolver disabled: %v", err)
		return nil
	}
	queryEmbedder, err := memoryService.NewEinoQueryEmbedder(embedder, redis.MemoryVectorDimension)
	if err != nil {
		log.Printf("memory topic resolver disabled: %v", err)
		return nil
	}
	resolver, err := memoryService.NewSemanticTopicResolver(repository, index, queryEmbedder, repository, resolverConfig)
	if err != nil {
		log.Printf("memory topic resolver disabled: %v", err)
		return nil
	}
	return resolver
}

// knowledgeWorkers lists the knowledge workers in start order (stopped in reverse). This build
// runs the in-process index worker and the cleanup worker; there is no remote executor.
func knowledgeWorkers(runtime *knowledgeRuntime) []namedWorker {
	return []namedWorker{{"knowledge index", runtime.index}, {"knowledge cleanup", runtime.cleanup}}
}

func Assemble(parent context.Context, settings *Settings, deps Dependencies) (app *Application, err error) {
	// The execution mode is settled before anything is opened or acquired.
	if settings == nil || settings.Config == nil {
		return nil, fmt.Errorf("settings must not be nil")
	}
	registration, err := userService.NewRegistrationService(deps.Registration)
	if err != nil {
		return nil, err
	}
	appCtx, l := newLifecycle(parent)
	defer func() {
		if err != nil {
			err = errors.Join(err, l.Close())
		}
	}()
	if err = openResources(appCtx, settings, deps.Infrastructure, l); err != nil {
		return nil, err
	}
	// The mail settings are needed by one thing only: sending a registration
	// captcha. Saying so at startup keeps an operator from hunting for a
	// "service busy" while every other part of the application works.
	if !myemail.Configured() {
		log.Printf("registration by e-mail captcha is unavailable: [emailConfig] has no sender address or authorization code; the server starts and every existing account keeps working")
	}
	conf := settings.Config
	if deps.ChatModel == nil || deps.Embedder == nil || deps.Chunks == nil {
		return nil, fmt.Errorf("model and chunk factories must be explicit")
	}
	runtime, err := buildKnowledgeRuntime(appCtx, conf, deps)
	if err != nil {
		return nil, fmt.Errorf("initialize knowledge runtime: %v", err)
	}
	agentModel, err := deps.ChatModel(appCtx, settings)
	if err != nil {
		return nil, fmt.Errorf("initialize agent model: %w", err)
	}
	var knowledgeTools agentService.ToolRegistry = agentService.StaticToolRegistry{}
	var knowledgeRegistry *agentTool.Registry
	if conf.AgentConfig.MaxToolCalls > 0 {
		knowledgeToolConfig := agentTool.DefaultKnowledgeSearchConfig()
		knowledgeToolConfig.DefaultTopK = conf.AgentConfig.KnowledgeDefaultTopK
		knowledgeToolConfig.MaxTopK = conf.AgentConfig.KnowledgeMaxTopK
		knowledgeToolConfig.MaxPerResultTokens = conf.AgentConfig.KnowledgeMaxResultTokens
		knowledgeToolConfig.MaxObservationTokens = conf.AgentConfig.KnowledgeMaxObservationTokens
		knowledgeToolConfig.MaxRunTokens = conf.AgentConfig.KnowledgeMaxRunTokens
		knowledgeToolConfig.MaxCalls = conf.AgentConfig.MaxToolCalls
		documentReadConfig := agentTool.DefaultDocumentReadConfig()
		documentReadConfig.MaxLines = conf.AgentConfig.DocumentReadMaxLines
		documentReadConfig.MaxBytes = conf.AgentConfig.DocumentReadMaxBytes
		documentReadConfig.MaxObservationTokens = conf.AgentConfig.DocumentReadMaxObservationTokens
		knowledgeRegistry, err = agentTool.NewKnowledgeRegistry(runtime.retrieval, runtime.documents, knowledgeToolConfig, documentReadConfig)
		if err != nil {
			return nil, fmt.Errorf("initialize agent knowledge tools: %v", err)
		}
		knowledgeTools = knowledgeRegistry
	}
	learningTaskState, err := agentService.NewLearningTaskStateProvider(learningDAO.DefaultRepository())
	if err != nil {
		return nil, fmt.Errorf("initialize agent task state provider: %v", err)
	}
	memoryRepository := memoryDAO.DefaultRepository()
	memoryUsage, err := memoryService.NewAsyncUsageRecorder(memoryRepository, 128, 2*time.Second)
	if err != nil {
		return nil, fmt.Errorf("initialize memory usage recorder: %v", err)
	}
	memoryRetriever, err := memoryService.NewRetriever(memoryRepository, learningDAO.DefaultRepository(), memoryUsage, memoryService.DefaultRetrieverConfig())
	if err != nil {
		return nil, fmt.Errorf("initialize memory retriever: %v", err)
	}
	memoryIndexer := buildMemoryIndex(memoryRepository, runtime.embedder)
	// STABILIZE-031: the read path is chosen as one complete mode, and the
	// default is legacy_v2 until a brand-new blind set says otherwise.
	memoryReadMode, err := memoryrecall.ParseReadMode(conf.AgentConfig.MemoryReadMode)
	if err != nil {
		return nil, fmt.Errorf("initialize memory read mode: %v", err)
	}
	log.Printf("memory read mode: %s", memoryReadMode)
	if knowledgeRegistry != nil && memoryReadMode.UsesExplicitKnowledgeSourceName() {
		knowledgeRegistry.UseToolSurface(agentService.SurfaceExplicitSources)
	}
	// agent_memory_tool_v1 needs the Episode index while it is building tools,
	// which happens before the block that builds the index for every other
	// mode. It is opened here rather than moving that block, so no other mode's
	// wiring order changes.
	var memorySearchEpisodeIndex *episodeDAO.RedisEpisodeIndex
	if memoryReadMode.UsesMemorySearchTool() {
		index, indexErr := episodeDAO.NewRedisEpisodeIndex(redis.Rdb, redis.EpisodeVectorDimension)
		if indexErr != nil {
			return nil, fmt.Errorf("initialize agent memory search episode index: %v", indexErr)
		}
		if indexErr = index.EnsureIndex(appCtx); indexErr != nil {
			return nil, fmt.Errorf("initialize agent memory search episode index: %v", indexErr)
		}
		memorySearchEpisodeIndex = index
	}
	memoryTopicResolver := buildTopicResolver(memoryReadMode, conf.AgentConfig, memoryRepository, runtime.embedder)
	if memoryTopicResolver != nil {
		memoryRetriever.UseTopicResolver(memoryTopicResolver)
	}
	memoryGovernance, err := memoryService.NewExplicitGovernanceService(memoryRepository, learningDAO.DefaultRepository(), memoryService.DefaultDreamConfig())
	if err != nil {
		return nil, fmt.Errorf("initialize memory governance: %v", err)
	}
	if memoryIndexer != nil {
		memoryGovernance.UseIndexSync(memoryIndexer)
	}
	globalRelations, err := memoryService.NewGlobalRelationClassifier(agentModel)
	if err != nil {
		return nil, fmt.Errorf("initialize global memory relation classifier: %v", err)
	}
	if err := memoryGovernance.UseGlobalRelations(memoryRepository, globalRelations); err != nil {
		return nil, fmt.Errorf("configure global memory relation classifier: %v", err)
	}
	if err := memoryService.SetDefaultGovernanceService(memoryGovernance); err != nil {
		return nil, fmt.Errorf("configure memory governance: %v", err)
	}
	// One exact-key provider is shared by the v2 bootstrap and the v3 tool. The
	// mode decides which consumer receives it; defining the adapter once prevents
	// the two paths from drifting on filtering or evidence identity.
	topicMemoryProvider := agentService.AuthoritativeTopicMemoryProviderFunc(
		func(ctx context.Context, input agentService.RunContext, keys []string) ([]agentService.TopicMemoryRecord, error) {
			rows, topicErr := memoryRetriever.TopicMemoriesByKeys(ctx, input.UserID, keys)
			if topicErr != nil {
				return nil, topicErr
			}
			records := make([]agentService.TopicMemoryRecord, 0, len(rows))
			for _, row := range rows {
				records = append(records, agentService.TopicMemoryRecord{
					ID: row.ID, Version: row.Version, Owner: row.Owner, Status: row.Status,
					Scope: row.Scope, TopicKey: row.TopicKey, Subject: row.Subject,
					Content: row.Content, Line: row.Line,
				})
			}
			return records, nil
		})
	topicMemorySnapshotProvider := agentService.AuthoritativeTopicMemorySnapshotProviderFunc(
		func(ctx context.Context, input agentService.RunContext, keys []string) (agentService.AuthoritativeTopicMemorySnapshot, error) {
			snapshot, topicErr := memoryRetriever.TopicMemorySnapshotByKeys(ctx, input.UserID, keys)
			if topicErr != nil {
				return agentService.AuthoritativeTopicMemorySnapshot{}, topicErr
			}
			records := make([]agentService.TopicMemoryRecord, 0, len(snapshot.Rows))
			for _, row := range snapshot.Rows {
				records = append(records, agentService.TopicMemoryRecord{
					ID: row.ID, Version: row.Version, Owner: row.Owner, Status: row.Status,
					Scope: row.Scope, TopicKey: row.TopicKey, Subject: row.Subject,
					Content: row.Content, Line: row.Line,
				})
			}
			return agentService.AuthoritativeTopicMemorySnapshot{
				Records: records, EligibleCount: snapshot.EligibleCount,
			}, nil
		})

	// Declared outside the tool block because the confirmed mode's suggester
	// needs the same catalogue the draft tool uses: one list of this learner's
	// topics, read by both, so a topic offered for confirmation is a topic the
	// rest of the system agrees exists.
	var topicCatalogue *memoryService.TopicCatalogue
	var agentTools agentService.ToolRegistry = knowledgeTools
	if conf.AgentConfig.MaxToolCalls > 0 {
		memoryDraftTools, draftErr := agentTool.NewMemoryDraftRegistry(memoryGovernance)
		if draftErr != nil {
			return nil, fmt.Errorf("initialize agent memory draft tool: %v", draftErr)
		}
		catalogue, topicErr := memoryService.NewTopicCatalogue(learningDAO.DefaultRepository(), memoryService.MemoryTopicAdapter{Repository: memoryRepository}, 100)
		if topicErr != nil {
			return nil, fmt.Errorf("initialize memory topic catalogue: %v", topicErr)
		}
		topicCatalogue = catalogue
		memoryDraftTools.UseTopicCatalogue(topicCatalogue)
		if deps.Clock != nil {
			memoryDraftTools.UseClock(deps.Clock)
		}
		// STABILIZE-061 renames the two memory actions together or not at all.
		// Renaming one of them would produce a tool table where a model reading
		// "save_memory_draft" beside "memory_search" still has to guess, which
		// is the arrangement this round exists to replace.
		memoryDraftTools.UseExplicitActionNames(memoryReadMode.UsesExplicitMemoryActionNames())
		historyTools, historyErr := agentTool.NewHistoryMessageRegistry(message.DefaultRepository())
		if historyErr != nil {
			return nil, fmt.Errorf("initialize agent history message tool: %v", historyErr)
		}
		// read_history_message may honour an episode citation only if the store still
		// agrees the card exists, is active, still carries this version, and still
		// cites this id. The rendered card is not that proof: it was read earlier in
		// the same turn, and between render and call a projection can be rewritten or
		// archived. With no reader wired the tool refuses every episode grant rather
		// than trusting the render.
		historyTools.UseEpisodeEvidenceReader(agentTool.EpisodeEvidenceReaderFunc(
			func(ctx context.Context, userID int64, episodeID string) (agentTool.EpisodeEvidenceFacts, error) {
				episode, err := episodeDAO.DefaultRepository().GetByID(ctx, userID, episodeID)
				if err != nil {
					if errors.Is(err, episodeDAO.ErrEpisodeNotFound) {
						return agentTool.EpisodeEvidenceFacts{},
							fmt.Errorf("%w: %s", agentTool.ErrEpisodeEvidenceMissing, episodeID)
					}
					return agentTool.EpisodeEvidenceFacts{}, err
				}
				cited := make([]uint, 0, 12)
				for _, group := range episode.Document.AssertionGroups() {
					for _, entry := range *group.Entries {
						cited = append(cited, entry.EvidenceMessageIDs...)
					}
				}
				return agentTool.EpisodeEvidenceFacts{
					EpisodeID: episode.ID, UserID: episode.UserID, SessionID: episode.SessionID,
					Version: episode.Version, Status: string(episode.Status),
					IndexStatus: string(episode.IndexStatus), Watermark: episode.SourceLastMessageID,
					EvidenceMessageIDs: cited,
				}, nil
			}))
		registries := []agentService.ToolRegistry{knowledgeTools, memoryDraftTools, historyTools}
		if memoryReadMode.UsesMemorySearchTool() {
			// read_history_message stays registered but has nothing to offer
			// under this mode: its allow-list is filled from the episodes the
			// ContextBuilder injected, and this mode injects none. The bounded
			// quotations a learner asks for come back inside memory_search
			// instead, from the episodes the server itself selected, so no
			// second registry holds a private allow-list the first cannot see.
			memorySearchTools, memoryToolErr := buildMemorySearchRegistry(memoryReadMode, conf.AgentConfig,
				memoryRepository, episodeDAO.DefaultRepository(), memorySearchEpisodeIndex, runtime.embedder)
			if memoryToolErr != nil {
				return nil, fmt.Errorf("initialize agent memory search tool: %v", memoryToolErr)
			}
			memorySearchTools.UseSharedIssuedRecords(memoryReadMode.SharesIssuedRecordsAcrossMemoryTools())
			// The user-confirmed mode closes the one door that would let the
			// model reach topic personal state without a confirmation. Without
			// this line the mode's central claim is false while every other
			// part of it looks correct.
			memorySearchTools.UseEpisodicOnly(memoryReadMode.RestrictsMemorySearchToEpisodic())
			memorySearchTools.UseExplicitActionNames(memoryReadMode.UsesExplicitMemoryActionNames())
			registries = append(registries, memorySearchTools)
		}
		topicReadTools, topicReadErr := buildTopicMemoryReadRegistry(memoryReadMode,
			topicMemoryProvider, topicMemorySnapshotProvider)
		if topicReadErr != nil {
			return nil, fmt.Errorf("initialize agent topic memory read tool: %v", topicReadErr)
		}
		if topicReadTools != nil {
			registries = append(registries, topicReadTools)
		}
		agentTools = agentService.CompositeToolRegistry{Registries: registries}
	}
	summaryConfig := agentService.SummaryConfig{
		TriggerMessages: conf.AgentConfig.SummaryTriggerMessages, TriggerTokens: conf.AgentConfig.SummaryTriggerTokens,
		RetainMessages: conf.AgentConfig.SummaryRetainMessages, MaxInputTokens: conf.AgentConfig.SummaryMaxInputTokens,
		MaxOutputTokens: conf.AgentConfig.ContextMaxSummaryTokens, FetchLimit: conf.AgentConfig.SummaryFetchLimit,
		Timeout:     time.Duration(conf.AgentConfig.SummaryTimeoutSeconds) * time.Second,
		StageBudget: time.Duration(conf.AgentConfig.SummaryStageBudgetSeconds) * time.Second,
		// The share stays a code-level invariant: it exists to keep compaction
		// proportional on an already-late request, and there is no deployment
		// where widening it is the right answer.
		MaxDeadlineShare: agentService.DefaultSummaryConfig().MaxDeadlineShare,
		FailureCooldown:  time.Duration(conf.AgentConfig.SummaryFailureCooldownSeconds) * time.Second,
	}
	summaryGenerator, err := agentService.NewModelSummaryGenerator(agentModel, summaryConfig.MaxInputTokens, summaryConfig.MaxOutputTokens)
	if err != nil {
		return nil, fmt.Errorf("initialize agent summary generator: %v", err)
	}
	summaryProvider, err := agentService.NewRollingSummaryProvider(sessionDAO.DefaultRepository(), message.DefaultRepository(), summaryGenerator, summaryConfig)
	if err != nil {
		return nil, fmt.Errorf("initialize agent summary provider: %v", err)
	}
	// AGENT-017: mastery comes straight from TopicProgress on every turn, so a
	// newly graded quiz changes the next explanation without any extraction,
	// governance or staleness window in between.
	masteryProvider, err := agentService.NewMasteryProvider(
		learningDAO.DefaultRepository(),
		learningDAO.DefaultRepository(),
		agentService.DefaultMasteryConfig(),
	)
	if err != nil {
		return nil, fmt.Errorf("initialize agent mastery provider: %v", err)
	}
	// AGENT-021: the band alone tells the model how deep to go; the questions
	// already asked and the points the grader found missing tell it what to
	// cover. Both come from one topic key. A failure here leaves the band in
	// place rather than removing mastery.
	// One bundle provider serves both consumers: the mastery detail block and,
	// under the confirmed mode, the learning half of a confirmed topic. They
	// read the same three stores through the same key, and building two would
	// be two chances for them to disagree about one learner's state.
	topicBundleProvider, bundleErr := agentService.NewTopicBundleProvider(
		learningDAO.DefaultRepository(), learningDAO.DefaultRepository(), memoryRepository,
		agentService.DefaultTopicBundleConfig(),
	)
	if bundleErr != nil {
		log.Printf("agent topic detail disabled: %v", bundleErr)
		topicBundleProvider = nil
	} else {
		masteryProvider.UseTopicDetail(topicBundleProvider)
	}
	// A learner asks about the sub-concept, not the topic they named: someone
	// quizzed on "RabbitMQ 消息确认" asks why autoAck is dangerous. Those words
	// live in the questions they were asked, so matching reads them too.
	masteryProvider.UseTopicTerms(learningDAO.DefaultRepository())
	contextBuilder, err := agentService.NewContextBuilder(agentService.ContextConfig{
		ModelWindowTokens: conf.AgentConfig.ContextModelWindowTokens, InputTargetTokens: conf.AgentConfig.ContextInputTargetTokens,
		OutputReserveTokens: conf.AgentConfig.ContextOutputReserveTokens, EvidenceReserve: conf.AgentConfig.KnowledgeMaxRunTokens,
		MaxGlobalUserMemoryTokens: conf.AgentConfig.ContextMaxGlobalUserMemoryTokens, MaxTopicUserMemoryTokens: conf.AgentConfig.ContextMaxTopicUserMemoryTokens,
		MaxSummaryTokens:  conf.AgentConfig.ContextMaxSummaryTokens,
		MaxMasteryTokens:  conf.AgentConfig.ContextMaxMasteryTokens,
		MaxEpisodeTokens:  conf.AgentConfig.ContextMaxEpisodeTokens,
		EpisodeRecallMode: agentService.EpisodeRecallMode(conf.AgentConfig.EpisodeRecallMode),
		HistoryFetchLimit: conf.AgentConfig.ContextHistoryFetchLimit,
		MemoryReadMode:    memoryReadMode,
	}, message.DefaultRepository(), learningTaskState, masteryProvider, agentService.MemoryProviderFunc(func(ctx context.Context, input agentService.RunContext) (agentService.MemoryRecall, error) {
		recalled, err := memoryRetriever.RelevantMemories(ctx, memoryService.RecallRequest{
			UserID: input.UserID, SessionID: input.SessionID, Question: input.Question,
			StrongTopicKeys: input.StrongTopicKeys, Cycle: input.RecallCycle,
		})
		if err != nil {
			return agentService.MemoryRecall{}, err
		}

		return agentService.MemoryRecall{Global: recalled.Global, Topic: recalled.Topic}, nil
	}), summaryProvider)
	if err != nil {
		return nil, fmt.Errorf("initialize agent context builder: %v", err)
	}
	if memoryReadMode.UsesMemorySearchTool() {
		// The same GlobalMemories function legacy_v2 uses, called directly.
		// "global did not change" is then a property of the code rather than a
		// claim about two copies that happen to agree today.
		contextBuilder.UseGlobalMemoryProvider(agentService.GlobalMemoryProviderFunc(
			func(ctx context.Context, input agentService.RunContext) ([]string, error) {
				global, _, globalErr := memoryRetriever.GlobalMemories(ctx, input.UserID)
				return global, globalErr
			}))
	}
	if memoryReadMode.UsesAuthoritativeTopicBootstrap() {
		// The exact-key reader, wired only under the mode that may use it. It
		// takes the keys the builder computed from server state; it has no way
		// to find keys of its own, which is what keeps the trust boundary a
		// property of the wiring rather than of a comment.
		contextBuilder.UseAuthoritativeTopicMemoryProvider(topicMemoryProvider)
	}
	if memoryReadMode.UsesUserConfirmedTopicContext() {
		// The same exact-key reader, reached under a different authority: here
		// the keys came from a signed confirmation rather than from an active
		// learning session. It still cannot find keys of its own.
		contextBuilder.UseAuthoritativeTopicMemoryProvider(topicMemoryProvider)
		signer, signerErr := topicconsent.NewSigner(conf.AgentConfig.TopicConsentSecret,
			time.Duration(conf.AgentConfig.TopicConsentTTLMinutes)*time.Minute)
		if signerErr != nil {
			return nil, fmt.Errorf("initialize topic consent signer: %v", signerErr)
		}
		contextBuilder.UseTopicConsentVerifier(signer)
		contextBuilder.UseConfirmedTopicLearningStateProvider(agentService.ConfirmedTopicLearningStateProviderFunc(
			func(ctx context.Context, input agentService.RunContext, keys []string) ([]agentService.ConfirmedTopicLearningState, error) {
				return agentService.ReadConfirmedLearningState(ctx, topicBundleProvider, input.UserID, keys)
			}))
		candidateLimit := conf.AgentConfig.TopicConsentMaxCandidates
		if candidateLimit == 0 {
			candidateLimit = topicconsent.MaxCandidates
		}
		suggester, suggestErr := topicconsent.NewSuggester(topicconsent.TopicListerFunc(
			func(ctx context.Context, userID int64) ([]topicconsent.Topic, error) {
				rows, listErr := topicCatalogue.List(ctx, userID)
				if listErr != nil {
					return nil, listErr
				}
				// Copied field by field into a type with nowhere to put a
				// mastery level, so the suggestion path cannot leak one even
				// if the catalogue grows a new column.
				out := make([]topicconsent.Topic, 0, len(rows))
				for _, row := range rows {
					out = append(out, topicconsent.Topic{
						Key: row.Key, Name: row.Name, Graded: row.Graded, Notes: row.Notes,
					})
				}
				return out, nil
			}), candidateLimit)
		if suggestErr != nil {
			return nil, fmt.Errorf("initialize topic suggester: %v", suggestErr)
		}
		suggestionService, suggestionErr := agentService.NewTopicSuggestionService(suggester, signer)
		if suggestionErr != nil {
			return nil, fmt.Errorf("initialize topic suggestion service: %v", suggestionErr)
		}
		agentService.SetDefaultTopicSuggestionService(suggestionService)
	}
	if memoryTopicResolver != nil {
		contextBuilder.UseTopicKeyResolver(agentService.TopicKeyResolverFunc(func(ctx context.Context, input agentService.RunContext) ([]string, error) {
			// The signals come from the read plan rather than being re-derived,
			// so the resolver and the plan cannot disagree about one sentence.
			signals := episodeService.TopicSignalsFor(input.Question)
			return memoryTopicResolver.Resolve(ctx, memoryService.TopicResolveRequest{
				UserID: input.UserID, Question: input.Question,
				AuthoritativeKeys: input.StrongTopicKeys,
				Signals:           &memoryService.TopicSignals{TopicTerms: signals.TopicTerms, Identifiers: signals.Identifiers},
				Cycle:             input.RecallCycle,
			}).Keys, nil
		}))
	}
	agentRunner, err := agentService.NewRunner(
		agentModel,
		checkedRegistry{agentTools},
		agentDAO.DefaultRepository(),
		contextBuilder,
		agentRunnerConfig(memoryReadMode, conf.AgentConfig),
	)
	if err != nil {
		return nil, fmt.Errorf("initialize agent runner: %v", err)
	}
	configuredAgentService, err := agentService.NewDatabaseService(agentRunner)
	if err != nil {
		return nil, fmt.Errorf("initialize agent service: %v", err)
	}
	runLeaseReaper, err := agentService.NewRunLeaseReaper(agentDAO.DefaultRepository(), agentService.DefaultRunLeaseReaperConfig())
	if err != nil {
		return nil, fmt.Errorf("initialize agent run lease reaper: %v", err)
	}
	episodeRepository := episodeDAO.DefaultRepository()
	episodeLoader, err := episodeService.NewDatabaseEvidenceLoader(mysql.DB, conf.AgentConfig.EpisodeMaxMessages)
	if err != nil {
		return nil, fmt.Errorf("initialize episode evidence loader: %v", err)
	}
	episodeGenerator, err := episodeService.NewModelGenerator(agentModel, conf.AgentConfig.EpisodeMaxInputTokens, conf.AgentConfig.EpisodeMaxOutputTokens)
	if err != nil {
		return nil, fmt.Errorf("initialize episode generator: %v", err)
	}
	// The second semantic layer, and it is assembled before the projector
	// because the projector will not be built without it.
	//
	// It reuses the same chat component as the generator, but it is a separate
	// component making a separate request with its own prompt and a minimal
	// context. Reusing the generator's answer would be the model grading
	// itself, and the prompt it is already given says not to promote assistant
	// text into user facts - which is the very thing the pass-1 audit caught it
	// doing.
	//
	// A failure here stops the Episode projector and worker rather than falling
	// back to an unverified projector. Degrading would mean the one deployment
	// whose verifier could not be built is the one that stores every assertion
	// a model produces, unchecked, and it would look exactly like a healthy one.
	episodeSupportVerifier, err := episodeService.NewModelSupportVerifier(
		episodeService.SupportVerifierConfig{Chat: agentModel})
	if err != nil {
		return nil, fmt.Errorf("initialize episode support verifier: %v", err)
	}
	episodeProjector, err := episodeService.NewProjector(episodeLoader, episodeGenerator,
		episodeRepository, episodeSupportVerifier)
	if err != nil {
		return nil, fmt.Errorf("initialize episode projector: %v", err)
	}
	// Only the mode that governs assertion time can orphan a relation, so only
	// it makes the projector fail closed on one. Every other mode keeps the
	// projection behaviour its measurements were produced under.
	if memoryReadMode.GovernsAssertionTime() {
		governanceIndex, err := episodeassertdao.NewRepository(mysql.DB)
		if err != nil {
			return nil, fmt.Errorf("initialize assertion governance index: %v", err)
		}
		episodeProjector.UseGovernanceIndex(governanceIndex)
	}
	// The episodic write-side relation pass.
	//
	// Off is the shipped default and the mode is parsed rather than defaulted:
	// a typo that silently became "off" would look like a deployment that opted
	// out, and one that silently became "apply" would start changing what every
	// learner's memory says on the strength of a classifier nobody has measured
	// on real sessions yet.
	relationMode, err := episoderelationService.ParseMode(conf.AgentConfig.EpisodeRelationMode)
	if err != nil {
		return nil, fmt.Errorf("initialize episode relation mode: %v", err)
	}
	var relationWriter *episoderelationService.RelationWriter
	if relationMode != episoderelationService.ModeOff {
		relationStore, err := episoderelationdao.NewRepository(mysql.DB)
		if err != nil {
			return nil, fmt.Errorf("initialize episode relation store: %v", err)
		}
		relationCandidates, err := episoderelationdao.NewCandidateRepository(mysql.DB,
			episoderelationdao.DefaultHistoryLimit)
		if err != nil {
			return nil, fmt.Errorf("initialize episode relation candidates: %v", err)
		}
		relationStates, err := episodeassertdao.NewRepository(mysql.DB)
		if err != nil {
			return nil, fmt.Errorf("initialize episode relation state store: %v", err)
		}
		relationGovernor, err := episodeassertService.NewGovernor(relationStates)
		if err != nil {
			return nil, fmt.Errorf("initialize episode relation governor: %v", err)
		}
		// The classifier reuses the agent chat component rather than opening a
		// second HTTP client, so there is one place a base URL, a timeout and
		// eventually a meter are configured.
		relationClassifier, err := episoderelationService.NewChatClassifier(
			episoderelationService.ChatClassifierConfig{
				Chat:          agentModel,
				ModelName:     conf.RagModelConfig.RagChatModelName,
				PromptVersion: conf.AgentConfig.EpisodeRelationPromptVersion,
			})
		if err != nil {
			return nil, fmt.Errorf("initialize episode relation classifier: %v", err)
		}
		relationGenerator, err := episoderelationService.New(episoderelationService.Config{
			Source: relationCandidates, Classifier: relationClassifier,
			Proposals: relationStore, Governor: relationGovernor,
			States: relationStates, Mode: relationMode,
			MaxCandidates: conf.AgentConfig.EpisodeRelationMaxCandidates,
		})
		if err != nil {
			return nil, fmt.Errorf("initialize episode relation generator: %v", err)
		}
		relationWriter, err = episoderelationService.NewRelationWriter(relationGenerator)
		if err != nil {
			return nil, fmt.Errorf("initialize episode relation writer: %v", err)
		}
		log.Printf("episode relation pass enabled: mode=%s", relationMode)
	}
	episodeQuietWindow := time.Duration(conf.AgentConfig.EpisodeQuietWindowSeconds) * time.Second
	episodeReconciler, err := episodeService.NewReconciler(episodeRepository, episodeQuietWindow, conf.AgentConfig.EpisodeQueueSize)
	if err != nil {
		return nil, fmt.Errorf("initialize episode reconciler: %v", err)
	}
	episodeWorker, err := episodeService.NewWorker(episodeRepository, episodeProjector, episodeReconciler, episodeService.WorkerConfig{
		WorkerCount: conf.AgentConfig.EpisodeWorkerCount, QueueSize: conf.AgentConfig.EpisodeQueueSize,
		ScanInterval:      time.Duration(conf.AgentConfig.EpisodeScanIntervalSeconds) * time.Second,
		ReconcileInterval: time.Duration(conf.AgentConfig.EpisodeReconcileIntervalSeconds) * time.Second,
		TaskTimeout:       time.Duration(conf.AgentConfig.EpisodeTaskTimeoutSeconds) * time.Second,
		Lease:             time.Duration(conf.AgentConfig.EpisodeLeaseSeconds) * time.Second,
		RetryDelay:        time.Duration(conf.AgentConfig.EpisodeRetryDelaySeconds) * time.Second,
		RecoveryInterval:  time.Duration(conf.AgentConfig.EpisodeRecoveryIntervalSeconds) * time.Second,
		QuietWindow:       episodeQuietWindow, MaxAttempts: conf.AgentConfig.EpisodeMaxAttempts,
	})
	if err != nil {
		return nil, fmt.Errorf("initialize episode worker: %v", err)
	}
	if relationWriter != nil {
		episodeWorker.UseRelationWriter(relationWriter)
	}
	// plannedMemoryWired records whether model_planned_v1 actually got its read
	// path. It is checked after this block, because the block degrades on a
	// Redis outage and that degradation is acceptable for every other mode.
	plannedMemoryWired := false
	var deletionEpisodeIndex episodeService.EpisodeIndex
	// Episode indexing remains a derived, degradable path. Projection keeps
	// running when Redis is unavailable; failed rows are visible through
	// index_status and cmd/episodereindex repairs them later.
	if episodeIndex, indexErr := episodeDAO.NewRedisEpisodeIndex(redis.Rdb, redis.EpisodeVectorDimension); indexErr != nil {
		log.Printf("episode index disabled: %v", indexErr)
	} else {
		deletionEpisodeIndex = episodeIndex
		if indexErr = episodeIndex.EnsureIndex(appCtx); indexErr != nil {
			log.Printf("episode index writer disabled: ensure index: %v", indexErr)
		} else if episodeIndexer, buildErr := episodeService.NewIndexer(episodeIndex, episodeRepository, runtime.embedder, redis.EpisodeVectorDimension); buildErr != nil {
			log.Printf("episode index writer disabled: %v", buildErr)
		} else {
			episodeWorker.UseIndexer(episodeIndexer)
		}
		queryEmbedder, embedErr := memoryService.NewEinoQueryEmbedder(runtime.embedder, redis.EpisodeVectorDimension)
		// The mode decides which read path is constructed at all. Under
		// legacy_v2 the v3 retriever is never built, so no later branch can
		// reach the multi-axis plan, the resolved keys or the recall cycle.
		var episodeRetriever episodeService.Recaller
		var recallErr error
		if embedErr == nil {
			if memoryReadMode.UsesReadPlan() {
				episodeRetriever, recallErr = episodeService.NewRetriever(episodeService.NewQueryRouter(), episodeIndex, episodeRepository, queryEmbedder, episodeService.DefaultRetrieverConfig())
			} else {
				episodeRetriever, recallErr = episodeService.NewLegacyRetriever(episodeIndex, episodeRepository, queryEmbedder, episodeService.DefaultRetrieverConfig())
			}
		}
		// Only this mode builds the small-model chain. Under every other mode
		// nothing here is constructed, so nothing can read a planner key or
		// call a planner endpoint by accident.
		if memoryReadMode.UsesModelPlanner() {
			return nil, fmt.Errorf("experimental model planner is not assembled by this isolated entry")
		}
		if embedErr != nil {
			log.Printf("episode recall vector channel disabled: %v", embedErr)
		} else if recallErr != nil {
			log.Printf("episode recall disabled: %v", recallErr)
		} else if memoryReadMode.UsesModelPlanner() {
			// The planned path owns episode injection for this mode. Attaching
			// the v2 retriever as well would build a second reader that the
			// context builder never consults, and a reader that exists gets
			// called eventually.
			log.Printf("episode recall handled by %s", memoryReadMode)
		} else {
			switch agentService.EpisodeRecallMode(conf.AgentConfig.EpisodeRecallMode) {
			case agentService.EpisodeRecallShadow:
				if observer, observerErr := episodeService.NewShadowObserver(episodeRepository, episodeRetriever); observerErr != nil {
					log.Printf("episode shadow recall disabled: %v", observerErr)
				} else {
					configuredAgentService.SetEpisodeShadowObserver(observer)
				}
			case agentService.EpisodeRecallInject:
				contextBuilder.UseEpisodeProvider(agentService.EpisodeProviderFunc(func(ctx context.Context, input agentService.RunContext) ([]model.EpisodeMemory, error) {
					result, episodeErr := episodeRetriever.Recall(ctx, episodeService.RecallRequest{
						UserID: input.UserID, SessionID: input.SessionID, Question: input.Question,
						CurrentSessionHasHistory: input.CurrentSessionHasHistory,
						ProposedTopicKeys:        input.ProposedTopicKeys, Cycle: input.RecallCycle,
					})
					if episodeErr != nil {
						return nil, episodeErr
					}
					return result.Episodes, nil
				}))
				configuredAgentService.SetEpisodeSessionHistoryReader(episodeRepository)
			}
		}
	}
	if memoryReadMode.UsesModelPlanner() && !plannedMemoryWired {
		// Fail closed at startup rather than at the first learner question. A
		// mode that cannot build its own read path must not run: silently
		// serving turns with no long-term memory under a mode chosen for its
		// memory behaviour is the failure STABILIZE-031 spent a blind run on.
		return nil, fmt.Errorf("initialize model-planned memory recall: the mode is %s but its read path could not be built", memoryReadMode)
	}
	if inventory, inventoryErr := redis.InspectManagedSearchIndexInventory(appCtx, redis.Rdb); inventoryErr != nil {
		log.Printf("inspect managed search indexes: %v", inventoryErr)
	} else {
		if len(inventory.LegacyPresent) > 0 {
			log.Printf("legacy search indexes detected; run cmd/redisindexcleanup dry-run: indexes=%v", inventory.LegacyPresent)
		}
		if len(inventory.UnknownManaged) > 0 {
			log.Printf("unknown managed search indexes require manual review: indexes=%v", inventory.UnknownManaged)
		}
	}
	episodePublisher, err := episodeService.NewPublisher(episodeRepository, episodeQuietWindow)
	if err != nil {
		return nil, fmt.Errorf("initialize episode publisher: %v", err)
	}
	configuredAgentService.SetEpisodePublisher(episodePublisher)
	if err := agentService.SetDefaultService(configuredAgentService); err != nil {
		return nil, fmt.Errorf("configure agent service: %v", err)
	}
	learningEvidence, err := learningService.NewRAGEvidenceProvider(runtime.retrieval)
	if err != nil {
		return nil, fmt.Errorf("initialize learning evidence provider: %v", err)
	}
	learningGenerator, err := learningService.NewModelQuestionGenerator(agentModel)
	if err != nil {
		return nil, fmt.Errorf("initialize learning question generator: %v", err)
	}
	learningGrader, err := learningService.NewModelAnswerGrader(agentModel)
	if err != nil {
		return nil, fmt.Errorf("initialize learning answer grader: %v", err)
	}
	configuredLearningService, err := learningService.NewDatabaseService(learningEvidence, learningGenerator, learningGrader)
	if err != nil {
		return nil, fmt.Errorf("initialize learning service: %v", err)
	}
	if err := learningService.SetDefaultService(configuredLearningService); err != nil {
		return nil, fmt.Errorf("configure learning service: %v", err)
	}
	workers := knowledgeWorkers(runtime)
	workers = append(workers, namedWorker{"episode", episodeWorker}, namedWorker{"run lease reaper", runLeaseReaper}, namedWorker{"memory usage", memoryUsage})
	for _, entry := range workers {
		if err := l.start(appCtx, entry.name, entry.worker); err != nil {
			return nil, err
		}
	}
	handler := buildRouter(runtime.documents, conf, registration)
	return &Application{Lifecycle: l, Handler: handler, Agent: configuredAgentService, Runner: agentRunner, Context: contextBuilder, Documents: runtime.documents, EpisodeIndex: deletionEpisodeIndex}, nil
}
