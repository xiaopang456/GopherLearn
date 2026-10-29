package learning

import (
	learningdao "GopherAI/dao/learning"
	"GopherAI/model"
	agentservice "GopherAI/service/agent"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

const gradingTimeout = 45 * time.Second
const gradingLease = 60 * time.Second

type LearningCompletionEvent struct {
	UserID            int64
	SessionID         string
	LearningSessionID string
	TopicKey          string
	CompletedAt       time.Time
}

type LearningCompletionPublisher interface {
	PublishLearningCompleted(context.Context, LearningCompletionEvent) error
}
type LearningCompletionPublisherFunc func(context.Context, LearningCompletionEvent) error

func (f LearningCompletionPublisherFunc) PublishLearningCompleted(ctx context.Context, event LearningCompletionEvent) error {
	return f(ctx, event)
}

var DiscardLearningCompletions LearningCompletionPublisher = LearningCompletionPublisherFunc(func(context.Context, LearningCompletionEvent) error { return nil })

type AnswerRequest struct {
	UserID            int64
	SessionID         string
	LearningSessionID string
	QuestionID        string
	SubmissionID      string
	Answer            string
}

type LearningAction string

const (
	LearningActionSkip   LearningAction = "skip"
	LearningActionCancel LearningAction = "cancel"
)

type ActionRequest struct {
	UserID            int64
	SessionID         string
	LearningSessionID string
	QuestionID        string
	SubmissionID      string
	Action            LearningAction
}

type ProgressRequest struct {
	UserID int64
	Topic  string
}
type CorrectRequest struct {
	UserID         int64
	AttemptID      string
	EffectiveTotal int
	Reason         string
}

type AttemptView struct {
	ID               string                      `json:"id"`
	SubmissionID     string                      `json:"submissionId"`
	QuestionID       string                      `json:"questionId"`
	Status           model.LearningAttemptStatus `json:"status"`
	Correctness      int                         `json:"correctness"`
	Completeness     int                         `json:"completeness"`
	Clarity          int                         `json:"clarity"`
	AutoTotal        int                         `json:"autoTotal"`
	EffectiveTotal   int                         `json:"effectiveTotal"`
	Feedback         string                      `json:"feedback"`
	MissingPoints    []string                    `json:"missingPoints"`
	Confidence       float64                     `json:"confidence"`
	CorrectionReason string                      `json:"correctionReason,omitempty"`
}

type AnswerView struct {
	Learning SessionView          `json:"learning"`
	Attempt  AttemptView          `json:"attempt"`
	Progress *model.TopicProgress `json:"progress,omitempty"`
}

func (s *Service) Answer(ctx context.Context, request AnswerRequest) (AnswerView, error) {
	if s.answers == nil || s.grader == nil {
		return AnswerView{}, ErrServiceNotConfigured
	}
	if err := validateAnswerRequest(request); err != nil {
		return AnswerView{}, err
	}
	owned, err := s.sessions.GetByIDAndUserID(ctx, request.SessionID, request.UserID)
	if err != nil {
		return AnswerView{}, err
	}
	if owned.Mode != model.SessionModeAgent {
		return AnswerView{}, agentservice.ErrAgentSessionRequired
	}
	now := s.now()
	leaseToken := uuid.NewString()
	claim, err := s.answers.ClaimAnswer(ctx, learningdao.ClaimAnswerInput{Attempt: model.LearningAttempt{
		ID: uuid.NewString(), UserID: request.UserID, SessionID: request.SessionID,
		LearningSessionID: request.LearningSessionID, QuestionID: request.QuestionID,
		SubmissionID: strings.TrimSpace(request.SubmissionID), Status: model.LearningAttemptGrading,
		Answer: strings.TrimSpace(request.Answer), LeaseToken: leaseToken,
	}, Now: now, LeaseEnds: now.Add(gradingLease)})
	if errors.Is(err, learningdao.ErrGradingInProgress) {
		return AnswerView{}, ErrLearningAnswerBusy
	}
	if errors.Is(err, learningdao.ErrSubmissionConflict) {
		return AnswerView{}, ErrLearningSubmission
	}
	if err != nil {
		return AnswerView{}, err
	}
	if !claim.NeedsGrading {
		return s.replayAnswer(ctx, claim)
	}
	run, err := s.moveRunToGrading(ctx, claim.Session)
	if err != nil {
		_, _ = s.answers.FailAnswer(context.WithoutCancel(ctx), request.UserID, claim.Attempt.ID, claim.Attempt.LeaseToken, "run_state_failed", s.now())
		return AnswerView{}, err
	}
	gradeCtx, cancel := context.WithTimeout(ctx, gradingTimeout)
	grade, gradeErr := s.grader.Grade(gradeCtx, GradeRequest{
		Question: claim.Question.Question, ReferenceAnswer: claim.Question.ReferenceAnswer,
		Rubric: claim.Attempt.Rubric, Evidence: claim.Attempt.Evidence, Answer: claim.Attempt.Answer,
	})
	cancel()
	if gradeErr != nil {
		s.failGrading(ctx, claim, run, gradeErr)
		return AnswerView{}, fmt.Errorf("%w: %v", ErrLearningGradingFailed, gradeErr)
	}
	completed, err := s.answers.CompleteAnswer(ctx, request.UserID, claim.Attempt.ID, claim.Attempt.LeaseToken, learningdao.GradeCommit{
		Correctness: grade.Correctness, Completeness: grade.Completeness, Clarity: grade.Clarity,
		Feedback: grade.Feedback, MissingPoints: grade.MissingPoints, Confidence: grade.Confidence,
	}, s.now())
	if err != nil {
		s.failGrading(ctx, claim, run, err)
		return AnswerView{}, err
	}
	s.finishAnswerRun(ctx, run, completed)
	return buildAnswerView(completed), nil
}

func (s *Service) Action(ctx context.Context, request ActionRequest) (AnswerView, error) {
	if request.Action == LearningActionCancel {
		view, err := s.Cancel(ctx, CancelRequest{UserID: request.UserID, SessionID: request.SessionID, LearningSessionID: request.LearningSessionID})
		return AnswerView{Learning: view}, err
	}
	if s.answers == nil || request.Action != LearningActionSkip || request.UserID <= 0 || strings.TrimSpace(request.SessionID) == "" || strings.TrimSpace(request.LearningSessionID) == "" || strings.TrimSpace(request.QuestionID) == "" || strings.TrimSpace(request.SubmissionID) == "" {
		return AnswerView{}, fmt.Errorf("invalid learning action")
	}
	if _, err := s.sessions.GetByIDAndUserID(ctx, request.SessionID, request.UserID); err != nil {
		return AnswerView{}, err
	}
	session, err := s.learning.GetByID(ctx, request.UserID, request.SessionID, request.LearningSessionID)
	if err != nil {
		return AnswerView{}, err
	}
	result, err := s.answers.SkipQuestion(ctx, &model.LearningAttempt{ID: uuid.NewString(), UserID: request.UserID, SessionID: request.SessionID, LearningSessionID: request.LearningSessionID, QuestionID: request.QuestionID, QuestionOrdinal: session.CurrentQuestion, SubmissionID: request.SubmissionID}, s.now())
	if errors.Is(err, learningdao.ErrSubmissionConflict) {
		return AnswerView{}, ErrLearningSubmission
	}
	if err != nil {
		return AnswerView{}, err
	}
	run, _ := s.runs.GetRun(ctx, request.UserID, request.SessionID, session.AgentRunID)
	s.finishAnswerRun(ctx, run, result)
	return buildAnswerView(result), nil
}

func (s *Service) Progress(ctx context.Context, request ProgressRequest) (*model.TopicProgress, error) {
	if s.answers == nil || request.UserID <= 0 {
		return nil, ErrServiceNotConfigured
	}
	topicKey := model.NormalizeLearningTopicKey(request.Topic)
	if topicKey == "" {
		return nil, fmt.Errorf("invalid learning topic")
	}
	return s.answers.GetTopicProgress(ctx, request.UserID, topicKey)
}

func (s *Service) Correct(ctx context.Context, request CorrectRequest) (AttemptView, *model.TopicProgress, error) {
	if s.answers == nil || request.UserID <= 0 || strings.TrimSpace(request.AttemptID) == "" || strings.TrimSpace(request.Reason) == "" || request.EffectiveTotal < 0 || request.EffectiveTotal > 100 {
		return AttemptView{}, nil, fmt.Errorf("invalid learning correction")
	}
	attempt, progress, err := s.answers.CorrectAttempt(ctx, request.UserID, request.AttemptID, request.EffectiveTotal, request.Reason, s.now())
	if err != nil {
		return AttemptView{}, nil, err
	}
	return buildAttemptView(attempt), progress, nil
}

func validateAnswerRequest(request AnswerRequest) error {
	if request.UserID <= 0 || strings.TrimSpace(request.SessionID) == "" || strings.TrimSpace(request.LearningSessionID) == "" || strings.TrimSpace(request.QuestionID) == "" || strings.TrimSpace(request.SubmissionID) == "" || strings.TrimSpace(request.Answer) == "" {
		return fmt.Errorf("invalid learning answer")
	}
	if len([]rune(strings.TrimSpace(request.Answer))) > 12000 || len(request.SubmissionID) > 64 {
		return fmt.Errorf("invalid learning answer")
	}
	return nil
}

func (s *Service) replayAnswer(ctx context.Context, claim *learningdao.ClaimAnswerResult) (AnswerView, error) {
	result := &learningdao.CompleteAnswerResult{Attempt: claim.Attempt, Session: claim.Session}
	if claim.Session.Status == model.LearningSessionWaitingAnswer {
		result.NextQuestion, _ = s.learning.GetQuestion(ctx, claim.Session.UserID, claim.Session.ID, claim.Session.CurrentQuestion)
	}
	result.Progress, _ = s.answers.GetTopicProgress(ctx, claim.Session.UserID, claim.Attempt.TopicKey)
	run, _ := s.runs.GetRun(ctx, claim.Session.UserID, claim.Session.SessionID, claim.Session.AgentRunID)
	s.finishAnswerRun(ctx, run, result)
	return buildAnswerView(result), nil
}

func (s *Service) moveRunToGrading(ctx context.Context, session *model.LearningSession) (*model.AgentRun, error) {
	run, err := s.runs.GetRun(ctx, session.UserID, session.SessionID, session.AgentRunID)
	if err != nil {
		return nil, err
	}
	if run.Status == model.AgentRunRunning {
		return run, nil
	}
	if run.Status != model.AgentRunWaitingUser {
		return nil, learningdao.ErrLearningStateConflict
	}
	return s.runs.TransitionRun(ctx, run.UserID, run.SessionID, run.ID, run.Version, model.AgentRunWaitingUser, model.AgentRunRunning, "", "")
}

func (s *Service) failGrading(ctx context.Context, claim *learningdao.ClaimAnswerResult, run *model.AgentRun, cause error) {
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	_, _ = s.answers.FailAnswer(finishCtx, claim.Session.UserID, claim.Attempt.ID, claim.Attempt.LeaseToken, cause.Error(), s.now())
	if run != nil && run.Status == model.AgentRunRunning {
		_, _ = s.runs.TransitionRun(finishCtx, run.UserID, run.SessionID, run.ID, run.Version, model.AgentRunRunning, model.AgentRunWaitingUser, "grading_retry", "")
	}
}

func (s *Service) finishAnswerRun(ctx context.Context, run *model.AgentRun, result *learningdao.CompleteAnswerResult) {
	if run == nil || result == nil || result.Session == nil || run.Status.Terminal() {
		return
	}
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if run.Status == model.AgentRunWaitingUser {
		moved, err := s.runs.TransitionRun(finishCtx, run.UserID, run.SessionID, run.ID, run.Version, model.AgentRunWaitingUser, model.AgentRunRunning, "", "")
		if err == nil {
			run = moved
		}
	}
	if run.Status != model.AgentRunRunning {
		return
	}
	if result.Session.Status == model.LearningSessionCompleted {
		_, _ = s.runs.TransitionRun(finishCtx, run.UserID, run.SessionID, run.ID, run.Version, model.AgentRunRunning, model.AgentRunCompleted, "learning_completed", "")
		_ = s.completion.PublishLearningCompleted(finishCtx, LearningCompletionEvent{UserID: result.Session.UserID, SessionID: result.Session.SessionID, LearningSessionID: result.Session.ID, TopicKey: result.Session.TopicKey, CompletedAt: s.now()})
	} else {
		_, _ = s.runs.TransitionRun(finishCtx, run.UserID, run.SessionID, run.ID, run.Version, model.AgentRunRunning, model.AgentRunWaitingUser, "waiting_for_answer", "")
	}
}

func buildAnswerView(result *learningdao.CompleteAnswerResult) AnswerView {
	if result == nil {
		return AnswerView{}
	}
	return AnswerView{Learning: buildSessionView(result.Session, result.NextQuestion), Attempt: buildAttemptView(result.Attempt), Progress: result.Progress}
}

func buildAttemptView(attempt *model.LearningAttempt) AttemptView {
	if attempt == nil {
		return AttemptView{}
	}
	return AttemptView{ID: attempt.ID, SubmissionID: attempt.SubmissionID, QuestionID: attempt.QuestionID, Status: attempt.Status, Correctness: attempt.AutoCorrectness, Completeness: attempt.AutoCompleteness, Clarity: attempt.AutoClarity, AutoTotal: attempt.AutoTotal, EffectiveTotal: attempt.EffectiveTotal, Feedback: attempt.Feedback, MissingPoints: append([]string(nil), attempt.MissingPoints...), Confidence: attempt.GradingConfidence, CorrectionReason: attempt.CorrectionReason}
}
