package learning

import (
	"GopherAI/common/mysql"
	"GopherAI/model"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
)

var (
	ErrLearningSessionNotFound = errors.New("learning session not found")
	ErrActiveLearningSession   = errors.New("an active learning session already exists")
	ErrLearningStateConflict   = errors.New("learning session state conflict")
	ErrInvalidTransition       = errors.New("invalid learning session transition")
	ErrSubmissionConflict      = errors.New("learning submission id conflicts with another answer")
	ErrGradingInProgress       = errors.New("learning answer is still being graded")
	ErrAttemptNotFound         = errors.New("learning attempt not found")
	ErrAttemptNotCorrectable   = errors.New("learning attempt cannot be corrected")
)

type Repository struct {
	db *gorm.DB
}

func NewRepository(db *gorm.DB) *Repository { return &Repository{db: db} }

func DefaultRepository() *Repository { return NewRepository(mysql.DB) }

func (r *Repository) CreateSession(ctx context.Context, session *model.LearningSession) error {
	if err := r.ready(); err != nil {
		return err
	}
	if err := session.ValidateForCreate(); err != nil {
		return err
	}
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var chat model.Session
		if err := tx.Where("id = ? AND user_id = ?", session.SessionID, session.UserID).First(&chat).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrLearningSessionNotFound
			}
			return err
		}
		if chat.Mode != model.SessionModeAgent {
			return ErrLearningSessionNotFound
		}
		if err := tx.Create(session).Error; err != nil {
			if isDuplicateKey(err) {
				return ErrActiveLearningSession
			}
			return err
		}
		return nil
	})
	return err
}

func (r *Repository) SaveQuestionsAndWait(
	ctx context.Context,
	userID int64,
	chatSessionID, learningSessionID string,
	expectedVersion uint64,
	questions []model.LearningQuestion,
) (*model.LearningSession, error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	if len(questions) == 0 || len(questions) > 5 {
		return nil, fmt.Errorf("learning questions must contain between 1 and 5 items")
	}
	for i := range questions {
		if questions[i].UserID != userID || questions[i].LearningSessionID != learningSessionID || questions[i].Ordinal != i+1 {
			return nil, fmt.Errorf("learning question ownership or order is invalid")
		}
		if err := questions[i].ValidateForCreate(); err != nil {
			return nil, err
		}
	}
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&model.LearningSession{}).
			Where("id = ? AND user_id = ? AND session_id = ? AND status = ? AND version = ?",
				learningSessionID, userID, chatSessionID, model.LearningSessionAsking, expectedVersion).
			Updates(map[string]interface{}{
				"status":  model.LearningSessionWaitingAnswer,
				"version": gorm.Expr("version + 1"),
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrLearningStateConflict
		}
		if err := tx.Create(&questions).Error; err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return r.GetByID(ctx, userID, chatSessionID, learningSessionID)
}

func (r *Repository) GetByID(ctx context.Context, userID int64, chatSessionID, learningSessionID string) (*model.LearningSession, error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	var session model.LearningSession
	err := r.db.WithContext(ctx).
		Where("id = ? AND user_id = ? AND session_id = ?", learningSessionID, userID, chatSessionID).
		First(&session).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrLearningSessionNotFound
	}
	if err != nil {
		return nil, err
	}
	return &session, nil
}

func (r *Repository) GetActive(ctx context.Context, userID int64, chatSessionID string) (*model.LearningSession, error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	activeKey := model.LearningActiveKey(userID, chatSessionID)
	var session model.LearningSession
	err := r.db.WithContext(ctx).
		Where("active_key = ? AND user_id = ? AND session_id = ?", activeKey, userID, chatSessionID).
		First(&session).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrLearningSessionNotFound
	}
	if err != nil {
		return nil, err
	}
	return &session, nil
}

func (r *Repository) GetQuestion(ctx context.Context, userID int64, learningSessionID string, ordinal int) (*model.LearningQuestion, error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	var question model.LearningQuestion
	err := r.db.WithContext(ctx).
		Where("user_id = ? AND learning_session_id = ? AND ordinal = ?", userID, learningSessionID, ordinal).
		First(&question).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrLearningSessionNotFound
	}
	if err != nil {
		return nil, err
	}
	return &question, nil
}

func (r *Repository) Transition(
	ctx context.Context,
	userID int64,
	chatSessionID, learningSessionID string,
	expectedVersion uint64,
	from, to model.LearningSessionStatus,
) (*model.LearningSession, error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	if !from.CanTransition(to) {
		return nil, fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, from, to)
	}
	updates := map[string]interface{}{
		"status":  to,
		"version": gorm.Expr("version + 1"),
	}
	if to.Terminal() {
		now := time.Now()
		updates["completed_at"] = &now
		updates["active_key"] = nil
	}
	result := r.db.WithContext(ctx).Model(&model.LearningSession{}).
		Where("id = ? AND user_id = ? AND session_id = ? AND status = ? AND version = ?",
			learningSessionID, userID, chatSessionID, from, expectedVersion).
		Updates(updates)
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		if _, err := r.GetByID(ctx, userID, chatSessionID, learningSessionID); err != nil {
			return nil, err
		}
		return nil, ErrLearningStateConflict
	}
	return r.GetByID(ctx, userID, chatSessionID, learningSessionID)
}

func (r *Repository) ExpireIfNeeded(ctx context.Context, session *model.LearningSession, now time.Time) (*model.LearningSession, error) {
	if session == nil {
		return nil, ErrLearningSessionNotFound
	}
	if session.Status.Terminal() || session.ExpiresAt.After(now) {
		return session, nil
	}
	return r.Transition(ctx, session.UserID, session.SessionID, session.ID, session.Version, session.Status, model.LearningSessionExpired)
}

func (r *Repository) ready() error {
	if r == nil || r.db == nil {
		return fmt.Errorf("learning repository is not initialized")
	}
	return nil
}

func isDuplicateKey(err error) bool {
	var mysqlErr *mysqldriver.MySQLError
	if errors.As(err, &mysqlErr) && mysqlErr.Number == 1062 {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "duplicate")
}
