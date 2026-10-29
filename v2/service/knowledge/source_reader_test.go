package knowledge

import (
	"bytes"
	"context"
	"errors"
	"testing"

	daoknowledge "GopherAI/dao/knowledge"
	"GopherAI/model"
)

func TestReadActiveSourceUsesExactCurrentVersion(t *testing.T) {
	storage, err := NewLocalFileStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("# Gin\r\n\r\nUse context cancellation.\r\n")
	path := publishTestSource(t, storage, 73, "doc-current", 1, "md", content)
	repository := &fakeDocumentRepository{docs: []model.KnowledgeDocument{{
		ID: "doc-current", UserID: 73, OriginalName: "gin.md", FileType: "md",
		StoragePath: path, ContentHash: ContentHash(content), FileSize: int64(len(content)),
		Version: 1, ActiveVersion: 1, IndexStatus: model.KnowledgeStatusReady,
	}}}
	service := NewDocumentService(repository, storage, testKnowledgeLimits())

	source, err := service.ReadActiveSource(context.Background(), 73, "doc-current", 1)
	if err != nil {
		t.Fatal(err)
	}
	if source.Content != "# Gin\n\nUse context cancellation.\n" || source.FileType != "md" || source.Version != 1 {
		t.Fatalf("source = %#v", source)
	}
	if _, err := service.ReadActiveSource(context.Background(), 73, "doc-current", 2); !errors.Is(err, ErrDocumentVersionNotActive) {
		t.Fatalf("inactive version error = %v", err)
	}
	if _, err := service.ReadActiveSource(context.Background(), 74, "doc-current", 1); !errors.Is(err, daoknowledge.ErrDocumentNotFound) {
		t.Fatalf("cross-user error = %v", err)
	}
}

func TestReadActiveSourceUsesPreservedOldVersionDuringStagedUpdate(t *testing.T) {
	storage, err := NewLocalFileStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	oldContent := []byte("old active source")
	newContent := []byte("new pending source")
	oldPath := publishTestSource(t, storage, 73, "doc-update", 1, "txt", oldContent)
	newPath := publishTestSource(t, storage, 73, "doc-update", 2, "md", newContent)
	repository := &fakeDocumentRepository{docs: []model.KnowledgeDocument{{
		ID: "doc-update", UserID: 73, OriginalName: "next.md", FileType: "md",
		StoragePath: newPath, ContentHash: ContentHash(newContent), FileSize: int64(len(newContent)),
		Version: 2, ActiveVersion: 1, CleanupVersion: 1, CleanupStoragePath: oldPath,
		IndexStatus: model.KnowledgeStatusPending,
	}}}
	service := NewDocumentService(repository, storage, testKnowledgeLimits())

	source, err := service.ReadActiveSource(context.Background(), 73, "doc-update", 1)
	if err != nil {
		t.Fatal(err)
	}
	if source.Content != string(oldContent) || source.FileType != "txt" {
		t.Fatalf("old source = %#v", source)
	}
}

func TestReadActiveSourceHonorsCancellationAndRejectsBrokenMetadata(t *testing.T) {
	storage, err := NewLocalFileStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repository := &fakeDocumentRepository{docs: []model.KnowledgeDocument{{
		ID: "doc-broken", UserID: 73, Version: 2, ActiveVersion: 1,
	}}}
	service := NewDocumentService(repository, storage, testKnowledgeLimits())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := service.ReadActiveSource(ctx, 73, "doc-broken", 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error = %v", err)
	}
	if _, err := service.ReadActiveSource(context.Background(), 73, "doc-broken", 1); !errors.Is(err, ErrDocumentSourceInvalid) {
		t.Fatalf("metadata error = %v", err)
	}
}

func publishTestSource(t *testing.T, storage *LocalFileStorage, userID int64, documentID string, version int, fileType string, content []byte) string {
	t.Helper()
	stored, err := storage.SaveTemp(context.Background(), userID, documentID, bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	path, err := storage.Publish(stored.TempPath, userID, documentID, version, fileType)
	if err != nil {
		t.Fatal(err)
	}
	return path
}
