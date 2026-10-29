package learning

import (
	learningdao "GopherAI/dao/learning"
	"GopherAI/model"
	"context"
	"errors"
	"testing"
	"time"
)

type fakeAnswerRepository struct {
	claimResult    *learningdao.ClaimAnswerResult
	claimErr       error
	completeResult *learningdao.CompleteAnswerResult
	failCount      int
	completeCount  int
}

func (r *fakeAnswerRepository) ClaimAnswer(_ context.Context, input learningdao.ClaimAnswerInput) (*learningdao.ClaimAnswerResult, error) {
	if r.claimErr != nil {
		return nil, r.claimErr
	}
	copy := *r.claimResult
	if copy.Attempt != nil && copy.NeedsGrading {
		attempt := *copy.Attempt
		attempt.LeaseToken = input.Attempt.LeaseToken
		attempt.LeaseExpiresAt = &input.LeaseEnds
		copy.Attempt = &attempt
	}
	return &copy, nil
}
func (r *fakeAnswerRepository) FailAnswer(context.Context, int64, string, string, string, time.Time) (*model.LearningSession, error) {
	r.failCount++
	return r.claimResult.Session, nil
}
func (r *fakeAnswerRepository) CompleteAnswer(context.Context, int64, string, string, learningdao.GradeCommit, time.Time) (*learningdao.CompleteAnswerResult, error) {
	r.completeCount++
	return r.completeResult, nil
}
func (r *fakeAnswerRepository) SkipQuestion(context.Context, *model.LearningAttempt, time.Time) (*learningdao.CompleteAnswerResult, error) {
	return r.completeResult, nil
}
func (r *fakeAnswerRepository) GetTopicProgress(context.Context, int64, string) (*model.TopicProgress, error) {
	if r.completeResult != nil {
		return r.completeResult.Progress, nil
	}
	return nil, learningdao.ErrAttemptNotFound
}
func (r *fakeAnswerRepository) CorrectAttempt(context.Context, int64, string, int, string, time.Time) (*model.LearningAttempt, *model.TopicProgress, error) {
	return nil, nil, errors.New("unused")
}

type fakeAnswerGrader struct {
	result GradeResult
	err    error
	calls  int
}

func (g *fakeAnswerGrader) Grade(context.Context, GradeRequest) (GradeResult, error) {
	g.calls++
	return g.result, g.err
}

func answerServiceFixture(t *testing.T, grader *fakeAnswerGrader) (*Service, *fakeAnswerRepository, *memoryRunRepository, AnswerRequest) {
	t.Helper()
	chat := model.Session{ID: "chat-answer", UserID: 7, UserName: "alice", Mode: model.SessionModeAgent}
	learningSession := &model.LearningSession{ID: "learning-answer", UserID: chat.UserID, SessionID: chat.ID, AgentRunID: "run-answer", Topic: "RPC", TopicKey: "rpc", Status: model.LearningSessionGrading, CurrentQuestion: 1, TotalQuestions: 3, Version: 4, ExpiresAt: time.Now().Add(time.Hour)}
	question := &model.LearningQuestion{ID: "question-1", UserID: chat.UserID, LearningSessionID: learningSession.ID, Ordinal: 1, Type: model.LearningQuestionShortAnswer, Question: "Deadline 如何传播？", AssessmentGoal: "理解 Deadline 传播", ReferenceAnswer: "沿调用链传播", Rubric: []string{"说明调用链"}, Evidence: testEvidence()}
	attempt := &model.LearningAttempt{ID: "attempt-1", UserID: chat.UserID, SessionID: chat.ID, LearningSessionID: learningSession.ID, QuestionID: question.ID, QuestionOrdinal: 1, SubmissionID: "submission-1", TopicKey: "rpc", Status: model.LearningAttemptGrading, Answer: "通过 context 传播", Rubric: question.Rubric, Evidence: question.Evidence}
	nextSession := *learningSession
	nextSession.Status = model.LearningSessionWaitingAnswer
	nextSession.CurrentQuestion = 2
	nextSession.Version++
	nextQuestion := *question
	nextQuestion.ID = "question-2"
	nextQuestion.Ordinal = 2
	completedAttempt := *attempt
	completedAttempt.Status = model.LearningAttemptCompleted
	completedAttempt.AutoCorrectness = 50
	completedAttempt.AutoCompleteness = 20
	completedAttempt.AutoClarity = 10
	completedAttempt.AutoTotal = 80
	completedAttempt.EffectiveTotal = 80
	progress := &model.TopicProgress{UserID: chat.UserID, TopicKey: "rpc", Topic: "RPC", MasteryScore: 80, Confidence: 1.0 / 6, ValidAttempts: 1, Level: model.TopicProgressInsufficient}
	answers := &fakeAnswerRepository{claimResult: &learningdao.ClaimAnswerResult{Attempt: attempt, Session: learningSession, Question: question, NeedsGrading: true}, completeResult: &learningdao.CompleteAnswerResult{Attempt: &completedAttempt, Session: &nextSession, NextQuestion: &nextQuestion, Progress: progress}}
	learningRepo := &memoryLearningRepository{session: learningSession, questions: map[int]model.LearningQuestion{1: *question, 2: nextQuestion}}
	runs := newMemoryRunRepository()
	runs.runs["run-answer"] = model.AgentRun{ID: "run-answer", UserID: chat.UserID, SessionID: chat.ID, Status: model.AgentRunWaitingUser, Version: 2, StartedAt: time.Now()}
	service, err := NewService(memorySessionRepository{session: chat}, learningRepo, runs, staticEvidenceProvider{evidence: testEvidence()}, staticQuestionGenerator{questions: learningQuestions(testEvidence())})
	if err != nil {
		t.Fatal(err)
	}
	service.answers, service.grader = answers, grader
	request := AnswerRequest{UserID: chat.UserID, SessionID: chat.ID, LearningSessionID: learningSession.ID, QuestionID: question.ID, SubmissionID: attempt.SubmissionID, Answer: attempt.Answer}
	return service, answers, runs, request
}

func TestServiceAnswerFailureDoesNotAdvanceAndCanRetry(t *testing.T) {
	grader := &fakeAnswerGrader{err: errors.New("model timeout")}
	service, answers, runs, request := answerServiceFixture(t, grader)
	if _, err := service.Answer(context.Background(), request); !errors.Is(err, ErrLearningGradingFailed) {
		t.Fatalf("answer error = %v", err)
	}
	if answers.failCount != 1 || answers.completeCount != 0 || runs.runs["run-answer"].Status != model.AgentRunWaitingUser {
		t.Fatalf("fail=%d complete=%d run=%#v", answers.failCount, answers.completeCount, runs.runs["run-answer"])
	}
}

func TestServiceAnswerScoresOnceAndReplaySkipsModel(t *testing.T) {
	grader := &fakeAnswerGrader{result: GradeResult{Correctness: 50, Completeness: 20, Clarity: 10, Feedback: "good", Confidence: .8}}
	service, answers, _, request := answerServiceFixture(t, grader)
	view, err := service.Answer(context.Background(), request)
	if err != nil || view.Attempt.EffectiveTotal != 80 || view.Learning.CurrentQuestion != 2 || grader.calls != 1 || answers.completeCount != 1 {
		t.Fatalf("view=%#v calls=%d complete=%d err=%v", view, grader.calls, answers.completeCount, err)
	}
	answers.claimResult = &learningdao.ClaimAnswerResult{Attempt: answers.completeResult.Attempt, Session: answers.completeResult.Session, Replay: true, NeedsGrading: false}
	if _, err := service.Answer(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if grader.calls != 1 || answers.completeCount != 1 {
		t.Fatalf("replay called grader=%d complete=%d", grader.calls, answers.completeCount)
	}
}
