package learning

import (
	agentdao "GopherAI/dao/agent"
	learningdao "GopherAI/dao/learning"
	sessiondao "GopherAI/dao/session"
	"GopherAI/model"
	agentservice "GopherAI/service/agent"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

var (
	ErrServiceNotConfigured   = errors.New("learning service is not configured")
	ErrLearningSessionActive  = errors.New("an active learning session already exists")
	ErrLearningSessionGone    = errors.New("learning session does not exist")
	ErrLearningSessionExpired = errors.New("learning session has expired")
	ErrLearningAnswerBusy     = errors.New("learning answer is being graded")
	ErrLearningSubmission     = errors.New("learning submission conflicts with an existing submission")
)

const defaultQuestionCount = 3

const questionGenerationTimeout = 45 * time.Second

type SessionRepository interface {
	GetByIDAndUserID(context.Context, string, int64) (*model.Session, error)
}

type LearningRepository interface {
	CreateSession(context.Context, *model.LearningSession) error
	SaveQuestionsAndWait(context.Context, int64, string, string, uint64, []model.LearningQuestion) (*model.LearningSession, error)
	GetByID(context.Context, int64, string, string) (*model.LearningSession, error)
	GetActive(context.Context, int64, string) (*model.LearningSession, error)
	GetQuestion(context.Context, int64, string, int) (*model.LearningQuestion, error)
	ListTopicQuestions(context.Context, int64, string, int) ([]model.LearningQuestion, error)
	Transition(context.Context, int64, string, string, uint64, model.LearningSessionStatus, model.LearningSessionStatus) (*model.LearningSession, error)
	ExpireIfNeeded(context.Context, *model.LearningSession, time.Time) (*model.LearningSession, error)
}

type AnswerRepository interface {
	ClaimAnswer(context.Context, learningdao.ClaimAnswerInput) (*learningdao.ClaimAnswerResult, error)
	FailAnswer(context.Context, int64, string, string, string, time.Time) (*model.LearningSession, error)
	CompleteAnswer(context.Context, int64, string, string, learningdao.GradeCommit, time.Time) (*learningdao.CompleteAnswerResult, error)
	SkipQuestion(context.Context, *model.LearningAttempt, time.Time) (*learningdao.CompleteAnswerResult, error)
	GetTopicProgress(context.Context, int64, string) (*model.TopicProgress, error)
	CorrectAttempt(context.Context, int64, string, int, string, time.Time) (*model.LearningAttempt, *model.TopicProgress, error)
}

type RunRepository interface {
	CreateRun(context.Context, *model.AgentRun) error
	AppendStep(context.Context, string, *model.AgentStep) error
	GetRun(context.Context, int64, string, string) (*model.AgentRun, error)
	TransitionRun(context.Context, int64, string, string, uint64, model.AgentRunStatus, model.AgentRunStatus, string, string) (*model.AgentRun, error)
}

type Service struct {
	sessions   SessionRepository
	learning   LearningRepository
	runs       RunRepository
	evidence   EvidenceProvider
	generator  QuestionGenerator
	answers    AnswerRepository
	grader     AnswerGrader
	completion LearningCompletionPublisher
	now        func() time.Time
}

type StartRequest struct {
	UserID    int64
	UserName  string
	SessionID string
	Topic     string
}

type CurrentRequest struct {
	UserID    int64
	SessionID string
}

type CancelRequest struct {
	UserID            int64
	SessionID         string
	LearningSessionID string
}

type EvidenceSummary struct {
	DocumentID   string   `json:"documentId"`
	DocumentName string   `json:"documentName"`
	DocVersion   int      `json:"documentVersion"`
	TitlePath    []string `json:"titlePath"`
	StartLine    int      `json:"startLine"`
	EndLine      int      `json:"endLine"`
}

type QuestionView struct {
	ID       string                     `json:"id"`
	Ordinal  int                        `json:"ordinal"`
	Type     model.LearningQuestionType `json:"type"`
	Question string                     `json:"question"`
	Sources  []EvidenceSummary          `json:"sources"`
}

type SessionView struct {
	ID              string                      `json:"id"`
	RunID           string                      `json:"runId"`
	SessionID       string                      `json:"sessionId"`
	Topic           string                      `json:"topic"`
	TopicKey        string                      `json:"topicKey"`
	Status          model.LearningSessionStatus `json:"status"`
	CurrentQuestion int                         `json:"currentQuestion"`
	TotalQuestions  int                         `json:"totalQuestions"`
	ExpiresAt       time.Time                   `json:"expiresAt"`
	Question        *QuestionView               `json:"question,omitempty"`
}

func NewService(
	sessions SessionRepository,
	learning LearningRepository,
	runs RunRepository,
	evidence EvidenceProvider,
	generator QuestionGenerator,
) (*Service, error) {
	if sessions == nil || learning == nil || runs == nil || evidence == nil || generator == nil {
		return nil, fmt.Errorf("learning service dependencies must not be nil")
	}
	service := &Service{sessions: sessions, learning: learning, runs: runs, evidence: evidence, generator: generator, completion: DiscardLearningCompletions, now: time.Now}
	if answers, ok := learning.(AnswerRepository); ok {
		service.answers = answers
	}
	return service, nil
}

func NewDatabaseService(evidence EvidenceProvider, generator QuestionGenerator, grader AnswerGrader) (*Service, error) {
	repository := learningdao.DefaultRepository()
	service, err := NewService(sessiondao.DefaultRepository(), repository, agentdao.DefaultRepository(), evidence, generator)
	if err != nil {
		return nil, err
	}
	if grader == nil {
		return nil, fmt.Errorf("learning answer grader must not be nil")
	}
	service.answers, service.grader = repository, grader
	return service, nil
}

func (s *Service) SetCompletionPublisher(publisher LearningCompletionPublisher) {
	if publisher == nil {
		publisher = DiscardLearningCompletions
	}
	s.completion = publisher
}

func (s *Service) Start(ctx context.Context, request StartRequest, sink agentservice.EventSink) (SessionView, error) {
	if err := validateStartRequest(request); err != nil {
		return SessionView{}, err
	}
	if sink == nil {
		sink = agentservice.DiscardEvents
	}
	owned, err := s.sessions.GetByIDAndUserID(ctx, request.SessionID, request.UserID)
	if err != nil {
		return SessionView{}, err
	}
	if owned.Mode != model.SessionModeAgent {
		return SessionView{}, agentservice.ErrAgentSessionRequired
	}
	if active, activeErr := s.learning.GetActive(ctx, owned.UserID, owned.ID); activeErr == nil {
		active, expireErr := s.learning.ExpireIfNeeded(ctx, active, s.now())
		if expireErr != nil {
			return SessionView{}, expireErr
		}
		if !active.Status.Terminal() {
			return SessionView{}, ErrLearningSessionActive
		}
	} else if !errors.Is(activeErr, learningdao.ErrLearningSessionNotFound) {
		return SessionView{}, activeErr
	}

	now := s.now()
	run := &model.AgentRun{
		ID: uuid.NewString(), UserID: owned.UserID, SessionID: owned.ID,
		ExecutionKind: model.AgentRunExecutionLearning,
		Status:        model.AgentRunRunning, Version: 1, StartedAt: now,
		InputSummary: "学习测试：" + truncateText(request.Topic, 900),
	}
	if err := s.runs.CreateRun(ctx, run); err != nil {
		return SessionView{}, fmt.Errorf("create learning agent run: %w", err)
	}
	if err := sink.Send(ctx, agentservice.Event{Type: agentservice.EventRunStarted, RunID: run.ID}); err != nil {
		return SessionView{RunID: run.ID}, s.failRun(ctx, run, nil, sink, "client_disconnected", err)
	}

	activeKey := model.LearningActiveKey(owned.UserID, owned.ID)
	learningSession := &model.LearningSession{
		ID: uuid.NewString(), UserID: owned.UserID, SessionID: owned.ID, AgentRunID: run.ID,
		Topic: strings.TrimSpace(request.Topic), TopicKey: model.NormalizeLearningTopicKey(request.Topic),
		Status: model.LearningSessionCreated, CurrentQuestion: 1, TotalQuestions: defaultQuestionCount,
		Version: 1, ActiveKey: &activeKey, ExpiresAt: now.Add(24 * time.Hour),
	}
	if err := s.learning.CreateSession(ctx, learningSession); err != nil {
		if errors.Is(err, learningdao.ErrActiveLearningSession) {
			err = ErrLearningSessionActive
		}
		return SessionView{RunID: run.ID}, s.failRun(ctx, run, nil, sink, "learning_session_create_failed", err)
	}
	learningSession, err = s.learning.Transition(
		ctx, owned.UserID, owned.ID, learningSession.ID, learningSession.Version,
		model.LearningSessionCreated, model.LearningSessionAsking,
	)
	if err != nil {
		return SessionView{RunID: run.ID}, s.failRun(ctx, run, learningSession, sink, "learning_state_failed", err)
	}
	if err := s.appendStep(ctx, run, 1, model.AgentStepPlan, "", "正在根据个人笔记生成测试题", nil); err != nil {
		return SessionView{RunID: run.ID}, s.failRun(ctx, run, learningSession, sink, "learning_step_failed", err)
	}
	if err := sink.Send(ctx, agentservice.Event{
		Type: agentservice.EventStepStarted, RunID: run.ID, Sequence: 1,
		StepType: string(model.AgentStepPlan), Message: "正在根据个人笔记生成测试题",
	}); err != nil {
		return SessionView{RunID: run.ID}, s.failRun(ctx, run, learningSession, sink, "client_disconnected", err)
	}

	generationCtx, cancelGeneration := context.WithTimeout(ctx, questionGenerationTimeout)
	history, err := s.learning.ListTopicQuestions(generationCtx, owned.UserID, learningSession.TopicKey, maxQuestionHistory)
	if err != nil {
		cancelGeneration()
		return SessionView{RunID: run.ID}, s.failRun(ctx, run, learningSession, sink, "learning_history_failed", err)
	}
	weakPoints, err := s.reliableWeakPoints(generationCtx, owned.UserID, learningSession.TopicKey)
	if err != nil {
		cancelGeneration()
		return SessionView{RunID: run.ID}, s.failRun(ctx, run, learningSession, sink, "learning_progress_failed", err)
	}
	evidence, err := s.evidence.Evidence(generationCtx, EvidenceRequest{
		UserID: owned.UserID, Topic: learningSession.Topic, History: history, WeakPoints: weakPoints,
	})
	if err != nil {
		cancelGeneration()
		return SessionView{RunID: run.ID}, s.failRun(ctx, run, learningSession, sink, "learning_evidence_failed", err)
	}
	generated, err := s.generator.Generate(generationCtx, QuestionGenerationRequest{
		Topic: learningSession.Topic, Evidence: evidence, History: history,
		WeakPoints: weakPoints, Count: defaultQuestionCount,
	})
	cancelGeneration()
	if err != nil {
		return SessionView{RunID: run.ID}, s.failRun(ctx, run, learningSession, sink, "question_generation_failed", err)
	}
	questions := make([]model.LearningQuestion, 0, len(generated))
	for i, item := range generated {
		questions = append(questions, model.LearningQuestion{
			ID: uuid.NewString(), UserID: owned.UserID, LearningSessionID: learningSession.ID,
			Ordinal: i + 1, Type: item.Type, Question: item.Question,
			AssessmentGoal:  item.AssessmentGoal,
			ReferenceAnswer: item.ReferenceAnswer, Rubric: append([]string(nil), item.Rubric...),
			Evidence: append([]model.LearningEvidence(nil), item.Evidence...),
		})
	}
	learningSession, err = s.learning.SaveQuestionsAndWait(
		ctx, owned.UserID, owned.ID, learningSession.ID, learningSession.Version, questions,
	)
	if err != nil {
		return SessionView{RunID: run.ID}, s.failRun(ctx, run, learningSession, sink, "question_persistence_failed", err)
	}
	current := &questions[0]
	if err := s.appendStep(ctx, run, 2, model.AgentStepAskUser, "", truncateText(current.Question, 1000), evidenceSourceIDs(current.Evidence)); err != nil {
		return SessionView{RunID: run.ID}, s.failRun(ctx, run, learningSession, sink, "learning_step_failed", err)
	}
	waitingRun, err := s.runs.TransitionRun(
		ctx, run.UserID, run.SessionID, run.ID, run.Version,
		model.AgentRunRunning, model.AgentRunWaitingUser, "waiting_for_answer", truncateText(current.Question, 1000),
	)
	if err != nil {
		return SessionView{RunID: run.ID}, s.failRun(ctx, run, learningSession, sink, "run_waiting_failed", err)
	}
	run.Version = waitingRun.Version
	view := buildSessionView(learningSession, current)
	if err := sink.Send(ctx, agentservice.Event{
		Type: agentservice.EventWaitingUser, RunID: run.ID,
		LearningSessionID: learningSession.ID, QuestionID: current.ID,
		Topic: learningSession.Topic, CurrentQuestion: 1, TotalQuestions: learningSession.TotalQuestions,
		QuestionType: string(current.Type), Content: current.Question,
	}); err != nil {
		return view, err
	}
	return view, nil
}

func (s *Service) reliableWeakPoints(ctx context.Context, userID int64, topicKey string) ([]string, error) {
	if s.answers == nil {
		return nil, nil
	}
	progress, err := s.answers.GetTopicProgress(ctx, userID, topicKey)
	if errors.Is(err, learningdao.ErrAttemptNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if progress == nil || progress.ValidAttempts < 3 || progress.Level == model.TopicProgressInsufficient {
		return nil, nil
	}
	return append([]string(nil), progress.WeakPoints...), nil
}

func (s *Service) Current(ctx context.Context, request CurrentRequest) (SessionView, error) {
	if request.UserID <= 0 || strings.TrimSpace(request.SessionID) == "" {
		return SessionView{}, fmt.Errorf("invalid current learning request")
	}
	owned, err := s.sessions.GetByIDAndUserID(ctx, request.SessionID, request.UserID)
	if err != nil {
		return SessionView{}, err
	}
	if owned.Mode != model.SessionModeAgent {
		return SessionView{}, agentservice.ErrAgentSessionRequired
	}
	session, err := s.learning.GetActive(ctx, request.UserID, request.SessionID)
	if errors.Is(err, learningdao.ErrLearningSessionNotFound) {
		return SessionView{}, ErrLearningSessionGone
	}
	if err != nil {
		return SessionView{}, err
	}
	session, err = s.learning.ExpireIfNeeded(ctx, session, s.now())
	if err != nil {
		return SessionView{}, err
	}
	if session.Status == model.LearningSessionExpired {
		s.finishExpiredRun(ctx, session)
		return buildSessionView(session, nil), ErrLearningSessionExpired
	}
	if session.Status != model.LearningSessionWaitingAnswer && session.Status != model.LearningSessionGrading {
		return buildSessionView(session, nil), nil
	}
	question, err := s.learning.GetQuestion(ctx, request.UserID, session.ID, session.CurrentQuestion)
	if err != nil {
		return SessionView{}, err
	}
	return buildSessionView(session, question), nil
}

func (s *Service) Cancel(ctx context.Context, request CancelRequest) (SessionView, error) {
	if request.UserID <= 0 || strings.TrimSpace(request.SessionID) == "" || strings.TrimSpace(request.LearningSessionID) == "" {
		return SessionView{}, fmt.Errorf("invalid cancel learning request")
	}
	if _, err := s.sessions.GetByIDAndUserID(ctx, request.SessionID, request.UserID); err != nil {
		return SessionView{}, err
	}
	session, err := s.learning.GetByID(ctx, request.UserID, request.SessionID, request.LearningSessionID)
	if errors.Is(err, learningdao.ErrLearningSessionNotFound) {
		return SessionView{}, ErrLearningSessionGone
	}
	if err != nil {
		return SessionView{}, err
	}
	if session.Status.Terminal() {
		return buildSessionView(session, nil), learningdao.ErrLearningStateConflict
	}
	cancelled, err := s.learning.Transition(
		ctx, session.UserID, session.SessionID, session.ID, session.Version,
		session.Status, model.LearningSessionCancelled,
	)
	if err != nil {
		return SessionView{}, err
	}
	if run, runErr := s.runs.GetRun(ctx, session.UserID, session.SessionID, session.AgentRunID); runErr == nil && !run.Status.Terminal() {
		_, _ = s.runs.TransitionRun(ctx, run.UserID, run.SessionID, run.ID, run.Version, run.Status, model.AgentRunCancelled, "learning_cancelled", "")
	}
	return buildSessionView(cancelled, nil), nil
}

func (s *Service) appendStep(ctx context.Context, run *model.AgentRun, sequence int, stepType model.AgentStepType, toolName, result string, sourceIDs []string) error {
	return s.runs.AppendStep(ctx, run.SessionID, &model.AgentStep{
		UserID: run.UserID, RunID: run.ID, Sequence: sequence, Type: stepType,
		ToolName: toolName, ResultSummary: result, SourceIDs: append([]string(nil), sourceIDs...),
	})
}

func (s *Service) failRun(ctx context.Context, run *model.AgentRun, learningSession *model.LearningSession, sink agentservice.EventSink, reason string, cause error) error {
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if learningSession != nil && !learningSession.Status.Terminal() {
		if cancelled, err := s.learning.Transition(
			finishCtx, learningSession.UserID, learningSession.SessionID, learningSession.ID,
			learningSession.Version, learningSession.Status, model.LearningSessionCancelled,
		); err == nil {
			learningSession = cancelled
		}
	}
	if run != nil && !run.Status.Terminal() {
		if transitioned, err := s.runs.TransitionRun(
			finishCtx, run.UserID, run.SessionID, run.ID, run.Version,
			run.Status, model.AgentRunFailed, reason, "",
		); err == nil {
			run.Version = transitioned.Version
			run.Status = transitioned.Status
		}
	}
	_ = sink.Send(finishCtx, agentservice.Event{
		Type: agentservice.EventRunFailed, RunID: run.ID, Message: "学习测试创建失败", TerminationReason: reason,
	})
	return cause
}

func (s *Service) finishExpiredRun(ctx context.Context, session *model.LearningSession) {
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	run, err := s.runs.GetRun(finishCtx, session.UserID, session.SessionID, session.AgentRunID)
	if err != nil || run.Status.Terminal() {
		return
	}
	_, _ = s.runs.TransitionRun(finishCtx, run.UserID, run.SessionID, run.ID, run.Version, run.Status, model.AgentRunTimeout, "learning_expired", "")
}

func buildSessionView(session *model.LearningSession, question *model.LearningQuestion) SessionView {
	if session == nil {
		return SessionView{}
	}
	view := SessionView{
		ID: session.ID, RunID: session.AgentRunID, SessionID: session.SessionID,
		Topic: session.Topic, TopicKey: session.TopicKey, Status: session.Status,
		CurrentQuestion: session.CurrentQuestion, TotalQuestions: session.TotalQuestions, ExpiresAt: session.ExpiresAt,
	}
	if question != nil {
		sources := make([]EvidenceSummary, 0, len(question.Evidence))
		for _, evidence := range question.Evidence {
			sources = append(sources, EvidenceSummary{
				DocumentID: evidence.DocumentID, DocumentName: evidence.DocumentName, DocVersion: evidence.DocVersion,
				TitlePath: append([]string(nil), evidence.TitlePath...), StartLine: evidence.StartLine, EndLine: evidence.EndLine,
			})
		}
		view.Question = &QuestionView{
			ID: question.ID, Ordinal: question.Ordinal, Type: question.Type,
			Question: question.Question, Sources: sources,
		}
	}
	return view
}

func evidenceSourceIDs(evidence []model.LearningEvidence) []string {
	ids := make([]string, 0, len(evidence))
	for _, item := range evidence {
		ids = append(ids, fmt.Sprintf("%s:v%d:%s", item.DocumentID, item.DocVersion, item.ChunkID))
	}
	return ids
}

func validateStartRequest(request StartRequest) error {
	if request.UserID <= 0 || strings.TrimSpace(request.UserName) == "" || strings.TrimSpace(request.SessionID) == "" || strings.TrimSpace(request.Topic) == "" {
		return fmt.Errorf("invalid learning start request")
	}
	if model.NormalizeLearningTopicKey(request.Topic) == "" || len([]rune(strings.TrimSpace(request.Topic))) > 160 {
		return fmt.Errorf("invalid learning topic")
	}
	return nil
}

func truncateText(value string, limit int) string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) <= limit {
		return string(runes)
	}
	return string(runes[:limit])
}

var defaultService struct {
	sync.RWMutex
	service *Service
}

func SetDefaultService(service *Service) error {
	if service == nil {
		return fmt.Errorf("default learning service must not be nil")
	}
	defaultService.Lock()
	defaultService.service = service
	defaultService.Unlock()
	return nil
}

func DefaultService() (*Service, error) {
	defaultService.RLock()
	service := defaultService.service
	defaultService.RUnlock()
	if service == nil {
		return nil, ErrServiceNotConfigured
	}
	return service, nil
}
