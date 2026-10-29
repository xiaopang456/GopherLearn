package learning

import (
	agentdao "GopherAI/dao/agent"
	learningdao "GopherAI/dao/learning"
	sessiondao "GopherAI/dao/session"
	"GopherAI/model"
	agentservice "GopherAI/service/agent"
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type memorySessionRepository struct {
	session model.Session
}

func (r memorySessionRepository) GetByIDAndUserID(_ context.Context, id string, userID int64) (*model.Session, error) {
	if r.session.ID != id || r.session.UserID != userID {
		return nil, sessiondao.ErrSessionNotFound
	}
	copy := r.session
	return &copy, nil
}

type memoryLearningRepository struct {
	mu        sync.Mutex
	session   *model.LearningSession
	questions map[int]model.LearningQuestion
	history   []model.LearningQuestion
}

func (r *memoryLearningRepository) CreateSession(_ context.Context, session *model.LearningSession) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.session != nil && r.session.ActiveKey != nil && !r.session.Status.Terminal() {
		return learningdao.ErrActiveLearningSession
	}
	copy := *session
	r.session = &copy
	r.questions = make(map[int]model.LearningQuestion)
	return nil
}

func (r *memoryLearningRepository) SaveQuestionsAndWait(_ context.Context, userID int64, chatID, learningID string, version uint64, questions []model.LearningQuestion) (*model.LearningSession, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.session == nil || r.session.UserID != userID || r.session.SessionID != chatID || r.session.ID != learningID {
		return nil, learningdao.ErrLearningSessionNotFound
	}
	if r.session.Status != model.LearningSessionAsking || r.session.Version != version {
		return nil, learningdao.ErrLearningStateConflict
	}
	for _, question := range questions {
		r.questions[question.Ordinal] = question
	}
	r.session.Status = model.LearningSessionWaitingAnswer
	r.session.Version++
	copy := *r.session
	return &copy, nil
}

func (r *memoryLearningRepository) GetByID(_ context.Context, userID int64, chatID, learningID string) (*model.LearningSession, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.session == nil || r.session.UserID != userID || r.session.SessionID != chatID || r.session.ID != learningID {
		return nil, learningdao.ErrLearningSessionNotFound
	}
	copy := *r.session
	return &copy, nil
}

func (r *memoryLearningRepository) GetActive(_ context.Context, userID int64, chatID string) (*model.LearningSession, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.session == nil || r.session.UserID != userID || r.session.SessionID != chatID || r.session.ActiveKey == nil || r.session.Status.Terminal() {
		return nil, learningdao.ErrLearningSessionNotFound
	}
	copy := *r.session
	return &copy, nil
}

func (r *memoryLearningRepository) GetQuestion(_ context.Context, userID int64, learningID string, ordinal int) (*model.LearningQuestion, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	question, ok := r.questions[ordinal]
	if !ok || question.UserID != userID || question.LearningSessionID != learningID {
		return nil, learningdao.ErrLearningSessionNotFound
	}
	copy := question
	return &copy, nil
}

func (r *memoryLearningRepository) ListTopicQuestions(_ context.Context, userID int64, _ string, limit int) ([]model.LearningQuestion, error) {
	result := make([]model.LearningQuestion, 0, minInt(limit, len(r.history)))
	for _, question := range r.history {
		if question.UserID == userID && len(result) < limit {
			result = append(result, question)
		}
	}
	return result, nil
}

func (r *memoryLearningRepository) Transition(_ context.Context, userID int64, chatID, learningID string, version uint64, from, to model.LearningSessionStatus) (*model.LearningSession, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.session == nil || r.session.UserID != userID || r.session.SessionID != chatID || r.session.ID != learningID {
		return nil, learningdao.ErrLearningSessionNotFound
	}
	if r.session.Status != from || r.session.Version != version || !from.CanTransition(to) {
		return nil, learningdao.ErrLearningStateConflict
	}
	r.session.Status = to
	r.session.Version++
	if to.Terminal() {
		r.session.ActiveKey = nil
		now := time.Now()
		r.session.CompletedAt = &now
	}
	copy := *r.session
	return &copy, nil
}

func (r *memoryLearningRepository) ExpireIfNeeded(ctx context.Context, session *model.LearningSession, now time.Time) (*model.LearningSession, error) {
	if session.ExpiresAt.After(now) || session.Status.Terminal() {
		return session, nil
	}
	return r.Transition(ctx, session.UserID, session.SessionID, session.ID, session.Version, session.Status, model.LearningSessionExpired)
}

type memoryRunRepository struct {
	mu    sync.Mutex
	runs  map[string]model.AgentRun
	steps []model.AgentStep
}

func newMemoryRunRepository() *memoryRunRepository {
	return &memoryRunRepository{runs: make(map[string]model.AgentRun)}
}

func (r *memoryRunRepository) CreateRun(_ context.Context, run *model.AgentRun) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runs[run.ID] = *run
	return nil
}

func (r *memoryRunRepository) AppendStep(_ context.Context, _ string, step *model.AgentStep) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.steps = append(r.steps, *step)
	return nil
}

func (r *memoryRunRepository) GetRun(_ context.Context, userID int64, chatID, runID string) (*model.AgentRun, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	run, ok := r.runs[runID]
	if !ok || run.UserID != userID || run.SessionID != chatID {
		return nil, agentdao.ErrRunNotFound
	}
	return &run, nil
}

func (r *memoryRunRepository) TransitionRun(_ context.Context, userID int64, chatID, runID string, version uint64, from, to model.AgentRunStatus, reason, answer string) (*model.AgentRun, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	run, ok := r.runs[runID]
	if !ok || run.UserID != userID || run.SessionID != chatID {
		return nil, agentdao.ErrRunNotFound
	}
	if run.Status != from || run.Version != version || !from.CanTransition(to) {
		return nil, agentdao.ErrRunConflict
	}
	run.Status = to
	run.Version++
	run.TerminationReason = reason
	run.AnswerSummary = answer
	if to.Terminal() {
		now := time.Now()
		run.FinishedAt = &now
	}
	r.runs[runID] = run
	return &run, nil
}

type staticEvidenceProvider struct {
	evidence []model.LearningEvidence
	err      error
}

func (p staticEvidenceProvider) Evidence(context.Context, EvidenceRequest) ([]model.LearningEvidence, error) {
	return append([]model.LearningEvidence(nil), p.evidence...), p.err
}

type staticQuestionGenerator struct {
	questions []GeneratedQuestion
	err       error
}

type capturingEvidenceProvider struct {
	request  EvidenceRequest
	evidence []model.LearningEvidence
}

func (p *capturingEvidenceProvider) Evidence(_ context.Context, request EvidenceRequest) ([]model.LearningEvidence, error) {
	p.request = request
	return append([]model.LearningEvidence(nil), p.evidence...), nil
}

type capturingQuestionGenerator struct {
	request   QuestionGenerationRequest
	questions []GeneratedQuestion
}

func (g *capturingQuestionGenerator) Generate(_ context.Context, request QuestionGenerationRequest) ([]GeneratedQuestion, error) {
	g.request = request
	return append([]GeneratedQuestion(nil), g.questions...), nil
}

func (g staticQuestionGenerator) Generate(context.Context, QuestionGenerationRequest) ([]GeneratedQuestion, error) {
	return append([]GeneratedQuestion(nil), g.questions...), g.err
}

func learningQuestions(evidence []model.LearningEvidence) []GeneratedQuestion {
	return []GeneratedQuestion{
		{Type: model.LearningQuestionShortAnswer, Question: "问题一", AssessmentGoal: "目标一", ReferenceAnswer: "答案一", Rubric: []string{"要点一"}, Evidence: evidence},
		{Type: model.LearningQuestionConceptComparison, Question: "问题二", AssessmentGoal: "目标二", ReferenceAnswer: "答案二", Rubric: []string{"要点二"}, Evidence: evidence},
		{Type: model.LearningQuestionCodeAnalysis, Question: "问题三", AssessmentGoal: "目标三", ReferenceAnswer: "答案三", Rubric: []string{"要点三"}, Evidence: evidence},
	}
}

func TestServiceStartPersistsWaitingSessionAndRecovers(t *testing.T) {
	chat := model.Session{ID: "chat-1", UserID: 7, UserName: "alice", Mode: model.SessionModeAgent}
	learningRepo := &memoryLearningRepository{}
	runRepo := newMemoryRunRepository()
	evidence := testEvidence()
	service, err := NewService(
		memorySessionRepository{session: chat}, learningRepo, runRepo,
		staticEvidenceProvider{evidence: evidence}, staticQuestionGenerator{questions: learningQuestions(evidence)},
	)
	if err != nil {
		t.Fatal(err)
	}
	fixedNow := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return fixedNow }
	var events []agentservice.Event
	sink := agentservice.EventSinkFunc(func(_ context.Context, event agentservice.Event) error {
		events = append(events, event)
		return nil
	})

	view, err := service.Start(context.Background(), StartRequest{
		UserID: chat.UserID, UserName: chat.UserName, SessionID: chat.ID, Topic: " Redis 缓存 ",
	}, sink)
	if err != nil {
		t.Fatal(err)
	}
	if view.Status != model.LearningSessionWaitingAnswer || view.TopicKey != "redis-缓存" || view.Question == nil || view.Question.Question != "问题一" {
		t.Fatalf("learning view = %#v", view)
	}
	if len(events) != 3 || events[2].Type != agentservice.EventWaitingUser || events[2].LearningSessionID != view.ID {
		t.Fatalf("events = %#v", events)
	}
	if len(runRepo.steps) != 2 || runRepo.runs[view.RunID].Status != model.AgentRunWaitingUser {
		t.Fatalf("steps/runs = %#v / %#v", runRepo.steps, runRepo.runs)
	}

	recovered, err := service.Current(context.Background(), CurrentRequest{UserID: chat.UserID, SessionID: chat.ID})
	if err != nil || recovered.Question == nil || recovered.Question.ID != view.Question.ID {
		t.Fatalf("recovered = %#v, err = %v", recovered, err)
	}
	if _, err := service.Start(context.Background(), StartRequest{
		UserID: chat.UserID, UserName: chat.UserName, SessionID: chat.ID, Topic: "Gin",
	}, agentservice.DiscardEvents); !errors.Is(err, ErrLearningSessionActive) {
		t.Fatalf("second start error = %v, want ErrLearningSessionActive", err)
	}

	cancelled, err := service.Cancel(context.Background(), CancelRequest{
		UserID: chat.UserID, SessionID: chat.ID, LearningSessionID: view.ID,
	})
	if err != nil || cancelled.Status != model.LearningSessionCancelled {
		t.Fatalf("cancelled = %#v, err = %v", cancelled, err)
	}
	if _, err := service.Current(context.Background(), CurrentRequest{UserID: chat.UserID, SessionID: chat.ID}); !errors.Is(err, ErrLearningSessionGone) {
		t.Fatalf("current after cancel error = %v", err)
	}
}

func TestServiceGenerationFailureCancelsReservedSession(t *testing.T) {
	chat := model.Session{ID: "chat-2", UserID: 8, UserName: "bob", Mode: model.SessionModeAgent}
	learningRepo := &memoryLearningRepository{}
	runRepo := newMemoryRunRepository()
	service, err := NewService(
		memorySessionRepository{session: chat}, learningRepo, runRepo,
		staticEvidenceProvider{err: ErrNoLearningEvidence}, staticQuestionGenerator{},
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Start(context.Background(), StartRequest{
		UserID: chat.UserID, UserName: chat.UserName, SessionID: chat.ID, Topic: "missing",
	}, agentservice.DiscardEvents)
	if !errors.Is(err, ErrNoLearningEvidence) {
		t.Fatalf("start error = %v, want ErrNoLearningEvidence", err)
	}
	if learningRepo.session == nil || learningRepo.session.Status != model.LearningSessionCancelled || learningRepo.session.ActiveKey != nil {
		t.Fatalf("learning session after failure = %#v", learningRepo.session)
	}
	for _, run := range runRepo.runs {
		if run.Status != model.AgentRunFailed {
			t.Fatalf("run after failure = %#v", run)
		}
	}
}

func TestServiceRejectsCrossUserRecovery(t *testing.T) {
	chat := model.Session{ID: "chat-3", UserID: 9, UserName: "carol", Mode: model.SessionModeAgent}
	service, err := NewService(
		memorySessionRepository{session: chat}, &memoryLearningRepository{}, newMemoryRunRepository(),
		staticEvidenceProvider{}, staticQuestionGenerator{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Current(context.Background(), CurrentRequest{UserID: 99, SessionID: chat.ID}); !errors.Is(err, sessiondao.ErrSessionNotFound) {
		t.Fatalf("cross-user error = %v", err)
	}
}

func TestServiceBuildsAdaptivePlanFromHistoryAndReliableProgress(t *testing.T) {
	chat := model.Session{ID: "chat-adaptive", UserID: 17, UserName: "learner", Mode: model.SessionModeAgent}
	evidence := testEvidence()
	history := []model.LearningQuestion{{
		ID: "old-question", UserID: chat.UserID, Type: model.LearningQuestionShortAnswer,
		Question: "旧问题", AssessmentGoal: "旧目标", Evidence: evidence,
	}}
	repository := &memoryLearningRepository{history: history}
	evidenceProvider := &capturingEvidenceProvider{evidence: evidence}
	questionGenerator := &capturingQuestionGenerator{questions: learningQuestions(evidence)}
	service, err := NewService(memorySessionRepository{session: chat}, repository, newMemoryRunRepository(), evidenceProvider, questionGenerator)
	if err != nil {
		t.Fatal(err)
	}
	progress := &model.TopicProgress{
		UserID: chat.UserID, TopicKey: "redis", Topic: "Redis", ValidAttempts: 4,
		Level: model.TopicProgressDeveloping, WeakPoints: []string{"缓存击穿与穿透的区别"},
	}
	service.answers = &fakeAnswerRepository{completeResult: &learningdao.CompleteAnswerResult{Progress: progress}}
	if _, err := service.Start(context.Background(), StartRequest{
		UserID: chat.UserID, UserName: chat.UserName, SessionID: chat.ID, Topic: "Redis",
	}, agentservice.DiscardEvents); err != nil {
		t.Fatal(err)
	}
	if len(evidenceProvider.request.History) != 1 || len(questionGenerator.request.History) != 1 {
		t.Fatalf("history was not propagated: evidence=%#v generator=%#v", evidenceProvider.request, questionGenerator.request)
	}
	if len(evidenceProvider.request.WeakPoints) != 1 || questionGenerator.request.WeakPoints[0] != "缓存击穿与穿透的区别" {
		t.Fatalf("reliable weak points were not propagated: evidence=%#v generator=%#v", evidenceProvider.request, questionGenerator.request)
	}
}

func TestServiceDoesNotTrustWeakPointsBeforeThreeAttempts(t *testing.T) {
	service := &Service{answers: &fakeAnswerRepository{completeResult: &learningdao.CompleteAnswerResult{Progress: &model.TopicProgress{
		UserID: 18, TopicKey: "redis", ValidAttempts: 2,
		Level: model.TopicProgressInsufficient, WeakPoints: []string{"尚未可靠的薄弱点"},
	}}}}
	weakPoints, err := service.reliableWeakPoints(context.Background(), 18, "redis")
	if err != nil || len(weakPoints) != 0 {
		t.Fatalf("unreliable weak points = %#v, err = %v", weakPoints, err)
	}
}
