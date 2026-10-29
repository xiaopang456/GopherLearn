package agent

import (
	"GopherAI/common/rag"
	"GopherAI/model"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

type EventType string

const (
	EventRunStarted          EventType = "run_started"
	EventStepStarted         EventType = "step_started"
	EventToolStarted         EventType = "tool_started"
	EventToolResult          EventType = "tool_result"
	EventAnswerDelta         EventType = "answer_delta"
	EventCitation            EventType = "citation"
	EventWaitingUser         EventType = "waiting_user"
	EventWaitingConfirmation EventType = "waiting_confirmation"
	EventRunCompleted        EventType = "run_completed"
	EventRunFailed           EventType = "run_failed"
)

type Event struct {
	Type              EventType           `json:"type"`
	RunID             string              `json:"runId,omitempty"`
	LearningSessionID string              `json:"learningSessionId,omitempty"`
	QuestionID        string              `json:"questionId,omitempty"`
	Topic             string              `json:"topic,omitempty"`
	QuestionType      string              `json:"questionType,omitempty"`
	CurrentQuestion   int                 `json:"currentQuestion,omitempty"`
	TotalQuestions    int                 `json:"totalQuestions,omitempty"`
	Sequence          int                 `json:"sequence,omitempty"`
	StepType          string              `json:"stepType,omitempty"`
	ToolName          string              `json:"toolName,omitempty"`
	Content           string              `json:"content,omitempty"`
	Message           string              `json:"message,omitempty"`
	TerminationReason string              `json:"terminationReason,omitempty"`
	DurationMillis    int64               `json:"durationMillis,omitempty"`
	Source            *rag.Source         `json:"source,omitempty"`
	Sources           []rag.Source        `json:"sources,omitempty"`
	AnswerBasis       rag.AnswerBasis     `json:"answerBasis,omitempty"`
	KnowledgeStatus   rag.KnowledgeStatus `json:"knowledgeStatus,omitempty"`
	KnowledgeReason   rag.KnowledgeReason `json:"knowledgeReason,omitempty"`
	ContextBudget     *BudgetBreakdown    `json:"contextBudget,omitempty"`
	MemoryDraft       *MemoryDraftCard    `json:"memoryDraft,omitempty"`
	// UsedTopicKeys is the streaming half of the confirmation receipt. It
	// carries the same values the synchronous response returns, so a client
	// that streams and a client that does not can render the same line about
	// which of the learner's subjects this answer used.
	UsedTopicKeys []string `json:"usedTopicKeys,omitempty"`
}

type MemoryDraftCard struct {
	DraftID     string                 `json:"draftId"`
	Scope       model.MemoryScope      `json:"scope"`
	Subject     string                 `json:"subject"`
	Content     string                 `json:"content"`
	Tags        model.StringList       `json:"tags"`
	TopicKey    string                 `json:"topicKey,omitempty"`
	Durability  model.MemoryDurability `json:"durability"`
	ValidUntil  *time.Time             `json:"validUntil,omitempty"`
	ConflictIDs model.StringList       `json:"conflictIds"`
}

type EventSink interface {
	Send(context.Context, Event) error
}

type EventSinkFunc func(context.Context, Event) error

func (f EventSinkFunc) Send(ctx context.Context, event Event) error {
	return f(ctx, event)
}

var DiscardEvents EventSink = EventSinkFunc(func(context.Context, Event) error { return nil })

type SSEEventSink struct {
	writer http.ResponseWriter
	mu     sync.Mutex
}

func NewSSEEventSink(writer http.ResponseWriter) (*SSEEventSink, error) {
	if writer == nil {
		return nil, fmt.Errorf("SSE writer is nil")
	}
	if _, ok := writer.(http.Flusher); !ok {
		return nil, fmt.Errorf("SSE writer does not support flush")
	}
	return &SSEEventSink{writer: writer}, nil
}

func (s *SSEEventSink) Send(ctx context.Context, event Event) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal agent event: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := fmt.Fprintf(s.writer, "data: %s\n\n", payload); err != nil {
		return fmt.Errorf("write agent event: %w", err)
	}
	s.writer.(http.Flusher).Flush()
	return nil
}
