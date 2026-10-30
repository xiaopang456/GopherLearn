package agent

import (
	agentdao "GopherAI/dao/agent"
	sessiondao "GopherAI/dao/session"
	"GopherAI/model"
	"context"
	"strings"
	"time"
)

// MaxRunsPerSessionRead bounds how many runs one session read returns. The
// repository keeps the same bound, so a caller cannot ask for more than the
// store is willing to read.
const MaxRunsPerSessionRead = agentdao.MaxRunsPerSessionRead

// The two states a persisted step can be reported in. A step that failed is
// reported as failed and nothing more: the error text stays in the database,
// because it can name an internal path or a provider's own wording, and the
// live progress stream never published it either.
const (
	RunStepStatusCompleted = "completed"
	RunStepStatusFailed    = "failed"
)

// RunReadRepository is the storage the run read path needs.
//
// ListSteps keeps the owner+session fence of one run; ListStepsByRuns reads
// several runs of one session at once; ListRunsBySession lists the runs
// themselves and says whether the list was cut.
type RunReadRepository interface {
	GetRun(ctx context.Context, userID int64, sessionID, runID string) (*model.AgentRun, error)
	ListSteps(ctx context.Context, userID int64, sessionID, runID string) ([]model.AgentStep, error)
	ListStepsByRuns(ctx context.Context, userID int64, runIDs []string) ([]model.AgentStep, error)
	ListRunsBySession(ctx context.Context, userID int64, sessionID string, limit int) ([]model.AgentRun, bool, error)
}

// RunStepView is the public shape of one persisted step.
//
// It carries what the live progress stream already published — the server's
// sequence, the step kind, the tool name, the duration — and one fact the
// stream did not have: whether the step failed. The argument summary, the
// result summary and the raw error are deliberately absent. They can carry
// the user's own question, retrieved passages and private memory, and the
// streaming wire refused to carry them for the same reason
// (hybrid/agent_sse.go: "arguments/results never reach this wire"). A reader
// who opens a conversation again sees the same steps the live run showed,
// not more.
type RunStepView struct {
	Sequence       int    `json:"sequence"`
	StepType       string `json:"step_type"`
	ToolName       string `json:"tool_name"`
	DurationMillis int64  `json:"duration_millis"`
	Status         string `json:"status"`
}

// RunView is the public shape of one run: its identity, how it ended, and the
// steps it took. The execution lease, the request key, the input hash and the
// stored result JSON are not here; they are the run's internals.
type RunView struct {
	RunID             string        `json:"run_id"`
	Status            string        `json:"status"`
	ExecutionKind     string        `json:"execution_kind"`
	StartedAt         time.Time     `json:"started_at"`
	FinishedAt        *time.Time    `json:"finished_at,omitempty"`
	TerminationReason string        `json:"termination_reason,omitempty"`
	Steps             []RunStepView `json:"steps"`
}

// SessionRunsView is the read of one session's runs.
//
// Truncated says that older runs were left out because the session holds
// more turns than the read returns. A reader who sees it knows the list is
// not the whole story, which is the difference between a bounded answer and
// a silently wrong one.
type SessionRunsView struct {
	Runs      []RunView `json:"runs"`
	Truncated bool      `json:"truncated"`
}

// RunReadService reads what a run did.
//
// It is separate from the Agent service on purpose: reading a past turn must
// not require a model, a runner or a configured Agent, because the moment a
// reader needs it most is after a restart.
type RunReadService struct {
	sessions SessionRepository
	runs     RunReadRepository
}

func NewRunReadService(sessions SessionRepository, runs RunReadRepository) *RunReadService {
	return &RunReadService{sessions: sessions, runs: runs}
}

// DefaultRunReadService builds the read service on the process-wide store.
func DefaultRunReadService() (*RunReadService, error) {
	repository := agentdao.DefaultRepository()
	if repository == nil {
		return nil, ErrServiceNotConfigured
	}
	return NewRunReadService(sessiondao.DefaultRepository(), repository), nil
}

// ListSessionRuns reads the runs of one session that belong to the caller.
//
// The session is checked first and a session that is not the caller's is
// refused exactly like a session that does not exist, so the answer never
// says which of the two it was. The runs come back oldest first, the order a
// transcript reads in.
func (s *RunReadService) ListSessionRuns(ctx context.Context, userID int64, sessionID string) (SessionRunsView, error) {
	if s == nil || s.sessions == nil || s.runs == nil {
		return SessionRunsView{}, ErrServiceNotConfigured
	}
	if userID <= 0 || strings.TrimSpace(sessionID) == "" {
		return SessionRunsView{}, sessiondao.ErrSessionNotFound
	}
	if _, err := s.sessions.GetByIDAndUserID(ctx, sessionID, userID); err != nil {
		return SessionRunsView{}, err
	}
	runs, truncated, err := s.runs.ListRunsBySession(ctx, userID, sessionID, MaxRunsPerSessionRead)
	if err != nil {
		return SessionRunsView{}, err
	}
	view := SessionRunsView{Runs: make([]RunView, 0, len(runs)), Truncated: truncated}
	if len(runs) == 0 {
		return view, nil
	}
	runIDs := make([]string, 0, len(runs))
	for _, run := range runs {
		runIDs = append(runIDs, run.ID)
	}
	steps, err := s.runs.ListStepsByRuns(ctx, userID, runIDs)
	if err != nil {
		return SessionRunsView{}, err
	}
	byRun := make(map[string][]model.AgentStep, len(runs))
	for _, step := range steps {
		byRun[step.RunID] = append(byRun[step.RunID], step)
	}
	// The repository answers newest first; a transcript reads oldest first.
	for index := len(runs) - 1; index >= 0; index-- {
		run := runs[index]
		view.Runs = append(view.Runs, RunView{
			RunID:             run.ID,
			Status:            string(run.Status),
			ExecutionKind:     string(run.ExecutionKind),
			StartedAt:         run.StartedAt,
			FinishedAt:        run.FinishedAt,
			TerminationReason: run.TerminationReason,
			Steps:             stepViews(byRun[run.ID]),
		})
	}
	return view, nil
}

// ListRunSteps reads the steps of one run of one session.
//
// The run is fetched through the same owner+session fence the message read
// uses, so a run of another account is absent rather than refused.
func (s *RunReadService) ListRunSteps(ctx context.Context, userID int64, sessionID, runID string) (RunView, error) {
	if s == nil || s.sessions == nil || s.runs == nil {
		return RunView{}, ErrServiceNotConfigured
	}
	if userID <= 0 || strings.TrimSpace(sessionID) == "" || strings.TrimSpace(runID) == "" {
		return RunView{}, agentdao.ErrRunNotFound
	}
	if _, err := s.sessions.GetByIDAndUserID(ctx, sessionID, userID); err != nil {
		return RunView{}, err
	}
	run, err := s.runs.GetRun(ctx, userID, sessionID, runID)
	if err != nil {
		return RunView{}, err
	}
	steps, err := s.runs.ListSteps(ctx, userID, sessionID, runID)
	if err != nil {
		return RunView{}, err
	}
	return RunView{
		RunID:             run.ID,
		Status:            string(run.Status),
		ExecutionKind:     string(run.ExecutionKind),
		StartedAt:         run.StartedAt,
		FinishedAt:        run.FinishedAt,
		TerminationReason: run.TerminationReason,
		Steps:             stepViews(steps),
	}, nil
}

// stepViews projects persisted steps onto the public shape.
//
// A step that carries an error is reported as failed. The error itself is
// not carried over: it is the tool's own wording about an internal failure,
// and the live stream did not publish it either.
func stepViews(steps []model.AgentStep) []RunStepView {
	views := make([]RunStepView, 0, len(steps))
	for _, step := range steps {
		status := RunStepStatusCompleted
		if strings.TrimSpace(step.ErrorMessage) != "" {
			status = RunStepStatusFailed
		}
		views = append(views, RunStepView{
			Sequence:       step.Sequence,
			StepType:       string(step.Type),
			ToolName:       step.ToolName,
			DurationMillis: step.DurationMillis,
			Status:         status,
		})
	}
	return views
}
