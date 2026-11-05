package session

import (
	"GopherAI/model"
	"context"
	"fmt"
)

// ListSummarySegmentGaps finds conversations whose latest assistant turn is
// newer than the last summary segment, so a segment is owed.
//
// A conversation with no segment at all is a gap too: the watermark is zero
// then, and every assistant turn is uncovered. The comparison is against the
// segment's high-water mark rather than the board's coverage because the
// segment is what makes a stretch of the conversation answerable later.
//
// Only assistant turns count. A half-finished turn that only the learner has
// spoken in is not a conversation that finished, and summarizing it would
// record a segment about half of something.
func (r *Repository) ListSummarySegmentGaps(ctx context.Context, limit int) ([]model.SummarySegmentGap, error) {
	if r == nil || r.db == nil || limit < 1 || limit > 1000 {
		return nil, fmt.Errorf("invalid summary segment gap query")
	}
	var gaps []model.SummarySegmentGap
	err := r.db.WithContext(ctx).Raw(`
		SELECT s.user_id, s.id AS session_id, latest.latest_message_id
		FROM sessions s
		JOIN (
			SELECT session_id, MAX(id) AS latest_message_id
			FROM messages
			GROUP BY session_id
		) latest ON latest.session_id = s.id
		JOIN messages last_message ON last_message.id = latest.latest_message_id AND last_message.session_id = s.id
		LEFT JOIN (
			SELECT session_id, MAX(end_message_id) AS covered_message_id
			FROM session_summary_segments
			GROUP BY session_id
		) covered ON covered.session_id = s.id
		WHERE s.deleted_at IS NULL AND s.user_id > 0 AND s.mode = ?
		  AND last_message.is_user = FALSE
		  AND latest.latest_message_id > COALESCE(covered.covered_message_id, 0)
		ORDER BY last_message.created_at ASC
		LIMIT ?`, model.SessionModeAgent, limit).Scan(&gaps).Error
	if err != nil {
		return nil, err
	}
	return gaps, nil
}

// GetSummarySegmentWatermark returns the message id the session's segments
// cover, and the segment's sequence number.
//
// The watermark is the highest end_message_id among the session's segments, or
// zero when there are none. It is the point from which the next segment has to
// start, and it is what keeps a retry covering the same stretch of turns: a
// failed segment never lands, so the watermark does not move.
//
// The sequence number comes with it because the caller has to write the next
// segment under a number no other writer took, and reading it here keeps that
// decision on one side of the same transaction.
func (r *Repository) GetSummarySegmentWatermark(ctx context.Context, userID int64, sessionID string) (uint, int, error) {
	if r == nil || r.db == nil || userID <= 0 || sessionID == "" {
		return 0, 0, fmt.Errorf("invalid summary segment watermark query")
	}
	var row struct {
		Watermark uint
		Seq       int
	}
	err := r.db.WithContext(ctx).Model(&model.SessionSummarySegment{}).
		Select("COALESCE(MAX(end_message_id), 0) AS watermark, COALESCE(MAX(seq), 0) AS seq").
		Where("user_id = ? AND session_id = ?", userID, sessionID).
		Scan(&row).Error
	if err != nil {
		return 0, 0, err
	}
	return row.Watermark, row.Seq, nil
}
