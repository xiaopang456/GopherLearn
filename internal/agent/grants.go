package agent

import (
	"encoding/json"
	"strings"

	"github.com/xiaopang456/GopherLearn/internal/domain"
)

// This file restores the reference project's read-back authorisation contract.
//
// A compressed card - a session summary item or an episode entry - cites the
// ids of the learner's own messages it was drawn from, and read_history_message
// reads those ids back as raw words. What keeps that readable path from
// becoming a history walk is that the server decides which ids are in play:
// message ids are opaque strings here, but a tool that read whatever ids the
// model asked for would still let it enumerate every row it owns. The grant
// set is the allow-list, assembled from what the window actually showed.

const (
	// HistorySourceSummary marks a grant that came from the session summary
	// injected into this turn's window.
	HistorySourceSummary = "summary"
	// HistorySourceEpisode marks a grant that came from an injected episode.
	HistorySourceEpisode = "episode"
)

// HistoryMessageGrant authorises reading one stored message in one turn, and
// records which compressed card asked for it.
//
// The two sources live in different sessions: summary evidence belongs to the
// session being summarised, while an episode usually describes an earlier one.
// Restricting every readable message to the current session would make the
// episode path unreadable; restricting nothing would let a summary grant reach
// any session the learner owns. The session is therefore carried per grant,
// and the database re-checks it.
//
// Adaptation from the reference: its message ids are sequential integers and
// its watermark bound is a numeric ceiling (`id <= watermark`). Here message
// ids are opaque strings, so the same bound is expressed as closure
// containment - the id must appear in the cited closure the granting card
// carries, which the summary scanner and the episode reader establish before a
// grant is issued at all.
type HistoryMessageGrant struct {
	MessageID  string
	SourceKind string
	// SourceSessionID is the session the row must belong to. It is server state
	// in both directions: the summary grant takes it from the turn, and the
	// episode grant from the episode card that cited the id.
	SourceSessionID string

	// SummaryVersion is the summary version whose decoded document cited this
	// id. Zero means the grant did not come from a decoded summary, which
	// authorises nothing.
	SummaryVersion   uint64
	SummaryWatermark string

	// EpisodeID and EpisodeVersion name the card whose closure cited this id.
	EpisodeID        string
	EpisodeVersion   uint64
	EpisodeWatermark string
}

// UsableIn reports whether this grant still means what it claimed when the
// read tool is built. A grant assembled wrongly - or against a different
// session - must not reach a query.
func (g HistoryMessageGrant) UsableIn(userID, sessionID string) bool {
	if strings.TrimSpace(g.MessageID) == "" || strings.TrimSpace(g.SourceSessionID) == "" {
		return false
	}
	switch g.SourceKind {
	case HistorySourceSummary:
		return g.SummaryVersion > 0 && g.SummaryWatermark != "" &&
			g.SourceSessionID == strings.TrimSpace(sessionID) &&
			domain.CompareMessageIDs(g.MessageID, g.SummaryWatermark) <= 0
	case HistorySourceEpisode:
		return strings.TrimSpace(g.EpisodeID) != "" && g.EpisodeVersion > 0 &&
			(g.EpisodeWatermark == "" || domain.CompareMessageIDs(g.MessageID, g.EpisodeWatermark) <= 0)
	default:
		return false
	}
}

// MergeHistoryMessageGrants joins per-source grant lists into the turn's
// authorisation set, in list order, dropping exact repeats.
//
// Deduplication is on the whole grant rather than on the message id, because an
// id cited by both a summary and an episode has two provenances and the turn
// must be able to say so later.
func MergeHistoryMessageGrants(groups ...[]HistoryMessageGrant) []HistoryMessageGrant {
	type grantKey struct {
		kind, session, messageID, episodeID string
		summaryVersion, episodeVersion      uint64
	}
	var merged []HistoryMessageGrant
	seen := make(map[grantKey]struct{})
	for _, group := range groups {
		for _, grant := range group {
			key := grantKey{
				kind: grant.SourceKind, session: grant.SourceSessionID, episodeID: grant.EpisodeID,
				messageID: grant.MessageID, summaryVersion: grant.SummaryVersion,
				episodeVersion: grant.EpisodeVersion,
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

// HistoryGrantSet is one turn's read-back authorisation set, indexed by
// message id and keeping every provenance that id was granted under.
//
// It is a multimap rather than a set of ids because "may this id be read?" and
// "why may it be read?" have different answers for an id the summary and an
// episode both cite: the read happens once, while the reason stays two.
type HistoryGrantSet struct {
	byMessage map[string][]HistoryMessageGrant
}

// NewHistoryGrantSet filters a merged grant list down to what this turn may
// act on. Unusable grants are dropped rather than reported: they authorise
// nothing, and a grant that reached here wrongly is an assembly bug, not a
// model error.
func NewHistoryGrantSet(userID, sessionID string, grants []HistoryMessageGrant) *HistoryGrantSet {
	set := &HistoryGrantSet{byMessage: make(map[string][]HistoryMessageGrant, len(grants))}
	for _, grant := range grants {
		if !grant.UsableIn(userID, sessionID) {
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

// Len reports how many distinct messages this turn may read. Zero means the
// read-back tool should not be offered at all.
func (s *HistoryGrantSet) Len() int {
	if s == nil {
		return 0
	}
	return len(s.byMessage)
}

// Grants returns every provenance behind one id, or nil when the id is not
// authorised.
func (s *HistoryGrantSet) Grants(messageID string) []HistoryMessageGrant {
	if s == nil {
		return nil
	}
	return s.byMessage[messageID]
}

// Covers reports whether one id was granted specifically from the session the
// stored row turned out to belong to.
func (s *HistoryGrantSet) Covers(messageID, sessionID string) bool {
	for _, grant := range s.Grants(messageID) {
		if grant.SourceSessionID == sessionID {
			return true
		}
	}
	return false
}

// summaryEvidenceKey is how a structured summary document renders one item's
// citations. The scanner reads only the arrays that follow this key, so a
// number or id that happens to appear in goal prose or another field cannot
// authorise a read.
const summaryEvidenceKey = `"evidence_message_ids"`

// VisibleSummaryEvidenceIDs keeps the cited ids whose token is complete in the
// rendered block.
//
// It is a token scanner rather than a substring search: an id inside a longer
// id is not a visible id, and a cut anywhere - before the key finishes, between
// the key and its array, after `[`, or in the middle of an id - yields fewer
// ids and never a wrong one. The intersection with the document's own cited
// ids keeps this a pure tightening: nothing an untruncated summary could not
// already grant.
//
// Adaptation from the reference: its ids are JSON numbers, so its scanner
// reads number tokens; here message ids are strings, so the scanner reads
// complete quoted string tokens. The visibility rule - only what the rendered
// text actually shows, decided per id - is unchanged.
func VisibleSummaryEvidenceIDs(rendered string, cited []string) []string {
	visible := make(map[string]struct{}, len(cited))
	for at := 0; at < len(rendered); {
		offset := strings.Index(rendered[at:], summaryEvidenceKey)
		if offset < 0 {
			break
		}
		cursor := at + offset + len(summaryEvidenceKey)
		for _, id := range jsonStringTokens(rendered, cursor) {
			visible[id] = struct{}{}
		}
		at = cursor
	}
	if len(visible) == 0 {
		return nil
	}
	ids := make([]string, 0, len(cited))
	seen := make(map[string]struct{}, len(cited))
	for _, id := range cited {
		if id == "" {
			continue
		}
		if _, ok := visible[id]; !ok {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids
}

// jsonStringTokens reads the complete quoted strings of the array that starts
// after the cursor, which must be `:` then `[`. It stops at the first
// character that cannot belong to such an array, which is also what a
// mid-array truncation looks like. An unterminated string token yields nothing:
// a cut inside a quoted id is a cut, not an id.
func jsonStringTokens(text string, cursor int) []string {
	cursor = skipJSONSpace(text, cursor)
	if cursor >= len(text) || text[cursor] != ':' {
		return nil
	}
	cursor = skipJSONSpace(text, cursor+1)
	if cursor >= len(text) || text[cursor] != '[' {
		return nil
	}
	var ids []string
array:
	for index := cursor + 1; index < len(text); {
		index = skipJSONSpace(text, index)
		if index >= len(text) {
			break
		}
		if text[index] == ']' {
			break
		}
		if text[index] != '"' {
			// Not a string where one belongs: stop trusting this array.
			break
		}
		start := index + 1
		end := start
		for end < len(text) && text[end] != '"' {
			if text[end] == '\\' {
				// An escape pair is one unit; a backslash cut short by the end of
				// the text is an unterminated token.
				end++
				if end >= len(text) {
					break array
				}
			}
			end++
		}
		if end >= len(text) {
			// The cut stopped inside this string token.
			break
		}
		terminated := skipJSONSpace(text, end+1)
		if terminated >= len(text) {
			break
		}
		if text[terminated] != ',' && text[terminated] != ']' {
			break array
		}
		// text[index:end+1] is the complete quoted token, escapes included. Only
		// a token the JSON grammar accepts is an id: anything else is a cut or
		// a shape this array never held.
		var id string
		if err := json.Unmarshal([]byte(text[index:end+1]), &id); err == nil && id != "" {
			ids = append(ids, id)
		}
		index = terminated + 1
	}
	return ids
}

func skipJSONSpace(text string, index int) int {
	for index < len(text) {
		switch text[index] {
		case ' ', '\t', '\n', '\r':
			index++
		default:
			return index
		}
	}
	return index
}

// HistoryGrantsFromSummary turns the evidence ids of the summary that actually
// reached this turn's window into read-back authorisations.
//
// rendered is the block this turn sends, not the stored row. A long summary is
// cut to the remaining token budget, and a cut document still decodes while its
// tail is simply absent from the prompt - so visibility is decided per id
// against the text the model will actually see. Granting an id the budget
// trimmed away would mean the model can read back a citation it was never
// shown, which is exactly the "only ids you were shown" rule.
//
// Every guard below is a different way a summary can be present without
// licensing a read: no rendered text, no version, or a document with no cited
// ids. Each yields nothing rather than an error, because the summary is an
// optimisation and a turn that cannot read it back must still be answered.
//
// The decode itself is a seam: this file does not own the summary schema. The
// caller hands in citedEvidenceIDs, which the summary side fills from its
// decoded document; while the deployment still stores prose summaries the
// caller passes nil and no summary grant exists, exactly as in the reference
// project's modes that do not read evidence back.
func HistoryGrantsFromSummary(rendered, sessionID string, version uint64, watermark string, citedEvidenceIDs []string) []HistoryMessageGrant {
	sessionID = strings.TrimSpace(sessionID)
	if rendered == "" || version == 0 || sessionID == "" || len(citedEvidenceIDs) == 0 {
		return nil
	}
	ids := VisibleSummaryEvidenceIDs(rendered, citedEvidenceIDs)
	grants := make([]HistoryMessageGrant, 0, len(ids))
	for _, id := range ids {
		if strings.TrimSpace(id) == "" {
			continue
		}
		grants = append(grants, HistoryMessageGrant{
			MessageID:        id,
			SourceKind:       HistorySourceSummary,
			SourceSessionID:  sessionID,
			SummaryVersion:   version,
			SummaryWatermark: watermark,
		})
	}
	return grants
}
