package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/model"
	"github.com/xiaopang456/GopherLearn/internal/repository"
	"github.com/xiaopang456/GopherLearn/internal/retrieval"
)

// learningScriptedModel answers with prepared text, in order, and records every prompt
// it was sent. It stands in for a provider here for the same reason the rest of this
// suite does: the rules under test are about what the server accepts and refuses, and a
// real model would make those assertions depend on a coin flip.
type learningScriptedModel struct {
	replies  []string
	err      error
	calls    int
	requests []model.Request
}

func (m *learningScriptedModel) Complete(ctx context.Context, request model.Request) (model.Completion, error) {
	m.calls++
	m.requests = append(m.requests, request)
	if m.err != nil {
		return model.Completion{}, m.err
	}
	index := m.calls - 1
	if index >= len(m.replies) {
		// The last reply repeats rather than running out, so a test that expects fewer
		// calls than it made still fails on the count instead of erroring here.
		index = len(m.replies) - 1
	}
	if index < 0 {
		return model.Completion{}, model.ErrEmptyCompletion
	}
	return model.Completion{Content: m.replies[index], FinishReason: model.FinishStop}, nil
}

func (m *learningScriptedModel) Stream(ctx context.Context, request model.Request,
	onDelta model.DeltaHandler) (model.Completion, error) {
	completion, err := m.Complete(ctx, request)
	if err != nil {
		return model.Completion{}, err
	}
	if onDelta != nil {
		if err := onDelta(completion.Content); err != nil {
			return model.Completion{}, err
		}
	}
	return completion, nil
}

// promptOf returns the last user message of one recorded call, which is where the
// server puts the material a model is asked to work with.
func (m *learningScriptedModel) promptOf(index int) string {
	if index >= len(m.requests) {
		return ""
	}
	messages := m.requests[index].Messages
	return messages[len(messages)-1].Content
}

// learningRecall hands out prepared retrieval results and records the queries it was
// given, so scope and pool width can be asserted rather than assumed.
type learningRecall struct {
	results []retrieval.Result
	err     error
	calls   int
	queries []retrieval.Query
}

func (r *learningRecall) Search(ctx context.Context, query retrieval.Query) ([]retrieval.Result, error) {
	r.calls++
	r.queries = append(r.queries, query)
	if r.err != nil {
		return nil, r.err
	}
	return r.results, nil
}

// learningCatalog is the knowledge side of evidence: titles and the caller's own
// collection list.
type learningCatalog struct {
	titles      map[string]string
	collections []string
	titleErr    error
	listErr     error
}

func (c *learningCatalog) DocumentTitles(ctx context.Context, userID string,
	documentIDs []string) (map[string]string, error) {
	if c.titleErr != nil {
		return nil, c.titleErr
	}
	found := map[string]string{}
	for _, id := range documentIDs {
		if title, exists := c.titles[id]; exists {
			found[id] = title
		}
	}
	return found, nil
}

func (c *learningCatalog) ListCollections(ctx context.Context, userID string) ([]domain.Collection, error) {
	if c.listErr != nil {
		return nil, c.listErr
	}
	listed := make([]domain.Collection, 0, len(c.collections))
	for _, id := range c.collections {
		listed = append(listed, domain.Collection{ID: id, UserID: userID, Title: "集合 " + id})
	}
	return listed, nil
}

// learningFixture is one assembled quiz workflow over a fresh in-memory store.
type learningFixture struct {
	store      repository.Repository
	chat       *learningScriptedModel
	recall     *learningRecall
	catalog    *learningCatalog
	service    *LearningService
	userID     string
	other      string
	agentChat  string
	plainChat  string
	collection string
}

func newLearningFixture(t *testing.T, replies ...string) *learningFixture {
	t.Helper()
	ctx := t.Context()
	store := repository.NewInMemoryRepository()
	user, err := store.CreateUser(ctx, &domain.User{Email: "learner@example.com", Username: "learner"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.CreateUser(ctx, &domain.User{Email: "other@example.com", Username: "other"})
	if err != nil {
		t.Fatal(err)
	}
	agentChat, err := store.CreateSession(ctx, &domain.Session{UserID: user.ID, Title: "A",
		Mode: domain.SessionModeLearningAgent})
	if err != nil {
		t.Fatal(err)
	}
	plainChat, err := store.CreateSession(ctx, &domain.Session{UserID: user.ID, Title: "B",
		Mode: domain.SessionModeChat})
	if err != nil {
		t.Fatal(err)
	}

	fixture := &learningFixture{
		store:  store,
		chat:   &learningScriptedModel{replies: replies},
		recall: &learningRecall{results: learningChunks(8)},
		catalog: &learningCatalog{
			titles:      map[string]string{"doc-a": "Redis 笔记", "doc-b": "Go 笔记"},
			collections: []string{"col-1"},
		},
		userID: user.ID, other: other.ID, agentChat: agentChat.ID, plainChat: plainChat.ID,
		collection: "col-1",
	}
	fixture.service = NewLearningService(store, fixture.chat, fixture.recall, fixture.catalog, fixture.catalog)
	return fixture
}

// learningChunks fabricates retrieval results. Their text is long enough to survive the
// minimum-snapshot floor, so "too short to be evidence" stays a case a test reaches
// deliberately rather than by accident.
func learningChunks(count int) []retrieval.Result {
	results := make([]retrieval.Result, 0, count)
	for index := range count {
		document, section := "doc-a", "持久化"
		if index%3 == 2 {
			document = "doc-b"
		}
		if index%2 == 1 {
			section = "数据结构"
		}
		results = append(results, retrieval.Result{
			Chunk: domain.DocumentChunk{
				ID: fmt.Sprintf("chk-%02d", index), DocumentID: document, CollectionID: "col-1",
				Version: 1, Order: index, Section: section,
				// Real chunks from this project's chunker target about 700 runes, so the
				// fixture is written at that scale: far enough above the minimum-snapshot
				// floor that selection is being tested, not the floor.
				Text: fmt.Sprintf("第 %d 段：Redis 提供 RDB 快照与 AOF 追加日志两种持久化方式，"+
					"两者的取舍在于恢复速度与写入开销。RDB 在指定触发条件下把整个数据集写成一个"+
					"压缩的二进制文件，重启时可以直接载入，因此恢复很快；代价是两次快照之间的数据"+
					"会丢失。AOF 把每条写命令追加到日志末尾，按策略回写并做重写压缩，丢得更少但文件"+
					"更大、重放更慢。生产中通常两者同时开启，用 AOF 保证完整性、用 RDB 加速冷启动。"+
					"编号 %02d 的这段内容与其它段落彼此可区分，用于验证选择顺序。", index, index),
				StartLine: index*10 + 1, EndLine: index*10 + 9,
			},
			Score: float64(count-index) / float64(count),
		})
	}
	return results
}

// learningQuestionSpec is one question exactly as a generator's reply would describe
// it, so a test can build an unlawful question as easily as a lawful one.
type learningQuestionSpec struct {
	Type     string   `json:"type"`
	Question string   `json:"question"`
	Goal     string   `json:"assessment_goal"`
	Answer   string   `json:"reference_answer"`
	Rubric   []string `json:"rubric"`
	Evidence []int    `json:"evidence"`
}

// learningGeneratorReply renders a generator's JSON answer from explicit question
// specs. Anything the model is expected to produce can be spelled with it, including
// the shapes the server must refuse.
func learningGeneratorReply(specs ...learningQuestionSpec) string {
	// Marshalling these types cannot fail: they are strings and slices of strings.
	encoded, _ := json.Marshal(map[string]any{"questions": specs})
	return string(encoded)
}

// learningQuestions builds a lawful reply for the given evidence numbers, one question
// per group.
func learningQuestions(groups ...[]int) string {
	return learningQuestionsTagged("", groups...)
}

// learningQuestionsTagged is learningQuestions with a label woven into each question and
// goal, so one test can hold two sets that are about the same material without being the
// same questions. Citing different chunks is not enough on its own: the novelty rules
// require shared evidence as well as similar wording, so a set that differs only in its
// citations is deliberately read as one question reworded.
func learningQuestionsTagged(tag string, groups ...[]int) string {
	label := ""
	if tag != "" {
		label = tag + " "
	}
	specs := make([]learningQuestionSpec, 0, len(groups))
	for index, evidence := range groups {
		specs = append(specs, learningQuestionSpec{
			Type:     "short_answer",
			Question: fmt.Sprintf("%s第 %d 题：请说明这一组要点的取舍。", label, index+1),
			Goal:     fmt.Sprintf("%s目标 %d：区分两种做法的适用条件。", label, index+1),
			Answer:   "参考答案：快照面向恢复速度，追加日志面向持久性。",
			Rubric:   []string{"指出两种机制", "说出一种取舍"},
			Evidence: evidence,
		})
	}
	return learningGeneratorReply(specs...)
}

// learningGradeReply renders a grader's JSON answer.
func learningGradeReply(correctness, completeness, clarity int) string {
	return fmt.Sprintf(`{"correctness":%d,"completeness":%d,"clarity":%d,`+
		`"feedback":"要点基本答到，缺少一种取舍的说明。","missing_points":["没说出恢复速度与写入开销的取舍"],`+
		`"confidence":0.8}`, correctness, completeness, clarity)
}

func TestLearningStartStoresGeneratedQuestionsAndOpensTheQuiz(t *testing.T) {
	fixture := newLearningFixture(t, learningQuestions([]int{1, 2}, []int{3}, []int{4, 5}))

	view, err := fixture.service.Start(t.Context(), fixture.userID, fixture.agentChat, " Redis 持久化 ", nil)
	if err != nil {
		t.Fatal(err)
	}
	if view.Status != string(domain.LearningSessionWaitingAnswer) {
		t.Fatalf("status = %q, want waiting_answer", view.Status)
	}
	if view.TotalQuestions != domain.LearningQuestionCount || view.CurrentQuestion != 1 {
		t.Fatalf("position = %d/%d, want 1/%d", view.CurrentQuestion, view.TotalQuestions,
			domain.LearningQuestionCount)
	}
	if view.Topic != "Redis 持久化" {
		t.Fatalf("topic = %q, want the typed topic with its surrounding space trimmed", view.Topic)
	}
	if view.Question == nil || view.Question.Ordinal != 1 {
		t.Fatalf("question = %+v, want the first one", view.Question)
	}
	// The view is what a learner sees, so the answer key must not be in it.
	encoded, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	for _, leaked := range []string{"reference_answer", "rubric", "snapshot", "参考答案", "评分点"} {
		if strings.Contains(strings.ToLower(string(encoded)), strings.ToLower(leaked)) {
			t.Fatalf("the quiz view leaked %q into a learner-visible response: %s", leaked, encoded)
		}
	}
	if fixture.chat.calls != 1 {
		t.Fatalf("generation used %d model calls, want 1", fixture.chat.calls)
	}
	// Evidence reached the prompt numbered by position, with no internal identifier a
	// model could echo back.
	prompt := fixture.chat.promptOf(0)
	if !strings.Contains(prompt, "[1] 出自《Redis 笔记》") {
		t.Fatalf("the evidence block is not numbered as the prompt describes:\n%s", prompt)
	}
	for _, identifier := range []string{"chk-0", "doc-a", "col-1"} {
		if strings.Contains(prompt, identifier) {
			t.Fatalf("the generation prompt carried the internal identifier %q", identifier)
		}
	}
	stored, err := fixture.store.ListLearningQuestions(t.Context(), fixture.userID, view.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != domain.LearningQuestionCount {
		t.Fatalf("stored %d questions, want %d", len(stored), domain.LearningQuestionCount)
	}
	for index, question := range stored {
		if question.Ordinal != index+1 {
			t.Fatalf("question %d carries ordinal %d", index, question.Ordinal)
		}
		if question.UserID != fixture.userID || question.LearningSessionID != view.ID {
			t.Fatalf("question %d is filed under %+v", index, question)
		}
		if len(question.Evidence) == 0 || question.Evidence[0].Snapshot == "" {
			t.Fatalf("question %d lost the snapshot it must be graded against", index)
		}
	}
}

func TestLearningStartNeedsAConversationOfItsOwn(t *testing.T) {
	fixture := newLearningFixture(t, learningQuestions([]int{1}, []int{2}, []int{3}))
	if _, err := fixture.service.Start(t.Context(), fixture.userID, "ses-unknown", "主题", nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("quiz over an unknown conversation = %v, want not-found", err)
	}
	if _, err := fixture.service.Start(t.Context(), fixture.other, fixture.agentChat, "主题", nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("quiz over another user's conversation = %v, want the same answer as a missing one", err)
	}
	for _, topic := range []string{"", "   ", "-", strings.Repeat("长", domain.MaxLearningTopicRunes+1)} {
		if _, err := fixture.service.Start(t.Context(), fixture.userID, fixture.agentChat, topic,
			nil); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("topic %q = %v, want a request refusal", topic, err)
		}
	}
	if fixture.chat.calls != 0 {
		t.Fatalf("%d model calls were made for refusals that should not have reached one", fixture.chat.calls)
	}
}

// TestLearningStartRequiresALearningAgentConversation pins the binding between a quiz and
// the conversation it runs on, and - just as importantly - what a refusal must not do.
//
// A quiz is only read back into a conversation by the agent turn, which is where the
// mastery note is injected; on a plain chat conversation the record would be accurate and
// never used. So the rule is not decoration, and the refusal has to be as cheap as if the
// request had never arrived: no model call, no reservation on the conversation, no change
// to the learner's projection.
func TestLearningStartRequiresALearningAgentConversation(t *testing.T) {
	ctx := t.Context()
	fixture := newLearningFixture(t, learningQuestions([]int{1}, []int{2}, []int{3}))
	// A conversation belonging to the other user that is also a plain chat: it must fail as
	// "not yours", never as "wrong mode", or the refusal would confirm what modes exist in
	// someone else's account.
	foreignChat, err := fixture.store.CreateSession(ctx, &domain.Session{
		UserID: fixture.other, Title: "别人的普通对话", Mode: domain.SessionModeChat,
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := fixture.service.Progress(ctx, fixture.userID, "Redis 持久化"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("progress before anything happened = %v, want not-found", err)
	}
	if _, err := fixture.service.Start(ctx, fixture.userID, fixture.plainChat, "Redis 持久化", nil); !errors.Is(err, ErrLearningSessionMode) {
		t.Fatalf("quiz on the caller's own chat conversation = %v, want the mode refusal", err)
	}
	if fixture.chat.calls != 0 {
		t.Fatalf("%d model calls spent on a request refused before any work", fixture.chat.calls)
	}
	if _, err := fixture.store.GetActiveLearningSession(ctx, fixture.userID, fixture.plainChat); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("a rejected start still reserved the conversation: %v", err)
	}
	if _, err := fixture.service.Progress(ctx, fixture.userID, "Redis 持久化"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the rejected start changed the learner's record: %v", err)
	}

	// The ownership refusals are about a different fact and stay what they were: identical
	// to each other, and not the mode error.
	unknown, err := fixture.service.Start(ctx, fixture.userID, "ses-unknown", "Redis 持久化", nil)
	if !errors.Is(err, ErrNotFound) || errors.Is(err, ErrLearningSessionMode) {
		t.Fatalf("unknown conversation = %v, want plain not-found", err)
	}
	foreign, err := fixture.service.Start(ctx, fixture.userID, foreignChat.ID, "Redis 持久化", nil)
	if !errors.Is(err, ErrNotFound) || errors.Is(err, ErrLearningSessionMode) {
		t.Fatalf("another user's chat conversation = %v, want the same answer as a missing one", err)
	}
	if unknown != nil || foreign != nil {
		t.Fatal("a refused start returned a quiz view")
	}
	if fixture.chat.calls != 0 {
		t.Fatalf("%d model calls after four refusals, want none", fixture.chat.calls)
	}

	// The allowed path is unchanged: an agent conversation gets its quiz and spends exactly
	// one generation call on it.
	view, err := fixture.service.Start(ctx, fixture.userID, fixture.agentChat, "Redis 持久化", nil)
	if err != nil {
		t.Fatal(err)
	}
	if view == nil || view.Question == nil || view.TotalQuestions != domain.LearningQuestionCount {
		t.Fatalf("an agent conversation did not get its quiz: %+v", view)
	}
	if fixture.chat.calls != 1 {
		t.Fatalf("%d model calls for the accepted start, want 1", fixture.chat.calls)
	}
}

func TestLearningStartGivesTheConversationBackWhenItCannotBuildAQuiz(t *testing.T) {
	cases := []struct {
		name    string
		replies []string
		prepare func(*learningFixture)
		want    error
		// recoverable marks the case where the cause can be fixed from outside, which is
		// also the case that proves the reservation was released rather than reused.
		recoverable bool
	}{
		{
			name:    "nothing relevant in the notes",
			replies: []string{learningQuestions([]int{1}, []int{2}, []int{3})},
			prepare: func(f *learningFixture) { f.recall.results = nil },
			want:    ErrLearningNoEvidence, recoverable: true,
		},
		{
			name:    "the provider fails",
			want:    ErrLearningGeneration,
			prepare: func(f *learningFixture) { f.chat.err = errors.New("provider is unavailable") },
		},
		{
			name:    "the output is unusable three times",
			replies: []string{"not json", "still not json", `{"questions":[]}`},
			want:    ErrLearningGeneration,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newLearningFixture(t, testCase.replies...)
			if testCase.prepare != nil {
				testCase.prepare(fixture)
			}
			if _, err := fixture.service.Start(t.Context(), fixture.userID, fixture.agentChat, "Redis 持久化", nil); !errors.Is(err, testCase.want) {
				t.Fatalf("start = %v, want %v", err, testCase.want)
			}
			// The claim is narrow and it is the one that matters: no quiz is left open on
			// the conversation, so the learner is not told about a quiz they never got.
			if _, err := fixture.store.GetActiveLearningSession(t.Context(), fixture.userID,
				fixture.agentChat); !errors.Is(err, repository.ErrNotFound) {
				t.Fatalf("a failed start left an open quiz: %v", err)
			}
			empty, err := fixture.service.Current(t.Context(), fixture.userID, fixture.agentChat)
			if err != nil || empty != nil {
				t.Fatalf("Current after a failed start = (%v, %v), want the empty state, not an error",
					empty, err)
			}
			if !testCase.recoverable {
				return
			}
			fixture.recall.results = learningChunks(8)
			if _, err := fixture.service.Start(t.Context(), fixture.userID, fixture.agentChat, "Redis 持久化", nil); err != nil {
				t.Fatalf("starting again once the cause is gone: %v", err)
			}
		})
	}
}

func TestLearningGenerationRefusesEverythingTheContractForbids(t *testing.T) {
	cases := []struct {
		name   string
		reply  string
		accept string
	}{
		{
			name: "a total field the model invented",
			reply: `{"questions":[{"type":"short_answer","question":"问？","assessment_goal":"目",` +
				`"reference_answer":"答","rubric":["点"],"evidence":[1],"total":90}]}`,
		},
		{
			name:  "the wrong number of questions",
			reply: learningQuestions([]int{1}, []int{2}),
		},
		{
			name:  "an evidence number that was never shown",
			reply: learningQuestions([]int{1}, []int{2}, []int{99}),
		},
		{
			name: "a code question over prose",
			reply: learningGeneratorReply(
				learningQuestionSpec{Type: "short_answer", Question: "第一题？", Goal: "目标一",
					Answer: "答案", Rubric: []string{"点"}, Evidence: []int{1}},
				learningQuestionSpec{Type: "short_answer", Question: "第二题？", Goal: "目标二",
					Answer: "答案", Rubric: []string{"点"}, Evidence: []int{2}},
				learningQuestionSpec{Type: "code_analysis", Question: "这段代码在边界上会怎样？", Goal: "目标三",
					Answer: "答案", Rubric: []string{"点"}, Evidence: []int{3}}),
		},
		{
			name: "an unknown question type",
			reply: learningGeneratorReply(
				learningQuestionSpec{Type: "short_answer", Question: "第一题？", Goal: "目标一",
					Answer: "答案", Rubric: []string{"点"}, Evidence: []int{1}},
				learningQuestionSpec{Type: "multiple_choice", Question: "第二题？", Goal: "目标二",
					Answer: "答案", Rubric: []string{"点"}, Evidence: []int{2}},
				learningQuestionSpec{Type: "short_answer", Question: "第三题？", Goal: "目标三",
					Answer: "答案", Rubric: []string{"点"}, Evidence: []int{3}}),
		},
		{
			name:  "two questions with the same goal",
			reply: learningSameGoalReply(),
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newLearningFixture(t, testCase.reply, testCase.reply, testCase.reply)
			if _, err := fixture.service.Start(t.Context(), fixture.userID, fixture.agentChat, "Redis 持久化", nil); !errors.Is(err, ErrLearningGeneration) {
				t.Fatalf("start = %v, want the generation failure", err)
			}
			if fixture.chat.calls > DefaultLearningGenerationCalls {
				t.Fatalf("%d model calls, want at most %d", fixture.chat.calls, DefaultLearningGenerationCalls)
			}
			if _, err := fixture.store.GetActiveLearningSession(t.Context(), fixture.userID,
				fixture.agentChat); !errors.Is(err, repository.ErrNotFound) {
				t.Fatalf("a refused quiz is still open on the conversation: %v", err)
			}
		})
	}
}

// learningSameGoalReply is a set whose three questions share one assessment goal, which
// the generator contract forbids because it means the same thing is measured three times.
func learningSameGoalReply() string {
	return learningGeneratorReply(
		learningQuestionSpec{Type: "short_answer", Question: "第一题：说清 RDB 的作用？", Goal: "说明持久化取舍",
			Answer: "答案", Rubric: []string{"点"}, Evidence: []int{1}},
		learningQuestionSpec{Type: "concept_comparison", Question: "第二题：比较 RDB 与 AOF 的差异？", Goal: "说明持久化取舍",
			Answer: "答案", Rubric: []string{"点"}, Evidence: []int{2}},
		learningQuestionSpec{Type: "short_answer", Question: "第三题：何时选 AOF？", Goal: "说明持久化取舍",
			Answer: "答案", Rubric: []string{"点"}, Evidence: []int{3}},
	)
}

func TestLearningGenerationTriesAgainAndTellsTheModelWhatItRepeated(t *testing.T) {
	// Repetition is measured against what this learner has already been shown, so the
	// first quiz has to exist before a second one can repeat it.
	seen := learningQuestionsTagged("已出", []int{1, 2}, []int{3}, []int{4})
	fresh := learningQuestionsTagged("新的", []int{5, 6}, []int{7}, []int{8})
	fixture := newLearningFixture(t, seen, seen, seen, fresh)
	if _, err := fixture.service.Start(t.Context(), fixture.userID, fixture.agentChat, "Redis 持久化", nil); err != nil {
		t.Fatal(err)
	}
	second, err := fixture.store.CreateSession(t.Context(), &domain.Session{
		UserID: fixture.userID, Title: "C", Mode: domain.SessionModeLearningAgent,
	})
	if err != nil {
		t.Fatal(err)
	}
	view, err := fixture.service.Start(t.Context(), fixture.userID, second.ID, "Redis 持久化", nil)
	if err != nil {
		t.Fatal(err)
	}
	// One call for the first quiz, then two refusals and one acceptance for the second.
	if fixture.chat.calls != 4 {
		t.Fatalf("%d model calls, want the second quiz's third attempt to succeed", fixture.chat.calls)
	}
	if !strings.Contains(fixture.chat.promptOf(1), "[已出过的题]") {
		t.Fatal("the retry prompt carried no exposure history")
	}
	// The third prompt names the questions that were rejected, which is what makes a
	// retry a different request rather than the same roll again.
	last := fixture.chat.promptOf(fixture.chat.calls - 1)
	if !strings.Contains(last, "[必须避开]") || !strings.Contains(last, "第 1 题") {
		t.Fatalf("the retry prompt did not carry the rejected questions:\n%s", last)
	}
	if view.Status != string(domain.LearningSessionWaitingAnswer) {
		t.Fatalf("status = %q, want the accepted set to have opened the quiz", view.Status)
	}
}

func TestLearningGenerationStopsAfterThreeRepeats(t *testing.T) {
	seen := learningQuestionsTagged("已出", []int{1}, []int{2}, []int{3})
	fixture := newLearningFixture(t, seen, seen, seen, seen)
	if _, err := fixture.service.Start(t.Context(), fixture.userID, fixture.agentChat, "Redis 持久化", nil); err != nil {
		t.Fatal(err)
	}
	other, err := fixture.store.CreateSession(t.Context(), &domain.Session{
		UserID: fixture.userID, Title: "C", Mode: domain.SessionModeLearningAgent,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.Start(t.Context(), fixture.userID, other.ID, "Redis 持久化", nil); !errors.Is(err, ErrLearningGeneration) {
		t.Fatalf("start = %v, want the failure after the retry budget", err)
	}
	if fixture.chat.calls != 1+DefaultLearningGenerationCalls {
		t.Fatalf("%d model calls, want the first quiz's one plus exactly %d retries",
			fixture.chat.calls, DefaultLearningGenerationCalls)
	}
}

func TestLearningAnswerGradesOncePerQuestionAndProjectsMastery(t *testing.T) {
	fixture := newLearningFixture(t,
		append([]string{learningQuestions([]int{1}, []int{2}, []int{3})},
			learningGradeReply(45, 20, 12), learningGradeReply(45, 20, 12), learningGradeReply(45, 20, 12))...)
	view, err := fixture.service.Start(t.Context(), fixture.userID, fixture.agentChat, "Redis 持久化", nil)
	if err != nil {
		t.Fatal(err)
	}
	for index := 1; index <= domain.LearningQuestionCount; index++ {
		current, err := fixture.service.Current(t.Context(), fixture.userID, fixture.agentChat)
		if err != nil {
			t.Fatal(err)
		}
		if current.Question == nil || current.Question.Ordinal != index {
			t.Fatalf("question %d is not the one being asked: %+v", index, current.Question)
		}
		graded, err := fixture.service.Answer(t.Context(), fixture.userID, view.ID,
			fmt.Sprintf("sub-%d", index), index, "两种方式各有侧重：快照恢复快，追加日志丢得少，取舍在恢复点。")
		if err != nil {
			t.Fatal(err)
		}
		// The answer response is a quiz response and nothing more: it says where the quiz
		// stands, and the score the answer caused is read from the progress endpoint. A
		// client that wants the number asks for it; a client that is handed it at the
		// moment of grading reads it as a verdict on the person rather than on the answer.
		wantPosition := index + 1
		wantStatus := domain.LearningSessionWaitingAnswer
		if index == domain.LearningQuestionCount {
			// The last card closes the quiz where it stands: "question 3 of 3, done", not
			// a phantom fourth question.
			wantPosition, wantStatus = index, domain.LearningSessionCompleted
		}
		if graded.CurrentQuestion != wantPosition || graded.Status != string(wantStatus) {
			t.Fatalf("after answer %d the response says %s %d/%d, want %s %d/%d", index,
				graded.Status, graded.CurrentQuestion, graded.TotalQuestions,
				wantStatus, wantPosition, domain.LearningQuestionCount)
		}
		progress, err := fixture.service.Progress(t.Context(), fixture.userID, "Redis 持久化")
		if err != nil {
			t.Fatal(err)
		}
		if progress.ValidCount != index {
			t.Fatalf("projection counts %d attempts, want %d", progress.ValidCount, index)
		}
		// 77 every time, so mastery is 77 and the level stays developing: 85 with three
		// or more valid attempts is what earns "mastered".
		if progress.Mastery != 77 {
			t.Fatalf("mastery = %d, want 77", progress.Mastery)
		}
		// The level is the part that depends on how much has been answered: one or two
		// valid attempts is not a description of a learner whatever the score, and 77 is
		// below the mastered bar, so the third answer is the first that earns
		// "developing".
		wantLevel := domain.TopicProgressInsufficient
		if index == domain.LearningQuestionCount {
			wantLevel = domain.TopicProgressDeveloping
		}
		if progress.Level != string(wantLevel) {
			t.Fatalf("after answer %d level = %q, want %q", index, progress.Level, wantLevel)
		}
	}
	if fixture.chat.calls != 1+domain.LearningQuestionCount {
		t.Fatalf("%d model calls, want one generation and one grading per question", fixture.chat.calls)
	}
	if finished, err := fixture.service.Current(t.Context(), fixture.userID, fixture.agentChat); err != nil ||
		finished != nil {
		t.Fatalf("Current after the quiz finished = (%v, %v), want the empty state: the quiz is over, not lost",
			finished, err)
	}
	progress, err := fixture.service.Progress(t.Context(), fixture.userID, "Redis 持久化")
	if err != nil {
		t.Fatal(err)
	}
	if progress.Mastery != 77 || progress.ValidCount != domain.LearningQuestionCount {
		t.Fatalf("progress = %+v", progress)
	}
	if len(progress.WeakPoints) == 0 {
		t.Fatal("the missing points the grader named did not reach the projection")
	}
}

func TestLearningAnswerIsIdempotentPerSubmission(t *testing.T) {
	fixture := newLearningFixture(t, learningQuestions([]int{1}, []int{2}, []int{3}), learningGradeReply(40, 15, 10))
	view, err := fixture.service.Start(t.Context(), fixture.userID, fixture.agentChat, "Redis 持久化", nil)
	if err != nil {
		t.Fatal(err)
	}
	answer := "两种方式各有侧重：快照恢复快，追加日志丢得少。"
	first, err := fixture.service.Answer(t.Context(), fixture.userID, view.ID, "sub-once", 1, answer)
	if err != nil {
		t.Fatal(err)
	}
	before := fixture.chat.calls
	replay, err := fixture.service.Answer(t.Context(), fixture.userID, view.ID, "sub-once", 1, answer)
	if err != nil {
		t.Fatalf("retried submission: %v", err)
	}
	if fixture.chat.calls != before {
		t.Fatalf("a replayed submission made %d extra model calls, want none", fixture.chat.calls-before)
	}
	// A replay is not a fresh verdict: the client gets back the same response it got the
	// first time, field for field. That is what makes "send it again with the same
	// submission id" safe rather than merely cheap.
	if replayed, err := json.Marshal(replay); err != nil {
		t.Fatal(err)
	} else if original, err := json.Marshal(first); err != nil {
		t.Fatal(err)
	} else if string(replayed) != string(original) {
		t.Fatalf("replay = %s, want the byte-identical first response %s", replayed, original)
	}
	if replay.CurrentQuestion != first.CurrentQuestion {
		t.Fatalf("a replay advanced the quiz from %d to %d", first.CurrentQuestion, replay.CurrentQuestion)
	}
}

func TestLearningAnswerRefusesOutOfOrderAndForeignQuizzes(t *testing.T) {
	fixture := newLearningFixture(t, learningQuestions([]int{1}, []int{2}, []int{3}), learningGradeReply(40, 15, 10))
	view, err := fixture.service.Start(t.Context(), fixture.userID, fixture.agentChat, "Redis 持久化", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.Answer(t.Context(), fixture.userID, view.ID, "sub-two", 2,
		"这是第二题的答案。"); !errors.Is(err, ErrLearningQuestionOrder) {
		t.Fatalf("answering question 2 first = %v, want an ordering refusal", err)
	}
	if _, err := fixture.service.Answer(t.Context(), fixture.other, view.ID, "sub-intruder", 1,
		"别人的答案。"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another user answering = %v, want the answer for a missing quiz", err)
	}
	if _, err := fixture.service.Answer(t.Context(), fixture.userID, "lsn-nonexistent", "sub-x", 1,
		"答案"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown quiz = %v, want not-found", err)
	}
	for _, bad := range []struct{ submission, answer string }{
		{"", "有答案没编号"}, {"sub-ok", "   "},
		{"sub-ok", strings.Repeat("答", domain.MaxLearningAnswerRunes+1)},
		{strings.Repeat("编", 65), "编号太长"},
	} {
		if _, err := fixture.service.Answer(t.Context(), fixture.userID, view.ID, bad.submission, 1,
			bad.answer); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("submission %q / answer %q = %v, want a request refusal",
				clipLearningRunes(bad.submission, 8), clipLearningRunes(bad.answer, 8), err)
		}
	}
	// None of those refusals reached a model.
	if fixture.chat.calls != 1 {
		t.Fatalf("%d model calls, want only the generation one", fixture.chat.calls)
	}
}

func TestLearningGradingFailureKeepsTheQuestionOpen(t *testing.T) {
	// A grader that returns a total of its own has broken the contract, and a total it
	// chose is a number the server cannot re-derive. The answer stays outstanding.
	fixture := newLearningFixture(t, learningQuestions([]int{1}, []int{2}, []int{3}),
		`{"correctness":40,"completeness":15,"clarity":10,"total":65,"feedback":"还行。",`+
			`"missing_points":[],"confidence":0.8}`,
		learningGradeReply(30, 10, 5))
	view, err := fixture.service.Start(t.Context(), fixture.userID, fixture.agentChat, "Redis 持久化", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.Answer(t.Context(), fixture.userID, view.ID, "sub-total", 1,
		"两种方式各有侧重。"); !errors.Is(err, ErrLearningInvalidGrade) {
		t.Fatalf("a grade carrying its own total = %v, want a contract refusal", err)
	}
	held, err := fixture.store.GetLearningSession(t.Context(), fixture.userID, view.ID)
	if err != nil {
		t.Fatal(err)
	}
	if held.Status != domain.LearningSessionWaitingAnswer || held.CurrentQuestion != 1 {
		t.Fatalf("quiz after a failed grading = %+v, want waiting_answer on question 1", held)
	}
	if _, err := fixture.service.Progress(t.Context(), fixture.userID, "Redis 持久化"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a grade that never happened produced a projection: %v", err)
	}
	// The failed submission stays spent, so retrying needs a new one - which is also
	// what keeps a retry distinguishable from a replay.
	if _, err := fixture.service.Answer(t.Context(), fixture.userID, view.ID, "sub-total", 1,
		"两种方式各有侧重。"); !errors.Is(err, ErrLearningSubmissionConflict) {
		t.Fatalf("retry under the same submission = %v, want a refusal to reuse the id", err)
	}
	retried, err := fixture.service.Answer(t.Context(), fixture.userID, view.ID, "sub-again", 1,
		"两种方式各有侧重：快照面向恢复速度。")
	if err != nil {
		t.Fatalf("retry under a new submission: %v", err)
	}
	if retried.CurrentQuestion != 2 {
		t.Fatalf("after the retry the quiz is at %d, want the next question", retried.CurrentQuestion)
	}
	if progress, err := fixture.service.Progress(t.Context(), fixture.userID, "Redis 持久化"); err != nil {
		t.Fatalf("the retry's own projection could not be read: %v", err)
	} else if progress.ValidCount != 1 {
		t.Fatalf("retry counted %d attempts, want the one that was just graded", progress.ValidCount)
	}
}

func TestLearningProviderFailureWhileGradingIsReportedAsGrading(t *testing.T) {
	fixture := newLearningFixture(t, learningQuestions([]int{1}, []int{2}, []int{3}))
	view, err := fixture.service.Start(t.Context(), fixture.userID, fixture.agentChat, "Redis 持久化", nil)
	if err != nil {
		t.Fatal(err)
	}
	fixture.chat.err = errors.New("dial tcp: connection refused")
	if _, err := fixture.service.Answer(t.Context(), fixture.userID, view.ID, "sub-down", 1,
		"两种方式各有侧重。"); !errors.Is(err, ErrLearningGrading) {
		t.Fatalf("grading with the provider down = %v, want a grading failure", err)
	}
	held, err := fixture.store.GetLearningSession(t.Context(), fixture.userID, view.ID)
	if err != nil {
		t.Fatal(err)
	}
	if held.Status != domain.LearningSessionWaitingAnswer || held.CurrentQuestion != 1 {
		t.Fatalf("a provider outage moved the quiz: %+v", held)
	}
}

func TestLearningSkipAdvancesAndContributesNothing(t *testing.T) {
	fixture := newLearningFixture(t, learningQuestions([]int{1}, []int{2}, []int{3}))
	view, err := fixture.service.Start(t.Context(), fixture.userID, fixture.agentChat, "Redis 持久化", nil)
	if err != nil {
		t.Fatal(err)
	}
	skipped, err := fixture.service.Skip(t.Context(), fixture.userID, view.ID, "skip-1")
	if err != nil {
		t.Fatal(err)
	}
	if skipped.CurrentQuestion != 2 {
		t.Fatalf("quiz is at question %d after a skip, want 2", skipped.CurrentQuestion)
	}
	if _, err := fixture.service.Progress(t.Context(), fixture.userID, "Redis 持久化"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("skipping produced progress: %v", err)
	}
	if _, err := fixture.service.Skip(t.Context(), fixture.userID, view.ID, ""); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("skip with no submission = %v, want a request refusal", err)
	}
}

func TestLearningCancelClosesTheQuizAndReleasesTheConversation(t *testing.T) {
	fixture := newLearningFixture(t, learningQuestionsTagged("第一份", []int{1}, []int{2}, []int{3}),
		learningQuestionsTagged("第二份", []int{4}, []int{5}, []int{6}))
	view, err := fixture.service.Start(t.Context(), fixture.userID, fixture.agentChat, "Redis 持久化", nil)
	if err != nil {
		t.Fatal(err)
	}
	closed, err := fixture.service.Cancel(t.Context(), fixture.userID, view.ID)
	if err != nil {
		t.Fatal(err)
	}
	if closed.Status != string(domain.LearningSessionCancelled) {
		t.Fatalf("status = %q, want cancelled", closed.Status)
	}
	// Cancelling something already cancelled is the state the caller wanted, not an
	// error, and it must not move the row again.
	again, err := fixture.service.Cancel(t.Context(), fixture.userID, view.ID)
	if err != nil {
		t.Fatalf("second cancel: %v", err)
	}
	if again.Status != string(domain.LearningSessionCancelled) || again.CurrentQuestion != closed.CurrentQuestion {
		t.Fatalf("second cancel = %+v, want the same closed quiz", again)
	}
	if _, err := fixture.service.Start(t.Context(), fixture.userID, fixture.agentChat, "Redis 持久化", nil); err != nil {
		t.Fatalf("starting again after cancelling: %v", err)
	}
}

func TestLearningExpiredQuizIsClosedOnRead(t *testing.T) {
	fixture := newLearningFixture(t, learningQuestionsTagged("第一份", []int{1}, []int{2}, []int{3}),
		learningQuestionsTagged("第二份", []int{4}, []int{5}, []int{6}))
	// Expiry is a wall-clock rule the store owns, so a row cannot be expired for the
	// reader and current for the writer. The quiz is therefore opened with a deliberately
	// short lifetime and the test waits past it - the assertion is about the transition,
	// not about timer precision, so the margins are generous.
	fixture.service.WithLearningLifetime(80 * time.Millisecond)
	view, err := fixture.service.Start(t.Context(), fixture.userID, fixture.agentChat, "Redis 持久化", nil)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if _, err := fixture.service.Current(t.Context(), fixture.userID, fixture.agentChat); !errors.Is(err, ErrLearningClosed) {
		t.Fatalf("Current past the deadline = %v, want a closed answer", err)
	}
	held, err := fixture.store.GetLearningSession(t.Context(), fixture.userID, view.ID)
	if err != nil {
		t.Fatal(err)
	}
	if held.Status != domain.LearningSessionExpired {
		t.Fatalf("status = %q, want the read to have expired it", held.Status)
	}
	if _, err := fixture.service.Answer(t.Context(), fixture.userID, view.ID, "sub-late", 1,
		"迟到的答案"); !errors.Is(err, ErrLearningClosed) {
		t.Fatalf("answering an expired quiz = %v, want a closed refusal", err)
	}
	if _, err := fixture.service.Start(t.Context(), fixture.userID, fixture.agentChat, "Redis 持久化", nil); err != nil {
		t.Fatalf("starting after an expiry: %v", err)
	}
}

func TestLearningEvidenceScopeIsResolvedNeverWidened(t *testing.T) {
	fixture := newLearningFixture(t, learningQuestions([]int{1}, []int{2}, []int{3}))
	fixture.catalog.collections = []string{"col-1", "col-2"}

	if _, err := fixture.service.Start(t.Context(), fixture.userID, fixture.agentChat, "Redis 持久化", nil); err != nil {
		t.Fatal(err)
	}
	if len(fixture.recall.queries) != 1 {
		t.Fatalf("%d retrieval queries, want 1", len(fixture.recall.queries))
	}
	// With no scope supplied the query is resolved to the caller's own collections
	// rather than sent empty, where a retrieval layer could read it as "no restriction".
	if got := strings.Join(fixture.recall.queries[0].CollectionIDs, ","); got != "col-1,col-2" {
		t.Fatalf("resolved scope = %q, want both of the caller's collections", got)
	}
	if fixture.recall.queries[0].TopK != DefaultLearningCandidateLimit {
		t.Fatalf("candidate TopK = %d, want the wider learning pool %d",
			fixture.recall.queries[0].TopK, DefaultLearningCandidateLimit)
	}
	if fixture.recall.queries[0].UserID != fixture.userID {
		t.Fatalf("retrieval ran as %q, want the authenticated owner", fixture.recall.queries[0].UserID)
	}

	// A supplied scope is honored rather than expanded, and a retrieval failure is
	// reported rather than answered with a fabricated quiz. This second attempt runs on a
	// different learning-agent conversation on purpose: one conversation may hold only one
	// unfinished quiz, and a plain chat conversation is now refused before it reaches the
	// resolver (TestLearningStartRequiresALearningAgentConversation).
	another, err := fixture.store.CreateSession(t.Context(), &domain.Session{
		UserID: fixture.userID, Title: "B2", Mode: domain.SessionModeLearningAgent,
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.recall.queries = nil
	if _, err := fixture.service.Start(t.Context(), fixture.userID, another.ID, "Go 调度", []string{"col-9"}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(fixture.recall.queries[0].CollectionIDs, ","); got != "col-9" {
		t.Fatalf("supplied scope = %q, want it passed through unchanged", got)
	}

	broken := newLearningFixture(t, learningQuestions([]int{1}, []int{2}, []int{3}))
	broken.recall.err = errors.New("index unavailable")
	if _, err := broken.service.Start(t.Context(), broken.userID, broken.agentChat, "Redis 持久化", nil); err == nil {
		t.Fatal("a retrieval failure produced a quiz anyway")
	} else if errors.Is(err, ErrLearningNoEvidence) {
		t.Fatalf("a retrieval failure was reported as an empty corpus: %v", err)
	}
}

func TestLearningProgressNeedsATopicTheLearnerHasPractised(t *testing.T) {
	fixture := newLearningFixture(t, learningQuestions([]int{1}, []int{2}, []int{3}))
	if _, err := fixture.service.Progress(t.Context(), fixture.userID, "从未测过的主题"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("progress on an unpractised topic = %v, want not-found", err)
	}
	if _, err := fixture.service.Progress(t.Context(), "", "任意"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("progress with no identity = %v, want unauthorized", err)
	}
	if _, err := fixture.service.Progress(t.Context(), fixture.userID, "  "); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("progress with no topic = %v, want a request refusal", err)
	}
}

// TestQuizWorkflowRefusesToRunWithoutAChatModel pins the service's own half of the
// "learning needs a real provider" rule.
//
// cmd/gopherlearn only builds this service when the assembly exposes a model, but a check
// that lives in one caller is a convention. Refusing here means a miswired build answers
// "no model, no quiz" instead of panicking on a nil interface - and, critically, refuses
// before it reserves the conversation. A Start that failed halfway would leave an open
// marker on the row and lock the learner out of their own chat session for the rest of
// the quiz's lifetime, which is a worse outcome than the misconfiguration itself.
func TestQuizWorkflowRefusesToRunWithoutAChatModel(t *testing.T) {
	fixture := newLearningFixture(t, learningQuestions([]int{1}, []int{2}, []int{3}))
	modelless := NewLearningService(fixture.store, nil, fixture.recall, fixture.catalog, fixture.catalog)

	if _, err := modelless.Start(t.Context(), fixture.userID, fixture.agentChat, "Redis 持久化", nil); !errors.Is(err, ErrLearningModelNotConfigured) {
		t.Fatalf("start without a model = %v, want the wiring refusal", err)
	}
	if _, err := modelless.Answer(t.Context(), fixture.userID, "quiz-that-does-not-exist", "sub-1", 1, "我的回答"); !errors.Is(err, ErrLearningModelNotConfigured) {
		t.Fatalf("answer without a model = %v, want the same refusal before any lease is taken", err)
	}

	// The refusal left nothing behind: the same conversation is a real, owned one with
	// nothing open on it - the empty state, not an error - and a service that does have a
	// model can start one on it.
	resumed, err := modelless.Current(t.Context(), fixture.userID, fixture.agentChat)
	if err != nil || resumed != nil {
		t.Fatalf("resume after the refusal = (%v, %v), want the empty state", resumed, err)
	}
	if _, err := fixture.service.Start(t.Context(), fixture.userID, fixture.agentChat, "Redis 持久化", nil); err != nil {
		t.Fatalf("a configured service could not start on the conversation the refusal left alone: %v", err)
	}

	// Reading is not model-backed. An empty progress record is the ordinary not-found,
	// not the wiring refusal: nothing here needed a model to answer.
	if _, err := modelless.Progress(t.Context(), fixture.userID, "Redis 持久化"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("progress without a model = %v, want the ordinary not-found for a topic nobody has practised", err)
	}
	if _, err := modelless.Progress(t.Context(), fixture.userID, "  "); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("progress with a blank topic = %v, want a request refusal", err)
	}
}
