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

	cases := []struct {
		name    string
		dialect string
		enabled bool
		opts    []model.Option
		want    string
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
				ExtraFields: ThinkingExtraFields(tc.dialect, tc.enabled),
			})
			if err != nil {
				t.Fatalf("create chat model: %v", err)
			}

			if _, err := llm.Generate(context.Background(), []*schema.Message{schema.UserMessage("hello")}, tc.opts...); err != nil {
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
