package agent

import (
	"context"
	"fmt"
	"testing"

	"GopherAI/common/rag"

	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"
)

// The A/B harness for the history-intent fix.
//
// This file is copied unchanged into the baseline worktree and into the
// candidate worktree. It asserts nothing on purpose: its job is to record what
// the two builds do with the same four questions, the same fake model, the
// same tools and the same initial state, so the two reports can be compared
// line by line.
//
// Every model here is the repository's own scripted fake. No paid model is
// called, and nothing in this report is a statement about how a real model
// behaves.

// abStatement is the learner's concept statement about the layered summary.
//
// It mentions 原话, and the phrase table the runner used to keep read that as
// a request for the learner's own words.
const abStatement = "我现在对分层摘要的理解是：白板记录当前生效的状态，摘要段保留当时发生的变化，原话用来核对证据。我用一句口诀区分它们：「现值看白板，旧值翻档案」。我最容易搞混的是把摘要段里的旧值当成现值。"

// abDocumentQuote asks for a document's wording.
const abDocumentQuote = "请引用《03-layered-summary.md》里白板定义的原文"

// abVerbatimRecall asks for an earlier message, word for word.
const abVerbatimRecall = "请逐字贴出我上次说的口诀"

// abFileNameHistoryRecall asks for the learner's own words about a file. The
// file name is the topic of those words, not the thing being quoted.
const abFileNameHistoryRecall = "我上次提到的 03-layered-summary.md 里的原话是什么？"

// abChineseTitleQuote asks for a document's wording, and names the document in
// Chinese.
const abChineseTitleQuote = "请引用《分层摘要》里白板定义的原文"

// abRedisFileRecall asks for the learner's own words, and mentions a file as
// the topic of those words.
const abRedisFileRecall = "请逐字贴出我上次聊到 Redis.md 时说的口诀"

// abModifierDocument asks for a document's wording and puts the modifier
// ("逐字") after a comma.
const abModifierDocument = "请引用《分层摘要》里的原文，逐字贴出来"

// abModifierHistory asks for the learner's own words and puts the modifier
// ("逐字") after a comma.
const abModifierHistory = "我上次聊到 Redis.md 时的口诀，逐字贴出来"

// The four sentences from the review.
const (
	// abReviewConcept discusses quoting as a subject.
	abReviewConcept = "我理解的逐字复述是用来核对证据的，不是用来改写内容的。"
	// abReviewDocument quotes a document the learner uploaded last time.
	abReviewDocument = "请引用我上次上传的文档里白板定义的原文"
	// abReviewTrailing asks for a message of this conversation and puts the
	// modifier after the comma.
	abReviewTrailing = "请贴出我刚才那段话，逐字"
)

func TestHistoryIntentABReport(t *testing.T) {
	// A. The concept statement. The knowledge base and the topic memory are
	// both off, so the history reader is the only tool the turn has.
	statementState := &runState{
		config:               DefaultRunnerConfig(),
		input:                RunContext{Question: abStatement},
		historyReadAvailable: true,
	}
	statementFake := &scriptedChatModel{respond: func(_ context.Context, _ int) (*schema.Message, error) {
		return schema.AssistantMessage("白板记录当前生效的状态，摘要段保留当时发生的变化。", nil), nil
	}}
	statementRunner := mustRunner(t, statementFake,
		StaticToolRegistry{Registered: []tool.BaseTool{abHistoryTool(t, "", []int{11})}},
		newMemoryStepRepository(), DefaultRunnerConfig())
	statementOutcome, statementBasis, statementAnswer := abRun(t, statementRunner, abStatement)

	// B. The document quotation. The knowledge search is the tool this
	// question needs.
	documentState := &runState{
		config:                   DefaultRunnerConfig(),
		input:                    RunContext{Question: abDocumentQuote},
		knowledgeSearchAvailable: true,
		knowledgePolicy:          rag.KnowledgePreferred,
	}
	documentSearch := &trackedKnowledgeSearch{}
	documentFake := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		if call == 1 {
			return knowledgeCall("k1"), nil
		}
		return schema.AssistantMessage("白板记录当前生效的状态，摘要段保留当时发生的变化 [1]。", nil), nil
	}}
	documentRunner := mustRunner(t, documentFake,
		StaticToolRegistry{Registered: []tool.BaseTool{documentSearch.build(t, groundedResults())}},
		newMemoryStepRepository(), DefaultRunnerConfig())
	documentOutcome, documentBasis, documentAnswer := abRun(t, documentRunner, abDocumentQuote)

	// C. The verbatim recall, with the learner's own words behind it.
	recallState := &runState{
		config:               DefaultRunnerConfig(),
		input:                RunContext{Question: abVerbatimRecall},
		historyReadAvailable: true,
	}
	recallFake := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		if call == 1 {
			return schema.AssistantMessage("", []schema.ToolCall{{
				ID:       "call-1",
				Function: schema.FunctionCall{Name: readHistoryMessageToolName, Arguments: `{"message_ids":[11]}`},
			}}), nil
		}
		return schema.AssistantMessage("你上次说的是「现值看白板，旧值翻档案」。", nil), nil
	}}
	recallRunner := mustRunner(t, recallFake,
		StaticToolRegistry{Registered: []tool.BaseTool{abHistoryTool(t, "现值看白板，旧值翻档案", nil)}},
		newMemoryStepRepository(), DefaultRunnerConfig())
	recallOutcome, recallBasis, recallAnswer := abRun(t, recallRunner, abVerbatimRecall)

	// D. The same verbatim recall with nothing behind it.
	emptyFake := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		if call == 1 {
			return schema.AssistantMessage("", []schema.ToolCall{{
				ID:       "call-1",
				Function: schema.FunctionCall{Name: readHistoryMessageToolName, Arguments: `{"message_ids":[11]}`},
			}}), nil
		}
		return schema.AssistantMessage("你上次说的好像是「现值看白板，旧值翻档案」。", nil), nil
	}}
	emptyRunner := mustRunner(t, emptyFake,
		StaticToolRegistry{Registered: []tool.BaseTool{abHistoryTool(t, "", []int{11})}},
		newMemoryStepRepository(), DefaultRunnerConfig())
	emptyOutcome, emptyBasis, emptyAnswer := abRun(t, emptyRunner, abVerbatimRecall)

	// B2. The document quotation again, with a model that does not reach for
	// the search on its own. This is the case the first tool choice decides:
	// when the server binds the document search, the turn reaches the
	// material whatever the model does, and when it does not, the turn is at
	// the mercy of what the model happened to choose.
	documentPlainFake := &scriptedChatModel{respond: func(_ context.Context, _ int) (*schema.Message, error) {
		return schema.AssistantMessage("白板记录当前生效的状态，摘要段保留当时发生的变化。", nil), nil
	}}
	documentPlainRunner := mustRunner(t, documentPlainFake,
		StaticToolRegistry{Registered: []tool.BaseTool{documentSearch.build(t, groundedResults())}},
		newMemoryStepRepository(), DefaultRunnerConfig())
	documentPlainOutcome, documentPlainBasis, documentPlainAnswer := abRun(t, documentPlainRunner, abDocumentQuote)

	abReport(t, "A_statement", abStatement, statementState, statementOutcome, statementBasis, statementAnswer)
	abReport(t, "B_document_quote", abDocumentQuote, documentState, documentOutcome, documentBasis, documentAnswer)
	abReport(t, "B2_document_quote_model_answers_directly", abDocumentQuote, documentState,
		documentPlainOutcome, documentPlainBasis, documentPlainAnswer)
	abReport(t, "C_verbatim_recall_with_evidence", abVerbatimRecall, recallState, recallOutcome, recallBasis, recallAnswer)
	abReport(t, "D_verbatim_recall_without_evidence", abVerbatimRecall, recallState, emptyOutcome, emptyBasis, emptyAnswer)

	// H. A history request whose file name is the topic of the learner's
	// words. The file name is not the thing being quoted.
	fileNameState := &runState{
		config:               DefaultRunnerConfig(),
		input:                RunContext{Question: abFileNameHistoryRecall},
		historyReadAvailable: true,
	}
	fileNameFake := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		if call == 1 {
			return schema.AssistantMessage("", []schema.ToolCall{{
				ID:       "call-1",
				Function: schema.FunctionCall{Name: readHistoryMessageToolName, Arguments: `{"message_ids":[11]}`},
			}}), nil
		}
		return schema.AssistantMessage("你上次说的是「现值看白板，旧值翻档案」。", nil), nil
	}}
	fileNameRunner := mustRunner(t, fileNameFake,
		StaticToolRegistry{Registered: []tool.BaseTool{
			abHistoryTool(t, "现值看白板，旧值翻档案", nil),
			mustNamedEvidenceTool(t, knowledgeSearchToolName, groundedResults()),
		}},
		newMemoryStepRepository(), DefaultRunnerConfig())
	fileNameOutcome, fileNameBasis, fileNameAnswer := abRun(t, fileNameRunner, abFileNameHistoryRecall)

	fileNameEmptyFake := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		if call == 1 {
			return schema.AssistantMessage("", []schema.ToolCall{{
				ID:       "call-1",
				Function: schema.FunctionCall{Name: readHistoryMessageToolName, Arguments: `{"message_ids":[11]}`},
			}}), nil
		}
		return schema.AssistantMessage("你上次说的好像是「现值看白板，旧值翻档案」。", nil), nil
	}}
	fileNameEmptyRunner := mustRunner(t, fileNameEmptyFake,
		StaticToolRegistry{Registered: []tool.BaseTool{
			abHistoryTool(t, "", []int{11}),
			mustNamedEvidenceTool(t, knowledgeSearchToolName, groundedResults()),
		}},
		newMemoryStepRepository(), DefaultRunnerConfig())
	fileNameEmptyOutcome, fileNameEmptyBasis, fileNameEmptyAnswer := abRun(t, fileNameEmptyRunner, abFileNameHistoryRecall)

	abReport(t, "H_file_name_history_recall_with_evidence", abFileNameHistoryRecall, fileNameState,
		fileNameOutcome, fileNameBasis, fileNameAnswer)
	abReport(t, "H2_file_name_history_recall_without_evidence", abFileNameHistoryRecall, fileNameState,
		fileNameEmptyOutcome, fileNameEmptyBasis, fileNameEmptyAnswer)

	// I. A Chinese title: the document the learner wants quoted.
	chineseTitleState := &runState{
		config:                   DefaultRunnerConfig(),
		input:                    RunContext{Question: abChineseTitleQuote},
		knowledgeSearchAvailable: true,
		knowledgePolicy:          rag.KnowledgePreferred,
		historyReadAvailable:     true,
	}
	chineseSearch := &trackedKnowledgeSearch{}
	chineseFake := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		if call == 1 {
			return knowledgeCall("k1"), nil
		}
		return schema.AssistantMessage("白板记录当前生效的状态，摘要段保留当时发生的变化 [1]。", nil), nil
	}}
	chineseRunner := mustRunner(t, chineseFake,
		StaticToolRegistry{Registered: []tool.BaseTool{
			abHistoryTool(t, "", []int{11}),
			chineseSearch.build(t, groundedResults()),
		}},
		newMemoryStepRepository(), DefaultRunnerConfig())
	chineseOutcome, chineseBasis, chineseAnswer := abRun(t, chineseRunner, abChineseTitleQuote)

	chinesePlainFake := &scriptedChatModel{respond: func(_ context.Context, _ int) (*schema.Message, error) {
		return schema.AssistantMessage("白板记录当前生效的状态，摘要段保留当时发生的变化。", nil), nil
	}}
	chinesePlainRunner := mustRunner(t, chinesePlainFake,
		StaticToolRegistry{Registered: []tool.BaseTool{
			abHistoryTool(t, "", []int{11}),
			chineseSearch.build(t, groundedResults()),
		}},
		newMemoryStepRepository(), DefaultRunnerConfig())
	chinesePlainOutcome, chinesePlainBasis, chinesePlainAnswer := abRun(t, chinesePlainRunner, abChineseTitleQuote)

	abReport(t, "I_chinese_title_document_quote", abChineseTitleQuote, chineseTitleState,
		chineseOutcome, chineseBasis, chineseAnswer)
	abReport(t, "I2_chinese_title_document_quote_model_answers_directly", abChineseTitleQuote, chineseTitleState,
		chinesePlainOutcome, chinesePlainBasis, chinesePlainAnswer)

	// J. A file name as the topic of a verbatim recall of the learner's own
	// words.
	redisState := &runState{
		config:               DefaultRunnerConfig(),
		input:                RunContext{Question: abRedisFileRecall},
		historyReadAvailable: true,
	}
	redisFake := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		if call == 1 {
			return schema.AssistantMessage("", []schema.ToolCall{{
				ID:       "call-1",
				Function: schema.FunctionCall{Name: readHistoryMessageToolName, Arguments: `{"message_ids":[11]}`},
			}}), nil
		}
		return schema.AssistantMessage("你上次说的是「现值看白板，旧值翻档案」。", nil), nil
	}}
	redisRunner := mustRunner(t, redisFake,
		StaticToolRegistry{Registered: []tool.BaseTool{
			abHistoryTool(t, "现值看白板，旧值翻档案", nil),
			mustNamedEvidenceTool(t, knowledgeSearchToolName, groundedResults()),
		}},
		newMemoryStepRepository(), DefaultRunnerConfig())
	redisOutcome, redisBasis, redisAnswer := abRun(t, redisRunner, abRedisFileRecall)

	redisEmptyFake := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		if call == 1 {
			return schema.AssistantMessage("", []schema.ToolCall{{
				ID:       "call-1",
				Function: schema.FunctionCall{Name: readHistoryMessageToolName, Arguments: `{"message_ids":[11]}`},
			}}), nil
		}
		return schema.AssistantMessage("你上次说的好像是「现值看白板，旧值翻档案」。", nil), nil
	}}
	redisEmptyRunner := mustRunner(t, redisEmptyFake,
		StaticToolRegistry{Registered: []tool.BaseTool{
			abHistoryTool(t, "", []int{11}),
			mustNamedEvidenceTool(t, knowledgeSearchToolName, groundedResults()),
		}},
		newMemoryStepRepository(), DefaultRunnerConfig())
	redisEmptyOutcome, redisEmptyBasis, redisEmptyAnswer := abRun(t, redisEmptyRunner, abRedisFileRecall)

	abReport(t, "J_redis_file_history_recall_with_evidence", abRedisFileRecall, redisState,
		redisOutcome, redisBasis, redisAnswer)
	abReport(t, "J2_redis_file_history_recall_without_evidence", abRedisFileRecall, redisState,
		redisEmptyOutcome, redisEmptyBasis, redisEmptyAnswer)

	// K. A modifier after a comma, on a document request.
	modifierDocumentState := &runState{
		config:                   DefaultRunnerConfig(),
		input:                    RunContext{Question: abModifierDocument},
		knowledgeSearchAvailable: true,
		knowledgePolicy:          rag.KnowledgePreferred,
		historyReadAvailable:     true,
	}
	modifierDocumentFake := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		if call == 1 {
			return knowledgeCall("k1"), nil
		}
		return schema.AssistantMessage("白板记录当前生效的状态，摘要段保留当时发生的变化 [1]。", nil), nil
	}}
	modifierDocumentRunner := mustRunner(t, modifierDocumentFake,
		StaticToolRegistry{Registered: []tool.BaseTool{
			abHistoryTool(t, "", []int{11}),
			mustNamedEvidenceTool(t, knowledgeSearchToolName, groundedResults()),
		}},
		newMemoryStepRepository(), DefaultRunnerConfig())
	modifierDocumentOutcome, modifierDocumentBasis, modifierDocumentAnswer := abRun(t, modifierDocumentRunner, abModifierDocument)

	modifierDocumentPlainFake := &scriptedChatModel{respond: func(_ context.Context, _ int) (*schema.Message, error) {
		return schema.AssistantMessage("白板记录当前生效的状态，摘要段保留当时发生的变化。", nil), nil
	}}
	modifierDocumentPlainRunner := mustRunner(t, modifierDocumentPlainFake,
		StaticToolRegistry{Registered: []tool.BaseTool{
			abHistoryTool(t, "", []int{11}),
			mustNamedEvidenceTool(t, knowledgeSearchToolName, groundedResults()),
		}},
		newMemoryStepRepository(), DefaultRunnerConfig())
	modifierDocumentPlainOutcome, modifierDocumentPlainBasis, modifierDocumentPlainAnswer :=
		abRun(t, modifierDocumentPlainRunner, abModifierDocument)

	abReport(t, "K_modifier_after_comma_document_quote", abModifierDocument, modifierDocumentState,
		modifierDocumentOutcome, modifierDocumentBasis, modifierDocumentAnswer)
	abReport(t, "K2_modifier_after_comma_document_quote_model_answers_directly", abModifierDocument,
		modifierDocumentState, modifierDocumentPlainOutcome, modifierDocumentPlainBasis, modifierDocumentPlainAnswer)

	// L. A modifier after a comma, on a request for the learner's own words.
	modifierHistoryState := &runState{
		config:               DefaultRunnerConfig(),
		input:                RunContext{Question: abModifierHistory},
		historyReadAvailable: true,
	}
	modifierHistoryFake := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		if call == 1 {
			return schema.AssistantMessage("", []schema.ToolCall{{
				ID:       "call-1",
				Function: schema.FunctionCall{Name: readHistoryMessageToolName, Arguments: `{"message_ids":[11]}`},
			}}), nil
		}
		return schema.AssistantMessage("你上次说的是「现值看白板，旧值翻档案」。", nil), nil
	}}
	modifierHistoryRunner := mustRunner(t, modifierHistoryFake,
		StaticToolRegistry{Registered: []tool.BaseTool{
			abHistoryTool(t, "现值看白板，旧值翻档案", nil),
			mustNamedEvidenceTool(t, knowledgeSearchToolName, groundedResults()),
		}},
		newMemoryStepRepository(), DefaultRunnerConfig())
	modifierHistoryOutcome, modifierHistoryBasis, modifierHistoryAnswer := abRun(t, modifierHistoryRunner, abModifierHistory)

	abReport(t, "L_modifier_after_comma_history_quote", abModifierHistory, modifierHistoryState,
		modifierHistoryOutcome, modifierHistoryBasis, modifierHistoryAnswer)

	// R. The four sentences from the review.
	//
	// R1 is a concept question whose subject is quoting itself. R2 quotes a
	// document the learner uploaded last time. R3 asks for a message of this
	// conversation with the modifier after the comma. R4 is C above.
	reviewConceptState := &runState{
		config:               DefaultRunnerConfig(),
		input:                RunContext{Question: abReviewConcept},
		historyReadAvailable: true,
	}
	reviewConceptFake := &scriptedChatModel{respond: func(_ context.Context, _ int) (*schema.Message, error) {
		return schema.AssistantMessage("逐字复述是用来核对证据的，不改写内容。", nil), nil
	}}
	reviewConceptRunner := mustRunner(t, reviewConceptFake,
		StaticToolRegistry{Registered: []tool.BaseTool{abHistoryTool(t, "", []int{11})}},
		newMemoryStepRepository(), DefaultRunnerConfig())
	reviewConceptOutcome, reviewConceptBasis, reviewConceptAnswer := abRun(t, reviewConceptRunner, abReviewConcept)

	reviewDocumentState := &runState{
		config:                   DefaultRunnerConfig(),
		input:                    RunContext{Question: abReviewDocument},
		knowledgeSearchAvailable: true,
		knowledgePolicy:          rag.KnowledgePreferred,
		historyReadAvailable:     true,
	}
	reviewDocumentSearch := &trackedKnowledgeSearch{}
	reviewDocumentFake := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		if call == 1 {
			return knowledgeCall("k1"), nil
		}
		return schema.AssistantMessage("白板记录当前生效的状态，摘要段保留当时发生的变化 [1]。", nil), nil
	}}
	reviewDocumentRunner := mustRunner(t, reviewDocumentFake,
		StaticToolRegistry{Registered: []tool.BaseTool{
			abHistoryTool(t, "", []int{11}),
			reviewDocumentSearch.build(t, groundedResults()),
		}},
		newMemoryStepRepository(), DefaultRunnerConfig())
	reviewDocumentOutcome, reviewDocumentBasis, reviewDocumentAnswer := abRun(t, reviewDocumentRunner, abReviewDocument)

	reviewDocumentPlainFake := &scriptedChatModel{respond: func(_ context.Context, _ int) (*schema.Message, error) {
		return schema.AssistantMessage("白板记录当前生效的状态。", nil), nil
	}}
	reviewDocumentPlainRunner := mustRunner(t, reviewDocumentPlainFake,
		StaticToolRegistry{Registered: []tool.BaseTool{
			abHistoryTool(t, "", []int{11}),
			reviewDocumentSearch.build(t, groundedResults()),
		}},
		newMemoryStepRepository(), DefaultRunnerConfig())
	reviewDocumentPlainOutcome, reviewDocumentPlainBasis, reviewDocumentPlainAnswer :=
		abRun(t, reviewDocumentPlainRunner, abReviewDocument)

	reviewTrailingState := &runState{
		config:               DefaultRunnerConfig(),
		input:                RunContext{Question: abReviewTrailing},
		historyReadAvailable: true,
	}
	reviewTrailingFake := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		if call == 1 {
			return schema.AssistantMessage("", []schema.ToolCall{{
				ID:       "call-1",
				Function: schema.FunctionCall{Name: readHistoryMessageToolName, Arguments: `{"message_ids":[11]}`},
			}}), nil
		}
		return schema.AssistantMessage("你刚才说的是「现值看白板，旧值翻档案」。", nil), nil
	}}
	reviewTrailingRunner := mustRunner(t, reviewTrailingFake,
		StaticToolRegistry{Registered: []tool.BaseTool{
			abHistoryTool(t, "现值看白板，旧值翻档案", nil),
			mustNamedEvidenceTool(t, knowledgeSearchToolName, groundedResults()),
		}},
		newMemoryStepRepository(), DefaultRunnerConfig())
	reviewTrailingOutcome, reviewTrailingBasis, reviewTrailingAnswer := abRun(t, reviewTrailingRunner, abReviewTrailing)

	reviewTrailingEmptyFake := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		if call == 1 {
			return schema.AssistantMessage("", []schema.ToolCall{{
				ID:       "call-1",
				Function: schema.FunctionCall{Name: readHistoryMessageToolName, Arguments: `{"message_ids":[11]}`},
			}}), nil
		}
		return schema.AssistantMessage("你刚才说的好像是「现值看白板，旧值翻档案」。", nil), nil
	}}
	reviewTrailingEmptyRunner := mustRunner(t, reviewTrailingEmptyFake,
		StaticToolRegistry{Registered: []tool.BaseTool{
			abHistoryTool(t, "", []int{11}),
			mustNamedEvidenceTool(t, knowledgeSearchToolName, groundedResults()),
		}},
		newMemoryStepRepository(), DefaultRunnerConfig())
	reviewTrailingEmptyOutcome, reviewTrailingEmptyBasis, reviewTrailingEmptyAnswer :=
		abRun(t, reviewTrailingEmptyRunner, abReviewTrailing)

	abReport(t, "R1_review_concept_sentence", abReviewConcept, reviewConceptState,
		reviewConceptOutcome, reviewConceptBasis, reviewConceptAnswer)
	abReport(t, "R2_review_document_sentence", abReviewDocument, reviewDocumentState,
		reviewDocumentOutcome, reviewDocumentBasis, reviewDocumentAnswer)
	abReport(t, "R3_review_document_sentence_model_answers_directly", abReviewDocument, reviewDocumentState,
		reviewDocumentPlainOutcome, reviewDocumentPlainBasis, reviewDocumentPlainAnswer)
	abReport(t, "R4_review_trailing_modifier_sentence", abReviewTrailing, reviewTrailingState,
		reviewTrailingOutcome, reviewTrailingBasis, reviewTrailingAnswer)
	abReport(t, "R5_review_trailing_modifier_sentence_without_evidence", abReviewTrailing, reviewTrailingState,
		reviewTrailingEmptyOutcome, reviewTrailingEmptyBasis, reviewTrailingEmptyAnswer)

	// The two questions the fix had to leave alone.
	for _, pinned := range []struct{ id, question string }{
		{"E_pinned_own_words", "我的原话是什么？"},
		{"F_pinned_verbatim_instruction", "把我的原话一字不差地说出来。"},
		{"G_pinned_ordinary_question", "解释一下 TCP 粘包"},
	} {
		state := &runState{
			config:               DefaultRunnerConfig(),
			input:                RunContext{Question: pinned.question},
			historyReadAvailable: true,
		}
		abReport(t, pinned.id, pinned.question, state, "-", "-", "-")
	}
}

// abReport prints one comparable line.
func abReport(t *testing.T, id, question string, state *runState, outcome, basis, answer string) {
	t.Helper()
	fmt.Printf("AB case=%s exact=%v recall=%v first_tool=%s outcome=%s basis=%s answer=%q\n",
		id,
		requiresExactHistoryEvidence(question),
		isHistoryRecallQuestion(question),
		abFirstTool(state),
		outcome, basis, answer)
}

// abFirstTool reads the first tool the turn would bind.
func abFirstTool(state *runState) string {
	forced := state.forcedToolName()
	if forced == "" {
		return "none"
	}
	return forced
}

// abRun runs one turn and reports what came back.
func abRun(t *testing.T, runner *Runner, question string) (string, string, string) {
	t.Helper()
	input := testRunContext()
	input.Question = question
	result, err := runner.Run(context.Background(), input, DiscardEvents)
	if err != nil {
		return "run_error", "-", err.Error()
	}
	switch {
	case result.Answer == UnsupportedHistoryAnswer:
		return "refused_history", string(result.AnswerBasis), result.Answer
	case result.AnswerBasis == rag.AnswerBasisRefused:
		return "refused_other", string(result.AnswerBasis), result.Answer
	case len(result.Sources) > 0:
		return "answered_with_document_evidence", string(result.AnswerBasis), result.Answer
	default:
		return "answered", string(result.AnswerBasis), result.Answer
	}
}

// abHistoryTool builds the history reader for the harness.
//
// A non-empty content makes the tool return one message the learner wrote; an
// empty content with refused ids makes it return nothing readable.
func abHistoryTool(t *testing.T, content string, refused []int) tool.InvokableTool {
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
