package agent

import (
	agentdao "GopherAI/dao/agent"
	sessiondao "GopherAI/dao/session"
	"GopherAI/model"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"
)

type fakeReadSessions struct {
	session *model.Session
	err     error
}

func (f *fakeReadSessions) GetByIDAndUserID(_ context.Context, sessionID string, userID int64) (*model.Session, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.session == nil || f.session.ID != sessionID || f.session.UserID != userID {
		return nil, sessiondao.ErrSessionNotFound
	}
	return f.session, nil
}

type fakeReadRuns struct {
	runs      []model.AgentRun
	steps     []model.AgentStep
	truncated bool
	err       error
	listed    int
}

func (f *fakeReadRuns) GetRun(_ context.Context, userID int64, sessionID, runID string) (*model.AgentRun, error) {
	for _, run := range f.runs {
		if run.ID == runID && run.UserID == userID && run.SessionID == sessionID {
			copied := run
			return &copied, nil
		}
	}
	return nil, agentdao.ErrRunNotFound
}

func (f *fakeReadRuns) ListSteps(_ context.Context, userID int64, sessionID, runID string) ([]model.AgentStep, error) {
	for _, run := range f.runs {
		if run.ID == runID && run.UserID == userID && run.SessionID == sessionID {
			return f.stepsOf(userID, runID), nil
		}
	}
	return nil, agentdao.ErrRunNotFound
}

func (f *fakeReadRuns) ListStepsByRuns(_ context.Context, userID int64, runIDs []string) ([]model.AgentStep, error) {
	wanted := make(map[string]bool, len(runIDs))
	for _, id := range runIDs {
		wanted[id] = true
	}
	out := make([]model.AgentStep, 0)
	for _, step := range f.stepsOf(userID, "") {
		if wanted[step.RunID] {
			out = append(out, step)
		}
	}
	return out, nil
}

func (f *fakeReadRuns) ListRunsBySession(_ context.Context, userID int64, sessionID string, limit int) ([]model.AgentRun, bool, error) {
	if f.err != nil {
		return nil, false, f.err
	}
	f.listed = limit
	owned := make([]model.AgentRun, 0, len(f.runs))
	for _, run := range f.runs {
		if run.UserID == userID && run.SessionID == sessionID {
			owned = append(owned, run)
		}
	}
	// The repository answers newest first.
	sort.Slice(owned, func(i, j int) bool { return owned[i].StartedAt.After(owned[j].StartedAt) })
	truncated := len(owned) > limit
	if truncated {
		owned = owned[:limit]
	}
	return owned, truncated, nil
}

func (f *fakeReadRuns) stepsOf(userID int64, runID string) []model.AgentStep {
	out := make([]model.AgentStep, 0, len(f.steps))
	for _, step := range f.steps {
		if step.UserID != userID {
			continue
		}
		if runID != "" && step.RunID != runID {
			continue
		}
		out = append(out, step)
	}
	return out
}

func readServiceFor(sessions *fakeReadSessions, runs *fakeReadRuns) *RunReadService {
	return NewRunReadService(sessions, runs)
}

func TestListSessionRunsRefusesASessionThatIsNotTheCallers(t *testing.T) {
	sessions := &fakeReadSessions{session: &model.Session{ID: "s-1", UserID: 7}}
	runs := &fakeReadRuns{runs: []model.AgentRun{{ID: "r-1", UserID: 7, SessionID: "s-1"}}}
	service := readServiceFor(sessions, runs)

	if _, err := service.ListSessionRuns(context.Background(), 8, "s-1"); !errors.Is(err, sessiondao.ErrSessionNotFound) {
		t.Fatalf("a foreign session was not refused as not found: %v", err)
	}
	if runs.listed != 0 {
		t.Fatal("the runs of a session that is not the caller's were read")
	}
}

func TestListSessionRunsAnswersAnEmptyStepListWithAnEmptyArray(t *testing.T) {
	sessions := &fakeReadSessions{session: &model.Session{ID: "s-1", UserID: 7}}
	runs := &fakeReadRuns{runs: []model.AgentRun{{
		ID: "r-1", UserID: 7, SessionID: "s-1", Status: model.AgentRunCompleted,
		ExecutionKind: model.AgentRunExecutionChat, StartedAt: time.Now(),
	}}}
	service := readServiceFor(sessions, runs)

	view, err := service.ListSessionRuns(context.Background(), 7, "s-1")
	if err != nil {
		t.Fatalf("a run without steps was refused: %v", err)
	}
	if len(view.Runs) != 1 {
		t.Fatalf("runs %v", view.Runs)
	}
	if view.Runs[0].Steps == nil || len(view.Runs[0].Steps) != 0 {
		t.Fatalf("a run without steps answered %v", view.Runs[0].Steps)
	}
	body, err := json.Marshal(view.Runs[0].Steps)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "[]" {
		t.Fatalf("an empty step list answered %s, not []", body)
	}
}

func TestListSessionRunsProjectsOnlyThePublicStepFields(t *testing.T) {
	sessions := &fakeReadSessions{session: &model.Session{ID: "s-1", UserID: 7}}
	runs := &fakeReadRuns{
		runs: []model.AgentRun{{
			ID: "r-1", UserID: 7, SessionID: "s-1", Status: model.AgentRunCompleted,
			ExecutionKind: model.AgentRunExecutionChat, StartedAt: time.Now(),
			TerminationReason: "answered",
		}},
		steps: []model.AgentStep{
			{
				UserID: 7, RunID: "r-1", Sequence: 1, Type: model.AgentStepPlan,
				ArgumentsSummary: "模型生成 1", LeaseToken: "lease-secret-plan",
			},
			{
				UserID: 7, RunID: "r-1", Sequence: 2, Type: model.AgentStepTool,
				ToolName: "knowledge_search", DurationMillis: 412,
				ArgumentsSummary: `{"query":"用户的问题"}`,
				ResultSummary:    "检索到的私人笔记原文",
				SourceIDs:        []string{"chunk-7"},
			},
			{
				UserID: 7, RunID: "r-1", Sequence: 3, Type: model.AgentStepTool,
				ToolName: "document_read", ErrorMessage: "open /var/private/notes.md: no such file",
			},
		},
	}
	service := readServiceFor(sessions, runs)

	view, err := service.ListSessionRuns(context.Background(), 7, "s-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Runs) != 1 || len(view.Runs[0].Steps) != 3 {
		t.Fatalf("steps %v", view.Runs)
	}
	steps := view.Runs[0].Steps
	if steps[0].Sequence != 1 || steps[0].StepType != "plan" || steps[0].Status != RunStepStatusCompleted {
		t.Fatalf("the first step is wrong: %+v", steps[0])
	}
	if steps[1].ToolName != "knowledge_search" || steps[1].DurationMillis != 412 || steps[1].Status != RunStepStatusCompleted {
		t.Fatalf("the second step is wrong: %+v", steps[1])
	}
	if steps[2].Status != RunStepStatusFailed {
		t.Fatalf("a failed step is not reported as failed: %+v", steps[2])
	}

	body, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, secret := range []string{
		"lease-secret-plan", "用户的问题", "检索到的私人笔记原文", "chunk-7",
		"no such file", "/var/private/notes.md",
		"arguments_summary", "result_summary", "source_ids", "error_message", "lease_token",
	} {
		if strings.Contains(text, secret) {
			t.Fatalf("the answer carried %q: %s", secret, text)
		}
	}
}

func TestListSessionRunsKeepsTheNewestRunsAndSaysTheListWasCut(t *testing.T) {
	sessions := &fakeReadSessions{session: &model.Session{ID: "s-1", UserID: 7}}
	base := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	stored := make([]model.AgentRun, 0, MaxRunsPerSessionRead+1)
	for index := 0; index < MaxRunsPerSessionRead+1; index++ {
		stored = append(stored, model.AgentRun{
			ID: fmt.Sprintf("r-%03d", index), UserID: 7, SessionID: "s-1",
			Status: model.AgentRunCompleted, ExecutionKind: model.AgentRunExecutionChat,
			StartedAt: base.Add(time.Duration(index) * time.Minute),
		})
	}
	runs := &fakeReadRuns{runs: stored}
	service := readServiceFor(sessions, runs)

	view, err := service.ListSessionRuns(context.Background(), 7, "s-1")
	if err != nil {
		t.Fatal(err)
	}
	if !view.Truncated {
		t.Fatal("a list that was cut did not say so")
	}
	if len(view.Runs) != MaxRunsPerSessionRead {
		t.Fatalf("a cut list answered %d runs", len(view.Runs))
	}
	// The oldest run is the one that did not fit, and the newest is the one a
	// reader is looking at.
	if view.Runs[0].RunID != "r-001" || view.Runs[len(view.Runs)-1].RunID != "r-100" {
		t.Fatalf("the wrong runs were kept: %s .. %s", view.Runs[0].RunID, view.Runs[len(view.Runs)-1].RunID)
	}
	// The answer reads oldest first, the order a transcript reads in.
	for index := 1; index < len(view.Runs); index++ {
		if view.Runs[index-1].StartedAt.After(view.Runs[index].StartedAt) {
			t.Fatalf("the runs are not in reading order at %d", index)
		}
	}
}

func TestListSessionRunsKeepsTheWholeListWhenItFits(t *testing.T) {
	sessions := &fakeReadSessions{session: &model.Session{ID: "s-1", UserID: 7}}
	base := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	stored := make([]model.AgentRun, 0, 3)
	for index := 0; index < 3; index++ {
		stored = append(stored, model.AgentRun{
			ID: fmt.Sprintf("r-%d", index), UserID: 7, SessionID: "s-1",
			Status: model.AgentRunCompleted, ExecutionKind: model.AgentRunExecutionChat,
			StartedAt: base.Add(time.Duration(index) * time.Minute),
		})
	}
	service := readServiceFor(sessions, &fakeReadRuns{runs: stored})

	view, err := service.ListSessionRuns(context.Background(), 7, "s-1")
	if err != nil {
		t.Fatal(err)
	}
	if view.Truncated {
		t.Fatal("a list that fits was marked as cut")
	}
	if len(view.Runs) != 3 || view.Runs[0].RunID != "r-0" || view.Runs[2].RunID != "r-2" {
		t.Fatalf("runs %v", view.Runs)
	}
}

func TestListRunStepsRefusesARunOfAnotherAccount(t *testing.T) {
	sessions := &fakeReadSessions{session: &model.Session{ID: "s-1", UserID: 7}}
	runs := &fakeReadRuns{
		// The run row claims another account while the session is the
		// caller's. The state cannot arise from the service, and that is the
		// point: the run's own fence has to refuse it on its own, not because
		// the session happened to be checked first.
		runs: []model.AgentRun{{ID: "r-1", UserID: 8, SessionID: "s-1"}},
		steps: []model.AgentStep{{
			UserID: 8, RunID: "r-1", Sequence: 1, Type: model.AgentStepTool,
			ToolName: "knowledge_search",
		}},
	}
	service := readServiceFor(sessions, runs)

	if _, err := service.ListRunSteps(context.Background(), 7, "s-1", "r-1"); !errors.Is(err, agentdao.ErrRunNotFound) {
		t.Fatalf("a run of another account was not refused as not found: %v", err)
	}
	if _, err := service.ListRunSteps(context.Background(), 7, "s-1", "r-2"); !errors.Is(err, agentdao.ErrRunNotFound) {
		t.Fatalf("a run that never existed was not refused as not found: %v", err)
	}
}

func TestListRunStepsRefusesARunOfAnotherAccountButReadsTheCallersOwn(t *testing.T) {
	sessions := &fakeReadSessions{session: &model.Session{ID: "s-1", UserID: 7}}
	runs := &fakeReadRuns{
		runs: []model.AgentRun{{ID: "r-1", UserID: 7, SessionID: "s-1"}},
		steps: []model.AgentStep{{
			UserID: 7, RunID: "r-1", Sequence: 1, Type: model.AgentStepTool,
			ToolName: "knowledge_search",
		}},
	}
	service := readServiceFor(sessions, runs)

	view, err := service.ListRunSteps(context.Background(), 7, "s-1", "r-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Steps) != 1 || view.Steps[0].ToolName != "knowledge_search" {
		t.Fatalf("steps %v", view.Steps)
	}
}

func TestListRunStepsRefusesARunOfAnotherSession(t *testing.T) {
	sessions := &fakeReadSessions{session: &model.Session{ID: "s-1", UserID: 7}}
	runs := &fakeReadRuns{runs: []model.AgentRun{{ID: "r-1", UserID: 7, SessionID: "s-2"}}}
	service := readServiceFor(sessions, runs)

	if _, err := service.ListRunSteps(context.Background(), 7, "s-1", "r-1"); !errors.Is(err, agentdao.ErrRunNotFound) {
		t.Fatalf("a run of another session was not refused as not found: %v", err)
	}
}
