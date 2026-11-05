package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"GopherAI/common/observability"
	"GopherAI/model"
)

// The segment worker turns one queued job into one summary segment and the
// board that goes with it.
//
// The work is in the table, not in the process. A job that was interrupted is
// reclaimed by a lease timeout and picked up again, so a restart does not lose
// a segment and does not start one over from a different point.
//
// Three properties are what make the retry safe:
//
//   - The batch of turns a retry covers is the same batch the failed attempt
//     covered. The end of the batch is frozen when the batch starts, so a
//     target that moved forward while the attempt was retried does not widen
//     the batch, and a failed segment never lands, so the start does not move
//     either.
//   - One session has one job. The row is unique per session and the lease
//     makes it exclusive, so two workers cannot summarize the same stretch.
//   - The board moves with the segment or not at all. The segment and the
//     board are written together, so a board never claims coverage that no
//     segment records.

// SummarySegmentJobRepository is the queue the worker drives.
//
// It is an interface so the worker's decisions can be tested without a
// database, and so the worker does not depend on the session repository's
// shape.
type SummarySegmentJobRepository interface {
	// ListDueSummarySegmentJobs returns the jobs that are ready to run.
	ListDueSummarySegmentJobs(ctx context.Context, limit, maxAttempts int, now time.Time) ([]model.SessionSummarySegmentJob, error)
	// ClaimSummarySegmentJob takes one job for one worker.
	ClaimSummarySegmentJob(ctx context.Context, userID int64, jobID string, now time.Time, lease time.Duration, maxAttempts int) (*model.SessionSummarySegmentJob, error)
	// CompleteSummarySegmentJob records that the job produced its segment.
	// coveredThrough is the newest turn the segment covers, which is not
	// always the end of the job's target.
	CompleteSummarySegmentJob(ctx context.Context, claimed *model.SessionSummarySegmentJob, coveredThrough uint, now time.Time) error
	// RetrySummarySegmentJob puts a failed job back in the queue.
	RetrySummarySegmentJob(ctx context.Context, claimed *model.SessionSummarySegmentJob, errorCode string, availableAt time.Time, maxAttempts int) error
	// RecoverExpiredSummarySegmentJobs requeues the jobs whose worker died.
	RecoverExpiredSummarySegmentJobs(ctx context.Context, now time.Time, maxAttempts int) (int64, error)
}

// SummarySegmentProducer produces one segment and the board it implies.
type SummarySegmentProducer interface {
	ProduceSegment(ctx context.Context, job *model.SessionSummarySegmentJob) (SummarySegmentOutcome, error)
}

// SummarySegmentOutcome is what one attempt produced.
type SummarySegmentOutcome struct {
	// SegmentSeq is the sequence number the segment took.
	SegmentSeq int
	// StartMessageID is the oldest message the segment covers.
	StartMessageID uint
	// EndMessageID is the newest message the segment covers.
	EndMessageID uint
	// WhiteboardVersion is the board version after the write.
	WhiteboardVersion uint64
	// WhiteboardChanges is how many board entries the segment changed.
	WhiteboardChanges int
	// NothingToCover says the batch held no turns to summarize.
	//
	// Nothing was written in that case, and the batch is accounted for: the
	// job moves past it instead of reading it again.
	NothingToCover bool
}

// ErrSummarySegmentWorkerAlreadyStarted is what a second Start returns.
var ErrSummarySegmentWorkerAlreadyStarted = errors.New("summary segment worker already started")

// SummarySegmentWorkerConfig is the worker's timing and limits.
type SummarySegmentWorkerConfig struct {
	// WorkerCount is how many jobs are processed at the same time.
	WorkerCount int
	// QueueSize is how many listed jobs wait to be claimed.
	QueueSize int
	// ScanInterval is how often the worker looks for due jobs.
	ScanInterval time.Duration
	// ReconcileInterval is how often the worker repairs a missed enqueue.
	ReconcileInterval time.Duration
	// RecoveryInterval is how often expired leases are reclaimed while the
	// worker runs. Recovery at Start covers a process that died; this covers a
	// job this process stranded.
	RecoveryInterval time.Duration
	// TaskTimeout is how long one attempt may take.
	TaskTimeout time.Duration
	// Lease is how long a claim is held. It has to outlast the task timeout,
	// or a slow attempt would lose its job to a second worker.
	Lease time.Duration
	// RetryDelay is how long a failed job waits before it is tried again.
	RetryDelay time.Duration
	// MaxAttempts is how many times a job is tried before it is failed.
	MaxAttempts int
}

// DefaultSummarySegmentWorkerConfig returns the worker's defaults.
//
// The numbers are the same ones the episode worker uses. They are not
// arbitrary: the lease outlasts the task timeout, recovery runs more often
// than a lease lasts, and the retry delay is long enough that a failing
// provider is not hammered.
func DefaultSummarySegmentWorkerConfig() SummarySegmentWorkerConfig {
	return SummarySegmentWorkerConfig{
		WorkerCount:       1,
		QueueSize:         32,
		ScanInterval:      3 * time.Second,
		ReconcileInterval: time.Minute,
		RecoveryInterval:  30 * time.Second,
		TaskTimeout:       30 * time.Second,
		Lease:             time.Minute,
		RetryDelay:        15 * time.Second,
		MaxAttempts:       3,
	}
}

// Validate checks the configuration.
func (c SummarySegmentWorkerConfig) Validate() error {
	if c.WorkerCount < 1 || c.WorkerCount > 16 ||
		c.QueueSize < c.WorkerCount || c.QueueSize > 1000 ||
		c.ScanInterval <= 0 || c.ReconcileInterval <= 0 || c.RecoveryInterval <= 0 ||
		c.TaskTimeout <= 0 || c.Lease <= c.TaskTimeout || c.RetryDelay <= 0 ||
		c.MaxAttempts < 1 || c.MaxAttempts > 10 {
		return fmt.Errorf("invalid summary segment worker config")
	}
	// Recovery has to run more often than a lease lasts, or a job stranded by
	// this process waits most of a lease before anything looks at it.
	if c.RecoveryInterval >= c.Lease {
		return fmt.Errorf("invalid summary segment worker config")
	}
	return nil
}

// SummarySegmentWorker processes the summary segment queue.
type SummarySegmentWorker struct {
	repository SummarySegmentJobRepository
	producer   SummarySegmentProducer
	reconciler *SummarySegmentReconciler
	config     SummarySegmentWorkerConfig
	queue      chan model.SessionSummarySegmentJob
	now        func() time.Time
	logf       func(string, ...any)
	mu         sync.Mutex
	started    bool
	cancel     context.CancelFunc
	done       chan struct{}
}

// NewSummarySegmentWorker builds a worker.
func NewSummarySegmentWorker(
	repository SummarySegmentJobRepository,
	producer SummarySegmentProducer,
	reconciler *SummarySegmentReconciler,
	config SummarySegmentWorkerConfig,
) (*SummarySegmentWorker, error) {
	if repository == nil || producer == nil || reconciler == nil {
		return nil, fmt.Errorf("summary segment worker dependencies are required")
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &SummarySegmentWorker{
		repository: repository,
		producer:   producer,
		reconciler: reconciler,
		config:     config,
		queue:      make(chan model.SessionSummarySegmentJob, config.QueueSize),
		now:        time.Now,
		logf:       log.Printf,
	}, nil
}

// Start begins the work.
//
// The recovery and the reconciliation run before the workers start, so a
// process that comes up after a crash reclaims what the dead one held and
// repairs a session whose enqueue was missed.
func (w *SummarySegmentWorker) Start(parent context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.started {
		return ErrSummarySegmentWorkerAlreadyStarted
	}
	if _, err := w.repository.RecoverExpiredSummarySegmentJobs(parent, w.now(), w.config.MaxAttempts); err != nil {
		return fmt.Errorf("recover summary segment jobs: %w", err)
	}
	if _, err := w.reconciler.RunOnce(parent); err != nil {
		return fmt.Errorf("reconcile summary segment jobs: %w", err)
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

// Stop ends the work and waits for the workers to finish.
func (w *SummarySegmentWorker) Stop(ctx context.Context) error {
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

// scan looks for due jobs and queues them.
func (w *SummarySegmentWorker) scan(ctx context.Context) {
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

func (w *SummarySegmentWorker) scanOnce(ctx context.Context) {
	jobs, err := w.repository.ListDueSummarySegmentJobs(ctx, w.config.QueueSize, w.config.MaxAttempts, w.now())
	if err != nil {
		if ctx.Err() == nil {
			w.logf("summary segment worker scan failed: error_type=%T", err)
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

// reconcile repairs the sessions whose enqueue was missed.
func (w *SummarySegmentWorker) reconcile(ctx context.Context) {
	ticker := time.NewTicker(w.config.ReconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := w.reconciler.RunOnce(ctx); err != nil && ctx.Err() == nil {
				w.logf("summary segment reconciliation failed: error_type=%T", err)
			}
		}
	}
}

// recover reclaims the jobs whose lease expired while this worker was running.
//
// Start covers a process that died. It does not cover a job this process
// stranded: a completion write can be rolled back, leaving the row in
// processing with a lease nobody will release. Without a periodic pass that
// job waits for the next restart, and the session it belongs to silently
// stops being summarized.
func (w *SummarySegmentWorker) recover(ctx context.Context) {
	ticker := time.NewTicker(w.config.RecoveryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			recovered, err := w.repository.RecoverExpiredSummarySegmentJobs(ctx, w.now(), w.config.MaxAttempts)
			if err != nil {
				if ctx.Err() == nil {
					w.logf("summary segment job recovery failed: error_type=%T", err)
				}
				continue
			}
			if recovered > 0 {
				w.logf("summary segment job recovery reclaimed %d expired job(s)", recovered)
			}
		}
	}
}

// run takes jobs off the queue.
func (w *SummarySegmentWorker) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case listed := <-w.queue:
			w.process(ctx, listed)
		}
	}
}

// process runs one attempt.
func (w *SummarySegmentWorker) process(ctx context.Context, listed model.SessionSummarySegmentJob) {
	claimed, err := w.repository.ClaimSummarySegmentJob(ctx, listed.UserID, listed.ID, w.now(), w.config.Lease, w.config.MaxAttempts)
	if err != nil {
		// The job was taken by somebody else, or it is no longer claimable.
		// Either way it is not this worker's to do.
		return
	}
	started := w.now()
	taskCtx, cancel := context.WithTimeout(ctx, w.config.TaskTimeout)
	outcome, produceErr := w.producer.ProduceSegment(taskCtx, claimed)
	cancel()

	if produceErr == nil {
		if outcome.NothingToCover {
			observability.Memory.SummarySegmentJob("empty", "", w.now().Sub(started))
			w.logf("summary segment batch held no turns: job=%s batch=%d..%d",
				claimed.ID, outcome.StartMessageID, outcome.EndMessageID)
		} else {
			observability.Memory.SummarySegmentJob("completed", "", w.now().Sub(started))
			w.logf("summary segment written: job=%s seq=%d covered=%d..%d changes=%d",
				claimed.ID, outcome.SegmentSeq, outcome.StartMessageID, outcome.EndMessageID, outcome.WhiteboardChanges)
		}
	} else {
		observability.Memory.SummarySegmentJob("failed", summarySegmentErrorCode(produceErr), w.now().Sub(started))
	}

	persistCtx, cancelPersist := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancelPersist()
	if produceErr == nil {
		// The job is finished with what the segment actually covered, not
		// with what the job asked for. A segment that covers fewer turns than
		// the target asked for leaves the rest of the turns owed, and the job
		// has to stay open for them.
		if err := w.repository.CompleteSummarySegmentJob(persistCtx, claimed, outcome.EndMessageID, w.now()); err != nil && ctx.Err() == nil {
			w.logf("summary segment worker completion failed: job=%s error_type=%T", claimed.ID, err)
		}
		return
	}
	// The retry covers the same stretch of turns as the attempt that failed.
	// The failed segment never landed, so the start did not move, and the end
	// of the batch is still frozen where the failed attempt found it.
	if err := w.repository.RetrySummarySegmentJob(persistCtx, claimed, summarySegmentErrorCode(produceErr),
		w.now().Add(w.config.RetryDelay), w.config.MaxAttempts); err != nil && ctx.Err() == nil {
		w.logf("summary segment worker retry failed: job=%s error_type=%T", claimed.ID, err)
	}
}

// summarySegmentErrorCode names why an attempt failed.
//
// The code is bounded on purpose: it goes into the job row and into a metric
// label, so an unnamed failure has to land in a fixed set rather than become
// free text.
func summarySegmentErrorCode(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "task_timeout"
	case errors.Is(err, context.Canceled):
		return "task_cancelled"
	case errors.Is(err, ErrSummarySegmentWhiteboardConflict):
		return "whiteboard_conflict"
	case errors.Is(err, ErrSummarySegmentMessagesUnavailable):
		return "source_not_found"
	case errors.Is(err, ErrSummarySegmentOutputInvalid):
		return "invalid_model_output"
	default:
		return "generation_failed"
	}
}
