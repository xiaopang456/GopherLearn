package domain

import "time"

// User is the identity that owns sessions and their messages.
//
// Relations are represented by IDs rather than nested objects so the domain
// models stay independent of persistence and transport concerns.
type User struct {
	ID           string
	Email        string
	DisplayName  string
	Username     string
	PasswordHash string
	CreatedAt    time.Time
}

// SessionMode identifies the user-facing conversation mode of a session. The
// set is intentionally closed at the domain boundary; callers must use
// IsValid before creating a session. Knowledge grounding is not a mode: it is
// a per-turn Chat option resolved into an AnswerStrategy.
type SessionMode string

const (
	SessionModeChat          SessionMode = "chat"
	SessionModeLearningAgent SessionMode = "learning_agent"
)

func (m SessionMode) IsValid() bool {
	switch m {
	case SessionModeChat, SessionModeLearningAgent:
		return true
	default:
		return false
	}
}

// Session is a conversation owned by UserID. Mode is assigned at creation and
// has no update operation in the repository contract.
type Session struct {
	ID        string
	UserID    string
	Title     string
	Mode      SessionMode
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Message is an item appended to a Session in repository order.
type Message struct {
	ID        string
	SessionID string
	Role      string
	Content   string
	// Sources are the citations this answer was built from, in the order the
	// answer cites them. Only a grounded assistant message has any; a nil slice
	// is the ordinary case and must stay indistinguishable from "no citations"
	// on the wire. Persisting them with the message is what lets a refreshed
	// session show the same sources the turn returned.
	Sources   []Source
	CreatedAt time.Time
}
