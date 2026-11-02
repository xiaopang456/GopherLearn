package config

import (
	"strings"
	"testing"
)

// The DeepSeek dialect with the Agent's thinking switch is a working
// configuration. The client adds the reasoning_content field that DeepSeek
// asks for on the turns that did not think, so there is nothing left to
// refuse: a deployment that turns thinking on with this dialect has to start.
func TestAgentThinkingIsAcceptedWithEveryDialect(t *testing.T) {
	for _, dialect := range []string{"", ChatThinkingDialectQwen, ChatThinkingDialectDeepSeek, ChatThinkingDialectNone} {
		loaded := defaultConfig()
		loaded.RagModelConfig.RagChatThinkingDialect = dialect
		loaded.AgentConfig.AgentThinkingEnabled = true
		if err := loaded.RagModelConfig.Validate(); err != nil {
			t.Fatalf("dialect %q with thinking on: %v", dialect, err)
		}
		if err := loaded.AgentConfig.Validate(); err != nil {
			t.Fatalf("dialect %q with thinking on: %v", dialect, err)
		}
	}
}

// Thinking off is the default, and a deployment with it off is the
// configuration this product ships against.
func TestAgentThinkingOffIsAcceptedWithEveryDialect(t *testing.T) {
	for _, dialect := range []string{"", ChatThinkingDialectQwen, ChatThinkingDialectDeepSeek, ChatThinkingDialectNone} {
		loaded := defaultConfig()
		loaded.RagModelConfig.RagChatThinkingDialect = dialect
		loaded.AgentConfig.AgentThinkingEnabled = false
		if err := loaded.RagModelConfig.Validate(); err != nil {
			t.Fatalf("dialect %q with thinking off: %v", dialect, err)
		}
	}
}

// An unknown dialect is still refused. The switch has to name a dialect the
// client knows how to speak, or the request would go out without the field
// the provider expects.
func TestAnUnknownThinkingDialectIsStillRefused(t *testing.T) {
	loaded := defaultConfig()
	loaded.RagModelConfig.RagChatThinkingDialect = "klingon"
	loaded.AgentConfig.AgentThinkingEnabled = true
	err := loaded.RagModelConfig.Validate()
	if err == nil {
		t.Fatal("an unknown dialect was accepted")
	}
	if !strings.Contains(err.Error(), "unknown chat thinking dialect") {
		t.Fatalf("error = %v, want it to name the unknown dialect", err)
	}
}
