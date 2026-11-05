package hybrid

import (
	"encoding/json"
	"os"
	"testing"
)

func TestHTTPCompatibilityGoldenDoesNotReplaceOriginalGolden(t *testing.T) {
	bytes, err := os.ReadFile("testdata/http_compatibility.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden struct {
		Errors []struct {
			Method   string
			Path     string
			Body     string
			Token    string
			Status   int
			Response string
		} `json:"errors"`
		SSE string `json:"ordinary_sse"`
	}
	if err := json.Unmarshal(bytes, &golden); err != nil {
		t.Fatal(err)
	}
	app, _, _, _, token, _ := httpFixture(t)
	for _, tc := range golden.Errors {
		auth := tc.Token
		if auth == "authenticated" {
			auth = token
		}
		status, _, body := requestHTTP(t, app.Handler, tc.Method, tc.Path, tc.Body, auth)
		if status != tc.Status || body != tc.Response {
			t.Fatalf("new compatibility golden %s: %d %q", tc.Path, status, body)
		}
	}
	sid := createHTTP(t, app.Handler, token, "golden")
	status, _, sse := requestHTTP(t, app.Handler, "POST", "/api/v1/sessions/"+sid+"/stream", `{"content":"golden"}`, token)
	if status != 200 {
		t.Fatalf("ordinary stream status: %d %q", status, sse)
	}
	// The identity is checked before it is normalized away, because normalizing a
	// stream whose frames disagreed about which run they belong to would hide the
	// one thing this turn has to get right.
	if runID, problem := sseRunIdentity(sse); problem != "" {
		t.Fatalf("ordinary stream identity: %s\n%s", problem, sse)
	} else if runID == "" {
		t.Fatalf("ordinary stream carried no run identity\n%s", sse)
	}
	// original_v2 is compared as structure rather than as numbers. It carries the
	// whole RunResult, contextBudget included, and the assembly those numbers come
	// from is still being changed by other work; what this golden owes a reader is
	// that a conversation turn publishes the same fields an Agent turn does. The
	// values belong to the budget tests, which can say what a number should be.
	if normalized := normalizeSSE(t, sse); normalized != golden.SSE {
		t.Fatalf("new ordinary SSE golden: %d\nnormalized actual: %q", status, normalized)
	}
}
