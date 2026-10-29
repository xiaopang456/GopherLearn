package domain

import (
	"fmt"
	"strings"
)

// MemoryOrigin names who produced a memory, and it is the field that decides what the
// rest of the system may assume about it.
//
// It exists because two very different writes were sharing one path. A learner typing in
// a fact themselves is the confirmation: asking them to confirm again in a second request
// is theatre, and there is no conversation for the record to cite - the sentence IS the
// evidence. An agent's proposal is the opposite: it is a claim about what just happened,
// it must point at a stored message, and only the learner's decision can make it a fact.
// Those two cannot be served by one rule about provenance and one rule about status, so
// they are served by one discriminator and two write paths.
type MemoryOrigin string

const (
	// MemoryOriginUserAuthored is a memory that came from the user's own words. It is
	// stored active, and it has no source session or source messages by design.
	MemoryOriginUserAuthored MemoryOrigin = "user_authored"
	// MemoryOriginAgentProposed is a candidate an agent (or any caller acting on the
	// conversation) filed for the user to decide on. It is stored pending and it must
	// carry provenance.
	MemoryOriginAgentProposed MemoryOrigin = "agent_proposed"
)

// IsValid reports membership in the closed set.
func (o MemoryOrigin) IsValid() bool {
	switch o {
	case MemoryOriginUserAuthored, MemoryOriginAgentProposed:
		return true
	default:
		return false
	}
}

// AuthoredBy classifies a stored row.
//
// A row written before this column existed reads back with an empty origin, and guessing
// "user authored" for it would hand an unconfirmed record the status of a decision someone
// already made. Every such row was produced by the draft path - provenance plus a
// confirmation - so that is what it reports, without pretending the history was recorded.
func (m Memory) AuthoredBy() MemoryOrigin {
	if m.Origin == MemoryOriginUserAuthored {
		return MemoryOriginUserAuthored
	}
	return MemoryOriginAgentProposed
}

// UserAuthoredMemory is the whole of what a person can say when they add a memory
// themselves: which layer, which subject if it is a topic, and the sentence.
//
// There is no status, no confidence, no source session and no source message list here,
// and that absence is the design. A caller cannot submit a confirmation for a record that
// is already confirmed by being submitted, cannot lower a confidence nobody assessed, and
// cannot cite a conversation this statement did not come from. Those fields belong to the
// proposal path, where they are checked; here they would be a way to write a memory that
// looks like something it was not.
type UserAuthoredMemory struct {
	UserID   string
	Scope    MemoryScope
	TopicKey string
	Content  string
}

// Validate checks the author's input against the rules that still apply to a fact someone
// stated outright.
//
// The scope set is narrower than the storage set: an episode is by definition something
// that happened in a conversation, so "episodic" with no conversation to point at is not a
// memory but a claim about one, and this surface refuses to store it. The topic-key rule
// and the content bounds are shared with proposals, because a shorter limit for typed-in
// text than for agent text would be a rule nobody could explain.
func (d UserAuthoredMemory) Validate() error {
	if strings.TrimSpace(d.UserID) == "" {
		return fmt.Errorf("memory: user id is required")
	}
	switch d.Scope {
	case MemoryScopeGlobal, MemoryScopeTopic:
	case MemoryScopeEpisodic:
		return fmt.Errorf("memory: an episodic memory must come from a real conversation; it cannot be added by hand")
	default:
		return fmt.Errorf("memory: scope %q is not one of global, topic", string(d.Scope))
	}
	if err := validateMemoryTopicKey(d.Scope, d.TopicKey); err != nil {
		return err
	}
	content := strings.TrimSpace(d.Content)
	if content == "" {
		return fmt.Errorf("memory: content is required")
	}
	if len([]rune(content)) > MaxMemoryContentRunes {
		return fmt.Errorf("memory: content is longer than %d characters; store a document in the knowledge base instead", MaxMemoryContentRunes)
	}
	return nil
}

// StoredMemory returns the record an authored memory becomes: already active, already
// attributable to its author, with the confidence the server pins for a statement nobody
// had to assess, and no provenance to re-check.
//
// Status is set here and nowhere else for this path, which is what keeps "typed in means
// decided" from being re-derived - and therefore re-litigated - in two stores.
func (d UserAuthoredMemory) StoredMemory() Memory {
	return Memory{
		UserID:     strings.TrimSpace(d.UserID),
		Scope:      d.Scope,
		TopicKey:   strings.TrimSpace(d.TopicKey),
		Content:    strings.TrimSpace(d.Content),
		Status:     MemoryStatusActive,
		Origin:     MemoryOriginUserAuthored,
		Confidence: 1,
	}
}
