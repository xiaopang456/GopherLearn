package aihelper

import (
	"context"
	"testing"

	"GopherAI/config"
)

func TestThinkingExtraFieldsAreExplicitPerRequest(t *testing.T) {
	t.Setenv("OPENAI_THINKING_BUDGET", "2048")

	disabled := ThinkingExtraFields(config.ChatThinkingDialectQwen, false)
	if got, ok := disabled["enable_thinking"].(bool); !ok || got {
		t.Fatalf("disabled fields = %#v", disabled)
	}
	// The budget travels whenever one is configured, whether or not thinking is
	// on. That is what the qwen dialect did before the dialect existed, and the
	// dialect has to keep doing it: a table that changes shape depending on the
	// state is a table nobody can predict from the configuration alone.
	if got := disabled["thinking_budget"]; got != 2048 {
		t.Fatalf("thinking budget = %#v, want 2048", got)
	}

	enabled := ThinkingExtraFields(config.ChatThinkingDialectQwen, true)
	if got, ok := enabled["enable_thinking"].(bool); !ok || !got {
		t.Fatalf("enabled fields = %#v", enabled)
	}
	if got := enabled["thinking_budget"]; got != 2048 {
		t.Fatalf("thinking budget = %#v, want 2048", got)
	}
}

// The qwen dialect is what every deployment used before the dialect existed,
// so it has to keep producing exactly the same table.
func TestTheQwenDialectIsTheDefaultAndKeepsItsShape(t *testing.T) {
	t.Setenv("OPENAI_THINKING_BUDGET", "2048")

	unset := ThinkingExtraFields("", false)
	if got, ok := unset["enable_thinking"].(bool); !ok || got {
		t.Fatalf("unset dialect fields = %#v", unset)
	}
	if len(unset) != 2 {
		t.Fatalf("unset dialect carries %d fields, want the qwen table: %#v", len(unset), unset)
	}

	qwen := ThinkingExtraFields(config.ChatThinkingDialectQwen, true)
	if len(qwen) != 2 {
		t.Fatalf("qwen fields = %#v", qwen)
	}
	if got := qwen["thinking_budget"]; got != 2048 {
		t.Fatalf("thinking budget = %#v, want 2048", got)
	}

	// Without a configured budget the table is the switch and nothing else.
	t.Setenv("OPENAI_THINKING_BUDGET", "")
	bare := ThinkingExtraFields("", false)
	if len(bare) != 1 {
		t.Fatalf("qwen table without a budget = %#v", bare)
	}
}

// The DeepSeek dialect writes the switch the way DeepSeek reads it, and sends
// nothing the provider does not know: a provider that ignores an unknown field
// would otherwise leave thinking on, which is DeepSeek's default.
func TestTheDeepSeekDialectWritesItsOwnSwitch(t *testing.T) {
	t.Setenv("OPENAI_THINKING_BUDGET", "2048")

	for _, enabled := range []bool{false, true} {
		fields := ThinkingExtraFields(config.ChatThinkingDialectDeepSeek, enabled)
		if len(fields) != 1 {
			t.Fatalf("deepseek fields = %#v", fields)
		}
		if _, ok := fields["enable_thinking"]; ok {
			t.Fatalf("deepseek fields carry the qwen switch: %#v", fields)
		}
		if _, ok := fields["thinking_budget"]; ok {
			t.Fatalf("deepseek fields carry a qwen budget: %#v", fields)
		}
		control, ok := fields["thinking"].(map[string]any)
		if !ok {
			t.Fatalf("deepseek thinking control = %#v", fields["thinking"])
		}
		want := "disabled"
		if enabled {
			want = "enabled"
		}
		if got := control["type"]; got != want {
			t.Fatalf("thinking type = %#v, want %q", got, want)
		}
	}
}

func TestTheNoneDialectSendsNoThinkingSwitch(t *testing.T) {
	fields := ThinkingExtraFields(config.ChatThinkingDialectNone, true)
	if len(fields) != 0 {
		t.Fatalf("none fields = %#v", fields)
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
