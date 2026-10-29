package gormmysql

import (
	"context"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/repository"
)

// Long-term memory on the production store. Everything that changes a memory's state is
// one transaction, because the interesting failures are all partial ones: a draft made
// active while its predecessor is still active leaves two answers to one question, and a
// memory stored without its provenance rows claims evidence it cannot show.

// orderByMemoryCreation is the memory listing order: oldest confirmed fact first, which is
// the order a reader expects a person's history to arrive in. The id is the tie-breaker
// because generated ids carry a fixed-width nanosecond timestamp, so two memories written
// in the same microsecond still have one defined order on every engine.
const orderByMemoryCreation = "created_at ASC, memory_id ASC"

// CreateMemoryDraft stores a candidate as a pending memory with its ordered provenance.
func (r *Repository) CreateMemoryDraft(ctx context.Context, draft *domain.MemoryDraft) (*domain.Memory, error) {
	memory, _, err := r.storeMemoryDraft(ctx, draft, false)
	return memory, err
}

// CreateMemoryDraftOnce answers with an existing pending row when one already makes the
// same claim, and inserts only when it does not.
func (r *Repository) CreateMemoryDraftOnce(ctx context.Context, draft *domain.MemoryDraft) (*domain.Memory, bool, error) {
	return r.storeMemoryDraft(ctx, draft, true)
}

// storeMemoryDraft is the one path that writes a pending row here, so the duplicate search
// and the insert share a transaction: two callers proposing the same thing cannot each read
// "not there yet" and each insert. Everything else about the write is unchanged, including
// the provenance checks, because an idempotent path has no business being looser.
func (r *Repository) storeMemoryDraft(
	ctx context.Context, draft *domain.MemoryDraft, reuseEquivalent bool,
) (*domain.Memory, bool, error) {
	if err := checkContext(ctx); err != nil {
		return nil, false, err
	}
	if draft == nil {
		return nil, false, repository.ErrInvalidArgument
	}
	if err := draft.Validate(); err != nil {
		return nil, false, repository.ErrInvalidArgument
	}

	memory := draft.StoredMemory()
	memory.MemoryID = newID("mem")
	now := time.Now().UTC()
	memory.CreatedAt, memory.UpdatedAt = now, now
	record := MemoryRecord{
		MemoryID:        memory.MemoryID,
		UserID:          memory.UserID,
		Scope:           string(memory.Scope),
		TopicKey:        memory.TopicKey,
		Status:          string(memory.Status),
		Content:         memory.Content,
		Origin:          string(memory.Origin),
		Confidence:      memory.Confidence,
		SourceSessionID: memory.SourceSessionID,
		CreatedAt:       now,
		UpdatedAt:       now,
	}

	var reused *domain.Memory
	err := r.transaction(ctx, func(tx *gorm.DB) error {
		if err := requireUser(tx, record.UserID); err != nil {
			return err
		}
		if err := requireOwnedSession(tx, record.UserID, record.SourceSessionID); err != nil {
			return err
		}
		if err := requireSessionMessages(tx, record.SourceSessionID, memory.SourceMessageIDs); err != nil {
			return err
		}
		if reuseEquivalent {
			existing, err := matchingPendingDraft(tx, memory)
			if err != nil {
				return err
			}
			if existing != nil {
				reused = existing
				return nil
			}
		}
		if err := tx.Create(&record).Error; err != nil {
			return err
		}
		sources := memorySourceRecords(record.MemoryID, memory.SourceMessageIDs)
		if len(sources) == 0 {
			return nil
		}
		return tx.Create(&sources).Error
	})
	if err != nil {
		return nil, false, translate(err)
	}
	if reused != nil {
		return reused, false, nil
	}
	stored := memory.Clone()
	return &stored, true, nil
}

// matchingPendingDraft looks for a pending row of this user that makes the same claim. The
// SQL narrows by the columns that can be indexed, and the cited message set is compared in
// Go because citation order is part of the identity and no index says anything about it.
func matchingPendingDraft(tx *gorm.DB, memory domain.Memory) (*domain.Memory, error) {
	var candidates []MemoryRecord
	if err := tx.Where("user_id = ? AND scope = ? AND topic_key = ? AND content = ? AND source_session_id = ? AND status = ?",
		memory.UserID, string(memory.Scope), memory.TopicKey, memory.Content, memory.SourceSessionID,
		string(domain.MemoryStatusPending)).
		Order(orderByMemoryCreation).Find(&candidates).Error; err != nil {
		return nil, err
	}
	for _, candidate := range candidates {
		ids, err := memorySourceIDs(tx, candidate.MemoryID)
		if err != nil {
			return nil, err
		}
		stored, err := candidate.toDomain(ids)
		if err != nil {
			return nil, err
		}
		if stored.ProposalsMatch(memory) {
			clone := stored.Clone()
			return &clone, nil
		}
	}
	return nil, nil
}

// StoreUserAuthoredMemory writes a hand-stated memory as an active row and retires the
// previous active version of the same subject in the same transaction.
//
// One transaction is the contract: if the retirement fails, the insert goes with it, so a
// failed request cannot leave two active answers to one question, nor a new fact nobody can
// see. The retirement runs before the insert, so it needs no "not this id" guard.
func (r *Repository) StoreUserAuthoredMemory(ctx context.Context, authored *domain.UserAuthoredMemory) (*domain.Memory, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if authored == nil {
		return nil, repository.ErrInvalidArgument
	}
	if err := authored.Validate(); err != nil {
		return nil, repository.ErrInvalidArgument
	}

	memory := authored.StoredMemory()
	memory.MemoryID = newID("mem")
	now := time.Now().UTC()
	memory.CreatedAt, memory.UpdatedAt = now, now
	record := MemoryRecord{
		MemoryID:   memory.MemoryID,
		UserID:     memory.UserID,
		Scope:      string(memory.Scope),
		TopicKey:   memory.TopicKey,
		Status:     string(memory.Status),
		Origin:     string(memory.Origin),
		Content:    memory.Content,
		Confidence: memory.Confidence,
		CreatedAt:  now,
		UpdatedAt:  now,
	}

	err := r.transaction(ctx, func(tx *gorm.DB) error {
		if err := requireUser(tx, record.UserID); err != nil {
			return err
		}
		if memory.Scope.SupersedesSiblings() {
			retired := time.Now().UTC()
			if err := tx.Model(&MemoryRecord{}).
				Where("user_id = ? AND scope = ? AND topic_key = ? AND status = ?",
					record.UserID, record.Scope, record.TopicKey, string(domain.MemoryStatusActive)).
				Updates(map[string]any{
					"status":        string(domain.MemoryStatusSuperseded),
					"superseded_at": retired,
					"updated_at":    retired,
				}).Error; err != nil {
				return err
			}
		}
		// No memory_sources rows: this record cites no conversation. An empty child-table
		// entry would read back as "the evidence was lost" rather than "there was never
		// any to have".
		return tx.Create(&record).Error
	})
	if err != nil {
		return nil, translate(err)
	}
	stored := memory.Clone()
	return &stored, nil
}

// GetMemory reads one of the user's memories with its ordered provenance. A memory owned
// by somebody else and a memory that does not exist are the same answer.
func (r *Repository) GetMemory(ctx context.Context, userID, memoryID string) (*domain.Memory, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(memoryID) == "" {
		return nil, repository.ErrInvalidArgument
	}

	var record MemoryRecord
	err := r.transaction(ctx, func(tx *gorm.DB) error {
		return tx.Where("memory_id = ? AND user_id = ?", memoryID, userID).Take(&record).Error
	})
	if err != nil {
		return nil, translate(err)
	}
	ids, err := r.messageIDsFor(ctx, record.MemoryID)
	if err != nil {
		return nil, translate(err)
	}
	memory, err := record.toDomain(ids)
	if err != nil {
		return nil, err
	}
	return &memory, nil
}

// ConfirmMemoryDraft activates one pending draft and supersedes whatever was active for
// the same subject, in the same transaction.
//
// The status change is a conditional update - `WHERE status = 'pending'` - so a second
// confirmation cannot take effect even if two readers saw the draft as pending at the same
// time. The loser finds zero rows affected, re-reads the row, and is told which state it
// actually met.
func (r *Repository) ConfirmMemoryDraft(ctx context.Context, userID, memoryID string) (*domain.Memory, error) {
	return r.transitionMemory(ctx, userID, memoryID, domain.MemoryStatusPending, domain.MemoryStatusActive)
}

// RejectMemoryDraft retires a pending draft without deleting it.
func (r *Repository) RejectMemoryDraft(ctx context.Context, userID, memoryID string) (*domain.Memory, error) {
	return r.transitionMemory(ctx, userID, memoryID, domain.MemoryStatusPending, domain.MemoryStatusArchived)
}

// ArchiveMemory retires an active memory.
func (r *Repository) ArchiveMemory(ctx context.Context, userID, memoryID string) (*domain.Memory, error) {
	return r.transitionMemory(ctx, userID, memoryID, domain.MemoryStatusActive, domain.MemoryStatusArchived)
}

func (r *Repository) transitionMemory(
	ctx context.Context, userID, memoryID string, requireFrom, target domain.MemoryStatus,
) (*domain.Memory, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(memoryID) == "" {
		return nil, repository.ErrInvalidArgument
	}
	if !requireFrom.CanTransitionTo(target) {
		return nil, repository.ErrInvalidArgument
	}

	var changed MemoryRecord
	err := r.transaction(ctx, func(tx *gorm.DB) error {
		var current MemoryRecord
		if err := tx.Where("memory_id = ? AND user_id = ?", memoryID, userID).Take(&current).Error; err != nil {
			return err
		}
		if current.Status != string(requireFrom) {
			return &repository.MemoryStateConflict{MemoryID: memoryID, Status: domain.MemoryStatus(current.Status)}
		}
		if target == domain.MemoryStatusActive {
			sources, err := memorySourceIDs(tx, memoryID)
			if err != nil {
				return err
			}
			if err := requireMemoryProvenance(tx, current.UserID, current.SourceSessionID, sources); err != nil {
				return err
			}
		}
		// The status test above is only a nicer error message: the write below is what
		// makes it true, because another transaction can move the row in between.
		result := tx.Model(&MemoryRecord{}).
			Where("memory_id = ? AND user_id = ? AND status = ?", memoryID, userID, string(requireFrom)).
			Updates(map[string]any{
				"status":     string(target),
				"updated_at": time.Now().UTC(),
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			var raced MemoryRecord
			if err := tx.Where("memory_id = ? AND user_id = ?", memoryID, userID).Take(&raced).Error; err != nil {
				return err
			}
			return &repository.MemoryStateConflict{MemoryID: memoryID, Status: domain.MemoryStatus(raced.Status)}
		}
		if target == domain.MemoryStatusActive && domain.MemoryScope(current.Scope).SupersedesSiblings() {
			// Exactly one active memory per (user, scope, topic_key). This runs after the
			// new one is active and excludes it by id, so a single statement carries the
			// whole version transition: if it fails, the transaction rolls the promotion
			// back too and no half-versioned pair is left behind.
			supersededAt := time.Now().UTC()
			if err := tx.Model(&MemoryRecord{}).
				Where("user_id = ? AND scope = ? AND topic_key = ? AND status = ? AND memory_id <> ?",
					userID, current.Scope, current.TopicKey, string(domain.MemoryStatusActive), memoryID).
				Updates(map[string]any{
					"status":        string(domain.MemoryStatusSuperseded),
					"superseded_at": supersededAt,
					"updated_at":    supersededAt,
				}).Error; err != nil {
				return err
			}
		}
		return tx.Where("memory_id = ?", memoryID).Take(&changed).Error
	})
	if err != nil {
		return nil, translate(err)
	}
	ids, err := r.messageIDsFor(ctx, changed.MemoryID)
	if err != nil {
		return nil, translate(err)
	}
	memory, err := changed.toDomain(ids)
	if err != nil {
		return nil, err
	}
	return &memory, nil
}

// ListActiveMemories returns confirmed memories only. Pending, superseded and archived
// rows are filtered in the query rather than by the caller, so no reader has to remember
// the rule to obey it.
func (r *Repository) ListActiveMemories(ctx context.Context, userID string, scope domain.MemoryScope, topicKey string) ([]domain.Memory, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if strings.TrimSpace(userID) == "" {
		return nil, repository.ErrInvalidArgument
	}
	if !scope.IsValid() {
		return nil, repository.ErrInvalidArgument
	}
	topicKey = strings.TrimSpace(topicKey)
	switch scope {
	case domain.MemoryScopeGlobal:
		if topicKey != "" {
			return nil, repository.ErrInvalidArgument
		}
	case domain.MemoryScopeTopic:
		if topicKey == "" {
			return nil, repository.ErrInvalidArgument
		}
	}

	var records []MemoryRecord
	err := r.transaction(ctx, func(tx *gorm.DB) error {
		query := tx.Where("user_id = ? AND scope = ? AND status = ?",
			userID, string(scope), string(domain.MemoryStatusActive))
		if topicKey != "" {
			query = query.Where("topic_key = ?", topicKey)
		}
		return query.Order(orderByMemoryCreation).Find(&records).Error
	})
	if err != nil {
		return nil, translate(err)
	}
	return r.memoriesFromRecords(ctx, records)
}

// ListMemoriesByStatus is the owner's management view: one status, the caller's own rows,
// every scope at once. A topic memory cannot be enumerated through ListActiveMemories
// without its key, which is exactly why a confirmation surface needs this read - and why
// it stays separate from the prompt-facing listing, so no one mistakes the wider read for
// a recall path with the read-side grants applied.
func (r *Repository) ListMemoriesByStatus(ctx context.Context, userID string, status domain.MemoryStatus) ([]domain.Memory, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if strings.TrimSpace(userID) == "" {
		return nil, repository.ErrInvalidArgument
	}
	if !status.IsValid() {
		return nil, repository.ErrInvalidArgument
	}

	var records []MemoryRecord
	err := r.transaction(ctx, func(tx *gorm.DB) error {
		return tx.Where("user_id = ? AND status = ?", userID, string(status)).
			Order(orderByMemoryCreation).Find(&records).Error
	})
	if err != nil {
		return nil, translate(err)
	}
	return r.memoriesFromRecords(ctx, records)
}

// memoriesFromRecords attaches each row's provenance in one batched read and keeps the
// query's creation order. The provenance ids are read separately because they live in
// their own table; sorting them by position is what makes citation order survive a restart.
func (r *Repository) memoriesFromRecords(ctx context.Context, records []MemoryRecord) ([]domain.Memory, error) {
	if len(records) == 0 {
		return []domain.Memory{}, nil
	}
	ids := make([]string, 0, len(records))
	for _, record := range records {
		ids = append(ids, record.MemoryID)
	}
	grouped, err := r.messageIDsByMemory(ctx, ids)
	if err != nil {
		return nil, translate(err)
	}
	found := make([]domain.Memory, 0, len(records))
	for _, record := range records {
		memory, err := record.toDomain(grouped[record.MemoryID])
		if err != nil {
			return nil, err
		}
		found = append(found, memory)
	}
	return found, nil
}

// DeleteMemory removes a memory and its provenance rows in one transaction. It does not
// touch the sessions or messages it cited: provenance is a pointer to conversation, not a
// copy of it, and the conversation outlives any single memory about it.
func (r *Repository) DeleteMemory(ctx context.Context, userID, memoryID string) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(memoryID) == "" {
		return repository.ErrInvalidArgument
	}
	err := r.transaction(ctx, func(tx *gorm.DB) error {
		result := tx.Where("memory_id = ? AND user_id = ?", memoryID, userID).Delete(&MemoryRecord{})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return repository.ErrNotFound
		}
		return tx.Where("memory_id = ?", memoryID).Delete(&MemorySourceRecord{}).Error
	})
	return translate(err)
}

// memorySourceIDs reads one memory's cited messages back in citation order.
func memorySourceIDs(tx *gorm.DB, memoryID string) ([]string, error) {
	var ids []string
	if err := tx.Model(&MemorySourceRecord{}).
		Where("memory_id = ?", memoryID).
		Order("position ASC").
		Pluck("message_id", &ids).Error; err != nil {
		return nil, err
	}
	return ids, nil
}

// requireMemoryProvenance refuses a confirmation whose cited conversation is gone. Creating
// a draft checked this once; the check matters at confirmation, because that is the moment
// the claim stops being a proposal, and a session can be deleted in between.
func requireMemoryProvenance(tx *gorm.DB, userID, sessionID string, messageIDs []string) error {
	var owned int64
	if err := tx.Model(&SessionRecord{}).
		Where("id = ? AND user_id = ?", sessionID, userID).
		Count(&owned).Error; err != nil {
		return err
	}
	if owned == 0 {
		return repository.ErrMemoryProvenanceMissing
	}
	intact, err := sessionMessagesFound(tx, sessionID, messageIDs)
	if err != nil {
		return err
	}
	if !intact {
		return repository.ErrMemoryProvenanceMissing
	}
	return nil
}

// requireSessionMessages rejects a draft whose cited messages are not messages of the
// session it names. The session itself has already been shown to belong to the caller, so
// the only way to reach this is a wrong list, and a wrong list is the caller's mistake
// rather than a missing resource.
func requireSessionMessages(tx *gorm.DB, sessionID string, messageIDs []string) error {
	intact, err := sessionMessagesFound(tx, sessionID, messageIDs)
	if err != nil {
		return err
	}
	if !intact {
		return repository.ErrInvalidArgument
	}
	return nil
}

// sessionMessagesFound reports whether every id is a message of this session. Both the
// write-time and confirm-time checks go through here so they cannot drift apart.
func sessionMessagesFound(tx *gorm.DB, sessionID string, messageIDs []string) (bool, error) {
	if len(messageIDs) == 0 {
		return false, nil
	}
	var found int64
	if err := tx.Model(&MessageRecord{}).
		Where("session_id = ? AND id IN ?", sessionID, messageIDs).
		Count(&found).Error; err != nil {
		return false, err
	}
	return found == int64(len(messageIDs)), nil
}

func (r *Repository) messageIDsFor(ctx context.Context, memoryID string) ([]string, error) {
	return memorySourceIDs(r.db.WithContext(ctx), memoryID)
}

// messageIDsByMemory loads the provenance of many memories in one query and regroups it by
// memory, so listing memories costs two statements rather than one per row. Position
// ordering is preserved because the rows arrive ordered by (memory_id, position).
func (r *Repository) messageIDsByMemory(ctx context.Context, memoryIDs []string) (map[string][]string, error) {
	var records []MemorySourceRecord
	err := r.db.WithContext(ctx).
		Where("memory_id IN ?", memoryIDs).
		Order("memory_id ASC, position ASC").
		Find(&records).Error
	if err != nil {
		return nil, err
	}
	grouped := make(map[string][]string, len(memoryIDs))
	for _, record := range records {
		grouped[record.MemoryID] = append(grouped[record.MemoryID], record.MessageID)
	}
	return grouped, nil
}
