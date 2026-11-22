package episode

import (
	"GopherAI/model"
	"context"
	"fmt"
	"strings"
)

// The thin card's job projector.
//
// The thin card is kept current by the program, on the turns the session
// moves, and that is where it is written. The job is still there, and it is
// still worth having: the job is the scheduler's record that a session may
// have moved, and a session that moved while the product was down is
// repaired by the job rather than by the next turn.
//
// It is the card projector's shape so that the worker's machinery runs it
// unchanged - the same lease, the same retry budget, the same optimistic
// versioning - and it is a different thing inside: there is no model call and
// no document to write, only a card to bring up to date.

// ThinCardJobProjector writes the thin card of one session.
//
// It implements ProjectionRunner, so the existing worker drives it unchanged.
type ThinCardJobProjector struct {
	service *ThinCardService
}

// NewThinCardJobProjector builds the projector.
func NewThinCardJobProjector(service *ThinCardService) (*ThinCardJobProjector, error) {
	if service == nil {
		return nil, fmt.Errorf("thin card job projector needs a service")
	}
	return &ThinCardJobProjector{service: service}, nil
}

// Project writes the thin card of one session up to the claimed target.
//
// The target is not used: a thin card is about the session as it is, and it
// reads the session's own last message for itself. A job that names a target
// the session has already passed is still a job about that session.
//
// The INDEX is written here too, and that is the point of the job.
//
// The job is the repair path: it is what brings a session's card up to date
// when the turn-level sync did not run - a session that moved while the
// product was down, a session whose turn-level sync timed out, a deployment
// that was restarted between the answer and the card. A job that wrote the
// row and stopped would leave a card that exists in the store and cannot be
// found by any search, and the row would claim that it was indexed.
//
// The failure is returned rather than swallowed, so that the job retries it.
// The card is already in the store when the index write failed, and the retry
// writes the index without touching the card: the retry finds the card
// pending, and a pending card is written into the index even when its words
// did not change.
func (p *ThinCardJobProjector) Project(ctx context.Context, userID int64, sessionID string, targetMessageID uint) (*model.EpisodeMemory, error) {
	if p == nil || p.service == nil || userID <= 0 || strings.TrimSpace(sessionID) == "" {
		return nil, fmt.Errorf("invalid thin card projection request")
	}
	card, err := p.service.SyncCard(ctx, userID, sessionID)
	if err != nil {
		return nil, err
	}
	return card, nil
}
