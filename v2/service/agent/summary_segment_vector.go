package agent

import (
	"context"

	summarysegmentdao "GopherAI/dao/summarysegment"
)

// The vector channel and the recent-use record.
//
// The two live together because they answer the same question from two
// sides: the vector channel finds the segments a question is about, and the
// recent-use record remembers the ones the last few turns reached for.
//
// The record exists because the newest segments are not always the ones the
// learner is asking about. A learner who has been going back to the same
// stretch of the conversation for three turns wants that stretch in front of
// them again, and the newest segments would have moved past it.

// SummarySegmentVectorHit is one segment the vector channel found.
//
// It is the index's own hit type, so a search result does not have to be
// translated between the index and the channel that reads it.
type SummarySegmentVectorHit = summarysegmentdao.SummarySegmentVectorHit

// SummarySegmentVectorSearcher is the vector channel of the preselection.
//
// It is the index the segments are written to. A deployment without one runs
// the lexical channel alone, which is a narrower selection and not a broken
// one.
type SummarySegmentVectorSearcher interface {
	// SearchByVector returns the segments of one session that are closest to
	// the query vector, best first.
	SearchByVector(ctx context.Context, userID int64, sessionID string, vector []float32, limit int) ([]SummarySegmentVectorHit, error)
}

// SummarySegmentRecentUse records the segments a turn reached for.
//
// The record is derived, like the index: losing it costs the learner the
// segments they were asking about, and nothing else.
type SummarySegmentRecentUse interface {
	// RecordRecentUse remembers that a segment was reached for.
	RecordRecentUse(ctx context.Context, userID int64, sessionID string, seq int) error
	// ListRecentUse returns the segments that were reached for, newest first.
	ListRecentUse(ctx context.Context, userID int64, sessionID string, limit int) ([]int, error)
}

// SummarySegmentRecentUseLimit is how many recent segments the read side adds
// to the resident set.
const SummarySegmentRecentUseLimit = 4
