package agent

import "strings"

const (
	// HistorySourceSummary marks a grant that came from the session summary
	// injected into this run's window.
	HistorySourceSummary = "summary"
	// HistorySourceEpisode marks a grant that came from an injected episode.
	HistorySourceEpisode = "episode"
)

// HistoryMessageGrant authorises reading one stored message in one run, and
// records which compressed card asked for it.
//
// A bare id is not enough because the two sources live in different sessions:
// summary evidence belongs to the session being summarised, while an episode
// usually describes an earlier one. Restricting every readable message to the
// current session would make the episode path unreadable; restricting nothing
// would let a summary grant reach any session the learner owns. The session is
// therefore carried per grant, and the database re-checks it.
type HistoryMessageGrant struct {
	MessageID  uint
	SourceKind string
	// SourceSessionID is the session the row must belong to. It is server state
	// in both directions: the summary grant takes it from the run, and the
	// episode grant from the EpisodeMemory row that cited the id.
	SourceSessionID string

	SummaryVersion   uint64
	SummaryWatermark uint

	EpisodeID      string
	EpisodeVersion uint64
	// EpisodeWatermark is the cited session's own high-water message id when this
	// version of the card was written. Zero means the row predates the field and
	// bounds nothing; a positive value is a ceiling, and a grant above it is not a
	// citation the card ever had.
	EpisodeWatermark uint
}

// UsableIn reports whether this grant still means what it claimed when the
// tool is built.
//
// The ContextBuilder already filters ids against the summary watermark, but the
// builder and the tool are separate files with separate futures, and this is the
// place where the filter is load-bearing for a database read. Re-checking here
// means a grant that was assembled wrongly - or assembled against a different
// session - cannot reach the query at all.
func (g HistoryMessageGrant) UsableIn(run RunContext) bool {
	if g.MessageID == 0 || strings.TrimSpace(g.SourceSessionID) == "" {
		return false
	}
	switch g.SourceKind {
	case HistorySourceSummary:
		// Summary grants are bounded by the watermark they were drawn from, and
		// only ever cover the session this run is reading.
		return g.SummaryVersion > 0 && g.SummaryWatermark > 0 &&
			g.MessageID <= g.SummaryWatermark &&
			g.SourceSessionID == strings.TrimSpace(run.SessionID)
	case HistorySourceEpisode:
		if strings.TrimSpace(g.EpisodeID) == "" || g.EpisodeVersion == 0 {
			return false
		}
		// The same bound the builder applied when it drew the grant, re-checked here
		// because this is the place the database read depends on: a grant assembled
		// against a different card version, or carried over from an older one, must not
		// reach the query.
		return g.EpisodeWatermark == 0 || g.MessageID <= g.EpisodeWatermark
	default:
		return false
	}
}

// MergeHistoryMessageGrants joins per-source grant lists into the run's
// authorisation set, in list order, dropping exact repeats.
//
// Deduplication is on the whole grant rather than on the message id, because an
// id cited by both a summary and an episode has two provenances and the run must
// be able to say so later. Repeats of the same provenance - the same id cited by
// two entries of one episode - carry no extra information and would otherwise
// grow the query.
func MergeHistoryMessageGrants(groups ...[]HistoryMessageGrant) []HistoryMessageGrant {
	type grantKey struct {
		kind, session, episodeID   string
		messageID, watermark       uint
		summaryVersion, episodeVer uint64
	}
	var merged []HistoryMessageGrant
	seen := make(map[grantKey]struct{})
	for _, group := range groups {
		for _, grant := range group {
			key := grantKey{
				kind: grant.SourceKind, session: grant.SourceSessionID, episodeID: grant.EpisodeID,
				messageID: grant.MessageID, watermark: grant.SummaryWatermark,
				summaryVersion: grant.SummaryVersion, episodeVer: grant.EpisodeVersion,
			}
			if _, duplicate := seen[key]; duplicate {
				continue
			}
			seen[key] = struct{}{}
			merged = append(merged, grant)
		}
	}
	return merged
}

// HistoryGrantSet is one run's read-back authorisation set, indexed by message
// id and keeping every provenance that id was granted under.
//
// The index is a multimap rather than a set of ids because "may this id be
// read?" and "why may it be read?" have different answers for an id the summary
// and an episode both cite: the read happens once, while the reason stays two.
type HistoryGrantSet struct {
	byMessage map[uint][]HistoryMessageGrant
}

// NewHistoryGrantSet filters a merged grant list down to what this run may act
// on. Unusable grants are dropped rather than reported: they authorise nothing,
// and a grant that reached here wrongly is a builder bug, not a model error.
func NewHistoryGrantSet(run RunContext, grants []HistoryMessageGrant) *HistoryGrantSet {
	set := &HistoryGrantSet{byMessage: make(map[uint][]HistoryMessageGrant, len(grants))}
	for _, grant := range grants {
		if !grant.UsableIn(run) {
			continue
		}
		duplicate := false
		for _, held := range set.byMessage[grant.MessageID] {
			if held == grant {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		set.byMessage[grant.MessageID] = append(set.byMessage[grant.MessageID], grant)
	}
	return set
}

// Len reports how many distinct messages this run may read. Zero means the
// read-back tool should not be offered at all.
func (s *HistoryGrantSet) Len() int {
	if s == nil {
		return 0
	}
	return len(s.byMessage)
}

// Grants returns every provenance behind one id, or nil when the id is not
// authorised.
func (s *HistoryGrantSet) Grants(messageID uint) []HistoryMessageGrant {
	if s == nil {
		return nil
	}
	return s.byMessage[messageID]
}

// Covers reports whether one id was granted specifically from the session the
// stored row turned out to belong to. The database query is already scoped this
// way; the check stays because a reader that ignored the scoping would otherwise
// be trusted to have filtered correctly.
func (s *HistoryGrantSet) Covers(messageID uint, sessionID string) bool {
	for _, grant := range s.Grants(messageID) {
		if grant.SourceSessionID == sessionID {
			return true
		}
	}
	return false
}
