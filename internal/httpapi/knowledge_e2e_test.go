package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/responder"
)

// groundedTurn sends one grounded message and returns the assistant message as
// the client sees it: content plus the citations attached to that message.
func groundedTurn(t *testing.T, fixture *apiFixture, token, content, collectionID string) (string, []domain.Source) {
	t.Helper()
	message := sendTurn(t, fixture, token, content, collectionID)
	return message.Content, message.Sources
}

func sendTurn(t *testing.T, fixture *apiFixture, token, content, collectionID string) messageView {
	t.Helper()
	body := `{"content":"` + content + `","knowledge":{"enabled":true,"collection_ids":["` + collectionID + `"]}}`
	response := doJSON(t, fixture.server, http.MethodPost, "/api/v1/sessions/"+fixture.session+"/messages", token, body)
	requireStatus(t, response, http.StatusOK, "grounded turn")
	var envelope struct {
		Data struct {
			AssistantMessage messageView `json:"assistant_message"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode grounded response %s: %v", response.Body.String(), err)
	}
	return envelope.Data.AssistantMessage
}

// listDocuments reads one collection's document list, requiring the call to have
// succeeded; the refusal cases use doJSON directly because they expect a 404.
func listDocuments(t *testing.T, fixture *apiFixture, token, collectionID string) []documentView {
	t.Helper()
	response := doJSON(t, fixture.server, http.MethodGet, "/api/v1/collections/"+collectionID+"/documents", token, "")
	requireStatus(t, response, http.StatusOK, "list documents")
	var envelope struct {
		Data struct {
			Items []documentView `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode document list %s: %v", response.Body.String(), err)
	}
	return envelope.Data.Items
}

// streamTurn sends the same kind of turn over SSE and returns the raw event
// stream, which is how a client sees the citations on the terminal event.
func streamTurn(t *testing.T, fixture *apiFixture, token, content, collectionID string) string {
	t.Helper()
	body := `{"content":"` + content + `","knowledge":{"enabled":true,"collection_ids":["` + collectionID + `"]}}`
	recorder := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/"+fixture.session+"/stream", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	fixture.server.Handler().ServeHTTP(recorder, request)
	requireStatus(t, recorder.ResponseRecorder, http.StatusOK, "grounded stream")
	return recorder.Body.String()
}

// completedEvent returns the terminal SSE block, failing the test when a stream
// ended without one.
func completedEvent(t *testing.T, streamBody string) string {
	t.Helper()
	for _, block := range strings.Split(streamBody, "\n\n") {
		if strings.Contains(block, "event: message.completed") {
			return block
		}
	}
	t.Fatalf("the stream never completed: %s", streamBody)
	return ""
}

// readHistory re-reads the session the way a client that pressed F5 would.
func readHistory(t *testing.T, fixture *apiFixture, token string) []messageView {
	t.Helper()
	response := doJSON(t, fixture.server, http.MethodGet, "/api/v1/sessions/"+fixture.session+"/messages", token, "")
	requireStatus(t, response, http.StatusOK, "read history")
	var envelope struct {
		Data struct {
			Items []messageView `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode history %s: %v", response.Body.String(), err)
	}
	return envelope.Data.Items
}

func fixtureToken(t *testing.T, fixture *apiFixture, user string) string {
	t.Helper()
	return issueTestToken(t, fixture.server, user)
}

// TestGroundedTurnCitesRetrievedChunks checks what the golden table cannot: that
// every citation field points at a real stored chunk, and that the citation
// identifiers are derived rather than invented.
func TestGroundedTurnCitesRetrievedChunks(t *testing.T) {
	fixture := newFixture(t, apiResponder{})
	token := fixtureToken(t, fixture, fixture.owner)

	content, sources := groundedTurn(t, fixture, token, "Redis 持久化 AOF 与 RDB 怎么取舍", fixture.collection)
	if len(sources) == 0 {
		t.Fatalf("a grounded turn returned no citations; answer=%q", content)
	}
	for _, source := range sources {
		if source.DocumentID != fixture.document {
			t.Fatalf("citation points at an unexpected document: %#v", source)
		}
		if source.CollectionID != fixture.collection {
			t.Fatalf("citation points at an unexpected collection: %#v", source)
		}
		if !strings.HasPrefix(source.ChunkID, fixture.document+"-v1-c") {
			t.Fatalf("chunk id %q does not look like a stored chunk of %q", source.ChunkID, fixture.document)
		}
		if source.SourceID != "src-"+source.ChunkID {
			t.Fatalf("source id %q is not derived from the chunk id %q", source.SourceID, source.ChunkID)
		}
		if source.Title != "redis-persistence.md" || source.Snippet == "" {
			t.Fatalf("citation lost its provenance: %#v", source)
		}
	}
	// The answer text must attribute each passage and say it is extractive.
	if !strings.Contains(content, "redis-persistence.md") || !strings.Contains(content, "未接入生成模型") {
		t.Fatalf("answer text is not a labelled extract: %q", content)
	}
}

// TestDirectTurnCarriesNoCitations is the byte-level half of the compatibility
// promise: an ungrounded answer must not grow a sources field at all.
func TestDirectTurnCarriesNoCitations(t *testing.T) {
	fixture := newFixture(t, apiResponder{})
	token := fixtureToken(t, fixture, fixture.owner)

	response := doJSON(t, fixture.server, http.MethodPost, "/api/v1/sessions/"+fixture.session+"/messages", token, `{"content":"question"}`)
	requireStatus(t, response, http.StatusOK, "direct turn")
	if strings.Contains(response.Body.String(), `"sources"`) {
		t.Fatalf("a direct_chat response grew a sources field: %s", response.Body.String())
	}

	streamResponse := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/"+fixture.session+"/stream", strings.NewReader(`{"content":"question"}`))
	request.Header.Set("Authorization", "Bearer "+token)
	fixture.server.Handler().ServeHTTP(streamResponse, request)
	body := streamResponse.Body.String()
	if strings.Contains(body, "sources") {
		t.Fatalf("a direct_chat SSE payload grew sources: %s", body)
	}
	if !strings.Contains(body, "event: message.completed") || !strings.Contains(body, `"content":"safe answer"`) {
		t.Fatalf("SSE stream changed shape: %s", body)
	}
}

// TestRewrittenDocumentStopsAnsweringFromTheOldVersion proves the version switch
// end to end over HTTP: after a replace, the retired text must not come back.
func TestRewrittenDocumentStopsAnsweringFromTheOldVersion(t *testing.T) {
	fixture := newFixture(t, apiResponder{})
	token := fixtureToken(t, fixture, fixture.owner)

	before, beforeSources := groundedTurn(t, fixture, token, "durability 恢复速度", fixture.collection)
	if len(beforeSources) == 0 || !strings.Contains(before, "durability") {
		t.Fatalf("the first version was not retrievable: %q", before)
	}

	replaced := doJSON(t, fixture.server, http.MethodPut, "/api/v1/documents/"+fixture.document, token,
		`{"format":"markdown","content":"# Redis 持久化（第二版）\n\n快照文件的定时生成策略决定了恢复速度，但不再讨论日志。"}`)
	requireStatus(t, replaced, http.StatusOK, "replace document")
	if !strings.Contains(replaced.Body.String(), `"version":2`) {
		t.Fatalf("replace did not advance the version: %s", replaced.Body.String())
	}

	_, afterSources := groundedTurn(t, fixture, token, "durability 恢复速度", fixture.collection)
	if len(afterSources) == 0 {
		t.Fatal("the rewritten document is no longer retrievable at all")
	}
	for _, source := range afterSources {
		// The question itself is echoed by the extractive generator, so only the
		// cited snippets can prove the old text is gone.
		if strings.Contains(source.Snippet, "durability") {
			t.Fatalf("the retired version is still being cited: %#v", source)
		}
		if !strings.HasPrefix(source.ChunkID, fixture.document+"-v2-c") {
			t.Fatalf("citation %q still points at a version 1 chunk", source.ChunkID)
		}
	}
}

// TestGroundedTurnSharesOneStrategyAcrossSyncAndStream asserts the SSE route
// grounds the same way the synchronous route does, down to the citation set.
func TestGroundedTurnSharesOneStrategyAcrossSyncAndStream(t *testing.T) {
	fixture := newFixture(t, apiResponder{})
	token := fixtureToken(t, fixture, fixture.owner)
	body := `{"content":"Redis 持久化 AOF","knowledge":{"enabled":true,"collection_ids":["` + fixture.collection + `"]}}`

	syncContent, syncSources := groundedTurn(t, fixture, token, "Redis 持久化 AOF", fixture.collection)

	streamResponse := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/"+fixture.session+"/stream", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	fixture.server.Handler().ServeHTTP(streamResponse, request)
	requireStatus(t, streamResponse.ResponseRecorder, http.StatusOK, "grounded stream")
	streamBody := streamResponse.Body.String()

	var completed string
	for _, block := range strings.Split(streamBody, "\n\n") {
		if strings.Contains(block, "event: message.completed") {
			completed = block
		}
	}
	if completed == "" {
		t.Fatalf("the grounded stream never completed: %s", streamBody)
	}
	for _, source := range syncSources {
		if !strings.Contains(completed, source.ChunkID) {
			t.Fatalf("the SSE completed event lost the citation %q: %s", source.ChunkID, completed)
		}
	}
	if !strings.Contains(completed, `"content":`) {
		t.Fatalf("the completed event lost its content: %s", completed)
	}
	if strings.Count(streamBody, "event: message.delta") < 2 {
		t.Fatalf("a grounded answer was not streamed in deltas: %s", streamBody)
	}
	if !strings.Contains(syncContent, "AOF") {
		t.Fatalf("the sync grounded answer lost the query topic: %q", syncContent)
	}
}

// TestGroundedSourcesComeBackOnAReRead is the product requirement behind
// persisting citations: after a refresh the history endpoint must hand back the
// same sources, on the same message, in the same order the answer cited them -
// over the synchronous route and over SSE alike.
func TestGroundedSourcesComeBackOnAReRead(t *testing.T) {
	fixture := newFixture(t, apiResponder{})
	token := fixtureToken(t, fixture, fixture.owner)

	sent := sendTurn(t, fixture, token, "Redis 持久化 AOF 与 RDB 怎么取舍", fixture.collection)
	content, sources := sent.Content, sent.Sources
	if len(sources) == 0 {
		t.Fatal("the turn cited nothing, so this test would prove nothing")
	}
	history := readHistory(t, fixture, token)
	if len(history) != 2 {
		t.Fatalf("history holds %d messages after one turn, want the pair", len(history))
	}
	assistant := history[1]
	if assistant.Role != "assistant" {
		t.Fatalf("history[1] role = %q, want the assistant message", assistant.Role)
	}
	if sent.ID == "" || assistant.ID != sent.ID {
		t.Fatalf("history[1] id = %q, want the turn's assistant message %q", assistant.ID, sent.ID)
	}
	if assistant.Content != content {
		t.Fatalf("re-read content = %q, want the answer the turn returned", assistant.Content)
	}
	if !reflect.DeepEqual(assistant.Sources, sources) {
		t.Fatalf("re-read sources = %#v, want %#v in the same order", assistant.Sources, sources)
	}

	streamBody := streamTurn(t, fixture, token, "分布式锁 Redlock 安全吗", fixture.collection)
	completed := completedEvent(t, streamBody)
	history = readHistory(t, fixture, token)
	if len(history) != 4 {
		t.Fatalf("history holds %d messages after a turn and a stream, want two pairs", len(history))
	}
	streamed := history[3]
	if len(streamed.Sources) == 0 {
		t.Fatalf("the streamed answer stored no citations: %#v", streamed)
	}
	for index, source := range streamed.Sources {
		if !strings.Contains(completed, source.ChunkID) {
			t.Fatalf("citation %d (%q) is not the one the SSE completed event sent: %s", index, source.ChunkID, completed)
		}
	}
}

// TestOrdinaryHistoryCarriesNoCitationField keeps the compatible shape honest: a
// conversation that never touched the knowledge base must not grow a sources key
// anywhere, not in the turn response and not in the re-read history.
func TestOrdinaryHistoryCarriesNoCitationField(t *testing.T) {
	fixture := newFixture(t, apiResponder{})
	token := fixtureToken(t, fixture, fixture.owner)

	response := doJSON(t, fixture.server, http.MethodPost, "/api/v1/sessions/"+fixture.session+"/messages", token, `{"content":"question"}`)
	requireStatus(t, response, http.StatusOK, "direct turn")
	if strings.Contains(response.Body.String(), `"sources"`) {
		t.Fatalf("a direct turn grew a sources field: %s", response.Body.String())
	}

	listed := doJSON(t, fixture.server, http.MethodGet, "/api/v1/sessions/"+fixture.session+"/messages", token, "")
	requireStatus(t, listed, http.StatusOK, "read history")
	if strings.Contains(listed.Body.String(), `"sources"`) {
		t.Fatalf("an ordinary history grew a sources field: %s", listed.Body.String())
	}

	// A session that has citations is still only readable by its owner, so the
	// stored evidence cannot be collected from outside.
	other := fixtureToken(t, fixture, fixture.other)
	foreign := doJSON(t, fixture.server, http.MethodGet, "/api/v1/sessions/"+fixture.session+"/messages", other, "")
	if foreign.Code != http.StatusNotFound || strings.Contains(foreign.Body.String(), "src-") {
		t.Fatalf("another user read the session with status %d and body %s", foreign.Code, foreign.Body.String())
	}
}

// TestDeletedDocumentStopsBeingListedAndRetrieved is the delete-then-no-recall
// requirement over HTTP: the listing drops it, a grounded turn can no longer find
// anything to cite, and a second delete reads as missing rather than "ok".
func TestDeletedDocumentStopsBeingListedAndRetrieved(t *testing.T) {
	fixture := newFixture(t, apiResponder{})
	token := fixtureToken(t, fixture, fixture.owner)

	_, before := groundedTurn(t, fixture, token, "Redis 持久化 AOF 与 RDB 怎么取舍", fixture.collection)
	if len(before) == 0 {
		t.Fatal("the seeded document cited nothing before deletion")
	}
	if listed := listDocuments(t, fixture, token, fixture.collection); len(listed) != 1 || listed[0].ID != fixture.document {
		t.Fatalf("listing before delete = %#v, want the seeded document", listed)
	}

	deleted := doJSON(t, fixture.server, http.MethodDelete, "/api/v1/documents/"+fixture.document, token, "")
	requireStatus(t, deleted, http.StatusOK, "delete document")
	if strings.Contains(deleted.Body.String(), `"id"`) {
		t.Fatalf("a delete returned a body with identifiers: %s", deleted.Body.String())
	}
	if listed := listDocuments(t, fixture, token, fixture.collection); len(listed) != 0 {
		t.Fatalf("listing after delete = %#v, want the collection empty", listed)
	}
	content, sources := groundedTurn(t, fixture, token, "Redis 持久化 AOF 与 RDB 怎么取舍", fixture.collection)
	if len(sources) != 0 {
		t.Fatalf("a deleted document is still being cited: %#v", sources)
	}
	if !strings.Contains(content, "没有检索到") {
		t.Fatalf("answer %q should say the deleted content is gone rather than answer from it", content)
	}
	if again := doJSON(t, fixture.server, http.MethodDelete, "/api/v1/documents/"+fixture.document, token, ""); again.Code != http.StatusNotFound {
		t.Fatalf("second delete status = %d, want 404: %s", again.Code, again.Body.String())
	}
}

// TestDocumentsOfAForeignCollectionAreNotListableOrDeletable keeps the two new
// endpoints on the same ownership rule as the rest of the API: another user's
// collection reads as missing, and their document cannot be deleted.
func TestDocumentsOfAForeignCollectionAreNotListableOrDeletable(t *testing.T) {
	fixture := newFixture(t, apiResponder{})
	token := fixtureToken(t, fixture, fixture.owner)

	foreignListing := doJSON(t, fixture.server, http.MethodGet, "/api/v1/collections/"+fixture.foreignCollection+"/documents", token, "")
	if foreignListing.Code != http.StatusNotFound || !strings.Contains(foreignListing.Body.String(), `"code":"not_found"`) {
		t.Fatalf("listing a foreign collection = %d %s", foreignListing.Code, foreignListing.Body.String())
	}
	foreignDelete := doJSON(t, fixture.server, http.MethodDelete, "/api/v1/documents/"+fixture.foreignDocument, token, "")
	if foreignDelete.Code != http.StatusNotFound {
		t.Fatalf("deleting another user's document = %d %s", foreignDelete.Code, foreignDelete.Body.String())
	}
	// The route still answers the wrong verb with 405 rather than pretending the
	// collection exists: method handling happens before ownership.
	wrongMethod := doJSON(t, fixture.server, http.MethodDelete, "/api/v1/collections/"+fixture.collection+"/documents", token, "")
	if wrongMethod.Code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE on the collection documents route = %d %s", wrongMethod.Code, wrongMethod.Body.String())
	}
	// Both users can still list what they own, so the refusals above are scope
	// failures rather than a broken route.
	if listed := listDocuments(t, fixture, token, fixture.collection); len(listed) != 1 || listed[0].ID != fixture.document {
		t.Fatalf("own listing = %#v, want the seeded document", listed)
	}
	other := fixtureToken(t, fixture, fixture.other)
	if listed := listDocuments(t, fixture, other, fixture.foreignCollection); len(listed) != 1 || listed[0].ID != fixture.foreignDocument {
		t.Fatalf("the other user's own listing = %#v, want their document", listed)
	}
}

// TestUnknownAndForeignCollectionsAreRefusedBeforeRetrieval checks the HTTP
// surface of the ownership gate: a bad scope must never reach the model.
func TestUnknownAndForeignCollectionsAreRefusedBeforeRetrieval(t *testing.T) {
	requests := make(chan responder.Request, 4)
	fixture := newFixture(t, apiResponder{respondRequest: requests})
	token := fixtureToken(t, fixture, fixture.owner)

	for _, collectionID := range []string{"collection-does-not-exist", fixture.foreignCollection} {
		response := doJSON(t, fixture.server, http.MethodPost, "/api/v1/sessions/"+fixture.session+"/messages", token,
			`{"content":"question","knowledge":{"enabled":true,"collection_ids":["`+collectionID+`"]}}`)
		if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), `"code":"not_found"`) {
			t.Fatalf("collection %s status/body = %d %s", collectionID, response.Code, response.Body.String())
		}
	}
	if pending := len(requests); pending != 0 {
		t.Fatalf("refused scopes still reached the responder %d times", pending)
	}
}

// TestUnrelatedQuestionCitesNothingOnBothPaths is the product-facing half of the
// admission fix. The collection is owned, readable and full of chunks, and the
// question is simply not about any of them, so the honest answer says nothing was
// found and carries no citation at all. Before the fix the head of the ranking
// was cited regardless of its score, which turned "I did not find this" into an
// answer that looked sourced.
func TestUnrelatedQuestionCitesNothingOnBothPaths(t *testing.T) {
	fixture := newFixture(t, apiResponder{})
	token := fixtureToken(t, fixture, fixture.owner)
	body := `{"content":"量子色动力学渐近自由","knowledge":{"enabled":true,"collection_ids":["` + fixture.collection + `"]}}`

	response := doJSON(t, fixture.server, http.MethodPost, "/api/v1/sessions/"+fixture.session+"/messages", token, body)
	requireStatus(t, response, http.StatusOK, "unrelated grounded turn")
	syncBody := response.Body.String()
	if strings.Contains(syncBody, `"sources"`) {
		t.Fatalf("a turn with no evidence still returned a sources field: %s", syncBody)
	}
	content, sources := groundedTurn(t, fixture, token, "量子色动力学渐近自由", fixture.collection)
	if len(sources) != 0 {
		t.Fatalf("an unrelated question cited %d chunks: %#v", len(sources), sources)
	}
	if !strings.Contains(content, "没有检索到") {
		t.Fatalf("answer %q must state that nothing was retrieved", content)
	}
	if strings.Contains(content, "检索到 ") {
		t.Fatalf("answer %q claims retrieved passages it does not cite", content)
	}

	streamResponse := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/"+fixture.session+"/stream", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	fixture.server.Handler().ServeHTTP(streamResponse, request)
	requireStatus(t, streamResponse.ResponseRecorder, http.StatusOK, "unrelated grounded stream")
	streamBody := streamResponse.Body.String()
	if strings.Contains(streamBody, "sources") {
		t.Fatalf("the SSE stream attached citations to a turn with no evidence: %s", streamBody)
	}
	if !strings.Contains(streamBody, "没有检索到") {
		t.Fatalf("the SSE answer did not say nothing was retrieved: %s", streamBody)
	}
}
