package service

import (
	"context"
	"errors"
	"strings"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/repository"
)

// MemoryService owns the confirmation gate: the only door between "someone proposed a
// fact" and "this is what we believe about the user".
//
// The gate is deliberately not a model. Nothing here asks a model to judge a memory,
// because a system that decides what the user believes by asking the thing that will later
// answer from those beliefs cannot be corrected: a wrong memory would be confirmed by the
// same judgement that produced it. The user confirms, in their own account, and every
// path to active status goes through this file.
type MemoryService struct {
	repo repository.Repository
}

// NewMemoryService wires the service to a store. A nil store is tolerated at
// construction so a binary can be assembled without memory support, and every method
// then refuses rather than panicking.
func NewMemoryService(repo repository.Repository) *MemoryService {
	return &MemoryService{repo: repo}
}

// The memory lifecycle errors are distinct because a user facing each of them needs a
// different answer: "you already did that", "you refused that one", "something newer took
// its place". Collapsing them into ErrConflict would make the confirmation surface guess.
var (
	// ErrMemoryAlreadyConfirmed reports a second confirmation of a memory that is
	// already active.
	ErrMemoryAlreadyConfirmed = errors.New("memory draft was already confirmed")
	// ErrMemoryArchived reports a memory that was rejected while pending or archived
	// after being active. Both end in the same stored state, and neither can be
	// confirmed: reviving it is a new draft, not a retry.
	ErrMemoryArchived = errors.New("memory is archived and no longer usable")
	// ErrMemorySuperseded reports a memory that a newer confirmation replaced.
	ErrMemorySuperseded = errors.New("memory was replaced by a newer one")
	// ErrMemoryNotActive reports an archive request on something that is not an active
	// memory - typically a draft, which is rejected rather than archived.
	ErrMemoryNotActive = errors.New("memory is not active")
	// ErrMemoryEvidenceMissing reports a draft whose cited conversation is no longer
	// stored. The proposal may have been reasonable when it was written; there is now
	// nothing left to check it against, so it stays a proposal.
	ErrMemoryEvidenceMissing = errors.New("the conversation this memory cites is no longer stored")
)

// CreateMemoryDraft stores a candidate for the authenticated user. The draft's own user
// field is overwritten rather than trusted: filing a memory into another account would be
// a privilege issue, not a validation detail, and the caller's identity is the one fact
// this layer already knows.
func (s *MemoryService) CreateMemoryDraft(ctx context.Context, userID string, draft domain.MemoryDraft) (*domain.Memory, error) {
	if s.repo == nil {
		return nil, ErrInternal
	}
	if strings.TrimSpace(userID) == "" {
		return nil, ErrUnauthorized
	}
	draft.UserID = userID
	if err := draft.Validate(); err != nil {
		return nil, ErrInvalidRequest
	}
	created, err := s.repo.CreateMemoryDraft(ctx, &draft)
	if err != nil {
		return nil, mapMemoryError(err)
	}
	return created, nil
}

// AddUserMemory stores a memory the person typed in: it is active on arrival, and the
// previous active version of the same subject is superseded by the same write. There is no
// second request to confirm a decision the submit button already made.
//
// This is not CreateMemoryDraft with the rules loosened - that path still demands a session
// and a stored message, because those are what make somebody else's claim about a
// conversation checkable. The two writes are separate methods on the store for that reason:
// a change here cannot reach the agent.
func (s *MemoryService) AddUserMemory(ctx context.Context, userID string, authored domain.UserAuthoredMemory) (*domain.Memory, error) {
	if s.repo == nil {
		return nil, ErrInternal
	}
	if strings.TrimSpace(userID) == "" {
		return nil, ErrUnauthorized
	}
	// Identity is taken, never accepted: the authenticated account is the only owner this
	// path will write for.
	authored.UserID = userID
	if err := authored.Validate(); err != nil {
		return nil, ErrInvalidRequest
	}
	created, err := s.repo.StoreUserAuthoredMemory(ctx, &authored)
	if err != nil {
		return nil, mapMemoryError(err)
	}
	return created, nil
}

// ProposeMemoryDraft stores one candidate on a user's behalf and reports whether it was a
// new row. It exists for a program that observes a conversation - the learning agent's
// propose_memory tool - and it stops exactly where storing stops: the returned memory is
// pending, and only ConfirmMemoryDraft, called by the user, makes it a fact.
//
// Two values are owned here rather than by the caller. The user id is overwritten, because
// filing a memory into another account would be a privilege issue. And confidence is fixed
// at "certain", because a program cannot grade its own reading of a conversation: a
// sub-threshold value would store a memory the recall floor then never shows - a fact
// recorded and silently ignored - and that choice is not the proposer's to make.
//
// Idempotency is the difference from CreateMemoryDraft. A caller that repeats the same claim
// gets the row that is already waiting, with created=false, rather than a second draft that
// says the same thing twice on the confirmation screen.
func (s *MemoryService) ProposeMemoryDraft(ctx context.Context, userID string, draft domain.MemoryDraft) (*domain.Memory, bool, error) {
	if s.repo == nil {
		return nil, false, ErrInternal
	}
	if strings.TrimSpace(userID) == "" {
		return nil, false, ErrUnauthorized
	}
	draft.UserID = userID
	draft.Confidence = 1
	if err := draft.Validate(); err != nil {
		return nil, false, ErrInvalidRequest
	}
	proposed, created, err := s.repo.CreateMemoryDraftOnce(ctx, &draft)
	if err != nil {
		return nil, false, mapMemoryError(err)
	}
	return proposed, created, nil
}

// ConfirmMemoryDraft makes the user's own pending draft active.
func (s *MemoryService) ConfirmMemoryDraft(ctx context.Context, userID, memoryID string) (*domain.Memory, error) {
	if s.repo == nil {
		return nil, ErrInternal
	}
	if strings.TrimSpace(userID) == "" {
		return nil, ErrUnauthorized
	}
	if strings.TrimSpace(memoryID) == "" {
		return nil, ErrInvalidRequest
	}
	confirmed, err := s.repo.ConfirmMemoryDraft(ctx, userID, memoryID)
	if err != nil {
		return nil, mapMemoryError(err)
	}
	return confirmed, nil
}

// RejectMemoryDraft refuses a candidate the user does not want remembered. The row stays,
// so "what did I already say no to" remains answerable.
func (s *MemoryService) RejectMemoryDraft(ctx context.Context, userID, memoryID string) (*domain.Memory, error) {
	return s.transition(ctx, userID, memoryID, s.repoReject)
}

// ArchiveMemory retires a memory that has stopped applying.
func (s *MemoryService) ArchiveMemory(ctx context.Context, userID, memoryID string) (*domain.Memory, error) {
	return s.transition(ctx, userID, memoryID, s.repoArchive)
}

func (s *MemoryService) transition(
	ctx context.Context, userID, memoryID string, call func(context.Context, string, string) (*domain.Memory, error),
) (*domain.Memory, error) {
	if s.repo == nil {
		return nil, ErrInternal
	}
	if strings.TrimSpace(userID) == "" {
		return nil, ErrUnauthorized
	}
	if strings.TrimSpace(memoryID) == "" {
		return nil, ErrInvalidRequest
	}
	memory, err := call(ctx, userID, memoryID)
	if err != nil {
		return nil, mapMemoryError(err)
	}
	return memory, nil
}

func (s *MemoryService) repoReject(ctx context.Context, userID, memoryID string) (*domain.Memory, error) {
	return s.repo.RejectMemoryDraft(ctx, userID, memoryID)
}

func (s *MemoryService) repoArchive(ctx context.Context, userID, memoryID string) (*domain.Memory, error) {
	return s.repo.ArchiveMemory(ctx, userID, memoryID)
}

// Memory reads one of the user's own memories in whatever state it is in, which is how a
// confirmation surface shows a draft before deciding on it.
func (s *MemoryService) Memory(ctx context.Context, userID, memoryID string) (*domain.Memory, error) {
	if s.repo == nil {
		return nil, ErrInternal
	}
	if strings.TrimSpace(userID) == "" {
		return nil, ErrUnauthorized
	}
	if strings.TrimSpace(memoryID) == "" {
		return nil, ErrInvalidRequest
	}
	memory, err := s.repo.GetMemory(ctx, userID, memoryID)
	if err != nil {
		return nil, mapMemoryError(err)
	}
	return memory, nil
}

// ActiveMemories lists the confirmed memories for one scope. The store filters by status,
// and this method only ever calls that filtered read - the pending rows are reachable
// through Memory, never through a list a model might one day be shown.
func (s *MemoryService) ActiveMemories(ctx context.Context, userID string, scope domain.MemoryScope, topicKey string) ([]domain.Memory, error) {
	if s.repo == nil {
		return nil, ErrInternal
	}
	if strings.TrimSpace(userID) == "" {
		return nil, ErrUnauthorized
	}
	if !scope.IsValid() {
		return nil, ErrInvalidRequest
	}
	items, err := s.repo.ListActiveMemories(ctx, userID, scope, strings.TrimSpace(topicKey))
	if err != nil {
		return nil, mapMemoryError(err)
	}
	return items, nil
}

// PendingDrafts lists what the user has been asked to decide on: their own rows still in
// pending, oldest first, across every scope. Two named methods rather than one that takes
// a status, so a caller cannot ask this service for an archived or superseded listing it
// has no use for - and so the confirmation surface cannot drift into reading a state the
// gate is supposed to be the only writer of.
func (s *MemoryService) PendingDrafts(ctx context.Context, userID string) ([]domain.Memory, error) {
	return s.listByStatus(ctx, userID, domain.MemoryStatusPending)
}

// ConfirmedMemories lists the user's own active memories, oldest first, across every scope
// including topics keyed by name. This is the management view that lets someone archive
// what they confirmed; it is not the recall read, which applies grants and floors on top.
func (s *MemoryService) ConfirmedMemories(ctx context.Context, userID string) ([]domain.Memory, error) {
	return s.listByStatus(ctx, userID, domain.MemoryStatusActive)
}

func (s *MemoryService) listByStatus(ctx context.Context, userID string, status domain.MemoryStatus) ([]domain.Memory, error) {
	if s.repo == nil {
		return nil, ErrInternal
	}
	if strings.TrimSpace(userID) == "" {
		return nil, ErrUnauthorized
	}
	items, err := s.repo.ListMemoriesByStatus(ctx, userID, status)
	if err != nil {
		return nil, mapMemoryError(err)
	}
	return items, nil
}

// DeleteMemory erases one of the user's memories outright, which is a different act from
// archiving it and is the user's to request.
func (s *MemoryService) DeleteMemory(ctx context.Context, userID, memoryID string) error {
	if s.repo == nil {
		return ErrInternal
	}
	if strings.TrimSpace(userID) == "" {
		return ErrUnauthorized
	}
	if strings.TrimSpace(memoryID) == "" {
		return ErrInvalidRequest
	}
	return mapMemoryError(s.repo.DeleteMemory(ctx, userID, memoryID))
}

// mapMemoryError keeps the store's vocabulary inside the service's own errors. The state
// conflict is decoded first, because it is the one repository error whose meaning depends
// on the state it found: after that it is also a conflict, and mapping it as one would
// lose the difference the caller needs.
func mapMemoryError(err error) error {
	if err == nil {
		return nil
	}
	var conflict *repository.MemoryStateConflict
	if errors.As(err, &conflict) {
		switch conflict.Status {
		case domain.MemoryStatusActive:
			return ErrMemoryAlreadyConfirmed
		case domain.MemoryStatusArchived:
			return ErrMemoryArchived
		case domain.MemoryStatusSuperseded:
			return ErrMemorySuperseded
		default:
			// Still pending, or a status this service does not know: the transition the
			// caller asked for is not the one available.
			return ErrMemoryNotActive
		}
	}
	switch {
	case errors.Is(err, repository.ErrMemoryProvenanceMissing):
		return ErrMemoryEvidenceMissing
	default:
		return mapRepositoryError(err)
	}
}
