package memory

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	memorydao "GopherAI/dao/memory"
)

// DreamWorker runs the governance step that decides whether a candidate becomes
// a memory.
//
// Every other background stage in this project already has one: knowledge
// indexing, knowledge cleanup, episode projection, and the extraction that
// produces these candidates in the first place. Governance was reachable only
// through POST /agent/memories/dream, so nothing triggered it, and the
// consequence was visible in the data: seventy extraction jobs had all
// completed, fourteen candidates sat pending, and every memory in the database
// had a null origin_candidate_id — none of them had come through this path.
// Automatic memory was, in effect, switched off.
//
// The worker is thin because GovernanceService.Process already owns the hard
// parts. It claims a candidate under a row lock, replays an already-applied
// candidate idempotently, refuses one another run is holding, and validates
// again inside the write transaction. Adding concurrency or a queue here would
// duplicate that machinery rather than extend it, so this is one goroutine
// walking a list of owners.
type DreamWorker struct {
	// process is GovernanceService.Process, held as a field so a test can drive
	// the loop without a database and a provider behind it. The loop's own
	// behaviour — which owners it visits, when it stops asking — is what is
	// worth asserting; the governance step has its own tests.
	process    func(context.Context, DreamRequest) (DreamResult, error)
	repository DreamQueueRepository
	config     DreamWorkerConfig
	logf       func(string, ...any)
	mu         sync.Mutex
	started    bool
	cancel     context.CancelFunc
	done       chan struct{}
}

// DreamQueueRepository names the owners with work waiting.
type DreamQueueRepository interface {
	ListUsersWithPendingCandidates(context.Context, int) ([]int64, error)
}

type DreamWorkerConfig struct {
	ScanInterval time.Duration
	// OwnerBatch bounds how many owners one pass serves.
	OwnerBatch int
	// PerOwner bounds how many candidates one owner may have governed in a
	// single pass, so a backlog drains without one account holding the loop.
	PerOwner int
}

// The interval is deliberately slower than the extraction worker's three
// seconds. Each candidate governed here can cost a model call, and unlike
// extraction there is no user waiting on the result: a memory that becomes
// active a minute after the conversation is no less useful than one that
// becomes active immediately.
func DefaultDreamWorkerConfig() DreamWorkerConfig {
	return DreamWorkerConfig{ScanInterval: 30 * time.Second, OwnerBatch: 20, PerOwner: 5}
}

func (c DreamWorkerConfig) Validate() error {
	if c.ScanInterval < time.Second || c.ScanInterval > time.Hour ||
		c.OwnerBatch < 1 || c.OwnerBatch > 1000 || c.PerOwner < 1 || c.PerOwner > 100 {
		return fmt.Errorf("invalid memory dream worker config")
	}
	return nil
}

func NewDreamWorker(governance *GovernanceService, repository DreamQueueRepository, config DreamWorkerConfig) (*DreamWorker, error) {
	if governance == nil || repository == nil {
		return nil, fmt.Errorf("memory dream worker dependencies are required")
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &DreamWorker{process: governance.Process, repository: repository, config: config, logf: log.Printf}, nil
}

func (w *DreamWorker) Start(parent context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.started {
		return ErrWorkerAlreadyStarted
	}
	ctx, cancel := context.WithCancel(parent)
	w.cancel, w.done, w.started = cancel, make(chan struct{}), true
	go func() {
		defer close(w.done)
		w.loop(ctx)
	}()
	return nil
}

func (w *DreamWorker) Stop(ctx context.Context) error {
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

func (w *DreamWorker) loop(ctx context.Context) {
	ticker := time.NewTicker(w.config.ScanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.runOnce(ctx)
		}
	}
}

// RunOnce serves one pass. It is exported so a test can drive the worker
// without waiting on a ticker.
func (w *DreamWorker) RunOnce(ctx context.Context) { w.runOnce(ctx) }

func (w *DreamWorker) runOnce(ctx context.Context) {
	owners, err := w.repository.ListUsersWithPendingCandidates(ctx, w.config.OwnerBatch)
	if err != nil {
		if ctx.Err() == nil {
			w.logf("memory dream worker scan failed: error_type=%T", err)
		}
		return
	}
	for _, userID := range owners {
		if ctx.Err() != nil {
			return
		}
		w.serve(ctx, userID)
	}
}

func (w *DreamWorker) serve(ctx context.Context, userID int64) {
	for served := 0; served < w.config.PerOwner; served++ {
		if ctx.Err() != nil {
			return
		}
		_, err := w.process(ctx, DreamRequest{UserID: userID})
		if err == nil {
			continue
		}
		switch {
		case errors.Is(err, memorydao.ErrCandidateNotFound):
			// The backlog for this owner is drained, or another run took the
			// last one. Either way there is nothing left to do this pass.
			return
		case errors.Is(err, memorydao.ErrDreamBusy), errors.Is(err, memorydao.ErrDreamConflict):
			// Another run holds this owner's candidate. Yield rather than spin:
			// the next pass will find it either finished or expired.
			return
		default:
			if ctx.Err() == nil {
				w.logf("memory dream worker failed: user=%d error_type=%T", userID, err)
			}
			return
		}
	}
}
