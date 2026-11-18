package agenttool

import (
	"context"
	"testing"

	"GopherAI/dao/summarysegment"
	serviceagent "GopherAI/service/agent"
	"GopherAI/service/episode"
)

// The order of the search.
//
// The rule is the one the learner would expect: a question that only points -
// "last time", "last week" - and a conversation that already has a history of
// its own is answered HERE first. The two searches refuse to go looking
// elsewhere before looking here, and they say why.
//
// The rule reads the LEARNER's question rather than the tool's query, because
// the model may compress the question into the query: the model may pass
// "last time" while the learner wrote "the three exercises you set me last
// time", and the second is a question about an earlier conversation.

// countingCardRetriever answers everything and counts how often it was asked.
type countingCardRetriever struct {
	calls int
	// identityLookups counts the lookups by session id.
	identityLookups int
	// cards are the conversations that have a card.
	cards map[string]episode.ThinCardCandidate
}

func (f *countingCardRetriever) Recall(_ context.Context, _ int64, _ string,
	_, _ []string, _ episode.TemporalMode) ([]episode.ThinCardCandidate, error) {
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

// A bare pointer is refused, and the refusal says where to look first.
func TestTheCardSearchStaysInThisConversationForABarePointer(t *testing.T) {
	retriever := &countingCardRetriever{}
	registry, err := NewSearchSessionCardsRegistry(retriever)
	if err != nil {
		t.Fatalf("build the card search: %v", err)
	}
	run := gateRun("上次讲了什么")
	tools, err := registry.Tools(context.Background(), run)
	if err != nil {
		t.Fatalf("build the tool: %v", err)
	}
	out := callCards(t, tools[0], "", "上次")
	if out.Reason != cardsReasonCurrentFirst {
		t.Fatalf("the search went looking with the reason %q, want %q", out.Reason, cardsReasonCurrentFirst)
	}
	if len(out.Candidates) != 0 {
		t.Fatalf("the search returned %d candidates", len(out.Candidates))
	}
	if retriever.calls != 0 {
		t.Fatalf("the retriever was asked %d times, want none", retriever.calls)
	}
	if out.Notes == "" {
		t.Fatalf("the refusal says nothing about where to look first")
	}
}

// A question that names something goes looking, and the retriever is asked.
func TestTheCardSearchLooksForANamedSubject(t *testing.T) {
	retriever := &countingCardRetriever{}
	registry, err := NewSearchSessionCardsRegistry(retriever)
	if err != nil {
		t.Fatalf("build the card search: %v", err)
	}
	run := gateRun("上次你给我排的那三道题里，第二道练的是什么？")
	tools, err := registry.Tools(context.Background(), run)
	if err != nil {
		t.Fatalf("build the tool: %v", err)
	}
	out := callCards(t, tools[0], "", "三道题")
	if out.Reason == cardsReasonCurrentFirst {
		t.Fatalf("a named subject stayed in this conversation: %+v", out)
	}
	if retriever.calls == 0 {
		t.Fatalf("the retriever was never asked")
	}
}

// The segment search follows the same rule.
func TestTheSegmentSearchStaysInThisConversationForABarePointer(t *testing.T) {
	finder := &countingSegmentFinder{}
	registry, err := NewSearchHistorySegmentsRegistry(finder, nil, &countingCardRetriever{})
	if err != nil {
		t.Fatalf("build the segment search: %v", err)
	}
	run := gateRun("上次讲了什么")
	tools, err := registry.Tools(context.Background(), run)
	if err != nil {
		t.Fatalf("build the tool: %v", err)
	}
	out := callSegments(t, tools[0], "上次")
	if out.Reason != segmentsReasonCurrentFirst {
		t.Fatalf("the search went looking with the reason %q, want %q", out.Reason, segmentsReasonCurrentFirst)
	}
	if finder.calls != 0 {
		t.Fatalf("the finder was asked %d times, want none", finder.calls)
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
