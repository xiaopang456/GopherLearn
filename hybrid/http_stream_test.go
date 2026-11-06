package hybrid

import (
	"GopherAI/model"
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"github.com/cloudwego/eino/schema"
	"gorm.io/gorm"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A turn with its knowledge switched off has no retrieved evidence to validate the
// answer against, so its text is published while the model writes it. This fixture
// releases its second fragment only on command, which is what makes "the first text
// arrived while generation was still running" a fact a test can observe rather than
// assert about the code.
func TestHTTPStreamPublishesACapabilityFreeTurnWhileItGenerates(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	app, _, agent, ordinary, token, _ := sseFixture(t, func(_ int, _ []*schema.Message) streamPlan {
		return streamPlan{parts: pieces("第一段：", "第二段"), entered: entered, release: release}
	})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	sid := createHTTP(t, app.Handler, token, "flush")
	server := httptest.NewServer(app.Handler)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", server.URL+"/api/v1/sessions/"+sid+"/stream", strings.NewReader(`{"content":"边生成边发"}`))
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
	frames := collectSSEFrames(response.Body)
	started := frames.next(t, 10*time.Second)
	if !strings.Contains(started, "event: message.started") {
		t.Fatalf("first frame was not the run starting: %s", started)
	}
	// The first fragment reached the wire, so the model is mid-answer...
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// ...and the client already has text, while the model has not been allowed to
	// finish. A turn that only published after generation ended would block here.
	early := frames.next(t, 10*time.Second)
	if !strings.Contains(early, "event: message.delta") {
		t.Fatalf("no content frame before the model finished: %s", early)
	}
	if !strings.Contains(early, `"content":"第一段："`) {
		t.Fatalf("the first content frame is not the text written so far: %s", early)
	}
	if strings.Contains(early, "第二段") {
		t.Fatalf("a frame carried text the model had not written yet: %s", early)
	}
	if !strings.Contains(early, `"publication":"model_stream"`) {
		t.Fatalf("a live frame is labelled as something it is not: %s", early)
	}
	// The model may now finish. Frames are snapshots, so the next one carries the whole
	// text written so far rather than the piece that just arrived - which is what lets a
	// client replace instead of append.
	go close(release)
	second := frames.next(t, 10*time.Second)
	if !strings.Contains(second, `"content":"第一段：第二段"`) {
		t.Fatalf("the second frame is a fragment instead of a snapshot: %s", second)
	}
	rest := frames.rest()
	if !strings.Contains(rest, "event: message.completed") {
		t.Fatalf("completion missing: %s", rest)
	}
	wire := started + early + second + rest
	if runID, problem := sseRunIdentity(wire); problem != "" {
		t.Fatalf("stream identity: %s\n%s", problem, wire)
	} else if runID == "" {
		t.Fatalf("the stream carried no run identity: %s", wire)
	}
	history := dataHTTP(t, app.Handler, "GET", "/api/v1/sessions/"+sid+"/messages", "", token)["items"].([]any)
	if len(history) != 2 || history[1].(map[string]any)["content"] != "第一段：第二段" {
		t.Fatalf("completed record missing %v", history)
	}
	if agent.CallCount() != 1 {
		t.Fatalf("a conversation turn generated %d times, want 1", agent.CallCount())
	}
	if calls := len(ordinary.all()); calls != 0 {
		t.Fatalf("a conversation turn reached the retired ordinary model %d times", calls)
	}
}

// A turn that kept its capability is the other half: its answer is held until the
// citations in it have been checked, so nothing before that point may carry text.
func TestHTTPStreamHoldsAKnowledgeTurnUntilItsCitationsAreChecked(t *testing.T) {
	app, _, _, _, token, _ := httpFixture(t)
	sid := createAgentHTTP(t, app, token, "held-turn", "Agent")
	server := httptest.NewServer(app.Handler)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	body, _ := json.Marshal(map[string]any{"content": "查一下资料", "requestId": "held-turn-run"})
	req, err := http.NewRequestWithContext(ctx, "POST", server.URL+"/api/v1/sessions/"+sid+"/stream", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	frames := collectSSEFrames(response.Body)
	started := frames.next(t, 10*time.Second)
	if !strings.Contains(started, "event: message.started") {
		t.Fatalf("first frame was not the run starting: %s", started)
	}
	rest := frames.rest()
	if !strings.Contains(rest, `"publication":"validated_snapshot"`) {
		t.Fatalf("a grounded turn published live text: %s", rest)
	}
	if !strings.Contains(rest, "event: message.completed") {
		t.Fatalf("completion missing: %s", rest)
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

// Each case injects one failure into the path a conversation turn now runs and asks three
// questions of it: what survived in the store, how many times the model was called, and
// what the client was told. They are answered per case rather than by one shared
// expectation, because the shared path commits the answer before this layer gets control
// back - a late failure can no longer un-write an earlier success, and pretending
// otherwise would be asserting a contract the code does not have.
//
// The retired ordinary model is asserted unused in every case: a turn that reached it
// would mean the move back to two engines had happened without anyone noticing.
func TestHTTPFailureEventsAndPersistenceUseOriginalStreamContract(t *testing.T) {
	for _, name := range []string{"model-send", "model-stream", "user-store", "assistant-store", "touch-store", "cancel-model", "cancel-delta", "write-started", "write-delta", "write-completed", "no-real-flusher"} {
		t.Run(name, func(t *testing.T) {
			app, db, ordinary, agent, token, _ := httpFixture(t)
			sid := createHTTP(t, app.Handler, token, name)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			wantRows, wantGenerations := 1, 1
			wantTerminal := "failed"
			switch name {
			case "model-send", "model-stream":
				agent.setRespond(func(context.Context, int) (*schema.Message, error) {
					return nil, errors.New("synthetic model failure")
				})
				if name == "model-send" {
					// Not a stream: this endpoint answers with an error envelope.
					wantTerminal = ""
				}
			case "cancel-model":
				agent.setRespond(func(ctx context.Context, _ int) (*schema.Message, error) {
					cancel()
					return nil, ctx.Err()
				})
				wantTerminal = ""
			case "user-store", "assistant-store":
				if err := db.Callback().Create().Before("gorm:create").Register("test:http-store-error", func(tx *gorm.DB) {
					if row, ok := tx.Statement.Dest.(*model.Message); ok && (name == "user-store" && row.IsUser || name == "assistant-store" && !row.IsUser) {
						tx.AddError(errors.New("synthetic SQL failure"))
					}
				}); err != nil {
					t.Fatal(err)
				}
				if name == "user-store" {
					// The learner's own message could not be stored, so there is no turn
					// to generate for and nothing to roll back.
					wantRows, wantGenerations = 0, 0
				}
			case "touch-store":
				if err := db.Callback().Update().Before("gorm:update").Register("test:http-touch-error", func(tx *gorm.DB) {
					if tx.Statement.Table == "sessions" {
						tx.AddError(errors.New("synthetic touch failure"))
					}
				}); err != nil {
					t.Fatal(err)
				}
				// The recency touch runs after the answer is committed, so it can no
				// longer abort the turn the way the retired append's single transaction
				// did. It is logged and the turn completes: withholding an answer that
				// is already stored over a stale sort key would lose more than it saves.
				wantRows, wantTerminal = 2, "completed"
			case "cancel-delta":
				// The snapshot is published before the assistant row is written, so a
				// cancellation here leaves the learner's message and no answer.
				wantRows, wantTerminal = 1, ""
			case "write-started":
				// The first frame could not be written, so there is no wire left to carry
				// a failure frame either, and the run is cancelled before it generates.
				wantGenerations, wantTerminal = 0, ""
			case "write-delta":
				wantRows, wantTerminal = 1, ""
			case "write-completed":
				// Both rows are committed before the completion frame is written, so a
				// broken wire costs the client the frame and not the answer.
				wantRows, wantTerminal = 2, ""
			case "no-real-flusher":
				wantRows, wantGenerations, wantTerminal = 0, 0, ""
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
			if len(history) != wantRows || agent.CallCount() != wantGenerations {
				t.Fatalf("HTTP facts rows=%d generations=%d want %d/%d: %s", len(history), agent.CallCount(), wantRows, wantGenerations, recorder.Body.String())
			}
			if calls := len(ordinary.all()); calls != 0 {
				t.Fatalf("a conversation turn reached the retired ordinary model %d times", calls)
			}
			body := recorder.Body.String()
			switch wantTerminal {
			case "":
				if strings.Contains(body, "message.completed") || strings.Contains(body, "message.failed") {
					t.Fatalf("a terminal frame was fabricated for a turn that could not finish: %s", body)
				}
			case "failed":
				if !strings.Contains(body, "event: message.failed") {
					t.Fatalf("failure terminal missing: %s", body)
				}
			case "completed":
				if !strings.Contains(body, "event: message.completed") || strings.Contains(body, "event: message.failed") {
					t.Fatalf("a committed answer was not reported as completed: %s", body)
				}
			}
			if name == "model-send" && recorder.Code == 200 {
				t.Fatalf("a failed turn was reported as a success: %s", body)
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

// sseFrames reads one response body frame by frame on its own goroutine, so a test can
// ask what has arrived so far, how long it waited, and whether nothing arrived at all.
//
// One reader owns the body. Two bufio.Readers over one stream would each swallow bytes
// the other never sees, which is how a test ends up asserting about a frame it read
// half of.
type sseFrames struct{ frames chan string }

func collectSSEFrames(body io.Reader) *sseFrames {
	collected := &sseFrames{frames: make(chan string)}
	go func() {
		defer close(collected.frames)
		reader := bufio.NewReader(body)
		var current strings.Builder
		for {
			line, err := reader.ReadString('\n')
			current.WriteString(line)
			if line == "\n" {
				collected.frames <- current.String()
				current.Reset()
			}
			if err != nil {
				if current.Len() > 0 {
					collected.frames <- current.String()
				}
				return
			}
		}
	}()
	return collected
}

func (c *sseFrames) next(t *testing.T, within time.Duration) string {
	t.Helper()
	select {
	case frame, ok := <-c.frames:
		if !ok {
			t.Fatal("the stream ended before the frame this test was waiting for")
		}
		return frame
	case <-time.After(within):
		t.Fatalf("no frame arrived within %s", within)
		return ""
	}
}

// nothingWithin is how a test says "this frame cannot have arrived yet" rather than the
// much weaker "it had not arrived when I looked".
func (c *sseFrames) nothingWithin(t *testing.T, within time.Duration) {
	t.Helper()
	select {
	case frame := <-c.frames:
		t.Fatalf("a frame arrived while generation was still blocked: %s", frame)
	case <-time.After(within):
	}
}

func (c *sseFrames) rest() string {
	var out strings.Builder
	for frame := range c.frames {
		out.WriteString(frame)
	}
	return out.String()
}

// drain keeps the reader goroutine from being left blocked on a send by a test that
// stops listening, which is what a test does when it deliberately leaves a stream
// unfinished.
func (c *sseFrames) drain() {
	for range c.frames {
	}
}
