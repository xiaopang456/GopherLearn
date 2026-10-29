package aihelper

import (
	"context"
	"testing"
)

func TestThinkingExtraFieldsAreExplicitPerRequest(t *testing.T) {
	t.Setenv("OPENAI_THINKING_BUDGET", "2048")

	disabled := thinkingExtraFields(false)
	if got, ok := disabled["enable_thinking"].(bool); !ok || got {
		t.Fatalf("disabled fields = %#v", disabled)
	}
	if _, ok := disabled["thinking_budget"]; ok {
		t.Fatalf("disabled thinking unexpectedly carries a budget: %#v", disabled)
	}

	enabled := thinkingExtraFields(true)
	if got, ok := enabled["enable_thinking"].(bool); !ok || !got {
		t.Fatalf("enabled fields = %#v", enabled)
	}
	if got := enabled["thinking_budget"]; got != 2048 {
		t.Fatalf("thinking budget = %#v, want 2048", got)
	}
}

func TestThinkingChoiceIsRequestScoped(t *testing.T) {
	base := context.Background()
	if thinkingEnabled(base) {
		t.Fatal("thinking must default to disabled")
	}
	if !thinkingEnabled(WithThinking(base, true)) {
		t.Fatal("enabled request lost its thinking choice")
	}
	if thinkingEnabled(WithThinking(base, false)) {
		t.Fatal("disabled request gained thinking")
	}
}
