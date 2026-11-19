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
	if !strings.Contains(block, "Answer THAT question") {
		t.Fatalf("the block does not tell the model to answer the pending question: %s", block)
	}
}

// The learner's own words are a clue, and the block says so.
//
// The learner is not forced to pick one of the offered conversations, and a
// clue that names a conversation outside the list is a better search - not a
// refusal.
func TestTheBlockSaysThatTheLearnersOwnWordsAreAClue(t *testing.T) {
	clarification := &model.SessionClarification{
		Question: "哪一次？",
		Candidates: []model.SessionClarificationCandidate{
			{SessionID: "session-a", Title: "洄漪"},
		},
	}
	block := clarificationBlock(clarification, "2026-04-04 那次，讲 HTTP 的")
	if !strings.Contains(block, "clue") {
		t.Fatalf("the block does not call the learner's own words a clue: %s", block)
	}
	if !strings.Contains(block, "not in the list you offered") {
		t.Fatalf("the block does not say that the clue may reach other conversations: %s", block)
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
