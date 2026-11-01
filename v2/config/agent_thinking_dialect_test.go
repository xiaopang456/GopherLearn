package config

import (
	"errors"
	"strings"
	"testing"
)

// DeepSeek and the Agent's thinking switch cannot be combined, and a
// deployment that combines them has to be refused at startup rather than
// discovered on the first question of the day.
func TestAgentThinkingIsRefusedWithTheDeepSeekDialect(t *testing.T) {
	loaded := defaultConfig()
	loaded.RagModelConfig.RagChatThinkingDialect = ChatThinkingDialectDeepSeek
	loaded.AgentConfig.AgentThinkingEnabled = true

	err := loaded.ValidateAgentThinking()
	if !errors.Is(err, ErrInvalidAgentThinkingDialect) {
		t.Fatalf("error = %v, want %v", err, ErrInvalidAgentThinkingDialect)
	}
	message := err.Error()
	// The operator has to be able to act on this without reading the source:
	// what breaks, why, and what to do instead.
	for _, want := range []string{
		"reasoning_content",
		"HTTP 400",
		"agentThinkingEnabled = false",
		ChatThinkingDialectQwen,
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("message %q does not mention %q", message, want)
		}
	}
}

// The qwen dialect has no such rule, so the same switch is a normal setting
// there.
func TestAgentThinkingIsAcceptedWithTheQwenDialect(t *testing.T) {
	loaded := defaultConfig()
	loaded.RagModelConfig.RagChatThinkingDialect = ChatThinkingDialectQwen
	loaded.AgentConfig.AgentThinkingEnabled = true
	if err := loaded.ValidateAgentThinking(); err != nil {
		t.Fatalf("qwen dialect with thinking on: %v", err)
	}

	unset := defaultConfig()
	unset.AgentConfig.AgentThinkingEnabled = true
	if err := unset.ValidateAgentThinking(); err != nil {
		t.Fatalf("default dialect with thinking on: %v", err)
	}
}

// Thinking off is the default, and a DeepSeek deployment with it off is the
// configuration this product ships against.
func TestAgentThinkingOffIsAcceptedWithEveryDialect(t *testing.T) {
	for _, dialect := range []string{"", ChatThinkingDialectQwen, ChatThinkingDialectDeepSeek, ChatThinkingDialectNone} {
		loaded := defaultConfig()
		loaded.RagModelConfig.RagChatThinkingDialect = dialect
		loaded.AgentConfig.AgentThinkingEnabled = false
		if err := loaded.ValidateAgentThinking(); err != nil {
			t.Fatalf("dialect %q with thinking off: %v", dialect, err)
		}
	}
}
