package agent

import (
	"context"
	"encoding/json"
	"testing"

	"GopherAI/common/memoryrecall"
	"GopherAI/common/rag"

	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"
)

// STABILIZE-056.
//
// Under agent_memory_user_confirmed_v1 three of the six STABILIZE-055 browser
// scenarios came back refused with knowledgeReason=search_required, having made
// no tool call at all. The recorded outcome is itself the proof of two of the
// three inputs: reaching search_required requires knowledgeSearchAvailable and
// requiresGrounding to both be true, so knowledge_search was bound and the
// questions did need grounding. The first tool was still never forced, and the
// only branch that can return "" ahead of the knowledge branch on a turn with
// zero tool calls was the memory_search one - a rule written for the four
// agent_memory_tool identities, where the model's first tool choice is the
// thing being measured, applied to a mode where it is not.
//
// These tests pin the corrected boundary and everything it must not move.

// userConfirmedRunnerConfig is the run envelope of the user-confirmed mode:
// agent_memory_tool_v1's budget, unchanged, plus the knowledge-first rule.
func userConfirmedRunnerConfig() RunnerConfig {
	config := AgentMemoryToolRunnerConfig()
	config.ForcesKnowledgeSearchWithMemoryTool = true
	return config
}

// topicWindowBuilder stands in for a window a verified consent produced. It
// carries the confirmed topic keys and nothing else; the consent chain itself
// is unchanged by STABILIZE-056 and is tested where it lives.
type topicWindowBuilder struct{ used []string }

func (b topicWindowBuilder) Build(ctx context.Context, input RunContext) (ContextWindow, error) {
	window, err := MinimalContextBuilder{}.Build(ctx, input)
	if err != nil {
		return ContextWindow{}, err
	}
	window.UsedTopicKeys = append([]string(nil), b.used...)
	return window, nil
}

type trackedKnowledgeSearch struct{ calls int }

func (k *trackedKnowledgeSearch) build(t *testing.T, results []structuredEvidenceResult) tool.InvokableTool {
	t.Helper()
	built, err := toolutils.InferTool(knowledgeSearchToolName, "search the learner's documents",
		func(context.Context, *struct{}) (*structuredEvidenceOutput, error) {
			k.calls++
			return &structuredEvidenceOutput{Summary: "evidence", Results: results}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	return built
}

func groundedResults() []structuredEvidenceResult {
	return []structuredEvidenceResult{{
		SourceID: "rpc-note:v1:chunk-1",
		Citation: 1, DocumentID: "rpc-note", DocumentVersion: 1, DocumentName: "RPC.md",
		ChunkID: "chunk-1", StartLine: 1, EndLine: 8,
		Content: "TCP 是面向字节流的协议，应用层必须自己划定消息边界。",
	}}
}

// runConfirmedMode drives one turn in the user-confirmed envelope.
func runConfirmedMode(t *testing.T, question string, tools []tool.BaseTool, builder ContextBuilder,
	policy rag.KnowledgePolicy, script func(int) *schema.Message) (*scriptedChatModel, RunnerResult) {
	t.Helper()
	fake := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		return script(call), nil
	}}
	runner, err := NewRunner(fake, StaticToolRegistry{Registered: tools}, newMemoryStepRepository(),
		builder, userConfirmedRunnerConfig())
	if err != nil {
		t.Fatal(err)
	}
	input := testRunContext()
	input.Question = question
	input.KnowledgePolicy = policy
	input.MemoryEvidence = NewMemoryEvidenceRegistry()
	result, err := runner.Run(context.Background(), input, DiscardEvents)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	return fake, result
}

// knowledgeCall is the tool call a model makes when the forced binding leaves
// it knowledge_search and nothing else.
func knowledgeCall(id string) *schema.Message {
	return &schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
		ID: id, Function: schema.FunctionCall{Name: knowledgeSearchToolName, Arguments: "{}"},
	}}}
}

// searchThenAnswer is the ordinary shape: the forced search, then the answer.
func searchThenAnswer(call int) *schema.Message {
	if call == 1 {
		return knowledgeCall("k1")
	}
	return citedAnswer(call)
}

func citedAnswer(int) *schema.Message {
	return schema.AssistantMessage("TCP 面向字节流，消息边界要在应用层自己划 [1]。", nil)
}

// STABILIZE-062 found this at the real-provider boundary: the explicit-source
// registry exposed search_uploaded_documents, while forcedToolName still
// returned the legacy knowledge_search constant. react rejected that forced
// choice before the first model request, so a routing result looked like a
// model abstention even though the model never ran.
func TestKnowledgeForceUsesTheRegisteredSurfaceName(t *testing.T) {
	for _, test := range []struct {
		name   string
		config RunnerConfig
		want   string
	}{
		{"existing explicit-actions surface stays unchanged", AgentMemoryExplicitActionsV1RunnerConfig(), knowledgeSearchToolName},
		{"explicit-source surface uses its registered name", AgentMemoryExplicitSourcesV1RunnerConfig(), SearchUploadedDocumentsToolName},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := &runState{
				config:                   test.config,
				input:                    RunContext{Question: "解释一下 TCP 粘包为什么需要应用层划分消息边界"},
				knowledgeSearchAvailable: true,
				memorySearchAvailable:    true,
			}
			if got := state.forcedToolName(); got != test.want {
				t.Fatalf("forced tool = %q, want the registered surface name %q", got, test.want)
			}
		})
	}
}

// The zero-call diagnosis, recomputed rather than asserted from memory: the
// three refused questions are ordinary grounding questions, not conversation
// questions and not history recall. Nothing about them explains the refusal.
func TestRefusedScenarioQuestionsAreOrdinaryGroundingQuestions(t *testing.T) {
	questions := []string{
		"TCP 粘包到底是什么？请结合我的学习记录讲。",
		"RPC 粘包处理和 Codec 注册表之间是怎么配合的？请结合我的学习记录。",
		"Go 里 defer 的执行顺序是怎样的？",
	}
	for _, question := range questions {
		if !requiresGrounding(question) {
			t.Errorf("requiresGrounding(%q) = false; search_required could not have been reached", question)
		}
		if isSessionContextQuestion(question) {
			t.Errorf("%q must not read as a conversation question", question)
		}
		if isHistoryRecallQuestion(question) {
			t.Errorf("%q must not read as history recall", question)
		}
		if !documentKnowledgeReachable(question) {
			t.Errorf("knowledge tools must be registered for %q", question)
		}
	}
}

// 1. The mode boundary itself: memory_search being available no longer
// suppresses the knowledge-first rule.
func TestUserConfirmedModeForcesKnowledgeSearchFirst(t *testing.T) {
	search := &trackedKnowledgeSearch{}
	memory := &scriptedMemorySearch{}
	fake, result := runConfirmedMode(t, "Go 里 defer 的执行顺序是怎样的？",
		[]tool.BaseTool{memory.build(t), search.build(t, groundedResults())},
		MinimalContextBuilder{}, rag.KnowledgePreferred, searchThenAnswer)

	if fake.ForcedToolChoiceCount() == 0 || !fake.HasExclusiveToolBinding(knowledgeSearchToolName) {
		t.Fatalf("first tool bindings = %v, want an exclusive knowledge_search", fake.BoundToolCalls())
	}
	if search.calls != 1 {
		t.Fatalf("knowledge_search ran %d times", search.calls)
	}
	if result.AnswerBasis == rag.AnswerBasisRefused || result.KnowledgeReason == rag.KnowledgeReasonSearchRequired {
		t.Fatalf("the turn was still refused: basis=%s reason=%s", result.AnswerBasis, result.KnowledgeReason)
	}
}

// 2 and 3. A confirmed topic personalises the answer; it does not stand in for
// technical evidence, and the receipt still reports exactly what was confirmed.
func TestConfirmedTopicsStillReceiptExactlyWhileKnowledgeSearchRuns(t *testing.T) {
	for name, confirmed := range map[string][]string{
		"single": {"tcp粘包"},
		"double": {"codec-注册表", "rpc粘包处理"},
	} {
		t.Run(name, func(t *testing.T) {
			search := &trackedKnowledgeSearch{}
			memory := &scriptedMemorySearch{}
			_, result := runConfirmedMode(t, "TCP 粘包到底是什么？请结合我的学习记录讲。",
				[]tool.BaseTool{memory.build(t), search.build(t, groundedResults())},
				topicWindowBuilder{used: confirmed}, rag.KnowledgePreferred, searchThenAnswer)

			if search.calls != 1 {
				t.Fatalf("knowledge_search ran %d times", search.calls)
			}
			if len(result.UsedTopicKeys) != len(confirmed) {
				t.Fatalf("usedTopicKeys = %v, want %v", result.UsedTopicKeys, confirmed)
			}
			for i, key := range confirmed {
				if result.UsedTopicKeys[i] != key {
					t.Fatalf("usedTopicKeys = %v, want %v", result.UsedTopicKeys, confirmed)
				}
			}
			if result.AnswerBasis == rag.AnswerBasisRefused {
				t.Fatalf("basis = %s", result.AnswerBasis)
			}
		})
	}
}

// 4. No topic confirmed at all: an ordinary technical question still searches.
func TestOrdinaryTechnicalQuestionWithoutTopicsStillSearches(t *testing.T) {
	search := &trackedKnowledgeSearch{}
	memory := &scriptedMemorySearch{}
	_, result := runConfirmedMode(t, "RPC 粘包处理和 Codec 注册表之间是怎么配合的？",
		[]tool.BaseTool{memory.build(t), search.build(t, groundedResults())},
		MinimalContextBuilder{}, rag.KnowledgePreferred, searchThenAnswer)

	if search.calls != 1 {
		t.Fatalf("knowledge_search ran %d times", search.calls)
	}
	if len(result.UsedTopicKeys) != 0 {
		t.Fatalf("usedTopicKeys = %v, want none", result.UsedTopicKeys)
	}
}

// 5. A question about the learner's own past must not be sent to the document
// index. The model reaches Episodes and nothing is forced.
func TestPureHistoryQuestionIsNotForcedIntoKnowledgeSearch(t *testing.T) {
	search := &trackedKnowledgeSearch{}
	memory := &scriptedMemorySearch{episodes: 1}
	arguments, _ := json.Marshal(map[string]any{
		"query": "what the learner decided about framing", "kinds": []string{"episodic"}, "top_k": 5})
	fake, result := runConfirmedMode(t, "我上次是怎么决定用固定头部方案的？",
		[]tool.BaseTool{memory.build(t), search.build(t, groundedResults())},
		MinimalContextBuilder{}, rag.KnowledgePreferred, func(call int) *schema.Message {
			if call == 1 {
				return &schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
					ID: "h1", Function: schema.FunctionCall{Name: memorySearchToolName, Arguments: string(arguments)},
				}}}
			}
			return schema.AssistantMessage("你当时定的是固定 10 字节头部加长度字段。", nil)
		})

	if fake.ForcedToolChoiceCount() != 0 {
		t.Fatalf("a history question forced %d tool choices", fake.ForcedToolChoiceCount())
	}
	if search.calls != 0 {
		t.Fatalf("an irrelevant knowledge_search ran %d times", search.calls)
	}
	if memory.entries.Load() != 1 {
		t.Fatalf("the episodic search ran %d times", memory.entries.Load())
	}
	if result.Answer == "" {
		t.Fatal("the run must answer from the episode")
	}
}

// 6. A mixed question - technical subject, personal framing - takes document
// evidence first and may still read Episodes afterwards.
func TestMixedQuestionTakesDocumentEvidenceThenEpisodes(t *testing.T) {
	search := &trackedKnowledgeSearch{}
	memory := &scriptedMemorySearch{episodes: 1}
	arguments, _ := json.Marshal(map[string]any{
		"query": "earlier discussion about packet framing", "kinds": []string{"episodic"}, "top_k": 5})
	fake, result := runConfirmedMode(t, "TCP 粘包到底是什么？请结合我的学习记录讲。",
		[]tool.BaseTool{memory.build(t), search.build(t, groundedResults())},
		topicWindowBuilder{used: []string{"tcp粘包"}}, rag.KnowledgePreferred, func(call int) *schema.Message {
			if call == 1 {
				return knowledgeCall("k1")
			}
			if call == 2 {
				return &schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
					ID: "m1", Function: schema.FunctionCall{Name: memorySearchToolName, Arguments: string(arguments)},
				}}}
			}
			return citedAnswer(call)
		})

	if !fake.HasExclusiveToolBinding(knowledgeSearchToolName) {
		t.Fatalf("bindings = %v, want knowledge_search forced first", fake.BoundToolCalls())
	}
	if search.calls != 1 {
		t.Fatalf("knowledge_search ran %d times", search.calls)
	}
	if memory.entries.Load() != 1 {
		t.Fatalf("Episode retrieval after the document search ran %d times", memory.entries.Load())
	}
	if result.AnswerBasis == rag.AnswerBasisRefused {
		t.Fatalf("basis = %s", result.AnswerBasis)
	}
}

// 7. The knowledge search runs and finds nothing. Neither policy changes.
func TestNoCandidateKeepsExistingPolicyBehaviour(t *testing.T) {
	t.Run("preferred falls back to general knowledge", func(t *testing.T) {
		search := &trackedKnowledgeSearch{}
		memory := &scriptedMemorySearch{}
		_, result := runConfirmedMode(t, "Go 里 defer 的执行顺序是怎样的？",
			[]tool.BaseTool{memory.build(t), search.build(t, nil)},
			MinimalContextBuilder{}, rag.KnowledgePreferred, func(call int) *schema.Message {
				if call == 1 {
					return knowledgeCall("k1")
				}
				return schema.AssistantMessage("defer 按后进先出顺序执行。", nil)
			})
		if search.calls != 1 {
			t.Fatalf("knowledge_search ran %d times", search.calls)
		}
		if result.AnswerBasis == rag.AnswerBasisRefused {
			t.Fatalf("preferred must still fall back, basis = %s", result.AnswerBasis)
		}
	})
	t.Run("only still refuses", func(t *testing.T) {
		search := &trackedKnowledgeSearch{}
		memory := &scriptedMemorySearch{}
		_, result := runConfirmedMode(t, "Go 里 defer 的执行顺序是怎样的？",
			[]tool.BaseTool{memory.build(t), search.build(t, nil)},
			MinimalContextBuilder{}, rag.KnowledgeOnly, func(call int) *schema.Message {
				if call == 1 {
					return knowledgeCall("k1")
				}
				return schema.AssistantMessage("defer 按后进先出顺序执行。", nil)
			})
		if search.calls != 1 {
			t.Fatalf("knowledge_search ran %d times", search.calls)
		}
		if result.AnswerBasis != rag.AnswerBasisRefused {
			t.Fatalf("knowledge_only must still refuse, basis = %s", result.AnswerBasis)
		}
	})
}

// 8. The citation boundary is untouched: evidence exists, the answer cites
// nothing, and knowledge_only still refuses rather than shipping it.
func TestUncitedAnswerStillRefusedUnderKnowledgeOnly(t *testing.T) {
	search := &trackedKnowledgeSearch{}
	memory := &scriptedMemorySearch{}
	_, result := runConfirmedMode(t, "Go 里 defer 的执行顺序是怎样的？",
		[]tool.BaseTool{memory.build(t), search.build(t, groundedResults())},
		MinimalContextBuilder{}, rag.KnowledgeOnly, func(call int) *schema.Message {
			if call == 1 {
				return knowledgeCall("k1")
			}
			return schema.AssistantMessage("defer 按后进先出顺序执行。", nil)
		})
	if result.AnswerBasis != rag.AnswerBasisRefused || len(result.Sources) != 0 {
		t.Fatalf("result = %#v", result)
	}
}

// 9 and 10. The frozen identities keep their first-tool semantics, and the
// production default is not touched.
func TestFirstToolChoicePredicateIsFrozenForExperimentalModes(t *testing.T) {
	for mode, want := range map[memoryrecall.ReadMode]bool{
		memoryrecall.ModeAgentMemoryToolV1:          true,
		memoryrecall.ModeAgentMemoryToolV2:          true,
		memoryrecall.ModeAgentMemoryToolV3:          true,
		memoryrecall.ModeAgentMemoryToolV4:          true,
		memoryrecall.ModeAgentMemoryUserConfirmedV1: false,
		memoryrecall.ModeLegacyV2:                   false,
		memoryrecall.ModeEpisodeV3:                  false,
		memoryrecall.ModeEpisodeV3Lexical:           false,
		memoryrecall.ModeEpisodeV3Semantic:          false,
		memoryrecall.ModeModelPlannedV1:             false,
	} {
		if got := mode.DefersFirstToolChoiceToModel(); got != want {
			t.Errorf("%s.DefersFirstToolChoiceToModel() = %v, want %v", mode, got, want)
		}
	}
	if memoryrecall.DefaultReadMode() != memoryrecall.ModeLegacyV2 {
		t.Fatal("the production default must remain legacy_v2")
	}
	// The frozen budgets must not opt in by accident.
	for name, config := range map[string]RunnerConfig{
		"default": DefaultRunnerConfig(), "v1": AgentMemoryToolRunnerConfig(),
		"v3": AgentMemoryToolV3RunnerConfig(), "v4": AgentMemoryToolV4RunnerConfig(),
	} {
		if config.ForcesKnowledgeSearchWithMemoryTool {
			t.Errorf("%s config must not force knowledge search", name)
		}
	}
}

// 9, at the behaviour level: with the flag off, memory_search availability
// still suppresses the forced knowledge search.
func TestExperimentalModeKeepsModelChosenFirstTool(t *testing.T) {
	search := &trackedKnowledgeSearch{}
	memory := &scriptedMemorySearch{}
	fake := &scriptedChatModel{respond: func(_ context.Context, _ int) (*schema.Message, error) {
		return schema.AssistantMessage("defer 按后进先出顺序执行。", nil), nil
	}}
	runner := mustRunner(t, fake, StaticToolRegistry{Registered: []tool.BaseTool{memory.build(t),
		search.build(t, groundedResults())}}, newMemoryStepRepository(), AgentMemoryToolRunnerConfig())
	input := testRunContext()
	input.Question = "Go 里 defer 的执行顺序是怎样的？"
	input.MemoryEvidence = NewMemoryEvidenceRegistry()
	if _, err := runner.Run(context.Background(), input, DiscardEvents); err != nil {
		t.Fatal(err)
	}
	if fake.ForcedToolChoiceCount() != 0 {
		t.Fatalf("agent_memory_tool_v1 forced %d tool choices", fake.ForcedToolChoiceCount())
	}
	if search.calls != 0 {
		t.Fatalf("agent_memory_tool_v1 ran an unrequested knowledge_search %d times", search.calls)
	}
}

// 11. The forced choice is decided in one place, so the SSE transport reaches
// the same terminal state and the same basis as the sync one.
func TestStreamingAndSyncAgreeOnForcedKnowledgeSearch(t *testing.T) {
	run := func(t *testing.T, sink EventSink) RunnerResult {
		t.Helper()
		search := &trackedKnowledgeSearch{}
		memory := &scriptedMemorySearch{}
		fake := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
			return searchThenAnswer(call), nil
		}}
		runner, err := NewRunner(fake, StaticToolRegistry{Registered: []tool.BaseTool{memory.build(t),
			search.build(t, groundedResults())}}, newMemoryStepRepository(), MinimalContextBuilder{},
			userConfirmedRunnerConfig())
		if err != nil {
			t.Fatal(err)
		}
		input := testRunContext()
		input.Question = "Go 里 defer 的执行顺序是怎样的？"
		input.MemoryEvidence = NewMemoryEvidenceRegistry()
		result, err := runner.Run(context.Background(), input, sink)
		if err != nil {
			t.Fatal(err)
		}
		if !fake.HasExclusiveToolBinding(knowledgeSearchToolName) {
			t.Fatalf("bindings = %v, want knowledge_search forced first", fake.BoundToolCalls())
		}
		if search.calls != 1 {
			t.Fatalf("knowledge_search ran %d times", search.calls)
		}
		return result
	}
	var streamed []Event
	sync := run(t, DiscardEvents)
	live := run(t, EventSinkFunc(func(_ context.Context, event Event) error {
		streamed = append(streamed, event)
		return nil
	}))
	if sync.AnswerBasis != live.AnswerBasis || sync.KnowledgeStatus != live.KnowledgeStatus ||
		sync.KnowledgeReason != live.KnowledgeReason || sync.Answer != live.Answer {
		t.Fatalf("sync=%#v streamed=%#v", sync, live)
	}
	if len(streamed) == 0 {
		t.Fatal("the SSE path emitted no events")
	}
}

// 12 and 13. Forcing the first tool must not reopen STABILIZE-055: the forced
// call happens once, the run still ends, and the budgets are the frozen ones.
func TestForcedSearchDoesNotReopenTheToolLoop(t *testing.T) {
	config := userConfirmedRunnerConfig()
	frozen := AgentMemoryToolRunnerConfig()
	if config.MaxToolCalls != frozen.MaxToolCalls || config.MaxModelCalls != frozen.MaxModelCalls ||
		config.MaxGraphSteps != frozen.MaxGraphSteps || config.PerTool != frozen.PerTool {
		t.Fatalf("the run envelope moved: %#v vs %#v", config, frozen)
	}

	search := &trackedKnowledgeSearch{}
	memory := &scriptedMemorySearch{}
	semantic, _ := json.Marshal(map[string]any{
		"query": "learner's own understanding", "kinds": []string{"semantic"}, "top_k": 5})
	_, result := runConfirmedMode(t, "TCP 粘包到底是什么？请结合我的学习记录讲。",
		[]tool.BaseTool{memory.build(t), search.build(t, groundedResults())},
		MinimalContextBuilder{}, rag.KnowledgePreferred, func(call int) *schema.Message {
			if call == 1 {
				return knowledgeCall("k1")
			}
			if call >= 2 && call <= 6 {
				// A model that keeps trying the refused capability after the
				// forced search, exactly the STABILIZE-055 shape.
				return &schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
					ID: "s1", Function: schema.FunctionCall{Name: memorySearchToolName, Arguments: string(semantic)},
				}}}
			}
			return citedAnswer(call)
		})

	if search.calls != 1 {
		t.Fatalf("the forced knowledge_search ran %d times", search.calls)
	}
	if entered := memory.entries.Load(); entered > int32(noProgressAllowance) {
		t.Fatalf("the refused capability was entered %d times", entered)
	}
	if result.Answer == "" {
		t.Fatal("the run must still finish with an answer")
	}
	if result.ToolCalls > config.MaxToolCalls {
		t.Fatalf("tool calls = %d", result.ToolCalls)
	}
}

// The pre-fix reproduction, kept as a control rather than described in prose.
//
// With the old rule - memory_search available suppresses the knowledge-first
// choice - the three STABILIZE-055 questions reach the grounding gate having
// made no tool call, and the gate refuses them with search_required. This is
// the exact recorded shape, and it is what the fix has to move.
func TestOldRuleStillReproducesTheSearchRequiredRefusal(t *testing.T) {
	questions := []string{
		"TCP 粘包到底是什么？请结合我的学习记录讲。",
		"RPC 粘包处理和 Codec 注册表之间是怎么配合的？请结合我的学习记录。",
		"Go 里 defer 的执行顺序是怎样的？",
	}
	for _, question := range questions {
		search := &trackedKnowledgeSearch{}
		memory := &scriptedMemorySearch{}
		fake := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
			// The recorded behaviour: the model answers directly.
			return citedAnswer(call), nil
		}}
		// AgentMemoryToolRunnerConfig is the old envelope: the flag is off, so
		// this is the pre-STABILIZE-056 first-tool rule.
		runner := mustRunner(t, fake, StaticToolRegistry{Registered: []tool.BaseTool{memory.build(t),
			search.build(t, groundedResults())}}, newMemoryStepRepository(), AgentMemoryToolRunnerConfig())
		input := testRunContext()
		input.Question = question
		input.MemoryEvidence = NewMemoryEvidenceRegistry()
		result, err := runner.Run(context.Background(), input, DiscardEvents)
		if err != nil {
			t.Fatal(err)
		}
		if result.ToolCalls != 0 || search.calls != 0 {
			t.Fatalf("%q: the old rule must reach the gate with no tool call, tools=%d", question, result.ToolCalls)
		}
		if result.AnswerBasis != rag.AnswerBasisRefused ||
			result.KnowledgeStatus != rag.KnowledgeStatusInvalidGrounding ||
			result.KnowledgeReason != rag.KnowledgeReasonSearchRequired {
			t.Fatalf("%q: basis=%s status=%s reason=%s, want the recorded refusal",
				question, result.AnswerBasis, result.KnowledgeStatus, result.KnowledgeReason)
		}
	}
}
