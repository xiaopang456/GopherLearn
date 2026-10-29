package episode

import (
	"GopherAI/model"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var episodeErrorCodePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_:-]{0,63}$`)

const episodeMutationMaxAttempts = 3

// UpsertDesired provides the durable debounce boundary. A higher desired
// message revives terminal jobs and postpones pending jobs, but it never steals
// a live lease. The current worker finishes ClaimedMessageID and requeues.
func (r *Repository) UpsertDesired(ctx context.Context, userID int64, sessionID string, desiredMessageID uint, availableAt time.Time) (*model.EpisodeProjectionJob, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("episode repository is not initialized")
	}
	requested := &model.EpisodeProjectionJob{
		ID: uuid.NewString(), UserID: userID, SessionID: strings.TrimSpace(sessionID), DesiredMessageID: desiredMessageID,
		Status: model.EpisodeProjectionPending, AvailableAt: availableAt,
	}
	if err := requested.ValidateForCreate(); err != nil {
		return nil, err
	}
	var job *model.EpisodeProjectionJob
	err := withEpisodeMutationTransaction(ctx, r.db, func(tx *gorm.DB) error {
		job = nil
		attempt := *requested
		if err := requireActiveSession(tx, userID, attempt.SessionID, true); err != nil {
			return ErrJobNotFound
		}
		// clause.Set preserves assignment order. desired_message_id must be last
		// because every preceding expression compares the incoming value with the
		// old database value.
		updates := clause.Set{
			{Column: clause.Column{Name: "available_at"}, Value: gorm.Expr("IF(VALUES(desired_message_id) > desired_message_id, VALUES(available_at), available_at)")},
			{Column: clause.Column{Name: "completed_at"}, Value: gorm.Expr("IF(VALUES(desired_message_id) > desired_message_id AND status <> ?, NULL, completed_at)", model.EpisodeProjectionProcessing)},
			{Column: clause.Column{Name: "error_code"}, Value: gorm.Expr("IF(VALUES(desired_message_id) > desired_message_id AND status <> ?, '', error_code)", model.EpisodeProjectionProcessing)},
			{Column: clause.Column{Name: "attempts"}, Value: gorm.Expr("IF(VALUES(desired_message_id) > desired_message_id AND status <> ?, 0, attempts)", model.EpisodeProjectionProcessing)},
			{Column: clause.Column{Name: "claimed_message_id"}, Value: gorm.Expr("IF(VALUES(desired_message_id) > desired_message_id AND status <> ?, 0, claimed_message_id)", model.EpisodeProjectionProcessing)},
			{Column: clause.Column{Name: "lease_token"}, Value: gorm.Expr("IF(VALUES(desired_message_id) > desired_message_id AND status <> ?, '', lease_token)", model.EpisodeProjectionProcessing)},
			{Column: clause.Column{Name: "lease_ends_at"}, Value: gorm.Expr("IF(VALUES(desired_message_id) > desired_message_id AND status <> ?, NULL, lease_ends_at)", model.EpisodeProjectionProcessing)},
			{Column: clause.Column{Name: "status"}, Value: gorm.Expr("IF(VALUES(desired_message_id) > desired_message_id AND status <> ?, ?, status)", model.EpisodeProjectionProcessing, model.EpisodeProjectionPending)},
			{Column: clause.Column{Name: "updated_at"}, Value: gorm.Expr("IF(VALUES(desired_message_id) > desired_message_id, VALUES(updated_at), updated_at)")},
			{Column: clause.Column{Name: "desired_message_id"}, Value: gorm.Expr("GREATEST(desired_message_id, VALUES(desired_message_id))")},
		}
		if err := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "user_id"}, {Name: "session_id"}},
			DoUpdates: updates,
		}).Create(&attempt).Error; err != nil {
			return err
		}
		current, err := getJobBySession(tx, userID, attempt.SessionID)
		if err != nil {
			return err
		}
		job = current
		return nil
	})
	return job, err
}

func (r *Repository) ListDue(ctx context.Context, limit, maxAttempts int, now time.Time) ([]model.EpisodeProjectionJob, error) {
	if r == nil || r.db == nil || limit < 1 || limit > 200 || maxAttempts < 1 || maxAttempts > 10 || now.IsZero() {
		return nil, fmt.Errorf("invalid episode projection job query")
	}
	var jobs []model.EpisodeProjectionJob
	err := r.db.WithContext(ctx).Model(&model.EpisodeProjectionJob{}).
		Select("episode_projection_jobs.*").
		Joins("JOIN sessions ON sessions.id = episode_projection_jobs.session_id AND sessions.user_id = episode_projection_jobs.user_id AND sessions.deleted_at IS NULL").
		Where("episode_projection_jobs.status = ? AND episode_projection_jobs.available_at <= ? AND episode_projection_jobs.attempts < ?", model.EpisodeProjectionPending, now, maxAttempts).
		Order("episode_projection_jobs.available_at ASC, episode_projection_jobs.created_at ASC").
		Limit(limit).Find(&jobs).Error
	return jobs, err
}

func (r *Repository) GetJob(ctx context.Context, userID int64, jobID string) (*model.EpisodeProjectionJob, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("episode repository is not initialized")
	}
	return getActiveJob(r.db.WithContext(ctx), userID, strings.TrimSpace(jobID))
}

func (r *Repository) Claim(ctx context.Context, userID int64, jobID string, now time.Time, lease time.Duration, maxAttempts int) (*model.EpisodeProjectionJob, error) {
	if r == nil || r.db == nil || userID <= 0 || strings.TrimSpace(jobID) == "" || now.IsZero() || lease <= 0 || maxAttempts < 1 || maxAttempts > 10 {
		return nil, fmt.Errorf("invalid episode projection job claim")
	}
	var claimed *model.EpisodeProjectionJob
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		job, err := lockJob(tx, userID, jobID)
		if err != nil {
			return err
		}
		if err := requireActiveSession(tx, userID, job.SessionID, false); err != nil {
			return ErrJobNotFound
		}
		if job.Status != model.EpisodeProjectionPending || job.AvailableAt.After(now) || job.Attempts >= maxAttempts {
			return ErrJobConflict
		}
		token := uuid.NewString()
		leaseEnds := now.Add(lease)
		result := tx.Model(&model.EpisodeProjectionJob{}).
			Where("id = ? AND user_id = ? AND status = ? AND desired_message_id = ?", job.ID, userID, model.EpisodeProjectionPending, job.DesiredMessageID).
			Updates(map[string]any{
				"status": model.EpisodeProjectionProcessing, "attempts": gorm.Expr("attempts + 1"),
				"claimed_message_id": job.DesiredMessageID, "lease_token": token, "lease_ends_at": leaseEnds,
				"error_code": "", "completed_at": nil,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrJobConflict
		}
		claimed, err = getClaimedJob(tx, userID, job.ID, token, false)
		return err
	})
	return claimed, err
}

// CompleteOrRequeue acknowledges exactly the target frozen at Claim time. If
// DesiredMessageID advanced meanwhile, the same durable row returns to pending
// with its quiet-window AvailableAt preserved.
func (r *Repository) CompleteOrRequeue(ctx context.Context, claimed *model.EpisodeProjectionJob, coveredMessageID uint, now time.Time) (*model.EpisodeProjectionJob, error) {
	if r == nil || r.db == nil || claimed == nil || claimed.UserID <= 0 || claimed.ID == "" || claimed.LeaseToken == "" || coveredMessageID == 0 || now.IsZero() {
		return nil, fmt.Errorf("invalid episode projection completion")
	}
	if coveredMessageID != claimed.ClaimedMessageID {
		return nil, fmt.Errorf("covered message id does not match claimed target")
	}
	var completed *model.EpisodeProjectionJob
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		job, err := getClaimedJob(tx, claimed.UserID, claimed.ID, claimed.LeaseToken, true)
		if err != nil {
			return err
		}
		if job.ClaimedMessageID != coveredMessageID {
			return ErrJobConflict
		}
		updates := map[string]any{
			"claimed_message_id": 0, "lease_token": "", "lease_ends_at": nil, "error_code": "",
		}
		if job.DesiredMessageID > job.ClaimedMessageID {
			updates["status"] = model.EpisodeProjectionPending
			updates["attempts"] = 0
			updates["completed_at"] = nil
		} else {
			updates["status"] = model.EpisodeProjectionCompleted
			updates["completed_at"] = now
		}
		result := tx.Model(&model.EpisodeProjectionJob{}).
			Where("id = ? AND user_id = ? AND status = ? AND lease_token = ?", job.ID, job.UserID, model.EpisodeProjectionProcessing, job.LeaseToken).
			Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrJobConflict
		}
		completed, err = getJobByID(tx, job.UserID, job.ID)
		return err
	})
	return completed, err
}

func (r *Repository) Retry(ctx context.Context, claimed *model.EpisodeProjectionJob, errorCode string, availableAt time.Time, maxAttempts int) (*model.EpisodeProjectionJob, error) {
	if r == nil || r.db == nil || claimed == nil || claimed.UserID <= 0 || claimed.ID == "" || claimed.LeaseToken == "" || availableAt.IsZero() || maxAttempts < 1 || maxAttempts > 10 || !episodeErrorCodePattern.MatchString(errorCode) {
		return nil, fmt.Errorf("invalid episode projection retry")
	}
	var retried *model.EpisodeProjectionJob
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		job, err := getClaimedJob(tx, claimed.UserID, claimed.ID, claimed.LeaseToken, true)
		if err != nil {
			return err
		}
		updates := map[string]any{
			"claimed_message_id": 0, "lease_token": "", "lease_ends_at": nil, "error_code": errorCode,
		}
		switch {
		case job.DesiredMessageID > job.ClaimedMessageID:
			updates["status"] = model.EpisodeProjectionPending
			updates["attempts"] = 0
			updates["available_at"] = laterTime(job.AvailableAt, availableAt)
			updates["completed_at"] = nil
		case job.Attempts >= maxAttempts:
			now := time.Now().UTC()
			updates["status"] = model.EpisodeProjectionFailed
			updates["completed_at"] = now
		case true:
			updates["status"] = model.EpisodeProjectionPending
			updates["available_at"] = availableAt
			updates["completed_at"] = nil
		}
		result := tx.Model(&model.EpisodeProjectionJob{}).
			Where("id = ? AND user_id = ? AND status = ? AND lease_token = ?", job.ID, job.UserID, model.EpisodeProjectionProcessing, job.LeaseToken).
			Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrJobConflict
		}
		retried, err = getJobByID(tx, job.UserID, job.ID)
		return err
	})
	return retried, err
}

// RecoverExpired never burns the retry budget for a newer desired target. A
// worker that died on message 10 must not make message 20 inherit its failure.
func (r *Repository) RecoverExpired(ctx context.Context, now time.Time, maxAttempts int) (int64, error) {
	return r.recoverExpired(ctx, now, maxAttempts, 0)
}

// recoverExpired accepts an optional owner only for isolated package tests.
// Production callers always use RecoverExpired and scan every owner.
// recoverExpiredBatch bounds one recovery pass. A pass that tried to reclaim an
// unbounded backlog in one go would hold locks for as long as that took.
const recoverExpiredBatch = 200

// recoverExpired accepts an optional owner only for isolated package tests.
// Production callers always use RecoverExpired and scan every owner.
//
// The scan and the writes are deliberately separate statements, and every write
// is a point update keyed by the primary key.
//
// The previous version issued three multi-row UPDATEs whose WHERE clause led
// with status, so InnoDB entered through idx_episode_jobs_status and then took
// the primary key. CompleteOrRequeue enters through the primary key and then
// touches that index, which is the opposite order — a lock-order inversion, and
// a load run caught it deadlocking for real:
//
//	TX A (recovery)   holds idx_episode_jobs_status, waits for PRIMARY
//	TX B (completion) holds PRIMARY, waits for idx_episode_jobs_status
//
// The rolled-back side was the completion, which left its job in processing
// holding a lease. dao/agent/run.go already carries the same fix for
// agent_runs, made after the same failure; this is that fix applied here.
//
// Reading the candidates without locking them means a row can change between
// the scan and its update, so every condition the branch depends on is repeated
// in the update itself. A row that moved on simply matches nothing.
func (r *Repository) recoverExpired(ctx context.Context, now time.Time, maxAttempts int, userID int64) (int64, error) {
	if r == nil || r.db == nil || now.IsZero() || maxAttempts < 1 || maxAttempts > 10 {
		return 0, fmt.Errorf("invalid episode projection recovery")
	}

	type expiredJob struct {
		ID               string
		DesiredMessageID uint
		ClaimedMessageID uint
		Attempts         int
	}
	var candidates []expiredJob
	scan := r.db.WithContext(ctx).Model(&model.EpisodeProjectionJob{}).
		Select("id", "desired_message_id", "claimed_message_id", "attempts").
		Where("status = ? AND lease_ends_at IS NOT NULL AND lease_ends_at <= ?",
			model.EpisodeProjectionProcessing, now)
	if userID > 0 {
		scan = scan.Where("user_id = ?", userID)
	}
	if err := scan.Order("lease_ends_at ASC, id ASC").Limit(recoverExpiredBatch).Scan(&candidates).Error; err != nil {
		return 0, err
	}

	var affected int64
	for _, candidate := range candidates {
		updates := map[string]any{
			"claimed_message_id": 0, "lease_token": "", "lease_ends_at": nil,
			"error_code": "worker_interrupted",
		}
		// The branch conditions match the old three queries exactly. A newer
		// desired target never inherits the dead worker's attempts: a worker
		// that died on message 10 must not make message 20 start out failing.
		switch {
		case candidate.DesiredMessageID > candidate.ClaimedMessageID:
			updates["status"] = model.EpisodeProjectionPending
			updates["attempts"] = 0
			updates["completed_at"] = nil
		case candidate.Attempts >= maxAttempts:
			updates["status"] = model.EpisodeProjectionFailed
			updates["completed_at"] = now
		default:
			updates["status"] = model.EpisodeProjectionPending
			updates["available_at"] = now
			updates["completed_at"] = nil
		}

		result := r.db.WithContext(ctx).Model(&model.EpisodeProjectionJob{}).
			Where("id = ? AND status = ? AND lease_ends_at IS NOT NULL AND lease_ends_at <= ? AND desired_message_id = ? AND claimed_message_id = ? AND attempts = ?",
				candidate.ID, model.EpisodeProjectionProcessing, now,
				candidate.DesiredMessageID, candidate.ClaimedMessageID, candidate.Attempts).
			Updates(updates)
		if result.Error != nil {
			return affected, result.Error
		}
		affected += result.RowsAffected
	}
	return affected, nil
}

func getActiveJob(db *gorm.DB, userID int64, jobID string) (*model.EpisodeProjectionJob, error) {
	if userID <= 0 || jobID == "" {
		return nil, ErrJobNotFound
	}
	var job model.EpisodeProjectionJob
	err := db.Model(&model.EpisodeProjectionJob{}).
		Select("episode_projection_jobs.*").
		Joins("JOIN sessions ON sessions.id = episode_projection_jobs.session_id AND sessions.user_id = episode_projection_jobs.user_id AND sessions.deleted_at IS NULL").
		Where("episode_projection_jobs.id = ? AND episode_projection_jobs.user_id = ?", jobID, userID).
		First(&job).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrJobNotFound
	}
	return &job, err
}

func getJobBySession(db *gorm.DB, userID int64, sessionID string) (*model.EpisodeProjectionJob, error) {
	var job model.EpisodeProjectionJob
	err := db.Where("user_id = ? AND session_id = ?", userID, sessionID).First(&job).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrJobNotFound
	}
	return &job, err
}

func getJobByID(db *gorm.DB, userID int64, jobID string) (*model.EpisodeProjectionJob, error) {
	var job model.EpisodeProjectionJob
	err := db.Where("id = ? AND user_id = ?", jobID, userID).First(&job).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrJobNotFound
	}
	return &job, err
}

func lockJob(db *gorm.DB, userID int64, jobID string) (*model.EpisodeProjectionJob, error) {
	var job model.EpisodeProjectionJob
	err := db.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND user_id = ?", jobID, userID).First(&job).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrJobNotFound
	}
	return &job, err
}

func getClaimedJob(db *gorm.DB, userID int64, jobID, leaseToken string, lock bool) (*model.EpisodeProjectionJob, error) {
	query := db
	if lock {
		query = query.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var job model.EpisodeProjectionJob
	err := query.Where("id = ? AND user_id = ? AND status = ? AND lease_token = ?", jobID, userID, model.EpisodeProjectionProcessing, leaseToken).First(&job).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrJobConflict
	}
	return &job, err
}

func laterTime(left, right time.Time) time.Time {
	if left.After(right) {
		return left
	}
	return right
}

// ReleaseOtherSessions makes this user's pending projections for every other
// session immediately claimable.
//
// The quiet window exists so an active conversation is not projected on every
// turn, only to be superseded by the next message. Once the user is writing in a
// different session, that reasoning no longer applies to the previous one: it is
// over, and holding its projection for another ten minutes only makes it
// invisible to recall. Measured before this existed: a question asked at 10:06
// about "上次" returned a session from 09:48, because the sessions that ended at
// 09:58 and 10:00 had no episode yet.
//
// Returning to the older session is handled by UpsertDesired, which raises
// desired_message_id and schedules another projection; an early projection is
// therefore wasted work at worst, never a wrong answer.
func (r *Repository) ReleaseOtherSessions(ctx context.Context, userID int64, currentSessionID string, now time.Time) (int64, error) {
	if r == nil || r.db == nil || userID <= 0 || strings.TrimSpace(currentSessionID) == "" || now.IsZero() {
		return 0, fmt.Errorf("invalid episode projection release")
	}
	var ids []string
	if err := r.db.WithContext(ctx).Model(&model.EpisodeProjectionJob{}).
		Where("user_id = ? AND session_id <> ? AND status = ? AND available_at > ?",
			userID, currentSessionID, model.EpisodeProjectionPending, now).
		Order("id ASC").Pluck("id", &ids).Error; err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}
	// Keep a global primary-key order even if a database collation returns UUIDs
	// differently. Concurrent releases exclude different sessions but acquire
	// every shared row in this same order, removing the old A->B / B->A cycle.
	sort.Strings(ids)
	var released int64
	err := withEpisodeMutationTransaction(ctx, r.db, func(tx *gorm.DB) error {
		released = 0
		for _, id := range ids {
			result := tx.Model(&model.EpisodeProjectionJob{}).
				Where("id = ? AND user_id = ? AND session_id <> ? AND status = ? AND available_at > ?",
					id, userID, currentSessionID, model.EpisodeProjectionPending, now).
				Update("available_at", now)
			if result.Error != nil {
				return result.Error
			}
			released += result.RowsAffected
		}
		return nil
	})
	return released, err
}

func withEpisodeMutationTransaction(ctx context.Context, db *gorm.DB, fn func(*gorm.DB) error) error {
	var lastErr error
	for attempt := 0; attempt < episodeMutationMaxAttempts; attempt++ {
		lastErr = db.WithContext(ctx).Transaction(fn, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
		if !isEpisodeMySQLDeadlock(lastErr) {
			return lastErr
		}
		if attempt+1 == episodeMutationMaxAttempts {
			break
		}
		timer := time.NewTimer(time.Duration(attempt+1) * 5 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
	return fmt.Errorf("episode mutation deadlock retries exhausted: %w", lastErr)
}

func isEpisodeMySQLDeadlock(err error) bool {
	var mysqlErr *mysqldriver.MySQLError
	return errors.As(err, &mysqlErr) && mysqlErr.Number == 1213
}
