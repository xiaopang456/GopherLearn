package aihelper

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"GopherAI/config"

	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// The bodies below are the bytes the provider received before the OpenAI
// component was upgraded. They are pinned here because the dialect decides
// what a provider sees, and a deployment upgrade must not change that.
//
// The other test in this package pins the extra-field table. That table is an
// intermediate value: it only becomes a request once the client has put it on
// the wire, and the client decides the order and the spelling. These cases
// pin the result, so a change in the client shows up here instead of in
// production.
//
// The model is built the way the product builds it - the dialect's switch on
// the model, the per-request options on the call - because that is the only
// way these cases pin what a deployment sends. The DeepSeek cases carry the
// reasoning_content patch, because that is what a DeepSeek deployment with
// thinking on sends.
func TestTheRequestBodyOnTheWireIsPinned(t *testing.T) {
	// A configured budget would add a field to the qwen table, which is a
	// different request. The budget has its own test; this one pins the
	// plain table.
	t.Setenv("OPENAI_THINKING_BUDGET", "")

	knowledgeSearch := &schema.ToolInfo{
		Name: "knowledge_search",
		Desc: "the knowledge_search tool",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"query": {Type: schema.String, Required: true, Desc: "the query"},
		}),
	}
	memoryRecall := &schema.ToolInfo{
		Name: "memory_recall",
		Desc: "the memory_recall tool",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"query": {Type: schema.String, Required: true, Desc: "the query"},
		}),
	}

	const toolSchema = `{"properties":{"query":{"description":"the query","type":"string"}},"required":["query"],"type":"object"}`

	// The turn DeepSeek asks about: an assistant turn that asked for a tool.
	const toolCallTurn = `{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"knowledge_search","arguments":"{\"query\":\"golang\"}"}}]}`

	assistantWithToolCall := schema.AssistantMessage("", []schema.ToolCall{{
		ID:       "call_1",
		Type:     "function",
		Function: schema.FunctionCall{Name: "knowledge_search", Arguments: `{"query":"golang"}`},
	}})
	assistantThatThought := schema.AssistantMessage("", []schema.ToolCall{{
		ID:       "call_1",
		Type:     "function",
		Function: schema.FunctionCall{Name: "knowledge_search", Arguments: `{"query":"golang"}`},
	}})
	assistantThatThought.ReasoningContent = "I looked it up"

	cases := []struct {
		name     string
		dialect  string
		enabled  bool
		opts     []model.Option
		messages []*schema.Message
		want     string
	}{
		{
			name:    "qwen dialect, thinking off",
			dialect: config.ChatThinkingDialectQwen,
			want:    `{"enable_thinking":false,"messages":[{"role":"user","content":"hello"}],"model":"test"}`,
		},
		{
			name:    "qwen dialect, thinking on",
			dialect: config.ChatThinkingDialectQwen,
			enabled: true,
			want:    `{"enable_thinking":true,"messages":[{"role":"user","content":"hello"}],"model":"test"}`,
		},
		{
			name:    "deepseek dialect, thinking on",
			dialect: config.ChatThinkingDialectDeepSeek,
			enabled: true,
			want:    `{"messages":[{"role":"user","content":"hello"}],"model":"test","thinking":{"type":"enabled"}}`,
		},
		{
			name:    "none dialect sends no switch at all",
			dialect: config.ChatThinkingDialectNone,
			want:    `{"model":"test","messages":[{"role":"user","content":"hello"}]}`,
		},
		{
			name:    "two tools, the model chooses",
			dialect: config.ChatThinkingDialectQwen,
			opts:    []model.Option{model.WithTools([]*schema.ToolInfo{knowledgeSearch, memoryRecall})},
			want: `{"enable_thinking":false,"messages":[{"role":"user","content":"hello"}],"model":"test","tools":[` +
				`{"type":"function","function":{"name":"knowledge_search","description":"the knowledge_search tool","parameters":` + toolSchema + `}},` +
				`{"type":"function","function":{"name":"memory_recall","description":"the memory_recall tool","parameters":` + toolSchema + `}}]}`,
		},
		{
			name:    "one tool, the caller insists on it",
			dialect: config.ChatThinkingDialectQwen,
			opts: []model.Option{
				model.WithTools([]*schema.ToolInfo{knowledgeSearch}),
				model.WithToolChoice(schema.ToolChoiceForced),
			},
			want: `{"enable_thinking":false,"messages":[{"role":"user","content":"hello"}],"model":"test","tool_choice":{"type":"function","function":{"name":"knowledge_search"}},"tools":[` +
				`{"type":"function","function":{"name":"knowledge_search","description":"the knowledge_search tool","parameters":` + toolSchema + `}}]}`,
		},
		{
			name:    "two tools, the caller insists on one of them",
			dialect: config.ChatThinkingDialectQwen,
			opts: []model.Option{
				model.WithTools([]*schema.ToolInfo{knowledgeSearch, memoryRecall}),
				model.WithToolChoice(schema.ToolChoiceForced),
			},
			want: `{"enable_thinking":false,"messages":[{"role":"user","content":"hello"}],"model":"test","tool_choice":"required","tools":[` +
				`{"type":"function","function":{"name":"knowledge_search","description":"the knowledge_search tool","parameters":` + toolSchema + `}},` +
				`{"type":"function","function":{"name":"memory_recall","description":"the memory_recall tool","parameters":` + toolSchema + `}}]}`,
		},
		{
			name:    "deepseek dialect with tools",
			dialect: config.ChatThinkingDialectDeepSeek,
			enabled: true,
			opts:    []model.Option{model.WithTools([]*schema.ToolInfo{knowledgeSearch, memoryRecall})},
			want: `{"messages":[{"role":"user","content":"hello"}],"model":"test","thinking":{"type":"enabled"},"tools":[` +
				`{"type":"function","function":{"name":"knowledge_search","description":"the knowledge_search tool","parameters":` + toolSchema + `}},` +
				`{"type":"function","function":{"name":"memory_recall","description":"the memory_recall tool","parameters":` + toolSchema + `}}]}`,
		},
		{
			name:    "deepseek dialect, thinking off",
			dialect: config.ChatThinkingDialectDeepSeek,
			want:    `{"messages":[{"role":"user","content":"hello"}],"model":"test","thinking":{"type":"disabled"}}`,
		},
		{
			name:     "deepseek dialect, thinking on, an assistant turn that asked for a tool gets the field",
			dialect:  config.ChatThinkingDialectDeepSeek,
			enabled:  true,
			messages: []*schema.Message{assistantWithToolCall},
			want:     `{"messages":[{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"knowledge_search","arguments":"{\"query\":\"golang\"}"}}],"reasoning_content":""}],"model":"test","thinking":{"type":"enabled"}}`,
		},
		{
			name:     "deepseek dialect, thinking on, an assistant turn that thought keeps its own content",
			dialect:  config.ChatThinkingDialectDeepSeek,
			enabled:  true,
			messages: []*schema.Message{assistantThatThought},
			want:     `{"messages":[{"role":"assistant","reasoning_content":"I looked it up","tool_calls":[{"id":"call_1","type":"function","function":{"name":"knowledge_search","arguments":"{\"query\":\"golang\"}"}}]}],"model":"test","thinking":{"type":"enabled"}}`,
		},
		{
			name:     "qwen dialect, thinking on, an assistant turn that asked for a tool is left alone",
			dialect:  config.ChatThinkingDialectQwen,
			enabled:  true,
			messages: []*schema.Message{assistantWithToolCall},
			want:     `{"enable_thinking":true,"messages":[{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"knowledge_search","arguments":"{\"query\":\"golang\"}"}}]}],"model":"test"}`,
		},
		{
			name:     "deepseek dialect, thinking off, an assistant turn that asked for a tool is left alone",
			dialect:  config.ChatThinkingDialectDeepSeek,
			messages: []*schema.Message{assistantWithToolCall},
			want:     `{"messages":[{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"knowledge_search","arguments":"{\"query\":\"golang\"}"}}]}],"model":"test","thinking":{"type":"disabled"}}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var captured []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("read request body: %v", err)
					return
				}
				captured = append(captured, string(body))
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"1","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
			}))
			defer server.Close()

			llm, err := openai.NewChatModel(context.Background(), &openai.ChatModelConfig{
				BaseURL:     server.URL,
				Model:       "test",
				APIKey:      "sk-test",
				ExtraFields: ThinkingExtraFields(tc.dialect, false),
			})
			if err != nil {
				t.Fatalf("create chat model: %v", err)
			}

			messages := tc.messages
			if messages == nil {
				messages = []*schema.Message{schema.UserMessage("hello")}
			}
			opts := append([]model.Option{}, tc.opts...)
			opts = append(opts, ThinkingModelOptions(tc.dialect, tc.enabled)...)

			if _, err := llm.Generate(context.Background(), messages, opts...); err != nil {
				t.Fatalf("generate: %v", err)
			}
			if len(captured) != 1 {
				t.Fatalf("captured %d requests, want 1", len(captured))
			}
			if captured[0] != tc.want {
				t.Fatalf("request body changed\n got: %s\nwant: %s", captured[0], tc.want)
			}
		})
	}
}
