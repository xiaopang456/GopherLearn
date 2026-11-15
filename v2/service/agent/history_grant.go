package agent

import (
	"sort"
	"strings"
	"sync"
)

const (
	// HistorySourceSummary marks a grant that came from the session summary
	// injected into this run's window.
	HistorySourceSummary = "summary"
	// HistorySourceEpisode marks a grant that came from an injected episode.
	HistorySourceEpisode = "episode"
	// HistorySourceSummarySegment marks a grant that came from a summary
	// segment injected into this run's window.
	HistorySourceSummarySegment = "summary_segment"
	// HistorySourceWhiteboard marks a grant that came from the session board
	// injected into this run's window.
	//
	// It is a kind of its own rather than another summary-segment grant: the
	// board is bounded by the message it was last written at, and a segment
	// by the stretch it covers. The same id therefore has different provenances
	// under the two, and the run has to be able to say which card granted it.
	HistorySourceWhiteboard = "whiteboard"
	// HistorySourceSessionCard marks a grant that came from an injected
	// session card.
	//
	// A card is the entry point to an earlier session: it says what that
	// session was about and points at the messages and summary segments it
	// was written from. The read-back it authorises is therefore two steps
	// down - the card, then the source session - and the grant carries the
	// card's own watermark so a citation above it is refused.
	HistorySourceSessionCard = "session_card"
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

	// SegmentSeq is the sequence number of the summary segment a grant came
	// from, inside its own session.
	SegmentSeq int
	// SegmentStartMessageID and SegmentEndMessageID are the segment's range.
	//
	// Both ends are carried because one end is not enough: a segment that
	// was deleted and a later one that took the same sequence number can end
	// at the same message while starting somewhere else, and a check that
	// only looked at the end would accept the replacement.
	SegmentStartMessageID uint
	SegmentEndMessageID   uint

	// WhiteboardVersion is the board's version a grant came from, and
	// WhiteboardCoveredMessageID is the message the board was last written
	// at. A board entry cannot cite a message the board does not reach.
	WhiteboardVersion          uint64
	WhiteboardCoveredMessageID uint

	// CardID and CardVersion identify the session card a grant was drawn
	// from, and CardWatermark is that card's own high-water message id.
	//
	// They are set on the card's own grants, and on the grants a card
	// authorised in another session: a summary segment of the source session
	// is readable only because a card pointed at that session, and the card
	// is what the check is made against.
	CardID        string
	CardVersion   uint64
	CardWatermark uint
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
	case HistorySourceSummarySegment:
		// A segment is a stretch of this session's own turns. The grant is
		// good for the messages inside that stretch and nothing else: a
		// segment cites the messages it was compressed from, and a message
		// outside that stretch was not one of them.
		//
		// The lower bound is the segment's start. A grant below it is a
		// message some other stretch covered, and this segment did not say
		// anything about it.
		//
		// The session is this run's own, unless a card authorised the
		// segment: the source session of a card is another conversation, and
		// its segments are browsable only because the card pointed there.
		return g.SegmentSeq > 0 && g.SegmentEndMessageID > 0 &&
			(g.SegmentStartMessageID == 0 || g.MessageID >= g.SegmentStartMessageID) &&
			g.MessageID <= g.SegmentEndMessageID &&
			(g.SourceSessionID == strings.TrimSpace(run.SessionID) || strings.TrimSpace(g.CardID) != "")
	case HistorySourceWhiteboard:
		// The board is this session's own current state, and it is bounded by
		// the message it was last written at.
		return g.WhiteboardVersion > 0 && g.WhiteboardCoveredMessageID > 0 &&
			g.MessageID <= g.WhiteboardCoveredMessageID &&
			g.SourceSessionID == strings.TrimSpace(run.SessionID)
	case HistorySourceSessionCard:
		// A card's citation is bounded by the card's own watermark: the
		// session may have moved on since the card was written, and what the
		// card said about that later part is not what it said.
		if strings.TrimSpace(g.CardID) == "" || g.CardVersion == 0 {
			return false
		}
		return g.CardWatermark == 0 || g.MessageID <= g.CardWatermark
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
		segmentSeq                 int
		segmentStart, segmentEnd   uint
		whiteboardVer              uint64
		whiteboardEnd              uint
		cardID                     string
		cardVer                    uint64
		cardWatermark              uint
	}
	var merged []HistoryMessageGrant
	seen := make(map[grantKey]struct{})
	for _, group := range groups {
		for _, grant := range group {
			key := grantKey{
				kind: grant.SourceKind, session: grant.SourceSessionID, episodeID: grant.EpisodeID,
				messageID: grant.MessageID, watermark: grant.SummaryWatermark,
				summaryVersion: grant.SummaryVersion, episodeVer: grant.EpisodeVersion,
				segmentSeq: grant.SegmentSeq, segmentStart: grant.SegmentStartMessageID,
				segmentEnd:    grant.SegmentEndMessageID,
				whiteboardVer: grant.WhiteboardVersion, whiteboardEnd: grant.WhiteboardCoveredMessageID,
				cardID: grant.CardID, cardVer: grant.CardVersion, cardWatermark: grant.CardWatermark,
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
	// mu guards byMessage. A tool that finds evidence mid-turn - the browse
	// tool reading an older segment - extends the set while another tool may
	// be reading it.
	mu        sync.Mutex
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
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.byMessage)
}

// Grants returns every provenance behind one id, or nil when the id is not
// authorised.
func (s *HistoryGrantSet) Grants(messageID uint) []HistoryMessageGrant {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byMessage[messageID]
}

// References returns the identifiers this run's conversation evidence carried:
// the message ids it may read back, and the sequence numbers of the summary
// segments it showed.
//
// They are the numbers a model that answers from the conversation may point
// at - the message ids the evidence cited, and the numbers the segments were
// labelled with. A number that is neither points at nothing.
func (s *HistoryGrantSet) References() []int {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := make(map[int]struct{}, len(s.byMessage))
	out := make([]int, 0, len(s.byMessage))
	for messageID, grants := range s.byMessage {
		if _, ok := seen[int(messageID)]; !ok {
			seen[int(messageID)] = struct{}{}
			out = append(out, int(messageID))
		}
		for _, grant := range grants {
			if grant.SegmentSeq <= 0 {
				continue
			}
			if _, ok := seen[grant.SegmentSeq]; !ok {
				seen[grant.SegmentSeq] = struct{}{}
				out = append(out, grant.SegmentSeq)
			}
		}
	}
	sort.Ints(out)
	return out
}

// Extend adds grants that a tool discovered during the turn.
//
// A tool that reads something the window did not carry - the browse tool
// opening an older segment - has to be able to say so, or the read-back tool
// would refuse the ids the browse tool just showed the model. Only grants
// that pass the same check as the ones the window handed over are added, so
// extending the set cannot widen what the run may read.
func (s *HistoryGrantSet) Extend(run RunContext, grants []HistoryMessageGrant) {
	if s == nil || len(grants) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, grant := range grants {
		if !grant.UsableIn(run) {
			continue
		}
		duplicate := false
		for _, held := range s.byMessage[grant.MessageID] {
			if held == grant {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		s.byMessage[grant.MessageID] = append(s.byMessage[grant.MessageID], grant)
	}
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

// CardSessions are the earlier sessions this run may reach because a session
// card pointed at them.
//
// It is the answer to "may this run browse the summary of that other
// conversation", and the answer is yes only for a session a card in this very
// run named. The model may not fill in an arbitrary session id: a session
// nobody showed it is not reachable, and the list here is the whole of what
// is.
func (s *HistoryGrantSet) CardSessions() []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := make(map[string]struct{})
	out := make([]string, 0, 4)
	for _, grants := range s.byMessage {
		for _, grant := range grants {
			if grant.SourceKind != HistorySourceSessionCard {
				continue
			}
			session := strings.TrimSpace(grant.SourceSessionID)
			if session == "" {
				continue
			}
			if _, exists := seen[session]; exists {
				continue
			}
			seen[session] = struct{}{}
			out = append(out, session)
		}
	}
	sort.Strings(out)
	return out
}

// AuthorisesSession reports whether this run may read the given session.
//
// The run's own session is always readable. An earlier one is readable only
// when a card in this run pointed at it.
func (s *HistoryGrantSet) AuthorisesSession(run RunContext, sessionID string) bool {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return false
	}
	if sessionID == strings.TrimSpace(run.SessionID) {
		return true
	}
	for _, granted := range s.CardSessions() {
		if granted == sessionID {
			return true
		}
	}
	return false
}

// CardAuthorisation returns the card that pointed at a session.
//
// It is the grant that makes a source session reachable, and the card's own
// identity is what the grants issued against that session carry. A run that
// reaches a session without naming the card that authorised it would have a
// way to read an earlier conversation that no card pointed at.
//
// The watermark comes with it because it is the ceiling of what the card
// authorises: the card describes the session as far as it had reached, and a
// message above that line is not something the card ever saw.
func (s *HistoryGrantSet) CardAuthorisation(sessionID string) (string, uint64, uint, bool) {
	if s == nil {
		return "", 0, 0, false
	}
	sessionID = strings.TrimSpace(sessionID)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, grants := range s.byMessage {
		for _, grant := range grants {
			if grant.SourceKind != HistorySourceSessionCard {
				continue
			}
			if strings.TrimSpace(grant.SourceSessionID) != sessionID {
				continue
			}
			return strings.TrimSpace(grant.CardID), grant.CardVersion, grant.CardWatermark, true
		}
	}
	return "", 0, 0, false
}
