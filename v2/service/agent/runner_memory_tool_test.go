package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"GopherAI/common/rag"

	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"
)

// fakeMemoryRegistry builds this run's memory_search over the registry the
// runner put in the RunContext, exactly as the production registry does.
type fakeMemoryRegistry struct {
	build func(*MemoryEvidenceRegistry) []tool.BaseTool
	seen  *MemoryEvidenceRegistry
	calls atomic.Int32
}

func (r *fakeMemoryRegistry) Tools(_ context.Context, run RunContext) ([]tool.BaseTool, error) {
	r.calls.Add(1)
	if run.MemoryEvidence == nil {
		return nil, fmt.Errorf("the run carries no memory evidence registry")
	}
	r.seen = run.MemoryEvidence
	return r.build(run.MemoryEvidence), nil
}

type fakeMemorySearchInput struct {
	Query string `json:"query" jsonschema:"required"`
}

type fakeMemoryCard struct {
	Ref     string `json:"ref"`
	Kind    string `json:"kind"`
	Subject string `json:"subject"`
	Content string `json:"content"`
}

type fakeMemorySearchOutput struct {
	Status  string           `json:"status"`
	Reason  string           `json:"reason"`
	Query   string           `json:"query"`
	Summary string           `json:"summary"`
	Results []fakeMemoryCard `json:"results"`
}

// mustMemorySearchTool builds a memory_search stand-in whose envelope has the
// same shape as the real one, including the results array the shared knowledge
// observation parser looks for.
func mustMemorySearchTool(t *testing.T, evidence *MemoryEvidenceRegistry, executions *atomic.Int32,
	status string, cards []fakeMemoryCard) tool.InvokableTool {
	t.Helper()
	built, err := toolutils.InferTool(memorySearchToolName, "search the learner's long-term memory",
		func(_ context.Context, input *fakeMemorySearchInput) (*fakeMemorySearchOutput, error) {
			executions.Add(1)
			issued := make([]fakeMemoryCard, 0, len(cards))
			refs := make([]MemoryEvidenceRef, 0, len(cards))
			for _, card := range cards {
				kind := MemoryEvidenceTopic
				if card.Kind == "episodic" {
					kind = MemoryEvidenceEpisodic
				}
				ref := evidence.Register(MemoryEvidenceEntry{
					Kind: kind, InternalID: "internal-" + card.Ref, Subject: card.Subject, Owner: 1,
				})
				card.Ref = ref
				issued = append(issued, card)
				refs = append(refs, MemoryEvidenceRef{Ref: ref, Kind: string(kind), InternalID: "internal-" + card.Ref})
			}
			evidence.Record(MemorySearchCall{Status: status, Query: input.Query, Refs: refs})
			return &fakeMemorySearchOutput{Status: status, Reason: "none", Query: input.Query,
				Summary: "memory", Results: issued}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	return built
}

// memoryToolRegistry wires a fake memory_search over whatever registry the run
// supplies, so a test exercises the same sharing production does.
func memoryToolRegistry(t *testing.T, executions *atomic.Int32, status string,
	cards []fakeMemoryCard, extra ...tool.BaseTool) *fakeMemoryRegistry {
	t.Helper()
	return &fakeMemoryRegistry{build: func(evidence *MemoryEvidenceRegistry) []tool.BaseTool {
		return append([]tool.BaseTool{mustMemorySearchTool(t, evidence, executions, status, cards)}, extra...)
	}}
}

func memorySearchCall(id int, query string) *schema.Message {
	return schema.AssistantMessage("", []schema.ToolCall{{
		ID:       fmt.Sprintf("call-%d", id),
		Function: schema.FunctionCall{Name: memorySearchToolName, Arguments: fmt.Sprintf(`{"query":%q}`, query)},
	}})
}

// Only agent_memory_tool_v1 gets the wider budget, and it gets the model
// ceiling with it: six serial tool rounds need six "call a tool" generations
// plus a final answer, so a six/four budget would leave the last three tool
// slots unreachable and the wider ceiling would be a number rather than a
// capability.
func TestAgentMemoryToolBudgetIsSixSevenSixteenAtTheSameDeadline(t *testing.T) {
	config := AgentMemoryToolRunnerConfig()
	if config.MaxToolCalls != 6 || config.MaxModelCalls != 7 || config.MaxGraphSteps != 16 {
		t.Fatalf("budget = %#v", config)
	}
	if config.Deadline != 45*time.Second || config.Deadline != DefaultRunnerConfig().Deadline {
		t.Fatalf("deadline = %s", config.Deadline)
	}
	if config.PerTool != (ToolCallLimits{MemorySearch: 2, KnowledgeSearch: 3, DocumentRead: 2, ProposeMemory: 1}) {
		t.Fatalf("layered limits = %#v", config.PerTool)
	}
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
	// A layered limit above the whole-run total is a budget that cannot be
	// honoured, and it must be refused rather than silently clipped.
	broken := config
	broken.PerTool.MemorySearch = 7
	if err := broken.Validate(); err == nil {
		t.Fatalf("a per-tool limit above the run total was accepted")
	}
}

// A memory card is not a document citation. Before this, the shared observation
// parser saw the results array, filed the search as a knowledge search with no
// usable sources, and sent the turn into the general-knowledge fallback — which
// rebuilds the prompt from the original window and therefore discards the
// evidence the search had just produced.
func TestMemorySearchEvidenceIsNotTreatedAsDocumentGrounding(t *testing.T) {
	var executions atomic.Int32
	fake := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		if call == 1 {
			return memorySearchCall(1, "学习者对消息边界的理解"), nil
		}
		return schema.AssistantMessage("你上次把消息边界理解成水管，我们从那儿接着说。[S1]", nil), nil
	}}
	registry := memoryToolRegistry(t, &executions, "ok", []fakeMemoryCard{
		{Kind: "semantic", Subject: "消息边界", Content: "把 TCP 当成没有刻度的水管"},
	})
	events := newEventCollector()
	runner := mustRunner(t, fake, registry, newMemoryStepRepository(), AgentMemoryToolRunnerConfig())
	result, err := runner.Run(context.Background(), testRunContext(), events)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Answer, "水管") {
		t.Fatalf("answer = %q", result.Answer)
	}
	for _, banner := range []string{noEvidenceFallbackBanner, unavailableFallbackBanner} {
		if strings.Contains(result.Answer, banner) {
			t.Fatalf("a memory answer carried the knowledge-base fallback banner")
		}
	}
	if len(result.Sources) != 0 {
		t.Fatalf("memory evidence became document sources: %#v", result.Sources)
	}
	if result.KnowledgeStatus != "" && result.AnswerBasis == "" {
		t.Fatalf("knowledge state was set by a memory search: %#v", result)
	}
	if events.contains(EventCitation) {
		t.Fatalf("a memory card emitted a citation event")
	}
	if len(result.MemoryEvidence) != 1 || result.MemoryEvidence[0].Refs[0].Ref != "S1" {
		t.Fatalf("memory evidence = %#v", result.MemoryEvidence)
	}
	if len(result.MemoryRefs) != 1 || result.MemoryRefs[0].Kind != MemoryEvidenceTopic {
		t.Fatalf("registry entries = %#v", result.MemoryRefs)
	}
	if result.ModelCalls != 2 || result.ToolCalls != 1 {
		t.Fatalf("a single memory question cost %d model calls and %d tool calls", result.ModelCalls, result.ToolCalls)
	}
}

// The trace is server-side. Nothing in it — the query, the refs, the internal
// ids — may reach an event the client receives.
func TestMemoryEvidenceNeverReachesTheEventStream(t *testing.T) {
	var executions atomic.Int32
	fake := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		if call == 1 {
			return memorySearchCall(1, "非常隐私的检索意图"), nil
		}
		return schema.AssistantMessage("已根据你此前的说法回答。[E1]", nil), nil
	}}
	registry := memoryToolRegistry(t, &executions, "ok", []fakeMemoryCard{
		{Kind: "episodic", Subject: "上次会话", Content: "非常隐私的记忆正文"},
	})
	events := newEventCollector()
	steps := newMemoryStepRepository()
	runner := mustRunner(t, fake, registry, steps, AgentMemoryToolRunnerConfig())
	if _, err := runner.Run(context.Background(), testRunContext(), events); err != nil {
		t.Fatal(err)
	}
	for _, event := range events.events {
		blob, _ := json.Marshal(event)
		for _, secret := range []string{"非常隐私的记忆正文", "internal-E1"} {
			if strings.Contains(string(blob), secret) {
				t.Fatalf("event %s carried %q", event.Type, secret)
			}
		}
	}
	steps.mu.Lock()
	defer steps.mu.Unlock()
	for _, step := range steps.steps {
		if strings.Contains(step.ResultSummary, "非常隐私的记忆正文") || strings.Contains(step.ResultSummary, "internal-E1") {
			t.Fatalf("persisted step carried memory content: %q", step.ResultSummary)
		}
	}
}

// Under this mode the first tool is the model's decision, and that decision is
// the method. Forcing knowledge_search first would answer the question the mode
// exists to ask.
func TestNoToolIsForcedWhenMemorySearchIsRegistered(t *testing.T) {
	var executions atomic.Int32
	fake := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		if call == 1 {
			return memorySearchCall(1, "学习者过去的偏好"), nil
		}
		return schema.AssistantMessage("回答。[S1]", nil), nil
	}}
	registry := memoryToolRegistry(t, &executions, "ok",
		[]fakeMemoryCard{{Kind: "semantic", Subject: "s", Content: "c"}},
		mustNamedEvidenceTool(t, knowledgeSearchToolName, nil))
	runner := mustRunner(t, fake, registry, newMemoryStepRepository(), AgentMemoryToolRunnerConfig())
	if _, err := runner.Run(context.Background(), testRunContext(), newEventCollector()); err != nil {
		t.Fatal(err)
	}
	if fake.forcedToolChoices != 0 {
		t.Fatalf("the runner forced a tool %d times", fake.forcedToolChoices)
	}
}

// A search that ran and found nothing is an answer about this learner. It must
// not be replaced by the knowledge-base refusal, which would report a missing
// note when what is missing is a memory.
func TestEmptyMemorySearchStillAnswers(t *testing.T) {
	var executions atomic.Int32
	fake := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		if call == 1 {
			return memorySearchCall(1, "从未记录过的事"), nil
		}
		return schema.AssistantMessage("你的记忆里没有记录过这件事。", nil), nil
	}}
	registry := memoryToolRegistry(t, &executions, "no_evidence", nil)
	runner := mustRunner(t, fake, registry, newMemoryStepRepository(), AgentMemoryToolRunnerConfig())
	result, err := runner.Run(context.Background(), testRunContext(), newEventCollector())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Answer, "没有记录") {
		t.Fatalf("answer = %q", result.Answer)
	}
	if result.Answer == UnsupportedGroundingAnswer || result.Answer == NoGroundingEvidenceAnswer {
		t.Fatalf("an empty memory search was answered with the knowledge-base refusal")
	}
}

// The layered limit stops one capability from eating the whole-run budget while
// leaving the model a chance to answer from what it already has.
func TestMemorySearchLayeredLimitStopsAtTwoAndStillAnswers(t *testing.T) {
	var executions atomic.Int32
	fake := &scriptedChatModel{
		forbiddenAnswer: "基于已有证据的最终回答",
		respond: func(_ context.Context, call int) (*schema.Message, error) {
			return memorySearchCall(call, fmt.Sprintf("第 %d 次不同的检索", call)), nil
		},
	}
	registry := memoryToolRegistry(t, &executions, "ok",
		[]fakeMemoryCard{{Kind: "semantic", Subject: "s", Content: "c"}})
	runner := mustRunner(t, fake, registry, newMemoryStepRepository(), AgentMemoryToolRunnerConfig())
	_, err := runner.Run(context.Background(), testRunContext(), newEventCollector())
	if err != nil && !errors.Is(err, ErrToolCallLimit) {
		t.Fatal(err)
	}
	if executions.Load() != 2 {
		t.Fatalf("memory_search executed %d times, want 2", executions.Load())
	}
}

// One registry per run, reached by everyone who needs it. The window issues G
// and C references while it is being built, the tool issues S and E while it
// runs, and the final check resolves whatever the answer cited; a registry
// owned by any one of them would be invisible to the other two.
func TestOneEvidenceRegistryIsSharedByTheWholeRun(t *testing.T) {
	var executions atomic.Int32
	fake := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		if call == 1 {
			return memorySearchCall(1, "过去的决定"), nil
		}
		return schema.AssistantMessage("你上次决定用双写迁移。[E1]", nil), nil
	}}
	registry := memoryToolRegistry(t, &executions, "ok",
		[]fakeMemoryCard{{Kind: "episodic", Subject: "分片扩容", Content: "决定用双写迁移"}})
	composite := CompositeToolRegistry{Registries: []ToolRegistry{StaticToolRegistry{}, registry}}
	runner := mustRunner(t, fake, composite, newMemoryStepRepository(), AgentMemoryToolRunnerConfig())
	result, err := runner.Run(context.Background(), testRunContext(), newEventCollector())
	if err != nil {
		t.Fatal(err)
	}
	if registry.seen == nil {
		t.Fatal("the composite did not pass the run's registry to its member")
	}
	if len(result.MemoryRefs) != 1 || result.MemoryRefs[0].Ref != "E1" {
		t.Fatalf("registry entries = %#v", result.MemoryRefs)
	}
	if len(result.MemoryCitation.Cited) != 1 || result.MemoryCitation.Failed() {
		t.Fatalf("citation outcome = %#v", result.MemoryCitation)
	}
}

// A run with no memory tool records nothing and behaves exactly as before.
func TestRunWithoutMemoryToolHasNoTrace(t *testing.T) {
	fake := &scriptedChatModel{respond: func(context.Context, int) (*schema.Message, error) {
		return schema.AssistantMessage("直接回答。", nil), nil
	}}
	runner := mustRunner(t, fake, StaticToolRegistry{}, newMemoryStepRepository(), DefaultRunnerConfig())
	result, err := runner.Run(context.Background(), testRunContext(), newEventCollector())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.MemoryEvidence) != 0 {
		t.Fatalf("memory evidence = %#v", result.MemoryEvidence)
	}
	if result.ModelCalls != 1 {
		t.Fatalf("a question needing no tool cost %d model calls", result.ModelCalls)
	}
}

// A knowledge search that found nothing ends the turn only when the turn has
// nothing else. With memory evidence already gathered, the terminal path would
// abandon the loop, rebuild the prompt from the original window — discarding
// every memory observation — and then tell the learner their notes are missing,
// when what the question needed was their own recorded past.
func TestEmptyKnowledgeSearchDoesNotDiscardMemoryEvidence(t *testing.T) {
	var executions atomic.Int32
	fake := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		switch call {
		case 1:
			return memorySearchCall(1, "学习者过去的理解"), nil
		case 2:
			return schema.AssistantMessage("", []schema.ToolCall{{
				ID:       "call-2",
				Function: schema.FunctionCall{Name: knowledgeSearchToolName, Arguments: `{"query":"消息边界"}`},
			}}), nil
		default:
			return schema.AssistantMessage("你上次把消息边界理解成水管，笔记里没有对应记录。[S1]", nil), nil
		}
	}}
	registry := memoryToolRegistry(t, &executions, "ok",
		[]fakeMemoryCard{{Kind: "semantic", Subject: "消息边界", Content: "把 TCP 当成没有刻度的水管"}},
		mustEmptyKnowledgeTool(t))
	runner := mustRunner(t, fake, registry, newMemoryStepRepository(), AgentMemoryToolRunnerConfig())
	result, err := runner.Run(context.Background(), testRunContext(), newEventCollector())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Answer, "水管") {
		t.Fatalf("the memory-grounded answer was replaced: %q", result.Answer)
	}
	for _, banner := range []string{noEvidenceFallbackBanner, unavailableFallbackBanner} {
		if strings.Contains(result.Answer, banner) {
			t.Fatalf("an empty knowledge search added the fallback banner to a memory answer")
		}
	}
	if result.AnswerBasis == rag.AnswerBasisGeneralKnowledge || result.AnswerBasis == rag.AnswerBasisRefused {
		t.Fatalf("answer basis = %s", result.AnswerBasis)
	}
}

func mustEmptyKnowledgeTool(t *testing.T) tool.InvokableTool {
	t.Helper()
	built, err := toolutils.InferTool(knowledgeSearchToolName, "search the learner's notes",
		func(context.Context, *fakeMemorySearchInput) (*structuredEvidenceOutput, error) {
			return &structuredEvidenceOutput{Status: "no_candidates", Reason: "no_candidate",
				Summary: "个人知识库未找到匹配证据。", Results: []structuredEvidenceResult{}}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	return built
}
