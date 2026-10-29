package knowledge

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/domain"
)

// This file is the migration of v2's knowledge index and cleanup drivers
// (service/knowledge/worker.go, service/knowledge/cleanup_worker.go). v2 runs them as
// two pools over a RabbitMQ-free in-process queue: a scanner turns the persisted queue
// into work items, a fixed number of workers take one claim each, and every way an
// attempt can end leaves a specific state behind. The states and the failure semantics
// are the part being migrated; the pool shape is just how v2 reaches them.
//
// Nothing here is wired into the running binary: constructing and starting a pool
// belongs to the assembly, which is window A's file. The synchronous path already
// drives claim -> index -> complete -> cleanup inside a request, so a pool is additive
// - and safe to run alongside it, because ClaimForIndexing is compare-and-set, so a
// queued pass and an inline pass cannot both index the same version.

// requeueTimeout is v2's bound for the write-back that follows an abandoned pass
// (worker.go:204). Five seconds, because that write must not be bounded by the context
// that just died.
const requeueTimeout = 5 * time.Second

// cleanupLease is how long a taken cleanup item is left alone (v2's
// ClaimCleanup(..., now, now.Add(time.Minute))). A worker that dies mid-cleanup
// therefore costs one minute, not the document.
const cleanupLease = time.Minute

// CompensationContext detaches a write from the caller that is going away and bounds it
// on its own clock. v2 does this for the one write that must survive a cancellation
// (worker.go:204: a fresh context with five seconds); everything else may legitimately die
// with the request.
//
// The two halves are both load-bearing. On the caller's context the write would be
// refused outright and the row would stay in whatever intermediate state it was left in;
// with no deadline of its own a stuck datastore would keep the process alive after the
// request that provoked it was already gone.
func CompensationContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), requeueTimeout)
}

// IndexFailureReason is the bounded, non-echoing reason recorded with a failed pass -
// v2's SafeIndexError (ingestor.go:121-147). An embedder's own error can carry request
// details, a model name or a credential-bearing URL, and none of that belongs in a row
// a client can read back. v2's staged chunk reasons have no counterpart here because
// chunking happens before staging in this codebase: by the time an index pass runs, the
// chunks it embeds are already stored.
func IndexFailureReason(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.Canceled):
		return "indexing canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "indexing timed out"
	default:
		return "indexing operation failed"
	}
}

// CleanupBackoff is v2's cleanup retry schedule (cleanup_worker.go:178-186): one second
// doubling per attempt, capped at eight attempts, so the longest wait is 128 seconds.
// The cap is the point - an unfixed index must not be hammered, and a schedule that
// stopped at eight seconds would.
func CleanupBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 8 {
		attempt = 8
	}
	return time.Duration(1<<uint(attempt-1)) * time.Second
}

// CleanupFailureMessage phrases a recorded cleanup failure the way v2 does
// ("cleanup: vector delete failed"). The stage names what was being removed, so a
// reader can tell leftover search rows from leftover source bytes.
func CleanupFailureMessage(stage string) string {
	return "cleanup: " + stage + " delete failed"
}

// ResolveAbandonedIndexing decides what an unfinished indexing pass leaves behind, and
// it is the same decision whether the pass ran inline in a request or out of a queue:
//
//   - the caller went away, or the pass reported a cancellation while its caller was
//     still waiting: the work was abandoned, not failed, so it returns to pending with
//     no reason recorded;
//   - anything else, including a task that outlived its own budget while the caller
//     waited: a failure, recorded as failed with a bounded reason, leaving whatever
//     version was answering still answering.
//
// v2's condition is worker.go:203 - ctx.Err() != nil || errors.Is(err, context.Canceled)
// - and both halves matter: the second one catches an embedder that gave up on its own
// internal context while the request was still alive.
//
// The write itself runs on a fresh bounded context. Completing or re-queuing with the
// context that just died fails the same way the index did, and the row would stay
// claimed with nobody left to claim it - which is worse than the failure being reported.
func ResolveAbandonedIndexing(ctx context.Context, lifecycle Lifecycle, userID, documentID string,
	version int, indexErr error) error {
	if lifecycle == nil {
		return errors.New("knowledge: an abandoned index pass needs a lifecycle to record it on")
	}
	requeue := ctx.Err() != nil || errors.Is(indexErr, context.Canceled)
	bounded, cancel := CompensationContext(ctx)
	defer cancel()
	if requeue {
		return errors.Join(indexErr, lifecycle.RequeueIndexing(bounded, userID, documentID, version))
	}
	return errors.Join(indexErr, lifecycle.CompleteIndexing(bounded, userID, documentID, version, IndexOutcome{
		Status: domain.DocumentStatusFailed, ErrorMessage: IndexFailureReason(indexErr),
	}))
}

// WorkerConfig is v2's WorkerConfig with the same validation rules: a pool of no
// workers scans without ever working, and a queue smaller than the worker count would
// park a scanner against a channel nobody can drain.
type WorkerConfig struct {
	// Workers is how many documents are indexed or cleaned at the same time.
	Workers int
	// QueueSize bounds both the in-flight channel and how many items one scan takes.
	QueueSize int
	// ScanInterval is how often the persisted queue is re-read.
	ScanInterval time.Duration
	// TaskTimeout bounds one indexing attempt, apart from the caller's deadline. Zero
	// means an attempt inherits its caller, which is the behaviour the synchronous
	// path has always had.
	TaskTimeout time.Duration
}

func (c WorkerConfig) Validate() error {
	if c.Workers <= 0 {
		return errors.New("knowledge: worker count must be greater than zero")
	}
	if c.QueueSize < c.Workers {
		return errors.New("knowledge: queue size must be at least the worker count")
	}
	if c.ScanInterval <= 0 {
		return errors.New("knowledge: scan interval must be greater than zero")
	}
	return nil
}

// IndexRunner performs the derived-index work for one claimed version and reports how
// many rows it wrote. The implementer is whatever owns the index - in this codebase the
// redisindex decorator - because only it knows how a chunk becomes a search row.
type IndexRunner interface {
	IndexVersion(ctx context.Context, userID, documentID string, version int) (int, error)
}

// RowCleaner removes derived rows again. Its vocabulary is the index's own, not v2's:
// the target's index deletes "everything except this version" in one sweep
// (retrieval.VectorWriter.DeleteVersions) rather than one version at a time, so a
// superseded version is dropped by keeping the one that is answering. The two are the
// same operation for one document, and the sweep is the narrower window - v2's loop over
// versions 1..Version (cleanup_worker.go:163-170) leaves a document with some of its
// rows gone and the rest still searchable if the process dies halfway.
type RowCleaner interface {
	SweepExcept(ctx context.Context, userID, documentID string, keep int) error
	DropDocument(ctx context.Context, userID, documentID string) error
}

// poolState is the start/stop bookkeeping the two pools share. It is the reason a
// second Start is refused rather than silently spawning a second set of goroutines
// scanning the same queue, and why everything that has to happen exactly once before
// the workers exist (startup recovery) happens under the same lock that decides whether
// this call is the one that starts the pool.
type poolState struct {
	mu      sync.Mutex
	started bool
	cancel  context.CancelFunc
	done    chan struct{}
}

// start runs prepare, then spins up count workers plus one scanner, the way v2 does.
// A prepare failure leaves the pool unstarted, so the caller can fix whatever the
// datastore refused and try again.
func (st *poolState) start(parent context.Context, count int, interval time.Duration,
	work, scan func(context.Context), prepare func(context.Context) error) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.started {
		return errPoolStarted
	}
	runCtx := parent
	if runCtx == nil {
		runCtx = context.Background()
	}
	if prepare != nil {
		if err := prepare(runCtx); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithCancel(runCtx)
	var group sync.WaitGroup
	group.Add(count + 1)
	for index := 0; index < count; index++ {
		go func() {
			defer group.Done()
			work(ctx)
		}()
	}
	go func() {
		defer group.Done()
		scanUntilStopped(ctx, interval, scan)
	}()
	st.cancel, st.done, st.started = cancel, make(chan struct{}), true
	go func() {
		group.Wait()
		close(st.done)
	}()
	return nil
}

// Stop cancels and waits, with the caller's context as the bound: a driver that will
// not drain is reported as a shutdown failure rather than hidden behind a clean return.
// A pool that has stopped stays stopped - v2's rule (worker.go:112-127) - so a second
// Stop is a no-op and Start after Stop is refused instead of silently doubling the
// goroutines that work the same queue.
func (st *poolState) Stop(ctx context.Context) error {
	st.mu.Lock()
	if !st.started {
		st.mu.Unlock()
		return nil
	}
	cancel, done := st.cancel, st.done
	st.mu.Unlock()
	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// errPoolStarted is v2's ErrWorkerAlreadyStarted: starting the same pool twice would
// mean two scanners and two worker sets competing, which the claims survive but nobody
// would be able to explain from the logs.
var errPoolStarted = errors.New("knowledge: worker pool already started")

// IndexPool works the pending queue: it recovers interrupted passes first, then claims,
// indexes and completes one version at a time, v2's process function step for step.
type IndexPool struct {
	lifecycle Lifecycle
	runner    IndexRunner
	config    WorkerConfig
	queue     chan QueuedDocument
	logf      func(string, ...any)
	state     poolState
}

func NewIndexPool(lifecycle Lifecycle, runner IndexRunner, config WorkerConfig) (*IndexPool, error) {
	if lifecycle == nil || runner == nil {
		return nil, errors.New("knowledge: an index pool needs a lifecycle and a runner")
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &IndexPool{
		lifecycle: lifecycle, runner: runner, config: config,
		queue: make(chan QueuedDocument, config.QueueSize), logf: log.Printf,
	}, nil
}

// WithLogger replaces the pool's log sink. Only counts, ids and enumerated statuses
// reach it - never document text, vectors, keys or hashes.
func (p *IndexPool) WithLogger(logf func(string, ...any)) *IndexPool {
	if logf != nil {
		p.logf = logf
	}
	return p
}

// Start recovers interrupted indexing before any goroutine runs, and a recovery failure
// is a start failure: v2 returns the error and leaves the pool unstarted, because a
// process that cannot tell which passes were left mid-flight must not present itself as
// one that will finish them.
func (p *IndexPool) Start(parent context.Context) error {
	return p.state.start(parent, p.config.Workers, p.config.ScanInterval, p.runWorker, p.scan, p.recoverInterrupted)
}

func (p *IndexPool) recoverInterrupted(ctx context.Context) error {
	recovered, err := p.lifecycle.RecoverInterruptedIndexing(ctx)
	if err != nil {
		return fmt.Errorf("knowledge: recover interrupted indexing: %w", err)
	}
	if recovered > 0 {
		p.logf("knowledge index pool: recovered %d interrupted passes", recovered)
	}
	return nil
}

func (p *IndexPool) Stop(ctx context.Context) error { return p.state.Stop(ctx) }

func (p *IndexPool) runWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case item := <-p.queue:
			if ctx.Err() != nil {
				// Never claimed, so it is still pending for whoever starts next.
				return
			}
			p.process(ctx, item)
		}
	}
}

// scan drains one page of the persisted queue into the in-flight channel. A scan error
// is logged and skipped, not fatal: the queue is re-read on the next tick, and a
// document nobody can index now is a retry, not a crash.
func (p *IndexPool) scan(ctx context.Context) {
	documents, err := p.lifecycle.ListPending(ctx, p.config.QueueSize)
	if err != nil {
		if ctx.Err() == nil {
			p.logf("knowledge index pool: scanning the pending queue failed")
		}
		return
	}
	for _, item := range documents {
		select {
		case <-ctx.Done():
			return
		case p.queue <- item:
		}
	}
}

// process is the whole failure semantics of the driver in twelve lines, which is why
// each branch is spelled out rather than folded into a helper the reader has to chase.
func (p *IndexPool) process(ctx context.Context, item QueuedDocument) {
	document := item.Document
	claimed, err := p.lifecycle.ClaimForIndexing(ctx, item.UserID, document.ID, document.Version)
	if err != nil {
		if ctx.Err() == nil {
			p.logf("knowledge index pool: claiming %s failed", document.ID)
		}
		return
	}
	if !claimed {
		// Somebody else holds it - an inline publish, or another worker.
		return
	}
	taskCtx, cancelTask := ctx, func() {}
	if p.config.TaskTimeout > 0 {
		taskCtx, cancelTask = context.WithTimeout(ctx, p.config.TaskTimeout)
	}
	count, indexErr := p.runner.IndexVersion(taskCtx, item.UserID, document.ID, document.Version)
	cancelTask()
	if indexErr == nil {
		// The completion is written with the pool's context, not the task's: a pass
		// that finished must not lose its success because its own budget expired.
		if err := p.lifecycle.CompleteIndexing(ctx, item.UserID, document.ID, document.Version, IndexOutcome{
			Status: domain.DocumentStatusReady, ChunkCount: count,
		}); err != nil {
			if ctx.Err() == nil {
				p.logf("knowledge index pool: completing %s failed", document.ID)
			}
			return
		}
		p.logf("knowledge index pool: document %s version %d indexed count=%d", document.ID, document.Version, count)
		return
	}
	if err := ResolveAbandonedIndexing(ctx, p.lifecycle, item.UserID, document.ID, document.Version, indexErr); err != nil &&
		ctx.Err() == nil {
		p.logf("knowledge index pool: recording the failed pass for %s failed", document.ID)
	}
}

// CleanupPool works the recorded cleanup: superseded versions and the rows of documents
// already marked deleting. It is a separate pool because the two kinds of work fail for
// different reasons and retry on different schedules.
type CleanupPool struct {
	lifecycle Lifecycle
	cleaner   RowCleaner
	sources   SourceCleaner
	config    WorkerConfig
	queue     chan QueuedDocument
	logf      func(string, ...any)
	state     poolState
}

// SourceCleaner is the second half of v2's cleanup: after the derived rows go, the
// stored source bytes for the superseded version or the whole document go with them.
// It is optional - a deployment that never stores source bytes has nothing to drop -
// and nil is reported as "no source storage", never as a successful removal.
type SourceCleaner interface {
	DropVersion(ctx context.Context, userID, documentID string, version int) error
	DropDocument(ctx context.Context, userID, documentID string) error
}

func NewCleanupPool(lifecycle Lifecycle, cleaner RowCleaner, sources SourceCleaner, config WorkerConfig) (*CleanupPool, error) {
	if lifecycle == nil || cleaner == nil {
		return nil, errors.New("knowledge: a cleanup pool needs a lifecycle and a row cleaner")
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &CleanupPool{
		lifecycle: lifecycle, cleaner: cleaner, sources: sources, config: config,
		queue: make(chan QueuedDocument, config.QueueSize), logf: log.Printf,
	}, nil
}

// WithLogger replaces the pool's log sink.
func (p *CleanupPool) WithLogger(logf func(string, ...any)) *CleanupPool {
	if logf != nil {
		p.logf = logf
	}
	return p
}

// Start brings the cleanup pool up. It does not recover anything: an interrupted
// cleanup is already recorded with a lease, and the lease running out is the recovery.
func (p *CleanupPool) Start(parent context.Context) error {
	return p.state.start(parent, p.config.Workers, p.config.ScanInterval, p.runWorker, p.scan, nil)
}

func (p *CleanupPool) Stop(ctx context.Context) error { return p.state.Stop(ctx) }

func (p *CleanupPool) runWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case item := <-p.queue:
			if ctx.Err() != nil {
				return
			}
			p.process(ctx, item)
		}
	}
}

func (p *CleanupPool) scan(ctx context.Context) {
	documents, err := p.lifecycle.ListDueCleanup(ctx, time.Now().UTC(), p.config.QueueSize)
	if err != nil {
		if ctx.Err() == nil {
			p.logf("knowledge cleanup pool: scanning the cleanup queue failed")
		}
		return
	}
	for _, item := range documents {
		select {
		case <-ctx.Done():
			return
		case p.queue <- item:
		}
	}
}

// process takes the lease, removes the rows, then removes the source bytes, then closes
// the record. A failure at either removal is persisted with its next attempt; the
// cancellation path returns without recording anything, because the lease is what makes
// an interrupted cleanup come back.
func (p *CleanupPool) process(ctx context.Context, item QueuedDocument) {
	document := item.Document
	now := time.Now().UTC()
	claimed, err := p.lifecycle.ClaimCleanup(ctx, item.UserID, document.ID, now, now.Add(cleanupLease))
	if err != nil || !claimed {
		return
	}
	stage := "vector"
	err = p.dropRows(ctx, item)
	if err == nil && p.sources != nil {
		stage = "source"
		err = p.dropSources(ctx, item)
	}
	if err == nil {
		if completeErr := p.lifecycle.CompleteCleanup(ctx, item.UserID, document.ID); completeErr != nil && ctx.Err() == nil {
			p.logf("knowledge cleanup pool: completing %s failed", document.ID)
		}
		return
	}
	if ctx.Err() != nil {
		return
	}
	message := CleanupFailureMessage(stage)
	retryAt := now.Add(CleanupBackoff(document.CleanupAttempts + 1))
	if failErr := p.lifecycle.FailCleanup(ctx, item.UserID, document.ID, message, retryAt); failErr != nil {
		p.logf("knowledge cleanup pool: recording the failure of %s failed", document.ID)
	}
}

// dropRows removes the derived rows for one item. A cleanup_pending record is swept with
// the answering version kept; a deleting one has no version to keep, so everything the
// document ever had goes.
func (p *CleanupPool) dropRows(ctx context.Context, item QueuedDocument) error {
	document := item.Document
	if document.IndexStatus == domain.DocumentStatusCleanupPending {
		if document.CleanupVersion <= 0 {
			return nil
		}
		return p.cleaner.SweepExcept(ctx, item.UserID, document.ID, document.ServingVersion())
	}
	return p.cleaner.DropDocument(ctx, item.UserID, document.ID)
}

// dropSources applies the same split to the stored source bytes. A cleanup_pending
// record with no version named has nothing to drop: the bytes of the version that is
// still answering must stay, or the document loses the text it is quoting.
func (p *CleanupPool) dropSources(ctx context.Context, item QueuedDocument) error {
	document := item.Document
	if document.IndexStatus == domain.DocumentStatusCleanupPending {
		if document.CleanupVersion <= 0 {
			return nil
		}
		return p.sources.DropVersion(ctx, item.UserID, document.ID, document.CleanupVersion)
	}
	return p.sources.DropDocument(ctx, item.UserID, document.ID)
}

// scanUntilStopped runs one scan immediately, then on every tick - v2's scanner, which
// has to pick up work left by the previous process before it starts waiting.
func scanUntilStopped(ctx context.Context, interval time.Duration, scan func(context.Context)) {
	scan(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			scan(ctx)
		}
	}
}
