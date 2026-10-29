package domain

import "time"

// TopicMemoryPreference is one conversation's answer to "may the learning agent read my topic
// memory while we work on this subject".
//
// It is its own record rather than a column on Session because a stored conversation is
// deliberately immutable here: the repository exposes no update entry point for a session, and
// a guard test reads the method names of every implementation to keep it that way. A consent
// the learner can turn on and off mid-conversation is mutable state *about* a conversation,
// which is the same shape as the rolling summary - its own row, its own commit verb, and
// removed with the conversation it belongs to.
//
// What it stores is the permission and nothing else. There is deliberately no topic key, no
// scope and no owner wider than these two ids, because which subject the permission unlocks is
// decided at recall time from the quiz the server has open on this conversation (see
// service.LearningTopicGrant). A record that could name a subject would be a record a client
// could aim.
type TopicMemoryPreference struct {
	UserID    string
	SessionID string
	// Enabled is the learner's decision as stored. Absence of a row means the same thing as
	// false, so a reader never has to tell "never asked" from "asked to stop" - and can
	// never guess "on" from a row that is not there.
	Enabled bool
	// CreatedAt is when the learner first decided, and UpdatedAt when that decision last
	// changed. Together they are what the record owes its owner: "when did you start reading
	// my topic memory, and when did I tell you to stop" has to be answerable from the row.
	CreatedAt time.Time
	UpdatedAt time.Time
}
