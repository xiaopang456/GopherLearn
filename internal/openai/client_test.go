package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/model"
)

const testAPIKey = "sk-unit-test-key-0123456789"

// scriptedClient starts a local test server and points a client at it. Nothing in
// this file can reach a network that is not a loopback listener owned by the test.
func scriptedClient(t *testing.T, timeout time.Duration, handler http.HandlerFunc) (*Client, *httptest.Server, *atomic.Int64) {
	t.Helper()
	return scriptedClientWith(t, Config{Timeout: timeout, MaxOutputTokens: 128}, handler)
}

// scriptedClientWith is the same with a caller-supplied configuration, for tests
// that are about the knobs rather than the protocol.
func scriptedClientWith(t *testing.T, config Config, handler http.HandlerFunc) (*Client, *httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	config.BaseURL = server.URL
	if config.APIKey == "" {
		config.APIKey = testAPIKey
	}
	if config.Model == "" {
		config.Model = "test-model"
	}
	client, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	return client, server, &hits
}

// echoPayloadClient answers every request with a valid completion and hands back
// the decoded request body, for configuration tests that only inspect the wire.
func echoPayloadClient(t *testing.T, config Config) (*Client, *map[string]any, *atomic.Int64) {
	t.Helper()
	payload := map[string]any{}
	client, _, hits := scriptedClientWith(t, config, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Errorf("request body is not JSON: %v", err)
		}
		_, _ = w.Write([]byte(completeBody("ok", "stop")))
	})
	return client, &payload, hits
}

func oneUserRequest() model.Request {
	return model.Request{Messages: []model.Message{{Role: model.RoleUser, Content: "q"}}}
}

func completeBody(content, finish string) string {
	return `{"model":"answered-model","choices":[{"message":{"role":"assistant","content":"` + content + `"},"finish_reason":"` + finish + `"}],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`
}

func TestCompleteSendsTheDocumentedRequest(t *testing.T) {
	var captured struct {
		method        string
		path          string
		authorization string
		contentType   string
		accept        string
		payload       map[string]any
		raw           string
	}
	client, _, hits := scriptedClient(t, 2*time.Second, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured.method = r.Method
		captured.path = r.URL.Path
		captured.authorization = r.Header.Get("Authorization")
		captured.contentType = r.Header.Get("Content-Type")
		captured.accept = r.Header.Get("Accept")
		captured.raw = string(body)
		if err := json.Unmarshal(body, &captured.payload); err != nil {
			t.Errorf("request body is not JSON: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(completeBody("回答", "stop")))
	})

	completion, err := client.Complete(context.Background(), model.Request{
		Messages: []model.Message{
			{Role: model.RoleSystem, Content: "system prompt"},
			{Role: model.RoleUser, Content: "问题"},
			{Role: model.RoleAssistant, Content: "旧回答"},
			{Role: model.RoleUser, Content: "追问"},
		},
		Temperature:     0.4,
		MaxOutputTokens: 64,
	})
	if err != nil {
		t.Fatal(err)
	}
	if completion.Content != "回答" || completion.Model != "answered-model" || completion.FinishReason != model.FinishStop {
		t.Fatalf("completion = %#v", completion)
	}
	if completion.Usage != (model.Usage{PromptTokens: 11, CompletionTokens: 7, TotalTokens: 18}) {
		t.Fatalf("usage = %#v", completion.Usage)
	}
	if captured.method != http.MethodPost || captured.path != "/chat/completions" {
		t.Fatalf("%s %s, want POST /chat/completions", captured.method, captured.path)
	}
	if captured.authorization != "Bearer "+testAPIKey {
		t.Fatalf("Authorization = %q", captured.authorization)
	}
	if captured.contentType != "application/json" || captured.accept != "application/json" {
		t.Fatalf("content/accept = %q/%q", captured.contentType, captured.accept)
	}
	if captured.payload["model"] != "test-model" {
		t.Fatalf("body model = %v, want the configured name", captured.payload["model"])
	}
	if captured.payload["stream"] != false {
		t.Fatalf("stream = %v, want false on a completion call", captured.payload["stream"])
	}
	// enable_thinking is a provider extension, so an unconfigured client must not
	// put it on the wire at all. See TestEnableThinkingOnlyAppearsWhenConfigured.
	if _, present := captured.payload["enable_thinking"]; present {
		t.Fatalf("enable_thinking was sent although it was never configured: %v", captured.payload["enable_thinking"])
	}
	if captured.payload["max_tokens"] != float64(64) || captured.payload["temperature"] != 0.4 {
		t.Fatalf("generation knobs = %v/%v", captured.payload["max_tokens"], captured.payload["temperature"])
	}
	messages, ok := captured.payload["messages"].([]any)
	if !ok || len(messages) != 4 {
		t.Fatalf("messages = %v, want all four turns in order", captured.payload["messages"])
	}
	for index, raw := range messages {
		turn, _ := raw.(map[string]any)
		wantRole := []string{"system", "user", "assistant", "user"}[index]
		if turn["role"] != wantRole {
			t.Fatalf("message %d role = %v, want %v", index, turn["role"], wantRole)
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("one completion cost %d provider calls", hits.Load())
	}

	// The key travels in exactly one place. Scanning the wire text of the body and
	// the request line is what proves it, since a body that echoes a credential
	// tends to look otherwise normal.
	if strings.Contains(captured.raw, testAPIKey) || strings.Contains(captured.path, testAPIKey) {
		t.Fatal("the API key appeared outside the Authorization header")
	}
}

func TestZeroTemperatureAndConfiguredTokenBudgetAreOmitted(t *testing.T) {
	var payload map[string]any
	client, _, _ := scriptedClient(t, 2*time.Second, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &payload)
		_, _ = w.Write([]byte(completeBody("ok", "stop")))
	})
	if _, err := client.Complete(context.Background(), model.Request{
		Messages: []model.Message{{Role: model.RoleUser, Content: "问题"}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, present := payload["temperature"]; present {
		t.Fatalf("temperature %v was sent although unset; zero must mean \"do not override\"", payload["temperature"])
	}
	// max_tokens falls back to the configured budget rather than sending nothing.
	if payload["max_tokens"] != float64(128) {
		t.Fatalf("max_tokens = %v, want the configured 128", payload["max_tokens"])
	}
}

// TestGenerationKnobPrecedence is the fix for a configuration that was parsed and
// printed but never sent: the client default must reach the wire, a request may
// still override it, and only when both are zero may the field be omitted.
func TestGenerationKnobPrecedence(t *testing.T) {
	cases := []struct {
		name        string
		configured  float64
		requested   float64
		wantPresent bool
		wantValue   float64
	}{
		{"neither set omits the field", 0, 0, false, 0},
		{"client configuration reaches the wire", 0.7, 0, true, 0.7},
		{"request overrides configuration", 0.7, 0.2, true, 0.2},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			client, payload, _ := echoPayloadClient(t, Config{Temperature: testCase.configured, MaxOutputTokens: 128})
			if _, err := client.Complete(context.Background(), model.Request{
				Messages:    []model.Message{{Role: model.RoleUser, Content: "q"}},
				Temperature: testCase.requested,
			}); err != nil {
				t.Fatal(err)
			}
			value, present := (*payload)["temperature"]
			if present != testCase.wantPresent {
				t.Fatalf("temperature present=%v value=%v, want present=%v", present, value, testCase.wantPresent)
			}
			if present && value != testCase.wantValue {
				t.Fatalf("temperature = %v, want %v", value, testCase.wantValue)
			}
			// The token budget must keep inheriting the configured default.
			if (*payload)["max_tokens"] != float64(128) {
				t.Fatalf("max_tokens = %v, want the configured 128", (*payload)["max_tokens"])
			}
		})
	}

	// A per-request token budget still beats the configuration.
	client, payload, _ := echoPayloadClient(t, Config{MaxOutputTokens: 128})
	if _, err := client.Complete(context.Background(), model.Request{
		Messages: []model.Message{{Role: model.RoleUser, Content: "q"}}, MaxOutputTokens: 32,
	}); err != nil {
		t.Fatal(err)
	}
	if (*payload)["max_tokens"] != float64(32) {
		t.Fatalf("max_tokens = %v, want the request's 32", (*payload)["max_tokens"])
	}
}

// TestEnableThinkingOnlyAppearsWhenConfigured keeps the Qwen extension compatible
// without forcing it on providers that validate their request schema.
func TestEnableThinkingOnlyAppearsWhenConfigured(t *testing.T) {
	state := pointerTo(true)
	off := pointerTo(false)
	cases := []struct {
		name      string
		config    *bool
		present   bool
		wantValue any
	}{
		{"unset sends nothing", nil, false, nil},
		{"explicit false is still sent", off, true, false},
		{"explicit true is sent", state, true, true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			client, payload, _ := echoPayloadClient(t, Config{EnableThinking: testCase.config})
			if _, err := client.Complete(context.Background(), oneUserRequest()); err != nil {
				t.Fatal(err)
			}
			value, present := (*payload)["enable_thinking"]
			if present != testCase.present {
				t.Fatalf("enable_thinking present=%v value=%v, want present=%v", present, value, testCase.present)
			}
			if present && value != testCase.wantValue {
				t.Fatalf("enable_thinking = %v, want %v", value, testCase.wantValue)
			}
		})
	}
	// The same rule holds on the streaming path, which is where a provider that
	// rejects unknown fields would otherwise be discovered in production.
	client, _, _ := echoPayloadClient(t, Config{EnableThinking: nil})
	var streamPayload map[string]any
	streaming, _, _ := scriptedClientWith(t, Config{Timeout: 2 * time.Second}, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &streamPayload)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sse(chunkDelta("好"), "[DONE]"))
	})
	if _, err := streaming.Stream(context.Background(), oneUserRequest(), func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, present := streamPayload["enable_thinking"]; present {
		t.Fatalf("the stream sent enable_thinking although it was never configured: %v", streamPayload["enable_thinking"])
	}
	_ = client
}

func TestRequestValidationHappensBeforeAnyCall(t *testing.T) {
	client, _, hits := scriptedClient(t, 2*time.Second, func(w http.ResponseWriter, r *http.Request) {
		t.Error("a malformed request reached the provider")
	})
	for name, request := range map[string]model.Request{
		"no messages":   {},
		"no user turn":  {Messages: []model.Message{{Role: model.RoleSystem, Content: "s"}}},
		"system last":   {Messages: []model.Message{{Role: model.RoleUser, Content: "q"}, {Role: model.RoleSystem, Content: "s"}}},
		"unknown role":  {Messages: []model.Message{{Role: "tool", Content: "x"}}},
		"negative knob": {Messages: []model.Message{{Role: model.RoleUser, Content: "q"}}, Temperature: -1},
	} {
		if _, err := client.Complete(context.Background(), request); !errors.Is(err, model.ErrInvalidRequest) {
			t.Fatalf("%s error = %v, want ErrInvalidRequest", name, err)
		}
		if _, err := client.Stream(context.Background(), request, func(string) error { return nil }); !errors.Is(err, model.ErrInvalidRequest) {
			t.Fatalf("%s stream error = %v, want ErrInvalidRequest", name, err)
		}
	}
	if _, err := client.Stream(context.Background(), model.Request{Messages: []model.Message{{Role: model.RoleUser, Content: "q"}}}, nil); !errors.Is(err, model.ErrInvalidRequest) {
		t.Fatalf("nil delta handler error = %v", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("rejected requests still dialed %d times", hits.Load())
	}
}

func TestFailureModesHaveStableErrors(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
		would   error
		// forbidden substrings: an error message must not carry prompt or answer
		// content, because error text is what ends up in a log.
		forbids []string
	}{
		{
			name: "401 with an echoed prompt",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":{"message":"bad key for prompt 问题","code":"invalid_api_key","type":"auth"}}`))
			},
			would:   model.ErrProviderStatus,
			forbids: []string{"问题", "bad key"},
		},
		{
			name: "429 without a body",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusTooManyRequests)
			},
			would: model.ErrProviderStatus,
		},
		{
			name: "500 that is not JSON",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte("<html>gateway blew up</html>"))
			},
			would:   model.ErrProviderStatus,
			forbids: []string{"gateway blew up"},
		},
		{
			name: "200 with a truncated body",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"choices": [`))
			},
			would: model.ErrMalformedResponse,
		},
		{
			name: "200 with no choices",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"choices":[]}`))
			},
			would: model.ErrMalformedResponse,
		},
		{
			name: "200 with blank content",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(completeBody("   ", "stop")))
			},
			would: model.ErrEmptyCompletion,
		},
		{
			name: "200 with no content field",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"stop"}]}`))
			},
			would: model.ErrEmptyCompletion,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			client, _, _ := scriptedClient(t, 2*time.Second, testCase.handler)
			_, err := client.Complete(context.Background(), model.Request{
				Messages: []model.Message{{Role: model.RoleUser, Content: "问题"}},
			})
			if !errors.Is(err, testCase.would) {
				t.Fatalf("error = %v, want %v", err, testCase.would)
			}
			for _, forbidden := range testCase.forbids {
				if strings.Contains(err.Error(), forbidden) {
					t.Fatalf("error text carried %q: %v", forbidden, err)
				}
			}
		})
	}
}

func TestUnreachableProviderIsNotSilentlyRetried(t *testing.T) {
	// A closed listener makes every attempt fail at connect time, which is how a
	// test proves "one user turn, one dial" without a network.
	closed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	closed.Close()
	unreachable, err := New(Config{BaseURL: closed.URL, APIKey: testAPIKey, Model: "m", Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unreachable.Complete(context.Background(), model.Request{
		Messages: []model.Message{{Role: model.RoleUser, Content: "q"}},
	}); !errors.Is(err, model.ErrUnavailable) {
		t.Fatalf("error = %v, want ErrUnavailable", err)
	}

	failing, _, hits := scriptedClient(t, 2*time.Second, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	if _, err := failing.Complete(context.Background(), model.Request{
		Messages: []model.Message{{Role: model.RoleUser, Content: "q"}},
	}); !errors.Is(err, model.ErrProviderStatus) {
		t.Fatalf("error = %v, want ErrProviderStatus", err)
	}
	// retry=0 is a product decision: one user turn must not become five paid calls.
	if got := hits.Load(); got != 1 {
		t.Fatalf("a failing call dialed %d times, want exactly 1", got)
	}
}

func TestTimeoutProducesAStableError(t *testing.T) {
	// The handler outlives the client's budget by design; it finishes on its own
	// so a timed-out test does not wait for the server to shut down.
	slow := func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_, _ = w.Write([]byte(completeBody("too late", "stop")))
	}
	complete, _, _ := scriptedClient(t, 60*time.Millisecond, slow)
	_, err := complete.Complete(context.Background(), model.Request{
		Messages: []model.Message{{Role: model.RoleUser, Content: "q"}},
	})
	if !errors.Is(err, model.ErrTimeout) {
		t.Fatalf("complete timeout error = %v, want ErrTimeout", err)
	}

	streaming, _, _ := scriptedClient(t, 60*time.Millisecond, slow)
	_, err = streaming.Stream(context.Background(), model.Request{
		Messages: []model.Message{{Role: model.RoleUser, Content: "q"}},
	}, func(string) error { return nil })
	if !errors.Is(err, model.ErrTimeout) {
		t.Fatalf("stream timeout error = %v, want ErrTimeout before the first byte", err)
	}
}

// TestCallerCancellationAbandonsTheUpstreamRequest is the cancellation half of the
// error taxonomy: a user hanging up must surface as context.Canceled, never as a
// provider outage or a timeout, and the client must stop waiting instead of
// sitting through the answer it no longer has anyone to deliver.
func TestCallerCancellationAbandonsTheUpstreamRequest(t *testing.T) {
	const providerLatency = 400 * time.Millisecond
	dialed := make(chan struct{})
	finished := make(chan error, 1)
	client, _, _ := scriptedClient(t, 10*time.Second, func(w http.ResponseWriter, r *http.Request) {
		close(dialed)
		time.Sleep(providerLatency)
		_, _ = w.Write([]byte(completeBody("回答", "stop")))
	})
	ctx, cancel := context.WithCancel(context.Background())
	started := time.Now()
	go func() {
		_, err := client.Complete(ctx, model.Request{Messages: []model.Message{{Role: model.RoleUser, Content: "q"}}})
		finished <- err
	}()
	<-dialed
	cancel()

	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation error = %v, want context.Canceled", err)
		}
		// Returning early is the observable proof the in-flight request was given
		// up on; waiting for the provider's answer would take at least its latency.
		if elapsed := time.Since(started); elapsed >= providerLatency {
			t.Fatalf("the call returned after %v, which is the provider's own latency: it kept waiting", elapsed)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the call did not return after the caller cancelled")
	}
}

func sse(events ...string) string {
	var builder strings.Builder
	for _, event := range events {
		builder.WriteString("event: chat.completion.chunk\n")
		builder.WriteString("data: " + event + "\n\n")
	}
	return builder.String()
}

func chunkDelta(content string) string {
	return `{"model":"stream-model","choices":[{"index":0,"delta":{"content":"` + content + `"}}]}`
}

func TestStreamMapsProviderEventsToDeltas(t *testing.T) {
	client, _, hits := scriptedClient(t, 2*time.Second, func(w http.ResponseWriter, r *http.Request) {
		if accept := r.Header.Get("Accept"); accept != "text/event-stream" {
			t.Errorf("Accept = %q, want text/event-stream", accept)
		}
		var payload map[string]any
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &payload)
		if payload["stream"] != true {
			t.Errorf("stream = %v, want true", payload["stream"])
		}
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		_, _ = io.WriteString(w, ": keep-alive\n\n")
		_, _ = io.WriteString(w, sse(
			`{"choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
			chunkDelta("持久"),
			chunkDelta("化："),
			// A reasoning field is not answer text and must not reach the client.
			`{"choices":[{"index":0,"delta":{"reasoning_content":"内部思考"}}]}`,
			chunkDelta("AOF"),
			`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`,
		))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})

	var deltas []string
	completion, err := client.Stream(context.Background(), model.Request{
		Messages: []model.Message{{Role: model.RoleUser, Content: "q"}},
	}, func(delta string) error {
		deltas = append(deltas, delta)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(deltas, "") != "持久化：AOF" {
		t.Fatalf("deltas = %q, want only answer content", deltas)
	}
	if strings.Contains(strings.Join(deltas, ""), "内部思考") || strings.Contains(completion.Content, "内部思考") {
		t.Fatal("non-answer fields leaked into the answer")
	}
	if completion.Content != "持久化：AOF" || completion.Model != "stream-model" || completion.FinishReason != model.FinishStop {
		t.Fatalf("completion = %#v", completion)
	}
	if completion.Usage.TotalTokens != 8 {
		t.Fatalf("usage = %#v", completion.Usage)
	}
	if hits.Load() != 1 {
		t.Fatalf("one stream cost %d calls", hits.Load())
	}
}

// TestStreamParserSurvivesByteAtATimeInput is the cross-network-block case: a
// multi-byte character can be split across reads, and a parser that decoded per
// read would corrupt it. Reading one byte at a time forces every split there is.
func TestStreamParserSurvivesByteAtATimeInput(t *testing.T) {
	client, _, _ := scriptedClient(t, 2*time.Second, func(w http.ResponseWriter, r *http.Request) {})
	body := sse(
		chunkDelta("每"),
		`{"choices":[{"index":0,"delta":{"content":"一个字节地"}}]}`,
		chunkDelta("到达 🎉 ok"),
		`{"choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`,
	) + "data: [DONE]\n\n"

	var assembled strings.Builder
	completion, err := client.consumeStream(context.Background(), iotest.OneByteReader(strings.NewReader(body)), http.StatusOK, func(delta string) error {
		assembled.WriteString(delta)
		return nil
	}, nil)
	// The provider said "length": the answer it delivered is a prefix of what it was
	// writing, so this is a failure and not a completion with a note attached. What the
	// byte-at-a-time parser still did is proved by the fragments below - it forwarded
	// every one of them, splitting no character and dropping no text.
	if !errors.Is(err, model.ErrTruncatedStream) {
		t.Fatalf("error = %v, want a truncated stream refused as an answer", err)
	}
	if completion.Content != "" {
		t.Fatalf("a truncated stream returned content %q, want none", completion.Content)
	}
	if got, want := assembled.String(), "每一个字节地到达 🎉 ok"; got != want {
		t.Fatalf("assembled %q, want %q", got, want)
	}
}

func TestStreamRejectsBadProtocol(t *testing.T) {
	cases := []struct {
		name    string
		media   string
		body    string
		would   error
		message string
	}{
		{
			name: "json body for a stream", media: "application/json", body: `{"choices":[]}`, would: model.ErrMalformedResponse,
		},
		{
			name: "data line is not json", media: "text/event-stream", body: "data: {nope\n\n", would: model.ErrMalformedResponse,
		},
		{
			name: "provider error mid stream", media: "text/event-stream", body: sse(chunkDelta("前半"), `{"error":{"code":"server_error","type":"api_error"}}`), would: model.ErrProviderStatus,
		},
		{
			name: "no content at all", media: "text/event-stream", body: sse(`{"choices":[{"index":0,"delta":{"role":"assistant"}}]}`) + "data: [DONE]\n\n", would: model.ErrEmptyCompletion,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			client, _, _ := scriptedClient(t, 2*time.Second, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", testCase.media)
				_, _ = io.WriteString(w, testCase.body)
			})
			_, err := client.Stream(context.Background(), model.Request{
				Messages: []model.Message{{Role: model.RoleUser, Content: "q"}},
			}, func(string) error { return nil })
			if !errors.Is(err, testCase.would) {
				t.Fatalf("error = %v, want %v", err, testCase.would)
			}
		})
	}
}

func TestStreamParserRecordsAnUnfinishedStream(t *testing.T) {
	client, _, _ := scriptedClient(t, 2*time.Second, func(w http.ResponseWriter, r *http.Request) {})
	// A stream that ends without [DONE] and without a finish reason was cut off;
	// reporting "stop" would hide that from the caller, and reporting it as a finished
	// answer would store a half sentence as the assistant's word.
	_, err := client.consumeStream(context.Background(), strings.NewReader(sse(chunkDelta("半句"))), http.StatusOK, func(string) error { return nil }, nil)
	if !errors.Is(err, model.ErrTruncatedStream) {
		t.Fatalf("error = %v, want the cut-off stream refused", err)
	}
	// The marker alone is enough: some vendors send no finish reason at all, and their
	// turn did announce its end.
	if _, err := client.consumeStream(context.Background(),
		strings.NewReader(sse(chunkDelta("完整一句"))+"data: [DONE]\n\n"), http.StatusOK,
		func(string) error { return nil }, nil); err != nil {
		t.Fatalf("a stream closed by its completion marker was refused: %v", err)
	}
}

// TestStreamToolRoundCannotBeTruncatedIntoACall is the same rule on the agent's path,
// where the cost of getting it wrong is higher than a half sentence: a tool call's
// arguments arrive in fragments, and a stream cut in the middle of one can leave
// syntactically valid JSON holding a truncated value. Executing that would run a request
// the model never finished making.
func TestStreamToolRoundCannotBeTruncatedIntoACall(t *testing.T) {
	cases := []struct{ name, tail string }{
		{"closed_without_marker_or_reason", ""},
		{"stopped_at_output_limit", `{"choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			client, _, _ := scriptedClient(t, 2*time.Second, func(w http.ResponseWriter, r *http.Request) {})
			body := sse(
				chunkDelta("PARTIAL ANSWER"),
				`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"knowledge_search","arguments":"{\"query\":\"per\""}}]}}]}`,
			)
			if testCase.tail != "" {
				body += sse(testCase.tail)
			}
			var fragments int
			_, err := client.consumeToolStream(context.Background(), strings.NewReader(body), http.StatusOK,
				func(model.StreamDelta) error {
					fragments++
					return nil
				}, nil)
			if !errors.Is(err, model.ErrTruncatedStream) {
				t.Fatalf("error = %v, want a cut-off tool round refused", err)
			}
			// The text the provider did send was forwarded before the refusal, which is
			// what lets the caller label it incomplete instead of losing it.
			if fragments == 0 {
				t.Fatal("the truncated stream's own fragments never reached the caller")
			}
		})
	}
}

// TestStreamStopsWhenTheConsumerCancelsMidBody covers the case that matters for a
// chat UI: the reader is inside the body when the caller goes away, and the parser
// must return the caller's cancellation instead of reporting a provider failure.
func TestStreamStopsWhenTheConsumerCancelsMidBody(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	client, _, _ := scriptedClient(t, 10*time.Second, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		_, _ = io.WriteString(w, sse(chunkDelta("第一段")))
		flusher.Flush()
		// Hold the body open until the test says the reader is gone, then finish.
		select {
		case <-release:
		case <-time.After(3 * time.Second):
		}
		_, _ = io.WriteString(w, sse(chunkDelta("第二段"), `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)+"data: [DONE]\n\n")
	})
	ctx, cancel := context.WithCancel(context.Background())
	err := func() error {
		_, err := client.Stream(ctx, model.Request{Messages: []model.Message{{Role: model.RoleUser, Content: "q"}}}, func(string) error {
			cancel()
			return nil
		})
		return err
	}()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled from a caller that hung up", err)
	}
}

func TestStreamHandlerErrorPropagatesImmediately(t *testing.T) {
	client, _, _ := scriptedClient(t, 2*time.Second, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sse(chunkDelta("第一段"), chunkDelta("第二段")))
	})
	boom := errors.New("sse write failed")
	seen := 0
	_, err := client.Stream(context.Background(), model.Request{
		Messages: []model.Message{{Role: model.RoleUser, Content: "q"}},
	}, func(string) error {
		seen++
		if seen == 2 {
			return boom
		}
		return nil
	})
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want the handler failure to surface", err)
	}
	if seen != 2 {
		t.Fatalf("handler ran %d times after aborting at 2", seen)
	}
}

func TestRequestsRefuseANilContextAndACancelledOne(t *testing.T) {
	client, _, hits := scriptedClient(t, 2*time.Second, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(completeBody("never", "stop")))
	})
	request := model.Request{Messages: []model.Message{{Role: model.RoleUser, Content: "q"}}}
	if _, err := client.Complete(nil, request); err == nil {
		t.Fatal("a nil context was accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.Complete(ctx, request); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if _, err := client.Stream(ctx, request, func(string) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("stream error = %v, want context.Canceled", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("refused calls still dialed %d times", hits.Load())
	}
}

func TestProbeErrorBodyIsBounded(t *testing.T) {
	// A hostile endpoint must not be able to make the client read without limit.
	huge := strings.Repeat("x", 2*1024*1024)
	client, _, _ := scriptedClient(t, 3*time.Second, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, huge)
	})
	_, err := client.Complete(context.Background(), model.Request{
		Messages: []model.Message{{Role: model.RoleUser, Content: "q"}},
	})
	if !errors.Is(err, model.ErrProviderStatus) {
		t.Fatalf("error = %v, want ErrProviderStatus", err)
	}
	if len(err.Error()) > 512 {
		t.Fatalf("error text grew with the response body: %d bytes", len(err.Error()))
	}
}

// TestStatusErrorExposesOnlyTheStatus is what lets a caller archive a failure
// without archiving a provider's words: the number is readable through
// ProviderStatusOf, while the invented message text appears nowhere in it.
func TestStatusErrorExposesOnlyTheStatus(t *testing.T) {
	client, _, _ := scriptedClient(t, 2*time.Second, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = fmt.Fprint(w, `{"error":{"code":"quota","type":"rate_limit","message":"echo of the prompt"}}`)
	})
	_, err := client.Complete(context.Background(), model.Request{
		Messages: []model.Message{{Role: model.RoleUser, Content: "q"}},
	})
	if !errors.Is(err, model.ErrProviderStatus) {
		t.Fatalf("error = %v, want ErrProviderStatus", err)
	}
	status, ok := ProviderStatusOf(err)
	if !ok || status != http.StatusTooManyRequests {
		t.Fatalf("ProviderStatusOf = %d/%v, want %d/true", status, ok, http.StatusTooManyRequests)
	}
	if strings.Contains(err.Error(), "echo of the prompt") || strings.Contains(err.Error(), testAPIKey) {
		t.Fatalf("provider wording or the key reached the error: %q", err)
	}
	// The shape the sentinel has always produced, so no log line changed.
	if want := "model: provider returned an error status: status 429 type rate_limit code quota"; err.Error() != want {
		t.Fatalf("error text = %q, want %q", err, want)
	}
	if _, ok := ProviderStatusOf(model.ErrUnavailable); ok {
		t.Fatal("ProviderStatusOf claimed a status for an error that has none")
	}
	if _, ok := ProviderStatusOf(errors.New("plain")); ok {
		t.Fatal("ProviderStatusOf matched an unrelated error")
	}
}
