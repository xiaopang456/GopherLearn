package hybrid

import (
	"fmt"
	"strings"
	"testing"
)

// The two retired ordinary-chat fields are read by hand now, so they have to keep the
// strictness the request decoder gave them for free. A client that misspells `enabled`
// must be refused rather than answered as if it had asked for nothing: the request said
// something about the learner's notes, and the answer would pretend it did not.
func TestRetiredNestedFieldsStayStrictOnBothEndpoints(t *testing.T) {
	app, _, ordinary, agent, token, _ := httpFixture(t)
	session := createSession(t, app, token, "nested-strict", "chat")

	for _, tc := range []struct {
		name   string
		path   string
		body   string
		status int
		calls  int
	}{
		{
			name: "misspelt enabled", path: "/messages",
			body:   `{"content":"你好","knowledge":{"enabled_typo":true}}`,
			status: 400, calls: 0,
		},
		{
			name: "unknown key beside a known one", path: "/messages",
			body:   `{"content":"你好","knowledge":{"enabled":false,"collections":["c-1"]}}`,
			status: 400, calls: 0,
		},
		{
			name: "stream_steps of the wrong type", path: "/messages",
			body:   `{"content":"你好","stream_steps":"yes"}`,
			status: 400, calls: 0,
		},
		{
			name: "the same misspelling on the stream", path: "/stream",
			body:   `{"content":"你好","knowledge":{"enabled_typo":true}}`,
			status: 400, calls: 0,
		},
		{
			// The control: inert values were accepted before this change and stay
			// accepted, so the strictness above is not a new rule wearing an old name.
			name: "inert values are still accepted", path: "/messages",
			body:   `{"content":"你好","knowledge":{"enabled":false},"stream_steps":false}`,
			status: 200, calls: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := agent.CallCount()
			status, envelope, raw := requestHTTP(t, app.Handler, "POST", "/api/v1/sessions/"+session+tc.path, tc.body, token)
			if status != tc.status {
				t.Fatalf("%s: status %d, want %d (%s)", tc.name, status, tc.status, raw)
			}
			if tc.status == 400 {
				if code := fmt.Sprint(envelope["error"].(map[string]any)["code"]); !strings.HasPrefix(code, "invalid_request") {
					t.Fatalf("%s: error code %s", tc.name, code)
				}
			}
			if got := agent.CallCount() - before; got != tc.calls {
				t.Fatalf("%s: the V2 model was asked %d times, want %d", tc.name, got, tc.calls)
			}
			if len(ordinary.all()) != 0 {
				t.Fatalf("%s: the retired chat path answered instead", tc.name)
			}
		})
	}
}
