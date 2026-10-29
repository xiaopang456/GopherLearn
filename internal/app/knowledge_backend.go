package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/knowledge"
	"github.com/xiaopang456/GopherLearn/internal/repository/redisindex"
	"github.com/xiaopang456/GopherLearn/internal/service"
)

// The knowledge backend assembly: the half of the process that turns a staged
// document version into the version that answers. The lifecycle, the pools and
// the source storage are C's migrated components; this file is the one place
// that starts them, so a binary cannot end up with a service that stages
// pending versions and no driver that ever publishes them.
//
// v2's rule this assembly exists to enforce: a deployment that keeps original
// bytes must run the pools. The service's ingest ends at "the version exists,
// its source is readable, and the row is pending" - it never waits for
// embedding - so without a driver the pending queue would grow forever and
// every upload would be invisible to retrieval.

// Knowledge backend defaults, per the frozen C/V2 production contract. The task
// timeout is deliberately non-zero: a pool without one inherits whatever context
// its caller handed it, which is how an indexing pass silently becomes unbounded.
const (
	DefaultKnowledgeWorkers      = 2
	DefaultKnowledgeQueueSize    = 100
	DefaultKnowledgeScanInterval = 5 * time.Second
	DefaultKnowledgeTaskTimeout  = 300 * time.Second
	// DefaultKnowledgeSourceDir is v2's own storage default. The production
	// configuration keeps original bytes by default - source storage and the
	// asynchronous pending path are not opt-in features - and an unset or empty
	// variable resolves here, never to "off".
	DefaultKnowledgeSourceDir = "data/knowledge"
	// EnvKnowledgeSourceDir is the one variable the backend configuration reads.
	// Setting it names the directory; there is deliberately no value that turns
	// source storage off through configuration.
	EnvKnowledgeSourceDir = "GOPHERLEARN_KNOWLEDGE_SOURCE_DIR"
)

// partialStartStopGrace bounds the reclamation of an already-started pool when a
// later startup step fails. It matches the requeue write-off bound the pools
// themselves use: long enough for a bounded stop, short enough that a wedged
// datastore cannot hold a failing startup hostage.
const partialStartStopGrace = 5 * time.Second

// KnowledgeBackendConfigFromEnvironment is the one configuration entry the binary
// and the tests share. An unset or empty source directory is v2's default - the
// deployment keeps original bytes and the ingest goes asynchronous - and it is
// resolved against the process's working directory, exactly like v2's own
// relative storage_dir. There is no configuration value that disables source
// storage: an opt-out is an assembly decision this entry refuses to grow.
func KnowledgeBackendConfigFromEnvironment(lookup func(string) string) KnowledgeBackendConfig {
	if lookup == nil {
		lookup = func(string) string { return "" }
	}
	config := KnowledgeBackendConfig{SourceDir: DefaultKnowledgeSourceDir}
	if dir := strings.TrimSpace(lookup(EnvKnowledgeSourceDir)); dir != "" {
		config.SourceDir = dir
	}
	return config
}

// KnowledgeBackendConfig bounds the backend. The zero SourceDir means the
// deployment keeps no original bytes: the service then behaves exactly as it did
// (the store's own inline composition publishes versions), and the pools still
// run - their queues simply stay empty, because that path never stages pending
// work for a driver.
type KnowledgeBackendConfig struct {
	// SourceDir roots the immutable per-version source files. Empty disables
	// source storage and read-back.
	SourceDir string
	// SourceMaxBytes caps one stored source. Zero keeps the store's own default.
	SourceMaxBytes int64
	// Workers, QueueSize, ScanInterval and TaskTimeout default to the frozen
	// production figures when zero; a negative value is a configuration fault
	// and stops assembly rather than being silently corrected.
	Workers      int
	QueueSize    int
	ScanInterval time.Duration
	TaskTimeout  time.Duration
}

// normalize resolves the defaults. It reports the first negative field instead of
// clamping: an operator's typo must stop startup, not become a quiet number.
func (c KnowledgeBackendConfig) normalize() (knowledge.WorkerConfig, int64, error) {
	if c.Workers < 0 || c.QueueSize < 0 || c.ScanInterval < 0 || c.TaskTimeout < 0 {
		return knowledge.WorkerConfig{}, 0, errors.New("app: knowledge backend limits must not be negative")
	}
	workers, queue, scan, timeout := c.Workers, c.QueueSize, c.ScanInterval, c.TaskTimeout
	if workers == 0 {
		workers = DefaultKnowledgeWorkers
	}
	if queue == 0 {
		queue = DefaultKnowledgeQueueSize
	}
	if scan == 0 {
		scan = DefaultKnowledgeScanInterval
	}
	if timeout == 0 {
		timeout = DefaultKnowledgeTaskTimeout
	}
	return knowledge.WorkerConfig{
		Workers: workers, QueueSize: queue, ScanInterval: scan, TaskTimeout: timeout,
	}, c.SourceMaxBytes, nil
}

// KnowledgeBackend is the started state of the knowledge driver: the pools over
// one lifecycle store, plus the source storage the service publishes into. It
// owns no model call and no credential - the embedder, when the mode has one,
// lives behind the IndexRunner the caller supplied.
type KnowledgeBackend struct {
	sources     knowledge.SourceStorage
	indexPool   *knowledge.IndexPool
	cleanupPool *knowledge.CleanupPool
	startupLine string
}

// StartupLine names what the process actually started, for the startup log.
func (b *KnowledgeBackend) StartupLine() string { return b.startupLine }

// AssembleKnowledgeBackend wires the pools over a lifecycle store and turns the
// service's source path on when source storage was configured.
//
// runner and cleaner are the mode's index side: the semantic deployment passes
// the Redis driver (it embeds staged chunks and sweeps vector rows), the
// deterministic deployment passes the store-backed pair below, whose "index" is
// the chunk set the row already carries and whose cleanup needs no rows outside
// the store. Both are real adapters over the mode's own storage, not stubs:
// a deterministic IndexVersion that answered 0 without reading the staged
// chunks would publish versions nobody chunked.
func AssembleKnowledgeBackend(knowledgeService *service.KnowledgeService, store knowledge.Store,
	runner knowledge.IndexRunner, cleaner knowledge.RowCleaner, config KnowledgeBackendConfig,
) (*KnowledgeBackend, error) {
	if knowledgeService == nil || store == nil {
		return nil, errors.New("app: the knowledge backend needs the service and its store")
	}
	lifecycle, ok := store.(knowledge.Lifecycle)
	if !ok {
		// The service would refuse the first ingest with the same answer, but the
		// right place to find out is startup: a store that cannot record index
		// state cannot be driven, and half a backend is worse than none.
		return nil, errors.New("app: the knowledge store does not expose the index lifecycle")
	}
	workerConfig, sourceMaxBytes, err := config.normalize()
	if err != nil {
		return nil, err
	}
	backend := &KnowledgeBackend{}
	if config.SourceDir != "" {
		sources, err := knowledge.NewLocalSourceStorage(config.SourceDir, sourceMaxBytes)
		if err != nil {
			return nil, fmt.Errorf("app: knowledge source storage: %w", err)
		}
		// WithSourceStorage is what moves the service's ingest onto the lifecycle
		// path: publish bytes, stage pending, return. It also wires the read-back
		// reader over the same store, so what the pool publishes is what the
		// reader will vouch for.
		knowledgeService.WithSourceStorage(sources)
		backend.sources = sources
	}
	indexPool, err := knowledge.NewIndexPool(lifecycle, runner, workerConfig)
	if err != nil {
		return nil, fmt.Errorf("app: knowledge index pool: %w", err)
	}
	cleanupPool, err := knowledge.NewCleanupPool(lifecycle, cleaner, backend.sources, workerConfig)
	if err != nil {
		return nil, fmt.Errorf("app: knowledge cleanup pool: %w", err)
	}
	backend.indexPool = indexPool
	backend.cleanupPool = cleanupPool
	backend.startupLine = "knowledge=pools workers=" + itoa(workerConfig.Workers) +
		" queue=" + itoa(workerConfig.QueueSize) +
		" scan=" + workerConfig.ScanInterval.String() +
		" task_timeout=" + workerConfig.TaskTimeout.String() +
		" sources=" + sourceMode(config.SourceDir)
	return backend, nil
}

// Start runs the backend: index recovery first, then the pools. A recovery
// failure stops the start - a process that cannot tell which passes were left
// mid-flight must not present itself as one that will finish them - and because
// the failure happens in the pool's own prepare step, the pool is left
// unstarted, with no worker or scanner running. A cleanup pool that fails to
// start reclaims the already-started index pool on a bounded, freshly built
// context, so a partial start never leaves one pool running unowned.
func (b *KnowledgeBackend) Start(ctx context.Context) error {
	if err := b.indexPool.Start(ctx); err != nil {
		return fmt.Errorf("app: knowledge index pool start: %w", err)
	}
	if err := b.cleanupPool.Start(ctx); err != nil {
		// Nothing was served yet, so a bounded stop with a fresh context is safe:
		// the index pool must not outlive a start that failed.
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), partialStartStopGrace)
		defer cancel()
		_ = b.indexPool.Stop(stopCtx)
		return fmt.Errorf("app: knowledge cleanup pool start: %w", err)
	}
	return nil
}

// Stop drains the pools in reverse start order: cleanup first, so an in-flight
// cleanup is not racing an index pass that is publishing new cleanup work, then
// indexing. The caller bounds the wait; unfinished work stays recorded in the
// lifecycle for the next start's recovery, and nothing in flight is marked
// successful on the way out.
func (b *KnowledgeBackend) Stop(ctx context.Context) error {
	cleanupErr := b.cleanupPool.Stop(ctx)
	indexErr := b.indexPool.Stop(ctx)
	return errors.Join(cleanupErr, indexErr)
}

// StoreIndexRunner is the deterministic mode's index side. A deployment without
// an external vector index has no derived rows to build: the chunks staged with
// the row are the index, and retrieval filters them by the serving version. The
// runner therefore reads exactly the claimed version's staged chunks - refusing
// a version that was never stored - and reports its size for the outcome record.
type StoreIndexRunner struct {
	Lifecycle knowledge.Lifecycle
}

// IndexVersion implements knowledge.IndexRunner.
func (r StoreIndexRunner) IndexVersion(ctx context.Context, userID, documentID string, version int) (int, error) {
	chunks, err := r.Lifecycle.StagedChunks(ctx, userID, documentID, version)
	if err != nil {
		return 0, err
	}
	return len(chunks), nil
}

// NoExternalRows is the deterministic mode's row cleaner. Every row the store
// keeps is removed by the store's own CompleteCleanup - retired chunks and
// deleted documents alike - so the pool's row step has nothing outside the store
// to delete. It exists so the cleanup pool can run the same state machine in
// both modes without pretending an external index was swept.
type NoExternalRows struct{}

// SweepExcept implements knowledge.RowCleaner.
func (NoExternalRows) SweepExcept(context.Context, string, string, int) error { return nil }

// DropDocument implements knowledge.RowCleaner.
func (NoExternalRows) DropDocument(context.Context, string, string) error { return nil }

// KnowledgeIndexSide picks the mode's index runner and row cleaner off the store
// the retrieval stack built. The Redis synchronizer drives both from its driver;
// every other store is driven from its own lifecycle.
func KnowledgeIndexSide(store knowledge.Store) (knowledge.IndexRunner, knowledge.RowCleaner, error) {
	if synchronizer, ok := store.(*redisindex.Synchronizer); ok {
		driver := synchronizer.Driver()
		return driver, driver, nil
	}
	lifecycle, ok := store.(knowledge.Lifecycle)
	if !ok {
		return nil, nil, errors.New("app: the knowledge store does not expose the index lifecycle")
	}
	return StoreIndexRunner{Lifecycle: lifecycle}, NoExternalRows{}, nil
}

func sourceMode(sourceDir string) string {
	if sourceDir == "" {
		return "off"
	}
	return "on"
}

func itoa(value int) string { return fmt.Sprintf("%d", value) }

// EpisodeDriver is the startup-window seam of a second background driver - the
// episodic worker - that shares the process's startup and shutdown with the
// knowledge pools. Both halves are the ones the binary actually calls.
type EpisodeDriver interface {
	Start(context.Context) error
	Stop(context.Context) error
}

// StartBackgroundDrivers starts the knowledge pools first, then any episodic
// worker, and owns reclaiming the pools when a later step in that window fails:
// the returned error has already drained every driver that managed to start, on
// a bounded context built for the reclamation - never on the caller's start
// context, and never by relying on the caller to run a deferred stop it may
// skip with a fatal exit. On success, ownership of both drivers transfers to
// the caller's shutdown path.
func StartBackgroundDrivers(ctx context.Context, backend *KnowledgeBackend, episodes EpisodeDriver) error {
	if backend == nil {
		return errors.New("app: a knowledge backend is required")
	}
	if err := backend.Start(ctx); err != nil {
		return err
	}
	if episodes == nil {
		return nil
	}
	if err := episodes.Start(ctx); err != nil {
		// The pools are running, and the caller is about to receive an error:
		// reclaim them here, on the return path, so the failure cannot leave
		// background work behind it.
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), partialStartStopGrace)
		defer cancel()
		if stopErr := backend.Stop(stopCtx); stopErr != nil {
			return errors.Join(fmt.Errorf("app: episodic worker start: %w", err), stopErr)
		}
		return fmt.Errorf("app: episodic worker start: %w", err)
	}
	return nil
}
