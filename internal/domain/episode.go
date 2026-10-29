package domain

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Episode memory is the cross-session projection of one chat session.
//
// It is migrated from the original project's episode system and keeps its
// founding decision: the raw Message rows stay authoritative, and an Episode is
// a derived, versioned card whose source watermark makes every version
// auditable. A stale projector can move nothing back, and a card can always be
// re-derived from what it claims to cover.
//
// An Episode is deliberately NOT a Memory. Memory rows here are user-confirmed
// facts with a confirmation gate (see memory.go); an Episode is written by the
// background projector without asking, is one per session, and is judged by
// whether its assertions cite real user messages. The two live in different
// tables and never enter each other's lifecycles.

// Limits carried over from the original implementation. They bound what a
// model may write into one card: without them a chatty model could park an
// unbounded transcript in MySQL and ship it into every later prompt.
const (
	EpisodeTitleMaxRunes        = 160
	EpisodeSynopsisMaxRunes     = 1200
	EpisodeEntryTextMaxRunes    = 400
	EpisodeSearchTextMaxRunes   = 12000
	EpisodeTopicLimit           = 12
	EpisodeEntryLimit           = 12
	EpisodeEvidenceLimit        = 32
	EpisodeSourceHandleLimit    = 64
	EpisodeSourceHandleMaxRunes = 200
	EpisodeSourceRefLimit       = 64
)

// EpisodeStatus is the lifecycle of one Episode row. The set is closed.
type EpisodeStatus string

const (
	EpisodeStatusActive   EpisodeStatus = "active"
	EpisodeStatusArchived EpisodeStatus = "archived"
	EpisodeStatusDeleted  EpisodeStatus = "deleted"
	// EpisodeStatusAbstained is a session the projector read and decided held
	// nothing worth remembering. It is a row rather than an absence on purpose:
	// "we looked and there was nothing" must be distinguishable from "the
	// projector never ran", "it ran and failed" and "the job is still queued".
	// Every recall filters on EpisodeStatusActive, so an abstained row is
	// invisible to retrieval while still answering why a session has no memory.
	EpisodeStatusAbstained EpisodeStatus = "abstained"
)

func (s EpisodeStatus) Valid() bool {
	switch s {
	case EpisodeStatusActive, EpisodeStatusArchived, EpisodeStatusDeleted,
		EpisodeStatusAbstained:
		return true
	default:
		return false
	}
}

// AbstainReason is the closed vocabulary for why a projection produced no
// memory. It is never free text from a model: the string is persisted and read
// by operators, and a model sentence here would carry a learner's own words
// into an operational field.
type AbstainReason string

const (
	// AbstainNoDurableAssertion is the ordinary case: the session was read and
	// held no decision, open loop, correction, preference or durable experience.
	AbstainNoDurableAssertion AbstainReason = "no_durable_assertion"
	// AbstainAllAssertionsTrimmed means the model proposed assertions and the
	// server removed every one. It is separate because the two say different
	// things about where to look: the first is about the session, the second is
	// about the generation.
	AbstainAllAssertionsTrimmed AbstainReason = "all_assertions_trimmed"
)

func (r AbstainReason) Valid() bool {
	switch r {
	case AbstainNoDurableAssertion, AbstainAllAssertionsTrimmed:
		return true
	default:
		return false
	}
}

// EpisodeIndexStatus records whether the derived retrieval view for one Episode
// version is in place. MySQL remains authoritative; this field only says
// whether the index caught up.
type EpisodeIndexStatus string

const (
	EpisodeIndexPending EpisodeIndexStatus = "pending"
	EpisodeIndexIndexed EpisodeIndexStatus = "indexed"
	EpisodeIndexFailed  EpisodeIndexStatus = "failed"
)

func (s EpisodeIndexStatus) Valid() bool {
	switch s {
	case EpisodeIndexPending, EpisodeIndexIndexed, EpisodeIndexFailed:
		return true
	default:
		return false
	}
}

// EpisodeProjectionStatus is the job state machine for the background worker.
type EpisodeProjectionStatus string

const (
	EpisodeProjectionPending    EpisodeProjectionStatus = "pending"
	EpisodeProjectionProcessing EpisodeProjectionStatus = "processing"
	EpisodeProjectionCompleted  EpisodeProjectionStatus = "completed"
	EpisodeProjectionFailed     EpisodeProjectionStatus = "failed"
)

func (s EpisodeProjectionStatus) Valid() bool {
	switch s {
	case EpisodeProjectionPending, EpisodeProjectionProcessing,
		EpisodeProjectionCompleted, EpisodeProjectionFailed:
		return true
	default:
		return false
	}
}

// EpisodeTopic is a stable topic handle plus its user-facing label. Topic keys
// are retrieval metadata produced by deterministic normalization, never a
// model-owned namespace.
type EpisodeTopic struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

// EpisodeEvidence records one session fact and the complete messages that
// support it. A projector may lose detail, but it must never claim a decision
// after seeing only part of a message.
type EpisodeEvidence struct {
	// ID is the server-issued stable identity of this assertion. It is
	// omitempty so a model that proposes a brand-new fact leaves it empty; the
	// server issues or reconciles identities before anything is persisted. The
	// model may echo an identity it was shown, and the server refuses any id it
	// did not actually issue.
	ID string `json:"id,omitempty"`
	// Text is the assertion sentence.
	Text string `json:"text"`
	// EvidenceMessageIDs are the ids of the session's USER messages the fact
	// rests on. They are the only things an assertion may cite; assistant turns
	// and retrieved documents appear in the synopsis, not as evidence.
	EvidenceMessageIDs []string `json:"evidence_message_ids"`
	// OccurredAt is when the assertion was made, derived by the server from the
	// messages it cites. The model never supplies a time.
	OccurredAt time.Time `json:"occurred_at,omitempty"`
}

// EpisodeDocument is the structured, cross-session projection of one chat
// session. It is deliberately narrower than a session summary: only durable
// history handles used by cross-session recall belong here.
type EpisodeDocument struct {
	Synopsis        string            `json:"synopsis"`
	Topics          []EpisodeTopic    `json:"topics"`
	Decisions       []EpisodeEvidence `json:"decisions"`
	OpenLoops       []EpisodeEvidence `json:"open_loops"`
	UserCorrections []EpisodeEvidence `json:"user_corrections"`
	// Preferences and DurableExperiences exist because the two clearest durable
	// statements had nowhere correct to go: "from now on explain code in Go" is
	// not a decision about a project, and "I understand that TCP has no message
	// boundaries" is not a correction. Both are omitempty so an older stored
	// document still decodes.
	Preferences        []EpisodeEvidence `json:"preferences,omitempty"`
	DurableExperiences []EpisodeEvidence `json:"durable_experiences,omitempty"`
	// SourceHandles are the provenance handles the projection used, each one
	// supplied by the server: message:<id>, summary:<session>:v<version>,
	// knowledge:<doc>:v<version>:<lines> or abstain:<session>:<id>.
	SourceHandles []string `json:"source_handles"`
}

// AssertionKind is which slot of an Episode document an assertion came from.
type AssertionKind string

const (
	// AssertionSynopsis is the card's summary. It is deliberately weak: a
	// synopsis paraphrases a session, so it can support "this was discussed"
	// but never "the learner decided".
	AssertionSynopsis AssertionKind = "synopsis"
	// AssertionDecision is something the learner settled.
	AssertionDecision AssertionKind = "decision"
	// AssertionOpenLoop is something the learner left outstanding.
	AssertionOpenLoop AssertionKind = "open_loop"
	// AssertionCorrection is a place the learner's understanding was corrected.
	AssertionCorrection AssertionKind = "correction"
	// AssertionPreference is a standing instruction about how the learner wants
	// to be worked with. It is not a decision about a project.
	AssertionPreference AssertionKind = "preference"
	// AssertionDurableExperience is a stable understanding the learner states
	// about themselves. Nothing was chosen and nothing was wrong before it.
	AssertionDurableExperience AssertionKind = "durable_experience"
)

// EpisodeAssertionGroup pairs one slot with its entries so generic code can
// walk every assertion without five literal lists drifting apart.
type EpisodeAssertionGroup struct {
	Kind    AssertionKind
	Entries *[]EpisodeEvidence
}

// AssertionGroups is every slot that carries a real assertion, in the order a
// reader sees them. The synopsis is not here: it asserts nothing.
func (d *EpisodeDocument) AssertionGroups() []EpisodeAssertionGroup {
	return []EpisodeAssertionGroup{
		{AssertionDecision, &d.Decisions},
		{AssertionOpenLoop, &d.OpenLoops},
		{AssertionCorrection, &d.UserCorrections},
		{AssertionPreference, &d.Preferences},
		{AssertionDurableExperience, &d.DurableExperiences},
	}
}

// AssertionCount is how many real assertions this document carries. It is the
// question "is there anything to remember here", asked before an Episode is
// persisted at all: a synopsis is not a reason to remember a session.
func (d EpisodeDocument) AssertionCount() int {
	total := 0
	for _, group := range d.AssertionGroups() {
		for _, entry := range *group.Entries {
			if strings.TrimSpace(entry.Text) != "" {
				total++
			}
		}
	}
	return total
}

func (d EpisodeDocument) Value() (driver.Value, error) {
	return json.Marshal(d)
}

func (d *EpisodeDocument) Scan(value any) error {
	if value == nil {
		*d = EpisodeDocument{}
		return nil
	}
	var data []byte
	switch typed := value.(type) {
	case []byte:
		data = typed
	case string:
		data = []byte(typed)
	default:
		return fmt.Errorf("scan episode document: unsupported type %T", value)
	}
	decoded, err := DecodeEpisodeDocument(data)
	if err != nil {
		return fmt.Errorf("scan episode document: %w", err)
	}
	*d = decoded
	return nil
}

// DecodeEpisodeDocument is the single strict decoder for stored and
// model-produced documents. Unknown fields and trailing JSON are rejected
// instead of being silently persisted as an expanded protocol.
func DecodeEpisodeDocument(data []byte) (EpisodeDocument, error) {
	var document EpisodeDocument
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return EpisodeDocument{}, fmt.Errorf("decode episode document: %w", err)
	}
	var extra any
	err := decoder.Decode(&extra)
	if err == io.EOF {
		if err := document.Validate(); err != nil {
			return EpisodeDocument{}, err
		}
		return document, nil
	}
	if err != nil {
		return EpisodeDocument{}, fmt.Errorf("decode trailing episode document: %w", err)
	}
	return EpisodeDocument{}, fmt.Errorf("episode document contains trailing JSON")
}

func (d *EpisodeDocument) Validate() error {
	if d == nil || strings.TrimSpace(d.Synopsis) == "" || utf8.RuneCountInString(d.Synopsis) > EpisodeSynopsisMaxRunes {
		return fmt.Errorf("invalid episode synopsis")
	}
	if len(d.Topics) > EpisodeTopicLimit {
		return fmt.Errorf("episode document exceeds entry limits")
	}
	for _, group := range d.AssertionGroups() {
		if len(*group.Entries) > EpisodeEntryLimit {
			return fmt.Errorf("episode document exceeds entry limits")
		}
	}
	seenTopics := make(map[string]struct{}, len(d.Topics))
	for _, topic := range d.Topics {
		key := strings.TrimSpace(topic.Key)
		label := strings.TrimSpace(topic.Label)
		if key == "" || label == "" || utf8.RuneCountInString(key) > EpisodeTitleMaxRunes || utf8.RuneCountInString(label) > EpisodeTitleMaxRunes {
			return fmt.Errorf("invalid episode topic")
		}
		if _, exists := seenTopics[key]; exists {
			return fmt.Errorf("duplicate episode topic %q", key)
		}
		seenTopics[key] = struct{}{}
	}
	for _, group := range d.AssertionGroups() {
		for _, entry := range *group.Entries {
			if err := validateEpisodeEvidence(entry); err != nil {
				return err
			}
		}
	}
	if len(d.SourceHandles) == 0 || len(d.SourceHandles) > EpisodeSourceHandleLimit {
		return fmt.Errorf("invalid episode source handle count")
	}
	seenHandles := make(map[string]struct{}, len(d.SourceHandles))
	for _, handle := range d.SourceHandles {
		handle = strings.TrimSpace(handle)
		if handle == "" || utf8.RuneCountInString(handle) > EpisodeSourceHandleMaxRunes {
			return fmt.Errorf("invalid episode source handle")
		}
		if _, exists := seenHandles[handle]; exists {
			return fmt.Errorf("duplicate episode source handle %q", handle)
		}
		seenHandles[handle] = struct{}{}
	}
	return nil
}

func validateEpisodeEvidence(entry EpisodeEvidence) error {
	if strings.TrimSpace(entry.Text) == "" || utf8.RuneCountInString(entry.Text) > EpisodeEntryTextMaxRunes ||
		len(entry.EvidenceMessageIDs) == 0 || len(entry.EvidenceMessageIDs) > EpisodeEvidenceLimit {
		return fmt.Errorf("invalid episode evidence")
	}
	seen := make(map[string]struct{}, len(entry.EvidenceMessageIDs))
	for _, id := range entry.EvidenceMessageIDs {
		if strings.TrimSpace(id) == "" {
			return fmt.Errorf("episode evidence message id must not be empty")
		}
		if _, exists := seen[id]; exists {
			return fmt.Errorf("duplicate episode evidence message id %q", id)
		}
		seen[id] = struct{}{}
	}
	return nil
}

// Episode is one updateable, cross-session projection per session. The
// uniqueness key is (UserID, SessionID): a re-projection overwrites the same
// row and moves Version forward, so there is exactly one current card per
// conversation and every older version is invalidated by the watermark.
type Episode struct {
	ID         string
	UserID     string
	SessionID  string
	Title      string
	Document   EpisodeDocument
	SearchText string
	TopicKeys  []string
	// SourceEpisodes is the versioned closure of other Episodes this card was
	// projected from. IDs without versions would be insufficient: an updated
	// source must invalidate answers derived from its older content.
	SourceEpisodes EpisodeSourceRefs
	// SourceSummaryVersion and SourceSummaryWatermark name the rolling summary
	// the projection read (this project's SessionSummary version and covered
	// message id). Both are set together or not at all.
	SourceSummaryVersion   int64
	SourceSummaryWatermark string
	// SourceLastMessageID is the projection watermark: the id of the last
	// message this card covers. Message ids in this project embed a UTC
	// nanosecond timestamp and compare lexicographically in creation order, so
	// "covered" is a string comparison.
	SourceLastMessageID string
	LastMessageAt       time.Time
	Version             uint64
	Status              EpisodeStatus
	// AbstainReason is set only on an abstained row and empty otherwise, so a
	// row cannot claim both to be memory and to be a reason there is none.
	AbstainReason AbstainReason
	IndexStatus   EpisodeIndexStatus
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

func (e *Episode) Validate() error {
	if e == nil || strings.TrimSpace(e.ID) == "" || strings.TrimSpace(e.UserID) == "" || strings.TrimSpace(e.SessionID) == "" {
		return fmt.Errorf("invalid episode identity")
	}
	if strings.TrimSpace(e.Title) == "" || utf8.RuneCountInString(e.Title) > EpisodeTitleMaxRunes ||
		strings.TrimSpace(e.SearchText) == "" || utf8.RuneCountInString(e.SearchText) > EpisodeSearchTextMaxRunes {
		return fmt.Errorf("invalid episode content")
	}
	if err := e.Document.Validate(); err != nil {
		return err
	}
	if len(e.TopicKeys) > EpisodeTopicLimit {
		return fmt.Errorf("episode exceeds topic key limit")
	}
	if err := e.SourceEpisodes.Validate(); err != nil {
		return err
	}
	for _, source := range e.SourceEpisodes {
		if source.EpisodeID == e.ID {
			return fmt.Errorf("episode cannot depend on itself")
		}
	}
	seenTopics := make(map[string]struct{}, len(e.TopicKeys))
	for _, topic := range e.TopicKeys {
		topic = strings.TrimSpace(topic)
		if topic == "" || utf8.RuneCountInString(topic) > EpisodeTitleMaxRunes {
			return fmt.Errorf("invalid episode topic key")
		}
		if _, exists := seenTopics[topic]; exists {
			return fmt.Errorf("duplicate episode topic key %q", topic)
		}
		seenTopics[topic] = struct{}{}
	}
	if len(seenTopics) != len(e.Document.Topics) {
		return fmt.Errorf("episode topic keys do not match document topics")
	}
	for _, topic := range e.Document.Topics {
		if _, exists := seenTopics[strings.TrimSpace(topic.Key)]; !exists {
			return fmt.Errorf("episode topic keys do not match document topics")
		}
	}
	if strings.TrimSpace(e.SourceLastMessageID) == "" || e.LastMessageAt.IsZero() || e.Version == 0 ||
		!e.Status.Valid() || !e.IndexStatus.Valid() {
		return fmt.Errorf("invalid episode state")
	}
	if (e.SourceSummaryVersion == 0) != (strings.TrimSpace(e.SourceSummaryWatermark) == "") {
		return fmt.Errorf("invalid episode summary source watermark")
	}
	if strings.TrimSpace(e.SourceSummaryWatermark) != "" && CompareMessageIDs(e.SourceSummaryWatermark, e.SourceLastMessageID) > 0 {
		return fmt.Errorf("invalid episode summary source watermark")
	}
	for _, group := range e.Document.AssertionGroups() {
		for _, entry := range *group.Entries {
			for _, messageID := range entry.EvidenceMessageIDs {
				if CompareMessageIDs(messageID, e.SourceLastMessageID) > 0 {
					return fmt.Errorf("episode evidence exceeds source message watermark")
				}
			}
		}
	}
	return nil
}

// CompareMessageIDs orders two stored message ids. Identifiers of this project
// embed a fixed-width UTC nanosecond timestamp before a process/counter suffix,
// so byte-wise comparison matches creation order; the helper exists so the
// watermark rule has one home instead of scattered `<` on strings.
func CompareMessageIDs(left, right string) int {
	return strings.Compare(strings.TrimSpace(left), strings.TrimSpace(right))
}

// ValidEpisodeErrorCode is the closed-vocabulary check for the codes a worker
// may record on a projection job. The code is persisted and read by operators;
// free text from a model or a provider does not belong there.
func ValidEpisodeErrorCode(code string) bool {
	if code == "" || len(code) > 64 {
		return false
	}
	first := code[0]
	if !(first >= 'a' && first <= 'z' || first >= '0' && first <= '9') {
		return false
	}
	for _, r := range code {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == ':', r == '-':
		default:
			return false
		}
	}
	return true
}

// EpisodeSourceRef identifies the exact version of an Episode that was used to
// produce derived conversation text.
type EpisodeSourceRef struct {
	EpisodeID string `json:"episode_id"`
	Version   uint64 `json:"version"`
}

type EpisodeSourceRefs []EpisodeSourceRef

func (refs EpisodeSourceRefs) Value() (driver.Value, error) {
	if refs == nil {
		refs = EpisodeSourceRefs{}
	}
	if err := refs.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(refs)
}

func (refs *EpisodeSourceRefs) Scan(value any) error {
	if value == nil {
		*refs = EpisodeSourceRefs{}
		return nil
	}
	var data []byte
	switch typed := value.(type) {
	case []byte:
		data = typed
	case string:
		data = []byte(typed)
	default:
		return fmt.Errorf("scan episode source refs: unsupported type %T", value)
	}
	if len(data) == 0 || string(data) == "null" {
		*refs = EpisodeSourceRefs{}
		return nil
	}
	if err := json.Unmarshal(data, refs); err != nil {
		return fmt.Errorf("scan episode source refs: %w", err)
	}
	if *refs == nil {
		*refs = EpisodeSourceRefs{}
	}
	return refs.Validate()
}

func (refs EpisodeSourceRefs) Validate() error {
	if len(refs) > EpisodeSourceRefLimit {
		return fmt.Errorf("episode source refs exceed %d", EpisodeSourceRefLimit)
	}
	seen := make(map[string]uint64, len(refs))
	for _, ref := range refs {
		id := strings.TrimSpace(ref.EpisodeID)
		if id == "" || ref.Version == 0 {
			return fmt.Errorf("invalid episode source ref")
		}
		if version, exists := seen[id]; exists {
			if version != ref.Version {
				return fmt.Errorf("episode source %q has conflicting versions", id)
			}
			return fmt.Errorf("duplicate episode source %q", id)
		}
		seen[id] = ref.Version
	}
	return nil
}

// MergeEpisodeSourceRefs returns a deterministic dependency closure. It never
// truncates: silently dropping an ancestor would make deletion fail open.
func MergeEpisodeSourceRefs(groups ...EpisodeSourceRefs) (EpisodeSourceRefs, error) {
	byID := make(map[string]uint64)
	for _, group := range groups {
		for _, ref := range group {
			id := strings.TrimSpace(ref.EpisodeID)
			if id == "" || ref.Version == 0 {
				return nil, fmt.Errorf("invalid episode source ref")
			}
			if version, exists := byID[id]; exists && version != ref.Version {
				return nil, fmt.Errorf("episode source %q has conflicting versions", id)
			}
			byID[id] = ref.Version
		}
	}
	if len(byID) > EpisodeSourceRefLimit {
		return nil, fmt.Errorf("episode source refs exceed %d", EpisodeSourceRefLimit)
	}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	refs := make(EpisodeSourceRefs, 0, len(ids))
	for _, id := range ids {
		refs = append(refs, EpisodeSourceRef{EpisodeID: id, Version: byID[id]})
	}
	return refs, nil
}

// EpisodeProjectionJob is a durable debounce and lease record. DesiredMessageID
// may advance while a worker owns ClaimedMessageID; completion then requeues
// the same row instead of losing the newly arrived tail.
type EpisodeProjectionJob struct {
	ID               string
	UserID           string
	SessionID        string
	DesiredMessageID string
	ClaimedMessageID string
	Status           EpisodeProjectionStatus
	Attempts         int
	AvailableAt      time.Time
	LeaseToken       string
	LeaseEndsAt      *time.Time
	ErrorCode        string
	CreatedAt        time.Time
	UpdatedAt        time.Time
	CompletedAt      *time.Time
}

func (j *EpisodeProjectionJob) ValidateForCreate() error {
	if j == nil || strings.TrimSpace(j.ID) == "" || strings.TrimSpace(j.UserID) == "" ||
		strings.TrimSpace(j.SessionID) == "" || strings.TrimSpace(j.DesiredMessageID) == "" || j.AvailableAt.IsZero() {
		return fmt.Errorf("invalid episode projection job identity")
	}
	if j.Status != EpisodeProjectionPending || j.Attempts != 0 || strings.TrimSpace(j.ClaimedMessageID) != "" ||
		j.LeaseToken != "" || j.LeaseEndsAt != nil || j.CompletedAt != nil || j.ErrorCode != "" {
		return fmt.Errorf("invalid initial episode projection job state")
	}
	return nil
}

// NormalizeEpisodeTopicKey deliberately performs only deterministic lexical
// normalization. Semantic alias merging belongs to a later, auditable layer.
func NormalizeEpisodeTopicKey(topic string) string {
	topic = strings.TrimSpace(strings.ToLower(topic))
	var builder strings.Builder
	separatorPending := false
	for _, r := range topic {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if separatorPending && builder.Len() > 0 {
				builder.WriteByte('-')
			}
			separatorPending = false
			builder.WriteRune(r)
			continue
		}
		separatorPending = builder.Len() > 0
	}
	value := strings.Trim(builder.String(), "-")
	runes := []rune(value)
	if len(runes) > 80 {
		value = strings.TrimRight(string(runes[:80]), "-")
	}
	return value
}

// Assertion is one slot of an Episode, resolved for reading. It is assembled at
// read time from the document; it is never stored in this shape.
type Assertion struct {
	ID         string
	EpisodeID  string
	UserID     string
	Kind       AssertionKind
	Ordinal    int
	Text       string
	OccurredAt time.Time
	// TimeKnown says whether OccurredAt was actually resolved. An assertion
	// whose evidence cannot be dated must not be used to establish that
	// something was decided on a particular day.
	TimeKnown bool
	// EvidenceMessageIDs are the learner messages this slot rests on. Messages
	// remain the fact source; the Episode is derived and stays traceable.
	EvidenceMessageIDs []string
	// EpisodeVersion is carried so a citation can be rejected when the card has
	// moved on since the reference was issued.
	EpisodeVersion uint64
}

// AssertionsOf resolves every slot of an Episode in a deterministic order:
// synopsis, decisions, open loops, corrections, each by ordinal. Two reads of
// the same Episode must produce the same order or a citation issued in one turn
// means something else in the next.
func AssertionsOf(episode Episode) []Assertion {
	out := make([]Assertion, 0, 1+episode.Document.AssertionCount())
	base := Assertion{EpisodeID: episode.ID, UserID: episode.UserID, EpisodeVersion: episode.Version}
	if text := strings.TrimSpace(episode.Document.Synopsis); text != "" {
		synopsis := base
		synopsis.ID = LegacyAssertionID(episode.ID, AssertionSynopsis, nil, text)
		synopsis.Kind, synopsis.Text = AssertionSynopsis, text
		synopsis.OccurredAt, synopsis.TimeKnown = episode.LastMessageAt.UTC(), !episode.LastMessageAt.IsZero()
		out = append(out, synopsis)
	}
	for _, group := range episode.Document.AssertionGroups() {
		for index, entry := range *group.Entries {
			if strings.TrimSpace(entry.Text) == "" {
				continue
			}
			item := base
			item.ID = strings.TrimSpace(entry.ID)
			if item.ID == "" {
				item.ID = LegacyAssertionID(episode.ID, group.Kind, entry.EvidenceMessageIDs, entry.Text)
			}
			item.Kind, item.Ordinal, item.Text = group.Kind, index+1, entry.Text
			item.EvidenceMessageIDs = append([]string(nil), entry.EvidenceMessageIDs...)
			if !entry.OccurredAt.IsZero() {
				item.OccurredAt, item.TimeKnown = entry.OccurredAt.UTC(), true
			}
			out = append(out, item)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		left, right := assertionRank(out[i].Kind), assertionRank(out[j].Kind)
		if left != right {
			return left < right
		}
		return out[i].Ordinal < out[j].Ordinal
	})
	return out
}

func assertionRank(kind AssertionKind) int {
	switch kind {
	case AssertionSynopsis:
		return 0
	case AssertionDecision:
		return 1
	case AssertionOpenLoop:
		return 2
	case AssertionCorrection:
		return 3
	default:
		return 4
	}
}

const (
	assertionIDPrefix         = "ea_"
	legacyAssertionIDPrefix   = "lg_"
	legacyAssertionHexLength  = 32
	issuedAssertionHexLength  = 24
	stableAssertionIDSeedSalt = "gopherlearn-episode-assertion|"
)

// NewAssertionID issues a fresh identity. It is generated on the server: the
// projector model proposes text and may echo back an identity it was given, but
// it can never mint one, because an id the model invented would be an unowned
// handle into somebody's memory.
func NewAssertionID() (string, error) {
	buffer := make([]byte, 12)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("issue assertion id: %w", err)
	}
	return assertionIDPrefix + hex.EncodeToString(buffer), nil
}

// DeterministicAssertionID mints a server-shaped identity from a seed. It
// exists for tests and fixture corpora, which must produce the same identities
// on every run. Production issues random identities.
func DeterministicAssertionID(seed string) string {
	sum := sha256.Sum256([]byte(stableAssertionIDSeedSalt + seed))
	return assertionIDPrefix + hex.EncodeToString(sum[:])[:issuedAssertionHexLength]
}

// ValidAssertionID reports whether a string is an identity this server issued
// or derived. It is the check applied to anything a model echoes back.
func ValidAssertionID(value string) bool {
	value = strings.TrimSpace(value)
	switch {
	case strings.HasPrefix(value, assertionIDPrefix):
		_, err := hex.DecodeString(strings.TrimPrefix(value, assertionIDPrefix))
		return err == nil && len(value) == len(assertionIDPrefix)+issuedAssertionHexLength
	case strings.HasPrefix(value, legacyAssertionIDPrefix):
		_, err := hex.DecodeString(strings.TrimPrefix(value, legacyAssertionIDPrefix))
		return err == nil && len(value) == len(legacyAssertionIDPrefix)+legacyAssertionHexLength
	}
	return false
}

// LegacyAssertionID derives a one-time identity for an assertion that has none.
// The fingerprint is kind + sorted evidence message ids + normalized text,
// which identifies the fact rather than its position. It is deterministic so
// two reads agree.
func LegacyAssertionID(episodeID string, kind AssertionKind, messageIDs []string, text string) string {
	sum := sha256.Sum256([]byte(assertionFingerprint(episodeID, kind, messageIDs, text)))
	return legacyAssertionIDPrefix + hex.EncodeToString(sum[:])[:legacyAssertionHexLength]
}

func assertionFingerprint(episodeID string, kind AssertionKind, messageIDs []string, text string) string {
	sorted := append([]string(nil), messageIDs...)
	sort.Strings(sorted)
	return strings.Join([]string{episodeID, string(kind), strings.Join(sorted, ","),
		NormalizeAssertionText(text)}, "|")
}

// NormalizeAssertionText collapses whitespace and case so a cosmetic edit does
// not read as a different fact.
func NormalizeAssertionText(text string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(text))), "")
}

// evidenceKey is kind plus the sorted message ids: the handle on the underlying
// fact, independent of wording and position.
func episodeEvidenceKey(kind AssertionKind, messageIDs []string) string {
	sorted := append([]string(nil), messageIDs...)
	sort.Strings(sorted)
	return string(kind) + "|" + strings.Join(sorted, ",")
}

// ReconcileDocumentIdentities assigns identities to a re-projected Episode,
// inheriting from the assertions that were there before. The preference order
// is how much the match is trusted: an id the projector echoed back, then the
// same evidence messages, then the same normalized text, then a new identity
// for a genuinely new fact. A pass only assigns a pair when it is unique in
// both directions; anything else would be a guess, and a guess silently
// re-points a recorded reference at a different sentence.
func ReconcileDocumentIdentities(prior []Assertion, episodeID string, document EpisodeDocument) (EpisodeDocument, error) {
	priorByKind := map[AssertionKind][]Assertion{}
	knownIDs := map[string]Assertion{}
	for _, item := range prior {
		if item.Kind == AssertionSynopsis {
			continue
		}
		knownIDs[item.ID] = item
		priorByKind[item.Kind] = append(priorByKind[item.Kind], item)
	}

	claimed := map[string]struct{}{}
	for _, group := range document.AssertionGroups() {
		reconciled, err := reconcileEpisodeSlot(episodeID, group.Kind, *group.Entries,
			priorByKind[group.Kind], knownIDs, claimed)
		if err != nil {
			return EpisodeDocument{}, err
		}
		*group.Entries = reconciled
	}
	return document, nil
}

func reconcileEpisodeSlot(episodeID string, kind AssertionKind, entries []EpisodeEvidence,
	priors []Assertion, knownIDs map[string]Assertion, claimed map[string]struct{}) ([]EpisodeEvidence, error) {
	assigned := make([]string, len(entries))

	for index, entry := range entries {
		echoed := strings.TrimSpace(entry.ID)
		if echoed == "" {
			continue
		}
		prior, known := knownIDs[echoed]
		if !known {
			return nil, fmt.Errorf("assertion id %q was never issued for episode %s", echoed, episodeID)
		}
		if prior.Kind != kind {
			return nil, fmt.Errorf("assertion id %q is a %s and was returned as a %s", echoed, prior.Kind, kind)
		}
		if _, taken := claimed[echoed]; taken {
			return nil, fmt.Errorf("assertion id %q was returned twice", echoed)
		}
		claimed[echoed] = struct{}{}
		assigned[index] = echoed
	}

	matches := []struct {
		entry func(EpisodeEvidence) string
		prior func(Assertion) string
	}{
		{
			entry: func(e EpisodeEvidence) string {
				return episodeEvidenceKey(kind, e.EvidenceMessageIDs) + "|" + NormalizeAssertionText(e.Text)
			},
			prior: func(a Assertion) string {
				return episodeEvidenceKey(kind, a.EvidenceMessageIDs) + "|" + NormalizeAssertionText(a.Text)
			},
		},
		{
			entry: func(e EpisodeEvidence) string { return episodeEvidenceKey(kind, e.EvidenceMessageIDs) },
			prior: func(a Assertion) string { return episodeEvidenceKey(kind, a.EvidenceMessageIDs) },
		},
		{
			entry: func(e EpisodeEvidence) string { return NormalizeAssertionText(e.Text) },
			prior: func(a Assertion) string { return NormalizeAssertionText(a.Text) },
		},
	}
	for _, keyOf := range matches {
		entryBuckets := map[string][]int{}
		for index, entry := range entries {
			if assigned[index] != "" {
				continue
			}
			key := keyOf.entry(entry)
			entryBuckets[key] = append(entryBuckets[key], index)
		}
		priorBuckets := map[string][]Assertion{}
		for _, candidate := range priors {
			if _, taken := claimed[candidate.ID]; taken {
				continue
			}
			priorBuckets[keyOf.prior(candidate)] = append(priorBuckets[keyOf.prior(candidate)], candidate)
		}
		for key, indexes := range entryBuckets {
			candidates := priorBuckets[key]
			if len(candidates) == 0 {
				continue
			}
			if len(indexes) != 1 || len(candidates) != 1 {
				return nil, fmt.Errorf("assertion reconciliation is ambiguous for %s %s: "+
					"%d re-projected assertions and %d prior assertions share one handle",
					episodeID, kind, len(indexes), len(candidates))
			}
			assigned[indexes[0]] = candidates[0].ID
			claimed[candidates[0].ID] = struct{}{}
		}
	}

	out := make([]EpisodeEvidence, 0, len(entries))
	for index, entry := range entries {
		if assigned[index] == "" {
			fresh, err := NewAssertionID()
			if err != nil {
				return nil, err
			}
			assigned[index] = fresh
		}
		entry.ID = assigned[index]
		out = append(out, entry)
	}
	return out, nil
}
