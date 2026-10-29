package agent

import (
	agentdao "GopherAI/dao/agent"

	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"GopherAI/model"
)

func TestRunLeaseKeeperCancelsExecutionWhenRenewalFails(t *testing.T) {
	want := errors.New("lease rejected")
	renewer := &fakeLeaseRenewer{err: want, called: make(chan struct{})}
	run := &model.AgentRun{ID: "run-lease", UserID: 7, SessionID: "session-lease", LeaseToken: "lease-token"}
	ctx, keeper := startRunLeaseKeeper(context.Background(), renewer, run, 50*time.Millisecond, time.Millisecond)
	defer keeper.Stop()
	select {
	case <-renewer.called:
	case <-time.After(time.Second):
		t.Fatal("lease renewal was not attempted")
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("execution context was not cancelled")
	}
	if !errors.Is(keeper.Err(), want) {
		t.Fatalf("keeper error=%v, want %v", keeper.Err(), want)
	}
}

func TestRunLeaseKeeperStopDoesNotRenewAfterCompletion(t *testing.T) {
	renewer := &fakeLeaseRenewer{}
	run := &model.AgentRun{ID: "run-stop", UserID: 7, SessionID: "session-stop", LeaseToken: "lease-token"}
	_, keeper := startRunLeaseKeeper(context.Background(), renewer, run, time.Second, time.Hour)
	keeper.Stop()
	keeper.Stop()
	if renewer.Calls() != 0 {
		t.Fatalf("renew calls=%d, want 0", renewer.Calls())
	}
}

type fakeLeaseRenewer struct {
	mu     sync.Mutex
	calls  int
	err    error
	called chan struct{}
	once   sync.Once
}

func (r *fakeLeaseRenewer) RenewRunLease(context.Context, int64, string, string, string, time.Time, time.Time) error {
	r.mu.Lock()
	r.calls++
	err := r.err
	r.mu.Unlock()
	if r.called != nil {
		r.once.Do(func() { close(r.called) })
	}
	return err
}

func (r *fakeLeaseRenewer) Calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// BUG-AGENT-103. A renewal is a database write, so it can fail for reasons that
// have nothing to do with ownership. These four tests pin what the keeper does
// with each kind of failure — the difference between them is the whole fix.

// scriptedLeaseRenewer answers each renewal from a script, so a test can say
// exactly which attempt fails and how.
type scriptedLeaseRenewer struct {
	mu      sync.Mutex
	calls   int
	replies []error
	tail    error
}

func (r *scriptedLeaseRenewer) RenewRunLease(context.Context, int64, string, string, string, time.Time, time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.calls <= len(r.replies) {
		return r.replies[r.calls-1]
	}
	return r.tail
}

func (r *scriptedLeaseRenewer) Calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func fastLeaseTuning() runLeaseTuning {
	return runLeaseTuning{retryFloor: 2 * time.Millisecond, retryCeiling: 5 * time.Millisecond,
		safetyMargin: 30 * time.Millisecond}
}

func TestRunLeaseKeeperRenewsNormallyWithoutCancelling(t *testing.T) {
	renewer := &scriptedLeaseRenewer{}
	run := &model.AgentRun{ID: "run-ok", UserID: 7, SessionID: "s", LeaseToken: "token"}
	ctx, keeper := startRunLeaseKeeperTuned(context.Background(), renewer, run,
		time.Second, 5*time.Millisecond, fastLeaseTuning())
	defer keeper.Stop()

	deadline := time.Now().Add(time.Second)
	for renewer.Calls() < 3 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if renewer.Calls() < 3 {
		t.Fatalf("renew calls = %d, want at least 3", renewer.Calls())
	}
	select {
	case <-ctx.Done():
		t.Fatal("a healthy run was cancelled while its lease was being renewed normally")
	default:
	}
	if err := keeper.Err(); err != nil {
		t.Errorf("keeper error = %v, want none", err)
	}
}

func TestRunLeaseKeeperRetriesTransientFailuresAndRecovers(t *testing.T) {
	// Three transient failures, then the database comes back. This is the case
	// the old keeper turned into a killed run.
	renewer := &scriptedLeaseRenewer{replies: []error{
		context.DeadlineExceeded,
		errors.New("Error 1213: Deadlock found when trying to get lock"),
		context.DeadlineExceeded,
	}}
	run := &model.AgentRun{ID: "run-transient", UserID: 7, SessionID: "s", LeaseToken: "token"}
	ctx, keeper := startRunLeaseKeeperTuned(context.Background(), renewer, run,
		2*time.Second, 5*time.Millisecond, fastLeaseTuning())
	defer keeper.Stop()

	deadline := time.Now().Add(2 * time.Second)
	for renewer.Calls() < 4 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if calls := renewer.Calls(); calls < 4 {
		t.Fatalf("renew calls = %d, want at least 4 (three failures then a success)", calls)
	}
	select {
	case <-ctx.Done():
		t.Fatal("a run whose renewal recovered was cancelled anyway")
	default:
	}
	if err := keeper.Err(); err != nil {
		t.Errorf("keeper error = %v, want none after recovery", err)
	}
}

func TestRunLeaseKeeperStopsImmediatelyWhenTheLeaseIsLost(t *testing.T) {
	renewer := &scriptedLeaseRenewer{tail: agentdao.ErrRunLeaseLost}
	run := &model.AgentRun{ID: "run-lost", UserID: 7, SessionID: "s", LeaseToken: "token"}
	ctx, keeper := startRunLeaseKeeperTuned(context.Background(), renewer, run,
		10*time.Second, 5*time.Millisecond, fastLeaseTuning())
	defer keeper.Stop()

	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("a run that lost its lease kept executing")
	}
	// Exactly one attempt: ownership is gone, and retrying would only delay
	// stopping a run that another owner may already have taken.
	if calls := renewer.Calls(); calls != 1 {
		t.Errorf("renew calls = %d, want 1; a lost lease must not be retried", calls)
	}
	if !errors.Is(keeper.Err(), agentdao.ErrRunLeaseLost) {
		t.Errorf("keeper error = %v, want ErrRunLeaseLost", keeper.Err())
	}
	if reason := keeper.Reason(); reason != runLeaseReasonLost {
		t.Errorf("reason = %q, want %q", reason, runLeaseReasonLost)
	}
}

func TestRunLeaseKeeperStopsAtTheSafetyBoundaryWhenFailuresPersist(t *testing.T) {
	// The database never comes back. The keeper must retry, but it must also
	// stop while the lease it last confirmed is still valid: past that point the
	// reaper may reclaim the run, and a second owner could start writing.
	renewer := &scriptedLeaseRenewer{tail: context.DeadlineExceeded}
	leaseEnds := time.Now().Add(200 * time.Millisecond)
	run := &model.AgentRun{ID: "run-exhausted", UserID: 7, SessionID: "s", LeaseToken: "token",
		LeaseExpiresAt: &leaseEnds}
	ctx, keeper := startRunLeaseKeeperTuned(context.Background(), renewer, run,
		200*time.Millisecond, 5*time.Millisecond, fastLeaseTuning())
	defer keeper.Stop()

	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the keeper retried past its lease instead of giving up")
	}
	stopped := time.Now()

	if calls := renewer.Calls(); calls < 2 {
		t.Errorf("renew calls = %d, want more than 1; a transient failure must be retried", calls)
	}
	if stopped.After(leaseEnds) {
		t.Errorf("the keeper kept the run alive %s past the lease it had confirmed",
			stopped.Sub(leaseEnds))
	}
	if !errors.Is(keeper.Err(), context.DeadlineExceeded) {
		t.Errorf("keeper error = %v, want the last transient error", keeper.Err())
	}
	if reason := keeper.Reason(); reason != runLeaseReasonRenew {
		t.Errorf("reason = %q, want %q", reason, runLeaseReasonRenew)
	}
}
