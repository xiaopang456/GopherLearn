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
		Agent     string `json:"agent"`
		RunID     string `json:"runId"`
		MessageID string `json:"messageId"`
		Content   string `json:"content"`
		History   int    `json:"historyCount"`
		Ordinary  bool   `json:"ordinaryCompleted"`
	}
	// Vue development warnings can precede the final JSON line.
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &result); err != nil {
		t.Fatal(err)
	}
	if model.CallCount() != 1 || result.History != 1 || !result.Ordinary {
		t.Fatalf("calls/result: %d %+v", model.CallCount(), result)
	}
	rows := agentRows(t, db, result.Agent)
	if len(rows) != 2 || rows[1].Content != result.Content || result.RunID == "" || result.MessageID == "" {
		t.Fatalf("actual DAO rows/result: %+v %+v", rows, result)
	}
	_ = ordinary // Original ordinary Stream was invoked by the same frontend API.
}
