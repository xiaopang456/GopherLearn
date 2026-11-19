package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"GopherAI/model"

	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"
)

// The clarification, end to end.
//
// The shape this defends is the one the round was measured on: the model
// cannot tell the conversations apart, it asks, the learner picks one, and
// the NEXT turn answers the question the learner asked BEFORE the
// clarification - in the conversation they picked.
//
// The learner never asks again. That is the whole promise of the flow, and
// it is broken by the two mistakes this file guards against: a turn that
// answers the reply instead of the pending question, and a turn that does
// not know which conversation was picked.

// clarificationTool is the question the model asks.
//
// It is the product's own tool shape: a tool that returns a clarification
// the runner reads out of the result.
func clarificationTool(t *testing.T, candidates []string) tool.BaseTool {
	t.Helper()
	result, err := toolutils.InferTool("ask_which_conversation", "ask which conversation",
		func(context.Context, *struct {
			Question   string   `json:"question"`
			Candidates []string `json:"candidates"`
		}) (*struct {
			Clarification *model.SessionClarification `json:"clarification"`
		}, error) {
			return &struct {
				Clarification *model.SessionClarification `json:"clarification"`
			}{Clarification: &model.SessionClarification{
				ClarificationID: "clarification-1",
				Question:        "上次你给我排的那三道题里，第二道练的是什么？",
				AllowFreeText:   true,
				Candidates: []model.SessionClarificationCandidate{
					{SessionID: candidates[0], Title: "洄漪"},
					{SessionID: candidates[1], Title: "浔漳"},
				},
			}}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// The turn that asks: the result carries the clarification, and the run
// carries it too.
func TestTheTurnThatAsksCarriesTheClarification(t *testing.T) {
	chat := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		if call == 1 {
			return schema.AssistantMessage("", []schema.ToolCall{{
				ID: "call-1",
				Function: schema.FunctionCall{Name: "ask_which_conversation",
					Arguments: `{"question":"哪一次？","candidates":["a","b"]}`},
			}}), nil
		}
		return schema.AssistantMessage("你在问哪一次？", nil), nil
	}}
	runner := mustRunner(t, chat, StaticToolRegistry{Registered: []tool.BaseTool{
		clarificationTool(t, []string{"a", "b"})}}, newMemoryStepRepository(), DefaultRunnerConfig())
	result, err := runner.Run(context.Background(), testRunContext(), DiscardEvents)
	if err != nil {
		t.Fatal(err)
	}
	if result.Clarification == nil {
		t.Fatalf("the turn carried no clarification: %+v", result)
	}
	if result.Clarification.Question != "上次你给我排的那三道题里，第二道练的是什么？" {
		t.Fatalf("the clarification carried %q", result.Clarification.Question)
	}
	if len(result.Clarification.Candidates) != 2 {
		t.Fatalf("the clarification carried %d candidates", len(result.Clarification.Candidates))
	}
}

// The block the next turn carries says what the learner chose AND which
// question was waiting.
//
// Both halves matter: without the first the model opens the wrong
// conversation, and without the second it answers the learner's reply -
// "2" - instead of the question.
func TestTheBlockSaysWhatWasChosenAndWhatWasPending(t *testing.T) {
	clarification := &model.SessionClarification{
		Question: "上次你给我排的那三道题里，第二道练的是什么？",
		Candidates: []model.SessionClarificationCandidate{
			{SessionID: "session-a", Title: "洄漪"},
			{SessionID: "session-b", Title: "浔漳"},
		},
	}
	block := clarificationBlock(clarification, "session-b")
	if !strings.Contains(block, "session-b") {
		t.Fatalf("the block does not say which conversation was chosen: %s", block)
	}
	if !strings.Contains(block, "上次你给我排的那三道题") {
		t.Fatalf("the block does not carry the pending question: %s", block)
	}
	if !strings.Contains(block, "Answer the waiting question in that conversation") {
		t.Fatalf("the block does not tell the model to answer in the chosen conversation: %s", block)
	}
}

// A reply that names none of the candidates is the MODEL'S to read.
//
// Three things it can be, and the server cannot tell them apart: a clue for
// a conversation outside the list, a pointer at the conversation in front of
// the learner, or a new question. The block therefore hands over the waiting
// question as background and the learner's own words, and says NOTHING about
// what to do with them.
//
// A block that said "search again" would send the model through the
// learner's history to answer a question the learner may have abandoned; a
// block that said "answer the waiting question" would make the model answer a
// question the learner replaced.
func TestTheBlockDoesNotTellTheModelWhatToDoWithAnUnmatchedReply(t *testing.T) {
	clarification := &model.SessionClarification{
		Question: "上次你给我排的那三道题里，第二道练的是什么？",
		Candidates: []model.SessionClarificationCandidate{
			{SessionID: "session-a", Title: "洄漪"},
		},
	}
	for _, test := range []struct {
		name  string
		reply string
	}{
		// A clue for a conversation that was NOT among the candidates.
		{"a clue outside the candidates", "周一聊 context 的那次"},
		// A pointer at the conversation in front of the learner.
		{"a pointer at this conversation", "我指这个聊天"},
		// A question that replaces the one that was waiting.
		{"a new question", "换个问题，怎么创建 channel？"},
	} {
		t.Run(test.name, func(t *testing.T) {
			block := clarificationBlock(clarification, test.reply)
			if !strings.Contains(block, test.reply) {
				t.Fatalf("the block does not carry the learner's own words %q: %s", test.reply, block)
			}
			if !strings.Contains(block, "上次你给我排的那三道题") {
				t.Fatalf("the block does not carry the pending question as background: %s", block)
			}
			for _, forbidden := range []string{
				"search again",
				"use them to search",
				"not in the list you offered",
				"Answer THAT question",
				"Answer the waiting question",
			} {
				if strings.Contains(block, forbidden) {
					t.Fatalf("the block tells the model what to do with %q (%q): %s",
						test.reply, forbidden, block)
				}
			}
		})
	}
}

// The three readings the model has to be able to make are all left open.
//
// The block names the possibilities without choosing between them: a
// different conversation, this one, or something else.
func TestTheBlockLeavesTheThreeReadingsOpen(t *testing.T) {
	clarification := &model.SessionClarification{
		Question:   "哪一次？",
		Candidates: []model.SessionClarificationCandidate{{SessionID: "session-a", Title: "洄漪"}},
	}
	block := clarificationBlock(clarification, "我指这个聊天")
	for _, phrase := range []string{"different conversation", "this one", "something else"} {
		if !strings.Contains(block, phrase) {
			t.Fatalf("the block does not leave %q open: %s", phrase, block)
		}
	}
}

// The clarification is pending only while it is unanswered.
//
// A clarification that has an answer after it was answered, and answering it
// again would answer a question the learner is no longer asking.
func TestAClarificationIsPendingOnlyWhileItIsUnanswered(t *testing.T) {
	clarification := &model.SessionClarification{
		Question:   "哪一次？",
		Candidates: []model.SessionClarificationCandidate{{SessionID: "session-a", Title: "洄漪"}},
	}
	asked := model.Message{IsUser: false, Clarification: clarification}
	answered := model.Message{IsUser: true, Content: "2"}
	after := model.Message{IsUser: false, Content: "第二道练的是持久化方式。"}

	if pendingClarification([]model.Message{asked, answered}) == nil {
		t.Fatal("an unanswered clarification is not pending")
	}
	if pendingClarification([]model.Message{asked, answered, after}) != nil {
		t.Fatal("an answered clarification is still pending")
	}
	if pendingClarification([]model.Message{{IsUser: true, Content: "2"}}) != nil {
		t.Fatal("a turn with no clarification is pending one")
	}
}

// The clarification travels with the message it was asked in.
//
// The learner answers it in their next message, and the answer has to be
// resolvable against the candidates that were offered - so the payload has
// to survive the turn.
func TestTheClarificationIsCarriedByTheMessageItWasAskedIn(t *testing.T) {
	clarification := &model.SessionClarification{
		ClarificationID: "clarification-1",
		Question:        "哪一次？",
		Candidates:      []model.SessionClarificationCandidate{{SessionID: "session-a", Title: "洄漪"}},
	}
	message := model.Message{Content: "你在问哪一次？", Clarification: clarification}
	encoded, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	var decoded model.Message
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Clarification == nil {
		t.Fatalf("the clarification did not travel with the message: %s", encoded)
	}
	if decoded.Clarification.Question != "哪一次？" {
		t.Fatalf("the message carried %+v", decoded.Clarification)
	}
}

// The card the learner is shown names the conversations and says they may
// write their own words.
func TestTheCardNamesTheConversationsAndAllowsTheirOwnWords(t *testing.T) {
	clarification := &model.SessionClarification{
		ClarificationID: "clarification-1",
		Question:        "哪一次？",
		Prompt:          "点一个，或者直接告诉我时间、标题或主题。",
		AllowFreeText:   true,
		Candidates: []model.SessionClarificationCandidate{
			{SessionID: "session-a", Title: "洄漪", Topics: []string{"慢日志与锁"}},
			{SessionID: "session-b", Title: "浔漳"},
		},
	}
	card := ClarificationCardOf(clarification)
	if card == nil || len(card.Candidates) != 2 {
		t.Fatalf("the card is %+v", card)
	}
	if card.Candidates[0].SessionID != "session-a" || card.Candidates[0].Title != "洄漪" {
		t.Fatalf("the card named %+v", card.Candidates[0])
	}
	if !card.AllowFreeText {
		t.Fatal("the card does not allow the learner to write their own words")
	}
	if card.Prompt == "" {
		t.Fatal("the card says nothing about what the learner may do")
	}
}

// The clarification block is in the window once, and it is in the budget
// once.
//
// The block travels once and the total counts it once: a block that was
// counted twice would push a turn over its target that actually fits, and a
// block that was not counted at all would let the window grow past what the
// numbers say it is.
func TestTheClarificationIsCountedOnceInTheBudget(t *testing.T) {
	clarification := &model.SessionClarification{
		Question: "上次你给我排的那三道题里，第二道练的是什么？",
		Candidates: []model.SessionClarificationCandidate{
			{SessionID: "session-a", Title: "洄漪"},
			{SessionID: "session-b", Title: "浔漳"},
		},
	}
	withBlock := &fakeHistoryReader{messages: []model.Message{
		{ID: 1, IsUser: false, Content: "你在问哪一次？", Clarification: clarification},
		{ID: 2, IsUser: true, Content: "2"},
	}}
	withoutBlock := &fakeHistoryReader{messages: []model.Message{
		{ID: 1, IsUser: false, Content: "你在问哪一次？"},
		{ID: 2, IsUser: true, Content: "2"},
	}}
	build := func(history *fakeHistoryReader) ContextWindow {
		t.Helper()
		builder, err := NewContextBuilder(testContextConfig(), history,
			staticTaskState("active learning task"), nil, staticMemories{"prefers examples"},
			staticSummary("earlier decision"))
		if err != nil {
			t.Fatal(err)
		}
		window, err := builder.Build(context.Background(),
			RunContext{UserID: 7, SessionID: "session", Question: "2"})
		if err != nil {
			t.Fatal(err)
		}
		return window
	}
	answered := build(withBlock)
	plain := build(withoutBlock)

	// The block is in the window exactly once.
	occurrences := 0
	for _, message := range answered.Messages {
		if message == nil {
			continue
		}
		if strings.Contains(message.Content, "The learner was asked which conversation they meant") {
			occurrences++
		}
	}
	if occurrences != 1 {
		t.Fatalf("the clarification block is in the window %d times, want once", occurrences)
	}
	// And it is counted once: every other line of the breakdown is in the
	// total exactly once, and the clarification is in it exactly once too.
	//
	// The sum is written out rather than derived from the builder's own
	// arithmetic, because the claim IS that arithmetic: a line counted
	// twice would push a turn over its target that actually fits, and a
	// line counted not at all would let the window grow past what the
	// numbers say it is.
	budget := answered.Budget
	other := budget.System + budget.TaskState + budget.Mastery + budget.GlobalUserMemory +
		budget.TopicUserMemory + budget.EpisodeMemory + budget.SessionCardMemory +
		budget.Summary + budget.RecentMessages + budget.CurrentQuestion +
		budget.CurrentEvidence + budget.PreRetrievalEvidence + budget.Whiteboard +
		budget.ResidentSegments + budget.RetrievedSegments
	if budget.Assembled != other+budget.Clarification {
		t.Fatalf("the assembled total is %d and the lines add up to %d plus a clarification of %d: %+v",
			budget.Assembled, other, budget.Clarification, budget)
	}
	// And it is in the projected input, which is what the ceiling is
	// checked against.
	if budget.ProjectedInput != budget.Assembled+budget.EvidenceReserved {
		t.Fatalf("projected input = %d, assembled = %d, evidence = %d",
			budget.ProjectedInput, budget.Assembled, budget.EvidenceReserved)
	}
	if budget.ProjectedInput <= plain.Budget.ProjectedInput {
		t.Fatalf("the clarification did not reach the projected input: %+v", budget)
	}
}

// A window with no room left gets no clarification, and the turn still
// happens.
//
// Failing the turn over a block of text would be the wrong trade: the
// learner asked a question and would get no answer at all. A turn without
// the explanation is a worse turn, not a failed one.
func TestAClarificationThatDoesNotFitIsDroppedAndTheTurnGoesOn(t *testing.T) {
	clarification := &model.SessionClarification{
		Question: "上次你给我排的那三道题里，第二道练的是什么？",
		Candidates: []model.SessionClarificationCandidate{
			{SessionID: "session-a", Title: "洄漪"},
		},
	}
	config := testContextConfig()
	// A summary long enough to take the room the clarification would have
	// had. The blocks before it are clamped by what is left, so a long
	// summary is what leaves nothing over.
	config.MaxSummaryTokens = 240
	long := strings.Repeat("这轮会话的滚动摘要。", 80)
	history := &fakeHistoryReader{messages: []model.Message{
		{ID: 1, IsUser: false, Content: "你在问哪一次？", Clarification: clarification},
		{ID: 2, IsUser: true, Content: "2"},
	}}
	builder, err := NewContextBuilder(config, history, staticTaskState("active learning task"),
		nil, staticMemories{"prefers examples"}, staticSummary(long))
	if err != nil {
		t.Fatal(err)
	}
	window, err := builder.Build(context.Background(),
		RunContext{UserID: 7, SessionID: "session", Question: "2"})
	if err != nil {
		t.Fatalf("the turn failed over a block of text: %v", err)
	}
	for _, message := range window.Messages {
		if message == nil {
			continue
		}
		if strings.Contains(message.Content, "The learner was asked which conversation they meant") {
			t.Fatal("a clarification that did not fit reached the window")
		}
	}
	if window.Budget.Clarification != 0 {
		t.Fatalf("a clarification that did not fit was charged: %+v", window.Budget)
	}
}
