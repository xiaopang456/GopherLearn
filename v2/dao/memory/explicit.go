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
var ErrDraftNotPending = errors.New("memory draft is not pending")
var ErrDraftAttributionRequired = errors.New("topic memory draft requires its topic key before confirmation")

// ErrMemoryTargetModified reports that the replace target changed after the
// draft captured it: the row inside the write transaction carries a different
// ContentHash than the one the user saw and confirmed against. The draft stays
// pending, nothing is written, and the caller answers 409 with the latest row.
var ErrMemoryTargetModified = errors.New("memory replace target was modified after it was shown to the user")

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
	// ExpectedTargetHash is the ContentHash the user saw when they chose to
	// replace TargetMemoryID: either the hash carried by the confirm request
	// after the user re-checked the latest content, or the hash pinned on the
	// draft when it was proposed. Verified inside the write transaction after
	// the target row is locked, so a concurrent correction that lands before
	// the lock turns into ErrMemoryTargetModified instead of a silent
	// wrong-baseline replacement. Empty disables the check for callers that
	// never showed a version (the manual two-step create flow keeps its own
	// decision UX).
	ExpectedTargetHash string
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
			// The version the user confirmed against is checked here, inside
			// the transaction, right after the row lock and before anything
			// else can fail the write. A concurrent correction that committed
			// before this lock lands changes ContentHash under the same active
			// row; superseding it now would silently write a replacement the
			// user never saw. The specific answer is ErrMemoryTargetModified -
			// 409 with the latest row, draft stays pending, nothing written.
			if params.ExpectedTargetHash != "" && target.ContentHash != params.ExpectedTargetHash {
				return ErrMemoryTargetModified
			}
			before = &target
			action = model.MemoryDreamSupersede
		}
		// The global set fingerprint protects the entries OTHER than the
		// target: it runs after the target version check so a modified target
		// answers its specific 409 instead of a generic conflict, while a
		// change anywhere else in the active set still closes the write.
		if params.Memory.Scope == model.MemoryScopeGlobal && params.GlobalSnapshotHash != "" {
			actual, err := loadActiveGlobalSnapshot(tx, params.Memory.UserID, params.Now, GlobalMemorySnapshotLimit)
			if err != nil {
				return err
			}
			if GlobalSnapshotFingerprint(actual) != params.GlobalSnapshotHash {
				return ErrMemoryConflict
			}
		}
		if action == model.MemoryDreamSupersede {
			if err := tx.Model(&model.LongTermMemory{}).Where("id = ? AND user_id = ? AND status = ?", before.ID, memory.UserID, model.LongTermMemoryActive).Update("status", model.LongTermMemorySuperseded).Error; err != nil {
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

// FindDraftByContent returns the newest draft of one user whose content hash
// matches, optionally narrowed to one scope/topic and one source session.
//
// The comparison stays inside one scope class: a topic-scope query only
// matches drafts of the same topic key (an empty topic key matches the
// attribution-pending drafts), and a global-scope query only matches global
// drafts. statuses narrows the lifecycle stage; sourceSession, when non-empty,
// scopes the result to one origin session - the ignore-suppression key. A
// re-expression of ignored content from a different session is a new
// candidate, not an echo of the suppressed one.
func (r *Repository) FindDraftByContent(ctx context.Context, userID int64, scope model.MemoryScope, topicKey, contentHash, sourceSession string, statuses ...model.MemoryDraftStatus) (*model.MemoryDraft, error) {
	if r == nil || r.db == nil || userID <= 0 || !scope.Valid() || strings.TrimSpace(contentHash) == "" {
		return nil, fmt.Errorf("invalid memory draft content query")
	}
	db := r.db.WithContext(ctx).Where("user_id = ? AND memory_scope = ? AND content_hash = ?", userID, scope, strings.TrimSpace(contentHash))
	if scope == model.MemoryScopeTopic {
		db = db.Where("topic_key = ?", strings.TrimSpace(topicKey))
	}
	if sourceSession = strings.TrimSpace(sourceSession); sourceSession != "" {
		db = db.Where("session_id = ?", sourceSession)
	}
	if len(statuses) > 0 {
		db = db.Where("status IN ?", statuses)
	}
	var draft model.MemoryDraft
	err := db.Order("created_at DESC, id DESC").Limit(1).First(&draft).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &draft, nil
}

// DraftEditParams carries the post-recheck state of one pending draft. The
// caller recomputes conflicts and runs the semantic judgement outside the
// transaction; this transaction only re-locks the row, re-verifies that it is
// still the pending draft the caller read, and applies the result. Confirm
// remains the safety net: it verifies the pinned conflict hashes against the
// live rows before superseding anything.
type DraftEditParams struct {
	UserID         int64
	DraftID        string
	Subject        string
	Content        string
	ContentHash    string
	Tags           model.StringList
	TopicKey       string
	Durability     model.MemoryDurability
	ValidUntil     *time.Time
	ConflictIDs    model.StringList
	ConflictHashes model.StringList
	// Close ends the draft as cancelled without any user action. It is set
	// when the re-check judged the final content identical to an active
	// memory; CancelReason then records why.
	Close        *bool
	CancelReason string
	Now          time.Time
}

func (r *Repository) ApplyDraftEdit(ctx context.Context, params DraftEditParams) (*model.MemoryDraft, error) {
	if r == nil || r.db == nil || params.UserID <= 0 || strings.TrimSpace(params.DraftID) == "" || params.Now.IsZero() {
		return nil, fmt.Errorf("invalid memory draft edit")
	}
	var draft model.MemoryDraft
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND user_id = ?", strings.TrimSpace(params.DraftID), params.UserID).First(&draft).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrDraftNotFound
			}
			return err
		}
		if draft.Status != model.MemoryDraftPending {
			return ErrDraftNotPending
		}
		draft.Subject = strings.TrimSpace(params.Subject)
		draft.Content = strings.TrimSpace(params.Content)
		draft.ContentHash = strings.TrimSpace(params.ContentHash)
		draft.Tags = append(model.StringList(nil), params.Tags...)
		draft.TopicKey = strings.TrimSpace(params.TopicKey)
		draft.Durability = params.Durability
		draft.ValidUntil = params.ValidUntil
		draft.ConflictIDs = append(model.StringList(nil), params.ConflictIDs...)
		draft.ConflictHashes = append(model.StringList(nil), params.ConflictHashes...)
		draft.UpdatedAt = params.Now
		if params.Close != nil && *params.Close {
			draft.Status = model.MemoryDraftCancelled
			draft.CancelReason = strings.TrimSpace(params.CancelReason)
			draft.ResolvedAt = &params.Now
		}
		if err := draft.Validate(); err != nil {
			return err
		}
		updates := map[string]any{
			"subject": draft.Subject, "content": draft.Content, "content_hash": draft.ContentHash,
			"tags": draft.Tags, "topic_key": draft.TopicKey, "durability": draft.Durability,
			"valid_until": draft.ValidUntil, "conflict_ids": draft.ConflictIDs, "conflict_hashes": draft.ConflictHashes,
			"updated_at": draft.UpdatedAt,
		}
		if params.Close != nil && *params.Close {
			updates["status"] = draft.Status
			updates["cancel_reason"] = draft.CancelReason
			updates["resolved_at"] = draft.ResolvedAt
		}
		return tx.Model(&model.MemoryDraft{}).Where("id = ? AND user_id = ? AND status = ?", draft.ID, params.UserID, model.MemoryDraftPending).Updates(updates).Error
	})
	if err != nil {
		return nil, err
	}
	return &draft, nil
}

// FindMemoriesByIDs loads the given memories of one user regardless of
// status (deleted tombstones excluded), order not guaranteed. Used to hydrate
// the conflict details a draft card shows: a pinned entry the draft captured
// may have been superseded since, and the learner re-checking a late draft
// still needs to see its old content and save date - while the superseded row
// itself remains ineligible as a replace target.
func (r *Repository) FindMemoriesByIDs(ctx context.Context, userID int64, ids []string) ([]model.LongTermMemory, error) {
	if r == nil || r.db == nil || userID <= 0 || len(ids) == 0 {
		return nil, nil
	}
	var rows []model.LongTermMemory
	err := r.db.WithContext(ctx).
		Where("user_id = ? AND id IN ? AND status <> ?", userID, ids, model.LongTermMemoryDeleted).
		Find(&rows).Error
	return rows, err
}

// FindBySourceSession returns every memory one user's confirmed candidates
// produced from a source session, newest first. The user id is part of the
// query, so the trace-back stays inside the owner's data; superseded rows are
// included because a session trail is history, not just the current values.
func (r *Repository) FindBySourceSession(ctx context.Context, userID int64, sessionID string) ([]model.LongTermMemory, error) {
	if r == nil || r.db == nil || userID <= 0 || strings.TrimSpace(sessionID) == "" {
		return nil, fmt.Errorf("invalid memory source session query")
	}
	var rows []model.LongTermMemory
	err := r.db.WithContext(ctx).
		Where("user_id = ? AND source_session = ?", userID, strings.TrimSpace(sessionID)).
		Order("created_at DESC, id DESC").Find(&rows).Error
	return rows, err
}

type ResolveDraftParams struct {
	UserID             int64
	DraftID            string
	Action             string
	TargetMemoryID     string
	Reason             string
	Now                time.Time
	GlobalSnapshotHash string
	// ExpectedTargetHash carries the version the user re-confirmed against on
	// the replace path. Empty falls back to the hash pinned on the draft.
	ExpectedTargetHash string
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
		if resolved.Scope == model.MemoryScopeTopic && strings.TrimSpace(resolved.TopicKey) == "" {
			return ErrDraftAttributionRequired
		}
		memory := model.LongTermMemory{
			UserID: resolved.UserID, Scope: resolved.Scope, Subject: resolved.Subject, Content: resolved.Content, StructuredJSON: `{}`,
			Tags: append(model.StringList(nil), resolved.Tags...), TopicKey: resolved.TopicKey, Durability: resolved.Durability, ValidUntil: resolved.ValidUntil,
			SourceRefs: model.StringList{"agent_run:" + resolved.RunID, "memory_draft:" + resolved.ID}, Confidence: 1, Importance: 8,
			// 自动候选来源契约：确认时把候选上的来源字段随正式记忆保留。
			ProfileClass: resolved.ProfileClass, EvidenceMessages: append(model.StringList(nil), resolved.EvidenceMessages...),
			AssistantMessages: append(model.StringList(nil), resolved.AssistantMessages...), StatementTime: resolved.StatementTime,
			SourceSession: resolved.SessionID,
		}
		target := ""
		expectedHash := ""
		if params.Action == "replace" {
			target = strings.TrimSpace(params.TargetMemoryID)
			if target == "" || !stringListContains(resolved.ConflictIDs, target) {
				return ErrMemoryConflict
			}
			// The version the user saw: the hash carried by the confirm request
			// after they re-checked the latest content, or the hash pinned when
			// the draft captured the conflict. Verified inside the write
			// transaction right after the target row is locked.
			expectedHash = strings.TrimSpace(params.ExpectedTargetHash)
			if expectedHash == "" {
				expectedHash = PinnedConflictHash(resolved, target)
			}
		}
		var err error
		write, err = createUserMemoryTx(ctx, tx, ExplicitWriteParams{Memory: memory, TargetMemoryID: target, RequestKey: "draft:" + resolved.ID, Reason: params.Reason, Now: params.Now, GlobalSnapshotHash: params.GlobalSnapshotHash, ExpectedTargetHash: expectedHash})
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

// PinnedConflictHash returns the ContentHash the draft pinned for the given
// conflict target when it was proposed or last edited. Empty when the draft
// predates hash pinning or the pair drifted out of alignment.
func PinnedConflictHash(draft model.MemoryDraft, targetID string) string {
	for i, id := range draft.ConflictIDs {
		if id == targetID && i < len(draft.ConflictHashes) {
			return strings.TrimSpace(draft.ConflictHashes[i])
		}
	}
	return ""
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
