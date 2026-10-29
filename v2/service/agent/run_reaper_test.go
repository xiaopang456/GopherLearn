package agent

import (
	"context"
	"sync"
	"testing"
	"time"
)

type reaperRepository struct {
	mu      sync.Mutex
	calls   int
	expired int64
	called  chan struct{}
}

func (r *reaperRepository) ExpireAbandonedChatRuns(_ context.Context, _ time.Time, _ int) (int64, error) {
	r.mu.Lock()
	r.calls++
	expired := r.expired
	r.mu.Unlock()
	select {
	case r.called <- struct{}{}:
	default:
	}
	return expired, nil
}

func TestRunLeaseReaperSweepsImmediatelyAndStops(t *testing.T) {
	repository := &reaperRepository{expired: 2, called: make(chan struct{}, 1)}
	reaper, err := NewRunLeaseReaper(repository, RunLeaseReaperConfig{Interval: time.Hour, Batch: 10})
	if err != nil {
		t.Fatal(err)
	}
	if err = reaper.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-repository.called:
	case <-time.After(time.Second):
		t.Fatal("initial run lease sweep did not execute")
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err = reaper.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
	repository.mu.Lock()
	calls := repository.calls
	repository.mu.Unlock()
	if calls != 1 {
		t.Fatalf("sweep calls=%d, want 1", calls)
	}
}

func TestRunLeaseReaperRejectsInvalidConfiguration(t *testing.T) {
	if _, err := NewRunLeaseReaper(nil, DefaultRunLeaseReaperConfig()); err == nil {
		t.Fatal("expected nil repository to be rejected")
	}
	if _, err := NewRunLeaseReaper(&reaperRepository{}, RunLeaseReaperConfig{}); err == nil {
		t.Fatal("expected empty configuration to be rejected")
	}
}
