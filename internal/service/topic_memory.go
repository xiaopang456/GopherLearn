package service

import (
	"context"
	"errors"
	"strings"

	"github.com/xiaopang456/GopherLearn/internal/domain"
)

// ErrTopicMemoryNeedsLearningAgent reports the switch being turned on for a conversation where
// topic memory has no meaning. It is a service refusal, not a storage one: the store keeps a
// consent for whichever conversation is named and owns only the rules that must not differ
// between engines, and the quiz workflow refuses a chat conversation at exactly this layer for
// exactly this reason. A chat conversation is where ordinary questions and knowledge-grounded
// answers happen, and neither of them is studying a subject the learner could have consented to.
var ErrTopicMemoryNeedsLearningAgent = errors.New("memory: 主题记忆只能挂在学习 Agent 会话上")

// CommitTopicMemory records the learner's own decision about one conversation.
//
// The decision is a conversation's, and nothing wider: no topic key, no scope and no user list
// is accepted here, because which subject a yes unlocks is worked out later, on the read side,
// from the quiz this conversation has open. A learner who consents and then closes the quiz has
// consented to nothing at all - not because their answer is ignored, but because there is no
// longer a subject for it to apply to.
//
// Ownership is checked before the mode, so this cannot be used to discover which of another
// account's conversations are learning agents: a conversation that is not yours answers the same
// way whether its mode is chat, agent, or never created at all.
//
// Writing the same answer twice changes nothing, so a client that retries a click the network
// lost is not a second decision - and there is no version to pass, because the stored state is
// the whole point of the call.
func (s *SessionService) CommitTopicMemory(ctx context.Context, userID, sessionID string,
	enabled bool) (*domain.TopicMemoryPreference, error) {
	if s.repo == nil {
		return nil, ErrInternal
	}
	if strings.TrimSpace(userID) == "" {
		return nil, ErrUnauthorized
	}
	if strings.TrimSpace(sessionID) == "" {
		return nil, ErrInvalidRequest
	}
	conversation, err := s.repo.GetSession(ctx, userID, sessionID)
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	if conversation.Mode != domain.SessionModeLearningAgent {
		return nil, ErrTopicMemoryNeedsLearningAgent
	}
	committed, err := s.repo.CommitTopicMemoryPreference(ctx, userID, sessionID, enabled)
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	return committed, nil
}

// TopicMemoryState reads the decision a learner last stored for one conversation.
//
// It answers "what did they choose", not "will this turn read topic memory". The second question
// has two more inputs - whether this conversation is a learning agent at all, and whether a live
// quiz names a subject - and none of them belong in a switch's answer: a page that showed "on"
// while no quiz is open would promise a read that is not happening, and a page that hid the switch
// until a quiz existed would make the setting look like part of the quiz.
//
// A conversation with no record answers false with no error, so a client never has to interpret a
// missing row. A conversation that is not the caller's and one that never existed are both
// ErrNotFound, which is the same pair every other session read refuses to distinguish. The mode is
// deliberately not checked here: refusing to let a learner *read* their own stored answer would be
// a refusal with nothing to correct, and the recall side checks the mode for itself.
func (s *SessionService) TopicMemoryState(ctx context.Context, userID, sessionID string) (bool, error) {
	if s.repo == nil {
		return false, ErrInternal
	}
	if strings.TrimSpace(userID) == "" {
		return false, ErrUnauthorized
	}
	if strings.TrimSpace(sessionID) == "" {
		return false, ErrInvalidRequest
	}
	stored, err := s.repo.GetTopicMemoryPreference(ctx, userID, sessionID)
	if err != nil {
		return false, mapRepositoryError(err)
	}
	return stored.Enabled, nil
}
