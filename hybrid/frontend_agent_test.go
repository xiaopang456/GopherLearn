package hybrid

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
)

// Real local HTTP -> original auth/Service/Runner/DAO, using the current Vue
// composable and API/parser/reducers in Node; model and database stay offline.
func TestFrontendAgentComposableThroughHybridHTTP(t *testing.T) {
	app, db, model, ordinary, _, _ := sseFixture(t, func(_ int, _ []*schema.Message) streamPlan {
		return streamPlan{parts: pieces("你好", "，公开片段")}
	})
	server := httptest.NewServer(app.Handler)
	defer server.Close()
	command := exec.Command("node", "../web/test/helpers/hybridAgentClient.mjs")
	command.Env = append(os.Environ(), "HYBRID_TEST_URL="+server.URL)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("frontend original chain: %v\n%s", err, output)
	}
	var result struct {
		Agent           string `json:"agent"`
		RunID           string `json:"runId"`
		MessageID       string `json:"messageId"`
		Content         string `json:"content"`
		History         int    `json:"historyCount"`
		Ordinary        string `json:"ordinary"`
		OrdinaryRunID   string `json:"ordinaryRunId"`
		OrdinaryContent string `json:"ordinaryContent"`
		OrdinaryDone    bool   `json:"ordinaryCompleted"`
	}
	// Vue development warnings can precede the final JSON line.
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &result); err != nil {
		t.Fatal(err)
	}
	// Two generations: the Agent turn and the conversation turn, both on the shared
	// path. The frontend asserts the conversation answer rendered exactly once, which is
	// what the old append rule would have doubled.
	if model.CallCount() != 2 || result.History != 1 || !result.OrdinaryDone {
		t.Fatalf("calls/result: %d %+v", model.CallCount(), result)
	}
	if calls := len(ordinary.all()); calls != 0 {
		t.Fatalf("the frontend's conversation turn reached the retired ordinary model %d times", calls)
	}
	rows := agentRows(t, db, result.Agent)
	if len(rows) != 2 || rows[1].Content != result.Content || result.RunID == "" || result.MessageID == "" {
		t.Fatalf("actual DAO rows/result: %+v %+v", rows, result)
	}
	ordinaryRows := agentRows(t, db, result.Ordinary)
	if len(ordinaryRows) != 2 || ordinaryRows[1].Content != result.OrdinaryContent {
		t.Fatalf("conversation rows/result: %+v %+v", ordinaryRows, result.OrdinaryContent)
	}
	if result.OrdinaryRunID == "" || ordinaryRows[1].AgentRunID == nil || *ordinaryRows[1].AgentRunID != result.OrdinaryRunID {
		t.Fatalf("the rendered conversation answer names a run the row does not: %+v rows=%+v", result.OrdinaryRunID, ordinaryRows[1].AgentRunID)
	}
}
