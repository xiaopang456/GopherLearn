package hybrid

import (
	"encoding/json"
	"fmt"
	"testing"

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

func TestAgentStreamHonoursTheRequestThinkingChoice(t *testing.T) {
	for _, deployment := range []bool{false, true} {
		t.Run(fmt.Sprintf("deployment_%t", deployment), func(t *testing.T) {
			settings := testSettings(t)
			settings.Config.AgentConfig.AgentThinkingEnabled = deployment
			app, _, m, _, token, _ := sseFixtureWithSettings(t, settings, func(int, []*schema.Message) streamPlan {
				return streamPlan{parts: pieces("答案")}
			})
			sid := createAgentHTTP(t, app, token, fmt.Sprintf("create-thinking-%t", deployment), "Agent")

			first := streamThinkingFrames(t, app, token, sid, thinkingStreamBody("request-thinking-001", "你好", nil))
			done := named(first, "message.completed")
			if len(done) != 1 {
				t.Fatalf("first turn %v", first)
			}
			runID := done[0].data["run_id"]

			// A request that says nothing and one that says what the
			// deployment already says are the same run.
			inherited := streamThinkingFrames(t, app, token, sid, thinkingStreamBody("request-thinking-001", "你好", boolPointer(deployment)))
			done = named(inherited, "message.completed")
			if len(done) != 1 || done[0].data["run_id"] != runID || done[0].data["publication"] != "replay" || m.CallCount() != 1 {
				t.Fatalf("inherited turn %v", inherited)
			}

			// A request that says the opposite is a different run, and the
			// request number it borrowed already belongs to the other one.
			overridden := streamThinkingFrames(t, app, token, sid, thinkingStreamBody("request-thinking-001", "你好", boolPointer(!deployment)))
			fail := named(overridden, "message.failed")
			if len(fail) != 1 || fail[0].data["native_code"] != float64(10002) || m.CallCount() != 1 {
				t.Fatalf("overridden turn %v", overridden)
			}
		})
	}
}

// The turn that does not stream is the same surface with the same choice, so
// the field has to travel there too.
func TestAgentMessageHonoursTheRequestThinkingChoice(t *testing.T) {
	app, _, m, _, token, _ := sseFixture(t, func(int, []*schema.Message) streamPlan {
		return streamPlan{parts: pieces("答案")}
	})
	sid := createAgentHTTP(t, app, token, "create-thinking-native", "Agent")
	path := "/api/v1/sessions/" + sid + "/messages"

	first := dataHTTP(t, app.Handler, "POST", path, thinkingBody("request-thinking-native-001", "你好", nil), token)
	runID := first["runId"]
	if runID == "" {
		t.Fatalf("first turn %+v", first)
	}
	inherited := dataHTTP(t, app.Handler, "POST", path, thinkingBody("request-thinking-native-001", "你好", boolPointer(false)), token)
	if inherited["runId"] != runID || inherited["replayed"] != true || m.CallCount() != 1 {
		t.Fatalf("inherited turn %+v", inherited)
	}
	status, envelope, _ := requestHTTP(t, app.Handler, "POST", path, thinkingBody("request-thinking-native-001", "你好", boolPointer(true)), token)
	if status != 409 || envelope["error"] == nil || m.CallCount() != 1 {
		t.Fatalf("overridden turn %d %v", status, envelope)
	}
}

func thinkingBody(key, question string, thinking *bool) string {
	body := map[string]any{"content": question, "requestId": key}
	if thinking != nil {
		body["enableThinking"] = *thinking
	}
	b, _ := json.Marshal(body)
	return string(b)
}

func thinkingStreamBody(key, question string, thinking *bool) string {
	body := map[string]any{"content": question, "requestId": key, "stream_steps": true}
	if thinking != nil {
		body["enableThinking"] = *thinking
	}
	b, _ := json.Marshal(body)
	return string(b)
}

func streamThinkingFrames(t *testing.T, app *Application, token, sid, body string) []wireFrame {
	t.Helper()
	status, _, text := requestHTTP(t, app.Handler, "POST", "/api/v1/sessions/"+sid+"/stream", body, token)
	if status != 200 {
		t.Fatalf("Agent SSE HTTP %d %s", status, text)
	}
	return frames(t, text)
}

func boolPointer(value bool) *bool { return &value }
