package isolated

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// These are the source workers' Start/Stop methods, not a new lease policy.
type Worker interface {
	Start(context.Context) error
	Stop(context.Context) error
}
type namedWorker struct {
	name   string
	worker Worker
}
type Lifecycle struct {
	cancel  context.CancelFunc
	workers []namedWorker
	closers []func() error
	mu      sync.Mutex
	closed  bool
}

func newLifecycle(parent context.Context) (context.Context, *Lifecycle) {
	ctx, cancel := context.WithCancel(parent)
	return ctx, &Lifecycle{cancel: cancel}
}
func (l *Lifecycle) start(ctx context.Context, name string, w Worker) error {
	if err := w.Start(ctx); err != nil {
		return fmt.Errorf("start %s: %w", name, err)
	}
	l.workers = append(l.workers, namedWorker{name, w})
	return nil
}
func (l *Lifecycle) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	// Source main creates a new 15s context at exit, independent of cancellation.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	l.cancel()
	var errs []error
	for i := len(l.workers) - 1; i >= 0; i-- {
		if err := l.workers[i].worker.Stop(ctx); err != nil {
			errs = append(errs, fmt.Errorf("stop %s: %w", l.workers[i].name, err))
		}
	}
	for i := len(l.closers) - 1; i >= 0; i-- {
		errs = append(errs, l.closers[i]())
	}
	return errors.Join(errs...)
}
