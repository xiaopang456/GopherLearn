package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestReUploadingStoredContentIsRefused checks the duplicate-content rule where a
// client actually meets it: over HTTP, on the collection's document route. The
// refusal must also store nothing, which is what the listing proves - a 409 that
// left a second row behind would double every hit for that text.
func TestReUploadingStoredContentIsRefused(t *testing.T) {
	fixture := newFixture(t, apiResponder{})
	token := fixtureToken(t, fixture, fixture.owner)

	response := postDocument(t, fixture, token, fixture.collection, "redis-persistence-copy.md", fixtureMarkdown)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"code":"conflict"`) {
		t.Fatalf("re-upload of stored content = %d %s, want 409 conflict", response.Code, response.Body.String())
	}
	if listed := listDocuments(t, fixture, token, fixture.collection); len(listed) != 1 || listed[0].ID != fixture.document {
		t.Fatalf("listing after a refused duplicate = %#v, want only the seeded document", listed)
	}

	// The same title over a different text is refused too: v2 keeps one live name
	// per user so a listing cannot show two documents the reader cannot tell apart.
	sameTitle := postDocument(t, fixture, token, fixture.collection, "redis-persistence.md", "# 另一份笔记\n\n内容并不相同，只是名字撞上了。\n")
	if sameTitle.Code != http.StatusConflict {
		t.Fatalf("reused title = %d %s, want 409", sameTitle.Code, sameTitle.Body.String())
	}
}

// TestAnotherUserMayStoreTheSameTitleAndBytes is the isolation half of the rule:
// uniqueness is per user, so a second person's copy of the same notes is a normal
// upload. A global uniqueness index would have made the knowledge base answer to
// whoever got there first.
func TestAnotherUserMayStoreTheSameTitleAndBytes(t *testing.T) {
	fixture := newFixture(t, apiResponder{})
	other := fixtureToken(t, fixture, fixture.other)

	response := postDocument(t, fixture, other, fixture.foreignCollection, "redis-persistence.md", fixtureMarkdown)
	if response.Code != http.StatusOK {
		t.Fatalf("another user storing identical bytes = %d %s, want it accepted", response.Code, response.Body.String())
	}
	listed := listDocuments(t, fixture, other, fixture.foreignCollection)
	if len(listed) != 2 {
		t.Fatalf("the other user's listing = %#v, want their own document plus the copy", listed)
	}
	// The seeded document of the first user is still the only one there, so the
	// accepted copy did not travel sideways through the duplicate check.
	if ownerListing := listDocuments(t, fixture, fixtureToken(t, fixture, fixture.owner), fixture.collection); len(ownerListing) != 1 {
		t.Fatalf("owner listing = %#v, want the copy to belong to nobody else", ownerListing)
	}
}

// TestRewriteWithIdenticalContentKeepsTheVersion is the v2 "unchanged" rule seen
// from the client side: an upload of the same bytes answers 200 and leaves the
// version where it was, so no rewrite, no new chunk ids and no re-embedding of
// text that did not change. The HTTP shape is unchanged - the version number is
// already part of the response - so this needed no contract decision.
func TestRewriteWithIdenticalContentKeepsTheVersion(t *testing.T) {
	fixture := newFixture(t, apiResponder{})
	token := fixtureToken(t, fixture, fixture.owner)

	body, err := json.Marshal(map[string]string{"format": "markdown", "content": fixtureMarkdown})
	if err != nil {
		t.Fatal(err)
	}
	response := doJSON(t, fixture.server, http.MethodPut, "/api/v1/documents/"+fixture.document, token, string(body))
	if response.Code != http.StatusOK {
		t.Fatalf("identical rewrite = %d %s, want 200", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"version":1`) {
		t.Fatalf("identical rewrite advanced the version: %s", response.Body.String())
	}

	// And a real change still advances, so the rule above is not just a stuck field.
	changed, err := json.Marshal(map[string]string{"format": "markdown", "content": "# Redis 持久化（第二版）\n\n只谈快照。\n"})
	if err != nil {
		t.Fatal(err)
	}
	response = doJSON(t, fixture.server, http.MethodPut, "/api/v1/documents/"+fixture.document, token, string(changed))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"version":2`) {
		t.Fatalf("a real rewrite = %d %s, want 200 with version 2", response.Code, response.Body.String())
	}
}

func postDocument(t *testing.T, fixture *apiFixture, token, collectionID, title, content string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]string{"title": title, "format": "markdown", "content": content})
	if err != nil {
		t.Fatal(err)
	}
	return doJSON(t, fixture.server, http.MethodPost, "/api/v1/collections/"+collectionID+"/documents", token, string(body))
}
