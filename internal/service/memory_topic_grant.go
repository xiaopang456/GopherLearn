package service

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/repository"
)

// LearningTopicStore is the whole set of server-owned facts that can decide whether one session
// may read its topic memory: whose conversation this is and which mode it was created in, whether
// the learner has asked for topic memory here, and which quiz is open on it right now.
//
// It is a three-method seam rather than repository.Repository because an authorizer that could
// also write a memory, list another account, or read rows directly would hold a wider permission
// than the one it is asked about. "Decide what this turn may recall" needs reads that are already
// ownership-scoped and nothing else.
type LearningTopicStore interface {
	GetSession(ctx context.Context, userID, sessionID string) (*domain.Session, error)
	GetTopicMemoryPreference(ctx context.Context, userID, sessionID string) (domain.TopicMemoryPreference, error)
	GetActiveLearningSession(ctx context.Context, userID, chatSessionID string) (*domain.LearningSession, error)
}

// ErrNoLearningTopicStore reports a topic authorizer assembled with nothing to ask. It is
// refused at construction, where a deployment can still fail to start, rather than at the first
// recall, where it would look exactly like a learner who never started a quiz.
var ErrNoLearningTopicStore = errors.New("service: topic grant has no learning store")

// LearningTopicGrant is the authorizer that may add topic memory to the default grant, and it
// adds it only when three independent server-side facts all hold: the conversation is a learning
// agent conversation, the learner has committed an on-decision for that conversation, and a live
// quiz on one subject is open on it.
//
// The quiz requirement is intentional for this existing API. The future legacy_v2 relevant-topic
// supply is a separate provider seam and must not be claimed by this authorizer until implemented.
type LearningTopicGrant struct {
	base  MemoryAuthorizer
	store LearningTopicStore
}

// NewLearningTopicGrant installs the topic authorizer over a store that can answer all three
// questions. The base grant is SessionMemoryGrant, so the layers that were already readable - the
// user's global memories and this session's own episodes - keep the shape they have on the default
// authorizer, byte for byte, including the scope note, for every conversation that does not meet
// all three conditions.
func NewLearningTopicGrant(store LearningTopicStore) (*LearningTopicGrant, error) {
	if store == nil {
		return nil, ErrNoLearningTopicStore
	}
	return &LearningTopicGrant{base: SessionMemoryGrant{}, store: store}, nil
}

// AuthorizeMemoryScope implements MemoryAuthorizer.
//
// Every failure to establish the topic is answered by "no topic", never by a wider read and never
// by an error: a missing or foreign conversation, a conversation with no open quiz and a
// conversation whose learner never asked are all ordinary states, and the honest description of
// each of them is the default grant. A store that fails for another reason is different - it is
// returned raw so Recall can map it once, and the read ends rather than proceeding on a guess.
//
// The consent is read before the quiz, which is the order the learner would expect: a conversation
// that was never switched on does not go looking for a quiz to justify itself, and the scope note a
// switched-off conversation produces is the same text the default authorizer writes.
func (g LearningTopicGrant) AuthorizeMemoryScope(ctx context.Context, userID, sessionID string) (MemoryGrant, error) {
	grant, err := g.base.AuthorizeMemoryScope(ctx, userID, sessionID)
	if err != nil {
		return MemoryGrant{}, err
	}
	// Without a conversation there is nothing to look a consent or a quiz up on. This is the
	// same shape as the default grant, which also drops the episodic layer when the session is
	// absent.
	chatSessionID := strings.TrimSpace(sessionID)
	if chatSessionID == "" {
		return grant, nil
	}

	// Ownership before mode, exactly as starting a quiz does: a conversation that is not the
	// caller's is "not found" whatever its mode and whatever its stored consent say, so neither
	// rule can be used to discover which of another account's sessions are learning agents.
	conversation, err := g.store.GetSession(ctx, userID, chatSessionID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return grant, nil
		}
		return MemoryGrant{}, err
	}
	if conversation == nil || conversation.Mode != domain.SessionModeLearningAgent {
		return grant, nil
	}

	// The learner's own answer. Absence of a record and a record that says false are read the
	// same way by the store, and both mean the grant stops here - which is what keeps the
	// default closed the moment the switch is turned off, with no cache in this process that
	// could keep reading topic memory for one more turn.
	consent, err := g.store.GetTopicMemoryPreference(ctx, userID, chatSessionID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			// The conversation disappeared while this read was happening. Nothing is
			// authorized beyond the default, and that is the safe direction.
			return grant, nil
		}
		return MemoryGrant{}, err
	}
	if !consent.Enabled {
		return grant, nil
	}

	quiz, err := g.store.GetActiveLearningSession(ctx, userID, chatSessionID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return grant, nil
		}
		return MemoryGrant{}, err
	}
	if !quizGrantsTopic(quiz, userID, chatSessionID, time.Now().UTC()) {
		return grant, nil
	}
	grant.TopicKeys = []string{strings.TrimSpace(quiz.TopicKey)}
	return grant, nil
}

// quizGrantsTopic decides whether one returned quiz is a live, owned reason to read topic memory.
//
// The expiry check is the one that is not already done for the caller. GetActiveLearningSession
// answers "is there a quiz on this conversation", and both stores deliberately keep a quiz that is
// past its deadline until a reader recovers it - so without this check a quiz that expired last
// week would still be describing today's learner. The identity checks are the assertion this whole
// grant exists to make: the topic comes from a quiz on this session, belonging to this user, and
// not from anything a caller named.
func quizGrantsTopic(quiz *domain.LearningSession, userID, sessionID string, now time.Time) bool {
	if quiz == nil {
		return false
	}
	key := strings.TrimSpace(quiz.TopicKey)
	if key == "" {
		return false
	}
	if quiz.UserID != userID || quiz.SessionID != sessionID {
		return false
	}
	return !quiz.Status.IsTerminal() && !quiz.IsExpired(now)
}
