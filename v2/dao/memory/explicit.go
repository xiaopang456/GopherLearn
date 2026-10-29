package memory

import (
	"GopherAI/model"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var ErrDraftNotFound = errors.New("memory draft not found")

type ExplicitWriteParams struct {
	Memory         model.LongTermMemory
	TargetMemoryID string
	RequestKey     string
	Reason         string
	Now            time.Time
	// GlobalSnapshotHash binds an out-of-transaction semantic decision to the
	// active global set that was classified. Empty disables the check for
	// legacy/topic callers; the production global service always supplies it.
	GlobalSnapshotHash string
}

type ExplicitWriteResult struct {
	Memory   model.LongTermMemory
	Revision model.MemoryRevision
	Replay   bool
}

func (r *Repository) GetExplicitWrite(ctx context.Context, userID int64, requestKey string) (*ExplicitWriteResult, error) {
	if r == nil || r.db == nil || userID <= 0 || strings.TrimSpace(requestKey) == "" {
		return nil, fmt.Errorf("invalid explicit memory replay query")
	}
	var revision model.MemoryRevision
	err := r.db.WithContext(ctx).Where("user_id = ? AND request_key = ?", userID, requestKey).First(&revision).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var memory model.LongTermMemory
	if err := r.db.WithContext(ctx).Where("id = ? AND user_id = ?", revision.MemoryID, userID).First(&memory).Error; err != nil {
		return nil, err
	}
	return &ExplicitWriteResult{Memory: memory, Revision: revision, Replay: true}, nil
}

func (r *Repository) CreateUserMemory(ctx context.Context, params ExplicitWriteParams) (*ExplicitWriteResult, error) {
	if r == nil || r.db == nil || params.Memory.UserID <= 0 || strings.TrimSpace(params.RequestKey) == "" || len([]rune(params.RequestKey)) > 128 || strings.TrimSpace(params.Reason) == "" || params.Now.IsZero() {
		return nil, fmt.Errorf("invalid explicit memory write")
	}
	if params.Memory.Scope == model.MemoryScopeGlobal {
		var result *ExplicitWriteResult
		err := withUserMemoryLock(ctx, r.db, params.Memory.UserID, func(locked *gorm.DB) error {
			var writeErr error
			result, writeErr = createUserMemoryTx(ctx, locked, params)
			return writeErr
		})
		return result, err
	}
	return createUserMemoryTx(ctx, r.db, params)
}

func createUserMemoryTx(ctx context.Context, db *gorm.DB, params ExplicitWriteParams) (*ExplicitWriteResult, error) {
	result := new(ExplicitWriteResult)
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var replay model.MemoryRevision
		err := tx.Where("user_id = ? AND request_key = ?", params.Memory.UserID, params.RequestKey).First(&replay).Error
		if err == nil {
			if replay.MemoryID == "" {
				return ErrMemoryConflict
			}
			if err := tx.Where("id = ? AND user_id = ?", replay.MemoryID, params.Memory.UserID).First(&result.Memory).Error; err != nil {
				return err
			}
			result.Revision, result.Replay = replay, true
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if params.Memory.Scope == model.MemoryScopeGlobal && params.GlobalSnapshotHash != "" {
			actual, err := loadActiveGlobalSnapshot(tx, params.Memory.UserID, params.Now, GlobalMemorySnapshotLimit)
			if err != nil {
				return err
			}
			if GlobalSnapshotFingerprint(actual) != params.GlobalSnapshotHash {
				return ErrMemoryConflict
			}
		}

		memory := params.Memory
		memory.ID = strings.TrimSpace(memory.ID)
		if memory.ID == "" {
			memory.ID = uuid.NewString()
		}
		memory.UserConfirmed = true
		memory.Status = model.LongTermMemoryActive
		memory.CreatedAt, memory.UpdatedAt = params.Now, params.Now
		memory.ContentHash = model.MemoryContentHash(memory.Content)
		if err := memory.Validate(); err != nil {
			return err
		}

		var before *model.LongTermMemory
		action := model.MemoryDreamAdd
		if targetID := strings.TrimSpace(params.TargetMemoryID); targetID != "" {
			var target model.LongTermMemory
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND user_id = ? AND status = ?", targetID, memory.UserID, model.LongTermMemoryActive).First(&target).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return ErrMemoryConflict
				}
				return err
			}
			if target.Scope != memory.Scope {
				return ErrMemoryConflict
			}
			before = &target
			action = model.MemoryDreamSupersede
			if err := tx.Model(&model.LongTermMemory{}).Where("id = ? AND user_id = ? AND status = ?", target.ID, memory.UserID, model.LongTermMemoryActive).Update("status", model.LongTermMemorySuperseded).Error; err != nil {
				return err
			}
		}
		if err := tx.Create(&memory).Error; err != nil {
			return err
		}
		requestKey := params.RequestKey
		revision := model.MemoryRevision{
			ID: uuid.NewString(), UserID: memory.UserID, MemoryID: memory.ID, Action: action, Actor: model.MemoryRevisionUser,
			RequestKey: &requestKey, Reason: strings.TrimSpace(params.Reason), BeforeSnapshot: memorySnapshot(before), AfterSnapshot: memorySnapshot(&memory),
			BeforeHash: memoryHash(before), AfterHash: memory.ContentHash, CreatedAt: params.Now,
		}
		if err := revision.Validate(); err != nil {
			return err
		}
		if err := tx.Create(&revision).Error; err != nil {
			return err
		}
		result.Memory, result.Revision = memory, revision
		return nil
	})
	return result, err
}

const GlobalMemorySnapshotLimit = 31

func GlobalSnapshotFingerprint(memories []model.LongTermMemory) string {
	rows := append([]model.LongTermMemory(nil), memories...)
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	h := sha256.New()
	for _, memory := range rows {
		validUntil := ""
		if memory.ValidUntil != nil {
			validUntil = memory.ValidUntil.UTC().Format(time.RFC3339Nano)
		}
		_, _ = fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\n", memory.ID, memory.ContentHash, memory.Status, validUntil)
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

func loadActiveGlobalSnapshot(db *gorm.DB, userID int64, now time.Time, limit int) ([]model.LongTermMemory, error) {
	var rows []model.LongTermMemory
	err := db.Where("user_id = ? AND memory_scope = ? AND status = ? AND (valid_until IS NULL OR valid_until > ?)", userID, model.MemoryScopeGlobal, model.LongTermMemoryActive, now).
		Order("id ASC").Limit(limit).Find(&rows).Error
	return rows, err
}

func (r *Repository) GetActiveByID(ctx context.Context, userID int64, memoryID string) (*model.LongTermMemory, error) {
	if r == nil || r.db == nil || userID <= 0 || strings.TrimSpace(memoryID) == "" {
		return nil, fmt.Errorf("invalid active memory query")
	}
	var memory model.LongTermMemory
	err := r.db.WithContext(ctx).Where("id = ? AND user_id = ? AND status = ?", strings.TrimSpace(memoryID), userID, model.LongTermMemoryActive).First(&memory).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrMemoryNotFound
	}
	return &memory, err
}

func (r *Repository) FindExplicitConflicts(ctx context.Context, userID int64, scope model.MemoryScope, topicKey, contentHash string, tags []string, limit int) ([]model.LongTermMemory, error) {
	if r == nil || r.db == nil || userID <= 0 || !scope.Valid() || strings.TrimSpace(contentHash) == "" || limit < 1 || limit > 20 {
		return nil, fmt.Errorf("invalid explicit conflict query")
	}
	db := r.db.WithContext(ctx).Model(&model.LongTermMemory{}).
		Where("user_id = ? AND memory_scope = ? AND status = ? AND (valid_until IS NULL OR valid_until > ?)", userID, scope, model.LongTermMemoryActive, time.Now())
	if scope == model.MemoryScopeTopic {
		db = db.Where("topic_key = ? OR content_hash = ?", strings.TrimSpace(topicKey), contentHash)
	} else if len(tags) > 0 {
		tagsJSON, err := json.Marshal(tags)
		if err != nil {
			return nil, err
		}
		db = db.Where("content_hash = ? OR JSON_OVERLAPS(tags, CAST(? AS JSON))", contentHash, string(tagsJSON))
	} else {
		db = db.Where("content_hash = ?", contentHash)
	}
	var rows []model.LongTermMemory
	err := db.Order("user_confirmed DESC, importance DESC, updated_at DESC, id ASC").Limit(limit).Find(&rows).Error
	return rows, err
}

func (r *Repository) UpsertDraft(ctx context.Context, draft *model.MemoryDraft) (*model.MemoryDraft, bool, error) {
	if r == nil || r.db == nil || draft == nil {
		return nil, false, fmt.Errorf("invalid memory draft")
	}
	if err := draft.Validate(); err != nil {
		return nil, false, err
	}
	created := false
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing model.MemoryDraft
		err := tx.Where("user_id = ? AND idempotency_key = ?", draft.UserID, draft.IdempotencyKey).First(&existing).Error
		if err == nil {
			*draft = existing
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if err := tx.Create(draft).Error; err != nil {
			if errors.Is(err, gorm.ErrDuplicatedKey) {
				return tx.Where("user_id = ? AND idempotency_key = ?", draft.UserID, draft.IdempotencyKey).First(draft).Error
			}
			return err
		}
		created = true
		return nil
	})
	return draft, created, err
}

func (r *Repository) ListDrafts(ctx context.Context, userID int64, status model.MemoryDraftStatus, limit int) ([]model.MemoryDraft, error) {
	if r == nil || r.db == nil || userID <= 0 || !status.Valid() || limit < 1 || limit > 100 {
		return nil, fmt.Errorf("invalid memory draft query")
	}
	var drafts []model.MemoryDraft
	err := r.db.WithContext(ctx).Where("user_id = ? AND status = ?", userID, status).Order("created_at DESC, id DESC").Limit(limit).Find(&drafts).Error
	return drafts, err
}

func (r *Repository) GetDraft(ctx context.Context, userID int64, draftID string) (*model.MemoryDraft, error) {
	if r == nil || r.db == nil || userID <= 0 || strings.TrimSpace(draftID) == "" {
		return nil, fmt.Errorf("invalid memory draft query")
	}
	var draft model.MemoryDraft
	err := r.db.WithContext(ctx).Where("id = ? AND user_id = ?", strings.TrimSpace(draftID), userID).First(&draft).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrDraftNotFound
	}
	return &draft, err
}

type ResolveDraftParams struct {
	UserID             int64
	DraftID            string
	Action             string
	TargetMemoryID     string
	Reason             string
	Now                time.Time
	GlobalSnapshotHash string
}

func (r *Repository) ResolveDraft(ctx context.Context, params ResolveDraftParams) (*model.MemoryDraft, *ExplicitWriteResult, error) {
	if r == nil || r.db == nil || params.UserID <= 0 || strings.TrimSpace(params.DraftID) == "" || params.Now.IsZero() {
		return nil, nil, fmt.Errorf("invalid memory draft resolution")
	}
	draft, err := r.GetDraft(ctx, params.UserID, params.DraftID)
	if err != nil {
		return nil, nil, err
	}
	if draft.Scope == model.MemoryScopeGlobal {
		var resolved *model.MemoryDraft
		var write *ExplicitWriteResult
		err = withUserMemoryLock(ctx, r.db, params.UserID, func(locked *gorm.DB) error {
			var resolveErr error
			resolved, write, resolveErr = resolveDraftTx(ctx, locked, params)
			return resolveErr
		})
		return resolved, write, err
	}
	return resolveDraftTx(ctx, r.db, params)
}

func resolveDraftTx(ctx context.Context, db *gorm.DB, params ResolveDraftParams) (*model.MemoryDraft, *ExplicitWriteResult, error) {
	var resolved model.MemoryDraft
	var write *ExplicitWriteResult
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND user_id = ?", params.DraftID, params.UserID).First(&resolved).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrDraftNotFound
			}
			return err
		}
		if resolved.Status == model.MemoryDraftConfirmed {
			var revision model.MemoryRevision
			if err := tx.Where("memory_id = ? AND user_id = ? AND request_key = ?", resolved.MemoryID, params.UserID, "draft:"+resolved.ID).First(&revision).Error; err != nil {
				return err
			}
			var memory model.LongTermMemory
			if err := tx.Where("id = ? AND user_id = ?", resolved.MemoryID, params.UserID).First(&memory).Error; err != nil {
				return err
			}
			write = &ExplicitWriteResult{Memory: memory, Revision: revision, Replay: true}
			return nil
		}
		if resolved.Status == model.MemoryDraftCancelled {
			return nil
		}
		if params.Action == "cancel" {
			resolved.Status, resolved.ResolvedAt = model.MemoryDraftCancelled, &params.Now
			return tx.Model(&model.MemoryDraft{}).Where("id = ? AND user_id = ? AND status = ?", resolved.ID, params.UserID, model.MemoryDraftPending).
				Updates(map[string]any{"status": resolved.Status, "resolved_at": params.Now}).Error
		}
		if params.Action != "add" && params.Action != "replace" {
			return fmt.Errorf("invalid memory draft action")
		}
		memory := model.LongTermMemory{
			UserID: resolved.UserID, Scope: resolved.Scope, Subject: resolved.Subject, Content: resolved.Content, StructuredJSON: `{}`,
			Tags: append(model.StringList(nil), resolved.Tags...), TopicKey: resolved.TopicKey, Durability: resolved.Durability, ValidUntil: resolved.ValidUntil,
			SourceRefs: model.StringList{"agent_run:" + resolved.RunID, "memory_draft:" + resolved.ID}, Confidence: 1, Importance: 8,
		}
		target := ""
		if params.Action == "replace" {
			target = strings.TrimSpace(params.TargetMemoryID)
			if target == "" || !stringListContains(resolved.ConflictIDs, target) {
				return ErrMemoryConflict
			}
		}
		var err error
		write, err = createUserMemoryTx(ctx, tx, ExplicitWriteParams{Memory: memory, TargetMemoryID: target, RequestKey: "draft:" + resolved.ID, Reason: params.Reason, Now: params.Now, GlobalSnapshotHash: params.GlobalSnapshotHash})
		if err != nil {
			return err
		}
		resolved.Status, resolved.MemoryID, resolved.ResolvedAt = model.MemoryDraftConfirmed, write.Memory.ID, &params.Now
		return tx.Model(&model.MemoryDraft{}).Where("id = ? AND user_id = ? AND status = ?", resolved.ID, params.UserID, model.MemoryDraftPending).
			Updates(map[string]any{"status": resolved.Status, "memory_id": resolved.MemoryID, "resolved_at": params.Now}).Error
	})
	return &resolved, write, err
}

func stringListContains(values model.StringList, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func (r *Repository) ArchiveMemory(ctx context.Context, userID int64, memoryID, reason string, now time.Time) (*model.LongTermMemory, *model.MemoryRevision, error) {
	if r == nil || r.db == nil || userID <= 0 || strings.TrimSpace(memoryID) == "" || strings.TrimSpace(reason) == "" || now.IsZero() {
		return nil, nil, fmt.Errorf("invalid memory archive")
	}
	scope, err := activeMemoryScope(ctx, r.db, userID, memoryID)
	if err != nil {
		return nil, nil, err
	}
	if scope == model.MemoryScopeGlobal {
		var archived *model.LongTermMemory
		var revision *model.MemoryRevision
		err = withUserMemoryLock(ctx, r.db, userID, func(locked *gorm.DB) error {
			var archiveErr error
			archived, revision, archiveErr = archiveMemoryTx(ctx, locked, userID, memoryID, reason, now)
			return archiveErr
		})
		return archived, revision, err
	}
	return archiveMemoryTx(ctx, r.db, userID, memoryID, reason, now)
}

func archiveMemoryTx(ctx context.Context, db *gorm.DB, userID int64, memoryID, reason string, now time.Time) (*model.LongTermMemory, *model.MemoryRevision, error) {
	var archived model.LongTermMemory
	var revision model.MemoryRevision
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND user_id = ? AND status = ?", memoryID, userID, model.LongTermMemoryActive).First(&archived).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrMemoryNotFound
			}
			return err
		}
		before := archived
		archived.Status, archived.UpdatedAt = model.LongTermMemoryArchived, now
		if err := tx.Model(&model.LongTermMemory{}).Where("id = ? AND user_id = ? AND status = ?", memoryID, userID, model.LongTermMemoryActive).Updates(map[string]any{"status": archived.Status, "updated_at": now}).Error; err != nil {
			return err
		}
		revision = model.MemoryRevision{ID: uuid.NewString(), UserID: userID, MemoryID: memoryID, Action: model.MemoryDreamArchive, Actor: model.MemoryRevisionUser, Reason: strings.TrimSpace(reason), BeforeSnapshot: memorySnapshot(&before), AfterSnapshot: memorySnapshot(&archived), BeforeHash: before.ContentHash, AfterHash: archived.ContentHash, CreatedAt: now}
		return tx.Create(&revision).Error
	})
	return &archived, &revision, err
}

func activeMemoryScope(ctx context.Context, db *gorm.DB, userID int64, memoryID string) (model.MemoryScope, error) {
	var memory model.LongTermMemory
	err := db.WithContext(ctx).Select("memory_scope").Where("id = ? AND user_id = ? AND status = ?", strings.TrimSpace(memoryID), userID, model.LongTermMemoryActive).First(&memory).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", ErrMemoryNotFound
	}
	return memory.Scope, err
}
