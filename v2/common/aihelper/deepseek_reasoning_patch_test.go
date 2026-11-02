package aihelper

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"GopherAI/config"
)

// The patch adds one field to one turn and leaves every other byte of the
// request alone. These cases pin that, one rule at a time.
func TestTheReasoningContentPatchTouchesOnlyTheTurnsThatNeedIt(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "an assistant turn with tool calls and no field gets an empty one",
			body: `{"messages":[{"role":"assistant","content":"hi","tool_calls":[{"type":"function"}]}],"model":"m"}`,
			want: `{"messages":[{"role":"assistant","content":"hi","tool_calls":[{"type":"function"}],"reasoning_content":""}],"model":"m"}`,
		},
		{
			name: "a turn that thought keeps its own content",
			body: `{"messages":[{"role":"assistant","tool_calls":[{"type":"function"}],"reasoning_content":"I looked it up"}],"model":"m"}`,
			want: `{"messages":[{"role":"assistant","tool_calls":[{"type":"function"}],"reasoning_content":"I looked it up"}],"model":"m"}`,
		},
		{
			name: "a turn whose field is null keeps its null",
			body: `{"messages":[{"role":"assistant","tool_calls":[{"type":"function"}],"reasoning_content":null}],"model":"m"}`,
			want: `{"messages":[{"role":"assistant","tool_calls":[{"type":"function"}],"reasoning_content":null}],"model":"m"}`,
		},
		{
			name: "a turn whose field is an empty string keeps its empty string",
			body: `{"messages":[{"role":"assistant","tool_calls":[{"type":"function"}],"reasoning_content":""}],"model":"m"}`,
			want: `{"messages":[{"role":"assistant","tool_calls":[{"type":"function"}],"reasoning_content":""}],"model":"m"}`,
		},
		{
			name: "an assistant turn without tool calls is not a turn DeepSeek asks about",
			body: `{"messages":[{"role":"assistant","content":"hi"}],"model":"m"}`,
			want: `{"messages":[{"role":"assistant","content":"hi"}],"model":"m"}`,
		},
		{
			name: "a tool turn is not an assistant turn",
			body: `{"messages":[{"role":"tool","tool_call_id":"call_1","content":"result"}],"model":"m"}`,
			want: `{"messages":[{"role":"tool","tool_call_id":"call_1","content":"result"}],"model":"m"}`,
		},
		{
			name: "only the turn that needs the field gets it",
			body: `{"messages":[{"role":"user","content":"q"},` +
				`{"role":"assistant","tool_calls":[{"type":"function"}]},` +
				`{"role":"tool","tool_call_id":"call_1","content":"r"}],"model":"m"}`,
			want: `{"messages":[{"role":"user","content":"q"},` +
				`{"role":"assistant","tool_calls":[{"type":"function"}],"reasoning_content":""},` +
				`{"role":"tool","tool_call_id":"call_1","content":"r"}],"model":"m"}`,
		},
		{
			name: "a null tool_calls value is not a set of tool calls",
			body: `{"messages":[{"role":"assistant","tool_calls":null}],"model":"m"}`,
			want: `{"messages":[{"role":"assistant","tool_calls":null}],"model":"m"}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			patched, err := patchMissingReasoningContent([]byte(tc.body))
			if err != nil {
				t.Fatalf("patch: %v", err)
			}
			if string(patched) != tc.want {
				t.Fatalf("patched body\n got: %s\nwant: %s", patched, tc.want)
			}
		})
	}
}

// The patch edits bytes. It must not re-encode the request, because that
// would change the number formats and the field order of values that have
// nothing to do with the field being added.
func TestTheReasoningContentPatchLeavesTheRestOfTheRequestAlone(t *testing.T) {
	body := `{"model":"m","max_tokens":1.50,"temperature":1e3,"seed":0.30000000000000004,` +
		`"messages":[{"role":"user","content":"a \"quoted\" brace } and a backslash \\"},` +
		`{"role":"assistant","tool_calls":[{"type":"function","function":{"arguments":"{\"query\":\"go\"}"}}]}],"stream":false}`

	want := `{"model":"m","max_tokens":1.50,"temperature":1e3,"seed":0.30000000000000004,` +
		`"messages":[{"role":"user","content":"a \"quoted\" brace } and a backslash \\"},` +
		`{"role":"assistant","tool_calls":[{"type":"function","function":{"arguments":"{\"query\":\"go\"}"}}],"reasoning_content":""}],"stream":false}`

	patched, err := patchMissingReasoningContent([]byte(body))
	if err != nil {
		t.Fatalf("patch: %v", err)
	}
	if string(patched) != want {
		t.Fatalf("patched body\n got: %s\nwant: %s", patched, want)
	}
}

// A body the patch cannot read goes out unchanged, and the failure is not
// turned into a failed request.
func TestTheReasoningContentPatchSurvivesABodyItCannotRead(t *testing.T) {
	bodies := []string{
		"",
		"not json at all",
		"[]",
		`{"messages":"not an array"}`,
		`{"messages":[{"role":123}]}`,
		`{"messages":[{"role":"assistant","tool_calls":[{"type":"function"}]}`,
	}
	for _, body := range bodies {
		t.Run(body, func(t *testing.T) {
			patched, err := deepSeekReasoningPatch(context.Background(), nil, []byte(body))
			if err != nil {
				t.Fatalf("the patch failed the request: %v", err)
			}
			if string(patched) != body {
				t.Fatalf("patched body\n got: %s\nwant: %s", patched, body)
			}
		})
	}
}

// A body the patch cannot read is also a body it does not touch: the failure
// stops the patch, it does not half-apply it.
func TestTheReasoningContentPatchAppliesNothingWhenItCannotReadTheBody(t *testing.T) {
	body := `{"messages":[{"role":"assistant","tool_calls":[{"type":"function"}]},{"role":123}]}`
	patched, err := patchMissingReasoningContent([]byte(body))
	if err == nil {
		t.Fatalf("expected the unreadable body to be reported")
	}
	if string(patched) != body {
		t.Fatalf("a failed patch must hand back the original body\n got: %s\nwant: %s", patched, body)
	}
}

// The way out is checked as well as the way in. A patch that would leave a
// body that is not valid JSON any more is thrown away, and the original body
// is what goes out.
func TestThePatchIsDiscardedWhenItWouldProduceInvalidJson(t *testing.T) {
	body := `{"messages":[{"role":"assistant","tool_calls":[{"type":"function"}]}],"model":"m"}`
	// The fragment is missing its closing quote, so the body it produces is
	// not JSON.
	patched, err := patchMissingReasoningContentWith([]byte(body), []byte(`,"reasoning_content:"`))
	if !errors.Is(err, errInvalidPatchedBody) {
		t.Fatalf("error = %v, want %v", err, errInvalidPatchedBody)
	}
	if string(patched) != body {
		t.Fatalf("the broken patch was not discarded\n got: %s\nwant: %s", patched, body)
	}
}

// The same thing end to end: a body the patch cannot make valid is sent
// unchanged, and the caller never sees an error.
func TestARequestThatWouldBreakIsSentUnchanged(t *testing.T) {
	// The number has a leading zero, which is not valid JSON. The patch can
	// still read the body, and the patched body would still not be JSON, so
	// it is thrown away.
	body := `{"messages":[{"role":"assistant","tool_calls":[{"type":"function"}],"n":01}],"model":"m"}`
	patched, err := deepSeekReasoningPatch(context.Background(), nil, []byte(body))
	if err != nil {
		t.Fatalf("the patch failed the request: %v", err)
	}
	if string(patched) != body {
		t.Fatalf("the request went out changed\n got: %s\nwant: %s", patched, body)
	}
}

// The patch is registered where DeepSeek needs it and nowhere else. A
// request in any other shape keeps the request path it had.
func TestTheReasoningContentPatchIsRegisteredOnlyForDeepSeekWithThinkingOn(t *testing.T) {
	cases := []struct {
		dialect string
		enabled bool
		want    bool
	}{
		{config.ChatThinkingDialectDeepSeek, true, true},
		{config.ChatThinkingDialectDeepSeek, false, false},
		{config.ChatThinkingDialectQwen, true, false},
		{config.ChatThinkingDialectQwen, false, false},
		{config.ChatThinkingDialectNone, true, false},
		{config.ChatThinkingDialectNone, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.dialect, func(t *testing.T) {
			if got := needsDeepSeekReasoningPatch(tc.dialect, tc.enabled); got != tc.want {
				t.Fatalf("needsDeepSeekReasoningPatch(%q, %v) = %v, want %v", tc.dialect, tc.enabled, got, tc.want)
			}
			// The patch rides along with the thinking switch, so it is one
			// more option exactly when the patch is needed.
			wantOptions := 1
			if tc.want {
				wantOptions = 2
			}
			if got := len(ThinkingModelOptions(tc.dialect, tc.enabled)); got != wantOptions {
				t.Fatalf("ThinkingModelOptions(%q, %v) returned %d options, want %d", tc.dialect, tc.enabled, got, wantOptions)
			}
		})
	}
}

// The turn the patch is about: an assistant turn that asked for a tool and
// never thought. DeepSeek wants the field on it, and the value has to be
// there rather than missing.
func TestThePatchedTurnCarriesTheFieldDeepSeekAsksFor(t *testing.T) {
	body := `{"messages":[{"role":"assistant","tool_calls":[{"type":"function"}]}]}`
	patched, err := patchMissingReasoningContent([]byte(body))
	if err != nil {
		t.Fatalf("patch: %v", err)
	}
	var request struct {
		Messages []struct {
			Role             string  `json:"role"`
			ReasoningContent *string `json:"reasoning_content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(patched, &request); err != nil {
		t.Fatalf("read the patched turn: %v", err)
	}
	if len(request.Messages) != 1 {
		t.Fatalf("the request body has %d turns, want 1", len(request.Messages))
	}
	field := request.Messages[0].ReasoningContent
	if field == nil {
		t.Fatalf("the patched turn has no reasoning_content field")
	}
	if *field != "" {
		t.Fatalf("reasoning content = %q, want an empty string", *field)
	}
}
