package httpapi

import (
	"encoding/json"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// updateGolden rewrites testdata/http_contract.golden from the observed
// responses. Reviewers should treat a diff in that file as a contract change
// and check it line by line rather than regenerating it blindly.
var updateGolden = flag.Bool("update-golden", false, "rewrite the HTTP contract golden file from the observed responses")

const goldenFile = "testdata/http_contract.golden"

var (
	timestampPattern = regexp.MustCompile(`"(?:created_at|updated_at)":"[^"]*"`)
	jwtPattern       = regexp.MustCompile(`"token":"[^"]*"`)
	sessionIDPattern = regexp.MustCompile(`session-\d+`)
	messageIDPattern = regexp.MustCompile(`message-\d+`)
	// opaqueIDPattern covers the identifiers the stores generate from a clock and a
	// counter. They cannot be pinned literally, so the golden rows assert their shape and
	// everything around them - title, section, snippet, order, memory content and status -
	// while the exact values are checked structurally by the end-to-end tests. Memory ids
	// are minted the same way, so they belong in this set rather than in the literal text.
	opaqueIDPattern = regexp.MustCompile(`(collection|document|chunk|src|mem|lsn|lq|lat|tpr)-[0-9a-zA-Z-]+`)
	// The quiz deadlines and the projection's own timestamps. These are absent from every
	// row written before the learning surface existed, so widening this cannot restate an
	// older golden line: the canonicalizer only ever rewrites keys that appear.
	deadlinePattern = regexp.MustCompile(`"(?:expires_at|last_attempt|completed_at)":"[^"]*"`)
)

// canonicalizeContractText removes only what legitimately varies between runs: issued
// JWTs, wall-clock timestamps including a quiz's deadline, and clock-derived identifiers -
// knowledge, memory and learning rows are all minted the same way. Everything else,
// including the user id and every session and message field, is compared exactly.
func canonicalizeContractText(value string) string {
	value = timestampPattern.ReplaceAllStringFunc(value, func(match string) string {
		key, _, found := strings.Cut(match, `":"`)
		if !found {
			return match
		}
		return key + `":"-"`
	})
	value = jwtPattern.ReplaceAllString(value, `"token":"-"`)
	value = deadlinePattern.ReplaceAllStringFunc(value, func(match string) string {
		key, _, found := strings.Cut(match, `":"`)
		if !found {
			return match
		}
		return key + `":"-"`
	})
	value = sessionIDPattern.ReplaceAllString(value, "session-N")
	value = messageIDPattern.ReplaceAllString(value, "message-N")
	return opaqueIDPattern.ReplaceAllString(value, "${1}-O")
}

// contractCase is one replayed HTTP call plus nothing: the expected answer lives
// in testdata/http_contract.golden so the contract is a reviewable artifact.
type contractCase struct {
	name   string
	method string
	path   string
	body   string
	// token selects the credential: tokenOwner | tokenOther | tokenNone |
	// tokenBad | tokenMalformed.
	token string
	// quiet drives the request through a writer that cannot flush.
	quiet bool
	// memory asks for a fixture that also holds seeded long-term memory rows. Only
	// those cases see them: seeding consumes identifiers from the repository's shared
	// sequence, which would move the literal user id that the authentication rows
	// compare exactly.
	memory bool
	// learning asks for a fixture with the quiz workflow installed and one question
	// already answered. The same reason as `memory`: a quiz consumes model replies and
	// identifiers, and the answer it produced is what makes the progress and the
	// already-answered cases address real rows instead of a guessed id.
	learning bool
	// agent asks for a fixture holding one extra learning-agent conversation and no quiz.
	// The topic-memory rows are the only ones that need a mode without a subject: the switch
	// is stored and read on its own, and the quiz that would give it something to unlock is
	// exactly what those rows must not silently depend on.
	agent bool
}

const (
	tokenOwner     = "owner"
	tokenOther     = "other"
	tokenNone      = "empty"
	tokenBad       = "bad"
	tokenMalformed = "malformed"
)

// Paths carry placeholders filled from the fixture: the in-memory stores number
// entities from a shared clock-and-counter sequence, so a hardcoded id would
// silently aim at a missing row.
const (
	sessionPlaceholder         = "{{session}}"
	collectionPlaceholder      = "{{collection}}"
	documentPlaceholder        = "{{document}}"
	foreignPlaceholder         = "{{foreign}}"
	foreignDocumentPlaceholder = "{{foreign_document}}"
	memoryDraftPlaceholder     = "{{memory}}"
	memoryActivePlaceholder    = "{{memory_active}}"
	memoryForeignPlaceholder   = "{{memory_foreign}}"
	memoryMessagePlaceholder   = "{{memory_message}}"
	foreignSessionPlaceholder  = "{{foreign_session}}"
	// quizPlaceholder and learningSessionPlaceholder name the learning-agent conversation
	// a quiz is open on, and that quiz. Both come from a fixture that really ran one
	// answer, so no learning case can quietly address a row that was never written.
	quizPlaceholder            = "{{quiz}}"
	learningSessionPlaceholder = "{{learning_session}}"
	learningBusyPlaceholder    = "{{busy_session}}"
	// agentSessionPlaceholder names a learning-agent conversation with nothing running on it.
	// The topic-memory rows need a conversation whose mode grants the switch, and they must not
	// get one by borrowing the quiz fixture: that fixture grades an answer, which consumes
	// identifiers and would move every unrelated row's literals.
	agentSessionPlaceholder = "{{agent_session}}"
	topicMemoryPath         = "/api/v1/sessions/" + agentSessionPlaceholder + "/topic-memory"
	chatTopicMemoryPath     = sessionItemPath + "/topic-memory"
	learningPath            = "/api/v1/learning/sessions"
	learningItemPath        = learningPath + "/" + quizPlaceholder
	memoryPath              = "/api/v1/memories"
	memoryItemPath          = memoryPath + "/" + memoryDraftPlaceholder
	memoryActiveItemPath    = memoryPath + "/" + memoryActivePlaceholder
	memoryForeignItemPath   = memoryPath + "/" + memoryForeignPlaceholder
	sessionItemPath         = "/api/v1/sessions/" + sessionPlaceholder
	sessionMessagesPath     = sessionItemPath + "/messages"
	sessionStreamPath       = sessionItemPath + "/stream"
	collectionsPath         = "/api/v1/collections"
	collectionDocumentsPath = collectionsPath + "/" + collectionPlaceholder + "/documents"
	documentPath            = "/api/v1/documents/" + documentPlaceholder
)

func contractCases() []contractCase {
	repeat := strings.Repeat("t", 201)
	return []contractCase{
		// Authentication.
		{name: "register ok", method: http.MethodPost, path: "/api/v1/auth/register", body: `{"email":"new@example.com","password":"correct horse","display_name":"New"}`},
		{name: "register duplicate email", method: http.MethodPost, path: "/api/v1/auth/register", body: `{"email":"owner@example.com","password":"correct horse","display_name":"Owner"}`},
		{name: "register short password", method: http.MethodPost, path: "/api/v1/auth/register", body: `{"email":"x@example.com","password":"short","display_name":"X"}`},
		{name: "register unknown field", method: http.MethodPost, path: "/api/v1/auth/register", body: `{"email":"x@example.com","password":"correct horse","display_name":"X","role":"admin"}`},
		{name: "register wrong method", method: http.MethodGet, path: "/api/v1/auth/register"},
		{name: "register empty body", method: http.MethodPost, path: "/api/v1/auth/register", body: ""},
		{name: "register two json values", method: http.MethodPost, path: "/api/v1/auth/register", body: `{"email":"x@example.com","password":"correct horse"}{"email":"y@example.com"}`},
		{name: "register not json", method: http.MethodPost, path: "/api/v1/auth/register", body: `email=x`},
		// The fixture seeds its users straight through the repository, so the
		// stored password hash is not a bcrypt digest of any passphrase: login is
		// refused. TestAuthRoundTripOverHTTP covers the success path.
		{name: "login ok", method: http.MethodPost, path: "/api/v1/auth/login", body: `{"email":"owner@example.com","password":"correct horse"}`},
		{name: "login bad password", method: http.MethodPost, path: "/api/v1/auth/login", body: `{"email":"owner@example.com","password":"wrong passphrase"}`},
		{name: "login wrong method", method: http.MethodPut, path: "/api/v1/auth/login", body: `{}`},
		{name: "me anonymous", method: http.MethodGet, path: "/api/v1/me", token: tokenNone},
		{name: "me bad token", method: http.MethodGet, path: "/api/v1/me", token: tokenBad},
		{name: "me non bearer", method: http.MethodGet, path: "/api/v1/me", token: tokenMalformed},
		{name: "me ok", method: http.MethodGet, path: "/api/v1/me", token: tokenOwner},
		{name: "me wrong method", method: http.MethodPost, path: "/api/v1/me", token: tokenOwner},

		// Session creation, listing and the closed mode set.
		{name: "sessions list", method: http.MethodGet, path: "/api/v1/sessions", token: tokenOwner},
		{name: "sessions list anonymous", method: http.MethodGet, path: "/api/v1/sessions", token: tokenNone},
		{name: "create chat", method: http.MethodPost, path: "/api/v1/sessions", body: `{"title":"Chat","mode":"chat"}`, token: tokenOwner},
		{name: "create learning agent", method: http.MethodPost, path: "/api/v1/sessions", body: `{"title":"Agent","mode":"learning_agent"}`, token: tokenOwner},
		{name: "create missing mode", method: http.MethodPost, path: "/api/v1/sessions", body: `{"title":"No mode"}`, token: tokenOwner},
		{name: "create retired fast_chat", method: http.MethodPost, path: "/api/v1/sessions", body: `{"mode":"fast_chat"}`, token: tokenOwner},
		{name: "create knowledge_qa as mode", method: http.MethodPost, path: "/api/v1/sessions", body: `{"mode":"knowledge_qa"}`, token: tokenOwner},
		{name: "create direct_chat as mode", method: http.MethodPost, path: "/api/v1/sessions", body: `{"mode":"direct_chat"}`, token: tokenOwner},
		{name: "create agent as mode", method: http.MethodPost, path: "/api/v1/sessions", body: `{"mode":"agent"}`, token: tokenOwner},
		{name: "create unknown mode", method: http.MethodPost, path: "/api/v1/sessions", body: `{"mode":"nope"}`, token: tokenOwner},
		{name: "create numeric mode", method: http.MethodPost, path: "/api/v1/sessions", body: `{"mode":7}`, token: tokenOwner},
		{name: "create oversized title", method: http.MethodPost, path: "/api/v1/sessions", body: `{"mode":"chat","title":"` + repeat + `"}`, token: tokenOwner},
		{name: "sessions unsupported method", method: http.MethodPatch, path: "/api/v1/sessions", body: `{}`, token: tokenOwner},

		// Messages, per-turn knowledge options and ownership.
		{name: "send message", method: http.MethodPost, path: sessionMessagesPath, body: `{"content":"question"}`, token: tokenOwner},
		{name: "send with knowledge", method: http.MethodPost, path: sessionMessagesPath, body: `{"content":"question","knowledge":{"enabled":true,"collection_ids":["a"," a ","b"]}}`, token: tokenOwner},
		{name: "send knowledge without collections", method: http.MethodPost, path: sessionMessagesPath, body: `{"content":"question","knowledge":{"enabled":true}}`, token: tokenOwner},
		{name: "send disabled with collections", method: http.MethodPost, path: sessionMessagesPath, body: `{"content":"question","knowledge":{"collection_ids":["a"]}}`, token: tokenOwner},
		{name: "send blank collection", method: http.MethodPost, path: sessionMessagesPath, body: `{"content":"question","knowledge":{"enabled":true,"collection_ids":["  "]}}`, token: tokenOwner},
		{name: "send with mode field", method: http.MethodPost, path: sessionMessagesPath, body: `{"content":"question","mode":"learning_agent"}`, token: tokenOwner},
		{name: "send with strategy field", method: http.MethodPost, path: sessionMessagesPath, body: `{"content":"question","answer_strategy":"agent"}`, token: tokenOwner},
		{name: "send legacy message field", method: http.MethodPost, path: sessionMessagesPath, body: `{"message":"question"}`, token: tokenOwner},
		{name: "send empty content", method: http.MethodPost, path: sessionMessagesPath, body: `{"content":"   "}`, token: tokenOwner},
		{name: "send malformed json", method: http.MethodPost, path: sessionMessagesPath, body: `{"content":`, token: tokenOwner},
		{name: "send cross owner", method: http.MethodPost, path: sessionMessagesPath, body: `{"content":"question"}`, token: tokenOther},
		{name: "send unknown session", method: http.MethodPost, path: "/api/v1/sessions/session-404/messages", body: `{"content":"question"}`, token: tokenOwner},
		{name: "send anonymous", method: http.MethodPost, path: sessionMessagesPath, body: `{"content":"question"}`, token: tokenNone},
		{name: "messages wrong method", method: http.MethodDelete, path: sessionMessagesPath, token: tokenOwner},
		{name: "list messages", method: http.MethodGet, path: sessionMessagesPath, token: tokenOwner},
		{name: "list messages cross owner", method: http.MethodGet, path: sessionMessagesPath, token: tokenOther},
		{name: "list messages anonymous", method: http.MethodGet, path: sessionMessagesPath, token: tokenNone},

		// SSE.
		{name: "stream ok", method: http.MethodPost, path: sessionStreamPath, body: `{"content":"question"}`, token: tokenOwner},
		{name: "stream with knowledge", method: http.MethodPost, path: sessionStreamPath, body: `{"content":"question","knowledge":{"enabled":true,"collection_ids":["a"]}}`, token: tokenOwner},
		{name: "stream invalid knowledge", method: http.MethodPost, path: sessionStreamPath, body: `{"content":"question","knowledge":{"enabled":true}}`, token: tokenOwner},
		{name: "stream mode field rejected", method: http.MethodPost, path: sessionStreamPath, body: `{"content":"question","mode":"chat"}`, token: tokenOwner},
		{name: "stream cross owner", method: http.MethodPost, path: sessionStreamPath, body: `{"content":"question"}`, token: tokenOther},
		{name: "stream anonymous", method: http.MethodPost, path: sessionStreamPath, body: `{"content":"question"}`, token: tokenNone},
		{name: "stream wrong method", method: http.MethodGet, path: sessionStreamPath, token: tokenOwner},
		{name: "stream writer cannot flush", method: http.MethodPost, path: sessionStreamPath, body: `{"content":"question"}`, token: tokenOwner, quiet: true},

		// Deleting, and the routing shapes a hand written mux used to decide.
		{name: "delete owner", method: http.MethodDelete, path: sessionItemPath, token: tokenOwner},
		{name: "delete cross owner", method: http.MethodDelete, path: sessionItemPath, token: tokenOther},
		{name: "delete unknown session", method: http.MethodDelete, path: "/api/v1/sessions/session-404", token: tokenOwner},
		{name: "item wrong method", method: http.MethodPut, path: sessionItemPath, body: `{"mode":"chat"}`, token: tokenOwner},
		{name: "item patch anonymous", method: http.MethodPatch, path: sessionItemPath, body: `{}`, token: tokenNone},
		{name: "item get", method: http.MethodGet, path: sessionItemPath, token: tokenOwner},
		{name: "unknown api path", method: http.MethodGet, path: "/api/v1/nope"},
		{name: "root path", method: http.MethodGet, path: "/"},
		{name: "sessions trailing slash", method: http.MethodGet, path: "/api/v1/sessions/"},
		{name: "session trailing slash", method: http.MethodDelete, path: sessionItemPath + "/", token: tokenOwner},
		{name: "session double slash", method: http.MethodGet, path: "/api/v1/sessions//messages", token: tokenOwner},
		{name: "session deep path", method: http.MethodGet, path: sessionItemPath + "/messages/extra", token: tokenOwner},
		{name: "session deep path anonymous", method: http.MethodGet, path: sessionItemPath + "/messages/extra", token: tokenNone},
		{name: "encoded separator", method: http.MethodDelete, path: "/api/v1/sessions/a%2Fb", token: tokenOwner},
		{name: "unknown action", method: http.MethodGet, path: sessionItemPath + "/other", token: tokenOwner},
		{name: "me trailing slash", method: http.MethodGet, path: "/api/v1/me/", token: tokenOwner},
		{name: "head on me", method: http.MethodHead, path: "/api/v1/me", token: tokenOwner},
		{name: "options on session item", method: http.MethodOptions, path: sessionItemPath, token: tokenOwner},
		{name: "options on collection", method: http.MethodOptions, path: "/api/v1/sessions", token: tokenOwner},
		{name: "post to stream item path", method: http.MethodPost, path: sessionItemPath, body: `{}`, token: tokenOwner},
		{name: "delete on messages", method: http.MethodDelete, path: sessionMessagesPath, token: tokenOwner},
		{name: "trace on register", method: http.MethodTrace, path: "/api/v1/auth/register"},
		{name: "uppercase path", method: http.MethodGet, path: "/API/V1/ME", token: tokenOwner},
		{name: "health style unknown", method: http.MethodPost, path: "/api/v1/sessions", body: `{"mode":"chat","title":"ok"}`, token: tokenOwner},
		// Phase 8D: a body that cannot be UTF-8 is refused rather than stored as replacement
		// characters. Every one of these answers with the same bytes a malformed body already
		// produced, and the last one is the positive control - a correctly paired surrogate
		// escape is a real emoji and must still be accepted.
		{name: "send message invalid utf-8", method: http.MethodPost, path: sessionMessagesPath, body: `{"content":"pre` + "\xff\xfe" + `post"}`, token: tokenOwner},
		{name: "stream invalid utf-8", method: http.MethodPost, path: sessionStreamPath, body: `{"content":"pre` + "\xff\xfe" + `post"}`, token: tokenOwner},
		{name: "send message truncated utf-8", method: http.MethodPost, path: sessionMessagesPath, body: `{"content":"中文` + "\xe6\x9c" + `"}`, token: tokenOwner},
		{name: "send message lone surrogate escape", method: http.MethodPost, path: sessionMessagesPath, body: `{"content":"` + `\ud800"}`, token: tokenOwner},
		{name: "send message dangling high surrogate", method: http.MethodPost, path: sessionMessagesPath, body: `{"content":"` + `\ud83d"}`, token: tokenOwner},
		{name: "collection title invalid utf-8", method: http.MethodPost, path: "/api/v1/collections", body: `{"title":"手册` + "\xc3\x28" + `"}`, token: tokenOwner},
		{name: "memory draft invalid utf-8", method: http.MethodPost, path: "/api/v1/memories", body: `{"scope":"global","content":"` + "\xf4\x90\x80\x80" + `","source_session_id":"session-1","source_message_ids":["message-1"]}`, token: tokenOwner},
		{name: "send message surrogate pair emoji", method: http.MethodPost, path: sessionMessagesPath, body: `{"content":"` + `\ud83d\udd2c"}`, token: tokenOwner},
	}
}

type goldenEntry struct {
	Name    string `json:"name"`
	Status  int    `json:"status"`
	Headers string `json:"headers"`
	Body    string `json:"body"`
}

func loadGolden(t *testing.T) map[string]goldenEntry {
	t.Helper()
	contents, err := os.ReadFile(goldenFile)
	if err != nil {
		t.Fatalf("read %s: %v", goldenFile, err)
	}
	var entries []goldenEntry
	if err := json.Unmarshal(contents, &entries); err != nil {
		t.Fatalf("decode %s: %v", goldenFile, err)
	}
	byName := make(map[string]goldenEntry, len(entries))
	for _, entry := range entries {
		byName[entry.Name] = entry
	}
	return byName
}

func TestHTTPContractGolden(t *testing.T) {
	cases := allContractCases()
	golden := loadGolden(t)

	names := make(map[string]bool, len(cases))
	for _, testCase := range cases {
		if names[testCase.name] {
			t.Fatalf("duplicate case name %q", testCase.name)
		}
		names[testCase.name] = true
	}
	// A stale golden row would hide a deleted case, and a missing row would let a
	// new case pass unasserted. Regenerating is the one moment when that is
	// expected, so the check is skipped for -update-golden.
	if !*updateGolden {
		for name := range names {
			if _, ok := golden[name]; !ok {
				t.Errorf("golden file has no entry for case %q", name)
			}
		}
		for name := range golden {
			if !names[name] {
				t.Errorf("golden file holds an unknown case %q", name)
			}
		}
		if t.Failed() {
			t.FailNow()
		}
	}

	observed := make(map[string]goldenEntry, len(cases))
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newFixture(t, apiResponder{})
			if testCase.memory {
				fixture = newMemoryFixture(t)
			}
			if testCase.learning {
				fixture = newLearningContractFixture(t)
			}
			if testCase.agent {
				fixture = newAgentContractFixture(t)
			}
			status, headers, body := performContractRequest(t, fixture.server.Handler(), testCase, fixture)
			entry := goldenEntry{Name: testCase.name, Status: status, Headers: flattenHeaders(headers), Body: canonicalizeContractText(body)}
			observed[testCase.name] = entry
			want := golden[testCase.name]
			if entry.Status != want.Status {
				t.Fatalf("status = %d, golden says %d\nbody: %s", entry.Status, want.Status, entry.Body)
			}
			if entry.Headers != want.Headers {
				t.Fatalf("headers = %q, golden says %q", entry.Headers, want.Headers)
			}
			if entry.Body != want.Body {
				t.Fatalf("body mismatch\n got: %q\nwant: %q", entry.Body, want.Body)
			}
		})
	}

	if *updateGolden {
		writeGoldenContract(t, observed)
	}
}

func writeGoldenContract(t *testing.T, observed map[string]goldenEntry) {
	t.Helper()
	entries := make([]goldenEntry, 0, len(observed))
	for _, testCase := range allContractCases() {
		entries = append(entries, observed[testCase.name])
	}
	encoded, err := json.MarshalIndent(entries, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(goldenFile), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(goldenFile, append(encoded, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("rewrote %s with %d cases", goldenFile, len(entries))
}

// performContractRequest replays one case against a router. Ids come from the
// fixture so no case can quietly target a row that was never seeded.
func performContractRequest(t *testing.T, handler http.Handler, testCase contractCase, fixture *apiFixture) (int, http.Header, string) {
	t.Helper()
	replacements := []string{
		sessionPlaceholder, fixture.session,
		collectionPlaceholder, fixture.collection,
		documentPlaceholder, fixture.document,
		foreignPlaceholder, fixture.foreignCollection,
		foreignDocumentPlaceholder, fixture.foreignDocument,
		memoryDraftPlaceholder, fixture.memoryDraft,
		memoryActivePlaceholder, fixture.memoryActive,
		memoryForeignPlaceholder, fixture.foreignDraft,
		memoryMessagePlaceholder, fixture.memoryMessage,
		foreignSessionPlaceholder, fixture.memoryForeignSession,
		quizPlaceholder, fixture.learningQuiz,
		learningSessionPlaceholder, fixture.learningSession,
		learningBusyPlaceholder, fixture.learningBusySession,
		agentSessionPlaceholder, fixture.agentSession,
	}
	path := strings.NewReplacer(replacements...).Replace(testCase.path)
	body := strings.NewReplacer(replacements...).Replace(testCase.body)

	request := httptest.NewRequest(testCase.method, path, strings.NewReader(body))
	switch testCase.token {
	case tokenNone, "":
	case tokenBad:
		request.Header.Set("Authorization", "Bearer not-a-jwt")
	case tokenMalformed:
		request.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	case tokenOther:
		request.Header.Set("Authorization", "Bearer "+issueTestToken(t, fixture.server, fixture.other))
	default:
		request.Header.Set("Authorization", "Bearer "+issueTestToken(t, fixture.server, fixture.owner))
	}
	request.Header.Set("Content-Type", "application/json")

	if testCase.quiet {
		recorder := &nonFlushingRecorder{}
		handler.ServeHTTP(recorder, request)
		return recorder.status(), recorder.Header(), recorder.body.String()
	}
	recorder := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	handler.ServeHTTP(recorder, request)
	return recorder.Code, recorder.Header(), recorder.Body.String()
}

func flattenHeaders(header http.Header) string {
	keys := make([]string, 0, len(header))
	for key := range header {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, strings.ToLower(key)+"="+strings.Join(header[key], ","))
	}
	return strings.Join(parts, ";")
}

// nonFlushingRecorder has no Flush method on purpose: it reproduces a writer
// that cannot stream, which the stream route must answer with a 500 envelope
// rather than a silently buffered 200.
type nonFlushingRecorder struct {
	header http.Header
	code   int
	body   strings.Builder
}

func (r *nonFlushingRecorder) Header() http.Header {
	if r.header == nil {
		r.header = http.Header{}
	}
	return r.header
}

func (r *nonFlushingRecorder) Write(payload []byte) (int, error) {
	if r.code == 0 {
		r.code = http.StatusOK
	}
	return r.body.Write(payload)
}

func (r *nonFlushingRecorder) WriteHeader(status int) {
	if r.code == 0 {
		r.code = status
	}
}

func (r *nonFlushingRecorder) status() int {
	if r.code == 0 {
		return http.StatusOK
	}
	return r.code
}

// TestAuthRoundTripOverHTTP covers the path the golden table cannot: the fixture
// seeds users straight through the repository, so their stored hash is not a
// bcrypt digest and login is refused. Registering creates a real hash, and the
// token that comes back must then open the protected routes.
func TestAuthRoundTripOverHTTP(t *testing.T) {
	server, _, _, _ := newAPIFixture(t, apiResponder{})
	handler := server.Handler()

	registered := performRaw(t, handler, http.MethodPost, "/api/v1/auth/register", "", `{"email":"roundtrip@example.com","password":"correct horse battery","display_name":"Round Trip"}`)
	if registered.status != http.StatusOK {
		t.Fatalf("register status/body = %d %s", registered.status, registered.body)
	}
	if !strings.Contains(registered.body, `"email":"roundtrip@example.com"`) || strings.Contains(registered.body, "password") {
		t.Fatalf("register body is wrong or leaked the password: %s", registered.body)
	}

	loggedIn := performRaw(t, handler, http.MethodPost, "/api/v1/auth/login", "", `{"email":"roundtrip@example.com","password":"correct horse battery"}`)
	if loggedIn.status != http.StatusOK {
		t.Fatalf("login status/body = %d %s", loggedIn.status, loggedIn.body)
	}
	var envelope struct {
		Data struct {
			Token string `json:"token"`
			User  struct {
				ID string `json:"id"`
			} `json:"user"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(loggedIn.body), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Data.Token == "" || envelope.Data.User.ID == "" {
		t.Fatalf("login response is missing credentials: %s", loggedIn.body)
	}

	me := performRaw(t, handler, http.MethodGet, "/api/v1/me", envelope.Data.Token, "")
	if me.status != http.StatusOK || !strings.Contains(me.body, `"email":"roundtrip@example.com"`) {
		t.Fatalf("me with the issued token = %d %s", me.status, me.body)
	}

	created := performRaw(t, handler, http.MethodPost, "/api/v1/sessions", envelope.Data.Token, `{"title":"Round trip","mode":"chat"}`)
	if created.status != http.StatusOK || !strings.Contains(created.body, `"mode":"chat"`) {
		t.Fatalf("session created with the issued token = %d %s", created.status, created.body)
	}

	// A token from a different issuer or a tampered signature must not open the
	// protected route.
	tampered := envelope.Data.Token[:len(envelope.Data.Token)-2] + "ab"
	rejected := performRaw(t, handler, http.MethodGet, "/api/v1/me", tampered, "")
	if rejected.status != http.StatusUnauthorized {
		t.Fatalf("tampered token status = %d, want 401", rejected.status)
	}
}

type rawResponse struct {
	status  int
	headers http.Header
	body    string
}

func performRaw(t *testing.T, handler http.Handler, method, path, token, body string) rawResponse {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return rawResponse{status: recorder.Code, headers: recorder.Header(), body: recorder.Body.String()}
}
