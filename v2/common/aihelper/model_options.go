package aihelper

import (
 "fmt"
 "context"
 "os"
 "strconv"
 "strings"
 "github.com/cloudwego/eino-ext/components/model/openai"
 "github.com/cloudwego/eino/components/model"
)

type thinkingContextKey struct{}

// WithThinking records the user's choice for one chat request. It is scoped to
// the request context so concurrent users can select different modes without
// rebuilding or mutating the shared model client.
func WithThinking(ctx context.Context, enabled bool) context.Context {
	return context.WithValue(ctx, thinkingContextKey{}, enabled)
}

func thinkingEnabled(ctx context.Context) bool {
	enabled, _ := ctx.Value(thinkingContextKey{}).(bool)
	return enabled
}

func thinkingExtraFields(enabled bool) map[string]any {
	fields := map[string]any{"enable_thinking": enabled}
	if budget, err := strconv.Atoi(strings.TrimSpace(os.Getenv("OPENAI_THINKING_BUDGET"))); err == nil && budget > 0 {
		if enabled {
			fields["thinking_budget"] = budget
		}
	}
	return fields
}

// ThinkingModelOption applies the per-request Qwen thinking choice to any
// Eino OpenAI-compatible model call, including calls made inside ReAct.
func ThinkingModelOption(enabled bool) model.Option {
	return openai.WithExtraFields(thinkingExtraFields(enabled))
}

func NewToolCallingChatModel(ctx context.Context) (model.ToolCallingChatModel, error) {
	key := os.Getenv("OPENAI_API_KEY")
	modelName := os.Getenv("OPENAI_MODEL_NAME")
	baseURL := os.Getenv("OPENAI_BASE_URL")

	llm, err := openai.NewChatModel(ctx, &openai.ChatModelConfig{
		BaseURL:     baseURL,
		Model:       modelName,
		APIKey:      key,
		ExtraFields: thinkingExtraFields(false),
	})
	if err != nil {
		return nil, fmt.Errorf("create openai model failed: %v", err)
	}
	return llm, nil
}
