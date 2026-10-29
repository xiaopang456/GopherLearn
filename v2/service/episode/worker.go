package episode

import (
	"GopherAI/common/observability"
	"GopherAI/model"
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"
)

var ErrWorkerAlreadyStarted = errors.New("episode worker already started")

// ErrRelationPass marks a job failure that came from the relation pass rather
// than from the projection.
//
// The distinction is not cosmetic. A projection error means the Episode is
// wrong or could not be built; a relation error means the Episode is fine and
// durable, and a later stage could not record how its assertions relate. They
// are investigated in different places, and a shared error code would send
// every investigation to the wrong one.
var ErrRelationPass = errors.New("episode relation pass failed")

// ErrRelationPrerequisite marks a job failure that happened because the
// relation pass's prerequisite - an indexed, governor-readable Episode - was
// not available: either no indexer is attached at all, or the index write
// failed. It is deliberately not ErrRelationPass: the relation stage never
// ran, so "relation_failed" would claim a classifier or store fault that did
// not happen, and a projection code would point at the one stage that
// succeeded. The underlying index error stays in the local error chain
// (errors.Is/As reachable) but only this closed code reaches the job row.
var ErrRelationPrerequisite = errors.New(
	"episode relation prerequisite unavailable")

type JobRepository interface {
	ListDue(context.Context, int, int, time.Time) ([]model.EpisodeProjectionJob, error)
	Claim(context.Context, int64, string, time.Time, time.Duration, int) (*model.EpisodeProjectionJob, error)
	CompleteOrRequeue(context.Context, *model.EpisodeProjectionJob, uint, time.Time) (*model.EpisodeProjectionJob, error)
	Retry(context.Context, *model.EpisodeProjectionJob, string, time.Time, int) (*model.EpisodeProjectionJob, error)
	RecoverExpired(context.Context, time.Time, int) (int64, error)
	UpsertDesired(context.Context, int64, string, uint, time.Time) (*model.EpisodeProjectionJob, error)
	ReleaseOtherSessions(context.Context, int64, string, time.Time) (int64, error)
}

type ProjectionRunner interface {
	Project(context.Context, int64, string, uint) (*model.EpisodeMemory, error)
}

type ProjectionIndexWriter interface {
	Index(context.Context, *model.EpisodeMemory) error
}

// RelationWriter is the episodic write side's relation pass, attached after a
// projection has landed and been indexed.
//
// It is an interface here for the same reason GovernanceIndex is: this package
// must keep working unchanged when nothing is attached, and it must not import
// the relation DAO. Nil means no relation pass exists, which is the shipped
// default.
//
// Its error contract differs from the indexer's deliberately. An indexing
// failure is deferred to the reindexer and the job completes, because the
// Episode is written and the index will catch up. A relation failure is not
// deferred anywhere - nothing else re-runs it - so the job is retried rather
// than recorded as finished with a stage that silently did not happen.
type RelationWriter interface {
	Process(context.Context, *model.EpisodeMemory) error
	// DecidesRelations says whether Process would decide anything for a
	// freshly projected Episode. An indexing failure must not change the job's
	// outcome for a deployment whose relation pass is off, so the worker asks
	// before it lets a deferred index turn into a relation retry.
	//
	// The question is behavioural rather than the generator's Mode enum
	// because this package must not import the relation service: its test
	// suite imports this one, and an import there would close a cycle.
	DecidesRelations() bool
}

type WorkerConfig struct {
	WorkerCount       int
	QueueSize         int
	ScanInterval      time.Duration
	ReconcileInterval time.Duration
	TaskTimeout       time.Duration
	Lease             time.Duration
	// RecoveryInterval is how often expired processing jobs are reclaimed while
	// the worker is running. Recovery used to happen only at Start, so a job
	// whose completion write was rolled back — by a deadlock, or any transient
	// error — stayed in processing until the process was restarted.
	RecoveryInterval time.Duration
	RetryDelay       time.Duration
	QuietWindow      time.Duration
	MaxAttempts      int
}

func DefaultWorkerConfig() WorkerConfig {
	return WorkerConfig{
		WorkerCount: 1, QueueSize: 32, ScanInterval: 3 * time.Second, ReconcileInterval: time.Minute,
		TaskTimeout: 30 * time.Second, Lease: time.Minute, RetryDelay: 15 * time.Second,
		RecoveryInterval: 30 * time.Second,
		QuietWindow:      10 * time.Minute, MaxAttempts: 3,
	}
}

func (c WorkerConfig) Validate() error {
	if c.WorkerCount < 1 || c.WorkerCount > 16 || c.QueueSize < c.WorkerCount || c.QueueSize > 1000 ||
		c.ScanInterval <= 0 || c.ReconcileInterval <= 0 || c.TaskTimeout <= 0 || c.Lease <= c.TaskTimeout ||
		c.RecoveryInterval <= 0 ||
		c.RetryDelay <= 0 || c.QuietWindow < 0 || c.QuietWindow > 24*time.Hour || c.MaxAttempts < 1 || c.MaxAttempts > 10 {
		return fmt.Errorf("invalid episode worker config")
	}
	return nil
}

type Worker struct {
	repository JobRepository
	projector  ProjectionRunner
	reconciler *Reconciler
	config     WorkerConfig
	queue      chan model.EpisodeProjectionJob
	now        func() time.Time
	logf       func(string, ...any)
	mu         sync.Mutex
	started    bool
	cancel     context.CancelFunc
	done       chan struct{}
	indexer    ProjectionIndexWriter
	// relations is the optional relation pass. Nil is the shipped default.
	relations RelationWriter
}

func NewWorker(repository JobRepository, projector ProjectionRunner, reconciler *Reconciler, config WorkerConfig) (*Worker, error) {
	if repository == nil || projector == nil || reconciler == nil {
		return nil, fmt.Errorf("episode worker dependencies are required")
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &Worker{repository: repository, projector: projector, reconciler: reconciler, config: config, queue: make(chan model.EpisodeProjectionJob, config.QueueSize), now: time.Now, logf: log.Printf}, nil
}

func (w *Worker) UseIndexer(indexer ProjectionIndexWriter) { w.indexer = indexer }

// UseRelationWriter attaches the episodic relation pass.
//
// Only a deployment configured for shadow or apply attaches one. Attaching it
// to the shipped default would start deciding relations for every learner on
// the strength of a classifier nobody has measured on real sessions.
func (w *Worker) UseRelationWriter(writer RelationWriter) { w.relations = writer }

func (w *Worker) Start(parent context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.started {
		return ErrWorkerAlreadyStarted
	}
	if _, err := w.repository.RecoverExpired(parent, w.now(), w.config.MaxAttempts); err != nil {
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

func (w *Worker) Stop(ctx context.Context) error {
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

func (w *Worker) scan(ctx context.Context) {
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

func (w *Worker) scanOnce(ctx context.Context) {
	jobs, err := w.repository.ListDue(ctx, w.config.QueueSize, w.config.MaxAttempts, w.now())
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

func (w *Worker) reconcile(ctx context.Context) {
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

// recover reclaims jobs whose lease expired while this worker is running.
//
// Start already does one pass, which covers a process that died. It does not
// cover a job stranded by this process: a completion write can be rolled back
// (a deadlock is the case that actually happened), leaving the row in
// processing with a lease nobody will release. Without a periodic pass that job
// waits for the next restart, and the session it belongs to silently stops
// being projected.
func (w *Worker) recover(ctx context.Context) {
	ticker := time.NewTicker(w.config.RecoveryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			recovered, err := w.repository.RecoverExpired(ctx, w.now(), w.config.MaxAttempts)
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

func (w *Worker) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case listed := <-w.queue:
			w.process(ctx, listed)
		}
	}
}

func (w *Worker) process(ctx context.Context, listed model.EpisodeProjectionJob) {
	claimed, err := w.repository.Claim(ctx, listed.UserID, listed.ID, w.now(), w.config.Lease, w.config.MaxAttempts)
	if err != nil {
		return
	}
	started := w.now()
	taskCtx, cancel := context.WithTimeout(ctx, w.config.TaskTimeout)
	episode, projectErr := w.projector.Project(taskCtx, claimed.UserID, claimed.SessionID, claimed.ClaimedMessageID)
	cancel()
	status, errorCode := "completed", ""
	if projectErr != nil {
		status, errorCode = "failed", projectionErrorCode(projectErr)
	}
	observability.Memory.Projection(status, errorCode, w.now().Sub(started), started.Sub(claimed.AvailableAt))
	var indexErr error
	if projectErr == nil && w.indexer != nil {
		indexCtx, cancelIndex := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		indexErr = w.indexer.Index(indexCtx, episode)
		if indexErr != nil && ctx.Err() == nil {
			// For a deployment without a deciding relation pass this stays a
			// deferral: the Episode is durable, the reindexer will catch up,
			// and the job completes. The relation pass below decides whether
			// the deferral is safe to complete over.
			w.logf("episode indexing deferred to reindex: episode=%s error_type=%T", episode.ID, indexErr)
		}
		cancelIndex()
	}
	// The relation pass runs after the projection is durable and the index has
	// been offered the card, and before the job is recorded as complete.
	//
	// When this writer decides relations, the index is a prerequisite, not a
	// deferral: no indexer at all, or a failed index write, means the pass is
	// skipped and the job retries on the closed prerequisite code. Running it
	// would be worse than useless here: the governor refuses the unindexed
	// Episode, the refusal is recorded as a decided proposal, and a decided
	// pair is never re-decided - so a completed job would leave that relation
	// permanently lost behind a row that says it was considered. Retrying
	// re-runs an idempotent projection (no model calls, no classification)
	// and re-offers the index; when the index lands, the pass runs against an
	// Episode it can read. If the prerequisite never lands within the attempt
	// budget, the job fails visibly instead of completing silently. A writer
	// that decides nothing keeps the pure index-deferral semantics exactly.
	if projectErr == nil && w.relations != nil {
		if w.relations.DecidesRelations() && (w.indexer == nil ||
			(indexErr != nil && ctx.Err() == nil)) {
			if w.indexer == nil {
				w.logf("episode relation prerequisite failed: no indexer attached; "+
					"job will retry: episode=%s", episode.ID)
				projectErr = fmt.Errorf(
					"%w: no episode indexer is attached to satisfy the relation pass",
					ErrRelationPrerequisite)
			} else {
				w.logf("episode relation prerequisite failed: index deferred; "+
					"job will retry: episode=%s error_type=%T", episode.ID, indexErr)
				projectErr = fmt.Errorf(
					"%w: index deferred; relation pass cannot read an unindexed "+
						"episode: %w", ErrRelationPrerequisite, indexErr)
			}
		} else {
			relCtx, cancelRel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			relErr := w.relations.Process(relCtx, episode)
			cancelRel()
			if relErr != nil && ctx.Err() == nil {
				// Only projectErr is set. The projection metric was already
				// emitted above and reflects the projection, which did succeed;
				// rewriting it here would report a projection failure that did
				// not happen. What changes is the job outcome: it retries.
				//
				// The error is wrapped in ErrRelationPass so the recorded code
				// names the stage that actually failed. Without it the job carries
				// "projection_failed", which sends whoever reads it to the
				// generator's output - the one stage that was fine.
				w.logf("episode relation pass failed, job will retry: episode=%s error_type=%T",
					episode.ID, relErr)
				projectErr = fmt.Errorf("%w: %w", ErrRelationPass, relErr)
			}
		}
	}
	persistCtx, cancelPersist := persistenceContext(ctx)
	defer cancelPersist()
	if projectErr == nil {
		if _, err := w.repository.CompleteOrRequeue(persistCtx, claimed, claimed.ClaimedMessageID, w.now()); err != nil && ctx.Err() == nil {
			w.logf("episode worker completion failed: job=%s error_type=%T", claimed.ID, err)
		}
		return
	}
	if _, err := w.repository.Retry(persistCtx, claimed, projectionErrorCode(projectErr), w.now().Add(w.config.RetryDelay), w.config.MaxAttempts); err != nil && ctx.Err() == nil {
		w.logf("episode worker retry failed: job=%s error_type=%T", claimed.ID, err)
	}
}

func persistenceContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
}

func projectionErrorCode(err error) string {
	switch {
	// The relation sentinels are checked before DeadlineExceeded and friends
	// on purpose: both wrap whatever their stage returned, and a store or
	// index timeout inside them would otherwise be reported as a projection
	// timeout - the one stage that succeeded.
	case errors.Is(err, ErrRelationPrerequisite):
		return "relation_prerequisite_failed"
	case errors.Is(err, ErrRelationPass):
		return "relation_failed"
	case errors.Is(err, context.DeadlineExceeded):
		return "projection_timeout"
	case errors.Is(err, context.Canceled):
		return "projection_cancelled"
	case errors.Is(err, ErrEpisodeSensitive):
		return "sensitive_content"
	case errors.Is(err, ErrEpisodeSourceBoundary):
		return "source_boundary"
	case errors.Is(err, ErrEpisodeToolProtocol), errors.Is(err, ErrEmptyEpisode):
		return "invalid_model_output"
	case errors.Is(err, ErrProjectionSourceNotFound), errors.Is(err, ErrProjectionTargetMissing):
		return "source_not_found"
	default:
		return "projection_failed"
	}
}

type Publisher struct {
	repository  JobRepository
	quietWindow time.Duration
	now         func() time.Time
}

func NewPublisher(repository JobRepository, quietWindow time.Duration) (*Publisher, error) {
	if repository == nil || quietWindow < 0 || quietWindow > 24*time.Hour {
		return nil, fmt.Errorf("invalid episode publisher configuration")
	}
	return &Publisher{repository: repository, quietWindow: quietWindow, now: time.Now}, nil
}

func (p *Publisher) PublishConversationCompleted(ctx context.Context, userID int64, sessionID string, latestMessageID uint) error {
	if p == nil || userID <= 0 || sessionID == "" || latestMessageID == 0 {
		return fmt.Errorf("invalid episode completion event")
	}
	now := p.now()
	if _, err := p.repository.UpsertDesired(ctx, userID, sessionID, latestMessageID, now.Add(p.quietWindow)); err != nil {
		return err
	}
	// Writing here is evidence that every other session of this user is over, so
	// their projections no longer need to wait out the quiet window. Without
	// this a session that ended minutes ago stays absent from "上次聊到哪了",
	// which reads the projected episodes rather than the raw messages.
	if _, err := p.repository.ReleaseOtherSessions(ctx, userID, sessionID, now); err != nil {
		return err
	}
	return nil
}
