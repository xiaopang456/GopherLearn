package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/repository"
	"github.com/xiaopang456/GopherLearn/internal/service"
)

// The golden file records one request per row, so it cannot say anything about sequence. These
// tests are the sequence half of the same contract: what a learner sees after they wrote, whether
// a second click moved anything, and whether a refusal left the state exactly as it was.
//
// The last of those is the reason this file exists next to the golden rows. A 400 that also wrote
// would be invisible to a row-by-row comparison of responses, and it is the one failure mode a
// permission switch cannot afford.

// topicMemorySession creates one learning-agent conversation with a message inside it, so a
// preference in these tests is always attached to a conversation that really exists and a memory
// can cite provenance that really exists.
func topicMemorySession(t *testing.T, fixture *apiFixture, mode domain.SessionMode) (string, string) {
	t.Helper()
	ctx := t.Context()
	session, err := fixture.store.CreateSession(ctx, &domain.Session{
		UserID: fixture.owner, Title: "主题记忆会话", Mode: mode})
	if err != nil {
		t.Fatal(err)
	}
	message, err := fixture.store.AppendMessage(ctx, fixture.owner, session.ID,
		&domain.Message{Role: "user", Content: "AOF 的 fsync 该选哪个"})
	if err != nil {
		t.Fatal(err)
	}
	return session.ID, message.ID
}

// readTopicMemory asks the switch and returns the answer plus the body it came in, because some
// assertions below are about the bytes rather than about the value.
func readTopicMemory(t *testing.T, fixture *apiFixture, sessionID string) (*httptest.ResponseRecorder, *bool) {
	t.Helper()
	response := doJSON(t, fixture.server, http.MethodGet,
		"/api/v1/sessions/"+sessionID+"/topic-memory", issueTestToken(t, fixture.server, fixture.owner), "")
	var payload struct {
		Data *struct {
			Enabled *bool `json:"enabled"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response is not the contract envelope: %v - %s", err, response.Body.String())
	}
	if payload.Data == nil {
		return response, nil
	}
	return response, payload.Data.Enabled
}

// writeTopicMemory sends one decision. The body is passed as text so a case can send bytes that no
// Go string literal would build on purpose.
func writeTopicMemory(t *testing.T, fixture *apiFixture, sessionID, body string) *httptest.ResponseRecorder {
	t.Helper()
	return doJSON(t, fixture.server, http.MethodPut,
		"/api/v1/sessions/"+sessionID+"/topic-memory", issueTestToken(t, fixture.server, fixture.owner), body)
}

// TestTopicMemorySwitchRoundTrip is the learner's own sequence: off until asked, on after the
// write, and back to off after taking it away - each step confirmed by reading the server, never by
// remembering what was sent.
//
// The default read is asserted byte for byte as `{"enabled":false}`, not as "falsy": a client that
// showed an unset switch would look identical to a client that showed an off one, and only the first
// is what this product promised.
func TestTopicMemorySwitchRoundTrip(t *testing.T) {
	fixture := newFixture(t, apiResponder{})
	sessionID, _ := topicMemorySession(t, fixture, domain.SessionModeLearningAgent)

	response, enabled := readTopicMemory(t, fixture, sessionID)
	requireStatus(t, response, http.StatusOK, "the first read")
	if enabled == nil || *enabled {
		t.Fatalf("an unanswered switch reads %v, want a literal false: %s", enabled, response.Body.String())
	}
	if want := `{"data":{"enabled":false},"error":null}` + "\n"; response.Body.String() != want {
		t.Fatalf("the default body = %q, want %q", response.Body.String(), want)
	}

	for _, step := range []struct{ body, want string }{
		{`{"enabled":true}`, `{"data":{"enabled":true},"error":null}` + "\n"},
		{`{"enabled":false}`, `{"data":{"enabled":false},"error":null}` + "\n"},
		{`{"enabled":true}`, `{"data":{"enabled":true},"error":null}` + "\n"},
	} {
		written := writeTopicMemory(t, fixture, sessionID, step.body)
		requireStatus(t, written, http.StatusOK, "the write "+step.body)
		if written.Body.String() != step.want {
			t.Fatalf("write %s body = %q, want %q", step.body, written.Body.String(), step.want)
		}
		_, read := readTopicMemory(t, fixture, sessionID)
		if read == nil {
			t.Fatalf("the answer to %s cannot be read back: no enabled field", step.body)
		}
		if want := strings.Contains(step.want, "true"); *read != want {
			t.Fatalf("after %s the server reads %v, want %v", step.body, *read, want)
		}
	}
}

// TestTopicMemoryRepeatedWriteChangesNothing is the idempotence claim in the two places it can be
// observed: the response, and the stored row's own timestamps. A learner who double-clicked did not
// make a second decision, so there must be no second record of one - and "the response looks the
// same" alone would also be true of a store that rewrote the row every time.
func TestTopicMemoryRepeatedWriteChangesNothing(t *testing.T) {
	fixture := newFixture(t, apiResponder{})
	ctx := t.Context()
	sessionID, _ := topicMemorySession(t, fixture, domain.SessionModeLearningAgent)

	first := writeTopicMemory(t, fixture, sessionID, `{"enabled":true}`)
	requireStatus(t, first, http.StatusOK, "the first write")
	stored, err := fixture.store.GetTopicMemoryPreference(ctx, fixture.owner, sessionID)
	if err != nil {
		t.Fatal(err)
	}

	for _, repeat := range []string{"the second", "the third"} {
		again := writeTopicMemory(t, fixture, sessionID, `{"enabled":true}`)
		requireStatus(t, again, http.StatusOK, repeat+" write")
		if again.Body.String() != first.Body.String() {
			t.Fatalf("%s write answered %q, want the same bytes as the first: %q",
				repeat, again.Body.String(), first.Body.String())
		}
		reread, err := fixture.store.GetTopicMemoryPreference(ctx, fixture.owner, sessionID)
		if err != nil {
			t.Fatal(err)
		}
		if !reread.Enabled || !reread.CreatedAt.Equal(stored.CreatedAt) || !reread.UpdatedAt.Equal(stored.UpdatedAt) {
			t.Fatalf("%s of the same answer rewrote the record: %+v then %+v", repeat, stored, reread)
		}
	}

	// Turning it off and repeating that is the same claim in the other direction, and it is the
	// direction a client is most likely to retry - the "did that work?" click.
	off := writeTopicMemory(t, fixture, sessionID, `{"enabled":false}`)
	requireStatus(t, off, http.StatusOK, "the write that says no")
	storedOff, err := fixture.store.GetTopicMemoryPreference(ctx, fixture.owner, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if storedOff.Enabled {
		t.Fatal("the row still says yes after a no")
	}
	writeTopicMemory(t, fixture, sessionID, `{"enabled":false}`)
	reread, err := fixture.store.GetTopicMemoryPreference(ctx, fixture.owner, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if !reread.UpdatedAt.Equal(storedOff.UpdatedAt) || !reread.CreatedAt.Equal(storedOff.CreatedAt) {
		t.Fatalf("a repeated no moved the record: %+v then %+v", storedOff, reread)
	}
	// The decision survives being changed: created_at still dates the first answer, so "when did
	// this start" is answerable after any number of later changes.
	if !storedOff.CreatedAt.Equal(stored.CreatedAt) {
		t.Fatalf("changing the answer rewrote when it was first made: %v then %v", stored.CreatedAt, storedOff.CreatedAt)
	}
}

// TestRefusedWritesLeaveNoTrace runs every body the switch refuses and asks the only question that
// matters after a 400: did anything get stored anyway? Each case is followed by a read of the same
// conversation, so a write that decoded partially, or one that ran before a validation check, shows
// up as a changed answer rather than as a passing test.
func TestRefusedWritesLeaveNoTrace(t *testing.T) {
	fixture := newFixture(t, apiResponder{})
	sessionID, _ := topicMemorySession(t, fixture, domain.SessionModeLearningAgent)

	bodies := map[string]string{
		"unknown topic key":  `{"enabled":true,"topic_key":"redis"}`,
		"unknown user id":    `{"enabled":true,"user_id":"user-9"}`,
		"unknown scope":      `{"enabled":true,"scope":"global"}`,
		"unknown session id": `{"enabled":true,"session_id":"session-2"}`,
		"unknown all users":  `{"enabled":true,"all_users":true}`,
		"missing enabled":    `{}`,
		"null":               `null`,
		"array":              `[{"enabled":true}]`,
		"wrong type":         `{"enabled":"true"}`,
		"number":             `{"enabled":1}`,
		"trailing json":      `{"enabled":true}{"enabled":true}`,
		"not json":           `enabled=true`,
		"empty body":         ``,
		"invalid utf-8":      `{"enabled":true,"note":"中文` + "\xc3\x28" + `"}`,
		"truncated utf-8":    `{"enabled":true,"note":"中文` + "\xe6\x9c" + `"}`,
		"lone surrogate":     `{"enabled":true,"note":"` + `\ud800"}`,
		"dangling high":      `{"enabled":true,"note":"` + `\ud83d"}`,
	}
	for name, body := range bodies {
		response := writeTopicMemory(t, fixture, sessionID, body)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%s status = %d, want 400: %s", name, response.Code, response.Body.String())
		}
		var payload struct {
			Error apiError `json:"error"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
			t.Fatalf("%s: unreadable refusal: %v", name, err)
		}
		if payload.Error.Code != "invalid_request" {
			t.Fatalf("%s refused with code %q, want invalid_request", name, payload.Error.Code)
		}
		// The two messages are the whole vocabulary here: the body was not JSON this server can
		// use, or it was usable JSON that left the one required field out. Anything else would mean
		// the refusal happened after the decision had been stored.
		if payload.Error.Message != "request body is invalid" && payload.Error.Message != "enabled is required" {
			t.Fatalf("%s said %q, which is not a body refusal", name, payload.Error.Message)
		}
		_, read := readTopicMemory(t, fixture, sessionID)
		if read == nil || *read {
			t.Fatalf("%s was refused and the conversation still reads %v", name, read)
		}
	}
}

// TestTopicMemoryOwnershipAnswersAreIdenticalAcrossVerbs is the requirement that a conversation
// which is not yours and one that never existed cannot be told apart - stated for both verbs, and
// compared byte for byte rather than by status code, because a 404 whose body names which case it
// is would be a way to enumerate conversations.
func TestTopicMemoryOwnershipAnswersAreIdenticalAcrossVerbs(t *testing.T) {
	ctx := t.Context()
	fixture := newFixture(t, apiResponder{})
	foreign, err := fixture.store.CreateSession(ctx, &domain.Session{
		UserID: fixture.other, Title: "别人的学习会话", Mode: domain.SessionModeLearningAgent})
	if err != nil {
		t.Fatal(err)
	}
	missing := "session-never-created-at-all"
	token := issueTestToken(t, fixture.server, fixture.owner)

	for _, path := range []string{
		"/api/v1/sessions/" + foreign.ID + "/topic-memory",
		"/api/v1/sessions/" + missing + "/topic-memory",
	} {
		get := doJSON(t, fixture.server, http.MethodGet, path, token, "")
		put := doJSON(t, fixture.server, http.MethodPut, path, token, `{"enabled":true}`)
		requireStatus(t, get, http.StatusNotFound, "GET "+path)
		requireStatus(t, put, http.StatusNotFound, "PUT "+path)
		// Same bytes for both paths and both verbs: a body that said "not yours" for one and
		// "no such conversation" for the other would tell a caller which conversations exist.
		if get.Body.String() != put.Body.String() {
			t.Fatalf("the two verbs refused differently:\n get %q\n put %q", get.Body.String(), put.Body.String())
		}
		notFound := `{"data":null,"error":{"code":"not_found","message":"resource not found"}}` + "\n"
		if get.Body.String() != notFound {
			t.Fatalf("the not-found body = %q, want the bytes every other route answers with", get.Body.String())
		}
	}

	// And the refusals wrote nothing: read as the account that does own that conversation, and it is
	// still the unanswered default. Only the real owner's read can see the row, which is what makes
	// this a check on the write rather than a second look at the 404.
	stored, err := fixture.store.GetTopicMemoryPreference(ctx, fixture.other, foreign.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Enabled {
		t.Fatal("a refused write stored a decision in another account's conversation")
	}
	if !stored.CreatedAt.IsZero() || !stored.UpdatedAt.IsZero() {
		t.Fatalf("a refused write left a timestamp behind: %+v", stored)
	}
}

// TestTopicMemorySwitchAddsASubjectAndNeverReplacesTheDefault is the whole authorization set in one
// place, asserted as an exact row set at every step.
//
// The rule it pins is the one a passing test could still get wrong: switching topic memory on
// *adds* the current quiz's subject to the default grant. It does not narrow the read to that
// subject, and it does not replace the learner's own global facts or this conversation's episodes.
// So each state below is compared against the exact ids that must be there - no count alone, and
// no substring alone - and the scope note is checked against the layers the read actually searched.
//
// (1) the live quiz's subject is added; (2) no other topic is; (3) global is still there; (4) this
// conversation's own episode is still there; (5) another conversation's episode is not; (6) with
// the preference off the read is the original global + current-session episodic, row for row; (7)
// the scope note lists exactly the layers searched.
func TestTopicMemorySwitchAddsASubjectAndNeverReplacesTheDefault(t *testing.T) {
	ctx := t.Context()
	fixture := newFixture(t, apiResponder{})
	agentSession, agentMessage := topicMemorySession(t, fixture, domain.SessionModeLearningAgent)
	chatSession, _ := topicMemorySession(t, fixture, domain.SessionModeChat)
	access := topicMemoryAccessFor(t, fixture)
	subject := domain.NormalizeLearningTopicKey("Redis 持久化")

	// The five rows this test can tell apart: two always-readable layers, one episode that lives
	// in a different conversation, the quiz's own subject, and a neighbour subject.
	globalFact := confirmMemory(t, fixture, agentSession, agentMessage, domain.MemoryScopeGlobal, "",
		"全局：解释 fsync 时用中文")
	episodeHere := confirmMemory(t, fixture, agentSession, agentMessage, domain.MemoryScopeEpisodic, "",
		"本轮决定先掌握 fsync")
	episodeElsewhere := confirmMemory(t, fixture, chatSession,
		messageOf(t, fixture, chatSession), domain.MemoryScopeEpisodic, "",
		"另一会话里的 fsync 决定，读不到")
	topicSubject := confirmTopicMemory(t, fixture, agentSession, agentMessage, subject,
		"redis 主题：AOF 的 fsync 选 everysec")
	topicNeighbour := confirmTopicMemory(t, fixture, agentSession, agentMessage, "mysql",
		"mysql 主题：binlog 的 fsync 已确认")
	// The same row filed under the learner's own words rather than the server's key: the grant
	// reads the normalized key, so this one is unreachable by design.
	topicTyped := confirmTopicMemory(t, fixture, agentSession, agentMessage, "Redis 持久化",
		"原话拼写下的 fsync 结论，读不到")

	// (a) Neither the consent nor the quiz: the default two layers.
	requireRowSet(t, access, fixture.owner, agentSession, "a conversation nobody switched on",
		globalFact, episodeHere)
	requireNoteLayers(t, access, fixture.owner, agentSession, "")

	// (b) Consent without a quiz: legal to store, and still nothing to add. The switch on its own
	// must not read like a subject, because no subject has been named by anything yet.
	if response := writeTopicMemory(t, fixture, agentSession, `{"enabled":true}`); response.Code != http.StatusOK {
		t.Fatalf("storing a decision without a quiz was refused: %s", response.Body.String())
	}
	requireRowSet(t, access, fixture.owner, agentSession, "consent with no open quiz",
		globalFact, episodeHere)
	requireNoteLayers(t, access, fixture.owner, agentSession, "")

	// (c)+(1)+(2)+(3)+(4)+(5): consent and a live quiz. This is the state the test exists for, and
	// the set is three rows: the two layers that were always readable, plus one subject.
	openQuiz(t, fixture, agentSession, "Redis 持久化")
	requireRowSet(t, access, fixture.owner, agentSession, "consent with a live quiz",
		globalFact, episodeHere, topicSubject)
	// (7) with the topic granted: the note lists all three layers, in the order the grant builds
	// them, and names the one subject - not the neighbour, not the typed spelling.
	requireNoteLayers(t, access, fixture.owner, agentSession, subject)
	for _, hit := range recallOnly(t, access, fixture.owner, agentSession) {
		if hit.Memory.MemoryID == topicNeighbour || hit.Memory.MemoryID == topicTyped {
			t.Fatalf("the consent leaked beyond its own normalized key: %q", hit.Content)
		}
		if hit.Memory.Scope == domain.MemoryScopeEpisodic && hit.Memory.SourceSessionID != agentSession {
			t.Fatalf("the read carried another conversation's episode: %q", hit.Content)
		}
	}

	// (d) Consent off: the added layer goes away on the next read and the two that remain are
	// exactly the ones that were there before it was ever switched on - row for row, note for note.
	if response := writeTopicMemory(t, fixture, agentSession, `{"enabled":false}`); response.Code != http.StatusOK {
		t.Fatalf("revoking was refused: %s", response.Body.String())
	}
	// (6)
	requireRowSet(t, access, fixture.owner, agentSession, "a revoked decision", globalFact, episodeHere)
	requireNoteLayers(t, access, fixture.owner, agentSession, "")

	// (e) A chat conversation is refused the switch, and the recall refuses it too when a row
	// reaches storage by another route. The set here is also the cross-session check from the other
	// side: this conversation's global fact is user-level and does appear, and the agent
	// conversation's episode does not.
	if response := writeTopicMemory(t, fixture, chatSession, `{"enabled":true}`); response.Code != http.StatusBadRequest {
		t.Fatalf("a chat conversation was switched on: %d %s", response.Code, response.Body.String())
	}
	if _, err := fixture.store.CommitTopicMemoryPreference(ctx, fixture.owner, chatSession, true); err != nil {
		t.Fatal(err)
	}
	openQuiz(t, fixture, chatSession, "Redis 持久化")
	requireRowSet(t, access, fixture.owner, chatSession, "a chat conversation with a planted row",
		globalFact, episodeElsewhere)
	requireNoteLayers(t, access, fixture.owner, chatSession, "")

	// (f) An expired quiz stops being a subject however fresh the consent, because the deadline is
	// checked where the grant is made rather than assumed from the store's listing.
	expired := newFixture(t, apiResponder{})
	expiredSession, expiredMessage := topicMemorySession(t, expired, domain.SessionModeLearningAgent)
	if _, err := expired.store.CommitTopicMemoryPreference(ctx, expired.owner, expiredSession, true); err != nil {
		t.Fatal(err)
	}
	expiredGlobal := confirmMemory(t, expired, expiredSession, expiredMessage, domain.MemoryScopeGlobal, "",
		"全局：到期测验也要留住的 fsync 中文偏好")
	confirmTopicMemory(t, expired, expiredSession, expiredMessage, subject, "redis 主题：已到期的 fsync 结论")
	openQuizExpiresAt(t, expired, expiredSession, "Redis 持久化", time.Now().Add(-time.Hour))
	requireRowSet(t, topicMemoryAccessFor(t, expired), expired.owner, expiredSession,
		"a quiz past its deadline", expiredGlobal)

	// (g) A consent in another account reaches none of this account's rows.
	other := foreignAgentSession(t, fixture)
	if _, err := fixture.store.CommitTopicMemoryPreference(ctx, fixture.other, other, true); err != nil {
		t.Fatal(err)
	}
	openQuizOn(t, fixture, fixture.other, other, "Redis 持久化")
	for _, hit := range recallOnly(t, access, fixture.other, other) {
		if hit.Memory.UserID == fixture.owner {
			t.Fatalf("another account read the owner's memory: %q", hit.Content)
		}
	}
}

// messageOf returns a message id in one conversation, so an episode filed from it cites
// provenance that exists.
func messageOf(t *testing.T, fixture *apiFixture, sessionID string) string {
	t.Helper()
	message, err := fixture.store.AppendMessage(t.Context(), fixture.owner, sessionID,
		&domain.Message{Role: "user", Content: "另一会话里问的 fsync"})
	if err != nil {
		t.Fatal(err)
	}
	return message.ID
}

// recallOnly is the read without the note, for loops over the rows themselves.
func recallOnly(t *testing.T, access *service.MemoryAccess, userID, sessionID string) []service.MemoryHit {
	t.Helper()
	hits, _ := recallTopics(t, access, userID, sessionID)
	return hits
}

// requireRowSet compares the exact set of memory ids a read returned against the exact set that
// should be there. Comparing sets rather than counts is what keeps "the topic replaced the default"
// from passing: that answer has the right size and the wrong rows.
func requireRowSet(t *testing.T, access *service.MemoryAccess, userID, sessionID, context string,
	want ...string) {
	t.Helper()
	hits, note := recallTopics(t, access, userID, sessionID)
	got := append([]string(nil), want...)
	sort.Strings(got)
	parts := make([]string, 0, len(hits))
	for _, hit := range hits {
		parts = append(parts, hit.Memory.MemoryID)
	}
	sort.Strings(parts)
	if strings.Join(parts, ",") != strings.Join(got, ",") {
		t.Fatalf("%s: read %s, want exactly %s (note %q)", context, strings.Join(parts, ","),
			strings.Join(got, ","), note)
	}
}

// requireNoteLayers checks the scope note against the layers the read actually searched: the
// grant's clause between "当前用户的" and the first "；" must list exactly the global layer, the
// authorized subject when there is one, and this conversation's episodes - in that order, with
// nothing extra. A note that overclaims a subject is as wrong as one that omits a layer that was
// searched, because the note is what an empty result is explained by.
func requireNoteLayers(t *testing.T, access *service.MemoryAccess, userID, sessionID, subject string) {
	t.Helper()
	hits, note := recallTopics(t, access, userID, sessionID)
	const prefix = "本轮检索范围：当前用户的"
	const suffix = "；未列入的层与未授权的主题不参与检索。"
	start := strings.Index(note, prefix)
	if start != 0 {
		t.Fatalf("the note does not open with the scope clause it must: %q", note)
	}
	body := note[len(prefix):]
	end := strings.Index(body, suffix)
	if end < 0 {
		t.Fatalf("the note does not close the scope clause: %q", note)
	}
	layerText, trailing := body[:end], body[end+len(suffix):]
	layers := strings.Split(layerText, "、")

	want := []string{"全局记忆", "本轮授权会话里的经历记录"}
	if subject != "" {
		want = []string{"全局记忆", "已授权主题的记忆（" + subject + "）", "本轮授权会话里的经历记录"}
	}
	if strings.Join(layers, "|") != strings.Join(want, "|") {
		t.Fatalf("the note lists %v, want the %d layers actually searched: %v (read %d rows)",
			layers, len(want), want, len(hits))
	}
	// The sentence that keeps an empty result honest is only attached when a subject was not
	// authorized; with one authorized and found, the note must not hedge about what it read.
	const hedge = "没有找到内容不能说明"
	if subject == "" && !strings.Contains(trailing, hedge) {
		t.Fatalf("an unauthorized read failed to say so: %q", note)
	}
	if subject != "" && strings.Contains(trailing, hedge) {
		t.Fatalf("a read that found its authorized subject still hedges about it: %q", note)
	}
}

// topicMemoryAccessFor builds the read side exactly as the binary builds it, over the same store
// the HTTP handlers write through. Two constructions here would mean the test was comparing itself
// rather than the wiring.
func topicMemoryAccessFor(t *testing.T, fixture *apiFixture) *service.MemoryAccess {
	t.Helper()
	grant, err := service.NewLearningTopicGrant(fixture.store)
	if err != nil {
		t.Fatal(err)
	}
	access, err := service.NewMemoryAccess(fixture.store, grant,
		service.MemorySearchOptions{MaxResults: 10})
	if err != nil {
		t.Fatal(err)
	}
	return access
}

func recallTopics(t *testing.T, access *service.MemoryAccess, userID, sessionID string) ([]service.MemoryHit, string) {
	t.Helper()
	hits, note, err := access.Recall(t.Context(), service.MemoryQuery{
		UserID: userID, SessionID: sessionID, Query: "fsync",
	})
	if err != nil {
		t.Fatalf("recall for %s in %s: %v", userID, sessionID, err)
	}
	return hits, note
}

// requireDefaultScope says the two things the switch must never break at once: no topic row is

func idsOfHits(hits []service.MemoryHit) string {
	parts := make([]string, 0, len(hits))
	for _, hit := range hits {
		parts = append(parts, hit.Memory.MemoryID)
	}
	return strings.Join(parts, ",")
}

// confirmMemory files and confirms one memory of any scope for the owner, citing a message that
// really is in this conversation: a recallable row needs provenance the store accepts, and a
// half-built row would test the seeding rather than the grant.
func confirmMemory(t *testing.T, fixture *apiFixture, sessionID, messageID string, scope domain.MemoryScope,
	topicKey, content string) string {
	t.Helper()
	ctx := t.Context()
	memory := service.NewMemoryService(fixture.store)
	stored, err := memory.CreateMemoryDraft(ctx, fixture.owner, domain.MemoryDraft{
		Scope: scope, TopicKey: topicKey, Content: content, Confidence: 1,
		SourceSessionID: sessionID, SourceMessageIDs: []string{messageID},
	})
	if err != nil {
		t.Fatal(err)
	}
	confirmed, err := memory.ConfirmMemoryDraft(ctx, fixture.owner, stored.MemoryID)
	if err != nil {
		t.Fatal(err)
	}
	return confirmed.MemoryID
}

// confirmTopicMemory is the topic scope of the same helper, named for what the grant is about.
func confirmTopicMemory(t *testing.T, fixture *apiFixture, sessionID, messageID, topicKey, content string) string {
	t.Helper()
	return confirmMemory(t, fixture, sessionID, messageID, domain.MemoryScopeTopic, topicKey, content)
}

// openQuiz files a live quiz on one conversation through the store, the way LearningService.Start
// leaves one behind after it has generated its questions.
func openQuiz(t *testing.T, fixture *apiFixture, sessionID, topic string) {
	t.Helper()
	openQuizExpiresAt(t, fixture, sessionID, topic, time.Now().Add(time.Hour))
}

// openQuizOn does the same for an explicit account, which is how a test puts two accounts in the
// same position and asks whether a consent in one of them can read a row from the other.
func openQuizOn(t *testing.T, fixture *apiFixture, userID, sessionID, topic string) {
	t.Helper()
	openQuizExpiresAtOn(t, fixture, userID, sessionID, topic, time.Now().Add(time.Hour))
}

func openQuizExpiresAt(t *testing.T, fixture *apiFixture, sessionID, topic string, expiresAt time.Time) {
	t.Helper()
	openQuizExpiresAtOn(t, fixture, fixture.owner, sessionID, topic, expiresAt)
}

func openQuizExpiresAtOn(t *testing.T, fixture *apiFixture, userID, sessionID, topic string,
	expiresAt time.Time) {
	t.Helper()
	if _, err := fixture.store.CreateLearningSession(t.Context(), &domain.LearningSession{
		UserID: userID, SessionID: sessionID, Topic: topic,
		TopicKey:        domain.NormalizeLearningTopicKey(topic),
		Status:          domain.LearningSessionCreated,
		CurrentQuestion: domain.LearningQuestionOrdinalFirst,
		TotalQuestions:  domain.LearningQuestionCount,
		Version:         1,
		ExpiresAt:       expiresAt,
	}); err != nil {
		t.Fatalf("open a quiz on %s: %v", sessionID, err)
	}
}

func foreignAgentSession(t *testing.T, fixture *apiFixture) string {
	t.Helper()
	session, err := fixture.store.CreateSession(t.Context(), &domain.Session{
		UserID: fixture.other, Title: "别人的学习会话", Mode: domain.SessionModeLearningAgent})
	if err != nil {
		t.Fatal(err)
	}
	return session.ID
}

// dissentingStore answers a commit with a state other than the one it was asked for. It is not a
// shape either real store produces; it is here because the handler has exactly one promise about
// its response - it reports what is stored - and a handler that echoed the request would keep every
// single-threaded test in this file passing while telling a client that raced another write that
// its own click won.
// enabledText spells a boolean the way this contract does, once, instead of in three places.
func enabledText(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

type dissentingStore struct {
	repository.Repository
	storeState bool
}

func (s *dissentingStore) CommitTopicMemoryPreference(ctx context.Context, userID, sessionID string,
	enabled bool) (*domain.TopicMemoryPreference, error) {
	stored, err := s.Repository.CommitTopicMemoryPreference(ctx, userID, sessionID, enabled)
	if err != nil {
		return nil, err
	}
	stored.Enabled = s.storeState
	return stored, nil
}

// TestSwitchAnswerReportsWhatWasStoredAndNotWhatWasSent pins both halves of the read-back rule: a
// successful write answers with the state the store returned, and a read answers with the stored
// state rather than a remembered request.
func TestSwitchAnswerReportsWhatWasStoredAndNotWhatWasSent(t *testing.T) {
	fixture := newFixture(t, apiResponder{})
	sessionID, _ := topicMemorySession(t, fixture, domain.SessionModeLearningAgent)

	for _, pair := range [][2]bool{{true, false}, {false, true}} {
		sent, stored := pair[0], pair[1]
		fixture.server.sessions = service.NewSessionService(&dissentingStore{
			Repository: fixture.store, storeState: stored,
		})
		response := writeTopicMemory(t, fixture, sessionID, `{"enabled":`+enabledText(sent)+`}`)
		requireStatus(t, response, http.StatusOK, "a write the store answered differently")
		want := `{"data":{"enabled":` + enabledText(stored) + `},"error":null}
`
		if response.Body.String() != want {
			t.Fatalf("the write of %v answered %q, want the stored state %q",
				sent, response.Body.String(), want)
		}
	}

	// The real store underneath still holds exactly one row, and it holds the last value that
	// really reached the store - which is the value a later read must answer with.
	fixture.server.sessions = service.NewSessionService(fixture.store)
	ctx := t.Context()
	stored, err := fixture.store.GetTopicMemoryPreference(ctx, fixture.owner, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	read, readBack := readTopicMemory(t, fixture, sessionID)
	requireStatus(t, read, http.StatusOK, "the read after the dissenting writes")
	if readBack == nil || *readBack != stored.Enabled {
		t.Fatalf("GET answered %v while the store holds %v", readBack, stored.Enabled)
	}
}
