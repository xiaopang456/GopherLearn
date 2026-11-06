package message

import (
	"GopherAI/model"
	"context"
	"fmt"
)

// ReadOwnedRunAssistant is the publication seam for the exact persisted run
// assistant. It never approximates a run using recent messages.
func (r *Repository) ReadOwnedRunAssistant(ctx context.Context, userID int64, sessionID, runID string) (*model.Message, error) {
	return r.readOwnedRunMessage(ctx, userID, sessionID, runID, model.MessageAgentRunRoleAssistant)
}

// ReadOwnedRunUser is the same seam for the question the run answered.
//
// It exists because "the last two messages" is not the same fact as "the two
// messages of this run": a second turn can land in between, and a replayed
// request id belongs to an earlier pair. A caller that answers a replay with the
// newest rows would be reporting one turn's identity next to another turn's text.
func (r *Repository) ReadOwnedRunUser(ctx context.Context, userID int64, sessionID, runID string) (*model.Message, error) {
	return r.readOwnedRunMessage(ctx, userID, sessionID, runID, model.MessageAgentRunRoleUser)
}

func (r *Repository) readOwnedRunMessage(ctx context.Context, userID int64, sessionID, runID, role string) (*model.Message, error) {
	if r == nil || r.db == nil || userID <= 0 || sessionID == "" || runID == "" {
		return nil, fmt.Errorf("invalid owned run message read")
	}
	var message model.Message
	err := r.db.WithContext(ctx).Joins("JOIN sessions ON sessions.id = messages.session_id AND sessions.deleted_at IS NULL").
		Where("sessions.user_id = ? AND messages.session_id = ? AND messages.agent_run_id = ? AND messages.agent_run_role = ?", userID, sessionID, runID, role).First(&message).Error
	return &message, err
}
