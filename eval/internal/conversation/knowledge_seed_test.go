package conversation

import (
	"GopherAI/model"
	"testing"
)

// A scenario attempt carries no knowledge seed, so the only thing that can reach
// the product's knowledge_documents table is an empty list. If nil ever started
// to mean "seed something", scenario users would inherit the shared corpus and
// the conversation history would no longer be the only variable.
func TestScenarioSeedCarriesNoDocuments(t *testing.T) {
	var absent *KnowledgeSeed
	if got := absent.documents(); len(got) != 0 {
		t.Fatalf("a missing seed produced %d documents", len(got))
	}
	for _, seed := range []*KnowledgeSeed{{}, {UserID: 7, UserName: "l4_owner"}} {
		if got := seed.documents(); len(got) != 0 {
			t.Fatalf("an empty seed produced %d documents", len(got))
		}
	}
}

// A seed that does carry documents must keep them attached to their own user,
// which is what lets the evaluator refuse a seed from another identity.
func TestKnowledgeDocumentRowsKeepTheirOwnUser(t *testing.T) {
	seed := KnowledgeSeed{UserID: 7, Documents: []KnowledgeDocument{{
		ID: "doc-1", UserID: 7, OriginalName: "one.md", ActiveName: "one.md", ActiveHash: "hash",
		StoragePath: "one.md", ContentHash: "hash", FileSize: 12, Version: 1, ActiveVersion: 1,
		IndexStatus: string(model.KnowledgeStatusReady),
	}}}
	rows := seed.documents()
	if len(rows) != 1 {
		t.Fatalf("rows = %d want 1", len(rows))
	}
	if rows[0].UserID != 7 || rows[0].ID != "doc-1" {
		t.Fatalf("row identity = %d/%s", rows[0].UserID, rows[0].ID)
	}
	if rows[0].ActiveName == nil || *rows[0].ActiveName != "one.md" {
		t.Fatalf("active name = %+v", rows[0].ActiveName)
	}
}
