package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/model"
	"github.com/xiaopang456/GopherLearn/internal/service"
)

// The quiz surface as a reviewed contract: one row per call in
// testdata/http_contract.golden.
//
// What these rows are for is the shape of what leaves the server. A learner's client must
// be able to render a quiz from the answer endpoint alone, and everything that would let
// it instead read how a question will be scored - a reference answer, a rubric, an evidence
// snapshot, a raw mastery number attached to an answer - has to be absent from the bytes,
// not merely discouraged. A golden row is the cheapest place to see that: it is the whole
// body, compared exactly, and a new field shows up as a diff someone has to sign off on.
//
// Two markers below duplicate strings that live in the service package's prompt constants.
// That is deliberate: if a rename makes the routed double stop recognising which call it is
// answering, these cases fail rather than quietly sending a generator's answer to a grader.

const (
	// learningContractGeneration is the generator's own role word.
	learningContractGeneration = "出题器"
	// learningContractGrading is the grader's.
	learningContractGrading = "评分器"
	// learningContractSeedSubmission and learningContractSeedAnswer are replayed verbatim
	// by the idempotency row, so a retry of the stored answer is what is asserted, not a
	// similar one.
	learningContractSeedSubmission = "sub-golden-seed"
	learningContractSeedAnswer     = "快照为了恢复速度，追加日志为了少丢数据。"
)

// learningContractChat answers whichever call the quiz workflow makes by recognising the
// role it was given.
//
// A scripted queue could not serve these cases: every case gets a fresh fixture, so a
// fixture that pre-answers one question consumes the generation reply and the grading
// reply, and the case's own call would then be handed whichever reply happened to sit at
// index three - a generator's JSON to a grader, or the other way round. Routing on the
// prompt keeps one script honest for any number of calls.
type learningContractChat struct {
	generationCalls int
	gradingCalls    int
}

func (c *learningContractChat) Complete(_ context.Context, request model.Request) (model.Completion, error) {
	if len(request.Messages) == 0 {
		return model.Completion{}, model.ErrEmptyCompletion
	}
	system := request.Messages[0].Content
	switch {
	case strings.Contains(system, learningContractGrading):
		c.gradingCalls++
		return model.Completion{Content: learningGradeReply(), FinishReason: model.FinishStop}, nil
	case strings.Contains(system, learningContractGeneration):
		c.generationCalls++
		// The label follows the call count, so a second quiz on the same topic is not read
		// as a rewording of the first and refused by the novelty rules.
		return model.Completion{
			Content:      learningQuestionReply("卷 " + string(rune('A'+c.generationCalls))),
			FinishReason: model.FinishStop,
		}, nil
	}
	return model.Completion{}, model.ErrEmptyCompletion
}

func (c *learningContractChat) Stream(ctx context.Context, request model.Request, _ model.DeltaHandler) (model.Completion, error) {
	return c.Complete(ctx, request)
}

// newLearningContractFixture returns the seeded application with the quiz workflow
// installed and one answer already graded.
//
// The state it leaves behind is what most rows below ask about: a quiz on topic AOF, open
// on its second card after a scored first answer, plus a second learning-agent
// conversation with nothing running on it for the rows that start a quiz of their own.
func newLearningContractFixture(t *testing.T) *apiFixture {
	t.Helper()
	fixture := newFixture(t, apiResponder{})
	chat := &learningContractChat{}
	ctx := t.Context()

	free, err := fixture.store.CreateSession(ctx, &domain.Session{UserID: fixture.owner,
		Title: "空闲会话", Mode: domain.SessionModeLearningAgent})
	if err != nil {
		t.Fatal(err)
	}
	busy, err := fixture.store.CreateSession(ctx, &domain.Session{UserID: fixture.owner,
		Title: "测验中会话", Mode: domain.SessionModeLearningAgent})
	if err != nil {
		t.Fatal(err)
	}

	recall := &learningHTTPRecall{results: learningHTTPChunks(8)}
	fixture.server.learning = service.NewLearningService(fixture.store, chat, recall, fixture.service, fixture.service)

	owner, err := fixture.tokens.Issue(fixture.owner)
	if err != nil {
		t.Fatal(err)
	}
	// Opening the seeded quiz and grading one answer goes through the same handlers the
	// rows below do, so the fixture holds nothing a client could not have caused.
	started := decodeEnvelope(t, sendToHandler(fixture.server.Handler(), http.MethodPost,
		learningPath+"?session_id="+busy.ID, owner, `{"topic":"AOF"}`))
	if started.status != http.StatusCreated {
		t.Fatalf("seeding a quiz = %d: %s", started.status, started.body)
	}
	quizID, _ := started.data["id"].(string)
	if quizID == "" {
		t.Fatalf("the seeded quiz came back without an id: %s", started.body)
	}
	answered := decodeEnvelope(t, sendToHandler(fixture.server.Handler(), http.MethodPost,
		learningPath+"/"+quizID+"/answer", owner, learningContractAnswerBody(learningContractSeedSubmission, "1",
			learningContractSeedAnswer)))
	if answered.status != http.StatusOK {
		t.Fatalf("seeding an answer = %d: %s", answered.status, answered.body)
	}

	fixture.learningSession = free.ID
	fixture.learningBusySession = busy.ID
	fixture.learningQuiz = quizID
	return fixture
}

// learningContractAnswerBody renders one answer request. The shape is fixed here so every
// row sends the same three fields and a drift in one of them is visible in the row that
// regressed.
func learningContractAnswerBody(submission, ordinal, answer string) string {
	return `{"submission_id":"` + submission + `","ordinal":` + ordinal +
		`,"answer":"` + answer + `"}`
}

func learningContractCases() []contractCase {
	start := learningPath + "?session_id=" + learningSessionPlaceholder
	resume := learningPath + "?session_id=" + learningBusyPlaceholder
	answer := learningItemPath + "/answer"
	return []contractCase{
		// Starting.
		{
			name: "learning start", method: http.MethodPost, path: start,
			body: `{"topic":"RDB"}`, token: tokenOwner, learning: true,
		},
		{
			name: "learning start with an answer key field", method: http.MethodPost, path: start,
			body: `{"topic":"RDB","reference_answer":"替我写好答案"}`, token: tokenOwner, learning: true,
		},
		{name: "learning start without a topic", method: http.MethodPost, path: start,
			body: `{}`, token: tokenOwner, learning: true},
		{name: "learning start without a conversation", method: http.MethodPost, path: learningPath,
			body: `{"topic":"RDB"}`, token: tokenOwner, learning: true},
		{name: "learning start on an unknown conversation", method: http.MethodPost,
			path: learningPath + "?session_id=session-404", body: `{"topic":"RDB"}`,
			token: tokenOwner, learning: true},
		// A conversation of the caller's that is not a learning agent: theirs to see, so it
		// is a 400 about the request rather than a 404 about existence, and the message is
		// its own so a client can tell which of its two mistakes it made.
		{name: "learning start on a plain chat conversation", method: http.MethodPost,
			path: learningPath + "?session_id=" + sessionPlaceholder, body: `{"topic":"RDB"}`,
			token: tokenOwner, learning: true},
		{name: "learning start while a quiz is open", method: http.MethodPost, path: resume,
			body: `{"topic":"RDB"}`, token: tokenOwner, learning: true},
		{name: "learning start anonymous", method: http.MethodPost, path: start,
			body: `{"topic":"RDB"}`, token: tokenNone, learning: true},
		{name: "learning start cross owner", method: http.MethodPost, path: start,
			body: `{"topic":"RDB"}`, token: tokenOther, learning: true},
		{name: "learning collection wrong method", method: http.MethodDelete, path: learningPath,
			token: tokenOwner, learning: true},

		// Resuming is the reason a quiz survives a reload.
		{name: "learning resume", method: http.MethodGet, path: resume,
			token: tokenOwner, learning: true},
		{name: "learning resume with nothing open", method: http.MethodGet, path: start,
			token: tokenOwner, learning: true},
		// The pair the empty state must never be confused with: a conversation that is not
		// the caller's, and one that was never written, both 404 - while a conversation of
		// theirs that simply holds no quiz is 200 with null data.
		{name: "learning resume on an unknown conversation", method: http.MethodGet,
			path: learningPath + "?session_id=session-404", token: tokenOwner, learning: true},
		{name: "learning resume without a conversation", method: http.MethodGet, path: learningPath,
			token: tokenOwner, learning: true},
		{name: "learning resume anonymous", method: http.MethodGet, path: resume,
			token: tokenNone, learning: true},
		{name: "learning resume cross owner", method: http.MethodGet, path: resume,
			token: tokenOther, learning: true},

		// Answering: the idempotent case, the refusals, and the two ways an answer can be
		// addressed at a quiz the caller has no business grading.
		{
			name: "learning answer replay", method: http.MethodPost, path: answer,
			body:  learningContractAnswerBody(learningContractSeedSubmission, "1", learningContractSeedAnswer),
			token: tokenOwner, learning: true,
		},
		{
			name: "learning answer next card", method: http.MethodPost, path: answer,
			body:  learningContractAnswerBody("sub-golden-two", "2", "追加日志按策略回写，重写时压缩。"),
			token: tokenOwner, learning: true,
		},
		{
			name: "learning answer out of order", method: http.MethodPost, path: answer,
			body:  learningContractAnswerBody("sub-golden-three", "3", "跳过当前卡片直接答第三题。"),
			token: tokenOwner, learning: true,
		},
		{
			name: "learning answer without a submission id", method: http.MethodPost, path: answer,
			body: `{"ordinal":2,"answer":"没有编号的回答"}`, token: tokenOwner, learning: true,
		},
		{
			name: "learning answer blank", method: http.MethodPost, path: answer,
			body:  learningContractAnswerBody("sub-golden-blank", "2", "   "),
			token: tokenOwner, learning: true,
		},
		{
			name: "learning answer with a score field", method: http.MethodPost, path: answer,
			body:  `{"submission_id":"sub-golden-score","ordinal":2,"answer":"答一句","score":100}`,
			token: tokenOwner, learning: true,
		},
		{
			name: "learning answer anonymous", method: http.MethodPost, path: answer,
			body:  learningContractAnswerBody("sub-golden-anon", "2", "匿名作答"),
			token: tokenNone, learning: true,
		},
		{
			name: "learning answer cross owner", method: http.MethodPost, path: answer,
			body:  learningContractAnswerBody("sub-golden-foreign", "2", "别人代答"),
			token: tokenOther, learning: true,
		},
		{
			name: "learning answer unknown quiz", method: http.MethodPost,
			path:  learningPath + "/lsn-00000000-0000-0000-0000-000000000000/answer",
			body:  learningContractAnswerBody("sub-golden-absent", "1", "答一份不存在的测验"),
			token: tokenOwner, learning: true,
		},
		{name: "learning answer wrong method", method: http.MethodGet, path: answer,
			token: tokenOwner, learning: true},
		{name: "learning unknown action", method: http.MethodPost, path: learningItemPath + "/grade",
			body: `{}`, token: tokenOwner, learning: true},

		// Skipping and cancelling. Skip carries one field and nothing else: an ordinal
		// would let a client skip a card the learner is not looking at, and an answer or a
		// score would let it write the grade it is being given.
		{name: "learning skip", method: http.MethodPost, path: learningItemPath + "/skip",
			body: `{"submission_id":"sub-golden-skip"}`, token: tokenOwner, learning: true},
		{name: "learning skip without a submission id", method: http.MethodPost,
			path: learningItemPath + "/skip", body: `{}`, token: tokenOwner, learning: true},
		{name: "learning skip with an ordinal field", method: http.MethodPost,
			path:  learningItemPath + "/skip",
			body:  `{"submission_id":"sub-golden-skip-ordinal","ordinal":2}`,
			token: tokenOwner, learning: true},
		{name: "learning skip with a reference answer field", method: http.MethodPost,
			path:  learningItemPath + "/skip",
			body:  `{"submission_id":"sub-golden-skip-answer","reference_answer":"替我答对"}`,
			token: tokenOwner, learning: true},
		{name: "learning skip with a numeric submission id", method: http.MethodPost,
			path: learningItemPath + "/skip", body: `{"submission_id":7}`,
			token: tokenOwner, learning: true},
		{name: "learning cancel", method: http.MethodPost, path: learningItemPath + "/cancel",
			token: tokenOwner, learning: true},
		{name: "learning cancel with a body", method: http.MethodPost, path: learningItemPath + "/cancel",
			body: `{"reason":"不想做了"}`, token: tokenOwner, learning: true},

		// The learner's own record, and what it may not be asked about.
		{
			name: "learning progress", method: http.MethodGet,
			path: "/api/v1/learning/progress?topic=AOF", token: tokenOwner, learning: true,
		},
		{
			name: "learning progress for an unpractised topic", method: http.MethodGet,
			path: "/api/v1/learning/progress?topic=Sentinel", token: tokenOwner, learning: true,
		},
		{name: "learning progress without a topic", method: http.MethodGet,
			path: "/api/v1/learning/progress", token: tokenOwner, learning: true},
		{name: "learning progress anonymous", method: http.MethodGet,
			path: "/api/v1/learning/progress?topic=AOF", token: tokenNone, learning: true},
		{name: "learning progress cross owner", method: http.MethodGet,
			path: "/api/v1/learning/progress?topic=AOF", token: tokenOther, learning: true},

		// The same calls on a build with no quiz workflow: the routes exist, the feature
		// does not, and that is the server's condition rather than the caller's mistake.
		{name: "learning start without the workflow", method: http.MethodPost,
			path: learningPath + "?session_id=" + sessionPlaceholder, body: `{"topic":"AOF"}`,
			token: tokenOwner},
		{name: "learning progress without the workflow", method: http.MethodGet,
			path: "/api/v1/learning/progress?topic=AOF", token: tokenOwner},

		// Phase 8D: the quiz actions take bodies too, and a body that cannot be UTF-8 is
		// refused before anything is read - so the submission id below is not consumed by the
		// attempt, which the e2e test asserts separately.
		{name: "learning answer invalid utf-8", method: http.MethodPost, path: answer,
			body:  `{"submission_id":"sub-golden-utf8","ordinal":2,"answer":"答案` + "\xff\xfe" + `"}`,
			token: tokenOwner, learning: true},
		{name: "learning skip invalid utf-8", method: http.MethodPost,
			path:  learningItemPath + "/skip",
			body:  `{"submission_id":"` + `\ud800"}`,
			token: tokenOwner, learning: true},
	}
}
