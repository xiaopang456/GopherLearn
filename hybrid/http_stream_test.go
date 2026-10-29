package hybrid

import (
	"GopherAI/model"
	"bufio"
	"context"
	"errors"
	"github.com/xiaopang456/GopherLearn/conversation"
	"gorm.io/gorm"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type blockedHTTPModel struct {
	base    *ordinaryModel
	release chan struct{}
	entered chan struct{}
}

func (m *blockedHTTPModel) Complete(ctx context.Context, r conversation.Request) (conversation.Completion, error) {
	return m.base.Complete(ctx, r)
}
func (m *blockedHTTPModel) Stream(ctx context.Context, r conversation.Request, emit conversation.DeltaHandler) (conversation.Completion, error) {
	answer, err := m.base.Complete(ctx, r)
	if err != nil {
		return answer, err
	}
	if err := emit(answer.Content); err != nil {
		return conversation.Completion{}, err
	}
	close(m.entered)
	select {
	case <-m.release:
		return answer, nil
	case <-ctx.Done():
		return conversation.Completion{}, ctx.Err()
	}
}
func TestHTTPStreamFlushesBeforeModelCompletion(t *testing.T) {
	var blocked *blockedHTTPModel
	app, _, _, _, token, _ := httpFixtureWithModel(t, func(m *ordinaryModel) conversation.Model {
		blocked = &blockedHTTPModel{base: m, release: make(chan struct{}), entered: make(chan struct{})}
		return blocked
	})
	sid := createHTTP(t, app.Handler, token, "flush")
	server := httptest.NewServer(app.Handler)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", server.URL+"/api/v1/sessions/"+sid+"/stream", strings.NewReader(`{"content":"before-complete"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("stream HTTP %d", response.StatusCode)
	}
	first := readUntilDelta(t, response)
	if !strings.Contains(first, "回答：before-complete") {
		t.Fatalf("delta not flushed: %s", first)
	}
	select {
	case <-blocked.entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	history := dataHTTP(t, app.Handler, "GET", "/api/v1/sessions/"+sid+"/messages", "", token)["items"].([]any)
	if len(history) != 1 {
		t.Fatalf("model completion did not remain blocked: %v", history)
	}
	close(blocked.release)
	rest, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rest), "event: message.completed") {
		t.Fatalf("completion missing: %s", rest)
	}
	history = dataHTTP(t, app.Handler, "GET", "/api/v1/sessions/"+sid+"/messages", "", token)["items"].([]any)
	if len(history) != 2 || history[1].(map[string]any)["content"] != "回答：before-complete" {
		t.Fatalf("completed record missing %v", history)
	}
}

type failedStreamWriter struct {
	*httptest.ResponseRecorder
	failEvent string
	flushes   int
}

func (w *failedStreamWriter) Write(p []byte) (int, error) {
	if strings.Contains(string(p), "event: "+w.failEvent+"\n") {
		return 0, errors.New("synthetic HTTP write failure")
	}
	return w.ResponseRecorder.Write(p)
}
func (w *failedStreamWriter) WriteString(s string) (int, error) { return w.Write([]byte(s)) }
func (w *failedStreamWriter) Flush()                            { w.flushes++; w.ResponseRecorder.Flush() }

type noFlushWriter struct{ recorder *httptest.ResponseRecorder }

func (w *noFlushWriter) Header() http.Header         { return w.recorder.Header() }
func (w *noFlushWriter) WriteHeader(s int)           { w.recorder.WriteHeader(s) }
func (w *noFlushWriter) Write(p []byte) (int, error) { return w.recorder.Write(p) }

func TestHTTPFailureEventsAndPersistenceUseOriginalStreamContract(t *testing.T) {
	for _, name := range []string{"model-send", "model-stream", "user-store", "assistant-store", "touch-store", "cancel-model", "cancel-delta", "write-started", "write-delta", "write-completed", "no-real-flusher"} {
		t.Run(name, func(t *testing.T) {
			app, db, m, _, token, _ := httpFixture(t)
			sid := createHTTP(t, app.Handler, token, name)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			wantRows, wantCalls := 1, 1
			if strings.HasPrefix(name, "model-") {
				m.fail = true
			}
			if name == "cancel-model" {
				m.cancel = cancel
			}
			switch name {
			case "user-store", "assistant-store":
				if err := db.Callback().Create().Before("gorm:create").Register("test:http-store-error", func(tx *gorm.DB) {
					if row, ok := tx.Statement.Dest.(*model.Message); ok && (name == "user-store" && row.IsUser || name == "assistant-store" && !row.IsUser) {
						tx.AddError(errors.New("synthetic SQL failure"))
					}
				}); err != nil {
					t.Fatal(err)
				}
				if name == "user-store" {
					wantRows = 0
					wantCalls = 0
				}
			case "touch-store":
				if err := db.Callback().Update().Before("gorm:update").Register("test:http-touch-error", func(tx *gorm.DB) {
					if tx.Statement.Table == "sessions" {
						tx.AddError(errors.New("synthetic touch failure"))
					}
				}); err != nil {
					t.Fatal(err)
				}
				wantRows = 0
				wantCalls = 0
			case "write-started":
				wantCalls = 0
			case "write-completed":
				wantRows = 2
			case "no-real-flusher":
				wantRows = 0
				wantCalls = 0
			}
			path := "/api/v1/sessions/" + sid + "/stream"
			if name == "model-send" {
				path = "/api/v1/sessions/" + sid + "/messages"
			}
			req := httptest.NewRequest("POST", path, strings.NewReader(`{"content":"failure"}`)).WithContext(ctx)
			req.Header.Set("Authorization", "Bearer "+token)
			recorder := httptest.NewRecorder()
			var writer http.ResponseWriter = recorder
			if strings.HasPrefix(name, "write-") {
				writer = &failedStreamWriter{ResponseRecorder: recorder, failEvent: "message." + strings.TrimPrefix(name, "write-")}
			}
			if name == "cancel-delta" {
				writer = &cancelDeltaWriter{ResponseRecorder: recorder, cancel: cancel}
			}
			if name == "no-real-flusher" {
				writer = &noFlushWriter{recorder: recorder}
			}
			app.Handler.ServeHTTP(writer, req)
			history := dataHTTP(t, app.Handler, "GET", "/api/v1/sessions/"+sid+"/messages", "", token)["items"].([]any)
			if len(history) != wantRows || len(m.all()) != wantCalls {
				t.Fatalf("HTTP facts rows=%d calls=%d want %d/%d: %s", len(history), len(m.all()), wantRows, wantCalls, recorder.Body.String())
			}
			body := recorder.Body.String()
			if strings.HasPrefix(name, "cancel-") {
				if strings.Contains(body, "message.completed") || strings.Contains(body, "message.failed") {
					t.Fatalf("cancel fabricated terminal: %s", body)
				}
			} else if name != "model-send" && name != "no-real-flusher" {
				if !strings.Contains(body, "event: message.failed") {
					t.Fatalf("original failure terminal missing: %s", body)
				}
			}
			if name == "no-real-flusher" && (recorder.Code != 500 || recorder.Flushed) {
				t.Fatal("Gin wrapper pretended to support Flush")
			}
		})
	}
}

type cancelDeltaWriter struct {
	*httptest.ResponseRecorder
	cancel context.CancelFunc
}

func (w *cancelDeltaWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseRecorder.Write(p)
	if strings.Contains(string(p), "event: message.delta\n") {
		w.cancel()
	}
	return n, err
}
func (w *cancelDeltaWriter) WriteString(s string) (int, error) { return w.Write([]byte(s)) }

func readUntilDelta(t *testing.T, response *http.Response) string {
	t.Helper()
	reader := bufio.NewReader(response.Body)
	var out strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		out.WriteString(line)
		if strings.HasPrefix(line, "data:") && strings.Contains(out.String(), "event: message.delta") {
			return out.String()
		}
	}
}
