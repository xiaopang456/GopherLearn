package agenttool

import (
	"context"
	"encoding/json"
	"testing"

	"GopherAI/model"
	serviceagent "GopherAI/service/agent"
	"GopherAI/service/episode"

	"github.com/cloudwego/eino/components/tool"
)

// The clarification.
//
// The candidates of one learner look alike from the outside: several
// conversations may each have arranged three exercises, and the question
// names none of them. The model finds them all, cannot pick one, and asks.
//
// These tests cover the two halves of that: what the tool offers, and what
// the learner's answer means.

// clarifyingRetriever is a card index with conversations in it.
type clarifyingRetriever struct {
	candidates []episode.ThinCardCandidate
	asked      []string
}

func (f *clarifyingRetriever) Recall(_ context.Context, _ int64, _ string,
	_ episode.ThinCardQuery) ([]episode.ThinCardCandidate, error) {
	return f.candidates, nil
}

func (f *clarifyingRetriever) CardOfSession(_ context.Context, _ int64,
	sessionID string) (episode.ThinCardCandidate, bool, error) {
	f.asked = append(f.asked, sessionID)
	for _, candidate := range f.candidates {
		if candidate.SessionID == sessionID {
			return candidate, true, nil
		}
	}
	return episode.ThinCardCandidate{}, false, nil
}

// clarifyingRun is a run of a learner with a history.
func clarifyingRun(question string) serviceagent.RunContext {
	run := serviceagent.RunContext{UserID: 7, SessionID: "current-session",
		Question: question, CurrentSessionHasHistory: true}
	run.GrantSet = serviceagent.NewHistoryGrantSet(run, nil)
	return run
}

// askWhich runs the tool with one question.
func askWhich(t *testing.T, registry *AskWhichConversationRegistry, run serviceagent.RunContext,
	question string, candidates []string) *AskWhichConversationOutput {
	t.Helper()
	tools, err := registry.Tools(context.Background(), run)
	if err != nil {
		t.Fatalf("build the tool: %v", err)
	}
	if len(tools) == 0 {
		t.Fatal("the tool table offered no tool")
	}
	invokable, ok := tools[0].(tool.InvokableTool)
	if !ok {
		t.Fatal("the question tool is not invokable")
	}
	encoded, err := json.Marshal(map[string]any{"question": question, "candidates": candidates})
	if err != nil {
		t.Fatalf("encode the call: %v", err)
	}
	raw, err := invokable.InvokableRun(context.Background(), string(encoded))
	if err != nil {
		t.Fatalf("call the tool: %v", err)
	}
	out := &AskWhichConversationOutput{}
	if err := json.Unmarshal([]byte(raw), out); err != nil {
		t.Fatalf("decode the tool result: %v", err)
	}
	return out
}

// The tool offers the conversations the learner owns, and only those.
//
// A conversation the learner does not own, or one that has no card, is not
// offered: offering it would invite the model to open a conversation the
// boundaries refuse.
func TestTheQuestionOffersOnlyConversationsTheLearnerOwns(t *testing.T) {
	retriever := &clarifyingRetriever{candidates: []episode.ThinCardCandidate{
		{SessionID: "session-a", CardID: "card-a", CardVersion: 1, Title: "洄漪",
			Topics: []string{"慢日志与锁"}},
	}}
	registry, err := NewAskWhichConversationRegistry(retriever)
	if err != nil {
		t.Fatalf("build the registry: %v", err)
	}
	out := askWhich(t, registry, clarifyingRun("哪一次？"), "哪一次？",
		[]string{"session-a", "session-not-mine"})
	if out.Status != askWhichStatusOK {
		t.Fatalf("the question reported %q", out.Status)
	}
	if out.Clarification == nil {
		t.Fatalf("the question carried no clarification: %+v", out)
	}
	if len(out.Clarification.Candidates) != 1 {
		t.Fatalf("the question offered %d candidates, want only the learner's own",
			len(out.Clarification.Candidates))
	}
	if out.Clarification.Candidates[0].SessionID != "session-a" {
		t.Fatalf("the question offered %+v", out.Clarification.Candidates)
	}
}

// The question carries the learner's own question, because the next turn
// answers THAT question.
func TestTheQuestionCarriesThePendingQuestion(t *testing.T) {
	retriever := &clarifyingRetriever{candidates: []episode.ThinCardCandidate{
		{SessionID: "session-a", CardID: "card-a", CardVersion: 1, Title: "洄漪"},
	}}
	registry, err := NewAskWhichConversationRegistry(retriever)
	if err != nil {
		t.Fatalf("build the registry: %v", err)
	}
	pending := "上次你给我排的那三道题里，第二道练的是什么？"
	out := askWhich(t, registry, clarifyingRun(pending), pending, []string{"session-a"})
	if out.Clarification == nil || out.Clarification.Question != pending {
		t.Fatalf("the question carried %+v", out.Clarification)
	}
	if !out.Clarification.AllowFreeText {
		t.Fatal("the question does not allow the learner to write their own clue")
	}
}

// The learner's answer is read three ways: the id, the number, the title.
func TestTheLearnersAnswerIsRead(t *testing.T) {
	clarification := &model.SessionClarification{
		Question: "哪一次？",
		Candidates: []model.SessionClarificationCandidate{
			{SessionID: "session-a", Title: "洄漪"},
			{SessionID: "session-b", Title: "浔漳"},
		},
	}
	for _, reply := range []string{"session-b", "2", "浔漳"} {
		candidate, matched := clarification.Resolve(reply)
		if !matched || candidate == nil {
			t.Fatalf("%q was not read as a choice", reply)
		}
		if candidate.SessionID != "session-b" {
			t.Fatalf("%q was read as %q, want session-b", reply, candidate.SessionID)
		}
	}
}

// A reply that names none of the candidates is the learner's own clue, and
// it is not a refusal.
func TestAnAnswerThatNamesNoneIsTheLearnersOwnClue(t *testing.T) {
	clarification := &model.SessionClarification{
		Question: "哪一次？",
		Candidates: []model.SessionClarificationCandidate{
			{SessionID: "session-a", Title: "洄漪"},
		},
	}
	candidate, matched := clarification.Resolve("2026-04-04 那次，讲 HTTP 的")
	if matched {
		t.Fatalf("the learner's own words were read as a choice: %+v", candidate)
	}
}

// A clarification that cannot be shown is refused.
//
// A clarification with no candidate IS shown when the learner may browse
// their own conversation list: that is the case where one lightweight search
// found nothing credible, and the learner's own list is the way forward. A
// clarification with no candidate and no way to browse is not a question at
// all - there is nothing the learner could answer with.
func TestAClarificationWithoutCandidatesIsRefusedUnlessTheHistoryIsOpen(t *testing.T) {
	closed := &model.SessionClarification{Question: "哪一次？"}
	if err := closed.Validate(); err == nil {
		t.Fatal("a clarification with no candidates and no way to browse was accepted")
	}
	open := &model.SessionClarification{Question: "哪一次？", AllowHistoryBrowse: true}
	if err := open.Validate(); err != nil {
		t.Fatalf("a clarification that may be answered from the learner's own list was refused: %v", err)
	}
}

// A search that found nothing credible still asks.
//
// This is the case the tool used to refuse: no candidate, no question, and
// the learner left with a model that lists possibilities in its own words. A
// list of possibilities is not a question, and nobody was ever asked.
//
// The learner is offered their own conversation list instead, and a target
// that never made it into a candidate list is still reachable through it.
func TestASearchThatFoundNothingStillAsks(t *testing.T) {
	retriever := &clarifyingRetriever{}
	registry, err := NewAskWhichConversationRegistry(retriever)
	if err != nil {
		t.Fatalf("build the registry: %v", err)
	}
	pending := "上次你给我排的那三道题里，第二道练的是什么？"
	out := askWhich(t, registry, clarifyingRun(pending), pending, nil)
	if out.Status != askWhichStatusOK {
		t.Fatalf("the question reported %q", out.Status)
	}
	if out.Clarification == nil {
		t.Fatalf("the question carried no clarification: %+v", out)
	}
	if len(out.Clarification.Candidates) != 0 {
		t.Fatalf("the question offered %d candidates, want none", len(out.Clarification.Candidates))
	}
	if out.Clarification.Question != pending {
		t.Fatalf("the question carried %q, want the pending one", out.Clarification.Question)
	}
	if !out.Clarification.AllowHistoryBrowse {
		t.Fatal("the question does not offer the learner's own conversation list")
	}
	if !out.Clarification.AllowFreeText {
		t.Fatal("the question does not allow the learner to write their own clue")
	}
}

// The conversation the learner picked is opened BY IDENTITY.
//
// It is the case the clarification makes: the learner picked one of the
// conversations the model offered, and the model has to be able to open THAT
// one. Searching for it again with the question's words would find it only
// if its words happen to match, and the learner picked it precisely because
// the words did not tell them apart.
func TestTheChosenConversationIsOpenedByIdentity(t *testing.T) {
	retriever := &clarifyingRetriever{candidates: []episode.ThinCardCandidate{
		{SessionID: "session-a", CardID: "card-a", CardVersion: 1, Title: "浔漳"},
	}}
	registry, err := NewSearchSessionCardsRegistry(retriever)
	if err != nil {
		t.Fatalf("build the card search: %v", err)
	}
	run := clarifyingRun("2")
	tools, err := registry.Tools(context.Background(), run)
	if err != nil {
		t.Fatalf("build the tool: %v", err)
	}
	// The query is the question's words, and they do NOT match the
	// conversation the learner picked.
	out := callCards(t, tools[0], "session-a", "三道题")
	if len(out.Candidates) != 1 {
		t.Fatalf("the search returned %d candidates, want the chosen one", len(out.Candidates))
	}
	if out.Candidates[0].SessionID != "session-a" {
		t.Fatalf("the search returned %+v", out.Candidates[0])
	}
	// And the conversation is openable for the rest of the turn.
	if !run.GrantSet.AuthorisesSession(run, "session-a") {
		t.Fatal("the chosen conversation was not opened")
	}
}

// A conversation the learner does not own is not opened, however it was
// named.
func TestAConversationTheLearnerDoesNotOwnIsNotOpened(t *testing.T) {
	retriever := &clarifyingRetriever{}
	registry, err := NewSearchSessionCardsRegistry(retriever)
	if err != nil {
		t.Fatalf("build the card search: %v", err)
	}
	run := clarifyingRun("2")
	tools, err := registry.Tools(context.Background(), run)
	if err != nil {
		t.Fatalf("build the tool: %v", err)
	}
	out := callCards(t, tools[0], "session-not-mine", "三道题")
	if len(out.Candidates) != 0 {
		t.Fatalf("the search returned %d candidates for a conversation the learner does not own",
			len(out.Candidates))
	}
	if run.GrantSet.AuthorisesSession(run, "session-not-mine") {
		t.Fatal("a conversation the learner does not own was opened")
	}
}
