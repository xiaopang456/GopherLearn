package agent

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSSEEventSinkPreservesSpecialContentInOneFrame(t *testing.T) {
	recorder := httptest.NewRecorder()
	sink, err := NewSSEEventSink(recorder)
	if err != nil {
		t.Fatal(err)
	}
	content := "first line\n{\"kind\":\"answer: value\"}\n引号 \" 与冒号:"
	if err := sink.Send(context.Background(), Event{Type: EventAnswerDelta, RunID: "run-1", Content: content}); err != nil {
		t.Fatal(err)
	}
	body := recorder.Body.String()
	if strings.Count(body, "data: ") != 1 || strings.Count(body, "\n\n") != 1 {
		t.Fatalf("SSE frame was split: %q", body)
	}
	var decoded Event
	payload := strings.TrimSuffix(strings.TrimPrefix(body, "data: "), "\n\n")
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Content != content || decoded.Type != EventAnswerDelta {
		t.Fatalf("decoded event = %#v", decoded)
	}
}
