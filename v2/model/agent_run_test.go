package model

import (
	"testing"
	"time"
)

func TestAgentRunStatusTransitions(t *testing.T) {
	allowed := [][2]AgentRunStatus{
		{AgentRunRunning, AgentRunWaitingUser},
		{AgentRunRunning, AgentRunWaitingConfirmation},
		{AgentRunRunning, AgentRunCompleted},
		{AgentRunWaitingUser, AgentRunRunning},
		{AgentRunWaitingConfirmation, AgentRunFailed},
	}
	for _, transition := range allowed {
		if !transition[0].CanTransition(transition[1]) {
			t.Fatalf("expected transition %s -> %s to be allowed", transition[0], transition[1])
		}
	}

	rejected := [][2]AgentRunStatus{
		{AgentRunRunning, AgentRunRunning},
		{AgentRunCompleted, AgentRunRunning},
		{AgentRunFailed, AgentRunCompleted},
		{AgentRunWaitingUser, AgentRunWaitingConfirmation},
	}
	for _, transition := range rejected {
		if transition[0].CanTransition(transition[1]) {
			t.Fatalf("expected transition %s -> %s to be rejected", transition[0], transition[1])
		}
	}
}

func TestAgentRunValidateForCreate(t *testing.T) {
	run := AgentRun{
		ID:        "run-id",
		UserID:    1,
		SessionID: "session-id",
		Status:    AgentRunRunning,
		Version:   1,
		StartedAt: time.Now(),
	}
	if err := run.ValidateForCreate(); err != nil {
		t.Fatal(err)
	}

	run.Status = AgentRunCompleted
	if err := run.ValidateForCreate(); err == nil {
		t.Fatal("expected non-running initial state to be rejected")
	}
}

func TestAgentStepValidateForCreate(t *testing.T) {
	step := AgentStep{UserID: 1, RunID: "run-id", Sequence: 1, Type: AgentStepRetrieve}
	if err := step.ValidateForCreate(); err != nil {
		t.Fatal(err)
	}
	step.Sequence = 0
	if err := step.ValidateForCreate(); err == nil {
		t.Fatal("expected zero sequence to be rejected")
	}
}
