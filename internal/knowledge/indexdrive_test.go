package knowledge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/domain"
)

// The driver tests call process and scan directly, the way v2's worker tests do
// (service/knowledge/worker_test.go): the state machine is what is being pinned, and a
// ticker would only add a way to fail for the wrong reason. One test drives the whole
// Start -> scan -> queue -> worker path to prove the wiring exists.

type callRecord struct {
	userID     string
	documentID string
	version    int
}

// stubRunner records what the index was asked to do and answers with a fixed result.
type stubRunner struct {
	mu       sync.Mutex
	calls    []callRecord
	written  int
	err      error
	entered  chan callRecord
	onInvoke func(callRecord)
}

func (r *stubRunner) IndexVersion(ctx context.Context, userID, documentID string, version int) (int, error) {
	record := callRecord{userID: userID, documentID: documentID, version: version}
	r.mu.Lock()
	r.calls = append(r.calls, record)
	r.mu.Unlock()
	if r.entered != nil {
		// Closed-once signalling, so a second call cannot panic on a double close.
		select {
		case r.entered <- record:
		default:
		}
	}
	if r.onInvoke != nil {
		r.onInvoke(record)
	}
	if r.err != nil {
		return 0, r.err
	}
	return r.written, nil
}

func (r *stubRunner) snapshot() []callRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]callRecord(nil), r.calls...)
}

// blockedRunner is the cancellation fixture: it does not return until the context it was
// handed ends, which is what an HTTP embedder does when its caller walks away. The test
// cancels at the moment the call is known to be in flight, so no wall clock is guessed.
type blockedRunner struct {
	entered   chan callRecord
	release   chan struct{}
	returnErr error
}

func (b *blockedRunner) IndexVersion(ctx context.Context, userID, documentID string, version int) (int, error) {
	select {
	case b.entered <- callRecord{userID: userID, documentID: documentID, version: version}:
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	select {
	case <-b.release:
		return 0, b.returnErr
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

type stubCleaner struct {
	mu       sync.Mutex
	sweeps   []callRecord
	dropped  []callRecord
	err      error
	entered  chan callRecord
	release  chan struct{}
	returnEr error
}

func (c *stubCleaner) SweepExcept(ctx context.Context, userID, documentID string, keep int) error {
	c.mu.Lock()
	c.sweeps = append(c.sweeps, callRecord{userID: userID, documentID: documentID, version: keep})
	c.mu.Unlock()
	c.notify(ctx, callRecord{userID: userID, documentID: documentID, version: keep})
	return c.err
}

func (c *stubCleaner) DropDocument(ctx context.Context, userID, documentID string) error {
	c.mu.Lock()
	c.dropped = append(c.dropped, callRecord{userID: userID, documentID: documentID})
	c.mu.Unlock()
	c.notify(ctx, callRecord{userID: userID, documentID: documentID})
	if c.returnEr != nil {
		return c.returnEr
	}
	return c.err
}

func (c *stubCleaner) notify(ctx context.Context, record callRecord) {
	if c.entered == nil {
		return
	}
	select {
	case c.entered <- record:
	case <-ctx.Done():
	}
}

func (c *stubCleaner) snapshot() (sweeps, dropped []callRecord) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]callRecord(nil), c.sweeps...), append([]callRecord(nil), c.dropped...)
}

// sourceRecorder is the second removal stage, used to prove the source bytes are dropped
// only after the derived rows went, and that a source failure is recorded as one.
type sourceRecorder struct {
	versions  []callRecord
	documents []callRecord
	err       error
}

func (s *sourceRecorder) DropVersion(ctx context.Context, userID, documentID string, version int) error {
	s.versions = append(s.versions, callRecord{userID: userID, documentID: documentID, version: version})
	return s.err
}

func (s *sourceRecorder) DropDocument(ctx context.Context, userID, documentID string) error {
	s.documents = append(s.documents, callRecord{userID: userID, documentID: documentID})
	return s.err
}

// indexPoolFixture builds a pool over a memory store with a small queue.
func indexPoolFixture(t *testing.T, runner IndexRunner, config WorkerConfig) (*MemoryStore, *IndexPool) {
	t.Helper()
	store := NewMemoryStore()
	pool, err := NewIndexPool(store, runner, config)
	if err != nil {
		t.Fatalf("NewIndexPool: %v", err)
	}
	pool.WithLogger(func(string, ...any) {})
	return store, pool
}

func defaultPoolConfig() WorkerConfig {
	return WorkerConfig{Workers: 1, QueueSize: 4, ScanInterval: 10 * time.Millisecond}
}

// stageDocument stages one version of one document and returns it.
func stageDocument(t *testing.T, store *MemoryStore, userID, collectionID, title string) domain.Document {
	t.Helper()
	staged, err := store.StageDocument(context.Background(), userID, &domain.Document{
		CollectionID: collectionID, Title: title, Format: domain.DocumentFormatMarkdown,
	}, []domain.DocumentChunk{{Text: "内容 " + title}})
	if err != nil {
		t.Fatalf("StageDocument: %v", err)
	}
	return *staged
}

func newCollection(t *testing.T, store *MemoryStore, userID string) string {
	t.Helper()
	collection, err := store.CreateCollection(context.Background(), &domain.Collection{UserID: userID, Title: "手册"})
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	return collection.ID
}

func mustDocument(t *testing.T, store *MemoryStore, userID, documentID string) domain.Document {
	t.Helper()
	document, err := store.GetDocument(context.Background(), userID, documentID)
	if err != nil {
		t.Fatalf("GetDocument(%s): %v", documentID, err)
	}
	return *document
}

// publish drives one staged version to ready the way the store expects, so a test can
// set up "version 1 answering" without depending on the pool it is about to test.
func publish(t *testing.T, store *MemoryStore, userID string, document domain.Document) domain.Document {
	t.Helper()
	claimed, err := store.ClaimForIndexing(context.Background(), userID, document.ID, document.Version)
	if err != nil || !claimed {
		t.Fatalf("claim v%d = (%v, %v)", document.Version, claimed, err)
	}
	if err := store.CompleteIndexing(context.Background(), userID, document.ID, document.Version, IndexOutcome{
		Status: domain.DocumentStatusReady, ChunkCount: 1,
	}); err != nil {
		t.Fatalf("CompleteIndexing: %v", err)
	}
	published, err := store.GetDocument(context.Background(), userID, document.ID)
	if err != nil {
		t.Fatal(err)
	}
	return *published
}

// waitFor polls an observable with a deadline. It waits on a state the pool itself
// writes, never on a fixed sleep, so a slow machine cannot make it pass and a hung
// worker cannot make it hang.
func waitFor(t *testing.T, timeout time.Duration, description string, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

func TestWorkerConfigRefusesEveryBrokenValue(t *testing.T) {
	cases := []struct {
		name   string
		config WorkerConfig
		want   string
	}{
		{"no workers", WorkerConfig{Workers: 0, QueueSize: 4, ScanInterval: time.Second}, "worker count"},
		{"queue smaller than the pool", WorkerConfig{Workers: 3, QueueSize: 2, ScanInterval: time.Second}, "queue size"},
		{"no scan interval", WorkerConfig{Workers: 1, QueueSize: 4, ScanInterval: 0}, "scan interval"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			store := NewMemoryStore()
			runner := &stubRunner{written: 1}
			if _, err := NewIndexPool(store, runner, testCase.config); err == nil ||
				!strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("NewIndexPool = %v, want it to name %q", err, testCase.want)
			}
			if _, err := NewCleanupPool(store, &stubCleaner{}, nil, testCase.config); err == nil ||
				!strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("NewCleanupPool = %v, want it to name %q", err, testCase.want)
			}
		})
	}
	if _, err := NewIndexPool(nil, &stubRunner{}, defaultPoolConfig()); err == nil {
		t.Fatal("a pool with no lifecycle was accepted")
	}
	if _, err := NewIndexPool(NewMemoryStore(), nil, defaultPoolConfig()); err == nil {
		t.Fatal("a pool with no runner was accepted")
	}
}

func TestIndexPoolStartsOnceAndNeverWithoutRecovery(t *testing.T) {
	runner := &stubRunner{written: 1}
	store, pool := indexPoolFixture(t, runner, defaultPoolConfig())
	collectionID := newCollection(t, store, "user-1")
	stageDocument(t, store, "user-1", collectionID, "start.md")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := pool.Start(ctx); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	if err := pool.Start(ctx); !errors.Is(err, errPoolStarted) {
		t.Fatalf("second Start = %v, want the refusal", err)
	}
	// v2 leaves a stopped pool stopped: starting a second set of goroutines over the
	// same queue would be the thing the refusal exists to prevent.
	if err := pool.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := pool.Start(ctx); !errors.Is(err, errPoolStarted) {
		t.Fatalf("Start after Stop = %v, want the refusal", err)
	}

	broken := &recoveryRefusingLifecycle{Lifecycle: store}
	if _, err := NewIndexPool(broken, runner, defaultPoolConfig()); err != nil {
		t.Fatal(err)
	}
	brokenPool, err := NewIndexPool(broken, runner, defaultPoolConfig())
	if err != nil {
		t.Fatal(err)
	}
	brokenPool.WithLogger(func(string, ...any) {})
	startErr := brokenPool.Start(context.Background())
	if startErr == nil || !strings.Contains(startErr.Error(), "recover interrupted indexing") {
		t.Fatalf("Start over a store that could not recover = %v", startErr)
	}
	// A refused start must not leave half a pool behind.
	if err := brokenPool.Stop(context.Background()); err != nil {
		t.Fatalf("Stop after a refused Start: %v", err)
	}
}

// recoveryRefusingLifecycle is the fixture that proves recovery is a precondition, not a
// best-effort log line. It must forward StagedChunks too, because a pool that starts
// successfully will ask for it.
type recoveryRefusingLifecycle struct {
	Lifecycle
}

func (r *recoveryRefusingLifecycle) RecoverInterruptedIndexing(context.Context) (int, error) {
	return 0, errors.New("database is unreachable")
}

func TestIndexPoolWorksTheQueueEndToEnd(t *testing.T) {
	entered := make(chan callRecord, 4)
	runner := &stubRunner{written: 1, entered: entered}
	store, pool := indexPoolFixture(t, runner, defaultPoolConfig())
	collectionID := newCollection(t, store, "user-1")
	staged := stageDocument(t, store, "user-1", collectionID, "queued.md")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := pool.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := pool.Stop(context.Background()); err != nil {
			t.Errorf("Stop: %v", err)
		}
	}()

	call := <-entered
	if call.documentID != staged.ID || call.userID != "user-1" || call.version != staged.Version {
		t.Fatalf("index call = %+v, want user-1's staged version %d", call, staged.Version)
	}
	waitFor(t, 2*time.Second, "the queue to publish the document", func() bool {
		document, err := store.GetDocument(context.Background(), "user-1", staged.ID)
		return err == nil && document.IndexStatus == domain.DocumentStatusReady && document.ActiveVersion == staged.Version
	})
	if chunks, err := store.ChunksFor(context.Background(), "user-1", []string{collectionID}); err != nil || len(chunks) != 1 {
		t.Fatalf("retrieval after the queue ran = %#v (%v)", chunks, err)
	}
}

func TestIndexPoolRecordsAFailedPassAndKeepsTheAnsweringVersion(t *testing.T) {
	runner := &stubRunner{err: errors.New("embedder at http://internal-host:9/v1 returned 503")}
	store, pool := indexPoolFixture(t, runner, defaultPoolConfig())
	collectionID := newCollection(t, store, "user-1")
	first := publish(t, store, "user-1", stageDocument(t, store, "user-1", collectionID, "keep.md"))

	// An update is staged but never indexed, and the pass that would publish it fails.
	staged, err := store.StageReplacement(context.Background(), "user-1", first.ID, first.Version, DocumentReplacement{
		Format: domain.DocumentFormatMarkdown, Chunks: []domain.DocumentChunk{{Text: "改写后的内容"}},
	})
	if err != nil {
		t.Fatalf("StageReplacement: %v", err)
	}
	pool.process(context.Background(), QueuedDocument{UserID: "user-1", Document: *staged})

	after := mustDocument(t, store, "user-1", first.ID)
	if after.IndexStatus != domain.DocumentStatusFailed || after.Version != 2 || after.ActiveVersion != 1 {
		t.Fatalf("document after the failed pass = %#v, want v2 failed with v1 answering", after)
	}
	if after.ErrorMessage != "indexing operation failed" {
		t.Fatalf("error message = %q, want the bounded reason", after.ErrorMessage)
	}
	if !strings.Contains(after.ErrorMessage, "failed") {
		t.Fatal("no reason recorded")
	}
	// The runner's error text carries a host and a path; none of that may be echoed.
	if strings.Contains(after.ErrorMessage, "internal-host") || strings.Contains(after.ErrorMessage, "503") {
		t.Fatalf("the driver echoed the upstream error into a row a client reads: %q", after.ErrorMessage)
	}
	if chunks, err := store.ChunksFor(context.Background(), "user-1", []string{collectionID}); err != nil ||
		len(chunks) != 1 || chunks[0].Text != "内容 keep.md" {
		t.Fatalf("the answering version was displaced: %#v (%v)", chunks, err)
	}
	if err := store.RetryFailed(context.Background(), "user-1", first.ID); err != nil {
		t.Fatalf("a failed pass must be retryable: %v", err)
	}
}

func TestIndexPoolRequeuesACallerThatWalkedAway(t *testing.T) {
	runner := &blockedRunner{entered: make(chan callRecord, 1), release: make(chan struct{}), returnErr: context.Canceled}
	store, pool := indexPoolFixture(t, runner, defaultPoolConfig())
	collectionID := newCollection(t, store, "user-1")
	staged := stageDocument(t, store, "user-1", collectionID, "walk-away.md")

	ctx, cancel := context.WithCancel(context.Background())
	driven := make(chan struct{})
	go func() {
		pool.process(ctx, QueuedDocument{UserID: "user-1", Document: staged})
		close(driven)
	}()
	call := <-runner.entered
	if call.documentID != staged.ID {
		t.Fatalf("the pass ran for %q", call.documentID)
	}
	cancel()
	<-driven
	close(runner.release)

	// The row must be pending again, with no reason recorded, and claimable.
	after := mustDocument(t, store, "user-1", staged.ID)
	if after.IndexStatus != domain.DocumentStatusPending || after.ErrorMessage != "" {
		t.Fatalf("document after a cancellation = %#v, want it back in the queue with no failure", after)
	}
	claimed, err := store.ClaimForIndexing(context.Background(), "user-1", after.ID, after.Version)
	if err != nil || !claimed {
		t.Fatalf("re-claim = (%v, %v)", claimed, err)
	}
	if chunks, err := store.ChunksFor(context.Background(), "user-1", []string{collectionID}); err != nil || len(chunks) != 0 {
		t.Fatalf("a requeued version is answering: %#v (%v)", chunks, err)
	}
}

func TestIndexPoolRecordsSuccessWithThePoolContextNotTheTaskBudget(t *testing.T) {
	// The budget belongs to the attempt; the success belongs to the pool. A pass that
	// finished indexing must not lose its publication because its own task context had
	// already been released when the completion was written - that is a document stuck
	// in indexing with the vectors already in place, which is the worst of both states.
	runner := &stubRunner{written: 1}
	config := defaultPoolConfig()
	config.TaskTimeout = time.Second
	store, pool := indexPoolFixture(t, runner, config)
	collectionID := newCollection(t, store, "user-1")
	first := publish(t, store, "user-1", stageDocument(t, store, "user-1", collectionID, "budget.md"))
	staged, err := store.StageReplacement(context.Background(), "user-1", first.ID, first.Version, DocumentReplacement{
		Format: domain.DocumentFormatMarkdown, Chunks: []domain.DocumentChunk{{Text: "第二版"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	pool.process(context.Background(), QueuedDocument{UserID: "user-1", Document: *staged})

	after := mustDocument(t, store, "user-1", staged.ID)
	// A successful rewrite leaves the record in cleanup_pending with the superseded
	// version named: the index pool's job ends at publishing, and closing the record is
	// the cleanup pool's. What must never be the outcome is a document still in
	// indexing with its vectors already written.
	if after.ActiveVersion != 2 || after.Version != 2 || after.ChunkCount != 1 {
		t.Fatalf("document after a successful pass = %#v, want version 2 published", after)
	}
	if after.IndexStatus != domain.DocumentStatusReady && after.IndexStatus != domain.DocumentStatusCleanupPending {
		t.Fatalf("index status after a successful pass = %q, want it published", after.IndexStatus)
	}
	if after.IndexStatus == domain.DocumentStatusCleanupPending && after.CleanupVersion != 1 {
		t.Fatalf("cleanup version = %d, want the superseded version recorded", after.CleanupVersion)
	}
	chunks, err := store.ChunksFor(context.Background(), "user-1", []string{collectionID})
	if err != nil || len(chunks) != 1 || chunks[0].Text != "第二版" {
		t.Fatalf("retrieval after the pass = %#v (%v)", chunks, err)
	}
}

func TestIndexPoolRequeuesAPassThatGaveUpOnItsOwn(t *testing.T) {
	// v2's condition is `ctx.Err() != nil || errors.Is(ingestErr, context.Canceled)`
	// (worker.go:203). The second half is what this drives: the caller is still waiting,
	// and the pass reports a cancellation from inside - an embedder client whose own
	// connection was reset, say. That is an abandoned attempt, not a failed document,
	// and recording it as a failure would put a retry the user has to notice in front of
	// a queue that would have simply run it again.
	runner := &stubRunner{err: fmt.Errorf("embedder: %w", context.Canceled)}
	store, pool := indexPoolFixture(t, runner, defaultPoolConfig())
	collectionID := newCollection(t, store, "user-1")
	first := publish(t, store, "user-1", stageDocument(t, store, "user-1", collectionID, "reset.md"))
	staged, err := store.StageReplacement(context.Background(), "user-1", first.ID, first.Version, DocumentReplacement{
		Format: domain.DocumentFormatMarkdown, Chunks: []domain.DocumentChunk{{Text: "第二版"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	pool.process(context.Background(), QueuedDocument{UserID: "user-1", Document: *staged})

	after := mustDocument(t, store, "user-1", staged.ID)
	if after.IndexStatus != domain.DocumentStatusPending || after.ErrorMessage != "" {
		t.Fatalf("document = %#v, want the attempt abandoned and no reason recorded", after)
	}
	if after.ActiveVersion != 1 || !after.Retrievable() {
		t.Fatalf("the answering version did not survive: %#v", after)
	}
	// Pending work is on the queue, so the next scan takes it without an operator.
	queued, err := store.ListPending(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 1 || queued[0].Document.ID != after.ID || queued[0].UserID != "user-1" {
		t.Fatalf("queue = %+v, want the abandoned version back at the front", queued)
	}
}

func TestIndexPoolTimeoutIsAFailureEvenThoughTheCallerWaited(t *testing.T) {
	runner := &blockedRunner{entered: make(chan callRecord, 1), release: make(chan struct{})}
	config := defaultPoolConfig()
	config.TaskTimeout = 15 * time.Millisecond
	store, pool := indexPoolFixture(t, runner, config)
	collectionID := newCollection(t, store, "user-1")
	first := publish(t, store, "user-1", stageDocument(t, store, "user-1", collectionID, "slow.md"))
	staged, err := store.StageReplacement(context.Background(), "user-1", first.ID, first.Version, DocumentReplacement{
		Format: domain.DocumentFormatMarkdown, Chunks: []domain.DocumentChunk{{Text: "新内容"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	// The caller's context is alive for the whole pass; only the task budget runs out.
	driveCtx, cancelDrive := context.WithCancel(context.Background())
	defer cancelDrive()
	go func() {
		pool.process(driveCtx, QueuedDocument{UserID: "user-1", Document: *staged})
	}()
	<-runner.entered
	waitFor(t, 2*time.Second, "the timed-out pass to be recorded", func() bool {
		document, err := store.GetDocument(driveCtx, "user-1", staged.ID)
		return err == nil && document.IndexStatus == domain.DocumentStatusFailed
	})
	after := mustDocument(t, store, "user-1", staged.ID)
	if after.ErrorMessage != "indexing timed out" {
		t.Fatalf("error message = %q, want the timeout named", after.ErrorMessage)
	}
	if after.ActiveVersion != 1 || !after.Retrievable() {
		t.Fatalf("the answering version did not survive the timeout: %#v", after)
	}
	// A failed row cannot be re-claimed directly - only a retry releases it, which is
	// what stops a second worker from racing the operator.
	claimed, err := store.ClaimForIndexing(driveCtx, "user-1", after.ID, after.Version)
	if err != nil || claimed {
		t.Fatalf("a failed version claims as (%v, %v), want it held until retried", claimed, err)
	}
	if err := store.RetryFailed(driveCtx, "user-1", after.ID); err != nil {
		t.Fatalf("RetryFailed: %v", err)
	}
}

func TestIndexPoolRefusesToRequeueWithTheContextThatJustDied(t *testing.T) {
	// The requeue write runs on its own bounded context. This fixture makes the store
	// refuse a dead context (which it does on its own, through checkContext), so an
	// implementation that passed the cancelled one through would strand the row in
	// indexing - and the assertion below would fail instead of passing quietly.
	runner := &blockedRunner{entered: make(chan callRecord, 1), release: make(chan struct{})}
	store, pool := indexPoolFixture(t, runner, defaultPoolConfig())
	collectionID := newCollection(t, store, "user-1")
	staged := stageDocument(t, store, "user-1", collectionID, "stranded.md")

	ctx, cancel := context.WithCancel(context.Background())
	driven := make(chan error, 1)
	go func() {
		pool.process(ctx, QueuedDocument{UserID: "user-1", Document: staged})
		driven <- ctx.Err()
	}()
	<-runner.entered
	cancel()
	<-driven
	close(runner.release)

	after := mustDocument(t, store, "user-1", staged.ID)
	if after.IndexStatus != domain.DocumentStatusPending {
		t.Fatalf("status = %q, want the requeue to have been written on a live context", after.IndexStatus)
	}
	if _, err := store.ListPending(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
}

func TestIndexPoolStopIsBoundedAndLosesNoWork(t *testing.T) {
	// A pass that ignores cancellation is the only thing that can make a shutdown
	// unbounded, and unbounded is what Stop must refuse. (A pass that honours its
	// context drains immediately, which the cancellation tests above already cover -
	// measuring that here would prove nothing about the bound.)
	runner := &stuckRunner{entered: make(chan callRecord, 4), release: make(chan struct{}), returnErr: context.Canceled}
	config := defaultPoolConfig()
	config.QueueSize = 2
	store, pool := indexPoolFixture(t, runner, config)
	collectionID := newCollection(t, store, "user-1")
	staged := stageDocument(t, store, "user-1", collectionID, "in-flight.md")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := pool.Start(ctx); err != nil {
		t.Fatal(err)
	}
	<-runner.entered

	// A shutdown that cannot finish is reported, not hidden.
	boundCtx, cancelBound := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelBound()
	if err := pool.Stop(boundCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stop = %v, want it to give up on a pass that ignores cancellation", err)
	}

	// Once the pass returns, the cancellation the shutdown caused re-queues it: the
	// document is pending for the next process rather than claimed by nobody.
	close(runner.release)
	waitFor(t, 2*time.Second, "the in-flight pass to be requeued", func() bool {
		document, err := store.GetDocument(context.Background(), "user-1", staged.ID)
		return err == nil && document.IndexStatus == domain.DocumentStatusPending
	})
	if err := pool.Stop(context.Background()); err != nil {
		t.Fatalf("the second Stop = %v, want the pool to be drained", err)
	}
}

// stuckRunner never looks at its context: it returns only when the test releases it.
type stuckRunner struct {
	entered   chan callRecord
	release   chan struct{}
	returnErr error
}

func (b *stuckRunner) IndexVersion(_ context.Context, userID, documentID string, version int) (int, error) {
	select {
	case b.entered <- callRecord{userID: userID, documentID: documentID, version: version}:
	default:
	}
	<-b.release
	if b.returnErr != nil {
		return 0, b.returnErr
	}
	return 1, nil
}

func TestQueueItemsCarryTheirOwnerAcrossBothQueues(t *testing.T) {
	store := NewMemoryStore()
	firstCollection := newCollection(t, store, "user-1")
	secondCollection := newCollection(t, store, "user-2")
	first := stageDocument(t, store, "user-1", firstCollection, "one.md")
	second := stageDocument(t, store, "user-2", secondCollection, "two.md")

	queued, err := store.ListPending(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 2 {
		t.Fatalf("queue = %d items, want both users' staged work", len(queued))
	}
	owners := map[string]string{}
	for _, item := range queued {
		owners[item.Document.ID] = item.UserID
	}
	if owners[first.ID] != "user-1" || owners[second.ID] != "user-2" {
		t.Fatalf("queue owners = %v, want each item to name its own user", owners)
	}

	// Working the queue with a mismatched owner must fail: a driver that took the user
	// from anywhere other than the item could claim another user's document.
	claimed, err := store.ClaimForIndexing(context.Background(), "user-2", first.ID, first.Version)
	if err != nil || claimed {
		t.Fatalf("cross-user claim = (%v, %v), want it refused the way a missing document is", claimed, err)
	}
	runner := &stubRunner{written: 1}
	_, pool := indexPoolFixture(t, runner, defaultPoolConfig())
	pool.lifecycle = store
	for _, item := range queued {
		pool.process(context.Background(), item)
	}
	calls := runner.snapshot()
	if len(calls) != 2 {
		t.Fatalf("index calls = %d, want one per queued item", len(calls))
	}
	for _, call := range calls {
		if want := owners[call.documentID]; call.userID != want {
			t.Fatalf("indexed %s under %q, want its owner %q", call.documentID, call.userID, want)
		}
	}
	for _, item := range queued {
		after := mustDocument(t, store, owners[item.Document.ID], item.Document.ID)
		if after.IndexStatus != domain.DocumentStatusReady || after.ActiveVersion != after.Version {
			t.Fatalf("document %#v was not published under its own owner", after)
		}
	}
}

func TestCleanupPoolFinishesRecordedWorkInOrder(t *testing.T) {
	store := NewMemoryStore()
	collectionID := newCollection(t, store, "user-1")
	first := publish(t, store, "user-1", stageDocument(t, store, "user-1", collectionID, "v1.md"))
	staged, err := store.StageReplacement(context.Background(), "user-1", first.ID, first.Version, DocumentReplacement{
		Format: domain.DocumentFormatMarkdown, Chunks: []domain.DocumentChunk{{Text: "第二版"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	published := publish(t, store, "user-1", *staged)
	if published.IndexStatus != domain.DocumentStatusCleanupPending || published.CleanupVersion != 1 {
		t.Fatalf("document after a successful rewrite = %#v, want v1 queued for cleanup", published)
	}

	cleaner := &stubCleaner{}
	sources := &sourceRecorder{}
	pool, err := NewCleanupPool(store, cleaner, sources, defaultPoolConfig())
	if err != nil {
		t.Fatal(err)
	}
	pool.WithLogger(func(string, ...any) {})
	pool.process(context.Background(), QueuedDocument{UserID: "user-1", Document: published})

	sweeps, _ := cleaner.snapshot()
	if len(sweeps) != 1 || sweeps[0].version != 2 || sweeps[0].userID != "user-1" {
		t.Fatalf("sweeps = %+v, want the superseded rows dropped keeping version 2", sweeps)
	}
	if len(sources.versions) != 1 || sources.versions[0].version != 1 {
		t.Fatalf("source removals = %+v, want the superseded version's bytes dropped after the rows", sources.versions)
	}
	after := mustDocument(t, store, "user-1", published.ID)
	if after.IndexStatus != domain.DocumentStatusReady || after.CleanupVersion != 0 {
		t.Fatalf("document after cleanup = %#v, want the record closed", after)
	}
	due, err := store.ListDueCleanup(context.Background(), time.Now().UTC().Add(time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range due {
		if item.Document.ID == published.ID {
			t.Fatal("a finished cleanup is still due")
		}
	}
}

func TestCleanupPoolRecordsAFailureWithTheSourceSchedule(t *testing.T) {
	store := NewMemoryStore()
	collectionID := newCollection(t, store, "user-1")
	first := publish(t, store, "user-1", stageDocument(t, store, "user-1", collectionID, "sweep.md"))
	staged, err := store.StageReplacement(context.Background(), "user-1", first.ID, first.Version, DocumentReplacement{
		Format: domain.DocumentFormatMarkdown, Chunks: []domain.DocumentChunk{{Text: "第二版"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	published := publish(t, store, "user-1", *staged)

	cleaner := &stubCleaner{err: errors.New("redis: NOAUTH password=hunter2")}
	stayPut := &sourceRecorder{}
	pool, err := NewCleanupPool(store, cleaner, stayPut, defaultPoolConfig())
	if err != nil {
		t.Fatal(err)
	}
	pool.WithLogger(func(string, ...any) {})
	before := time.Now().UTC()
	pool.process(context.Background(), QueuedDocument{UserID: "user-1", Document: published})
	// The bytes of a version are removed only after nothing points at them: dropping the
	// source first would leave a searchable document whose citation can no longer be
	// read back.
	if len(stayPut.versions) != 0 || len(stayPut.documents) != 0 {
		t.Fatalf("source removals while the rows still failed: versions=%+v documents=%+v",
			stayPut.versions, stayPut.documents)
	}

	after := mustDocument(t, store, "user-1", published.ID)
	if after.CleanupAttempts != 1 || after.IndexStatus != domain.DocumentStatusCleanupPending {
		t.Fatalf("document after a failed cleanup = %#v, want the work still recorded", after)
	}
	if after.ErrorMessage != "cleanup: vector delete failed" {
		t.Fatalf("error message = %q, want v2's phrasing", after.ErrorMessage)
	}
	if strings.Contains(after.ErrorMessage, "NOAUTH") || strings.Contains(after.ErrorMessage, "hunter2") {
		t.Fatalf("the cleanup failure echoed the upstream error: %q", after.ErrorMessage)
	}
	// The first retry waits one step of v2's schedule (one second), and the item stays
	// out of the queue until then. Both bounds are measured against the recorded time,
	// so nothing here races a wall clock.
	if after.CleanupAfter.Before(before.Add(CleanupBackoff(1) - 250*time.Millisecond)) {
		t.Fatalf("retry at %v, want it at least %v after the attempt", after.CleanupAfter, CleanupBackoff(1))
	}
	if cleanupIsDue(t, store, before.Add(200*time.Millisecond), published.ID) {
		t.Fatal("a cleanup inside its backoff window came back as due")
	}
	if !cleanupIsDue(t, store, after.CleanupAfter, published.ID) {
		t.Fatal("the recorded retry never became due")
	}

	// The source stage is only reached once the rows are gone, and its own failure is
	// recorded as a source failure rather than retried as a row removal.
	if err := store.FailCleanup(context.Background(), "user-1", published.ID, "reset", before.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	sources := &sourceRecorder{err: errors.New("disk full")}
	sourcePool, err := NewCleanupPool(store, &stubCleaner{}, sources, defaultPoolConfig())
	if err != nil {
		t.Fatal(err)
	}
	sourcePool.WithLogger(func(string, ...any) {})
	secondRun := time.Now().UTC()
	sourcePool.process(context.Background(), QueuedDocument{UserID: "user-1", Document: mustDocument(t, store, "user-1", published.ID)})
	if len(sources.versions) != 1 {
		t.Fatalf("source removals = %+v, want the stage to have been attempted", sources.versions)
	}
	recorded := mustDocument(t, store, "user-1", published.ID)
	if recorded.ErrorMessage != "cleanup: source delete failed" {
		t.Fatalf("error message = %q, want the failing stage named", recorded.ErrorMessage)
	}
	// The wait grows with the attempt count - that is the whole point of a schedule: an
	// index that stays broken is retried more slowly, not at a fixed rate.
	if recorded.CleanupAfter.Before(secondRun.Add(CleanupBackoff(recorded.CleanupAttempts) - 250*time.Millisecond)) {
		t.Fatalf("retry at %v after %d attempts, want at least %v",
			recorded.CleanupAfter, recorded.CleanupAttempts, CleanupBackoff(recorded.CleanupAttempts))
	}
}

// cleanupIsDue asks the store who owes cleanup work at a given moment.
func cleanupIsDue(t *testing.T, store *MemoryStore, at time.Time, documentID string) bool {
	t.Helper()
	due, err := store.ListDueCleanup(context.Background(), at, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range due {
		if item.Document.ID == documentID {
			return true
		}
	}
	return false
}

func TestCleanupPoolLeavesWorkToTheLeaseHolder(t *testing.T) {
	store := NewMemoryStore()
	collectionID := newCollection(t, store, "user-1")
	first := publish(t, store, "user-1", stageDocument(t, store, "user-1", collectionID, "lease.md"))
	staged, err := store.StageReplacement(context.Background(), "user-1", first.ID, first.Version, DocumentReplacement{
		Format: domain.DocumentFormatMarkdown, Chunks: []domain.DocumentChunk{{Text: "第二版"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	published := publish(t, store, "user-1", *staged)

	now := time.Now().UTC()
	claimed, err := store.ClaimCleanup(context.Background(), "user-1", published.ID, now, now.Add(time.Minute))
	if err != nil || !claimed {
		t.Fatalf("first lease = (%v, %v)", claimed, err)
	}
	cleaner := &stubCleaner{}
	pool, err := NewCleanupPool(store, cleaner, nil, defaultPoolConfig())
	if err != nil {
		t.Fatal(err)
	}
	pool.WithLogger(func(string, ...any) {})
	// A second worker that reaches the same item must not do the work twice.
	pool.process(context.Background(), QueuedDocument{UserID: "user-1", Document: published})
	sweeps, dropped := cleaner.snapshot()
	if len(sweeps) != 0 || len(dropped) != 0 {
		t.Fatalf("a held lease was worked anyway: sweeps=%+v dropped=%+v", sweeps, dropped)
	}
	// The lease expires on its own, and then the work is claimable again.
	if again, err := store.ClaimCleanup(context.Background(), "user-1", published.ID,
		now.Add(2*time.Minute), now.Add(3*time.Minute)); err != nil || !again {
		t.Fatalf("after the lease = (%v, %v), want the work open again", again, err)
	}
}

func TestCleanupPoolRemovesEveryRowOfADeletingDocument(t *testing.T) {
	store := NewMemoryStore()
	collectionID := newCollection(t, store, "user-1")
	document := publish(t, store, "user-1", stageDocument(t, store, "user-1", collectionID, "gone.md"))
	deleting, err := store.MarkDeleting(context.Background(), "user-1", document.ID)
	if err != nil {
		t.Fatal(err)
	}
	cleaner := &stubCleaner{}
	sources := &sourceRecorder{}
	pool, err := NewCleanupPool(store, cleaner, sources, defaultPoolConfig())
	if err != nil {
		t.Fatal(err)
	}
	pool.WithLogger(func(string, ...any) {})
	pool.process(context.Background(), QueuedDocument{UserID: "user-1", Document: *deleting})
	_, dropped := cleaner.snapshot()
	if len(dropped) != 1 || dropped[0].documentID != document.ID {
		t.Fatalf("document drops = %+v, want every row of the document removed", dropped)
	}
	if len(sources.documents) != 1 || sources.documents[0].documentID != document.ID {
		t.Fatalf("source drops = %+v, want the stored bytes removed with the document", sources.documents)
	}
	if _, err := store.GetDocument(context.Background(), "user-1", document.ID); err == nil {
		t.Fatal("a cleaned document is still readable")
	}
}

func TestFailureAndBackoffHelpersMatchTheSource(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"canceled", context.Canceled, "indexing canceled"},
		{"deadline", context.DeadlineExceeded, "indexing timed out"},
		{"wrapped deadline", fmt.Errorf("embed: %w", context.DeadlineExceeded), "indexing timed out"},
		{"anything else", errors.New("redis: MOFN"), "indexing operation failed"},
	}
	for _, testCase := range cases {
		if got := IndexFailureReason(testCase.err); got != testCase.want {
			t.Errorf("IndexFailureReason(%v) = %q, want %q", testCase.err, got, testCase.want)
		}
	}
	// v2 doubles to eight attempts and then stops: 1, 2, 4, 8, 16, 32, 64, 128 seconds.
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
		16 * time.Second, 32 * time.Second, 64 * time.Second, 128 * time.Second}
	for attempt, expected := range want {
		if got := CleanupBackoff(attempt + 1); got != expected {
			t.Errorf("CleanupBackoff(%d) = %v, want %v", attempt+1, got, expected)
		}
	}
	if got := CleanupBackoff(99); got != 128*time.Second {
		t.Errorf("CleanupBackoff(99) = %v, want the schedule to stay capped at 128s", got)
	}
	if got := CleanupBackoff(0); got != time.Second {
		t.Errorf("CleanupBackoff(0) = %v, want the first step", got)
	}
}
