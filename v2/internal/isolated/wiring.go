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
	"GopherAI/common/observability"
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
	summarysegmentdao "GopherAI/dao/summarysegment"
	"GopherAI/internal/indexaudit"
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
	modelcomponent "github.com/cloudwego/eino/components/model"
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
	// embedder is shared with long-term memory. The recall index calibrated its
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
	runStartupIndexAudit(ctx, conf, documentRepository)
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
		reranker, rerankErr := rag.NewReranker(rag.RerankerOptions{
			Provider: conf.RagModelConfig.RagRerankProvider,
			Endpoint: conf.RagModelConfig.RagRerankEndpoint,
			Model:    conf.RagModelConfig.RagRerankModel,
			Token:    conf.RagModelConfig.RagRerankToken,
			Timeout:  conf.RagModelConfig.RagRerankTimeout(),
		})
		if rerankErr != nil {
			return nil, fmt.Errorf("initialize RAG reranker: %w", rerankErr)
		}
		if rerankErr = retrievalService.UseReranker(ctx, reranker); rerankErr != nil {
			return nil, fmt.Errorf("verify RAG reranker: %w", rerankErr)
		}
	}
	return &knowledgeRuntime{index: indexWorker, cleanup: cleanupWorker, retrieval: retrievalService, documents: documentService, embedder: embedder}, nil
}

// startupIndexAuditTimeout bounds the startup comparison.
//
// The comparison walks the knowledge prefix three times: once to count the
// blocks each document holds, and twice more for the number the search index
// reports for itself, so it can take a while on a large corpus. The bound
// keeps a slow comparison from turning into a slow start: an audit that did
// not finish is logged and recorded as failed, and the service starts anyway.
const startupIndexAuditTimeout = 2 * time.Minute

// runStartupIndexAudit compares the knowledge record with the knowledge index
// and reports what it finds.
//
// The audit never refuses the start. An index that holds fewer blocks than
// the record says is a retrieval that answers less, not a service that cannot
// run, so the outcome is a report in the log and in the metrics. Only the
// documents that are short are worth an operator's attention, and each of them
// is listed with the command that rebuilds it.
//
// In rebuild mode the documents that are short go back to the indexer, at most
// rebuildLimit of them; the rest are reported and wait for the next start.
//
// Two questions are asked, because they can have different answers: whether
// each document holds the blocks the record counts, and whether the search
// index over those blocks exists and agrees with them. A dropped index keeps
// every block, so the first question alone would call it consistent while
// retrieval returns nothing. The second is reported and not requeued: creating
// the index is what repairs it, and the retrieval service does that while it
// is being assembled, without re-embedding anything.
func runStartupIndexAudit(ctx context.Context, conf *config.Config, documentRepository *knowledgeDAO.Repository) {
	mode, err := indexaudit.ParseMode(conf.KnowledgeConfig.StartupIndexAudit.Mode)
	if err != nil {
		log.Printf("startup index audit skipped: %v", err)
		observability.Memory.KnowledgeIndexAudit(string(indexaudit.ModeOff), "failed", 0, 0, 0, 0, 0, 0)
		return
	}
	if mode == indexaudit.ModeOff {
		return
	}
	auditCtx, cancel := context.WithTimeout(ctx, startupIndexAuditTimeout)
	defer cancel()
	auditor := indexaudit.NewAuditor(
		indexaudit.MySQLSource{DB: mysql.DB, ExcludeInFlight: true},
		indexaudit.RedisCounter{Client: redis.Rdb},
		indexaudit.KnowledgeIndexChecker{Client: redis.Rdb},
		documentRepository,
		log.Printf,
	)
	result, auditErr := auditor.Run(auditCtx, mode, conf.KnowledgeConfig.StartupIndexAudit.StartupIndexAuditRebuildLimit())
	if auditErr != nil {
		log.Printf("startup index audit failed: %v", auditErr)
		observability.Memory.KnowledgeIndexAudit(string(mode), "failed", 0, 0, 0, 0, 0, 0)
		return
	}
	observability.Memory.KnowledgeIndexAudit(string(mode), result.Status(), result.Compared, len(result.Missing), len(result.Extra), result.Rebuilt, result.Skipped, result.Failed)
}

// buildMemoryIndex keeps the old re-buildable topic index synchronized for
// diagnostics and rollback. It is deliberately not attached to the
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
// otherwise — the silent-wrong-mode failure an earlier round spent a blind run
// discovering.
// agentRunnerConfig is the run budget for one mode.
//
// Agent memory-tool modes carry per-capability limits. Other modes keep a
// single total, which is what their measurements were taken under; adding
// sub-limits there would change behaviour outside this task.
func agentRunnerConfig(mode memoryrecall.ReadMode, agentConf config.AgentConfig, thinkingDialect string) agentService.RunnerConfig {
	runnerConfig := agentService.RunnerConfig{
		MaxToolCalls: agentConf.MaxToolCalls, MaxModelCalls: agentConf.MaxModelCalls,
		MaxGraphSteps: agentConf.MaxGraphSteps,
		Deadline:      time.Duration(agentConf.DeadlineSeconds) * time.Second,
		// A thinking turn writes its answer after the thinking and takes
		// measurably longer, so it runs under its own deadline. The plain one
		// stays where it was for every turn whose thinking is off.
		DeadlineThinking: time.Duration(agentConf.DeadlineThinkingSeconds) * time.Second,
		ThinkingDialect:  thinkingDialect,
		// The pre-retrieval's threshold is a deployment setting, because the
		// reranker's scores move with the corpus and with the reranker itself,
		// and the comparison run is what should move this number.
		RelevanceThreshold: agentConf.KnowledgeRelevanceThreshold,
		// The window keeps this much for the reply, and every generation of the
		// turn is sent with it as the output limit. It is the same number the
		// window's arithmetic rests on, so the request and the budget that
		// measured it cannot drift apart.
		OutputReserveTokens: agentConf.ContextOutputReserveTokens,
	}
	if mode.UsesMemorySearchTool() {
		runnerConfig.PerTool = agentService.AgentMemoryToolRunnerConfig().PerTool
		runnerConfig.PerTool.MemorySearch = agentConf.MemoryToolMaxCalls
		// A mode that registers memory_search but is not studying the model's
		// first tool choice keeps the server's knowledge-first rule.
		// The four agent_memory_tool identities are studying
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
	// The explicit-actions surface. The surface and the ordering rule travel together with the
	// mode rather than with the deployment's tuning knobs: a config file that
	// could rename the model's tools without changing the mode would make the
	// mode's identity stop describing what the model saw.
	runnerConfig.ToolSurface = toolSurfaceFor(mode)
	// The thin card's cross-session tools get a ceiling of their own.
	//
	// The ceiling is the point of the thin card: the question is answered from
	// ONE earlier conversation, so the round trips are bounded - find the
	// conversation, read its summary or its board, answer. A second card
	// search with different words is allowed, because the first card's
	// evidence may not be the conversation the learner meant; the segment
	// search is allowed once, because it is the fallback and it costs an
	// embedding; the board is read once per conversation.
	//
	// The ceilings are clamped to the whole-run tool budget, because
	// RunnerConfig.Validate refuses a per-capability limit above it.
	if agentConf.SessionCardThin() {
		runnerConfig.PerTool.SearchSessionCards = min(2, runnerConfig.MaxToolCalls)
		runnerConfig.PerTool.SearchHistorySegments = min(1, runnerConfig.MaxToolCalls)
		runnerConfig.PerTool.ReadSessionWhiteboard = min(2, runnerConfig.MaxToolCalls)
		// The question the model asks when the candidates cannot be told
		// apart is one question, and the turn ends with it.
		runnerConfig.PerTool.AskWhichConversation = min(1, runnerConfig.MaxToolCalls)
	}
	if mode.OrdersMemoryDraftBeforeMemoryRead() {
		runnerConfig.ForcesMemoryDraftBeforeMemoryRead = true
	}
	return runnerConfig
}

// toolSurfaceFor is the surface the tools of one mode are named under.
//
// It is a function because the surface and the ordering rule travel together
// with the mode rather than with the deployment's tuning knobs: a config file
// that could rename the model's tools without changing the mode would make
// the mode's identity stop describing what the model saw.
//
// The tool block and the runner read it from here, so a tool registered under
// one name is never called under another.
func toolSurfaceFor(mode memoryrecall.ReadMode) agentService.ToolSurface {
	if !mode.UsesExplicitMemoryActionNames() {
		return agentService.SurfaceLegacy
	}
	if mode.UsesExplicitKnowledgeSourceName() {
		return agentService.SurfaceExplicitSources
	}
	return agentService.SurfaceExplicitActions
}

// buildTopicMemoryReadRegistry keeps the application wiring under the same
// mode predicate the ContextBuilder and runner budget use. The earlier round's
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
	// The layered summary's write path. It is assembled here rather than next
	// to the worker because the applied mode's read side needs it: the mode
	// decides both what is written and what is sent, and a deployment that
	// wrote the segments without reading them would pay for a model call per
	// batch and answer nothing.
	summarySegmentAssembly, err := buildSummarySegmentAssembly(appCtx, conf,
		func(c context.Context) (modelcomponent.ToolCallingChatModel, error) {
			// The check has to be here, not in the function this closure is
			// passed to. A closure is never nil, so the assembly's own guard
			// cannot see a missing dependency: it would build, and the first
			// segment batch would call a nil function.
			if deps.JSONChatModel == nil {
				return nil, fmt.Errorf("segment writer needs a model factory; dependencies.JSONChatModel is nil")
			}
			return deps.JSONChatModel(c, settings)
		})
	if err != nil {
		return nil, fmt.Errorf("initialize summary segment assembly: %v", err)
	}
	// The segments' derived search vector.
	//
	// The index is created in every mode, like the episode index: it is the
	// instrument the segments are found with, and a deployment that has not
	// turned the mode on has no segments to put in it. Its writer is attached
	// only when the write path exists.
	summarySegmentIndex, indexErr := summarysegmentdao.NewRedisSummarySegmentIndex(redis.Rdb, redis.SummarySegmentVectorDimension)
	if indexErr != nil {
		log.Printf("summary segment index disabled: %v", indexErr)
	} else if indexErr = summarySegmentIndex.EnsureIndex(appCtx); indexErr != nil {
		log.Printf("summary segment index disabled: ensure index: %v", indexErr)
	} else if summarySegmentAssembly.Enabled() {
		indexer, buildErr := agentService.NewSummarySegmentIndexer(summarySegmentIndex,
			sessionDAO.DefaultRepository(), runtime.embedder, redis.SummarySegmentVectorDimension)
		if buildErr != nil {
			log.Printf("summary segment index writer disabled: %v", buildErr)
		} else {
			summarySegmentAssembly.UseIndexPublisher(indexer)
			summarySegmentAssembly.UseIndexRepairer(indexer)
		}
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
	// The read path is chosen as one complete mode, and the
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
	// The session-finalize extraction is the third phase's automatic
	// proposal. It is wired end to end - the close notification, the idle
	// fallback, the real draft entry and the confirmation list - but stays
	// off until the config turns it on, because the structured extraction
	// model is not verified against real sessions yet (the plan's next
	// batch). An off deployment registers nothing, and the controller
	// answers the notification with that fact.
	if conf.AgentConfig.MemoryFinalizeEnabled {
		finalizeCatalogue, finalizeErr := memoryService.NewTopicCatalogue(learningDAO.DefaultRepository(), memoryService.MemoryTopicAdapter{Repository: memoryRepository}, 100)
		if finalizeErr != nil {
			return nil, fmt.Errorf("initialize finalize topic catalogue: %v", finalizeErr)
		}
		finalizeProposer, finalizeErr := memoryService.NewModelSessionProposer(agentModel)
		if finalizeErr != nil {
			return nil, fmt.Errorf("initialize finalize extraction model: %v", finalizeErr)
		}
		finalizeService, finalizeErr := memoryService.BuildSessionFinalizeService(
			sessionDAO.DefaultRepository(), message.DefaultRepository(), memoryRepository,
			finalizeCatalogue, finalizeProposer, globalRelations, nil)
		if finalizeErr != nil {
			return nil, fmt.Errorf("initialize session finalize service: %v", finalizeErr)
		}
		finalizeRunner, finalizeErr := memoryService.NewFinalizeRunner(finalizeService, memoryGovernance, memoryRepository)
		if finalizeErr != nil {
			return nil, fmt.Errorf("initialize finalize runner: %v", finalizeErr)
		}
		if finalizeErr := memoryService.SetDefaultFinalizeRunner(finalizeRunner); finalizeErr != nil {
			return nil, fmt.Errorf("configure finalize runner: %v", finalizeErr)
		}
		finalizeSweeper, finalizeErr := memoryService.NewFinalizeIdleSweeper(finalizeRunner, memoryService.DefaultFinalizeSweeperConfig())
		if finalizeErr != nil {
			return nil, fmt.Errorf("initialize finalize idle sweeper: %v", finalizeErr)
		}
		if finalizeErr := finalizeSweeper.Start(appCtx); finalizeErr != nil {
			return nil, fmt.Errorf("start finalize idle sweeper: %v", finalizeErr)
		}
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
	// sessionCardEnabled says the card path is running. It is read here, in
	// the tool block, and again in the memory block further down, and both
	// read the same flag: the tools and the read path have to agree about
	// which path is on, or a turn could be offered a tool it may not use.
	sessionCardEnabled := conf.AgentConfig.SessionCardEnabled()
	// sessionCardThin says the card path is running with the THIN card: the
	// card the program derives, which costs no model call and carries no
	// body.
	//
	// The two cards are different instruments, so they are two flags. What
	// they share is the path they belong to: both replace the Episode path,
	// and both are reached through the same cross-session tools.
	sessionCardThin := conf.AgentConfig.SessionCardThin()
	// sessionCardPathEnabled says that a card path is running, whichever
	// card it writes. The tools that reach across sessions are registered
	// under it: a card arm without them could find a session and not be
	// able to read it.
	sessionCardPathEnabled := sessionCardEnabled || sessionCardThin
	// The thin card's own parts.
	//
	// They are assembled here, before the tools, because the tools are what
	// make the card worth keeping: a card the turn cannot reach would be a
	// card nobody ever finds, and a card nobody finds costs a write per
	// session for nothing.
	//
	// thinCards is the writer that keeps a session's card current, and
	// thinCardRetriever is the reader that finds the earlier sessions.
	var thinCards *episodeService.ThinCardService
	var thinCardRetriever *episodeService.ThinCardRetriever
	var thinCardProjectionRunner episodeService.ProjectionRunner
	// thinCardIndex is the index the thin cards live in. It is kept here
	// because it is also the index a deleted conversation's card has to be
	// removed from: the card must not outlive the session it describes.
	var thinCardIndex *episodeDAO.RedisThinCardIndex
	// deletionDerivedIndex is the index the session deletion cleans. It is
	// the Episode index on the old path and the thin card index under the
	// card path - the index this assembly actually writes is the index the
	// deletion has to clean.
	var deletionDerivedIndex episodeService.DerivedIndexDeleter
	if sessionCardThin {
		thinLoader, thinLoaderErr := episodeService.NewDatabaseThinCardSourceLoader(mysql.DB)
		if thinLoaderErr != nil {
			return nil, fmt.Errorf("initialize thin card source loader: %v", thinLoaderErr)
		}
		thinProjector, thinProjectorErr := episodeService.NewThinCardProjector(thinLoader,
			episodeService.ProgrammaticThinCardDeriver{}, episodeDAO.DefaultRepository())
		if thinProjectorErr != nil {
			return nil, fmt.Errorf("initialize thin card projector: %v", thinProjectorErr)
		}
		// The thin card's index has no vector field: a vector needs an
		// embedding, and an embedding is a model call. The card is found by
		// its words - the title, the topics, the names - which is what a
		// question about an earlier session is phrased in.
		thinIndex, thinIndexErr := episodeDAO.NewRedisThinCardIndex(redis.Rdb,
			thinCardIndexName(conf), thinCardKeyPrefix(conf))
		if thinIndexErr != nil {
			return nil, fmt.Errorf("initialize thin card index: %v", thinIndexErr)
		}
		if thinIndexErr = thinIndex.EnsureIndex(appCtx); thinIndexErr != nil {
			return nil, fmt.Errorf("ensure thin card index: %v", thinIndexErr)
		}
		thinIndexer, thinIndexerErr := episodeService.NewThinCardIndexer(thinIndex,
			episodeDAO.CardIndexSource{Repository: episodeDAO.DefaultRepository()})
		if thinIndexerErr != nil {
			return nil, fmt.Errorf("initialize thin card indexer: %v", thinIndexerErr)
		}
		thinCardIndex = thinIndex
		thinService, thinServiceErr := episodeService.NewThinCardService(thinProjector, thinIndexer)
		if thinServiceErr != nil {
			return nil, fmt.Errorf("initialize thin card service: %v", thinServiceErr)
		}
		thinCards = thinService
		thinRetriever, thinRetrieverErr := episodeService.NewThinCardRetriever(thinIndex,
			episodeDAO.ThinCardLoader{Repository: episodeDAO.DefaultRepository()}, thinCardCandidateLimit)
		if thinRetrieverErr != nil {
			return nil, fmt.Errorf("initialize thin card retriever: %v", thinRetrieverErr)
		}
		thinCardRetriever = thinRetriever
		thinJobProjector, thinJobErr := episodeService.NewThinCardJobProjector(thinService)
		if thinJobErr != nil {
			return nil, fmt.Errorf("initialize thin card job projector: %v", thinJobErr)
		}
		thinCardProjectionRunner = thinJobProjector
	}
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
		// One earlier change renames the two memory actions together or not at all.
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
				switch episode.ContentKind.Normalized() {
				case model.EpisodeContentSessionCard:
					cited = append(cited, episode.Card.CitedMessageIDs()...)
				default:
					for _, group := range episode.Document.AssertionGroups() {
						for _, entry := range *group.Entries {
							cited = append(cited, entry.EvidenceMessageIDs...)
						}
					}
				}
				return agentTool.EpisodeEvidenceFacts{
					EpisodeID: episode.ID, UserID: episode.UserID, SessionID: episode.SessionID,
					Version: episode.Version, Status: string(episode.Status),
					IndexStatus: string(episode.IndexStatus), Watermark: episode.SourceLastMessageID,
					ContentKind:        string(episode.ContentKind.Normalized()),
					EvidenceMessageIDs: cited,
				}, nil
			}))
		// A summary segment's citation is honoured only if the segment is
		// still there and still covers the same turns.
		historyTools.UseSummarySegmentEvidenceReader(
			agentTool.NewSummarySegmentEvidenceReader(sessionDAO.DefaultRepository()))
		// The assistant's own messages are readable only in the mode whose
		// summary records them. The grant set still decides which ids exist
		// for the run, so an assistant message nobody cited stays unreadable.
		historyTools.UseAssistantContent(conf.AgentConfig.SummaryAssistantContentEnabled())
		// Under the card path the same turn offers the conversation search,
		// and the search grants read-back for every hit it returns. The tool
		// table is built before any tool runs, so the read-back tool must stay
		// on the table while the grant set is still empty - otherwise the hit
		// the search just showed could not be read in full. The per-call grant
		// check remains the authorisation boundary.
		historyTools.UseOnDemandSearchGrants(sessionCardPathEnabled)
		// A search hit is a window onto a sentence: the answer often sits in
		// the message beside it. The neighbour read is authorised by the same
		// grant as the hit, so this does not widen what the run may read.
		historyTools.UseAdjacentRead(sessionCardPathEnabled)
		registries := []agentService.ToolRegistry{knowledgeTools, memoryDraftTools, historyTools}
		if summarySegmentAssembly.Applies() {
			// browse_summary_segments opens the session's older segments. It
			// is registered only in the applied mode, because there are no
			// segments to browse in the other two.
			browseTools, browseErr := agentTool.NewBrowseSummarySegmentsRegistry(sessionDAO.DefaultRepository())
			if browseErr != nil {
				return nil, fmt.Errorf("initialize agent summary segment browse tool: %v", browseErr)
			}
			if summarySegmentIndex != nil {
				// A segment the model opened is a segment the learner is
				// asking about, and the next turn should find it in front of
				// them without being asked again.
				browseTools.UseRecentUse(summarySegmentIndex)
			}
			// The session parameter is what lets the model open the source
			// conversation of a card. It is attached only under the card
			// path, because the tool's schema is part of its contract and the
			// modes whose measurements were published must keep the schema
			// they were measured with.
			browseTools.UseSessionParameter(sessionCardPathEnabled)
			// A named earlier conversation's summary can stop before the
			// conversation settles. The words after the summary are the only
			// place the later decision lives, and the browse tool carries
			// them when it opens such a conversation.
			browseTools.UseTailReader(message.DefaultRepository())
			registries = append(registries, browseTools)
		}
		if sessionCardPathEnabled {
			// The keyword search is the last step of the card's chain: the
			// detail that neither the card nor the summary kept.
			//
			// It is registered only under the card path. A deployment that
			// does not run the cards has no source conversation to search
			// in, and offering the tool would invite calls that can only be
			// refused.
			sessionSearchTools, sessionSearchErr := agentTool.NewSearchSessionHistoryRegistry(message.DefaultRepository())
			if sessionSearchErr != nil {
				return nil, fmt.Errorf("initialize agent session history search tool: %v", sessionSearchErr)
			}
			// The same switch the read-back path uses: a deployment that does
			// not record the assistant's products does not get them back
			// through a search either.
			sessionSearchTools.UseAssistantContent(conf.AgentConfig.SummaryAssistantContentEnabled())
			registries = append(registries, sessionSearchTools)
		}
		if sessionCardThin {
			// The three on-demand tools of the thin card.
			//
			// The card finds the conversation, the segment search finds the
			// stretch of turns when the card did not, and the whiteboard
			// says what that conversation finally arranged. Each one reads
			// only what it names, and each one is registered only under the
			// thin card path - a deployment that does not keep the cards has
			// no cross-session door to offer.
			cardSearchTools, cardSearchErr := agentTool.NewSearchSessionCardsRegistry(thinCardRetriever)
			if cardSearchErr != nil {
				return nil, fmt.Errorf("initialize agent session card search tool: %v", cardSearchErr)
			}
			cardSearchTools.UseToolSurface(toolSurfaceFor(memoryReadMode))
			registries = append(registries, cardSearchTools)
			// The segment search is the fallback of the card search, and the
			// only one of the three that makes a model call: it embeds the
			// question to reach the segments' own vectors. That call is
			// counted apart from the card's cost, which is zero.
			//
			// It needs the segment index, and a deployment without that
			// index has no segments to search: the tool is not registered
			// rather than registered broken.
			if summarySegmentIndex != nil {
				historySegmentTools, historySegmentErr := agentTool.NewSearchHistorySegmentsRegistry(
					summarySegmentIndex, sessionDAO.DefaultRepository(), thinCardRetriever)
				if historySegmentErr != nil {
					return nil, fmt.Errorf("initialize agent history segment search tool: %v", historySegmentErr)
				}
				// The query embedder is the segment index's own dimension:
				// the segments were written with the shared embedder at that
				// size, and a vector of another size is not a vector of that
				// space.
				queryEmbedder, embedderErr := memoryService.NewEinoQueryEmbedder(runtime.embedder,
					redis.SummarySegmentVectorDimension)
				if embedderErr != nil {
					return nil, fmt.Errorf("initialize history segment query embedder: %v", embedderErr)
				}
				historySegmentTools.UseSemanticChannel(summarySegmentIndex, queryEmbedder)
				// A hit segment's summary can stop before the conversation
				// settles; the words after it carry the later decision.
				historySegmentTools.UseTailReader(message.DefaultRepository())
				historySegmentTools.UseToolSurface(toolSurfaceFor(memoryReadMode))
				registries = append(registries, historySegmentTools)
			}
			// The whiteboard is the conversation's current state: what it had
			// settled on when it was last talked in.
			whiteboardTools, whiteboardErr := agentTool.NewReadSessionWhiteboardRegistry(sessionDAO.DefaultRepository())
			if whiteboardErr != nil {
				return nil, fmt.Errorf("initialize agent session whiteboard read tool: %v", whiteboardErr)
			}
			whiteboardTools.UseToolSurface(toolSurfaceFor(memoryReadMode))
			registries = append(registries, whiteboardTools)
			// The question the model asks when the candidates cannot be
			// told apart.
			//
			// It is registered with the card retriever rather than with the
			// index, because a candidate is offered only when the learner
			// owns it and it has a card - the same boundary the search
			// itself keeps.
			askTools, askErr := agentTool.NewAskWhichConversationRegistry(thinCardRetriever)
			if askErr != nil {
				return nil, fmt.Errorf("initialize agent ask-which-conversation tool: %v", askErr)
			}
			askTools.UseToolSurface(toolSurfaceFor(memoryReadMode))
			registries = append(registries, askTools)
		}
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
		Budget: agentService.SummaryBudgetOf(conf.AgentConfig.SummaryGroupItemLimit,
			conf.AgentConfig.SummaryOpenLoopsItemLimit, conf.AgentConfig.SummaryEvidenceIDLimit),
	}
	summaryGenerator, err := agentService.NewModelSummaryGenerator(agentModel, summaryConfig.MaxInputTokens, summaryConfig.MaxOutputTokens, summaryConfig.Budget)
	if err != nil {
		return nil, fmt.Errorf("initialize agent summary generator: %v", err)
	}
	summaryProvider, err := agentService.NewRollingSummaryProvider(sessionDAO.DefaultRepository(), message.DefaultRepository(), summaryGenerator, summaryConfig)
	if err != nil {
		return nil, fmt.Errorf("initialize agent summary provider: %v", err)
	}
	// In the applied mode the segments are the compression, so the rolling
	// summary is neither read nor written.
	//
	// Two compressors over one stream would pay for the same turns twice and
	// the window would carry both. The old table keeps what it already holds
	// and stops growing; the downstream switch to the board and the segments
	// is a later step.
	summarySource := agentService.SummaryProvider(summaryProvider)
	if summarySegmentAssembly.Applies() {
		summarySource = agentService.EmptySummaryProvider{}
	}
	// Mastery comes straight from TopicProgress on every turn, so a
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
	// The band alone tells the model how deep to go; the questions
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
		MaxSummaryTokens: conf.AgentConfig.ContextMaxSummaryTokens,
		MaxMasteryTokens: conf.AgentConfig.ContextMaxMasteryTokens,
		MaxEpisodeTokens: conf.AgentConfig.ContextMaxEpisodeTokens,
		// The layered summary's four block budgets. They are configuration, not
		// constants: a deployment that turns the mode on is choosing how much
		// of the window the board and the segments may take, and a scaled-down
		// run has to be able to say a smaller number.
		//
		// The two segment blocks carry the effective numbers rather than the
		// configured ones: a block has to hold one whole segment, and the
		// segment is larger in the mode that records the assistant's
		// products.
		MaxWhiteboardTokens:       conf.AgentConfig.ContextMaxWhiteboardTokens,
		MaxResidentSegmentTokens:  conf.AgentConfig.EffectiveResidentSegmentTokens(),
		MaxRetrievedSegmentTokens: conf.AgentConfig.EffectiveRetrievedSegmentTokens(),
		MaxSegmentTokens:          conf.AgentConfig.EffectiveSegmentTokens(),
		AssistantContent:          conf.AgentConfig.SummaryAssistantContentEnabled(),
		MinRecentMessages:         conf.AgentConfig.ContextMinRecentMessages,
		EpisodeRecallMode:         agentService.EpisodeRecallMode(conf.AgentConfig.EpisodeRecallMode),
		HistoryFetchLimit:         conf.AgentConfig.ContextHistoryFetchLimit,
		MemoryReadMode:            memoryReadMode,
	}, message.DefaultRepository(), learningTaskState, masteryProvider, agentService.MemoryProviderFunc(func(ctx context.Context, input agentService.RunContext) (agentService.MemoryRecall, error) {
		recalled, err := memoryRetriever.RelevantMemories(ctx, memoryService.RecallRequest{
			UserID: input.UserID, SessionID: input.SessionID, Question: input.Question,
			StrongTopicKeys: input.StrongTopicKeys, Cycle: input.RecallCycle,
		})
		if err != nil {
			return agentService.MemoryRecall{}, err
		}

		return agentService.MemoryRecall{Global: recalled.Global, Topic: recalled.Topic}, nil
	}), summarySource)
	if err != nil {
		return nil, fmt.Errorf("initialize agent context builder: %v", err)
	}
	// The server's own door to the learner's other conversations.
	//
	// It is attached under the card path only: a deployment that does not
	// keep the cards has no earlier conversation to point at, and a window
	// that resolved a clarification against conversations the deployment
	// never indexed would be reading a history it does not have.
	//
	// The locator is the card search - one lightweight lookup, no model
	// call - and the reader locks one conversation of the learner's own and
	// reads the evidence out of it. They go together: a reader without a
	// locator could only answer a click, and a locator without a reader
	// could only find candidates.
	if sessionCardThin {
		crossStore, crossStoreErr := agentService.NewDatabaseCrossSessionStore(
			sessionDAO.DefaultRepository(), message.DefaultRepository())
		if crossStoreErr != nil {
			return nil, fmt.Errorf("initialize cross session store: %v", crossStoreErr)
		}
		crossReader, crossReaderErr := agentService.NewDatabaseCrossSessionReader(crossStore)
		if crossReaderErr != nil {
			return nil, fmt.Errorf("initialize cross session reader: %v", crossReaderErr)
		}
		contextBuilder.UseCrossSession(
			agentService.CrossSessionLocatorFunc(func(ctx context.Context, userID int64,
				currentSession, clue string) ([]agentService.SessionCandidate, error) {
				found, recallErr := thinCardRetriever.Recall(ctx, userID, currentSession,
					agentTool.ThinCardQueryFor(clue, clue))
				if recallErr != nil {
					return nil, recallErr
				}
				out := make([]agentService.SessionCandidate, 0, len(found))
				for _, candidate := range found {
					out = append(out, agentService.SessionCandidate{
						SessionID:    candidate.SessionID,
						Title:        candidate.Title,
						Topics:       append([]string{}, candidate.Topics...),
						LastTalkedAt: candidate.LastTalkedAt,
					})
				}
				return out, nil
			}),
			crossReader,
		)
	}
	// The layered summary's read side. It is attached only in the applied
	// mode: the off and the shadow modes send exactly what they sent before.
	if summarySegmentAssembly.Applies() {
		// The preselection runs on two channels: the words of the question
		// and the meaning of it. The vector channel needs the index the
		// segments are written to, and without it the preselection runs on
		// the words alone - a narrower selection, not a broken one.
		var embed func(context.Context, string) ([]float32, error)
		var vectorSearcher agentService.SummarySegmentVectorSearcher
		if summarySegmentIndex != nil {
			queryEmbedder, embedErr := memoryService.NewEinoQueryEmbedder(runtime.embedder, redis.SummarySegmentVectorDimension)
			if embedErr != nil {
				log.Printf("summary segment query embedder disabled: %v", embedErr)
			} else {
				embed = queryEmbedder.EmbedQuery
				vectorSearcher = summarySegmentIndex
			}
		}
		preselector, preselectErr := buildSummarySegmentPreselector(conf, embed, vectorSearcher)
		if preselectErr != nil {
			return nil, fmt.Errorf("initialize summary segment preselector: %v", preselectErr)
		}
		if summarySegmentIndex != nil {
			preselector.UseRecentUse(summarySegmentIndex)
		}
		layeredProvider, layeredErr := agentService.NewDatabaseLayeredSummaryProvider(
			sessionDAO.DefaultRepository(), preselector,
			agentService.ResidentSegmentCount(conf.AgentConfig.EffectiveResidentSegmentTokens(),
				conf.AgentConfig.EffectiveSegmentTokens()))
		if layeredErr != nil {
			return nil, fmt.Errorf("initialize layered summary reader: %v", layeredErr)
		}
		if summarySegmentIndex != nil {
			// The segments the last few turns reached for stay in front of the
			// learner, so a question about the same stretch does not have to
			// find it again.
			//
			// The record only adds to the resident set: the newest segments
			// are read first and the block walks them from the newest
			// backwards, so what the record brings back takes the room that
			// is left and never the room the newest ones need.
			layeredProvider.UseRecentUse(summarySegmentIndex)
		}
		// A session the old path summarised has neither a board nor a segment.
		// Converting it when it is read gives it the same shape as a new one
		// without waiting for the background.
		migrator, migrateErr := agentService.NewSummarySegmentMigrator(
			sessionDAO.DefaultRepository(), sessionDAO.DefaultRepository(), sessionDAO.DefaultRepository())
		if migrateErr != nil {
			return nil, fmt.Errorf("initialize summary segment migrator: %v", migrateErr)
		}
		// The conversion writes the version the deployment's mode writes, so
		// a deployment that rolls back can still read what was converted
		// while the mode was off.
		migrator.UseAssistantContent(conf.AgentConfig.SummaryAssistantContentEnabled())
		layeredProvider.UseMigrator(migrator)
		if attachErr := contextBuilder.UseLayeredSummaryProvider(layeredProvider); attachErr != nil {
			return nil, fmt.Errorf("attach layered summary reader: %v", attachErr)
		}
		// The fallback generates a segment before the answer when the raw
		// conversation no longer fits. It is the last resort, after the
		// background has had its turn.
		contextBuilder.UseSummarySegmentFallback(summarySegmentAssembly)
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
		registryForRunner(agentTools),
		agentDAO.DefaultRepository(),
		contextBuilder,
		agentRunnerConfig(memoryReadMode, conf.AgentConfig, conf.RagModelConfig.RagChatThinkingDialect),
	)
	if err != nil {
		return nil, fmt.Errorf("initialize agent runner: %v", err)
	}
	configuredAgentService, err := agentService.NewDatabaseService(agentRunner,
		agentService.WithThinkingEnabled(conf.AgentConfig.AgentThinkingEnabled))
	if err != nil {
		return nil, fmt.Errorf("initialize agent service: %v", err)
	}
	runLeaseReaper, err := agentService.NewRunLeaseReaper(agentDAO.DefaultRepository(), agentService.DefaultRunLeaseReaperConfig())
	if err != nil {
		return nil, fmt.Errorf("initialize agent run lease reaper: %v", err)
	}
	episodeRepository := episodeDAO.DefaultRepository()
	// The session-card path and the Episode path are alternatives, and the
	// configuration picks one.
	//
	// They share the job table, the worker, the lease, the retry budget and
	// the versioning, so a deployment that switches paths keeps the same
	// scheduler and only changes what the projector writes. What it must
	// not do is run both: two generation models would mean two answers to
	// the same question about the same conversation, and the learner would
	// pay for both.
	// The session-card path replaces the Episode path, and this is the one
	// place that says so for the tools as well as the memory.
	var projectionRunner episodeService.ProjectionRunner
	if sessionCardEnabled {
		cardLoader, cardLoaderErr := episodeService.NewDatabaseCardEvidenceLoader(mysql.DB, conf.AgentConfig.EpisodeMaxMessages)
		if cardLoaderErr != nil {
			return nil, fmt.Errorf("initialize session card evidence loader: %v", cardLoaderErr)
		}
		cardGenerator, cardGeneratorErr := episodeService.NewModelCardGenerator(agentModel,
			conf.AgentConfig.EpisodeMaxInputTokens, cardOutputTokens(conf.AgentConfig))
		if cardGeneratorErr != nil {
			return nil, fmt.Errorf("initialize session card generator: %v", cardGeneratorErr)
		}
		cardProjector, cardProjectorErr := episodeService.NewCardProjector(cardLoader, cardGenerator, episodeRepository)
		if cardProjectorErr != nil {
			return nil, fmt.Errorf("initialize session card projector: %v", cardProjectorErr)
		}
		projectionRunner = cardProjector
		log.Printf("session card path enabled: the episode projector is not run")
	} else if sessionCardThin {
		// The thin card path.
		//
		// The card is derived by the program from what the session already
		// compressed, so there is no model to ask and no document to write:
		// the card is a title, a time, the topics and names the summary
		// segments recorded, and the way back to those segments.
		//
		// What it saves is the whole cost of the full card: one chat call
		// per session, and a card that repeats the summary in the window.
		// What it gives up is the card's own body, and the body is read from
		// the source session when a question needs it.
		//
		// The assembly itself lives above, next to the tools that read it:
		// the tools are what make the card worth keeping, and a card the
		// tools cannot reach would be a card nobody ever finds.
		projectionRunner = thinCardProjectionRunner
		// The card is kept current from two directions.
		//
		// After a segment is written, the segment's own topic, names and
		// number become the card's clues and the card's way back to the
		// body. After an answer ends, the title and the last talked time
		// move.
		//
		// Both go through the same service, and neither of them asks a
		// model: the card is written from what is already on disk.
		configuredAgentService.SetThinCardSyncer(thinCards)
		summarySegmentAssembly.UseThinCardSyncer(thinCards)
		log.Printf("thin session card path enabled: the card is derived by the program and costs no model call")
	} else {
		episodeLoader, err := episodeService.NewDatabaseEvidenceLoader(mysql.DB, conf.AgentConfig.EpisodeMaxMessages)
		if err != nil {
			return nil, fmt.Errorf("initialize episode evidence loader: %v", err)
		}
		// In the applied mode the episode is projected from the raw messages only.
		//
		// The old rolling summary is a frozen picture of an earlier part of the
		// session, taken before the layered summary existed. A value that was
		// replaced afterwards is still in it, and reading it would let a stale
		// statement become an assertion about the learner. Reading the summary
		// segments instead is a later change.
		if !agentService.EpisodeReadsLegacySummary(conf.AgentConfig.SummarySegmentMode) {
			episodeLoader.WithoutLegacySummary()
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
		projectionRunner = episodeProjector
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
	if sessionCardPathEnabled {
		// The reconciler repairs a missed enqueue by looking for the derived
		// row that is behind. A card that is behind is not repaired by an
		// Episode that is up to date, so the reconciler has to look at the
		// kind the path writes.
		episodeReconciler.UseContentKind(model.EpisodeContentSessionCard)
	}
	episodeWorker, err := episodeService.NewWorker(episodeRepository, projectionRunner, episodeReconciler, episodeService.WorkerConfig{
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
	// episodeRetriever is declared out here because the card path reads it
	// from outside the index block: the card path is attached whether or not
	// the index was built, and a retriever that could not be built is
	// reported rather than silently absent.
	var episodeRetriever episodeService.Recaller
	// Episode indexing remains a derived, degradable path. Projection keeps
	// running when Redis is unavailable; failed rows are visible through
	// index_status and cmd/episodereindex repairs them later.
	// The card path binds the same instrument to the card index's own
	// namespace. Two indexes rather than one index with two kinds of row in
	// it: a search that could see both would answer "what did we decide"
	// with whichever kind of row ranked higher.
	episodeIndex, indexErr := episodeDAO.NewRedisEpisodeIndex(redis.Rdb, redis.EpisodeVectorDimension)
	var indexSource episodeService.EpisodeIndexSource = episodeRepository
	if sessionCardEnabled {
		cardIndexName := strings.TrimSpace(conf.AgentConfig.SessionCardIndexName)
		cardKeyPrefix := strings.TrimSpace(conf.AgentConfig.SessionCardIndexKeyPrefix)
		if cardIndexName == "" {
			cardIndexName = redis.SessionCardIndexName
		}
		if cardKeyPrefix == "" {
			cardKeyPrefix = redis.SessionCardKeyPrefix
		}
		episodeIndex, indexErr = episodeDAO.NewRedisEpisodeIndexWithConfig(redis.Rdb, redis.EpisodeVectorDimension,
			episodeDAO.EpisodeIndexConfig{IndexName: cardIndexName, KeyPrefix: cardKeyPrefix})
		indexSource = episodeDAO.CardIndexSource{Repository: episodeRepository}
	}
	if indexErr != nil {
		log.Printf("episode index disabled: %v", indexErr)
	} else if sessionCardThin {
		// The thin card path writes the thin card index and nothing else.
		// The Episode index is a different instrument with a vector field,
		// and the thin card's index has no vector field at all - so the
		// Episode index is not created, and the worker is not given an
		// indexer that would write the thin card's row into it.
		//
		// The thin card's own index is the one the session deletion cleans:
		// a card must not outlive the conversation it describes.
		deletionDerivedIndex = thinCardIndex
		log.Printf("thin card path: the episode index is not written")
	} else {
		deletionDerivedIndex = episodeIndex
		if indexErr = episodeIndex.EnsureIndex(appCtx); indexErr != nil {
			log.Printf("episode index writer disabled: ensure index: %v", indexErr)
		} else if episodeIndexer, buildErr := episodeService.NewIndexer(episodeIndex, indexSource, runtime.embedder, redis.EpisodeVectorDimension); buildErr != nil {
			log.Printf("episode index writer disabled: %v", buildErr)
		} else {
			episodeWorker.UseIndexer(episodeIndexer)
		}
		queryEmbedder, embedErr := memoryService.NewEinoQueryEmbedder(runtime.embedder, redis.EpisodeVectorDimension)
		// The mode decides which read path is constructed at all. Under
		// legacy_v2 the v3 retriever is never built, so no later branch can
		// reach the multi-axis plan, the resolved keys or the recall cycle.
		var recallErr error
		if embedErr == nil {
			if sessionCardEnabled {
				// The card retriever is the same instrument over the card
				// index and the card rows, and it says so in its config so
				// that the evidence filter looks in the card's slots.
				cardConfig := episodeService.DefaultRetrieverConfig()
				cardConfig.CardMode = true
				episodeRetriever, recallErr = episodeService.NewRetriever(episodeService.NewQueryRouter(), episodeIndex,
					episodeDAO.CardLoader{Repository: episodeRepository}, queryEmbedder, cardConfig)
			} else if memoryReadMode.UsesReadPlan() {
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
		} else if !sessionCardEnabled {
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
	// The card path's read side.
	//
	// It is attached outside the recall-mode switch because the card path is
	// an explicit opt-in rather than a mode: a deployment that turned it on
	// asked for the cards to be shown, and the Episode recall mode still
	// describes the Episode path.
	if sessionCardEnabled && episodeRetriever != nil {
		contextBuilder.UseSessionCardProvider(agentService.SessionCardProviderFunc(
			func(ctx context.Context, input agentService.RunContext) ([]model.EpisodeMemory, error) {
				result, cardErr := episodeRetriever.Recall(ctx, episodeService.RecallRequest{
					UserID: input.UserID, SessionID: input.SessionID, Question: input.Question,
					CurrentSessionHasHistory: input.CurrentSessionHasHistory,
					ProposedTopicKeys:        input.ProposedTopicKeys, Cycle: input.RecallCycle,
				})
				if cardErr != nil {
					return nil, cardErr
				}
				return result.Episodes, nil
			}))
		// The reader is what tells the run that this session has history,
		// and the cards are only fetched for a run that has one.
		configuredAgentService.SetEpisodeSessionHistoryReader(episodeRepository)
		log.Printf("session card read path enabled")
	}
	if memoryReadMode.UsesModelPlanner() && !plannedMemoryWired {
		// Fail closed at startup rather than at the first learner question. A
		// mode that cannot build its own read path must not run: silently
		// serving turns with no long-term memory under a mode chosen for its
		// memory behaviour is the failure an earlier round spent a blind run on.
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
	// The layered summary's write path. It is assembled only when the mode
	// says it should run, and the enqueue that feeds it is wired either way:
	// an assembly that nobody asks does nothing, and an assembly that is
	// asked but not started is a queue that fills up and nobody drains.
	configuredAgentService.SetSummarySegmentEnqueuer(summarySegmentAssembly.Enqueuer())
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
	if summarySegmentAssembly.Enabled() {
		workers = append(workers, namedWorker{"summary segment", summarySegmentAssembly})
	}
	for _, entry := range workers {
		if err := l.start(appCtx, entry.name, entry.worker); err != nil {
			return nil, err
		}
	}
	handler := buildRouter(runtime.documents, conf, registration)
	return &Application{Lifecycle: l, Handler: handler, Agent: configuredAgentService, Runner: agentRunner, Context: contextBuilder, Documents: runtime.documents, DerivedIndex: deletionDerivedIndex}, nil
}

// cardOutputTokens is how many tokens the card writer may use.
//
// The card writes its whole document into a tool call, so its budget has to
// hold the document AND the JSON wrapper around it. The Episode's 1800 was
// written for a document of assertions, and a card of a real session ran into
// it: the model used every token it was given, the arguments stopped in the
// middle of the JSON, and the decoder refused the document as a protocol
// violation.
//
// A deployment that says nothing gets the card's own budget. A deployment
// that sets one gets that one, and the number is validated with the rest of
// the configuration.
func cardOutputTokens(agent config.AgentConfig) int {
	if agent.SessionCardMaxOutputTokens > 0 {
		return agent.SessionCardMaxOutputTokens
	}
	return agent.EpisodeMaxOutputTokens
}

// thinCardIndexName is the thin card index's name.
//
// It is the deployment's own, and a deployment that says nothing gets the
// default. The thin card's index has no vector field, so it cannot share a
// namespace with the full card's index.
func thinCardIndexName(conf *config.Config) string {
	if name := strings.TrimSpace(conf.AgentConfig.SessionCardThinIndexName); name != "" {
		return name
	}
	return redis.ThinCardIndexName
}

// thinCardKeyPrefix is the thin card index's key prefix.
func thinCardKeyPrefix(conf *config.Config) string {
	if prefix := strings.TrimSpace(conf.AgentConfig.SessionCardThinIndexKeyPrefix); prefix != "" {
		return prefix
	}
	return redis.ThinCardKeyPrefix
}

// thinCardCandidateLimit is how many candidate sessions a thin card search
// returns.
//
// It is a handful: the card is a finding aid and the reader is a person who
// has to recognise the session, so a list of five is already more than one
// person can choose between.
const thinCardCandidateLimit = 5

// buildSummarySegmentPreselector builds the preselector on the budgets the
// window will actually render with.
//
// The preselector keeps its own defaults for the harnesses that construct it
// directly, but a deployment must not run on them: left alone, it hands the
// renderer more segments than the block holds, and the renderer - which walks
// from the newest backwards - keeps the newest rather than the highest ranked.
func buildSummarySegmentPreselector(conf *config.Config, embed memoryrecall.EmbedFunc,
	vectors agentService.SummarySegmentVectorSearcher) (*agentService.SummarySegmentPreselector, error) {
	preselector, err := agentService.NewSummarySegmentPreselector(embed, vectors)
	if err != nil {
		return nil, err
	}
	preselector.UseLimits(agentService.SummarySegmentPreselectLimits(
		conf.AgentConfig.EffectiveRetrievedSegmentTokens(), conf.AgentConfig.EffectiveSegmentTokens()))
	return preselector, nil
}

// buildSummarySegmentAssembly wires the layered summary's write path.
//
// It returns nil when the mode is off, which is the shipped default: the
// tables exist and the worker is one line away, but nothing reads the
// segments yet.
//
// The mode is recorded on the counters before anything is assembled, so the
// metrics carry it from the first attempt. A shadow run and an applied run
// answer different questions, and their numbers have to be told apart.
func buildSummarySegmentAssembly(ctx context.Context, conf *config.Config,
	jsonChatModel func(context.Context) (modelcomponent.ToolCallingChatModel, error)) (*agentService.SummarySegmentAssembly, error) {
	settings, err := agentService.SummarySegmentAssemblyConfigFromSettings(conf.AgentConfig)
	if err != nil {
		return nil, err
	}
	observability.Memory.SetMode(settings.Mode)
	if settings.Mode == agentService.SummarySegmentModeOff {
		return nil, nil
	}
	// The segment writer answers in JSON, and the provider enforces it. The
	// prompt has said "Output JSON only" all along; a body that still came
	// back unparseable cost the whole stretch of turns. The answer path keeps
	// the model it had: one response format for both would put every answer
	// in JSON.
	//
	// The model comes from this entry's own factory, not from the environment:
	// the key, the model name and the endpoint are the operator's, and this
	// entry refuses environment credentials everywhere else.
	if jsonChatModel == nil {
		return nil, fmt.Errorf("segment json model factory must be explicit")
	}
	segmentModel, err := jsonChatModel(ctx)
	if err != nil {
		return nil, err
	}
	return agentService.NewSummarySegmentAssembly(
		settings,
		sessionDAO.DefaultRepository(),
		nil,
		segmentModel,
	)
}
