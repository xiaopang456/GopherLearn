package knowledge

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"GopherAI/common/rag"
	"GopherAI/model"
)

var ErrWorkerAlreadyStarted = errors.New("knowledge index worker already started")

type WorkerRepository interface {
	ListPending(context.Context, int) ([]model.KnowledgeDocument, error)
	ClaimForIndexing(context.Context, string, int64, int) (bool, error)
	CompleteIndexing(context.Context, string, int64, int, model.KnowledgeDocumentStatus, int, string) error
	RequeueIndexing(context.Context, string, int64, int) error
	RecoverInterruptedIndexing(context.Context) (int64, error)
}

type DocumentIngestion interface {
	Ingest(context.Context, model.KnowledgeDocument) (rag.ChunkIndexResult, error)
}

type WorkerConfig struct {
	WorkerCount  int
	QueueSize    int
	ScanInterval time.Duration
	TaskTimeout  time.Duration
}

func (c WorkerConfig) Validate() error {
	if c.WorkerCount <= 0 {
		return errors.New("worker count must be greater than zero")
	}
	if c.QueueSize < c.WorkerCount {
		return errors.New("queue size must be at least worker count")
	}
	if c.ScanInterval <= 0 {
		return errors.New("scan interval must be greater than zero")
	}
	return nil
}

type IndexWorkerPool struct {
	repository WorkerRepository
	ingestor   DocumentIngestion
	config     WorkerConfig
	queue      chan model.KnowledgeDocument
	logf       func(string, ...any)

	mu      sync.Mutex
	started bool
	cancel  context.CancelFunc
	done    chan struct{}
}

func NewIndexWorkerPool(repository WorkerRepository, ingestor DocumentIngestion, config WorkerConfig) (*IndexWorkerPool, error) {
	if repository == nil || ingestor == nil {
		return nil, errors.New("worker repository and ingestor must not be nil")
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &IndexWorkerPool{
		repository: repository, ingestor: ingestor, config: config,
		queue: make(chan model.KnowledgeDocument, config.QueueSize), logf: log.Printf,
	}, nil
}

func (w *IndexWorkerPool) Start(parent context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.started {
		return ErrWorkerAlreadyStarted
	}
	if parent == nil {
		parent = context.Background()
	}
	if _, err := w.repository.RecoverInterruptedIndexing(parent); err != nil {
		return fmt.Errorf("recover interrupted knowledge indexing: %w", err)
	}
	ctx, cancel := context.WithCancel(parent)
	w.cancel = cancel
	w.done = make(chan struct{})
	w.started = true

	var group sync.WaitGroup
	group.Add(w.config.WorkerCount + 1)
	for workerID := 0; workerID < w.config.WorkerCount; workerID++ {
		go func() {
			defer group.Done()
			w.runWorker(ctx)
		}()
	}
	go func() {
		defer group.Done()
		w.runScanner(ctx)
	}()
	go func() {
		group.Wait()
		close(w.done)
	}()
	return nil
}

func (w *IndexWorkerPool) Stop(ctx context.Context) error {
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

func (w *IndexWorkerPool) runScanner(ctx context.Context) {
	w.scanPending(ctx)
	ticker := time.NewTicker(w.config.ScanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.scanPending(ctx)
		}
	}
}

func (w *IndexWorkerPool) scanPending(ctx context.Context) {
	documents, err := w.repository.ListPending(ctx, w.config.QueueSize)
	if err != nil {
		if ctx.Err() == nil {
			w.logf("knowledge worker: scan pending documents failed")
		}
		return
	}
	for _, document := range documents {
		select {
		case <-ctx.Done():
			return
		case w.queue <- document:
		}
	}
}

func (w *IndexWorkerPool) runWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case document := <-w.queue:
			if ctx.Err() != nil {
				return
			}
			w.process(ctx, document)
		}
	}
}

func (w *IndexWorkerPool) process(ctx context.Context, document model.KnowledgeDocument) {
	claimed, err := w.repository.ClaimForIndexing(ctx, document.ID, document.UserID, document.Version)
	if err != nil {
		if ctx.Err() == nil {
			w.logf("knowledge worker: claim document %s failed", document.ID)
		}
		return
	}
	if !claimed {
		return
	}

	taskContext := ctx
	cancelTask := func() {}
	if w.config.TaskTimeout > 0 {
		taskContext, cancelTask = context.WithTimeout(ctx, w.config.TaskTimeout)
	}
	result, ingestErr := w.ingestor.Ingest(taskContext, document)
	cancelTask()
	if ingestErr == nil {
		// Counts and an enumerated status only: no content, vector, Redis key,
		// or hash may appear in this line.
		w.logf("knowledge worker: document %s version %d indexed total=%d reused=%d embedded=%d reuse=%s reason=%s",
			document.ID, document.Version, result.TotalChunks, result.ReusedChunks,
			result.EmbeddedChunks, result.ReuseStatus, result.FallbackReason)
		if err := w.repository.CompleteIndexing(ctx, document.ID, document.UserID, document.Version, model.KnowledgeStatusReady, result.TotalChunks, ""); err != nil && ctx.Err() == nil {
			w.logf("knowledge worker: complete document %s failed", document.ID)
		}
		return
	}
	if ctx.Err() != nil || errors.Is(ingestErr, context.Canceled) {
		requeueCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := w.repository.RequeueIndexing(requeueCtx, document.ID, document.UserID, document.Version); err != nil {
			w.logf("knowledge worker: requeue canceled document %s failed", document.ID)
		}
		return
	}
	safeMessage := SafeIndexError(ingestErr)
	if err := w.repository.CompleteIndexing(ctx, document.ID, document.UserID, document.Version, model.KnowledgeStatusFailed, 0, safeMessage); err != nil {
		w.logf("knowledge worker: mark document %s failed status failed", document.ID)
	}
}
