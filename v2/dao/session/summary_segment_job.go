package session

import (
	"GopherAI/model"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	// ErrSummarySegmentJobNotFound means the job row is gone or not owned.
	ErrSummarySegmentJobNotFound = model.ErrSummarySegmentJobNotFound
	// ErrSummarySegmentJobConflict means another worker holds the job, or the
	// job is no longer claimable.
	ErrSummarySegmentJobConflict = model.ErrSummarySegmentJobConflict
)

// UpsertSummarySegmentJob records that a session wants a segment covering up
// to desiredMessageID.
//
// The row is unique per session, so a session never has two segment jobs. A
// later request with a higher target moves the job forward; a request with a
// lower target is ignored, because the target only ever grows. A job that is
// being processed is never taken away from the worker holding it: the worker
// finishes the segment it started and the new target is picked up afterwards.
func (r *Repository) UpsertSummarySegmentJob(ctx context.Context, job *model.SessionSummarySegmentJob) error {
	if r == nil || r.db == nil {
		return fmt.Errorf("session repository is not initialized")
	}
	if job == nil {
		return fmt.Errorf("summary segment job is required")
	}
	if strings.TrimSpace(job.ID) == "" {
		job.ID = uuid.NewString()
	}
	if job.UserID <= 0 || strings.TrimSpace(job.SessionID) == "" {
		return fmt.Errorf("summary segment job identity is required")
	}
	if job.DesiredMessageID == 0 {
		return fmt.Errorf("summary segment job target is required")
	}
	if job.Status == "" {
		job.Status = model.SessionSummarySegmentJobPending
	}
	if job.AvailableAt.IsZero() {
		job.AvailableAt = time.Now()
	}
	// The stored timestamp has millisecond precision, and MySQL rounds what it
	// is given. Quantizing before the write keeps the value that comes back
	// equal to the value that went in, so a job that is due now is not read
	// back as due half a millisecond from now.
	job.AvailableAt = quantizeSummarySegmentTime(job.AvailableAt)
	var owned int64
	if err := r.db.WithContext(ctx).Model(&model.Session{}).
		Where("id = ? AND user_id = ?", job.SessionID, job.UserID).
		Count(&owned).Error; err != nil {
		return err
	}
	if owned != 1 {
		return ErrSummarySegmentJobNotFound
	}
	err := r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "user_id"}, {Name: "session_id"}},
		DoUpdates: summarySegmentJobUpsertUpdates(r.db.Dialector.Name()),
	}).Create(job).Error
	if err != nil {
		return err
	}
	// The row may have existed already, in which case it kept its own id and
	// the caller's id was never stored. The caller's id is the one it will
	// claim the job by, so it has to be the row's id.
	stored, err := r.getSummarySegmentJobBySession(ctx, job.UserID, job.SessionID)
	if err != nil {
		return err
	}
	job.ID = stored.ID
	job.Status = stored.Status
	job.DesiredMessageID = stored.DesiredMessageID
	job.StartMessageID = stored.StartMessageID
	job.Attempts = stored.Attempts
	return nil
}

// getSummarySegmentJobBySession returns the session's single job row.
func (r *Repository) getSummarySegmentJobBySession(ctx context.Context, userID int64, sessionID string) (*model.SessionSummarySegmentJob, error) {
	var job model.SessionSummarySegmentJob
	err := r.db.WithContext(ctx).
		Where("user_id = ? AND session_id = ?", userID, sessionID).
		First(&job).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrSummarySegmentJobNotFound
	}
	if err != nil {
		return nil, err
	}
	return &job, nil
}

// summarySegmentJobUpsertUpdates renders the conflict-update assignments.
//
// MySQL evaluates the assignments in order, so every expression here compares
// the incoming value with the row that is already stored and desired_message_id
// is assigned last: an assignment that came earlier would replace the value
// the later expressions compare against, and the target could not be told from
// the one it replaces.
//
// SQLite has neither VALUES() nor IF() nor GREATEST(); its upsert exposes the
// incoming row as "excluded", so the same three decisions are written with
// CASE WHEN and MAX(). The semantics are identical in both dialects: a higher
// target revives a finished or failed job, a live lease is never stolen, and
// the target never moves backwards.
//
// A revived job starts its attempts over. The job that failed was a job about
// an older target, and the turns that arrived after it are a new question: the
// old failure says nothing about them.
func summarySegmentJobUpsertUpdates(dialect string) clause.Set {
	if dialect == "sqlite" {
		return clause.Set{
			{Column: clause.Column{Name: "error_code"}, Value: gorm.Expr("CASE WHEN excluded.desired_message_id > desired_message_id AND status <> ? THEN '' ELSE error_code END", model.SessionSummarySegmentJobProcessing)},
			{Column: clause.Column{Name: "lease_token"}, Value: gorm.Expr("CASE WHEN excluded.desired_message_id > desired_message_id AND status <> ? THEN '' ELSE lease_token END", model.SessionSummarySegmentJobProcessing)},
			{Column: clause.Column{Name: "lease_ends_at"}, Value: gorm.Expr("CASE WHEN excluded.desired_message_id > desired_message_id AND status <> ? THEN NULL ELSE lease_ends_at END", model.SessionSummarySegmentJobProcessing)},
			{Column: clause.Column{Name: "completed_at"}, Value: gorm.Expr("CASE WHEN excluded.desired_message_id > desired_message_id AND status <> ? THEN NULL ELSE completed_at END", model.SessionSummarySegmentJobProcessing)},
			{Column: clause.Column{Name: "attempts"}, Value: gorm.Expr("CASE WHEN excluded.desired_message_id > desired_message_id AND status <> ? THEN 0 ELSE attempts END", model.SessionSummarySegmentJobProcessing)},
			{Column: clause.Column{Name: "status"}, Value: gorm.Expr("CASE WHEN excluded.desired_message_id > desired_message_id AND status <> ? THEN ? ELSE status END", model.SessionSummarySegmentJobProcessing, model.SessionSummarySegmentJobPending)},
			{Column: clause.Column{Name: "updated_at"}, Value: gorm.Expr("CASE WHEN excluded.desired_message_id > desired_message_id THEN excluded.updated_at ELSE updated_at END")},
			{Column: clause.Column{Name: "desired_message_id"}, Value: gorm.Expr("MAX(desired_message_id, excluded.desired_message_id)")},
		}
	}
	return clause.Set{
		{Column: clause.Column{Name: "error_code"}, Value: gorm.Expr("IF(VALUES(desired_message_id) > desired_message_id AND status <> ?, '', error_code)", model.SessionSummarySegmentJobProcessing)},
		{Column: clause.Column{Name: "lease_token"}, Value: gorm.Expr("IF(VALUES(desired_message_id) > desired_message_id AND status <> ?, '', lease_token)", model.SessionSummarySegmentJobProcessing)},
		{Column: clause.Column{Name: "lease_ends_at"}, Value: gorm.Expr("IF(VALUES(desired_message_id) > desired_message_id AND status <> ?, NULL, lease_ends_at)", model.SessionSummarySegmentJobProcessing)},
		{Column: clause.Column{Name: "completed_at"}, Value: gorm.Expr("IF(VALUES(desired_message_id) > desired_message_id AND status <> ?, NULL, completed_at)", model.SessionSummarySegmentJobProcessing)},
		{Column: clause.Column{Name: "attempts"}, Value: gorm.Expr("IF(VALUES(desired_message_id) > desired_message_id AND status <> ?, 0, attempts)", model.SessionSummarySegmentJobProcessing)},
		{Column: clause.Column{Name: "status"}, Value: gorm.Expr("IF(VALUES(desired_message_id) > desired_message_id AND status <> ?, ?, status)", model.SessionSummarySegmentJobProcessing, model.SessionSummarySegmentJobPending)},
		{Column: clause.Column{Name: "updated_at"}, Value: gorm.Expr("IF(VALUES(desired_message_id) > desired_message_id, VALUES(updated_at), updated_at)")},
		{Column: clause.Column{Name: "desired_message_id"}, Value: gorm.Expr("GREATEST(desired_message_id, VALUES(desired_message_id))")},
	}
}

// ListDueSummarySegmentJobs returns the jobs that are ready to run.
//
// A job whose session has been deleted is not due: the join keeps a deleted
// session's job from being picked up, which is what stops the worker from
// generating a segment for a conversation that no longer exists.
func (r *Repository) ListDueSummarySegmentJobs(ctx context.Context, limit, maxAttempts int, now time.Time) ([]model.SessionSummarySegmentJob, error) {
	if r == nil || r.db == nil || limit < 1 || limit > 200 || maxAttempts < 1 || maxAttempts > 10 || now.IsZero() {
		return nil, fmt.Errorf("invalid summary segment job query")
	}
	// The comparison is made on the precision the column holds, so a job that
	// was written as due now is listed as due.
	now = quantizeSummarySegmentTime(now)
	var jobs []model.SessionSummarySegmentJob
	err := r.db.WithContext(ctx).Model(&model.SessionSummarySegmentJob{}).
		Select("session_summary_segment_jobs.*").
		Joins("JOIN sessions ON sessions.id = session_summary_segment_jobs.session_id AND sessions.user_id = session_summary_segment_jobs.user_id AND sessions.deleted_at IS NULL").
		Where("session_summary_segment_jobs.status = ? AND session_summary_segment_jobs.available_at <= ? AND session_summary_segment_jobs.attempts < ?",
			model.SessionSummarySegmentJobPending, now, maxAttempts).
		Order("session_summary_segment_jobs.available_at ASC, session_summary_segment_jobs.created_at ASC").
		Limit(limit).Find(&jobs).Error
	if err != nil {
		return nil, err
	}
	return jobs, nil
}

// GetSummarySegmentJob returns one job by its id.
func (r *Repository) GetSummarySegmentJob(ctx context.Context, userID int64, jobID string) (*model.SessionSummarySegmentJob, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("session repository is not initialized")
	}
	if userID <= 0 || strings.TrimSpace(jobID) == "" {
		return nil, ErrSummarySegmentJobNotFound
	}
	var job model.SessionSummarySegmentJob
	err := r.db.WithContext(ctx).
		Where("id = ? AND user_id = ?", jobID, userID).
		First(&job).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrSummarySegmentJobNotFound
	}
	if err != nil {
		return nil, err
	}
	return &job, nil
}

// ClaimSummarySegmentJob takes a job for one worker.
//
// The claim is what makes the segment exclusive to this worker: the status
// becomes processing under a lease, and only the holder of that lease can
// finish or retry the job.
func (r *Repository) ClaimSummarySegmentJob(ctx context.Context, userID int64, jobID string, now time.Time, lease time.Duration, maxAttempts int) (*model.SessionSummarySegmentJob, error) {
	if r == nil || r.db == nil || userID <= 0 || strings.TrimSpace(jobID) == "" || now.IsZero() || lease <= 0 || maxAttempts < 1 || maxAttempts > 10 {
		return nil, fmt.Errorf("invalid summary segment job claim")
	}
	// The comparison is made on the precision the column holds, on both
	// sides. Otherwise a job written as due now can read back as due half a
	// millisecond from now, and the claim refuses a job that is ready.
	now = quantizeSummarySegmentTime(now)
	var claimed *model.SessionSummarySegmentJob
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var job model.SessionSummarySegmentJob
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND user_id = ?", jobID, userID).First(&job).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrSummarySegmentJobNotFound
			}
			return err
		}
		if job.Status != model.SessionSummarySegmentJobPending || job.AvailableAt.After(now) || job.Attempts >= maxAttempts {
			return ErrSummarySegmentJobConflict
		}
		token := uuid.NewString()
		leaseEnds := quantizeSummarySegmentTime(now.Add(lease))
		result := tx.Model(&model.SessionSummarySegmentJob{}).
			Where("id = ? AND user_id = ? AND status = ? AND desired_message_id = ?",
				job.ID, userID, model.SessionSummarySegmentJobPending, job.DesiredMessageID).
			Updates(map[string]any{
				"status": model.SessionSummarySegmentJobProcessing, "attempts": gorm.Expr("attempts + 1"),
				"lease_token": token, "lease_ends_at": leaseEnds, "error_code": "", "completed_at": nil,
				// The batch is frozen here, on the first attempt of it. The
				// target may move forward while the batch is retried, but the
				// batch may not: a retry that reads further than the attempt
				// that failed is not the same attempt again.
				"batch_message_id": gorm.Expr("CASE WHEN batch_message_id = 0 THEN ? ELSE batch_message_id END", job.DesiredMessageID),
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrSummarySegmentJobConflict
		}
		var found model.SessionSummarySegmentJob
		if err := tx.Where("id = ? AND lease_token = ?", job.ID, token).First(&found).Error; err != nil {
			return err
		}
		claimed = &found
		return nil
	})
	if err != nil {
		return nil, err
	}
	return claimed, nil
}

// GetSummarySegmentJobBySession returns the session's single job row.
//
// It is what a reader needs to know whether the background is still working on
// the session: a job that is pending or processing means it is, and a job
// that failed means the background gave up on it.
func (r *Repository) GetSummarySegmentJobBySession(ctx context.Context, userID int64, sessionID string) (*model.SessionSummarySegmentJob, error) {
	if r == nil || r.db == nil || userID <= 0 || strings.TrimSpace(sessionID) == "" {
		return nil, ErrSummarySegmentJobNotFound
	}
	var job model.SessionSummarySegmentJob
	err := r.db.WithContext(ctx).
		Where("user_id = ? AND session_id = ?", userID, sessionID).
		First(&job).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrSummarySegmentJobNotFound
	}
	if err != nil {
		return nil, err
	}
	return &job, nil
}

// TakeOverSummarySegmentJob moves a session's job into the processing state
// for an attempt that the background worker did not start.
//
// The takeover refuses while another holder has a live lease, so one session
// never has two generations running at once, whoever started them. The target
// is raised to the newest turn and the start is moved to the first turn no
// segment covers, because the attempt is about the conversation as it is now
// and not about the batch the background was working on.
//
// A job with no row yet gets one: the attempt needs a row to hold its lease,
// and a row that does not exist cannot hold one.
func (r *Repository) TakeOverSummarySegmentJob(
	ctx context.Context,
	userID int64,
	sessionID string,
	desired, start uint,
	now time.Time,
	lease time.Duration,
) (*model.SessionSummarySegmentJob, error) {
	if r == nil || r.db == nil || userID <= 0 || strings.TrimSpace(sessionID) == "" ||
		desired == 0 || start > desired || now.IsZero() || lease <= 0 {
		return nil, fmt.Errorf("invalid summary segment job takeover")
	}
	now = quantizeSummarySegmentTime(now)
	leaseEnds := quantizeSummarySegmentTime(now.Add(lease))
	var taken *model.SessionSummarySegmentJob
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var job model.SessionSummarySegmentJob
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("user_id = ? AND session_id = ?", userID, sessionID).First(&job).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if err == nil {
			if job.Status == model.SessionSummarySegmentJobProcessing &&
				job.LeaseEndsAt != nil && job.LeaseEndsAt.After(now) {
				// Somebody else is holding the session. Two generations for
				// one session would race on the board, and one of them would
				// be paid for nothing.
				return ErrSummarySegmentJobConflict
			}
			token := uuid.NewString()
			target := job.DesiredMessageID
			if desired > target {
				target = desired
			}
			result := tx.Model(&model.SessionSummarySegmentJob{}).
				Where("id = ? AND user_id = ?", job.ID, userID).
				Updates(map[string]any{
					"status":             model.SessionSummarySegmentJobProcessing,
					"desired_message_id": target, "start_message_id": start,
					"batch_message_id": 0, "attempts": gorm.Expr("attempts + 1"),
					"lease_token": token, "lease_ends_at": leaseEnds,
					"error_code": "", "completed_at": nil, "available_at": now,
				})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return ErrSummarySegmentJobConflict
			}
			var found model.SessionSummarySegmentJob
			if err := tx.Where("id = ? AND lease_token = ?", job.ID, token).First(&found).Error; err != nil {
				return err
			}
			taken = &found
			return nil
		}
		created := &model.SessionSummarySegmentJob{
			ID: uuid.NewString(), UserID: userID, SessionID: sessionID,
			DesiredMessageID: desired, StartMessageID: start,
			Status: model.SessionSummarySegmentJobProcessing, Attempts: 1,
			AvailableAt: now, LeaseToken: uuid.NewString(), LeaseEndsAt: &leaseEnds,
		}
		if err := tx.Create(created).Error; err != nil {
			if isDuplicateKey(err) {
				return ErrSummarySegmentJobConflict
			}
			return err
		}
		taken = created
		return nil
	})
	if err != nil {
		return nil, err
	}
	return taken, nil
}

// CompleteSummarySegmentJob finishes the batch a job was working on.
//
// coveredThrough is the newest turn the segment actually covers, which is not
// always the end of the job's target: a segment may cover fewer turns than
// the batch holds, and the turns it did not cover are still owed.
//
// The job is finished in one of three ways, and the difference matters:
//
//   - The batch was longer than one segment may cover. The job stays pending
//     and the start moves past what was covered. The end of the batch does
//     not move: the rest of it is the same batch, continued.
//   - The target moved forward while the attempt ran. The job stays pending
//     and the start moves past what was covered, so the next attempt reads
//     the turns that are still uncovered instead of reading them again.
//   - Everything the target asked for is covered. The job is done, and the
//     start is left one turn past the coverage so that a later target for
//     the same session starts where this one stopped.
func (r *Repository) CompleteSummarySegmentJob(ctx context.Context, claimed *model.SessionSummarySegmentJob, coveredThrough uint, now time.Time) error {
	if r == nil || r.db == nil {
		return fmt.Errorf("session repository is not initialized")
	}
	if claimed == nil {
		return fmt.Errorf("summary segment job is required")
	}
	now = quantizeSummarySegmentTime(now)
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var job model.SessionSummarySegmentJob
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND user_id = ?", claimed.ID, claimed.UserID).First(&job).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrSummarySegmentJobNotFound
			}
			return err
		}
		if job.Status != model.SessionSummarySegmentJobProcessing || job.LeaseToken != claimed.LeaseToken {
			return ErrSummarySegmentJobConflict
		}
		batchEnd := job.BatchMessageID
		if batchEnd == 0 {
			batchEnd = job.DesiredMessageID
		}
		updates := map[string]any{
			"start_message_id": coveredThrough + 1,
			"lease_token":      "", "lease_ends_at": nil,
			"error_code": "", "completed_at": nil, "attempts": 0,
		}
		switch {
		case coveredThrough < batchEnd:
			// The batch holds more turns than one segment covers. The rest of
			// it is owed, and it is still the same batch.
			updates["status"] = model.SessionSummarySegmentJobPending
			updates["available_at"] = now
			updates["batch_message_id"] = batchEnd
		case job.DesiredMessageID > coveredThrough:
			// The target moved forward while this attempt ran.
			updates["status"] = model.SessionSummarySegmentJobPending
			updates["available_at"] = now
			updates["batch_message_id"] = 0
		default:
			updates["status"] = model.SessionSummarySegmentJobCompleted
			updates["completed_at"] = now
			updates["batch_message_id"] = 0
		}
		result := tx.Model(&model.SessionSummarySegmentJob{}).
			Where("id = ? AND user_id = ? AND status = ? AND lease_token = ?",
				job.ID, job.UserID, model.SessionSummarySegmentJobProcessing, claimed.LeaseToken).
			Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrSummarySegmentJobConflict
		}
		return nil
	})
}

// RetrySummarySegmentJob puts a failed job back in the queue.
//
// The retry covers the same stretch of turns as the attempt that failed. A
// failed segment never leaves a half-written board behind, so the next attempt
// starts from the same board and the same turns - it cannot accumulate the way
// a repeatedly rewritten summary could.
func (r *Repository) RetrySummarySegmentJob(ctx context.Context, claimed *model.SessionSummarySegmentJob, errorCode string, availableAt time.Time, maxAttempts int) error {
	if r == nil || r.db == nil {
		return fmt.Errorf("session repository is not initialized")
	}
	if claimed == nil {
		return fmt.Errorf("summary segment job is required")
	}
	status := model.SessionSummarySegmentJobPending
	var completedAt any
	if claimed.Attempts >= maxAttempts {
		status = model.SessionSummarySegmentJobFailed
		completedAt = quantizeSummarySegmentTime(time.Now())
	}
	// The retry time is quantized for the same reason the claim time is: a
	// value that was meant to be due now must read back as due now.
	availableAt = quantizeSummarySegmentTime(availableAt)
	result := r.db.WithContext(ctx).Model(&model.SessionSummarySegmentJob{}).
		Where("id = ? AND user_id = ? AND status = ? AND lease_token = ?",
			claimed.ID, claimed.UserID, model.SessionSummarySegmentJobProcessing, claimed.LeaseToken).
		Updates(map[string]any{
			"status": status, "error_code": errorCode, "available_at": availableAt,
			"lease_token": "", "lease_ends_at": nil, "completed_at": completedAt,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrSummarySegmentJobConflict
	}
	return nil
}

// RecoverExpiredSummarySegmentJobs requeues the jobs whose worker died.
//
// A job whose lease expired while it was being processed is either retried or
// failed, depending on how many attempts it has had. The recovery is what makes
// a segment survive a restart: the work is in the table, not in the process.
func (r *Repository) RecoverExpiredSummarySegmentJobs(ctx context.Context, now time.Time, maxAttempts int) (int64, error) {
	if r == nil || r.db == nil || now.IsZero() || maxAttempts < 1 {
		return 0, fmt.Errorf("invalid summary segment job recovery")
	}
	// The comparison is made on the precision the column holds, so a lease
	// that has just expired is recovered rather than waiting for the next
	// millisecond to tick over.
	now = quantizeSummarySegmentTime(now)
	var affected int64
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		failed := tx.Model(&model.SessionSummarySegmentJob{}).
			Where("status = ? AND lease_ends_at IS NOT NULL AND lease_ends_at <= ? AND attempts >= ?",
				model.SessionSummarySegmentJobProcessing, now, maxAttempts).
			Updates(map[string]any{
				"status": model.SessionSummarySegmentJobFailed, "lease_token": "", "lease_ends_at": nil,
				"error_code": "worker_interrupted", "completed_at": now,
			})
		if failed.Error != nil {
			return failed.Error
		}
		pending := tx.Model(&model.SessionSummarySegmentJob{}).
			Where("status = ? AND lease_ends_at IS NOT NULL AND lease_ends_at <= ? AND attempts < ?",
				model.SessionSummarySegmentJobProcessing, now, maxAttempts).
			Updates(map[string]any{
				"status": model.SessionSummarySegmentJobPending, "lease_token": "", "lease_ends_at": nil,
				"available_at": now, "error_code": "worker_interrupted",
			})
		if pending.Error != nil {
			return pending.Error
		}
		affected = failed.RowsAffected + pending.RowsAffected
		return nil
	})
	if err != nil {
		return 0, err
	}
	return affected, nil
}

// DeleteSummarySegmentJobs removes a session's jobs.
func (r *Repository) DeleteSummarySegmentJobs(ctx context.Context, userID int64, sessionID string) error {
	if r == nil || r.db == nil {
		return fmt.Errorf("session repository is not initialized")
	}
	return r.db.WithContext(ctx).
		Where("user_id = ? AND session_id = ?", userID, sessionID).
		Delete(&model.SessionSummarySegmentJob{}).Error
}
