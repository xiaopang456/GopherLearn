package service

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/repository"
)

// The projection worker: the production executor for episodic memory. Four
// loops run under one Start - workers draining the claim queue, a scan loop
// feeding it from due job rows, a reconcile loop repairing missed publishes,
// and a recover loop reclaiming leases nobody will release. Everything is
// durable and idempotent: a repeated scan, a restarted process, a crashed
// worker and a re-delivered publish each produce one projection, never two.

var errEpisodeWorkerAlreadyStarted = errors.New("episode worker already started")

// EpisodeJobRepository is the durable job seam the worker claims through.
type EpisodeJobRepository interface {
	ListDueEpisodeJobs(ctx context.Context, limit, maxAttempts int, now time.Time) ([]domain.EpisodeProjectionJob, error)
	ClaimEpisodeJob(ctx context.Context, userID, jobID string, now time.Time, lease time.Duration, maxAttempts int) (*domain.EpisodeProjectionJob, error)
	CompleteOrRequeueEpisodeJob(ctx context.Context, claimed *domain.EpisodeProjectionJob, coveredMessageID string, now time.Time) (*domain.EpisodeProjectionJob, error)
	RetryEpisodeJob(ctx context.Context, claimed *domain.EpisodeProjectionJob, errorCode string, availableAt time.Time, maxAttempts int) (*domain.EpisodeProjectionJob, error)
	RecoverExpiredEpisodeJobs(ctx context.Context, now time.Time, maxAttempts int) (int64, error)
	UpsertEpisodeDesired(ctx context.Context, userID, sessionID, desiredMessageID string, availableAt time.Time) (*domain.EpisodeProjectionJob, error)
	ReleaseOtherEpisodeSessions(ctx context.Context, userID, currentSessionID string, now time.Time) (int64, error)
}

// EpisodeProjectionRunner is one projection pass.
type EpisodeProjectionRunner interface {
	Project(ctx context.Context, userID, sessionID, targetMessageID string) (*domain.Episode, error)
}

// EpisodeProjectionIndexWriter is the derived-view writer the worker offers
// each projected card to.
type EpisodeProjectionIndexWriter interface {
	Index(ctx context.Context, episode *domain.Episode) error
}

// EpisodeReconcileRepository repairs missed publishes.
type EpisodeReconcileRepository interface {
	ListEpisodeProjectionGaps(ctx context.Context, limit int) ([]repository.EpisodeProjectionGap, error)
	UpsertEpisodeDesired(ctx context.Context, userID, sessionID, desiredMessageID string, availableAt time.Time) (*domain.EpisodeProjectionJob, error)
}

// EpisodeWorkerConfig bounds the worker's loops.
type EpisodeWorkerConfig struct {
	WorkerCount       int
	QueueSize         int
	ScanInterval      time.Duration
	ReconcileInterval time.Duration
	TaskTimeout       time.Duration
	Lease             time.Duration
	// RecoveryInterval is how often expired processing jobs are reclaimed while
	// the worker is running. Recovery used to happen only at start, so a job
	// whose completion write was rolled back stayed in processing until the
	// process was restarted.
	RecoveryInterval time.Duration
	RetryDelay       time.Duration
	MaxAttempts      int
}

// EpisodeWorker is the background executor.
type EpisodeWorker struct {
	repository EpisodeJobRepository
	projector  EpisodeProjectionRunner
	reconciler *EpisodeReconciler
	config     EpisodeWorkerConfig
	queue      chan domain.EpisodeProjectionJob
	now        func() time.Time
	logf       func(string, ...any)
	mu         sync.Mutex
	started    bool
	cancel     context.CancelFunc
	done       chan struct{}
	indexer    EpisodeProjectionIndexWriter
}

// NewEpisodeWorker wires the worker and validates its configuration.
func NewEpisodeWorker(repo EpisodeJobRepository, projector EpisodeProjectionRunner, reconciler *EpisodeReconciler, config EpisodeWorkerConfig) (*EpisodeWorker, error) {
	if repo == nil || projector == nil || reconciler == nil {
		return nil, fmt.Errorf("episode worker dependencies are required")
	}
	if config.WorkerCount < 1 || config.WorkerCount > 16 || config.QueueSize < config.WorkerCount || config.QueueSize > 1000 ||
		config.ScanInterval <= 0 || config.ReconcileInterval <= 0 || config.TaskTimeout <= 0 ||
		config.Lease <= config.TaskTimeout || config.RecoveryInterval <= 0 ||
		config.RetryDelay <= 0 || config.MaxAttempts < 1 || config.MaxAttempts > 10 {
		return nil, fmt.Errorf("invalid episode worker config")
	}
	return &EpisodeWorker{
		repository: repo, projector: projector, reconciler: reconciler, config: config,
		queue: make(chan domain.EpisodeProjectionJob, config.QueueSize), now: time.Now, logf: logPrintf,
	}, nil
}

// UseIndexer attaches the derived-view writer. Nil means the view is not being
// written; projections continue and rows stay visibly unindexed, which is how a
// deployment without the view still accumulates authority-side memory.
func (w *EpisodeWorker) UseIndexer(indexer EpisodeProjectionIndexWriter) { w.indexer = indexer }

// Start begins the loops. It first recovers leases left behind by a previous
// process and runs one reconciliation pass, so a restart repairs what it missed
// before serving.
func (w *EpisodeWorker) Start(parent context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.started {
		return errEpisodeWorkerAlreadyStarted
	}
	if _, err := w.repository.RecoverExpiredEpisodeJobs(parent, w.now(), w.config.MaxAttempts); err != nil {
		return fmt.Errorf("recover episode jobs: %w", err)
	}
	if _, err := w.reconciler.RunOnce(parent); err != nil {
		return fmt.Errorf("reconcile episode jobs: %w", err)
	}
	ctx, cancel := context.WithCancel(parent)
	w.cancel, w.done, w.started = cancel, make(chan struct{}), true
	var group sync.WaitGroup
	group.Add(w.config.WorkerCount + 3)
	for index := 0; index < w.config.WorkerCount; index++ {
		go func() { defer group.Done(); w.run(ctx) }()
	}
	go func() { defer group.Done(); w.scan(ctx) }()
	go func() { defer group.Done(); w.reconcile(ctx) }()
	go func() { defer group.Done(); w.recover(ctx) }()
	go func() { group.Wait(); close(w.done) }()
	return nil
}

// Stop cancels the loops and waits for them to finish.
func (w *EpisodeWorker) Stop(ctx context.Context) error {
	w.mu.Lock()
	if !w.started {
		w.mu.Unlock()
		return nil
	}
	cancel, done := w.cancel, w.done
	w.mu.Unlock()
	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (w *EpisodeWorker) scan(ctx context.Context) {
	w.scanOnce(ctx)
	ticker := time.NewTicker(w.config.ScanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.scanOnce(ctx)
		}
	}
}

func (w *EpisodeWorker) scanOnce(ctx context.Context) {
	jobs, err := w.repository.ListDueEpisodeJobs(ctx, w.config.QueueSize, w.config.MaxAttempts, w.now())
	if err != nil {
		if ctx.Err() == nil {
			w.logf("episode worker scan failed: error_type=%T", err)
		}
		return
	}
	for _, job := range jobs {
		select {
		case <-ctx.Done():
			return
		case w.queue <- job:
		}
	}
}

func (w *EpisodeWorker) reconcile(ctx context.Context) {
	ticker := time.NewTicker(w.config.ReconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := w.reconciler.RunOnce(ctx); err != nil && ctx.Err() == nil {
				w.logf("episode reconciliation failed: error_type=%T", err)
			}
		}
	}
}

// recover reclaims jobs whose lease expired while this worker is running. Start
// already does one pass, which covers a process that died; it does not cover a
// job stranded by this process - a completion write rolled back by a transient
// error leaves the row in processing with a lease nobody will release, and
// without a periodic pass that session silently stops being projected.
func (w *EpisodeWorker) recover(ctx context.Context) {
	ticker := time.NewTicker(w.config.RecoveryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			recovered, err := w.repository.RecoverExpiredEpisodeJobs(ctx, w.now(), w.config.MaxAttempts)
			if err != nil {
				if ctx.Err() == nil {
					w.logf("episode job recovery failed: error_type=%T", err)
				}
				continue
			}
			if recovered > 0 {
				w.logf("episode job recovery reclaimed %d expired job(s)", recovered)
			}
		}
	}
}

func (w *EpisodeWorker) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case listed := <-w.queue:
			w.process(ctx, listed)
		}
	}
}

// process claims one listed job and drives it to completion. Every failure path
// ends in either CompleteOrRequeue or Retry, both of which release the lease;
// nothing leaves the row in processing with a live claim except a crash, and
// the recover loop exists for exactly that case.
func (w *EpisodeWorker) process(ctx context.Context, listed domain.EpisodeProjectionJob) {
	claimed, err := w.repository.ClaimEpisodeJob(ctx, listed.UserID, listed.ID, w.now(), w.config.Lease, w.config.MaxAttempts)
	if err != nil {
		return
	}
	taskCtx, cancel := context.WithTimeout(ctx, w.config.TaskTimeout)
	episode, projectErr := w.projector.Project(taskCtx, claimed.UserID, claimed.SessionID, claimed.ClaimedMessageID)
	cancel()
	var indexErr error
	if projectErr == nil && w.indexer != nil {
		// The view write is deferred from the task timeout: a slow index must
		// not eat the projection budget. A failed index write is also not a
		// failed projection - the card is durable, the row records the exact
		// version's view state, and a reindex repairs the view later - so the
		// job still completes.
		indexCtx, cancelIndex := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		indexErr = w.indexer.Index(indexCtx, episode)
		if indexErr != nil && ctx.Err() == nil {
			w.logf("episode indexing deferred to reindex: episode=%s error_type=%T", episode.ID, indexErr)
		}
		cancelIndex()
	}
	persistCtx, cancelPersist := episodePersistenceContext(ctx)
	defer cancelPersist()
	if projectErr == nil {
		if _, err := w.repository.CompleteOrRequeueEpisodeJob(persistCtx, claimed, claimed.ClaimedMessageID, w.now()); err != nil && ctx.Err() == nil {
			w.logf("episode worker completion failed: job=%s error_type=%T", claimed.ID, err)
		}
		return
	}
	if _, err := w.repository.RetryEpisodeJob(persistCtx, claimed, projectionErrorCode(projectErr), w.now().Add(w.config.RetryDelay), w.config.MaxAttempts); err != nil && ctx.Err() == nil {
		w.logf("episode worker retry failed: job=%s error_type=%T", claimed.ID, err)
	}
}

// episodePersistenceContext survives the caller's cancellation long enough to
// record the job's outcome: a shutdown that cancels the worker's context must
// not also cancel the write that releases the lease.
func episodePersistenceContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
}

// EpisodeReconciler repairs missed publishes. The publish is one durable write
// after the message write; a crash between the two loses it, and no amount of
// in-process care can close that gap. The gap scan turns it back into work.
type EpisodeReconciler struct {
	repository  EpisodeReconcileRepository
	quietWindow time.Duration
	batchSize   int
	now         func() time.Time
}

// NewEpisodeReconciler builds the repair loop.
func NewEpisodeReconciler(repo EpisodeReconcileRepository, quietWindow time.Duration, batchSize int) (*EpisodeReconciler, error) {
	if repo == nil || quietWindow < 0 || quietWindow > 24*time.Hour || batchSize < 1 || batchSize > 1000 {
		return nil, fmt.Errorf("invalid episode reconciler configuration")
	}
	return &EpisodeReconciler{repository: repo, quietWindow: quietWindow, batchSize: batchSize, now: time.Now}, nil
}

// RunOnce scans once and re-enqueues what it found. It reports how many gaps it
// repaired; a failure to enqueue one gap is logged and skipped, because one
// stuck session must not stop the others.
func (r *EpisodeReconciler) RunOnce(ctx context.Context) (int, error) {
	gaps, err := r.repository.ListEpisodeProjectionGaps(ctx, r.batchSize)
	if err != nil {
		return 0, err
	}
	repaired := 0
	for _, gap := range gaps {
		if _, err := r.repository.UpsertEpisodeDesired(ctx, gap.UserID, gap.SessionID, gap.LatestMessageID, r.now().Add(r.quietWindow)); err != nil {
			if ctx.Err() != nil {
				return repaired, ctx.Err()
			}
			continue
		}
		repaired++
	}
	return repaired, nil
}

// EpisodePublisher is the trigger surface the conversation service calls. It
// only writes durable job rows; it never generates.
type EpisodePublisher struct {
	repository  EpisodeJobRepository
	quietWindow time.Duration
	now         func() time.Time
}

// NewEpisodePublisher validates the debounce window.
func NewEpisodePublisher(repo EpisodeJobRepository, quietWindow time.Duration) (*EpisodePublisher, error) {
	if repo == nil || quietWindow < 0 || quietWindow > 24*time.Hour {
		return nil, fmt.Errorf("invalid episode publisher configuration")
	}
	return &EpisodePublisher{repository: repo, quietWindow: quietWindow, now: time.Now}, nil
}

// PublishConversationCompleted records that one session's conversation moved.
// It is safe to call repeatedly: the job row is unique per session and the
// desired watermark only advances, so a re-delivered publish is debounce, not
// duplication.
//
// Writing here is evidence that every other session of this user is over, so
// their projections no longer wait out the quiet window: a session that ended
// minutes ago should be recallable now, and returning to the older session is
// handled by the next publish raising its desired watermark again. An early
// projection is wasted work at worst, never a wrong answer.
func (p *EpisodePublisher) PublishConversationCompleted(ctx context.Context, userID, sessionID, latestMessageID string) error {
	if p == nil || strings.TrimSpace(userID) == "" || strings.TrimSpace(sessionID) == "" || strings.TrimSpace(latestMessageID) == "" {
		return fmt.Errorf("invalid episode completion event")
	}
	now := p.now()
	if _, err := p.repository.UpsertEpisodeDesired(ctx, userID, sessionID, latestMessageID, now.Add(p.quietWindow)); err != nil {
		return err
	}
	if _, err := p.repository.ReleaseOtherEpisodeSessions(ctx, userID, sessionID, now); err != nil {
		return err
	}
	return nil
}

// logPrintf is the worker's logging seam, kept as a variable so tests can
// silence or capture it.
var logPrintf = log.Printf
