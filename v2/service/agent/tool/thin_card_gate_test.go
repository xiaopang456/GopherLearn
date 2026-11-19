package agenttool

import (
	"context"
	"strings"
	"testing"

	"GopherAI/dao/summarysegment"
	serviceagent "GopherAI/service/agent"
	"GopherAI/service/episode"
)

// The routing is the model's.
//
// The server does not read the question and decide that the turn is about
// another conversation. A rule applied to the question's words refuses the
// questions that need it - "what did we talk about last week in the other
// chat" looks like a bare pointer and is not about this conversation at all.
//
// So the two searches run when the model calls them, and the RULE is in the
// tool description, which is what the model reads. What the server keeps is
// the boundaries: whose history it reaches, which conversations a read may be
// taken in, which ids it may cite, and how much of it comes back.

// countingCardRetriever answers everything and counts how often it was asked.
type countingCardRetriever struct {
	calls int
	// identityLookups counts the lookups by session id.
	identityLookups int
	// cards are the conversations that have a card.
	cards map[string]episode.ThinCardCandidate
}

func (f *countingCardRetriever) Recall(_ context.Context, _ int64, _ string,
	_ episode.ThinCardQuery) ([]episode.ThinCardCandidate, error) {
	f.calls++
	return nil, nil
}

// CardOfSession answers from the conversations it was given, and counts the
// lookups.
func (f *countingCardRetriever) CardOfSession(_ context.Context, _ int64,
	sessionID string) (episode.ThinCardCandidate, bool, error) {
	f.identityLookups++
	candidate, found := f.cards[sessionID]
	return candidate, found, nil
}

// countingSegmentFinder answers with the hits it was given and counts how
// often it was asked.
type countingSegmentFinder struct {
	calls int
	hits  []summarysegment.UserSegmentHit
}

func (f *countingSegmentFinder) SearchUserSegmentsByText(_ context.Context, _ int64,
	_ []string, _ int) ([]summarysegment.UserSegmentHit, error) {
	f.calls++
	return f.hits, nil
}

// gateRun is a run whose conversation already has a history.
func gateRun(question string) serviceagent.RunContext {
	run := serviceagent.RunContext{UserID: 7, SessionID: "current-session",
		Question: question, CurrentSessionHasHistory: true}
	run.GrantSet = serviceagent.NewHistoryGrantSet(run, nil)
	return run
}

// The card search runs when the model calls it, whatever the question says.
//
// It used to refuse a question that only pointed, and the refusal was the
// wrong order: the model is the one that can see whether the learner asked
// about another conversation, and a refusal on the question's words sends it
// back with a query that says no more than the question did.
func TestTheCardSearchRunsWhateverTheQuestionSays(t *testing.T) {
	for _, question := range []string{
		"上次讲了什么",
		"上周讲了什么",
		"上周另一个聊天里讲了什么",
		"上次你给我排的那三道题里，第二道练的是什么？",
	} {
		retriever := &countingCardRetriever{}
		registry, err := NewSearchSessionCardsRegistry(retriever)
		if err != nil {
			t.Fatalf("build the card search: %v", err)
		}
		run := gateRun(question)
		tools, err := registry.Tools(context.Background(), run)
		if err != nil {
			t.Fatalf("build the tool: %v", err)
		}
		out := callCards(t, tools[0], "", "上周")
		if out.Reason == cardsReasonCurrentFirst {
			t.Fatalf("%q was refused by the server: %+v", question, out)
		}
		if retriever.calls == 0 {
			t.Fatalf("%q never reached the retriever", question)
		}
	}
}

// The segment search follows the same rule.
func TestTheSegmentSearchRunsWhateverTheQuestionSays(t *testing.T) {
	for _, question := range []string{"上次讲了什么", "上周讲了什么"} {
		finder := &countingSegmentFinder{}
		registry, err := NewSearchHistorySegmentsRegistry(finder, nil, &countingCardRetriever{})
		if err != nil {
			t.Fatalf("build the segment search: %v", err)
		}
		run := gateRun(question)
		tools, err := registry.Tools(context.Background(), run)
		if err != nil {
			t.Fatalf("build the tool: %v", err)
		}
		out := callSegments(t, tools[0], "上周")
		if finder.calls == 0 {
			t.Fatalf("%q never reached the finder", question)
		}
		if out.Status == segmentsStatusUnavailable {
			t.Fatalf("%q was refused by the server: %+v", question, out)
		}
	}
}

// The rule is in the description, because the description is what the model
// reads.
//
// It has to say three things: answer from this conversation first, go
// looking only when the learner asked about another conversation, and ask the
// learner when the reference is still ambiguous.
func TestTheDescriptionsCarryTheRoutingRule(t *testing.T) {
	for name, description := range map[string]string{
		"search_session_cards":    searchSessionCardsDescription,
		"search_history_segments": searchHistorySegmentsDescription,
	} {
		lowered := strings.ToLower(description)
		for _, phrase := range []string{"this conversation", "ask the learner"} {
			if !strings.Contains(lowered, phrase) {
				t.Fatalf("the %s description does not say %q: %s", name, phrase, description)
			}
		}
	}
}

// The period the question named reaches the index as a filter.
//
// The words of the question are the subject, and the period says when. A
// card of a conversation outside the period is not an answer to the question,
// however well its words match.
func TestThePeriodReachesTheIndexAsAFilter(t *testing.T) {
	retriever := &fakeThinCardRetriever{}
	registry, err := NewSearchSessionCardsRegistry(retriever)
	if err != nil {
		t.Fatalf("build the card search: %v", err)
	}
	run := gateRun("上周那份练习清单，单项里最后定的是写什么？")
	tools, err := registry.Tools(context.Background(), run)
	if err != nil {
		t.Fatalf("build the tool: %v", err)
	}
	callCards(t, tools[0], "", "练习清单")
	if !retriever.period.HasPeriod() {
		t.Fatalf("the query carries no period: %+v", retriever.period)
	}
	if retriever.period.From.IsZero() || retriever.period.To.IsZero() {
		t.Fatalf("the period is half empty: %+v", retriever.period)
	}
}

// A segment hit in a conversation whose card the WORD search missed is still
// reachable.
//
// It is the case the segment search exists for: the question's words and the
// card's words are different words, so the card search found nothing and the
// segment search found the conversation anyway. Checking the hit against the
// question's words again would make the hit unreachable exactly there.
func TestASegmentHitIsCheckedAgainstTheCardOfItsOwnConversation(t *testing.T) {
	finder := &countingSegmentFinder{}
	finder.hits = []summarysegment.UserSegmentHit{{SessionID: "older-session", Seq: 3,
		Topic: "练习编排", Content: "本地替身段"}}
	cards := &countingCardRetriever{cards: map[string]episode.ThinCardCandidate{
		"older-session": {SessionID: "older-session", CardID: "card-1", CardVersion: 1},
	}}
	registry, err := NewSearchHistorySegmentsRegistry(finder, nil, cards)
	if err != nil {
		t.Fatalf("build the segment search: %v", err)
	}
	run := serviceagent.RunContext{UserID: 7, SessionID: "current-session",
		Question: "上次你给我排的那三道题里，第二道练的是什么？", CurrentSessionHasHistory: true}
	run.GrantSet = serviceagent.NewHistoryGrantSet(run, nil)
	tools, err := registry.Tools(context.Background(), run)
	if err != nil {
		t.Fatalf("build the tool: %v", err)
	}
	out := callSegments(t, tools[0], "三道题")
	if len(out.Segments) != 1 {
		t.Fatalf("the search returned %d segments, want the hit", len(out.Segments))
	}
	if !out.Segments[0].Reachable {
		t.Fatalf("the hit is not reachable: %+v", out.Segments[0])
	}
	if cards.identityLookups != 1 {
		t.Fatalf("the card was looked up %d times by identity, want once", cards.identityLookups)
	}
	if cards.calls != 0 {
		t.Fatalf("the card was searched %d times by words, want none", cards.calls)
	}
}
