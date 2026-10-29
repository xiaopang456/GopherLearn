package httpapi

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/repository"
	"github.com/xiaopang456/GopherLearn/internal/responder"
	"github.com/xiaopang456/GopherLearn/internal/security"
	"github.com/xiaopang456/GopherLearn/internal/service"
)

// flushProbeResponder samples the recorder's flush counter between two emitted
// deltas, from the very goroutine that writes the response. If any layer
// buffered the stream the counter would not have advanced when the first delta
// was handed back, and the sampled value shows it.
type flushProbeResponder struct {
	flushes func() int
	seen    *[]int
}

func (p flushProbeResponder) Respond(_ context.Context, _ responder.Request) (responder.Answer, error) {
	return responder.Answer{Content: "answer"}, nil
}

func (p flushProbeResponder) Stream(_ context.Context, _ responder.Request, emit func(string) error) (responder.Answer, error) {
	if err := emit("first-half"); err != nil {
		return responder.Answer{}, err
	}
	*p.seen = append(*p.seen, p.flushes())
	if err := emit("second-half"); err != nil {
		return responder.Answer{}, err
	}
	*p.seen = append(*p.seen, p.flushes())
	return responder.Answer{Content: "first-halfsecond-half"}, nil
}

// gateResponder blocks after the first delta until a reader confirms the bytes
// arrived, which is what separates a streamed response from one held until the
// handler returned.
type gateResponder struct {
	release  chan struct{}
	deadline time.Duration
}

func (p gateResponder) Respond(_ context.Context, _ responder.Request) (responder.Answer, error) {
	return responder.Answer{Content: "first-halfsecond-half"}, nil
}

func (p gateResponder) Stream(ctx context.Context, _ responder.Request, emit func(string) error) (responder.Answer, error) {
	if err := emit("first-half"); err != nil {
		return responder.Answer{}, err
	}
	select {
	case <-p.release:
	case <-time.After(p.deadline):
		return responder.Answer{}, errors.New("stream was buffered: the first delta never reached the reader")
	case <-ctx.Done():
		return responder.Answer{}, ctx.Err()
	}
	if err := emit("second-half"); err != nil {
		return responder.Answer{}, err
	}
	return responder.Answer{Content: "first-halfsecond-half"}, nil
}

// newSSEServer seeds one owner with one chat session and the given responder.
func newSSEServer(t *testing.T, assistant responder.Responder) (server *Server, token, sessionID string) {
	t.Helper()
	ctx := context.Background()
	store := repository.NewInMemoryRepository()
	user, err := store.CreateUser(ctx, &domain.User{Email: "sse@example.com", DisplayName: "SSE"})
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.CreateSession(ctx, &domain.Session{UserID: user.ID, Title: "SSE", Mode: domain.SessionModeChat})
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := security.NewTokenManagerWithLifetime("test-secret", "test-issuer", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	value, err := tokens.Issue(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	server = NewServer(
		service.NewAuthService(store, tokens, time.Now),
		service.NewSessionService(store),
		service.NewMessageService(store, assistant),
		tokens,
	)
	return server, value, session.ID
}

// TestSSEFlushesEveryEventBeforeTheNextIsWritten pins the requirement that the
// Gin stack still pushes each SSE event to the writer before producing the next.
func TestSSEFlushesEveryEventBeforeTheNextIsWritten(t *testing.T) {
	recorder := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	seen := &[]int{}
	server, token, sessionID := newSSEServer(t, flushProbeResponder{
		flushes: func() int { return recorder.flushes },
		seen:    seen,
	})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/"+sessionID+"/stream", strings.NewReader(`{"content":"question"}`))
	request.Header.Set("Authorization", "Bearer "+token)
	server.Handler().ServeHTTP(recorder, request)

	if len(*seen) != 2 {
		t.Fatalf("responder never observed the stream: %#v", *seen)
	}
	// message.started plus the first delta must be flushed before the second
	// delta is produced.
	if (*seen)[0] < 2 {
		t.Fatalf("flush count after the first delta = %d, want at least 2: the stream was buffered", (*seen)[0])
	}
	if (*seen)[1] <= (*seen)[0] {
		t.Fatalf("flush count did not advance between deltas: %v", *seen)
	}
	if recorder.flushes < 4 {
		t.Fatalf("total flushes = %d, want one per event", recorder.flushes)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "data: {\"delta\":\"first-half\"}") || !strings.Contains(body, "event: message.completed") {
		t.Fatalf("stream body incomplete: %s", body)
	}
}

// TestSSEOverRealListenerDeliversDeltasIncrementally drives the same route over a
// real TCP socket on 127.0.0.1. An in-process recorder cannot prove that no
// middleware, the transport, or chunked encoding buffers the stream, and the
// gate only opens once this reader has actually received a delta.
func TestSSEOverRealListenerDeliversDeltasIncrementally(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	openGate := func() { once.Do(func() { close(release) }) }

	server, token, sessionID := newSSEServer(t, gateResponder{release: release, deadline: 5 * time.Second})
	listener := httptest.NewServer(server.Handler())
	defer listener.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, listener.URL+"/api/v1/sessions/"+sessionID+"/stream", strings.NewReader(`{"content":"question"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "text/event-stream")
	response, err := listener.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	if !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("content-type = %q, want an SSE stream", response.Header.Get("Content-Type"))
	}

	var events []string
	gateOpened := false
	scanner := bufio.NewScanner(response.Body)
	pending := ""
	for scanner.Scan() {
		line := scanner.Text()
		if line != "" {
			if value, ok := strings.CutPrefix(line, "event: "); ok {
				pending = strings.TrimSpace(value)
			}
			continue
		}
		// A blank line closes one event block: the client has now received that
		// whole event.
		if pending == "" {
			continue
		}
		events = append(events, pending)
		if pending == "message.delta" && !gateOpened {
			gateOpened = true
			openGate()
		}
		pending = ""
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("reader failed after %v: %v", events, err)
	}
	if !gateOpened {
		t.Fatalf("no delta ever reached the reader, so streaming is unproven (events %v)", events)
	}
	want := []string{"message.started", "message.delta", "message.delta", "message.completed"}
	if strings.Join(events, ",") != strings.Join(want, ",") {
		t.Fatalf("events over the socket = %v, want %v", events, want)
	}
}
