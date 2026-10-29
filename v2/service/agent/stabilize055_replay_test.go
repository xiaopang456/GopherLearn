package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"GopherAI/common/memoryrecall"

	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"
)

// This file replays the six STABILIZE-054 acceptance scenarios with a scripted
// model and no external calls at all. Three of them passed before; three failed
// on tool_call_limit. The point is that all six now finish, and that the three
// that already worked still work.

// scriptedMemorySearch is a memory_search stand-in that answers the way the
// real one does under agent_memory_user_confirmed_v1, and counts how many times
// it was actually entered - which is what "did not reach storage" means here.
type scriptedMemorySearch struct {
	entries atomic.Int32
	mu      sync.Mutex
	kinds   []string
	// episodes is what an episodic search returns when it is allowed to run.
	episodes int
}

type memorySearchArgs struct {
	Query string   `json:"query" jsonschema:"required"`
	Kinds []string `json:"kinds,omitempty"`
	TopK  int      `json:"top_k,omitempty"`
}

type memorySearchReply struct {
	Status    string   `json:"status"`
	Reason    string   `json:"reason"`
	Summary   string   `json:"summary"`
	Results   []string `json:"results"`
	Retryable bool     `json:"retryable"`
}

func (s *scriptedMemorySearch) build(t *testing.T) tool.InvokableTool {
	t.Helper()
	built, err := toolutils.InferTool(memorySearchToolName, "search long-term memory",
		func(_ context.Context, input *memorySearchArgs) (*memorySearchReply, error) {
			s.entries.Add(1)
			s.mu.Lock()
			s.kinds = append(s.kinds, strings.Join(input.Kinds, "+"))
			s.mu.Unlock()
			for _, kind := range input.Kinds {
				if kind == "semantic" {
					// The real refusal under the confirmed mode.
					return &memorySearchReply{Status: "invalid_request", Reason: "semantic_not_permitted",
						Summary: "本轮不能通过 memory_search 读取主题语义记忆。", Results: []string{}, Retryable: false}, nil
				}
			}
			if input.TopK > 5 {
				return &memorySearchReply{Status: "invalid_request", Reason: "top_k_invalid",
					Summary: "top_k 超出范围。", Results: []string{}, Retryable: true}, nil
			}
			if s.episodes == 0 {
				return &memorySearchReply{Status: "no_evidence", Reason: "no_evidence",
					Summary: "没有找到相关历史。", Results: []string{}, Retryable: false}, nil
			}
			return &memorySearchReply{Status: "ok", Reason: "", Summary: "找到历史记录。",
				Results: []string{"episode"}, Retryable: false}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	return built
}

func (s *scriptedMemorySearch) observedKinds() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.kinds...)
}

// replayAcceptance drives one scenario and returns the run result.
func replayAcceptance(t *testing.T, search *scriptedMemorySearch, script func(int) *schema.Message) (RunnerResult, error) {
	t.Helper()
	model := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		return script(call), nil
	}}
	config := AgentMemoryToolRunnerConfig()
	runner := mustRunner(t, model, StaticToolRegistry{Registered: []tool.BaseTool{search.build(t)}},
		newMemoryStepRepository(), config)
	input := testRunContext()
	input.MemoryEvidence = NewMemoryEvidenceRegistry()
	return runner.Run(context.Background(), input, DiscardEvents)
}

func scriptedSearchCall(id, query string, kinds []string, topK int) *schema.Message {
	arguments, _ := json.Marshal(map[string]any{"query": query, "kinds": kinds, "top_k": topK})
	return &schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
		ID: id, Function: schema.FunctionCall{Name: memorySearchToolName, Arguments: string(arguments)},
	}}}
}

// Scenarios 4 and 5: the user picked a topic that cannot support the question.
// The recorded model behaviour was semantic -> episodic(top_k=6) -> corrected
// episodic(top_k=5), and the third call was refused because the two protocol
// rejections had already spent the per-tool budget.
func TestMismatchedTopicScenarioNoLongerHitsTheToolLimit(t *testing.T) {
	search := &scriptedMemorySearch{}
	result, err := replayAcceptance(t, search, func(call int) *schema.Message {
		switch call {
		case 1:
			return scriptedSearchCall("c1", "learner understanding of etcd service discovery",
				[]string{"semantic", "episodic"}, 6)
		case 2:
			return scriptedSearchCall("c2", "what the learner discussed about etcd in earlier sessions",
				[]string{"episodic"}, 6)
		case 3:
			return scriptedSearchCall("c3", "what the learner discussed about etcd in earlier sessions",
				[]string{"episodic"}, 5)
		default:
			return schema.AssistantMessage("我没有找到和这个问题相关的学习记录，下面基于通用知识回答。", nil)
		}
	})
	if err != nil {
		t.Fatalf("the run must not fail: %v", err)
	}
	if result.Answer == "" {
		t.Fatal("the run must produce an answer")
	}
	// The corrected episodic call must have been allowed to reach the tool:
	// blocking it is what the acceptance was punishing.
	kinds := search.observedKinds()
	if len(kinds) < 3 {
		t.Fatalf("the corrected call never reached the tool, kinds=%v", kinds)
	}
	if kinds[2] != "episodic" {
		t.Fatalf("third call kinds = %q", kinds[2])
	}
}

// Scenario 6: the user asked for their learning records but chose "not this
// time". No consent exists, so the semantic path is refused, and the run must
// end with an explanation rather than a tool-limit failure.
func TestNoConsentScenarioEndsWithAnExplanation(t *testing.T) {
	search := &scriptedMemorySearch{}
	result, err := replayAcceptance(t, search, func(call int) *schema.Message {
		if call <= 3 {
			// A model that keeps trying to reach topic memory by rewording.
			return scriptedSearchCall(fmt.Sprintf("c%d", call),
				fmt.Sprintf("learner's own understanding, attempt %d", call), []string{"semantic"}, 5)
		}
		return schema.AssistantMessage("本轮没有获得学习记录授权，我先基于通用知识回答；你可以重新开启并选择主题。", nil)
	})
	if err != nil {
		t.Fatalf("the run must not fail: %v", err)
	}
	if !strings.Contains(result.Answer, "没有获得学习记录授权") {
		t.Fatalf("answer = %q", result.Answer)
	}
	// Rewording must not buy unlimited attempts at a refused capability.
	if entered := search.entries.Load(); entered > int32(noProgressAllowance) {
		t.Fatalf("the refused capability was entered %d times", entered)
	}
}

// The legitimate Episode path must still work: a topic restriction must not
// close history retrieval as well.
func TestEpisodeRetrievalStillSucceeds(t *testing.T) {
	search := &scriptedMemorySearch{episodes: 1}
	result, err := replayAcceptance(t, search, func(call int) *schema.Message {
		if call == 1 {
			return scriptedSearchCall("c1", "what did we decide last time about framing", []string{"episodic"}, 5)
		}
		return schema.AssistantMessage("根据上次的记录，我们当时定了消息边界的处理方式。", nil)
	})
	if err != nil {
		t.Fatalf("episode retrieval must not fail: %v", err)
	}
	if search.entries.Load() != 1 {
		t.Fatalf("the episodic search ran %d times", search.entries.Load())
	}
	if result.Answer == "" {
		t.Fatal("the run must answer from the episode")
	}
}

// Scenarios 1-3 are the ones that already worked. A run that never searches, or
// searches once productively, must be untouched by the ledger.
func TestScenariosThatAlreadyWorkedAreUnchanged(t *testing.T) {
	t.Run("no memory tool at all", func(t *testing.T) {
		search := &scriptedMemorySearch{}
		result, err := replayAcceptance(t, search, func(int) *schema.Message {
			return schema.AssistantMessage("这是一个普通问题的直接回答。", nil)
		})
		if err != nil {
			t.Fatal(err)
		}
		if search.entries.Load() != 0 || result.Answer == "" {
			t.Fatalf("entries=%d answer=%q", search.entries.Load(), result.Answer)
		}
		if len(result.UsedTopicKeys) != 0 {
			t.Fatalf("usedTopicKeys = %v, want empty", result.UsedTopicKeys)
		}
	})
	t.Run("one productive search", func(t *testing.T) {
		search := &scriptedMemorySearch{episodes: 1}
		result, err := replayAcceptance(t, search, func(call int) *schema.Message {
			if call == 1 {
				return scriptedSearchCall("c1", "earlier discussion", []string{"episodic"}, 5)
			}
			return schema.AssistantMessage("结合历史记录回答。", nil)
		})
		if err != nil {
			t.Fatal(err)
		}
		if search.entries.Load() != 1 || result.Answer == "" {
			t.Fatalf("entries=%d answer=%q", search.entries.Load(), result.Answer)
		}
	})
}

// An identical repeat must be answered without entering the tool again, and
// must not fail the run.
func TestIdenticalRepeatIsSuppressedWithoutReachingTheTool(t *testing.T) {
	search := &scriptedMemorySearch{}
	result, err := replayAcceptance(t, search, func(call int) *schema.Message {
		if call <= 3 {
			// Byte-identical every time.
			return scriptedSearchCall(fmt.Sprintf("c%d", call), "the very same question", []string{"episodic"}, 5)
		}
		return schema.AssistantMessage("没有找到相关历史，我直接回答。", nil)
	})
	if err != nil {
		t.Fatalf("a repeated call must not fail the run: %v", err)
	}
	if search.entries.Load() != 1 {
		t.Fatalf("the tool was entered %d times, want exactly 1", search.entries.Load())
	}
	if result.Answer == "" {
		t.Fatal("the run must still answer")
	}
}

// The mode's own guarantees must be untouched by this change.
func TestConfirmedModeGuaranteesUnchanged(t *testing.T) {
	if memoryrecall.DefaultReadMode() != memoryrecall.ModeLegacyV2 {
		t.Fatalf("default = %s", memoryrecall.DefaultReadMode())
	}
	if !memoryrecall.ModeAgentMemoryUserConfirmedV1.RestrictsMemorySearchToEpisodic() {
		t.Fatal("the confirmed mode must still refuse semantic memory_search")
	}
	if !memoryrecall.ModeAgentMemoryUserConfirmedV1.UsesMemorySearchTool() {
		t.Fatal("episode retrieval must stay available")
	}
}
