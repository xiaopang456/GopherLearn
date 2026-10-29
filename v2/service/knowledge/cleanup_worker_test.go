package knowledge

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"GopherAI/model"
)

func TestCleanupWorkerRemovesOnlySupersededVersion(t *testing.T) {
	storage, oldPath, newPath := cleanupStorageVersions(t)
	document := model.KnowledgeDocument{
		ID: "doc-cleanup", UserID: 73, Version: 2, ActiveVersion: 2,
		IndexStatus: model.KnowledgeStatusCleanupPending,
		StoragePath: newPath, CleanupVersion: 1, CleanupStoragePath: oldPath,
	}
	repository := newFakeCleanupRepository(document)
	chunks := &fakeChunkCleaner{}
	worker := mustCleanupWorker(t, repository, chunks, storage)
	worker.process(context.Background(), document)

	stored := repository.snapshot()
	if stored.IndexStatus != model.KnowledgeStatusReady || stored.CleanupVersion != 0 {
		t.Fatalf("cleanup result = %+v", stored)
	}
	if len(chunks.versions) != 1 || chunks.versions[0] != 1 {
		t.Fatalf("deleted chunk versions = %v", chunks.versions)
	}
	if _, err := storage.Open(73, document.ID, oldPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old source should be deleted, error = %v", err)
	}
	if file, err := storage.Open(73, document.ID, newPath); err != nil {
		t.Fatalf("new source must remain: %v", err)
	} else {
		_ = file.Close()
	}
}

func TestCleanupWorkerDeletesEveryVersionAndDocumentDirectory(t *testing.T) {
	storage, _, newPath := cleanupStorageVersions(t)
	document := model.KnowledgeDocument{
		ID: "doc-cleanup", UserID: 73, Version: 2, ActiveVersion: 2,
		IndexStatus: model.KnowledgeStatusDeleting, StoragePath: newPath,
	}
	repository := newFakeCleanupRepository(document)
	chunks := &fakeChunkCleaner{}
	worker := mustCleanupWorker(t, repository, chunks, storage)
	worker.process(context.Background(), document)

	if stored := repository.snapshot(); stored.IndexStatus != model.KnowledgeStatusDeleted || stored.StoragePath != "" {
		t.Fatalf("delete result = %+v", stored)
	}
	if len(chunks.versions) != 2 || chunks.versions[0] != 1 || chunks.versions[1] != 2 {
		t.Fatalf("deleted versions = %v", chunks.versions)
	}
	if _, err := storage.Open(73, document.ID, newPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("document directory should be deleted, error = %v", err)
	}
}

func TestCleanupWorkerPersistsSafeFailureAndBoundedBackoff(t *testing.T) {
	storage, oldPath, newPath := cleanupStorageVersions(t)
	document := model.KnowledgeDocument{
		ID: "doc-cleanup", UserID: 73, Version: 2, ActiveVersion: 2,
		IndexStatus: model.KnowledgeStatusCleanupPending,
		StoragePath: newPath, CleanupVersion: 1, CleanupStoragePath: oldPath, CleanupAttempts: 20,
	}
	repository := newFakeCleanupRepository(document)
	chunks := &fakeChunkCleaner{err: errors.New("redis password=secret private content")}
	worker := mustCleanupWorker(t, repository, chunks, storage)
	before := time.Now()
	worker.process(context.Background(), document)
	stored := repository.snapshot()
	if stored.IndexStatus != model.KnowledgeStatusCleanupPending || stored.CleanupAttempts != 21 {
		t.Fatalf("failed cleanup state = %+v", stored)
	}
	if stored.ErrorMessage != "cleanup: vector delete failed" {
		t.Fatalf("unsafe cleanup error = %q", stored.ErrorMessage)
	}
	if stored.CleanupAfter == nil || stored.CleanupAfter.Sub(before) > 129*time.Second {
		t.Fatalf("backoff was not capped: %v", stored.CleanupAfter)
	}
}

func TestCleanupWorkerRetriesFileDeletionFailure(t *testing.T) {
	storage, oldPath, newPath := cleanupStorageVersions(t)
	document := model.KnowledgeDocument{
		ID: "doc-cleanup", UserID: 73, Version: 2, ActiveVersion: 2,
		IndexStatus: model.KnowledgeStatusCleanupPending,
		StoragePath: newPath, CleanupVersion: 1, CleanupStoragePath: oldPath,
	}
	repository := newFakeCleanupRepository(document)
	worker := mustCleanupWorker(t, repository, &fakeChunkCleaner{}, &failingDeleteStorage{FileStorage: storage})
	worker.process(context.Background(), document)
	stored := repository.snapshot()
	if stored.IndexStatus != model.KnowledgeStatusCleanupPending || stored.CleanupAttempts != 1 || stored.ErrorMessage != "cleanup: file delete failed" {
		t.Fatalf("file cleanup failure = %+v", stored)
	}
}

func TestCleanupWorkerRetriesVectorFailureThenCompletes(t *testing.T) {
	storage, oldPath, newPath := cleanupStorageVersions(t)
	document := model.KnowledgeDocument{
		ID: "doc-cleanup", UserID: 73, Version: 2, ActiveVersion: 2,
		IndexStatus: model.KnowledgeStatusCleanupPending,
		StoragePath: newPath, CleanupVersion: 1, CleanupStoragePath: oldPath,
	}
	repository := newFakeCleanupRepository(document)
	chunks := &fakeChunkCleaner{err: errors.New("redis unavailable")}
	worker := mustCleanupWorker(t, repository, chunks, storage)
	worker.process(context.Background(), document)
	failed := repository.snapshot()
	if failed.IndexStatus != model.KnowledgeStatusCleanupPending || failed.CleanupAttempts != 1 || failed.CleanupVersion != 1 {
		t.Fatalf("first cleanup attempt = %+v", failed)
	}
	if file, err := storage.Open(73, document.ID, oldPath); err != nil {
		t.Fatalf("old source must remain while vector cleanup failed: %v", err)
	} else {
		_ = file.Close()
	}

	chunks.err = nil
	worker.process(context.Background(), failed)
	completed := repository.snapshot()
	if completed.IndexStatus != model.KnowledgeStatusReady || completed.CleanupVersion != 0 || completed.CleanupAttempts != 0 {
		t.Fatalf("retried cleanup = %+v", completed)
	}
	if _, err := storage.Open(73, document.ID, oldPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old source still exists after successful retry: %v", err)
	}
	if file, err := storage.Open(73, document.ID, newPath); err != nil {
		t.Fatalf("active source must survive cleanup retry: %v", err)
	} else {
		_ = file.Close()
	}
}

type fakeCleanupRepository struct {
	mu       sync.Mutex
	document model.KnowledgeDocument
}

func newFakeCleanupRepository(document model.KnowledgeDocument) *fakeCleanupRepository {
	return &fakeCleanupRepository{document: document}
}

func (f *fakeCleanupRepository) ListCleanupDue(context.Context, time.Time, int) ([]model.KnowledgeDocument, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return []model.KnowledgeDocument{f.document}, nil
}

func (f *fakeCleanupRepository) ClaimCleanup(_ context.Context, id string, userID int64, version int, _, leaseUntil time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.document.ID != id || f.document.UserID != userID || f.document.Version != version {
		return false, nil
	}
	f.document.CleanupAfter = &leaseUntil
	return true, nil
}

func (f *fakeCleanupRepository) CompleteCleanup(_ context.Context, document model.KnowledgeDocument) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if document.IndexStatus == model.KnowledgeStatusDeleting {
		f.document.IndexStatus, f.document.StoragePath = model.KnowledgeStatusDeleted, ""
	} else {
		f.document.IndexStatus = model.KnowledgeStatusReady
	}
	f.document.CleanupVersion, f.document.CleanupStoragePath, f.document.CleanupAttempts = 0, "", 0
	f.document.CleanupAfter, f.document.ErrorMessage = nil, ""
	return nil
}

func (f *fakeCleanupRepository) FailCleanup(_ context.Context, _ model.KnowledgeDocument, message string, retryAt time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.document.CleanupAttempts++
	f.document.CleanupAfter, f.document.ErrorMessage = &retryAt, message
	return nil
}

func (f *fakeCleanupRepository) snapshot() model.KnowledgeDocument {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.document
}

type fakeChunkCleaner struct {
	versions []int
	err      error
}

type failingDeleteStorage struct{ FileStorage }

func (s *failingDeleteStorage) Delete(int64, string, string) error {
	return errors.New("private source path")
}

func (s *failingDeleteStorage) DeleteDocument(int64, string) error {
	return errors.New("private source path")
}

func (f *fakeChunkCleaner) DeleteDocumentVersion(_ context.Context, _ int64, _ string, version int) error {
	f.versions = append(f.versions, version)
	return f.err
}

func mustCleanupWorker(t *testing.T, repository CleanupRepository, chunks ChunkCleaner, storage FileStorage) *CleanupWorkerPool {
	t.Helper()
	worker, err := NewCleanupWorkerPool(repository, chunks, storage, WorkerConfig{WorkerCount: 1, QueueSize: 1, ScanInterval: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	worker.logf = func(string, ...any) {}
	return worker
}

func cleanupStorageVersions(t *testing.T) (*LocalFileStorage, string, string) {
	t.Helper()
	storage, err := NewLocalFileStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	publish := func(version int, content string) string {
		stored, saveErr := storage.SaveTemp(context.Background(), 73, "doc-cleanup", strings.NewReader(content))
		if saveErr != nil {
			t.Fatal(saveErr)
		}
		path, publishErr := storage.Publish(stored.TempPath, 73, "doc-cleanup", version, "md")
		if publishErr != nil {
			t.Fatal(publishErr)
		}
		return path
	}
	return storage, publish(1, "old"), publish(2, "new")
}
