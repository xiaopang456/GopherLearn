package gormmysql

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/repository"
)

// The learning store on the production engine.
//
// Two rules shape everything below. First, a quiz's state only ever changes through
// a write conditioned on (status, version) with its RowsAffected checked: no
// read-modify-write, no reliance on a row lock that a different engine may treat as
// a suggestion. Second, the composite steps - questions with the transition that
// opens them, a grade with the advance and the mastery recomputation - are single
// transactions, because the interesting failures here are all partial ones: half a
// question set reads as a quiz with missing items, and a stored score whose
// projection was never rebuilt makes the next explanation describe a learner who
// has already answered something else.

// orderByLearningAttemptCompletion is the mastery window's order: newest finished
// first. The id is the tie-breaker because generated ids carry a fixed-width
// nanosecond timestamp, so two attempts completed in the same instant still have one
// defined order on every engine - and it is the same order the in-memory store gets
// from walking its insertion list.
const orderByLearningAttemptCompletion = "completed_at DESC, id ASC"

// LearningSessionRecord is one quiz.
type LearningSessionRecord struct {
	ID              string `gorm:"column:id;type:varchar(96);primaryKey"`
	UserID          string `gorm:"column:user_id;type:varchar(96);not null;index:idx_learning_open,priority:1"`
	SessionID       string `gorm:"column:session_id;type:varchar(96);not null;index:idx_learning_open,priority:2"`
	Topic           string `gorm:"column:topic;type:varchar(192);not null"`
	TopicKey        string `gorm:"column:topic_key;type:varchar(192);not null;index:idx_learning_topic"`
	Status          string `gorm:"column:status;type:varchar(24);not null;index:idx_learning_open,priority:3"`
	CurrentQuestion int    `gorm:"column:current_question;type:int;not null"`
	TotalQuestions  int    `gorm:"column:total_questions;type:int;not null"`
	Version         uint64 `gorm:"column:version;not null"`
	GenerationToken string `gorm:"column:generation_token;type:varchar(96);not null;default:''"`
	// GenerationExpiresAt is nullable because most quizzes are not generating: an empty
	// string or a zero time would make "no claim" and "claim that expired at the
	// epoch" indistinguishable on an engine that rounds timestamps.
	GenerationExpiresAt *time.Time `gorm:"column:generation_expires_at"`
	ExpiresAt           time.Time  `gorm:"column:expires_at;not null;index:idx_learning_expiry"`
	CreatedAt           time.Time  `gorm:"column:created_at;not null"`
	UpdatedAt           time.Time  `gorm:"column:updated_at;not null"`
	CompletedAt         *time.Time `gorm:"column:completed_at"`
	// ActiveKey holds "user:chat session" while a quiz is unfinished and NULL once it
	// is not. A nullable unique column is how "exactly one open quiz per conversation"
	// is decided by the database rather than by two callers each reading "none yet";
	// NULLs do not collide, so finished quizzes do not fight over the index.
	ActiveKey *string `gorm:"column:active_key;type:varchar(200);uniqueIndex:uniq_learning_open"`
}

func (LearningSessionRecord) TableName() string { return "learning_sessions" }

func (r LearningSessionRecord) toDomain() (domain.LearningSession, error) {
	status := domain.LearningSessionStatus(r.Status)
	if !status.IsValid() {
		return domain.LearningSession{}, fmt.Errorf("%w: learning session %q has status %q",
			errCorruptStoredData, r.ID, r.Status)
	}
	return domain.LearningSession{
		ID: r.ID, UserID: r.UserID, SessionID: r.SessionID,
		Topic: r.Topic, TopicKey: r.TopicKey, Status: status,
		CurrentQuestion: r.CurrentQuestion, TotalQuestions: r.TotalQuestions, Version: r.Version,
		GenerationToken:     r.GenerationToken,
		GenerationExpiresAt: r.GenerationExpiresAt,
		ExpiresAt:           r.ExpiresAt, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
		CompletedAt: r.CompletedAt,
	}, nil
}

// LearningQuestionRecord is one generated question. Rubric and Evidence are stored as
// JSON rather than as child tables, which is a deliberate departure from how
// citations and memory provenance are kept: both lists are read only as the whole
// set belonging to one question, they are never counted or joined across questions,
// and the set is copied verbatim onto an attempt so a grade stays reproducible after
// the source document is edited. A child table would double the write path to gain
// an index nobody queries.
type LearningQuestionRecord struct {
	ID                string                    `gorm:"column:id;type:varchar(96);primaryKey"`
	UserID            string                    `gorm:"column:user_id;type:varchar(96);not null;index:idx_learning_question_user"`
	LearningSessionID string                    `gorm:"column:learning_session_id;type:varchar(96);not null;uniqueIndex:uniq_learning_question,priority:1"`
	Ordinal           int                       `gorm:"column:ordinal;type:int;not null;uniqueIndex:uniq_learning_question,priority:2"`
	Type              string                    `gorm:"column:type;type:varchar(32);not null"`
	Question          string                    `gorm:"column:question;type:text"`
	AssessmentGoal    string                    `gorm:"column:assessment_goal;type:varchar(256);not null;default:''"`
	ReferenceAnswer   string                    `gorm:"column:reference_answer;type:text"`
	Rubric            []string                  `gorm:"column:rubric;serializer:json;type:text"`
	Evidence          []domain.LearningEvidence `gorm:"column:evidence;serializer:json;type:text"`
	CreatedAt         time.Time                 `gorm:"column:created_at;not null"`
}

func (LearningQuestionRecord) TableName() string { return "learning_questions" }

func (r LearningQuestionRecord) toDomain() (domain.LearningQuestion, error) {
	kind := domain.LearningQuestionType(r.Type)
	if !kind.IsValid() {
		return domain.LearningQuestion{}, fmt.Errorf("%w: learning question %q has type %q",
			errCorruptStoredData, r.ID, r.Type)
	}
	return domain.LearningQuestion{
		ID: r.ID, UserID: r.UserID, LearningSessionID: r.LearningSessionID, Ordinal: r.Ordinal,
		Type: kind, Question: r.Question, AssessmentGoal: r.AssessmentGoal,
		ReferenceAnswer: r.ReferenceAnswer,
		Rubric:          append([]string(nil), r.Rubric...),
		Evidence:        append([]domain.LearningEvidence(nil), r.Evidence...),
		CreatedAt:       r.CreatedAt,
	}, nil
}

// LearningAttemptRecord is one answer and what became of it. Rubric and Evidence are
// the question's copies, taken at claim time.
type LearningAttemptRecord struct {
	ID                string                    `gorm:"column:id;type:varchar(96);primaryKey"`
	UserID            string                    `gorm:"column:user_id;type:varchar(96);not null;uniqueIndex:uniq_learning_submission,priority:1"`
	SessionID         string                    `gorm:"column:session_id;type:varchar(96);not null;index:idx_learning_attempt_session"`
	LearningSessionID string                    `gorm:"column:learning_session_id;type:varchar(96);not null;index:idx_learning_attempt_quiz"`
	QuestionID        string                    `gorm:"column:question_id;type:varchar(96);not null;index:idx_learning_attempt_question"`
	QuestionOrdinal   int                       `gorm:"column:question_ordinal;type:int;not null"`
	SubmissionID      string                    `gorm:"column:submission_id;type:varchar(96);not null;uniqueIndex:uniq_learning_submission,priority:2"`
	TopicKey          string                    `gorm:"column:topic_key;type:varchar(192);not null;index:idx_learning_attempt_topic"`
	Status            string                    `gorm:"column:status;type:varchar(24);not null;index:idx_learning_attempt_status"`
	Answer            string                    `gorm:"column:answer;type:mediumtext"`
	Rubric            []string                  `gorm:"column:rubric;serializer:json;type:text"`
	Evidence          []domain.LearningEvidence `gorm:"column:evidence;serializer:json;type:text"`
	Correctness       int                       `gorm:"column:correctness;type:int;not null;default:0"`
	Completeness      int                       `gorm:"column:completeness;type:int;not null;default:0"`
	Clarity           int                       `gorm:"column:clarity;type:int;not null;default:0"`
	AutoTotal         int                       `gorm:"column:auto_total;type:int;not null;default:0"`
	EffectiveTotal    int                       `gorm:"column:effective_total;type:int;not null;default:0"`
	Feedback          string                    `gorm:"column:feedback;type:text"`
	MissingPoints     []string                  `gorm:"column:missing_points;serializer:json;type:text"`
	GradingConfidence float64                   `gorm:"column:grading_confidence;type:double;not null;default:0"`
	LeaseToken        string                    `gorm:"column:lease_token;type:varchar(96);not null;default:''"`
	LeaseExpiresAt    *time.Time                `gorm:"column:lease_expires_at;index:idx_learning_attempt_lease"`
	FailureCode       string                    `gorm:"column:failure_code;type:varchar(32);not null;default:''"`
	GradingAt         *time.Time                `gorm:"column:grading_at"`
	CompletedAt       *time.Time                `gorm:"column:completed_at;index:idx_learning_attempt_completed"`
	CreatedAt         time.Time                 `gorm:"column:created_at;not null"`
}

func (LearningAttemptRecord) TableName() string { return "learning_attempts" }

func (r LearningAttemptRecord) toDomain() (domain.LearningAttempt, error) {
	status := domain.LearningAttemptStatus(r.Status)
	if !status.IsValid() {
		return domain.LearningAttempt{}, fmt.Errorf("%w: learning attempt %q has status %q",
			errCorruptStoredData, r.ID, r.Status)
	}
	code := domain.LearningFailureCode(r.FailureCode)
	if r.FailureCode != "" && !code.IsValid() {
		return domain.LearningAttempt{}, fmt.Errorf("%w: learning attempt %q has failure code %q",
			errCorruptStoredData, r.ID, r.FailureCode)
	}
	return domain.LearningAttempt{
		ID: r.ID, UserID: r.UserID, SessionID: r.SessionID, LearningSessionID: r.LearningSessionID,
		QuestionID: r.QuestionID, QuestionOrdinal: r.QuestionOrdinal,
		SubmissionID: r.SubmissionID, TopicKey: r.TopicKey, Status: status, Answer: r.Answer,
		Rubric:   append([]string(nil), r.Rubric...),
		Evidence: append([]domain.LearningEvidence(nil), r.Evidence...),
		Grade: domain.LearningGrade{
			Correctness: r.Correctness, Completeness: r.Completeness, Clarity: r.Clarity,
			Feedback: r.Feedback, MissingPoints: append([]string(nil), r.MissingPoints...),
			Confidence: r.GradingConfidence,
		},
		EffectiveTotal: r.EffectiveTotal, FailureCode: code,
		LeaseToken: r.LeaseToken, LeaseExpiresAt: r.LeaseExpiresAt,
		GradingAt: r.GradingAt, CompletedAt: r.CompletedAt, CreatedAt: r.CreatedAt,
	}, nil
}

// TopicProgressRecord is one topic's projection. It is derived storage: it can be
// rebuilt from learning_attempts, which is why nothing in here accepts a value a
// caller supplied.
type TopicProgressRecord struct {
	ID          string     `gorm:"column:id;type:varchar(96);primaryKey"`
	UserID      string     `gorm:"column:user_id;type:varchar(96);not null;uniqueIndex:uniq_topic_progress,priority:1"`
	TopicKey    string     `gorm:"column:topic_key;type:varchar(192);not null;uniqueIndex:uniq_topic_progress,priority:2"`
	Topic       string     `gorm:"column:topic;type:varchar(192);not null"`
	Mastery     int        `gorm:"column:mastery_score;type:int;not null;default:0"`
	Confidence  float64    `gorm:"column:confidence;type:double;not null;default:0"`
	ValidCount  int        `gorm:"column:valid_attempts;type:int;not null;default:0"`
	Level       string     `gorm:"column:level;type:varchar(24);not null"`
	WeakPoints  []string   `gorm:"column:weak_points;serializer:json;type:text"`
	LastAttempt *time.Time `gorm:"column:last_attempt_at"`
	CreatedAt   time.Time  `gorm:"column:created_at;not null"`
	UpdatedAt   time.Time  `gorm:"column:updated_at;not null"`
}

func (TopicProgressRecord) TableName() string { return "topic_progress" }

func (r TopicProgressRecord) toDomain() (domain.TopicProgress, error) {
	level := domain.TopicProgressLevel(r.Level)
	if !level.IsValid() {
		return domain.TopicProgress{}, fmt.Errorf("%w: topic progress for %q has level %q",
			errCorruptStoredData, r.TopicKey, r.Level)
	}
	return domain.TopicProgress{
		ID: r.ID, UserID: r.UserID, TopicKey: r.TopicKey, Topic: r.Topic,
		Mastery: r.Mastery, Confidence: r.Confidence, ValidCount: r.ValidCount, Level: level,
		WeakPoints:  append([]string(nil), r.WeakPoints...),
		LastAttempt: r.LastAttempt, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}, nil
}

// CreateLearningSession stores a new quiz. The active-key uniqueness is the whole
// concurrency story: two simultaneous Starts produce one quiz and one refusal, and
// neither of them had to check first.
func (r *Repository) CreateLearningSession(ctx context.Context, session *domain.LearningSession) (*domain.LearningSession, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if session == nil {
		return nil, repository.ErrInvalidArgument
	}
	if err := session.ValidateForCreate(); err != nil {
		return nil, repository.ErrInvalidArgument
	}

	record := LearningSessionRecord{
		UserID: session.UserID, SessionID: session.SessionID,
		Topic: session.Topic, TopicKey: session.TopicKey,
		Status: string(session.Status), CurrentQuestion: session.CurrentQuestion,
		TotalQuestions: session.TotalQuestions, Version: session.Version,
		ExpiresAt: session.ExpiresAt.UTC(),
	}
	if session.ID == "" {
		record.ID = newID("lsn")
	} else {
		record.ID = session.ID
	}
	now := time.Now().UTC()
	record.CreatedAt, record.UpdatedAt = now, now
	active := session.ActiveKey()
	record.ActiveKey = &active

	err := r.transaction(ctx, func(tx *gorm.DB) error {
		if err := requireOwnedSession(tx, record.UserID, record.SessionID); err != nil {
			return err
		}
		return tx.Create(&record).Error
	})
	if err != nil {
		return nil, learningDuplicate(err, repository.ErrLearningSessionActive)
	}
	stored, err := record.toDomain()
	if err != nil {
		return nil, err
	}
	return &stored, nil
}

// GetLearningSession reads one of the caller's own quizzes.
func (r *Repository) GetLearningSession(ctx context.Context, userID, learningSessionID string) (*domain.LearningSession, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(learningSessionID) == "" {
		return nil, repository.ErrInvalidArgument
	}
	var record LearningSessionRecord
	err := r.transaction(ctx, func(tx *gorm.DB) error {
		return tx.Where("id = ? AND user_id = ?", learningSessionID, userID).Take(&record).Error
	})
	if err != nil {
		return nil, translate(err)
	}
	session, err := record.toDomain()
	if err != nil {
		return nil, err
	}
	return &session, nil
}

// GetActiveLearningSession returns the quiz currently open on one conversation.
func (r *Repository) GetActiveLearningSession(ctx context.Context, userID, chatSessionID string) (*domain.LearningSession, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(chatSessionID) == "" {
		return nil, repository.ErrInvalidArgument
	}
	var record LearningSessionRecord
	err := r.transaction(ctx, func(tx *gorm.DB) error {
		// The open marker is indexed and unique, so this is one row lookup rather than
		// a scan of statuses - and it cannot return two.
		return tx.Where("active_key = ?", userID+":"+chatSessionID).Take(&record).Error
	})
	if err != nil {
		return nil, translate(err)
	}
	session, err := record.toDomain()
	if err != nil {
		return nil, err
	}
	return &session, nil
}

// TransitionLearningSession moves one quiz along a legal edge at the version the
// caller read.
func (r *Repository) TransitionLearningSession(ctx context.Context, userID, learningSessionID string,
	from, to domain.LearningSessionStatus, expectVersion uint64, lease repository.LearningLease) (*domain.LearningSession, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(learningSessionID) == "" {
		return nil, repository.ErrInvalidArgument
	}
	if !from.CanTransitionTo(to) {
		return nil, repository.ErrLearningTransition
	}
	if to == domain.LearningSessionAsking && (lease.Token == "" || lease.Until.IsZero()) {
		return nil, repository.ErrInvalidArgument
	}

	var updated domain.LearningSession
	err := r.transaction(ctx, func(tx *gorm.DB) error {
		now := time.Now().UTC()
		if err := r.recoverLearningTx(tx, userID, learningSessionID, now); err != nil {
			return err
		}
		var record LearningSessionRecord
		if err := tx.Where("id = ? AND user_id = ?", learningSessionID, userID).Take(&record).Error; err != nil {
			return translate(err)
		}
		if record.Status != string(from) {
			if domain.LearningSessionStatus(record.Status).IsTerminal() {
				// Recovery closed this quiz between the read and the write. That is not a
				// race the caller can win by re-reading: the quiz is finished, and saying
				// so is the only honest answer.
				return repository.ErrLearningQuizClosed
			}
			return repository.ErrLearningStaleWrite
		}
		if record.Version != expectVersion {
			return repository.ErrLearningStaleWrite
		}
		if !record.ExpiresAt.After(now) {
			return repository.ErrLearningQuizClosed
		}

		fields := map[string]any{
			"status": string(to), "version": gorm.Expr("version + 1"), "updated_at": now,
		}
		if to == domain.LearningSessionAsking {
			deadline := lease.Until.UTC()
			fields["generation_token"] = lease.Token
			fields["generation_expires_at"] = &deadline
		}
		if from == domain.LearningSessionAsking {
			fields["generation_token"] = ""
			fields["generation_expires_at"] = nil
		}
		if to.IsTerminal() {
			fields["active_key"] = nil
			if to == domain.LearningSessionCompleted {
				fields["completed_at"] = now
			}
		}
		result := tx.Model(&LearningSessionRecord{}).
			Where("id = ? AND user_id = ? AND status = ? AND version = ?", learningSessionID, userID, string(from), expectVersion).
			Updates(fields)
		if result.Error != nil {
			return translate(result.Error)
		}
		if result.RowsAffected == 0 {
			// Somebody else moved this row between the read above and the write here.
			// Saying so is the whole point of checking RowsAffected: the alternative is
			// a success that describes a state nobody wrote.
			return repository.ErrLearningStaleWrite
		}
		refreshed, err := r.readLearningSessionTx(tx, userID, learningSessionID)
		if err != nil {
			return err
		}
		updated = *refreshed
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &updated, nil
}

// StoreLearningQuestions inserts a quiz's questions and opens it for answers.
func (r *Repository) StoreLearningQuestions(ctx context.Context, userID, learningSessionID string,
	questions []domain.LearningQuestion, expectVersion uint64) (*domain.LearningSession, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(learningSessionID) == "" {
		return nil, repository.ErrInvalidArgument
	}
	if len(questions) != domain.LearningQuestionCount {
		return nil, repository.ErrInvalidArgument
	}

	var updated domain.LearningSession
	err := r.transaction(ctx, func(tx *gorm.DB) error {
		now := time.Now().UTC()
		record, err := r.readLearningSessionTx(tx, userID, learningSessionID)
		if err != nil {
			return err
		}
		if record.Status != domain.LearningSessionAsking {
			return repository.ErrLearningStaleWrite
		}
		if record.Version != expectVersion {
			return repository.ErrLearningStaleWrite
		}
		if record.IsExpired(now) {
			return repository.ErrLearningQuizClosed
		}

		seen := make(map[int]struct{}, len(questions))
		rows := make([]LearningQuestionRecord, 0, len(questions))
		for index := range questions {
			question := questions[index]
			if question.UserID != userID || question.LearningSessionID != learningSessionID {
				return repository.ErrInvalidArgument
			}
			if _, duplicate := seen[question.Ordinal]; duplicate {
				return repository.ErrInvalidArgument
			}
			seen[question.Ordinal] = struct{}{}
			if question.ID == "" {
				question.ID = newID("lq")
			}
			if question.CreatedAt.IsZero() {
				question.CreatedAt = now
			}
			if err := question.ValidateForCreate(); err != nil {
				return repository.ErrInvalidArgument
			}
			rows = append(rows, LearningQuestionRecord{
				ID: question.ID, UserID: question.UserID, LearningSessionID: question.LearningSessionID,
				Ordinal: question.Ordinal, Type: string(question.Type), Question: question.Question,
				AssessmentGoal: question.AssessmentGoal, ReferenceAnswer: question.ReferenceAnswer,
				Rubric: cloneLearningStrings(question.Rubric), Evidence: cloneLearningEvidence(question.Evidence),
				CreatedAt: question.CreatedAt.UTC(),
			})
		}
		for ordinal := 1; ordinal <= len(rows); ordinal++ {
			if _, exists := seen[ordinal]; !exists {
				return repository.ErrInvalidArgument
			}
		}

		if err := tx.Create(&rows).Error; err != nil {
			return learningDuplicate(err, repository.ErrConflict)
		}
		result := tx.Model(&LearningSessionRecord{}).
			Where("id = ? AND user_id = ? AND status = ? AND version = ?",
				learningSessionID, userID, string(domain.LearningSessionAsking), expectVersion).
			Updates(map[string]any{
				"status": string(domain.LearningSessionWaitingAnswer), "version": gorm.Expr("version + 1"),
				"updated_at": now, "generation_token": "", "generation_expires_at": nil,
			})
		if result.Error != nil {
			return translate(result.Error)
		}
		if result.RowsAffected == 0 {
			// Nothing is inserted either: the question rows and this update are one
			// transaction, so a lost race leaves no half a quiz behind.
			return repository.ErrLearningStaleWrite
		}
		refreshed, err := r.readLearningSessionTx(tx, userID, learningSessionID)
		if err != nil {
			return err
		}
		updated = *refreshed
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &updated, nil
}

// ListLearningQuestions returns a quiz's questions in the order they are asked.
func (r *Repository) ListLearningQuestions(ctx context.Context, userID, learningSessionID string) ([]domain.LearningQuestion, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(learningSessionID) == "" {
		return nil, repository.ErrInvalidArgument
	}

	var rows []LearningQuestionRecord
	var exists int64
	err := r.transaction(ctx, func(tx *gorm.DB) error {
		if err := tx.Model(&LearningSessionRecord{}).
			Where("id = ? AND user_id = ?", learningSessionID, userID).Count(&exists).Error; err != nil {
			return err
		}
		if exists == 0 {
			return repository.ErrNotFound
		}
		return tx.Where("learning_session_id = ? AND user_id = ?", learningSessionID, userID).
			Order("ordinal ASC").Find(&rows).Error
	})
	if err != nil {
		return nil, translate(err)
	}
	questions := make([]domain.LearningQuestion, 0, len(rows))
	for _, row := range rows {
		question, err := row.toDomain()
		if err != nil {
			return nil, err
		}
		questions = append(questions, question)
	}
	return questions, nil
}

// ListRecentTopicQuestions returns exposure history for one topic across every quiz
// the caller started, newest first.
func (r *Repository) ListRecentTopicQuestions(ctx context.Context, userID, topicKey string, limit int) ([]domain.LearningQuestion, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(topicKey) == "" || limit <= 0 {
		return nil, repository.ErrInvalidArgument
	}

	var rows []LearningQuestionRecord
	err := r.transaction(ctx, func(tx *gorm.DB) error {
		// The topic lives on the quiz, so the join is what makes "my history for this
		// topic" mean the same thing here that it does in memory. Status is not a
		// filter: a question that was generated and skipped was still seen.
		return tx.Table("learning_questions").
			Select("learning_questions.*").
			Joins("JOIN learning_sessions ON learning_sessions.id = learning_questions.learning_session_id").
			Where("learning_questions.user_id = ? AND learning_sessions.topic_key = ?", userID, topicKey).
			Order("learning_questions.created_at DESC, learning_questions.id DESC").
			Limit(limit).Find(&rows).Error
	})
	if err != nil {
		return nil, translate(err)
	}
	questions := make([]domain.LearningQuestion, 0, len(rows))
	for _, row := range rows {
		question, err := row.toDomain()
		if err != nil {
			return nil, err
		}
		questions = append(questions, question)
	}
	return questions, nil
}

// BeginLearningAttempt stores one answer and claims the quiz for grading.
func (r *Repository) BeginLearningAttempt(ctx context.Context, attempt *domain.LearningAttempt) (repository.LearningAttemptClaim, error) {
	if err := checkContext(ctx); err != nil {
		return repository.LearningAttemptClaim{}, err
	}
	if attempt == nil {
		return repository.LearningAttemptClaim{}, repository.ErrInvalidArgument
	}
	claimed := *attempt
	if err := claimed.ValidateForClaim(); err != nil {
		return repository.LearningAttemptClaim{}, repository.ErrInvalidArgument
	}

	var result repository.LearningAttemptClaim
	err := r.transaction(ctx, func(tx *gorm.DB) error {
		now := time.Now().UTC()
		if err := r.recoverLearningTx(tx, claimed.UserID, claimed.LearningSessionID, now); err != nil {
			return err
		}
		record, err := r.readLearningSessionTx(tx, claimed.UserID, claimed.LearningSessionID)
		if err != nil {
			return err
		}
		// The submission is settled before anything about the quiz's current position. A
		// client retrying an answer it already sent must get the stored outcome even
		// after the quiz has moved on; otherwise a response lost in transit turns into a
		// complaint about ordering, and the caller retries under a new id and is graded
		// twice for one answer.
		var existing LearningAttemptRecord
		lookup := tx.Where("user_id = ? AND submission_id = ?", claimed.UserID, claimed.SubmissionID).Take(&existing)
		if lookup.Error == nil {
			outcome, handled, err := r.reconcileSubmission(existing, claimed, *record)
			if err != nil {
				return err
			}
			if handled {
				result = outcome
				return nil
			}
		} else if !errors.Is(lookup.Error, gorm.ErrRecordNotFound) {
			return translate(lookup.Error)
		}

		switch record.Status {
		case domain.LearningSessionGrading:
			// recoverLearningTx already failed any attempt whose lease had run out, and
			// the lookup above settled anything already in flight, so a quiz still
			// grading here is grading for real.
			return repository.ErrLearningAnswerBusy
		case domain.LearningSessionWaitingAnswer:
		default:
			return repository.ErrLearningQuizClosed
		}
		if claimed.QuestionOrdinal != record.CurrentQuestion ||
			claimed.QuestionOrdinal < 1 || claimed.QuestionOrdinal > record.TotalQuestions {
			return repository.ErrLearningQuestionOrder
		}
		var question LearningQuestionRecord
		if err := tx.Where("learning_session_id = ? AND user_id = ? AND ordinal = ? AND id = ?",
			record.ID, claimed.UserID, claimed.QuestionOrdinal, claimed.QuestionID).
			Take(&question).Error; err != nil {
			return translate(err)
		}

		if claimed.ID == "" {
			claimed.ID = newID("lat")
		}
		claimed.CreatedAt = now
		if claimed.GradingAt == nil {
			stamped := now
			claimed.GradingAt = &stamped
		}
		// The rubric and evidence the question carries are copied onto the attempt here,
		// so a grade is reproducible from this row alone even after the document behind
		// the question is edited.
		if err := tx.Create(&LearningAttemptRecord{
			ID: claimed.ID, UserID: claimed.UserID, SessionID: claimed.SessionID,
			LearningSessionID: claimed.LearningSessionID, QuestionID: claimed.QuestionID,
			QuestionOrdinal: claimed.QuestionOrdinal, SubmissionID: claimed.SubmissionID,
			TopicKey: claimed.TopicKey, Status: string(claimed.Status), Answer: claimed.Answer,
			Rubric: cloneLearningStrings(question.Rubric), Evidence: cloneLearningEvidence(question.Evidence),
			LeaseToken: claimed.LeaseToken, LeaseExpiresAt: utcPointer(claimed.LeaseExpiresAt),
			GradingAt: utcPointer(claimed.GradingAt), CreatedAt: now,
		}).Error; err != nil {
			return learningDuplicate(err, repository.ErrLearningSubmissionConflict)
		}
		claimed.Rubric = cloneLearningStrings(question.Rubric)
		claimed.Evidence = cloneLearningEvidence(question.Evidence)

		if err := r.writeLearningStatusTx(tx, record.ID, claimed.UserID,
			domain.LearningSessionWaitingAnswer, domain.LearningSessionGrading, record.Version, now); err != nil {
			return err
		}
		refreshed, err := r.readLearningSessionTx(tx, claimed.UserID, claimed.LearningSessionID)
		if err != nil {
			return err
		}
		stored, err := r.readLearningAttemptTx(tx, claimed.UserID, claimed.SubmissionID)
		if err != nil {
			return err
		}
		result = repository.LearningAttemptClaim{Attempt: *stored, NeedsGrading: true, Session: *refreshed}
		return nil
	})
	if err != nil {
		return repository.LearningAttemptClaim{}, err
	}
	return result, nil
}

// reconcileSubmission decides what a repeat of the same submission id means: the
// stored outcome, a refusal, or "carry on and insert".
//
// handled is false only when the caller may proceed. A failed attempt is not
// retryable under the same id: that row is the record that this answer was scored
// and could not be, and a second row with the same key would erase it.
func (r *Repository) reconcileSubmission(existing LearningAttemptRecord, claimed domain.LearningAttempt,
	record domain.LearningSession) (repository.LearningAttemptClaim, bool, error) {
	if existing.LearningSessionID != claimed.LearningSessionID ||
		existing.QuestionOrdinal != claimed.QuestionOrdinal || existing.Answer != claimed.Answer {
		return repository.LearningAttemptClaim{}, true, repository.ErrLearningSubmissionConflict
	}
	switch domain.LearningAttemptStatus(existing.Status) {
	case domain.LearningAttemptCompleted, domain.LearningAttemptSkipped:
		attempt, err := existing.toDomain()
		if err != nil {
			return repository.LearningAttemptClaim{}, true, err
		}
		return repository.LearningAttemptClaim{Attempt: attempt, NeedsGrading: false, Session: record}, true, nil
	case domain.LearningAttemptGrading:
		return repository.LearningAttemptClaim{}, true, repository.ErrLearningAnswerBusy
	default:
		return repository.LearningAttemptClaim{}, true, repository.ErrLearningSubmissionConflict
	}
}

// CompleteLearningAttempt commits a grade, advances the quiz, and re-projects mastery.
func (r *Repository) CompleteLearningAttempt(ctx context.Context, userID, submissionID, leaseToken string,
	grade domain.LearningGrade) (repository.LearningGradeCommit, error) {
	if err := checkContext(ctx); err != nil {
		return repository.LearningGradeCommit{}, err
	}
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(submissionID) == "" {
		return repository.LearningGradeCommit{}, repository.ErrInvalidArgument
	}
	// An empty lease token is accepted here on purpose: a committed attempt has no
	// token left to give, and a caller retrying after a response it lost cannot know
	// one. The fence is not weakened, because an attempt that is still grading always
	// carries a non-empty token, so an empty one can never match the row a live grader
	// is working on.
	if err := grade.Validate(); err != nil {
		return repository.LearningGradeCommit{}, repository.ErrInvalidArgument
	}

	var commit repository.LearningGradeCommit
	err := r.transaction(ctx, func(tx *gorm.DB) error {
		now := time.Now().UTC()
		row, err := r.readLearningAttemptTx(tx, userID, submissionID)
		if err != nil {
			return err
		}
		if row.Status == domain.LearningAttemptCompleted {
			// The grade is already in. Handing back the stored answer makes a lost
			// response on the grader's side harmless, and mastery is not counted twice
			// because nothing here writes.
			replayed, err := r.replayedGradeCommit(tx, userID, *row)
			if err != nil {
				return err
			}
			commit = replayed
			return nil
		}
		if row.Status != domain.LearningAttemptGrading {
			return repository.ErrLearningStaleWrite
		}
		if row.LeaseToken != leaseToken {
			// Fenced out: this quiz was taken over while the caller was scoring it.
			return repository.ErrLearningStaleWrite
		}
		record, err := r.readLearningSessionTx(tx, userID, row.LearningSessionID)
		if err != nil {
			return err
		}
		if record.Status != domain.LearningSessionGrading || record.CurrentQuestion != row.QuestionOrdinal {
			return repository.ErrLearningStaleWrite
		}

		completed := LearningAttemptRecord{
			Status: string(domain.LearningAttemptCompleted),
			// The stored total is this package's arithmetic, never a number that came
			// from outside it.
			Correctness: grade.Correctness, Completeness: grade.Completeness, Clarity: grade.Clarity,
			AutoTotal: grade.Total(), EffectiveTotal: grade.Total(),
			Feedback: grade.Feedback, MissingPoints: cloneLearningStrings(grade.MissingPoints),
			GradingConfidence: grade.Confidence,
			LeaseToken:        "", FailureCode: "", CompletedAt: &now,
		}
		// Naming the columns is what lets a zero value - a cleared lease, a clarity
		// score of none, a released grading deadline - actually reach the row.
		result := tx.Model(&LearningAttemptRecord{}).
			Where("id = ? AND user_id = ? AND status = ? AND lease_token = ?",
				row.ID, userID, string(domain.LearningAttemptGrading), leaseToken).
			Select("status", "correctness", "completeness", "clarity", "auto_total", "effective_total",
				"feedback", "missing_points", "grading_confidence", "lease_token", "lease_expires_at",
				"failure_code", "grading_at", "completed_at").
			Updates(&completed)
		if result.Error != nil {
			return translate(result.Error)
		}
		if result.RowsAffected == 0 {
			return repository.ErrLearningStaleWrite
		}

		progress, err := r.projectTopicTx(tx, userID, row.TopicKey, record.Topic, now)
		if err != nil {
			return err
		}
		if err := r.advanceLearningTx(tx, record, now); err != nil {
			return err
		}

		committed, err := r.readLearningAttemptTx(tx, userID, submissionID)
		if err != nil {
			return err
		}
		refreshed, err := r.readLearningSessionTx(tx, userID, record.ID)
		if err != nil {
			return err
		}
		commit = repository.LearningGradeCommit{Attempt: *committed, Session: *refreshed, Progress: progress}
		return nil
	})
	if err != nil {
		return repository.LearningGradeCommit{}, err
	}
	return commit, nil
}

// replayedGradeCommit loads what a repeated completion needs, without writing.
func (r *Repository) replayedGradeCommit(tx *gorm.DB, userID string,
	attempt domain.LearningAttempt) (repository.LearningGradeCommit, error) {
	record, err := r.readLearningSessionTx(tx, userID, attempt.LearningSessionID)
	if err != nil {
		return repository.LearningGradeCommit{}, err
	}
	var progress TopicProgressRecord
	if err := tx.Where("user_id = ? AND topic_key = ?", userID, attempt.TopicKey).Take(&progress).Error; err != nil {
		return repository.LearningGradeCommit{}, translate(err)
	}
	stored, err := progress.toDomain()
	if err != nil {
		return repository.LearningGradeCommit{}, err
	}
	return repository.LearningGradeCommit{Attempt: attempt, Session: *record, Progress: stored,
		Replayed: true}, nil
}

// FailLearningAttempt records that an answer could not be scored and hands the
// question back.
func (r *Repository) FailLearningAttempt(ctx context.Context, userID, submissionID, leaseToken string,
	code domain.LearningFailureCode) (domain.LearningAttempt, error) {
	if err := checkContext(ctx); err != nil {
		return domain.LearningAttempt{}, err
	}
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(submissionID) == "" || strings.TrimSpace(leaseToken) == "" {
		return domain.LearningAttempt{}, repository.ErrInvalidArgument
	}
	if !code.IsValid() {
		return domain.LearningAttempt{}, repository.ErrInvalidArgument
	}

	var failed domain.LearningAttempt
	err := r.transaction(ctx, func(tx *gorm.DB) error {
		now := time.Now().UTC()
		row, err := r.readLearningAttemptTx(tx, userID, submissionID)
		if err != nil {
			return err
		}
		if row.Status != domain.LearningAttemptGrading {
			return repository.ErrLearningStaleWrite
		}
		if row.LeaseToken != leaseToken {
			return repository.ErrLearningStaleWrite
		}
		result := tx.Model(&LearningAttemptRecord{}).
			Where("id = ? AND user_id = ? AND status = ? AND lease_token = ?",
				row.ID, userID, string(domain.LearningAttemptGrading), leaseToken).
			Updates(map[string]any{
				"status": string(domain.LearningAttemptFailed), "failure_code": string(code),
				"lease_token": "", "lease_expires_at": nil, "grading_at": now,
			})
		if result.Error != nil {
			return translate(result.Error)
		}
		if result.RowsAffected == 0 {
			return repository.ErrLearningStaleWrite
		}
		// The question is not advanced: an answer lost to a server-side failure is
		// resubmittable, and a learner who is told "that question is over" for a
		// failure that was ours has paid twice.
		record, err := r.readLearningSessionTx(tx, userID, row.LearningSessionID)
		if err != nil {
			return err
		}
		if record.Status == domain.LearningSessionGrading {
			if err := r.writeLearningStatusTx(tx, record.ID, userID,
				domain.LearningSessionGrading, domain.LearningSessionWaitingAnswer, record.Version, now); err != nil {
				return err
			}
		}
		stored, err := r.readLearningAttemptTx(tx, userID, submissionID)
		if err != nil {
			return err
		}
		failed = *stored
		return nil
	})
	if err != nil {
		return domain.LearningAttempt{}, err
	}
	return failed, nil
}

// SkipLearningQuestion records a declined answer and moves the quiz on.
func (r *Repository) SkipLearningQuestion(ctx context.Context, userID, learningSessionID, submissionID string) (repository.LearningAttemptClaim, error) {
	if err := checkContext(ctx); err != nil {
		return repository.LearningAttemptClaim{}, err
	}
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(learningSessionID) == "" || strings.TrimSpace(submissionID) == "" {
		return repository.LearningAttemptClaim{}, repository.ErrInvalidArgument
	}

	var result repository.LearningAttemptClaim
	err := r.transaction(ctx, func(tx *gorm.DB) error {
		now := time.Now().UTC()
		if err := r.recoverLearningTx(tx, userID, learningSessionID, now); err != nil {
			return err
		}
		record, err := r.readLearningSessionTx(tx, userID, learningSessionID)
		if err != nil {
			return err
		}

		// A repeat of a skip is the same operation, so it answers with the stored
		// attempt. The quiz's position is deliberately not part of the match: it moved
		// on precisely because the skip worked.
		var existing LearningAttemptRecord
		lookup := tx.Where("user_id = ? AND submission_id = ?", userID, submissionID).Take(&existing)
		switch {
		case lookup.Error == nil:
			attempt, err := existing.toDomain()
			if err != nil {
				return err
			}
			if attempt.LearningSessionID != learningSessionID ||
				attempt.Status != domain.LearningAttemptSkipped {
				return repository.ErrLearningSubmissionConflict
			}
			result = repository.LearningAttemptClaim{Attempt: attempt, NeedsGrading: false, Session: *record}
			return nil
		case !errors.Is(lookup.Error, gorm.ErrRecordNotFound):
			return translate(lookup.Error)
		}

		switch record.Status {
		case domain.LearningSessionWaitingAnswer:
		case domain.LearningSessionGrading:
			return repository.ErrLearningAnswerBusy
		default:
			return repository.ErrLearningQuizClosed
		}
		var question LearningQuestionRecord
		if err := tx.Where("learning_session_id = ? AND user_id = ? AND ordinal = ?",
			learningSessionID, userID, record.CurrentQuestion).Take(&question).Error; err != nil {
			return translate(err)
		}

		skipped := domain.LearningAttempt{
			ID: newID("lat"), UserID: userID, SessionID: record.SessionID,
			LearningSessionID: record.ID, QuestionID: question.ID, QuestionOrdinal: question.Ordinal,
			SubmissionID: submissionID, TopicKey: record.TopicKey, Status: domain.LearningAttemptSkipped,
			CreatedAt: now, CompletedAt: &now,
		}
		if err := tx.Create(&LearningAttemptRecord{
			ID: skipped.ID, UserID: skipped.UserID, SessionID: skipped.SessionID,
			LearningSessionID: skipped.LearningSessionID, QuestionID: skipped.QuestionID,
			QuestionOrdinal: skipped.QuestionOrdinal, SubmissionID: skipped.SubmissionID,
			TopicKey: skipped.TopicKey, Status: string(skipped.Status),
			Rubric: cloneLearningStrings(question.Rubric), Evidence: cloneLearningEvidence(question.Evidence),
			CreatedAt: now, CompletedAt: &now,
		}).Error; err != nil {
			return learningDuplicate(err, repository.ErrLearningSubmissionConflict)
		}
		if err := r.advanceLearningTx(tx, record, now); err != nil {
			return err
		}
		stored, err := r.readLearningAttemptTx(tx, userID, submissionID)
		if err != nil {
			return err
		}
		refreshed, err := r.readLearningSessionTx(tx, userID, learningSessionID)
		if err != nil {
			return err
		}
		result = repository.LearningAttemptClaim{Attempt: *stored, NeedsGrading: false, Session: *refreshed}
		return nil
	})
	if err != nil {
		return repository.LearningAttemptClaim{}, err
	}
	return result, nil
}

// RecoverLearningSession applies the deadline and abandoned-work rules.
func (r *Repository) RecoverLearningSession(ctx context.Context, userID, learningSessionID string) (*domain.LearningSession, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(learningSessionID) == "" {
		return nil, repository.ErrInvalidArgument
	}
	var recovered *domain.LearningSession
	err := r.transaction(ctx, func(tx *gorm.DB) error {
		if err := r.recoverLearningTx(tx, userID, learningSessionID, time.Now().UTC()); err != nil {
			return err
		}
		read, err := r.readLearningSessionTx(tx, userID, learningSessionID)
		recovered = read
		return err
	})
	if err != nil {
		return nil, err
	}
	return recovered, nil
}

// GetTopicProgress reads one topic's projection.
func (r *Repository) GetTopicProgress(ctx context.Context, userID, topicKey string) (*domain.TopicProgress, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(topicKey) == "" {
		return nil, repository.ErrInvalidArgument
	}
	var record TopicProgressRecord
	err := r.transaction(ctx, func(tx *gorm.DB) error {
		return tx.Where("user_id = ? AND topic_key = ?", userID, topicKey).Take(&record).Error
	})
	if err != nil {
		return nil, translate(err)
	}
	progress, err := record.toDomain()
	if err != nil {
		return nil, err
	}
	return &progress, nil
}

// ListTopicProgress returns the caller's topics, most recently attempted first.
func (r *Repository) ListTopicProgress(ctx context.Context, userID string, limit int) ([]domain.TopicProgress, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if strings.TrimSpace(userID) == "" || limit <= 0 {
		return nil, repository.ErrInvalidArgument
	}
	var rows []TopicProgressRecord
	err := r.transaction(ctx, func(tx *gorm.DB) error {
		// last_attempt_at alone is not a total order: two topics answered back to back can
		// share a clock tick, and then "most recently practised" would be decided by the
		// storage engine. The id is the row's creation order on both engines, so the tie
		// breaks the same way here and in memory.
		return tx.Where("user_id = ?", userID).
			Order("last_attempt_at DESC, id ASC").Limit(limit).Find(&rows).Error
	})
	if err != nil {
		return nil, translate(err)
	}
	progress := make([]domain.TopicProgress, 0, len(rows))
	for _, row := range rows {
		item, err := row.toDomain()
		if err != nil {
			return nil, err
		}
		progress = append(progress, item)
	}
	return progress, nil
}

// recoverLearningTx is the one exit rule for every non-terminal state, in the same
// three steps the in-memory store applies: deadline first, then an abandoned
// generation, then grading work nobody is holding.
func (r *Repository) recoverLearningTx(tx *gorm.DB, userID, learningSessionID string, now time.Time) error {
	record, err := r.readLearningSessionTx(tx, userID, learningSessionID)
	if err != nil {
		return err
	}
	if record.Status.IsTerminal() {
		return nil
	}

	if record.IsExpired(now) {
		if err := r.releaseGradingTx(tx, record.UserID, record.ID, domain.LearningFailureLeaseExpired, now); err != nil {
			return err
		}
		return r.writeLearningStatusTx(tx, record.ID, record.UserID, record.Status,
			domain.LearningSessionExpired, record.Version, now)
	}

	if record.Status == domain.LearningSessionAsking && record.GenerationAbandoned(now) {
		if err := r.writeLearningStatusTx(tx, record.ID, record.UserID, domain.LearningSessionAsking,
			domain.LearningSessionCreated, record.Version, now); err != nil {
			return err
		}
		return nil
	}

	if record.Status != domain.LearningSessionGrading {
		return nil
	}
	var live int64
	if err := tx.Model(&LearningAttemptRecord{}).
		Where("user_id = ? AND learning_session_id = ? AND status = ? AND lease_expires_at > ?",
			userID, learningSessionID, string(domain.LearningAttemptGrading), now).
		Count(&live).Error; err != nil {
		return err
	}
	if live > 0 {
		return nil
	}
	// Grading with nobody holding the lease: fail the abandoned attempt and use the
	// retry edge back to waiting_answer. A quiz left here has no path a user could
	// take, which is the state this whole method exists to prevent.
	if err := r.releaseGradingTx(tx, userID, learningSessionID, domain.LearningFailureLeaseExpired, now); err != nil {
		return err
	}
	return r.writeLearningStatusTx(tx, record.ID, userID, domain.LearningSessionGrading,
		domain.LearningSessionWaitingAnswer, record.Version, now)
}

// releaseGradingTx fails whichever attempts claim to be grading this quiz's work.
func (r *Repository) releaseGradingTx(tx *gorm.DB, userID, learningSessionID string,
	code domain.LearningFailureCode, now time.Time) error {
	result := tx.Model(&LearningAttemptRecord{}).
		Where("user_id = ? AND learning_session_id = ? AND status = ?",
			userID, learningSessionID, string(domain.LearningAttemptGrading)).
		Updates(map[string]any{
			"status": string(domain.LearningAttemptFailed), "failure_code": string(code),
			"lease_token": "", "lease_expires_at": nil, "grading_at": now,
		})
	return translate(result.Error)
}

// writeLearningStatusTx performs one conditioned state change and refuses to claim
// success when the row did not move.
//
// The lease columns are handled here so no edge can leave a generation claim
// attached to a quiz that has stopped generating.
func (r *Repository) writeLearningStatusTx(tx *gorm.DB, learningSessionID, userID string,
	from, to domain.LearningSessionStatus, expectVersion uint64, now time.Time) error {
	if !from.CanTransitionTo(to) {
		return repository.ErrLearningTransition
	}
	fields := map[string]any{
		"status": string(to), "version": gorm.Expr("version + 1"), "updated_at": now,
	}
	if from == domain.LearningSessionAsking {
		fields["generation_token"] = ""
		fields["generation_expires_at"] = nil
	}
	if to.IsTerminal() {
		fields["active_key"] = nil
		if to == domain.LearningSessionCompleted {
			fields["completed_at"] = now
		}
	}
	result := tx.Model(&LearningSessionRecord{}).
		Where("id = ? AND user_id = ? AND status = ? AND version = ?",
			learningSessionID, userID, string(from), expectVersion).
		Updates(fields)
	if result.Error != nil {
		return translate(result.Error)
	}
	if result.RowsAffected == 0 {
		return repository.ErrLearningStaleWrite
	}
	return nil
}

// advanceLearningTx moves a quiz to its next card, or closes it when the card it was on
// was the last one. The status travels with the ordinal for the same reason it does in the
// in-memory store: a graded answer must not leave the row claiming it is still being
// scored, and a skip - whose status is already "waiting for an answer" - rewrites the value
// it holds. The WHERE clause pins the previous status, so this cannot move a row somebody
// else has already advanced.
func (r *Repository) advanceLearningTx(tx *gorm.DB, record *domain.LearningSession, now time.Time) error {
	if record.CurrentQuestion >= record.TotalQuestions {
		return r.writeLearningStatusTx(tx, record.ID, record.UserID, record.Status,
			domain.LearningSessionCompleted, record.Version, now)
	}
	target := domain.LearningSessionWaitingAnswer
	if record.Status != domain.LearningSessionGrading {
		target = record.Status
	}
	if target != record.Status && !record.Status.CanTransitionTo(target) {
		return repository.ErrLearningTransition
	}
	result := tx.Model(&LearningSessionRecord{}).
		Where("id = ? AND user_id = ? AND status = ? AND version = ?",
			record.ID, record.UserID, string(record.Status), record.Version).
		Updates(map[string]any{
			"current_question": gorm.Expr("current_question + 1"),
			"status":           string(target),
			"version":          gorm.Expr("version + 1"),
			"updated_at":       now,
		})
	if result.Error != nil {
		return translate(result.Error)
	}
	if result.RowsAffected == 0 {
		return repository.ErrLearningStaleWrite
	}
	return nil
}

// projectTopicTx recomputes one topic's projection from stored attempts and writes it
// back, inside the caller's transaction.
func (r *Repository) projectTopicTx(tx *gorm.DB, userID, topicKey, topic string, now time.Time) (domain.TopicProgress, error) {
	var rows []LearningAttemptRecord
	if err := tx.Where("user_id = ? AND topic_key = ? AND status = ?",
		userID, topicKey, string(domain.LearningAttemptCompleted)).
		Order(orderByLearningAttemptCompletion).Limit(domain.LearningRecentAttemptWindow).
		Find(&rows).Error; err != nil {
		return domain.TopicProgress{}, translate(err)
	}
	completed := make([]domain.LearningAttempt, 0, len(rows))
	for _, row := range rows {
		attempt, err := row.toDomain()
		if err != nil {
			return domain.TopicProgress{}, err
		}
		completed = append(completed, attempt)
	}
	progress := domain.ProjectTopicProgress(userID, topicKey, topic, completed, now)

	var existing TopicProgressRecord
	lookup := tx.Where("user_id = ? AND topic_key = ?", userID, topicKey).Take(&existing)
	switch {
	case lookup.Error == nil:
		progress.ID, progress.CreatedAt = existing.ID, existing.CreatedAt
	case errors.Is(lookup.Error, gorm.ErrRecordNotFound):
		progress.ID = newID("tpr")
		progress.CreatedAt = now
	default:
		return domain.TopicProgress{}, translate(lookup.Error)
	}
	progress.UpdatedAt = now
	if err := progress.Validate(); err != nil {
		return domain.TopicProgress{}, repository.ErrInvalidArgument
	}

	record := TopicProgressRecord{
		ID: progress.ID, UserID: progress.UserID, TopicKey: progress.TopicKey, Topic: progress.Topic,
		Mastery: progress.Mastery, Confidence: progress.Confidence, ValidCount: progress.ValidCount,
		Level: string(progress.Level), WeakPoints: cloneLearningStrings(progress.WeakPoints),
		LastAttempt: utcPointer(progress.LastAttempt), CreatedAt: progress.CreatedAt, UpdatedAt: progress.UpdatedAt,
	}
	// A full re-projection rather than an incremental roll: the last ten attempts are
	// re-read every time, so a row that was wrong for any reason is corrected by the
	// next grade instead of being carried forward forever.
	err := tx.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "user_id"}, {Name: "topic_key"}},
		DoUpdates: clause.AssignmentColumns([]string{"topic", "mastery_score", "confidence",
			"valid_attempts", "level", "weak_points", "last_attempt_at", "updated_at"}),
	}).Create(&record).Error
	if err != nil {
		return domain.TopicProgress{}, translate(err)
	}
	return progress, nil
}

// readLearningSessionTx reads a quiz the caller owns.
func (r *Repository) readLearningSessionTx(tx *gorm.DB, userID, learningSessionID string) (*domain.LearningSession, error) {
	var record LearningSessionRecord
	if err := tx.Where("id = ? AND user_id = ?", learningSessionID, userID).Take(&record).Error; err != nil {
		return nil, translate(err)
	}
	session, err := record.toDomain()
	if err != nil {
		return nil, err
	}
	return &session, nil
}

// readLearningAttemptTx reads one answer by the submission id that names it.
func (r *Repository) readLearningAttemptTx(tx *gorm.DB, userID, submissionID string) (*domain.LearningAttempt, error) {
	var record LearningAttemptRecord
	if err := tx.Where("user_id = ? AND submission_id = ?", userID, submissionID).Take(&record).Error; err != nil {
		return nil, translate(err)
	}
	attempt, err := record.toDomain()
	if err != nil {
		return nil, err
	}
	return &attempt, nil
}

// learningDuplicate decides what a violated unique key means at the site that hit
// it.
//
// A duplicate key cannot be classified centrally, because the tables here are
// guarded by different unique keys that mean different things to a caller: the open
// quiz marker colliding is "this conversation already has a quiz", while the
// submission id colliding is "you already sent that answer". Each write passes the
// refusal its own indexes can produce, and anything that is not a duplicate is left
// for translate to classify.
func learningDuplicate(err error, refusal error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, gorm.ErrDuplicatedKey) || isDuplicateKey(err) {
		return refusal
	}
	return translate(err)
}

func cloneLearningStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	return append([]string(nil), values...)
}

func cloneLearningEvidence(values []domain.LearningEvidence) []domain.LearningEvidence {
	if len(values) == 0 {
		return nil
	}
	return append([]domain.LearningEvidence(nil), values...)
}

func utcPointer(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	normalized := value.UTC()
	return &normalized
}
