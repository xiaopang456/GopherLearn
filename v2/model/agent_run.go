package model

import (
	"fmt"
	"strings"
	"time"
)

type AgentRunStatus string

type AgentRunExecutionKind string

const (
	AgentRunExecutionChat     AgentRunExecutionKind = "agent_chat"
	AgentRunExecutionLearning AgentRunExecutionKind = "learning"
)

func (k AgentRunExecutionKind) Valid() bool {
	return k == AgentRunExecutionChat || k == AgentRunExecutionLearning
}

const (
	AgentRunRunning             AgentRunStatus = "running"
	AgentRunWaitingUser         AgentRunStatus = "waiting_user"
	AgentRunWaitingConfirmation AgentRunStatus = "waiting_confirmation"
	AgentRunCompleted           AgentRunStatus = "completed"
	AgentRunFailed              AgentRunStatus = "failed"
	AgentRunCancelled           AgentRunStatus = "cancelled"
	AgentRunTimeout             AgentRunStatus = "timeout"
)

func (s AgentRunStatus) Valid() bool {
	switch s {
	case AgentRunRunning, AgentRunWaitingUser, AgentRunWaitingConfirmation,
		AgentRunCompleted, AgentRunFailed, AgentRunCancelled, AgentRunTimeout:
		return true
	default:
		return false
	}
}

func (s AgentRunStatus) Terminal() bool {
	switch s {
	case AgentRunCompleted, AgentRunFailed, AgentRunCancelled, AgentRunTimeout:
		return true
	default:
		return false
	}
}

func (s AgentRunStatus) CanTransition(to AgentRunStatus) bool {
	if !s.Valid() || !to.Valid() || s.Terminal() || s == to {
		return false
	}
	switch s {
	case AgentRunRunning:
		return to == AgentRunWaitingUser || to == AgentRunWaitingConfirmation || to.Terminal()
	case AgentRunWaitingUser, AgentRunWaitingConfirmation:
		return to == AgentRunRunning || to.Terminal()
	default:
		return false
	}
}

type AgentRun struct {
	ID                string                `gorm:"primaryKey;type:varchar(36)" json:"id"`
	UserID            int64                 `gorm:"not null;index:idx_agent_runs_user_session,priority:1;uniqueIndex:idx_agent_runs_user_request,priority:1" json:"user_id"`
	SessionID         string                `gorm:"type:varchar(36);not null;index:idx_agent_runs_user_session,priority:2" json:"session_id"`
	RequestKey        *string               `gorm:"type:varchar(64);uniqueIndex:idx_agent_runs_user_request,priority:2" json:"-"`
	InputHash         string                `gorm:"type:char(64)" json:"-"`
	ResultJSON        string                `gorm:"type:longtext" json:"-"`
	ExecutionKind     AgentRunExecutionKind `gorm:"type:varchar(24);not null;default:agent_chat;index" json:"execution_kind"`
	Status            AgentRunStatus        `gorm:"type:varchar(32);not null;index" json:"status"`
	InputSummary      string                `gorm:"type:text" json:"input_summary"`
	AnswerSummary     string                `gorm:"type:text" json:"answer_summary"`
	TerminationReason string                `gorm:"type:varchar(64)" json:"termination_reason"`
	Version           uint64                `gorm:"not null;default:1" json:"version"`
	LeaseToken        string                `gorm:"type:char(36);index" json:"-"`
	LeaseExpiresAt    *time.Time            `gorm:"index" json:"-"`
	HeartbeatAt       *time.Time            `json:"-"`
	StartedAt         time.Time             `gorm:"not null" json:"started_at"`
	FinishedAt        *time.Time            `json:"finished_at,omitempty"`
	CreatedAt         time.Time             `json:"created_at"`
	UpdatedAt         time.Time             `json:"updated_at"`
}

func ValidRequestKey(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) < 8 || len(value) > 64 {
		return false
	}
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' ||
			char >= '0' && char <= '9' || strings.ContainsRune("-_.:", char) {
			continue
		}
		return false
	}
	return true
}

func (r *AgentRun) ValidateForCreate() error {
	if r == nil {
		return fmt.Errorf("agent run is nil")
	}
	if strings.TrimSpace(r.ID) == "" {
		return fmt.Errorf("agent run id is required")
	}
	if r.UserID <= 0 {
		return fmt.Errorf("agent run user id must be positive")
	}
	if strings.TrimSpace(r.SessionID) == "" {
		return fmt.Errorf("agent run session id is required")
	}
	if r.Status != AgentRunRunning {
		return fmt.Errorf("new agent run must start in %q status", AgentRunRunning)
	}
	if r.ExecutionKind == "" {
		r.ExecutionKind = AgentRunExecutionChat
	}
	if !r.ExecutionKind.Valid() {
		return fmt.Errorf("invalid agent run execution kind %q", r.ExecutionKind)
	}
	if r.Version != 1 {
		return fmt.Errorf("new agent run must start at version 1")
	}
	if r.StartedAt.IsZero() {
		return fmt.Errorf("agent run start time is required")
	}
	if r.RequestKey != nil {
		if !ValidRequestKey(*r.RequestKey) {
			return fmt.Errorf("invalid agent run request key")
		}
		if len(r.InputHash) != 64 {
			return fmt.Errorf("agent run input hash is required")
		}
	}
	return nil
}

type AgentStepType string

const (
	AgentStepPlan         AgentStepType = "plan"
	AgentStepRetrieve     AgentStepType = "retrieve"
	AgentStepTool         AgentStepType = "tool"
	AgentStepAskUser      AgentStepType = "ask_user"
	AgentStepConfirmation AgentStepType = "confirmation"
	AgentStepAnswer       AgentStepType = "answer"
)

func (s AgentStepType) Valid() bool {
	switch s {
	case AgentStepPlan, AgentStepRetrieve, AgentStepTool, AgentStepAskUser,
		AgentStepConfirmation, AgentStepAnswer:
		return true
	default:
		return false
	}
}

type AgentStep struct {
	ID               uint64        `gorm:"primaryKey;autoIncrement" json:"id"`
	UserID           int64         `gorm:"not null;index" json:"user_id"`
	RunID            string        `gorm:"type:varchar(36);not null;uniqueIndex:idx_agent_steps_run_sequence,priority:1" json:"run_id"`
	Sequence         int           `gorm:"not null;uniqueIndex:idx_agent_steps_run_sequence,priority:2" json:"sequence"`
	Type             AgentStepType `gorm:"type:varchar(32);not null" json:"type"`
	ToolName         string        `gorm:"type:varchar(100)" json:"tool_name"`
	ArgumentsSummary string        `gorm:"type:text" json:"arguments_summary"`
	ResultSummary    string        `gorm:"type:text" json:"result_summary"`
	SourceIDs        []string      `gorm:"serializer:json;type:json" json:"source_ids"`
	DurationMillis   int64         `gorm:"not null;default:0" json:"duration_millis"`
	ErrorMessage     string        `gorm:"type:varchar(512)" json:"error_message"`
	LeaseToken       string        `gorm:"type:char(36)" json:"-"`
	CreatedAt        time.Time     `json:"created_at"`
}

func (s *AgentStep) ValidateForCreate() error {
	if s == nil {
		return fmt.Errorf("agent step is nil")
	}
	if s.UserID <= 0 {
		return fmt.Errorf("agent step user id must be positive")
	}
	if strings.TrimSpace(s.RunID) == "" {
		return fmt.Errorf("agent step run id is required")
	}
	if s.Sequence <= 0 {
		return fmt.Errorf("agent step sequence must be positive")
	}
	if !s.Type.Valid() {
		return fmt.Errorf("invalid agent step type %q", s.Type)
	}
	if s.DurationMillis < 0 {
		return fmt.Errorf("agent step duration must not be negative")
	}
	return nil
}
