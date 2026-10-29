package knowledge

import (
	"context"
	"testing"

	"github.com/xiaopang456/GopherLearn/internal/domain"
)

// TestMemoryStoreDeleteDocumentErasesEveryTrace is white-box on purpose: the
// guarantee being checked is that deletion drops the document from all three
// structures at once, and `documents`, `chunks` and `documentOrder` are
// unexported. A black-box test can only observe ListDocuments, which would still
// pass if a retired document's chunks stayed behind and kept being retrieved.
func TestMemoryStoreDeleteDocumentErasesEveryTrace(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()

	collection, err := store.CreateCollection(ctx, &domain.Collection{UserID: "user-1", Title: "手册"})
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}

	victim, err := store.CreateDocument(ctx, "user-1", &domain.Document{
		CollectionID: collection.ID,
		Title:        "to-delete.md",
		Format:       domain.DocumentFormatMarkdown,
	}, []domain.DocumentChunk{{Text: "这段内容删除后不该再被检索到"}})
	if err != nil {
		t.Fatalf("CreateDocument victim: %v", err)
	}
	survivor, err := store.CreateDocument(ctx, "user-1", &domain.Document{
		CollectionID: collection.ID,
		Title:        "to-keep.md",
		Format:       domain.DocumentFormatMarkdown,
	}, []domain.DocumentChunk{{Text: "保留文档的第一段"}, {Text: "保留文档的第二段"}})
	if err != nil {
		t.Fatalf("CreateDocument survivor: %v", err)
	}

	// Guard the premise: all three structures really did hold the victim first.
	store.mu.RLock()
	_, inDocuments := store.documents[victim.ID]
	inChunks := len(store.chunks[victim.ID]) > 0
	inOrder := containsID(store.documentOrder, victim.ID)
	store.mu.RUnlock()
	if !inDocuments || !inChunks || !inOrder {
		t.Fatalf("precondition: documents=%v chunks=%v order=%v, want all true",
			inDocuments, inChunks, inOrder)
	}

	if err := store.DeleteDocument(ctx, "user-1", victim.ID); err != nil {
		t.Fatalf("DeleteDocument: %v", err)
	}

	store.mu.RLock()
	defer store.mu.RUnlock()

	if _, exists := store.documents[victim.ID]; exists {
		t.Errorf("documents still holds %s", victim.ID)
	}
	if chunks, exists := store.chunks[victim.ID]; exists {
		t.Errorf("chunks still holds %s (%d chunks), so retrieval could still read it",
			victim.ID, len(chunks))
	}
	if containsID(store.documentOrder, victim.ID) {
		t.Errorf("documentOrder still lists %s, so ListDocuments would still return it",
			victim.ID)
	}

	// Deletion must not take anything else with it.
	if _, exists := store.documents[survivor.ID]; !exists {
		t.Errorf("survivor %s disappeared from documents", survivor.ID)
	}
	if len(store.chunks[survivor.ID]) != 2 {
		t.Errorf("survivor chunks = %d, want 2", len(store.chunks[survivor.ID]))
	}
	if !containsID(store.documentOrder, survivor.ID) {
		t.Errorf("survivor %s disappeared from documentOrder", survivor.ID)
	}
	if len(store.documentOrder) != 1 {
		t.Errorf("documentOrder length = %d, want 1", len(store.documentOrder))
	}
}

func containsID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}
