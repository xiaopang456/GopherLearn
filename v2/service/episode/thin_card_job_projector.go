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
func (p *ThinCardJobProjector) Project(ctx context.Context, userID int64, sessionID string, targetMessageID uint) (*model.EpisodeMemory, error) {
	if p == nil || p.service == nil || userID <= 0 || strings.TrimSpace(sessionID) == "" {
		return nil, fmt.Errorf("invalid thin card projection request")
	}
	card, _, err := p.service.projector.Project(ctx, userID, sessionID, targetMessageID)
	if err != nil {
		return nil, err
	}
	return card, nil
}
