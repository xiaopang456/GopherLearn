package episode

import (
	"GopherAI/model"
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	episodedao "GopherAI/dao/episode"
)

// The thin card's indexer.
//
// It writes the card into the index and NOTHING ELSE. There is no embedding
// step, and that is the difference the whole thin card rests on: the episode
// indexer embeds a card's text before it is indexed, and an embedding is a
// model call. A card that is derived from data the program already has must
// not cost a model call to keep current, or the saving is only on paper.

// ThinCardIndexWriter writes one card into the index.
type ThinCardIndexWriter interface {
	Store(context.Context, model.EpisodeMemory) error
}

// ThinCardIndexStatusSource records the index outcome on the row.
type ThinCardIndexStatusSource interface {
	UpdateIndexStatus(context.Context, int64, string, uint64, model.EpisodeIndexStatus) error
}

// ThinCardIndexer writes thin cards into the index.
type ThinCardIndexer struct {
	index  ThinCardIndexWriter
	source ThinCardIndexStatusSource
}

// NewThinCardIndexer builds the indexer.
func NewThinCardIndexer(index ThinCardIndexWriter, source ThinCardIndexStatusSource) (*ThinCardIndexer, error) {
	if index == nil || source == nil {
		return nil, fmt.Errorf("thin card indexer dependencies are required")
	}
	return &ThinCardIndexer{index: index, source: source}, nil
}

// Index writes one card into the index.
//
// The index is only written when the card's searchable text changed. A card
// that was rewritten with the same words does not need to be indexed again,
// and an index write is a write to a shared store that every other attempt
// is also writing to.
//
// The previous version of the card is what says whether the text changed. It
// is passed in rather than read back, because the projector had it in hand a
// moment ago and reading it again would be a query for a fact that is known.
func (i *ThinCardIndexer) Index(ctx context.Context, card *model.EpisodeMemory, previous *model.EpisodeMemory) error {
	if card == nil {
		return fmt.Errorf("thin card index needs a card")
	}
	if card.Status != model.EpisodeStatusActive {
		return i.source.UpdateIndexStatus(ctx, card.UserID, card.ID, card.Version, model.EpisodeIndexFailed)
	}
	if !ThinCardNeedsReindex(previous, card.Card) {
		// The searchable text is the same. The card is in the index already,
		// and the row says so.
		return i.source.UpdateIndexStatus(ctx, card.UserID, card.ID, card.Version, model.EpisodeIndexIndexed)
	}
	if err := i.index.Store(ctx, *card); err != nil {
		return err
	}
	return i.source.UpdateIndexStatus(ctx, card.UserID, card.ID, card.Version, model.EpisodeIndexIndexed)
}

// The candidate retriever.
//
// It answers the one question a thin card exists for: which earlier sessions
// could this question be about? It returns CANDIDATES - the card's own words
// and where the session's body is - and not the body itself.
//
// The body is read afterwards, from the session the candidate names, and only
// for the session that was picked. That is the saving: a question about an
// earlier session brings ONE session's summary into the window, instead of
// every session's card.

// ThinCardCandidate is one earlier session the retriever found.
type ThinCardCandidate struct {
	// SessionID is the session the card belongs to.
	SessionID string
	// CardID and CardVersion identify the card, so that a read taken in that
	// session can be checked against the card this turn actually showed.
	CardID      string
	CardVersion uint64
	// Title is the session's own title.
	Title string
	// Topics are the subjects the card is filed under.
	Topics []string
	// Names are the proper names the card carries.
	Names []string
	// SegmentSeqs are the summary segments of that session the card points at.
	//
	// They are where the body is: the reader goes to those segments and not
	// to the card.
	SegmentSeqs []int
	// LastTalkedAt is when the session was last talked in.
	LastTalkedAt time.Time
	// Watermark is how far the card reached: the source session's high-water
	// message id when this version of the card was written.
	//
	// It is the ceiling of every read taken in that session through this
	// card. A candidate that carried none would put the ceiling at zero, and
	// a ceiling of zero hides every segment and every message of the
	// conversation the candidate just pointed at.
	Watermark uint
	// Score is the text search's score. Zero for a topic lookup.
	Score float64
}

// ThinCardRetriever finds the earlier sessions a question could be about.
//
// It reads the cards of this user's history and returns a few of them. It
// never reads the body of any of them: that is what the answer is for, and
// it is read only from the session that was picked.
type ThinCardRetriever struct {
	index   ThinCardCandidateIndex
	loader  ThinCardCandidateLoader
	limit   int
	nowFunc func() time.Time
}

// ThinCardCandidateIndex is the index the retriever searches.
type ThinCardCandidateIndex interface {
	SearchByText(context.Context, int64, string, []string, int) ([]episodedao.ThinCardSearchResult, error)
	SearchExactTopics(context.Context, int64, string, []string, int) ([]episodedao.ThinCardSearchResult, error)
}

// ThinCardCandidateLoader loads the cards the index found.
type ThinCardCandidateLoader interface {
	LoadThinCards(context.Context, int64, []string) ([]model.EpisodeMemory, error)
	ListThinCardsByTime(context.Context, int64, string, time.Time, time.Time, int) ([]model.EpisodeMemory, error)
	// LoadThinCardOfSession loads the thin card of one session, by identity.
	//
	// It is the lookup that does not depend on the question's words. A
	// segment search can find a stretch of turns in a conversation whose
	// card the word search did not find, and the card is what says the
	// conversation may be opened.
	LoadThinCardOfSession(context.Context, int64, string) (*model.EpisodeMemory, error)
}

// NewThinCardRetriever builds the retriever.
func NewThinCardRetriever(index ThinCardCandidateIndex, loader ThinCardCandidateLoader, limit int) (*ThinCardRetriever, error) {
	if index == nil || loader == nil {
		return nil, fmt.Errorf("thin card retriever dependencies are required")
	}
	if limit <= 0 || limit > 20 {
		limit = 5
	}
	return &ThinCardRetriever{index: index, loader: loader, limit: limit,
		nowFunc: func() time.Time { return time.Now().UTC() }}, nil
}

// Recall returns the earlier sessions that could be the one.
//
// The three channels are the ones a card is found by: the exact topics the
// question named, the words of the question, and the time.
//
// The time channel is not a fallback. It used to be one - the most recent
// cards were read only when the words had found nothing - and that made the
// two readings of a question compete: "did we talk about load testing last
// time" is a question about the LAST conversation, and the words of it
// ("load testing") point at the conversations that DID talk about load
// testing, which is the answer to a different question.
//
// So the time channel always contributes, and what it contributes depends on
// what the question asked for:
//
//   - A question that names a period asks for that period.
//   - A question that names a subject and a pointer ("the exercises you set
//     me last time") asks for the newest of the conversations that match the
//     subject - and for the newest conversation overall, because "we never
//     talked about that last time" is an answer only the newest one can
//     give.
//   - A question that names no subject and no period is about the newest
//     conversation, and the newest conversations are the whole answer.
func (r *ThinCardRetriever) Recall(ctx context.Context, userID int64, currentSession string,
	terms, topics []string, temporal TemporalMode) ([]ThinCardCandidate, error) {
	if r == nil || userID <= 0 {
		return nil, nil
	}
	seen := map[string]struct{}{}
	ids := make([]string, 0, r.limit*2)
	collect := func(results []episodedao.ThinCardSearchResult) {
		for _, result := range results {
			if _, exists := seen[result.CardID]; exists {
				continue
			}
			seen[result.CardID] = struct{}{}
			ids = append(ids, result.CardID)
		}
	}
	// The subject channels. They are skipped for a question that names no
	// subject at all, because the words of such a question are the words of
	// the pointer and they point at everything.
	bySubject := false
	if temporal != TemporalLatestAny {
		if len(topics) > 0 {
			results, err := r.index.SearchExactTopics(ctx, userID, currentSession, topics, r.limit)
			if err != nil {
				return nil, err
			}
			collect(results)
			bySubject = len(ids) > 0
		}
		if len(terms) > 0 && len(ids) < r.limit {
			results, err := r.index.SearchByText(ctx, userID, currentSession, terms, r.limit)
			if err != nil {
				return nil, err
			}
			collect(results)
			bySubject = bySubject || len(ids) > 0
		}
	}
	// The time channel. It always runs, and it is the whole answer for a
	// question that named no subject.
	if temporal == TemporalLatestAny || temporal == TemporalLatestMatching || !bySubject {
		recent, err := r.recentCards(ctx, userID, currentSession)
		if err != nil {
			return nil, err
		}
		collect(recent)
	}
	if len(ids) == 0 {
		return []ThinCardCandidate{}, nil
	}
	cards, err := r.loader.LoadThinCards(ctx, userID, ids)
	if err != nil {
		return nil, err
	}
	out := make([]ThinCardCandidate, 0, len(cards))
	for _, card := range cards {
		candidate := ThinCardCandidate{
			SessionID:    card.SessionID,
			CardID:       card.ID,
			CardVersion:  card.Version,
			Title:        card.Card.Title,
			SegmentSeqs:  card.Card.SegmentSeqs,
			LastTalkedAt: card.LastMessageAt,
			Watermark:    card.SourceLastMessageID,
		}
		for _, topic := range card.Card.Topics {
			candidate.Topics = append(candidate.Topics, topic.Label)
		}
		candidate.Names = append(candidate.Names, card.Card.Names...)
		out = append(out, candidate)
	}
	// The order of the candidates is the order of the answer.
	//
	// A question that names a pointer asks for the newest conversation, so
	// the candidates are read newest first: the model checks the first one
	// against the question and moves on to the second when the evidence does
	// not match. A question that names no pointer has no order of its own,
	// and the index's own score is the order it is left in.
	if temporal == TemporalLatestAny || temporal == TemporalLatestMatching {
		sort.SliceStable(out, func(left, right int) bool {
			return out[left].LastTalkedAt.After(out[right].LastTalkedAt)
		})
	}
	return out, nil
}

// CardOfSession returns the thin card of one session.
//
// It is the lookup by identity, and it is what a segment hit is checked
// against. The check cannot be done with the question's words: a segment
// search finds a stretch of turns in a conversation whose card the word
// search did NOT find, because the question's words and the card's words are
// different words. Checking by words would make the segment search's hits
// unreachable in exactly the case it exists for.
//
// The second return value says whether there was a card at all.
func (r *ThinCardRetriever) CardOfSession(ctx context.Context, userID int64,
	sessionID string) (ThinCardCandidate, bool, error) {
	if r == nil || r.loader == nil || userID <= 0 || strings.TrimSpace(sessionID) == "" {
		return ThinCardCandidate{}, false, nil
	}
	card, err := r.loader.LoadThinCardOfSession(ctx, userID, sessionID)
	if err != nil {
		return ThinCardCandidate{}, false, err
	}
	if card == nil {
		return ThinCardCandidate{}, false, nil
	}
	candidate := ThinCardCandidate{
		SessionID:    card.SessionID,
		CardID:       card.ID,
		CardVersion:  card.Version,
		Title:        card.Card.Title,
		SegmentSeqs:  card.Card.SegmentSeqs,
		LastTalkedAt: card.LastMessageAt,
		Watermark:    card.SourceLastMessageID,
	}
	for _, topic := range card.Card.Topics {
		candidate.Topics = append(candidate.Topics, topic.Label)
	}
	candidate.Names = append(candidate.Names, card.Card.Names...)
	return candidate, true, nil
}

// recentCards are the user's most recent thin cards.
//
// It is the time answer: "上次" and "上周" ask about the order of the
// sessions, and the order is what this returns. It is read from the store
// rather than from the index, because the index finds a card by its words and
// the time question has no words to find it by.
func (r *ThinCardRetriever) recentCards(ctx context.Context, userID int64, currentSession string) ([]episodedao.ThinCardSearchResult, error) {
	cards, err := r.loader.ListThinCardsByTime(ctx, userID, currentSession,
		time.Time{}, time.Time{}, r.limit)
	if err != nil {
		return nil, err
	}
	out := make([]episodedao.ThinCardSearchResult, 0, len(cards))
	for _, card := range cards {
		out = append(out, episodedao.ThinCardSearchResult{
			CardID:          card.ID,
			LastMessageUnix: card.LastMessageAt.Unix(),
		})
	}
	return out, nil
}
