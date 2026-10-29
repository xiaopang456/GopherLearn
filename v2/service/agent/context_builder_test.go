package agent

import (
	"GopherAI/common/memoryrecall"
	"GopherAI/common/rag"
	"GopherAI/model"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestAgentSystemPromptUsesAdaptiveAnswerDepth(t *testing.T) {
	if strings.Contains(agentSystemPrompt, "Return a concise") {
		t.Fatal("agent system prompt still forces every answer to be concise")
	}
	for _, required := range []string{"Match the depth to the question", "mechanism", "important boundaries", "explicitly asks for a short answer"} {
		if !strings.Contains(agentSystemPrompt, required) {
			t.Fatalf("agent system prompt is missing adaptive-depth rule %q", required)
		}
	}
}

type fakeHistoryReader struct {
	messages []model.Message
	err      error
	limit    int
	afterID  uint
}

func (f *fakeHistoryReader) ListRecentAfter(_ context.Context, _ string, afterID uint, limit int) ([]model.Message, error) {
	f.limit = limit
	f.afterID = afterID
	filtered := make([]model.Message, 0, len(f.messages))
	for _, message := range f.messages {
		if message.ID > afterID {
			filtered = append(filtered, message)
		}
	}
	return filtered, f.err
}

type staticTaskState string

func (s staticTaskState) CurrentTaskState(context.Context, RunContext) (string, error) {
	return string(s), nil
}

type staticMemories []string

func (s staticMemories) RelevantMemories(context.Context, RunContext) (MemoryRecall, error) {
	return MemoryRecall{Topic: append([]string(nil), s...)}, nil
}

type staticSummary string

func (s staticSummary) SessionSummary(context.Context, RunContext) (SummarySnapshot, error) {
	return SummarySnapshot{Content: string(s)}, nil
}

type failingSummary struct{ snapshot SummarySnapshot }

func (f failingSummary) SessionSummary(context.Context, RunContext) (SummarySnapshot, error) {
	return f.snapshot, context.DeadlineExceeded
}

type failingMemories struct{}

func (failingMemories) RelevantMemories(context.Context, RunContext) (MemoryRecall, error) {
	return MemoryRecall{}, errors.New("memory mysql unavailable")
}

type failingEpisodes struct{}

func (failingEpisodes) RelevantEpisodes(context.Context, RunContext) ([]model.EpisodeMemory, error) {
	return nil, errors.New("episode redis unavailable")
}

func testEpisode(id, title string, at time.Time) model.EpisodeMemory {
	return model.EpisodeMemory{
		ID: id, Version: 1, Title: title, LastMessageAt: at,
		// The session the episode describes, which is not the session asking. A
		// grant that lost it would authorise nothing.
		UserID: 7, SessionID: "episode-source-" + id,
		Document: model.EpisodeDocument{
			Synopsis:        "must not be rendered",
			Topics:          []model.EpisodeTopic{{Key: "rpc-粘包", Label: "RPC 粘包"}},
			Decisions:       []model.EpisodeEvidence{{Text: "使用长度字段拆包", EvidenceMessageIDs: []uint{11}}},
			OpenLoops:       []model.EpisodeEvidence{{Text: "补充半包测试", EvidenceMessageIDs: []uint{13}}},
			UserCorrections: []model.EpisodeEvidence{{Text: "must not be rendered", EvidenceMessageIDs: []uint{15}}},
			SourceHandles:   []string{"session:" + id},
		},
	}
}

func testContextConfig() ContextConfig {
	return ContextConfig{
		ModelWindowTokens: 8192, InputTargetTokens: 2048, OutputReserveTokens: 512,
		EvidenceReserve: 500, MaxGlobalUserMemoryTokens: 90, MaxTopicUserMemoryTokens: 90, MaxSummaryTokens: 180, MaxMasteryTokens: 120,
		HistoryFetchLimit: 64,
		// Mirrors the shipped default, so a test that does not say otherwise is
		// exercising what production runs.
		MemoryReadMode: memoryrecall.DefaultReadMode(),
	}
}

func TestContextBuilderOrdersSectionsAndKeepsCurrentQuestionOnce(t *testing.T) {
	history := &fakeHistoryReader{messages: []model.Message{
		{ID: 1, IsUser: true, Content: "older user"},
		{ID: 2, IsUser: false, Content: "older assistant"},
		{ID: 3, IsUser: true, Content: "current question"}, // persisted before Runner starts
	}}
	builder, err := NewContextBuilder(testContextConfig(), history, staticTaskState("active learning task"), nil, staticMemories{"prefers examples"}, staticSummary("earlier decision"))
	if err != nil {
		t.Fatal(err)
	}
	window, err := builder.Build(context.Background(), RunContext{UserID: 7, SessionID: "session", Question: "current question"})
	if err != nil {
		t.Fatal(err)
	}
	if history.limit != 64 {
		t.Fatalf("history limit = %d", history.limit)
	}
	if len(window.Messages) != 7 {
		t.Fatalf("messages = %d, want system/task/memory/summary/two history/current", len(window.Messages))
	}
	if !strings.Contains(window.Messages[1].Content, "active learning") || !strings.Contains(window.Messages[2].Content, "prefers examples") || !strings.Contains(window.Messages[3].Content, "earlier decision") {
		t.Fatalf("section order is wrong: %#v", window.Messages)
	}
	currentCount := 0
	for _, message := range window.Messages {
		if message.Content == "current question" {
			currentCount++
		}
	}
	if currentCount != 1 {
		t.Fatalf("current question count = %d", currentCount)
	}
	if window.Messages[len(window.Messages)-1].Role != "user" {
		t.Fatalf("last role = %s", window.Messages[len(window.Messages)-1].Role)
	}
	if window.Budget.ProjectedInput > window.Budget.InputTarget || window.Budget.TaskState == 0 || window.Budget.RecentMessages == 0 {
		t.Fatalf("invalid budget breakdown: %+v", window.Budget)
	}
}

func TestContextBuilderInjectsAtMostThreeWholeEpisodesBeforeSummary(t *testing.T) {
	config := DefaultContextConfig()
	config.EpisodeRecallMode = EpisodeRecallInject
	config.MaxEpisodeTokens = 1000
	builder, err := NewContextBuilder(config, &fakeHistoryReader{}, nil, nil, nil, staticSummary("current-session summary"))
	if err != nil {
		t.Fatal(err)
	}
	episodes := []model.EpisodeMemory{
		testEpisode("episode-1", "第一次讨论", time.Date(2026, 8, 1, 8, 0, 0, 0, time.UTC)),
		testEpisode("episode-2", "第二次讨论", time.Date(2026, 8, 2, 8, 0, 0, 0, time.UTC)),
		testEpisode("episode-3", "第三次讨论", time.Date(2026, 8, 3, 8, 0, 0, 0, time.UTC)),
		testEpisode("episode-4", "不得进入", time.Date(2026, 8, 4, 8, 0, 0, 0, time.UTC)),
	}
	builder.UseEpisodeProvider(EpisodeProviderFunc(func(context.Context, RunContext) ([]model.EpisodeMemory, error) {
		return episodes, nil
	}))
	window, err := builder.Build(context.Background(), RunContext{UserID: 7, SessionID: "current", Question: "上次我们决定了什么？", EpisodeRecallReady: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(window.EpisodeIDs) != 3 || window.Budget.EpisodeCount != 3 || window.Budget.EpisodeMemory == 0 {
		t.Fatalf("episode diagnostics=%v budget=%+v", window.EpisodeIDs, window.Budget)
	}
	if len(window.EpisodeSources) != 3 {
		t.Fatalf("episode source closure=%+v", window.EpisodeSources)
	}
	joined := ""
	episodeIndex, summaryIndex := -1, -1
	for index, message := range window.Messages {
		joined += message.Content + "\n"
		if strings.Contains(message.Content, "untrusted historical context") {
			episodeIndex = index
		}
		if strings.Contains(message.Content, "current-session summary") {
			summaryIndex = index
		}
	}
	if episodeIndex < 0 || summaryIndex <= episodeIndex || strings.Contains(joined, "不得进入") || strings.Contains(joined, "must not be rendered") {
		t.Fatalf("episode rendering/order is unsafe: %s", joined)
	}
	// The cited message ids anchor each entry to something checkable and stay.
	if !strings.Contains(joined, "message_ids=11") {
		t.Fatalf("episode provenance missing: %s", joined)
	}
	// The handle list does not. It is verified once at write time against stored
	// data, and the model has no way to fetch anything it names, so injecting it
	// spent more than half the episode block on unreachable strings.
	if strings.Contains(joined, "source_handle") {
		t.Fatalf("storage-only provenance reached the prompt: %s", joined)
	}
	if window.Budget.ProjectedInput > window.Budget.InputTarget {
		t.Fatalf("budget overflow: %+v", window.Budget)
	}
}

func TestEpisodeSelectionCarriesTransitiveVersionedSources(t *testing.T) {
	episode := testEpisode("episode-b", "派生讨论", time.Now())
	episode.Version = 3
	episode.SourceEpisodes = model.EpisodeSourceRefs{{EpisodeID: "episode-a", Version: 2}}
	text, ids, sources, _, err := renderEpisodesWithinBudget([]model.EpisodeMemory{episode}, 1000, false)
	if err != nil {
		t.Fatal(err)
	}
	if text == "" || len(ids) != 1 || ids[0] != "episode-b" {
		t.Fatalf("selection text=%q ids=%v", text, ids)
	}
	want := model.EpisodeSourceRefs{{EpisodeID: "episode-a", Version: 2}, {EpisodeID: "episode-b", Version: 3}}
	if !reflect.DeepEqual(sources, want) {
		t.Fatalf("sources=%+v want=%+v", sources, want)
	}
}

func TestContextBuilderEpisodeFailureAndFlagOffPreserveRecent(t *testing.T) {
	history := &fakeHistoryReader{messages: []model.Message{{ID: 1, IsUser: false, Content: "recent authoritative answer"}}}
	config := DefaultContextConfig()
	config.EpisodeRecallMode = EpisodeRecallInject
	builder, err := NewContextBuilder(config, history, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	builder.UseEpisodeProvider(failingEpisodes{})
	window, err := builder.Build(context.Background(), RunContext{UserID: 7, SessionID: "current", Question: "上次呢？", EpisodeRecallReady: true})
	if err != nil || window.Budget.EpisodeMemory != 0 || window.Budget.RecentMessages == 0 {
		t.Fatalf("degraded window=%+v err=%v", window.Budget, err)
	}

	called := false
	config.EpisodeRecallMode = EpisodeRecallShadow
	offBuilder, err := NewContextBuilder(config, history, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	offBuilder.UseEpisodeProvider(EpisodeProviderFunc(func(context.Context, RunContext) ([]model.EpisodeMemory, error) {
		called = true
		return []model.EpisodeMemory{testEpisode("unexpected", "unexpected", time.Now())}, nil
	}))
	offWindow, err := offBuilder.Build(context.Background(), RunContext{UserID: 7, SessionID: "current", Question: "上次呢？", EpisodeRecallReady: true})
	if err != nil || called || offWindow.Budget.EpisodeMemory != 0 || offWindow.Budget.RecentMessages != window.Budget.RecentMessages {
		t.Fatalf("flag-off regression called=%t budget=%+v err=%v", called, offWindow.Budget, err)
	}
}

func TestContextBuilderTrimsHistoryByTokensAndPreservesRequiredSections(t *testing.T) {
	messages := make([]model.Message, 0, 40)
	for i := 0; i < 40; i++ {
		messages = append(messages, model.Message{ID: uint(i + 1), IsUser: i%2 == 0, Content: strings.Repeat("history-token ", 30)})
	}
	history := &fakeHistoryReader{messages: messages}
	builder, err := NewContextBuilder(testContextConfig(), history, staticTaskState("must keep this task state"), nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	window, err := builder.Build(context.Background(), RunContext{UserID: 1, SessionID: "s", Question: "must keep current question"})
	if err != nil {
		t.Fatal(err)
	}
	if window.Budget.DroppedMessages == 0 {
		t.Fatal("expected older messages to be dropped")
	}
	if window.Budget.ProjectedInput > 2048 {
		t.Fatalf("projected input = %d", window.Budget.ProjectedInput)
	}
	if !strings.Contains(window.Messages[1].Content, "must keep this task state") || window.Messages[len(window.Messages)-1].Content != "must keep current question" {
		t.Fatalf("required context was lost: %#v", window.Messages)
	}
	actual := 0
	for _, message := range window.Messages {
		actual += rag.EstimateTokens(message.Content) + 4
	}
	if actual != window.Budget.Assembled {
		t.Fatalf("assembled = %d, actual = %d", window.Budget.Assembled, actual)
	}
}

func TestContextBuilderRejectsRequiredContentBeyondBudget(t *testing.T) {
	builder, err := NewContextBuilder(testContextConfig(), &fakeHistoryReader{}, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = builder.Build(context.Background(), RunContext{UserID: 1, SessionID: "s", Question: strings.Repeat("超", 3000)})
	if !errors.Is(err, ErrRequiredContextExceedsBudget) {
		t.Fatalf("error = %v", err)
	}
}

func TestContextBuilderTruncatesOneOversizedMostRecentMessage(t *testing.T) {
	history := &fakeHistoryReader{messages: []model.Message{{ID: 1, IsUser: false, Content: strings.Repeat("很长的历史回答", 1000)}}}
	builder, err := NewContextBuilder(testContextConfig(), history, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	window, err := builder.Build(context.Background(), RunContext{UserID: 1, SessionID: "s", Question: "current"})
	if err != nil {
		t.Fatal(err)
	}
	if window.Budget.TruncatedMessages != 1 || window.Budget.RecentMessages == 0 {
		t.Fatalf("budget = %+v", window.Budget)
	}
	if len(window.Messages) != 3 || !strings.HasSuffix(window.Messages[1].Content, "…") {
		t.Fatalf("oversized history was not retained as a bounded prefix: %#v", window.Messages)
	}
}

func TestContextBuilderPropagatesHistoryFailure(t *testing.T) {
	builder, err := NewContextBuilder(testContextConfig(), &fakeHistoryReader{err: errors.New("mysql down")}, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = builder.Build(context.Background(), RunContext{UserID: 1, SessionID: "s", Question: "question"})
	if err == nil || !strings.Contains(err.Error(), "load recent messages") {
		t.Fatalf("error = %v", err)
	}
}

func TestContextBuilderUsesLastSummaryAndRecentWindowWhenCompactionFails(t *testing.T) {
	history := &fakeHistoryReader{messages: []model.Message{
		{ID: 1, IsUser: true, Content: "already summarized"},
		{ID: 8, IsUser: false, Content: "recent answer"},
	}}
	builder, err := NewContextBuilder(testContextConfig(), history, nil, nil, nil, failingSummary{snapshot: SummarySnapshot{Content: "early confirmed decision", CoveredMessageID: 7, Version: 2}})
	if err != nil {
		t.Fatal(err)
	}
	window, err := builder.Build(context.Background(), RunContext{UserID: 1, SessionID: "s", Question: "continue"})
	if err != nil {
		t.Fatal(err)
	}
	if history.afterID != 7 {
		t.Fatalf("history high-water mark=%d", history.afterID)
	}
	joined := ""
	for _, message := range window.Messages {
		joined += message.Content + "\n"
	}
	if !strings.Contains(joined, "early confirmed decision") || !strings.Contains(joined, "recent answer") || strings.Contains(joined, "already summarized") {
		t.Fatalf("degraded context is wrong: %s", joined)
	}
}

func TestContextBuilderDegradesMemoryFailureToRecentMessages(t *testing.T) {
	history := &fakeHistoryReader{messages: []model.Message{{ID: 1, IsUser: false, Content: "recent answer"}}}
	builder, err := NewContextBuilder(testContextConfig(), history, nil, nil, failingMemories{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	window, err := builder.Build(context.Background(), RunContext{UserID: 1, SessionID: "s", Question: "continue"})
	if err != nil {
		t.Fatal(err)
	}
	if window.Budget.TopicUserMemory != 0 || window.Budget.RecentMessages == 0 || len(window.Messages) != 3 {
		t.Fatalf("degraded memory context=%+v messages=%#v", window.Budget, window.Messages)
	}
}

func TestGlobalUserMemoryUsesBudgetInsteadOfFiveEntryCap(t *testing.T) {
	memories := make([]string, 30)
	for i := range memories {
		memories[i] = fmt.Sprintf("preference-%02d", i)
	}
	text, count := renderMemoriesWithinBudget("Global user memories", memories, 2000)
	if count != 30 || !strings.Contains(text, "preference-29") {
		t.Fatalf("count=%d text=%q", count, text)
	}
	oversized := strings.Repeat("x", 300)
	text, count = renderMemoriesWithinBudget("Topic user memories", []string{oversized, "small-complete-entry"}, 60)
	if count != 1 || !strings.Contains(text, "small-complete-entry") || strings.Contains(text, oversized[:20]) {
		t.Fatalf("whole-entry selection count=%d text=%q", count, text)
	}
}

func TestContextBuilderThirtyRoundsRemainBoundedAfterCompaction(t *testing.T) {
	messages := make([]model.Message, 0, 60)
	for i := 1; i <= 60; i++ {
		messages = append(messages, model.Message{ID: uint(i), IsUser: i%2 == 1, Content: strings.Repeat("bounded-history ", 20)})
	}
	history := &fakeHistoryReader{messages: messages}
	provider := summarySnapshotProvider{snapshot: SummarySnapshot{Content: "round 1 decision remains active", CoveredMessageID: 44, Version: 5}}
	builder, err := NewContextBuilder(testContextConfig(), history, nil, nil, nil, provider)
	if err != nil {
		t.Fatal(err)
	}
	window, err := builder.Build(context.Background(), RunContext{UserID: 1, SessionID: "s", Question: "round 30"})
	if err != nil {
		t.Fatal(err)
	}
	if history.afterID != 44 || window.Budget.ProjectedInput > window.Budget.InputTarget || window.Budget.Summary == 0 {
		t.Fatalf("unbounded compacted context: after=%d budget=%+v", history.afterID, window.Budget)
	}
}

type summarySnapshotProvider struct{ snapshot SummarySnapshot }

func (s summarySnapshotProvider) SessionSummary(context.Context, RunContext) (SummarySnapshot, error) {
	return s.snapshot, nil
}

type fakeActiveLearningReader struct {
	session  *model.LearningSession
	question *model.LearningQuestion
	err      error
}

func (f fakeActiveLearningReader) GetActive(context.Context, int64, string) (*model.LearningSession, error) {
	return f.session, f.err
}
func (f fakeActiveLearningReader) GetQuestion(context.Context, int64, string, int) (*model.LearningQuestion, error) {
	return f.question, f.err
}

func TestLearningTaskStateProviderReturnsBoundedStructuredState(t *testing.T) {
	provider, err := NewLearningTaskStateProvider(fakeActiveLearningReader{
		session:  &model.LearningSession{ID: "learning", UserID: 1, SessionID: "chat", Topic: "Redis", Status: model.LearningSessionWaitingAnswer, CurrentQuestion: 2, TotalQuestions: 3, ExpiresAt: time.Now().Add(time.Hour)},
		question: &model.LearningQuestion{Question: "解释 Redis TTL"},
	})
	if err != nil {
		t.Fatal(err)
	}
	state, err := provider.CurrentTaskState(context.Background(), RunContext{UserID: 1, SessionID: "chat"})
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"Redis", "waiting_answer", "2/3", "解释 Redis TTL", "ordinary chat is not an answer"} {
		if !strings.Contains(state, expected) {
			t.Fatalf("state missing %q: %s", expected, state)
		}
	}
}

func TestContextConfigValidation(t *testing.T) {
	valid := DefaultContextConfig()
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	valid.InputTargetTokens = valid.ModelWindowTokens
	if err := valid.Validate(); err == nil {
		t.Fatal("expected output reserve overflow")
	}
}

// AGENT-017: mastery sits between task state and long-term memory, so that a
// graded topic still shapes the answer after everything below it is trimmed.
type staticMastery string

func (s staticMastery) CurrentMastery(context.Context, RunContext) (string, error) {
	return string(s), nil
}

type failingMastery struct{}

func (failingMastery) CurrentMastery(context.Context, RunContext) (string, error) {
	return "", errors.New("topic progress unavailable")
}

type topicMastery struct{}

func (topicMastery) CurrentMastery(context.Context, RunContext) (string, error) {
	return "matched mastery", nil
}
func (topicMastery) CurrentMasteryWithTopics(context.Context, RunContext) (string, []string, error) {
	return "matched mastery", []string{"rpc-粘包"}, nil
}

func TestContextBuilderPassesMatchedMasteryTopicAsStrongMemoryEvidence(t *testing.T) {
	var got []string
	builder, err := NewContextBuilder(testContextConfig(), &fakeHistoryReader{}, EmptyTaskStateProvider{}, topicMastery{}, MemoryProviderFunc(func(_ context.Context, input RunContext) (MemoryRecall, error) {
		got = append([]string(nil), input.StrongTopicKeys...)
		return MemoryRecall{}, nil
	}), EmptySummaryProvider{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := builder.Build(context.Background(), RunContext{UserID: 7, SessionID: "s", Question: "讲讲这道题"}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "rpc-粘包" {
		t.Fatalf("strong topic keys=%v", got)
	}
}

func TestContextBuilderInjectsMasteryBeforeMemory(t *testing.T) {
	history := &fakeHistoryReader{messages: []model.Message{
		{ID: 1, IsUser: true, Content: "older user"},
	}}
	builder, err := NewContextBuilder(testContextConfig(), history,
		staticTaskState("active learning task"),
		staticMastery("mastery instruction"),
		staticMemories{"prefers examples"},
		staticSummary("earlier decision"))
	if err != nil {
		t.Fatal(err)
	}
	window, err := builder.Build(context.Background(), RunContext{UserID: 7, SessionID: "session", Question: "current question"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(window.Messages[1].Content, "active learning") {
		t.Fatalf("task state should stay first: %q", window.Messages[1].Content)
	}
	if !strings.Contains(window.Messages[2].Content, "mastery instruction") {
		t.Fatalf("mastery should follow task state: %q", window.Messages[2].Content)
	}
	if !strings.Contains(window.Messages[3].Content, "prefers examples") {
		t.Fatalf("memory should follow mastery: %q", window.Messages[3].Content)
	}
	if window.Budget.Mastery <= 0 {
		t.Errorf("mastery budget should be reported, got %d", window.Budget.Mastery)
	}
	if window.Budget.Assembled <= window.Budget.Mastery {
		t.Errorf("assembled total should include mastery: %+v", window.Budget)
	}
}

// A TopicProgress outage must not take ordinary chat down with it: mastery
// only decides how to explain, never what the answer is.
func TestContextBuilderDegradesWhenMasteryFails(t *testing.T) {
	history := &fakeHistoryReader{messages: []model.Message{
		{ID: 1, IsUser: true, Content: "older user"},
	}}
	builder, err := NewContextBuilder(testContextConfig(), history, nil, failingMastery{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	window, err := builder.Build(context.Background(), RunContext{UserID: 7, SessionID: "session", Question: "current question"})
	if err != nil {
		t.Fatalf("mastery failure must not fail the build: %v", err)
	}
	if window.Budget.Mastery != 0 {
		t.Errorf("failed mastery should contribute nothing, got %d", window.Budget.Mastery)
	}
	if len(window.Messages) == 0 {
		t.Fatal("expected the run to continue without mastery")
	}
}

func TestContextConfigRejectsOversizedMasteryBudget(t *testing.T) {
	config := testContextConfig()
	config.MaxMasteryTokens = config.InputTargetTokens
	if err := config.Validate(); err == nil {
		t.Fatal("an oversized mastery budget must be rejected")
	}
	config = testContextConfig()
	config.MaxMasteryTokens = -1
	if err := config.Validate(); err == nil {
		t.Fatal("a negative mastery budget must be rejected")
	}
}

func TestSessionContextQuestionReturnsTheEvidenceReserveToHistory(t *testing.T) {
	messages := make([]model.Message, 0, 40)
	for i := 0; i < 40; i++ {
		messages = append(messages, model.Message{ID: uint(i + 1), IsUser: i%2 == 0, Content: strings.Repeat("history-token ", 30)})
	}
	builder, err := NewContextBuilder(testContextConfig(), &fakeHistoryReader{messages: messages}, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	retrieval, err := builder.Build(context.Background(), RunContext{UserID: 7, SessionID: "current", Question: "TCP 粘包是怎么处理的？"})
	if err != nil {
		t.Fatal(err)
	}
	session, err := builder.Build(context.Background(), RunContext{UserID: 7, SessionID: "current", Question: "你还记得刚才确认的结论吗？"})
	if err != nil {
		t.Fatal(err)
	}
	if retrieval.Budget.EvidenceReserved != testContextConfig().EvidenceReserve || session.Budget.EvidenceReserved != 0 {
		t.Fatalf("reserve retrieval=%d session=%d", retrieval.Budget.EvidenceReserved, session.Budget.EvidenceReserved)
	}
	if session.Budget.RecentMessages <= retrieval.Budget.RecentMessages {
		t.Fatalf("the returned reserve did not reach history: session=%d retrieval=%d", session.Budget.RecentMessages, retrieval.Budget.RecentMessages)
	}
	if session.Budget.ProjectedInput > session.Budget.InputTarget {
		t.Fatalf("returning the reserve overshot the input target: %d > %d", session.Budget.ProjectedInput, session.Budget.InputTarget)
	}
}
