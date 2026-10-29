package model

import "testing"

func TestKnowledgeDocumentPrepareForCreate(t *testing.T) {
	document := KnowledgeDocument{OriginalName: "gin.md"}
	document.PrepareForCreate()
	if document.ActiveName == nil || *document.ActiveName != "gin.md" {
		t.Fatalf("active name = %v", document.ActiveName)
	}
	if document.Version != 1 || document.IndexStatus != KnowledgeStatusPending {
		t.Fatalf("defaults not applied: %+v", document)
	}
}

func TestKnowledgeDocumentStatusValidation(t *testing.T) {
	for _, status := range []KnowledgeDocumentStatus{
		KnowledgeStatusPending, KnowledgeStatusIndexing, KnowledgeStatusReady,
		KnowledgeStatusFailed, KnowledgeStatusCleanupPending,
		KnowledgeStatusDeleting, KnowledgeStatusDeleted,
	} {
		if !status.Valid() {
			t.Fatalf("expected %q to be valid", status)
		}
	}
	if KnowledgeDocumentStatus("unknown").Valid() {
		t.Fatal("unknown status must be invalid")
	}
}
