package episode

import (
	"GopherAI/model"
	"context"
	"fmt"
	"time"
)

// The thin card's loader.
//
// The thin card is a session card row - the same row, the same table, the
// same index status - so the loader is the card loader. The only thing that
// differs is which rows it accepts: a thin card is a card whose document
// says it is one.
//
// That matters because the two kinds of card live in the same table and the
// same index namespace. A loader that did not say which kind it wanted would
// return whichever row the database happened to order first, and the reader
// would be shown a full card where it expected a thin one.

// ThinCardLoader loads the thin cards of one user.
type ThinCardLoader struct {
	Repository *Repository
}

// LoadThinCards loads the thin cards with the given ids.
//
// The rows come back in the order they were asked for: the index ranked them,
// and the ranking is the answer's most valuable part.
func (l ThinCardLoader) LoadThinCards(ctx context.Context, userID int64, ids []string) ([]model.EpisodeMemory, error) {
	if l.Repository == nil {
		return nil, fmt.Errorf("episode repository is not initialized")
	}
	rows, err := l.Repository.LoadCardCandidates(ctx, userID, "", ids)
	if err != nil {
		return nil, err
	}
	out := make([]model.EpisodeMemory, 0, len(rows))
	for _, row := range rows {
		if !row.Card.IsThin() {
			continue
		}
		out = append(out, row)
	}
	return out, nil
}

// LoadThinCardOfSession loads the thin card of one session.
//
// It is the lookup by identity rather than by words, and it exists because
// the words are not always enough: a segment search can find a stretch of
// turns in a conversation whose card the word search did NOT find, because the
// question's words and the card's words are different words. The card of that
// conversation still exists, and it is the card that says the conversation
// may be opened.
func (l ThinCardLoader) LoadThinCardOfSession(ctx context.Context, userID int64,
	sessionID string) (*model.EpisodeMemory, error) {
	if l.Repository == nil {
		return nil, fmt.Errorf("episode repository is not initialized")
	}
	card, err := l.Repository.LoadCardOfSession(ctx, userID, sessionID)
	if err != nil {
		return nil, err
	}
	if card == nil || !card.Card.IsThin() {
		return nil, nil
	}
	return card, nil
}

// ListThinCardsByTime lists the user's thin cards by the time their session
// was last talked in.
//
// It is the answer to a question about order: "上次" and "上周" ask which
// session came when, and no amount of matching on words answers that.
func (l ThinCardLoader) ListThinCardsByTime(ctx context.Context, userID int64, currentSession string,
	from, to time.Time, limit int) ([]model.EpisodeMemory, error) {
	if l.Repository == nil {
		return nil, fmt.Errorf("episode repository is not initialized")
	}
	rows, err := l.Repository.ListCardsByTime(ctx, userID, currentSession, from, to, limit)
	if err != nil {
		return nil, err
	}
	out := make([]model.EpisodeMemory, 0, len(rows))
	for _, row := range rows {
		if !row.Card.IsThin() {
			continue
		}
		out = append(out, row)
	}
	return out, nil
}
