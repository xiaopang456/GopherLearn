package knowledge

import (
	"GopherAI/config"
	daoknowledge "GopherAI/dao/knowledge"
	"GopherAI/model"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestDocumentServiceCreatePending(t *testing.T) {
	storage, err := NewLocalFileStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repository := &fakeDocumentRepository{}
	service := NewDocumentService(repository, storage, testKnowledgeLimits())
	service.newID = func() string { return "doc-123" }

	document, err := service.CreatePending(context.Background(), 73, "gin.md", "md", strings.NewReader("# Gin\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if document.IndexStatus != model.KnowledgeStatusPending || document.ActiveVersion != 0 {
		t.Fatalf("new document lifecycle = %+v", document)
	}
	if repository.created != document {
		t.Fatal("repository did not receive the document")
	}
	file, err := storage.Open(73, "doc-123", document.StoragePath)
	if err != nil {
		t.Fatal(err)
	}
	content, _ := io.ReadAll(file)
	_ = file.Close()
	if string(content) != "# Gin\r\n" {
		t.Fatalf("stored content = %q", content)
	}
}

func TestDocumentServiceFileFailureDoesNotCreateMetadata(t *testing.T) {
	storage, err := NewLocalFileStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repository := &fakeDocumentRepository{}
	service := NewDocumentService(repository, storage, testKnowledgeLimits())
	service.newID = func() string { return "doc-123" }

	_, err = service.CreatePending(context.Background(), 73, "gin.md", "md", errorReader{})
	if err == nil {
		t.Fatal("expected file failure")
	}
	if repository.created != nil {
		t.Fatal("metadata must not be created after file failure")
	}
}

func TestDocumentServiceDatabaseFailureCleansPublishedFile(t *testing.T) {
	storage, err := NewLocalFileStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repository := &fakeDocumentRepository{err: errors.New("database failed")}
	service := NewDocumentService(repository, storage, testKnowledgeLimits())
	service.newID = func() string { return "doc-123" }

	document, err := service.CreatePending(context.Background(), 73, "gin.md", "md", strings.NewReader("content"))
	if err == nil || document != nil {
		t.Fatalf("create result = (%+v, %v)", document, err)
	}
	if _, openErr := storage.Open(73, "doc-123", "73/doc-123/source-v1.md"); openErr == nil {
		t.Fatal("published file must be removed after database failure")
	}
}

func TestDocumentServiceCreateDocumentValidation(t *testing.T) {
	tests := []struct {
		name      string
		filename  string
		content   string
		configure func(*fakeDocumentRepository)
		wantError error
	}{
		{name: "unsupported type", filename: "note.pdf", content: "x", wantError: ErrUnsupportedFile},
		{name: "empty", filename: "note.md", content: "", wantError: ErrEmptyDocument},
		{name: "too large", filename: "note.md", content: strings.Repeat("x", 1025), wantError: ErrFileTooLarge},
		{name: "document limit", filename: "note.md", content: "x", configure: func(r *fakeDocumentRepository) {
			r.usage.DocumentCount = 3
		}, wantError: ErrDocumentLimit},
		{name: "storage limit", filename: "note.md", content: "x", configure: func(r *fakeDocumentRepository) {
			r.usage.TotalBytes = 2048
		}, wantError: ErrStorageLimit},
		{name: "name conflict", filename: "note.md", content: "x", configure: func(r *fakeDocumentRepository) {
			r.byName = &model.KnowledgeDocument{}
		}, wantError: ErrDocumentNameExists},
		{name: "content conflict", filename: "note.md", content: "x", configure: func(r *fakeDocumentRepository) {
			r.byHash = &model.KnowledgeDocument{}
		}, wantError: ErrDocumentHashExists},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			storage, err := NewLocalFileStorage(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			repository := &fakeDocumentRepository{}
			if tt.configure != nil {
				tt.configure(repository)
			}
			service := NewDocumentService(repository, storage, testKnowledgeLimits())
			_, err = service.CreateDocument(context.Background(), 73, tt.filename, strings.NewReader(tt.content))
			if !errors.Is(err, tt.wantError) {
				t.Fatalf("CreateDocument() error = %v, want %v", err, tt.wantError)
			}
		})
	}
}

func TestDocumentServiceUpdateUnchangedSkipsNewVersion(t *testing.T) {
	storage, err := NewLocalFileStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("# Gin\nold content\n")
	name, hash := "gin.md", ContentHash(content)
	repository := &fakeDocumentRepository{docs: []model.KnowledgeDocument{{
		ID: "doc-update", UserID: 73, OriginalName: name, ActiveName: &name,
		ContentHash: hash, Version: 1, ActiveVersion: 1, IndexStatus: model.KnowledgeStatusReady,
	}}}
	service := NewDocumentService(repository, storage, testKnowledgeLimits())
	document, unchanged, err := service.UpdateDocument(context.Background(), 73, "doc-update", name, strings.NewReader(string(content)))
	if err != nil || !unchanged || document.Version != 1 {
		t.Fatalf("unchanged update = (%+v, %v, %v)", document, unchanged, err)
	}
	if _, err := storage.Open(73, "doc-update", "73/doc-update/source-v2.md"); err == nil {
		t.Fatal("unchanged update must not publish source-v2")
	}
}

func TestDocumentServiceUpdateStagesNewVersionAndPreservesOldSource(t *testing.T) {
	storage, err := NewLocalFileStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stored, err := storage.SaveTemp(context.Background(), 73, "doc-update", strings.NewReader("old"))
	if err != nil {
		t.Fatal(err)
	}
	oldPath, err := storage.Publish(stored.TempPath, 73, "doc-update", 1, "md")
	if err != nil {
		t.Fatal(err)
	}
	name := "gin.md"
	repository := &fakeDocumentRepository{docs: []model.KnowledgeDocument{{
		ID: "doc-update", UserID: 73, OriginalName: name, ActiveName: &name,
		StoragePath: oldPath, ContentHash: ContentHash([]byte("old")), FileSize: 3,
		Version: 1, ActiveVersion: 1, ChunkCount: 2, IndexStatus: model.KnowledgeStatusReady,
	}}}
	service := NewDocumentService(repository, storage, testKnowledgeLimits())
	document, unchanged, err := service.UpdateDocument(context.Background(), 73, "doc-update", name, strings.NewReader("new content"))
	if err != nil || unchanged || document.Version != 2 || document.ActiveVersion != 1 || document.IndexStatus != model.KnowledgeStatusPending {
		t.Fatalf("staged update = (%+v, %v, %v)", document, unchanged, err)
	}
	if document.CleanupVersion != 1 || document.CleanupStoragePath != oldPath {
		t.Fatalf("old-version cleanup metadata = %+v", document)
	}
	if old, openErr := storage.Open(73, "doc-update", oldPath); openErr != nil {
		t.Fatalf("old source must remain before new ready: %v", openErr)
	} else {
		_ = old.Close()
	}
	if next, openErr := storage.Open(73, "doc-update", document.StoragePath); openErr != nil {
		t.Fatalf("new source missing: %v", openErr)
	} else {
		_ = next.Close()
	}
}

type fakeDocumentRepository struct {
	created *model.KnowledgeDocument
	err     error
	usage   daoknowledge.Usage
	byName  *model.KnowledgeDocument
	byHash  *model.KnowledgeDocument
	docs    []model.KnowledgeDocument
}

func (f *fakeDocumentRepository) Create(_ context.Context, document *model.KnowledgeDocument) error {
	f.created = document
	return f.err
}

func (f *fakeDocumentRepository) CreateWithinLimits(ctx context.Context, document *model.KnowledgeDocument, maxDocuments int, maxTotalBytes int64) error {
	if f.usage.DocumentCount >= int64(maxDocuments) {
		return daoknowledge.ErrDocumentLimit
	}
	if f.usage.TotalBytes+document.FileSize > maxTotalBytes {
		return daoknowledge.ErrStorageLimit
	}
	return f.Create(ctx, document)
}

func (f *fakeDocumentRepository) GetByIDAndUserID(_ context.Context, id string, userID int64) (*model.KnowledgeDocument, error) {
	for i := range f.docs {
		if f.docs[i].ID == id && f.docs[i].UserID == userID {
			return &f.docs[i], nil
		}
	}
	return nil, daoknowledge.ErrDocumentNotFound
}

func (f *fakeDocumentRepository) ListByUserID(_ context.Context, userID int64) ([]model.KnowledgeDocument, error) {
	var result []model.KnowledgeDocument
	for _, document := range f.docs {
		if document.UserID == userID {
			result = append(result, document)
		}
	}
	return result, f.err
}

func (f *fakeDocumentRepository) FindActiveByName(context.Context, int64, string) (*model.KnowledgeDocument, error) {
	if f.byName != nil {
		return f.byName, nil
	}
	return nil, daoknowledge.ErrDocumentNotFound
}

func (f *fakeDocumentRepository) FindActiveByHash(context.Context, int64, string) (*model.KnowledgeDocument, error) {
	if f.byHash != nil {
		return f.byHash, nil
	}
	return nil, daoknowledge.ErrDocumentNotFound
}

func (f *fakeDocumentRepository) UsageByUserID(context.Context, int64) (daoknowledge.Usage, error) {
	return f.usage, f.err
}

func (f *fakeDocumentRepository) RetryFailed(_ context.Context, id string, userID int64) error {
	for index := range f.docs {
		if f.docs[index].ID == id && f.docs[index].UserID == userID {
			if f.docs[index].IndexStatus != model.KnowledgeStatusFailed {
				return daoknowledge.ErrDocumentNotRetryable
			}
			f.docs[index].IndexStatus = model.KnowledgeStatusPending
			f.docs[index].ErrorMessage = ""
			return nil
		}
	}
	return daoknowledge.ErrDocumentNotFound
}

func (f *fakeDocumentRepository) StageUpdate(_ context.Context, id string, userID int64, expectedVersion int, update daoknowledge.DocumentUpdate, _ int64) (*model.KnowledgeDocument, error) {
	for index := range f.docs {
		document := &f.docs[index]
		if document.ID == id && document.UserID == userID {
			if document.Version != expectedVersion {
				return nil, daoknowledge.ErrDocumentChanged
			}
			oldPath, oldActive := document.StoragePath, document.ActiveVersion
			document.OriginalName, document.StoragePath, document.FileType = update.OriginalName, update.StoragePath, update.FileType
			document.ContentHash, document.FileSize = update.ContentHash, update.FileSize
			document.Version, document.IndexStatus = expectedVersion+1, model.KnowledgeStatusPending
			document.CleanupVersion, document.CleanupStoragePath = oldActive, oldPath
			return document, nil
		}
	}
	return nil, daoknowledge.ErrDocumentNotFound
}

func (f *fakeDocumentRepository) MarkDeleting(_ context.Context, id string, userID int64) (*model.KnowledgeDocument, error) {
	for index := range f.docs {
		document := &f.docs[index]
		if document.ID == id && document.UserID == userID {
			document.IndexStatus = model.KnowledgeStatusDeleting
			document.ActiveName, document.ActiveHash = nil, nil
			return document, nil
		}
	}
	return nil, daoknowledge.ErrDocumentNotFound
}

func (f *fakeDocumentRepository) Reindex(_ context.Context, id string, userID int64) error {
	for index := range f.docs {
		if f.docs[index].ID == id && f.docs[index].UserID == userID {
			f.docs[index].IndexStatus = model.KnowledgeStatusPending
			return nil
		}
	}
	return daoknowledge.ErrDocumentNotFound
}

func testKnowledgeLimits() config.KnowledgeConfig {
	return config.KnowledgeConfig{
		MaxFileSizeBytes: 1024, MaxDocumentsPerUser: 3, MaxTotalSizeBytes: 2048,
	}
}
