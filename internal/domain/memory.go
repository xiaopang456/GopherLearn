package domain

import (
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
)

// Long-term memory is a separate concern from the conversation transcript: a session's
// rolling summary (SessionSummary) is rebuilt from messages and dies with the session,
// while a memory is a fact the user has confirmed and that outlives every session.
//
// Phase 5B.1 stores memories and the confirmation gate around them, Phase 5B.2 reads them
// back for one agent tool under a server-owned grant, and Phase 5B.3 publishes the HTTP
// surface a user confirms through. What still does not exist is anything proposing a draft
// on its own: a memory in this database is not standing context, and neither Chat nor
// knowledge-grounded Chat reads this table at all.

// MemoryScope is the closed set of long-term memory layers.
type MemoryScope string

const (
	// MemoryScopeGlobal holds stable user-level facts and preferences: "I work in Go",
	// "answer in Chinese". Exactly one active version per user is meaningful, so
	// confirming a new global memory supersedes the previous one.
	MemoryScopeGlobal MemoryScope = "global"
	// MemoryScopeTopic holds what the user understands about one topic - mastery and
	// weak points - keyed by TopicKey. Also one active version per key.
	MemoryScopeTopic MemoryScope = "topic"
	// MemoryScopeEpisodic records experienced episodes: a decision, how a question was
	// worked through, what happened. Episodes accumulate instead of replacing each
	// other, so confirming one never supersedes another.
	MemoryScopeEpisodic MemoryScope = "episodic"
)

// IsValid reports whether scope is one of the three known layers.
func (s MemoryScope) IsValid() bool {
	switch s {
	case MemoryScopeGlobal, MemoryScopeTopic, MemoryScopeEpisodic:
		return true
	default:
		return false
	}
}

// SupersedesSiblings reports whether confirming a memory in this scope retires the
// previously active one for the same topic. Global and topic memory are versioned
// statements about a subject - two active versions would be two answers to one question -
// while an episode is an event, and events do not overwrite each other.
func (s MemoryScope) SupersedesSiblings() bool {
	return s == MemoryScopeGlobal || s == MemoryScopeTopic
}

// MemoryStatus is where one memory stands in the confirmation lifecycle.
type MemoryStatus string

const (
	// MemoryStatusPending is an unconfirmed candidate. It is the only status a draft can
	// be created in, and it is invisible to every recall read: an unconfirmed guess is
	// not a fact about the user.
	MemoryStatusPending MemoryStatus = "pending"
	// MemoryStatusActive is a memory the user has confirmed. Only active memories are
	// recallable.
	MemoryStatusActive MemoryStatus = "active"
	// MemoryStatusSuperseded is an active memory that a newer confirmation in the same
	// scope and topic replaced. It is kept - not deleted - so the version history stays
	// auditable.
	MemoryStatusSuperseded MemoryStatus = "superseded"
	// MemoryStatusArchived is the terminal "not to be used" state, reached either by
	// rejecting a candidate or by archiving a memory that has stopped applying.
	MemoryStatusArchived MemoryStatus = "archived"
)

// IsValid reports whether status is one of the four lifecycle states.
func (s MemoryStatus) IsValid() bool {
	switch s {
	case MemoryStatusPending, MemoryStatusActive, MemoryStatusSuperseded, MemoryStatusArchived:
		return true
	default:
		return false
	}
}

// CanTransitionTo is the single state machine both stores enforce, so an in-memory row and
// a MySQL row cannot disagree about what "confirm" means. Terminal states are terminal:
// reviving one would need a new draft, not an edit of the old record.
func (s MemoryStatus) CanTransitionTo(next MemoryStatus) bool {
	if !s.IsValid() || !next.IsValid() {
		return false
	}
	switch s {
	case MemoryStatusPending:
		return next == MemoryStatusActive || next == MemoryStatusArchived
	case MemoryStatusActive:
		return next == MemoryStatusSuperseded || next == MemoryStatusArchived
	default:
		return false
	}
}

// Size and shape limits. A memory is meant to be a sentence or two with provenance; a
// caller that wants to store a document should upload it to the knowledge base instead.
const (
	MaxMemoryContentRunes   = 4000
	MaxMemoryTopicKeyRunes  = 96
	MaxMemorySourceMessages = 20
)

// Memory is one stored long-term memory, in whichever lifecycle state it currently is.
type Memory struct {
	MemoryID string
	UserID   string
	Scope    MemoryScope
	// TopicKey names the subject a topic memory is about. It is empty exactly when the
	// scope does not use one, and it is the third component of the "one active version"
	// key: (user, scope, topic_key).
	TopicKey string
	Content  string
	Status   MemoryStatus
	// Origin names who produced this row, and it decides which other fields are allowed
	// to be empty: see MemoryOrigin and domain.UserAuthoredMemory. It is history, not a
	// state - nothing transitions a row from one origin to the other.
	Origin MemoryOrigin
	// Confidence is the proposer's self-assessment, 0 to 1. It is a read rule and never a
	// storage rule: no numeric threshold promotes a draft - the user does - but the recall
	// floor (service.DefaultMemoryMinConfidence, 0.6) hides a confirmed memory below it from
	// a prompt. A surface that lets someone set it low is therefore storing a fact the agent
	// will not use, which is why the HTTP create call fixes it instead.
	Confidence float64
	// SourceSessionID and SourceMessageIDs are the provenance: where this memory came
	// from, in the order it was cited. Confirmation checks that the session belongs to
	// the user and that every listed message still exists in it, so an active memory
	// always points at real conversation - which is also what makes a wrong memory
	// correctable by looking at what it claimed to be built from.
	SourceSessionID  string
	SourceMessageIDs []string
	CreatedAt        time.Time
	UpdatedAt        time.Time
	// SupersededAt is the zero time until something replaces this memory.
	SupersededAt time.Time
}

// MemoryDraft is a candidate that has not been confirmed. It carries no status field on
// purpose: the type is how "a draft is only ever pending" is expressed, so no caller can
// construct a draft that arrives already active, already recalled, or already a fact.
type MemoryDraft struct {
	UserID           string
	Scope            MemoryScope
	TopicKey         string
	Content          string
	Confidence       float64
	SourceSessionID  string
	SourceMessageIDs []string
}

// StoredMemory returns the pending record a draft becomes once stored. Status is set here
// and nowhere else, so "drafts are only pending" survives a refactor of either store.
func (d MemoryDraft) StoredMemory() Memory {
	return Memory{
		UserID:           strings.TrimSpace(d.UserID),
		Scope:            d.Scope,
		TopicKey:         strings.TrimSpace(d.TopicKey),
		Content:          strings.TrimSpace(d.Content),
		Status:           MemoryStatusPending,
		Origin:           MemoryOriginAgentProposed,
		Confidence:       d.Confidence,
		SourceSessionID:  strings.TrimSpace(d.SourceSessionID),
		SourceMessageIDs: cloneMessageIDs(d.SourceMessageIDs),
	}
}

// ProposalsMatch reports whether two memories are the same proposal: same owner, same
// subject, same words, same evidence. The definition lives next to the fields that make it
// up rather than inside each store, so an idempotent write and a duplicate check cannot
// disagree about what "already proposed" means.
//
// Status is deliberately not part of it: the caller compares against pending rows only.
// Message ids are compared in order, because citation order is part of the record.
func (m Memory) ProposalsMatch(other Memory) bool {
	return m.UserID == other.UserID &&
		m.Scope == other.Scope &&
		m.TopicKey == other.TopicKey &&
		m.Content == other.Content &&
		m.SourceSessionID == other.SourceSessionID &&
		slices.Equal(m.SourceMessageIDs, other.SourceMessageIDs)
}

// Validate checks the shape of a candidate. It is the rule the repository applies before
// writing, so a stored row can always be interpreted by the recall and confirmation code
// that reads it later.
func (d MemoryDraft) Validate() error {
	if strings.TrimSpace(d.UserID) == "" {
		return fmt.Errorf("memory draft: user id is required")
	}
	if !d.Scope.IsValid() {
		return fmt.Errorf("memory draft: scope %q is not one of global, topic, episodic", string(d.Scope))
	}
	if err := validateMemoryTopicKey(d.Scope, d.TopicKey); err != nil {
		return err
	}
	return validateMemoryBody(strings.TrimSpace(d.Content), d.Confidence, d.SourceSessionID, d.SourceMessageIDs)
}

// Validate checks a stored memory against the same rules, so a row read back from a
// database that someone else wrote cannot silently become an unusable record.
func (m Memory) Validate() error {
	if strings.TrimSpace(m.MemoryID) == "" {
		return fmt.Errorf("memory: id is required")
	}
	if strings.TrimSpace(m.UserID) == "" {
		return fmt.Errorf("memory: user id is required")
	}
	if !m.Scope.IsValid() {
		return fmt.Errorf("memory: scope %q is not one of global, topic, episodic", string(m.Scope))
	}
	if !m.Status.IsValid() {
		return fmt.Errorf("memory %s: status %q is unknown", m.MemoryID, string(m.Status))
	}
	if err := validateMemoryTopicKey(m.Scope, m.TopicKey); err != nil {
		return err
	}
	if m.AuthoredBy() == MemoryOriginUserAuthored {
		// An episode is something that happened inside a conversation, so a row claiming one
		// while saying nobody was looking at a conversation is a contradiction rather than a
		// memory. The authored door refuses the scope on input; this refuses it on the way
		// back out, for a row some other writer produced.
		if m.Scope == MemoryScopeEpisodic {
			return fmt.Errorf("memory %s: an episodic memory cannot be user-authored", m.MemoryID)
		}
		// A hand-written fact is judged on what it says. Requiring it to cite a
		// conversation would make every record the authored path stores invalid - and
		// an authored row that did grow a source is claiming evidence nobody checked,
		// so that contradiction is refused here too.
		if strings.TrimSpace(m.SourceSessionID) != "" || len(m.SourceMessageIDs) > 0 {
			return fmt.Errorf("memory %s: a user-authored memory cannot carry source conversation", m.MemoryID)
		}
		if strings.TrimSpace(m.Content) == "" {
			return fmt.Errorf("memory: content is required")
		}
		if len([]rune(strings.TrimSpace(m.Content))) > MaxMemoryContentRunes {
			return fmt.Errorf("memory: content is longer than %d characters; store a document in the knowledge base instead", MaxMemoryContentRunes)
		}
		if m.Confidence < 0 || m.Confidence > 1 {
			return fmt.Errorf("memory: confidence %v is outside 0..1", m.Confidence)
		}
		return nil
	}
	return validateMemoryBody(strings.TrimSpace(m.Content), m.Confidence, m.SourceSessionID, m.SourceMessageIDs)
}

// Clone returns a detached copy, so a caller that sorts or truncates the provenance list
// it was handed cannot rewrite what is stored - the same rule the message citations
// follow.
func (m Memory) Clone() Memory {
	m.SourceMessageIDs = cloneMessageIDs(m.SourceMessageIDs)
	return m
}

// Recallable reports whether this memory may be presented as a fact about the user. Only
// a confirmed, not-yet-replaced memory qualifies; a pending draft is explicitly not, which
// is why the draft type cannot even express the other statuses.
func (m Memory) Recallable() bool {
	return m.Status == MemoryStatusActive
}

func validateMemoryTopicKey(scope MemoryScope, key string) error {
	key = strings.TrimSpace(key)
	switch scope {
	case MemoryScopeGlobal:
		// A global fact with a topic key would stop being global: the "one active
		// version" key is (user, scope, topic_key), so a stray key quietly splits the
		// namespace into two, each with its own surviving answer.
		if key != "" {
			return fmt.Errorf("memory: a global memory must not carry a topic key, got %q", key)
		}
	case MemoryScopeTopic:
		if key == "" {
			return fmt.Errorf("memory: a topic memory needs a topic key")
		}
		if err := validateTopicKeyShape(key); err != nil {
			return err
		}
	case MemoryScopeEpisodic:
		// An episode may name a topic and may also stand alone; what pins it down is its
		// session and message provenance, not a key.
		if err := validateTopicKeyShape(key); err != nil {
			return err
		}
	}
	return nil
}

func validateTopicKeyShape(key string) error {
	if key == "" {
		return nil
	}
	if len([]rune(key)) > MaxMemoryTopicKeyRunes {
		return fmt.Errorf("memory: topic key is longer than %d characters", MaxMemoryTopicKeyRunes)
	}
	// Control characters and stray newlines would make the key unwritable as a label and
	// unmatched across stores, so the shape is fixed here rather than at every reader.
	if strings.IndexFunc(key, unicode.IsControl) >= 0 {
		return fmt.Errorf("memory: topic key must not contain control characters")
	}
	return nil
}

func validateMemoryBody(content string, confidence float64, sessionID string, messageIDs []string) error {
	if content == "" {
		return fmt.Errorf("memory: content is required")
	}
	if len([]rune(content)) > MaxMemoryContentRunes {
		return fmt.Errorf("memory: content is longer than %d characters; store a document in the knowledge base instead", MaxMemoryContentRunes)
	}
	if confidence < 0 || confidence > 1 {
		return fmt.Errorf("memory: confidence %v is outside 0..1", confidence)
	}
	if strings.TrimSpace(sessionID) == "" {
		return fmt.Errorf("memory: a source session is required; a fact with no provenance cannot be checked later")
	}
	if len(messageIDs) == 0 {
		return fmt.Errorf("memory: at least one source message is required")
	}
	if len(messageIDs) > MaxMemorySourceMessages {
		return fmt.Errorf("memory: %d source messages exceed the limit of %d", len(messageIDs), MaxMemorySourceMessages)
	}
	seen := make(map[string]struct{}, len(messageIDs))
	for _, id := range messageIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			return fmt.Errorf("memory: source message ids must not be blank")
		}
		if _, exists := seen[id]; exists {
			return fmt.Errorf("memory: source message %q is listed twice", id)
		}
		seen[id] = struct{}{}
	}
	return nil
}

func cloneMessageIDs(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	cloned := make([]string, 0, len(ids))
	for _, id := range ids {
		cloned = append(cloned, strings.TrimSpace(id))
	}
	return cloned
}
