package episode

import (
	episodedao "GopherAI/dao/episode"
	"GopherAI/model"
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type workerRepository struct {
	mu           sync.Mutex
	listed       []model.EpisodeProjectionJob
	claimed      *model.EpisodeProjectionJob
	completed    int
	retried      int
	retryCode    string
	gaps         []episodedao.ProjectionGap
	upserts      []episodedao.ProjectionGap
	recovered    int64
	recoverCalls int
	released     []string
}

func (r *workerRepository) ReleaseOtherSessions(_ context.Context, _ int64, currentSessionID string, _ time.Time) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.released = append(r.released, currentSessionID)
	return 0, nil
}

func (r *workerRepository) ListDue(context.Context, int, int, time.Time) ([]model.EpisodeProjectionJob, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]model.EpisodeProjectionJob(nil), r.listed...), nil
}

func (r *workerRepository) Claim(context.Context, int64, string, time.Time, time.Duration, int) (*model.EpisodeProjectionJob, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.claimed == nil {
		return nil, episodedao.ErrJobConflict
	}
	copy := *r.claimed
	return &copy, nil
}

func (r *workerRepository) CompleteOrRequeue(context.Context, *model.EpisodeProjectionJob, uint, time.Time) (*model.EpisodeProjectionJob, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.completed++
	return r.claimed, nil
}

func (r *workerRepository) Retry(_ context.Context, _ *model.EpisodeProjectionJob, code string, _ time.Time, _ int) (*model.EpisodeProjectionJob, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.retried++
	r.retryCode = code
	return r.claimed, nil
}

func (r *workerRepository) RecoverExpired(context.Context, time.Time, int) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recoverCalls++
	return r.recovered, nil
}

func (r *workerRepository) RecoverCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.recoverCalls
}

func (r *workerRepository) UpsertDesired(_ context.Context, userID int64, sessionID string, messageID uint, _ time.Time) (*model.EpisodeProjectionJob, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.upserts = append(r.upserts, episodedao.ProjectionGap{UserID: userID, SessionID: sessionID, LatestMessageID: messageID})
	return &model.EpisodeProjectionJob{UserID: userID, SessionID: sessionID, DesiredMessageID: messageID}, nil
}

func (r *workerRepository) ListProjectionGaps(context.Context, int) ([]episodedao.ProjectionGap, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]episodedao.ProjectionGap(nil), r.gaps...), nil
}

type stubProjectionRunner struct{ err error }

func (s stubProjectionRunner) Project(context.Context, int64, string, uint) (*model.EpisodeMemory, error) {
	return &model.EpisodeMemory{}, s.err
}

type stubProjectionIndexer struct {
	calls int
	err   error
}

func (s *stubProjectionIndexer) Index(context.Context, *model.EpisodeMemory) error {
	s.calls++
	return s.err
}

func testClaimedJob() *model.EpisodeProjectionJob {
	return &model.EpisodeProjectionJob{ID: "job-1", UserID: 7, SessionID: "session-1", ClaimedMessageID: 9, LeaseToken: "lease", Status: model.EpisodeProjectionProcessing}
}

func testWorkerConfig() WorkerConfig {
	return WorkerConfig{WorkerCount: 1, QueueSize: 2, ScanInterval: 5 * time.Millisecond, ReconcileInterval: 10 * time.Millisecond, TaskTimeout: time.Second, Lease: 2 * time.Second, RetryDelay: time.Millisecond, RecoveryInterval: 20 * time.Millisecond, QuietWindow: 0, MaxAttempts: 3}
}

func newTestReconciler(t *testing.T, repository *workerRepository) *Reconciler {
	t.Helper()
	reconciler, err := NewReconciler(repository, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	return reconciler
}

func TestWorkerCompletesSuccessAndRetriesFailure(t *testing.T) {
	for _, test := range []struct {
		name       string
		projectErr error
		completed  int
		retried    int
		code       string
	}{
		{name: "success", completed: 1},
		{name: "source boundary", projectErr: ErrEpisodeSourceBoundary, retried: 1, code: "source_boundary"},
		{name: "timeout", projectErr: context.DeadlineExceeded, retried: 1, code: "projection_timeout"},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository := &workerRepository{claimed: testClaimedJob()}
			worker, err := NewWorker(repository, stubProjectionRunner{err: test.projectErr}, newTestReconciler(t, repository), testWorkerConfig())
			if err != nil {
				t.Fatal(err)
			}
			worker.process(context.Background(), model.EpisodeProjectionJob{ID: "job-1", UserID: 7})
			if repository.completed != test.completed || repository.retried != test.retried || repository.retryCode != test.code {
				t.Fatalf("completed=%d retried=%d code=%s", repository.completed, repository.retried, repository.retryCode)
			}
		})
	}
}

func TestWorkerIndexFailureDoesNotRepeatProjection(t *testing.T) {
	repository := &workerRepository{claimed: testClaimedJob()}
	worker, err := NewWorker(repository, stubProjectionRunner{}, newTestReconciler(t, repository), testWorkerConfig())
	if err != nil {
		t.Fatal(err)
	}
	indexer := &stubProjectionIndexer{err: errors.New("redis down")}
	worker.UseIndexer(indexer)
	worker.process(context.Background(), model.EpisodeProjectionJob{ID: "job-1", UserID: 7})
	if indexer.calls != 1 || repository.completed != 1 || repository.retried != 0 {
		t.Fatalf("calls=%d completed=%d retried=%d", indexer.calls, repository.completed, repository.retried)
	}
}

func TestReconcilerRepairsBoundedGaps(t *testing.T) {
	repository := &workerRepository{gaps: []episodedao.ProjectionGap{{UserID: 7, SessionID: "s1", LatestMessageID: 4}, {UserID: 8, SessionID: "s2", LatestMessageID: 9}}}
	reconciler := newTestReconciler(t, repository)
	repaired, err := reconciler.RunOnce(context.Background())
	if err != nil || repaired != 2 || len(repository.upserts) != 2 {
		t.Fatalf("repaired=%d upserts=%v error=%v", repaired, repository.upserts, err)
	}
}

func TestWorkerStartStopAndDuplicateStart(t *testing.T) {
	repository := &workerRepository{}
	worker, err := NewWorker(repository, stubProjectionRunner{}, newTestReconciler(t, repository), testWorkerConfig())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := worker.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := worker.Start(ctx); !errors.Is(err, ErrWorkerAlreadyStarted) {
		t.Fatalf("duplicate start error=%v", err)
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
	defer stopCancel()
	if err := worker.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
}

func TestPublisherUsesQuietWindow(t *testing.T) {
	repository := &workerRepository{}
	publisher, err := NewPublisher(repository, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	publisher.now = func() time.Time { return now }
	if err := publisher.PublishConversationCompleted(context.Background(), 7, "s1", 11); err != nil {
		t.Fatal(err)
	}
	if len(repository.upserts) != 1 || repository.upserts[0].LatestMessageID != 11 {
		t.Fatalf("upserts=%v", repository.upserts)
	}
	// Writing in one session ends the others, so their projections stop waiting.
	// Without this, a session that finished minutes ago has no episode and is
	// invisible to a "last session" recall.
	if len(repository.released) != 1 || repository.released[0] != "s1" {
		t.Fatalf("released=%v want the current session excluded once", repository.released)
	}
}

// BUG-AGENT-104. Recovery used to run only at Start, so a job stranded by a
// rolled-back completion — the deadlock this fix is about produced exactly that
// — stayed in processing until the process restarted. It must now be reclaimed
// by a running worker.
func TestWorkerReclaimsExpiredJobsWithoutRestarting(t *testing.T) {
	repository := &workerRepository{}
	config := testWorkerConfig()
	config.RecoveryInterval = 10 * time.Millisecond
	worker, err := NewWorker(repository, stubProjectionRunner{}, newTestReconciler(t, repository), config)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := worker.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
		defer stopCancel()
		_ = worker.Stop(stopCtx)
	}()

	// Start performs one pass. The point of this test is the passes after it.
	deadline := time.Now().Add(2 * time.Second)
	for repository.RecoverCalls() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if calls := repository.RecoverCalls(); calls < 3 {
		t.Fatalf("RecoverExpired calls = %d, want at least 3; recovery is still only happening at startup",
			calls)
	}
}

func TestWorkerConfigRequiresARecoveryInterval(t *testing.T) {
	config := testWorkerConfig()
	config.RecoveryInterval = 0
	if err := config.Validate(); err == nil {
		t.Fatal("a worker with no recovery interval was accepted; stranded jobs would wait for a restart")
	}
}
