package agenttool

import "testing"

// STABILIZE-063: the three source-priority contracts are frozen separately and
// differ from the explicit-sources contracts they replace.
func TestSourcePriorityDescriptionsDifferFromExplicitSources(t *testing.T) {
	saveOld, err := SaveMemoryDraftSchemaDigest()
	if err != nil {
		t.Fatal(err)
	}
	pastOld, err := SearchPastSessionsSchemaDigest()
	if err != nil {
		t.Fatal(err)
	}
	docOld, err := SearchUploadedDocumentsSchemaDigest()
	if err != nil {
		t.Fatal(err)
	}
	pastNew, err := SearchPastSessionsSourcePriorityDigest()
	if err != nil {
		t.Fatal(err)
	}
	docNew, err := SearchUploadedDocumentsSourcePriorityDigest()
	if err != nil {
		t.Fatal(err)
	}
	if pastNew == pastOld {
		t.Fatal("the source-priority past-session digest equals the explicit-sources one")
	}
	if docNew == docOld {
		t.Fatal("the source-priority uploaded-document digest equals the explicit-sources one")
	}
	if pastNew == docNew {
		t.Fatal("the two source-priority digests are equal")
	}
	if saveOld == pastOld || saveOld == docOld {
		t.Fatal("the draft digest collides with a read digest")
	}
}
