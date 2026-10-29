package learning

import (
	"GopherAI/model"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type ClaimAnswerInput struct {
	Attempt   model.LearningAttempt
	Now       time.Time
	LeaseEnds time.Time
}

type ClaimAnswerResult struct {
	Attempt      *model.LearningAttempt
	Session      *model.LearningSession
	Question     *model.LearningQuestion
	Replay       bool
	NeedsGrading bool
}

type GradeCommit struct {
	Correctness   int
	Completeness  int
	Clarity       int
	Feedback      string
	MissingPoints []string
	Confidence    float64
}

type CompleteAnswerResult struct {
	Attempt      *model.LearningAttempt
	Session      *model.LearningSession
	NextQuestion *model.LearningQuestion
	Progress     *model.TopicProgress
}

func (r *Repository) ClaimAnswer(ctx context.Context, input ClaimAnswerInput) (*ClaimAnswerResult, error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	if input.LeaseEnds.IsZero() || !input.LeaseEnds.After(input.Now) {
		return nil, fmt.Errorf("learning grading lease is invalid")
	}
	input.Attempt.LeaseExpiresAt = &input.LeaseEnds
	if input.Attempt.UserID <= 0 || strings.TrimSpace(input.Attempt.ID) == "" || strings.TrimSpace(input.Attempt.SessionID) == "" ||
		strings.TrimSpace(input.Attempt.LearningSessionID) == "" || strings.TrimSpace(input.Attempt.QuestionID) == "" ||
		strings.TrimSpace(input.Attempt.SubmissionID) == "" || strings.TrimSpace(input.Attempt.Answer) == "" ||
		strings.TrimSpace(input.Attempt.LeaseToken) == "" {
		return nil, fmt.Errorf("learning attempt identity is incomplete")
	}
	var result ClaimAnswerResult
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var session model.LearningSession
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND user_id = ? AND session_id = ?", input.Attempt.LearningSessionID, input.Attempt.UserID, input.Attempt.SessionID).
			First(&session).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrLearningSessionNotFound
			}
			return err
		}

		var existing model.LearningAttempt
		existingErr := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("user_id = ? AND submission_id = ?", input.Attempt.UserID, input.Attempt.SubmissionID).
			First(&existing).Error
		if existingErr == nil {
			if existing.SessionID != input.Attempt.SessionID || existing.LearningSessionID != input.Attempt.LearningSessionID ||
				existing.QuestionID != input.Attempt.QuestionID || strings.TrimSpace(existing.Answer) != strings.TrimSpace(input.Attempt.Answer) {
				return ErrSubmissionConflict
			}
			result.Attempt = &existing
			result.Session = &session
			result.Replay = true
			if existing.Status == model.LearningAttemptCompleted || existing.Status == model.LearningAttemptSkipped {
				return nil
			}
			if existing.Status == model.LearningAttemptGrading && existing.LeaseExpiresAt != nil && existing.LeaseExpiresAt.After(input.Now) {
				return ErrGradingInProgress
			}
			if session.Status != model.LearningSessionWaitingAnswer && session.Status != model.LearningSessionGrading {
				return ErrLearningStateConflict
			}
			if session.CurrentQuestion != existing.QuestionOrdinal {
				return ErrLearningStateConflict
			}
			if err := tx.Model(&existing).Updates(map[string]interface{}{
				"status": model.LearningAttemptGrading, "lease_token": input.Attempt.LeaseToken,
				"lease_expires_at": input.LeaseEnds, "failure_reason": "",
			}).Error; err != nil {
				return err
			}
			if session.Status == model.LearningSessionWaitingAnswer {
				if err := tx.Model(&session).Updates(map[string]interface{}{
					"status": model.LearningSessionGrading, "version": gorm.Expr("version + 1"),
				}).Error; err != nil {
					return err
				}
				session.Status = model.LearningSessionGrading
				session.Version++
			}
			existing.Status = model.LearningAttemptGrading
			existing.LeaseToken = input.Attempt.LeaseToken
			existing.LeaseExpiresAt = &input.LeaseEnds
			result.Attempt = &existing
			result.NeedsGrading = true
			return loadClaimQuestion(tx, &result, input.Attempt.UserID)
		}
		if !errors.Is(existingErr, gorm.ErrRecordNotFound) {
			return existingErr
		}
		if session.Status == model.LearningSessionGrading {
			var stale model.LearningAttempt
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Where("user_id = ? AND learning_session_id = ? AND question_ordinal = ? AND status = ?", input.Attempt.UserID, input.Attempt.LearningSessionID, session.CurrentQuestion, model.LearningAttemptGrading).
				Order("updated_at DESC").First(&stale).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return ErrLearningStateConflict
				}
				return err
			}
			if stale.LeaseExpiresAt != nil && stale.LeaseExpiresAt.After(input.Now) {
				return ErrGradingInProgress
			}
			if err := tx.Model(&stale).Updates(map[string]interface{}{"status": model.LearningAttemptFailed, "failure_reason": "grading_lease_expired", "lease_token": "", "lease_expires_at": nil}).Error; err != nil {
				return err
			}
			if err := tx.Model(&session).Updates(map[string]interface{}{"status": model.LearningSessionWaitingAnswer, "version": gorm.Expr("version + 1")}).Error; err != nil {
				return err
			}
			session.Status = model.LearningSessionWaitingAnswer
			session.Version++
		}
		if session.Status != model.LearningSessionWaitingAnswer || session.Status.Terminal() {
			return ErrLearningStateConflict
		}
		var question model.LearningQuestion
		if err := tx.Where("id = ? AND user_id = ? AND learning_session_id = ?",
			input.Attempt.QuestionID, input.Attempt.UserID, input.Attempt.LearningSessionID).
			First(&question).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrLearningSessionNotFound
			}
			return err
		}
		if question.Ordinal != session.CurrentQuestion {
			return ErrLearningStateConflict
		}
		input.Attempt.QuestionOrdinal = question.Ordinal
		input.Attempt.TopicKey = session.TopicKey
		input.Attempt.Rubric = append([]string(nil), question.Rubric...)
		input.Attempt.Evidence = append([]model.LearningEvidence(nil), question.Evidence...)
		if err := input.Attempt.ValidateForClaim(); err != nil {
			return err
		}
		if err := tx.Create(&input.Attempt).Error; err != nil {
			if isDuplicateKey(err) {
				return ErrSubmissionConflict
			}
			return err
		}
		if err := tx.Model(&session).Updates(map[string]interface{}{
			"status": model.LearningSessionGrading, "version": gorm.Expr("version + 1"),
		}).Error; err != nil {
			return err
		}
		session.Status = model.LearningSessionGrading
		session.Version++
		result.Attempt = &input.Attempt
		result.Session = &session
		result.Question = &question
		result.NeedsGrading = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}

func loadClaimQuestion(tx *gorm.DB, result *ClaimAnswerResult, userID int64) error {
	var question model.LearningQuestion
	if err := tx.Where("id = ? AND user_id = ?", result.Attempt.QuestionID, userID).First(&question).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrLearningSessionNotFound
		}
		return err
	}
	result.Question = &question
	return nil
}

func (r *Repository) FailAnswer(ctx context.Context, userID int64, attemptID, leaseToken, reason string, now time.Time) (*model.LearningSession, error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	var session model.LearningSession
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var locator model.LearningAttempt
		if err := tx.Select("learning_session_id").Where("id = ? AND user_id = ?", attemptID, userID).First(&locator).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrAttemptNotFound
			}
			return err
		}
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND user_id = ?", locator.LearningSessionID, userID).First(&session).Error; err != nil {
			return err
		}
		var attempt model.LearningAttempt
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND user_id = ?", attemptID, userID).First(&attempt).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrAttemptNotFound
			}
			return err
		}
		if attempt.Status != model.LearningAttemptGrading || attempt.LeaseToken != leaseToken {
			return ErrLearningStateConflict
		}
		if err := tx.Model(&attempt).Updates(map[string]interface{}{
			"status": model.LearningAttemptFailed, "failure_reason": truncateDAO(reason, 160),
			"lease_token": "", "lease_expires_at": nil,
		}).Error; err != nil {
			return err
		}
		if session.Status == model.LearningSessionGrading && session.CurrentQuestion == attempt.QuestionOrdinal {
			if err := tx.Model(&session).Updates(map[string]interface{}{
				"status": model.LearningSessionWaitingAnswer, "version": gorm.Expr("version + 1"), "updated_at": now,
			}).Error; err != nil {
				return err
			}
			session.Status = model.LearningSessionWaitingAnswer
			session.Version++
		}
		return nil
	})
	return &session, err
}

func (r *Repository) CompleteAnswer(ctx context.Context, userID int64, attemptID, leaseToken string, grade GradeCommit, now time.Time) (*CompleteAnswerResult, error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	if err := validateGradeCommit(grade); err != nil {
		return nil, err
	}
	var result CompleteAnswerResult
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var locator model.LearningAttempt
		if err := tx.Select("learning_session_id").Where("id = ? AND user_id = ?", attemptID, userID).First(&locator).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrAttemptNotFound
			}
			return err
		}
		var session model.LearningSession
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND user_id = ?", locator.LearningSessionID, userID).First(&session).Error; err != nil {
			return err
		}
		var attempt model.LearningAttempt
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND user_id = ?", attemptID, userID).First(&attempt).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrAttemptNotFound
			}
			return err
		}
		if attempt.Status == model.LearningAttemptCompleted {
			result.Attempt = &attempt
			return nil
		}
		if attempt.Status != model.LearningAttemptGrading || attempt.LeaseToken != leaseToken {
			return ErrLearningStateConflict
		}
		if session.Status != model.LearningSessionGrading || session.CurrentQuestion != attempt.QuestionOrdinal {
			return ErrLearningStateConflict
		}
		total := grade.Correctness + grade.Completeness + grade.Clarity
		missingJSON, err := json.Marshal(grade.MissingPoints)
		if err != nil {
			return err
		}
		if err := tx.Model(&attempt).Updates(map[string]interface{}{
			"status": model.LearningAttemptCompleted, "auto_correctness": grade.Correctness,
			"auto_completeness": grade.Completeness, "auto_clarity": grade.Clarity,
			"auto_total": total, "effective_total": total, "feedback": grade.Feedback,
			"missing_points": string(missingJSON), "grading_confidence": grade.Confidence,
			"lease_token": "", "lease_expires_at": nil, "failure_reason": "", "completed_at": now,
		}).Error; err != nil {
			return err
		}
		attempt.Status = model.LearningAttemptCompleted
		attempt.AutoCorrectness, attempt.AutoCompleteness, attempt.AutoClarity = grade.Correctness, grade.Completeness, grade.Clarity
		attempt.AutoTotal, attempt.EffectiveTotal = total, total
		attempt.Feedback, attempt.MissingPoints, attempt.GradingConfidence = grade.Feedback, append([]string(nil), grade.MissingPoints...), grade.Confidence
		attempt.LeaseToken, attempt.LeaseExpiresAt, attempt.CompletedAt = "", nil, &now

		updates := map[string]interface{}{"version": gorm.Expr("version + 1")}
		if session.CurrentQuestion >= session.TotalQuestions {
			updates["status"] = model.LearningSessionCompleted
			updates["active_key"] = nil
			updates["completed_at"] = now
			session.Status, session.ActiveKey, session.CompletedAt = model.LearningSessionCompleted, nil, &now
		} else {
			updates["status"] = model.LearningSessionWaitingAnswer
			updates["current_question"] = gorm.Expr("current_question + 1")
			session.Status = model.LearningSessionWaitingAnswer
			session.CurrentQuestion++
		}
		if err := tx.Model(&session).Updates(updates).Error; err != nil {
			return err
		}
		session.Version++
		progress, err := rebuildTopicProgress(tx, userID, attempt.TopicKey, session.Topic, now)
		if err != nil {
			return err
		}
		if session.Status == model.LearningSessionWaitingAnswer {
			var next model.LearningQuestion
			if err := tx.Where("user_id = ? AND learning_session_id = ? AND ordinal = ?", userID, session.ID, session.CurrentQuestion).First(&next).Error; err != nil {
				return err
			}
			result.NextQuestion = &next
		}
		result.Attempt, result.Session, result.Progress = &attempt, &session, progress
		return nil
	})
	if err != nil {
		return nil, err
	}
	if result.Session == nil && result.Attempt != nil {
		result.Session, _ = r.GetByID(ctx, userID, result.Attempt.SessionID, result.Attempt.LearningSessionID)
		result.Progress, _ = r.GetTopicProgress(ctx, userID, result.Attempt.TopicKey)
	}
	return &result, nil
}

func (r *Repository) SkipQuestion(ctx context.Context, attempt *model.LearningAttempt, now time.Time) (*CompleteAnswerResult, error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	if attempt == nil || attempt.UserID <= 0 || strings.TrimSpace(attempt.SubmissionID) == "" {
		return nil, fmt.Errorf("invalid skip attempt")
	}
	var result CompleteAnswerResult
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing model.LearningAttempt
		err := tx.Where("user_id = ? AND submission_id = ?", attempt.UserID, attempt.SubmissionID).First(&existing).Error
		if err == nil {
			if existing.LearningSessionID != attempt.LearningSessionID || existing.QuestionID != attempt.QuestionID || existing.Status != model.LearningAttemptSkipped {
				return ErrSubmissionConflict
			}
			result.Attempt = &existing
			var session model.LearningSession
			if err := tx.Where("id = ? AND user_id = ?", existing.LearningSessionID, attempt.UserID).First(&session).Error; err != nil {
				return err
			}
			result.Session = &session
			if session.Status == model.LearningSessionWaitingAnswer {
				var next model.LearningQuestion
				if err := tx.Where("user_id = ? AND learning_session_id = ? AND ordinal = ?", attempt.UserID, session.ID, session.CurrentQuestion).First(&next).Error; err != nil {
					return err
				}
				result.NextQuestion = &next
			}
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		var session model.LearningSession
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND user_id = ? AND session_id = ?", attempt.LearningSessionID, attempt.UserID, attempt.SessionID).First(&session).Error; err != nil {
			return err
		}
		if session.Status != model.LearningSessionWaitingAnswer || session.CurrentQuestion != attempt.QuestionOrdinal {
			return ErrLearningStateConflict
		}
		var question model.LearningQuestion
		if err := tx.Where("id = ? AND user_id = ? AND learning_session_id = ? AND ordinal = ?", attempt.QuestionID, attempt.UserID, attempt.LearningSessionID, attempt.QuestionOrdinal).First(&question).Error; err != nil {
			return err
		}
		attempt.Status, attempt.TopicKey = model.LearningAttemptSkipped, session.TopicKey
		attempt.Rubric, attempt.Evidence, attempt.CompletedAt = question.Rubric, question.Evidence, &now
		if err := tx.Create(attempt).Error; err != nil {
			if isDuplicateKey(err) {
				return ErrSubmissionConflict
			}
			return err
		}
		updates := map[string]interface{}{"version": gorm.Expr("version + 1")}
		if session.CurrentQuestion >= session.TotalQuestions {
			updates["status"], updates["active_key"], updates["completed_at"] = model.LearningSessionCompleted, nil, now
			session.Status, session.ActiveKey, session.CompletedAt = model.LearningSessionCompleted, nil, &now
		} else {
			updates["current_question"] = gorm.Expr("current_question + 1")
			session.CurrentQuestion++
		}
		if err := tx.Model(&session).Updates(updates).Error; err != nil {
			return err
		}
		session.Version++
		if session.Status == model.LearningSessionWaitingAnswer {
			var next model.LearningQuestion
			if err := tx.Where("user_id = ? AND learning_session_id = ? AND ordinal = ?", attempt.UserID, session.ID, session.CurrentQuestion).First(&next).Error; err != nil {
				return err
			}
			result.NextQuestion = &next
		}
		result.Attempt, result.Session = attempt, &session
		return nil
	})
	return &result, err
}

func (r *Repository) GetTopicProgress(ctx context.Context, userID int64, topicKey string) (*model.TopicProgress, error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	var progress model.TopicProgress
	err := r.db.WithContext(ctx).Where("user_id = ? AND topic_key = ?", userID, topicKey).First(&progress).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrAttemptNotFound
	}
	return &progress, err
}

// ListTopicProgress returns a bounded slice of the user's topic progress,
// most recently assessed first. AGENT-017 uses it to match the current
// question against topics the learner has actually been graded on.
//
// The limit is a database guard, not a context budget: the caller matches and
// then keeps far fewer rows. Only the owning user's rows are ever visible.
func (r *Repository) ListTopicProgress(ctx context.Context, userID int64, limit int) ([]model.TopicProgress, error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	if userID <= 0 {
		return nil, fmt.Errorf("topic progress listing requires a trusted user id")
	}
	if limit < 1 || limit > 200 {
		return nil, fmt.Errorf("topic progress limit must be between 1 and 200")
	}
	var progress []model.TopicProgress
	err := r.db.WithContext(ctx).
		Where("user_id = ?", userID).
		Order("last_attempt_at DESC, updated_at DESC, id ASC").
		Limit(limit).
		Find(&progress).Error
	return progress, err
}

// GetProgressForLearningSession resolves the authoritative deterministic
// TopicProgress for a completed learning event. Dream uses this instead of
// accepting mastery fields generated by a language model.
func (r *Repository) GetProgressForLearningSession(ctx context.Context, userID int64, learningSessionID string) (*model.TopicProgress, error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	if userID <= 0 || strings.TrimSpace(learningSessionID) == "" {
		return nil, ErrLearningSessionNotFound
	}
	var session model.LearningSession
	err := r.db.WithContext(ctx).Where("id = ? AND user_id = ?", learningSessionID, userID).First(&session).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrLearningSessionNotFound
	}
	if err != nil {
		return nil, err
	}
	return r.GetTopicProgress(ctx, userID, session.TopicKey)
}

func (r *Repository) CorrectAttempt(ctx context.Context, userID int64, attemptID string, effectiveTotal int, reason string, now time.Time) (*model.LearningAttempt, *model.TopicProgress, error) {
	if err := r.ready(); err != nil {
		return nil, nil, err
	}
	if effectiveTotal < 0 || effectiveTotal > 100 || strings.TrimSpace(reason) == "" {
		return nil, nil, fmt.Errorf("invalid learning correction")
	}
	var attempt model.LearningAttempt
	var progress *model.TopicProgress
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND user_id = ?", attemptID, userID).First(&attempt).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrAttemptNotFound
			}
			return err
		}
		if attempt.Status != model.LearningAttemptCompleted {
			return ErrAttemptNotCorrectable
		}
		if err := tx.Model(&attempt).Updates(map[string]interface{}{
			"effective_total": effectiveTotal, "correction_reason": truncateDAO(reason, 500),
			"corrected_by_user_id": userID, "corrected_at": now,
		}).Error; err != nil {
			return err
		}
		attempt.EffectiveTotal, attempt.CorrectionReason, attempt.CorrectedByUserID, attempt.CorrectedAt = effectiveTotal, truncateDAO(reason, 500), userID, &now
		var session model.LearningSession
		if err := tx.Select("topic").Where("id = ? AND user_id = ?", attempt.LearningSessionID, userID).First(&session).Error; err != nil {
			return err
		}
		var err error
		progress, err = rebuildTopicProgress(tx, userID, attempt.TopicKey, session.Topic, now)
		return err
	})
	return &attempt, progress, err
}

func validateGradeCommit(grade GradeCommit) error {
	if grade.Correctness < 0 || grade.Correctness > 60 || grade.Completeness < 0 || grade.Completeness > 25 || grade.Clarity < 0 || grade.Clarity > 15 || grade.Confidence < 0 || grade.Confidence > 1 {
		return fmt.Errorf("learning grade is out of range")
	}
	if strings.TrimSpace(grade.Feedback) == "" || len([]rune(grade.Feedback)) > 2000 || len(grade.MissingPoints) > 8 {
		return fmt.Errorf("learning grade feedback is invalid")
	}
	for _, item := range grade.MissingPoints {
		if strings.TrimSpace(item) == "" || len([]rune(item)) > 300 {
			return fmt.Errorf("learning missing point is invalid")
		}
	}
	return nil
}

func rebuildTopicProgress(tx *gorm.DB, userID int64, topicKey, topic string, now time.Time) (*model.TopicProgress, error) {
	var attempts []model.LearningAttempt
	if err := tx.Where("user_id = ? AND topic_key = ? AND status = ?", userID, topicKey, model.LearningAttemptCompleted).
		Order("completed_at DESC, id DESC").Limit(10).Find(&attempts).Error; err != nil {
		return nil, err
	}
	progress := calculateTopicProgress(userID, topicKey, topic, attempts)
	err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "user_id"}, {Name: "topic_key"}}, DoUpdates: clause.AssignmentColumns([]string{"topic", "mastery_score", "confidence", "valid_attempts", "level", "weak_points", "last_attempt_at", "updated_at"})}).Create(progress).Error
	if err != nil {
		return nil, err
	}
	_ = now
	return progress, nil
}

func calculateTopicProgress(userID int64, topicKey, topic string, attempts []model.LearningAttempt) *model.TopicProgress {
	weighted, weights := 0, 0
	type weakness struct {
		text  string
		count int
		first int
	}
	weaknesses := map[string]*weakness{}
	for i, attempt := range attempts {
		weight := len(attempts) - i
		weighted += attempt.EffectiveTotal * weight
		weights += weight
		for _, raw := range attempt.MissingPoints {
			text := strings.TrimSpace(raw)
			if text == "" {
				continue
			}
			key := strings.ToLower(strings.Join(strings.Fields(text), " "))
			if item, ok := weaknesses[key]; ok {
				item.count++
			} else {
				weaknesses[key] = &weakness{text: text, count: 1, first: i}
			}
		}
	}
	mastery := 0
	if weights > 0 {
		mastery = int(math.Round(float64(weighted) / float64(weights)))
	}
	confidence := math.Min(1, float64(len(attempts))/6)
	level := model.TopicProgressDeveloping
	if len(attempts) < 3 {
		level = model.TopicProgressInsufficient
	} else if mastery >= 85 && confidence >= .5 {
		level = model.TopicProgressMastered
	}
	ordered := make([]*weakness, 0, len(weaknesses))
	for _, item := range weaknesses {
		ordered = append(ordered, item)
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].count != ordered[j].count {
			return ordered[i].count > ordered[j].count
		}
		return ordered[i].first < ordered[j].first
	})
	weakPoints := make([]string, 0, minDAO(8, len(ordered)))
	for i, item := range ordered {
		if i >= 8 {
			break
		}
		weakPoints = append(weakPoints, item.text)
	}
	progress := &model.TopicProgress{UserID: userID, TopicKey: topicKey, Topic: topic, MasteryScore: mastery, Confidence: confidence, ValidAttempts: len(attempts), Level: level, WeakPoints: weakPoints}
	if len(attempts) > 0 {
		progress.LastAttemptAt = attempts[0].CompletedAt
	}
	return progress
}

func truncateDAO(value string, limit int) string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) > limit {
		runes = runes[:limit]
	}
	return string(runes)
}
func minDAO(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ListRecentAttemptsByTopic returns the learner's graded attempts on one topic,
// most recent first.
//
// AGENT-021 uses it to answer questions the mastery score cannot: "we did three
// questions on this" is a fact about attempts, not about a level. Only completed
// attempts are returned — a skipped or failed one records that grading did not
// happen, which says nothing about what the learner knows.
func (r *Repository) ListRecentAttemptsByTopic(ctx context.Context, userID int64, topicKey string, limit int) ([]model.LearningAttempt, error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	if userID <= 0 || strings.TrimSpace(topicKey) == "" || limit < 1 || limit > 50 {
		return nil, fmt.Errorf("invalid attempt listing for topic")
	}
	var attempts []model.LearningAttempt
	err := r.db.WithContext(ctx).
		Where("user_id = ? AND topic_key = ? AND status = ?", userID, topicKey, model.LearningAttemptCompleted).
		Order("completed_at DESC, id DESC").
		Limit(limit).
		Find(&attempts).Error
	return attempts, err
}

// TopicAttemptRecord is one graded attempt with its question text attached.
//
// The attempt row records how the learner did; the question text lives in a
// separate table and until now was never joined. Both are needed: "答错了" is
// not teachable without "答错了什么题".
type TopicAttemptRecord struct {
	model.LearningAttempt
	QuestionText string
}

// ListTopicAttemptsWithQuestions returns the learner's graded attempts on one
// topic, most recent first, each carrying the text of the question asked.
//
// Only completed attempts appear: a skipped or failed one records that grading
// did not happen, which says nothing about what the learner knows.
func (r *Repository) ListTopicAttemptsWithQuestions(ctx context.Context, userID int64, topicKey string, limit int) ([]TopicAttemptRecord, error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	if userID <= 0 || strings.TrimSpace(topicKey) == "" || limit < 1 || limit > 50 {
		return nil, fmt.Errorf("invalid attempt listing for topic")
	}
	var attempts []model.LearningAttempt
	err := r.db.WithContext(ctx).
		Where("user_id = ? AND topic_key = ? AND status = ?", userID, topicKey, model.LearningAttemptCompleted).
		Order("completed_at DESC, id DESC").
		Limit(limit).
		Find(&attempts).Error
	if err != nil || len(attempts) == 0 {
		return nil, err
	}

	questionIDs := make([]string, 0, len(attempts))
	for _, attempt := range attempts {
		questionIDs = append(questionIDs, attempt.QuestionID)
	}
	var questions []model.LearningQuestion
	if err := r.db.WithContext(ctx).
		Where("user_id = ? AND id IN ?", userID, questionIDs).
		Find(&questions).Error; err != nil {
		return nil, err
	}
	textOf := make(map[string]string, len(questions))
	for _, question := range questions {
		// Ownership is re-checked on the rows themselves, not only in the query.
		if question.UserID == userID {
			textOf[question.ID] = question.Question
		}
	}

	records := make([]TopicAttemptRecord, 0, len(attempts))
	for _, attempt := range attempts {
		records = append(records, TopicAttemptRecord{
			LearningAttempt: attempt,
			QuestionText:    textOf[attempt.QuestionID],
		})
	}
	return records, nil
}

// ListRecentAttemptTexts returns the question text and grader notes from the
// learner's recent graded attempts across every topic.
//
// Matching a chat question against topic names alone misses the common case:
// learners ask about a sub-concept the topic name never contained. Someone
// quizzed on "RabbitMQ 消息确认" asks why autoAck is dangerous, and someone
// quizzed on "Go 并发" asks how goroutine and channel fit together. Those words
// are in the questions they were asked, so that is where to look for them.
//
// One query for all topics rather than one per topic: the caller is choosing
// between a handful of candidates and would otherwise issue a read for each.
func (r *Repository) ListRecentAttemptTexts(ctx context.Context, userID int64, limit int) ([]TopicAttemptRecord, error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	if userID <= 0 || limit < 1 || limit > 200 {
		return nil, fmt.Errorf("invalid attempt text listing")
	}
	var attempts []model.LearningAttempt
	err := r.db.WithContext(ctx).
		Where("user_id = ? AND status = ?", userID, model.LearningAttemptCompleted).
		Order("completed_at DESC, id DESC").
		Limit(limit).
		Find(&attempts).Error
	if err != nil || len(attempts) == 0 {
		return nil, err
	}
	questionIDs := make([]string, 0, len(attempts))
	for _, attempt := range attempts {
		questionIDs = append(questionIDs, attempt.QuestionID)
	}
	var questions []model.LearningQuestion
	if err := r.db.WithContext(ctx).
		Where("user_id = ? AND id IN ?", userID, questionIDs).
		Find(&questions).Error; err != nil {
		return nil, err
	}
	textOf := make(map[string]string, len(questions))
	for _, question := range questions {
		if question.UserID == userID {
			textOf[question.ID] = question.Question
		}
	}
	records := make([]TopicAttemptRecord, 0, len(attempts))
	for _, attempt := range attempts {
		records = append(records, TopicAttemptRecord{
			LearningAttempt: attempt,
			QuestionText:    textOf[attempt.QuestionID],
		})
	}
	return records, nil
}
