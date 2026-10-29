// Package repository defines persistence boundaries for the Phase 1 domain.
package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/xiaopang456/GopherLearn/internal/domain"
)

// ErrNotFound reports that a requested entity does not exist or is not owned
// by the supplied user. Ownership failures intentionally use the same error as
// missing entities so callers cannot use this repository to probe other users'
// data.
var ErrNotFound = errors.New("repository: not found")

// ErrConflict reports that creating an entity would violate a repository
// uniqueness constraint.
var ErrConflict = errors.New("repository: conflict")

// ErrInvalidArgument reports a nil entity or an otherwise invalid repository
// argument.
var ErrInvalidArgument = errors.New("repository: invalid argument")

// ErrMemoryProvenanceMissing reports that a memory's cited session or cited messages are
// no longer stored, so a pending draft cannot be confirmed. Confirmation is the point where
// a claim becomes a fact the system will keep, and a fact whose evidence has been deleted
// is not confirmable - the alternative is an active memory nobody can ever check. It is a
// distinct error rather than ErrNotFound because the memory itself was found; what is
// missing is what it points at.
var ErrMemoryProvenanceMissing = errors.New("repository: memory provenance is no longer available")

// MemoryStateConflict reports that a memory transition was refused because the stored
// record is not in a state that allows it. It carries the state that was actually found,
// because "already confirmed", "that one was rejected" and "a newer one replaced it"
// deserve different answers to a user even though all three are conflicts. It satisfies
// errors.Is(err, ErrConflict), so a caller that only cares that the row moved can stay
// simple while the service decides what to say.
type MemoryStateConflict struct {
	MemoryID string
	Status   domain.MemoryStatus
}

func (e *MemoryStateConflict) Error() string {
	return fmt.Sprintf("repository: memory %s is %s, not the state this transition needs", e.MemoryID, string(e.Status))
}

// Is makes every state conflict match ErrConflict while keeping its own identity for
// errors.As.
func (e *MemoryStateConflict) Is(target error) bool { return target == ErrConflict }

// Repository stores users, sessions, and messages.
//
// Session and message operations take userID explicitly. Implementations must
// enforce that the requested session belongs to that user. A missing resource
// and a resource owned by another user are both reported as ErrNotFound.
//
// The memory operations are the storage half of the long-term memory confirmation
// gate. They belong on this interface rather than a parallel one because they
// need the same three things the conversation methods need - one transaction, one
// ownership rule, and one conformance suite both stores must pass - so a
// development process and a MySQL deployment cannot disagree about whether a
// draft is still a draft.
type Repository interface {
	CreateUser(ctx context.Context, user *domain.User) (*domain.User, error)
	FindUserByEmail(ctx context.Context, email string) (*domain.User, error)
	GetUser(ctx context.Context, userID string) (*domain.User, error)

	CreateSession(ctx context.Context, session *domain.Session) (*domain.Session, error)
	ListSessions(ctx context.Context, userID string) ([]domain.Session, error)
	GetSession(ctx context.Context, userID, sessionID string) (*domain.Session, error)
	DeleteSession(ctx context.Context, userID, sessionID string) error

	AppendMessage(ctx context.Context, userID, sessionID string, message *domain.Message) (*domain.Message, error)
	ListMessages(ctx context.Context, userID, sessionID string) ([]domain.Message, error)

	// GetSessionSummary returns the session's rolling summary. A session with none
	// yet, a session that does not exist and a session owned by somebody else are
	// all ErrNotFound, so a summary cannot be probed across accounts.
	GetSessionSummary(ctx context.Context, userID, sessionID string) (*domain.SessionSummary, error)

	// CommitSessionSummary stores one new summary version built on expectVersion,
	// where 0 means "there was no summary when I read". If the stored row has moved
	// on in the meantime the commit is refused with ErrConflict and nothing is
	// written: a refresh that took longer than another one must not roll back the
	// newer text. On success the stored row - with its new version and update time -
	// is returned.
	CommitSessionSummary(ctx context.Context, summary *domain.SessionSummary, expectVersion int64) (*domain.SessionSummary, error)

	// GetTopicMemoryPreference reads one conversation's topic-memory consent.
	//
	// A conversation with no record answers Enabled=false rather than ErrNotFound, because
	// "never asked" and "asked to stop" must be the same state to the recall path - a
	// missing row that read as an error would invite a caller to decide what to do instead
	// of reading nothing. The *session* must exist and belong to userID, and a session that
	// does not is ErrNotFound, so this cannot be used to probe another account's settings.
	//
	// It reports the learner's decision only. Which subject that decision unlocks is decided
	// elsewhere, from the quiz open on the conversation.
	GetTopicMemoryPreference(ctx context.Context, userID, sessionID string) (domain.TopicMemoryPreference, error)

	// CommitTopicMemoryPreference stores the learner's decision as the new state of one
	// conversation, creating the row when there is none.
	//
	// It is a commit of a value, not a transition of a state machine, so it is idempotent:
	// committing the same value twice leaves the same readable state, and no version or
	// lease is needed to say so. What it must not be is a way to write somebody else's
	// conversation - a session that is missing or owned by another account is ErrNotFound,
	// the same answer for both cases and the same one every other session method gives.
	CommitTopicMemoryPreference(ctx context.Context, userID, sessionID string, enabled bool) (*domain.TopicMemoryPreference, error)

	// CreateMemoryDraft stores a candidate as pending. Its provenance is checked while
	// it is written: the source session must belong to this user and every listed
	// message must exist in that session, so no stored memory ever claims evidence that
	// was not there. A draft cannot be created already active - see domain.MemoryDraft.
	CreateMemoryDraft(ctx context.Context, draft *domain.MemoryDraft) (*domain.Memory, error)

	// CreateMemoryDraftOnce is the idempotent form of the same write, for a caller that
	// may ask twice about one thing - an agent turn that retries a proposal, a client on a
	// flaky connection. When one of the caller's own pending rows already makes the same
	// claim (domain.Memory.ProposalsMatch), that row is returned with created=false and
	// nothing is inserted; otherwise it behaves exactly like CreateMemoryDraft.
	//
	// The check and the insert happen in one critical section, so two concurrent
	// identical proposals cannot both decide they are the first. Only pending rows are
	// matched: a memory that was confirmed, rejected or superseded is history, and a new
	// claim on the same subject is a new decision for the user to make.
	CreateMemoryDraftOnce(ctx context.Context, draft *domain.MemoryDraft) (*domain.Memory, bool, error)

	// StoreUserAuthoredMemory writes one memory the person stated themselves: it lands
	// active, it cites no conversation, and for a versioned scope the row it replaces is
	// superseded in the same critical section, so one request cannot leave two active
	// answers to one subject behind.
	//
	// It is a separate method rather than a flag on CreateMemoryDraft because the two
	// writes answer different questions. A draft is a claim about a conversation and is
	// only a fact after someone decides; an authored memory IS the decision, and its
	// source is the sentence itself. Sharing one entry point would mean sharing the
	// provenance gate, and loosening that gate for one caller would loosen it for the
	// agent - which is the caller that must never gain a way to write itself a fact.
	StoreUserAuthoredMemory(ctx context.Context, authored *domain.UserAuthoredMemory) (*domain.Memory, error)

	// GetMemory reads one of the user's memories in whatever state it is in. This is the
	// owner's view of pending work, not a recall: the answer path has no business
	// reading a row the user has not confirmed.
	GetMemory(ctx context.Context, userID, memoryID string) (*domain.Memory, error)

	// ConfirmMemoryDraft moves one pending draft to active in a single transaction, and
	// where the scope is versioned it supersedes the memory that was active for the same
	// (user, scope, topic_key) in the same transaction. The write is conditional on the
	// stored status, so two callers confirming the same draft cannot both take effect:
	// one gets the memory, the other a *MemoryStateConflict.
	ConfirmMemoryDraft(ctx context.Context, userID, memoryID string) (*domain.Memory, error)

	// RejectMemoryDraft retires a pending draft without deleting it, so what was
	// proposed and refused stays auditable. Only a pending row can be rejected.
	RejectMemoryDraft(ctx context.Context, userID, memoryID string) (*domain.Memory, error)

	// ArchiveMemory retires a memory that has stopped applying. Superseded and archived
	// are terminal: nothing in this interface revives a memory, because "put the old
	// answer back" is a new confirmation, not an undo.
	ArchiveMemory(ctx context.Context, userID, memoryID string) (*domain.Memory, error)

	// ListActiveMemories is the only memory read a future prompt may be built from, and
	// it never returns pending, superseded or archived rows. Global scope takes an empty
	// topic key, topic scope requires one, and episodic scope accepts either an empty key
	// for all episodes or one specific topic.
	ListActiveMemories(ctx context.Context, userID string, scope domain.MemoryScope, topicKey string) ([]domain.Memory, error)

	// ListMemoriesByStatus is the owner's management view: every one of the caller's own
	// rows that currently sits in one status, in creation order, across all scopes. It
	// exists because a confirmation surface has to enumerate what it is being asked to
	// decide on, and a topic memory can only be listed by key through
	// ListActiveMemories. This is not a recall path: it filters by status and ownership
	// and nothing else, so a caller that wants prompt-facing rows must use
	// ListActiveMemories and keep applying the read-side grants.
	ListMemoriesByStatus(ctx context.Context, userID string, status domain.MemoryStatus) ([]domain.Memory, error)

	// DeleteMemory removes a record outright. That is a different act from archiving -
	// the user's right to erase their own data - which is why both operations exist.
	DeleteMemory(ctx context.Context, userID, memoryID string) error

	// LearningStore is the quiz workflow: one unfinished quiz per conversation,
	// ordered questions, one idempotent attempt per submission, and the topic
	// projection that follows from a committed grade. It is a named interface rather
	// than fourteen more lines in here because it is a state machine with its own
	// vocabulary, and a reader who only cares about messages should not have to
	// scroll past a lease fence to find out what AppendMessage means.
	LearningStore

	// EpisodeStore is the episodic memory system: one derived, versioned card
	// per session plus the durable projection jobs the background worker
	// claims. It is part of this interface for the same reason LearningStore
	// is: it needs the same ownership rules and the same conformance suite as
	// the conversation rows it is derived from.
	EpisodeStore
}

var _ Repository = (*InMemoryRepository)(nil)
