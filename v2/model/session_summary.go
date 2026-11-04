package model

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
)

const (
	SessionSummarySchemaV2      = 2
	SessionSummaryItemLimit     = 20
	SessionSummaryItemMaxRunes  = 1000
	SessionSummaryEvidenceLimit = 32
	SessionSummarySourceLimit   = 64
)

// The groups of a summary document. They are named the way the JSON names
// them, because a truncation report has to say which group it cut.
const (
	SummaryGroupConfirmedDecisions = "confirmed_decisions"
	SummaryGroupOpenLoops          = "open_loops"
	SummaryGroupUserCorrections    = "user_corrections"
	SummaryGroupCompletedItems     = "completed_items"
	SummaryGroupKeyErrors          = "key_errors"
	SummaryGroupReferences         = "references"
)

// SummaryBudget is how much one summary document may hold.
//
// These limits are a compression budget, not a rule about what a document may
// say. A summary that outgrew its budget is still a summary: the rolling
// summary trims it to the budget instead of throwing the whole thing away,
// because a rejected summary is a summary that never updates again.
type SummaryBudget struct {
	// GroupItemLimit is how many items one group may hold.
	GroupItemLimit int
	// OpenLoopsItemLimit is the same limit for the open loops group, which is
	// the group that grows fastest and the one an operator may want to widen.
	OpenLoopsItemLimit int
	// ItemEvidenceLimit is how many message ids one item may cite.
	ItemEvidenceLimit int
	// EvidenceIDLimit is how many distinct message ids the whole document may
	// cite. It has to grow with the group limits: every item of every group
	// cites at least one message.
	EvidenceIDLimit int
}

// DefaultSummaryBudget is the budget of a summary that was not given another
// one.
func DefaultSummaryBudget() SummaryBudget {
	return SummaryBudget{
		GroupItemLimit:     SessionSummaryItemLimit,
		OpenLoopsItemLimit: SessionSummaryItemLimit,
		ItemEvidenceLimit:  SessionSummaryEvidenceLimit,
		EvidenceIDLimit:    SessionSummarySourceLimit,
	}
}

// GroupLimit is the limit that applies to the named group. A limit of zero
// means "no limit".
func (b SummaryBudget) GroupLimit(group string) int {
	if group == SummaryGroupOpenLoops && b.OpenLoopsItemLimit > 0 {
		return b.OpenLoopsItemLimit
	}
	return b.GroupItemLimit
}

// SummaryTruncation is one cut the compression budget made.
type SummaryTruncation struct {
	// Kind is what was counted: "items" for entries of a group, or
	// "evidence_ids" for the message ids a group cites.
	Kind string `json:"kind"`
	// Group is the group the cut came from.
	Group string `json:"group"`
	// Kept is how many survived.
	Kept int `json:"kept"`
	// Dropped is how many were cut.
	Dropped int `json:"dropped"`
	// DroppedMessageIDs are the messages the cut took away: the messages the
	// dropped items cited, or the citations that were cut out of the items
	// that stayed. They are what the summary lost, and a run that wants to
	// know whether what was lost mattered has to be able to name it.
	DroppedMessageIDs []uint `json:"dropped_message_ids,omitempty"`
}

// SessionSummaryEvidence is the auditable form used by summary schema v2.
// Empty EvidenceMessageIDs are accepted only for legacy v1 items; producers
// must separately prove that such an item was preserved from an old summary.
type SessionSummaryEvidence struct {
	Text               string `json:"text"`
	EvidenceMessageIDs []uint `json:"evidence_message_ids"`
}

type SessionSummaryEvidenceList []SessionSummaryEvidence

// UnmarshalJSON accepts both the v1 string arrays already stored in MySQL and
// v2 evidence objects. MarshalJSON always emits the v2 object representation.
func (l *SessionSummaryEvidenceList) UnmarshalJSON(data []byte) error {
	var values []json.RawMessage
	if err := json.Unmarshal(data, &values); err != nil {
		return err
	}
	items := make(SessionSummaryEvidenceList, 0, len(values))
	for _, value := range values {
		var legacy string
		if err := json.Unmarshal(value, &legacy); err == nil {
			items = append(items, SessionSummaryEvidence{Text: legacy})
			continue
		}
		decoder := json.NewDecoder(bytes.NewReader(value))
		decoder.DisallowUnknownFields()
		var item SessionSummaryEvidence
		if err := decoder.Decode(&item); err != nil {
			return err
		}
		if err := ensureSummaryJSONEOF(decoder); err != nil {
			return err
		}
		items = append(items, item)
	}
	*l = items
	return nil
}

type SessionSummaryDocument struct {
	SchemaVersion      int                        `json:"schema_version,omitempty"`
	CurrentGoal        string                     `json:"current_goal"`
	ConfirmedDecisions SessionSummaryEvidenceList `json:"confirmed_decisions"`
	CompletedItems     []string                   `json:"completed_items"`
	OpenLoops          SessionSummaryEvidenceList `json:"open_loops"`
	UserCorrections    SessionSummaryEvidenceList `json:"user_corrections"`
	KeyErrors          []string                   `json:"key_errors"`
	References         []string                   `json:"references"`
}

// DecodeSessionSummaryDocument reads a summary document.
//
// It checks everything about the document except how much of it there is. The
// item limits are a compression budget rather than a rule about what a summary
// may say, so a document that outgrew its budget still decodes: the rolling
// summary trims it to the budget instead of throwing the whole summary away.
// A summary that is thrown away is a summary that never updates again.
//
// The budget itself is checked by Validate, against the budget the deployment
// configured.
func DecodeSessionSummaryDocument(content string) (SessionSummaryDocument, error) {
	content = strings.TrimSpace(content)
	if !strings.HasPrefix(content, "{") {
		return SessionSummaryDocument{}, fmt.Errorf("session summary must be a JSON object")
	}
	decoder := json.NewDecoder(strings.NewReader(content))
	decoder.DisallowUnknownFields()
	var document SessionSummaryDocument
	if err := decoder.Decode(&document); err != nil {
		return SessionSummaryDocument{}, fmt.Errorf("decode session summary: %w", err)
	}
	if err := ensureSummaryJSONEOF(decoder); err != nil {
		return SessionSummaryDocument{}, err
	}
	if document.SchemaVersion != 0 && document.SchemaVersion != SessionSummarySchemaV2 {
		return SessionSummaryDocument{}, fmt.Errorf("unsupported session summary schema version %d", document.SchemaVersion)
	}
	if err := document.ValidateContent(); err != nil {
		return SessionSummaryDocument{}, err
	}
	return document, nil
}

func ensureSummaryJSONEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("decode trailing session summary: %w", err)
	}
	return fmt.Errorf("session summary contains trailing JSON")
}

// ValidateContent checks everything about a document except how much of it
// there is: the goal, every evidence item and every plain item have to be
// well formed.
func (d *SessionSummaryDocument) ValidateContent() error {
	if d == nil || len([]rune(strings.TrimSpace(d.CurrentGoal))) > SessionSummaryItemMaxRunes {
		return fmt.Errorf("invalid session summary goal")
	}
	for _, group := range []SessionSummaryEvidenceList{d.ConfirmedDecisions, d.OpenLoops, d.UserCorrections} {
		for _, item := range group {
			if err := item.Validate(); err != nil {
				return err
			}
		}
	}
	for _, group := range [][]string{d.CompletedItems, d.KeyErrors, d.References} {
		for _, item := range group {
			if len([]rune(strings.TrimSpace(item))) > SessionSummaryItemMaxRunes {
				return fmt.Errorf("session summary item is too long")
			}
		}
	}
	return nil
}

// Validate checks that the document is a summary the product can store and
// use: every item is well formed and the document stays inside the budget.
//
// The budget is the deployment's, not the schema's: an operator who widened
// the open loops limit gets a document that is valid against that budget.
func (d *SessionSummaryDocument) Validate(budget SummaryBudget) error {
	if err := d.ValidateContent(); err != nil {
		return err
	}
	if budget.GroupItemLimit > 0 {
		for _, group := range []struct {
			name  string
			items SessionSummaryEvidenceList
		}{
			{SummaryGroupConfirmedDecisions, d.ConfirmedDecisions},
			{SummaryGroupOpenLoops, d.OpenLoops},
			{SummaryGroupUserCorrections, d.UserCorrections},
		} {
			if len(group.items) > budget.GroupLimit(group.name) {
				return fmt.Errorf("session summary has too many evidence items")
			}
		}
		for _, group := range [][]string{d.CompletedItems, d.KeyErrors, d.References} {
			if len(group) > budget.GroupItemLimit {
				return fmt.Errorf("session summary has too many items")
			}
		}
	}
	if budget.ItemEvidenceLimit > 0 {
		for _, group := range []SessionSummaryEvidenceList{d.ConfirmedDecisions, d.OpenLoops, d.UserCorrections} {
			for _, item := range group {
				if len(item.EvidenceMessageIDs) > budget.ItemEvidenceLimit {
					return fmt.Errorf("invalid session summary evidence")
				}
			}
		}
	}
	if budget.EvidenceIDLimit > 0 && len(d.EvidenceMessageIDs()) > budget.EvidenceIDLimit {
		return fmt.Errorf("session summary has too many evidence messages")
	}
	return nil
}

func (e SessionSummaryEvidence) Validate() error {
	if strings.TrimSpace(e.Text) == "" || len([]rune(strings.TrimSpace(e.Text))) > SessionSummaryItemMaxRunes {
		return fmt.Errorf("invalid session summary evidence")
	}
	seen := make(map[uint]struct{}, len(e.EvidenceMessageIDs))
	for _, id := range e.EvidenceMessageIDs {
		if id == 0 {
			return fmt.Errorf("session summary evidence id must be positive")
		}
		if _, exists := seen[id]; exists {
			return fmt.Errorf("duplicate session summary evidence id %d", id)
		}
		seen[id] = struct{}{}
	}
	return nil
}

func (d SessionSummaryDocument) EvidenceMessageIDs() []uint {
	seen := make(map[uint]struct{})
	for _, group := range []SessionSummaryEvidenceList{d.ConfirmedDecisions, d.OpenLoops, d.UserCorrections} {
		for _, item := range group {
			for _, id := range item.EvidenceMessageIDs {
				seen[id] = struct{}{}
			}
		}
	}
	ids := make([]uint, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// summaryEvidenceGroup is one of the three groups of a summary document that
// carry evidence items.
type summaryEvidenceGroup struct {
	name  string
	items *SessionSummaryEvidenceList
}

// Truncate trims the document to the budget instead of letting it break the
// limits.
//
// The rule is the newest wins: an item is as old as the message it rests on,
// and what survives is what a later message still backs. An item that cites
// nothing is the oldest there is, because nothing dates it.
//
// The order of the work is part of the rule. Every group is trimmed to its own
// limit first, then the document as a whole is trimmed to the number of
// message ids it may cite, and only then are the ids of a single item
// trimmed. Trimming the ids of an item before the groups would throw away
// citations of items that were about to be dropped anyway.
//
// A document that is already inside the budget comes back unchanged, with no
// truncation to report.
func (d SessionSummaryDocument) Truncate(budget SummaryBudget) (SessionSummaryDocument, []SummaryTruncation) {
	document := d
	evidenceGroups := []summaryEvidenceGroup{
		{SummaryGroupConfirmedDecisions, &document.ConfirmedDecisions},
		{SummaryGroupOpenLoops, &document.OpenLoops},
		{SummaryGroupUserCorrections, &document.UserCorrections},
	}
	stringGroups := []struct {
		name  string
		items *[]string
	}{
		{SummaryGroupCompletedItems, &document.CompletedItems},
		{SummaryGroupKeyErrors, &document.KeyErrors},
		{SummaryGroupReferences, &document.References},
	}

	droppedItems := make(map[string]int, len(evidenceGroups)+len(stringGroups))
	// The ids of the items that were dropped, and the ids that were cut out
	// of the items that stayed. They are different losses and they are
	// reported apart.
	droppedItemIDs := make(map[string][]uint, len(evidenceGroups))
	droppedCitationIDs := make(map[string][]uint, len(evidenceGroups))

	// Every group is trimmed to its own limit. The evidence groups keep the
	// items a later message still backs; the plain groups have no citations
	// to date them, so their order in the document is what they have: the
	// model is told to preserve what was already there and to add the new
	// items after it, so the tail of the list is the newest part of it.
	for _, group := range evidenceGroups {
		limit := budget.GroupLimit(group.name)
		if limit <= 0 || len(*group.items) <= limit {
			continue
		}
		kept, dropped := keepNewestEvidence(*group.items, limit)
		droppedItems[group.name] += len(dropped)
		droppedItemIDs[group.name] = append(droppedItemIDs[group.name], summaryEvidenceIDs(dropped)...)
		*group.items = kept
	}
	for _, group := range stringGroups {
		limit := budget.GroupLimit(group.name)
		if limit <= 0 || len(*group.items) <= limit {
			continue
		}
		kept := append([]string(nil), (*group.items)[len(*group.items)-limit:]...)
		droppedItems[group.name] += len(*group.items) - len(kept)
		*group.items = kept
	}

	// The document as a whole may not cite more messages than the budget
	// allows. The oldest items go first, and an item that holds the only
	// citation of a message goes before an item whose messages are cited
	// elsewhere, because dropping the second frees nothing.
	if budget.EvidenceIDLimit > 0 {
		for len(document.EvidenceMessageIDs()) > budget.EvidenceIDLimit {
			groupIndex, itemIndex, ok := oldestRemovableEvidence(evidenceGroups)
			if !ok {
				break
			}
			group := evidenceGroups[groupIndex]
			items := *group.items
			droppedItemIDs[group.name] = append(droppedItemIDs[group.name], items[itemIndex].EvidenceMessageIDs...)
			*group.items = append(items[:itemIndex:itemIndex], items[itemIndex+1:]...)
			droppedItems[group.name]++
		}
	}

	// The citations of a single item are trimmed last: an item that cites too
	// many messages keeps the newest ones, because those are the ones the
	// item was still about.
	for _, group := range evidenceGroups {
		if budget.ItemEvidenceLimit <= 0 {
			continue
		}
		for i := range *group.items {
			trimmed, cut := keepNewestEvidenceIDs((*group.items)[i], budget.ItemEvidenceLimit)
			(*group.items)[i] = trimmed
			droppedCitationIDs[group.name] = append(droppedCitationIDs[group.name], cut...)
		}
	}

	var truncations []SummaryTruncation
	for _, group := range evidenceGroups {
		if dropped := droppedItems[group.name]; dropped > 0 {
			truncations = append(truncations, SummaryTruncation{
				Kind: "items", Group: group.name, Kept: len(*group.items), Dropped: dropped,
				DroppedMessageIDs: distinctMessageIDs(droppedItemIDs[group.name]),
			})
		}
	}
	for _, group := range stringGroups {
		if dropped := droppedItems[group.name]; dropped > 0 {
			truncations = append(truncations, SummaryTruncation{
				Kind: "items", Group: group.name, Kept: len(*group.items), Dropped: dropped,
			})
		}
	}
	for _, group := range evidenceGroups {
		if len(droppedCitationIDs[group.name]) == 0 {
			continue
		}
		truncations = append(truncations, SummaryTruncation{
			Kind: "evidence_ids", Group: group.name, Kept: summaryGroupEvidenceIDs(*group.items),
			Dropped: len(droppedCitationIDs[group.name]), DroppedMessageIDs: distinctMessageIDs(droppedCitationIDs[group.name]),
		})
	}
	return document, truncations
}

// summaryGroupEvidenceIDs counts the distinct messages one group cites.
func summaryGroupEvidenceIDs(items SessionSummaryEvidenceList) int {
	seen := make(map[uint]struct{})
	for _, item := range items {
		for _, id := range item.EvidenceMessageIDs {
			seen[id] = struct{}{}
		}
	}
	return len(seen)
}

// keepNewestEvidence keeps the items a later message still backs, and says
// which ones it dropped.
//
// The recency of an item is the largest message id it cites: that message is
// the newest one the item rests on. An item that cites nothing has no date at
// all and is therefore the oldest there is. Items of the same recency keep
// the order the model put them in.
func keepNewestEvidence(items SessionSummaryEvidenceList, limit int) (SessionSummaryEvidenceList, SessionSummaryEvidenceList) {
	if limit <= 0 || len(items) <= limit {
		return items, nil
	}
	order := make([]int, len(items))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int {
		left, right := summaryEvidenceRecency(items[a]), summaryEvidenceRecency(items[b])
		switch {
		case left > right:
			return -1
		case left < right:
			return 1
		default:
			return 0
		}
	})
	keep := make([]bool, len(items))
	for _, index := range order[:limit] {
		keep[index] = true
	}
	kept := make(SessionSummaryEvidenceList, 0, limit)
	dropped := make(SessionSummaryEvidenceList, 0, len(items)-limit)
	for i, item := range items {
		if keep[i] {
			kept = append(kept, item)
			continue
		}
		dropped = append(dropped, item)
	}
	return kept, dropped
}

// keepNewestEvidenceIDs keeps the newest citations of one item and says which
// citations were cut. The citations that survive keep the order they had.
func keepNewestEvidenceIDs(item SessionSummaryEvidence, limit int) (SessionSummaryEvidence, []uint) {
	if limit <= 0 || len(item.EvidenceMessageIDs) <= limit {
		return item, nil
	}
	ids := append([]uint(nil), item.EvidenceMessageIDs...)
	slices.Sort(ids)
	keep := make(map[uint]struct{}, limit)
	for _, id := range ids[len(ids)-limit:] {
		keep[id] = struct{}{}
	}
	trimmed := make([]uint, 0, limit)
	var cut []uint
	for _, id := range item.EvidenceMessageIDs {
		if _, exists := keep[id]; exists {
			trimmed = append(trimmed, id)
			continue
		}
		cut = append(cut, id)
	}
	item.EvidenceMessageIDs = trimmed
	return item, cut
}

// summaryEvidenceIDs are the messages a list of items cites.
func summaryEvidenceIDs(items SessionSummaryEvidenceList) []uint {
	ids := make([]uint, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.EvidenceMessageIDs...)
	}
	return ids
}

// distinctMessageIDs are the messages of a list, once each and in order.
func distinctMessageIDs(ids []uint) []uint {
	seen := make(map[uint]struct{}, len(ids))
	distinct := make([]uint, 0, len(ids))
	for _, id := range ids {
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		distinct = append(distinct, id)
	}
	return distinct
}

// summaryEvidenceRecency is the newest message an item rests on.
func summaryEvidenceRecency(item SessionSummaryEvidence) uint {
	var newest uint
	for _, id := range item.EvidenceMessageIDs {
		if id > newest {
			newest = id
		}
	}
	return newest
}

// oldestRemovableEvidence picks the item to drop next so that the document
// cites fewer messages.
//
// An item that holds the only citation of some message is preferred: dropping
// it frees that message. An item whose messages are all cited elsewhere frees
// nothing, so it is only dropped when nothing better is left.
func oldestRemovableEvidence(groups []summaryEvidenceGroup) (groupIndex, itemIndex int, ok bool) {
	citations := make(map[uint]int)
	for _, group := range groups {
		for _, item := range *group.items {
			for _, id := range item.EvidenceMessageIDs {
				citations[id]++
			}
		}
	}
	bestRecency := uint(0)
	found := false
	for pass := 0; pass < 2; pass++ {
		// The first pass looks for an item that frees a message, the second
		// accepts any item that cites something.
		for gi, group := range groups {
			for ii, item := range *group.items {
				if len(item.EvidenceMessageIDs) == 0 {
					continue
				}
				frees := false
				for _, id := range item.EvidenceMessageIDs {
					if citations[id] == 1 {
						frees = true
						break
					}
				}
				if pass == 0 && !frees {
					continue
				}
				recency := summaryEvidenceRecency(item)
				if !found || recency < bestRecency {
					found, bestRecency, groupIndex, itemIndex = true, recency, gi, ii
				}
			}
		}
		if found {
			return groupIndex, itemIndex, true
		}
	}
	return 0, 0, false
}

// SessionSummary is a lossy, session-scoped compression of older messages.
// CoveredMessageID is the high-water mark: messages at or below it must not be
// summarized again or injected as recent history.
type SessionSummary struct {
	ID               uint64    `gorm:"primaryKey;autoIncrement" json:"id"`
	UserID           int64     `gorm:"not null;index:idx_session_summaries_user_session,unique,priority:1" json:"user_id"`
	SessionID        string    `gorm:"type:varchar(36);not null;index:idx_session_summaries_user_session,unique,priority:2;index" json:"session_id"`
	Content          string    `gorm:"type:longtext;not null" json:"content"`
	CoveredMessageID uint      `gorm:"not null;default:0;index" json:"covered_message_id"`
	TokenEstimate    int       `gorm:"not null;default:0" json:"token_estimate"`
	Version          uint64    `gorm:"not null;default:1" json:"version"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
	Session          Session   `gorm:"foreignKey:SessionID;references:ID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE" json:"-"`
}

func (s *SessionSummary) Validate() error {
	if s == nil {
		return fmt.Errorf("session summary is nil")
	}
	if s.UserID <= 0 || strings.TrimSpace(s.SessionID) == "" {
		return fmt.Errorf("session summary identity is required")
	}
	if strings.TrimSpace(s.Content) == "" || s.CoveredMessageID == 0 {
		return fmt.Errorf("session summary content and coverage are required")
	}
	if s.TokenEstimate < 0 {
		return fmt.Errorf("session summary token estimate cannot be negative")
	}
	return nil
}
