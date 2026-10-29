package message

import (
	"GopherAI/model"
	"context"
	"fmt"
)

// ReadOwnedRunAssistant is the publication seam for the exact persisted run
// assistant. It never approximates a run using recent messages.
func (r *Repository) ReadOwnedRunAssistant(ctx context.Context, userID int64, sessionID, runID string) (*model.Message, error) {
	if r == nil || r.db == nil || userID <= 0 || sessionID == "" || runID == "" {
		return nil, fmt.Errorf("invalid owned run message read")
	}
	var message model.Message
	err := r.db.WithContext(ctx).Joins("JOIN sessions ON sessions.id = messages.session_id AND sessions.deleted_at IS NULL").
		Where("sessions.user_id = ? AND messages.session_id = ? AND messages.agent_run_id = ? AND messages.agent_run_role = ?", userID, sessionID, runID, model.MessageAgentRunRoleAssistant).First(&message).Error
	return &message, err
}
