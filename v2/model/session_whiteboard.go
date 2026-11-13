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

// The whiteboard is the current effective state of one session: what the learner
// is doing now, which constraints are in force, which plan and numbers are
// current, and what is still open.
//
// It is rewritten rather than extended. A value that changed replaces the old
// one under the same key, so the board never accumulates history it has already
// superseded - the failure mode that made a rolling summary grow until it could
// not be written at all.
//
// The board is sent to the model on every turn, so its size is a fixed cost
// rather than a growing one. Every limit here exists to keep that cost bounded.
const (
	// SessionWhiteboardSchemaV1 is the version of the whiteboard document
	// that could only hold what the learner said.
	//
	// It is the version the mode that does not record what the assistant
	// produced writes, so a board written in that mode is the one it always
	// wrote. It stays readable: every entry in a version 1 board is the
	// learner's, and a board written before the origin field existed says so
	// by not having one.
	SessionWhiteboardSchemaV1 = 1

	// SessionWhiteboardSchemaV2 is the version of the whiteboard document
	// that says where each entry came from.
	//
	// The mode that records what the assistant produced wrote this one
	// before the unconfirmed statements existed, and it is still read: a
	// version 2 entry with no origin is the learner's, exactly as a version 1
	// entry is.
	SessionWhiteboardSchemaV2 = 2

	// SessionWhiteboardSchemaV3 is the version of the whiteboard document
	// that can also carry the learner's own words behind a refused change.
	//
	// It is a different format, not an extension of version 2: a decoder
	// that was written before this version refuses a document that carries
	// the statements, because the field is one it does not know. That is
	// why the version was raised rather than the field added to version 2 -
	// a document that no old decoder can read must say so in its version,
	// and a deployment that has to roll back has to be able to see that it
	// cannot.
	//
	// Everything version 2 could hold is still held: the origins, the
	// claims and the rules about them are unchanged.
	SessionWhiteboardSchemaV3 = 3

	// SessionWhiteboardItemLimit is how many entries one board category may hold.
	// A category that reaches the limit has its oldest entries merged away, and
	// the removal is recorded as a change so the superseded value stays
	// answerable from the summary segments.
	SessionWhiteboardItemLimit = 20

	// SessionWhiteboardTextMaxRunes bounds one entry's text. A board entry is a
	// sentence, not a paragraph.
	SessionWhiteboardTextMaxRunes = 200

	// SessionWhiteboardEvidenceLimit is how many source message ids one entry may
	// cite. An entry citing more than this is describing a stretch of history
	// rather than a current fact.
	SessionWhiteboardEvidenceLimit = 8

	// SessionWhiteboardTokenLimit bounds the whole board. The board travels with
	// every turn, so its ceiling is what keeps the per-turn cost flat.
	SessionWhiteboardTokenLimit = 1200
)

// Where a board entry came from.
const (
	// SessionWhiteboardOriginUser is what the learner said.
	//
	// It is the default: an entry with no origin is the learner's, and so is
	// every entry of a board written before the field existed.
	SessionWhiteboardOriginUser = "user"
	// SessionWhiteboardOriginAssistantAccepted is something the assistant
	// proposed and the learner explicitly took on.
	//
	// It is the only other origin that may reach the board, and only with the
	// evidence that says so: the assistant's message and the learner's
	// message after it.
	SessionWhiteboardOriginAssistantAccepted = "assistant_accepted"
	// SessionWhiteboardOriginAssistant is something the assistant produced
	// that the learner did not take a position on.
	//
	// It never reaches the board: a proposal nobody agreed to is not a
	// current fact. The program reads it as "this belongs in the summary
	// segment as one of the assistant's products", which is where it goes.
	SessionWhiteboardOriginAssistant = "assistant"
)

// ValidSessionWhiteboardOrigin reports whether an origin is one the program
// knows.
func ValidSessionWhiteboardOrigin(origin string) bool {
	switch origin {
	case SessionWhiteboardOriginUser, SessionWhiteboardOriginAssistant,
		SessionWhiteboardOriginAssistantAccepted:
		return true
	default:
		return false
	}
}

// SessionWhiteboardEntry is one current fact on the board.
//
// Key is what makes overwriting possible: two entries about the same thing carry
// the same key, and the newer one replaces the older. An entry without a key
// cannot be overwritten and can only be added.
type SessionWhiteboardEntry struct {
	// Key identifies what the entry is about, so a changed value replaces the old
	// one instead of joining it. It is optional; an entry without one is
	// additive only.
	Key string `json:"key,omitempty"`
	// Text is the human-readable statement of the entry.
	Text string `json:"text"`
	// Value is the literal value of the entry when it has one, such as a
	// duration or a name. It is what a later change is compared against.
	Value string `json:"value,omitempty"`
	// State is the entry's lifecycle marker. Only plan entries use it.
	State string `json:"state,omitempty"`
	// SourceMessageIDs are the learner's own messages this entry came from.
	// They are what authorises reading the original wording back.
	SourceMessageIDs []uint `json:"source_message_ids,omitempty"`
	// Origin says whether the entry is the learner's or an assistant
	// proposal the learner took on.
	//
	// It is empty in a board written before the field existed, and it means
	// the learner there. An entry that says the learner accepted an
	// assistant proposal has to cite the proposal and the message that
	// accepted it, and the board engine checks that before the entry is
	// stored.
	//
	// It is written only when it says something other than the learner, so
	// a board written by the mode that does not record the assistant's
	// products is the document it always was, byte for byte.
	Origin string `json:"origin,omitempty"`
	// UpdatedAtMessageID is the last message that changed this entry.
	UpdatedAtMessageID uint `json:"updated_at_message_id,omitempty"`
}

// EntryOrigin is the entry's origin, with the empty origin read as the
// learner's.
//
// The empty origin is the learner's rather than an error because a board
// written before the field existed carries no origin at all, and reading it
// as the learner's is what keeps those boards usable.
func (e SessionWhiteboardEntry) EntryOrigin() string {
	origin := strings.TrimSpace(e.Origin)
	if origin == "" {
		return SessionWhiteboardOriginUser
	}
	return origin
}

// The reasons a learner's own words could not be confirmed as a current fact.
//
// They are a fixed set, because they end up in a metric label and in a stored
// board: a reason that could carry the learner's own words would carry the
// learner's own words into a place that is not the learner's own session.
const (
	// SessionWhiteboardAnchorStanceUnknown says the claim did not report what
	// kind of statement the learner's words were.
	SessionWhiteboardAnchorStanceUnknown = "stance_unknown"
	// SessionWhiteboardAnchorUpdateUnknown says the claim did not report
	// whether those words set the value on the item.
	SessionWhiteboardAnchorUpdateUnknown = "update_unknown"
	// SessionWhiteboardAnchorMessageNotLearner says the words the claim rests
	// on are not the learner's own words of this batch.
	SessionWhiteboardAnchorMessageNotLearner = "message_not_learner"
	// SessionWhiteboardAnchorQuoteMissing says the claim carried no quote.
	SessionWhiteboardAnchorQuoteMissing = "quote_missing"
	// SessionWhiteboardAnchorQuoteNotInMessage says the quote is not in the
	// message it was taken from.
	SessionWhiteboardAnchorQuoteNotInMessage = "quote_not_in_message"
	// SessionWhiteboardAnchorValueNotInQuote says the value is not in the
	// quoted words.
	SessionWhiteboardAnchorValueNotInQuote = "value_not_in_quote"
	// SessionWhiteboardAnchorValueUnitMismatch says the value is in the
	// quoted words but measures something else.
	SessionWhiteboardAnchorValueUnitMismatch = "value_unit_mismatch"
	// SessionWhiteboardAnchorValueMismatch says the claim's value is not the
	// value of the entry it was written for.
	SessionWhiteboardAnchorValueMismatch = "value_mismatch"
	// SessionWhiteboardAnchorSubjectNotInQuote says the subject is not what
	// the learner spoke about.
	SessionWhiteboardAnchorSubjectNotInQuote = "subject_not_in_quote"
	// SessionWhiteboardAnchorSubjectNotInEntry says the entry does not name
	// the subject the claim is about.
	SessionWhiteboardAnchorSubjectNotInEntry = "subject_not_in_entry"
	// SessionWhiteboardAnchorAttributeNotInQuote says the attribute is not
	// what the learner spoke about.
	SessionWhiteboardAnchorAttributeNotInQuote = "attribute_not_in_quote"
	// SessionWhiteboardAnchorAttributeNotInEntry says the entry does not
	// name the attribute the claim is about.
	SessionWhiteboardAnchorAttributeNotInEntry = "attribute_not_in_entry"
	// SessionWhiteboardAnchorScopeConflict says the learner's own words left
	// another value of the same quantity standing.
	SessionWhiteboardAnchorScopeConflict = "scope_conflict"
	// SessionWhiteboardAnchorUnsettled says the learner's words do not settle
	// the value: they proposed it, asked about it, ruled it out or were
	// unsure about it.
	SessionWhiteboardAnchorUnsettled = "unsettled"
	// SessionWhiteboardAnchorUnbackedChange says the entry replaced a value
	// the board already held and nothing the learner said was behind it.
	SessionWhiteboardAnchorUnbackedChange = "unbacked_change"
	// SessionWhiteboardAnchorKeyAmbiguous says the claim could not be placed
	// under the entry it was written for.
	SessionWhiteboardAnchorKeyAmbiguous = "key_ambiguous"
)

// ValidSessionWhiteboardAnchorReason reports whether a reason is one the
// program knows.
func ValidSessionWhiteboardAnchorReason(reason string) bool {
	switch reason {
	case SessionWhiteboardAnchorStanceUnknown,
		SessionWhiteboardAnchorUpdateUnknown,
		SessionWhiteboardAnchorMessageNotLearner,
		SessionWhiteboardAnchorQuoteMissing,
		SessionWhiteboardAnchorQuoteNotInMessage,
		SessionWhiteboardAnchorValueNotInQuote,
		SessionWhiteboardAnchorValueUnitMismatch,
		SessionWhiteboardAnchorValueMismatch,
		SessionWhiteboardAnchorSubjectNotInQuote,
		SessionWhiteboardAnchorSubjectNotInEntry,
		SessionWhiteboardAnchorAttributeNotInQuote,
		SessionWhiteboardAnchorAttributeNotInEntry,
		SessionWhiteboardAnchorScopeConflict,
		SessionWhiteboardAnchorUnsettled,
		SessionWhiteboardAnchorUnbackedChange,
		SessionWhiteboardAnchorKeyAmbiguous:
		return true
	}
	return false
}

// SessionWhiteboardAnchor is a learner's own words that could not be
// confirmed as a current fact.
//
// It is the half of a refused entry that is still true: the learner said
// something, the program can show which words, and a reader can ask for the
// message they came from. What it does not say is that the value in it is the
// current one - that is exactly what could not be confirmed - so it is kept
// apart from the board's entries and rendered apart from them.
//
// Without it, a claim the program could not confirm took the learner's words
// with it: the entry was refused, the message was cited by nothing, and the
// words were no longer findable. The reader was left with a board that said
// the old value and no sign that the learner had said something else.
type SessionWhiteboardAnchor struct {
	// Key is the name the proposal gave the entry the words were about.
	//
	// It is the model's name for the item, so it is a hint and not a fact:
	// the entry it was refused from may have been about something else. It
	// is kept because it is what says the board now holds what these words
	// said - and nothing else is used to place the words on an item.
	Key string `json:"key,omitempty"`
	// Value is the value the refused entry wanted to put on the board.
	//
	// It is the candidate, not the current value: the words carry it, but
	// the program could not confirm that it is the value of that item. It
	// is empty when the learner did not put a value up at all.
	Value string `json:"value,omitempty"`
	// MessageID is the learner's message the words come from.
	//
	// It is what authorises reading the original wording back.
	MessageID uint `json:"message_id"`
	// Text is the learner's own message.
	//
	// It is the whole message or nothing: a message that was cut in half
	// would hide the end of the sentence, and the end of the sentence is
	// where a condition or a negation lives. A message that is too long to
	// be kept whole is kept as a message id alone, and a reader is told to
	// read it back rather than shown half of it.
	Text string `json:"text,omitempty"`
	// Complete says whether Text is the whole message.
	//
	// It is false for a message that is kept as a pointer only.
	Complete bool `json:"complete,omitempty"`
	// Reason says why the words could not be confirmed.
	//
	// It is one of the fixed reasons above, so it is never the learner's
	// own text.
	Reason string `json:"reason"`
	// UpdatedAtMessageID is the newest message of the batch the words came
	// from.
	UpdatedAtMessageID uint `json:"updated_at_message_id,omitempty"`
}

// SessionWhiteboardAnchorLimit is how many unconfirmed learner statements one
// board may carry.
//
// The board travels with every turn, so its size is a fixed cost. The limit
// keeps that cost flat, and the newest statements are the ones kept: a
// statement from many turns ago has had every chance to be confirmed since.
const SessionWhiteboardAnchorLimit = 8

// SessionWhiteboardAnchorMessageMaxRunes bounds the learner's message one
// anchor keeps whole.
//
// A message that is longer than this is kept as a message id alone, and a
// reader is told to read it back. Cutting a message in half would hide the
// end of the sentence - and the end of the sentence is where the condition
// or the negation is - so a long message is either whole or not shown.
const SessionWhiteboardAnchorMessageMaxRunes = 600

// SessionWhiteboardGoal is the learner's current goal. It is a single entry
// rather than a list because a session has one goal at a time; keeping older
// goals would be history, and history belongs to the summary segments.
type SessionWhiteboardGoal struct {
	Text               string `json:"text"`
	SourceMessageIDs   []uint `json:"source_message_ids,omitempty"`
	UpdatedAtMessageID uint   `json:"updated_at_message_id,omitempty"`
}

// ErrWhiteboardNotFound means the session has no board yet.
//
// It lives here rather than in the repository because the reader that has to
// tell "no board yet" from "a board I may not read" is not the only one that
// needs to know: the writer compares a version it read against the version it
// is about to write, and it has to recognise the same condition.
var ErrWhiteboardNotFound = errors.New("session whiteboard not found")

// ErrWhiteboardConflict means another writer moved the board on.
//
// It is not a failure: the caller re-reads and decides. The work that was
// done is not lost, only the write was refused.
var ErrWhiteboardConflict = errors.New("session whiteboard version conflict")

// SessionWhiteboardDocument is the stored board.
type SessionWhiteboardDocument struct {
	SchemaVersion int                      `json:"schema_version"`
	Goal          *SessionWhiteboardGoal   `json:"goal,omitempty"`
	Constraints   []SessionWhiteboardEntry `json:"constraints,omitempty"`
	Plan          []SessionWhiteboardEntry `json:"plan,omitempty"`
	OpenItems     []SessionWhiteboardEntry `json:"open_items,omitempty"`
	// Unverified are the learner's own words that could not be confirmed as
	// current facts.
	//
	// They are not entries and they are not the current state: they are the
	// words themselves, kept so that a reader can find them and ask for the
	// message they came from. An entry that was refused took the learner's
	// words with it, and this is where they went instead.
	//
	// Only a version 3 document carries them. A version 1 or version 2
	// document that carries the field is refused, because those versions
	// are the formats that were written before it existed.
	Unverified []SessionWhiteboardAnchor `json:"unverified,omitempty"`
	// MigratedFromVersion is the version of the old rolling summary this
	// board was built from, and it is zero for a board the new path wrote.
	//
	// The old summary is rewritten as the session goes on, so the version
	// is what says which revision of it this board came from.
	MigratedFromVersion uint64 `json:"migrated_from_version,omitempty"`
}

// DecodeSessionWhiteboardDocument parses a stored board.
//
// Unknown fields are refused rather than ignored: a board that grows a field
// without this decoder knowing about it is a board whose meaning has changed
// underneath the code that reads it.
func DecodeSessionWhiteboardDocument(content string) (SessionWhiteboardDocument, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return SessionWhiteboardDocument{}, fmt.Errorf("session whiteboard is empty")
	}
	if !strings.HasPrefix(content, "{") {
		return SessionWhiteboardDocument{}, fmt.Errorf("session whiteboard must be a JSON object")
	}
	decoder := json.NewDecoder(strings.NewReader(content))
	decoder.DisallowUnknownFields()
	var document SessionWhiteboardDocument
	if err := decoder.Decode(&document); err != nil {
		return SessionWhiteboardDocument{}, fmt.Errorf("decode session whiteboard: %w", err)
	}
	if err := ensureWhiteboardJSONEOF(decoder); err != nil {
		return SessionWhiteboardDocument{}, err
	}
	if document.SchemaVersion != SessionWhiteboardSchemaV1 &&
		document.SchemaVersion != SessionWhiteboardSchemaV2 &&
		document.SchemaVersion != SessionWhiteboardSchemaV3 {
		return SessionWhiteboardDocument{}, fmt.Errorf("unsupported session whiteboard schema version %d", document.SchemaVersion)
	}
	if err := document.Validate(); err != nil {
		return SessionWhiteboardDocument{}, err
	}
	return document, nil
}

func ensureWhiteboardJSONEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("decode trailing session whiteboard: %w", err)
	}
	return fmt.Errorf("session whiteboard contains trailing JSON")
}

// Validate checks the board's shape and bounds.
//
// The checks here are the ones a reader can rely on: a board that passed them
// has a bounded size, and every entry it carries can be traced to messages.
func (d *SessionWhiteboardDocument) Validate() error {
	if d == nil {
		return fmt.Errorf("session whiteboard is nil")
	}
	switch d.SchemaVersion {
	case SessionWhiteboardSchemaV1, SessionWhiteboardSchemaV2, SessionWhiteboardSchemaV3:
	default:
		return fmt.Errorf("session whiteboard schema version must be %d, %d or %d",
			SessionWhiteboardSchemaV1, SessionWhiteboardSchemaV2, SessionWhiteboardSchemaV3)
	}
	if d.Goal != nil {
		if err := validateWhiteboardGoal(*d.Goal); err != nil {
			return err
		}
	}
	for _, group := range [][]SessionWhiteboardEntry{d.Constraints, d.Plan, d.OpenItems} {
		if len(group) > SessionWhiteboardItemLimit {
			return fmt.Errorf("session whiteboard has too many entries")
		}
		for _, entry := range group {
			if err := entry.Validate(); err != nil {
				return err
			}
			if d.SchemaVersion == SessionWhiteboardSchemaV1 &&
				entry.EntryOrigin() != SessionWhiteboardOriginUser {
				// A version 1 board could only hold what the learner said,
				// so an entry that names an assistant proposal is a body this
				// schema cannot describe.
				return fmt.Errorf("session whiteboard version %d cannot carry an assistant proposal",
					SessionWhiteboardSchemaV1)
			}
		}
	}
	if len(d.Unverified) > SessionWhiteboardAnchorLimit {
		return fmt.Errorf("session whiteboard has too many unconfirmed statements")
	}
	if len(d.Unverified) > 0 && d.SchemaVersion != SessionWhiteboardSchemaV3 {
		// The statements are a version 3 field. A version 1 or version 2
		// document that carries them is a document in a format that did not
		// exist when it was written, and it is refused rather than read
		// with part of itself dropped.
		return fmt.Errorf("session whiteboard version %d cannot carry an unconfirmed statement",
			d.SchemaVersion)
	}
	for _, anchor := range d.Unverified {
		if err := anchor.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// Validate checks one unconfirmed learner statement.
func (a SessionWhiteboardAnchor) Validate() error {
	if a.MessageID == 0 {
		return fmt.Errorf("session whiteboard unconfirmed statement has no message")
	}
	if a.Complete && strings.TrimSpace(a.Text) == "" {
		return fmt.Errorf("session whiteboard unconfirmed statement is marked complete but has no text")
	}
	if !a.Complete && strings.TrimSpace(a.Text) != "" {
		// A statement that is not complete carries no text: half a message
		// is worse than none, because a reader cannot tell that the end of
		// the sentence is missing.
		return fmt.Errorf("session whiteboard unconfirmed statement is not complete but has text")
	}
	if len([]rune(a.Text)) > SessionWhiteboardAnchorMessageMaxRunes {
		return fmt.Errorf("session whiteboard unconfirmed statement text is too long")
	}
	if len([]rune(a.Key)) > SessionWhiteboardTextMaxRunes {
		return fmt.Errorf("session whiteboard unconfirmed statement key is too long")
	}
	if len([]rune(a.Value)) > SessionWhiteboardTextMaxRunes {
		return fmt.Errorf("session whiteboard unconfirmed statement value is too long")
	}
	if !ValidSessionWhiteboardAnchorReason(a.Reason) {
		return fmt.Errorf("session whiteboard unconfirmed statement has an unknown reason %q", a.Reason)
	}
	return nil
}

func validateWhiteboardGoal(goal SessionWhiteboardGoal) error {
	if len([]rune(strings.TrimSpace(goal.Text))) > SessionWhiteboardTextMaxRunes {
		return fmt.Errorf("session whiteboard goal is too long")
	}
	return validateWhiteboardEvidence(goal.SourceMessageIDs)
}

// Validate checks one entry.
func (e SessionWhiteboardEntry) Validate() error {
	if strings.TrimSpace(e.Text) == "" {
		return fmt.Errorf("session whiteboard entry has no text")
	}
	if len([]rune(strings.TrimSpace(e.Text))) > SessionWhiteboardTextMaxRunes {
		return fmt.Errorf("session whiteboard entry is too long")
	}
	if len([]rune(strings.TrimSpace(e.Key))) > SessionWhiteboardTextMaxRunes {
		return fmt.Errorf("session whiteboard entry key is too long")
	}
	if len([]rune(strings.TrimSpace(e.Value))) > SessionWhiteboardTextMaxRunes {
		return fmt.Errorf("session whiteboard entry value is too long")
	}
	if !ValidSessionWhiteboardOrigin(e.EntryOrigin()) {
		return fmt.Errorf("session whiteboard entry has an unknown origin %q", e.Origin)
	}
	switch e.State {
	case "", "open", "done":
	default:
		return fmt.Errorf("session whiteboard entry has an unknown state %q", e.State)
	}
	return validateWhiteboardEvidence(e.SourceMessageIDs)
}

func validateWhiteboardEvidence(ids []uint) error {
	if len(ids) > SessionWhiteboardEvidenceLimit {
		return fmt.Errorf("session whiteboard entry cites too many messages")
	}
	seen := make(map[uint]struct{}, len(ids))
	for _, id := range ids {
		if id == 0 {
			return fmt.Errorf("session whiteboard evidence id must be positive")
		}
		if _, exists := seen[id]; exists {
			return fmt.Errorf("duplicate session whiteboard evidence id %d", id)
		}
		seen[id] = struct{}{}
	}
	return nil
}

// EvidenceMessageIDs returns every message id the board cites, sorted and
// deduplicated. It is what authorises reading the learner's own words back.
//
// The unconfirmed statements are cited too: they are the learner's own words,
// and a reader that may not ask for them may not find them either. A
// statement that was confirmed is no longer one of them, so its message is
// cited by the entry that took its place.
func (d SessionWhiteboardDocument) EvidenceMessageIDs() []uint {
	seen := make(map[uint]struct{})
	if d.Goal != nil {
		for _, id := range d.Goal.SourceMessageIDs {
			seen[id] = struct{}{}
		}
	}
	for _, group := range [][]SessionWhiteboardEntry{d.Constraints, d.Plan, d.OpenItems} {
		for _, entry := range group {
			for _, id := range entry.SourceMessageIDs {
				seen[id] = struct{}{}
			}
		}
	}
	for _, anchor := range d.Unverified {
		seen[anchor.MessageID] = struct{}{}
	}
	ids := make([]uint, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// SessionWhiteboard is the stored board of one session.
//
// It is one row per session, replaced in place. Version is the compare-and-set
// token: a writer that built a board on version N can only commit if the stored
// row is still at N, so a slow generation cannot overwrite a newer one.
type SessionWhiteboard struct {
	ID               uint64    `gorm:"primaryKey;autoIncrement" json:"id"`
	UserID           int64     `gorm:"not null;uniqueIndex:idx_session_whiteboards_user_session,priority:1" json:"user_id"`
	SessionID        string    `gorm:"type:varchar(36);not null;uniqueIndex:idx_session_whiteboards_user_session,priority:2" json:"session_id"`
	Content          string    `gorm:"type:longtext;not null" json:"content"`
	CoveredMessageID uint      `gorm:"not null;default:0;index" json:"covered_message_id"`
	TokenEstimate    int       `gorm:"not null;default:0" json:"token_estimate"`
	Version          uint64    `gorm:"not null;default:1" json:"version"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
	Session          Session   `gorm:"foreignKey:SessionID;references:ID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE" json:"-"`
}

// Validate checks the stored row.
func (w *SessionWhiteboard) Validate() error {
	if w == nil {
		return fmt.Errorf("session whiteboard is nil")
	}
	if w.UserID <= 0 || strings.TrimSpace(w.SessionID) == "" {
		return fmt.Errorf("session whiteboard identity is required")
	}
	if strings.TrimSpace(w.Content) == "" {
		return fmt.Errorf("session whiteboard content is required")
	}
	if w.TokenEstimate < 0 {
		return fmt.Errorf("session whiteboard token estimate cannot be negative")
	}
	return nil
}

// MarshalSessionWhiteboardDocument encodes a board for storage.
//
// The encoding is canonical so that two boards with the same content produce the
// same bytes, which is what lets a writer tell "nothing changed" from "changed
// and changed back".
func MarshalSessionWhiteboardDocument(document SessionWhiteboardDocument) (string, error) {
	if err := document.Validate(); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return "", fmt.Errorf("encode session whiteboard: %w", err)
	}
	return string(encoded), nil
}

// jsonEquals reports whether two documents encode to the same bytes.
func jsonEquals(left, right any) bool {
	leftBytes, leftErr := json.Marshal(left)
	rightBytes, rightErr := json.Marshal(right)
	if leftErr != nil || rightErr != nil {
		return false
	}
	return bytes.Equal(leftBytes, rightBytes)
}
