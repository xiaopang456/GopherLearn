package hybrid

import (
	"GopherAI/model"
	"encoding/json"
	"fmt"
	"testing"
)

// The per-turn switch, read from outside the process: what a client may say, what
// the server answers when it says something unrecognised, and what a turn that
// forbade the library leaves behind in the ledger.

// createSession makes one session of the given frontend mode and returns its id.
func createSession(t *testing.T, app *Application, token, key, mode string) string {
	t.Helper()
	body := fmt.Sprintf(`{"mode":%q,"title":"开关","requestId":%q}`, mode, key+"-session")
	id := dataHTTP(t, app.Handler, "POST", "/api/v1/sessions", body, token)["id"].(string)
	if id == "" {
		t.Fatalf("no session id for mode %s", mode)
	}
	return id
}

// postTurn sends one message with an explicit knowledgePolicy value and reports
// the status, the envelope and the raw body for the caller's failure message.
func postTurn(t *testing.T, app *Application, token, session, policy string) (int, map[string]any, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"content": "随便聊聊", "requestId": "turn-" + session, "knowledgePolicy": policy})
	return requestHTTP(t, app.Handler, "POST", "/api/v1/sessions/"+session+"/messages", string(body), token)
}

func TestSessionListStatesTheKnowledgeCeilingTheServerSet(t *testing.T) {
	app, _, _, token, _ := httpFixture(t)
	agentSession := createSession(t, app, token, "ceiling-agent", "learning_agent")
	chatSession := createSession(t, app, token, "ceiling-chat", "chat")

	list := dataHTTP(t, app.Handler, "GET", "/api/v1/sessions", "", token)
	items, ok := list["items"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("the list is not the two sessions: %v", list)
	}
	seen := map[string]bool{}
	for _, item := range items {
		row := item.(map[string]any)
		seen[row["id"].(string)] = row["can_use_knowledge"].(bool)
	}
	if !seen[agentSession] {
		t.Fatal("an Agent session is reported as unable to reach the library, so the client would hide a switch that works")
	}
	if seen[chatSession] {
		t.Fatal("a legacy chat session is reported as able to reach the library; the ceiling is the server's fact, not a guess")
	}
}

// A value the server does not know is refused before anything is asked of a model.
// The alternative - folding it into "use the library" - is the direction that reads
// somebody's documents when they meant not to.
func TestUnknownKnowledgePolicyIsRefusedBeforeAnyCall(t *testing.T) {
	app, _, agent, token, _ := httpFixture(t)
	session := createSession(t, app, token, "policy-unknown", "learning_agent")

	status, envelope, raw := postTurn(t, app, token, session, "knowledge_force")
	if status != 400 {
		t.Fatalf("an unknown policy got %d, want 400: %s", status, raw)
	}
	if code := fmt.Sprint(envelope["error"].(map[string]any)["code"]); code != "invalid_request" {
		t.Fatalf("error code = %s, want invalid_request: %s", code, raw)
	}
	if agent.CallCount() != 0 {
		t.Fatalf("a refused turn still asked the model %d times", agent.CallCount())
	}
}

// A session that was never granted the library cannot be granted it by a request:
// the answer is an error, not a quietly downgraded turn, so a client that believes
// it asked for the library learns right away that it did not get it.
func TestLegacyChatSessionCannotBeAskedForTheLibrary(t *testing.T) {
	app, _, agent, token, _ := httpFixture(t)
	session := createSession(t, app, token, "policy-legacy", "chat")

	for _, policy := range []string{"knowledge_preferred", "knowledge_only"} {
		status, envelope, raw := postTurn(t, app, token, session, policy)
		if status != 400 {
			t.Fatalf("%s on a chat session got %d, want 400: %s", policy, status, raw)
		}
		if envelope["error"] == nil {
			t.Fatalf("%s on a chat session returned no error: %s", policy, raw)
		}
	}
	if agent.CallCount() != 0 {
		t.Fatalf("a chat session that asked for the library still ran %d model calls", agent.CallCount())
	}
}

// The switched-off turn is a run like any other - it is recorded, it has an
// identity, and the client can read it back - with one difference that has to be
// observable rather than assumed: nothing in its ledger names a capability.
func TestSwitchedOffTurnAnswersWithoutAnyCapability(t *testing.T) {
	app, db, agent, token, _ := httpFixture(t)
	session := createSession(t, app, token, "off-turn", "learning_agent")

	status, envelope, raw := postTurn(t, app, token, session, "knowledge_disabled")
	if status != 200 || envelope["error"] != nil {
		t.Fatalf("the switched-off turn failed: %d %s", status, raw)
	}
	data := envelope["data"].(map[string]any)
	runID := fmt.Sprint(data["runId"])
	if runID == "" || runID == "<nil>" {
		t.Fatalf("a conversation turn must still be a run: %s", raw)
	}
	if fmt.Sprint(data["answer"]) != "agent response" {
		t.Fatalf("the answer did not come from the V2 model: %v", data["answer"])
	}
	if agent.CallCount() != 1 {
		t.Fatalf("a capability-free turn is one generation, got %d", agent.CallCount())
	}
	var steps []model.AgentStep
	if err := db.Where("run_id = ?", runID).Find(&steps).Error; err != nil {
		t.Fatal(err)
	}
	if len(steps) == 0 {
		t.Fatalf("the turn wrote no ledger rows at all: run=%s", runID)
	}
	for _, step := range steps {
		if step.ToolName != "" {
			t.Fatalf("a turn with no capabilities recorded a tool step: tool=%s type=%s", step.ToolName, step.Type)
		}
	}
}
