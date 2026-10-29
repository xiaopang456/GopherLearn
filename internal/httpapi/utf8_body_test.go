package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/responder"
)

// Phase 8D: a request body that cannot be UTF-8 text is refused before the JSON decoder sees it.
// `encoding/json` would otherwise replace each bad byte with U+FFFD and report no error at all,
// storing a row that looks intact and is unreadable - the failure is silent in both directions,
// which is why the gate has to sit on the way in.

// mixedScript is what must keep working verbatim: a Chinese heading, an emoji and 𠮷, a
// character outside the basic plane.
const mixedScript = "主题：Redis 持久化 🔬 𠮷田 结束"

// rejectedBodyEnvelope is the one answer every route gives for every broken shape below: the same
// bytes a malformed body already produced, so this boundary adds no new public error code.
const rejectedBodyEnvelope = `{"data":null,"error":{"code":"invalid_request","message":"request body is invalid"}}` + "\n"

func utf8Token(t *testing.T, fixture *apiFixture) string {
	t.Helper()
	token, err := fixture.tokens.Issue(fixture.owner)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func doRaw(t *testing.T, server *Server, method, path, token string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(string(body)))
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	return response
}

func requireRefused(t *testing.T, response *httptest.ResponseRecorder, context string) {
	t.Helper()
	if response.Code != http.StatusBadRequest {
		t.Fatalf("%s: status=%d, want 400, body=%s", context, response.Code, response.Body.String())
	}
	if got := strings.TrimSuffix(response.Body.String(), "\n"); got != rejectedBodyEnvelope[:len(rejectedBodyEnvelope)-1] {
		t.Fatalf("%s: body=%q, want %q", context, response.Body.String(), rejectedBodyEnvelope)
	}
}

// countingResponder is the assistant double for the "nothing ran" assertions.
type countingResponder struct {
	responded int
	streamed  int
}

func (r *countingResponder) Respond(_ context.Context, _ responder.Request) (responder.Answer, error) {
	r.responded++
	return responder.Answer{Content: "counted answer"}, nil
}

func (r *countingResponder) Stream(_ context.Context, _ responder.Request, emit func(string) error) (responder.Answer, error) {
	r.streamed++
	if err := emit("counted "); err != nil {
		return responder.Answer{}, err
	}
	if err := emit("answer"); err != nil {
		return responder.Answer{}, err
	}
	return responder.Answer{Content: "counted answer"}, nil
}

// brokenBodyCases are the shapes a client produces by accident. `structure` names whether the
// JSON around the bad bytes is well formed, because that is the difference between "the request
// was junk" - which never worked - and "the request was a perfectly good envelope carrying
// destroyed text", which used to be stored.
type brokenBody struct {
	name      string
	value     string // the bad bytes, as they would appear inside a JSON string value
	structure string
}

func brokenBodyCases() []brokenBody {
	return []brokenBody{
		{name: "not json at all", value: "\xff\xfe\xfd\x00", structure: "none"},
		{name: "valid structure, bad bytes in a value", value: "pre\xff\xfe" + "post", structure: "object"},
		{name: "truncated multi-byte sequence", value: "中文" + string([]byte{0xe6, 0x9c}), structure: "object"},
		{name: "lone high surrogate escape", value: `\ud800`, structure: "object"},
		{name: "lone low surrogate escape", value: `\udcbf`, structure: "object"},
		{name: "high surrogate interrupted", value: `\ud83dx`, structure: "object"},
		{name: "high surrogate left dangling", value: `\ud83d`, structure: "object"},
	}
}

// turnBody and nameBody place a case's bytes in the field each route declares, so a refusal can
// only be about the encoding and never about an unknown key or a missing value.
func turnBody(caseValue string) []byte {
	return []byte(`{"content":"` + caseValue + `"}`)
}

func nameBody(field, caseValue string) []byte {
	return []byte(`{"` + field + `":"` + caseValue + `"}`)
}

// TestMixedScriptBodiesAreStoredVerbatim is the half that must keep working.
func TestMixedScriptBodiesAreStoredVerbatim(t *testing.T) {
	assistant := &countingResponder{}
	fixture := newFixture(t, assistant)
	token := utf8Token(t, fixture)
	ctx := t.Context()

	posted, err := json.Marshal(map[string]any{"content": mixedScript})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("message turn stores the exact runes", func(t *testing.T) {
		response := doRaw(t, fixture.server, http.MethodPost, "/api/v1/sessions/"+fixture.session+"/messages", token, posted)
		requireStatus(t, response, http.StatusOK, "message turn")
		messages, err := fixture.store.ListMessages(ctx, fixture.owner, fixture.session)
		if err != nil {
			t.Fatal(err)
		}
		if messages[0].Content != mixedScript {
			t.Fatalf("stored user message = %q, want %q", messages[0].Content, mixedScript)
		}
		if !utf8.ValidString(messages[0].Content) || strings.ContainsRune(messages[0].Content, 0xFFFD) {
			t.Fatalf("stored message is not clean UTF-8: %q", messages[0].Content)
		}
	})

	t.Run("emoji spelled as a surrogate pair arrives as the emoji", func(t *testing.T) {
		// The same character written two ways. A client that escapes non-ASCII must not be
		// punished for it, or this gate would break working clients.
		response := doRaw(t, fixture.server, http.MethodPost, "/api/v1/sessions/"+fixture.session+"/messages", token,
			[]byte(`{"content":"\ud83d\udd2c 𠮷"}`))
		requireStatus(t, response, http.StatusOK, "escaped pair")
		messages, err := fixture.store.ListMessages(ctx, fixture.owner, fixture.session)
		if err != nil {
			t.Fatal(err)
		}
		if got := messages[len(messages)-2].Content; got != "🔬 𠮷" {
			t.Fatalf("an escaped pair produced %q, want %q", got, "🔬 𠮷")
		}
	})

	t.Run("sse turn stores the exact runes", func(t *testing.T) {
		before := countStoredMessages(t, fixture.server, token, fixture.session)
		response := doRaw(t, fixture.server, http.MethodPost, "/api/v1/sessions/"+fixture.session+"/stream", token, posted)
		requireStatus(t, response, http.StatusOK, "stream turn")
		if !strings.Contains(response.Body.String(), "message.completed") {
			t.Fatalf("stream never reached its terminal event: %s", response.Body.String())
		}
		if countStoredMessages(t, fixture.server, token, fixture.session) != before+2 {
			t.Fatal("the stream turn did not persist a pair of messages")
		}
		messages, err := fixture.store.ListMessages(ctx, fixture.owner, fixture.session)
		if err != nil {
			t.Fatal(err)
		}
		if messages[len(messages)-2].Content != mixedScript {
			t.Fatalf("streamed user message = %q, want %q", messages[len(messages)-2].Content, mixedScript)
		}
	})

	t.Run("collection title and document body keep their scripts", func(t *testing.T) {
		title, err := json.Marshal(map[string]any{"title": mixedScript})
		if err != nil {
			t.Fatal(err)
		}
		created := doRaw(t, fixture.server, http.MethodPost, "/api/v1/collections", token, title)
		requireStatus(t, created, http.StatusOK, "collection create")
		var collectionEnvelope struct {
			Data collectionView `json:"data"`
		}
		if err := json.Unmarshal(created.Body.Bytes(), &collectionEnvelope); err != nil {
			t.Fatal(err)
		}
		if collectionEnvelope.Data.Title != mixedScript {
			t.Fatalf("collection title = %q, want %q", collectionEnvelope.Data.Title, mixedScript)
		}

		content := mixedScript + "\n\n" + strings.Repeat("这一节说明两种持久化方式的取舍条件与代价。", 6) + "\n"
		document, err := json.Marshal(map[string]any{"title": "运行手册.md", "format": "markdown", "content": content})
		if err != nil {
			t.Fatal(err)
		}
		uploaded := doRaw(t, fixture.server, http.MethodPost, "/api/v1/collections/"+collectionEnvelope.Data.ID+"/documents", token, document)
		requireStatus(t, uploaded, http.StatusOK, "document upload")
		var documentEnvelope struct {
			Data documentView `json:"data"`
		}
		if err := json.Unmarshal(uploaded.Body.Bytes(), &documentEnvelope); err != nil {
			t.Fatal(err)
		}
		// The document row keeps no body by design - the chunks are the stored text - so the
		// fidelity check runs against what retrieval would actually read back.
		chunks, err := fixture.knowledge.ChunksFor(ctx, fixture.owner, []string{collectionEnvelope.Data.ID})
		if err != nil {
			t.Fatal(err)
		}
		var joined strings.Builder
		for _, chunk := range chunks {
			joined.WriteString(chunk.Text)
		}
		if !strings.Contains(joined.String(), mixedScript) {
			t.Fatalf("the stored chunks lost the submitted scripts: %q", joined.String())
		}
		if strings.ContainsRune(joined.String(), 0xFFFD) {
			t.Fatalf("a replacement character reached storage: %q", joined.String())
		}
		if len(chunks) == 0 {
			t.Fatal("the upload produced no chunks at all")
		}
		_ = content
	})

	t.Run("memory proposal keeps its scripts", func(t *testing.T) {
		body, err := json.Marshal(map[string]any{
			"scope": "global", "content": mixedScript,
		})
		if err != nil {
			t.Fatal(err)
		}
		response := doRaw(t, fixture.server, http.MethodPost, "/api/v1/memories", token, body)
		requireStatus(t, response, http.StatusOK, "memory draft")
		var envelope struct {
			Data memoryView `json:"data"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.Data.Content != mixedScript {
			t.Fatalf("stored memory = %q, want %q", envelope.Data.Content, mixedScript)
		}
	})

	if assistant.responded == 0 || assistant.streamed == 0 {
		t.Fatalf("a valid turn never reached the assistant: responded=%d streamed=%d", assistant.responded, assistant.streamed)
	}
}

// TestBrokenBodiesAreRefusedOnEveryBodyRoute covers sync and streaming turns plus every route
// that takes a body, and pins that the answer is one stable envelope rather than a per-route
// opinion.
func TestBrokenBodiesAreRefusedOnEveryBodyRoute(t *testing.T) {
	fixture := newFixture(t, &countingResponder{})
	token := utf8Token(t, fixture)

	for _, shape := range brokenBodyCases() {
		for _, route := range []string{"/messages", "/stream"} {
			requireRefused(t, doRaw(t, fixture.server, http.MethodPost, "/api/v1/sessions/"+fixture.session+route, token, turnBody(shape.value)),
				"turn"+route+" / "+shape.name+" ("+shape.structure+" structure)")
		}
		// The same bytes inside fields this route actually declares, so the refusal below can
		// only be about the encoding and never about an unknown key.
		requireRefused(t, doRaw(t, fixture.server, http.MethodPost, "/api/v1/auth/register", token,
			[]byte(`{"email":"x@example.com","password":"correct horse battery","display_name":"`+shape.value+`"}`)),
			"register / "+shape.name)
	}

	// The routes whose fields differ from a turn get their own broken body each: the point is
	// the byte gate, and it lives in the one decoder they all share. The quiz actions are not
	// listed here because a server without the quiz workflow answers them with 500 before it
	// reads a body; they are refused with a real open quiz in
	// TestRejectedQuizAnswerKeepsTheSubmissionIdUsable.
	single := []struct {
		name, method, path, body string
	}{
		{name: "session title", method: http.MethodPost, path: "/api/v1/sessions", body: `{"title":"` + "\xc3\x28" + `","mode":"chat"}`},
		{name: "collection title", method: http.MethodPost, path: "/api/v1/collections", body: `{"title":"` + "\xff" + `"}`},
		{name: "document content", method: http.MethodPost, path: "/api/v1/collections/" + fixture.collection + "/documents",
			body: `{"title":"a.md","format":"markdown","content":"` + "\xed\xa0\x80" + `"}`},
		{name: "memory content", method: http.MethodPost, path: "/api/v1/memories",
			body: `{"scope":"global","content":"` + "\xf4\x90\x80\x80" + `","source_session_id":"` + fixture.session + `","source_message_ids":[]}`},
	}
	for _, route := range single {
		requireRefused(t, doRaw(t, fixture.server, route.method, route.path, token, []byte(route.body)), route.name)
	}
	// The two quiz actions are not in that list because a server without the quiz workflow
	// answers them with 500 before it reads a body at all. They are refused below, against a
	// real open quiz, where a 400 can only come from the decoder.
	quiz := newLearningHTTPFixture(t, learningQuestionReply("字节门"), learningGradeReply())
	quizID := startQuizForUTF8Test(t, quiz)
	requireRefused(t, doRaw(t, quiz.app.server, http.MethodPost, "/api/v1/learning/sessions/"+quizID+"/answer", quiz.token,
		[]byte(`{"submission_id":"utf8-route-1","ordinal":1,"answer":"`+string([]byte{0xff, 0xfe})+`"}`)), "quiz answer")
	requireRefused(t, doRaw(t, quiz.app.server, http.MethodPost, "/api/v1/learning/sessions/"+quizID+"/skip", quiz.token,
		[]byte(`{"submission_id":"`+string([]byte{0xc0, 0x80})+`"}`)), "quiz skip")
}

// TestEveryBodyRouteSharesTheOneDecoder is the cheap structural guarantee behind that matrix: a
// route added tomorrow cannot skip the gate unless it stops using decodeJSON, and this fails the
// moment it does.
func TestEveryBodyRouteSharesTheOneDecoder(t *testing.T) {
	for _, name := range []string{"server.go", "knowledge.go", "memory.go", "learning.go"} {
		text := httpapiSource(t, name)
		for _, forbidden := range []string{"json.NewDecoder(r.Body", "json.NewDecoder(request.Body", "json.Unmarshal(body"} {
			if strings.Contains(text, forbidden) {
				t.Errorf("%s parses a request body outside decodeJSON: %q", name, forbidden)
			}
		}
	}
}

// TestRejectedBodyChangesNothing is the part a status code cannot show: the request must not have
// written a row, spent a model call, or left the conversation wedged.
func TestRejectedBodyChangesNothing(t *testing.T) {
	assistant := &countingResponder{}
	fixture := newFixture(t, assistant)
	token := utf8Token(t, fixture)

	messagesBefore := countStoredMessages(t, fixture.server, token, fixture.session)
	sessionBefore := fixtureSession(t, fixture)
	collectionsBefore := len(fixtureCollections(t, fixture))
	memoriesBefore := len(listMemories(t, fixture, token, "pending")) + len(listMemories(t, fixture, token, "active"))

	for _, shape := range brokenBodyCases() {
		for _, route := range []string{"/messages", "/stream"} {
			requireRefused(t, doRaw(t, fixture.server, http.MethodPost, "/api/v1/sessions/"+fixture.session+route, token, turnBody(shape.value)),
				"rejected "+route+" "+shape.name)
		}
	}

	if after := countStoredMessages(t, fixture.server, token, fixture.session); after != messagesBefore {
		t.Fatalf("a rejected body persisted messages: %d -> %d", messagesBefore, after)
	}
	after := fixtureSession(t, fixture)
	if after.UpdatedAt != sessionBefore.UpdatedAt || after.Title != sessionBefore.Title || after.Mode != sessionBefore.Mode {
		t.Fatalf("a rejected body touched the conversation: %+v -> %+v", sessionBefore, after)
	}
	if assistant.responded != 0 || assistant.streamed != 0 {
		t.Fatalf("a rejected body reached the assistant: responded=%d streamed=%d", assistant.responded, assistant.streamed)
	}
	if len(fixtureCollections(t, fixture)) != collectionsBefore {
		t.Fatal("a rejected body created a collection")
	}
	if got := len(listMemories(t, fixture, token, "pending")) + len(listMemories(t, fixture, token, "active")); got != memoriesBefore {
		t.Fatalf("a rejected body wrote memory rows: %d -> %d", memoriesBefore, got)
	}

	// The same conversation still answers a well-formed turn afterwards, so a refusal is a
	// refusal and not a wedged session.
	good, err := json.Marshal(map[string]any{"content": mixedScript})
	if err != nil {
		t.Fatal(err)
	}
	requireStatus(t, doRaw(t, fixture.server, http.MethodPost, "/api/v1/sessions/"+fixture.session+"/messages", token, good),
		http.StatusOK, "turn after the rejections")
}

// TestRejectedQuizAnswerKeepsTheSubmissionIdUsable carries the 6C.1 lesson forward: refusing a
// body must not consume the caller's deduplication token or advance the quiz.
func TestRejectedQuizAnswerKeepsTheSubmissionIdUsable(t *testing.T) {
	quiz := newLearningHTTPFixture(t, learningQuestionReply("提交号不被烧掉"), learningGradeReply())
	quizID := startQuizForUTF8Test(t, quiz)
	callsBefore := quiz.chat.calls

	bad := []byte(`{"submission_id":"utf8-reuse-1","ordinal":1,"answer":"` + "\xff\xfe" + `"}`)
	requireRefused(t, doRaw(t, quiz.app.server, http.MethodPost, "/api/v1/learning/sessions/"+quizID+"/answer", quiz.token, bad),
		"quiz answer with broken bytes")
	if quiz.chat.calls != callsBefore {
		t.Fatalf("the grader ran for a body that was never accepted: %d -> %d", callsBefore, quiz.chat.calls)
	}
	requireRefused(t, doRaw(t, quiz.app.server, http.MethodPost, "/api/v1/learning/sessions/"+quizID+"/skip", quiz.token,
		[]byte(`{"submission_id":"`+string([]byte{0xc0, 0x80})+`"}`)), "quiz skip with broken bytes")
	state := resumeQuizForUTF8Test(t, quiz)
	if state.Status != string(domain.LearningSessionWaitingAnswer) || state.CurrentQuestion != 1 {
		t.Fatalf("the rejected answer moved the quiz: %+v", state)
	}

	good, err := json.Marshal(map[string]any{"submission_id": "utf8-reuse-1", "ordinal": 1, "answer": mixedScript})
	if err != nil {
		t.Fatal(err)
	}
	response := doRaw(t, quiz.app.server, http.MethodPost, "/api/v1/learning/sessions/"+quizID+"/answer", quiz.token, good)
	requireStatus(t, response, http.StatusOK, "the same submission id after the refusal")
	if !strings.Contains(response.Body.String(), `"current_question":2`) {
		t.Fatalf("the reused submission did not advance the quiz: %s", response.Body.String())
	}
	// The grader must have seen the exact runes, not a sanitised copy.
	requests := quiz.chat.requests
	last := requests[len(requests)-1]
	found := false
	for _, message := range last.Messages {
		if strings.Contains(message.Content, mixedScript) {
			found = true
		}
	}
	if !found {
		t.Fatal("the grader never saw the submitted text verbatim")
	}
}

func fixtureSession(t *testing.T, fixture *apiFixture) domain.Session {
	t.Helper()
	session, err := fixture.store.GetSession(t.Context(), fixture.owner, fixture.session)
	if err != nil {
		t.Fatal(err)
	}
	return *session
}

func fixtureCollections(t *testing.T, fixture *apiFixture) []domain.Collection {
	t.Helper()
	list, err := fixture.service.ListCollections(t.Context(), fixture.owner)
	if err != nil {
		t.Fatal(err)
	}
	return list
}

// startQuizForUTF8Test opens the seeded learning-agent conversation's one quiz. The topic is
// plain ASCII here so this helper is never the reason a case fails.
func startQuizForUTF8Test(t *testing.T, quiz *learningHTTPFixture) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{"topic": "Redis 持久化"})
	if err != nil {
		t.Fatal(err)
	}
	response := doRaw(t, quiz.app.server, http.MethodPost, "/api/v1/learning/sessions?session_id="+quiz.agentChat, quiz.token, body)
	if response.Code != http.StatusCreated {
		t.Fatalf("quiz start=%d body=%s", response.Code, response.Body.String())
	}
	var envelope struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	return envelope.Data.ID
}

func resumeQuizForUTF8Test(t *testing.T, quiz *learningHTTPFixture) quizShape {
	t.Helper()
	response := doRaw(t, quiz.app.server, http.MethodGet, "/api/v1/learning/sessions?session_id="+quiz.agentChat, quiz.token, nil)
	requireStatus(t, response, http.StatusOK, "quiz resume")
	var envelope struct {
		Data quizShape `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	return envelope.Data
}

type quizShape struct {
	Status          string `json:"status"`
	CurrentQuestion int    `json:"current_question"`
}

func httpapiSource(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(".", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}

// TestLoneSurrogateEscapeScan is the unit half of the gate: it must reject what cannot be text
// and accept what can, including an escaped pair that spells a real emoji.
func TestLoneSurrogateEscapeScan(t *testing.T) {
	for _, testCase := range []struct {
		text string
		want bool
	}{
		{text: `{"content":"\ud800"}`, want: true},
		{text: `{"content":"\udbff"}`, want: true},
		{text: `{"content":"\udc00"}`, want: true},
		{text: `{"content":"\ud83d"}`, want: true},
		{text: `{"content":"\ud83dx"}`, want: true},
		{text: `{"content":"\ud83d "}`, want: true},
		{text: `{"content":"\ud83d\udd2c"}`, want: false},
		{text: `{"content":"\ud83d\ude00 and \ud83c\udf89"}`, want: false},
		{text: `{"content":"plain \u4e2d\u6587"}`, want: false},
		// An escaped backslash then literal text: not an escape at all.
		{text: `{"content":"path\\ud800"}`, want: false},
		{text: `{"content":"𠮷"}`, want: false},
	} {
		if got := hasLoneSurrogateEscape([]byte(testCase.text)); got != testCase.want {
			t.Errorf("hasLoneSurrogateEscape(%q) = %v, want %v", testCase.text, got, testCase.want)
		}
	}
}
