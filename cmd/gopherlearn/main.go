package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/agent"
	"github.com/xiaopang456/GopherLearn/internal/app"
	"github.com/xiaopang456/GopherLearn/internal/httpapi"
	"github.com/xiaopang456/GopherLearn/internal/knowledge"
	"github.com/xiaopang456/GopherLearn/internal/model"
	"github.com/xiaopang456/GopherLearn/internal/modelcall"
	"github.com/xiaopang456/GopherLearn/internal/repository"
	"github.com/xiaopang456/GopherLearn/internal/repository/gormmysql"
	"github.com/xiaopang456/GopherLearn/internal/security"
	"github.com/xiaopang456/GopherLearn/internal/service"
	"github.com/xiaopang456/GopherLearn/internal/storage"
)

// shutdownGrace is the drain window: how long the process keeps serving requests that were
// already in flight after it has been asked to stop. It is a window, not a promise - an event
// stream still open when it expires is cut, because whoever sent the signal has already
// decided this instance goes away. `stop_grace_period` in compose.app.yml is larger than this
// so the container runtime's SIGKILL does not race the drain.
const shutdownGrace = 10 * time.Second

// workerStopGrace bounds the episodic worker's shutdown wait. It is separate from the HTTP
// drain on purpose: the worker is not serving connections, it is finishing one projection
// write, and five seconds covers the persistence paths that release a lease.
const workerStopGrace = 5 * time.Second

// stopEpisodeWorker stops the episodic worker and reports whether its loops
// finished inside the grace window.
//
// The timeout context is created HERE, at exit time, not at startup: a context
// built when the process started would already be expired by the first real
// shutdown of a process that ran longer than the window, and the wait it bounds
// would silently become a poll. The same rule the drain follows - the deadline
// describes the wait, so the deadline is built where the wait happens.
func stopEpisodeWorker(worker *service.EpisodeWorker, grace time.Duration) error {
	stop, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	return worker.Stop(stop)
}

// errShutdownForced reports a drain that ran out of time. It is a distinct error rather than
// a nil because "stopped cleanly" and "stopped with busy connections cut off" must not read
// the same in the log.
var errShutdownForced = errors.New("gopherlearn: shutdown window closed with requests still running")

func newHTTPServer(address string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              address,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
}

// serveUntil serves until ctx is cancelled, then drains.
//
// Shutdown stops the listener first - so a request arriving after the signal is refused rather
// than queued behind the drain - and then waits for what is already running. The wait has a
// deadline; if it fires, the remaining connections are closed hard and the caller is told the
// drain did not finish. The process still exits successfully, because refusing to exit would
// turn a graceful stop into a SIGKILL.
//
// serve is injected so a test can bind an ephemeral port and still exercise the real
// ListenAndServe/Shutdown pairing.
func serveUntil(ctx context.Context, server *http.Server, grace time.Duration, serve func() error) error {
	stopped := make(chan error, 1)
	go func() { stopped <- serve() }()

	select {
	case err := <-stopped:
		// The listener failed at startup - a port already in use is the usual case - or
		// somebody closed the server from inside the process.
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}

	drain, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	if err := server.Shutdown(drain); err != nil {
		_ = server.Close()
		return errors.Join(errShutdownForced, err)
	}
	return nil
}

// openStorage resolves storage configuration and builds the repository. It is
// separate from main so the startup decision, including which errors stop the
// process, can be tested without a database.
func openStorage(ctx context.Context, lookup func(string) string, opts storage.Options) (storage.Stores, storage.Config, error) {
	config, err := storage.ConfigFromEnvironment(lookup)
	if err != nil {
		return storage.Stores{}, storage.Config{}, err
	}
	stores, err := storage.Connect(ctx, config, opts)
	if err != nil {
		return storage.Stores{}, config, err
	}
	return stores, config, nil
}

// openRetrieval decides how a grounded turn finds evidence, before anything serves.
//
// The default is the deterministic pipeline this process has always built, and in that mode
// nothing here opens a socket: AssembleRetrieval returns the store unchanged and EnsureIndex
// is a no-op, so an unset GOPHERLEARN_RETRIEVAL_MODE cannot turn a keyless local run into a
// deployment that waits on Redis. In semantic_redis mode the whole stack is built here -
// embedder, vector index, synchronizing store, cross-encoder - and a missing or unusable
// piece stops the process by name. That is the second half of the rule that keeps a
// deployment from believing it reads learner notes with a model while it is actually ranking
// by surface-token overlap.
//
// The resolved configuration comes back with the assembly, because the episodic
// memory system derives its own view from the same mode and settings: one
// decision about how this deployment embeds, read twice, never two decisions.
//
// lookup is injected so the startup decision, including which errors stop the process, is
// testable without an embedder or a database.
func openRetrieval(ctx context.Context, store knowledge.Store, lookup func(string) string) (app.RetrievalAssembly, app.RetrievalConfig, error) {
	config, err := app.RetrievalConfigFromEnvironment(lookup)
	if err != nil {
		return app.RetrievalAssembly{}, app.RetrievalConfig{}, err
	}
	assembly, err := app.AssembleRetrieval(store, config)
	if err != nil {
		return app.RetrievalAssembly{}, config, err
	}
	if err := assembly.EnsureIndex(ctx); err != nil {
		_ = assembly.Close()
		return app.RetrievalAssembly{}, config, err
	}
	return assembly, config, nil
}

// productionStorageOptions wires the real MySQL construction path.
func productionStorageOptions() storage.Options {
	return storage.Options{
		NewMySQL: func(ctx context.Context, config storage.Config) (storage.Stores, error) {
			stores, err := gormmysql.Connect(ctx, config.MySQLDSN, gormmysql.Options{ConnectTimeout: config.ConnectTimeout})
			if err != nil {
				return storage.Stores{}, err
			}
			return storage.Stores{Repository: stores.Repository(), Knowledge: stores.Knowledge()}, nil
		},
	}
}

// storageSummary names the active backend in the startup log. The memory mode
// is easy to mistake for a working deployment, so the consequence is spelled
// out instead of only the mode.
func storageSummary(config storage.Config) string {
	if config.Mode == storage.ModeMySQL {
		return "storage=mysql"
	}
	return "storage=memory (sessions and messages are lost on restart)"
}

// memoryAccessFor is the binary's one decision about what the memory read side may reach, and it
// is a function for the same reason buildLearningService is one: this is a permission, and a
// permission that only exists inline in a startup sequence cannot be tested.
//
// The topic authorizer is installed, which grants nothing by itself: a read still needs this
// conversation to be a learning agent, the learner's own stored yes, and a live quiz naming the one
// subject. The alternative - wiring the default grant and adding topics later - is the shape that
// made "we installed the feature" and "the feature is on for everybody" hard to tell apart.
func memoryAccessFor(store repository.Repository) (*service.MemoryAccess, error) {
	return app.NewMemoryAccess(store, app.MemoryAccessConfig{TopicMemory: true})
}

// buildLearningService returns the quiz workflow for this process, or nil when nothing
// can generate questions.
//
// A real provider client comes first and is the same client the answer strategies were
// built with, so a deployment cannot end up asking one provider for answers and another
// for questions. Behind it sits only the scripted model that
// GOPHERLEARN_LOCAL_LEARNING_MODEL asks for - which the configuration refuses whenever a
// provider is set, so the two branches can never both be live and the order is a
// statement about precedence, not a guess about intent.
//
// In echo mode with no switch, both are nil and the routes answer 500
// learning_unavailable: a placeholder model cannot write questions worth grading, and
// endpoints that answered from one would be describing a quiz that never happened.
func buildLearningService(assembly app.Assembly, stores storage.Stores, knowledgeStore knowledge.Store,
	knowledgeService *service.KnowledgeService, metrics *modelcall.Publisher) *service.LearningService {
	chat := assembly.Model
	if chat == nil {
		chat = assembly.LearningModel
	}
	if chat == nil {
		return nil
	}
	return service.NewLearningService(stores.Repository, chat,
		app.NewRetrievalPipeline(knowledgeStore), knowledgeService, knowledgeService).WithTaskMetrics(metrics)
}

// workerStopContext was folded into main; the worker's stop wait is bounded
// where it is started.

func main() {
	// Startup and shutdown return through run()'s deferred cleanup: a fatal exit
	// would skip the defers, and a listener failure arriving after the background
	// pools started must still drain them. run() owns every resource from the
	// moment it opens one; main only turns a returned error into the exit code.
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() (err error) {
	// The stop signal is caught before anything is opened, so a SIGTERM that arrives during
	// startup reaches the same drain path as one that arrives later.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	secret := strings.TrimSpace(os.Getenv("GOPHERLEARN_JWT_SECRET"))
	if secret == "" {
		return errors.New("GOPHERLEARN_JWT_SECRET must be set")
	}
	issuer := strings.TrimSpace(os.Getenv("GOPHERLEARN_JWT_ISSUER"))
	if issuer == "" {
		issuer = "gopherlearn"
	}
	port := strings.TrimSpace(os.Getenv("GOPHERLEARN_HTTP_ADDR"))
	if port == "" {
		port = ":8080"
	} else if !strings.Contains(port, ":") {
		port = ":" + port
	}

	tokens, err := security.NewTokenManager(secret, issuer)
	if err != nil {
		return errors.New("invalid JWT configuration")
	}
	aiConfig, err := app.ConfigFromEnvironment(os.Getenv)
	aiConfig.MemoryReadMode = app.MemoryReadLegacyV2
	if err != nil {
		// The model configuration is validated before the key is used anywhere,
		// and only its host/model/limits may be printed.
		return fmt.Errorf("assistant startup failed: %v", aiConfig.Model.Sanitize(err))
	}
	stores, storageConfig, err := openStorage(ctx, os.Getenv, productionStorageOptions())
	if err != nil {
		return fmt.Errorf("storage startup failed: %v", storageConfig.Sanitize(err))
	}
	// Long-term memory has two doors and they are deliberately not the same one. The agent path
	// reads through a grant it decides for itself - a learner's global facts, this session's own
	// episodes, and a conversation's topic memory only where that conversation is a learning agent,
	// the learner switched it on, and a live quiz names the subject - and may write only a pending
	// draft whose owner, session and source it was never told: those come from the turn. The HTTP
	// path is where a draft becomes a fact and where the switch is set, and only a user can pull
	// either trigger. Installing the topic authorizer here grants nothing by itself for that reason:
	// the consent row and the open quiz are read fresh on every recall, so no deployment setting can
	// outrun a learner's answer or outlive their quiz.
	options := app.ProductionOptions()
	memoryAccess, err := memoryAccessFor(stores.Repository)
	if err != nil {
		return fmt.Errorf("memory startup failed: %w", err)
	}
	memoryContextSupply, err := app.NewMemoryContextSupply(stores.Repository, app.MemoryAccessConfig{TopicMemory: true})
	if err != nil {
		return fmt.Errorf("memory context startup failed: %w", err)
	}
	log.Printf("long-term memory read side: topic_memory=installed (grants only a learning agent " +
		"conversation that the learner switched on, for the subject its open quiz names)")
	memoryService := service.NewMemoryService(stores.Repository)
	options.Memory = memoryAccess
	options.MemoryProposer = memoryService
	// Retrieval is decided before the assistant strategies are assembled, and the store the
	// rest of the process uses is the one that assembly returned: in semantic mode it writes
	// a vector beside every chunk, so a knowledge upload that bypassed it would store text
	// nothing can recall. In the default mode it is the same store object, unchanged.
	retrievalStack, retrievalConfig, err := openRetrieval(ctx, stores.Knowledge, os.Getenv)
	if err != nil {
		return fmt.Errorf("retrieval startup failed: %w", err)
	}
	defer func() { _ = retrievalStack.Close() }()
	options.Retrieval = retrievalStack.Retriever
	knowledgeStore := retrievalStack.Store
	// Episodic memory is opt-in and it needs a real tool-calling provider: a
	// placeholder that cannot answer a conversation cannot project one either,
	// so asking for the feature behind the echo provider stops startup rather
	// than running a worker that only ever produces failures. Its derived view
	// follows the retrieval mode the same assembly just decided, so the two
	// retrieval stacks can never disagree about how this deployment embeds.
	episodeSetup, episodesEnabled, err := app.NewEpisodeSetup(stores.Repository, retrievalConfig, os.Getenv)
	if err != nil {
		return fmt.Errorf("episodic memory startup failed: %w", err)
	}
	var episodeStack *app.EpisodeStack
	if episodesEnabled {
		if aiConfig.Provider != app.ProviderOpenAI {
			return fmt.Errorf("episodic memory startup failed: %s=on needs %s=%s (a placeholder model cannot project conversations)",
				service.EnvEpisodes, app.EnvProvider, string(app.ProviderOpenAI))
		}
		var installedEpisodeTools app.EpisodeTools
		var episodeToolsInstalled bool
		installEpisodes := func(chat model.ToolChatModel) (app.EpisodeTools, error) {
			if episodeToolsInstalled {
				return installedEpisodeTools, nil
			}
			tools, installErr := episodeSetup.Install(chat)
			if installErr == nil {
				installedEpisodeTools, episodeToolsInstalled = tools, true
			}
			return tools, installErr
		}
		options.Episodes = installEpisodes
		options.HistoryReader = func(chat model.ToolChatModel) (agent.EpisodeHistoryReader, error) {
			tools, installErr := installEpisodes(chat)
			if installErr != nil {
				return nil, installErr
			}
			return tools.HistoryReader, nil
		}
	}
	assembly, err := app.Assemble(knowledgeStore, aiConfig, options)
	if err != nil {
		return fmt.Errorf("assistant startup failed: %v", aiConfig.Model.Sanitize(err))
	}
	if episodesEnabled {
		stack, err := episodeSetup.Stack()
		if err != nil {
			return fmt.Errorf("episodic memory startup failed: %w", err)
		}
		episodeStack = stack
		// The process-local view starts empty, so a deterministic deployment
		// re-reads the authority at startup instead of serving recall from a
		// view that looks healthy and knows nothing. A Redis-backed view keeps
		// its content across restarts and has no warm start here.
		if episodeStack.WarmStart != nil {
			warmed, err := episodeStack.WarmStart(ctx)
			if err != nil {
				return fmt.Errorf("episodic memory startup failed: warm start: %w", err)
			}
			log.Printf("episodic memory view warmed from store: %d card(s)", warmed)
		}
		log.Printf("%s", episodeStack.StartupLine)
	}
	assistant, assistantSummary := assembly.Assistant, assembly.StartupLine
	knowledgeService := service.NewKnowledgeService(knowledgeStore)
	// The knowledge backend is the driver half of ingestion: the service publishes
	// the bytes and stages the row pending, and these pools are what turn a pending
	// version into the one that answers. A deployment that stores sources must run
	// them - the service never waits for embedding - or every upload would stay
	// pending forever. The index side follows the retrieval mode: the Redis
	// synchronizer's driver embeds and sweeps vector rows; any other store is its
	// own index, so the store-backed runner publishes staged chunks and the store's
	// own cleanup removes retired ones.
	backendRunner, backendCleaner, err := app.KnowledgeIndexSide(knowledgeStore)
	if err != nil {
		return fmt.Errorf("knowledge backend startup failed: %w", err)
	}
	// The configuration comes from the one entry the tests drive too: an unset
	// source directory is v2's default (data/knowledge), which keeps original
	// bytes on and the ingest asynchronous - there is no opt-out through it.
	backendConfig := app.KnowledgeBackendConfigFromEnvironment(os.Getenv)
	knowledgeBackend, err := app.AssembleKnowledgeBackend(knowledgeService, knowledgeStore,
		backendRunner, backendCleaner, backendConfig)
	if err != nil {
		return fmt.Errorf("knowledge backend startup failed: %w", err)
	}
	// One publisher for the whole process, but no shared accumulation: every
	// operation takes its own collector inside it, so two concurrent turns cannot
	// add into each other's totals.
	metrics := modelcall.NewPublisher(aiConfig.TaskMetrics == app.TaskMetricsLog, modelcall.NewJSONSink(os.Stderr))
	learningService := buildLearningService(assembly, stores, knowledgeStore, knowledgeService, metrics)
	auth := service.NewAuthService(stores.Repository, tokens, time.Now)
	sessions := service.NewSessionService(stores.Repository)
	// The summary wiring comes from the same assembly that chose the strategies, so a
	// deployment cannot end up with a conversation service and an answer service that
	// disagree about what the session remembers.
	var messageOptions []service.MessageServiceOption
	messageOptions = append(messageOptions, service.WithTaskMetrics(metrics))
	if metrics.Enabled() {
		// Said out loud because a log line nobody announced looks like a leak.
		log.Printf("task metering: %s (one JSON line per user operation on stderr; no prompt, answer, tool argument or credential is written)", app.TaskMetricsLog)
	}
	if learningService != nil {
		// The agent turn is the only reader of this: it is told, in prose, how well the
		// learner has recently done, so the next explanation starts at the right depth.
		messageOptions = append(messageOptions, service.WithMasteryContext(learningService))
	}
	messageOptions = append(messageOptions, service.WithLearningContextSupply(memoryContextSupply, nil))
	var episodeDriver app.EpisodeDriver
	if episodeStack != nil {
		messageOptions = append(messageOptions, service.WithLearningContextSupply(memoryContextSupply, episodeStack.System))
		// The trigger fires after the assistant row is stored, on the turn's own
		// path; the worker runs in the background and the delete hook drops a
		// session's derived memory when the session goes.
		messageOptions = append(messageOptions, service.WithEpisodePublisher(episodeStack.Publisher))
		sessions.UseEpisodeDropper(episodeStack.System)
		episodeDriver = episodeStack.System.Worker()
	}
	// One startup window for the background drivers: the pools start first, the
	// worker second, and a failure in the second step has already reclaimed the
	// pools before the error reaches this return - the cleanup never rides on a
	// deferred stop that a fatal exit would skip.
	if err := app.StartBackgroundDrivers(ctx, knowledgeBackend, episodeDriver); err != nil {
		return fmt.Errorf("background drivers startup failed: %w", err)
	}
	log.Printf("%s", knowledgeBackend.StartupLine())
	// Both drivers are running, so their reclamation responsibility transfers to
	// these deferred stops: they execute on every return from here, including a
	// listener failure. The stop contexts are built at exit time - a startup-time
	// deadline would already be expired on the first real shutdown of a process
	// that lived longer than the window. Registered in this order so the LIFO
	// drain runs the pools down first, then the episodic worker, then the
	// retrieval stack's own close below.
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), workerStopGrace)
		defer cancel()
		if err := knowledgeBackend.Stop(stopCtx); err != nil {
			log.Printf("knowledge backend stop did not finish cleanly: %v", err)
		}
	}()
	if worker, ok := episodeDriver.(*service.EpisodeWorker); ok {
		defer func() {
			if err := stopEpisodeWorker(worker, workerStopGrace); err != nil {
				log.Printf("episodic memory worker stop did not finish in %v: %v", workerStopGrace, err)
			}
		}()
	}
	messages := app.NewMessageService(stores.Repository, assistant, knowledgeService, aiConfig.Context,
		append(assembly.SummaryOptions(), messageOptions...)...)
	serverOptions := []httpapi.Option{httpapi.WithKnowledge(knowledgeService), httpapi.WithMemory(memoryService)}
	if learningService != nil {
		serverOptions = append(serverOptions, httpapi.WithLearning(learningService))
	}
	server := newHTTPServer(port, httpapi.NewServer(auth, sessions, messages, tokens, serverOptions...).Handler())

	log.Printf("GopherLearn backend listening on %s [%s] [%s] [context messages=%d runes=%d]",
		port, storageSummary(storageConfig), assistantSummary, aiConfig.Context.MaxMessages, aiConfig.Context.MaxRunes)
	// Its own line, because "which retrieval stack grounded that answer" is the first thing
	// anyone reading a citation complaint needs, and the assembly - not this file - wrote the
	// text: mode, embedder model, index name and host, reranker model, cosine cut and the
	// vector identity. No key, no DSN, no path.
	log.Printf("%s", retrievalStack.StartupLine)
	if err := serveUntil(ctx, server, shutdownGrace, server.ListenAndServe); err != nil {
		if errors.Is(err, errShutdownForced) {
			// The stop still succeeded; only some work was cut short. Exiting non-zero here
			// would tell a supervisor to keep restarting a process that was asked to leave.
			log.Printf("stopped with the drain window exhausted: %v", err)
			return nil
		}
		// A listener failure is a startup failure with the pools already running:
		// it returns through the deferred cleanup instead of skipping it.
		return err
	}
	log.Printf("GopherLearn backend stopped")
	return nil
}
