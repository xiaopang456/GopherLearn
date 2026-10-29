package hybrid

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"GopherAI/config"
)

// compatUnauthorizedHandler builds the compatibility router on its own. An
// unauthenticated request is refused by the authentication middleware before
// any handler runs, so the whole application is not needed to observe the
// answer that reaches the caller.
func compatUnauthorizedHandler(t *testing.T) http.Handler {
	t.Helper()
	settings := testSettings(t)
	if err := config.Use(settings.Config); err != nil {
		t.Fatal(err)
	}
	return newHTTPHandler(nil, nil, nil)
}

// TestCompatUnauthorizedAnswersStandardEnvelope locks the compatibility
// contract: an unauthenticated request to any compatibility route answers 401
// with the standard {data, error} envelope and never with an empty body.
func TestCompatUnauthorizedAnswersStandardEnvelope(t *testing.T) {
	handler := compatUnauthorizedHandler(t)
	for _, path := range []string{
		"/api/v1/me",
		"/api/v1/sessions",
		"/api/v1/sessions/x",
		"/api/v1/sessions/x/messages",
		"/api/v1/sessions/x/stream",
		"/api/v1/auth/unknown",
		"/api/v1/knowledge/documents/x/source",
	} {
		for _, token := range []string{"", "not-a-token"} {
			request := httptest.NewRequest(http.MethodGet, path, nil)
			if token != "" {
				request.Header.Set("Authorization", "Bearer "+token)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)

			if response.Code != http.StatusUnauthorized {
				t.Fatalf("%s token=%q: status = %d, want %d; body = %q", path, token, response.Code, http.StatusUnauthorized, response.Body.String())
			}
			var envelope struct {
				Data  any `json:"data"`
				Error *struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
				t.Fatalf("%s token=%q: decode: %v body=%q", path, token, err, response.Body.String())
			}
			if envelope.Data != nil {
				t.Fatalf("%s token=%q: data = %v, want null", path, token, envelope.Data)
			}
			if envelope.Error == nil || envelope.Error.Code != "unauthorized" || envelope.Error.Message != "authentication is required" {
				t.Fatalf("%s token=%q: error = %+v", path, token, envelope.Error)
			}
		}
	}
}

// TestCompatErrorGivesEnvelopeToBareUnauthorized covers a native refusal that
// aborts with a bare 401 and no body of its own: the compatibility layer still
// answers with the standard envelope.
func TestCompatErrorGivesEnvelopeToBareUnauthorized(t *testing.T) {
	captured := &authCapture{header: make(http.Header)}
	captured.WriteHeader(http.StatusUnauthorized)

	status, key, message, ok := compatError(captured)
	if !ok {
		t.Fatal("a bare unauthorized refusal must still be answered")
	}
	if status != http.StatusUnauthorized || key != "unauthorized" || message != "authentication is required" {
		t.Fatalf("status = %d key = %q message = %q", status, key, message)
	}
}

// TestCompatErrorKeepsNativeServerFailure keeps the answer for a native
// failure that is not an authentication refusal.
func TestCompatErrorKeepsNativeServerFailure(t *testing.T) {
	captured := &authCapture{header: make(http.Header)}
	captured.WriteHeader(http.StatusServiceUnavailable)
	captured.Write([]byte(`{"status_code":4001,"status_msg":"service busy"}`))

	status, key, message, ok := compatError(captured)
	if !ok {
		t.Fatal("a native failure must still be answered")
	}
	if status != http.StatusServiceUnavailable || key != "internal_error" || message != "internal server error" {
		t.Fatalf("status = %d key = %q message = %q", status, key, message)
	}
}

// TestCompatErrorLeavesSuccessfulAnswersAlone keeps the compatibility layer
// silent when the route answered on its own.
func TestCompatErrorLeavesSuccessfulAnswersAlone(t *testing.T) {
	captured := &authCapture{header: make(http.Header)}
	if _, _, _, ok := compatError(captured); ok {
		t.Fatal("a route that answered on its own must not be answered twice")
	}
}
