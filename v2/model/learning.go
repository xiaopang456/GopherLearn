package model

import (
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

type LearningSessionStatus string

const (
	LearningSessionCreated       LearningSessionStatus = "created"
	LearningSessionAsking        LearningSessionStatus = "asking"
	LearningSessionWaitingAnswer LearningSessionStatus = "waiting_answer"
	LearningSessionGrading       LearningSessionStatus = "grading"
	LearningSessionCompleted     LearningSessionStatus = "completed"
	LearningSessionCancelled     LearningSessionStatus = "cancelled"
	LearningSessionExpired       LearningSessionStatus = "expired"
)

func (s LearningSessionStatus) Valid() bool {
	switch s {
	case LearningSessionCreated, LearningSessionAsking, LearningSessionWaitingAnswer,
		LearningSessionGrading, LearningSessionCompleted, LearningSessionCancelled,
		LearningSessionExpired:
		return true
	default:
		return false
	}
}

func (s LearningSessionStatus) Terminal() bool {
	return s == LearningSessionCompleted || s == LearningSessionCancelled || s == LearningSessionExpired
}

func (s LearningSessionStatus) CanTransition(to LearningSessionStatus) bool {
	if !s.Valid() || !to.Valid() || s.Terminal() || s == to {
		return false
	}
	switch s {
	case LearningSessionCreated:
		return to == LearningSessionAsking || to == LearningSessionCancelled || to == LearningSessionExpired
	case LearningSessionAsking:
		return to == LearningSessionWaitingAnswer || to == LearningSessionCancelled || to == LearningSessionExpired
	case LearningSessionWaitingAnswer:
		return to == LearningSessionGrading || to == LearningSessionCancelled || to == LearningSessionExpired
	case LearningSessionGrading:
		return to == LearningSessionWaitingAnswer || to == LearningSessionCompleted || to == LearningSessionCancelled || to == LearningSessionExpired
	default:
		return false
	}
}

type LearningQuestionType string

const (
	LearningQuestionShortAnswer       LearningQuestionType = "short_answer"
	LearningQuestionConceptComparison LearningQuestionType = "concept_comparison"
	LearningQuestionCodeAnalysis      LearningQuestionType = "code_analysis"
)

func (t LearningQuestionType) Valid() bool {
	switch t {
	case LearningQuestionShortAnswer, LearningQuestionConceptComparison, LearningQuestionCodeAnalysis:
		return true
	default:
		return false
	}
}

// LearningEvidence is an immutable, bounded snapshot of the evidence used to
// generate a question. Grading must use this snapshot instead of retrieving a
// possibly newer document version.
type LearningEvidence struct {
	EvidenceID   int      `json:"evidence_id"`
	DocumentID   string   `json:"document_id"`
	DocumentName string   `json:"document_name"`
	DocVersion   int      `json:"doc_version"`
	ChunkID      string   `json:"chunk_id"`
	TitlePath    []string `json:"title_path"`
	StartLine    int      `json:"start_line"`
	EndLine      int      `json:"end_line"`
	ContentType  string   `json:"content_type"`
	ContentHash  string   `json:"content_hash"`
	Snapshot     string   `json:"snapshot"`
}

func (e LearningEvidence) Validate() error {
	if e.EvidenceID <= 0 || strings.TrimSpace(e.DocumentID) == "" || e.DocVersion <= 0 || strings.TrimSpace(e.ChunkID) == "" {
		return fmt.Errorf("learning evidence identity is incomplete")
	}
	if e.StartLine <= 0 || e.EndLine < e.StartLine {
		return fmt.Errorf("learning evidence line range is invalid")
	}
	if strings.TrimSpace(e.ContentHash) == "" || strings.TrimSpace(e.Snapshot) == "" {
		return fmt.Errorf("learning evidence snapshot is required")
	}
	if utf8.RuneCountInString(e.Snapshot) > 6000 {
		return fmt.Errorf("learning evidence snapshot is too large")
	}
	return nil
}

type LearningSession struct {
	ID              string                `gorm:"primaryKey;type:varchar(36)" json:"id"`
	UserID          int64                 `gorm:"not null;index:idx_learning_user_chat,priority:1" json:"user_id"`
	SessionID       string                `gorm:"type:varchar(36);not null;index:idx_learning_user_chat,priority:2" json:"session_id"`
	AgentRunID      string                `gorm:"type:varchar(36);not null;uniqueIndex" json:"agent_run_id"`
	Topic           string                `gorm:"type:varchar(160);not null" json:"topic"`
	TopicKey        string                `gorm:"type:varchar(160);not null;index" json:"topic_key"`
	Status          LearningSessionStatus `gorm:"type:varchar(32);not null;index" json:"status"`
	CurrentQuestion int                   `gorm:"not null;default:1" json:"current_question"`
	TotalQuestions  int                   `gorm:"not null" json:"total_questions"`
	Version         uint64                `gorm:"not null;default:1" json:"version"`
	ActiveKey       *string               `gorm:"type:varchar(240);uniqueIndex:uniq_learning_active_key" json:"-"`
	ExpiresAt       time.Time             `gorm:"not null;index" json:"expires_at"`
	CompletedAt     *time.Time            `json:"completed_at,omitempty"`
	CreatedAt       time.Time             `json:"created_at"`
	UpdatedAt       time.Time             `json:"updated_at"`
}

func (s *LearningSession) ValidateForCreate() error {
	if s == nil || strings.TrimSpace(s.ID) == "" || s.UserID <= 0 || strings.TrimSpace(s.SessionID) == "" || strings.TrimSpace(s.AgentRunID) == "" {
		return fmt.Errorf("learning session identity is incomplete")
	}
	if strings.TrimSpace(s.Topic) == "" || strings.TrimSpace(s.TopicKey) == "" {
		return fmt.Errorf("learning session topic is required")
	}
	if s.Status != LearningSessionCreated || s.Version != 1 || s.CurrentQuestion != 1 {
		return fmt.Errorf("new learning session must start at created/version 1/question 1")
	}
	if s.TotalQuestions <= 0 || s.TotalQuestions > 5 {
		return fmt.Errorf("learning session total questions must be between 1 and 5")
	}
	if s.ActiveKey == nil || strings.TrimSpace(*s.ActiveKey) == "" || s.ExpiresAt.IsZero() {
		return fmt.Errorf("learning session active key and expiry are required")
	}
	return nil
}

type LearningQuestion struct {
	ID                string               `gorm:"primaryKey;type:varchar(36)" json:"id"`
	UserID            int64                `gorm:"not null;index" json:"user_id"`
	LearningSessionID string               `gorm:"type:varchar(36);not null;uniqueIndex:idx_learning_question_order,priority:1" json:"learning_session_id"`
	Ordinal           int                  `gorm:"not null;uniqueIndex:idx_learning_question_order,priority:2" json:"ordinal"`
	Type              LearningQuestionType `gorm:"type:varchar(32);not null" json:"type"`
	Question          string               `gorm:"type:text;not null" json:"question"`
	AssessmentGoal    string               `gorm:"type:varchar(240);not null;default:''" json:"assessment_goal"`
	ReferenceAnswer   string               `gorm:"type:text;not null" json:"-"`
	Rubric            []string             `gorm:"serializer:json;type:json" json:"-"`
	Evidence          []LearningEvidence   `gorm:"serializer:json;type:json" json:"-"`
	CreatedAt         time.Time            `json:"created_at"`
}

type LearningAttemptStatus string

const (
	LearningAttemptGrading   LearningAttemptStatus = "grading"
	LearningAttemptCompleted LearningAttemptStatus = "completed"
	LearningAttemptFailed    LearningAttemptStatus = "failed"
	LearningAttemptSkipped   LearningAttemptStatus = "skipped"
)

func (s LearningAttemptStatus) Valid() bool {
	switch s {
	case LearningAttemptGrading, LearningAttemptCompleted, LearningAttemptFailed, LearningAttemptSkipped:
		return true
	default:
		return false
	}
}

// LearningAttempt is the durable idempotency and audit boundary for one answer.
// Rubric and Evidence are copied from the question so grading can be reproduced
// even when the knowledge document is updated later.
type LearningAttempt struct {
	ID                string                `gorm:"primaryKey;type:varchar(36)" json:"id"`
	UserID            int64                 `gorm:"not null;uniqueIndex:uniq_learning_submission,priority:1;index" json:"user_id"`
	SessionID         string                `gorm:"type:varchar(36);not null;index" json:"session_id"`
	LearningSessionID string                `gorm:"type:varchar(36);not null;index" json:"learning_session_id"`
	QuestionID        string                `gorm:"type:varchar(36);not null;index" json:"question_id"`
	QuestionOrdinal   int                   `gorm:"not null" json:"question_ordinal"`
	SubmissionID      string                `gorm:"type:varchar(64);not null;uniqueIndex:uniq_learning_submission,priority:2" json:"submission_id"`
	TopicKey          string                `gorm:"type:varchar(160);not null;index" json:"topic_key"`
	Status            LearningAttemptStatus `gorm:"type:varchar(24);not null;index" json:"status"`
	Answer            string                `gorm:"type:longtext" json:"answer"`
	Rubric            []string              `gorm:"serializer:json;type:json" json:"rubric"`
	Evidence          []LearningEvidence    `gorm:"serializer:json;type:json" json:"evidence"`
	AutoCorrectness   int                   `gorm:"not null;default:0" json:"auto_correctness"`
	AutoCompleteness  int                   `gorm:"not null;default:0" json:"auto_completeness"`
	AutoClarity       int                   `gorm:"not null;default:0" json:"auto_clarity"`
	AutoTotal         int                   `gorm:"not null;default:0" json:"auto_total"`
	EffectiveTotal    int                   `gorm:"not null;default:0" json:"effective_total"`
	Feedback          string                `gorm:"type:text" json:"feedback"`
	MissingPoints     []string              `gorm:"serializer:json;type:json" json:"missing_points"`
	GradingConfidence float64               `gorm:"not null;default:0" json:"grading_confidence"`
	LeaseToken        string                `gorm:"type:varchar(36);index" json:"-"`
	LeaseExpiresAt    *time.Time            `gorm:"index" json:"lease_expires_at,omitempty"`
	FailureReason     string                `gorm:"type:varchar(160)" json:"failure_reason,omitempty"`
	CorrectionReason  string                `gorm:"type:varchar(500)" json:"correction_reason,omitempty"`
	CorrectedByUserID int64                 `gorm:"not null;default:0" json:"corrected_by_user_id,omitempty"`
	CorrectedAt       *time.Time            `json:"corrected_at,omitempty"`
	CompletedAt       *time.Time            `gorm:"index" json:"completed_at,omitempty"`
	CreatedAt         time.Time             `json:"created_at"`
	UpdatedAt         time.Time             `json:"updated_at"`
}

func (a *LearningAttempt) ValidateForClaim() error {
	if a == nil || strings.TrimSpace(a.ID) == "" || a.UserID <= 0 || strings.TrimSpace(a.SessionID) == "" ||
		strings.TrimSpace(a.LearningSessionID) == "" || strings.TrimSpace(a.QuestionID) == "" || a.QuestionOrdinal <= 0 ||
		strings.TrimSpace(a.SubmissionID) == "" || strings.TrimSpace(a.TopicKey) == "" {
		return fmt.Errorf("learning attempt identity is incomplete")
	}
	if a.Status != LearningAttemptGrading || strings.TrimSpace(a.Answer) == "" || utf8.RuneCountInString(a.Answer) > 12000 {
		return fmt.Errorf("learning attempt answer is invalid")
	}
	if strings.TrimSpace(a.LeaseToken) == "" || a.LeaseExpiresAt == nil {
		return fmt.Errorf("learning attempt lease is missing")
	}
	return nil
}

type TopicProgressLevel string

const (
	TopicProgressInsufficient TopicProgressLevel = "insufficient"
	TopicProgressDeveloping   TopicProgressLevel = "developing"
	TopicProgressMastered     TopicProgressLevel = "mastered"
)

// TopicProgress is a deterministic projection of recent valid attempts. It is
// not written directly by the model or by a future Dream/memory process.
type TopicProgress struct {
	ID            uint64             `gorm:"primaryKey;autoIncrement" json:"id"`
	UserID        int64              `gorm:"not null;uniqueIndex:uniq_topic_progress,priority:1" json:"user_id"`
	TopicKey      string             `gorm:"type:varchar(160);not null;uniqueIndex:uniq_topic_progress,priority:2" json:"topic_key"`
	Topic         string             `gorm:"type:varchar(160);not null" json:"topic"`
	MasteryScore  int                `gorm:"not null;default:0" json:"mastery_score"`
	Confidence    float64            `gorm:"not null;default:0" json:"confidence"`
	ValidAttempts int                `gorm:"not null;default:0" json:"valid_attempts"`
	Level         TopicProgressLevel `gorm:"type:varchar(24);not null" json:"level"`
	WeakPoints    []string           `gorm:"serializer:json;type:json" json:"weak_points"`
	LastAttemptAt *time.Time         `json:"last_attempt_at,omitempty"`
	CreatedAt     time.Time          `json:"created_at"`
	UpdatedAt     time.Time          `json:"updated_at"`
}

func (q *LearningQuestion) ValidateForCreate() error {
	if q == nil || strings.TrimSpace(q.ID) == "" || q.UserID <= 0 || strings.TrimSpace(q.LearningSessionID) == "" || q.Ordinal <= 0 {
		return fmt.Errorf("learning question identity is incomplete")
	}
	if !q.Type.Valid() || strings.TrimSpace(q.Question) == "" || strings.TrimSpace(q.AssessmentGoal) == "" || strings.TrimSpace(q.ReferenceAnswer) == "" {
		return fmt.Errorf("learning question content is invalid")
	}
	if utf8.RuneCountInString(q.Question) > 1000 || utf8.RuneCountInString(q.AssessmentGoal) > 240 || utf8.RuneCountInString(q.ReferenceAnswer) > 4000 {
		return fmt.Errorf("learning question content is too large")
	}
	if len(q.Rubric) == 0 || len(q.Rubric) > 6 || len(q.Evidence) == 0 || len(q.Evidence) > 5 {
		return fmt.Errorf("learning question rubric or evidence count is invalid")
	}
	for _, item := range q.Rubric {
		if strings.TrimSpace(item) == "" || utf8.RuneCountInString(item) > 500 {
			return fmt.Errorf("learning question rubric item is invalid")
		}
	}
	for _, evidence := range q.Evidence {
		if err := evidence.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func LearningActiveKey(userID int64, sessionID string) string {
	return fmt.Sprintf("%d:%s", userID, strings.TrimSpace(sessionID))
}

// NormalizeLearningTopicKey deliberately performs only deterministic lexical
// normalization. Semantic alias merging belongs to a later, auditable layer.
func NormalizeLearningTopicKey(topic string) string {
	topic = strings.TrimSpace(strings.ToLower(topic))
	var builder strings.Builder
	separatorPending := false
	for _, r := range topic {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if separatorPending && builder.Len() > 0 {
				builder.WriteByte('-')
			}
			separatorPending = false
			builder.WriteRune(r)
			continue
		}
		separatorPending = builder.Len() > 0
	}
	value := strings.Trim(builder.String(), "-")
	runes := []rune(value)
	if len(runes) > 80 {
		value = strings.TrimRight(string(runes[:80]), "-")
	}
	return value
}
