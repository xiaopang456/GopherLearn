package memory

import (
	"GopherAI/model"
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"
)

var ErrWorkerAlreadyStarted = errors.New("memory worker already started")

type JobRepository interface {
	ListReady(context.Context, int, time.Time) ([]model.MemoryExtractionJob, error)
	Claim(context.Context, string, time.Time, time.Duration, int) (*model.MemoryExtractionJob, error)
	Complete(context.Context, *model.MemoryExtractionJob, *model.MemoryCandidate, model.MemoryJobStatus, string, time.Time) error
	Retry(context.Context, *model.MemoryExtractionJob, string, time.Time, int) error
	RecoverExpired(context.Context, time.Time, int) (int64, error)
}

type WorkerConfig struct {
	WorkerCount  int
	QueueSize    int
	ScanInterval time.Duration
	TaskTimeout  time.Duration
	Lease        time.Duration
	RetryDelay   time.Duration
	MaxAttempts  int
}

func DefaultWorkerConfig() WorkerConfig {
	return WorkerConfig{WorkerCount: 1, QueueSize: 32, ScanInterval: 3 * time.Second, TaskTimeout: 30 * time.Second, Lease: time.Minute, RetryDelay: 15 * time.Second, MaxAttempts: 3}
}

func (c WorkerConfig) Validate() error {
	if c.WorkerCount < 1 || c.WorkerCount > 16 || c.QueueSize < c.WorkerCount || c.ScanInterval <= 0 || c.TaskTimeout <= 0 || c.Lease <= c.TaskTimeout || c.RetryDelay <= 0 || c.MaxAttempts < 1 || c.MaxAttempts > 10 {
		return fmt.Errorf("invalid memory worker config")
	}
	return nil
}

type Worker struct {
	repository JobRepository
	loader     EvidenceLoader
	extractor  Extractor
	config     WorkerConfig
	queue      chan model.MemoryExtractionJob
	logf       func(string, ...any)
	now        func() time.Time
	mu         sync.Mutex
	started    bool
	cancel     context.CancelFunc
	done       chan struct{}
}

func NewWorker(repository JobRepository, loader EvidenceLoader, extractor Extractor, config WorkerConfig) (*Worker, error) {
	if repository == nil || loader == nil || extractor == nil {
		return nil, fmt.Errorf("memory worker dependencies are required")
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &Worker{repository: repository, loader: loader, extractor: extractor, config: config, queue: make(chan model.MemoryExtractionJob, config.QueueSize), logf: log.Printf, now: time.Now}, nil
}

func (w *Worker) Start(parent context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.started {
		return ErrWorkerAlreadyStarted
	}
	if _, err := w.repository.RecoverExpired(parent, w.now(), w.config.MaxAttempts); err != nil {
		return fmt.Errorf("recover memory jobs: %w", err)
	}
	ctx, cancel := context.WithCancel(parent)
	w.cancel, w.done, w.started = cancel, make(chan struct{}), true
	var group sync.WaitGroup
	group.Add(w.config.WorkerCount + 1)
	for i := 0; i < w.config.WorkerCount; i++ {
		go func() { defer group.Done(); w.run(ctx) }()
	}
	go func() { defer group.Done(); w.scan(ctx) }()
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
	jobs, err := w.repository.ListReady(ctx, w.config.QueueSize, w.now())
	if err != nil {
		if ctx.Err() == nil {
			w.logf("memory worker scan failed")
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

func (w *Worker) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-w.queue:
			w.process(ctx, job)
		}
	}
}

func (w *Worker) process(ctx context.Context, listed model.MemoryExtractionJob) {
	job, err := w.repository.Claim(ctx, listed.ID, w.now(), w.config.Lease, w.config.MaxAttempts)
	if err != nil {
		return
	}
	taskCtx, cancel := context.WithTimeout(ctx, w.config.TaskTimeout)
	evidence, loadErr := w.loader.LoadEvidence(taskCtx, job)
	var candidate *model.MemoryCandidate
	if loadErr == nil {
		candidate, loadErr = w.extractor.Extract(taskCtx, evidence)
	}
	cancel()
	if loadErr == nil {
		candidate.JobID = job.ID
		persistCtx, cancelPersist := persistenceContext(ctx)
		err := w.repository.Complete(persistCtx, job, candidate, model.MemoryJobCompleted, "", w.now())
		cancelPersist()
		if err != nil && ctx.Err() == nil {
			w.logf("memory worker complete failed")
		}
		return
	}
	if errors.Is(loadErr, ErrNoDurableMemory) || errors.Is(loadErr, ErrSensitiveMemory) || errors.Is(loadErr, ErrInvalidSummaryEvidence) {
		code := "no_durable_memory"
		if errors.Is(loadErr, ErrSensitiveMemory) {
			code = "sensitive_content"
		} else if errors.Is(loadErr, ErrInvalidSummaryEvidence) {
			code = "invalid_summary_evidence"
		}
		persistCtx, cancelPersist := persistenceContext(ctx)
		_ = w.repository.Complete(persistCtx, job, nil, model.MemoryJobRejected, code, w.now())
		cancelPersist()
		return
	}
	persistCtx, cancelPersist := persistenceContext(ctx)
	_ = w.repository.Retry(persistCtx, job, safeErrorCode(loadErr), w.now().Add(w.config.RetryDelay), w.config.MaxAttempts)
	cancelPersist()
}

func persistenceContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), 3*time.Second)
}

func safeErrorCode(err error) string {
	return ExtractionErrorCode(err)
}
