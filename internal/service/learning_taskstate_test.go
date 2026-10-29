package service

import (
	"context"
	"strings"
	"testing"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/repository"
)

// The task state's rendering is the model-facing contract: the same block the
// reference project sent, line for line. A reworded block here would change
// what every quiz-carrying turn says without anything else moving.
func TestRenderTaskStateBytes(t *testing.T) {
	state := &ActiveLearningState{
		Topic: "TCP 粘包", Status: domain.LearningSessionWaitingAnswer,
		CurrentQuestion: 2, TotalQuestions: 5, QuestionText: "什么是粘包？",
	}
	want := "Active learning task (structured server state; ordinary chat is not an answer):\n" +
		"- topic: TCP 粘包\n- status: waiting_answer\n- progress: 2/5\n" +
		"- current question: 什么是粘包？"
	if got := renderTaskState(state); got != want {
		t.Fatalf("rendered:\n%s\nwant:\n%s", got, want)
	}
}

// A quiz that has not reached a question yet, or has finished, carries no
// question line: there is nothing to quote.
func TestRenderTaskStateOmitsTheQuestionWhenThereIsNone(t *testing.T) {
	asking := &ActiveLearningState{Topic: "etcd", Status: domain.LearningSessionAsking,
		CurrentQuestion: 0, TotalQuestions: 5}
	if got := renderTaskState(asking); strings.Contains(got, "current question") {
		t.Fatalf("a quiz with no current question rendered one:\n%s", got)
	}
	done := &ActiveLearningState{Topic: "etcd", Status: domain.LearningSessionCompleted,
		CurrentQuestion: 5, TotalQuestions: 5, QuestionText: "残留题干"}
	if got := renderTaskState(done); strings.Contains(got, "current question") {
		t.Fatalf("a terminal quiz rendered a question line:\n%s", got)
	}
	if got := renderTaskState(nil); got != "" {
		t.Fatalf("no state rendered %q", got)
	}
}

// taskStateFor resolves against the store the same way the quiz view does: the
// open quiz, its current question's stem, and an empty block when nothing is
// open.
func TestTaskStateForReadsTheActiveQuiz(t *testing.T) {
	service := &MessageService{learningState: &fakeActiveLearningReader{
		quiz: &domain.LearningSession{ID: "q-1", SessionID: "s-1", Topic: "RAG 检索",
			Status: domain.LearningSessionWaitingAnswer, CurrentQuestion: 1, TotalQuestions: 3},
		questions: []domain.LearningQuestion{{Ordinal: 1, Question: "向量检索和全文检索的区别？"}},
	}}
	state, err := service.taskStateFor(context.Background(), "u-1", "s-1", domain.AnswerStrategyAgent)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(state, "- topic: RAG 检索") ||
		!strings.Contains(state, "- progress: 1/3") ||
		!strings.Contains(state, "- current question: 向量检索和全文检索的区别？") {
		t.Fatalf("the block lost a field:\n%s", state)
	}
}

// Only an agent turn carries the block, and a session with no open quiz
// answers empty rather than failing.
func TestTaskStateForScopeAndAbsence(t *testing.T) {
	reader := &fakeActiveLearningReader{}
	service := &MessageService{learningState: reader}
	state, err := service.taskStateFor(context.Background(), "u-1", "s-1", domain.AnswerStrategyDirectChat)
	if err != nil || state != "" {
		t.Fatalf("a direct turn read the quiz: %q, %v", state, err)
	}
	if reader.calls != 0 {
		t.Fatalf("a direct turn read the store %d times", reader.calls)
	}
	reader.notFound = true
	state, err = service.taskStateFor(context.Background(), "u-1", "s-1", domain.AnswerStrategyAgent)
	if err != nil {
		t.Fatalf("no open quiz is an empty block, not an error: %v", err)
	}
	if state != "" {
		t.Fatalf("no open quiz rendered %q", state)
	}
}

// A store that cannot answer fails the turn: the task state is required
// context, and an empty block would make a quiz-carrying session look like a
// quiz-less one.
func TestTaskStateForFailsClosedOnStoreErrors(t *testing.T) {
	service := &MessageService{learningState: &fakeActiveLearningReader{
		boom: true,
	}}
	if _, err := service.taskStateFor(context.Background(), "u-1", "s-1", domain.AnswerStrategyAgent); err == nil {
		t.Fatal("a broken store answered with an empty block")
	}
}

type fakeActiveLearningReader struct {
	quiz      *domain.LearningSession
	questions []domain.LearningQuestion
	notFound  bool
	boom      bool
	calls     int
}

func (r *fakeActiveLearningReader) GetActiveLearningSession(ctx context.Context, userID, chatSessionID string) (*domain.LearningSession, error) {
	r.calls++
	if r.boom {
		return nil, ErrInternal
	}
	if r.notFound {
		// The store layer answers repository.ErrNotFound directly; the service's
		// own mapping happens above it.
		return nil, repository.ErrNotFound
	}
	return r.quiz, nil
}

func (r *fakeActiveLearningReader) ListLearningQuestions(ctx context.Context, userID, learningSessionID string) ([]domain.LearningQuestion, error) {
	if r.boom {
		return nil, ErrInternal
	}
	return r.questions, nil
}
