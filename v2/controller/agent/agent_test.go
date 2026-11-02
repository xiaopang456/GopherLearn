package agent

import (
	"GopherAI/common/code"
	serviceagent "GopherAI/service/agent"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestAgentErrorCodeMapsRunStorageFailureToServerBusy(t *testing.T) {
	if got := agentErrorCode(serviceagent.ErrRunTemporarilyUnavailable); got != code.CodeServerBusy {
		t.Fatalf("code=%d, want %d", got, code.CodeServerBusy)
	}
}

func TestRunStreamInvalidRequestEmitsTypedFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/run-stream", RunStream)
	req := httptest.NewRequest(http.MethodPost, "/run-stream", strings.NewReader(`{"question":"missing session"}`))
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)

	if contentType := response.Header().Get("Content-Type"); !strings.Contains(contentType, "text/event-stream") {
		t.Fatalf("content type = %q", contentType)
	}
	body := response.Body.String()
	if strings.Count(body, "data: ") != 1 {
		t.Fatalf("body = %q", body)
	}
	var event serviceagent.Event
	payload := strings.TrimSuffix(strings.TrimPrefix(body, "data: "), "\n\n")
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		t.Fatal(err)
	}
	if event.Type != serviceagent.EventRunFailed || event.TerminationReason != "invalid_request" {
		t.Fatalf("event = %#v", event)
	}
}

func TestRunRejectsInvalidRequestBeforeServiceLookup(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/run", Run)
	req := httptest.NewRequest(http.MethodPost, "/run", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"status_code":2001`) {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
}

// The thinking choice has three states on the wire, and a request that says
// nothing must stay different from one that says "no": the deployment's own
// setting applies to the first and not to the second.
func TestRunRequestKeepsThreeThinkingStates(t *testing.T) {
	for _, testCase := range []struct {
		body string
		want *bool
	}{
		{`{"sessionId":"s","question":"q","requestId":"r"}`, nil},
		{`{"sessionId":"s","question":"q","requestId":"r","enableThinking":true}`, boolPointer(true)},
		{`{"sessionId":"s","question":"q","requestId":"r","enableThinking":false}`, boolPointer(false)},
	} {
		request := new(RunRequest)
		if err := json.Unmarshal([]byte(testCase.body), request); err != nil {
			t.Fatal(err)
		}
		switch {
		case testCase.want == nil && request.EnableThinking != nil:
			t.Fatalf("absent field became %v", *request.EnableThinking)
		case testCase.want != nil && request.EnableThinking == nil:
			t.Fatalf("stated field was dropped: %s", testCase.body)
		case testCase.want != nil && *request.EnableThinking != *testCase.want:
			t.Fatalf("stated field became %v", *request.EnableThinking)
		}
	}
}

// A value that is not a boolean is not a choice this server can make sense
// of, so the turn is refused before the service is asked for anything.
func TestRunRejectsAThinkingValueThatIsNotABoolean(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/run", Run)
	req := httptest.NewRequest(http.MethodPost, "/run", strings.NewReader(`{"sessionId":"s","question":"q","requestId":"r","enableThinking":"yes"}`))
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)

	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"status_code":2001`) {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
}

func boolPointer(value bool) *bool { return &value }
