package episode

import (
	"context"
	"testing"
	"time"

	episodedao "GopherAI/dao/episode"
	"GopherAI/model"
)

// The retriever's own tests.
//
// The claim they defend is the one the arm is measured on: the time channel
// is one of the three ways a card is found, and it is not a fallback for the
// other two. A question that names no subject is answered by the newest
// conversations; a question that names a subject AND a pointer is answered by
// the newest of the matching conversations together with the newest one, so
// that "we never talked about that last time" has a conversation to say it.

// fakeThinCardIndex is the card index the retriever reads.
type fakeThinCardIndex struct {
	topics []episodedao.ThinCardSearchResult
	text   []episodedao.ThinCardSearchResult
}

func (f *fakeThinCardIndex) SearchExactTopics(_ context.Context, _ int64, _ string,
	_ []string, _ int) ([]episodedao.ThinCardSearchResult, error) {
	return f.topics, nil
}

func (f *fakeThinCardIndex) SearchByText(_ context.Context, _ int64, _ string,
	_ []string, _ int) ([]episodedao.ThinCardSearchResult, error) {
	return f.text, nil
}

// fakeThinCardLoader is the store the retriever loads the cards from.
//
// It resolves by id, the way the real store does: the index names the cards
// and the store hands them over.
type fakeThinCardLoader struct {
	pool   []model.EpisodeMemory
	recent []model.EpisodeMemory
}

func (f *fakeThinCardLoader) LoadThinCards(_ context.Context, _ int64,
	ids []string) ([]model.EpisodeMemory, error) {
	out := make([]model.EpisodeMemory, 0, len(ids))
	for _, id := range ids {
		for _, card := range f.pool {
			if card.ID == id {
				out = append(out, card)
				break
			}
		}
	}
	return out, nil
}

func (f *fakeThinCardLoader) ListThinCardsByTime(_ context.Context, _ int64, _ string,
	from, to time.Time, _ int) ([]model.EpisodeMemory, error) {
	// The store's own period filter: a card outside the window is not in the
	// list at all.
	out := make([]model.EpisodeMemory, 0, len(f.recent))
	for _, card := range f.recent {
		if !from.IsZero() && card.LastMessageAt.Before(from) {
			continue
		}
		if !to.IsZero() && !card.LastMessageAt.Before(to) {
			continue
		}
		out = append(out, card)
	}
	return out, nil
}

func (f *fakeThinCardLoader) LoadThinCardOfSession(_ context.Context, _ int64,
	_ string) (*model.EpisodeMemory, error) {
	return nil, nil
}

// cardRow is one card in the fake store.
func cardRow(id, session string, talkedAt time.Time) model.EpisodeMemory {
	return model.EpisodeMemory{
		ID: id, SessionID: session, LastMessageAt: talkedAt,
		Card: model.SessionCardDocument{
			Version: model.SessionThinCardContentVersion, Title: session,
		},
	}
}

// A question that names a subject and a pointer gets the newest of the
// matching conversations AND the newest conversation overall.
//
// The second one is what makes "we never talked about that last time"
// answerable: the words of the question point at the conversations that DID
// talk about it, and the newest conversation is the one the learner is
// asking about.
func TestTheNewestConversationIsACandidateEvenWhenTheWordsFoundSome(t *testing.T) {
	base := time.Unix(1000, 0).UTC()
	index := &fakeThinCardIndex{text: []episodedao.ThinCardSearchResult{{CardID: "card-old"}}}
	loader := &fakeThinCardLoader{
		pool: []model.EpisodeMemory{
			cardRow("card-old", "old", base.Add(-time.Hour)),
			cardRow("card-new", "new", base),
			cardRow("card-newer", "newer", base.Add(time.Hour)),
		},
		recent: []model.EpisodeMemory{
			cardRow("card-new", "new", base),
			cardRow("card-newer", "newer", base.Add(time.Hour)),
		},
	}
	retriever, err := NewThinCardRetriever(index, loader, 5)
	if err != nil {
		t.Fatalf("build the retriever: %v", err)
	}
	candidates, err := retriever.Recall(context.Background(), 7, "current",
		ThinCardQuery{Terms: []string{"压测"}, Temporal: TemporalLatestMatching})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	sessions := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		sessions = append(sessions, candidate.SessionID)
	}
	if len(sessions) != 3 {
		t.Fatalf("the candidates are %v, want the matched one and the two newest", sessions)
	}
	if sessions[0] != "newer" {
		t.Fatalf("the candidates are %v, want the newest first", sessions)
	}
	found := false
	for _, session := range sessions {
		if session == "new" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the newest conversation is not among the candidates: %v", sessions)
	}
}

// A question that names no subject is answered by the newest conversations,
// and the words of the question are not asked of the index at all.
func TestAQuestionWithNoSubjectIsAnsweredByTheNewestConversations(t *testing.T) {
	base := time.Unix(1000, 0).UTC()
	index := &fakeThinCardIndex{text: []episodedao.ThinCardSearchResult{{CardID: "card-word"}}}
	loader := &fakeThinCardLoader{
		pool: []model.EpisodeMemory{cardRow("card-word", "word", base.Add(-time.Hour)),
			cardRow("card-new", "new", base)},
		recent: []model.EpisodeMemory{cardRow("card-new", "new", base)},
	}
	retriever, err := NewThinCardRetriever(index, loader, 5)
	if err != nil {
		t.Fatalf("build the retriever: %v", err)
	}
	candidates, err := retriever.Recall(context.Background(), 7, "current",
		ThinCardQuery{Temporal: TemporalLatestAny})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(candidates) != 1 || candidates[0].SessionID != "new" {
		t.Fatalf("the candidates are %+v, want the newest conversation alone", candidates)
	}
}

// A question with no pointer at all is answered by the words, and the newest
// conversations are not mixed into it.
func TestAQuestionWithNoPointerIsAnsweredByTheWords(t *testing.T) {
	base := time.Unix(1000, 0).UTC()
	index := &fakeThinCardIndex{text: []episodedao.ThinCardSearchResult{{CardID: "card-word"}}}
	loader := &fakeThinCardLoader{
		pool:   []model.EpisodeMemory{cardRow("card-word", "word", base.Add(-time.Hour))},
		recent: []model.EpisodeMemory{cardRow("card-new", "new", base)},
	}
	retriever, err := NewThinCardRetriever(index, loader, 5)
	if err != nil {
		t.Fatalf("build the retriever: %v", err)
	}
	candidates, err := retriever.Recall(context.Background(), 7, "current",
		ThinCardQuery{Terms: []string{"隔离级别"}, Temporal: TemporalNone})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(candidates) != 1 || candidates[0].SessionID != "word" {
		t.Fatalf("the candidates are %+v, want the conversation the words found", candidates)
	}
}

// A period FILTERS: a conversation outside it is not an answer to the
// question, however well its words match.
//
// The period is asked of the store as well as of the loaded cards: the
// store's own list is what the time channel reads, and a filter that only
// applied to one of the two would let a conversation of the wrong week
// through the other.
func TestANamedPeriodFiltersTheCandidatesByDate(t *testing.T) {
	base := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	inWeek := base
	outOfWeek := base.AddDate(0, 0, -14)
	index := &fakeThinCardIndex{text: []episodedao.ThinCardSearchResult{
		{CardID: "card-in"}, {CardID: "card-out"}}}
	loader := &fakeThinCardLoader{
		pool: []model.EpisodeMemory{
			cardRow("card-in", "in", inWeek),
			cardRow("card-out", "out", outOfWeek),
		},
		recent: []model.EpisodeMemory{cardRow("card-in", "in", inWeek)},
	}
	retriever, err := NewThinCardRetriever(index, loader, 5)
	if err != nil {
		t.Fatalf("build the retriever: %v", err)
	}
	candidates, err := retriever.Recall(context.Background(), 7, "current", ThinCardQuery{
		Terms: []string{"清单"}, Temporal: TemporalRange,
		From: base.AddDate(0, 0, -7), To: base.AddDate(0, 0, 7),
	})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(candidates) != 1 || candidates[0].SessionID != "in" {
		t.Fatalf("the candidates are %+v, want only the conversation inside the period", candidates)
	}
}
