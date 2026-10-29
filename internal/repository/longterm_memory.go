package repository

import (
	"context"
	"strings"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/domain"
)

// This file is the in-memory half of long-term memory storage. It exists so the
// development mode of the binary has the same confirmation gate as a MySQL deployment,
// and so the shared conformance suite can hold both to one rule: a memory the user has
// not confirmed is not a fact, whichever store the process happens to use.

// CreateMemoryDraft stores a candidate as a pending memory. The provenance is checked
// here rather than at read time, because "which messages is this memory about" is part of
// what makes it checkable later: a row citing a message that never existed would turn
// every future correction exercise into a search for evidence that was never there.
func (r *InMemoryRepository) CreateMemoryDraft(ctx context.Context, draft *domain.MemoryDraft) (*domain.Memory, error) {
	memory, _, err := r.storeMemoryDraft(ctx, draft, false)
	return memory, err
}

// CreateMemoryDraftOnce returns an existing pending row instead of storing a second copy
// of the same claim.
func (r *InMemoryRepository) CreateMemoryDraftOnce(ctx context.Context, draft *domain.MemoryDraft) (*domain.Memory, bool, error) {
	return r.storeMemoryDraft(ctx, draft, true)
}

// storeMemoryDraft is the only place this file creates a pending row, so the duplicate
// check cannot drift from the write it guards: both happen under the same write lock, which
// is what stops two concurrent identical proposals from each deciding they were first.
func (r *InMemoryRepository) storeMemoryDraft(
	ctx context.Context, draft *domain.MemoryDraft, reuseEquivalent bool,
) (*domain.Memory, bool, error) {
	if err := checkContext(ctx); err != nil {
		return nil, false, err
	}
	if draft == nil {
		return nil, false, ErrInvalidArgument
	}
	if err := draft.Validate(); err != nil {
		return nil, false, ErrInvalidArgument
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.initLocked()

	memory := draft.StoredMemory()
	if _, exists := r.users[memory.UserID]; !exists {
		return nil, false, ErrNotFound
	}
	if session, exists := r.sessions[memory.SourceSessionID]; !exists || session.UserID != memory.UserID {
		// A session that is not this user's and one that never existed are the same
		// answer, so drafting a memory cannot be used to probe another account.
		return nil, false, ErrNotFound
	}
	if !r.messagesInSessionLocked(memory.SourceSessionID, memory.SourceMessageIDs) {
		return nil, false, ErrInvalidArgument
	}
	if reuseEquivalent {
		for _, memoryID := range r.memoryOrder {
			stored := r.memories[memoryID]
			if stored.Status != domain.MemoryStatusPending || !stored.ProposalsMatch(memory) {
				continue
			}
			existing := stored.Clone()
			return &existing, false, nil
		}
	}
	memory.MemoryID = r.nextID("mem")
	now := time.Now().UTC()
	memory.CreatedAt, memory.UpdatedAt = now, now
	r.memories[memory.MemoryID] = memory
	r.memoryOrder = append(r.memoryOrder, memory.MemoryID)
	stored := memory.Clone()
	return &stored, true, nil
}

// StoreUserAuthoredMemory writes one memory the person typed in, already active, and
// retires whatever was active for the same subject in the same critical section.
//
// The order is the point: the older row is superseded before the new one becomes visible,
// so a reader who takes the lock after this call returns can never see two active answers
// to one subject, and a reader who took it before sees neither half of the swap. One
// request, one state change - which is what "confirm by submitting" has to mean, since a
// write that left the old version active would have created the contradiction the
// confirmation path exists to avoid.
func (r *InMemoryRepository) StoreUserAuthoredMemory(ctx context.Context, authored *domain.UserAuthoredMemory) (*domain.Memory, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if authored == nil {
		return nil, ErrInvalidArgument
	}
	if err := authored.Validate(); err != nil {
		return nil, ErrInvalidArgument
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.initLocked()

	memory := authored.StoredMemory()
	// The account still has to exist, because a memory for a user who does not exist is a
	// row nobody will ever list or delete. There is no session to check and no message to
	// look for: this record's evidence is the sentence itself.
	if _, exists := r.users[memory.UserID]; !exists {
		return nil, ErrNotFound
	}
	memory.MemoryID = r.nextID("mem")
	now := time.Now().UTC()
	memory.CreatedAt, memory.UpdatedAt = now, now

	if memory.Scope.SupersedesSiblings() {
		for _, memoryID := range r.memoryOrder {
			stored := r.memories[memoryID]
			if stored.UserID != memory.UserID || stored.Scope != memory.Scope ||
				stored.TopicKey != memory.TopicKey || stored.Status != domain.MemoryStatusActive {
				continue
			}
			stored.Status = domain.MemoryStatusSuperseded
			stored.SupersededAt = now
			stored.UpdatedAt = now
			r.memories[memoryID] = stored
		}
	}

	r.memories[memory.MemoryID] = memory
	r.memoryOrder = append(r.memoryOrder, memory.MemoryID)
	stored := memory.Clone()
	return &stored, nil
}

// GetMemory returns the owner's copy of one memory, in whatever state it is in.
func (r *InMemoryRepository) GetMemory(ctx context.Context, userID, memoryID string) (*domain.Memory, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(memoryID) == "" {
		return nil, ErrInvalidArgument
	}

	r.mu.RLock()
	defer r.mu.RUnlock()
	r.initLocked()

	stored, exists := r.memories[memoryID]
	if !exists || stored.UserID != userID {
		// Somebody else's memory and no such memory are the same answer, so this store
		// cannot be used to probe which ids exist in another account.
		return nil, ErrNotFound
	}
	memory := stored.Clone()
	return &memory, nil
}

// ConfirmMemoryDraft makes one pending draft active, superseding whatever was active for
// the same scope and topic in the same critical section. All of it happens under one
// write lock on purpose: an active memory with a still-active predecessor would mean two
// answers to one question, and no reader could tell which was meant.
func (r *InMemoryRepository) ConfirmMemoryDraft(ctx context.Context, userID, memoryID string) (*domain.Memory, error) {
	return r.transitionMemory(ctx, userID, memoryID, domain.MemoryStatusPending, domain.MemoryStatusActive)
}

// RejectMemoryDraft retires a pending draft. An active memory cannot be rejected: that is
// what archiving is for, and conflating the two would let a typo undo something the user
// confirmed.
func (r *InMemoryRepository) RejectMemoryDraft(ctx context.Context, userID, memoryID string) (*domain.Memory, error) {
	return r.transitionMemory(ctx, userID, memoryID, domain.MemoryStatusPending, domain.MemoryStatusArchived)
}

// ArchiveMemory retires an active memory.
func (r *InMemoryRepository) ArchiveMemory(ctx context.Context, userID, memoryID string) (*domain.Memory, error) {
	return r.transitionMemory(ctx, userID, memoryID, domain.MemoryStatusActive, domain.MemoryStatusArchived)
}

func (r *InMemoryRepository) transitionMemory(
	ctx context.Context, userID, memoryID string, requireFrom, target domain.MemoryStatus,
) (*domain.Memory, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(memoryID) == "" {
		return nil, ErrInvalidArgument
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.initLocked()

	stored, exists := r.memories[memoryID]
	if !exists || stored.UserID != userID {
		return nil, ErrNotFound
	}
	if stored.Status != requireFrom || !stored.Status.CanTransitionTo(target) {
		return nil, &MemoryStateConflict{MemoryID: memoryID, Status: stored.Status}
	}
	if target == domain.MemoryStatusActive && !r.provenanceIntactLocked(stored.UserID, stored.SourceSessionID, stored.SourceMessageIDs) {
		// The draft passed this check when it was stored. Reaching it again catches the
		// case where the conversation it cites has been deleted in the meantime: the
		// state machine allows the promotion, but there is nothing left to confirm it
		// against.
		return nil, ErrMemoryProvenanceMissing
	}

	now := time.Now().UTC()
	if target == domain.MemoryStatusActive && stored.Scope.SupersedesSiblings() {
		for _, siblingID := range r.memoryOrder {
			sibling := r.memories[siblingID]
			if siblingID == memoryID || sibling.UserID != stored.UserID ||
				sibling.Scope != stored.Scope || sibling.TopicKey != stored.TopicKey ||
				sibling.Status != domain.MemoryStatusActive {
				continue
			}
			sibling.Status = domain.MemoryStatusSuperseded
			sibling.SupersededAt = now
			sibling.UpdatedAt = now
			r.memories[siblingID] = sibling
		}
	}
	stored.Status = target
	stored.UpdatedAt = now
	r.memories[memoryID] = stored
	memory := stored.Clone()
	return &memory, nil
}

// ListActiveMemories returns the user's confirmed memories in creation order. Pending,
// superseded and archived rows are excluded here rather than by every caller, because one
// forgetful reader would otherwise be enough to put an unconfirmed candidate in front of a
// model.
func (r *InMemoryRepository) ListActiveMemories(ctx context.Context, userID string, scope domain.MemoryScope, topicKey string) ([]domain.Memory, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if strings.TrimSpace(userID) == "" {
		return nil, ErrInvalidArgument
	}
	if !scope.IsValid() {
		return nil, ErrInvalidArgument
	}
	topicKey = strings.TrimSpace(topicKey)
	if err := validateMemoryListScope(scope, topicKey); err != nil {
		return nil, err
	}

	r.mu.RLock()
	defer r.mu.RUnlock()
	r.initLocked()

	found := make([]domain.Memory, 0)
	for _, memoryID := range r.memoryOrder {
		stored := r.memories[memoryID]
		if stored.UserID != userID || stored.Status != domain.MemoryStatusActive || stored.Scope != scope {
			continue
		}
		if topicKey != "" && stored.TopicKey != topicKey {
			continue
		}
		found = append(found, stored.Clone())
	}
	return found, nil
}

// ListMemoriesByStatus is the owner's management view in this store: one status, the
// caller's own rows, creation order. Listing walks the same insertion-order slice the
// active listing walks, so the two views cannot disagree about what "oldest first" means.
func (r *InMemoryRepository) ListMemoriesByStatus(ctx context.Context, userID string, status domain.MemoryStatus) ([]domain.Memory, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if strings.TrimSpace(userID) == "" {
		return nil, ErrInvalidArgument
	}
	if !status.IsValid() {
		return nil, ErrInvalidArgument
	}

	r.mu.RLock()
	defer r.mu.RUnlock()
	r.initLocked()

	found := make([]domain.Memory, 0)
	for _, memoryID := range r.memoryOrder {
		stored := r.memories[memoryID]
		if stored.UserID != userID || stored.Status != status {
			continue
		}
		found = append(found, stored.Clone())
	}
	return found, nil
}

// DeleteMemory removes the record and its provenance list.
func (r *InMemoryRepository) DeleteMemory(ctx context.Context, userID, memoryID string) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(memoryID) == "" {
		return ErrInvalidArgument
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.initLocked()

	stored, exists := r.memories[memoryID]
	if !exists || stored.UserID != userID {
		return ErrNotFound
	}
	delete(r.memories, memoryID)
	for index, candidate := range r.memoryOrder {
		if candidate == memoryID {
			r.memoryOrder = append(r.memoryOrder[:index], r.memoryOrder[index+1:]...)
			break
		}
	}
	return nil
}

// messagesInSessionLocked reports whether every cited id is a message of this session.
// The session's ownership has already been established by the caller, so a false answer
// here means the caller listed a message that does not belong to the conversation it
// named - the caller's mistake, not a missing resource. Callers must hold the lock.
func (r *InMemoryRepository) messagesInSessionLocked(sessionID string, messageIDs []string) bool {
	if len(messageIDs) == 0 {
		return false
	}
	for _, messageID := range messageIDs {
		if owner, exists := r.messagesByID[strings.TrimSpace(messageID)]; !exists || owner != sessionID {
			return false
		}
	}
	return true
}

// provenanceIntactLocked reports whether the conversation a memory cites is still stored
// and still this user's. Callers must hold the lock.
func (r *InMemoryRepository) provenanceIntactLocked(userID, sessionID string, messageIDs []string) bool {
	session, exists := r.sessions[sessionID]
	if !exists || session.UserID != userID {
		return false
	}
	return r.messagesInSessionLocked(sessionID, messageIDs)
}

// validateMemoryListScope is the same key rule the stored record follows: a global list
// cannot be narrowed by topic, and a topic list without a key would return an arbitrary
// one of the topics the user has confirmed.
func validateMemoryListScope(scope domain.MemoryScope, topicKey string) error {
	switch scope {
	case domain.MemoryScopeGlobal:
		if topicKey != "" {
			return ErrInvalidArgument
		}
	case domain.MemoryScopeTopic:
		if topicKey == "" {
			return ErrInvalidArgument
		}
	}
	return nil
}
