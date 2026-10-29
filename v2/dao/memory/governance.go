package memory

import (
	"GopherAI/model"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrCandidateNotFound = errors.New("memory candidate not found")
	ErrDreamBusy         = errors.New("memory candidate is being governed")
	ErrDreamConflict     = errors.New("memory dream state conflict")
	ErrMemoryNotFound    = errors.New("long-term memory not found")
	ErrMemoryConflict    = errors.New("long-term memory state conflict")
	// AGENT-018 retired the four original memory types. Rows written before the
	// migration are kept so no user content is lost, but they are frozen: not
	// recallable, not correctable. Correcting one would have to re-validate it
	// under the new shape rules, which it cannot satisfy.
	ErrDerivedMemory = errors.New("memory of a retired type is read-only and cannot be corrected")
)

type DreamClaim struct {
	Candidate model.MemoryCandidate
	Job       model.MemoryExtractionJob
	Run       model.MemoryDreamRun
	Replay    bool
}

// RecoverExpiredDreams returns abandoned processing candidates to pending.
// It uses StartedAt as an implicit lease boundary and locks Candidate before
// DreamRun, matching ApplyDream/FailDream lock order.
func (r *Repository) RecoverExpiredDreams(ctx context.Context, userID int64, now time.Time, lease time.Duration) (int64, error) {
	if r == nil || r.db == nil || userID <= 0 || now.IsZero() || lease <= 0 {
		return 0, fmt.Errorf("invalid dream recovery")
	}
	var recovered int64
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var candidates []model.MemoryCandidate
		if err := tx.Where("user_id = ? AND status = ? AND dream_run_id <> ''", userID, model.MemoryCandidateProcessing).
			Order("created_at ASC, id ASC").Limit(100).Find(&candidates).Error; err != nil {
			return err
		}
		cutoff := now.Add(-lease)
		for _, item := range candidates {
			var candidate model.MemoryCandidate
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Where("id = ? AND user_id = ? AND status = ? AND dream_run_id = ?", item.ID, userID, model.MemoryCandidateProcessing, item.DreamRunID).
				First(&candidate).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					continue
				}
				return err
			}
			var run model.MemoryDreamRun
			err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Where("id = ? AND user_id = ? AND candidate_id = ? AND status = ?", candidate.DreamRunID, userID, candidate.ID, model.MemoryDreamRunProcessing).
				First(&run).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				if result := tx.Model(&model.MemoryCandidate{}).
					Where("id = ? AND user_id = ? AND status = ? AND dream_run_id = ?", candidate.ID, userID, model.MemoryCandidateProcessing, candidate.DreamRunID).
					Updates(map[string]any{"status": model.MemoryCandidatePending, "dream_run_id": ""}); result.Error != nil {
					return result.Error
				} else if result.RowsAffected == 1 {
					recovered++
				}
				continue
			}
			if err != nil {
				return err
			}
			if run.StartedAt.After(cutoff) {
				continue
			}
			candidateResult := tx.Model(&model.MemoryCandidate{}).
				Where("id = ? AND user_id = ? AND status = ? AND dream_run_id = ?", candidate.ID, userID, model.MemoryCandidateProcessing, run.ID).
				Updates(map[string]any{"status": model.MemoryCandidatePending, "dream_run_id": ""})
			if candidateResult.Error != nil {
				return candidateResult.Error
			}
			if candidateResult.RowsAffected != 1 {
				continue
			}
			runResult := tx.Model(&model.MemoryDreamRun{}).
				Where("id = ? AND user_id = ? AND candidate_id = ? AND status = ?", run.ID, userID, candidate.ID, model.MemoryDreamRunProcessing).
				Updates(map[string]any{"status": model.MemoryDreamRunFailed, "error_code": "dream_lease_expired", "completed_at": now})
			if runResult.Error != nil {
				return runResult.Error
			}
			if runResult.RowsAffected != 1 {
				return ErrDreamConflict
			}
			recovered++
		}
		return nil
	})
	return recovered, err
}

func (r *Repository) ClaimCandidate(ctx context.Context, userID int64, candidateID string, now time.Time) (*DreamClaim, error) {
	if r == nil || r.db == nil || userID <= 0 || now.IsZero() {
		return nil, fmt.Errorf("invalid memory dream claim")
	}
	claim := new(DreamClaim)
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		query := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("user_id = ?", userID)
		if strings.TrimSpace(candidateID) == "" {
			query = query.Where("status = ?", model.MemoryCandidatePending).Order("created_at ASC, id ASC")
		} else {
			query = query.Where("id = ?", strings.TrimSpace(candidateID))
		}
		if err := query.First(&claim.Candidate).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrCandidateNotFound
			}
			return err
		}
		if claim.Candidate.ContentHash == "" {
			claim.Candidate.ContentHash = model.MemoryContentHash(claim.Candidate.ProposedContent)
			if err := tx.Model(&model.MemoryCandidate{}).Where("id = ? AND user_id = ?", claim.Candidate.ID, userID).Update("content_hash", claim.Candidate.ContentHash).Error; err != nil {
				return err
			}
		}
		if err := tx.Where("id = ? AND user_id = ?", claim.Candidate.JobID, userID).First(&claim.Job).Error; err != nil {
			return err
		}
		switch claim.Candidate.Status {
		case model.MemoryCandidateApplied, model.MemoryCandidateRejected:
			if err := tx.Where("candidate_id = ? AND user_id = ? AND status = ?", claim.Candidate.ID, userID, model.MemoryDreamRunCompleted).
				Order("completed_at DESC, created_at DESC").First(&claim.Run).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return ErrDreamConflict
				}
				return err
			}
			claim.Replay = true
			return nil
		case model.MemoryCandidateProcessing:
			return ErrDreamBusy
		case model.MemoryCandidatePending:
		default:
			return ErrDreamConflict
		}
		claim.Run = model.MemoryDreamRun{
			ID: uuid.NewString(), UserID: userID, CandidateID: claim.Candidate.ID,
			Status: model.MemoryDreamRunProcessing, StartedAt: now,
		}
		if err := claim.Run.ValidateForCreate(); err != nil {
			return err
		}
		result := tx.Model(&model.MemoryCandidate{}).
			Where("id = ? AND user_id = ? AND status = ?", claim.Candidate.ID, userID, model.MemoryCandidatePending).
			Updates(map[string]any{"status": model.MemoryCandidateProcessing, "dream_run_id": claim.Run.ID, "rejection_reason": ""})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrDreamConflict
		}
		return tx.Create(&claim.Run).Error
	})
	if err != nil {
		return nil, err
	}
	return claim, nil
}

func (r *Repository) ListComparable(ctx context.Context, userID int64, scope model.MemoryScope, tags []string, contentHash string, limit int) ([]model.LongTermMemory, error) {
	if r == nil || r.db == nil || userID <= 0 || !scope.Valid() || len(tags) > 5 || strings.TrimSpace(contentHash) == "" || limit < 1 || limit > 20 {
		return nil, fmt.Errorf("invalid comparable memory query")
	}
	tagsJSON, err := json.Marshal(tags)
	if err != nil {
		return nil, err
	}
	var memories []model.LongTermMemory
	db := r.db.WithContext(ctx).
		Where("user_id = ? AND memory_scope = ? AND status = ?", userID, scope, model.LongTermMemoryActive).
		Where("(valid_until IS NULL OR valid_until > ?)", time.Now())
	if len(tags) == 0 {
		db = db.Where("(content_hash = ? OR origin_content_hash = ?)", contentHash, contentHash)
	} else {
		db = db.Where("(JSON_OVERLAPS(tags, CAST(? AS JSON)) OR content_hash = ? OR origin_content_hash = ?)", string(tagsJSON), contentHash, contentHash)
	}
	err = db.
		Order("user_confirmed DESC, confidence DESC, importance DESC, updated_at DESC, id ASC").
		Limit(limit).Find(&memories).Error
	return memories, err
}

func (r *Repository) HasDeletedHash(ctx context.Context, userID int64, memoryType model.MemoryScope, contentHash string) (bool, error) {
	if r == nil || r.db == nil || userID <= 0 || !memoryType.Valid() || strings.TrimSpace(contentHash) == "" {
		return false, fmt.Errorf("invalid memory tombstone query")
	}
	var count int64
	err := r.db.WithContext(ctx).Model(&model.LongTermMemory{}).
		Where("user_id = ? AND memory_scope = ? AND status = ? AND (tombstone_hash = ? OR origin_content_hash = ?)", userID, memoryType, model.LongTermMemoryDeleted, contentHash, contentHash).
		Count(&count).Error
	return count > 0, err
}

type ApplyDreamParams struct {
	UserID             int64
	CandidateID        string
	DreamRunID         string
	Action             model.MemoryDreamAction
	TargetMemoryID     string
	ExpectedTargetHash string
	AllowUserConfirmed bool
	ResultMemory       *model.LongTermMemory
	Reason             string
	Now                time.Time
}

type DreamApplyResult struct {
	Run      model.MemoryDreamRun
	Memory   *model.LongTermMemory
	Revision model.MemoryRevision
}

func (r *Repository) ApplyDream(ctx context.Context, params ApplyDreamParams) (*DreamApplyResult, error) {
	if r == nil || r.db == nil || params.UserID <= 0 || strings.TrimSpace(params.CandidateID) == "" || strings.TrimSpace(params.DreamRunID) == "" || !params.Action.Valid() || params.Now.IsZero() || strings.TrimSpace(params.Reason) == "" {
		return nil, fmt.Errorf("invalid memory dream application")
	}
	if params.Action == model.MemoryUserCorrect || params.Action == model.MemoryUserDelete {
		return nil, fmt.Errorf("user action cannot be applied as dream")
	}
	result := new(DreamApplyResult)
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var candidate model.MemoryCandidate
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND user_id = ? AND status = ? AND dream_run_id = ?", params.CandidateID, params.UserID, model.MemoryCandidateProcessing, params.DreamRunID).
			First(&candidate).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrDreamConflict
			}
			return err
		}
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND user_id = ? AND candidate_id = ? AND status = ?", params.DreamRunID, params.UserID, params.CandidateID, model.MemoryDreamRunProcessing).
			First(&result.Run).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrDreamConflict
			}
			return err
		}

		var before, after *model.LongTermMemory
		loadTarget := func() (*model.LongTermMemory, error) {
			if strings.TrimSpace(params.TargetMemoryID) == "" {
				return nil, ErrMemoryNotFound
			}
			var target model.LongTermMemory
			err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Where("id = ? AND user_id = ? AND status = ?", params.TargetMemoryID, params.UserID, model.LongTermMemoryActive).
				First(&target).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, ErrMemoryConflict
			}
			if err != nil {
				return nil, err
			}
			if params.ExpectedTargetHash != "" && target.ContentHash != params.ExpectedTargetHash {
				return nil, ErrMemoryConflict
			}
			if target.UserConfirmed && !params.AllowUserConfirmed {
				return nil, ErrMemoryConflict
			}
			return &target, nil
		}

		switch params.Action {
		case model.MemoryDreamAdd:
			if params.ResultMemory == nil {
				return fmt.Errorf("add result memory is required")
			}
			if err := validateDreamResult(params.ResultMemory, params.UserID, params.CandidateID, ""); err != nil {
				return err
			}
			var duplicate int64
			if err := tx.Model(&model.LongTermMemory{}).
				Where("user_id = ? AND memory_scope = ? AND status = ? AND (valid_until IS NULL OR valid_until > ?) AND (content_hash = ? OR origin_content_hash = ?)", params.UserID, params.ResultMemory.Scope, model.LongTermMemoryActive, params.Now, params.ResultMemory.ContentHash, params.ResultMemory.OriginContentHash).
				Count(&duplicate).Error; err != nil {
				return err
			}
			if duplicate > 0 {
				return ErrMemoryConflict
			}
			if err := tx.Create(params.ResultMemory).Error; err != nil {
				return err
			}
			after = params.ResultMemory
		case model.MemoryDreamMerge:
			target, err := loadTarget()
			if err != nil {
				return err
			}
			before = target
			if params.ResultMemory == nil || params.ResultMemory.ID != target.ID {
				return fmt.Errorf("merge result must retain target identity")
			}
			if err := validateDreamResult(params.ResultMemory, params.UserID, "", target.ID); err != nil {
				return err
			}
			if err := updateMemory(tx, params.ResultMemory); err != nil {
				return err
			}
			after = params.ResultMemory
		case model.MemoryDreamSupersede:
			target, err := loadTarget()
			if err != nil {
				return err
			}
			before = target
			if params.ResultMemory == nil {
				return fmt.Errorf("supersede result memory is required")
			}
			if err := validateDreamResult(params.ResultMemory, params.UserID, params.CandidateID, ""); err != nil {
				return err
			}
			if err := tx.Model(&model.LongTermMemory{}).
				Where("id = ? AND user_id = ? AND status = ?", target.ID, params.UserID, model.LongTermMemoryActive).
				Update("status", model.LongTermMemorySuperseded).Error; err != nil {
				return err
			}
			if err := tx.Create(params.ResultMemory).Error; err != nil {
				return err
			}
			after = params.ResultMemory
		case model.MemoryDreamArchive:
			target, err := loadTarget()
			if err != nil {
				return err
			}
			before = target
			archived := *target
			archived.Status = model.LongTermMemoryArchived
			if err := tx.Model(&model.LongTermMemory{}).
				Where("id = ? AND user_id = ? AND status = ?", target.ID, params.UserID, model.LongTermMemoryActive).
				Update("status", model.LongTermMemoryArchived).Error; err != nil {
				return err
			}
			after = &archived
		case model.MemoryDreamReject:
		default:
			return fmt.Errorf("unsupported dream action")
		}

		memoryID := ""
		if after != nil {
			memoryID = after.ID
		} else if before != nil {
			memoryID = before.ID
		}
		result.Revision = model.MemoryRevision{
			ID: uuid.NewString(), UserID: params.UserID, MemoryID: memoryID, CandidateID: params.CandidateID,
			DreamRunID: params.DreamRunID, Action: params.Action, Actor: model.MemoryRevisionDream,
			Reason: strings.TrimSpace(params.Reason), BeforeSnapshot: memorySnapshot(before), AfterSnapshot: memorySnapshot(after),
			BeforeHash: memoryHash(before), AfterHash: memoryHash(after), CreatedAt: params.Now,
		}
		if err := result.Revision.Validate(); err != nil {
			return err
		}
		if err := tx.Create(&result.Revision).Error; err != nil {
			return err
		}
		candidateStatus := model.MemoryCandidateApplied
		if params.Action == model.MemoryDreamReject {
			candidateStatus = model.MemoryCandidateRejected
		}
		if err := tx.Model(&model.MemoryCandidate{}).
			Where("id = ? AND user_id = ? AND status = ? AND dream_run_id = ?", params.CandidateID, params.UserID, model.MemoryCandidateProcessing, params.DreamRunID).
			Updates(map[string]any{"status": candidateStatus, "rejection_reason": rejectionReason(params.Action, params.Reason), "processed_at": params.Now}).Error; err != nil {
			return err
		}
		completedAt := params.Now
		result.Run.Status = model.MemoryDreamRunCompleted
		result.Run.Action = params.Action
		result.Run.TargetMemoryID = params.TargetMemoryID
		result.Run.ResultMemoryID = memoryID
		result.Run.CompletedAt = &completedAt
		if err := tx.Model(&model.MemoryDreamRun{}).
			Where("id = ? AND user_id = ? AND status = ?", params.DreamRunID, params.UserID, model.MemoryDreamRunProcessing).
			Updates(map[string]any{"status": model.MemoryDreamRunCompleted, "action": params.Action, "target_memory_id": params.TargetMemoryID, "result_memory_id": memoryID, "completed_at": params.Now, "error_code": ""}).Error; err != nil {
			return err
		}
		if after != nil {
			copy := *after
			result.Memory = &copy
		}
		return nil
	})
	return result, err
}

func (r *Repository) FailDream(ctx context.Context, userID int64, candidateID, dreamRunID, errorCode string, now time.Time) error {
	if r == nil || r.db == nil || userID <= 0 || candidateID == "" || dreamRunID == "" || errorCode == "" || now.IsZero() {
		return fmt.Errorf("invalid dream failure")
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		candidate := tx.Model(&model.MemoryCandidate{}).
			Where("id = ? AND user_id = ? AND status = ? AND dream_run_id = ?", candidateID, userID, model.MemoryCandidateProcessing, dreamRunID).
			Updates(map[string]any{"status": model.MemoryCandidatePending, "dream_run_id": ""})
		if candidate.Error != nil {
			return candidate.Error
		}
		if candidate.RowsAffected != 1 {
			return ErrDreamConflict
		}
		run := tx.Model(&model.MemoryDreamRun{}).
			Where("id = ? AND user_id = ? AND candidate_id = ? AND status = ?", dreamRunID, userID, candidateID, model.MemoryDreamRunProcessing).
			Updates(map[string]any{"status": model.MemoryDreamRunFailed, "error_code": errorCode, "completed_at": now})
		if run.Error != nil {
			return run.Error
		}
		if run.RowsAffected != 1 {
			return ErrDreamConflict
		}
		return nil
	})
}

type MemoryListQuery struct {
	UserID int64
	Scope  model.MemoryScope
	Status model.LongTermMemoryStatus
	Limit  int
}

func (r *Repository) ListMemories(ctx context.Context, query MemoryListQuery) ([]model.LongTermMemory, error) {
	if r == nil || r.db == nil || query.UserID <= 0 || query.Limit < 1 || query.Limit > 200 {
		return nil, fmt.Errorf("invalid memory list query")
	}
	db := r.db.WithContext(ctx).Where("user_id = ?", query.UserID)
	if query.Scope != "" {
		if !query.Scope.Valid() {
			return nil, fmt.Errorf("invalid memory scope")
		}
		db = db.Where("memory_scope = ?", query.Scope)
	}
	if query.Status != "" {
		if !query.Status.Valid() {
			return nil, fmt.Errorf("invalid memory status")
		}
		db = db.Where("status = ?", query.Status)
	}
	var memories []model.LongTermMemory
	err := db.Order("user_confirmed DESC, updated_at DESC, id ASC").Limit(query.Limit).Find(&memories).Error
	return memories, err
}

func (r *Repository) PendingCandidateCount(ctx context.Context, userID int64) (int64, error) {
	if r == nil || r.db == nil || userID <= 0 {
		return 0, fmt.Errorf("invalid candidate count query")
	}
	var count int64
	err := r.db.WithContext(ctx).Model(&model.MemoryCandidate{}).Where("user_id = ? AND status = ?", userID, model.MemoryCandidatePending).Count(&count).Error
	return count, err
}

// ListUsersWithPendingCandidates names the owners that have something for the
// governance step to look at.
//
// The step is per-user by construction: ClaimCandidate takes a user id in its
// first predicate, so user isolation holds from the first SQL statement rather
// than being filtered afterwards. A background loop therefore needs the list of
// owners, not a global queue of candidates.
//
// Ordering by the oldest pending candidate keeps one busy account from starving
// the others: whoever has waited longest is served first.
func (r *Repository) ListUsersWithPendingCandidates(ctx context.Context, limit int) ([]int64, error) {
	if r == nil || r.db == nil || limit < 1 || limit > 1000 {
		return nil, fmt.Errorf("invalid pending candidate owner query")
	}
	var userIDs []int64
	err := r.db.WithContext(ctx).Model(&model.MemoryCandidate{}).
		Where("status = ?", model.MemoryCandidatePending).
		Group("user_id").
		Order("MIN(created_at) ASC").
		Limit(limit).
		Pluck("user_id", &userIDs).Error
	return userIDs, err
}

func (r *Repository) ListRevisions(ctx context.Context, userID int64, memoryID string, limit int) ([]model.MemoryRevision, error) {
	if r == nil || r.db == nil || userID <= 0 || strings.TrimSpace(memoryID) == "" || limit < 1 || limit > 200 {
		return nil, fmt.Errorf("invalid revision query")
	}
	var count int64
	if err := r.db.WithContext(ctx).Model(&model.LongTermMemory{}).Where("id = ? AND user_id = ?", memoryID, userID).Count(&count).Error; err != nil {
		return nil, err
	}
	if count != 1 {
		return nil, ErrMemoryNotFound
	}
	var revisions []model.MemoryRevision
	err := r.db.WithContext(ctx).Where("memory_id = ? AND user_id = ?", memoryID, userID).
		Order("created_at DESC, id DESC").Limit(limit).Find(&revisions).Error
	return revisions, err
}

type CorrectMemoryParams struct {
	UserID             int64
	MemoryID           string
	Subject            string
	Content            string
	Tags               []string
	Reason             string
	Now                time.Time
	GlobalSnapshotHash string
}

func (r *Repository) CorrectMemory(ctx context.Context, params CorrectMemoryParams) (*model.LongTermMemory, *model.MemoryRevision, error) {
	if r == nil || r.db == nil || params.UserID <= 0 || params.MemoryID == "" || params.Now.IsZero() || strings.TrimSpace(params.Reason) == "" {
		return nil, nil, fmt.Errorf("invalid memory correction")
	}
	scope, err := activeMemoryScope(ctx, r.db, params.UserID, params.MemoryID)
	if err != nil {
		return nil, nil, err
	}
	if scope == model.MemoryScopeGlobal {
		var corrected *model.LongTermMemory
		var revision *model.MemoryRevision
		err = withUserMemoryLock(ctx, r.db, params.UserID, func(locked *gorm.DB) error {
			var correctErr error
			corrected, revision, correctErr = correctMemoryTx(ctx, locked, params)
			return correctErr
		})
		return corrected, revision, err
	}
	return correctMemoryTx(ctx, r.db, params)
}

func correctMemoryTx(ctx context.Context, db *gorm.DB, params CorrectMemoryParams) (*model.LongTermMemory, *model.MemoryRevision, error) {
	var corrected model.LongTermMemory
	var revision model.MemoryRevision
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if params.GlobalSnapshotHash != "" {
			actual, err := loadActiveGlobalSnapshot(tx, params.UserID, params.Now, GlobalMemorySnapshotLimit)
			if err != nil {
				return err
			}
			if GlobalSnapshotFingerprint(actual) != params.GlobalSnapshotHash {
				return ErrMemoryConflict
			}
		}
		var current model.LongTermMemory
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND user_id = ? AND status = ?", params.MemoryID, params.UserID, model.LongTermMemoryActive).First(&current).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrMemoryNotFound
		}
		if err != nil {
			return err
		}
		if !current.Scope.Valid() {
			return ErrDerivedMemory
		}
		corrected = current
		corrected.Subject = strings.TrimSpace(params.Subject)
		corrected.Content = strings.TrimSpace(params.Content)
		corrected.ContentHash = model.MemoryContentHash(corrected.Content)
		corrected.Tags = append(model.StringList(nil), params.Tags...)
		corrected.UserConfirmed = true
		revision = model.MemoryRevision{
			ID: uuid.NewString(), UserID: params.UserID, MemoryID: current.ID, Action: model.MemoryUserCorrect,
			Actor: model.MemoryRevisionUser, Reason: strings.TrimSpace(params.Reason), BeforeSnapshot: memorySnapshot(&current), BeforeHash: current.ContentHash, AfterHash: corrected.ContentHash, CreatedAt: params.Now,
		}
		corrected.SourceRefs = appendUnique(corrected.SourceRefs, "user_revision:"+revision.ID, 32)
		if err := corrected.Validate(); err != nil {
			return err
		}
		revision.AfterSnapshot = memorySnapshot(&corrected)
		if err := updateMemory(tx, &corrected); err != nil {
			return err
		}
		return tx.Create(&revision).Error
	})
	return &corrected, &revision, err
}

func (r *Repository) DeleteMemory(ctx context.Context, userID int64, memoryID, reason string, now time.Time) (*model.MemoryRevision, error) {
	if r == nil || r.db == nil || userID <= 0 || strings.TrimSpace(memoryID) == "" || strings.TrimSpace(reason) == "" || now.IsZero() {
		return nil, fmt.Errorf("invalid memory deletion")
	}
	var scope model.MemoryScope
	var probe model.LongTermMemory
	if err := r.db.WithContext(ctx).Select("memory_scope", "status").Where("id = ? AND user_id = ?", memoryID, userID).First(&probe).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrMemoryNotFound
		}
		return nil, err
	}
	scope = probe.Scope
	if scope == model.MemoryScopeGlobal && probe.Status != model.LongTermMemoryDeleted {
		var revision *model.MemoryRevision
		err := withUserMemoryLock(ctx, r.db, userID, func(locked *gorm.DB) error {
			var deleteErr error
			revision, deleteErr = deleteMemoryTx(ctx, locked, userID, memoryID, reason, now)
			return deleteErr
		})
		return revision, err
	}
	return deleteMemoryTx(ctx, r.db, userID, memoryID, reason, now)
}

func deleteMemoryTx(ctx context.Context, db *gorm.DB, userID int64, memoryID, reason string, now time.Time) (*model.MemoryRevision, error) {
	var revision model.MemoryRevision
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current model.LongTermMemory
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND user_id = ?", memoryID, userID).First(&current).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrMemoryNotFound
		}
		if err != nil {
			return err
		}
		if current.Status == model.LongTermMemoryDeleted {
			return tx.Where("memory_id = ? AND user_id = ? AND action = ?", memoryID, userID, model.MemoryUserDelete).
				Order("created_at DESC, id DESC").First(&revision).Error
		}
		oldContent := current.Content
		revision = model.MemoryRevision{
			ID: uuid.NewString(), UserID: userID, MemoryID: current.ID, Action: model.MemoryUserDelete,
			Actor: model.MemoryRevisionUser, Reason: strings.TrimSpace(reason), BeforeHash: current.ContentHash, AfterHash: current.ContentHash, CreatedAt: now,
		}
		deleted := current
		deleted.Subject, deleted.Content, deleted.TopicKey = "", "", ""
		deleted.StructuredJSON = `{}`
		deleted.Tags, deleted.SourceRefs = model.StringList{}, model.StringList{}
		deleted.Status, deleted.UserConfirmed = model.LongTermMemoryDeleted, true
		deleted.TombstoneHash, deleted.DeletedAt = current.ContentHash, &now
		if err := deleted.Validate(); err != nil {
			return err
		}
		if err := updateMemory(tx, &deleted); err != nil {
			return err
		}
		hashes := []string{current.ContentHash}
		if current.OriginContentHash != "" && current.OriginContentHash != current.ContentHash {
			hashes = append(hashes, current.OriginContentHash)
		}
		if err := tx.Model(&model.MemoryCandidate{}).
			Where("user_id = ? AND memory_scope = ? AND status = ? AND (content_hash IN ? OR (content_hash = '' AND proposed_content = ?))", userID, current.Scope, model.MemoryCandidatePending, hashes, oldContent).
			Updates(map[string]any{"status": model.MemoryCandidateRejected, "rejection_reason": "deleted_tombstone", "processed_at": now}).Error; err != nil {
			return err
		}
		// A deletion is a privacy action, not only a lifecycle transition. Old
		// revisions must not remain a second readable copy of the body.
		if err := tx.Model(&model.MemoryRevision{}).Where("memory_id = ? AND user_id = ?", current.ID, userID).
			Updates(map[string]any{"before_snapshot": "", "after_snapshot": ""}).Error; err != nil {
			return err
		}
		return tx.Create(&revision).Error
	})
	return &revision, err
}

func validateDreamResult(memory *model.LongTermMemory, userID int64, candidateID, retainedID string) error {
	if memory == nil || memory.UserID != userID || memory.Status != model.LongTermMemoryActive {
		return fmt.Errorf("invalid dream result ownership or status")
	}
	if retainedID != "" && memory.ID != retainedID {
		return fmt.Errorf("dream result identity changed")
	}
	if candidateID != "" {
		if memory.OriginCandidateID == nil || *memory.OriginCandidateID != candidateID {
			return fmt.Errorf("dream result candidate origin is invalid")
		}
	}
	return memory.Validate()
}

func updateMemory(tx *gorm.DB, memory *model.LongTermMemory) error {
	return tx.Model(&model.LongTermMemory{}).Where("id = ? AND user_id = ?", memory.ID, memory.UserID).Updates(map[string]any{
		"memory_scope": memory.Scope, "subject": memory.Subject, "content": memory.Content, "content_hash": memory.ContentHash, "origin_content_hash": memory.OriginContentHash,
		"structured_json": memory.StructuredJSON, "tags": memory.Tags, "topic_key": memory.TopicKey, "source_refs": memory.SourceRefs,
		"durability": memory.Durability, "valid_until": memory.ValidUntil, "confidence": memory.Confidence, "importance": memory.Importance, "status": memory.Status, "user_confirmed": memory.UserConfirmed,
		"tombstone_hash": memory.TombstoneHash, "deleted_at": memory.DeletedAt,
	}).Error
}

func memorySnapshot(memory *model.LongTermMemory) string {
	if memory == nil {
		return ""
	}
	payload, _ := json.Marshal(map[string]any{
		"id": memory.ID, "scope": memory.Scope, "subject": memory.Subject, "content": memory.Content,
		"structured": json.RawMessage(memory.StructuredJSON), "tags": memory.Tags, "topic_key": memory.TopicKey,
		"durability": memory.Durability, "valid_until": memory.ValidUntil,
		"source_refs": memory.SourceRefs, "confidence": memory.Confidence, "importance": memory.Importance,
		"status": memory.Status, "user_confirmed": memory.UserConfirmed,
	})
	return string(payload)
}

func memoryHash(memory *model.LongTermMemory) string {
	if memory == nil {
		return ""
	}
	return memory.ContentHash
}

func rejectionReason(action model.MemoryDreamAction, reason string) string {
	if action != model.MemoryDreamReject {
		return ""
	}
	runes := []rune(strings.TrimSpace(reason))
	if len(runes) > 128 {
		runes = runes[:128]
	}
	return string(runes)
}

func appendUnique(existing []string, value string, max int) model.StringList {
	result := append(model.StringList(nil), existing...)
	for _, item := range result {
		if item == value {
			return result
		}
	}
	if len(result) < max {
		result = append(result, value)
	}
	return result
}
