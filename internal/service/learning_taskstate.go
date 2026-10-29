package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/repository"
)

// This file restores the reference project's active-quiz task state: the
// structured server statement of what the learner is mid-way through, injected
// into every learning-agent turn that has one open.
//
// Without it an agent knows only what a prose mastery note says - which
// deliberately carries no numbers and no question text - so a learner who says
// "继续刚才那题" leaves the model without any structured basis for knowing
// which question that is. The task state is the answer to that, and it is
// server state end to end: the provider reads the quiz the store holds, and
// nothing the model or client says can widen it.

// ActiveLearningState is one open quiz as the task state renders it.
type ActiveLearningState struct {
	Topic           string
	Status          domain.LearningSessionStatus
	CurrentQuestion int
	TotalQuestions  int
	// QuestionText is the stem of the question the learner is on, present only
	// when the quiz is not terminal and has reached a question. It is the
	// server's own stored text, quoted so the agent can answer "which question"
	// without a tool round trip.
	QuestionText string
}

// ActiveLearningReader is the narrow read the task state needs: the quiz open
// on one conversation, and the stored stem of one of its questions.
type ActiveLearningReader interface {
	GetActiveLearningSession(ctx context.Context, userID, chatSessionID string) (*domain.LearningSession, error)
	ListLearningQuestions(ctx context.Context, userID, learningSessionID string) ([]domain.LearningQuestion, error)
}

// TaskStateRenderer turns one active learning state into the block the agent
// turn carries. It is a function rather than a private detail so the tests can
// assert the exact bytes the model receives. A terminal quiz renders no
// question line even if a stem was handed in: the reference project guards the
// line on the quiz not being finished and having reached a question, and the
// renderer holds that rule so a caller cannot drop it.
func renderTaskState(state *ActiveLearningState) string {
	if state == nil {
		return ""
	}
	var out strings.Builder
	fmt.Fprintf(&out, "Active learning task (structured server state; ordinary chat is not an answer):\n"+
		"- topic: %s\n- status: %s\n- progress: %d/%d",
		state.Topic, state.Status, state.CurrentQuestion, state.TotalQuestions)
	if !state.Status.IsTerminal() && state.CurrentQuestion > 0 && state.QuestionText != "" {
		fmt.Fprintf(&out, "\n- current question: %s", state.QuestionText)
	}
	return out.String()
}

// taskStateFor resolves the active-quiz block for one turn. It follows the
// reference project's failure semantics exactly: a session with no open quiz
// yields an empty block, while a store that cannot answer is a failure of the
// turn - the task state is required context, and degrading it to empty would
// let a quiz-carrying turn look like a quiz-less one.
func (s *MessageService) taskStateFor(ctx context.Context, userID, sessionID string, strategy domain.AnswerStrategy) (string, error) {
	if s.learningState == nil || strategy != domain.AnswerStrategyAgent {
		return "", nil
	}
	quiz, err := s.learningState.GetActiveLearningSession(ctx, userID, sessionID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return "", nil
		}
		return "", fmt.Errorf("load active learning state: %w", err)
	}
	state := &ActiveLearningState{
		Topic:           quiz.Topic,
		Status:          quiz.Status,
		CurrentQuestion: quiz.CurrentQuestion,
		TotalQuestions:  quiz.TotalQuestions,
	}
	if !quiz.Status.IsTerminal() && quiz.CurrentQuestion > 0 {
		questions, questionErr := s.learningState.ListLearningQuestions(ctx, userID, quiz.ID)
		if questionErr != nil {
			return "", fmt.Errorf("load active learning question: %w", questionErr)
		}
		if question, found := learningQuestionAt(questions, quiz.CurrentQuestion); found {
			state.QuestionText = question.Question
		}
	}
	return renderTaskState(state), nil
}
