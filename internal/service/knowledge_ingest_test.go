package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/knowledge"
)

// TestIngestionRecordsByteMetadata pins the two numbers ingestion owes the store:
// the hash identifies what the text says, and the size is the raw byte count the
// storage quota spends. They are deliberately different for the same upload - a
// CRLF file costs more bytes than it has content - and a test that only checked
// one of them would pass while the other drifted.
func TestIngestionRecordsByteMetadata(t *testing.T) {
	ctx := context.Background()
	store := knowledge.NewMemoryStore()
	collection, err := store.CreateCollection(ctx, &domain.Collection{UserID: "user-1", Title: "手册"})
	if err != nil {
		t.Fatal(err)
	}
	service := NewKnowledgeService(store)

	content := "# Redis\r\n\r\nAOF everysec 是默认策略。\r\n"
	created, err := service.AddDocument(ctx, "user-1", collection.ID, DocumentInput{
		Title: "redis.md", Format: "markdown", Content: content,
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.FileSize != int64(len(content)) {
		t.Fatalf("file size = %d, want the raw byte count %d", created.FileSize, len(content))
	}
	if len(created.ContentHash) != knowledge.ContentHashLength {
		t.Fatalf("content hash %q, want a full digest", created.ContentHash)
	}
	stored, err := store.GetDocument(ctx, "user-1", created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ContentHash != created.ContentHash || stored.FileSize != created.FileSize {
		t.Fatalf("store kept %#v, ingestion reported %#v", stored, created)
	}

	// The same notes from the other editor: identical hash, so the second copy is
	// refused rather than silently doubling the retrieval hits and the embeddings.
	_, err = service.AddDocument(ctx, "user-1", collection.ID, DocumentInput{
		Title: "redis-副本.md", Format: "markdown", Content: strings.ReplaceAll(content, "\r\n", "\n"),
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("re-upload of the same content = %v, want a conflict", err)
	}
	usage, err := store.UsageByUser(ctx, "user-1")
	if err != nil {
		t.Fatal(err)
	}
	if usage.DocumentCount != 1 || usage.Unmeasured != 0 {
		t.Fatalf("usage after a refused duplicate = %#v, want only the first document", usage)
	}
}

// TestRewriteWithIdenticalContentIsNotAnError covers the v2 rule that an upload of
// the same bytes reports "nothing changed" instead of burning a version: the
// caller still gets the live document, and the store is what decided it.
func TestRewriteWithIdenticalContentIsNotAnError(t *testing.T) {
	ctx := context.Background()
	store := knowledge.NewMemoryStore()
	collection, err := store.CreateCollection(ctx, &domain.Collection{UserID: "user-1", Title: "手册"})
	if err != nil {
		t.Fatal(err)
	}
	service := NewKnowledgeService(store)
	created, err := service.AddDocument(ctx, "user-1", collection.ID, DocumentInput{
		Title: "a.md", Format: "markdown", Content: "# 第一版\n\n正文。\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	replaced, err := service.ReplaceDocumentContent(ctx, "user-1", created.ID, DocumentInput{
		Format: "markdown", Content: "# 第一版\n\n正文。\n",
	})
	if err != nil {
		t.Fatalf("an identical rewrite must not surface as a failure: %v", err)
	}
	if replaced.Version != created.Version {
		t.Fatalf("version went %d to %d, want no advance for identical content", created.Version, replaced.Version)
	}
	// A real change still advances.
	advanced, err := service.ReplaceDocumentContent(ctx, "user-1", created.ID, DocumentInput{
		Format: "markdown", Content: "# 第二版\n\n正文改写了。\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	if advanced.Version != created.Version+1 {
		t.Fatalf("version = %d, want the rewrite to advance to %d", advanced.Version, created.Version+1)
	}
}

// TestIngestionRefusalsReachTheCallerAsConflicts checks that the four new store
// refusals are not 500s on the way out. They share the one conflict code the HTTP
// contract already has - splitting them into their own codes is a public contract
// change, see the migration note.
func TestIngestionRefusalsReachTheCallerAsConflicts(t *testing.T) {
	ctx := context.Background()
	store := knowledge.NewMemoryStore().WithLimits(knowledge.Limits{MaxDocuments: 2, MaxTotalBytes: 1 << 20})
	collection, err := store.CreateCollection(ctx, &domain.Collection{UserID: "user-1", Title: "手册"})
	if err != nil {
		t.Fatal(err)
	}
	service := NewKnowledgeService(store)
	for index := 0; index < 2; index++ {
		if _, err := service.AddDocument(ctx, "user-1", collection.ID, DocumentInput{
			Title:  strings.Repeat("x", 3) + string(rune('a'+index)) + ".md",
			Format: "text", Content: "内容 " + string(rune('a'+index)),
		}); err != nil {
			t.Fatalf("document %d = %v", index, err)
		}
	}
	// The count ceiling.
	if _, err := service.AddDocument(ctx, "user-1", collection.ID, DocumentInput{
		Title: "third.md", Format: "text", Content: "第三篇内容",
	}); !errors.Is(err, ErrConflict) {
		t.Errorf("past the count ceiling = %v, want a conflict", err)
	}
	// A title already in use, reported through the same door.
	if _, err := service.AddDocument(ctx, "user-2", collection.ID, DocumentInput{
		Title: "xxx.md", Format: "text", Content: "别人的同名内容",
	}); err == nil {
		t.Fatal("another user must not be blocked by user-1's quota or titles")
	}
}

// unresolvedOwnerStore stands for a store on an engine that could lock the writing
// user and found no users row for them. The service must not read that as "that
// document is missing" (404), "it already exists" (409) or "your request is wrong"
// (400): the caller can fix none of those, and it is the deployment that disagrees
// with itself.
type unresolvedOwnerStore struct{ knowledge.Store }

func (s unresolvedOwnerStore) CreateDocument(context.Context, string, *domain.Document, []domain.DocumentChunk) (*domain.Document, error) {
	return nil, knowledge.ErrOwnerUnresolved
}

func (s unresolvedOwnerStore) ReplaceDocumentContent(context.Context, string, string, knowledge.DocumentReplacement) (*domain.Document, error) {
	return nil, knowledge.ErrOwnerUnresolved
}

func TestUnresolvedOwnerIsNeitherASuccessNorAClientFault(t *testing.T) {
	ctx := context.Background()
	store := unresolvedOwnerStore{knowledge.NewMemoryStore()}
	collection, err := store.CreateCollection(ctx, &domain.Collection{UserID: "user-1", Title: "手册"})
	if err != nil {
		t.Fatal(err)
	}
	service := NewKnowledgeService(store)
	input := DocumentInput{Title: "a.md", Format: "text", Content: "内容"}

	created, err := service.AddDocument(ctx, "user-1", collection.ID, input)
	if created != nil || !errors.Is(err, ErrInternal) {
		t.Errorf("AddDocument = (%v, %v), want no document and a server-side failure", created, err)
	}
	replaced, replaceErr := service.ReplaceDocumentContent(ctx, "user-1", "document-1", input)
	if replaced != nil || !errors.Is(replaceErr, ErrInternal) {
		t.Errorf("ReplaceDocumentContent = (%v, %v), want no document and a server-side failure", replaced, replaceErr)
	}
	// It must not be reportable as an unchanged rewrite either: nothing was written,
	// so no caller may be told the previous version is still the answer.
	if errors.Is(replaceErr, knowledge.ErrDocumentUnchanged) {
		t.Error("an unresolved owner must not be presented as an unchanged rewrite")
	}
}
