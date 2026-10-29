package openai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/model"
)

// Config.WrapTransport exists so a caller can count and bound the HTTP requests
// this client actually makes. These tests hold both halves of that promise: the
// wrapper sees every entry point and every redirect hop, and it changes nothing
// about how the request looks on the wire or how the two timeout contracts behave.

func identityWrapper() *TransportWrapper {
	return &TransportWrapper{RoundTrip: func(base http.RoundTripper) http.RoundTripper { return base }}
}

func funcWrapper(wrap func(http.RoundTripper) http.RoundTripper) *TransportWrapper {
	return &TransportWrapper{RoundTrip: wrap}
}

// hopCounter is shared by the two wrapped transports so one counter covers both
// clients, while each transport still delegates to the base it was handed.
type hopCounter struct {
	calls  atomic.Int64
	failAt int64
	capErr error
}

type countingTransport struct {
	outer *hopCounter
	base  http.RoundTripper
}

func (t countingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	seen := t.outer.calls.Add(1)
	if t.outer.failAt > 0 && seen > t.outer.failAt {
		return nil, t.outer.capErr
	}
	return t.base.RoundTrip(request)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func wrappedClient(t *testing.T, counter *hopCounter, body string) (*Client, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	stream := strings.HasPrefix(body, "data:")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if stream {
			w.Header().Set("content-type", "text/event-stream")
		} else {
			w.Header().Set("content-type", "application/json")
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	client, err := New(Config{
		BaseURL: server.URL, APIKey: testAPIKey, Model: "test-model",
		Timeout: 5 * time.Second, MaxOutputTokens: 128,
		WrapTransport: funcWrapper(func(base http.RoundTripper) http.RoundTripper {
			return countingTransport{outer: counter, base: base}
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	return client, &hits
}

func streamingBody(text string) string {
	return "data: " + `{"choices":[{"index":0,"delta":{"content":"` + text + `"}}]}` + "\n\n" +
		"data: " + `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		"data: [DONE]\n\n"
}

// toolRequest is the smallest request the two tool entries accept: they refuse a
// turn that carries no server-owned tool to offer.
func toolRequest() model.Request {
	request := oneUserRequest()
	request.Tools = []model.ToolDefinition{{
		Name:        "knowledge_search",
		Description: "在用户知识库中检索",
		Parameters:  json.RawMessage(`{"type":"object","additionalProperties":false}`),
	}}
	return request
}

func TestWrapTransportSeesEveryEntry(t *testing.T) {
	for _, entry := range []struct {
		name string
		body string
		call func(*Client) error
	}{
		{"Complete", completeBody("你好", "stop"), func(c *Client) error {
			_, err := c.Complete(context.Background(), oneUserRequest())
			return err
		}},
		{"CompleteWithTools", completeBody("你好", "stop"), func(c *Client) error {
			_, err := c.CompleteWithTools(context.Background(), toolRequest())
			return err
		}},
		{"Stream", streamingBody("你好"), func(c *Client) error {
			_, err := c.Stream(context.Background(), oneUserRequest(), func(string) error { return nil })
			return err
		}},
		{"StreamWithTools", streamingBody("你好"), func(c *Client) error {
			_, err := c.StreamWithTools(context.Background(), toolRequest(), func(model.StreamDelta) error { return nil })
			return err
		}},
	} {
		t.Run(entry.name, func(t *testing.T) {
			counter := &hopCounter{}
			client, hits := wrappedClient(t, counter, entry.body)
			if err := entry.call(client); err != nil {
				t.Fatalf("%s: %v", entry.name, err)
			}
			if got := counter.calls.Load(); got != 1 {
				t.Errorf("wrapper saw %d round trips, want 1", got)
			}
			if got := hits.Load(); got != 1 {
				t.Errorf("server saw %d requests, want 1", got)
			}
		})
	}
}

// The wire must not change: same method, path, headers and body, and the same
// response and error semantics, with and without the wrapper.
func TestWrapTransportKeepsTheWireIdentical(t *testing.T) {
	type captured struct {
		method, path, auth, contentType, accept, body string
		completed                                     model.Completion
		errText                                       string
	}
	run := func(t *testing.T, wrapIt bool) captured {
		t.Helper()
		var out captured
		var counted atomic.Int64
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			out.method, out.path = r.Method, r.URL.Path
			out.auth = r.Header.Get("Authorization")
			out.contentType = r.Header.Get("Content-Type")
			out.accept = r.Header.Get("Accept")
			out.body = string(body)
			w.Header().Set("content-type", "application/json")
			_, _ = io.WriteString(w, completeBody("答案", "stop"))
		}))
		t.Cleanup(server.Close)
		config := Config{
			BaseURL: server.URL, APIKey: testAPIKey, Model: "test-model",
			Timeout: 5 * time.Second, MaxOutputTokens: 128,
		}
		if wrapIt {
			config.WrapTransport = funcWrapper(func(base http.RoundTripper) http.RoundTripper {
				return roundTripFunc(func(request *http.Request) (*http.Response, error) {
					counted.Add(1)
					return base.RoundTrip(request)
				})
			})
		}
		client, err := New(config)
		if err != nil {
			t.Fatal(err)
		}
		completion, err := client.Complete(context.Background(), oneUserRequest())
		out.completed, out.errText = completion, errText(err)
		if wrapIt && counted.Load() == 0 {
			t.Error("wrapper never saw a request")
		}
		return out
	}
	plain, wrapped := run(t, false), run(t, true)
	if plain.body != wrapped.body || plain.method != wrapped.method || plain.path != wrapped.path ||
		plain.auth != wrapped.auth || plain.contentType != wrapped.contentType || plain.accept != wrapped.accept {
		t.Errorf("the request changed:\n plain=%+v\n wrapped=%+v", plain, wrapped)
	}
	if plain.errText != wrapped.errText || plain.completed.Content != wrapped.completed.Content ||
		plain.completed.Usage.TotalTokens != wrapped.completed.Usage.TotalTokens {
		t.Errorf("the response handling changed: %q vs %q", plain.errText, wrapped.errText)
	}

	errorCase := func(t *testing.T, wrapIt bool, handler http.HandlerFunc) string {
		t.Helper()
		server := httptest.NewServer(handler)
		t.Cleanup(server.Close)
		config := Config{BaseURL: server.URL, APIKey: testAPIKey, Model: "test-model", Timeout: 5 * time.Second}
		if wrapIt {
			config.WrapTransport = identityWrapper()
		}
		client, err := New(config)
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.Complete(context.Background(), oneUserRequest())
		// 错误文本可能带端点：只比较错误类别，不比较文本。
		return errorKindOf(err)
	}
	for _, scenario := range []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"500", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) }},
		{"429", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTooManyRequests) }},
		{"非 JSON", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("content-type", "application/json")
			_, _ = io.WriteString(w, "not json")
		}},
		{"连接被中断", func(w http.ResponseWriter, _ *http.Request) {
			if hijacker, ok := w.(http.Hijacker); ok {
				conn, _, _ := hijacker.Hijack()
				_ = conn.Close()
			}
		}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			plainKind := errorCase(t, false, scenario.handler)
			wrappedKind := errorCase(t, true, scenario.handler)
			if plainKind != wrappedKind {
				t.Errorf("错误类别变了：plain=%s wrapped=%s", plainKind, wrappedKind)
			}
		})
	}
}

// errorKindOf names an error without carrying any URL text into the report.
func errorKindOf(err error) string {
	switch {
	case err == nil:
		return "none"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, model.ErrTimeout):
		return "timeout"
	case errors.Is(err, model.ErrUnavailable):
		return "unavailable"
	case errors.Is(err, model.ErrProviderStatus):
		return "provider-status"
	case errors.Is(err, model.ErrMalformedResponse):
		return "malformed"
	case errors.Is(err, model.ErrTruncatedStream):
		return "truncated"
	case errors.Is(err, model.ErrInvalidRequest):
		return "invalid-request"
	}
	return "other"
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// The two clients keep their own timeout contracts even when wrapped.
func TestWrappedTimeoutContractsAreTheUnwrappedOnes(t *testing.T) {
	for _, wrapIt := range []bool{false, true} {
		config := Config{BaseURL: "https://example.invalid/v1", APIKey: testAPIKey,
			Model: "test-model", Timeout: 200 * time.Millisecond}
		if wrapIt {
			config.WrapTransport = identityWrapper()
		}
		client, err := New(config)
		if err != nil {
			t.Fatal(err)
		}
		if got := client.completeClient.Timeout; got != 200*time.Millisecond {
			t.Errorf("wrap=%v: complete Timeout = %v, want the whole-call bound", wrapIt, got)
		}
		if got := client.streamClient.Timeout; got != 0 {
			t.Errorf("wrap=%v: the streaming client gained a whole-stream deadline (%v)", wrapIt, got)
		}
		base, ok := client.streamClient.Transport.(*http.Transport)
		if !ok {
			t.Fatalf("wrap=%v: stream transport is %T, not the configured clone", wrapIt, client.streamClient.Transport)
		}
		if base.ResponseHeaderTimeout != 200*time.Millisecond {
			t.Errorf("wrap=%v: header timeout = %v", wrapIt, base.ResponseHeaderTimeout)
		}
	}
}

// A stream that keeps producing text must survive longer than Timeout in total,
// wrapped or not; only a stall between chunks counts as idle.
func TestWrappedStreamNotCutByTotalDuration(t *testing.T) {
	for _, wrapIt := range []bool{false, true} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("content-type", "text/event-stream")
			flusher, _ := w.(http.Flusher)
			for index := 0; index < 12; index++ {
				_, _ = io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"content":"x"}}]}`+"\n\n")
				if flusher != nil {
					flusher.Flush()
				}
				time.Sleep(25 * time.Millisecond) // 总计 ~300ms > Timeout 200ms，段间 25ms 不算空闲
			}
			_, _ = io.WriteString(w, `data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		}))
		t.Cleanup(server.Close)
		config := Config{BaseURL: server.URL, APIKey: testAPIKey, Model: "test-model", Timeout: 200 * time.Millisecond}
		if wrapIt {
			config.WrapTransport = identityWrapper()
		}
		client, err := New(config)
		if err != nil {
			t.Fatal(err)
		}
		deltas := 0
		completion, err := client.Stream(context.Background(), oneUserRequest(), func(string) error {
			deltas++
			return nil
		})
		if err != nil {
			t.Errorf("wrap=%v: a stream that kept producing was cut off: %v", wrapIt, errorKindOf(err))
		}
		if deltas < 10 {
			t.Errorf("wrap=%v: only %d deltas arrived", wrapIt, deltas)
		}
		if completion.Content == "" {
			t.Errorf("wrap=%v: no content assembled", wrapIt)
		}
	}
}

// Cancellation stays cancellation, not a transport failure.
func TestWrappedCancellationMatchesUnwrapped(t *testing.T) {
	for _, wrapIt := range []bool{false, true} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("content-type", "text/event-stream")
			_, _ = io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"content":"x"}}]}`+"\n\n")
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			time.Sleep(200 * time.Millisecond)
		}))
		t.Cleanup(server.Close)
		config := Config{BaseURL: server.URL, APIKey: testAPIKey, Model: "test-model", Timeout: 5 * time.Second}
		if wrapIt {
			config.WrapTransport = identityWrapper()
		}
		client, err := New(config)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(30 * time.Millisecond)
			cancel()
		}()
		_, err = client.Stream(ctx, oneUserRequest(), func(string) error { return nil })
		if kind := errorKindOf(err); kind != "cancelled" {
			t.Errorf("wrap=%v: 取消被归为 %s", wrapIt, kind)
		}
	}
}

// A wrapper that produces nothing - or an empty wrapper - is refused rather than
// silently dropping the caller's observation.
func TestWrapTransportReturningNilIsRejected(t *testing.T) {
	for _, item := range []struct {
		name string
		wrap *TransportWrapper
	}{
		{"第一个 transport 返回 nil", funcWrapper(func(http.RoundTripper) http.RoundTripper { return nil })},
		{"第二个 transport 返回 nil", funcWrapper(func() func(http.RoundTripper) http.RoundTripper {
			calls := 0
			return func(base http.RoundTripper) http.RoundTripper {
				calls++
				if calls == 2 {
					return nil
				}
				return base
			}
		}())},
		{"包装结构里没有函数", &TransportWrapper{}},
	} {
		t.Run(item.name, func(t *testing.T) {
			_, err := New(Config{BaseURL: "https://example.invalid/v1", APIKey: testAPIKey,
				Model: "test-model", WrapTransport: item.wrap})
			if !errors.Is(err, ErrInvalidWrapTransport) {
				t.Errorf("err = %v, want ErrInvalidWrapTransport", err)
			}
		})
	}
}

// Redirects are exactly the case the wrapper exists for: one API call, several
// HTTP requests. Every hop is counted, and a cap refuses the next hop before it
// reaches the underlying transport - so the wrapper's count and the server's count
// agree and the bound is real rather than nominal.
func TestWrappedTransportSeesRedirectHopsAndCanCapThem(t *testing.T) {
	for _, item := range []struct {
		name       string
		failAt     int64
		wantHops   int64
		wantServer int64
		wantErr    bool
	}{
		{"不设上限时两跳都发生", 0, 2, 2, false},
		{"上限为一跳则第二跳不发出", 1, 2, 1, true},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(item.name+map[bool]string{false: "（普通）", true: "（流式）"}[stream], func(t *testing.T) {
				var serverHits atomic.Int64
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					serverHits.Add(1)
					if r.URL.Query().Get("hop") == "" {
						// 307 保留方法与请求体，第二跳是同一次请求的真实重放。
						http.Redirect(w, r, r.URL.Path+"?hop=2", http.StatusTemporaryRedirect)
						return
					}
					if stream {
						w.Header().Set("content-type", "text/event-stream")
						_, _ = io.WriteString(w, streamingBody("ok"))
						return
					}
					w.Header().Set("content-type", "application/json")
					_, _ = io.WriteString(w, completeBody("ok", "stop"))
				}))
				t.Cleanup(server.Close)
				counter := &hopCounter{failAt: item.failAt, capErr: errors.New("over cap")}
				client, err := New(Config{
					BaseURL: server.URL, APIKey: testAPIKey, Model: "test-model", Timeout: 5 * time.Second,
					WrapTransport: funcWrapper(func(base http.RoundTripper) http.RoundTripper {
						return countingTransport{outer: counter, base: base}
					}),
				})
				if err != nil {
					t.Fatal(err)
				}
				var callErr error
				if stream {
					_, callErr = client.Stream(context.Background(), oneUserRequest(), func(string) error { return nil })
				} else {
					_, callErr = client.Complete(context.Background(), oneUserRequest())
				}
				if item.wantErr && callErr == nil {
					t.Error("超额的那一跳本该在派发前被挡住，调用却成功了")
				}
				if !item.wantErr && callErr != nil {
					t.Fatalf("不该失败：%v", callErr)
				}
				if got := counter.calls.Load(); got != item.wantHops {
					t.Errorf("wrapper 计数 %d，期望 %d", got, item.wantHops)
				}
				if got := serverHits.Load(); got != item.wantServer {
					t.Errorf("服务端实收 %d，期望 %d（超额的请求不能已经发出）", got, item.wantServer)
				}
			})
		}
	}
}

// Wrapping must not touch the process-global transport, and must not be handed back
// its own output.
func TestNoGlobalTransportIsReplaced(t *testing.T) {
	before := http.DefaultTransport
	for _, wrapIt := range []bool{false, true} {
		config := Config{BaseURL: "https://example.invalid/v1", APIKey: testAPIKey, Model: "test-model"}
		if wrapIt {
			config.WrapTransport = funcWrapper(func(base http.RoundTripper) http.RoundTripper {
				if _, ok := base.(countingTransport); ok {
					t.Error("wrapper 被喂了自己的产物：包装被串成了链")
				}
				return countingTransport{base: base}
			})
		}
		if _, err := New(config); err != nil {
			t.Fatal(err)
		}
	}
	if http.DefaultTransport != before {
		t.Error("http.DefaultTransport 被换掉了")
	}
}

func TestWrappedClientIsConcurrencySafe(t *testing.T) {
	counter := &hopCounter{}
	client, hits := wrappedClient(t, counter, completeBody("ok", "stop"))
	var wg sync.WaitGroup
	errs := make([]string, 8)
	for index := range errs {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			_, err := client.Complete(context.Background(), oneUserRequest())
			errs[slot] = errText(err)
		}(index)
	}
	wg.Wait()
	for index, text := range errs {
		if text != "" {
			t.Errorf("call %d failed: %s", index, text)
		}
	}
	if got := counter.calls.Load(); got != 8 || hits.Load() != 8 {
		t.Errorf("wrapper=%d server=%d, want 8/8", counter.calls.Load(), hits.Load())
	}
}

// The field must not break how callers already use Config: internal/app asserts a
// refused configuration yields a zero Config, which needs Config to stay
// comparable - the reason WrapTransport is a pointer to a one-field struct rather
// than a bare func.
func TestConfigStaysComparable(t *testing.T) {
	first := Config{BaseURL: "https://example.invalid/v1", APIKey: testAPIKey, Model: "test-model"}
	second := Config{BaseURL: "https://example.invalid/v1", APIKey: testAPIKey, Model: "test-model"}
	if first != second {
		t.Error("相同的两份配置不再相等：Config 变成了不可比较的结构")
	}
	if (Config{}) != (Config{}) {
		t.Error("零值比较失败")
	}
	withWrapper := Config{BaseURL: "https://example.invalid/v1", APIKey: testAPIKey, Model: "m",
		WrapTransport: identityWrapper()}
	if withWrapper == (Config{}) {
		t.Error("带包装的配置被当成了零值")
	}
}
