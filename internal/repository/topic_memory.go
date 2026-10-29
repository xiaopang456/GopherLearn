package repository

import (
	"context"
	"strings"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/domain"
)

// The topic-memory consent is stored beside the conversation rather than inside it, and read
// through two methods whose names contain no "update", "set" or "patch": a stored session is
// immutable in this project by design, and the row that says whether an agent may read topic
// memory is a fact about the conversation, not a fact of the conversation.
//
// Neither method looks at Session.Mode. Whether a chat conversation can hold this permission is
// a service rule - the same split the quiz workflow uses, where the store enforces ownership and
// LearningService.Start is the one that refuses a non-learning conversation. Keeping the mode out
// of storage is what lets the recall path check it independently, so a row that reached storage by
// some other route still grants nothing.

// GetTopicMemoryPreference reads one conversation's consent.
func (r *InMemoryRepository) GetTopicMemoryPreference(ctx context.Context, userID, sessionID string) (domain.TopicMemoryPreference, error) {
	if err := checkContext(ctx); err != nil {
		return domain.TopicMemoryPreference{}, err
	}
	userID = strings.TrimSpace(userID)
	sessionID = strings.TrimSpace(sessionID)
	if userID == "" || sessionID == "" {
		return domain.TopicMemoryPreference{}, ErrInvalidArgument
	}

	r.mu.RLock()
	defer r.mu.RUnlock()
	session, exists := r.sessions[sessionID]
	if !exists || session.UserID != userID {
		// Somebody else's conversation and one that never existed are the same answer here as
		// in GetSession, so a consent read cannot enumerate accounts.
		return domain.TopicMemoryPreference{}, ErrNotFound
	}
	stored, decided := r.topicMemory[sessionID]
	if !decided {
		// No row is "off", not "unknown". Returning a zero value with the identity filled in is
		// the shape that makes it impossible for a caller to guess permission from absence.
		return domain.TopicMemoryPreference{UserID: session.UserID, SessionID: session.ID}, nil
	}
	// The map holds values, so this is already a copy: a caller cannot rewrite stored consent
	// through the record it was handed.
	return stored, nil
}

// CommitTopicMemoryPreference stores the learner's decision as the new state of one
// conversation.
func (r *InMemoryRepository) CommitTopicMemoryPreference(ctx context.Context, userID, sessionID string,
	enabled bool) (*domain.TopicMemoryPreference, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	userID = strings.TrimSpace(userID)
	sessionID = strings.TrimSpace(sessionID)
	if userID == "" || sessionID == "" {
		return nil, ErrInvalidArgument
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.initLocked()
	session, exists := r.sessions[sessionID]
	if !exists || session.UserID != userID {
		return nil, ErrNotFound
	}

	stored, decided := r.topicMemory[sessionID]
	if decided && stored.Enabled == enabled {
		// Committing the answer that is already stored changes nothing, timestamps included:
		// the learner did not change their mind, and a record that says otherwise would be a
		// record that lies about when they did. This is what makes the switch idempotent
		// rather than merely repeatable.
		clone := stored
		return &clone, nil
	}
	now := time.Now().UTC()
	stored.UserID = session.UserID
	stored.SessionID = session.ID
	stored.Enabled = enabled
	if !decided {
		stored.CreatedAt = now
	}
	stored.UpdatedAt = now
	r.topicMemory[sessionID] = stored
	clone := stored
	return &clone, nil
}
