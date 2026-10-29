package knowledge_test

import (
	"context"
	"errors"
	"testing"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/knowledge"
	"github.com/xiaopang456/GopherLearn/internal/knowledge/knowledgetest"
	"github.com/xiaopang456/GopherLearn/internal/repository"
)

func TestMemoryStoreContract(t *testing.T) {
	newStore := func(t *testing.T, limits knowledge.Limits) knowledge.Store {
		return knowledge.NewMemoryStore().WithLimits(limits)
	}
	knowledgetest.Suite(t, newStore)
	knowledgetest.LifecycleSuite(t, newStore)
}

func TestChunkSourcePassesTheAuthorizedScope(t *testing.T) {
	ctx := context.Background()
	store := knowledge.NewMemoryStore()
	collection, err := store.CreateCollection(ctx, &domain.Collection{UserID: "user-1", Title: "手册"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateDocument(ctx, "user-1", &domain.Document{
		CollectionID: collection.ID, Title: "redis.md", Format: domain.DocumentFormatMarkdown,
	}, []domain.DocumentChunk{{Text: "AOF 追加写"}}); err != nil {
		t.Fatal(err)
	}

	source := knowledge.NewChunkSource(store)
	chunksRead, err := source.Chunks(ctx, "user-1", []string{collection.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(chunksRead) != 1 {
		t.Fatalf("chunks = %d, want 1", len(chunksRead))
	}
	if _, err := source.Chunks(ctx, " ", []string{collection.ID}); !errors.Is(err, repository.ErrInvalidArgument) {
		t.Fatalf("blank principal error = %v, want ErrInvalidArgument", err)
	}
	if _, err := source.Chunks(ctx, "user-2", []string{collection.ID}); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("cross-user error = %v, want ErrNotFound", err)
	}
	if _, err := (&knowledge.ChunkSource{}).Chunks(ctx, "user-1", []string{collection.ID}); !errors.Is(err, knowledge.ErrNotConfigured) {
		t.Fatalf("unconfigured source error = %v, want knowledge.ErrNotConfigured", err)
	}
}
