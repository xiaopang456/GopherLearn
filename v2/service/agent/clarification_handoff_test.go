package agent

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"GopherAI/model"

	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"
)

// The automatic-retrieval handoff.
//
// The acceptance run showed turns that found the conversations but could not
// settle on one: they opened candidates, ran out of retrieval, and then wrote
// a tool call into their answer. The learner saw "please retry" and had
// nothing to click. These tests pin the handoff that replaces that ending: the
// candidates and the original question become a formal clarification card, and
// the turn ends with no further model call and no tool executed.

type handoffCandidate struct {
	SessionID    string   `json:"session_id"`
	Title        string   `json:"title"`
	Topics       []string `json:"topics"`
	LastTalkedAt string   `json:"last_talked_at"`
}

type handoffCardQuery struct {
	Query string `json:"query"`
}

type handoffCardOutput struct {
	Status     string             `json:"status"`
	Reason     string             `json:"reason"`
	Candidates []handoffCandidate `json:"candidates"`
}

func mustHandoffCardTool(t *testing.T, executions *atomic.Int32, status string,
	candidates []handoffCandidate) tool.InvokableTool {
	t.Helper()
	built, err := toolutils.InferTool("search_session_cards", "find earlier conversations",
		func(context.Context, *handoffCardQuery) (*handoffCardOutput, error) {
			if executions != nil {
				executions.Add(1)
			}
			if status == "" {
				status = "ok"
			}
			return &handoffCardOutput{
				Status: status, Reason: "none", Candidates: candidates,
			}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	return built
}

func handoffCardSearchCall() *schema.Message {
	return handoffCardSearchCallWithID("call-1", "练习清单")
}

func handoffCardSearchCallWithArgs(query string) *schema.Message {
	return handoffCardSearchCallWithID("call-1", query)
}

func handoffCardSearchCallWithID(id, query string) *schema.Message {
	return schema.AssistantMessage("", []schema.ToolCall{{
		ID: id,
		Function: schema.FunctionCall{
			Name: "search_session_cards", Arguments: `{"query":"` + query + `"}`,
		},
	}})
}

func handoffEvents() (EventSink, *[]Event) {
	events := &[]Event{}
	return EventSinkFunc(func(_ context.Context, event Event) error {
		*events = append(*events, event)
		return nil
	}), events
}

func handoffRunner(t *testing.T, cardTool tool.InvokableTool) *Runner {
	t.Helper()
	chat := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		if call == 1 {
			return handoffCardSearchCall(), nil
		}
		// The malformed answer the real acceptance run produced: the model
		// wrote its next tool call into the answer text.
		return schema.AssistantMessage("<｜DSML｜ invoke name=\"browse_summary_segments\">", nil), nil
	}}
	return mustRunner(t, chat, StaticToolRegistry{Registered: []tool.BaseTool{cardTool}},
		newMemoryStepRepository(), DefaultRunnerConfig())
}

var twoCandidates = []handoffCandidate{
	{SessionID: "p2c-27", Title: "Evaluation conversation", Topics: []string{"goose 迁移流程"}, LastTalkedAt: "2026-05-07T06:31:00Z"},
	{SessionID: "p2c-28", Title: "Evaluation conversation", Topics: []string{"zoxide 入门说明"}, LastTalkedAt: "2026-05-09T06:21:00Z"},
}

func TestAFailedAutoRetrievalHandsTheQuestionBack(t *testing.T) {
	var executions atomic.Int32
	runner := handoffRunner(t, mustHandoffCardTool(t, &executions, "", twoCandidates))
	input := testRunContext()
	input.Question = "上次说写说明的时候，说明里要写清什么？"
	sink, events := handoffEvents()

	result, err := runner.Run(context.Background(), input, sink)
	if err != nil {
		t.Fatal(err)
	}
	if result.Closeout != TerminationWaitingChoice {
		t.Fatalf("the closeout is %q, want %q", result.Closeout, TerminationWaitingChoice)
	}
	if result.Answer != WaitingChoiceAnswer {
		t.Fatalf("the answer is %q, want the fixed waiting sentence", result.Answer)
	}
	if result.Clarification == nil {
		t.Fatal("the turn carried no clarification card")
	}
	if result.Clarification.Question != input.Question {
		t.Fatalf("the card carries %q, want the original question", result.Clarification.Question)
	}
	if len(result.Clarification.Candidates) != 2 {
		t.Fatalf("the card offers %d candidates, want two", len(result.Clarification.Candidates))
	}
	if result.Clarification.Candidates[0].SessionID != "p2c-27" {
		t.Fatalf("the first candidate is %+v", result.Clarification.Candidates[0])
	}
	if !result.Clarification.AllowHistoryBrowse || !result.Clarification.AllowFreeText {
		t.Fatal("the card does not offer the learner's own list and their own words")
	}
	waiting := false
	for _, event := range *events {
		if event.Type == EventWaitingChoice && event.Clarification != nil {
			waiting = true
		}
	}
	if !waiting {
		t.Fatal("the waiting-choice event did not carry the card")
	}
	if executions.Load() != 1 {
		t.Fatalf("the card search ran %d times, want once", executions.Load())
	}
	if result.ModelCalls != 2 {
		t.Fatalf("the turn made %d model calls, want the two it already spent", result.ModelCalls)
	}
}

func TestARetrievalFailureWithNoCandidatesStillOffersTheHistoryList(t *testing.T) {
	runner := handoffRunner(t, mustHandoffCardTool(t, nil, "unavailable", nil))
	result, err := runner.Run(context.Background(), testRunContext(), DiscardEvents)
	if err != nil {
		t.Fatal(err)
	}
	if result.Clarification == nil {
		t.Fatal("a failed retrieval ended with no card")
	}
	if len(result.Clarification.Candidates) != 0 {
		t.Fatalf("the card offers candidates from a failed search: %+v", result.Clarification.Candidates)
	}
	if !result.Clarification.AllowHistoryBrowse || !result.Clarification.AllowFreeText {
		t.Fatal("the empty card does not offer the learner's own list and words")
	}
	if result.Closeout != TerminationWaitingChoice {
		t.Fatalf("the closeout is %q", result.Closeout)
	}
}

func TestAProseAskWithCandidatesBecomesACard(t *testing.T) {
	chat := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		if call == 1 {
			return handoffCardSearchCall(), nil
		}
		return schema.AssistantMessage(
			"我找到两个都跟「写说明」有关的会话，但分不清是哪一次：\n- p2c-27：goose 迁移流程\n- p2c-28：zoxide 说明整理", nil), nil
	}}
	runner := mustRunner(t, chat, StaticToolRegistry{Registered: []tool.BaseTool{
		mustHandoffCardTool(t, nil, "", twoCandidates)}}, newMemoryStepRepository(), DefaultRunnerConfig())
	result, err := runner.Run(context.Background(), testRunContext(), DiscardEvents)
	if err != nil {
		t.Fatal(err)
	}
	if result.Clarification == nil {
		t.Fatalf("a prose question with candidates stayed prose: %q", result.Answer)
	}
	if len(result.Clarification.Candidates) != 2 {
		t.Fatalf("the card offers %d candidates", len(result.Clarification.Candidates))
	}
}

func TestALongAnswerThatMentionsAskingIsKept(t *testing.T) {
	long := strings.Repeat("我把那次记录能确认的要点按顺序说一遍，这些都是记录里有的内容，不是猜的。", 12)
	answer := long + "如果还有别的口径，你分不清的话可以再说，但我先按记录回答。"
	if len([]rune(answer)) <= maxProseClarificationRunes {
		t.Fatalf("the fixture is not long enough: %d runes", len([]rune(answer)))
	}
	chat := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		if call == 1 {
			return handoffCardSearchCall(), nil
		}
		return schema.AssistantMessage(answer, nil), nil
	}}
	runner := mustRunner(t, chat, StaticToolRegistry{Registered: []tool.BaseTool{
		mustHandoffCardTool(t, nil, "", twoCandidates)}}, newMemoryStepRepository(), DefaultRunnerConfig())
	result, err := runner.Run(context.Background(), testRunContext(), DiscardEvents)
	if err != nil {
		t.Fatal(err)
	}
	if result.Clarification != nil {
		t.Fatal("a long answer was turned into a card")
	}
	if result.Answer != answer {
		t.Fatalf("the long answer was rewritten: %q", result.Answer)
	}
}

func TestADirectAnswerAfterRetrievalIsNotInterrupted(t *testing.T) {
	chat := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		if call == 1 {
			return handoffCardSearchCall(), nil
		}
		return schema.AssistantMessage("说明里要写清覆盖哪些目录。", nil), nil
	}}
	runner := mustRunner(t, chat, StaticToolRegistry{Registered: []tool.BaseTool{
		mustHandoffCardTool(t, nil, "", twoCandidates)}}, newMemoryStepRepository(), DefaultRunnerConfig())
	result, err := runner.Run(context.Background(), testRunContext(), DiscardEvents)
	if err != nil {
		t.Fatal(err)
	}
	if result.Clarification != nil {
		t.Fatal("a direct answer was turned into a card")
	}
	if result.Answer != "说明里要写清覆盖哪些目录。" {
		t.Fatalf("answer=%q", result.Answer)
	}
	if result.Closeout != TerminationAnswered {
		t.Fatalf("the closeout is %q", result.Closeout)
	}
}

// A turn that already asked with the tool keeps its own card.
//
// The synthetic card exists because the model failed to ask; replacing a card
// the model DID ask for would discard its candidates and its wording.
func TestAnAlreadyAskedClarificationIsNotReplaced(t *testing.T) {
	state := &runState{
		input:                   RunContext{Question: "上次那三道题第二道是什么？"},
		clarificationCandidates: []model.SessionClarificationCandidate{{SessionID: "p2c-06"}},
		clarification: &model.SessionClarification{
			ClarificationID: "model-card", Question: "上次那三道题第二道是什么？",
			Candidates:    []model.SessionClarificationCandidate{{SessionID: "p2c-06"}},
			AllowFreeText: true, AllowHistoryBrowse: true,
		},
	}
	if got := state.clarificationHandoff(malformedToolCallAnswer); got != nil {
		t.Fatalf("a model-made card was replaced: %+v", got)
	}
}

// The card's candidate cap is enforced before the append, not after.
//
// The thin card search may be called twice, and the second search can return
// a candidate the first one did not. If the first search already filled the
// card, the extra candidate must be dropped: a ninth candidate makes the
// model's own card validator refuse the card, and the turn would end with no
// card and no list - the opposite of what the handoff is for.
func TestAFullCardDropsTheNextCandidateInsteadOfBeingRejected(t *testing.T) {
	var executions atomic.Int32
	full := make([]handoffCandidate, 0, maxClarificationCandidates)
	for index := 1; index <= maxClarificationCandidates; index++ {
		full = append(full, handoffCandidate{
			SessionID:    "p2c-" + string(rune('0'+index)),
			Title:        "Evaluation conversation",
			Topics:       []string{"话题"},
			LastTalkedAt: "2026-05-09T06:21:00Z",
		})
	}
	extra := handoffCandidate{SessionID: "p2c-99", Title: "Evaluation conversation",
		Topics: []string{"另一个候选"}, LastTalkedAt: "2026-05-09T06:22:00Z"}
	cardTool, err := toolutils.InferTool("search_session_cards", "find earlier conversations",
		func(context.Context, *handoffCardQuery) (*handoffCardOutput, error) {
			if executions.Add(1) == 1 {
				return &handoffCardOutput{Status: "ok", Reason: "none", Candidates: full}, nil
			}
			return &handoffCardOutput{Status: "ok", Reason: "none", Candidates: []handoffCandidate{extra}}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	chat := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		switch call {
		case 1:
			return handoffCardSearchCallWithID("call-1", "练习清单"), nil
		case 2:
			return handoffCardSearchCallWithID("call-2", "练习清单 说明"), nil
		default:
			return schema.AssistantMessage("<｜DSML｜ invoke name=\"browse_summary_segments\">", nil), nil
		}
	}}
	runner := mustRunner(t, chat, StaticToolRegistry{Registered: []tool.BaseTool{cardTool}},
		newMemoryStepRepository(), DefaultRunnerConfig())
	result, err := runner.Run(context.Background(), testRunContext(), DiscardEvents)
	if err != nil {
		t.Fatal(err)
	}
	if executions.Load() != 2 {
		t.Fatalf("the card search ran %d times, want two", executions.Load())
	}
	if result.Clarification == nil {
		t.Fatal("a full first search plus one new candidate produced no card")
	}
	if len(result.Clarification.Candidates) != maxClarificationCandidates {
		t.Fatalf("the card offers %d candidates, want the cap %d",
			len(result.Clarification.Candidates), maxClarificationCandidates)
	}
	for _, candidate := range result.Clarification.Candidates {
		if candidate.SessionID == extra.SessionID {
			t.Fatalf("the candidate beyond the cap was kept: %+v", candidate)
		}
	}
	if !result.Clarification.AllowHistoryBrowse || !result.Clarification.AllowFreeText {
		t.Fatal("the card does not keep the learner's own list and words")
	}
	if result.Closeout != TerminationWaitingChoice {
		t.Fatalf("the closeout is %q, want waiting_for_choice", result.Closeout)
	}
}

func TestTheHandoffCardCarriesTheQuestionTheModelWasAnswering(t *testing.T) {
	card := &runState{
		input: RunContext{RunID: "run-1", Question: "上次那份练习清单，单项里最后定的是写什么？"},
		clarificationCandidates: []model.SessionClarificationCandidate{
			{SessionID: "p2c-29"}, {SessionID: "p2c-30"},
		},
	}
	got := card.clarificationHandoff(malformedToolCallAnswer)
	if got == nil {
		t.Fatal("no card for an unusable answer with candidates")
	}
	if got.Question != "上次那份练习清单，单项里最后定的是写什么？" {
		t.Fatalf("the card carries %q", got.Question)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("the synthesized card is not valid: %v", err)
	}
}

// A prose list of candidates is not an answer: the model found several
// exercise lists, wrote them into its answer, and never called the ask tool.
// The product has to hand the learner the same formal card anyway.
func TestAProseCandidateListBecomesAFormalCard(t *testing.T) {
	var executions atomic.Int32
	chat := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		if call == 1 {
			return handoffCardSearchCallWithArgs("练习单子 清单 分组"), nil
		}
		return schema.AssistantMessage("“上次那份练习单”我这边对不上唯一的一份：\n\n"+
			"| 会话 | 练习清单 |\n|---|---|\n"+
			"| p2c-27 | goose 迁移流程 |\n| p2c-28 | zoxide 入门说明 |\n\n"+
			"这两份都可能是你说的，而分开列的维度大概率不一样，我不想替你猜。", nil), nil
	}}
	runner := mustRunner(t, chat, StaticToolRegistry{Registered: []tool.BaseTool{
		mustHandoffCardTool(t, &executions, "", twoCandidates)}},
		newMemoryStepRepository(), DefaultRunnerConfig())
	input := testRunContext()
	input.Question = "上次那份练习单子，你当时按哪几样东西分开列的？"
	result, err := runner.Run(context.Background(), input, DiscardEvents)
	if err != nil {
		t.Fatal(err)
	}
	if result.Clarification == nil {
		t.Fatalf("a prose candidate list was published as the answer: %q", result.Answer)
	}
	if result.Answer != WaitingChoiceAnswer {
		t.Fatalf("answer=%q, want the waiting sentence", result.Answer)
	}
	card := result.Clarification
	if card.Question != input.Question {
		t.Fatalf("the card lost the original question: %q", card.Question)
	}
	if len(card.Candidates) != 2 {
		t.Fatalf("the card offers %d candidates", len(card.Candidates))
	}
	if !card.AllowHistoryBrowse || !card.AllowFreeText {
		t.Fatal("the card does not offer the learner's own list and words")
	}
}

// A direct answer that names no candidates is not turned into a card, even
// when the card search offered several conversations.
func TestADirectAnswerWithSeveralCandidatesIsNotACard(t *testing.T) {
	var executions atomic.Int32
	chat := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		if call == 1 {
			return handoffCardSearchCallWithArgs("练习单子 清单 分组"), nil
		}
		return schema.AssistantMessage("答案是动作、提问、验收。", nil), nil
	}}
	runner := mustRunner(t, chat, StaticToolRegistry{Registered: []tool.BaseTool{
		mustHandoffCardTool(t, &executions, "", twoCandidates)}},
		newMemoryStepRepository(), DefaultRunnerConfig())
	input := testRunContext()
	input.Question = "上次那份练习单子，你当时按哪几样东西分开列的？"
	result, err := runner.Run(context.Background(), input, DiscardEvents)
	if err != nil {
		t.Fatal(err)
	}
	if result.Clarification != nil {
		t.Fatalf("a direct answer was turned into a card: %+v", result.Clarification)
	}
	if result.Answer != "答案是动作、提问、验收。" {
		t.Fatalf("answer=%q", result.Answer)
	}
}
