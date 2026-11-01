package aihelper

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"GopherAI/config"

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

// ThinkingExtraFields builds the fields that express the caller's thinking
// choice in the dialect the provider understands.
//
// The dialect decides the shape, the caller decides the state:
//
//   - qwen sends enable_thinking, and thinking_budget as well when one is
//     configured and thinking is on;
//   - deepseek sends thinking: {type: enabled|disabled};
//   - none sends nothing.
//
// The qwen branch is byte-for-byte what this file sent before the dialect
// existed, so a deployment that never names one behaves exactly as it did.
//
// A provider that does not know the field ignores it rather than rejecting
// the request, but an ignored field leaves the provider's own default in
// force, and DeepSeek's default is thinking on. That is why the dialect is a
// setting rather than something derived from the endpoint.
func ThinkingExtraFields(dialect string, enabled bool) map[string]any {
	switch dialect {
	case config.ChatThinkingDialectDeepSeek:
		state := "disabled"
		if enabled {
			state = "enabled"
		}
		return map[string]any{"thinking": map[string]any{"type": state}}
	case config.ChatThinkingDialectNone:
		return map[string]any{}
	default:
		fields := map[string]any{"enable_thinking": enabled}
		if budget, err := strconv.Atoi(strings.TrimSpace(os.Getenv("OPENAI_THINKING_BUDGET"))); err == nil && budget > 0 {
			fields["thinking_budget"] = budget
		}
		return fields
	}
}

// ThinkingModelOption applies the per-request thinking choice to any Eino
// OpenAI-compatible model call, including calls made inside ReAct.
//
// The dialect travels with the call rather than being read from the
// environment: one process may hold more than one model identity, and a
// request must not depend on which one happens to be configured.
//
// The extra fields replace the model's own table rather than merging into it
// (the Eino client's WithExtraFields assigns), so the table this returns is
// the whole table the request carries.
func ThinkingModelOption(dialect string, enabled bool) model.Option {
	return openai.WithExtraFields(ThinkingExtraFields(dialect, enabled))
}

func NewToolCallingChatModel(ctx context.Context) (model.ToolCallingChatModel, error) {
	key := os.Getenv("OPENAI_API_KEY")
	modelName := os.Getenv("OPENAI_MODEL_NAME")
	baseURL := os.Getenv("OPENAI_BASE_URL")
	dialect, err := config.NormalizeChatThinkingDialect(os.Getenv("OPENAI_THINKING_DIALECT"))
	if err != nil {
		return nil, fmt.Errorf("create openai model failed: %v", err)
	}

	llm, err := openai.NewChatModel(ctx, &openai.ChatModelConfig{
		BaseURL:     baseURL,
		Model:       modelName,
		APIKey:      key,
		ExtraFields: ThinkingExtraFields(dialect, false),
	})
	if err != nil {
		return nil, fmt.Errorf("create openai model failed: %v", err)
	}
	return llm, nil
}
