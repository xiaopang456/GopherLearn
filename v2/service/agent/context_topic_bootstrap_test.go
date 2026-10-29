package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"GopherAI/common/memoryrecall"
	"GopherAI/model"
)

func memoryToolV2ContextConfig() ContextConfig {
	config := DefaultContextConfig()
	config.MemoryReadMode = memoryrecall.ModeAgentMemoryToolV2
	config.EpisodeRecallMode = EpisodeRecallInject
	return config
}

// recordingTopicBootstrap is the exact-key reader, recording what it was asked
// for so a test can prove the keys came from server state and nowhere else.
type recordingTopicBootstrap struct {
	mu       sync.Mutex
	calls    atomic.Int32
	lastKeys []string
	records  []TopicMemoryRecord
	err      error
}

func (b *recordingTopicBootstrap) TopicMemoriesByKeys(_ context.Context, _ RunContext,
	keys []string) ([]TopicMemoryRecord, error) {
	b.calls.Add(1)
	b.mu.Lock()
	b.lastKeys = append([]string(nil), keys...)
	b.mu.Unlock()
	if b.err != nil {
		return nil, b.err
	}
	return b.records, nil
}

func (b *recordingTopicBootstrap) keys() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.lastKeys...)
}

func topicRecord(id, topicKey, subject, content string, version uint64, owner int64) TopicMemoryRecord {
	return TopicMemoryRecord{
		ID: id, Version: version, Owner: owner, Status: string(model.LongTermMemoryActive),
		Scope: string(model.MemoryScopeTopic), TopicKey: topicKey, Subject: subject, Content: content,
		Line: fmt.Sprintf("[topic; tags=%s] %s: %s", topicKey, subject, content),
	}
}

// v2 is v1 plus one thing, and this is the one thing: the topic block is in the
// window before the model has read anything, and its lines carry references.
func TestAuthoritativeTopicMemoryIsInjectedBeforeTheFirstModelCall(t *testing.T) {
	learning := &scopedActiveLearning{session: learningSession(11, "s-1", "RPC 粘包",
		model.LearningSessionWaitingAnswer, masteryTestClock.Add(time.Hour))}
	mastery := newTestMasteryProvider(t, &stubProgressReader{}, learning)
	bootstrap := &recordingTopicBootstrap{records: []TopicMemoryRecord{
		topicRecord("m-1", "rpc-粘包", "粘包", "把 TCP 当成没有刻度的水管", 1730000000, 11),
	}}
	provider := &recordingMemoryProvider{
		globalMemory:  []string{"讲解偏好：先说结论"},
		topicMemory:   []string{"这条不该出现"},
		episodeMemory: []model.EpisodeMemory{{ID: "e-1", Title: "复习消息边界", LastMessageAt: masteryTestClock}},
	}
	builder, err := NewContextBuilder(memoryToolV2ContextConfig(), stubHistory{}, nil, mastery, provider, nil)
	if err != nil {
		t.Fatal(err)
	}
	builder.UseEpisodeProvider(provider)
	builder.UseTopicKeyResolver(provider)
	builder.UsePlannedMemoryProvider(provider)
	builder.UseGlobalMemoryProvider(provider)
	builder.UseAuthoritativeTopicMemoryProvider(bootstrap)

	evidence := NewMemoryEvidenceRegistry()
	window, err := builder.Build(context.Background(), RunContext{
		UserID: 11, SessionID: "s-1", Question: "这个我还是没搞懂", EpisodeRecallReady: true,
		MemoryEvidence: evidence,
	})
	if err != nil {
		t.Fatal(err)
	}
	assembled := renderWindow(window)
	if !strings.Contains(assembled, "先说结论") {
		t.Fatalf("global memory was not injected: %s", assembled)
	}
	if !strings.Contains(assembled, "没有刻度的水管") {
		t.Fatalf("the authoritative topic memory was not injected: %s", assembled)
	}
	if strings.Contains(assembled, "这条不该出现") || strings.Contains(assembled, "复习消息边界") {
		t.Fatalf("an older reader reached the window: %s", assembled)
	}
	if got := bootstrap.keys(); len(got) != 1 || got[0] != "rpc-粘包" {
		t.Fatalf("the topic reader was asked for %v", got)
	}
	// Every older reader stays untouched, including the one that would have
	// paid for the retrieval this mode reads by key instead.
	if provider.memoryCalls.Load() != 0 || provider.episodeCalls.Load() != 0 ||
		provider.topicCalls.Load() != 0 || provider.plannedCalls.Load() != 0 {
		t.Fatalf("an older reader ran: %#v", provider)
	}
	if window.RecallEmbeddingCalls != 0 {
		t.Fatalf("the context bought %d embeddings", window.RecallEmbeddingCalls)
	}
	// Episodes are still the Agent's decision, exactly as under v1.
	if window.Budget.EpisodeMemory != 0 || len(window.EpisodeIDs) != 0 || len(window.EpisodeHistoryGrants) != 0 {
		t.Fatalf("v2 auto-injected an episode: %#v %v", window.Budget, window.EpisodeIDs)
	}

	// The reference resolves to the real record, not to a rendered sentence.
	if len(window.PreinjectedMemory) != 1 {
		t.Fatalf("preinjected = %#v", window.PreinjectedMemory)
	}
	preinjected := window.PreinjectedMemory[0]
	if preinjected.Kind != PreinjectedMemoryKindSemantic || preinjected.ID != "m-1" || preinjected.Version != 1730000000 {
		t.Fatalf("preinjected identity = %#v", preinjected)
	}
	entry, err := evidence.Lookup(preinjected.Ref)
	if err != nil {
		t.Fatalf("the issued reference does not resolve: %v", err)
	}
	if entry.Kind != MemoryEvidenceTopic || entry.InternalID != "m-1" || entry.Version != 1730000000 ||
		entry.Owner != 11 || entry.Status != string(model.LongTermMemoryActive) {
		t.Fatalf("registry entry lost the server identity: %#v", entry)
	}
	if !strings.Contains(entry.Source, "rpc-粘包") {
		t.Fatalf("the entry does not record how it reached the window: %q", entry.Source)
	}
	if !strings.Contains(assembled, "["+preinjected.Ref+"]") {
		t.Fatalf("the injected line does not carry its own reference")
	}
	if window.Budget.TopicUserMemory == 0 || window.Budget.GlobalUserMemory == 0 {
		t.Fatalf("the two blocks did not both get budget: %#v", window.Budget)
	}
}

// No active quiz means no authoritative key, and no authoritative key means the
// window is exactly the v1 window. The Agent keeps its full semantic search.
func TestWithoutAnActiveQuizNoTopicBlockIsInjected(t *testing.T) {
	mastery := newTestMasteryProvider(t, &stubProgressReader{}, &scopedActiveLearning{})
	bootstrap := &recordingTopicBootstrap{records: []TopicMemoryRecord{
		topicRecord("m-1", "rpc-粘包", "粘包", "把 TCP 当成没有刻度的水管", 1, 11),
	}}
	builder, err := NewContextBuilder(memoryToolV2ContextConfig(), stubHistory{}, nil, mastery, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	builder.UseGlobalMemoryProvider(&recordingMemoryProvider{globalMemory: []string{"讲解偏好"}})
	builder.UseAuthoritativeTopicMemoryProvider(bootstrap)

	window, err := builder.Build(context.Background(), RunContext{
		UserID: 11, SessionID: "s-1", Question: "我之前是怎么理解消息边界的？",
		MemoryEvidence: NewMemoryEvidenceRegistry(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if bootstrap.calls.Load() != 0 {
		t.Fatal("the topic reader ran with no authoritative key")
	}
	if window.Budget.TopicUserMemory != 0 || len(window.PreinjectedMemory) != 0 {
		t.Fatalf("a topic block appeared without a key: %#v", window.Budget)
	}
	if strings.Contains(renderWindow(window), "没有刻度的水管") {
		t.Fatal("topic memory reached the window with no key naming it")
	}
}

// A mode chosen for a memory behaviour must not serve turns without the wiring
// that behaviour needs. Serving a v2 turn that silently behaved like v1 would
// be measured as v2.
func TestV2RefusesWithoutTheAuthoritativeTopicReader(t *testing.T) {
	builder, err := NewContextBuilder(memoryToolV2ContextConfig(), stubHistory{}, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	builder.UseGlobalMemoryProvider(&recordingMemoryProvider{globalMemory: []string{"讲解偏好"}})
	if _, err := builder.Build(context.Background(), RunContext{
		UserID: 1, SessionID: "s-1", Question: "问题", MemoryEvidence: NewMemoryEvidenceRegistry(),
	}); err == nil {
		t.Fatal("the builder served a v2 turn with no authoritative topic reader")
	}
}

// v1 does not acquire the block by being in the same package as v2.
func TestV1StillInjectsGlobalOnlyEvenWithAReaderAttached(t *testing.T) {
	learning := &scopedActiveLearning{session: learningSession(11, "s-1", "RPC 粘包",
		model.LearningSessionWaitingAnswer, masteryTestClock.Add(time.Hour))}
	mastery := newTestMasteryProvider(t, &stubProgressReader{}, learning)
	bootstrap := &recordingTopicBootstrap{records: []TopicMemoryRecord{
		topicRecord("m-1", "rpc-粘包", "粘包", "把 TCP 当成没有刻度的水管", 1, 11),
	}}
	builder, err := NewContextBuilder(memoryToolContextConfig(), stubHistory{}, nil, mastery, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	builder.UseGlobalMemoryProvider(&recordingMemoryProvider{globalMemory: []string{"讲解偏好"}})
	builder.UseAuthoritativeTopicMemoryProvider(bootstrap)

	window, err := builder.Build(context.Background(), RunContext{
		UserID: 11, SessionID: "s-1", Question: "这个我还是没搞懂",
		MemoryEvidence: NewMemoryEvidenceRegistry(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if bootstrap.calls.Load() != 0 {
		t.Fatal("v1 called the v2 topic reader")
	}
	if window.Budget.TopicUserMemory != 0 || len(window.PreinjectedMemory) != 0 {
		t.Fatalf("v1 grew a preinjected topic block: %#v", window.Budget)
	}
}

// Fail closed on the block, not on the turn, and never onto another method.
func TestTopicBootstrapFailureDropsTheBlockAndKeepsTheTurn(t *testing.T) {
	learning := &scopedActiveLearning{session: learningSession(11, "s-1", "RPC 粘包",
		model.LearningSessionWaitingAnswer, masteryTestClock.Add(time.Hour))}
	mastery := newTestMasteryProvider(t, &stubProgressReader{}, learning)
	provider := &recordingMemoryProvider{
		globalMemory: []string{"讲解偏好"},
		topicMemory:  []string{"这条不该出现"},
		episodeMemory: []model.EpisodeMemory{
			{ID: "e-1", Title: "任何最近的会话", LastMessageAt: masteryTestClock},
		},
	}
	builder, err := NewContextBuilder(memoryToolV2ContextConfig(), stubHistory{}, nil, mastery, provider, nil)
	if err != nil {
		t.Fatal(err)
	}
	builder.UseEpisodeProvider(provider)
	builder.UseGlobalMemoryProvider(provider)
	builder.UseAuthoritativeTopicMemoryProvider(&recordingTopicBootstrap{err: errors.New("memory store down")})

	window, err := builder.Build(context.Background(), RunContext{
		UserID: 11, SessionID: "s-1", Question: "这个我还是没搞懂", EpisodeRecallReady: true,
		MemoryEvidence: NewMemoryEvidenceRegistry(),
	})
	if err != nil {
		t.Fatalf("a degraded topic read failed the turn: %v", err)
	}
	if window.Budget.TopicUserMemory != 0 || len(window.PreinjectedMemory) != 0 {
		t.Fatalf("a failed read still produced a block: %#v", window.Budget)
	}
	if window.Budget.GlobalUserMemory == 0 {
		t.Fatal("a topic failure took global memory down with it")
	}
	// No substitute method takes its place.
	assembled := renderWindow(window)
	if strings.Contains(assembled, "这条不该出现") || strings.Contains(assembled, "任何最近的会话") {
		t.Fatalf("the failure fell back to another reader: %s", assembled)
	}
	if provider.memoryCalls.Load() != 0 || provider.episodeCalls.Load() != 0 {
		t.Fatal("the failure fell back to an older reader")
	}
}

// A reference the model never saw would still resolve, which is a legal
// citation of a record it could not have read. Only lines that fit are issued.
func TestOnlyTopicLinesThatFitTheBudgetAreRegistered(t *testing.T) {
	learning := &scopedActiveLearning{session: learningSession(11, "s-1", "RPC 粘包",
		model.LearningSessionWaitingAnswer, masteryTestClock.Add(time.Hour))}
	mastery := newTestMasteryProvider(t, &stubProgressReader{}, learning)
	huge := strings.Repeat("这是一条很长的主题笔记。", 4000)
	bootstrap := &recordingTopicBootstrap{records: []TopicMemoryRecord{
		topicRecord("m-huge", "rpc-粘包", "太长", huge, 1, 11),
		topicRecord("m-small", "rpc-粘包", "简短", "读半包要靠长度前缀", 2, 11),
	}}
	config := memoryToolV2ContextConfig()
	builder, err := NewContextBuilder(config, stubHistory{}, nil, mastery, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	builder.UseGlobalMemoryProvider(&recordingMemoryProvider{})
	builder.UseAuthoritativeTopicMemoryProvider(bootstrap)

	evidence := NewMemoryEvidenceRegistry()
	window, err := builder.Build(context.Background(), RunContext{
		UserID: 11, SessionID: "s-1", Question: "这个我还是没搞懂", MemoryEvidence: evidence,
	})
	if err != nil {
		t.Fatal(err)
	}
	if window.Budget.TopicUserMemory > config.MaxTopicUserMemoryTokens+4 {
		t.Fatalf("the topic block exceeded its budget: %d", window.Budget.TopicUserMemory)
	}
	assembled := renderWindow(window)
	if !strings.Contains(assembled, "读半包要靠长度前缀") {
		t.Fatal("a short record was dropped because a long one came first")
	}
	// Exactly one record was shown, so exactly one topic reference exists.
	topicRefs := evidence.RefsOfKind(MemoryEvidenceTopic)
	if len(topicRefs) != 1 || len(window.PreinjectedMemory) != 1 {
		t.Fatalf("refs=%v preinjected=%#v", topicRefs, window.PreinjectedMemory)
	}
	for _, entry := range evidence.Entries() {
		if entry.InternalID == "m-huge" {
			t.Fatal("a record the model never saw was given a citable reference")
		}
	}
}

// Two runs in flight must not be able to see each other's keys, records,
// references or budget. Everything this mode adds hangs off the per-run
// RunContext and the per-run registry, and this is the assertion that says so.
func TestConcurrentBuildsShareNoBootstrapState(t *testing.T) {
	const runs = 8
	var wait sync.WaitGroup
	failures := make(chan string, runs)
	for index := 0; index < runs; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			user := int64(100 + index)
			topic := fmt.Sprintf("主题%d", index)
			topicKey := model.NormalizeLearningTopicKey(topic)
			session := fmt.Sprintf("s-%d", index)
			learning := &scopedActiveLearning{session: learningSession(user, session, topic,
				model.LearningSessionWaitingAnswer, masteryTestClock.Add(time.Hour))}
			mastery := newTestMasteryProvider(t, &stubProgressReader{}, learning)
			bootstrap := &recordingTopicBootstrap{records: []TopicMemoryRecord{
				topicRecord(fmt.Sprintf("m-%d", index), topicKey, topic,
					fmt.Sprintf("只属于第 %d 个人的内容", index), uint64(index+1), user),
			}}
			builder, err := NewContextBuilder(memoryToolV2ContextConfig(), stubHistory{}, nil, mastery, nil, nil)
			if err != nil {
				failures <- err.Error()
				return
			}
			builder.UseGlobalMemoryProvider(&recordingMemoryProvider{})
			builder.UseAuthoritativeTopicMemoryProvider(bootstrap)
			evidence := NewMemoryEvidenceRegistry()
			window, err := builder.Build(context.Background(), RunContext{
				UserID: user, SessionID: session, Question: "这个我还是没搞懂", MemoryEvidence: evidence,
			})
			if err != nil {
				failures <- err.Error()
				return
			}
			assembled := renderWindow(window)
			for other := 0; other < runs; other++ {
				if other == index {
					continue
				}
				if strings.Contains(assembled, fmt.Sprintf("只属于第 %d 个人的内容", other)) {
					failures <- fmt.Sprintf("run %d saw run %d", index, other)
					return
				}
			}
			if len(window.PreinjectedMemory) != 1 || window.PreinjectedMemory[0].Ref != "S1" {
				failures <- fmt.Sprintf("run %d refs = %#v", index, window.PreinjectedMemory)
				return
			}
			if entry, lookupErr := evidence.Lookup("S1"); lookupErr != nil || entry.Owner != user {
				failures <- fmt.Sprintf("run %d resolved S1 to owner %d", index, entry.Owner)
			}
		}(index)
	}
	wait.Wait()
	close(failures)
	for failure := range failures {
		t.Fatal(failure)
	}
}

// The v2 prompt is the v1 prompt plus its own paragraph, and the budget is
// computed on the prompt the turn actually carries.
func TestV2PromptIsV1PlusTheBootstrapBoundary(t *testing.T) {
	v1 := SystemPromptFor(memoryrecall.ModeAgentMemoryToolV1)
	v2 := SystemPromptFor(memoryrecall.ModeAgentMemoryToolV2)
	if v1 == v2 || !strings.HasPrefix(v2, v1) {
		t.Fatal("the v2 prompt is not the v1 prompt plus its own rules")
	}
	for _, phrase := range []string{"[S#]", "memory_search", "[E#]"} {
		if !strings.Contains(v2[len(v1):], phrase) {
			t.Fatalf("the v2 addendum does not draw the %s boundary", phrase)
		}
	}
	if strings.Contains(v1, "[S#]") {
		t.Fatal("the v1 prompt acquired the v2 addendum")
	}

	mastery := newTestMasteryProvider(t, &stubProgressReader{}, &scopedActiveLearning{})
	builder, err := NewContextBuilder(memoryToolV2ContextConfig(), stubHistory{}, nil, mastery, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	builder.UseGlobalMemoryProvider(&recordingMemoryProvider{})
	builder.UseAuthoritativeTopicMemoryProvider(&recordingTopicBootstrap{})
	window, err := builder.Build(context.Background(), RunContext{
		UserID: 11, SessionID: "s-1", Question: "问题", MemoryEvidence: NewMemoryEvidenceRegistry(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if window.Budget.System != messageTokenCost(v2) || window.Messages[0].Content != v2 {
		t.Fatalf("the window carries a prompt the budget was not computed for: %d", window.Budget.System)
	}
}

// The clause STABILIZE-039 added to SearchedAndFoundNothing must be an identity
// under every earlier mode: there, S and E are issued only by memory_search and
// never without landing in that call's Refs.
func TestSearchedAndFoundNothingIsUnchangedWhenOnlyTheToolIssuesEvidence(t *testing.T) {
	empty := NewMemoryEvidenceRegistry()
	if empty.SearchedAndFoundNothing() {
		t.Fatal("a run with no search reported an empty search")
	}
	searchedEmpty := NewMemoryEvidenceRegistry()
	searchedEmpty.Record(MemorySearchCall{Status: "no_evidence"})
	if !searchedEmpty.SearchedAndFoundNothing() {
		t.Fatal("a search that returned nothing was not reported as empty")
	}
	found := NewMemoryEvidenceRegistry()
	ref := found.Register(MemoryEvidenceEntry{Kind: MemoryEvidenceTopic, InternalID: "m-1"})
	found.Record(MemorySearchCall{Status: "ok", Refs: []MemoryEvidenceRef{{Ref: ref, InternalID: "m-1"}}})
	if found.SearchedAndFoundNothing() {
		t.Fatal("a search that returned a card was reported as empty")
	}

	// The v2 case: the window already holds topic evidence, and a search that
	// legitimately returned nothing new must not turn a correct [S1] citation
	// into "cited evidence that does not exist".
	preinjected := NewMemoryEvidenceRegistry()
	preinjected.Register(MemoryEvidenceEntry{Kind: MemoryEvidenceTopic, InternalID: "m-1",
		Source: "authoritative_topic_bootstrap:rpc-粘包"})
	preinjected.Record(MemorySearchCall{Status: "no_evidence", Reason: "already_in_context"})
	if preinjected.SearchedAndFoundNothing() {
		t.Fatal("a preinjected topic record was treated as no evidence at all")
	}
	if outcome := CheckMemoryCitations(preinjected, "你把它理解成一根没有刻度的水管。[S1]"); outcome.Failed() {
		t.Fatalf("a correct citation of preinjected memory failed the check: %#v", outcome)
	}
}
