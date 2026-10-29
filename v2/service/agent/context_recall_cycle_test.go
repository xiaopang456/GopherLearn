package agent

import (
	"GopherAI/common/memoryrecall"
	"GopherAI/model"
	"context"
	"errors"
	"testing"
	"time"
)

// The resolver runs before Episode recall, because the episode search needs the
// resolved key: a learner who names a topic in words the episode text does not
// contain is reachable through that key and through nothing else.
func TestResolvedTopicKeysReachEpisodeRecallAndTopicMemory(t *testing.T) {
	config := DefaultContextConfig()
	config.EpisodeRecallMode = EpisodeRecallInject
	config.MemoryReadMode = memoryrecall.ModeEpisodeV3Semantic
	var episodeSawKeys, memorySawKeys []string
	builder, err := NewContextBuilder(config, &fakeHistoryReader{}, nil, nil,
		MemoryProviderFunc(func(_ context.Context, input RunContext) (MemoryRecall, error) {
			memorySawKeys = append([]string(nil), input.StrongTopicKeys...)
			return MemoryRecall{Topic: []string{"topic memory"}}, nil
		}), nil)
	if err != nil {
		t.Fatal(err)
	}
	builder.UseTopicKeyResolver(TopicKeyResolverFunc(func(context.Context, RunContext) ([]string, error) {
		return []string{"packetbuffer"}, nil
	}))
	builder.UseEpisodeProvider(EpisodeProviderFunc(func(_ context.Context, input RunContext) ([]model.EpisodeMemory, error) {
		episodeSawKeys = append([]string(nil), input.ProposedTopicKeys...)
		return nil, nil
	}))
	window, err := builder.Build(context.Background(), RunContext{
		UserID: 7, SessionID: "current", Question: "上次在读缓冲上的取舍是什么？", EpisodeRecallReady: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(episodeSawKeys) != 1 || episodeSawKeys[0] != "packetbuffer" {
		t.Fatalf("episode recall saw keys=%v", episodeSawKeys)
	}
	if !containsKey(memorySawKeys, "packetbuffer") {
		t.Fatalf("topic memory read saw keys=%v", memorySawKeys)
	}
	if len(window.ResolvedTopicKeys) != 1 || window.ResolvedTopicKeys[0] != "packetbuffer" {
		t.Fatalf("window keys=%v", window.ResolvedTopicKeys)
	}
}

// D-151: mixed questions need no third retriever. The TopicKeys of the episodes
// that survived the reload are handed to the topic memory read, which is the
// existing chain and stays the chain.
func TestEpisodeTopicKeysStillDriveTopicUserMemory(t *testing.T) {
	config := DefaultContextConfig()
	config.EpisodeRecallMode = EpisodeRecallInject
	config.MemoryReadMode = memoryrecall.ModeEpisodeV3Semantic
	var memorySawKeys []string
	builder, err := NewContextBuilder(config, &fakeHistoryReader{}, nil, nil,
		MemoryProviderFunc(func(_ context.Context, input RunContext) (MemoryRecall, error) {
			memorySawKeys = append([]string(nil), input.StrongTopicKeys...)
			return MemoryRecall{}, nil
		}), nil)
	if err != nil {
		t.Fatal(err)
	}
	episode := testEpisode("episode-1", "切片重叠比例", time.Date(2026, 8, 18, 8, 0, 0, 0, time.UTC))
	episode.TopicKeys = model.StringList{"分块切片"}
	builder.UseEpisodeProvider(EpisodeProviderFunc(func(context.Context, RunContext) ([]model.EpisodeMemory, error) {
		return []model.EpisodeMemory{episode}, nil
	}))
	if _, err := builder.Build(context.Background(), RunContext{
		UserID: 7, SessionID: "current", Question: "我一直拿不准重叠留多少，上次讨论到什么程度？", EpisodeRecallReady: true,
	}); err != nil {
		t.Fatal(err)
	}
	if !containsKey(memorySawKeys, "分块切片") {
		t.Fatalf("episode topic keys did not reach the topic memory read: %v", memorySawKeys)
	}
}

// One turn, one embedding, however many readers want the query vector.
func TestOneTurnBuysAtMostOneQueryEmbedding(t *testing.T) {
	config := DefaultContextConfig()
	config.EpisodeRecallMode = EpisodeRecallInject
	config.MemoryReadMode = memoryrecall.ModeEpisodeV3Semantic
	calls := 0
	embed := func(context.Context, string) ([]float32, error) {
		calls++
		return []float32{1}, nil
	}
	builder, err := NewContextBuilder(config, &fakeHistoryReader{}, nil, nil,
		MemoryProviderFunc(func(ctx context.Context, input RunContext) (MemoryRecall, error) {
			if _, vectorErr := input.RecallCycle.QueryVector(ctx, input.Question, embed); vectorErr != nil {
				return MemoryRecall{}, vectorErr
			}
			return MemoryRecall{}, nil
		}), nil)
	if err != nil {
		t.Fatal(err)
	}
	builder.UseTopicKeyResolver(TopicKeyResolverFunc(func(ctx context.Context, input RunContext) ([]string, error) {
		if _, vectorErr := input.RecallCycle.QueryVector(ctx, input.Question, embed); vectorErr != nil {
			return nil, vectorErr
		}
		return nil, nil
	}))
	builder.UseEpisodeProvider(EpisodeProviderFunc(func(ctx context.Context, input RunContext) ([]model.EpisodeMemory, error) {
		if _, vectorErr := input.RecallCycle.QueryVector(ctx, input.Question, embed); vectorErr != nil {
			return nil, vectorErr
		}
		return nil, nil
	}))
	window, err := builder.Build(context.Background(), RunContext{
		UserID: 7, SessionID: "current", Question: "上次关于缓冲区复用的决定是什么？", EpisodeRecallReady: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || window.RecallEmbeddingCalls != 1 {
		t.Fatalf("provider calls=%d reported=%d", calls, window.RecallEmbeddingCalls)
	}
}

// Losing topic resolution costs personalisation, not the turn.
func TestTopicResolutionFailureDegradesTheTurn(t *testing.T) {
	builder, err := NewContextBuilder(testContextConfig(), &fakeHistoryReader{
		messages: []model.Message{{ID: 1, IsUser: true, Content: "较早的问题"}},
	}, nil, nil, staticMemories{"topic memory"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	builder.UseTopicKeyResolver(TopicKeyResolverFunc(func(context.Context, RunContext) ([]string, error) {
		return nil, errors.New("redis unavailable")
	}))
	window, err := builder.Build(context.Background(), RunContext{UserID: 7, SessionID: "s", Question: "消息边界那个比喻是什么？"})
	if err != nil {
		t.Fatal(err)
	}
	if len(window.ResolvedTopicKeys) != 0 {
		t.Fatalf("failed resolution must contribute nothing: %v", window.ResolvedTopicKeys)
	}
	if len(window.Messages) == 0 {
		t.Fatal("the turn must still be assembled")
	}
}

// The two blocks keep their own budgets, and topic resolution does not enlarge
// either of them.
func TestResolvedKeysDoNotEnlargeTheMemoryBudgets(t *testing.T) {
	config := DefaultContextConfig()
	long := make([]string, 0, 40)
	for i := 0; i < 40; i++ {
		long = append(long, "主题记忆条目，内容足够长以便占用预算，编号"+string(rune('A'+i%26)))
	}
	builder, err := NewContextBuilder(config, &fakeHistoryReader{}, nil, nil,
		MemoryProviderFunc(func(context.Context, RunContext) (MemoryRecall, error) {
			return MemoryRecall{Global: long, Topic: long}, nil
		}), nil)
	if err != nil {
		t.Fatal(err)
	}
	builder.UseTopicKeyResolver(TopicKeyResolverFunc(func(context.Context, RunContext) ([]string, error) {
		return []string{"a", "b", "c"}, nil
	}))
	window, err := builder.Build(context.Background(), RunContext{UserID: 7, SessionID: "s", Question: "讲讲这个"})
	if err != nil {
		t.Fatal(err)
	}
	if window.Budget.GlobalUserMemory > config.MaxGlobalUserMemoryTokens {
		t.Fatalf("global budget exceeded: %+v", window.Budget)
	}
	if window.Budget.TopicUserMemory > config.MaxTopicUserMemoryTokens {
		t.Fatalf("topic budget exceeded: %+v", window.Budget)
	}
	if window.Budget.ProjectedInput > window.Budget.InputTarget {
		t.Fatalf("budget overflow: %+v", window.Budget)
	}
}

// Without a resolver the builder behaves exactly as it did before, which is
// what keeps the maintenance commands and focused tests honest.
func TestBuilderWithoutAResolverIsUnchanged(t *testing.T) {
	var seen []string
	builder, err := NewContextBuilder(testContextConfig(), &fakeHistoryReader{}, EmptyTaskStateProvider{}, topicMastery{},
		MemoryProviderFunc(func(_ context.Context, input RunContext) (MemoryRecall, error) {
			seen = append([]string(nil), input.StrongTopicKeys...)
			return MemoryRecall{}, nil
		}), EmptySummaryProvider{})
	if err != nil {
		t.Fatal(err)
	}
	window, err := builder.Build(context.Background(), RunContext{UserID: 7, SessionID: "s", Question: "讲讲这道题"})
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0] != "rpc-粘包" {
		t.Fatalf("strong topic keys=%v", seen)
	}
	if window.RecallEmbeddingCalls != 0 || len(window.ResolvedTopicKeys) != 0 {
		t.Fatalf("no resolver must mean no resolution: calls=%d keys=%v", window.RecallEmbeddingCalls, window.ResolvedTopicKeys)
	}
}

// A cycle belongs to one turn. Two builds must not share a vector, or one
// learner's question would answer another's.
func TestEachBuildGetsItsOwnCycle(t *testing.T) {
	seen := map[*memoryrecall.Cycle]struct{}{}
	config := testContextConfig()
	config.MemoryReadMode = memoryrecall.ModeEpisodeV3Semantic
	builder, err := NewContextBuilder(config, &fakeHistoryReader{}, nil, nil,
		MemoryProviderFunc(func(_ context.Context, input RunContext) (MemoryRecall, error) {
			seen[input.RecallCycle] = struct{}{}
			return MemoryRecall{}, nil
		}), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, question := range []string{"第一个问题", "第二个问题"} {
		if _, err := builder.Build(context.Background(), RunContext{UserID: 7, SessionID: "s", Question: question}); err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != 2 {
		t.Fatalf("cycles observed=%d", len(seen))
	}
}

func containsKey(keys []string, want string) bool {
	for _, key := range keys {
		if key == want {
			return true
		}
	}
	return false
}

// legacy_v2 is the default, and it must not merely skip the v3 machinery: no
// cycle is created and no resolver is consulted, so a later branch cannot start
// using either by accident.
func TestLegacyModeCreatesNoCycleAndCallsNoResolver(t *testing.T) {
	resolverCalls := 0
	var seenCycle *memoryrecall.Cycle
	seen := false
	config := testContextConfig()
	if config.MemoryReadMode != memoryrecall.ModeLegacyV2 {
		t.Fatalf("the shipped default must be legacy_v2, got %q", config.MemoryReadMode)
	}
	builder, err := NewContextBuilder(config, &fakeHistoryReader{}, nil, nil,
		MemoryProviderFunc(func(_ context.Context, input RunContext) (MemoryRecall, error) {
			seenCycle, seen = input.RecallCycle, true
			return MemoryRecall{}, nil
		}), nil)
	if err != nil {
		t.Fatal(err)
	}
	builder.UseTopicKeyResolver(TopicKeyResolverFunc(func(context.Context, RunContext) ([]string, error) {
		resolverCalls++
		return []string{"不该出现的键"}, nil
	}))
	window, err := builder.Build(context.Background(), RunContext{UserID: 7, SessionID: "s", Question: "上次关于缓冲区复用的决定是什么？"})
	if err != nil {
		t.Fatal(err)
	}
	if !seen {
		t.Fatal("the memory provider was never called")
	}
	if seenCycle != nil {
		t.Fatal("legacy_v2 must not create a request-scoped recall cycle")
	}
	if resolverCalls != 0 {
		t.Fatalf("legacy_v2 consulted a topic resolver %d times", resolverCalls)
	}
	if len(window.ResolvedTopicKeys) != 0 || window.RecallEmbeddingCalls != 0 {
		t.Fatalf("legacy_v2 reported v3 diagnostics: keys=%v calls=%d", window.ResolvedTopicKeys, window.RecallEmbeddingCalls)
	}
}
