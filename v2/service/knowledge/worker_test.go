package knowledge

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"GopherAI/common/rag"
	"GopherAI/model"
)

func TestWorkerProcessesPendingWithConfiguredConcurrency(t *testing.T) {
	t.Parallel()

	repository := newFakeWorkerRepository(5, model.KnowledgeStatusPending)
	ingestor := &controlledIngestor{started: make(chan struct{}, 10), release: make(chan struct{})}
	worker := mustWorker(t, repository, ingestor, WorkerConfig{WorkerCount: 2, QueueSize: 5, ScanInterval: 10 * time.Millisecond})
	if err := worker.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		select {
		case <-ingestor.started:
		case <-time.After(time.Second):
			t.Fatal("workers did not start two concurrent tasks")
		}
	}
	if max := ingestor.max.Load(); max != 2 {
		t.Fatalf("maximum concurrency = %d, want 2", max)
	}
	select {
	case <-ingestor.started:
		t.Fatal("third task started before one of two worker slots was released")
	case <-time.After(40 * time.Millisecond):
	}
	close(ingestor.release)
	waitWorkerStatus(t, repository, model.KnowledgeStatusReady, 5)
	stopWorker(t, worker)
	for _, document := range repository.snapshot() {
		if document.ChunkCount != 2 || document.ActiveVersion != document.Version {
			t.Fatalf("ready document metadata = %+v", document)
		}
	}
}

func TestWorkerFailurePersistsOnlySafeError(t *testing.T) {
	t.Parallel()

	repository := newFakeWorkerRepository(1, model.KnowledgeStatusPending)
	ingestor := &controlledIngestor{err: &IngestError{Stage: IngestStageEmbedding, Err: errors.New("sk-secret private document")}}
	worker := mustWorker(t, repository, ingestor, WorkerConfig{WorkerCount: 1, QueueSize: 1, ScanInterval: 10 * time.Millisecond})
	if err := worker.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitWorkerStatus(t, repository, model.KnowledgeStatusFailed, 1)
	stopWorker(t, worker)
	document := repository.snapshot()[0]
	if document.ErrorMessage != "embedding: operation failed" {
		t.Fatalf("failed error message = %q", document.ErrorMessage)
	}
}

func TestWorkerShutdownRequeuesCanceledInFlightTask(t *testing.T) {
	t.Parallel()

	repository := newFakeWorkerRepository(1, model.KnowledgeStatusPending)
	ingestor := &controlledIngestor{started: make(chan struct{}, 1), waitForCancel: true}
	worker := mustWorker(t, repository, ingestor, WorkerConfig{WorkerCount: 1, QueueSize: 1, ScanInterval: time.Hour})
	if err := worker.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ingestor.started:
	case <-time.After(time.Second):
		t.Fatal("ingestion did not start")
	}
	stopWorker(t, worker)
	waitWorkerStatus(t, repository, model.KnowledgeStatusPending, 1)
}

func TestWorkerRecoversInterruptedIndexingOnStart(t *testing.T) {
	t.Parallel()

	repository := newFakeWorkerRepository(1, model.KnowledgeStatusIndexing)
	ingestor := &controlledIngestor{}
	worker := mustWorker(t, repository, ingestor, WorkerConfig{WorkerCount: 1, QueueSize: 1, ScanInterval: 10 * time.Millisecond})
	if err := worker.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitWorkerStatus(t, repository, model.KnowledgeStatusReady, 1)
	stopWorker(t, worker)
	if repository.recovered != 1 {
		t.Fatalf("recovered rows = %d, want 1", repository.recovered)
	}
	if err := worker.Start(context.Background()); !errors.Is(err, ErrWorkerAlreadyStarted) {
		t.Fatalf("second Start() error = %v", err)
	}
}

func TestWorkerUpdateFailurePreservesOldActiveVersion(t *testing.T) {
	t.Parallel()

	repository := newFakeWorkerRepository(1, model.KnowledgeStatusPending)
	repository.documents[0].Version = 2
	repository.documents[0].ActiveVersion = 1
	ingestor := &controlledIngestor{err: &IngestError{Stage: IngestStageVectorStore, Err: errors.New("redis unavailable")}}
	worker := mustWorker(t, repository, ingestor, WorkerConfig{WorkerCount: 1, QueueSize: 1, ScanInterval: 10 * time.Millisecond})
	if err := worker.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitWorkerStatus(t, repository, model.KnowledgeStatusFailed, 1)
	stopWorker(t, worker)
	document := repository.snapshot()[0]
	if document.ActiveVersion != 1 || document.Version != 2 || document.ErrorMessage != "vector_store: operation failed" {
		t.Fatalf("failed update replaced old active version: %+v", document)
	}
}

func TestWorkerRestartRequeuesUpdateThenActivatesNewVersion(t *testing.T) {
	t.Parallel()

	repository := newFakeWorkerRepository(1, model.KnowledgeStatusPending)
	repository.documents[0].Version = 2
	repository.documents[0].ActiveVersion = 1
	interrupted := &controlledIngestor{started: make(chan struct{}, 1), waitForCancel: true}
	first := mustWorker(t, repository, interrupted, WorkerConfig{WorkerCount: 1, QueueSize: 1, ScanInterval: time.Hour})
	if err := first.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-interrupted.started:
	case <-time.After(time.Second):
		t.Fatal("update ingestion did not start")
	}
	stopWorker(t, first)
	pending := repository.snapshot()[0]
	if pending.IndexStatus != model.KnowledgeStatusPending || pending.ActiveVersion != 1 {
		t.Fatalf("interrupted update state = %+v", pending)
	}
	select {
	case <-first.done:
	default:
		t.Fatal("worker-owned goroutines were not joined by Stop")
	}

	second := mustWorker(t, repository, &controlledIngestor{}, WorkerConfig{WorkerCount: 1, QueueSize: 1, ScanInterval: 10 * time.Millisecond})
	if err := second.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitWorkerStatus(t, repository, model.KnowledgeStatusReady, 1)
	stopWorker(t, second)
	ready := repository.snapshot()[0]
	if ready.ActiveVersion != 2 || ready.ChunkCount != 2 {
		t.Fatalf("recovered update state = %+v", ready)
	}
}

func TestWorkerTaskTimeoutReleasesSlotAndPersistsSafeFailure(t *testing.T) {
	t.Parallel()

	repository := newFakeWorkerRepository(1, model.KnowledgeStatusPending)
	ingestor := &controlledIngestor{started: make(chan struct{}, 1), waitForCancel: true}
	worker := mustWorker(t, repository, ingestor, WorkerConfig{
		WorkerCount: 1, QueueSize: 1, ScanInterval: 10 * time.Millisecond, TaskTimeout: 20 * time.Millisecond,
	})
	if err := worker.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitWorkerStatus(t, repository, model.KnowledgeStatusFailed, 1)
	stopWorker(t, worker)
	document := repository.snapshot()[0]
	if document.ErrorMessage != "indexing timed out" || ingestor.current.Load() != 0 {
		t.Fatalf("timed-out task = document %+v current=%d", document, ingestor.current.Load())
	}
}

type fakeWorkerRepository struct {
	mu        sync.Mutex
	documents []model.KnowledgeDocument
	recovered int64
}

func newFakeWorkerRepository(count int, status model.KnowledgeDocumentStatus) *fakeWorkerRepository {
	documents := make([]model.KnowledgeDocument, count)
	for index := range documents {
		documents[index] = model.KnowledgeDocument{
			ID: "doc-" + string(rune('a'+index)), UserID: 73, Version: 1,
			OriginalName: "note.md", IndexStatus: status,
		}
		active := "note.md"
		documents[index].ActiveName = &active
	}
	return &fakeWorkerRepository{documents: documents}
}

func (f *fakeWorkerRepository) ListPending(_ context.Context, limit int) ([]model.KnowledgeDocument, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	result := make([]model.KnowledgeDocument, 0, limit)
	for _, document := range f.documents {
		if document.IndexStatus == model.KnowledgeStatusPending && len(result) < limit {
			result = append(result, document)
		}
	}
	return result, nil
}

func (f *fakeWorkerRepository) ClaimForIndexing(_ context.Context, id string, userID int64, version int) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for index := range f.documents {
		document := &f.documents[index]
		if document.ID == id && document.UserID == userID && document.Version == version && document.IndexStatus == model.KnowledgeStatusPending {
			document.IndexStatus = model.KnowledgeStatusIndexing
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeWorkerRepository) CompleteIndexing(_ context.Context, id string, userID int64, version int, status model.KnowledgeDocumentStatus, count int, message string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for index := range f.documents {
		document := &f.documents[index]
		if document.ID == id && document.UserID == userID && document.Version == version && document.IndexStatus == model.KnowledgeStatusIndexing {
			document.IndexStatus, document.ErrorMessage = status, message
			if status == model.KnowledgeStatusReady {
				document.ChunkCount, document.ActiveVersion = count, version
			}
			return nil
		}
	}
	return errors.New("transition rejected")
}

func (f *fakeWorkerRepository) RequeueIndexing(_ context.Context, id string, userID int64, version int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for index := range f.documents {
		document := &f.documents[index]
		if document.ID == id && document.UserID == userID && document.Version == version && document.IndexStatus == model.KnowledgeStatusIndexing {
			document.IndexStatus = model.KnowledgeStatusPending
			return nil
		}
	}
	return errors.New("transition rejected")
}

func (f *fakeWorkerRepository) RecoverInterruptedIndexing(context.Context) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var count int64
	for index := range f.documents {
		if f.documents[index].IndexStatus == model.KnowledgeStatusIndexing {
			f.documents[index].IndexStatus = model.KnowledgeStatusPending
			count++
		}
	}
	f.recovered += count
	return count, nil
}

func (f *fakeWorkerRepository) snapshot() []model.KnowledgeDocument {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]model.KnowledgeDocument(nil), f.documents...)
}

type controlledIngestor struct {
	started       chan struct{}
	release       chan struct{}
	waitForCancel bool
	err           error
	current       atomic.Int32
	max           atomic.Int32
}

func (c *controlledIngestor) Ingest(ctx context.Context, _ model.KnowledgeDocument) (rag.ChunkIndexResult, error) {
	current := c.current.Add(1)
	defer c.current.Add(-1)
	for {
		maximum := c.max.Load()
		if current <= maximum || c.max.CompareAndSwap(maximum, current) {
			break
		}
	}
	if c.started != nil {
		c.started <- struct{}{}
	}
	if c.waitForCancel {
		<-ctx.Done()
		return rag.ChunkIndexResult{}, ctx.Err()
	}
	if c.release != nil {
		select {
		case <-ctx.Done():
			return rag.ChunkIndexResult{}, ctx.Err()
		case <-c.release:
		}
	}
	if c.err != nil {
		return rag.ChunkIndexResult{}, c.err
	}
	return rag.ChunkIndexResult{TotalChunks: 2, EmbeddedChunks: 2, ReuseStatus: rag.ChunkReuseNotApplicable}, nil
}

func mustWorker(t *testing.T, repository WorkerRepository, ingestor DocumentIngestion, config WorkerConfig) *IndexWorkerPool {
	t.Helper()
	worker, err := NewIndexWorkerPool(repository, ingestor, config)
	if err != nil {
		t.Fatal(err)
	}
	worker.logf = func(string, ...any) {}
	return worker
}

func waitWorkerStatus(t *testing.T, repository *fakeWorkerRepository, status model.KnowledgeDocumentStatus, count int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		matched := 0
		for _, document := range repository.snapshot() {
			if document.IndexStatus == status {
				matched++
			}
		}
		if matched == count {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("did not observe %d documents in status %s: %+v", count, status, repository.snapshot())
}

func stopWorker(t *testing.T, worker *IndexWorkerPool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := worker.Stop(ctx); err != nil {
		t.Fatalf("Stop() error: %v", err)
	}
}
