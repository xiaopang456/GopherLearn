package openai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/model"
)

// capturedBody decodes what the client actually put on the wire, which is the only
// way to check both directions of a tool contract: what was offered, and what a tool
// result looked like on the way back.
func capturedBody(t *testing.T, reply string) (*Client, func() map[string]any) {
	t.Helper()
	var latest map[string]any
	client, _, _ := scriptedClient(t, 3*time.Second, func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			t.Errorf("the request body could not be read: %v", err)
		}
		if err := json.Unmarshal(raw, &latest); err != nil {
			t.Errorf("the client sent a body that is not JSON: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, reply)
	})
	return client, func() map[string]any { return latest }
}

func toolOfferRequest() model.Request {
	return model.Request{
		Messages: []model.Message{
			{Role: model.RoleSystem, Content: "policy"},
			{Role: model.RoleUser, Content: "AOF 的 fsync 可选哪些"},
		},
		Tools: []model.ToolDefinition{{
			Name:        "knowledge_search",
			Description: "在用户知识库中检索",
			Parameters:  json.RawMessage(`{"type":"object","additionalProperties":false}`),
		}},
	}
}

const toolCallReply = `{"model":"answer-model","choices":[{"message":{"role":"assistant","content":"",` +
	`"tool_calls":[{"id":"call_9","type":"function","function":{"name":"knowledge_search","arguments":"{\"query\":\"AOF\"}"}}]},` +
	`"finish_reason":"tool_calls"}]}`

func TestCompleteWithToolsOffersDefinitionsAndReturnsTheCall(t *testing.T) {
	client, body := capturedBody(t, toolCallReply)

	completion, err := client.CompleteWithTools(context.Background(), toolOfferRequest())
	if err != nil {
		t.Fatal(err)
	}
	// The provider's structured field, not the empty text beside it, is the signal.
	if len(completion.ToolCalls) != 1 {
		t.Fatalf("tool calls = %+v", completion.ToolCalls)
	}
	call := completion.ToolCalls[0]
	if call.ID != "call_9" || call.Name != "knowledge_search" || call.Arguments != `{"query":"AOF"}` {
		t.Fatalf("call = %+v, want the model's own name and argument text unchanged", call)
	}
	if completion.FinishReason != model.FinishToolCalls {
		t.Fatalf("finish reason = %q", completion.FinishReason)
	}

	sent := body()
	tools, ok := sent["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("no tools were offered: %v", sent["tools"])
	}
	definition := tools[0].(map[string]any)
	if definition["type"] != "function" {
		t.Fatalf("tool type = %v", definition["type"])
	}
	function := definition["function"].(map[string]any)
	if function["name"] != "knowledge_search" || function["description"] != "在用户知识库中检索" {
		t.Fatalf("tool function = %v", function)
	}
	// The schema goes out as an object, not as an escaped string: a provider that
	// cannot read it would silently offer the model nothing.
	parameters, ok := function["parameters"].(map[string]any)
	if !ok || parameters["additionalProperties"] != false {
		t.Fatalf("parameters = %T %v", function["parameters"], function["parameters"])
	}
}

// TestToolResultRoundTripIsAttributable checks the second half of the protocol: the
// assistant turn that asked, and the tool turn that answers it, arrive at the provider
// in the shape it expects.
func TestToolResultRoundTripIsAttributable(t *testing.T) {
	client, body := capturedBody(t, `{"choices":[{"message":{"role":"assistant","content":"答案是 always、everysec 或 no"},"finish_reason":"stop"}]}`)

	request := toolOfferRequest()
	request.Messages = append(request.Messages,
		model.Message{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: "call_9", Name: "knowledge_search", Arguments: `{"query":"AOF"}`}}},
		model.Message{Role: model.RoleTool, ToolCallID: "call_9", Content: "[工具结果] knowledge_search：1 条资料"},
	)
	completion, err := client.CompleteWithTools(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if completion.Content == "" || len(completion.ToolCalls) != 0 {
		t.Fatalf("completion = %+v", completion)
	}

	messages, ok := body()["messages"].([]any)
	if !ok || len(messages) != 4 {
		t.Fatalf("messages = %v", body()["messages"])
	}
	asked := messages[2].(map[string]any)
	if asked["role"] != "assistant" {
		t.Fatalf("assistant turn = %v", asked)
	}
	calls, ok := asked["tool_calls"].([]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("the assistant turn lost its tool_calls: %v", asked)
	}
	first := calls[0].(map[string]any)
	if first["id"] != "call_9" || first["type"] != "function" {
		t.Fatalf("call = %v", first)
	}
	if function := first["function"].(map[string]any); function["name"] != "knowledge_search" || function["arguments"] != `{"query":"AOF"}` {
		t.Fatalf("arguments were rewritten on the way back: %v", function)
	}
	result := messages[3].(map[string]any)
	if result["role"] != "tool" || result["tool_call_id"] != "call_9" {
		t.Fatalf("tool result = %v", result)
	}
}

// TestMalformedToolCallResponsesAreRefusedNotRepaired is the strictness rule: a call
// this client could not answer correctly ends the turn as a malformed response. Nothing
// is filled in, because a synthesized id or a defaulted name turns a broken vendor
// answer into a legal-looking request the harness would then execute.
func TestMalformedToolCallResponsesAreRefusedNotRepaired(t *testing.T) {
	cases := []struct {
		name  string
		calls string
	}{
		{"no id", `[{"type":"function","function":{"name":"knowledge_search","arguments":"{}"}}]`},
		{"blank id", `[{"id":"   ","type":"function","function":{"name":"knowledge_search","arguments":"{}"}}]`},
		{"no name", `[{"id":"c1","type":"function","function":{"arguments":"{}"}}]`},
		{"blank name", `[{"id":"c1","type":"function","function":{"name":"  ","arguments":"{}"}}]`},
		{"not a function call", `[{"id":"c1","type":"mcp","function":{"name":"knowledge_search","arguments":"{}"}}]`},
		{"missing type", `[{"id":"c1","function":{"name":"knowledge_search","arguments":"{}"}}]`},
		{"duplicate ids", `[{"id":"c1","type":"function","function":{"name":"a","arguments":"{}"}},` +
			`{"id":"c1","type":"function","function":{"name":"b","arguments":"{}"}}]`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			client, _ := capturedBody(t, `{"choices":[{"message":{"role":"assistant","content":"",`+
				`"tool_calls":`+testCase.calls+`},"finish_reason":"tool_calls"}]}`)
			_, err := client.CompleteWithTools(context.Background(), toolOfferRequest())
			if !errors.Is(err, model.ErrMalformedResponse) {
				t.Fatalf("error = %v, want a malformed-response refusal", err)
			}
		})
	}
}

// TestToolCallWithEmptyContentAndNoCallsIsStillAnEmptyAnswer guards the other side of
// the same rule: refusing to repair must not become permission to accept a blank answer.
func TestToolCallWithEmptyContentAndNoCallsIsStillAnEmptyAnswer(t *testing.T) {
	client, _ := capturedBody(t, `{"choices":[{"message":{"role":"assistant","content":"  "},"finish_reason":"stop"}]}`)
	if _, err := client.CompleteWithTools(context.Background(), toolOfferRequest()); !errors.Is(err, model.ErrEmptyCompletion) {
		t.Fatalf("error = %v, want ErrEmptyCompletion", err)
	}
}

func TestToolPathsRefuseMismatchedRequestsBeforeTheWire(t *testing.T) {
	hits := 0
	client, _, counter := scriptedClient(t, 2*time.Second, func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"x"},"finish_reason":"stop"}]}`)
	})
	_ = client

	// Offering tools to the plain path is a wiring mistake, not something to serve.
	if _, err := client.Complete(context.Background(), toolOfferRequest()); !errors.Is(err, model.ErrInvalidRequest) {
		t.Fatalf("Complete with tools error = %v", err)
	}
	if _, err := client.Stream(context.Background(), toolOfferRequest(), func(string) error { return nil }); !errors.Is(err, model.ErrInvalidRequest) {
		t.Fatalf("Stream with tools error = %v", err)
	}
	if _, err := client.CompleteWithTools(context.Background(), model.Request{
		Messages: []model.Message{{Role: model.RoleUser, Content: "问题"}},
	}); !errors.Is(err, model.ErrInvalidRequest) {
		t.Fatalf("CompleteWithTools without tools error = %v", err)
	}
	broken := toolOfferRequest()
	broken.Tools[0].Parameters = json.RawMessage(`{"type":`)
	if _, err := client.CompleteWithTools(context.Background(), broken); !errors.Is(err, model.ErrInvalidRequest) {
		t.Fatalf("broken schema error = %v", err)
	}
	if got := counter.Load(); got != 0 {
		t.Fatalf("%d requests reached the endpoint, want none: a refused call must be free", got)
	}
	if hits != 0 {
		t.Fatalf("handler ran %d times", hits)
	}
}

func TestPlainCompletionStillSendsNoTools(t *testing.T) {
	client, body := capturedBody(t, `{"choices":[{"message":{"role":"assistant","content":"回答"},"finish_reason":"stop"}]}`)
	if _, err := client.Complete(context.Background(), model.Request{
		Messages: []model.Message{{Role: model.RoleUser, Content: "问题"}},
	}); err != nil {
		t.Fatal(err)
	}
	sent := body()
	if _, exists := sent["tools"]; exists {
		t.Fatalf("a plain completion offered tools: %v", sent["tools"])
	}
	if _, exists := sent["tool_calls"]; exists {
		t.Fatalf("a plain completion carried tool calls")
	}
}

func TestStreamFailsRatherThanSwallowingToolCalls(t *testing.T) {
	client, _, _ := scriptedClient(t, 3*time.Second, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"我先查一下"}}]}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"id":"c1","function":{"name":"knowledge_search","arguments":"{}"}}]}}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})
	_, err := client.Stream(context.Background(), model.Request{
		Messages: []model.Message{{Role: model.RoleUser, Content: "问题"}},
	}, func(string) error { return nil })
	if !errors.Is(err, ErrStreamToolCalls) {
		t.Fatalf("error = %v, want ErrStreamToolCalls rather than an answer that ignores the request", err)
	}
}

// The closeout is stated on the wire: a request that forbids tool choice says
// so, a request that says nothing keeps the exact shape it always had, and a
// tool choice without tools is a programming fault refused before the wire.
func TestToolChoiceIsStatedOnTheWire(t *testing.T) {
	client, body := capturedBody(t, toolCallReply)

	request := toolOfferRequest()
	request.ToolChoice = model.ToolChoiceNone
	if _, err := client.CompleteWithTools(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if body()["tool_choice"] != "none" {
		t.Fatalf("tool_choice = %v, want none on the wire", body()["tool_choice"])
	}

	quiet, quietBody := capturedBody(t, toolCallReply)
	if _, err := quiet.CompleteWithTools(context.Background(), toolOfferRequest()); err != nil {
		t.Fatal(err)
	}
	if _, stated := quietBody()["tool_choice"]; stated {
		t.Fatal("an unstated tool choice reached the wire")
	}

	plain, _ := capturedBody(t, toolCallReply)
	plainRequest := model.Request{Messages: []model.Message{{Role: model.RoleUser, Content: "问"}}}
	plainRequest.ToolChoice = model.ToolChoiceNone
	if _, err := plain.CompleteWithTools(context.Background(), plainRequest); err == nil {
		t.Fatal("tool_choice none without tools offered was sent anyway")
	}
}
