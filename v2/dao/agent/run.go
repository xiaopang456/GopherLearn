package agent

import (
	commonmysql "GopherAI/common/mysql"
	"GopherAI/model"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrSessionNotFound           = errors.New("agent session not found")
	ErrRunNotFound               = errors.New("agent run not found")
	ErrRunConflict               = errors.New("agent run state conflict")
	ErrInvalidTransition         = errors.New("invalid agent run transition")
	ErrRunInProgress             = errors.New("agent run already in progress")
	ErrRunRequestConflict        = errors.New("agent request key was used for different input")
	ErrRunRequestFinished        = errors.New("agent request already finished without a replayable result")
	ErrRunLeaseLost              = errors.New("agent run execution lease was lost")
	ErrMessageLimitReached       = errors.New("session message limit reached")
	ErrRunTemporarilyUnavailable = errors.New("agent run storage temporarily unavailable")
)

const runTransactionMaxAttempts = 3

// MaxRunsPerSessionRead bounds how many runs one session read returns. A
// session that holds more turns than this is not refused: the newest turns
// are the ones a reader is looking at, and the caller is told that older
// ones were left out.
const MaxRunsPerSessionRead = 100

type RunAcquisition struct {
	Run              *model.AgentRun
	Created          bool
	Resumed          bool
	NextStepSequence int
}

type Repository struct {
	db *gorm.DB
}

func NewRepository(db *gorm.DB) *Repository {
	return &Repository{db: db}
}

func DefaultRepository() *Repository {
	return NewRepository(commonmysql.DB)
}

func (r *Repository) CreateRun(ctx context.Context, run *model.AgentRun) error {
	if err := r.ready(); err != nil {
		return err
	}
	if err := run.ValidateForCreate(); err != nil {
		return err
	}
	if run.ExecutionKind != model.AgentRunExecutionLearning {
		return fmt.Errorf("CreateRun only accepts learning runs; agent chat must use AcquireRun")
	}
	return withRunTransaction(ctx, r.db, func(tx *gorm.DB) error {
		if err := lockOwnedSession(tx, run.UserID, run.SessionID); err != nil {
			return err
		}
		if active, err := findRunningRun(tx, run.UserID, run.SessionID, ""); err != nil {
			return err
		} else if active != nil {
			return ErrRunInProgress
		}
		return tx.Create(run).Error
	})
}

// AcquireRun combines request idempotency and the per-session running lease in
// one transaction. Completed requests are returned for replay; no side effect
// is repeated by the service.
func (r *Repository) AcquireRun(ctx context.Context, run *model.AgentRun) (*RunAcquisition, error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	if err := run.ValidateForCreate(); err != nil {
		return nil, err
	}
	if run.RequestKey == nil {
		return nil, fmt.Errorf("agent request key is required")
	}
	if run.ExecutionKind != model.AgentRunExecutionChat || strings.TrimSpace(run.LeaseToken) == "" ||
		run.LeaseExpiresAt == nil || run.HeartbeatAt == nil || !run.LeaseExpiresAt.After(*run.HeartbeatAt) {
		return nil, fmt.Errorf("agent chat run lease is invalid")
	}
	var acquired *RunAcquisition
	err := withRunTransaction(ctx, r.db, func(tx *gorm.DB) error {
		acquired = nil
		if err := lockOwnedSession(tx, run.UserID, run.SessionID); err != nil {
			return err
		}
		if existing, err := findRequestRun(tx, run.UserID, *run.RequestKey); err != nil {
			return err
		} else if existing != nil {
			result, classifyErr := classifyRequestRun(tx, existing, run, *run.HeartbeatAt)
			acquired = result
			return classifyErr
		}
		if active, err := findRunningRun(tx, run.UserID, run.SessionID, ""); err != nil {
			return err
		} else if active != nil {
			if !reclaimableChatRun(active, *run.HeartbeatAt) {
				acquired = &RunAcquisition{Run: active}
				return ErrRunInProgress
			}
			if err := expireLeasedRun(tx, active, *run.HeartbeatAt); err != nil {
				return err
			}
		}
		var messageCount int64
		if err := tx.Model(&model.Message{}).Where("session_id = ?", run.SessionID).Count(&messageCount).Error; err != nil {
			return err
		}
		if messageCount+2 > model.MaxMessagesPerSession {
			return ErrMessageLimitReached
		}
		if err := tx.Create(run).Error; err != nil {
			// The session row lock serializes one session, while a request key is
			// user-global. Resolve a rare cross-session unique race explicitly.
			if existing, lookupErr := findRequestRun(tx, run.UserID, *run.RequestKey); lookupErr == nil && existing != nil {
				result, classifyErr := classifyRequestRun(tx, existing, run, *run.HeartbeatAt)
				acquired = result
				return classifyErr
			}
			return err
		}
		copyOfRun := *run
		acquired = &RunAcquisition{Run: &copyOfRun, Created: true}
		return nil
	})
	if err != nil && !isRunAcquisitionResult(err) {
		return acquired, fmt.Errorf("%w: %v", ErrRunTemporarilyUnavailable, err)
	}
	return acquired, err
}

func withRunTransaction(ctx context.Context, db *gorm.DB, fn func(*gorm.DB) error) error {
	var lastErr error
	for attempt := 0; attempt < runTransactionMaxAttempts; attempt++ {
		lastErr = db.WithContext(ctx).Transaction(fn, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
		if !isMySQLDeadlock(lastErr) {
			return lastErr
		}
		if attempt+1 == runTransactionMaxAttempts {
			break
		}
		delay := time.Duration(attempt+1) * 5 * time.Millisecond
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
	return fmt.Errorf("%w: retries exhausted: %v", ErrRunTemporarilyUnavailable, lastErr)
}

func isMySQLDeadlock(err error) bool {
	var mysqlErr *mysqldriver.MySQLError
	return errors.As(err, &mysqlErr) && mysqlErr.Number == 1213
}

func isRunAcquisitionResult(err error) bool {
	return errors.Is(err, ErrSessionNotFound) || errors.Is(err, ErrRunInProgress) ||
		errors.Is(err, ErrRunRequestConflict) || errors.Is(err, ErrRunRequestFinished) ||
		errors.Is(err, ErrMessageLimitReached) || errors.Is(err, ErrRunTemporarilyUnavailable) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func (r *Repository) GetRun(ctx context.Context, userID int64, sessionID, runID string) (*model.AgentRun, error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	var run model.AgentRun
	err := r.db.WithContext(ctx).
		Where("id = ? AND user_id = ? AND session_id = ?", runID, userID, sessionID).
		First(&run).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrRunNotFound
	}
	if err != nil {
		return nil, err
	}
	return &run, nil
}

func (r *Repository) AppendStep(ctx context.Context, sessionID string, step *model.AgentStep) error {
	if err := r.ready(); err != nil {
		return err
	}
	if err := step.ValidateForCreate(); err != nil {
		return err
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var run model.AgentRun
		if err := tx.Select("id", "execution_kind", "status", "lease_token", "lease_expires_at").
			Where("id = ? AND user_id = ? AND session_id = ?", step.RunID, step.UserID, sessionID).
			First(&run).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrRunNotFound
			}
			return err
		}
		if run.ExecutionKind == model.AgentRunExecutionChat {
			if run.Status != model.AgentRunRunning || strings.TrimSpace(step.LeaseToken) == "" ||
				run.LeaseToken != step.LeaseToken || run.LeaseExpiresAt == nil || !run.LeaseExpiresAt.After(time.Now()) {
				return ErrRunLeaseLost
			}
		}
		return tx.Create(step).Error
	})
}

func (r *Repository) ListSteps(ctx context.Context, userID int64, sessionID, runID string) ([]model.AgentStep, error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	if _, err := r.GetRun(ctx, userID, sessionID, runID); err != nil {
		return nil, err
	}
	var steps []model.AgentStep
	err := r.db.WithContext(ctx).
		Where("user_id = ? AND run_id = ?", userID, runID).
		Order("sequence ASC").
		Find(&steps).Error
	return steps, err
}

// ListRunsBySession reads the runs of one session that belong to this user,
// newest first.
//
// The owner and the session are both part of the query, so a run of another
// account is absent from the result rather than reported: the caller has no
// legitimate use for knowing that a run id it may not read exists. The limit
// keeps the newest runs and the boolean says whether older ones were left
// out, so a reader can be told the list is incomplete instead of being left
// to assume it is whole.
func (r *Repository) ListRunsBySession(ctx context.Context, userID int64, sessionID string, limit int) ([]model.AgentRun, bool, error) {
	if err := r.ready(); err != nil {
		return nil, false, err
	}
	if userID <= 0 || strings.TrimSpace(sessionID) == "" {
		return nil, false, ErrRunNotFound
	}
	if limit <= 0 || limit > MaxRunsPerSessionRead {
		limit = MaxRunsPerSessionRead
	}
	var runs []model.AgentRun
	// One row beyond the limit is read to learn whether the list was cut. The
	// row that does not fit is the oldest one, so it is the one dropped.
	err := r.db.WithContext(ctx).
		Where("user_id = ? AND session_id = ?", userID, sessionID).
		Order("started_at DESC, id DESC").
		Limit(limit + 1).
		Find(&runs).Error
	if err != nil {
		return nil, false, err
	}
	truncated := len(runs) > limit
	if truncated {
		runs = runs[:limit]
	}
	return runs, truncated, nil
}

// ListStepsByRuns reads the steps of several runs of this user at once.
//
// The step table has no session column, so the fence is the owner plus the
// run ids, and those run ids come from a read that already carried the owner
// and the session. The order is by run and then by the server's own
// sequence, which is the order the steps happened in.
func (r *Repository) ListStepsByRuns(ctx context.Context, userID int64, runIDs []string) ([]model.AgentStep, error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	if userID <= 0 || len(runIDs) == 0 {
		return nil, nil
	}
	if len(runIDs) > MaxRunsPerSessionRead {
		runIDs = runIDs[:MaxRunsPerSessionRead]
	}
	var steps []model.AgentStep
	err := r.db.WithContext(ctx).
		Where("user_id = ? AND run_id IN ?", userID, runIDs).
		Order("run_id ASC, sequence ASC").
		Find(&steps).Error
	return steps, err
}

func (r *Repository) TransitionRun(
	ctx context.Context,
	userID int64,
	sessionID, runID string,
	expectedVersion uint64,
	from, to model.AgentRunStatus,
	terminationReason, answerSummary string,
) (*model.AgentRun, error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	if !from.CanTransition(to) {
		return nil, fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, from, to)
	}

	updates := map[string]interface{}{
		"status":         to,
		"version":        gorm.Expr("version + 1"),
		"answer_summary": answerSummary,
	}
	if from == model.AgentRunRunning && to != model.AgentRunRunning {
		updates["lease_token"] = ""
		updates["lease_expires_at"] = nil
		updates["heartbeat_at"] = nil
	}
	if to.Terminal() {
		now := time.Now()
		updates["finished_at"] = &now
		updates["termination_reason"] = terminationReason
	}

	var result *gorm.DB
	update := func(db *gorm.DB) error {
		query := db.Model(&model.AgentRun{}).
			Where("id = ? AND user_id = ? AND session_id = ? AND execution_kind = ? AND status = ? AND version = ?",
				runID, userID, sessionID, model.AgentRunExecutionLearning, from, expectedVersion)
		result = query.Updates(updates)
		return result.Error
	}
	if to == model.AgentRunRunning {
		err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if err := lockOwnedSession(tx, userID, sessionID); err != nil {
				return err
			}
			if active, err := findRunningRun(tx, userID, sessionID, runID); err != nil {
				return err
			} else if active != nil {
				return ErrRunInProgress
			}
			return update(tx)
		})
		if err != nil {
			return nil, err
		}
	} else if err := update(r.db.WithContext(ctx)); err != nil {
		return nil, err
	}
	if result.RowsAffected == 0 {
		var count int64
		if err := r.db.WithContext(ctx).Model(&model.AgentRun{}).
			Where("id = ? AND user_id = ? AND session_id = ?", runID, userID, sessionID).
			Count(&count).Error; err != nil {
			return nil, err
		}
		if count == 0 {
			return nil, ErrRunNotFound
		}
		return nil, ErrRunConflict
	}
	return r.GetRun(ctx, userID, sessionID, runID)
}

func (r *Repository) RenewRunLease(ctx context.Context, userID int64, sessionID, runID, leaseToken string, now, leaseEnds time.Time) error {
	if err := r.ready(); err != nil {
		return err
	}
	if strings.TrimSpace(leaseToken) == "" || now.IsZero() || !leaseEnds.After(now) {
		return fmt.Errorf("agent run lease renewal is invalid")
	}
	result := r.db.WithContext(ctx).Model(&model.AgentRun{}).
		Where("id = ? AND user_id = ? AND session_id = ? AND execution_kind = ? AND status = ? AND lease_token = ? AND lease_expires_at > ?",
			runID, userID, sessionID, model.AgentRunExecutionChat, model.AgentRunRunning, leaseToken, now).
		Updates(map[string]interface{}{
			// A client-side timeout does not prove that MySQL stopped the UPDATE.
			// An older timed-out attempt may therefore finish after a newer retry.
			// Keep both timestamps monotonic so that such a late statement cannot
			// shorten a lease which this owner has already renewed.
			"heartbeat_at": gorm.Expr(
				"CASE WHEN heartbeat_at IS NULL OR heartbeat_at < ? THEN ? ELSE heartbeat_at END",
				now, now,
			),
			"lease_expires_at": gorm.Expr(
				"CASE WHEN lease_expires_at IS NULL OR lease_expires_at < ? THEN ? ELSE lease_expires_at END",
				leaseEnds, leaseEnds,
			),
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		// MySQL reports changed rows, not merely matched rows. A retry whose
		// earlier attempt already stored an equal or later deadline is a valid
		// renewal even though the monotonic UPDATE changes nothing. Recheck the
		// complete ownership fence before calling it lost.
		var count int64
		if err := r.db.WithContext(ctx).Model(&model.AgentRun{}).
			Where("id = ? AND user_id = ? AND session_id = ? AND execution_kind = ? AND status = ? AND lease_token = ? AND lease_expires_at > ?",
				runID, userID, sessionID, model.AgentRunExecutionChat, model.AgentRunRunning, leaseToken, now).
			Count(&count).Error; err != nil {
			return err
		}
		if count == 0 {
			return ErrRunLeaseLost
		}
	}
	return nil
}

func (r *Repository) ExpireAbandonedChatRuns(ctx context.Context, now time.Time, limit int) (int64, error) {
	if err := r.ready(); err != nil {
		return 0, err
	}
	if now.IsZero() || limit <= 0 || limit > 1000 {
		return 0, fmt.Errorf("invalid abandoned run sweep")
	}
	type expiredRunIdentity struct {
		ID string
	}
	var candidates []expiredRunIdentity
	if err := r.db.WithContext(ctx).Model(&model.AgentRun{}).
		Select("id").
		Where("execution_kind = ? AND status = ? AND lease_expires_at IS NOT NULL AND lease_expires_at <= ?",
			model.AgentRunExecutionChat, model.AgentRunRunning, now).
		Order("lease_expires_at ASC, id ASC").Limit(limit).Scan(&candidates).Error; err != nil {
		return 0, err
	}
	if len(candidates) == 0 {
		return 0, nil
	}
	var expired int64
	for _, candidate := range candidates {
		// A point update makes the primary key the lock entry point. The old
		// multi-row status scan could lock idx_agent_runs_status first and then
		// deadlock with CompleteRun, which locks the primary row before changing
		// status. Conditions are repeated because the lease may have renewed
		// after the candidate scan.
		var affected int64
		if err := withRunTransaction(ctx, r.db, func(tx *gorm.DB) error {
			affected = 0
			result := tx.Model(&model.AgentRun{}).
				Where("id = ? AND execution_kind = ? AND status = ? AND lease_expires_at IS NOT NULL AND lease_expires_at <= ?",
					candidate.ID, model.AgentRunExecutionChat, model.AgentRunRunning, now).
				Updates(map[string]interface{}{
					"status": model.AgentRunTimeout, "version": gorm.Expr("version + 1"),
					"termination_reason": "lease_expired", "finished_at": now,
					"lease_token": "", "lease_expires_at": nil, "heartbeat_at": nil,
				})
			affected = result.RowsAffected
			return result.Error
		}); err != nil {
			return expired, err
		}
		expired += affected
	}
	return expired, nil
}

func (r *Repository) TransitionLeasedRun(
	ctx context.Context,
	userID int64,
	sessionID, runID, leaseToken string,
	expectedVersion uint64,
	to model.AgentRunStatus,
	terminationReason, answerSummary string,
) (*model.AgentRun, error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	if !model.AgentRunRunning.CanTransition(to) || to == model.AgentRunRunning || strings.TrimSpace(leaseToken) == "" {
		return nil, fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, model.AgentRunRunning, to)
	}
	now := time.Now()
	updates := map[string]interface{}{
		"status": to, "version": gorm.Expr("version + 1"), "answer_summary": answerSummary,
		"lease_token": "", "lease_expires_at": nil, "heartbeat_at": nil,
	}
	if to.Terminal() {
		updates["finished_at"] = &now
		updates["termination_reason"] = terminationReason
	}
	result := r.db.WithContext(ctx).Model(&model.AgentRun{}).
		Where("id = ? AND user_id = ? AND session_id = ? AND execution_kind = ? AND status = ? AND version = ? AND lease_token = ? AND lease_expires_at > ?",
			runID, userID, sessionID, model.AgentRunExecutionChat, model.AgentRunRunning, expectedVersion, leaseToken, now).
		Updates(updates)
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		return nil, ErrRunLeaseLost
	}
	return r.GetRun(ctx, userID, sessionID, runID)
}

func (r *Repository) CompleteRun(
	ctx context.Context,
	userID int64,
	sessionID, runID, leaseToken string,
	expectedVersion uint64,
	assistant *model.Message,
	answerSummary, resultJSON string,
) (*model.AgentRun, *model.Message, error) {
	if err := r.ready(); err != nil {
		return nil, nil, err
	}
	if strings.TrimSpace(resultJSON) == "" || strings.TrimSpace(leaseToken) == "" || assistant == nil ||
		assistant.IsUser || assistant.SessionID != sessionID {
		return nil, nil, fmt.Errorf("agent completion payload is invalid")
	}
	runIDCopy, role := runID, model.MessageAgentRunRoleAssistant
	assistant.AgentRunID, assistant.AgentRunRole = &runIDCopy, &role
	now := time.Now()
	var completed model.AgentRun
	var persistedAssistant model.Message
	err := withRunTransaction(ctx, r.db, func(tx *gorm.DB) error {
		completed = model.AgentRun{}
		persistedAssistant = model.Message{}
		var owned model.AgentRun
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND user_id = ? AND session_id = ?", runID, userID, sessionID).
			First(&owned).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrRunNotFound
			}
			return err
		}
		if owned.ExecutionKind != model.AgentRunExecutionChat || owned.Status != model.AgentRunRunning ||
			owned.Version != expectedVersion || owned.LeaseToken != leaseToken || owned.LeaseExpiresAt == nil || !owned.LeaseExpiresAt.After(now) {
			return ErrRunLeaseLost
		}
		attemptAssistant := *assistant
		if err := tx.Create(&attemptAssistant).Error; err != nil {
			return err
		}
		result := tx.Model(&model.AgentRun{}).
			Where("id = ? AND user_id = ? AND session_id = ? AND execution_kind = ? AND status = ? AND version = ? AND lease_token = ?",
				runID, userID, sessionID, model.AgentRunExecutionChat, model.AgentRunRunning, expectedVersion, leaseToken).
			Updates(map[string]interface{}{
				"status": model.AgentRunCompleted, "version": gorm.Expr("version + 1"),
				"answer_summary": answerSummary, "result_json": resultJSON,
				"finished_at": &now, "termination_reason": "answered",
				"lease_token": "", "lease_expires_at": nil, "heartbeat_at": nil,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return ErrRunLeaseLost
		}
		if err := tx.Where("id = ? AND user_id = ? AND session_id = ?", runID, userID, sessionID).First(&completed).Error; err != nil {
			return err
		}
		persistedAssistant = attemptAssistant
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	*assistant = persistedAssistant
	return &completed, assistant, nil
}

func lockOwnedSession(tx *gorm.DB, userID int64, sessionID string) error {
	var session model.Session
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Select("id").Where("id = ? AND user_id = ?", sessionID, userID).First(&session).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrSessionNotFound
	}
	return err
}

func findRunningRun(tx *gorm.DB, userID int64, sessionID, excludeRunID string) (*model.AgentRun, error) {
	var run model.AgentRun
	// Every caller already owns the Session row lock, which serializes creation
	// and transitions into running for this session. A second FOR UPDATE here is
	// both unnecessary and dangerous: MySQL chose the low-cardinality status
	// index, locked unrelated users' running entries, then waited for their
	// primary rows while CompleteRun held the primary row and waited to update
	// that same status index. Terminal updates remain conditional, so a
	// concurrent completion/reaper is still safely detected without this lock.
	query := tx.Where("user_id = ? AND session_id = ? AND status = ?", userID, sessionID, model.AgentRunRunning)
	if excludeRunID != "" {
		query = query.Where("id <> ?", excludeRunID)
	}
	err := query.First(&run).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &run, err
}

func findRequestRun(tx *gorm.DB, userID int64, requestKey string) (*model.AgentRun, error) {
	var run model.AgentRun
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("user_id = ? AND request_key = ?", userID, requestKey).First(&run).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &run, err
}

func classifyRequestRun(tx *gorm.DB, existing, requested *model.AgentRun, now time.Time) (*RunAcquisition, error) {
	acquired := &RunAcquisition{Run: existing}
	if existing.SessionID != requested.SessionID || existing.InputHash != requested.InputHash {
		return acquired, ErrRunRequestConflict
	}
	switch existing.Status {
	case model.AgentRunCompleted:
		if strings.TrimSpace(existing.ResultJSON) == "" {
			return acquired, ErrRunRequestFinished
		}
		return acquired, nil
	case model.AgentRunRunning:
		if !reclaimableChatRun(existing, now) {
			return acquired, ErrRunInProgress
		}
		result := tx.Model(&model.AgentRun{}).
			Where("id = ? AND user_id = ? AND session_id = ? AND execution_kind = ? AND status = ? AND version = ? AND lease_token = ?",
				existing.ID, existing.UserID, existing.SessionID, model.AgentRunExecutionChat, model.AgentRunRunning, existing.Version, existing.LeaseToken).
			Updates(map[string]interface{}{
				"lease_token": requested.LeaseToken, "lease_expires_at": requested.LeaseExpiresAt,
				"heartbeat_at": requested.HeartbeatAt, "version": gorm.Expr("version + 1"),
			})
		if result.Error != nil {
			return acquired, result.Error
		}
		if result.RowsAffected == 0 {
			return acquired, ErrRunInProgress
		}
		if err := tx.Where("id = ?", existing.ID).First(existing).Error; err != nil {
			return acquired, err
		}
		var maximum struct{ Sequence int }
		if err := tx.Model(&model.AgentStep{}).Select("COALESCE(MAX(sequence), 0) AS sequence").Where("run_id = ?", existing.ID).Scan(&maximum).Error; err != nil {
			return acquired, err
		}
		acquired.Run, acquired.Resumed, acquired.NextStepSequence = existing, true, maximum.Sequence
		return acquired, nil
	case model.AgentRunWaitingUser, model.AgentRunWaitingConfirmation:
		return acquired, ErrRunInProgress
	default:
		return acquired, ErrRunRequestFinished
	}
}

func reclaimableChatRun(run *model.AgentRun, now time.Time) bool {
	return run != nil && run.ExecutionKind == model.AgentRunExecutionChat && run.Status == model.AgentRunRunning &&
		strings.TrimSpace(run.LeaseToken) != "" && run.LeaseExpiresAt != nil && !run.LeaseExpiresAt.After(now)
}

func expireLeasedRun(tx *gorm.DB, run *model.AgentRun, now time.Time) error {
	result := tx.Model(&model.AgentRun{}).
		Where("id = ? AND user_id = ? AND session_id = ? AND execution_kind = ? AND status = ? AND version = ? AND lease_token = ?",
			run.ID, run.UserID, run.SessionID, model.AgentRunExecutionChat, model.AgentRunRunning, run.Version, run.LeaseToken).
		Updates(map[string]interface{}{
			"status": model.AgentRunTimeout, "version": gorm.Expr("version + 1"), "termination_reason": "lease_expired",
			"finished_at": now, "lease_token": "", "lease_expires_at": nil, "heartbeat_at": nil,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrRunInProgress
	}
	return nil
}

func (r *Repository) ready() error {
	if r == nil || r.db == nil {
		return fmt.Errorf("agent repository is not initialized")
	}
	return nil
}
