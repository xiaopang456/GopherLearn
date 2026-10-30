package hybrid

import (
	"GopherAI/model"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// agentTurn runs one Agent turn in an Agent session and returns the session
// id and the run id the turn produced.
func agentTurn(t *testing.T, app *Application, token, requestKey string) (string, string) {
	t.Helper()
	session := dataHTTP(t, app.Handler, "POST", "/api/v1/sessions",
		fmt.Sprintf(`{"mode":"learning_agent","title":"学习 Agent","requestId":%q}`, requestKey+"-session"), token)["id"].(string)
	body, _ := json.Marshal(map[string]any{"content": "查一下资料", "requestId": requestKey + "-turn"})
	status, envelope, raw := requestHTTP(t, app.Handler, "POST", "/api/v1/sessions/"+session+"/messages", string(body), token)
	if status != 200 || envelope["error"] != nil {
		t.Fatalf("agent turn %d %s", status, raw)
	}
	data, ok := envelope["data"].(map[string]any)
	if !ok {
		t.Fatalf("agent turn data %v", envelope)
	}
	runID, _ := data["runId"].(string)
	if runID == "" {
		t.Fatalf("the turn produced no run id: %v", data)
	}
	return session, runID
}

// runRead reads the runs of one session through the native V2 surface.
func runRead(t *testing.T, app *Application, token, session string) (map[string]any, string) {
	t.Helper()
	status, envelope, raw := requestHTTP(t, app.Handler, "GET", "/api/v1/AI/agent/sessions/"+session+"/runs", "", token)
	if status != 200 {
		t.Fatalf("run read %d %s", status, raw)
	}
	return envelope, raw
}

func TestAgentRunReadAnswersTheStepsOfTheCallersOwnTurn(t *testing.T) {
	app, _, _, _, token, _ := httpFixture(t)
	session, runID := agentTurn(t, app, token, "steps-own")

	envelope, raw := runRead(t, app, token, session)
	if envelope["status_code"] != float64(1000) {
		t.Fatalf("run read %s", raw)
	}
	runs, ok := envelope["runs"].([]any)
	if !ok || len(runs) != 1 {
		t.Fatalf("runs %s", raw)
	}
	run := runs[0].(map[string]any)
	if run["run_id"] != runID {
		t.Fatalf("the read named another run: %s", raw)
	}
	if run["status"] != "completed" || run["execution_kind"] != "agent_chat" {
		t.Fatalf("the run facts are wrong: %s", raw)
	}
	steps, ok := run["steps"].([]any)
	if !ok || len(steps) == 0 {
		t.Fatalf("the turn took no steps: %s", raw)
	}
	first := steps[0].(map[string]any)
	if first["sequence"] != float64(1) || first["step_type"] == "" {
		t.Fatalf("the first step is wrong: %s", raw)
	}
	if _, present := first["lease_token"]; present {
		t.Fatalf("a step published its lease token: %s", raw)
	}
}

func TestAgentRunReadRefusesAnotherAccountsSessionAndRun(t *testing.T) {
	app, _, _, _, token, other := httpFixture(t)
	session, runID := agentTurn(t, app, token, "steps-foreign")

	// Another account's session and another account's run answer exactly the
	// same way, and the same way a session that never existed answers: the
	// answer never says which of the three it was.
	for _, path := range []string{
		"/api/v1/AI/agent/sessions/" + session + "/runs",
		"/api/v1/AI/agent/sessions/" + session + "/runs/" + runID + "/steps",
		"/api/v1/AI/agent/sessions/session-that-never-existed/runs",
		"/api/v1/AI/agent/sessions/" + session + "/runs/run-that-never-existed/steps",
	} {
		status, envelope, raw := requestHTTP(t, app.Handler, "GET", path, "", other)
		if status != 200 {
			t.Fatalf("%s answered %d", path, status)
		}
		if envelope["status_code"] != float64(2009) {
			t.Fatalf("%s answered %s, want the not-found code", path, raw)
		}
		if strings.Contains(raw, runID) {
			t.Fatalf("%s leaked the run id it may not read: %s", path, raw)
		}
		if strings.Contains(raw, "steps") {
			t.Fatalf("%s published steps: %s", path, raw)
		}
	}
}

func TestAgentRunReadAnswersARunWithoutStepsWithAnEmptyArray(t *testing.T) {
	app, db, _, _, token, _ := httpFixture(t)
	session := dataHTTP(t, app.Handler, "POST", "/api/v1/sessions",
		`{"mode":"learning_agent","title":"空步骤","requestId":"empty-steps-session"}`, token)["id"].(string)
	if err := db.Create(&model.AgentRun{
		ID: "run-without-steps", UserID: 1, SessionID: session,
		ExecutionKind: model.AgentRunExecutionChat, Status: model.AgentRunCompleted,
		Version: 1, StartedAt: time.Now(),
	}).Error; err != nil {
		t.Fatal(err)
	}

	envelope, raw := runRead(t, app, token, session)
	if envelope["status_code"] != float64(1000) {
		t.Fatalf("run read %s", raw)
	}
	runs := envelope["runs"].([]any)
	if len(runs) != 1 {
		t.Fatalf("runs %s", raw)
	}
	steps, ok := runs[0].(map[string]any)["steps"].([]any)
	if !ok || len(steps) != 0 {
		t.Fatalf("a run without steps answered %s", raw)
	}
	if !strings.Contains(raw, `"steps":[]`) {
		t.Fatalf("an empty step list was not written as an empty array: %s", raw)
	}
}

func TestAgentRunReadNeverPublishesTheLeaseOrTheToolArgumentsAndResults(t *testing.T) {
	app, db, _, _, token, _ := httpFixture(t)
	session := dataHTTP(t, app.Handler, "POST", "/api/v1/sessions",
		`{"mode":"learning_agent","title":"不外泄","requestId":"secret-steps-session"}`, token)["id"].(string)
	if err := db.Create(&model.AgentRun{
		ID: "run-with-secrets", UserID: 1, SessionID: session,
		ExecutionKind: model.AgentRunExecutionChat, Status: model.AgentRunCompleted,
		Version: 1, LeaseToken: "lease-secret-token", StartedAt: time.Now(),
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&[]model.AgentStep{
		{
			UserID: 1, RunID: "run-with-secrets", Sequence: 1, Type: model.AgentStepPlan,
			ArgumentsSummary: "模型生成 1", LeaseToken: "lease-secret-token",
		},
		{
			UserID: 1, RunID: "run-with-secrets", Sequence: 2, Type: model.AgentStepTool,
			ToolName: "knowledge_search", DurationMillis: 412,
			ArgumentsSummary: `{"query":"用户的私人问题"}`,
			ResultSummary:    "检索到的私人笔记原文",
			SourceIDs:        []string{"chunk-7"},
		},
		{
			UserID: 1, RunID: "run-with-secrets", Sequence: 3, Type: model.AgentStepTool,
			ToolName: "document_read", ErrorMessage: "open /var/private/notes.md: no such file",
		},
	}).Error; err != nil {
		t.Fatal(err)
	}

	_, raw := runRead(t, app, token, session)
	for _, secret := range []string{
		"lease-secret-token", "用户的私人问题", "检索到的私人笔记原文", "chunk-7",
		"no such file", "/var/private/notes.md",
		"arguments_summary", "result_summary", "source_ids", "error_message", "lease_token",
	} {
		if strings.Contains(raw, secret) {
			t.Fatalf("the answer carried %q: %s", secret, raw)
		}
	}
	// The public facts are still there: the read is bounded, not empty.
	if !strings.Contains(raw, "knowledge_search") || !strings.Contains(raw, `"duration_millis":412`) {
		t.Fatalf("the answer lost the public step facts: %s", raw)
	}
	if !strings.Contains(raw, `"status":"failed"`) {
		t.Fatalf("a failed step was not reported as failed: %s", raw)
	}
}

func TestAgentRunReadKeepsTheNewestRunsAndSaysTheListWasCut(t *testing.T) {
	app, db, _, _, token, _ := httpFixture(t)
	session := dataHTTP(t, app.Handler, "POST", "/api/v1/sessions",
		`{"mode":"learning_agent","title":"截断","requestId":"truncated-session"}`, token)["id"].(string)
	base := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	runs := make([]model.AgentRun, 0, 101)
	for index := 0; index < 101; index++ {
		runs = append(runs, model.AgentRun{
			ID: fmt.Sprintf("run-%03d", index), UserID: 1, SessionID: session,
			ExecutionKind: model.AgentRunExecutionChat, Status: model.AgentRunCompleted,
			Version: 1, StartedAt: base.Add(time.Duration(index) * time.Minute),
		})
	}
	if err := db.Create(&runs).Error; err != nil {
		t.Fatal(err)
	}

	envelope, raw := runRead(t, app, token, session)
	if envelope["status_code"] != float64(1000) {
		t.Fatalf("run read %s", raw)
	}
	if envelope["truncated"] != true {
		t.Fatalf("a list that was cut did not say so: %s", raw)
	}
	items := envelope["runs"].([]any)
	if len(items) != 100 {
		t.Fatalf("a cut list answered %d runs", len(items))
	}
	// The oldest run is the one that did not fit; the newest is the one a
	// reader is looking at.
	if strings.Contains(raw, `"run-000"`) {
		t.Fatalf("the oldest run was kept: %s", raw)
	}
	if !strings.Contains(raw, `"run-100"`) {
		t.Fatalf("the newest run was dropped: %s", raw)
	}
	first := items[0].(map[string]any)
	last := items[len(items)-1].(map[string]any)
	if first["run_id"] != "run-001" || last["run_id"] != "run-100" {
		t.Fatalf("the wrong runs were kept: %s .. %s", first["run_id"], last["run_id"])
	}
}

func TestAgentRunReadAnswersTheLearningRunsOfTheSameSessionToo(t *testing.T) {
	app, db, _, _, token, _ := httpFixture(t)
	session := dataHTTP(t, app.Handler, "POST", "/api/v1/sessions",
		`{"mode":"learning_agent","title":"学习运行","requestId":"learning-run-session"}`, token)["id"].(string)
	if err := db.Create(&model.AgentRun{
		ID: "learning-run", UserID: 1, SessionID: session,
		ExecutionKind: model.AgentRunExecutionLearning, Status: model.AgentRunWaitingUser,
		Version: 1, StartedAt: time.Now(),
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.AgentStep{
		UserID: 1, RunID: "learning-run", Sequence: 1, Type: model.AgentStepPlan,
	}).Error; err != nil {
		t.Fatal(err)
	}

	envelope, raw := runRead(t, app, token, session)
	runs := envelope["runs"].([]any)
	if len(runs) != 1 {
		t.Fatalf("runs %s", raw)
	}
	run := runs[0].(map[string]any)
	if run["execution_kind"] != "learning" || run["status"] != "waiting_user" {
		t.Fatalf("the learning run was not reported as one: %s", raw)
	}
	if steps := run["steps"].([]any); len(steps) != 1 {
		t.Fatalf("the learning run's steps %s", raw)
	}
}

func TestHistoryItemsCarryTheRunThatProducedThem(t *testing.T) {
	app, _, _, _, token, _ := httpFixture(t)
	session, runID := agentTurn(t, app, token, "history-run-id")

	items := dataHTTP(t, app.Handler, "GET", "/api/v1/sessions/"+session+"/messages", "", token)["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("history items %v", items)
	}
	for _, item := range items {
		row := item.(map[string]any)
		if row["run_id"] != runID {
			t.Fatalf("a history item does not name its run: %v", row)
		}
	}
}

// TestHistoryItemsOfAnOrdinaryChatCarryNoRun pins the other half of the
// contract: the run id is the run's own fact, so a message that no run
// produced has none, and the key is absent rather than empty.
func TestHistoryItemsOfAnOrdinaryChatCarryNoRun(t *testing.T) {
	app, _, _, _, token, _ := httpFixture(t)
	session := createHTTP(t, app.Handler, token, "普通对话")
	dataHTTP(t, app.Handler, "POST", "/api/v1/sessions/"+session+"/messages", `{"content":"你好"}`, token)

	items := dataHTTP(t, app.Handler, "GET", "/api/v1/sessions/"+session+"/messages", "", token)["items"].([]any)
	if len(items) == 0 {
		t.Fatal("no history items")
	}
	for _, item := range items {
		row := item.(map[string]any)
		if _, present := row["run_id"]; present {
			t.Fatalf("a message of an ordinary chat named a run: %v", row)
		}
	}
}

func TestAgentRunReadAnswersARunOfTheCallersOwnWithItsSteps(t *testing.T) {
	app, db, _, _, token, _ := httpFixture(t)
	session := dataHTTP(t, app.Handler, "POST", "/api/v1/sessions",
		`{"mode":"learning_agent","title":"单轮","requestId":"single-run-session"}`, token)["id"].(string)
	if err := db.Create(&model.AgentRun{
		ID: "single-run", UserID: 1, SessionID: session,
		ExecutionKind: model.AgentRunExecutionChat, Status: model.AgentRunCompleted,
		Version: 1, StartedAt: time.Now(),
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.AgentStep{
		UserID: 1, RunID: "single-run", Sequence: 1, Type: model.AgentStepTool,
		ToolName: "knowledge_search", DurationMillis: 88,
	}).Error; err != nil {
		t.Fatal(err)
	}

	status, envelope, raw := requestHTTP(t, app.Handler, "GET",
		"/api/v1/AI/agent/sessions/"+session+"/runs/single-run/steps", "", token)
	if status != 200 || envelope["status_code"] != float64(1000) {
		t.Fatalf("run steps %d %s", status, raw)
	}
	run := envelope["run"].(map[string]any)
	if run["run_id"] != "single-run" {
		t.Fatalf("the read named another run: %s", raw)
	}
	steps := run["steps"].([]any)
	if len(steps) != 1 {
		t.Fatalf("steps %s", raw)
	}
	step := steps[0].(map[string]any)
	if step["tool_name"] != "knowledge_search" || step["duration_millis"] != float64(88) {
		t.Fatalf("step %v", step)
	}
}
