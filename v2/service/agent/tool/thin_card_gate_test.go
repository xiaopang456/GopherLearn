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

// THE PAIR, at the tool: a period on its own stays in this conversation, and
// a period with an explicit pointer to another conversation goes there.
//
// A period is a FILTER, not a reason to go looking. "上周讲了什么" names no
// conversation, and the conversation in front of the learner is the better
// answer to it. "上周另一个聊天里讲了什么" names another conversation, and
// that is the clearest possible "go and look".
func TestTheCardSearchStaysForABarePeriodAndGoesForANamedOtherChat(t *testing.T) {
	retriever := &countingCardRetriever{}
	registry, err := NewSearchSessionCardsRegistry(retriever)
	if err != nil {
		t.Fatalf("build the card search: %v", err)
	}
	stayRun := gateRun("上周讲了什么")
	stayTools, err := registry.Tools(context.Background(), stayRun)
	if err != nil {
		t.Fatalf("build the tool: %v", err)
	}
	stay := callCards(t, stayTools[0], "", "上周")
	if stay.Reason != cardsReasonCurrentFirst {
		t.Fatalf("a bare period went looking with the reason %q, want %q",
			stay.Reason, cardsReasonCurrentFirst)
	}
	if len(stay.Candidates) != 0 {
		t.Fatalf("a bare period returned %d candidates", len(stay.Candidates))
	}
	if retriever.calls != 0 {
		t.Fatalf("the retriever was asked %d times for a bare period, want none", retriever.calls)
	}
	goRun := gateRun("上周另一个聊天里讲了什么")
	goTools, err := registry.Tools(context.Background(), goRun)
	if err != nil {
		t.Fatalf("build the tool: %v", err)
	}
	out := callCards(t, goTools[0], "", "上周另一个聊天")
	if out.Reason == cardsReasonCurrentFirst {
		t.Fatalf("a named other conversation stayed in this conversation: %+v", out)
	}
	if retriever.calls == 0 {
		t.Fatalf("the retriever was never asked for a named other conversation")
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
