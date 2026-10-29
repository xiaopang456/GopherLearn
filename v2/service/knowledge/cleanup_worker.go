package knowledge

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"GopherAI/model"
)

type CleanupRepository interface {
	ListCleanupDue(context.Context, time.Time, int) ([]model.KnowledgeDocument, error)
	ClaimCleanup(context.Context, string, int64, int, time.Time, time.Time) (bool, error)
	CompleteCleanup(context.Context, model.KnowledgeDocument) error
	FailCleanup(context.Context, model.KnowledgeDocument, string, time.Time) error
}

type ChunkCleaner interface {
	DeleteDocumentVersion(context.Context, int64, string, int) error
}

type CleanupWorkerPool struct {
	repository CleanupRepository
	chunks     ChunkCleaner
	storage    FileStorage
	config     WorkerConfig
	queue      chan model.KnowledgeDocument
	logf       func(string, ...any)

	mu      sync.Mutex
	started bool
	cancel  context.CancelFunc
	done    chan struct{}
}

func NewCleanupWorkerPool(repository CleanupRepository, chunks ChunkCleaner, storage FileStorage, config WorkerConfig) (*CleanupWorkerPool, error) {
	if repository == nil || chunks == nil || storage == nil {
		return nil, errors.New("cleanup worker dependencies must not be nil")
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &CleanupWorkerPool{
		repository: repository, chunks: chunks, storage: storage, config: config,
		queue: make(chan model.KnowledgeDocument, config.QueueSize), logf: log.Printf,
	}, nil
}

func (w *CleanupWorkerPool) Start(parent context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.started {
		return ErrWorkerAlreadyStarted
	}
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	w.cancel, w.done, w.started = cancel, make(chan struct{}), true
	var group sync.WaitGroup
	group.Add(w.config.WorkerCount + 1)
	for range w.config.WorkerCount {
		go func() { defer group.Done(); w.runWorker(ctx) }()
	}
	go func() { defer group.Done(); w.runScanner(ctx) }()
	go func() { group.Wait(); close(w.done) }()
	return nil
}

func (w *CleanupWorkerPool) Stop(ctx context.Context) error {
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

func (w *CleanupWorkerPool) runScanner(ctx context.Context) {
	w.scan(ctx)
	ticker := time.NewTicker(w.config.ScanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.scan(ctx)
		}
	}
}

func (w *CleanupWorkerPool) scan(ctx context.Context) {
	documents, err := w.repository.ListCleanupDue(ctx, time.Now(), w.config.QueueSize)
	if err != nil {
		if ctx.Err() == nil {
			w.logf("knowledge cleanup worker: scan failed")
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

func (w *CleanupWorkerPool) runWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case document := <-w.queue:
			w.process(ctx, document)
		}
	}
}

func (w *CleanupWorkerPool) process(ctx context.Context, document model.KnowledgeDocument) {
	now := time.Now()
	claimed, err := w.repository.ClaimCleanup(ctx, document.ID, document.UserID, document.Version, now, now.Add(time.Minute))
	if err != nil || !claimed {
		return
	}
	stage := "vector"
	err = w.deleteChunks(ctx, document)
	if err == nil {
		stage = "file"
		if document.IndexStatus == model.KnowledgeStatusDeleting {
			err = w.storage.DeleteDocument(document.UserID, document.ID)
		} else {
			err = w.storage.Delete(document.UserID, document.ID, document.CleanupStoragePath)
		}
	}
	if err == nil {
		if completeErr := w.repository.CompleteCleanup(ctx, document); completeErr != nil && ctx.Err() == nil {
			w.logf("knowledge cleanup worker: complete document %s failed", document.ID)
		}
		return
	}
	if ctx.Err() != nil {
		return // the one-minute lease makes interrupted cleanup retryable
	}
	retryAt := time.Now().Add(cleanupBackoff(document.CleanupAttempts + 1))
	message := "cleanup: " + stage + " delete failed"
	if failErr := w.repository.FailCleanup(ctx, document, message, retryAt); failErr != nil {
		w.logf("knowledge cleanup worker: persist failure for document %s failed", document.ID)
	}
}

func (w *CleanupWorkerPool) deleteChunks(ctx context.Context, document model.KnowledgeDocument) error {
	if document.IndexStatus == model.KnowledgeStatusCleanupPending {
		if document.CleanupVersion <= 0 {
			return nil
		}
		return w.chunks.DeleteDocumentVersion(ctx, document.UserID, document.ID, document.CleanupVersion)
	}
	for version := 1; version <= document.Version; version++ {
		if err := w.chunks.DeleteDocumentVersion(ctx, document.UserID, document.ID, version); err != nil {
			return err
		}
	}
	return nil
}

func cleanupBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 8 {
		attempt = 8
	}
	return time.Duration(1<<uint(attempt-1)) * time.Second
}
