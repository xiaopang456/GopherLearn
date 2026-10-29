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
	if status != 200 || sse != golden.SSE {
		t.Fatalf("new ordinary SSE golden: %d %q", status, sse)
	}
}
