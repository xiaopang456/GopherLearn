package model

import (
	"bytes"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode/utf8"
)

// SessionCardContentVersion is the format version of a session card document.
//
// A card is a different shape from an Episode document, not a shorter one: an
// Episode carries five kinds of durable assertion with identities and
// relations between them, and a card carries what one session was about, what
// was arranged then, what the assistant produced, and what was left open. The
// version is on the document itself rather than on the row, so a card written
// by one release is readable by another that knows the shape.
const SessionCardContentVersion = 1

const (
	SessionCardTitleMaxRunes     = 160
	SessionCardSynopsisMaxRunes  = 1200
	SessionCardEntryTextMaxRunes = 400
	SessionCardEntryLimit        = 12
	SessionCardTopicLimit        = 12
)

// SessionCardEntryStatus says what happened to one item on a card.
//
// It is a closed vocabulary and it is not decoration. "The assistant proposed
// it", "the learner took it on", "the learner changed it afterwards" and "the
// learner simply stated it" are four different facts, and a card that kept
// only the text would turn a suggestion into a decision. The first release
// keeps them apart in the entry itself rather than in a rule that reads the
// text.
type SessionCardEntryStatus string

const (
	// SessionCardStatusStated is the learner saying something about their own
	// work. It is the default when a card entry carries no status at all.
	SessionCardStatusStated SessionCardEntryStatus = "stated"
	// SessionCardStatusAssistantProposed is something the assistant raised that
	// the learner did not take on. It is recorded because it is useful - it is
	// what the assistant offered - and it is kept apart from a decision.
	SessionCardStatusAssistantProposed SessionCardEntryStatus = "assistant_proposed"
	// SessionCardStatusAccepted is a proposal the learner took on. The entry
	// cites both the assistant's message and the learner's reply.
	SessionCardStatusAccepted SessionCardEntryStatus = "accepted"
	// SessionCardStatusLaterChanged is an item the learner changed after it was
	// settled. The card records that the later state exists; it does not claim
	// the earlier one still holds.
	SessionCardStatusLaterChanged SessionCardEntryStatus = "later_changed"
)

// SessionCardEntryStatusValues is the closed set, in the order a reader meets
// it. It is a function rather than a package variable so that a caller cannot
// mutate the vocabulary at runtime.
func SessionCardEntryStatusValues() []SessionCardEntryStatus {
	return []SessionCardEntryStatus{
		SessionCardStatusStated,
		SessionCardStatusAssistantProposed,
		SessionCardStatusAccepted,
		SessionCardStatusLaterChanged,
	}
}

// Valid reports whether the status is one the server knows.
//
// An empty status is valid and means "stated": the vocabulary grew a field
// after the first cards were written, and a card written before it existed
// must still read.
func (s SessionCardEntryStatus) Valid() bool {
	switch s {
	case "", SessionCardStatusStated, SessionCardStatusAssistantProposed,
		SessionCardStatusAccepted, SessionCardStatusLaterChanged:
		return true
	default:
		return false
	}
}

// Normalized returns the status with the empty default filled in.
func (s SessionCardEntryStatus) Normalized() SessionCardEntryStatus {
	if s == "" {
		return SessionCardStatusStated
	}
	return s
}

// SessionCardEntry is one line of a card.
//
// Text is what the card says; Status says whose it is; the ids say where it
// came from. A card without references is a claim with nothing behind it, so
// the program refuses one rather than storing it: a card exists to be
// followed back to the session that produced it.
type SessionCardEntry struct {
	Text   string                 `json:"text"`
	Status SessionCardEntryStatus `json:"status,omitempty"`
	// MessageIDs are the raw messages in the source session this entry came
	// from. They are the way back to the original words.
	MessageIDs []uint `json:"message_ids,omitempty"`
	// SegmentSeqs are the summary segments of the source session this entry
	// came from. They are the way back to the compressed form, which is
	// cheaper to read than the raw messages and usually enough.
	SegmentSeqs []int `json:"segment_seqs,omitempty"`
}

// SessionCardDocument is the content of one session card.
//
// The shape is deliberately lighter than an Episode document: a synopsis, a
// title, and three lists of entries. It carries no assertions, no identities
// and no relations, because a card answers a different question - "where was
// that, and what happened there" - and not "what is true about this learner".
type SessionCardDocument struct {
	Version  int    `json:"version"`
	Title    string `json:"title"`
	Synopsis string `json:"synopsis"`
	// Topics are the subjects the card is filed under.
	//
	// They are retrieval metadata rather than content: a card is found by the
	// words the learner uses, and a topic written in another language is a
	// second, unreachable key for a subject they already have.
	Topics []EpisodeTopic `json:"topics,omitempty"`
	// Arrangements is what the learner had settled at the time of that
	// session. It describes that session, not a rule every later session has
	// to follow.
	Arrangements []SessionCardEntry `json:"arrangements,omitempty"`
	// AssistantProducts is what the assistant offered: exercises, plans,
	// names, lists. It may include items the learner never took on.
	AssistantProducts []SessionCardEntry `json:"assistant_products,omitempty"`
	// OpenItems is what was still outstanding when the card was written.
	OpenItems []SessionCardEntry `json:"open_items,omitempty"`
}

// EntryGroups returns the three lists in the order a reader meets them.
func (d SessionCardDocument) EntryGroups() [][]SessionCardEntry {
	return [][]SessionCardEntry{d.Arrangements, d.AssistantProducts, d.OpenItems}
}

// EntryCount is how many entries this card carries.
func (d SessionCardDocument) EntryCount() int {
	total := 0
	for _, group := range d.EntryGroups() {
		total += len(group)
	}
	return total
}

// CitedMessageIDs is every raw message the card points at.
func (d SessionCardDocument) CitedMessageIDs() []uint {
	var out []uint
	for _, group := range d.EntryGroups() {
		for _, entry := range group {
			out = append(out, entry.MessageIDs...)
		}
	}
	return out
}

// CitedSegmentSeqs is every summary segment the card points at.
func (d SessionCardDocument) CitedSegmentSeqs() []int {
	var out []int
	for _, group := range d.EntryGroups() {
		for _, entry := range group {
			out = append(out, entry.SegmentSeqs...)
		}
	}
	return out
}

// CanonicalTopicKeys is the card's topic keys, sorted and deduplicated.
//
// It is the same recipe the Episode projector uses, so the row's topic column
// and the card's own topics cannot drift apart: the column is derived from the
// document, never written beside it.
func (d SessionCardDocument) CanonicalTopicKeys() StringList {
	keys := make(StringList, 0, len(d.Topics))
	seen := make(map[string]struct{}, len(d.Topics))
	for _, topic := range d.Topics {
		key := strings.TrimSpace(topic.Key)
		if key == "" {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// IsZero reports whether the card holds nothing at all.
//
// It is what decides whether a card row stores a card at all: a row of the
// other content kind leaves this column empty rather than storing an empty
// document, so a reader can tell "this row carries no card" from "this row
// carries a card with no entries".
func (d SessionCardDocument) IsZero() bool {
	return strings.TrimSpace(d.Title) == "" && strings.TrimSpace(d.Synopsis) == "" &&
		len(d.Topics) == 0 && d.EntryCount() == 0
}

// Value encodes the document for storage.
func (d SessionCardDocument) Value() (driver.Value, error) {
	if d.IsZero() {
		return nil, nil
	}
	return json.Marshal(d)
}

// Scan decodes a stored document.
func (d *SessionCardDocument) Scan(value any) error {
	if value == nil {
		*d = SessionCardDocument{}
		return nil
	}
	var data []byte
	switch typed := value.(type) {
	case []byte:
		data = typed
	case string:
		data = []byte(typed)
	default:
		return fmt.Errorf("scan session card document: unsupported type %T", value)
	}
	decoded, err := DecodeSessionCardDocument(data)
	if err != nil {
		return fmt.Errorf("scan session card document: %w", err)
	}
	*d = decoded
	return nil
}

// DecodeSessionCardDocument is the single strict decoder for card documents.
//
// It rejects unknown fields and trailing JSON for the same reason the Episode
// decoder does: a format that silently grows is a format whose readers cannot
// tell which version they are looking at.
func DecodeSessionCardDocument(data []byte) (SessionCardDocument, error) {
	var document SessionCardDocument
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return SessionCardDocument{}, fmt.Errorf("decode session card document: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return SessionCardDocument{}, fmt.Errorf("session card document contains trailing JSON")
		}
		return SessionCardDocument{}, fmt.Errorf("decode trailing session card document: %w", err)
	}
	if err := document.Validate(); err != nil {
		return SessionCardDocument{}, err
	}
	return document, nil
}

// DecodeSessionCardDocumentUnchecked decodes a stored document without the
// content checks.
//
// The generator needs it because one bad entry should not cost the whole
// card: an entry whose status is not one the server knows, or whose citation
// does not exist, is dropped by the generator, and the rest of the card is
// still worth keeping. Decoding with the checks on would refuse the document
// before the generator ever saw it.
//
// The field check stays on. An unknown field is a protocol violation, not a
// bad entry, and a document that grew a field must not be read as if it had
// not.
func DecodeSessionCardDocumentUnchecked(data []byte) (SessionCardDocument, error) {
	var document SessionCardDocument
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return SessionCardDocument{}, fmt.Errorf("decode session card document: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return SessionCardDocument{}, fmt.Errorf("session card document contains trailing JSON")
		}
		return SessionCardDocument{}, fmt.Errorf("decode trailing session card document: %w", err)
	}
	return document, nil
}

// Validate checks the stored document.
func (d *SessionCardDocument) Validate() error {
	if d == nil {
		return fmt.Errorf("session card document is nil")
	}
	if d.Version != SessionCardContentVersion {
		return fmt.Errorf("unsupported session card version %d", d.Version)
	}
	if strings.TrimSpace(d.Title) == "" || utf8.RuneCountInString(d.Title) > SessionCardTitleMaxRunes {
		return fmt.Errorf("invalid session card title")
	}
	if strings.TrimSpace(d.Synopsis) == "" || utf8.RuneCountInString(d.Synopsis) > SessionCardSynopsisMaxRunes {
		return fmt.Errorf("invalid session card synopsis")
	}
	if len(d.Topics) > SessionCardTopicLimit {
		return fmt.Errorf("session card exceeds topic limit")
	}
	seenTopics := make(map[string]struct{}, len(d.Topics))
	for _, topic := range d.Topics {
		key := strings.TrimSpace(topic.Key)
		label := strings.TrimSpace(topic.Label)
		if key == "" || label == "" || utf8.RuneCountInString(key) > EpisodeTitleMaxRunes || utf8.RuneCountInString(label) > EpisodeTitleMaxRunes {
			return fmt.Errorf("invalid session card topic")
		}
		if _, exists := seenTopics[key]; exists {
			return fmt.Errorf("duplicate session card topic %q", key)
		}
		seenTopics[key] = struct{}{}
	}
	for _, group := range d.EntryGroups() {
		if len(group) > SessionCardEntryLimit {
			return fmt.Errorf("session card exceeds entry limits")
		}
		for _, entry := range group {
			if err := validateSessionCardEntry(entry); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateSessionCardEntry(entry SessionCardEntry) error {
	if strings.TrimSpace(entry.Text) == "" || utf8.RuneCountInString(entry.Text) > SessionCardEntryTextMaxRunes {
		return fmt.Errorf("invalid session card entry text")
	}
	if !entry.Status.Valid() {
		return fmt.Errorf("invalid session card entry status %q", entry.Status)
	}
	if len(entry.MessageIDs) == 0 && len(entry.SegmentSeqs) == 0 {
		return fmt.Errorf("session card entry must cite a source")
	}
	seenMessages := make(map[uint]struct{}, len(entry.MessageIDs))
	for _, id := range entry.MessageIDs {
		if id == 0 {
			return fmt.Errorf("session card entry message id must be positive")
		}
		if _, exists := seenMessages[id]; exists {
			return fmt.Errorf("duplicate session card entry message id %d", id)
		}
		seenMessages[id] = struct{}{}
	}
	seenSegments := make(map[int]struct{}, len(entry.SegmentSeqs))
	for _, seq := range entry.SegmentSeqs {
		if seq <= 0 {
			return fmt.Errorf("session card entry segment sequence must be positive")
		}
		if _, exists := seenSegments[seq]; exists {
			return fmt.Errorf("duplicate session card entry segment sequence %d", seq)
		}
		seenSegments[seq] = struct{}{}
	}
	return nil
}

// EpisodeContentKind says which content format a stored row carries.
//
// The row is one table and one document column per format, not a union: the
// Episode document is decoded strictly, so a card written into its column
// would make the row unreadable for the code that reads Episodes. The
// discriminator is therefore explicit and every reader filters on it.
type EpisodeContentKind string

const (
	// EpisodeContentEpisode is the Episode document: five kinds of durable
	// assertion with identities and relations.
	EpisodeContentEpisode EpisodeContentKind = "episode"
	// EpisodeContentSessionCard is the session card: what one session was
	// about, what was arranged then, what the assistant produced, what was
	// left open.
	EpisodeContentSessionCard EpisodeContentKind = "session_card"
)

// Valid reports whether the kind is one the server knows.
func (k EpisodeContentKind) Valid() bool {
	switch k {
	case EpisodeContentEpisode, EpisodeContentSessionCard:
		return true
	default:
		return false
	}
}

// Normalized returns the kind with the empty default filled in.
//
// A row written before the discriminator existed is an Episode row, and the
// database default says the same. Reading an empty kind as an Episode keeps
// every existing document readable without a migration.
func (k EpisodeContentKind) Normalized() EpisodeContentKind {
	if k == "" {
		return EpisodeContentEpisode
	}
	return k
}
