package episode

import (
	"GopherAI/model"
	"context"
	"fmt"
)

type ProjectionGap struct {
	UserID          int64  `gorm:"column:user_id"`
	SessionID       string `gorm:"column:session_id"`
	LatestMessageID uint   `gorm:"column:latest_message_id"`
}

// ListProjectionGaps finds completed Agent conversations whose latest
// assistant message is newer than the durable Episode watermark. It repairs a
// missed online enqueue without treating a half-finished user-only Run as a
// complete episode source.
func (r *Repository) ListProjectionGaps(ctx context.Context, limit int) ([]ProjectionGap, error) {
	if r == nil || r.db == nil || limit < 1 || limit > 1000 {
		return nil, fmt.Errorf("invalid episode reconciliation query")
	}
	var gaps []ProjectionGap
	err := r.db.WithContext(ctx).Raw(`
		SELECT s.user_id, s.id AS session_id, latest.latest_message_id
		FROM sessions s
		JOIN (
			SELECT session_id, MAX(id) AS latest_message_id
			FROM messages
			GROUP BY session_id
		) latest ON latest.session_id = s.id
		JOIN messages last_message ON last_message.id = latest.latest_message_id AND last_message.session_id = s.id
		LEFT JOIN episode_memories e ON e.user_id = s.user_id AND e.session_id = s.id
		WHERE s.deleted_at IS NULL AND s.user_id > 0 AND s.mode = ?
		  AND last_message.is_user = FALSE
		  AND latest.latest_message_id > COALESCE(e.source_last_message_id, 0)
		ORDER BY last_message.created_at ASC
		LIMIT ?`, model.SessionModeAgent, limit).Scan(&gaps).Error
	return gaps, err
}
