package app

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/knowledge"
	"github.com/xiaopang456/GopherLearn/internal/repository"
	"github.com/xiaopang456/GopherLearn/internal/service"
)

// The knowledge backend's production regressions. Every test drives the real
// chain the binary runs - KnowledgeService with source storage, the lifecycle
// staging path, and the pools over the same store - on the in-memory store and a
// temporary source directory. No provider, no Redis, no database.

// gatedRunner is a real IndexRunner whose passes wait for the test's signal, so a
// regression can observe what the process does while indexing is blocked.
type gatedRunner struct {
	mu    sync.Mutex
	open  bool
	wait  chan struct{}
	fail  bool
	calls int
}

func newGatedRunner() *gatedRunner { return &gatedRunner{wait: make(chan struct{})} }

func (r *gatedRunner) release() {
	r.mu.Lock()
	r.open = true
	r.mu.Unlock()
	close(r.wait)
}

func (r *gatedRunner) setFail(fail bool) {
	r.mu.Lock()
	r.fail = fail
	r.mu.Unlock()
}

func (r *gatedRunner) IndexVersion(ctx context.Context, userID, documentID string, version int) (int, error) {
	r.mu.Lock()
	r.calls++
	fail := r.fail
	r.mu.Unlock()
	select {
	case <-r.wait:
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	if fail {
		return 0, errors.New("gated runner: indexing refused")
	}
	return 1, nil
}

func (r *gatedRunner) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// flakyCleaner fails the row step on its first claim and lets the retry through -
// the shape a transient removal failure has, and the one the cleanup retry
// schedule exists for.
type flakyCleaner struct {
	mu       sync.Mutex
	failures int
	sweeps   int
}

func (c *flakyCleaner) SweepExcept(context.Context, string, string, int) error {
	return c.remove()
}

func (c *flakyCleaner) DropDocument(context.Context, string, string) error {
	return c.remove()
}

func (c *flakyCleaner) remove() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweeps++
	if c.failures > 0 {
		c.failures--
		return errors.New("flaky cleaner: rows busy")
	}
	return nil
}

type backendFixture struct {
	service    *service.KnowledgeService
	backend    *KnowledgeBackend
	store      *knowledge.MemoryStore
	sources    knowledge.SourceStorage
	collection string
	userID     string
	started    bool
}

// newBackendFixture assembles the backend without starting it, so a test can
// arrange pre-start store state (the interrupted-pass case) first. runnerFn is
// handed the fixture's own store, which is how the store-backed runner under test
// gets bound to the same lifecycle the pool drives.
func newBackendFixture(t *testing.T, runnerFn func(*knowledge.MemoryStore) knowledge.IndexRunner,
	cleaner knowledge.RowCleaner) *backendFixture {
	t.Helper()
	store := knowledge.NewMemoryStore()
	ingest := service.NewKnowledgeService(store)
	ctx := context.Background()
	repo := repository.NewInMemoryRepository()
	user, err := repo.CreateUser(ctx, &domain.User{Email: "backend@example.com", DisplayName: "Backend"})
	if err != nil {
		t.Fatal(err)
	}
	collection, err := ingest.CreateCollection(ctx, user.ID, "学习笔记")
	if err != nil {
		t.Fatal(err)
	}
	backend, err := AssembleKnowledgeBackend(ingest, store, runnerFn(store), cleaner, KnowledgeBackendConfig{
		SourceDir:    t.TempDir(),
		Workers:      1,
		QueueSize:    4,
		ScanInterval: 5 * time.Millisecond,
		TaskTimeout:  10 * time.Second,
	})
	if err != nil {
		t.Fatalf("assemble knowledge backend: %v", err)
	}
	return &backendFixture{
		service: ingest, backend: backend, store: store,
		collection: collection.ID, userID: user.ID,
	}
}

func (f *backendFixture) start(t *testing.T) {
	t.Helper()
	if err := f.backend.Start(context.Background()); err != nil {
		t.Fatalf("start knowledge backend: %v", err)
	}
	f.started = true
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = f.backend.Stop(stopCtx)
	})
}

func (f *backendFixture) upload(t *testing.T, title, content string) *domain.Document {
	t.Helper()
	document, err := f.service.AddDocument(context.Background(), f.userID, f.collection, service.DocumentInput{
		Title: title, Format: string(domain.DocumentFormatMarkdown), Content: content,
	})
	if err != nil {
		t.Fatalf("upload %s: %v", title, err)
	}
	return document
}

func waitFor(t *testing.T, what string, deadline time.Duration, done func() bool) {
	t.Helper()
	limit := time.Now().Add(deadline)
	for time.Now().Before(limit) {
		if done() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (f *backendFixture) workState(t *testing.T, documentID string) *service.DocumentWorkState {
	t.Helper()
	state, err := f.service.DocumentWorkState(context.Background(), f.userID, documentID)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

// TestKnowledgeBackendDefaultsMatchTheFrozenContract pins the production figures
// the startup line reports, so a silent edit to the defaults is a diff someone reads.
func TestKnowledgeBackendDefaultsMatchTheFrozenContract(t *testing.T) {
	workerConfig, _, err := KnowledgeBackendConfig{}.normalize()
	if err != nil {
		t.Fatal(err)
	}
	if workerConfig.Workers != DefaultKnowledgeWorkers || workerConfig.QueueSize != DefaultKnowledgeQueueSize ||
		workerConfig.ScanInterval != DefaultKnowledgeScanInterval || workerConfig.TaskTimeout != DefaultKnowledgeTaskTimeout {
		t.Fatalf("backend defaults drifted: %+v", workerConfig)
	}
	if DefaultKnowledgeWorkers != 2 || DefaultKnowledgeQueueSize != 100 ||
		DefaultKnowledgeScanInterval != 5*time.Second || DefaultKnowledgeTaskTimeout != 300*time.Second {
		t.Fatalf("frozen contract figures changed: %d/%d/%s/%s", DefaultKnowledgeWorkers, DefaultKnowledgeQueueSize,
			DefaultKnowledgeScanInterval, DefaultKnowledgeTaskTimeout)
	}
	if _, _, err := (KnowledgeBackendConfig{Workers: -1}).normalize(); err == nil {
		t.Fatal("a negative worker count was accepted")
	}
}

// TestKnowledgeBackendUploadDoesNotWaitForABlockedIndex pins the v2 return
// semantics: the ingest ends when the bytes are published and the row is pending,
// and an indexing pass that never finishes cannot hold the upload hostage.
func TestKnowledgeBackendUploadDoesNotWaitForABlockedIndex(t *testing.T) {
	runner := newGatedRunner()
	fixture := newBackendFixture(t, func(*knowledge.MemoryStore) knowledge.IndexRunner { return runner }, NoExternalRows{})
	fixture.start(t)
	started := time.Now()
	document := fixture.upload(t, "aof.md", "# AOF 持久化\n\nfsync 可以选 always、everysec 或 no。\n")
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("the upload waited %v for a blocked index", elapsed)
	}
	if state := fixture.workState(t, document.ID); state.IndexStatus != domain.DocumentStatusPending || state.Retrievable {
		t.Fatalf("a staged upload reported %s retrievable=%v, want pending and not retrievable",
			state.IndexStatus, state.Retrievable)
	}
	waitFor(t, "the pool to pick up the pending version", 5*time.Second, func() bool {
		return runner.callCount() > 0
	})
	runner.release()
	waitFor(t, "the staged version to publish", 5*time.Second, func() bool {
		state := fixture.workState(t, document.ID)
		return state.IndexStatus == domain.DocumentStatusReady && state.Retrievable
	})
}

// TestKnowledgeBackendFailureKeepsSourceAndRetryAdvances pins the failure split:
// a background index failure keeps the submitted bytes and the row, and the retry
// API - not a re-upload - is what advances it.
func TestKnowledgeBackendFailureKeepsSourceAndRetryAdvances(t *testing.T) {
	runner := newGatedRunner()
	runner.setFail(true)
	fixture := newBackendFixture(t, func(*knowledge.MemoryStore) knowledge.IndexRunner { return runner }, NoExternalRows{})
	fixture.start(t)
	document := fixture.upload(t, "rdb.md", "# RDB\n\n快照是某时刻的全量。\n")
	runner.release()
	waitFor(t, "the pass to fail", 5*time.Second, func() bool {
		return fixture.workState(t, document.ID).IndexStatus == domain.DocumentStatusFailed
	})
	// The submitted bytes survive a failed pass: the storage still holds this
	// attempt's source, and the row still names the version it belongs to.
	if _, err := fixture.backend.sources.ReadVersion(context.Background(), fixture.userID, document.ID, document.Version); err != nil {
		t.Fatalf("a failed pass lost the submitted source: %v", err)
	}
	runner.setFail(false)
	if _, err := fixture.service.RetryDocument(context.Background(), fixture.userID, document.ID); err != nil {
		t.Fatalf("retry: %v", err)
	}
	waitFor(t, "the retried pass to publish", 5*time.Second, func() bool {
		state := fixture.workState(t, document.ID)
		return state.IndexStatus == domain.DocumentStatusReady && state.Retrievable
	})
}

// TestKnowledgeBackendFailedUpdateKeepsOldVersionAnswering pins the property a
// rewrite must never lose: until the new version publishes, the old one keeps
// answering, and retrieval serves exactly its text.
func TestKnowledgeBackendFailedUpdateKeepsOldVersionAnswering(t *testing.T) {
	runner := newGatedRunner()
	fixture := newBackendFixture(t, func(*knowledge.MemoryStore) knowledge.IndexRunner { return runner }, NoExternalRows{})
	fixture.start(t)
	document := fixture.upload(t, "缓存.md", "# 缓存\n\n穿透是查不到。\n")
	runner.release()
	waitFor(t, "the first version to publish", 5*time.Second, func() bool {
		return fixture.workState(t, document.ID).IndexStatus == domain.DocumentStatusReady
	})
	runner.setFail(true)
	if _, err := fixture.service.ReplaceDocumentContent(context.Background(), fixture.userID, document.ID, service.DocumentInput{
		Title: "缓存.md", Format: string(domain.DocumentFormatMarkdown), Content: "# 缓存\n\n击穿是热键失效。\n",
	}); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	waitFor(t, "the rewrite to fail", 5*time.Second, func() bool {
		return fixture.workState(t, document.ID).IndexStatus == domain.DocumentStatusFailed
	})
	chunks, err := fixture.store.ChunksFor(context.Background(), fixture.userID, []string{fixture.collection})
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) == 0 {
		t.Fatal("the old version stopped answering while its replacement was failed")
	}
	for _, chunk := range chunks {
		if !strings.Contains(chunk.Text, "穿透") {
			t.Fatalf("retrieval served the failed version's text: %q", chunk.Text)
		}
	}
	runner.setFail(false)
	if _, err := fixture.service.RetryDocument(context.Background(), fixture.userID, document.ID); err != nil {
		t.Fatalf("retry: %v", err)
	}
	waitFor(t, "the rewrite to publish", 5*time.Second, func() bool {
		state := fixture.workState(t, document.ID)
		return state.IndexStatus == domain.DocumentStatusReady && state.ActiveVersion == 2
	})
}

// TestKnowledgeBackendDeleteCleanupFailureKeepsTask pins the removal contract: a
// delete marks the document deleting and returns, and a cleanup step that fails
// stays recorded with its backoff until the pool completes it - the bytes and the
// rows go only when the work actually finished.
func TestKnowledgeBackendDeleteCleanupFailureKeepsTask(t *testing.T) {
	runner := newGatedRunner()
	runner.release()
	cleaner := &flakyCleaner{failures: 1}
	fixture := newBackendFixture(t, func(*knowledge.MemoryStore) knowledge.IndexRunner { return runner }, cleaner)
	fixture.start(t)
	document := fixture.upload(t, "主键.md", "# 主键\n\n自增和雪花都是主键方案。\n")
	waitFor(t, "the version to publish", 5*time.Second, func() bool {
		return fixture.workState(t, document.ID).IndexStatus == domain.DocumentStatusReady
	})
	if err := fixture.service.DeleteDocument(context.Background(), fixture.userID, document.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// The first cleanup attempt fails at the row step; the recorded work survives
	// with its backoff and the pool's retry completes the removal. A deleting row
	// is already invisible to reads, so the cleaner's own attempts are the progress
	// signal - the removal is finished when the retry has gone through.
	waitFor(t, "the failed cleanup to be retried", 10*time.Second, func() bool {
		cleaner.mu.Lock()
		defer cleaner.mu.Unlock()
		return cleaner.sweeps >= 2
	})
	if _, err := fixture.service.GetDocument(context.Background(), fixture.userID, document.ID); err == nil {
		t.Fatal("the document outlived a completed cleanup")
	}
}

// TestKnowledgeBackendPublishesAStagedVersionWithoutAnExternalIndex pins the
// deterministic mode's driver: with no vector index, the store-backed runner and
// the no-external-rows cleaner still drive the same pools, so a staged version
// publishes and pending never sits forever.
func TestKnowledgeBackendPublishesAStagedVersionWithoutAnExternalIndex(t *testing.T) {
	fixture := newBackendFixture(t,
		func(store *knowledge.MemoryStore) knowledge.IndexRunner { return StoreIndexRunner{Lifecycle: store} },
		NoExternalRows{})
	fixture.start(t)
	document := fixture.upload(t, "索引.md", "# 索引\n\nB+ 树是常见选择。\n")
	waitFor(t, "the store-backed pass to publish", 5*time.Second, func() bool {
		state := fixture.workState(t, document.ID)
		return state.IndexStatus == domain.DocumentStatusReady && state.ChunkCount > 0
	})
	if err := fixture.service.DeleteDocument(context.Background(), fixture.userID, document.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	waitFor(t, "the store-backed cleanup to finish", 5*time.Second, func() bool {
		_, err := fixture.service.GetDocument(context.Background(), fixture.userID, document.ID)
		return err != nil
	})
}

// TestKnowledgeBackendRecoversInterruptedIndexingAndStopsBounded pins the two
// lifecycle edges around the pools: a pass left in indexing by a previous process
// is recovered into pending at start and then finished by the pool, and a stop
// bounds its wait without marking the in-flight pass successful.
func TestKnowledgeBackendRecoversInterruptedIndexingAndStopsBounded(t *testing.T) {
	// The crash: a staged version was claimed and the process died mid-pass.
	crashedRunner := newGatedRunner()
	crashed := newBackendFixture(t, func(*knowledge.MemoryStore) knowledge.IndexRunner { return crashedRunner }, NoExternalRows{})
	document := crashed.upload(t, "索引.md", "# 索引\n\nB+ 树是常见选择。\n")
	if claimed, err := crashed.store.ClaimForIndexing(context.Background(), crashed.userID, document.ID, document.Version); err != nil || !claimed {
		t.Fatalf("simulating the interrupted pass: claimed=%v err=%v", claimed, err)
	}
	// The next start recovers the pass before any goroutine runs, and the pool
	// then finishes it once the mode's index side answers.
	crashed.start(t)
	crashedRunner.release()
	waitFor(t, "the recovered pass to publish", 5*time.Second, func() bool {
		return crashed.workState(t, document.ID).IndexStatus == domain.DocumentStatusReady
	})

	// Bounded stop: a runner blocked mid-pass must not hold the shutdown, and the
	// pass it held is left recorded - pending or indexing - never reported done.
	runner := newGatedRunner()
	stopping := newBackendFixture(t, func(*knowledge.MemoryStore) knowledge.IndexRunner { return runner }, NoExternalRows{})
	stopping.start(t)
	inFlight := stopping.upload(t, "事务.md", "# 事务\n\n隔离级别有四种。\n")
	waitFor(t, "the pass to be claimed", 5*time.Second, func() bool {
		state := stopping.workState(t, inFlight.ID)
		return state.IndexStatus == domain.DocumentStatusIndexing || state.IndexStatus == domain.DocumentStatusPending && runner.callCount() > 0
	})
	stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := stopping.backend.Stop(stopCtx); err != nil {
		t.Fatalf("bounded stop: %v", err)
	}
	switch state := stopping.workState(t, inFlight.ID); state.IndexStatus {
	case domain.DocumentStatusPending, domain.DocumentStatusIndexing:
		// Recorded work a later start can recover: exactly what a stop owes.
	case domain.DocumentStatusReady:
		t.Fatal("a pass in flight at shutdown was marked successful")
	default:
		t.Fatalf("a stopped pass ended in %s", state.IndexStatus)
	}
	runner.release()
}

// compile-time guards that the deterministic adapters really satisfy the pool seams
var (
	_ knowledge.IndexRunner = StoreIndexRunner{}
	_ knowledge.RowCleaner  = NoExternalRows{}
	_ knowledge.IndexRunner = (*gatedRunner)(nil)
)

// --- Production default and startup-failure reclamation regressions ----------

// failingRecoveryStore wraps the in-memory store so the startup recovery pass
// fails at an injected fault, and so the test can see the fault fire and can
// count every queue read that happens afterwards.
type failingRecoveryStore struct {
	*knowledge.MemoryStore
	mu       sync.Mutex
	attempts int
	lists    int
}

func (s *failingRecoveryStore) RecoverInterruptedIndexing(context.Context) (int, error) {
	s.mu.Lock()
	s.attempts++
	s.mu.Unlock()
	return 0, errors.New("injected recovery failure")
}

func (s *failingRecoveryStore) ListPending(ctx context.Context, limit int) ([]knowledge.QueuedDocument, error) {
	s.mu.Lock()
	s.lists++
	s.mu.Unlock()
	return s.MemoryStore.ListPending(ctx, limit)
}

func (s *failingRecoveryStore) snapshot() (attempts, lists int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts, s.lists
}

// failingWorker is the episodic worker whose start fails at an injected fault.
type failingWorker struct {
	mu       sync.Mutex
	attempts int
}

func (w *failingWorker) Start(context.Context) error {
	w.mu.Lock()
	w.attempts++
	w.mu.Unlock()
	return errors.New("injected worker start failure")
}

func (w *failingWorker) Stop(context.Context) error { return nil }

func (w *failingWorker) startCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.attempts
}

// TestKnowledgeBackendConfigDefaultsToV2SourceDirectory pins the configuration
// entry the binary and the tests share: an unset or empty directory resolves to
// v2's own data/knowledge default - source storage on - and only an explicit
// value moves it. There is no value that turns sources off.
func TestKnowledgeBackendConfigDefaultsToV2SourceDirectory(t *testing.T) {
	config := KnowledgeBackendConfigFromEnvironment(func(string) string { return "" })
	if config.SourceDir != DefaultKnowledgeSourceDir {
		t.Fatalf("an unset directory resolved to %q, want the v2 default %q", config.SourceDir, DefaultKnowledgeSourceDir)
	}
	config = KnowledgeBackendConfigFromEnvironment(func(key string) string {
		if key == EnvKnowledgeSourceDir {
			return "   "
		}
		return ""
	})
	if config.SourceDir != DefaultKnowledgeSourceDir {
		t.Fatalf("an empty directory resolved to %q, want the v2 default", config.SourceDir)
	}
	config = KnowledgeBackendConfigFromEnvironment(func(key string) string {
		if key == EnvKnowledgeSourceDir {
			return "/somewhere/else"
		}
		return ""
	})
	if config.SourceDir != "/somewhere/else" {
		t.Fatalf("an explicit directory was ignored: %q", config.SourceDir)
	}
}

// TestKnowledgeBackendDefaultConfigurationEnablesSourcesAndStagesPending drives
// the real production configuration with no environment at all: the working
// directory is a temporary one, the config entry resolves the v2 default, and an
// upload through it must publish the bytes, stage pending, and return without
// waiting for an index pass that never answers.
func TestKnowledgeBackendDefaultConfigurationEnablesSourcesAndStagesPending(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	config := KnowledgeBackendConfigFromEnvironment(func(string) string { return "" })

	store := knowledge.NewMemoryStore()
	ingest := service.NewKnowledgeService(store)
	runner := newGatedRunner()
	backend, err := AssembleKnowledgeBackend(ingest, store, runner, NoExternalRows{}, config)
	if err != nil {
		t.Fatalf("assemble with the default configuration: %v", err)
	}
	// Source storage is on by default, rooted at the default directory under the
	// process's working directory.
	storage, ok := backend.sources.(*knowledge.LocalSourceStorage)
	if !ok {
		t.Fatal("the default configuration did not enable source storage")
	}
	if !strings.HasSuffix(filepath.ToSlash(storage.Root()), "data/knowledge") {
		t.Fatalf("the default source root is %q, want the default under the working directory", storage.Root())
	}
	if !strings.HasPrefix(filepath.ToSlash(storage.Root()), filepath.ToSlash(filepath.Clean(workDir))) {
		t.Fatalf("the source root %q escaped the temporary working directory", storage.Root())
	}

	ctx := context.Background()
	repo := repository.NewInMemoryRepository()
	user, err := repo.CreateUser(ctx, &domain.User{Email: "default@example.com", DisplayName: "Default"})
	if err != nil {
		t.Fatal(err)
	}
	collection, err := ingest.CreateCollection(ctx, user.ID, "学习笔记")
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = backend.Stop(stopCtx)
	})

	started := time.Now()
	document, err := ingest.AddDocument(ctx, user.ID, collection.ID, service.DocumentInput{
		Title: "aof.md", Format: string(domain.DocumentFormatMarkdown), Content: "# AOF\n\nfsync 可以选 always、everysec 或 no。\n",
	})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("the upload waited %v for a blocked index", elapsed)
	}
	state, err := ingest.DocumentWorkState(ctx, user.ID, document.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.IndexStatus != domain.DocumentStatusPending || state.Retrievable {
		t.Fatalf("a default-configuration upload reported %s retrievable=%v, want pending and not retrievable",
			state.IndexStatus, state.Retrievable)
	}
	// The submitted bytes are already readable: the ingest published them before
	// staging, under the default directory.
	if _, err := storage.ReadVersion(ctx, user.ID, document.ID, document.Version); err != nil {
		t.Fatalf("the published source is not readable: %v", err)
	}
	// The release lets the pass through, but the pool's scanner works on the
	// frozen 5s default cadence: its immediate startup scan ran before the
	// upload staged anything, so publishing waits for the next scan. The budget
	// below spans several scan periods so the wait never races the cadence it
	// is observing.
	runner.release()
	waitFor(t, "the default-configuration pass to publish", 20*time.Second, func() bool {
		state, err := ingest.DocumentWorkState(ctx, user.ID, document.ID)
		return err == nil && state.IndexStatus == domain.DocumentStatusReady && state.Retrievable
	})
}

// TestKnowledgeBackendRecoveryFailureLeavesNoBackgroundWork pins the recovery
// edge: a recovery failure fails the start at the fault itself, no worker or
// scanner runs afterwards, and the pool stays unstarted - a retried Start runs
// the recovery again instead of continuing from a half-started pool.
func TestKnowledgeBackendRecoveryFailureLeavesNoBackgroundWork(t *testing.T) {
	store := &failingRecoveryStore{MemoryStore: knowledge.NewMemoryStore()}
	ingest := service.NewKnowledgeService(store)
	backend, err := AssembleKnowledgeBackend(ingest, store, newGatedRunner(), NoExternalRows{},
		KnowledgeBackendConfig{SourceDir: t.TempDir(), Workers: 1, QueueSize: 4,
			ScanInterval: 5 * time.Millisecond, TaskTimeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if err := backend.Start(context.Background()); err == nil {
		t.Fatal("a recovery failure did not fail the start")
	}
	attempts, lists := store.snapshot()
	if attempts != 1 {
		t.Fatalf("the recovery fault fired %d time(s), want exactly the one start attempt", attempts)
	}
	// No scanner picked work up after the failure: the queue read count would
	// grow if a scan loop were still running.
	time.Sleep(50 * time.Millisecond)
	if _, lists = store.snapshot(); lists != 0 {
		t.Fatalf("the queue was read %d time(s) after a failed start; background work is still running", lists)
	}
	// The pool was left unstarted, so a retried Start runs the recovery again
	// rather than silently continuing.
	if err := backend.Start(context.Background()); err == nil {
		t.Fatal("the retried start did not run recovery again")
	}
	if attempts, _ = store.snapshot(); attempts != 2 {
		t.Fatalf("the retried start did not re-run recovery: %d attempt(s)", attempts)
	}
}

// TestKnowledgeBackendPartialStartFailureStopsTheStartedPool pins the partial
// start: when the cleanup pool refuses to start, the already-started index pool
// is stopped before the error returns - it is not left running unowned.
func TestKnowledgeBackendPartialStartFailureStopsTheStartedPool(t *testing.T) {
	store := knowledge.NewMemoryStore()
	ingest := service.NewKnowledgeService(store)
	backend, err := AssembleKnowledgeBackend(ingest, store, newGatedRunner(), NoExternalRows{},
		KnowledgeBackendConfig{SourceDir: t.TempDir(), Workers: 1, QueueSize: 4,
			ScanInterval: 5 * time.Millisecond, TaskTimeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	// The fault: the cleanup pool is already started, so the backend's second
	// start step fails after the index pool is up.
	if err := backend.cleanupPool.Start(context.Background()); err != nil {
		t.Fatalf("pre-starting the cleanup pool: %v", err)
	}
	startErr := backend.Start(context.Background())
	if startErr == nil {
		t.Fatal("a cleanup pool that refuses to start did not fail the backend start")
	}
	if !strings.Contains(startErr.Error(), "cleanup pool") {
		t.Fatalf("the failure did not name the cleanup pool step: %v", startErr)
	}
	// The index pool was started and then reclaimed: starting it again is
	// refused, because a pool that has stopped stays stopped. A pool that had
	// never started would instead begin its recovery and succeed.
	indexErr := backend.indexPool.Start(context.Background())
	if indexErr == nil || !strings.Contains(indexErr.Error(), "already started") {
		t.Fatalf("the started index pool was not reclaimed: %v", indexErr)
	}
}

// TestStartBackgroundDriversReclaimsPoolsWhenTheWorkerFails pins the startup
// window the binary uses: when the episodic worker's start fails, the error that
// reaches the caller has already drained the knowledge pools - the cleanup runs
// on the return path, not on a deferred stop a fatal exit would skip.
func TestStartBackgroundDriversReclaimsPoolsWhenTheWorkerFails(t *testing.T) {
	store := knowledge.NewMemoryStore()
	ingest := service.NewKnowledgeService(store)
	backend, err := AssembleKnowledgeBackend(ingest, store, newGatedRunner(), NoExternalRows{},
		KnowledgeBackendConfig{SourceDir: t.TempDir(), Workers: 1, QueueSize: 4,
			ScanInterval: 5 * time.Millisecond, TaskTimeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	worker := &failingWorker{}
	startErr := StartBackgroundDrivers(context.Background(), backend, worker)
	if startErr == nil {
		t.Fatal("a failing worker did not fail the startup window")
	}
	if !strings.Contains(startErr.Error(), "episodic worker start") {
		t.Fatalf("the failure did not name the worker step: %v", startErr)
	}
	if worker.startCount() != 1 {
		t.Fatalf("the worker fault fired %d time(s), want exactly one", worker.startCount())
	}
	// The pools started inside the window were reclaimed before the error was
	// returned: starting the index pool again is refused, not silently doubled.
	indexErr := backend.indexPool.Start(context.Background())
	if indexErr == nil || !strings.Contains(indexErr.Error(), "already started") {
		t.Fatalf("the knowledge pools were not reclaimed on the worker's failure: %v", indexErr)
	}
}
