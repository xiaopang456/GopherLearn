package memory

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"
)

type UsageMarker interface {
	MarkUsed(context.Context, int64, []string, time.Time) error
}

type ExpiredMemoryArchiver interface {
	ArchiveExpired(context.Context, time.Time, int) (int64, error)
}

type usageEvent struct {
	userID int64
	ids    []string
	at     time.Time
}

type AsyncUsageRecorder struct {
	marker   UsageMarker
	archiver ExpiredMemoryArchiver
	queue    chan usageEvent
	timeout  time.Duration
	logf     func(string, ...any)
	mu       sync.Mutex
	started  bool
	cancel   context.CancelFunc
	done     chan struct{}
}

func NewAsyncUsageRecorder(marker UsageMarker, queueSize int, timeout time.Duration) (*AsyncUsageRecorder, error) {
	if marker == nil || queueSize < 1 || queueSize > 1000 || timeout <= 0 || timeout > 30*time.Second {
		return nil, fmt.Errorf("invalid memory usage recorder config")
	}
	archiver, _ := marker.(ExpiredMemoryArchiver)
	return &AsyncUsageRecorder{marker: marker, archiver: archiver, queue: make(chan usageEvent, queueSize), timeout: timeout, logf: log.Printf}, nil
}

func (r *AsyncUsageRecorder) Start(parent context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.started {
		return fmt.Errorf("memory usage recorder already started")
	}
	ctx, cancel := context.WithCancel(parent)
	r.cancel, r.done, r.started = cancel, make(chan struct{}), true
	go r.run(ctx)
	return nil
}

func (r *AsyncUsageRecorder) Record(userID int64, ids []string, at time.Time) bool {
	if userID <= 0 || len(ids) == 0 || at.IsZero() {
		return false
	}
	event := usageEvent{userID: userID, ids: append([]string(nil), ids...), at: at}
	select {
	case r.queue <- event:
		return true
	default:
		return false
	}
}

func (r *AsyncUsageRecorder) Stop(ctx context.Context) error {
	r.mu.Lock()
	if !r.started {
		r.mu.Unlock()
		return nil
	}
	cancel, done := r.cancel, r.done
	r.mu.Unlock()
	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *AsyncUsageRecorder) run(ctx context.Context) {
	defer close(r.done)
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	r.archiveExpired(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case event := <-r.queue:
			writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.timeout)
			err := r.marker.MarkUsed(writeCtx, event.userID, event.ids, event.at)
			cancel()
			if err != nil && ctx.Err() == nil {
				r.logf("memory usage update failed")
			}
		case <-ticker.C:
			r.archiveExpired(ctx)
		}
	}
}

func (r *AsyncUsageRecorder) archiveExpired(ctx context.Context) {
	if r.archiver == nil {
		return
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.timeout)
	_, err := r.archiver.ArchiveExpired(writeCtx, time.Now().UTC(), 500)
	cancel()
	if err != nil && ctx.Err() == nil {
		r.logf("expired memory archive failed")
	}
}
