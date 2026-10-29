package app

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/devmodel"
	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/httpapi"
	"github.com/xiaopang456/GopherLearn/internal/knowledge"
	"github.com/xiaopang456/GopherLearn/internal/model"
	"github.com/xiaopang456/GopherLearn/internal/openai"
	"github.com/xiaopang456/GopherLearn/internal/repository"
	"github.com/xiaopang456/GopherLearn/internal/responder"
	"github.com/xiaopang456/GopherLearn/internal/security"
	"github.com/xiaopang456/GopherLearn/internal/service"
)

// The local scripted learning model, checked where the decision is really made: the
// configuration gate that lets it in, the assembly that keeps it out of every other
// answer path, and one whole quiz driven through the real HTTP handlers, the real
// LearningService and a real repository.
//
// What it is for, stated plainly: it proves the plumbing carries a quiz - question,
// answer, verdict, next question, completion, projection - with no provider between the
// browser and the database. It says nothing about whether a question is good or a grade
// is fair, because a script that cannot read an answer has no standing to score one.

const testJWTSecret = "local-learning-model-test-secret-0123456789abcdef"

func TestLocalLearningModelGateIsClosedUnlessConfigured(t *testing.T) {
	withSwitch := func(values map[string]string) map[string]string {
		merged := map[string]string{EnvLocalLearningModel: devmodel.LearningModelName}
		for key, value := range values {
			merged[key] = value
		}
		return merged
	}
	cases := []struct {
		name    string
		values  map[string]string
		want    string
		wantErr error
	}{
		{name: "unset", values: nil, want: ""},
		{name: "blank", values: map[string]string{EnvLocalLearningModel: "   "}, want: ""},
		{
			name:   "echo and nothing else",
			values: map[string]string{EnvLocalLearningModel: devmodel.LearningModelName},
			want:   devmodel.LearningModelName,
		},
		{
			name:   "spelling and padding are normalized, the name is not guessed",
			values: map[string]string{EnvLocalLearningModel: " Deterministic "},
			want:   devmodel.LearningModelName,
		},
		{
			name:    "a name this build does not carry",
			values:  map[string]string{EnvLocalLearningModel: "gpt-4o"},
			wantErr: ErrUnsupportedLocalLearningModel,
		},
		{
			// The switch and a real provider are contradictory instructions, and neither
			// may quietly win.
			name:    "openai mode asked for",
			values:  withSwitch(openAIEnvironment()),
			wantErr: ErrLocalLearningModelNeedsEcho,
		},
		{
			// Echo mode still. A leftover key is enough to refuse: an operator who
			// believes the notes go to a model must never be served scripted questions.
			name:    "provider key present, mode still echo",
			values:  withSwitch(map[string]string{openai.EnvAPIKey: "a-leftover-key"}),
			wantErr: ErrLocalLearningModelNeedsEcho,
		},
		{
			name:    "provider base url present",
			values:  withSwitch(map[string]string{openai.EnvBaseURL: "https://provider.invalid/v1"}),
			wantErr: ErrLocalLearningModelNeedsEcho,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			config, err := ConfigFromEnvironment(lookup(testCase.values))
			if testCase.wantErr != nil {
				if !errors.Is(err, testCase.wantErr) {
					t.Fatalf("error = %v, want %v", err, testCase.wantErr)
				}
				if config != (Config{}) {
					t.Fatalf("a refused configuration still produced %#v", config)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if config.LocalLearningModel != testCase.want {
				t.Fatalf("switch = %q, want %q", config.LocalLearningModel, testCase.want)
			}
			if config.Provider != ProviderEcho {
				t.Fatalf("provider = %q, want echo in every accepted case", config.Provider)
			}
		})
	}
}

func TestDefaultEchoAssemblyStillOffersNoModelAtAll(t *testing.T) {
	// Byte-for-byte: the README quotes this line as the proof that a keyless start
	// contacts nobody, so adding the local switch must not restate or soften it.
	const defaultEchoLine = "provider=echo (no provider calls; answers are deterministic placeholders; " +
		"learning_agent is a placeholder echo; session summaries are disabled, context is the recent window)"
	store, _, _, _ := agentFixture(t)
	config, err := ConfigFromEnvironment(lookup(nil))
	if err != nil {
		t.Fatal(err)
	}
	assembly, err := Assemble(store, config, ProductionOptions())
	if err != nil {
		t.Fatal(err)
	}
	if assembly.StartupLine != defaultEchoLine {
		t.Fatalf("startup line changed:\n got %q\nwant %q", assembly.StartupLine, defaultEchoLine)
	}
	if assembly.Model != nil || assembly.LearningModel != nil {
		t.Fatalf("the default assembly handed out a model: %T / %T", assembly.Model, assembly.LearningModel)
	}
	// And the quiz workflow still cannot be built over it, which is what keeps echo's
	// "no generated quizzes" claim true after this phase.
	quiz := service.NewLearningService(repository.NewInMemoryRepository(), assembly.LearningModel, nil, nil, nil)
	if _, err := quiz.Start(t.Context(), "user-1", "session-1", quizTopic, nil); !errors.Is(err, service.ErrLearningModelNotConfigured) {
		t.Fatalf("start with no model = %v, want the wiring refusal", err)
	}
}

func TestLocalLearningModelReachesOnlyTheQuizWorkflow(t *testing.T) {
	store, _, _, _ := agentFixture(t)
	config, err := ConfigFromEnvironment(lookup(map[string]string{
		EnvLocalLearningModel: devmodel.LearningModelName,
	}))
	if err != nil {
		t.Fatal(err)
	}
	// The shipped construction path, so what is tested is the wiring a real process uses
	// rather than a copy a test assembled for the occasion.
	assembly, err := Assemble(store, config, ProductionOptions())
	if err != nil {
		t.Fatal(err)
	}
	scripted, ok := assembly.LearningModel.(*devmodel.Learning)
	if !ok {
		t.Fatalf("the assembly's learning model is %T", assembly.LearningModel)
	}
	if assembly.Model != nil {
		t.Fatalf("the scripted model was offered as a provider client: %T", assembly.Model)
	}
	if assembly.Summaries != nil {
		t.Fatal("a scripted model must not maintain session summaries")
	}
	if _, capable := model.ChatModel(scripted).(model.ToolChatModel); capable {
		t.Fatal("the scripted learning model could be wired into the agent harness")
	}
	for _, phrase := range []string{
		"provider=echo", "local_learning_model=" + devmodel.LearningModelName,
		"NOT a provider", "chat answers are still placeholders",
	} {
		if !strings.Contains(assembly.StartupLine, phrase) {
			t.Fatalf("startup line omits %q:\n%s", phrase, assembly.StartupLine)
		}
	}

	// A conversation turn through this assembly is answered by the echo strategy and
	// asks the script nothing: the seam reaches the quiz and no other answer path.
	turn, err := assembly.Assistant.Respond(t.Context(), responder.Request{
		UserID: "user-1", Content: "RDB 和 AOF 该选哪个？", AnswerStrategy: domain.AnswerStrategyDirectChat,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(turn.Content, "本地脚本") {
		t.Fatalf("a chat turn was answered by the quiz script: %q", turn.Content)
	}
	if generations, gradings := scripted.Calls(); generations != 0 || gradings != 0 {
		t.Fatalf("answering one chat question spent %d generations and %d gradings", generations, gradings)
	}

	// Wiring faults, in both directions.
	bare, err := Assemble(store, config, Options{})
	if !errors.Is(err, ErrNoLocalLearningModel) {
		t.Fatalf("assembly without a constructor = %v, want the wiring refusal", err)
	}
	if bare.LearningModel != nil || bare.Assistant != nil {
		t.Fatalf("a refused assembly still handed out pieces: %+v", bare)
	}
	// A Config built by hand, bypassing the environment gate.
	handBuilt := Config{Provider: ProviderOpenAI, LocalLearningModel: devmodel.LearningModelName}
	if _, err := Assemble(store, handBuilt, ProductionOptions()); !errors.Is(err, ErrLocalLearningModelNeedsEcho) {
		t.Fatalf("openai mode with the switch on = %v, want the refusal", err)
	}
	wanted := errors.New("constructor refused")
	if _, err := Assemble(store, config, Options{NewLocalLearningModel: func(string) (model.ChatModel, error) {
		return nil, wanted
	}}); !errors.Is(err, wanted) {
		t.Fatalf("error = %v, want the constructor's own failure", err)
	}
}

// --- one whole quiz, over the real HTTP handlers ---

const quizTopic = "Redis 持久化"

// persistenceNotes is written to clear the production evidence rules, not to look
// pretty: a chunk shorter than DefaultLearningEvidenceMinItemRunes is not evidence at
// all, and at most three chunks of one document may back a quiz. Notes that are only
// headings and one-line bullets therefore answer learning_no_evidence - which is the
// honest answer, and the reason the local model is not allowed to relax it.
func persistenceNotes() string {
	return strings.Join([]string{
		"# Redis 持久化",
		"",
		"## RDB 快照怎么做以及它贵在哪里",
		"",
		"RDB 把某一时刻的全部数据写成一份压缩的二进制文件，恢复时 Redis 直接把这文件读回内存，",
		"所以冷启动很快，也适合拿去做异地备份和全量存档。代价是两次快照之间的写入可能在崩溃时全部丢失：",
		"Redis 只能按配置的间隔触发快照，间隔越长丢得越多，fork 写时复制在大数据集上还会带来一秒级的抖动。",
		"",
		"## AOF 追加日志为什么丢得更少",
		"",
		"AOF 把每条改变数据的命令按协议追加到日志末尾，重启时顺序重放这份日志来重建数据。",
		"因为追加是顺序写、并且可以按策略尽快落盘，它的持久性比快照好得多。代价是文件通常比同等的 RDB 大很多，",
		"重放也比读回一份二进制慢；日志还会增长，所以需要重写来压缩：当文件超过设定的增长比例时，",
		"Redis 用当前内存状态生成一份新的日志替掉旧的。",
		"",
		"## 刷盘策略决定真正丢多少",
		"",
		"appendfsync 控制缓冲区什么时候真正落到磁盘：always 表示每条命令都刷一次，最安全但吞吐最低；",
		"everysec 每秒刷一次，崩溃时最多丢一秒，是常见的折中；no 表示完全不主动刷，交给操作系统和内核的决定，",
		"最快但也最不可靠，机器断电会丢掉不止一秒。要注意这只描述落盘时机，不代表命令没被写进文件。",
		"",
		"## 两种一起用时以谁为准",
		"",
		"生产上通常同时保留 RDB 与 AOF：RDB 负责快速恢复、异地备份和版本存档，AOF 负责把丢失窗口压到一秒以内。",
		"两者都开启时，Redis 重启只重放 AOF，因为它包含的数据更新，RDB 就被降级为备份用途；",
		"如果只有 AOF 在写而没人定期做 RDB，异地全量传输就得自己拼一份，这正是很多故障复盘里被点出的那一步。",
	}, "\n")
}

func TestLocalLearningModelDrivesAWholeQuizOverHTTP(t *testing.T) {
	send, scripted, agentSession := localLearningCaller(t)

	started := send(http.MethodPost, "/api/v1/learning/sessions?session_id="+agentSession,
		`{"topic":"`+quizTopic+`"}`)
	if started.status != http.StatusCreated {
		t.Fatalf("start = %d %s", started.status, started.body)
	}
	if started.data["status"] != string(domain.LearningSessionWaitingAnswer) ||
		started.data["current_question"] != float64(1) ||
		started.data["total_questions"] != float64(domain.LearningQuestionCount) {
		t.Fatalf("a fresh quiz is not open on its first card: %s", started.body)
	}
	quizID, _ := started.data["id"].(string)
	firstQuestion := questionText(t, started.data["question"])

	// The three cards, each answered through the production handler, and each response
	// naming the next card rather than this one's verdict.
	var (
		asked      = []string{firstQuestion}
		lastAnswer localResponse
	)
	for ordinal := 1; ordinal <= domain.LearningQuestionCount; ordinal++ {
		answered := send(http.MethodPost, "/api/v1/learning/sessions/"+quizID+"/answer",
			quizAnswerBody(ordinal, "sub-local"))
		if answered.status != http.StatusOK {
			t.Fatalf("answer %d = %d %s", ordinal, answered.status, answered.body)
		}
		lastAnswer = answered
		if ordinal < domain.LearningQuestionCount {
			if answered.data["current_question"] != float64(ordinal+1) {
				t.Fatalf("after answer %d the quiz sits on %v: %s", ordinal,
					answered.data["current_question"], answered.body)
			}
			asked = append(asked, questionText(t, answered.data["question"]))
		}
	}
	if lastAnswer.data["status"] != string(domain.LearningSessionCompleted) {
		t.Fatalf("three answers did not complete the quiz: %s", lastAnswer.body)
	}
	if lastAnswer.data["question"] != nil {
		t.Fatalf("a completed quiz still carries a question: %s", lastAnswer.body)
	}

	// The same submission id and the same answer again: the stored verdict is replayed,
	// byte for byte, and no fourth grading call is spent.
	generations, gradings := scripted.Calls()
	replay := send(http.MethodPost, "/api/v1/learning/sessions/"+quizID+"/answer",
		quizAnswerBody(1, "sub-local"))
	if replay.status != http.StatusOK || replay.body != lastAnswer.body {
		t.Fatalf("replaying a submission changed the response:\n got %d %s\nwant the last answer's body",
			replay.status, replay.body)
	}
	if afterGenerations, afterGradings := scripted.Calls(); afterGenerations != generations || afterGradings != gradings {
		t.Fatalf("a replayed submission spent %d generations and %d gradings, want neither",
			afterGenerations-generations, afterGradings-gradings)
	}

	// A finished quiz refuses an action, and a resume reports the idle conversation
	// exactly as the contract says: 200 with null data, not a missing resource.
	closed := send(http.MethodPost, "/api/v1/learning/sessions/"+quizID+"/skip",
		`{"submission_id":"sub-late"}`)
	closed.requireCode(t, http.StatusConflict, "learning_session_closed")
	resumed := send(http.MethodGet, "/api/v1/learning/sessions?session_id="+agentSession, "")
	if resumed.status != http.StatusOK || resumed.data != nil {
		t.Fatalf("resume after completion = %d %s, want 200 with null data", resumed.status, resumed.body)
	}

	// The projection this run caused. These are the numbers a client may read, and they
	// came out of the stored attempts rather than from anywhere near a response.
	progress := send(http.MethodGet, "/api/v1/learning/progress?topic="+url.QueryEscape(quizTopic), "")
	if progress.status != http.StatusOK {
		t.Fatalf("progress = %d %s", progress.status, progress.body)
	}
	record := progress.data
	if record["topic"] != quizTopic || record["topic_key"] == "" {
		t.Fatalf("progress does not name its topic: %s", progress.body)
	}
	if record["valid_attempts"] != float64(domain.LearningQuestionCount) {
		t.Fatalf("attempts = %v, want %d: %s", record["valid_attempts"], domain.LearningQuestionCount, progress.body)
	}
	if record["confidence"] != 0.5 {
		t.Fatalf("confidence = %v, want three counted attempts spread over six: %s", record["confidence"], progress.body)
	}
	if mastery, _ := record["mastery"].(float64); mastery <= 0 || mastery >= 85 {
		t.Fatalf("mastery = %v, want the scripted verdicts inside the developing band: %s", mastery, progress.body)
	}
	if record["level"] != string(domain.TopicProgressDeveloping) {
		t.Fatalf("level = %v: %s", record["level"], progress.body)
	}
	if weakPoints, _ := record["weak_points"].([]any); len(weakPoints) == 0 {
		t.Fatalf("no weak points after three graded attempts: %s", progress.body)
	}

	// A second quiz on the same topic must clear the workflow's own duplicate rules,
	// which is the property the scripted questions were designed to have.
	second := send(http.MethodPost, "/api/v1/learning/sessions?session_id="+agentSession,
		`{"topic":"`+quizTopic+`"}`)
	if second.status != http.StatusCreated {
		t.Fatalf("a second quiz on one topic was refused: %d %s", second.status, second.body)
	}
	secondQuiz, _ := second.data["id"].(string)
	if text := questionText(t, second.data["question"]); text == firstQuestion {
		t.Fatalf("the second quiz asked the first quiz's question again: %q", text)
	}
	// The code-reading angle is index 3 of 5, so it surfaces in the second quiz's first
	// card rather than the first quiz's - a check over round one alone would assert
	// nothing about it.
	cards := append(append([]string(nil), asked...), questionText(t, second.data["question"]))
	for _, text := range cards {
		// A learner reading this must be able to tell it came from a script.
		if !strings.Contains(text, "第") || !strings.Contains(text, "组") {
			t.Fatalf("a question does not say which scripted round wrote it: %q", text)
		}
		// ...and the notes above hold no fenced block, so no question may claim one. This
		// is the same defect the unit test guards, checked against the prompts the
		// workflow really renders rather than a hand-written imitation of them.
		for _, claim := range []string{"代码", "片段"} {
			if strings.Contains(text, claim) {
				t.Fatalf("a question promises a code fragment the notebook does not contain (%q): %q", claim, text)
			}
		}
	}

	// Skip and cancel are production transitions, and neither may reach a model.
	beforeGenerations, beforeGradings := scripted.Calls()
	skipped := send(http.MethodPost, "/api/v1/learning/sessions/"+secondQuiz+"/skip",
		`{"submission_id":"sub-skip-1"}`)
	if skipped.status != http.StatusOK || skipped.data["current_question"] != float64(2) {
		t.Fatalf("skip = %d %s", skipped.status, skipped.body)
	}
	cancelled := send(http.MethodPost, "/api/v1/learning/sessions/"+secondQuiz+"/cancel", "")
	if cancelled.status != http.StatusOK || cancelled.data["status"] != string(domain.LearningSessionCancelled) {
		t.Fatalf("cancel = %d %s", cancelled.status, cancelled.body)
	}
	if cancelled.data["question"] != nil {
		t.Fatalf("a cancelled quiz still carries a question: %s", cancelled.body)
	}
	afterGenerations, afterGradings := scripted.Calls()
	if afterGenerations != beforeGenerations || afterGradings != beforeGradings {
		t.Fatalf("skip and cancel spent %d generations and %d gradings, want none",
			afterGenerations-beforeGenerations, afterGradings-beforeGradings)
	}

	// Two quizzes and three graded answers, and nothing else asked this model anything.
	if totalGenerations, totalGradings := scripted.Calls(); totalGenerations != 2 || totalGradings != 3 {
		t.Fatalf("the session spent %d generations and %d gradings, want 2 and 3",
			totalGenerations, totalGradings)
	}
}

// TestLocalLearningModelRepeatsAQuizOnASingleSectionNotebook covers the thin-corpus case,
// which is where the scripted questions have the least room to move: one admitted chunk
// means every question cites the same evidence, so the workflow's duplicate rules weigh
// wording alone. A second quiz on the same topic must still be lawful.
func TestLocalLearningModelRepeatsAQuizOnASingleSectionNotebook(t *testing.T) {
	send, _, agentSession := localLearningCallerWithNotes(t, strings.Join([]string{
		"## RDB 快照与它的代价",
		"",
		"RDB 把某一时刻的全部数据写成一份压缩的二进制文件，恢复时 Redis 直接把这文件读回内存，所以冷启动很快，",
		"也适合拿去做异地备份和全量存档。代价是两次快照之间的写入可能在崩溃时全部丢失，因为 Redis 只能按配置的间隔触发快照，",
		"间隔越长丢得越多；而 fork 出来做快照时的写时复制，在大数据集上还会带来可感知的抖动，这一点在评估容量时经常被忽略。",
	}, "\n"))

	first := completeLocalQuiz(t, send, agentSession, "single")
	second := completeLocalQuiz(t, send, agentSession, "repeat")
	seen := map[string]struct{}{}
	for _, text := range first {
		seen[text] = struct{}{}
	}
	for _, text := range second {
		if _, repeat := seen[text]; repeat {
			t.Fatalf("the second quiz re-asked a question from the first, and the rules below let it through: %q", text)
		}
	}
	if len(second) != domain.LearningQuestionCount || len(first) != domain.LearningQuestionCount {
		t.Fatalf("%d and %d cards seen, want three each", len(first), len(second))
	}

	// Six counted attempts on one topic: the projection is the same reader's, and it is
	// allowed to say something about a learner only once the record is thick enough.
	progress := send(http.MethodGet, "/api/v1/learning/progress?topic="+url.QueryEscape(quizTopic), "")
	if progress.status != http.StatusOK {
		t.Fatalf("progress = %d %s", progress.status, progress.body)
	}
	if progress.data["valid_attempts"] != float64(2*domain.LearningQuestionCount) {
		t.Fatalf("attempts = %v, want six: %s", progress.data["valid_attempts"], progress.body)
	}
	if progress.data["confidence"] != float64(1) {
		t.Fatalf("confidence = %v, want the six-attempt ceiling: %s", progress.data["confidence"], progress.body)
	}
	if progress.data["level"] == string(domain.TopicProgressInsufficient) {
		t.Fatalf("six graded attempts still read as insufficient: %s", progress.body)
	}
}

// completeLocalQuiz answers one whole quiz and returns every question card the learner
// was shown, in order.
func completeLocalQuiz(t *testing.T, send func(string, string, string) localResponse, agentSession, prefix string) []string {
	t.Helper()
	started := send(http.MethodPost, "/api/v1/learning/sessions?session_id="+agentSession,
		`{"topic":"`+quizTopic+`"}`)
	if started.status != http.StatusCreated {
		t.Fatalf("start %q = %d %s", prefix, started.status, started.body)
	}
	quizID, _ := started.data["id"].(string)
	cards := []string{questionText(t, started.data["question"])}
	for ordinal := 1; ordinal <= domain.LearningQuestionCount; ordinal++ {
		answered := send(http.MethodPost, "/api/v1/learning/sessions/"+quizID+"/answer",
			quizAnswerBody(ordinal, prefix))
		if answered.status != http.StatusOK {
			t.Fatalf("answer %d of %q = %d %s", ordinal, prefix, answered.status, answered.body)
		}
		if ordinal < domain.LearningQuestionCount {
			cards = append(cards, questionText(t, answered.data["question"]))
		}
	}
	return cards
}

func TestLocalLearningQuizResponsesCarryNoAnswerKey(t *testing.T) {
	send, _, agentSession := localLearningCaller(t)

	started := send(http.MethodPost, "/api/v1/learning/sessions?session_id="+agentSession,
		`{"topic":"`+quizTopic+`"}`)
	quizID, _ := started.data["id"].(string)
	bodies := []string{started.body}
	for ordinal := 1; ordinal <= domain.LearningQuestionCount; ordinal++ {
		bodies = append(bodies, send(http.MethodPost,
			"/api/v1/learning/sessions/"+quizID+"/answer",
			quizAnswerBody(ordinal, "sub-key")).body)
	}
	bodies = append(bodies, send(http.MethodGet,
		"/api/v1/learning/progress?topic="+url.QueryEscape(quizTopic), "").body)
	bodies = append(bodies, send(http.MethodGet,
		"/api/v1/learning/sessions?session_id="+agentSession, "").body)

	forbidden := []string{
		"reference_answer", "rubric", "assessment_goal", "evidence", "snapshot",
		"本地脚本参考答案", "本地脚本判分", "missing_points", "feedback",
		"二进制文件", "写时复制", "appendfsync", "everysec", "重放这份日志",
		"document_version", "chunk_id", "collection_id",
	}
	for _, body := range bodies {
		for _, phrase := range forbidden {
			if strings.Contains(body, phrase) {
				t.Fatalf("a response leaks %q:\n%s", phrase, body)
			}
		}
	}
}

// --- fixture ---

// localLearningCaller assembles the process cmd/gopherlearn assembles, from the same
// production options, in echo mode with the local switch on: an in-memory repository, a
// real knowledge store, the real retrieval pipeline, the real responder strategies and
// the real HTTP handlers. A user, a notebook and a learning-agent conversation are
// created through those same handlers, so nothing the quiz touches was written behind
// the API.
func localLearningCaller(t *testing.T) (func(string, string, string) localResponse, *devmodel.Learning, string) {
	t.Helper()
	return localLearningCallerWithNotes(t, persistenceNotes())
}

// localLearningCallerWithNotes is the same process with a caller-chosen notebook, because
// what a notebook yields decides which production rules the scripted questions have to
// clear.
func localLearningCallerWithNotes(t *testing.T, notes string) (func(string, string, string) localResponse, *devmodel.Learning, string) {
	t.Helper()
	repo := repository.NewInMemoryRepository()
	store := knowledge.NewMemoryStore()
	ingest := service.NewKnowledgeService(store)

	tokens, err := security.NewTokenManager(testJWTSecret, "gopherlearn-test")
	if err != nil {
		t.Fatal(err)
	}
	config, err := ConfigFromEnvironment(lookup(map[string]string{
		EnvLocalLearningModel: devmodel.LearningModelName,
	}))
	if err != nil {
		t.Fatal(err)
	}
	assembly, err := Assemble(store, config, ProductionOptions())
	if err != nil {
		t.Fatal(err)
	}
	scripted, ok := assembly.LearningModel.(*devmodel.Learning)
	if !ok {
		t.Fatalf("the assembly's learning model is %T", assembly.LearningModel)
	}
	quiz := service.NewLearningService(repo, assembly.LearningModel,
		NewRetrievalPipeline(store), ingest, ingest)
	messages := NewMessageService(repo, assembly.Assistant, ingest, service.ContextBudget{}.Normalize(),
		assembly.SummaryOptions()...)
	handler := httpapi.NewServer(
		service.NewAuthService(repo, tokens, func() time.Time { return time.Now().UTC() }),
		service.NewSessionService(repo), messages, tokens,
		httpapi.WithKnowledge(ingest),
		httpapi.WithMemory(service.NewMemoryService(repo)),
		httpapi.WithLearning(quiz),
	).Handler()

	send := func(method, path, body string) localResponse {
		return roundTrip(t, handler, method, path, body)
	}

	registered := send(http.MethodPost, "/api/v1/auth/register",
		`{"email":"local-learner@example.test","password":"correct horse battery","display_name":"本地联调"}`)
	if registered.status != http.StatusOK {
		t.Fatalf("register = %d %s", registered.status, registered.body)
	}
	logged := send(http.MethodPost, "/api/v1/auth/login",
		`{"email":"local-learner@example.test","password":"correct horse battery"}`)
	token, _ := logged.data["token"].(string)
	if logged.status != http.StatusOK || token == "" {
		t.Fatalf("login = %d %s", logged.status, logged.body)
	}
	send = func(method, path, body string) localResponse {
		return roundTripWithToken(t, handler, method, path, token, body)
	}

	collection := send(http.MethodPost, "/api/v1/collections", `{"title":"Redis 手册"}`)
	collectionID, _ := collection.data["id"].(string)
	if collection.status >= 400 || collectionID == "" {
		t.Fatalf("collection = %d %s", collection.status, collection.body)
	}
	document := send(http.MethodPost, "/api/v1/collections/"+collectionID+"/documents",
		`{"title":"persistence.md","format":"markdown","content":`+jsonString(t, notes)+`}`)
	if document.status >= 400 {
		t.Fatalf("document = %d %s", document.status, document.body)
	}
	// The evidence a quiz is built from has to survive chunking and admission, or the
	// scripted run would be proving nothing about the real retrieval path - and the
	// sections above are long enough to do it.
	session := send(http.MethodPost, "/api/v1/sessions", `{"title":"本地联调会话","mode":"learning_agent"}`)
	agentSession, _ := session.data["id"].(string)
	if session.status >= 400 || agentSession == "" {
		t.Fatalf("session = %d %s", session.status, session.body)
	}
	return send, scripted, agentSession
}

type localResponse struct {
	status  int
	body    string
	data    map[string]any
	errorOf struct{ Code string }
}

func (r localResponse) requireCode(t *testing.T, status int, code string) {
	t.Helper()
	if r.status != status || r.errorOf.Code != code {
		t.Fatalf("%d %s, want %d %s", r.status, r.body, status, code)
	}
}

func roundTrip(t *testing.T, handler http.Handler, method, path, body string) localResponse {
	t.Helper()
	return roundTripWithToken(t, handler, method, path, "", body)
}

func roundTripWithToken(t *testing.T, handler http.Handler, method, path, token, body string) localResponse {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	encoded, err := io.ReadAll(recorder.Body)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	var envelope struct {
		Data  map[string]any        `json:"data"`
		Error struct{ Code string } `json:"error"`
	}
	if err := json.Unmarshal(encoded, &envelope); err != nil {
		t.Fatalf("decode %q: %v", text, err)
	}
	response := localResponse{status: recorder.Code, body: text, data: envelope.Data}
	response.errorOf = envelope.Error
	return response
}

// quizAnswerBody renders one submission. The id is derived from the prefix and the
// ordinal, so a replay in a test sends text that is identical byte for byte.
func quizAnswerBody(ordinal int, prefix string) string {
	return `{"submission_id":"` + prefix + "-" + strconv.Itoa(ordinal) +
		`","ordinal":` + strconv.Itoa(ordinal) +
		`,"answer":"本地联调答案：说明机制、前提与代价。"}`
}

func questionText(t *testing.T, raw any) string {
	t.Helper()
	question, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("no question card in this response: %#v", raw)
	}
	text, _ := question["question"].(string)
	if text == "" {
		t.Fatalf("the question card came back with no text: %#v", question)
	}
	return text
}

func jsonString(t *testing.T, value string) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}
