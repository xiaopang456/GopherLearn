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

// These two cases sit at the runner rather than at the HTTP surface on purpose.
// The service validates the policy first, so a test that only goes through the
// service cannot tell whether the runner would have caught it: the runner's own
// refusal is the last thing standing between a value nobody recognises and a turn
// that reads the learner's documents anyway, and a defence that nothing exercises
// is a defence that can be removed silently.

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

// A switched-off turn has to run on the path that already existed for a turn with
// no capabilities, not on a duplicated one: the point of taking the tools away
// instead of adding a mode is that the prompt, the evidence reserve, the
// finalisation rules and the retry behaviour stay single-defined.
//
// The pair of cases is what makes that claim testable rather than decorative. The
// same script - one generation that asks for a tool, one that answers - is run
// under both policies, and only the turn without capabilities fails to carry it
// out. A test that scripted a polite answer instead would pass on either path,
// because a ReAct loop with nothing to call also answers in one generation.
func TestRunnerSwitchedOffTurnCannotRunACapabilityTheModelAsksFor(t *testing.T) {
	for _, tc := range []struct {
		name       string
		policy     rag.KnowledgePolicy
		wantErr    bool
		wantTools  int
		wantModels int
	}{
		// The switched-off turn has nothing to answer with once the model reaches
		// for a tool, and it ends as a refusal rather than quietly doing the one
		// thing the learner asked it not to do.
		{name: "switched off", policy: rag.KnowledgeDisabled, wantErr: true, wantTools: 0, wantModels: 1},
		{name: "switched on", policy: rag.KnowledgePreferred, wantErr: false, wantTools: 1, wantModels: 2},
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
			if tc.wantErr && err == nil {
				t.Fatal("a capability-free turn carried out the tool call anyway")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("%s: %v", tc.name, err)
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
			for _, recorded := range steps.steps {
				if tc.wantTools == 0 && strings.TrimSpace(recorded.ToolName) != "" {
					t.Fatalf("the ledger names a capability this turn was not given: %s", recorded.ToolName)
				}
			}
		})
	}
}
