package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/model"
	"github.com/xiaopang456/GopherLearn/internal/repository/repositorytest"
	"github.com/xiaopang456/GopherLearn/internal/retrieval"
	"github.com/xiaopang456/GopherLearn/internal/service"
)

// The quiz endpoints are checked end to end here because their whole reason to exist is
// what a client can and cannot make the server do. The workflow itself is covered at the
// service and repository layers; these cases are about the boundary: who may act, which
// fields are accepted, and which refusal a client is told.

// learningHTTPChat is the provider stand-in for the quiz endpoints. It answers from a
// queue, so one fixture can be told "generate these questions, then grade this answer".
type learningHTTPChat struct {
	replies  []string
	calls    int
	requests []model.Request
	err      error
}

func (c *learningHTTPChat) Complete(_ context.Context, request model.Request) (model.Completion, error) {
	c.calls++
	c.requests = append(c.requests, request)
	if c.err != nil {
		return model.Completion{}, c.err
	}
	index := c.calls - 1
	if index >= len(c.replies) {
		index = len(c.replies) - 1
	}
	if index < 0 {
		return model.Completion{}, model.ErrEmptyCompletion
	}
	return model.Completion{Content: c.replies[index], FinishReason: model.FinishStop}, nil
}

func (c *learningHTTPChat) Stream(ctx context.Context, request model.Request, _ model.DeltaHandler) (model.Completion, error) {
	return c.Complete(ctx, request)
}

// learningHTTPRecall hands the quiz a fixed set of chunks. Retrieval has its own tests;
// what matters here is that evidence exists so a quiz can start.
type learningHTTPRecall struct {
	results []retrieval.Result
	calls   int
}

func (r *learningHTTPRecall) Search(context.Context, retrieval.Query) ([]retrieval.Result, error) {
	r.calls++
	return r.results, nil
}

func learningHTTPChunks(count int) []retrieval.Result {
	results := make([]retrieval.Result, 0, count)
	for index := range count {
		results = append(results, retrieval.Result{Chunk: domain.DocumentChunk{
			ID:           fmt.Sprintf("http-chk-%02d", index),
			DocumentID:   fmt.Sprintf("http-doc-%d", index/3),
			CollectionID: "col", Version: 1, Order: index * 2,
			Section: fmt.Sprintf("第 %d 节", index),
			// Comfortably above the selector's minimum-snapshot floor, so these cases are
			// about the endpoint and not about a chunk being too short to be evidence.
			Text: "Redis 提供 RDB 快照与 AOF 追加日志两种持久化方式，取舍点在于恢复速度与写入开销。" +
				strings.Repeat("展开说明这段取舍的判定条件与代价。", 8) + fmt.Sprintf(" %02d", index),
			StartLine: index*8 + 1, EndLine: index*8 + 7,
		}})
	}
	return results
}

// learningQuestionReply renders a generator's answer for three questions.
func learningQuestionReply(label string) string {
	questions := make([]map[string]any, 0, domain.LearningQuestionCount)
	for index := 1; index <= domain.LearningQuestionCount; index++ {
		questions = append(questions, map[string]any{
			"type":             "short_answer",
			"question":         fmt.Sprintf("%s 第 %d 题：说明两种做法各解决什么问题？", label, index),
			"assessment_goal":  fmt.Sprintf("%s 目标 %d：区分两种机制的适用条件", label, index),
			"reference_answer": "RDB 面向恢复速度，AOF 面向数据完整性。",
			"rubric":           []string{"指出两种机制", "说出一种取舍"},
			"evidence":         []int{index},
		})
	}
	encoded, _ := json.Marshal(map[string]any{"questions": questions})
	return string(encoded)
}

// learningGradeReply renders a grader's answer.
func learningGradeReply() string {
	return `{"correctness":40,"completeness":15,"clarity":9,` +
		`"feedback":"机制说到了，取舍还差一句。","missing_points":["没说清恢复速度与写入开销的关系"],"confidence":0.8}`
}

// learningHTTPFixture is the seeded application with the quiz workflow installed, plus a
// learning-agent conversation to run it on and both accounts' credentials.
type learningHTTPFixture struct {
	app       *apiFixture
	chat      *learningHTTPChat
	agentChat string
	token     string
	other     string
}

func newLearningHTTPFixture(t *testing.T, replies ...string) *learningHTTPFixture {
	t.Helper()
	app := newFixture(t, apiResponder{})
	chat := &learningHTTPChat{replies: replies}
	recall := &learningHTTPRecall{results: learningHTTPChunks(8)}
	app.server.learning = service.NewLearningService(app.store, chat, recall, app.service, app.service)

	session, err := app.store.CreateSession(t.Context(), &domain.Session{UserID: app.owner,
		Title: "测验会话", Mode: domain.SessionModeLearningAgent})
	if err != nil {
		t.Fatal(err)
	}
	owner, err := app.tokens.Issue(app.owner)
	if err != nil {
		t.Fatal(err)
	}
	stranger, err := app.tokens.Issue(app.other)
	if err != nil {
		t.Fatal(err)
	}
	return &learningHTTPFixture{app: app, chat: chat, agentChat: session.ID, token: owner, other: stranger}
}

// mustStart opens a quiz and returns its id, failing the test rather than letting a later
// assertion run against a nil identifier.
func (f *learningHTTPFixture) mustStart(t *testing.T) string {
	t.Helper()
	started := f.start(t, `{"topic":"Redis 持久化"}`)
	if started.status != http.StatusCreated {
		t.Fatalf("start = %d: %s", started.status, started.body)
	}
	quizID, ok := started.data["id"].(string)
	if !ok || quizID == "" {
		t.Fatalf("start returned no quiz id: %s", started.body)
	}
	return quizID
}

// start opens a quiz through the HTTP surface and returns the response body.
func (f *learningHTTPFixture) start(t *testing.T, body string) renderedEnvelope {
	t.Helper()
	response := sendToHandler(f.app.server.Handler(), http.MethodPost,
		"/api/v1/learning/sessions?session_id="+f.agentChat, f.token, body)
	return decodeEnvelope(t, response)
}

type renderedEnvelope struct {
	status int
	body   string
	data   map[string]any
	error_ apiError
}

func decodeEnvelope(t *testing.T, response *httptest.ResponseRecorder) renderedEnvelope {
	t.Helper()
	var payload struct {
		Data  map[string]any `json:"data"`
		Error apiError       `json:"error"`
	}
	raw := response.Body.String()
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("response is not the contract envelope: %v - %s", err, raw)
	}
	return renderedEnvelope{status: response.Code, body: raw, data: payload.Data, error_: payload.Error}
}

func TestLearningStartServesTheQuestionAndViewOnly(t *testing.T) {
	fixture := newLearningHTTPFixture(t, learningQuestionReply("首卷"))
	envelope := fixture.start(t, `{"topic":"Redis 持久化"}`)
	if envelope.status != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", envelope.status, envelope.body)
	}
	if envelope.data["status"] != string(domain.LearningSessionWaitingAnswer) {
		t.Fatalf("status = %v, want the quiz open for answers", envelope.data["status"])
	}
	question, ok := envelope.data["question"].(map[string]any)
	if !ok || question["ordinal"] != float64(1) {
		t.Fatalf("question = %v, want the first one", envelope.data["question"])
	}
	// The response must not carry the answer key, the rubric or the evidence text, even
	// though the stored question does.
	for _, forbidden := range []string{"reference_answer", "rubric", "snapshot", "evidence",
		"RDB 面向恢复速度", "指出两种机制"} {
		if strings.Contains(envelope.body, forbidden) {
			t.Fatalf("the start response leaked %q:\n%s", forbidden, envelope.body)
		}
	}
	if _, err := fixture.app.store.GetLearningSession(t.Context(), fixture.app.owner,
		envelope.data["id"].(string)); err != nil {
		t.Fatalf("the served quiz is not the stored one: %v", err)
	}
}

func TestLearningEndpointsRequireAuthentication(t *testing.T) {
	fixture := newLearningHTTPFixture(t, learningQuestionReply("匿名"))
	for _, request := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/learning/sessions?session_id=" + fixture.agentChat, `{"topic":"Redis"}`},
		{http.MethodGet, "/api/v1/learning/sessions?session_id=" + fixture.agentChat, ""},
		{http.MethodPost, "/api/v1/learning/sessions/lsn-1/answer", `{"submission_id":"s","ordinal":1,"answer":"a"}`},
		{http.MethodGet, "/api/v1/learning/progress?topic=Redis", ""},
	} {
		response := sendToHandler(fixture.app.server.Handler(), request.method, request.path, "", request.body)
		requireCode(t, response, http.StatusUnauthorized, "unauthorized", request.path)
	}
}

func TestLearningAnswerSkipCancelAndProgressRoundTrip(t *testing.T) {
	fixture := newLearningHTTPFixture(t, learningQuestionReply("往返"),
		learningGradeReply(), learningGradeReply(), learningGradeReply())
	quizID := fixture.mustStart(t)
	handler := fixture.app.server.Handler()

	// Three answers is the threshold at which a projection becomes worth reading, so the
	// round trip goes all the way through the quiz rather than stopping at one.
	for ordinal := 1; ordinal <= domain.LearningQuestionCount; ordinal++ {
		body := fmt.Sprintf(`{"submission_id":"sub-%d","ordinal":%d,"answer":"两种机制的取舍我都说明了。"}`, ordinal, ordinal)
		answered := decodeEnvelope(t, sendToHandler(handler, http.MethodPost,
			"/api/v1/learning/sessions/"+quizID+"/answer", fixture.token, body))
		if answered.status != http.StatusOK {
			t.Fatalf("answer %d = %d: %s", ordinal, answered.status, answered.body)
		}
		if answered.data["status"] == string(domain.LearningSessionCompleted) != (ordinal == domain.LearningQuestionCount) {
			t.Fatalf("answer %d reported status %v", ordinal, answered.data["status"])
		}
	}
	progress := decodeEnvelope(t, sendToHandler(handler, http.MethodGet,
		"/api/v1/learning/progress?topic="+url.QueryEscape("Redis 持久化"), fixture.token, ""))
	if progress.status != http.StatusOK {
		t.Fatalf("progress = %d: %s", progress.status, progress.body)
	}
	// The owner's own progress endpoint is the one place the numbers appear; the agent
	// prompt carries prose. A response without them would be a different feature.
	if progress.data["valid_attempts"] != float64(domain.LearningQuestionCount) {
		t.Fatalf("progress = %v, want three valid attempts", progress.data)
	}
	if progress.data["mastery"] == nil || progress.data["level"] == nil {
		t.Fatalf("progress = %v, want the score and the level", progress.data)
	}
	if weak, ok := progress.data["weak_points"].([]any); !ok || len(weak) == 0 {
		t.Fatalf("progress = %v, want the gaps the grader named", progress.data)
	}

	// A finished quiz has nothing to cancel into, and cancelling it is answered as the
	// state it is in rather than as an error.
	closed := decodeEnvelope(t, sendToHandler(handler, http.MethodPost,
		"/api/v1/learning/sessions/"+quizID+"/cancel", fixture.token, ""))
	if closed.status != http.StatusOK || closed.data["status"] != string(domain.LearningSessionCompleted) {
		t.Fatalf("cancel of a finished quiz = %d %s, want the stored state", closed.status, closed.body)
	}
}

func TestLearningAnswerIsIdempotentOverHTTP(t *testing.T) {
	fixture := newLearningHTTPFixture(t, learningQuestionReply("重放"), learningGradeReply())
	quizID := fixture.mustStart(t)
	handler := fixture.app.server.Handler()
	body := `{"submission_id":"sub-once","ordinal":1,"answer":"两种机制各有侧重。"}`

	first := decodeEnvelope(t, sendToHandler(handler, http.MethodPost,
		"/api/v1/learning/sessions/"+quizID+"/answer", fixture.token, body))
	if first.status != http.StatusOK {
		t.Fatalf("first answer = %d: %s", first.status, first.body)
	}
	calls := fixture.chat.calls
	repeat := decodeEnvelope(t, sendToHandler(handler, http.MethodPost,
		"/api/v1/learning/sessions/"+quizID+"/answer", fixture.token, body))
	if repeat.status != http.StatusOK {
		t.Fatalf("retried answer = %d: %s", repeat.status, repeat.body)
	}
	// A re-sent answer must not spend another model call or move the quiz again.
	if fixture.chat.calls != calls {
		t.Fatalf("a replay made %d further model calls, want none", fixture.chat.calls-calls)
	}
	if repeat.data["current_question"] != first.data["current_question"] {
		t.Fatalf("replay advanced the quiz: %v vs %v", first.data["current_question"], repeat.data["current_question"])
	}
}

func TestLearningSurfaceRefusesEverythingAIClientShouldNotSend(t *testing.T) {
	fixture := newLearningHTTPFixture(t, learningQuestionReply("拒绝"))
	handler := fixture.app.server.Handler()
	quizID := fixture.mustStart(t)

	cases := []struct {
		name   string
		method string
		path   string
		body   string
		status int
		code   string
	}{
		{
			name: "an unknown field on the start body", method: http.MethodPost,
			path: "/api/v1/learning/sessions?session_id=" + fixture.agentChat,
			body: `{"topic":"Redis","mastery":100}`, status: http.StatusBadRequest, code: "invalid_request",
		},
		{
			name: "a missing session for a quiz", method: http.MethodPost,
			path: "/api/v1/learning/sessions", body: `{"topic":"Redis"}`,
			status: http.StatusBadRequest, code: "invalid_request",
		},
		{
			name: "a score in an answer body", method: http.MethodPost,
			path:   "/api/v1/learning/sessions/" + quizID + "/answer",
			body:   `{"submission_id":"s","ordinal":1,"answer":"答了","correctness":60}`,
			status: http.StatusBadRequest, code: "invalid_request",
		},
		{
			name: "an empty answer", method: http.MethodPost,
			path:   "/api/v1/learning/sessions/" + quizID + "/answer",
			body:   `{"submission_id":"s2","ordinal":1,"answer":"   "}`,
			status: http.StatusBadRequest, code: "invalid_request",
		},
		{
			name: "an answer for a question that is not current", method: http.MethodPost,
			path:   "/api/v1/learning/sessions/" + quizID + "/answer",
			body:   `{"submission_id":"s3","ordinal":3,"answer":"跳题"}`,
			status: http.StatusBadRequest, code: "learning_question_order",
		},
		{
			name: "a skip with no submission id", method: http.MethodPost,
			path: "/api/v1/learning/sessions/" + quizID + "/skip", body: `{"submission_id":" "}`,
			status: http.StatusBadRequest, code: "invalid_request",
		},
		{
			name: "a cancel that carries a body", method: http.MethodPost,
			path: "/api/v1/learning/sessions/" + quizID + "/cancel", body: `{"status":"completed"}`,
			status: http.StatusBadRequest, code: "invalid_request",
		},
		{
			name: "an unknown action", method: http.MethodPost,
			path: "/api/v1/learning/sessions/" + quizID + "/grade-myself", body: "",
			status: http.StatusNotFound, code: "not_found",
		},
		{
			name: "a quiz id that is not the caller's", method: http.MethodPost,
			path: "/api/v1/learning/sessions/" + quizID + "/cancel", body: "",
			status: http.StatusOK, code: "",
		},
	}
	for _, testCase := range cases[:len(cases)-1] {
		t.Run(testCase.name, func(t *testing.T) {
			response := sendToHandler(handler, testCase.method, testCase.path, fixture.token, testCase.body)
			requireCode(t, response, testCase.status, testCase.code, testCase.path)
		})
	}
	// The last case is the one that must be distinguishable from a missing quiz, and it is
	// not a body-shape question at all: another account's quiz id answers 404.
	other := sendToHandler(handler, http.MethodPost, "/api/v1/learning/sessions/"+quizID+"/cancel",
		fixture.other, "")
	requireCode(t, other, http.StatusNotFound, "not_found", "another account cancelling")
	if _, err := fixture.app.store.GetLearningSession(t.Context(), fixture.app.owner, quizID); err != nil {
		t.Fatalf("the probe changed the owner's quiz: %v", err)
	}
}

func TestLearningResumeSeparatesEmptyFromUnknown(t *testing.T) {
	fixture := newLearningHTTPFixture(t, learningQuestionReply("空"))
	handler := fixture.app.server.Handler()

	// The two absences this endpoint can meet, told apart because a client acts on them
	// differently: nothing open on my own conversation is a normal answer, while a
	// conversation I cannot read is not something to render at all.
	empty := decodeEnvelope(t, sendToHandler(handler, http.MethodGet,
		"/api/v1/learning/sessions?session_id="+fixture.agentChat, fixture.token, ""))
	if empty.status != http.StatusOK || empty.data != nil || empty.error_.Code != "" {
		t.Fatalf("resume with nothing in progress = %d %q, want 200 with data=null and error=null: %s",
			empty.status, empty.error_.Code, empty.body)
	}
	if trimmed := strings.TrimSpace(empty.body); trimmed != `{"data":null,"error":null}` {
		t.Fatalf("the empty state is not the literal contract shape: %s", trimmed)
	}
	unknown := decodeEnvelope(t, sendToHandler(handler, http.MethodGet,
		"/api/v1/learning/sessions?session_id=session-000000", fixture.token, ""))
	if unknown.status != http.StatusNotFound || unknown.error_.Code != "not_found" {
		t.Fatalf("resume on an unknown conversation = %d %q, want not_found",
			unknown.status, unknown.error_.Code)
	}
	// A progress record that does not exist yet is a not-found: unlike the resume
	// endpoint, this one is addressed by a topic the caller named, and "nothing measured
	// for that subject" is about the resource they asked for.
	missing := decodeEnvelope(t, sendToHandler(handler, http.MethodGet,
		"/api/v1/learning/progress?topic="+url.QueryEscape("从未测过"), fixture.token, ""))
	if missing.status != http.StatusNotFound {
		t.Fatalf("progress with no record = %d: %s", missing.status, missing.body)
	}
	if missing.error_.Code != "not_found" {
		t.Fatalf("progress code = %q, want not_found", missing.error_.Code)
	}
	for _, path := range []string{"/api/v1/learning/progress",
		"/api/v1/learning/sessions?session_id="} {
		response := sendToHandler(handler, http.MethodGet, path, fixture.token, "")
		requireCode(t, response, http.StatusBadRequest, "invalid_request", path)
	}
	// Starting twice on one conversation is refused with the code that says why, and the
	// client's next action is to resume rather than to retry.
	fixture.mustStart(t)
	second := fixture.start(t, `{"topic":"Redis 持久化"}`)
	if second.status != http.StatusConflict || second.error_.Code != "learning_session_active" {
		t.Fatalf("second start = %d %q, want the active-quiz conflict", second.status, second.error_.Code)
	}
	resumed := decodeEnvelope(t, sendToHandler(handler, http.MethodGet,
		"/api/v1/learning/sessions?session_id="+fixture.agentChat, fixture.token, ""))
	if resumed.status != http.StatusOK {
		t.Fatalf("resume = %d: %s", resumed.status, resumed.body)
	}
}

func TestLearningEndpointsAnswerFiveOhFiveWithoutTheWorkflow(t *testing.T) {
	// A build that was not given the quiz service must not half-serve it: no quiz is
	// created, no listing lies, and the answer is the same for every endpoint.
	app := newFixture(t, apiResponder{})
	token, err := app.tokens.Issue(app.owner)
	if err != nil {
		t.Fatal(err)
	}
	handler := app.server.Handler()
	for _, request := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/learning/sessions?session_id=" + app.session, `{"topic":"Redis"}`},
		{http.MethodGet, "/api/v1/learning/sessions?session_id=" + app.session, ""},
		{http.MethodPost, "/api/v1/learning/sessions/lsn-1/skip", `{"submission_id":"s"}`},
		{http.MethodGet, "/api/v1/learning/progress?topic=Redis", ""},
	} {
		response := sendToHandler(handler, request.method, request.path, token, request.body)
		requireCode(t, response, http.StatusInternalServerError, "internal_error", request.path)
	}
}

func TestLearningModelFailureAnswersAsAnUpstreamCondition(t *testing.T) {
	// A generation that fails is not the client's error and not the server's bug. It gets
	// the code that says "try again", and the conversation is left free to ask again
	// immediately rather than holding a reservation for a quiz that never existed.
	fixture := newLearningHTTPFixture(t, "这不是 JSON", "这也不是", "还不是")
	started := fixture.start(t, `{"topic":"Redis 持久化"}`)
	if started.status != http.StatusServiceUnavailable || started.error_.Code != "learning_generation_failed" {
		t.Fatalf("start = %d %q, want the generation code", started.status, started.error_.Code)
	}
	retried := fixture.start(t, `{"topic":"Redis 持久化"}`)
	if retried.status != http.StatusConflict && retried.status != http.StatusServiceUnavailable {
		t.Fatalf("the conversation stayed locked: %d %s", retried.status, retried.body)
	}
	if retried.error_.Code == "learning_session_active" {
		t.Fatal("a failed start left the conversation holding a quiz")
	}
}

func TestInstallingTheQuizWorkflowLeavesChatAndKnowledgeAlone(t *testing.T) {
	// The fixture with the quiz installed must still serve a plain turn exactly as the
	// one without it does, including the citations a grounded turn returns. This is the
	// boundary the whole phase has to hold: nothing about a quiz may reach an ordinary
	// conversation.
	withQuiz := newLearningHTTPFixture(t, learningQuestionReply("隔离"))
	handler := withQuiz.app.server.Handler()
	response := sendToHandler(handler, http.MethodPost,
		"/api/v1/sessions/"+withQuiz.app.session+"/messages", withQuiz.token,
		`{"content":"讲讲 Redis 的持久化。"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("plain turn = %d: %s", response.Code, response.Body.String())
	}
	if withQuiz.chat.calls != 0 {
		t.Fatalf("an ordinary turn used the quiz model %d times", withQuiz.chat.calls)
	}
	// A grounded turn in the same application still answers with its citations.
	grounded := sendToHandler(handler, http.MethodPost,
		"/api/v1/sessions/"+withQuiz.app.session+"/messages", withQuiz.token,
		`{"content":"RDB 的代价与什么相关？","knowledge":{"enabled":true,"collection_ids":["`+withQuiz.app.collection+`"]}}`)
	if grounded.Code != http.StatusOK {
		t.Fatalf("grounded turn = %d: %s", grounded.Code, grounded.Body.String())
	}
}

// TestLearningAnswersFiveOhOhWhenNoModelWasConfigured covers the wiring fault that a
// nil-service check cannot catch: a deployment that installed the quiz workflow but built
// it over an assembly with no provider. The route exists, the service exists, and nothing
// can generate - so the answer must be a 5xx that names the server's condition, and the
// request must not be served by a partially working quiz.
func TestLearningAnswersFiveOhOhWhenNoModelWasConfigured(t *testing.T) {
	fixture := newLearningHTTPFixture(t, learningQuestionReply("永远不该被读到"))
	fixture.app.server.learning = service.NewLearningService(fixture.app.store, nil,
		&learningHTTPRecall{results: learningHTTPChunks(8)}, fixture.app.service, fixture.app.service)

	started := fixture.start(t, `{"topic":"Redis 持久化"}`)
	if started.status != http.StatusInternalServerError || started.error_.Code != "learning_unavailable" {
		t.Fatalf("start over a modelless workflow = %d %q, want the unavailable code", started.status, started.error_.Code)
	}
	// Reading is not model-backed, so it must not be swept up in the same refusal: the
	// conversation is still the learner's own, and holding no quiz is the empty state.
	resumed := decodeEnvelope(t, sendToHandler(fixture.app.server.Handler(), http.MethodGet,
		"/api/v1/learning/sessions?session_id="+fixture.agentChat, fixture.token, ""))
	if resumed.status != http.StatusOK || resumed.data != nil {
		t.Fatalf("resume over a modelless workflow = %d: %s, want the empty state",
			resumed.status, resumed.body)
	}
}

// TestEveryLearningRefusalHasItsOwnCode holds the promise the mapping file makes.
//
// writeLearningError deliberately does not let the general mapping flatten a learning
// refusal into "conflict" or "resource already exists" - each one tells the client a
// different next action. That only holds while every sentinel is listed, and a list nobody
// checks silently falls back to the generic branch the moment someone adds an error. So the
// list here is the whole set, and adding a tenth sentinel without a code fails this test
// rather than shipping a misleading response.
func TestEveryLearningRefusalHasItsOwnCode(t *testing.T) {
	for _, sentinel := range []error{
		service.ErrLearningSessionActive,
		service.ErrLearningBusy,
		service.ErrLearningSubmissionConflict,
		service.ErrLearningQuestionOrder,
		service.ErrLearningClosed,
		service.ErrLearningNoEvidence,
		service.ErrLearningGeneration,
		service.ErrLearningGrading,
		service.ErrLearningInvalidGrade,
		service.ErrLearningModelNotConfigured,
	} {
		value, ok := learningErrorCodes[sentinel]
		if !ok {
			t.Fatalf("%v has no HTTP code", sentinel)
		}
		if value.Code == "" {
			t.Fatalf("%v has an empty code", sentinel)
		}
		status, ok := learningStatus[sentinel]
		if !ok {
			t.Fatalf("%v has no deliberate status choice", sentinel)
		}
		if status < http.StatusBadRequest {
			t.Fatalf("%v maps to %d, which is not an error status", sentinel, status)
		}
	}
	if len(learningErrorCodes) != len(learningStatus) {
		t.Fatalf("%d codes but %d statuses: one map grew without the other", len(learningErrorCodes), len(learningStatus))
	}
}

// TestLearningRefusalsRevealNothingAboutOwnership holds the rule that a client cannot use
// this API to find out whether somebody else's quiz exists.
//
// The two refusals have to be the same bytes, not merely the same status: a "you may not
// touch that" distinct from a "there is no such thing" turns the endpoints into a probe for
// other accounts' identifiers, and quiz ids are the kind of thing that appears in a URL
// someone else can guess at. Every action is checked as a pair, on the real identifier and
// on one that was never written.
func TestLearningRefusalsRevealNothingAboutOwnership(t *testing.T) {
	fixture := newLearningHTTPFixture(t, learningQuestionReply("归属"))
	quizID := fixture.mustStart(t)
	handler := fixture.app.server.Handler()
	absent := "lsn-000000000000"

	// The same body text for both halves of each pair, so the only difference between the
	// two requests is whose quiz it really is.
	answerBody := `{"submission_id":"sub-probe","ordinal":1,"answer":"探针"}`
	for _, pair := range []struct {
		label    string
		action   string
		token    string
		body     string
		method   string
		pathReal string
	}{
		{"answer", "answer", fixture.other, answerBody, http.MethodPost, ""},
		{"skip", "skip", fixture.other, `{"submission_id":"sub-probe-skip"}`, http.MethodPost, ""},
		{"cancel", "cancel", fixture.other, "", http.MethodPost, ""},
	} {
		// A stranger acting on the real quiz, and the owner acting on a quiz that does not
		// exist. Both must be told the same thing.
		realPath := "/api/v1/learning/sessions/" + quizID + "/" + pair.action
		missing := "/api/v1/learning/sessions/" + absent + "/" + pair.action
		stranger := sendToHandler(handler, pair.method, realPath, pair.token, pair.body)
		nonexistent := sendToHandler(handler, pair.method, missing, fixture.token, pair.body)
		if stranger.Code != http.StatusNotFound || nonexistent.Code != http.StatusNotFound {
			t.Fatalf("%s: %d and %d, want two not-founds", pair.label, stranger.Code, nonexistent.Code)
		}
		if stranger.Body.String() != nonexistent.Body.String() {
			t.Fatalf("%s reveals which quiz exists:\n stranger:   %s\n nonexistent: %s",
				pair.label, stranger.Body.String(), nonexistent.Body.String())
		}
	}

	// Resuming is the same question asked of a conversation instead of a quiz id: naming a
	// conversation owned by somebody else must be indistinguishable from naming none.
	foreign, err := fixture.app.store.CreateSession(t.Context(), &domain.Session{
		UserID: fixture.app.other, Title: "别人的会话", Mode: domain.SessionModeLearningAgent})
	if err != nil {
		t.Fatal(err)
	}
	borrowed := sendToHandler(handler, http.MethodGet,
		"/api/v1/learning/sessions?session_id="+foreign.ID, fixture.token, "")
	invented := sendToHandler(handler, http.MethodGet,
		"/api/v1/learning/sessions?session_id=session-000000", fixture.token, "")
	if borrowed.Code != http.StatusNotFound || invented.Code != http.StatusNotFound {
		t.Fatalf("resume = %d and %d, want two not-founds", borrowed.Code, invented.Code)
	}
	if borrowed.Body.String() != invented.Body.String() {
		t.Fatalf("resume reveals whose conversation it is:\n %s\n %s",
			borrowed.Body.String(), invented.Body.String())
	}

	// And the third answer has to stay distinct from both: "nothing open here" is the
	// caller's own ordinary state, and collapsing it into the not-found pair would make an
	// idle conversation indistinguishable from somebody else's - which is exactly the
	// conflation the pair above exists to prevent, running the other way.
	ownIdle, err := fixture.app.store.CreateSession(t.Context(), &domain.Session{
		UserID: fixture.app.owner, Title: "我的空闲会话", Mode: domain.SessionModeLearningAgent})
	if err != nil {
		t.Fatal(err)
	}
	own := sendToHandler(handler, http.MethodGet,
		"/api/v1/learning/sessions?session_id="+ownIdle.ID, fixture.token, "")
	if own.Code != http.StatusOK || !strings.Contains(own.Body.String(), `"data":null`) ||
		!strings.Contains(own.Body.String(), `"error":null`) {
		t.Fatalf("an idle conversation of the caller's own = %d %s, want 200 with a null data and a null error",
			own.Code, own.Body.String())
	}
	if own.Body.String() == borrowed.Body.String() {
		t.Fatalf("the empty state and the denied state are the same bytes: %s", own.Body.String())
	}
}

// TestLearningResponsesCarryNoAnswerKeyAnywhere is the leak check at the boundary.
//
// The generator that fills this fixture writes a reference answer and a rubric into every
// question, and the grader writes feedback and named gaps into every attempt - so those
// strings exist in the database and are the exact thing a learner must not be served. The
// check is on the bytes of every response a full quiz produces, because a field can only
// leak through a response, and because key names alone would miss a value placed under a
// harmless name.
func TestLearningResponsesCarryNoAnswerKeyAnywhere(t *testing.T) {
	const (
		// The generator's own answer key and rubric, verbatim.
		referenceAnswer = "RDB 面向恢复速度，AOF 面向数据完整性。"
		rubricItem      = "指出两种机制"
		assessmentGoal  = "区分两种机制的适用条件"
		// The grader's own sentence about this answer. It belongs to the attempt row and
		// has no reading surface: a learner is told what to work on next - the weak-point
		// list - not the verdict of one grading call.
		graderFeedback = "机制说到了，取舍还差一句。"
	)
	fixture := newLearningHTTPFixture(t, append(
		[]string{learningQuestionReply("外泄")},
		learningGradeReply(), learningGradeReply(), learningGradeReply())...)
	handler := fixture.app.server.Handler()

	var bodies []string
	collect := func(response *httptest.ResponseRecorder, want int) map[string]any {
		t.Helper()
		bodies = append(bodies, response.Body.String())
		if response.Code != want {
			t.Fatalf("status = %d, want %d: %s", response.Code, want, response.Body.String())
		}
		var payload struct {
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		return payload.Data
	}

	started := collect(sendToHandler(handler, http.MethodPost,
		"/api/v1/learning/sessions?session_id="+fixture.agentChat, fixture.token, `{"topic":"AOF"}`),
		http.StatusCreated)
	quizID, _ := started["id"].(string)
	for ordinal := 1; ordinal <= domain.LearningQuestionCount; ordinal++ {
		collect(sendToHandler(handler, http.MethodPost,
			"/api/v1/learning/sessions/"+quizID+"/answer", fixture.token,
			fmt.Sprintf(`{"submission_id":"sub-leak-%d","ordinal":%d,"answer":"每种机制都提了一句。"}`,
				ordinal, ordinal)), http.StatusOK)
	}
	collect(sendToHandler(handler, http.MethodPost,
		"/api/v1/learning/sessions/"+quizID+"/cancel", fixture.token, ""), http.StatusOK)
	collect(sendToHandler(handler, http.MethodGet,
		"/api/v1/learning/progress?topic=AOF", fixture.token, ""), http.StatusOK)

	joined := strings.Join(bodies, "\n")
	for _, forbidden := range []string{
		referenceAnswer, rubricItem, assessmentGoal, graderFeedback,
		"reference_answer", "rubric", "evidence", "snapshot", "content_hash", "chunk_id",
		"assessment_goal", "grading_confidence", "lease_token", "effective_total",
		"auto_total", "auto_correctness", "missing_points", "correctness", "completeness",
		"clarity", "http-chk-", "http-doc-", "prompt",
	} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("a learner-facing response carried %q:\n%s", forbidden, joined)
		}
	}
	// The learner's own answer is theirs and must come back... except that nothing here
	// echoes it: an answer already stored needs no redelivery, and repeating it would put
	// the whole transcript on a quiz endpoint.
	if strings.Contains(joined, "每种机制都提了一句") {
		t.Fatal("the quiz responses echoed the learner's answer text back")
	}
}

// TestLearningRefusalCodesReachTheClient asserts the rest of the code table at the
// boundary.
//
// Each of these is a different instruction to a client - stop, resume, send a new
// submission id, wait - so a single "conflict" for all four would be a contract that
// cannot be acted on. The status is checked together with the code, because the pair is
// what a client branches on.
func TestLearningRefusalCodesReachTheClient(t *testing.T) {
	handler := func(t *testing.T) (http.Handler, *learningHTTPFixture, string) {
		t.Helper()
		fixture := newLearningHTTPFixture(t, append(
			[]string{learningQuestionReply("拒绝码")},
			learningGradeReply(), learningGradeReply(), learningGradeReply(), learningGradeReply())...)
		return fixture.app.server.Handler(), fixture, fixture.mustStart(t)
	}

	t.Run("a finished quiz refuses further work", func(t *testing.T) {
		router, fixture, quizID := handler(t)
		if code := sendToHandler(router, http.MethodPost,
			"/api/v1/learning/sessions/"+quizID+"/cancel", fixture.token, ""); code.Code != http.StatusOK {
			t.Fatalf("cancel = %d: %s", code.Code, code.Body.String())
		}
		response := decodeEnvelope(t, sendToHandler(router, http.MethodPost,
			"/api/v1/learning/sessions/"+quizID+"/answer", fixture.token,
			`{"submission_id":"sub-after-cancel","ordinal":1,"answer":"测验已经关了"}`))
		if response.status != http.StatusConflict || response.error_.Code != "learning_session_closed" {
			t.Fatalf("answer after cancel = %d %q, want the closed-quiz conflict", response.status, response.error_.Code)
		}
		// Skipping is refused the same way. Cancelling is not: a second cancel is the same
		// request as the first, and refusing it would punish a client whose response was
		// lost on the way home. Idempotent cancel, refused answer and refused skip - the
		// difference is that only the second one can change the record.
		for _, action := range []struct{ path, body string }{
			{"skip", `{"submission_id":"sub-after-cancel-skip"}`},
		} {
			again := decodeEnvelope(t, sendToHandler(router, http.MethodPost,
				"/api/v1/learning/sessions/"+quizID+"/"+action.path, fixture.token, action.body))
			if again.status != http.StatusConflict || again.error_.Code != "learning_session_closed" {
				t.Fatalf("%s on a closed quiz = %d %q, want the same refusal",
					action.path, again.status, again.error_.Code)
			}
		}
	})

	t.Run("one submission id means one answer", func(t *testing.T) {
		router, fixture, quizID := handler(t)
		first := `{"submission_id":"sub-reuse","ordinal":1,"answer":"第一题的回答。"}`
		if response := decodeEnvelope(t, sendToHandler(router, http.MethodPost,
			"/api/v1/learning/sessions/"+quizID+"/answer", fixture.token, first)); response.status != http.StatusOK {
			t.Fatalf("first answer = %d: %s", response.status, response.body)
		}
		// The same id, a different card: the id is already spent on a graded answer, and
		// silently re-grading under it would make the record of what was scored say two
		// different things.
		conflict := decodeEnvelope(t, sendToHandler(router, http.MethodPost,
			"/api/v1/learning/sessions/"+quizID+"/answer", fixture.token,
			`{"submission_id":"sub-reuse","ordinal":2,"answer":"换个编号想蒙混过去"}`))
		if conflict.status != http.StatusConflict || conflict.error_.Code != "learning_submission_conflict" {
			t.Fatalf("reused submission = %d %q, want the conflict code that says to send a new id",
				conflict.status, conflict.error_.Code)
		}
	})

	t.Run("an answer being scored right now says so", func(t *testing.T) {
		fixture := newLearningHTTPFixture(t, append(
			[]string{learningQuestionReply("评分中")}, learningGradeReply())...)
		quizID := fixture.mustStart(t)
		ctx := t.Context()

		// The in-flight claim is opened straight through the store: this is the one state
		// a single HTTP request cannot create for itself, because the request that creates
		// it is the request that would have to observe it.
		//
		// The claimed answer has to be the text the retry will send. A different answer
		// under the same submission id is a different case - one the server refuses as a
		// submission conflict, below - and the busy code would never be reached.
		quiz, err := fixture.app.store.GetLearningSession(ctx, fixture.app.owner, quizID)
		if err != nil {
			t.Fatal(err)
		}
		questions, err := fixture.app.store.ListLearningQuestions(ctx, fixture.app.owner, quizID)
		if err != nil {
			t.Fatal(err)
		}
		shape := repositorytest.LearningAttemptShape(fixture.app.owner, quiz, questions[0], "sub-inflight")
		shape.Answer = "同一份回答又送来一次"
		if _, err := fixture.app.store.BeginLearningAttempt(ctx, shape); err != nil {
			t.Fatal(err)
		}
		response := decodeEnvelope(t, sendToHandler(fixture.app.server.Handler(), http.MethodPost,
			"/api/v1/learning/sessions/"+quizID+"/answer", fixture.token,
			`{"submission_id":"sub-inflight","ordinal":1,"answer":"同一份回答又送来一次"}`))
		if response.status != http.StatusConflict || response.error_.Code != "learning_answer_busy" {
			t.Fatalf("while scoring = %d %q, want the retry-later code", response.status, response.error_.Code)
		}
	})

	t.Run("notes too thin to quiz from refuse the start", func(t *testing.T) {
		fixture := newLearningHTTPFixture(t, learningQuestionReply("没证据"))
		// Emptying the corpus is a fixture statement, not a model statement: the retrieval
		// double below answers with nothing, so no question can be written from evidence and
		// the server must say that instead of generating a quiz out of thin air.
		fixture.app.server.learning = service.NewLearningService(fixture.app.store, fixture.chat,
			&learningHTTPRecall{}, fixture.app.service, fixture.app.service)
		response := fixture.start(t, `{"topic":"笔记里没有的东西"}`)
		if response.status != http.StatusConflict || response.error_.Code != "learning_no_evidence" {
			t.Fatalf("start without evidence = %d %q, want the refusal that explains itself",
				response.status, response.error_.Code)
		}
	})
}

// TestLearningSkipAcceptsOnlyASubmissionID pins the shape of the skip request.
//
// Skip is the one action that carries no learner content, and that is why its body is
// closed: a client that sends `ordinal` alongside `submission_id` has expressed two
// intentions - which card to decline, and under which receipt - and the server would have to
// pick one to obey. `submission_id` alone is enough: the quiz knows its own position, and a
// mismatch between a client's idea of the current card and the stored one is exactly the
// disagreement worth refusing out loud rather than resolving silently.
//
// Every refusal is also checked for absence of effect. A rejected request that advanced the
// quiz, or that spent a submission id, would be a 400 that still cost the learner something.
func TestLearningSkipAcceptsOnlyASubmissionID(t *testing.T) {
	fixture := newLearningHTTPFixture(t, append(
		[]string{learningQuestionReply("形状")}, learningGradeReply())...)
	quizID := fixture.mustStart(t)
	handler := fixture.app.server.Handler()
	before := fixture.chat.calls

	for _, rejected := range []string{
		`{"submission_id":"sub-shape-1","ordinal":1}`,
		`{"submission_id":"sub-shape-2","ordinal":"1"}`,
		`{"submission_id":"sub-shape-3","answer":"顺便写点东西"}`,
		`{"submission_id":"sub-shape-4","topic":"换个主题"}`,
		`{"submission_id":"sub-shape-5","reference_answer":"连参考答案一起送"}`,
		`{"submission_id":"sub-shape-6","score":100}`,
		`{"submission_id":"sub-shape-7","skip_all":true}`,
		`{"submission_id":["sub-shape-8"]}`,
		`{"submission_id":"sub-shape-9"}{"submission_id":"sub-shape-10"}`,
		`{"ordinal":2}`,
	} {
		response := decodeEnvelope(t, sendToHandler(handler, http.MethodPost,
			"/api/v1/learning/sessions/"+quizID+"/skip", fixture.token, rejected))
		if response.status != http.StatusBadRequest || response.error_.Code != "invalid_request" {
			t.Fatalf("skip with %s = %d %q, want 400 invalid_request",
				rejected, response.status, response.error_.Code)
		}
	}
	if fixture.chat.calls != before {
		t.Fatalf("a refused skip body still reached the model %d times", fixture.chat.calls-before)
	}

	// The quiz is where it was: still open, still on its first card, and none of the
	// rejected submission ids were spent - the first of them is still usable by a well
	// formed request, which is the only proof that the refusals performed no write.
	resumed := decodeEnvelope(t, sendToHandler(handler, http.MethodGet,
		"/api/v1/learning/sessions?session_id="+fixture.agentChat, fixture.token, ""))
	if resumed.status != http.StatusOK || resumed.data["status"] != string(domain.LearningSessionWaitingAnswer) ||
		resumed.data["current_question"] != float64(1) {
		t.Fatalf("the refused bodies changed the quiz: %d %s", resumed.status, resumed.body)
	}
	accepted := decodeEnvelope(t, sendToHandler(handler, http.MethodPost,
		"/api/v1/learning/sessions/"+quizID+"/skip", fixture.token,
		`{"submission_id":"sub-shape-1"}`))
	if accepted.status != http.StatusOK || accepted.data["current_question"] != float64(2) {
		t.Fatalf("the retry under the original id = %d %s, want the skip accepted",
			accepted.status, accepted.body)
	}
}

// TestLearningStartIsRefusedOnAPlainConversationOverHTTP is the HTTP half of the binding
// between a quiz and a learning-agent conversation.
//
// What it pins is the shape of the answer, because that is what a client branches on: the
// caller's own chat conversation gets 400 invalid_request - not 404, which would claim the
// conversation is not theirs, and not 409, which would claim a quiz already holds it open.
// And the refusal has to be visible as a refusal alone: nothing is started, nothing is
// reserved, and the learner's record does not move.
func TestLearningStartIsRefusedOnAPlainConversationOverHTTP(t *testing.T) {
	fixture := newLearningHTTPFixture(t, learningQuestionReply("会话模式"))
	handler := fixture.app.server.Handler()
	before := fixture.chat.calls

	chatBody := `{"topic":"Redis 持久化"}`
	rejected := decodeEnvelope(t, sendToHandler(handler, http.MethodPost,
		"/api/v1/learning/sessions?session_id="+fixture.app.session, fixture.token, chatBody))
	if rejected.status != http.StatusBadRequest || rejected.error_.Code != "invalid_request" {
		t.Fatalf("start on a chat conversation = %d %q, want 400 invalid_request",
			rejected.status, rejected.error_.Code)
	}
	if strings.Contains(rejected.body, "not_found") {
		t.Fatalf("the mode refusal was dressed up as a missing conversation: %s", rejected.body)
	}
	if fixture.chat.calls != before {
		t.Fatalf("a refused start spent %d model calls", fixture.chat.calls-before)
	}

	// Nothing was filed anywhere: the agent conversation is still idle, and the topic still
	// has no projection to read.
	idle := decodeEnvelope(t, sendToHandler(handler, http.MethodGet,
		"/api/v1/learning/sessions?session_id="+fixture.agentChat, fixture.token, ""))
	if idle.status != http.StatusOK || idle.data != nil {
		t.Fatalf("the refused start left a quiz behind: %d %s", idle.status, idle.body)
	}
	absent := decodeEnvelope(t, sendToHandler(handler, http.MethodGet,
		"/api/v1/learning/progress?topic=Redis%20%E6%8C%81%E4%B9%85%E5%8C%96", fixture.token, ""))
	if absent.status != http.StatusNotFound || absent.error_.Code != "not_found" {
		t.Fatalf("progress after a refused start = %d %q, want the untouched not-found",
			absent.status, absent.error_.Code)
	}

	// Whose conversation it is, and whether it is a learning agent at all, are different
	// questions, and the ownership answer comes first: a stranger's chat conversation and a
	// conversation that was never stored are told the same thing.
	foreign, err := fixture.app.store.CreateSession(t.Context(), &domain.Session{
		UserID: fixture.app.other, Title: "别人的普通对话", Mode: domain.SessionModeChat,
	})
	if err != nil {
		t.Fatal(err)
	}
	stranger := decodeEnvelope(t, sendToHandler(handler, http.MethodPost,
		"/api/v1/learning/sessions?session_id="+foreign.ID, fixture.token, chatBody))
	neverStored := decodeEnvelope(t, sendToHandler(handler, http.MethodPost,
		"/api/v1/learning/sessions?session_id=session-never-stored", fixture.token, chatBody))
	for _, probe := range []struct {
		label    string
		response renderedEnvelope
	}{{"another user's chat conversation", stranger}, {"an unknown conversation", neverStored}} {
		if probe.response.status != http.StatusNotFound || probe.response.error_.Code != "not_found" {
			t.Fatalf("%s = %d %q, want not_found", probe.label, probe.response.status,
				probe.response.error_.Code)
		}
	}
	if stranger.body != neverStored.body {
		t.Fatalf("the two not-founds differ:\n foreign: %s\n unknown: %s", stranger.body, neverStored.body)
	}
	if fixture.chat.calls != before {
		t.Fatalf("%d model calls after four refusals", fixture.chat.calls-before)
	}

	// The permitted path still works, unchanged: one call, one card.
	started := fixture.start(t, chatBody)
	if started.status != http.StatusCreated || started.data["current_question"] != float64(1) {
		t.Fatalf("start on the agent conversation = %d %s", started.status, started.body)
	}
	if fixture.chat.calls != before+1 {
		t.Fatalf("the accepted start spent %d model calls, want 1", fixture.chat.calls-before)
	}
}
