package isolated

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"GopherAI/model"

	"github.com/cloudwego/eino/schema"
)

// A request may state whether it wants the chat model to think, and a request
// that states nothing inherits the deployment's own choice.
//
// The proof needs no provider. A run is identified by what it was asked to
// do, and the thinking state is part of that identity: a request that behaves
// like the run that already exists under the same request number is that run
// again, and one that behaves differently is refused as a conflict. That makes
// the three states - absent, true and false - observable from the outside.

func TestAgentRunInheritsTheDeploymentThinkingDefault(t *testing.T) {
	for _, deployment := range []bool{false, true} {
		t.Run(fmt.Sprintf("deployment_%t", deployment), func(t *testing.T) {
			settings := testSettings(t)
			settings.Config.AgentConfig.AgentThinkingEnabled = deployment
			app, db, _, token := newFixture(t, settings, func(context.Context, int) (*schema.Message, error) {
				return schema.AssistantMessage("你好", nil), nil
			})
			session := createSession(t, app, token, fmt.Sprintf("create-thinking-%t", deployment))

			first := runThinkingHTTP(t, app, token, session, "run-thinking-001", "你好", nil)
			if first["status_code"] != float64(1000) || first["runId"] == "" {
				t.Fatalf("first run: %+v", first)
			}
			runID := first["runId"]

			// A request that says nothing and one that says what the
			// deployment already says are the same run.
			inherited := runThinkingHTTP(t, app, token, session, "run-thinking-001", "你好", boolPointer(deployment))
			if inherited["status_code"] != float64(1000) || inherited["runId"] != runID || inherited["replayed"] != true {
				t.Fatalf("inherited run: %+v", inherited)
			}

			// A request that says the opposite is a different run, and the
			// request number it borrowed already belongs to the other one.
			overridden := runThinkingHTTP(t, app, token, session, "run-thinking-001", "你好", boolPointer(!deployment))
			if overridden["status_code"] != float64(10002) {
				t.Fatalf("overridden run: %+v", overridden)
			}
			var runs int64
			if err := db.Model(&model.AgentRun{}).Where("request_key = ?", "run-thinking-001").Count(&runs).Error; err != nil {
				t.Fatal(err)
			}
			if runs != 1 {
				t.Fatalf("the refused request started a second run: %d", runs)
			}
		})
	}
}

func runThinkingHTTP(t *testing.T, app *Application, token, session, key, question string, thinking *bool) map[string]any {
	t.Helper()
	body := map[string]any{"sessionId": session, "requestId": key, "question": question, "knowledgePolicy": "knowledge_preferred"}
	if thinking != nil {
		body["enableThinking"] = *thinking
	}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	postJSON(t, app.Handler, "/api/v1/AI/agent/run", string(data), token, &result)
	return result
}

func boolPointer(value bool) *bool { return &value }
