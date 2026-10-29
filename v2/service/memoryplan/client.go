package memoryplan

import (
	"context"
	"fmt"
	"strings"

	openaiChat "github.com/cloudwego/eino-ext/components/model/openai"
)

// NewChatModel builds the small-model client this mode uses for both stages.
//
// Thinking is off and temperature is pinned at zero. Both are frozen experiment
// parameters rather than defaults left to the environment: a planner whose
// sampling changed between the development run and the blind run would be a
// different method wearing the same name, and thinking on this provider was
// measured at four times the latency for no change in answer.
//
// If a future SDK stops honouring either field, the honest move is to record the
// real value in the freeze rather than keep asserting this one.
func NewChatModel(ctx context.Context, config Config, apiKey string) (PlannerCaller, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("model-planned recall requires its own API key")
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	temperature := float32(0)
	chat, err := openaiChat.NewChatModel(ctx, &openaiChat.ChatModelConfig{
		BaseURL: config.BaseURL, Model: config.Model, APIKey: apiKey,
		Temperature: &temperature,
		ExtraFields: map[string]any{"enable_thinking": false},
	})
	if err != nil {
		// The error is wrapped without its body. A provider error can echo the
		// request that caused it, and the request carries the key.
		return nil, fmt.Errorf("create model-planned recall client for %s: %w", config.Model, err)
	}
	return chat, nil
}
