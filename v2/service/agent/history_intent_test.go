package agent

import (
	"context"
	"strings"
	"testing"

	"GopherAI/common/rag"

	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"
)

// The sentence that started this.
//
// It is a statement about a design: what a whiteboard holds, what a summary
// segment holds, what a transcript is for, and which of the three the learner
// keeps mixing up. It mentions 原话, and the phrase table read that as a
// request for the learner's own words - so the turn was forced to read a
// history message and, with no history to read, answered with a refusal.
const layeredSummaryStatement = "我现在对分层摘要的理解是：白板记录当前生效的状态，摘要段保留当时发生的变化，原话用来核对证据。我用一句口诀区分它们：「现值看白板，旧值翻档案」。我最容易搞混的是把摘要段里的旧值当成现值。"

// The rule follows the request, not the vocabulary.
//
// Every question here is one a learner can actually type, and the right answer
// differs between them even though three of the four share a phrase with the
// old table.
func TestHistoryEvidenceRuleFollowsTheRequestNotTheVocabulary(t *testing.T) {
	for _, test := range []struct {
		name       string
		question   string
		wantExact  bool
		wantRecall bool
	}{
		{
			name:       "a statement about what a transcript is for is not a request for one",
			question:   layeredSummaryStatement,
			wantExact:  false,
			wantRecall: false,
		},
		{
			name:       "a document quotation is a document question",
			question:   "请引用《03-layered-summary.md》里白板定义的原文",
			wantExact:  false,
			wantRecall: false,
		},
		{
			name:       "a verbatim recall of an earlier message is a history request",
			question:   "请逐字贴出我上次说的口诀",
			wantExact:  true,
			wantRecall: true,
		},
		{
			name:       "the wording of an earlier message is a history request",
			question:   "我当时原话是怎么说的？",
			wantExact:  true,
			wantRecall: true,
		},
		{
			name:       "the learner's own words are a history request",
			question:   "我的原话是什么？",
			wantExact:  true,
			wantRecall: true,
		},
		{
			name:       "an instruction to reproduce the learner's words is a history request",
			question:   "把我的原话一字不差地说出来。",
			wantExact:  true,
			wantRecall: true,
		},
		{
			name:       "a question about the words alone is a history request",
			question:   "原话是什么？",
			wantExact:  true,
			wantRecall: true,
		},
		{
			name:       "a segment the learner pasted into the conversation is still the conversation",
			question:   "请逐字贴出我上次贴的那段原文",
			wantExact:  true,
			wantRecall: true,
		},
		{
			name:       "a file name inside a request for the learner's own words is the topic of those words",
			question:   "我上次提到的 03-layered-summary.md 里的原话是什么？",
			wantExact:  true,
			wantRecall: true,
		},
		{
			name:       "a Chinese title is a document of the learner's own",
			question:   "请引用《分层摘要》里白板定义的原文",
			wantExact:  false,
			wantRecall: false,
		},
		{
			name:       "a file name and a document title in one turn are read as two requests",
			question:   "请引用《分层摘要》里的原文，我上次也问过同一段",
			wantExact:  false,
			wantRecall: false,
		},
		{
			name:       "an ordinary technical question asks for no history",
			question:   "Go 里 defer 的执行顺序是怎样的？",
			wantExact:  false,
			wantRecall: false,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := requiresExactHistoryEvidence(test.question); got != test.wantExact {
				t.Errorf("requiresExactHistoryEvidence(%q) = %v, want %v", test.question, got, test.wantExact)
			}
			if got := isHistoryRecallQuestion(test.question); got != test.wantRecall {
				t.Errorf("isHistoryRecallQuestion(%q) = %v, want %v", test.question, got, test.wantRecall)
			}
		})
	}
}

// The two places that decide have to decide the same way.
//
// The first tool choice binds read_history_message because of
// requiresExactHistoryEvidence, and the final answer check refuses a turn
// without that evidence because of the same predicate. This test reads the
// first-tool decision off the runner state and compares it with the demand the
// final check makes, so the two cannot drift apart unnoticed.
func TestFirstToolChoiceAndFinalAnswerCheckAgreeOnTheHistoryRule(t *testing.T) {
	for _, question := range []string{
		layeredSummaryStatement,
		"请引用《03-layered-summary.md》里白板定义的原文",
		"请逐字贴出我上次说的口诀",
		"我当时原话是怎么说的？",
		"我的原话是什么？",
		"把我的原话一字不差地说出来。",
		"原话是什么？",
		"解释一下 TCP 粘包",
	} {
		state := &runState{
			config:               DefaultRunnerConfig(),
			input:                RunContext{Question: question},
			historyReadAvailable: true,
		}
		forced := state.forcedToolName() == readHistoryMessageToolName
		demanded := requiresExactHistoryEvidence(question)
		if forced != demanded {
			t.Errorf("%q: first tool forces a history read = %v, final check demands history evidence = %v",
				question, forced, demanded)
		}
	}
}

// The first tool choice is the half of the rule that is visible before the
// model runs.
//
// A concept statement must not spend the turn's first tool on a history read
// it never asked for, and a document quotation must not either: the document
// search is the tool that question needs.
func TestTheFirstToolChoiceFollowsTheSameRule(t *testing.T) {
	// The concept statement, with the history reader registered and ready.
	state := &runState{
		config:               DefaultRunnerConfig(),
		input:                RunContext{Question: layeredSummaryStatement},
		historyReadAvailable: true,
	}
	if got := state.forcedToolName(); got != "" {
		t.Errorf("first tool = %q, want none for a statement that asks for no history", got)
	}
	// The document quotation, with the knowledge search registered.
	document := &runState{
		config:                   DefaultRunnerConfig(),
		input:                    RunContext{Question: "请引用《03-layered-summary.md》里白板定义的原文"},
		knowledgeSearchAvailable: true,
		knowledgePolicy:          rag.KnowledgePreferred,
	}
	if got := document.forcedToolName(); got != knowledgeSearchToolName {
		t.Errorf("first tool = %q, want %q for a document quotation", got, knowledgeSearchToolName)
	}
	// The verbatim recall, with the history reader registered.
	recall := &runState{
		config:               DefaultRunnerConfig(),
		input:                RunContext{Question: "请逐字贴出我上次说的口诀"},
		historyReadAvailable: true,
	}
	if got := recall.forcedToolName(); got != readHistoryMessageToolName {
		t.Errorf("first tool = %q, want %q for a verbatim recall", got, readHistoryMessageToolName)
	}
}

// The three questions, asked of one runner, with the knowledge base and the
// topic memory off.
//
// The concept statement is answered. The document quotation is a document
// question. The verbatim recall is a history question, and it is refused for
// want of history evidence - the refusal that has to survive the fix.
func TestTheHistoryRefusalFiresOnlyForAHistoryRequest(t *testing.T) {
	for _, test := range []struct {
		name             string
		question         string
		wantHistoryRefus bool
	}{
		{
			name:             "a concept statement is answered",
			question:         layeredSummaryStatement,
			wantHistoryRefus: false,
		},
		{
			name:             "a document quotation is answered",
			question:         "请引用《03-layered-summary.md》里白板定义的原文",
			wantHistoryRefus: false,
		},
		{
			name:             "a verbatim recall without history evidence is refused",
			question:         "请逐字贴出我上次说的口诀",
			wantHistoryRefus: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
				if test.wantHistoryRefus && call == 1 {
					return schema.AssistantMessage("", []schema.ToolCall{{
						ID:       "call-1",
						Function: schema.FunctionCall{Name: readHistoryMessageToolName, Arguments: `{"message_ids":[11]}`},
					}}), nil
				}
				return schema.AssistantMessage("白板记录当前生效的状态，摘要段保留当时发生的变化。", nil), nil
			}}
			// Only the history reader is registered: the knowledge base and the
			// topic memory are off, exactly as the learner left them. And the
			// reader has nothing to give back, so no turn can be grounded on
			// history evidence.
			historyTool := mustHistoryReadTool(t, "", []int{11})
			runner := mustRunner(t, fake, StaticToolRegistry{Registered: []tool.BaseTool{historyTool}},
				newMemoryStepRepository(), DefaultRunnerConfig())
			input := testRunContext()
			input.Question = test.question

			result, err := runner.Run(context.Background(), input, DiscardEvents)
			if err != nil {
				t.Fatal(err)
			}
			refused := result.Answer == UnsupportedHistoryAnswer
			if refused != test.wantHistoryRefus {
				t.Fatalf("answer = %q, history refusal = %v, want %v", result.Answer, refused, test.wantHistoryRefus)
			}
			if !test.wantHistoryRefus && result.AnswerBasis == rag.AnswerBasisRefused {
				t.Fatalf("answer = %q, basis = %s: the turn was refused", result.Answer, result.AnswerBasis)
			}
			if !test.wantHistoryRefus && fake.ForcedToolChoiceCount() != 0 {
				t.Fatalf("the first tool was forced %d times for a question that asks for no history",
					fake.ForcedToolChoiceCount())
			}
		})
	}
}

// A document quotation is a document question end to end.
//
// The knowledge search is bound as the first tool, it runs, and the answer
// that cites it is kept. Before the fix the same question was routed to the
// history reader and refused for want of history evidence.
func TestADocumentQuotationIsAnsweredByTheDocumentSearch(t *testing.T) {
	search := &trackedKnowledgeSearch{}
	fake := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		if call == 1 {
			return knowledgeCall("k1"), nil
		}
		return schema.AssistantMessage("白板记录当前生效的状态，摘要段保留当时发生的变化 [1]。", nil), nil
	}}
	knowledgeTool := search.build(t, groundedResults())
	runner := mustRunner(t, fake, StaticToolRegistry{Registered: []tool.BaseTool{knowledgeTool}},
		newMemoryStepRepository(), DefaultRunnerConfig())
	input := testRunContext()
	input.Question = "请引用《03-layered-summary.md》里白板定义的原文"

	result, err := runner.Run(context.Background(), input, DiscardEvents)
	if err != nil {
		t.Fatal(err)
	}
	if search.calls != 1 {
		t.Fatalf("knowledge_search ran %d times, want 1", search.calls)
	}
	if !fake.HasExclusiveToolBinding(knowledgeSearchToolName) {
		t.Fatalf("tool bindings = %v, want an exclusive knowledge_search", fake.BoundToolCalls())
	}
	if result.Answer == UnsupportedHistoryAnswer {
		t.Fatalf("answer = %q: a document question was refused for want of history evidence", result.Answer)
	}
	if result.AnswerBasis == rag.AnswerBasisRefused {
		t.Fatalf("answer = %q, basis = %s: the turn was refused", result.Answer, result.AnswerBasis)
	}
	if len(result.Sources) == 0 {
		t.Fatalf("answer = %q carries no source: the document search was not used as the answer's basis", result.Answer)
	}
}

// A verbatim recall with evidence behind it is still answered.
//
// This is the behaviour the fix must not break: the learner asks for their own
// words, the server reads them back, and the answer is accepted on that
// evidence.
func TestAVerbatimRecallWithHistoryEvidenceIsAnswered(t *testing.T) {
	fake := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		if call == 1 {
			return schema.AssistantMessage("", []schema.ToolCall{{
				ID:       "call-1",
				Function: schema.FunctionCall{Name: readHistoryMessageToolName, Arguments: `{"message_ids":[11]}`},
			}}), nil
		}
		return schema.AssistantMessage("你上次说的是「现值看白板，旧值翻档案」。", nil), nil
	}}
	historyTool := mustHistoryReadTool(t, "现值看白板，旧值翻档案", nil)
	runner := mustRunner(t, fake, StaticToolRegistry{Registered: []tool.BaseTool{historyTool}},
		newMemoryStepRepository(), DefaultRunnerConfig())
	input := testRunContext()
	input.Question = "请逐字贴出我上次说的口诀"

	result, err := runner.Run(context.Background(), input, DiscardEvents)
	if err != nil {
		t.Fatal(err)
	}
	if result.Answer == UnsupportedHistoryAnswer || result.AnswerBasis == rag.AnswerBasisRefused {
		t.Fatalf("answer = %q, basis = %s: a grounded quotation was refused", result.Answer, result.AnswerBasis)
	}
	if !strings.Contains(result.Answer, "现值看白板") {
		t.Fatalf("answer = %q, want the learner's own words", result.Answer)
	}
	if !fake.HasExclusiveToolBinding(readHistoryMessageToolName) {
		t.Fatalf("tool bindings = %v, want an exclusive history read", fake.BoundToolCalls())
	}
}

// A verbatim recall with nothing behind it keeps the existing prompt.
//
// The history reader ran and returned only refused ids, so there is no
// verifiable wording to quote. The turn answers with the same safe fallback it
// has always answered with, rather than a paraphrase the learner would have to
// trust.
func TestAVerbatimRecallWithoutHistoryEvidenceKeepsTheExistingPrompt(t *testing.T) {
	fake := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		if call == 1 {
			return schema.AssistantMessage("", []schema.ToolCall{{
				ID:       "call-1",
				Function: schema.FunctionCall{Name: readHistoryMessageToolName, Arguments: `{"message_ids":[11]}`},
			}}), nil
		}
		return schema.AssistantMessage("你上次说的好像是「现值看白板，旧值翻档案」。", nil), nil
	}}
	historyTool := mustHistoryReadTool(t, "", []int{11})
	runner := mustRunner(t, fake, StaticToolRegistry{Registered: []tool.BaseTool{historyTool}},
		newMemoryStepRepository(), DefaultRunnerConfig())
	input := testRunContext()
	input.Question = "请逐字贴出我上次说的口诀"

	result, err := runner.Run(context.Background(), input, DiscardEvents)
	if err != nil {
		t.Fatal(err)
	}
	if result.Answer != UnsupportedHistoryAnswer {
		t.Fatalf("answer = %q, want %q", result.Answer, UnsupportedHistoryAnswer)
	}
	if result.AnswerBasis != rag.AnswerBasisRefused {
		t.Fatalf("basis = %s, want %s", result.AnswerBasis, rag.AnswerBasisRefused)
	}
}

// The three shapes a learner actually writes, at the runner level.
//
// The quote object is decided on the same request. A file name inside a request
// for the learner's own words is the topic of those words, and a Chinese title
// is a document of the learner's own. The runner shows which one it took - the
// first tool it binds - and what it finally returned.
func TestTheQuoteObjectIsDecidedOnTheSameRequest(t *testing.T) {
	// The concept statement. The knowledge base and the topic memory are off,
	// so the history reader is the only tool the turn has.
	t.Run("a concept statement is answered with no tool bound", func(t *testing.T) {
		fake := &scriptedChatModel{respond: func(_ context.Context, _ int) (*schema.Message, error) {
			return schema.AssistantMessage("白板记录当前生效的状态，摘要段保留当时发生的变化。", nil), nil
		}}
		runner := mustRunner(t, fake,
			StaticToolRegistry{Registered: []tool.BaseTool{mustHistoryReadTool(t, "", []int{11})}},
			newMemoryStepRepository(), DefaultRunnerConfig())
		input := testRunContext()
		input.Question = layeredSummaryStatement

		result, err := runner.Run(context.Background(), input, DiscardEvents)
		if err != nil {
			t.Fatal(err)
		}
		if result.Answer == UnsupportedHistoryAnswer || result.AnswerBasis == rag.AnswerBasisRefused {
			t.Fatalf("answer = %q, basis = %s: the turn was refused", result.Answer, result.AnswerBasis)
		}
		if fake.ForcedToolChoiceCount() != 0 {
			t.Fatalf("the turn forced %d tool choices for a question that asks for no history",
				fake.ForcedToolChoiceCount())
		}
	})

	// A history request whose file name is the topic of the learner's words.
	t.Run("a file name inside a history request is read back from the history", func(t *testing.T) {
		fake := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
			if call == 1 {
				return schema.AssistantMessage("", []schema.ToolCall{{
					ID:       "call-1",
					Function: schema.FunctionCall{Name: readHistoryMessageToolName, Arguments: `{"message_ids":[11]}`},
				}}), nil
			}
			return schema.AssistantMessage("你上次说的是「现值看白板，旧值翻档案」。", nil), nil
		}}
		runner := mustRunner(t, fake,
			StaticToolRegistry{Registered: []tool.BaseTool{
				mustHistoryReadTool(t, "现值看白板，旧值翻档案", nil),
				mustNamedEvidenceTool(t, knowledgeSearchToolName, groundedResults()),
			}},
			newMemoryStepRepository(), DefaultRunnerConfig())
		input := testRunContext()
		input.Question = "我上次提到的 03-layered-summary.md 里的原话是什么？"

		result, err := runner.Run(context.Background(), input, DiscardEvents)
		if err != nil {
			t.Fatal(err)
		}
		if result.Answer == UnsupportedHistoryAnswer || result.AnswerBasis == rag.AnswerBasisRefused {
			t.Fatalf("answer = %q, basis = %s: a grounded quotation was refused", result.Answer, result.AnswerBasis)
		}
		if !strings.Contains(result.Answer, "现值看白板") {
			t.Fatalf("answer = %q, want the learner's own words", result.Answer)
		}
		if !fake.HasExclusiveToolBinding(readHistoryMessageToolName) {
			t.Fatalf("tool bindings = %v, want an exclusive history read", fake.BoundToolCalls())
		}
	})

	t.Run("the same request without history evidence keeps the existing prompt", func(t *testing.T) {
		fake := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
			if call == 1 {
				return schema.AssistantMessage("", []schema.ToolCall{{
					ID:       "call-1",
					Function: schema.FunctionCall{Name: readHistoryMessageToolName, Arguments: `{"message_ids":[11]}`},
				}}), nil
			}
			return schema.AssistantMessage("你上次说的好像是「现值看白板，旧值翻档案」。", nil), nil
		}}
		runner := mustRunner(t, fake,
			StaticToolRegistry{Registered: []tool.BaseTool{
				mustHistoryReadTool(t, "", []int{11}),
				mustNamedEvidenceTool(t, knowledgeSearchToolName, groundedResults()),
			}},
			newMemoryStepRepository(), DefaultRunnerConfig())
		input := testRunContext()
		input.Question = "我上次提到的 03-layered-summary.md 里的原话是什么？"

		result, err := runner.Run(context.Background(), input, DiscardEvents)
		if err != nil {
			t.Fatal(err)
		}
		if result.Answer != UnsupportedHistoryAnswer {
			t.Fatalf("answer = %q, want %q", result.Answer, UnsupportedHistoryAnswer)
		}
		if result.AnswerBasis != rag.AnswerBasisRefused {
			t.Fatalf("basis = %s, want %s", result.AnswerBasis, rag.AnswerBasisRefused)
		}
	})

	// A Chinese title: the document the learner wants quoted.
	t.Run("a Chinese title is read from the document", func(t *testing.T) {
		search := &trackedKnowledgeSearch{}
		fake := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
			if call == 1 {
				return knowledgeCall("k1"), nil
			}
			return schema.AssistantMessage("白板记录当前生效的状态，摘要段保留当时发生的变化 [1]。", nil), nil
		}}
		runner := mustRunner(t, fake,
			StaticToolRegistry{Registered: []tool.BaseTool{
				mustHistoryReadTool(t, "", []int{11}),
				search.build(t, groundedResults()),
			}},
			newMemoryStepRepository(), DefaultRunnerConfig())
		input := testRunContext()
		input.Question = "请引用《分层摘要》里白板定义的原文"

		result, err := runner.Run(context.Background(), input, DiscardEvents)
		if err != nil {
			t.Fatal(err)
		}
		if search.calls != 1 {
			t.Fatalf("knowledge_search ran %d times, want 1", search.calls)
		}
		if !fake.HasExclusiveToolBinding(knowledgeSearchToolName) {
			t.Fatalf("tool bindings = %v, want an exclusive knowledge_search", fake.BoundToolCalls())
		}
		if result.Answer == UnsupportedHistoryAnswer {
			t.Fatalf("answer = %q: a document question was refused for want of history evidence", result.Answer)
		}
		if len(result.Sources) == 0 {
			t.Fatalf("answer = %q carries no source: the document search was not the answer's basis", result.Answer)
		}
	})
}

// The three kinds of thing a learner can ask to quote, checked together.
//
// The evidence check hangs on the object the learner explicitly asked for: a
// concept question needs nothing, an uploaded document keeps the document's
// own evidence requirement, and the learner's own words keep the history
// requirement - with the existing prompt when there is nothing to read back.
// Every case below states the tool the turn actually bound first and the
// answer it actually returned.
func TestTheQuoteTargetsAreCheckedTogether(t *testing.T) {
	// The concept question. Nothing is being quoted, so no tool is bound and
	// the turn is answered.
	t.Run("a concept question is answered", func(t *testing.T) {
		fake := &scriptedChatModel{respond: func(_ context.Context, _ int) (*schema.Message, error) {
			return schema.AssistantMessage("白板记录当前生效的状态，摘要段保留当时发生的变化。", nil), nil
		}}
		runner := mustRunner(t, fake,
			StaticToolRegistry{Registered: []tool.BaseTool{mustHistoryReadTool(t, "", []int{11})}},
			newMemoryStepRepository(), DefaultRunnerConfig())
		input := testRunContext()
		input.Question = layeredSummaryStatement

		result, err := runner.Run(context.Background(), input, DiscardEvents)
		if err != nil {
			t.Fatal(err)
		}
		if result.AnswerBasis == rag.AnswerBasisRefused {
			t.Fatalf("answer = %q, basis = %s: the concept question was refused", result.Answer, result.AnswerBasis)
		}
		if fake.ForcedToolChoiceCount() != 0 {
			t.Fatalf("the turn forced %d tool choices", fake.ForcedToolChoiceCount())
		}
	})

	// The uploaded document. The document's own evidence requirement stands:
	// the answer rests on the document, and a turn that cannot cite it is
	// refused for that reason - not for want of the learner's words.
	t.Run("an uploaded document quote keeps the document evidence requirement", func(t *testing.T) {
		search := &trackedKnowledgeSearch{}
		fake := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
			if call == 1 {
				return knowledgeCall("k1"), nil
			}
			return schema.AssistantMessage("白板记录当前生效的状态，摘要段保留当时发生的变化 [1]。", nil), nil
		}}
		runner := mustRunner(t, fake,
			StaticToolRegistry{Registered: []tool.BaseTool{
				mustHistoryReadTool(t, "", []int{11}),
				search.build(t, groundedResults()),
			}},
			newMemoryStepRepository(), DefaultRunnerConfig())
		input := testRunContext()
		input.Question = "请引用《03-layered-summary.md》里白板定义的原文"

		result, err := runner.Run(context.Background(), input, DiscardEvents)
		if err != nil {
			t.Fatal(err)
		}
		if !fake.HasExclusiveToolBinding(knowledgeSearchToolName) {
			t.Fatalf("tool bindings = %v, want an exclusive knowledge_search", fake.BoundToolCalls())
		}
		if result.Answer == UnsupportedHistoryAnswer {
			t.Fatalf("answer = %q: a document question was refused for want of history evidence", result.Answer)
		}
		if len(result.Sources) == 0 {
			t.Fatalf("answer = %q carries no source: the document search was not the answer's basis", result.Answer)
		}
	})

	t.Run("a document quote with no retrievable citation is refused for that reason", func(t *testing.T) {
		fake := &scriptedChatModel{respond: func(_ context.Context, _ int) (*schema.Message, error) {
			return schema.AssistantMessage("白板记录当前生效的状态，摘要段保留当时发生的变化。", nil), nil
		}}
		runner := mustRunner(t, fake,
			StaticToolRegistry{Registered: []tool.BaseTool{
				mustHistoryReadTool(t, "", []int{11}),
				mustNamedEvidenceTool(t, knowledgeSearchToolName, groundedResults()),
			}},
			newMemoryStepRepository(), DefaultRunnerConfig())
		input := testRunContext()
		input.Question = "请引用《03-layered-summary.md》里白板定义的原文"

		result, err := runner.Run(context.Background(), input, DiscardEvents)
		if err != nil {
			t.Fatal(err)
		}
		if result.Answer == UnsupportedHistoryAnswer {
			t.Fatalf("answer = %q: the document question was refused for want of history evidence", result.Answer)
		}
		if result.AnswerBasis != rag.AnswerBasisRefused {
			t.Fatalf("answer = %q, basis = %s: want the document evidence requirement to hold", result.Answer, result.AnswerBasis)
		}
	})

	// The learner's own words. The turn reads them back and answers on them.
	t.Run("a conversation quote is read back from the history", func(t *testing.T) {
		fake := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
			if call == 1 {
				return schema.AssistantMessage("", []schema.ToolCall{{
					ID:       "call-1",
					Function: schema.FunctionCall{Name: readHistoryMessageToolName, Arguments: `{"message_ids":[11]}`},
				}}), nil
			}
			return schema.AssistantMessage("你上次说的是「现值看白板，旧值翻档案」。", nil), nil
		}}
		runner := mustRunner(t, fake,
			StaticToolRegistry{Registered: []tool.BaseTool{
				mustHistoryReadTool(t, "现值看白板，旧值翻档案", nil),
				mustNamedEvidenceTool(t, knowledgeSearchToolName, groundedResults()),
			}},
			newMemoryStepRepository(), DefaultRunnerConfig())
		input := testRunContext()
		input.Question = "请逐字贴出我上次说的口诀"

		result, err := runner.Run(context.Background(), input, DiscardEvents)
		if err != nil {
			t.Fatal(err)
		}
		if !fake.HasExclusiveToolBinding(readHistoryMessageToolName) {
			t.Fatalf("tool bindings = %v, want an exclusive history read", fake.BoundToolCalls())
		}
		if result.AnswerBasis == rag.AnswerBasisRefused {
			t.Fatalf("answer = %q, basis = %s: a grounded quotation was refused", result.Answer, result.AnswerBasis)
		}
	})

	t.Run("a conversation quote with nothing to read back keeps the existing prompt", func(t *testing.T) {
		fake := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
			if call == 1 {
				return schema.AssistantMessage("", []schema.ToolCall{{
					ID:       "call-1",
					Function: schema.FunctionCall{Name: readHistoryMessageToolName, Arguments: `{"message_ids":[11]}`},
				}}), nil
			}
			return schema.AssistantMessage("你上次说的好像是「现值看白板，旧值翻档案」。", nil), nil
		}}
		runner := mustRunner(t, fake,
			StaticToolRegistry{Registered: []tool.BaseTool{
				mustHistoryReadTool(t, "", []int{11}),
				mustNamedEvidenceTool(t, knowledgeSearchToolName, groundedResults()),
			}},
			newMemoryStepRepository(), DefaultRunnerConfig())
		input := testRunContext()
		input.Question = "请逐字贴出我上次说的口诀"

		result, err := runner.Run(context.Background(), input, DiscardEvents)
		if err != nil {
			t.Fatal(err)
		}
		if result.Answer != UnsupportedHistoryAnswer {
			t.Fatalf("answer = %q, want %q", result.Answer, UnsupportedHistoryAnswer)
		}
	})

	// A file name is the topic of the learner's words, not the object of the
	// quote.
	t.Run("a file name in the topic of the learner's own words stays a history quote", func(t *testing.T) {
		for _, question := range []string{
			"我上次提到的 03-layered-summary.md 里的原话是什么？",
			"请逐字贴出我上次聊到 Redis.md 时说的口诀",
		} {
			fake := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
				if call == 1 {
					return schema.AssistantMessage("", []schema.ToolCall{{
						ID:       "call-1",
						Function: schema.FunctionCall{Name: readHistoryMessageToolName, Arguments: `{"message_ids":[11]}`},
					}}), nil
				}
				return schema.AssistantMessage("你上次说的是「现值看白板，旧值翻档案」。", nil), nil
			}}
			runner := mustRunner(t, fake,
				StaticToolRegistry{Registered: []tool.BaseTool{
					mustHistoryReadTool(t, "现值看白板，旧值翻档案", nil),
					mustNamedEvidenceTool(t, knowledgeSearchToolName, groundedResults()),
				}},
				newMemoryStepRepository(), DefaultRunnerConfig())
			input := testRunContext()
			input.Question = question

			result, err := runner.Run(context.Background(), input, DiscardEvents)
			if err != nil {
				t.Fatal(err)
			}
			if !fake.HasExclusiveToolBinding(readHistoryMessageToolName) {
				t.Fatalf("%q: tool bindings = %v, want an exclusive history read", question, fake.BoundToolCalls())
			}
			if result.AnswerBasis == rag.AnswerBasisRefused {
				t.Fatalf("%q: answer = %q, basis = %s: a grounded quotation was refused",
					question, result.Answer, result.AnswerBasis)
			}
		}
	})

	// A modifier after a comma belongs to the request it modifies.
	t.Run("a modifier after a comma belongs to the request it modifies", func(t *testing.T) {
		// "请引用《分层摘要》里的原文，逐字贴出来" is one request, and its
		// object is the document.
		documentFake := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
			if call == 1 {
				return knowledgeCall("k1"), nil
			}
			return schema.AssistantMessage("白板记录当前生效的状态，摘要段保留当时发生的变化 [1]。", nil), nil
		}}
		documentRunner := mustRunner(t, documentFake,
			StaticToolRegistry{Registered: []tool.BaseTool{
				mustHistoryReadTool(t, "", []int{11}),
				mustNamedEvidenceTool(t, knowledgeSearchToolName, groundedResults()),
			}},
			newMemoryStepRepository(), DefaultRunnerConfig())
		documentInput := testRunContext()
		documentInput.Question = "请引用《分层摘要》里的原文，逐字贴出来"

		documentResult, err := documentRunner.Run(context.Background(), documentInput, DiscardEvents)
		if err != nil {
			t.Fatal(err)
		}
		if !documentFake.HasExclusiveToolBinding(knowledgeSearchToolName) {
			t.Fatalf("tool bindings = %v, want an exclusive knowledge_search", documentFake.BoundToolCalls())
		}
		if documentResult.Answer == UnsupportedHistoryAnswer {
			t.Fatalf("answer = %q: a document question was refused for want of history evidence", documentResult.Answer)
		}
		if len(documentResult.Sources) == 0 {
			t.Fatalf("answer = %q carries no source", documentResult.Answer)
		}

		// "我上次聊到 Redis.md 时的口诀，逐字贴出来" is one request too, and
		// its object is the learner's own words.
		historyFake := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
			if call == 1 {
				return schema.AssistantMessage("", []schema.ToolCall{{
					ID:       "call-1",
					Function: schema.FunctionCall{Name: readHistoryMessageToolName, Arguments: `{"message_ids":[11]}`},
				}}), nil
			}
			return schema.AssistantMessage("你上次说的是「现值看白板，旧值翻档案」。", nil), nil
		}}
		historyRunner := mustRunner(t, historyFake,
			StaticToolRegistry{Registered: []tool.BaseTool{
				mustHistoryReadTool(t, "现值看白板，旧值翻档案", nil),
				mustNamedEvidenceTool(t, knowledgeSearchToolName, groundedResults()),
			}},
			newMemoryStepRepository(), DefaultRunnerConfig())
		historyInput := testRunContext()
		historyInput.Question = "我上次聊到 Redis.md 时的口诀，逐字贴出来"

		historyResult, err := historyRunner.Run(context.Background(), historyInput, DiscardEvents)
		if err != nil {
			t.Fatal(err)
		}
		if !historyFake.HasExclusiveToolBinding(readHistoryMessageToolName) {
			t.Fatalf("tool bindings = %v, want an exclusive history read", historyFake.BoundToolCalls())
		}
		if historyResult.AnswerBasis == rag.AnswerBasisRefused {
			t.Fatalf("answer = %q, basis = %s: a grounded quotation was refused",
				historyResult.Answer, historyResult.AnswerBasis)
		}
	})

	// A document that is only mentioned is not the object of the quote.
	//
	// "《分层摘要》是一份笔记，原话是什么？" says what the document is and
	// then asks for the learner's own words. The words asked for are the
	// learner's, so the turn reads the history back - the document in the
	// first clause is context, not the object of the quote.
	t.Run("a document that is only mentioned is not the object of the quote", func(t *testing.T) {
		question := "《分层摘要》是一份笔记，原话是什么？"
		if !requiresExactHistoryEvidence(question) {
			t.Fatalf("%q asks for the learner's own words and must require history evidence", question)
		}
		if !isHistoryRecallQuestion(question) {
			t.Fatalf("%q must be read as a question about the learner's own past", question)
		}
		fake := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
			if call == 1 {
				return schema.AssistantMessage("", []schema.ToolCall{{
					ID:       "call-1",
					Function: schema.FunctionCall{Name: readHistoryMessageToolName, Arguments: `{"message_ids":[11]}`},
				}}), nil
			}
			return schema.AssistantMessage("你当时说的是「现值看白板，旧值翻档案」。", nil), nil
		}}
		runner := mustRunner(t, fake,
			StaticToolRegistry{Registered: []tool.BaseTool{
				mustHistoryReadTool(t, "现值看白板，旧值翻档案", nil),
				mustNamedEvidenceTool(t, knowledgeSearchToolName, groundedResults()),
			}},
			newMemoryStepRepository(), DefaultRunnerConfig())
		input := testRunContext()
		input.Question = question

		result, err := runner.Run(context.Background(), input, DiscardEvents)
		if err != nil {
			t.Fatal(err)
		}
		if !fake.HasExclusiveToolBinding(readHistoryMessageToolName) {
			t.Fatalf("tool bindings = %v, want an exclusive history read", fake.BoundToolCalls())
		}
		if result.AnswerBasis == rag.AnswerBasisRefused {
			t.Fatalf("answer = %q, basis = %s: a grounded quotation was refused", result.Answer, result.AnswerBasis)
		}
	})
}

// The four sentences from the review, written into the regression before the
// rule was changed.
//
// Each one is a sentence a learner can type, and each one carries a structure
// the rule has to get right:
//
//  1. A concept question. "逐字复述" is the thing being discussed, not a
//     request to reproduce anything.
//  2. A document quotation where "上次上传" modifies the document: the object
//     being quoted is the document's definition.
//  3. A request for a message of this conversation, with the modifier "逐字"
//     after the comma: it modifies the action and the explicit object to its
//     left.
//  4. A request for a message of this conversation, named by what the learner
//     said last time.
//
// The strings live in constants so a reviewer can swap in their own wording
// without touching the rules.
const (
	reviewConceptSentence          = "我理解的逐字复述是用来核对证据的，不是用来改写内容的。"
	reviewDocumentSentence         = "请引用我上次上传的文档里白板定义的原文"
	reviewTrailingModifierSentence = "请贴出我刚才那段话，逐字"
	reviewConversationSentence     = "请逐字贴出我上次说的口诀"
)

// TestTheFourReviewSentencesAtTheRunner runs the four sentences through the
// runner and states the tool the turn bound first and the answer it returned.
//
//   - The concept question is answered, with no tool bound: "逐字复述" is the
//     subject of the sentence, not a request.
//   - The document quotation keeps the document's own evidence requirement:
//     the knowledge search is bound first, the answer rests on the document,
//     and a turn that cannot cite it is refused for that reason.
//   - The conversation quotations read the history back: the history reader is
//     bound first, the answer is accepted on that evidence, and a turn with
//     nothing to read back still shows the existing prompt.
func TestTheFourReviewSentencesAtTheRunner(t *testing.T) {
	t.Run("the concept question is answered", func(t *testing.T) {
		fake := &scriptedChatModel{respond: func(_ context.Context, _ int) (*schema.Message, error) {
			return schema.AssistantMessage("逐字复述是用来核对证据的，不改写内容。", nil), nil
		}}
		runner := mustRunner(t, fake,
			StaticToolRegistry{Registered: []tool.BaseTool{mustHistoryReadTool(t, "", []int{11})}},
			newMemoryStepRepository(), DefaultRunnerConfig())
		input := testRunContext()
		input.Question = reviewConceptSentence

		result, err := runner.Run(context.Background(), input, DiscardEvents)
		if err != nil {
			t.Fatal(err)
		}
		if result.Answer == UnsupportedHistoryAnswer || result.AnswerBasis == rag.AnswerBasisRefused {
			t.Fatalf("answer = %q, basis = %s: the concept question was refused", result.Answer, result.AnswerBasis)
		}
		if fake.ForcedToolChoiceCount() != 0 {
			t.Fatalf("the turn forced %d tool choices for a question that quotes nothing", fake.ForcedToolChoiceCount())
		}
	})

	t.Run("the document quotation keeps the document evidence requirement", func(t *testing.T) {
		search := &trackedKnowledgeSearch{}
		fake := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
			if call == 1 {
				return knowledgeCall("k1"), nil
			}
			return schema.AssistantMessage("白板记录当前生效的状态，摘要段保留当时发生的变化 [1]。", nil), nil
		}}
		runner := mustRunner(t, fake,
			StaticToolRegistry{Registered: []tool.BaseTool{
				mustHistoryReadTool(t, "", []int{11}),
				search.build(t, groundedResults()),
			}},
			newMemoryStepRepository(), DefaultRunnerConfig())
		input := testRunContext()
		input.Question = reviewDocumentSentence

		result, err := runner.Run(context.Background(), input, DiscardEvents)
		if err != nil {
			t.Fatal(err)
		}
		if !fake.HasExclusiveToolBinding(knowledgeSearchToolName) {
			t.Fatalf("tool bindings = %v, want an exclusive knowledge_search", fake.BoundToolCalls())
		}
		if result.Answer == UnsupportedHistoryAnswer {
			t.Fatalf("answer = %q: a document question was refused for want of history evidence", result.Answer)
		}
		if len(result.Sources) == 0 {
			t.Fatalf("answer = %q carries no source: the document search was not the answer's basis", result.Answer)
		}
	})

	t.Run("the document quotation without a citation is refused for that reason", func(t *testing.T) {
		fake := &scriptedChatModel{respond: func(_ context.Context, _ int) (*schema.Message, error) {
			return schema.AssistantMessage("白板记录当前生效的状态。", nil), nil
		}}
		runner := mustRunner(t, fake,
			StaticToolRegistry{Registered: []tool.BaseTool{
				mustHistoryReadTool(t, "", []int{11}),
				mustNamedEvidenceTool(t, knowledgeSearchToolName, groundedResults()),
			}},
			newMemoryStepRepository(), DefaultRunnerConfig())
		input := testRunContext()
		input.Question = reviewDocumentSentence

		result, err := runner.Run(context.Background(), input, DiscardEvents)
		if err != nil {
			t.Fatal(err)
		}
		if result.Answer == UnsupportedHistoryAnswer {
			t.Fatalf("answer = %q: the document question was refused for want of history evidence", result.Answer)
		}
		if result.AnswerBasis != rag.AnswerBasisRefused {
			t.Fatalf("answer = %q, basis = %s: want the document evidence requirement to hold", result.Answer, result.AnswerBasis)
		}
	})

	for _, question := range []string{reviewTrailingModifierSentence, reviewConversationSentence} {
		t.Run("the conversation quotation "+question+" is read back", func(t *testing.T) {
			fake := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
				if call == 1 {
					return schema.AssistantMessage("", []schema.ToolCall{{
						ID:       "call-1",
						Function: schema.FunctionCall{Name: readHistoryMessageToolName, Arguments: `{"message_ids":[11]}`},
					}}), nil
				}
				return schema.AssistantMessage("你上次说的是「现值看白板，旧值翻档案」。", nil), nil
			}}
			runner := mustRunner(t, fake,
				StaticToolRegistry{Registered: []tool.BaseTool{
					mustHistoryReadTool(t, "现值看白板，旧值翻档案", nil),
					mustNamedEvidenceTool(t, knowledgeSearchToolName, groundedResults()),
				}},
				newMemoryStepRepository(), DefaultRunnerConfig())
			input := testRunContext()
			input.Question = question

			result, err := runner.Run(context.Background(), input, DiscardEvents)
			if err != nil {
				t.Fatal(err)
			}
			if !fake.HasExclusiveToolBinding(readHistoryMessageToolName) {
				t.Fatalf("tool bindings = %v, want an exclusive history read", fake.BoundToolCalls())
			}
			if result.AnswerBasis == rag.AnswerBasisRefused {
				t.Fatalf("answer = %q, basis = %s: a grounded quotation was refused", result.Answer, result.AnswerBasis)
			}
		})

		t.Run("the conversation quotation "+question+" without evidence keeps the existing prompt", func(t *testing.T) {
			fake := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
				if call == 1 {
					return schema.AssistantMessage("", []schema.ToolCall{{
						ID:       "call-1",
						Function: schema.FunctionCall{Name: readHistoryMessageToolName, Arguments: `{"message_ids":[11]}`},
					}}), nil
				}
				return schema.AssistantMessage("你上次说的好像是「现值看白板，旧值翻档案」。", nil), nil
			}}
			runner := mustRunner(t, fake,
				StaticToolRegistry{Registered: []tool.BaseTool{
					mustHistoryReadTool(t, "", []int{11}),
					mustNamedEvidenceTool(t, knowledgeSearchToolName, groundedResults()),
				}},
				newMemoryStepRepository(), DefaultRunnerConfig())
			input := testRunContext()
			input.Question = question

			result, err := runner.Run(context.Background(), input, DiscardEvents)
			if err != nil {
				t.Fatal(err)
			}
			if result.Answer != UnsupportedHistoryAnswer {
				t.Fatalf("answer = %q, want %q", result.Answer, UnsupportedHistoryAnswer)
			}
			if result.AnswerBasis != rag.AnswerBasisRefused {
				t.Fatalf("basis = %s, want %s", result.AnswerBasis, rag.AnswerBasisRefused)
			}
		})
	}
}

// mustHistoryReadTool builds the history reader.
//
// A non-empty content makes the tool return one message the learner wrote; an
// empty content with refused ids makes it return nothing readable, which is the
// state in which no history evidence exists.
func mustHistoryReadTool(t *testing.T, content string, refused []int) tool.InvokableTool {
	t.Helper()
	built, err := toolutils.InferTool(readHistoryMessageToolName, "read the learner's own message",
		func(context.Context, *struct {
			MessageIDs []int `json:"message_ids"`
		}) (*struct {
			Messages []struct {
				MessageID int    `json:"message_id"`
				Role      string `json:"role"`
				Content   string `json:"content"`
			} `json:"messages"`
			Refused []int `json:"refused,omitempty"`
		}, error) {
			messages := []struct {
				MessageID int    `json:"message_id"`
				Role      string `json:"role"`
				Content   string `json:"content"`
			}{}
			if content != "" {
				messages = append(messages, struct {
					MessageID int    `json:"message_id"`
					Role      string `json:"role"`
					Content   string `json:"content"`
				}{MessageID: 11, Role: "user", Content: content})
			}
			return &struct {
				Messages []struct {
					MessageID int    `json:"message_id"`
					Role      string `json:"role"`
					Content   string `json:"content"`
				} `json:"messages"`
				Refused []int `json:"refused,omitempty"`
			}{Messages: messages, Refused: refused}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	return built
}
