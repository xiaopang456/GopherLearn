package domain

import "time"

// SessionSummary is the rolling, session-scoped compression of the conversation
// that the recent-message window no longer carries. It exists so a long session
// keeps its earlier goals, decisions and open questions after those messages
// fall out of the window, without resending the whole transcript on every turn.
//
// It is scoped to one session and one user by construction: there is no
// cross-session or global summary here, and none is implied. Long-term memory -
// per-user, per-topic or episodic - is a separate piece of work.
type SessionSummary struct {
	SessionID string
	UserID    string

	// Content is the summary text as the model wrote it. It is derived context,
	// never a system instruction: whoever renders it into a prompt must label it
	// as such, and it must not be able to change tool permissions.
	Content string

	// CoveredMessageID is the id of the last stored message this summary has
	// absorbed. The boundary is an identifier rather than a timestamp because a
	// session's order is defined by append order, and two messages can share a
	// millisecond: the point where "already summarized" stops and "recent window"
	// begins has to be exact, and everything after this id is what a refresh is
	// allowed to look at.
	//
	// A refresh only ever ends on a complete user→assistant pair, so this is an
	// assistant message id in practice.
	CoveredMessageID string

	// Version increments once per committed summary. It is the compare-and-set
	// token: a writer that built its summary on version N can only commit if the
	// stored row is still at N, which keeps a slow refresh from overwriting a
	// newer one.
	Version int64

	UpdatedAt time.Time
}
