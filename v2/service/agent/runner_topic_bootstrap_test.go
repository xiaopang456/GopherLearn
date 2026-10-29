package agent

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"GopherAI/model"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

// bootstrapRunner assembles the whole v2 turn: a real ContextBuilder over fake
// providers, the run's shared evidence registry, and a memory_search stand-in
// that sees what the window preinjected.
//
// It exists because the property under test spans three components. The builder
// issues S references, the runner passes the preinjected set to the tools, and
// the tool decides what to hand back; a test of any one of them in isolation
// would assert an interface rather than the behaviour.
func bootstrapRunner(t *testing.T, learning ActiveLearningReader, records []TopicMemoryRecord,
	chat *scriptedChatModel, tools ToolRegistry) *Runner {
	t.Helper()
	mastery := newTestMasteryProvider(t, &stubProgressReader{}, learning)
	builder, err := NewContextBuilder(memoryToolV2ContextConfig(), stubHistory{}, nil, mastery, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	builder.UseGlobalMemoryProvider(GlobalMemoryProviderFunc(
		func(context.Context, RunContext) ([]string, error) {
			return []string{"讲解偏好：先说结论"}, nil
		}))
	builder.UseAuthoritativeTopicMemoryProvider(&recordingTopicBootstrap{records: records})

	runner, err := NewRunner(chat, tools, newMemoryStepRepository(), builder, AgentMemoryToolRunnerConfig())
	if err != nil {
		t.Fatal(err)
	}
	return runner
}

// preinjectionAwareRegistry records what the runner handed it and answers with
// episodic cards only, the way the real tool would once the semantic hits it
// found were all suppressed.
type preinjectionAwareRegistry struct {
	fakeMemoryRegistry
	seenPreinjected atomic.Value
}

func (r *preinjectionAwareRegistry) Tools(ctx context.Context, run RunContext) ([]tool.BaseTool, error) {
	r.seenPreinjected.Store(append([]PreinjectedMemoryRef(nil), run.PreinjectedMemory...))
	return r.fakeMemoryRegistry.Tools(ctx, run)
}

func (r *preinjectionAwareRegistry) preinjected() []PreinjectedMemoryRef {
	value, _ := r.seenPreinjected.Load().([]PreinjectedMemoryRef)
	return value
}

// The whole chain, end to end. An active quiz on 粘包 puts the learner's own
// note in the window with an S reference; the Agent spends its one search on
// what it could not already see - what happened in an earlier session - and the
// answer cites both. The semantic half was never searched for, so no card
// duplicates the note the model was handed.
func TestActiveQuizTurnCitesPreinjectedTopicAndSearchedEpisode(t *testing.T) {
	// The identity has to be the one testRunContext carries, or the learning
	// session belongs to someone else and the bootstrap correctly refuses it.
	learning := &scopedActiveLearning{session: learningSession(1, "session-1", "RPC 粘包",
		model.LearningSessionWaitingAnswer, masteryTestClock.Add(time.Hour))}
	records := []TopicMemoryRecord{
		topicRecord("m-1", "rpc-粘包", "粘包", "把 TCP 当成没有刻度的水管", 1730000000, 1),
	}
	var executions atomic.Int32
	chat := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		if call == 1 {
			return memorySearchCall(1, "上一次会话里关于粘包的决定"), nil
		}
		return schema.AssistantMessage(
			"你把它理解成一根没有刻度的水管[S2]，而上次我们已经决定改用长度前缀[E3]。", nil), nil
	}}
	registry := &preinjectionAwareRegistry{fakeMemoryRegistry: *memoryToolRegistry(t, &executions, "ok",
		[]fakeMemoryCard{{Kind: "episodic", Subject: "粘包", Content: "决定改用长度前缀"}})}

	runner := bootstrapRunner(t, learning, records, chat, registry)

	result, err := runner.Run(context.Background(), testRunContext(), newEventCollector())
	if err != nil {
		t.Fatal(err)
	}
	preinjected := registry.preinjected()
	if len(preinjected) != 1 || preinjected[0].ID != "m-1" || preinjected[0].Ref != "S2" {
		t.Fatalf("the tool was not told what the window already showed: %#v", preinjected)
	}
	if executions.Load() != 1 {
		t.Fatalf("memory_search ran %d times", executions.Load())
	}
	if !strings.Contains(result.Answer, "[S2]") || !strings.Contains(result.Answer, "[E3]") {
		t.Fatalf("answer = %q", result.Answer)
	}
	if result.MemoryCitation.Failed() {
		t.Fatalf("a correct two-kind citation failed the check: %#v", result.MemoryCitation)
	}
	// S1 resolves to the record the server put there, E2 to the one the tool
	// found, and neither stands for the other.
	byRef := map[string]MemoryEvidenceEntry{}
	for _, entry := range result.MemoryRefs {
		byRef[entry.Ref] = entry
	}
	if byRef["S2"].Kind != MemoryEvidenceTopic || byRef["S2"].InternalID != "m-1" {
		t.Fatalf("S2 = %#v", byRef["S2"])
	}
	if !strings.Contains(byRef["S2"].Source, "authoritative_topic_bootstrap") {
		t.Fatalf("S2 does not record that the server injected it: %q", byRef["S2"].Source)
	}
	if byRef["E3"].Kind != MemoryEvidenceEpisodic {
		t.Fatalf("E3 = %#v", byRef["E3"])
	}
	// One search and one answer, inside the budget this mode already had.
	if result.ToolCalls != 1 || result.ModelCalls != 2 {
		t.Fatalf("the turn cost %d tool calls and %d model calls", result.ToolCalls, result.ModelCalls)
	}
}

// The reverse. No active quiz means no preinjected topic block, so a question
// about the learner's understanding still reaches memory_search with semantic
// kinds and still gets an answer out of it. v2 does not take that away.
func TestWithoutAnActiveQuizTheAgentStillSearchesSemanticMemory(t *testing.T) {
	var executions atomic.Int32
	chat := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		if call == 1 {
			return memorySearchCall(1, "学习者对消息边界的理解"), nil
		}
		return schema.AssistantMessage("你之前把消息边界理解成水管[S2]。", nil), nil
	}}
	registry := &preinjectionAwareRegistry{fakeMemoryRegistry: *memoryToolRegistry(t, &executions, "ok",
		[]fakeMemoryCard{{Kind: "semantic", Subject: "消息边界", Content: "把 TCP 当成没有刻度的水管"}})}

	runner := bootstrapRunner(t, &scopedActiveLearning{}, nil, chat, registry)

	result, err := runner.Run(context.Background(), testRunContext(), newEventCollector())
	if err != nil {
		t.Fatal(err)
	}
	if len(registry.preinjected()) != 0 {
		t.Fatalf("a turn with no active quiz preinjected %#v", registry.preinjected())
	}
	if executions.Load() != 1 {
		t.Fatalf("the Agent searched %d times", executions.Load())
	}
	if !strings.Contains(result.Answer, "水管") || result.MemoryCitation.Failed() {
		t.Fatalf("answer = %q outcome = %#v", result.Answer, result.MemoryCitation)
	}
}
