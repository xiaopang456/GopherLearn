package episoderelation

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"

	"GopherAI/model"
	"GopherAI/service/episoderelation"
)

// The production CandidateSource.
//
// Phase 1 shipped the interface and three test doubles, which meant the finder
// had never read a row from a database and every filter it claimed was a claim
// about a map literal. This is the implementation those tests were describing.
//
// What it may read is deliberately narrow, and the narrowness is enforced by
// the SQL rather than by the caller remembering: one learner's own Episode
// rows, active and indexed, excluding the session being projected. It never
// touches the knowledge documents, the semantic memory tables, or anything
// belonging to an experiment.

// DefaultHistoryLimit bounds how many Episodes one candidate search loads.
//
// The finder bounds the *candidates*; this bounds the *rows read to find
// them*. Without it a learner with two thousand sessions would have all two
// thousand documents deserialised into memory on every projection, to keep
// eight - which is the kind of cost that only shows up on the account with the
// most history.
const DefaultHistoryLimit = 200

// CandidateRepository reads one learner's own episodic history.
type CandidateRepository struct {
	db    *gorm.DB
	limit int
}

// NewCandidateRepository refuses a nil handle rather than failing at the first
// query.
func NewCandidateRepository(db *gorm.DB, limit int) (*CandidateRepository, error) {
	if db == nil {
		return nil, fmt.Errorf("the candidate repository needs a database")
	}
	if limit <= 0 {
		limit = DefaultHistoryLimit
	}
	return &CandidateRepository{db: db, limit: limit}, nil
}

// ActiveAssertions returns the learner's decision and correction assertions
// from active, indexed Episodes, excluding one session.
//
// The owner and session filters are in the WHERE clause and checked again in
// the loop. A scoped query that returns nothing and a scoped query that
// returns someone else's row fail differently, and only one of those is caught
// by the query - which is why the second check is not redundant.
func (r *CandidateRepository) ActiveAssertions(ctx context.Context, userID int64,
	excludeSessionID string) ([]episoderelation.Candidate, error) {
	out := []episoderelation.Candidate{}
	if userID <= 0 {
		return out, nil
	}
	var episodes []model.EpisodeMemory
	query := r.db.WithContext(ctx).
		Where("user_id = ? AND status = ? AND index_status = ?",
			userID, model.EpisodeStatusActive, model.EpisodeIndexIndexed).
		Order("last_message_at desc, id asc").
		Limit(r.limit)
	if session := strings.TrimSpace(excludeSessionID); session != "" {
		query = query.Where("session_id <> ?", session)
	}
	if err := query.Find(&episodes).Error; err != nil {
		return nil, fmt.Errorf("read the learner's episodes: %w", err)
	}

	// The assertion times come from the evidence messages, exactly as the
	// governor dates them. Reading them in one query rather than per
	// assertion keeps a candidate search to two round trips whatever the
	// history size.
	wanted := map[uint]struct{}{}
	for _, episodeRow := range episodes {
		for _, entries := range [][]model.EpisodeEvidence{
			episodeRow.Document.Decisions, episodeRow.Document.UserCorrections,
		} {
			for _, entry := range entries {
				for _, id := range entry.EvidenceMessageIDs {
					wanted[id] = struct{}{}
				}
			}
		}
	}
	times, err := r.messageTimes(ctx, userID, wanted)
	if err != nil {
		return nil, err
	}

	// The states are read in one query too, so a candidate carries what the
	// governance layer already knows about it.
	states, err := r.assertionStates(ctx, userID, episodes)
	if err != nil {
		return nil, err
	}

	for _, episodeRow := range episodes {
		// Re-checked, not assumed. See the comment on this method.
		if episodeRow.UserID != userID {
			continue
		}
		if session := strings.TrimSpace(excludeSessionID); session != "" &&
			episodeRow.SessionID == session {
			continue
		}
		for _, assertion := range model.AssertionsOfWithClock(episodeRow,
			func(ids []uint) (time.Time, bool) { return latestOf(ids, times) }) {
			switch assertion.Kind {
			case model.AssertionDecision, model.AssertionCorrection:
			default:
				// Synopsis asserts nothing; an open loop is not closed by a
				// relation. Both are excluded here so they never reach the
				// classifier at all.
				continue
			}
			out = append(out, episoderelation.Candidate{
				AssertionID: assertion.ID, EpisodeID: episodeRow.ID,
				SessionID: episodeRow.SessionID, UserID: userID,
				Kind: assertion.Kind, Text: assertion.Text,
				OccurredAt: assertion.OccurredAt, TimeKnown: assertion.TimeKnown,
				State: states[assertion.ID],
			})
		}
	}
	// A total order, so two searches over one history return the same set in
	// the same sequence and a re-projection is comparable with the run before
	// it rather than merely similar.
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].OccurredAt.Equal(out[j].OccurredAt) {
			return out[i].OccurredAt.After(out[j].OccurredAt)
		}
		return out[i].AssertionID < out[j].AssertionID
	})
	return out, nil
}

// messageTimes dates the evidence. A message the learner deleted has no row
// and therefore no time, and the assertion resting on it is time-unknown -
// which the finder then excludes rather than ordering against a time it
// invented.
func (r *CandidateRepository) messageTimes(ctx context.Context, userID int64,
	wanted map[uint]struct{}) (map[uint]time.Time, error) {
	out := map[uint]time.Time{}
	if len(wanted) == 0 {
		return out, nil
	}
	ids := make([]uint, 0, len(wanted))
	for id := range wanted {
		ids = append(ids, id)
	}
	var rows []struct {
		ID        uint
		CreatedAt time.Time
	}
	if err := r.db.WithContext(ctx).Model(&model.Message{}).
		Select("messages.id, messages.created_at").
		Joins("JOIN sessions ON sessions.id = messages.session_id").
		Where("messages.id IN ? AND sessions.user_id = ?", ids, userID).
		Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("date the assertion evidence: %w", err)
	}
	for _, row := range rows {
		out[row.ID] = row.CreatedAt
	}
	return out, nil
}

// assertionStates reads what governance already says about these episodes'
// assertions.
func (r *CandidateRepository) assertionStates(ctx context.Context, userID int64,
	episodes []model.EpisodeMemory) (map[string]model.AssertionState, error) {
	out := map[string]model.AssertionState{}
	if len(episodes) == 0 {
		return out, nil
	}
	ids := make([]string, 0, len(episodes))
	for _, episodeRow := range episodes {
		ids = append(ids, episodeRow.ID)
	}
	var rows []model.EpisodeAssertionState
	if err := r.db.WithContext(ctx).
		Where("user_id = ? AND episode_id IN ?", userID, ids).
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("read the assertion states: %w", err)
	}
	for _, row := range rows {
		out[row.AssertionID] = row.State
	}
	return out, nil
}

// latestOf dates an assertion by its newest piece of evidence: a decision is
// made once everything supporting it has been said, not when the first part
// was.
func latestOf(ids []uint, times map[uint]time.Time) (time.Time, bool) {
	latest, known := time.Time{}, false
	for _, id := range ids {
		at, ok := times[id]
		if !ok {
			continue
		}
		if !known || at.After(latest) {
			latest, known = at, true
		}
	}
	return latest, known
}

var _ episoderelation.CandidateSource = (*CandidateRepository)(nil)
