package agent

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"
)

type abandonedRunRepository interface {
	ExpireAbandonedChatRuns(context.Context, time.Time, int) (int64, error)
}

type RunLeaseReaperConfig struct {
	Interval time.Duration
	Batch    int
}

func DefaultRunLeaseReaperConfig() RunLeaseReaperConfig {
	return RunLeaseReaperConfig{Interval: 15 * time.Second, Batch: 100}
}

type RunLeaseReaper struct {
	repository abandonedRunRepository
	config     RunLeaseReaperConfig

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

func NewRunLeaseReaper(repository abandonedRunRepository, config RunLeaseReaperConfig) (*RunLeaseReaper, error) {
	if repository == nil || config.Interval <= 0 || config.Batch <= 0 || config.Batch > 1000 {
		return nil, fmt.Errorf("invalid agent run lease reaper configuration")
	}
	return &RunLeaseReaper{repository: repository, config: config}, nil
}

func (r *RunLeaseReaper) Start(parent context.Context) error {
	if parent == nil {
		return fmt.Errorf("agent run lease reaper context is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cancel != nil {
		return fmt.Errorf("agent run lease reaper already started")
	}
	ctx, cancel := context.WithCancel(parent)
	r.cancel = cancel
	r.done = make(chan struct{})
	go r.loop(ctx, r.done)
	return nil
}

func (r *RunLeaseReaper) Stop(ctx context.Context) error {
	r.mu.Lock()
	cancel, done := r.cancel, r.done
	if cancel == nil {
		r.mu.Unlock()
		return nil
	}
	r.cancel, r.done = nil, nil
	r.mu.Unlock()
	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *RunLeaseReaper) loop(ctx context.Context, done chan struct{}) {
	defer close(done)
	r.sweep(ctx)
	ticker := time.NewTicker(r.config.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.sweep(ctx)
		}
	}
}

func (r *RunLeaseReaper) sweep(ctx context.Context) {
	expired, err := r.repository.ExpireAbandonedChatRuns(ctx, time.Now(), r.config.Batch)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("agent run lease sweep failed: error_type=%T", err)
		}
		return
	}
	if expired > 0 {
		log.Printf("agent run lease sweep expired=%d", expired)
	}
}
