package agent

import (
	"GopherAI/common/rag"
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

// These cases sit at the runner rather than at the HTTP surface on purpose. The
// service validates the policy first, so a test that only goes through the service
// cannot tell whether the runner would have caught it: the runner's own refusal is
// the last thing standing between a value nobody recognises and a turn that reads
// the learner's documents anyway, and a defence that nothing exercises is a defence
// that can be removed silently.

func TestRunnerRefusesAnUnknownPolicyBeforeAnyCall(t *testing.T) {
	var executions atomic.Int32
	fake := &scriptedChatModel{respond: func(_ context.Context, _ int) (*schema.Message, error) {
		return schema.AssistantMessage("final answer", nil), nil
	}}
	runner := mustRunner(t, fake, StaticToolRegistry{Registered: []tool.BaseTool{mustFakeTool(t, &executions)}}, newMemoryStepRepository(), DefaultRunnerConfig())

	input := testRunContext()
	input.KnowledgePolicy = rag.KnowledgePolicy("knowledge_force")
	_, err := runner.Run(context.Background(), input, newEventCollector())
	if err == nil {
		t.Fatal("the runner answered a policy it does not know")
	}
	if !strings.Contains(err.Error(), "knowledge_force") {
		t.Fatalf("the refusal does not name the value it refused: %v", err)
	}
	if fake.CallCount() != 0 {
		t.Fatalf("a refused policy still asked the model %d times", fake.CallCount())
	}
	if executions.Load() != 0 {
		t.Fatalf("a refused policy still ran a tool %d times", executions.Load())
	}
}

// A switched-off turn runs on the path that already existed for a turn with no
// capabilities, not on a duplicated one: taking the tools away instead of adding a
// mode is what keeps the prompt, the evidence reserve, the finalisation rules and
// the retry behaviour single-defined.
//
// The pair is what makes that claim testable. A script that simply answers takes
// one generation whether or not the tools were removed, so such a test passes on
// both paths. The same script is therefore asked for a tool under both policies:
// only the turn with capabilities executes it.
func TestRunnerSwitchedOffTurnCannotRunACapabilityTheModelAsksFor(t *testing.T) {
	for _, tc := range []struct {
		name       string
		policy     rag.KnowledgePolicy
		wantTools  int
		wantModels int
	}{
		{name: "switched off", policy: rag.KnowledgeDisabled, wantTools: 0, wantModels: 2},
		{name: "switched on", policy: rag.KnowledgePreferred, wantTools: 1, wantModels: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var executions atomic.Int32
			fake := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
				if call == 1 {
					return fakeToolCall(1, "topic-1"), nil
				}
				return schema.AssistantMessage("我们接着聊。", nil), nil
			}}
			registry := StaticToolRegistry{Registered: []tool.BaseTool{mustFakeTool(t, &executions)}}
			steps := newMemoryStepRepository()
			runner := mustRunner(t, fake, registry, steps, DefaultRunnerConfig())

			input := testRunContext()
			input.KnowledgePolicy = tc.policy
			result, err := runner.Run(context.Background(), input, newEventCollector())
			if err != nil {
				t.Fatalf("%s: the turn failed instead of answering: %v", tc.name, err)
			}
			if int(executions.Load()) != tc.wantTools {
				t.Fatalf("%s: tool executed %d times, want %d", tc.name, executions.Load(), tc.wantTools)
			}
			if result.ToolCalls != tc.wantTools {
				t.Fatalf("%s: the result reports %d tool calls, want %d", tc.name, result.ToolCalls, tc.wantTools)
			}
			if fake.CallCount() != tc.wantModels {
				t.Fatalf("%s: generations = %d, want %d", tc.name, fake.CallCount(), tc.wantModels)
			}
			if strings.TrimSpace(result.Answer) == "" {
				t.Fatalf("%s: the turn produced no answer text", tc.name)
			}
			for _, recorded := range steps.steps {
				if tc.wantTools == 0 && strings.TrimSpace(recorded.ToolName) != "" {
					t.Fatalf("the ledger names a capability this turn was not given: %s", recorded.ToolName)
				}
			}
		})
	}
}

// The second half of the same promise. A provider that ignores tool_choice=none and
// answers with a tool call every time must not end the turn in failure: the calls
// are dropped and the learner gets readable text, because switching a capability off
// is not the same as cancelling the question.
func TestRunnerCapabilityFreeTurnNeverEndsInFailureOnToolCalls(t *testing.T) {
	var executions atomic.Int32
	fake := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		return fakeToolCall(call, "topic-1"), nil
	}}
	runner := mustRunner(t, fake, StaticToolRegistry{Registered: []tool.BaseTool{mustFakeTool(t, &executions)}}, newMemoryStepRepository(), DefaultRunnerConfig())

	input := testRunContext()
	input.KnowledgePolicy = rag.KnowledgeDisabled
	result, err := runner.Run(context.Background(), input, newEventCollector())
	if err != nil {
		t.Fatalf("a capability-free turn failed the learner out of a provider that ignored tool_choice: %v", err)
	}
	if executions.Load() != 0 {
		t.Fatalf("the turn ran a tool %d times", executions.Load())
	}
	if strings.TrimSpace(result.Answer) == "" {
		t.Fatal("the turn answered with nothing")
	}
	if result.Answer != CapabilityFreeEmptyAnswer {
		t.Fatalf("the turn neither got a plain answer nor used the honest fallback: %q", result.Answer)
	}
}
