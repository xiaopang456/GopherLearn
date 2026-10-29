package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/xiaopang456/GopherLearn/internal/domain"
)

// The golden file records the shape of every memory answer. These tests carry what a
// single replayed call cannot: ids that only exist after an earlier call, a conversation
// deleted from under a draft, and two requests racing for one confirmation.

// addMemoryBody is what a person sends to state a fact: scope, the subject if it is a
// topic, and the sentence. Nothing else - provenance, status and confidence belong to the
// proposal path and are refused here as unknown keys.
func addMemoryBody(scope, topicKey, content string) string {
	key := ""
	if topicKey != "" {
		key = `"topic_key":"` + topicKey + `",`
	}
	return `{"scope":"` + scope + `",` + key + `"content":"` + content + `"}`
}

func postMemory(t *testing.T, fixture *apiFixture, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	return doJSON(t, fixture.server, http.MethodPost, "/api/v1/memories", token, body)
}

func memoryAction(t *testing.T, fixture *apiFixture, token, memoryID, action, body string) *httptest.ResponseRecorder {
	t.Helper()
	return doJSON(t, fixture.server, http.MethodPost, "/api/v1/memories/"+memoryID+"/"+action, token, body)
}

// sendToHandler replays one request against a handler the caller already built, which is how
// concurrent requests must be driven here (see the confirmation race test).
func sendToHandler(handler http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func listMemories(t *testing.T, fixture *apiFixture, token, state string) []memoryView {
	t.Helper()
	response := doJSON(t, fixture.server, http.MethodGet, "/api/v1/memories?state="+state, token, "")
	requireStatus(t, response, http.StatusOK, "list "+state)
	var envelope struct {
		Data struct {
			Items []memoryView `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode %s listing %s: %v", state, response.Body.String(), err)
	}
	return envelope.Data.Items
}

func decodeMemory(t *testing.T, response *httptest.ResponseRecorder) memoryView {
	t.Helper()
	var envelope struct {
		Data memoryView `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode memory response %s: %v", response.Body.String(), err)
	}
	return envelope.Data
}

func requireCode(t *testing.T, response *httptest.ResponseRecorder, want int, code, context string) {
	t.Helper()
	requireStatus(t, response, want, context)
	if !strings.Contains(response.Body.String(), `"code":"`+code+`"`) {
		t.Fatalf("%s: body %s, want code %q", context, response.Body.String(), code)
	}
}

// TestMemoryLifecycleOverHTTP walks both shapes a memory can arrive in: one the learner
// typed, which is active on arrival, and one an agent filed, which only becomes a fact when
// the user says so. It then checks that a row moves between the two listings instead of
// being filtered out by whichever reader forgot the status.
func TestMemoryLifecycleOverHTTP(t *testing.T) {
	fixture := newMemoryFixture(t)
	owner := issueTestToken(t, fixture.server, fixture.owner)

	created := postMemory(t, fixture, owner, `{"scope":"global","content":"第一次把 AOF 调到 everysec 才敢上线"}`)
	requireStatus(t, created, http.StatusOK, "add a memory by hand")
	authored := decodeMemory(t, created)
	if authored.Status != domain.MemoryStatusActive {
		t.Fatalf("an authored memory came back %q, want active in the same request", authored.Status)
	}
	// The stored statement is certain because its author wrote it, and it cites nothing:
	// the sentence is the evidence. A surface that let a client attach a session would let
	// the same request invent provenance for a fact nobody observed.
	if authored.Confidence != 1 {
		t.Fatalf("authored confidence = %v, want the server's 1", authored.Confidence)
	}
	if strings.TrimSpace(authored.SourceSessionID) != "" || len(authored.SourceMessageIDs) != 0 {
		t.Fatalf("an authored memory carries provenance no client could cite: %+v", authored)
	}
	if strings.Contains(created.Body.String(), "user_id") {
		t.Fatalf("the response echoes an owner id the client never asked for: %s", created.Body.String())
	}
	if !containsMemory(listMemories(t, fixture, owner, "active"), authored.MemoryID) {
		t.Fatalf("a memory the user added by hand is missing from the active listing")
	}
	// An episode is something that happened in a conversation. Typed in, with nothing to
	// point at, it is a claim about an event and this surface refuses to store it.
	requireCode(t, postMemory(t, fixture, owner, `{"scope":"episodic","content":"第一次把 AOF 调到 everysec 才敢上线"}`),
		http.StatusBadRequest, "invalid_request", "add an episodic memory by hand")

	// The proposal path is the other shape, and its lifecycle is what the rest of this test
	// walks: pending until the user decides, then active, then archived.
	draft := fixture.memoryDraft
	pending := listMemories(t, fixture, owner, "pending")
	if !containsMemory(pending, draft) {
		t.Fatalf("the seeded proposal is missing from the pending listing: %+v", pending)
	}
	if containsMemory(listMemories(t, fixture, owner, "active"), draft) {
		t.Fatalf("an unconfirmed draft reached the active listing")
	}

	confirmed := decodeMemory(t, memoryAction(t, fixture, owner, draft, "confirm", ""))
	if confirmed.Status != domain.MemoryStatusActive {
		t.Fatalf("after confirm status = %q", confirmed.Status)
	}
	if !containsMemory(listMemories(t, fixture, owner, "active"), draft) ||
		containsMemory(listMemories(t, fixture, owner, "pending"), draft) {
		t.Fatalf("the confirmed row did not move between the two listings")
	}

	archived := decodeMemory(t, memoryAction(t, fixture, owner, draft, "archive", ""))
	if archived.Status != domain.MemoryStatusArchived {
		t.Fatalf("after archive status = %q", archived.Status)
	}
	if containsMemory(listMemories(t, fixture, owner, "active"), draft) ||
		containsMemory(listMemories(t, fixture, owner, "pending"), draft) {
		t.Fatalf("an archived row is still listed as something the user should act on")
	}

	// A topic memory keeps its key, which is how the surface tells the two topics apart
	// without the client having to remember what it proposed.
	topic := decodeMemory(t, postMemory(t, fixture, owner, addMemoryBody("topic", "kafka", "Redlock 的安全性存在争议")))
	if topic.TopicKey != "kafka" {
		t.Fatalf("topic memory key = %q, want kafka", topic.TopicKey)
	}
	if topic.Status != domain.MemoryStatusActive {
		t.Fatalf("a topic memory added by hand came back %q, want active", topic.Status)
	}
	// The same subject added twice retires the first version in the request that stored
	// the second, so no pair of active answers to one question is ever observable.
	replacement := decodeMemory(t, postMemory(t, fixture, owner, addMemoryBody("topic", "kafka", "Redlock 的争议点在时钟依赖")))
	if replacement.MemoryID == topic.MemoryID {
		t.Fatal("the replacement reused the retired row's id")
	}
	if containsMemory(listMemories(t, fixture, owner, "active"), topic.MemoryID) {
		t.Fatalf("adding a newer version left the older one active: %+v", listMemories(t, fixture, owner, "active"))
	}
	if !containsMemory(listMemories(t, fixture, owner, "active"), replacement.MemoryID) {
		t.Fatal("the newer version is not the active one")
	}
}

func containsMemory(items []memoryView, memoryID string) bool {
	for _, item := range items {
		if item.MemoryID == memoryID {
			return true
		}
	}
	return false
}

// TestMemoryWritesRefuseEveryStateTheClientCouldName: the gate's whole point is that a
// caller proposes and the server decides. Each of these fields would move a row to a state,
// date it, or file it into another account if the request were trusted.
func TestMemoryWritesRefuseEveryStateTheClientCouldName(t *testing.T) {
	fixture := newMemoryFixture(t)
	owner := issueTestToken(t, fixture.server, fixture.owner)

	forbidden := []string{
		`"status":"active"`, `"status":"superseded"`, `"status":"archived"`, `"status":"pending"`,
		`"active":true`, `"superseded_at":"2026-01-01T00:00:00Z"`, `"confidence":0.1`, `"confidence":2`,
		`"user_id":"user-9"`, `"memory_id":"mem-invented"`, `"id":"mem-invented"`,
		`"created_at":"2026-01-01T00:00:00Z"`, `"updated_at":"2026-01-01T00:00:00Z"`,
		// Provenance is the proposal path's field. A hand-written fact that could name a
		// conversation would be a way to fabricate the evidence the agent path exists to
		// check, so these two keys are refused here rather than ignored.
		`"source_session_id":"{{session}}"`, `"source_message_ids":["{{memory_message}}"]`,
		`"source_message_ids":"message-1"`,
	}
	for _, field := range forbidden {
		body := strings.ReplaceAll(`{"scope":"global","content":"客户端想自己决定状态",`+field+`}`,
			"{{session}}", fixture.session)
		body = strings.ReplaceAll(body, "{{memory_message}}", fixture.memoryMessage)
		response := postMemory(t, fixture, owner, body)
		requireCode(t, response, http.StatusBadRequest, "invalid_request", "add with "+field)
	}
	// A memory that survives with none of those fields is proof the refusal is not just
	// every request failing for an unrelated reason.
	requireStatus(t, postMemory(t, fixture, owner, addMemoryBody("global", "", "这条是我自己写的，不需要再确认")),
		http.StatusOK, "add without any refused field")

	// An action takes no input at all, so anything in the body is a client trying to write.
	for _, body := range []string{`{"status":"active"}`, `{"confidence":1}`, `[]`, `not json`, `{}{} `} {
		response := memoryAction(t, fixture, owner, fixture.memoryDraft, "confirm", body)
		if body == `{}{} ` {
			// Two JSON values in one body: the trailing garbage is refused before the
			// transition is attempted.
			requireCode(t, response, http.StatusBadRequest, "invalid_request", "confirm with a second JSON value")
			continue
		}
		requireCode(t, response, http.StatusBadRequest, "invalid_request", "confirm with body "+body)
	}
	// The same draft is still untouched after all of that, so a refused body never half-applied.
	if listed := listMemories(t, fixture, owner, "pending"); !containsMemory(listed, fixture.memoryDraft) {
		t.Fatalf("the draft left the pending listing although every action body was refused: %+v", listed)
	}
}

// TestForeignAndUnknownMemoryIdsAreIndistinguishable is the anti-probing rule, asserted on
// the answer bytes rather than just the status: an attacker holding a valid token learns
// nothing about whether a memory id exists in someone else's account.
func TestForeignAndUnknownMemoryIdsAreIndistinguishable(t *testing.T) {
	fixture := newMemoryFixture(t)
	intruder := issueTestToken(t, fixture.server, fixture.other)

	var bodies []string
	for _, action := range []string{"confirm", "reject", "archive"} {
		onForeign := memoryAction(t, fixture, intruder, fixture.memoryDraft, action, "")
		requireCode(t, onForeign, http.StatusNotFound, "not_found", "the other account may not "+action+" a foreign draft")
		onUnknown := memoryAction(t, fixture, intruder, "mem-does-not-exist", action, "")
		requireCode(t, onUnknown, http.StatusNotFound, "not_found", "the other account may not "+action+" a row that was never stored")
		bodies = append(bodies, onForeign.Body.String(), onUnknown.Body.String())
	}
	for index := 1; index < len(bodies); index++ {
		if bodies[index] != bodies[0] {
			t.Fatalf("a foreign id and an unknown id answered differently:\n%s\n%s", bodies[0], bodies[index])
		}
	}

	// Neither listing carries a row from the other account, in either direction.
	owner := issueTestToken(t, fixture.server, fixture.owner)
	if containsMemory(listMemories(t, fixture, owner, "pending"), fixture.foreignDraft) {
		t.Fatalf("the owner's pending listing carries another account's draft")
	}
	if containsMemory(listMemories(t, fixture, intruder, "active"), fixture.memoryActive) ||
		containsMemory(listMemories(t, fixture, intruder, "pending"), fixture.memoryDraft) {
		t.Fatalf("the intruder's listings carry the owner's rows")
	}
	// Anonymous requests are refused before any of that, including on the action paths.
	for _, action := range []string{"confirm", "reject", "archive"} {
		requireCode(t, memoryAction(t, fixture, "", fixture.memoryDraft, action, ""),
			http.StatusUnauthorized, "unauthorized", "anonymous "+action)
	}
	requireCode(t, doJSON(t, fixture.server, http.MethodGet, "/api/v1/memories?state=pending", "", ""),
		http.StatusUnauthorized, "unauthorized", "anonymous listing")
}

// TestConfirmRefusesADraftWhoseConversationIsGone: provenance is re-checked at the moment of
// the decision, not only when the proposal was written, so a memory cannot become a fact by
// citing a transcript that no longer exists.
func TestConfirmRefusesADraftWhoseConversationIsGone(t *testing.T) {
	fixture := newMemoryFixture(t)
	owner := issueTestToken(t, fixture.server, fixture.owner)

	requireStatus(t, doJSON(t, fixture.server, http.MethodDelete, "/api/v1/sessions/"+fixture.session, owner, ""),
		http.StatusOK, "delete the cited conversation")

	response := memoryAction(t, fixture, owner, fixture.memoryDraft, "confirm", "")
	requireCode(t, response, http.StatusConflict, "memory_evidence_missing", "confirm after the evidence was deleted")

	// The proposal survives its own refutation: it is still listed, still unconfirmed, and
	// archiving it is still the user's call.
	if listed := listMemories(t, fixture, owner, "pending"); !containsMemory(listed, fixture.memoryDraft) {
		t.Fatalf("a draft whose evidence disappeared vanished from the pending listing: %+v", listed)
	}
	if containsMemory(listMemories(t, fixture, owner, "active"), fixture.memoryDraft) {
		t.Fatalf("a draft with no surviving evidence became active")
	}
}

// TestMemoryProvenanceMustNameTheCitedSessionsOwnMessages: a draft may cite any message id
// that exists, but only the ones inside the session it names can carry its evidence.
func TestMemoryProvenanceMustNameTheCitedSessionsOwnMessages(t *testing.T) {
	fixture := newMemoryFixture(t)
	owner := issueTestToken(t, fixture.server, fixture.owner)

	secondSession, err := fixture.store.CreateSession(t.Context(), &domain.Session{UserID: fixture.owner, Title: "第二个会话", Mode: domain.SessionModeChat})
	if err != nil {
		t.Fatal(err)
	}
	otherMessage, err := fixture.store.AppendMessage(t.Context(), fixture.owner, secondSession.ID, &domain.Message{Role: "user", Content: "另一段对话"})
	if err != nil {
		t.Fatal(err)
	}

	// A hand-written memory cannot cite a conversation at all, so the mismatched pairing this
	// test used to build - a session that is the caller's plus a message from another one - is
	// now refused at the decoder, before any store is asked. The equivalent rule for the draft
	// path, where provenance is required, lives in the repository conformance suite and runs
	// against both stores.
	response := postMemory(t, fixture, owner, `{"scope":"global","content":"张冠李戴的引用","source_session_id":"`+
		fixture.session+`","source_message_ids":["`+otherMessage.ID+`"]}`)
	requireCode(t, response, http.StatusBadRequest, "invalid_request", "a hand-written memory citing a conversation")
	if len(listMemories(t, fixture, owner, "pending")) != 1 {
		t.Fatalf("the refused memory was stored anyway: %+v", listMemories(t, fixture, owner, "pending"))
	}
}

// TestConcurrentConfirmationsLetExactlyOneThrough: the promotion is a conditional write, so
// four clients that click 确认 at the same moment cannot all be told they succeeded, and the
// memory cannot end up double-counted in the listing.
func TestConcurrentConfirmationsLetExactlyOneThrough(t *testing.T) {
	fixture := newMemoryFixture(t)
	owner := issueTestToken(t, fixture.server, fixture.owner)
	// One handler for all four requests: Server.ServeHTTP rebuilds the Gin engine on every
	// call and gin's mode is a process global, so driving the requests through ServeHTTP
	// concurrently would make the race detector report the router rather than the store.
	// A real server builds the handler once in main and shares it.
	handler := fixture.server.Handler()

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		statuses []int
	)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			response := sendToHandler(handler, http.MethodPost, "/api/v1/memories/"+fixture.memoryDraft+"/confirm", owner, "")
			mu.Lock()
			defer mu.Unlock()
			statuses = append(statuses, response.Code)
		}()
	}
	wg.Wait()

	successes := 0
	for _, status := range statuses {
		if status == http.StatusOK {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("four concurrent confirmations produced %d successes: %v", successes, statuses)
	}
	active := listMemories(t, fixture, owner, "active")
	if !containsMemory(active, fixture.memoryDraft) {
		t.Fatalf("the winner's memory is missing from the active listing: %+v", active)
	}
	if len(active) != 2 {
		t.Fatalf("the active listing holds %d rows, want the seeded topic memory plus exactly one confirmation: %+v", len(active), active)
	}
}

// TestMemoryRoutesRefuseToHalfWorkWithoutTheCollaborator: a server assembled without the
// memory service answers 500 rather than inventing an empty listing, so a misconfigured
// deployment shows up as broken instead of as a user with no memories.
func TestMemoryRoutesRefuseToHalfWorkWithoutTheCollaborator(t *testing.T) {
	fixture := newMemoryFixture(t)
	owner := issueTestToken(t, fixture.server, fixture.owner)
	bare := NewServer(nil, nil, nil, fixture.tokens)

	for _, request := range []struct {
		method, path string
	}{
		{http.MethodGet, "/api/v1/memories?state=pending"},
		{http.MethodGet, "/api/v1/memories?state=active"},
		{http.MethodPost, "/api/v1/memories"},
	} {
		response := doJSON(t, bare, request.method, request.path, owner, `{}`)
		requireCode(t, response, http.StatusInternalServerError, "internal_error", request.method+" "+request.path+" without a memory service")
	}
	response := memoryActionOn(t, bare, owner, fixture.memoryDraft, "confirm")
	requireCode(t, response, http.StatusInternalServerError, "internal_error", "confirm without a memory service")
}

func memoryActionOn(t *testing.T, server *Server, token, memoryID, action string) *httptest.ResponseRecorder {
	t.Helper()
	return doJSON(t, server, http.MethodPost, "/api/v1/memories/"+memoryID+"/"+action, token, "")
}
