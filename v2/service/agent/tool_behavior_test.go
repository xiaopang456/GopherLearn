package agent

import (
	"context"
	"strings"
	"testing"

	"GopherAI/common/memoryrecall"
	"GopherAI/common/rag"
	"GopherAI/model"

	modelcomponent "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"
)

// The three behaviours this file pins down are the ones that changed the
// product's shape:
//
//   - the server-side pre-retrieval, which answers the question the model
//     cannot answer for itself - whether the learner's own material covers
//     this question;
//   - the prompt, which is rendered from the tool table the turn actually has;
//   - the honest labelling of a turn that retrieved nothing.

// toolStepNames lists the tool steps a run left behind.
//
// The step list also holds plan and answer steps, so a test that asks "did a
// tool run" has to look at the tool steps and not at the length of the list.
func toolStepNames(steps *memoryStepRepository) []string {
	names := []string{}
	for _, step := range steps.steps {
		if step.Type == model.AgentStepTool {
			names = append(names, step.ToolName)
		}
	}
	return names
}

// proberRegistry is a tool registry that can also answer the server-side
// relevance question.
type proberRegistry struct {
	StaticToolRegistry
	outcome RelevanceOutcome
	probes  int
}

func (r *proberRegistry) ProbeRelevance(_ context.Context, _ RunContext, _ string, _ float64) (RelevanceOutcome, error) {
	r.probes++
	return r.outcome, nil
}

// hitOutcome is a pre-retrieval that found one passage.
func hitOutcome() RelevanceOutcome {
	return RelevanceOutcome{
		Status: RelevanceHit, Threshold: DefaultRelevanceThreshold, TopScore: 0.93, RerankApplied: true,
		Chunks: []RelevanceChunk{{
			Citation: 1, Identity: "doc:v1:chunk-1", DocID: "doc", DocVersion: 1,
			Content: "TCP 是面向字节流的协议。",
			Source: rag.Source{
				Citation: 1, DocID: "doc", DocVersion: 1, FileName: "note.md",
				TitlePath: []string{"粘包"}, StartLine: 3, EndLine: 9, Score: 0.93,
			},
		}},
		NextCitation: 2,
	}
}

// belowThresholdOutcome is a pre-retrieval that ran and found nothing close.
func belowThresholdOutcome() RelevanceOutcome {
	return RelevanceOutcome{
		Status: RelevanceBelowThreshold, Threshold: DefaultRelevanceThreshold,
		TopScore: 0.61, RerankApplied: true,
	}
}

// mustRealWindowBuilder assembles the window the way production does.
//
// The minimal builder is a test double that knows nothing about the
// pre-retrieval, so a test that wants to see the pre-retrieval's evidence in
// the window has to use the real one.
func mustRealWindowBuilder(t *testing.T) ContextBuilder {
	t.Helper()
	builder, err := NewContextBuilder(DefaultContextConfig(), stubHistory{}, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return builder
}

// mustRunnerWithWindow is mustRunner with a window of the test's choosing.
func mustRunnerWithWindow(t *testing.T, chatModel modelcomponent.ToolCallingChatModel, registry ToolRegistry,
	steps StepRepository, config RunnerConfig, builder ContextBuilder) *Runner {
	t.Helper()
	runner, err := NewRunner(chatModel, registry, steps, builder, config)
	if err != nil {
		t.Fatal(err)
	}
	return runner
}

// The pre-retrieval's evidence reaches the model before the model is asked
// anything, and the turn records that it searched.
func TestPreRetrievalHandsItsEvidenceToTheModelBeforeTheFirstCall(t *testing.T) {
	fake := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		return schema.AssistantMessage("TCP 面向字节流 [1]。", nil), nil
	}}
	registry := &proberRegistry{StaticToolRegistry: StaticToolRegistry{Registered: []tool.BaseTool{
		mustNamedEvidenceTool(t, knowledgeSearchToolName, nil),
	}}, outcome: hitOutcome()}
	steps := newMemoryStepRepository()
	runner := mustRunnerWithWindow(t, fake, registry, steps, DefaultRunnerConfig(), mustRealWindowBuilder(t))
	input := testRunContext()
	input.Question = "TCP 粘包为什么会发生？"
	input.MemoryEvidence = NewMemoryEvidenceRegistry()
	result, err := runner.Run(context.Background(), input, DiscardEvents)
	if err != nil {
		t.Fatal(err)
	}
	if registry.probes != 1 {
		t.Fatalf("the pre-retrieval ran %d times", registry.probes)
	}
	if len(fake.received) == 0 {
		t.Fatal("the model was never called")
	}
	window := renderMessageText(fake.received[0])
	if !strings.Contains(window, "TCP 是面向字节流的协议。") {
		t.Fatalf("the pre-retrieval's passage did not reach the model: %s", window)
	}
	if !strings.Contains(window, "[1]") {
		t.Fatalf("the passage did not carry its citation number: %s", window)
	}
	// The search is a step and it costs a tool slot, so that the step list and
	// the tool count keep telling the same story.
	if result.ToolCalls != 1 {
		t.Fatalf("tool calls = %d, want the pre-retrieval to be one of them", result.ToolCalls)
	}
	if got := toolStepNames(steps); len(got) != 1 || got[0] != knowledgeSearchToolName {
		t.Fatalf("tool steps = %#v, want one knowledge_search step", got)
	}
	// The answer cites the pre-retrieval's evidence, so it is grounded in the
	// learner's material even though no search tool ever ran.
	if result.AnswerBasis != rag.AnswerBasisPersonalKnowledge || result.KnowledgeStatus != rag.KnowledgeStatusGrounded {
		t.Fatalf("basis=%s status=%s, want a grounded answer", result.AnswerBasis, result.KnowledgeStatus)
	}
	if len(result.Sources) != 1 || result.Sources[0].Citation != 1 {
		t.Fatalf("sources = %#v", result.Sources)
	}
}

// A pre-retrieval that found nothing close says so, in words that describe
// this one search rather than the learner's material.
func TestPreRetrievalBelowThresholdTellsTheModelItMaySearchAgain(t *testing.T) {
	fake := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		return schema.AssistantMessage("粘包是因为应用层没有划定消息边界。", nil), nil
	}}
	registry := &proberRegistry{StaticToolRegistry: StaticToolRegistry{Registered: []tool.BaseTool{
		mustNamedEvidenceTool(t, knowledgeSearchToolName, nil),
	}}, outcome: belowThresholdOutcome()}
	steps := newMemoryStepRepository()
	runner := mustRunnerWithWindow(t, fake, registry, steps, DefaultRunnerConfig(), mustRealWindowBuilder(t))
	input := testRunContext()
	input.Question = "TCP 粘包为什么会发生？"
	input.MemoryEvidence = NewMemoryEvidenceRegistry()
	result, err := runner.Run(context.Background(), input, DiscardEvents)
	if err != nil {
		t.Fatal(err)
	}
	window := renderMessageText(fake.received[0])
	if !strings.Contains(window, "relevance threshold") {
		t.Fatalf("the model was not told what the pre-retrieval found: %s", window)
	}
	if strings.Contains(window, "no relevant content") || strings.Contains(window, "your notes have nothing") {
		t.Fatalf("the model was told the material has nothing to say: %s", window)
	}
	// Nothing was retrieved, so no step and no tool slot: the pre-retrieval
	// found nothing, and a search that found nothing is not a search.
	if result.ToolCalls != 0 {
		t.Fatalf("tool calls = %d, want none", result.ToolCalls)
	}
	if got := toolStepNames(steps); len(got) != 0 {
		t.Fatalf("tool steps = %#v, want none", got)
	}
	// A search happened and had nothing: that is not the same fact as a turn
	// that never searched, and the two are told apart.
	if result.AnswerBasis != rag.AnswerBasisGeneralKnowledge ||
		result.KnowledgeStatus != rag.KnowledgeStatusNoEvidence ||
		result.KnowledgeReason != rag.KnowledgeReasonNoCandidate {
		t.Fatalf("basis=%s status=%s reason=%s", result.AnswerBasis, result.KnowledgeStatus, result.KnowledgeReason)
	}
}

// A pre-retrieval that could not run says nothing at all, because it says
// nothing about the material.
func TestPreRetrievalThatCouldNotRunSaysNothing(t *testing.T) {
	fake := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		return schema.AssistantMessage("粘包是因为应用层没有划定消息边界。", nil), nil
	}}
	registry := &proberRegistry{StaticToolRegistry: StaticToolRegistry{Registered: []tool.BaseTool{
		mustNamedEvidenceTool(t, knowledgeSearchToolName, nil),
	}}, outcome: RelevanceOutcome{Status: RelevanceUnavailable, Threshold: DefaultRelevanceThreshold}}
	runner := mustRunnerWithWindow(t, fake, registry, newMemoryStepRepository(), DefaultRunnerConfig(), mustRealWindowBuilder(t))
	input := testRunContext()
	input.Question = "TCP 粘包为什么会发生？"
	input.MemoryEvidence = NewMemoryEvidenceRegistry()
	result, err := runner.Run(context.Background(), input, DiscardEvents)
	if err != nil {
		t.Fatal(err)
	}
	window := renderMessageText(fake.received[0])
	if strings.Contains(window, "search") && strings.Contains(window, "before this turn") {
		t.Fatalf("the window talks about a search that did not happen: %s", window)
	}
	// Nothing was retrieved and nothing was searched, and that is what the
	// record says.
	if result.AnswerBasis != rag.AnswerBasisGeneralKnowledge ||
		result.KnowledgeStatus != rag.KnowledgeStatusNotRequired ||
		result.KnowledgeReason != rag.KnowledgeReasonSearchNotPerformed {
		t.Fatalf("basis=%s status=%s reason=%s", result.AnswerBasis, result.KnowledgeStatus, result.KnowledgeReason)
	}
}

// The threshold is a configuration value, and zero turns the pre-retrieval off
// entirely.
func TestRelevanceThresholdIsAConfigurationValue(t *testing.T) {
	if DefaultRelevanceThreshold != 0.80 {
		t.Fatalf("default threshold = %v, want 0.80", DefaultRelevanceThreshold)
	}
	config := DefaultRunnerConfig()
	if config.RelevanceThreshold != DefaultRelevanceThreshold {
		t.Fatalf("the shipped config does not carry the default threshold: %v", config.RelevanceThreshold)
	}
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []float64{-0.1, 1.1} {
		broken := DefaultRunnerConfig()
		broken.RelevanceThreshold = invalid
		if err := broken.Validate(); err == nil {
			t.Fatalf("threshold %v was accepted", invalid)
		}
	}
	// Zero means no pre-retrieval, which is what a deployment without a
	// reranker gets.
	off := DefaultRunnerConfig()
	off.RelevanceThreshold = 0
	fake := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		return schema.AssistantMessage("答案。", nil), nil
	}}
	registry := &proberRegistry{StaticToolRegistry: StaticToolRegistry{Registered: []tool.BaseTool{
		mustNamedEvidenceTool(t, knowledgeSearchToolName, nil),
	}}, outcome: hitOutcome()}
	runner := mustRunner(t, fake, registry, newMemoryStepRepository(), off)
	input := testRunContext()
	input.Question = "TCP 粘包为什么会发生？"
	input.MemoryEvidence = NewMemoryEvidenceRegistry()
	if _, err := runner.Run(context.Background(), input, DiscardEvents); err != nil {
		t.Fatal(err)
	}
	if registry.probes != 0 {
		t.Fatalf("the pre-retrieval ran %d times with the threshold at zero", registry.probes)
	}
}

// The prompt only names tools the turn actually has.
//
// This is the long-term defence of the sixth item: a rule that names a tool
// the model cannot see is a rule the model cannot obey, and the model that
// breaks it used to break the whole turn.
func TestPromptNamesOnlyToolsThisTurnHas(t *testing.T) {
	for _, test := range []struct {
		name     string
		question string
		tools    []tool.BaseTool
		present  []string
		absent   []string
	}{
		{
			// The question points at the learner's own material, so the
			// document tools stay on the table and the prompt keeps talking
			// about them.
			name:     "the knowledge tools and nothing else",
			question: "我的笔记里是怎么讲 TCP 粘包的？",
			tools:    []tool.BaseTool{mustNamedEvidenceTool(t, knowledgeSearchToolName, nil)},
			present:  []string{"knowledge_search"},
			absent:   []string{"read_history_message", "propose_memory", "document_read"},
		},
		{
			name: "the draft tool and the knowledge tools",
			// "记住我" is a save request. "请记住" would also read as a
			// question about this conversation, and a question about this
			// conversation never keeps the document tools on the table.
			question: "记住我的讲解偏好，另外我的笔记里是怎么讲 TCP 粘包的？",
			tools: []tool.BaseTool{
				mustNamedEvidenceTool(t, knowledgeSearchToolName, nil),
				mustNamedEvidenceTool(t, "propose_memory", nil),
			},
			present: []string{"knowledge_search", "propose_memory"},
			absent:  []string{"read_history_message", "document_read"},
		},
		{
			// A question that is not about the learner's material takes the
			// document tools off the table, and the prompt stops naming them:
			// the same rule, seen from the other side.
			name:     "the draft tool alone",
			question: "请记住我这次的讲解偏好",
			tools: []tool.BaseTool{
				mustNamedEvidenceTool(t, "propose_memory", nil),
			},
			present: []string{"propose_memory"},
			absent:  []string{"knowledge_search", "read_history_message", "document_read"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
				return schema.AssistantMessage("答案。", nil), nil
			}}
			runner := mustRunner(t, fake, StaticToolRegistry{Registered: test.tools},
				newMemoryStepRepository(), DefaultRunnerConfig())
			input := testRunContext()
			input.Question = test.question
			input.MemoryEvidence = NewMemoryEvidenceRegistry()
			if _, err := runner.Run(context.Background(), input, DiscardEvents); err != nil {
				t.Fatal(err)
			}
			if len(fake.received) == 0 {
				t.Fatal("the model was never called")
			}
			prompt := fake.received[0][0].Content
			for _, name := range test.present {
				if !strings.Contains(prompt, name) {
					t.Errorf("the prompt does not mention %s, which this turn has: %s", name, prompt)
				}
			}
			for _, name := range test.absent {
				if strings.Contains(prompt, name) {
					t.Errorf("the prompt mentions %s, which this turn does not have: %s", name, prompt)
				}
			}
		})
	}
}

// With every capability available, the prompt is the base prompt byte for
// byte. The modes with published measurements were measured against these
// bytes, and a prompt that moved under them would move every published number
// onto bytes nobody measured.
func TestPromptIsUnchangedWhenEveryCapabilityIsAvailable(t *testing.T) {
	capabilities := PromptCapabilities{Available: map[MemoryToolCapability]bool{}, MemoryIntent: true}
	for _, capability := range []MemoryToolCapability{
		CapabilityKnowledgeSearch, CapabilityDocumentRead, CapabilityHistoryRead,
		CapabilityEpisodeSearch, CapabilityTopicMemoryRead, CapabilityMemoryDraft,
	} {
		capabilities.Available[capability] = true
	}
	base := SystemPromptFor(memoryrecall.ModeLegacyV2)
	if got := RenderSystemPrompt(base, capabilities); got != base {
		t.Fatalf("the prompt changed although every capability was available")
	}
}

// The save rule is the one place where the prompt depends on the question
// rather than on the tool table: the draft tool is on the table every turn,
// and what the learner is told about using it follows the question.
func TestSaveRuleFollowsTheQuestionNotTheToolTable(t *testing.T) {
	capabilities := PromptCapabilities{Available: map[MemoryToolCapability]bool{
		CapabilityKnowledgeSearch: true, CapabilityMemoryDraft: true,
	}, MemoryIntent: true}
	base := SystemPromptFor(memoryrecall.ModeLegacyV2)
	withIntent := RenderSystemPrompt(base, capabilities)
	capabilities.MemoryIntent = false
	withoutIntent := RenderSystemPrompt(base, capabilities)
	if !strings.Contains(withIntent, "propose_memory") {
		t.Fatalf("a save request does not carry the save rule: %s", withIntent)
	}
	if strings.Contains(withoutIntent, "propose_memory") {
		t.Fatalf("an ordinary question still carries the save rule: %s", withoutIntent)
	}
}

// The forced first tool is one the turn can actually bind.
//
// The rule changed, and a rule that can name a tool this turn does not hold
// fails the turn before the first model request.
func TestForcedToolIsAlwaysRegistered(t *testing.T) {
	for _, question := range []string{
		"我的笔记里怎么解释 TCP 粘包？",
		"解释一下 TCP 粘包",
		"你好",
		"我之前是怎么决定的？",
	} {
		state := &runState{
			config:                   DefaultRunnerConfig(),
			input:                    RunContext{Question: question},
			knowledgeSearchAvailable: true,
			knowledgePolicy:          rag.KnowledgePreferred,
		}
		forced := state.forcedToolName()
		if forced == "" {
			continue
		}
		registered := []string{knowledgeSearchToolName, readHistoryMessageToolName}
		if missing := UnregisteredForcedTools(state.config, registered); len(missing) > 0 {
			t.Fatalf("question %q forces %q, which the table does not hold: %v", question, forced, missing)
		}
	}
}

// A question about the learner's own past is answered from that past, and the
// record says so.
func TestHistoryQuestionIsLabelledAsTheConversation(t *testing.T) {
	fake := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		return schema.AssistantMessage("你当时定的是固定 10 字节头部加长度字段。", nil), nil
	}}
	registry := &proberRegistry{StaticToolRegistry: StaticToolRegistry{Registered: []tool.BaseTool{
		mustNamedEvidenceTool(t, knowledgeSearchToolName, nil),
	}}, outcome: hitOutcome()}
	runner := mustRunner(t, fake, registry, newMemoryStepRepository(), DefaultRunnerConfig())
	input := testRunContext()
	input.Question = "我之前是怎么决定用固定头部方案的？"
	input.MemoryEvidence = NewMemoryEvidenceRegistry()
	result, err := runner.Run(context.Background(), input, DiscardEvents)
	if err != nil {
		t.Fatal(err)
	}
	if registry.probes != 0 {
		t.Fatalf("the pre-retrieval ran %d times for a question about the learner's own past", registry.probes)
	}
	if result.AnswerBasis != rag.AnswerBasisConversation {
		t.Fatalf("basis = %s, want the conversation", result.AnswerBasis)
	}
	if result.KnowledgeStatus != rag.KnowledgeStatusNotRequired || result.KnowledgeReason != rag.KnowledgeReasonNone {
		t.Fatalf("status=%s reason=%s", result.KnowledgeStatus, result.KnowledgeReason)
	}
}

// renderMessageText joins a request's messages so a test can look at what the
// model was shown.
func renderMessageText(messages []*schema.Message) string {
	var out strings.Builder
	for _, message := range messages {
		out.WriteString(message.Content)
		out.WriteString("\n")
	}
	return out.String()
}

// A draft the server refused leaves no card, and it does not take the turn
// with it.
//
// The draft tool refuses a content that did not come from the learner's own
// message. That refusal used to reach the runner as a result it could not
// parse, and an unparsable tool result is an error - so a learner who asked
// the assistant to remember something lost the whole answer because the
// assistant tried to remember something that was not theirs to remember.
func TestRefusedDraftLeavesNoCardAndKeepsTheTurn(t *testing.T) {
	fake := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		if call == 1 {
			return schema.AssistantMessage("", []schema.ToolCall{{
				ID: "draft-call", Type: "function",
				Function: schema.FunctionCall{Name: proposeMemoryToolName, Arguments: `{"scope":"topic","subject":"个人理解","content":"我从别处抄来的一句话","durability":"stable"}`},
			}}), nil
		}
		return schema.AssistantMessage("这一轮我还是回答了。", nil), nil
	}}
	registry := StaticToolRegistry{Registered: []tool.BaseTool{mustRefusingDraftTool(t)}}
	runner := mustRunner(t, fake, registry, newMemoryStepRepository(), DefaultRunnerConfig())
	input := testRunContext()
	input.Question = "请记住我用队列理解 Redis"
	input.MemoryEvidence = NewMemoryEvidenceRegistry()
	result, err := runner.Run(context.Background(), input, DiscardEvents)
	if err != nil {
		t.Fatalf("a refused draft failed the turn: %v", err)
	}
	if result.MemoryDraft != nil {
		t.Fatalf("a refused draft produced a card: %#v", result.MemoryDraft)
	}
	if result.Answer == "" {
		t.Fatal("the turn ended without an answer")
	}
	// The model is told why, so it can write the content the right way.
	if window := renderMessageText(fake.received[len(fake.received)-1]); !strings.Contains(window, "rejected") {
		t.Fatalf("the model was not told the draft was refused: %s", window)
	}
}

// refusingDraftOutput is what the draft tool returns when the server refuses
// the content: a result, not an error.
type refusingDraftOutput struct {
	DraftID         string `json:"draft_id"`
	Rejected        bool   `json:"rejected"`
	RejectionReason string `json:"rejection_reason"`
}

// mustRefusingDraftTool is a draft tool that always refuses.
func mustRefusingDraftTool(t *testing.T) tool.InvokableTool {
	t.Helper()
	result, err := toolutils.InferTool(proposeMemoryToolName, "propose a memory draft",
		func(_ context.Context, _ *map[string]any) (*refusingDraftOutput, error) {
			return &refusingDraftOutput{Rejected: true, RejectionReason: "the draft may only record what the learner's current message says"}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
