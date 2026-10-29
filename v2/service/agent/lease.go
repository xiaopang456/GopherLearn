package agent

import (
	agentdao "GopherAI/dao/agent"
	"GopherAI/model"
	"context"
	"errors"
	"log"
	"sync"
	"time"
)

const (
	defaultRunLeaseDuration     = 30 * time.Second
	defaultRunHeartbeatInterval = 10 * time.Second
	runLeaseRenewTimeout        = 3 * time.Second

	// runLeaseRetryFloor and runLeaseRetryCeiling bound the backoff between
	// renewal attempts after a transient failure.
	runLeaseRetryFloor   = 500 * time.Millisecond
	runLeaseRetryCeiling = 3 * time.Second

	// runLeaseSafetyMargin is how much of the lease is reserved at the end. A
	// retry that would land inside this window is not attempted: past that
	// point the reaper is entitled to reclaim the run, and continuing to
	// execute would risk two owners writing the same run.
	runLeaseSafetyMargin = 4 * time.Second
)

// Termination reasons a keeper can produce, so the caller can write an honest
// terminal state instead of a generic failure. They read differently in the
// row afterwards: one means the run was taken from us, the other means we
// could not reach the database.
const (
	runLeaseReasonLost  = "lease_lost"
	runLeaseReasonRenew = "lease_renewal_failed"
)

type runLeaseRenewer interface {
	RenewRunLease(context.Context, int64, string, string, string, time.Time, time.Time) error
}

// runLeaseKeeper renews one run's lease for as long as it is executing.
//
// The renewal is a database write, so it queues behind everything else the run
// is doing. A single slow write is not evidence that the run lost its lease:
// with a 30 second lease renewed every 10 seconds, a renewal that times out
// still leaves around 20 seconds in which a retry can succeed. Cancelling on
// the first error threw that away — in an S-LAT-2 load run a 20 second lock
// stall on agent_runs killed 32 of 32 in-flight runs that were all still
// executing normally.
//
// So the keeper separates two cases:
//
//   - the lease is definitely gone (ErrRunLeaseLost: the row no longer matches
//     this token, so someone else owns or already ended the run). Stop at once;
//     continuing would mean two owners.
//   - the write did not get through (timeout, deadlock, pool exhaustion).
//     Retry with backoff, but only while the lease this keeper last confirmed
//     still has margin left. Once it does not, stop.
//
// The margin is what keeps the safety boundary intact: the keeper never lets
// execution continue past the point where the reaper may reclaim the run.
type runLeaseKeeper struct {
	cancel   context.CancelFunc
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
	errMu    sync.Mutex
	err      error
	reason   string

	tuning runLeaseTuning
}

// runLeaseTuning exists so the retry behaviour can be driven deterministically
// in tests. Production always uses the constants above: a test that had to wait
// out real backoff would either take seconds or be written against a different
// code path than the one that ships.
type runLeaseTuning struct {
	retryFloor   time.Duration
	retryCeiling time.Duration
	safetyMargin time.Duration
}

func defaultRunLeaseTuning() runLeaseTuning {
	return runLeaseTuning{
		retryFloor: runLeaseRetryFloor, retryCeiling: runLeaseRetryCeiling,
		safetyMargin: runLeaseSafetyMargin,
	}
}

func startRunLeaseKeeper(
	parent context.Context,
	repository runLeaseRenewer,
	run *model.AgentRun,
	leaseDuration, heartbeatInterval time.Duration,
) (context.Context, *runLeaseKeeper) {
	return startRunLeaseKeeperTuned(parent, repository, run, leaseDuration, heartbeatInterval, defaultRunLeaseTuning())
}

func startRunLeaseKeeperTuned(
	parent context.Context,
	repository runLeaseRenewer,
	run *model.AgentRun,
	leaseDuration, heartbeatInterval time.Duration,
	tuning runLeaseTuning,
) (context.Context, *runLeaseKeeper) {
	runCtx, cancel := context.WithCancel(parent)
	keeper := &runLeaseKeeper{cancel: cancel, stop: make(chan struct{}), done: make(chan struct{}), tuning: tuning}

	// The lease this keeper knows it holds. Only a successful renewal extends
	// it: a value re-read from the row could have been written by a different
	// owner, and trusting that would defeat the fence.
	confirmedUntil := time.Now().Add(leaseDuration)
	if run.LeaseExpiresAt != nil {
		confirmedUntil = *run.LeaseExpiresAt
	}

	go func() {
		defer close(keeper.done)
		ticker := time.NewTicker(heartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-keeper.stop:
				return
			case <-ticker.C:
				renewed, reason, err := keeper.renew(runCtx, repository, run, confirmedUntil, leaseDuration)
				if err != nil {
					keeper.fail(err, reason)
					cancel()
					return
				}
				confirmedUntil = renewed
			}
		}
	}()
	return runCtx, keeper
}

// renew performs one renewal, retrying transient failures while the confirmed
// lease still has margin. It returns the new confirmed expiry, or the reason
// and error that ended the attempt.
func (k *runLeaseKeeper) renew(
	runCtx context.Context,
	repository runLeaseRenewer,
	run *model.AgentRun,
	confirmedUntil time.Time,
	leaseDuration time.Duration,
) (time.Time, string, error) {
	backoff := k.tuning.retryFloor
	attempts := 0
	for {
		attempts++
		attemptAt := time.Now()
		leaseEnds := attemptAt.Add(leaseDuration)
		// context.WithoutCancel: a renewal must still be attempted while the run
		// context is winding down, or a run that is finishing normally looks
		// abandoned to the next reader.
		renewCtx, cancelRenew := context.WithTimeout(context.WithoutCancel(runCtx), runLeaseRenewTimeout)
		err := repository.RenewRunLease(renewCtx, run.UserID, run.SessionID, run.ID, run.LeaseToken, attemptAt, leaseEnds)
		cancelRenew()
		if err == nil {
			if attempts > 1 {
				log.Printf("[agent-lease] renewal recovered after %d attempts: run=%s", attempts, run.ID)
			}
			return leaseEnds, "", nil
		}

		// Ownership is gone. No amount of retrying can get it back, and every
		// further step of execution would be a second owner's work.
		if errors.Is(err, agentdao.ErrRunLeaseLost) {
			return time.Time{}, runLeaseReasonLost, err
		}

		// Transient. Retry only while the confirmed lease still leaves room for
		// the backoff plus the safety margin.
		if !time.Now().Add(backoff + k.tuning.safetyMargin).Before(confirmedUntil) {
			log.Printf("[agent-lease] renewal exhausted the lease after %d attempts: run=%s error_type=%T",
				attempts, run.ID, err)
			return time.Time{}, runLeaseReasonRenew, err
		}
		select {
		case <-runCtx.Done():
			return time.Time{}, runLeaseReasonRenew, err
		case <-k.stop:
			return time.Time{}, runLeaseReasonRenew, err
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > k.tuning.retryCeiling {
			backoff = k.tuning.retryCeiling
		}
	}
}

func (k *runLeaseKeeper) fail(err error, reason string) {
	k.errMu.Lock()
	k.err, k.reason = err, reason
	k.errMu.Unlock()
}

func (k *runLeaseKeeper) Stop() {
	if k == nil {
		return
	}
	k.stopOnce.Do(func() {
		close(k.stop)
		k.cancel()
	})
	<-k.done
}

func (k *runLeaseKeeper) Err() error {
	if k == nil {
		return nil
	}
	k.errMu.Lock()
	defer k.errMu.Unlock()
	return k.err
}

// Reason is the termination reason to record when Err is set.
func (k *runLeaseKeeper) Reason() string {
	if k == nil {
		return ""
	}
	k.errMu.Lock()
	defer k.errMu.Unlock()
	if k.reason == "" {
		return runLeaseReasonRenew
	}
	return k.reason
}
