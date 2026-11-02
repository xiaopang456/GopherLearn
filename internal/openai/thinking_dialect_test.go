package openai

import (
	"context"
	"testing"
)

// The thinking switch is written in a dialect, not in one fixed shape. A
// provider that does not know the field ignores it, and an ignored field leaves
// that provider's own default in force - DeepSeek's default is thinking on,
// Qwen's is off. So the dialect decides which switch goes on the wire, and the
// operator's setting decides the state inside it.
func TestTheThinkingDialectDecidesWhichSwitchGoesOnTheWire(t *testing.T) {
	off := false
	on := true

	cases := []struct {
		name       string
		dialect    string
		thinking   *bool
		wantEnable any
		wantType   string
	}{
		{
			name:       "the default dialect keeps the qwen switch",
			dialect:    "",
			thinking:   nil,
			wantEnable: nil,
		},
		{
			name:       "the qwen dialect keeps the qwen switch",
			dialect:    ThinkingDialectQwen,
			thinking:   &off,
			wantEnable: false,
		},
		{
			name:     "the deepseek dialect writes its own switch and says nothing when unset",
			dialect:  ThinkingDialectDeepSeek,
			thinking: nil,
			wantType: "disabled",
		},
		{
			name:     "the deepseek dialect writes its own switch when thinking is on",
			dialect:  ThinkingDialectDeepSeek,
			thinking: &on,
			wantType: "enabled",
		},
		{
			name:     "the deepseek dialect writes its own switch when thinking is off",
			dialect:  ThinkingDialectDeepSeek,
			thinking: &off,
			wantType: "disabled",
		},
		{
			name:     "the none dialect writes no switch at all",
			dialect:  ThinkingDialectNone,
			thinking: &on,
			wantType: "",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			client, payload, _ := echoPayloadClient(t, Config{ThinkingDialect: testCase.dialect, EnableThinking: testCase.thinking})
			if _, err := client.Complete(context.Background(), oneUserRequest()); err != nil {
				t.Fatal(err)
			}

			value, present := (*payload)["enable_thinking"]
			switch {
			case testCase.wantEnable == nil && present:
				t.Fatalf("enable_thinking was sent although it was never configured: %v", value)
			case testCase.wantEnable != nil && !present:
				t.Fatalf("enable_thinking is missing, want %v", testCase.wantEnable)
			case testCase.wantEnable != nil && value != testCase.wantEnable:
				t.Fatalf("enable_thinking = %v, want %v", value, testCase.wantEnable)
			}

			control, present := (*payload)["thinking"]
			if testCase.wantType == "" {
				if present {
					t.Fatalf("the request carries a thinking control although the dialect writes none: %v", control)
				}
				return
			}
			if !present {
				t.Fatalf("the request carries no thinking control, want type %q", testCase.wantType)
			}
			fields, ok := control.(map[string]any)
			if !ok {
				t.Fatalf("thinking control = %#v", control)
			}
			if fields["type"] != testCase.wantType {
				t.Fatalf("thinking type = %#v, want %q", fields["type"], testCase.wantType)
			}
		})
	}
}

// A dialect name nobody supports has to stop startup rather than fall back to
// a default: a provider that ignores an unknown field would then answer with a
// thinking state the operator never chose.
func TestAnUnknownThinkingDialectIsRefused(t *testing.T) {
	if _, err := NormalizeThinkingDialect("qwen3"); err == nil {
		t.Fatal("an unknown dialect was accepted")
	}
	if dialect, err := NormalizeThinkingDialect(""); err != nil || dialect != ThinkingDialectQwen {
		t.Fatalf("unset dialect = %q/%v, want the qwen dialect", dialect, err)
	}
}
