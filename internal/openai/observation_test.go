package openai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/model"
	"github.com/xiaopang456/GopherLearn/internal/modelcall"
)

// completionWith is a provider body carrying exactly the usage text it is given,
// so a test can name the shape the provider sent rather than describe a struct.
func completionWith(usage string) string {
	body := `{"model":"answered-model","choices":[{"message":{"role":"assistant","content":"回答"},"finish_reason":"stop"}]}`
	if usage == "" {
		return body
	}
	return strings.TrimSuffix(body, "}") + `,"usage":` + usage + "}"
}

func recorded(t *testing.T, collector *modelcall.Collector) modelcall.Call {
	t.Helper()
	snapshot := collector.Snapshot()
	if len(snapshot) != 1 {
		t.Fatalf("records = %d, want exactly one per exported call: %+v", len(snapshot), snapshot)
	}
	return snapshot[0]
}

func chatRequest() model.Request {
	return model.Request{Messages: []model.Message{{Role: model.RoleUser, Content: "问题"}}}
}

func observing(t *testing.T) (*modelcall.Collector, context.Context) {
	t.Helper()
	collector := modelcall.New()
	return collector, modelcall.WithCollector(context.Background(), collector)
}

// TestRecordsEveryUsageShape is the core of the task: five shapes a provider can
// send, and none of them may be read as another one.
func TestRecordsEveryUsageShape(t *testing.T) {
	for _, testCase := range []struct {
		name           string
		usage          string
		wantPrompt     modelcall.Field
		wantCompletion modelcall.Field
		wantTotal      modelcall.Field
		wantFrames     int
	}{
		{
			name: "absent key", usage: "",
			wantPrompt: modelcall.Field{}, wantCompletion: modelcall.Field{}, wantTotal: modelcall.Field{},
		},
		{
			name: "explicit null", usage: `null`,
			wantPrompt: modelcall.Field{}, wantCompletion: modelcall.Field{}, wantTotal: modelcall.Field{},
		},
		{
			name: "empty object", usage: `{}`,
			wantPrompt: modelcall.Field{}, wantCompletion: modelcall.Field{}, wantTotal: modelcall.Field{},
		},
		{
			name: "reported zeros", usage: `{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}`,
			wantPrompt:     modelcall.Field{Value: 0, Reported: true},
			wantCompletion: modelcall.Field{Value: 0, Reported: true},
			wantTotal:      modelcall.Field{Value: 0, Reported: true},
			wantFrames:     1,
		},
		{
			name: "partial fields", usage: `{"prompt_tokens":11,"completion_tokens":7}`,
			wantPrompt:     modelcall.Field{Value: 11, Reported: true},
			wantCompletion: modelcall.Field{Value: 7, Reported: true},
			wantTotal:      modelcall.Field{},
			wantFrames:     1,
		},
		{
			name: "normal", usage: `{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}`,
			wantPrompt:     modelcall.Field{Value: 11, Reported: true},
			wantCompletion: modelcall.Field{Value: 7, Reported: true},
			wantTotal:      modelcall.Field{Value: 18, Reported: true},
			wantFrames:     1,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			collector, ctx := observing(t)
			client, _, hits := scriptedClient(t, 2*time.Second, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, completionWith(testCase.usage))
			})

			if _, err := client.Complete(ctx, chatRequest()); err != nil {
				t.Fatalf("Complete: %v", err)
			}
			record := recorded(t, collector)
			if record.Usage.Prompt != testCase.wantPrompt ||
				record.Usage.Completion != testCase.wantCompletion ||
				record.Usage.Total != testCase.wantTotal {
				t.Fatalf("recorded usage = %+v, want prompt %+v completion %+v total %+v",
					record.Usage, testCase.wantPrompt, testCase.wantCompletion, testCase.wantTotal)
			}
			if record.UsageFrames != testCase.wantFrames {
				t.Fatalf("usage frames = %d, want %d", record.UsageFrames, testCase.wantFrames)
			}
			if record.Kind != modelcall.KindComplete || record.Outcome != modelcall.OutcomeOK {
				t.Fatalf("record = %+v, want an ok complete call", record)
			}
			if !record.HTTPDispatched || int(hits.Load()) != 1 {
				t.Fatalf("dispatched = %v, transport hits = %d, want true and 1", record.HTTPDispatched, hits.Load())
			}
		})
	}
}

// TestBackwardShapeOfDomainUsageIsUnchanged keeps the promise that existing
// consumers still see the same model.Usage they saw before: absent reads as zero
// there, while the honest answer lives in the record.
func TestBackwardShapeOfDomainUsageIsUnchanged(t *testing.T) {
	collector, ctx := observing(t)
	client, _, _ := scriptedClient(t, 2*time.Second, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, completionWith(`{}`))
	})

	completion, err := client.Complete(ctx, chatRequest())
	if err != nil {
		t.Fatal(err)
	}
	if completion.Usage != (model.Usage{}) {
		t.Fatalf("domain usage = %+v, want the previous all-zero shape preserved", completion.Usage)
	}
	if collector.Snapshot()[0].Usage.AnyPresent() {
		t.Fatal("an empty usage object was recorded as if it were reported")
	}
}

// TestCachedTokensStayBesideTheTotals: a cache hit is a discount on prompt tokens
// already counted, never extra tokens.
func TestCachedTokensStayBesideTheTotals(t *testing.T) {
	for _, testCase := range []struct {
		name string
		body string
		want modelcall.Field
	}{
		{"absent", `{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120}`, modelcall.Field{}},
		{"reported zero", `{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"prompt_tokens_details":{"cached_tokens":0}}`,
			modelcall.Field{Value: 0, Reported: true}},
		{"reported hit", `{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"prompt_tokens_details":{"cached_tokens":64}}`,
			modelcall.Field{Value: 64, Reported: true}},
		{"details without the field", `{"prompt_tokens":100,"prompt_tokens_details":{}}`, modelcall.Field{}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			collector, ctx := observing(t)
			client, _, _ := scriptedClient(t, 2*time.Second, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, completionWith(testCase.body))
			})
			if _, err := client.Complete(ctx, chatRequest()); err != nil {
				t.Fatal(err)
			}

			usage := recorded(t, collector).Usage
			if usage.Cached != testCase.want {
				t.Fatalf("cached = %+v, want %+v", usage.Cached, testCase.want)
			}
			if usage.Prompt.Value != 100 {
				t.Fatalf("prompt = %d, want the reported 100 untouched by any cache figure", usage.Prompt.Value)
			}
			if usage.Total.Value != 120 && usage.Total.Reported {
				t.Fatalf("total = %d, want the provider's own figure only", usage.Total.Value)
			}
		})
	}
}

// TestStreamSnapshotsAreNotAddedFrameByFrame: providers repeat a cumulative
// snapshot, so the newest figure is the call's figure and repeats only count.
func TestStreamSnapshotsAreNotAddedFrameByFrame(t *testing.T) {
	collector, ctx := observing(t)
	client, _, _ := scriptedClient(t, 2*time.Second, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sse(
			chunkDelta("第"),
			`{"choices":[{"index":0,"delta":{"content":"一段"}}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
		)+"data: [DONE]\n\n")
	})

	var fragments strings.Builder
	if _, err := client.Stream(ctx, chatRequest(), func(delta string) error {
		fragments.WriteString(delta)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	record := recorded(t, collector)
	if record.Usage.Prompt.Value != 10 || record.Usage.Completion.Value != 5 || record.Usage.Total.Value != 15 {
		t.Fatalf("recorded usage = %+v, want the final snapshot 10/5/15 rather than a frame-by-frame sum", record.Usage)
	}
	if record.UsageFrames != 3 {
		t.Fatalf("usage frames = %d, want the three that repeated", record.UsageFrames)
	}
	totals := collector.Totals()
	if totals.Calls != 1 || totals.Total.Value != 15 {
		t.Fatalf("totals = %+v, want one call worth 15 tokens", totals)
	}
}

// TestUsageSurvivesAnAnswerThatWasRefusedAfterwards is the second half of the
// task's point: a call that was made and paid for stays in the record even when
// this client refuses to hand out an answer for it.
func TestUsageSurvivesAnAnswerThatWasRefusedAfterwards(t *testing.T) {
	t.Run("empty completion", func(t *testing.T) {
		collector, ctx := observing(t)
		client, _, hits := scriptedClient(t, 2*time.Second, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":""},"finish_reason":"stop"}],`+
				`"usage":{"prompt_tokens":400,"completion_tokens":0,"total_tokens":400}}`)
		})
		_, err := client.Complete(ctx, chatRequest())
		if !errors.Is(err, model.ErrEmptyCompletion) {
			t.Fatalf("error = %v, want the empty answer refused", err)
		}

		record := recorded(t, collector)
		if record.Outcome != modelcall.OutcomeEmpty {
			t.Fatalf("outcome = %s, want empty", record.Outcome)
		}
		if record.Usage.Prompt.Value != 400 || !record.Usage.Prompt.Reported {
			t.Fatalf("usage = %+v, want the 400 prompt tokens kept", record.Usage)
		}
		if !record.HTTPDispatched || int(hits.Load()) != 1 {
			t.Fatal("a refused answer still dispatched its request")
		}
	})

	t.Run("malformed tool output", func(t *testing.T) {
		collector, ctx := observing(t)
		client, _, _ := scriptedClient(t, 2*time.Second, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"",`+
				`"tool_calls":[{"id":"call-1","type":"function"}]},"finish_reason":"tool_calls"}],`+
				`"usage":{"prompt_tokens":30,"completion_tokens":3,"total_tokens":33}}`)
		})
		if _, err := client.CompleteWithTools(ctx, toolOfferRequest()); err == nil {
			t.Fatal("a tool call with no function was accepted")
		}

		record := recorded(t, collector)
		if record.Kind != modelcall.KindCompleteWithTools {
			t.Fatalf("kind = %s, want the tool path", record.Kind)
		}
		if record.Outcome == modelcall.OutcomeOK {
			t.Fatal("an invalid tool payload was recorded as a success")
		}
		if record.Usage.Total.Value != 33 {
			t.Fatalf("usage = %+v, want the tokens kept for a call that failed validation", record.Usage)
		}
	})

	t.Run("stream cut off", func(t *testing.T) {
		collector, ctx := observing(t)
		client, _, _ := scriptedClient(t, 2*time.Second, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, sse(
				`{"choices":[{"index":0,"delta":{"content":"半"}}],"usage":{"prompt_tokens":9,"completion_tokens":1,"total_tokens":10}}`,
			))
		})
		_, err := client.Stream(ctx, chatRequest(), func(string) error { return nil })
		if !errors.Is(err, model.ErrTruncatedStream) {
			t.Fatalf("error = %v, want the cut-off stream refused", err)
		}

		record := recorded(t, collector)
		if record.Outcome != modelcall.OutcomeMalformed {
			t.Fatalf("outcome = %s, want malformed", record.Outcome)
		}
		if record.Usage.Total.Value != 10 {
			t.Fatalf("usage = %+v, want the figures read before the cut-off kept", record.Usage)
		}
	})

	t.Run("consumer rejected the fragment", func(t *testing.T) {
		collector, ctx := observing(t)
		client, _, _ := scriptedClient(t, 2*time.Second, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, sse(
				`{"choices":[{"index":0,"delta":{"content":"第一"}}],"usage":{"prompt_tokens":7,"completion_tokens":1,"total_tokens":8}}`,
				chunkDelta("第二"),
			))
		})
		sent := false
		_, err := client.Stream(ctx, chatRequest(), func(string) error {
			if sent {
				return errors.New("handler gave up")
			}
			sent = true
			return nil
		})
		if err == nil || !strings.Contains(err.Error(), "handler gave up") {
			t.Fatalf("error = %v, want the consumer's own failure returned unchanged", err)
		}

		record := recorded(t, collector)
		if record.Outcome != modelcall.OutcomeCallbackFailed {
			t.Fatalf("outcome = %s, want callback_failed rather than a provider fault", record.Outcome)
		}
		if record.Usage.Total.Value != 8 {
			t.Fatalf("usage = %+v, want the tokens from the accepted fragment kept", record.Usage)
		}
	})
}

// TestDispatchBoundary is explicit about what "dispatched" means: a request was
// handed to the transport. Refusals and a context already dead never reach it.
func TestDispatchBoundary(t *testing.T) {
	t.Run("refused before the wire", func(t *testing.T) {
		collector, ctx := observing(t)
		client, _, hits := scriptedClient(t, 2*time.Second, func(w http.ResponseWriter, r *http.Request) {
			t.Error("a refused request must not reach the provider")
		})
		// Complete with tools offered is invalid by contract, checked before any body.
		_, err := client.Complete(ctx, toolOfferRequest())
		if !errors.Is(err, model.ErrInvalidRequest) {
			t.Fatalf("error = %v, want the request refused", err)
		}

		record := recorded(t, collector)
		if record.HTTPDispatched || record.UsageFrames != 0 {
			t.Fatalf("record = %+v, want nothing dispatched", record)
		}
		if record.Outcome != modelcall.OutcomeRefused {
			t.Fatalf("outcome = %s, want refused", record.Outcome)
		}
		if hits.Load() != 0 {
			t.Fatalf("transport hits = %d, want 0 for a call rejected before the wire", hits.Load())
		}
	})

	t.Run("context already cancelled", func(t *testing.T) {
		collector := modelcall.New()
		cancelled, cancel := context.WithCancel(context.Background())
		cancel()
		client, _, hits := scriptedClient(t, 2*time.Second, func(w http.ResponseWriter, r *http.Request) {
			t.Error("a cancelled context must not dispatch")
		})
		if _, err := client.Complete(modelcall.WithCollector(cancelled, collector), chatRequest()); err == nil {
			t.Fatal("a cancelled context completed anyway")
		}
		record := recorded(t, collector)
		if record.HTTPDispatched || record.Outcome != modelcall.OutcomeCancelled {
			t.Fatalf("record = %+v, want cancelled and undispatched", record)
		}
		if hits.Load() != 0 {
			t.Fatalf("transport hits = %d, want 0", hits.Load())
		}
	})

	t.Run("provider status", func(t *testing.T) {
		collector, ctx := observing(t)
		client, _, hits := scriptedClient(t, 2*time.Second, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":{"code":"overloaded","type":"server_error"}}`)
		})
		_, err := client.Complete(ctx, chatRequest())
		if !errors.Is(err, model.ErrProviderStatus) {
			t.Fatalf("error = %v, want the provider status", err)
		}
		record := recorded(t, collector)
		if !record.HTTPDispatched || record.Outcome != modelcall.OutcomeProviderError {
			t.Fatalf("record = %+v, want dispatched and provider_error", record)
		}
		if hits.Load() != 1 {
			t.Fatalf("transport hits = %d, want the one dispatch recorded", hits.Load())
		}
	})

	t.Run("transport failure after dispatch", func(t *testing.T) {
		collector, ctx := observing(t)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Hijack and close without a response: the request left, the answer never came.
			hijacked, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Fatal(err)
			}
			_ = hijacked.Close()
		}))
		t.Cleanup(server.Close)
		client, err := New(Config{BaseURL: server.URL, APIKey: testAPIKey, Model: "test-model"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.Complete(ctx, chatRequest()); err == nil {
			t.Fatal("a severed connection was reported as an answer")
		}
		record := recorded(t, collector)
		if !record.HTTPDispatched {
			t.Fatal("a request that reached the transport was recorded as undispatched")
		}
		if record.Outcome != modelcall.OutcomeTransport && record.Outcome != modelcall.OutcomeOther {
			t.Fatalf("outcome = %s, want a transport-class failure", record.Outcome)
		}
		if record.Usage.AnyPresent() {
			t.Fatal("a call that never got a body reported tokens")
		}
	})
}

// TestOneRecordPerExportedCall proves the shared internal body does not bill the
// same call twice, and that each face is named.
func TestOneRecordPerExportedCall(t *testing.T) {
	collector, ctx := observing(t)
	client, _, hits := scriptedClient(t, 2*time.Second, func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Accept") {
		case "text/event-stream":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, sse(
				`{"choices":[{"index":0,"delta":{"content":"流"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`,
			)+"data: [DONE]\n\n")
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, completionWith(`{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}`))
		}
	})

	if _, err := client.Complete(ctx, chatRequest()); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CompleteWithTools(ctx, toolOfferRequest()); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Stream(ctx, chatRequest(), func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := client.StreamWithTools(ctx, toolOfferRequest(), func(model.StreamDelta) error { return nil }); err != nil {
		t.Fatal(err)
	}

	snapshot := collector.Snapshot()
	if len(snapshot) != 4 {
		t.Fatalf("records = %d, want one per exported call, got %+v", len(snapshot), snapshot)
	}
	want := []modelcall.Kind{
		modelcall.KindComplete, modelcall.KindCompleteWithTools,
		modelcall.KindStream, modelcall.KindStreamWithTools,
	}
	for index, record := range snapshot {
		if record.Kind != want[index] {
			t.Fatalf("record %d kind = %s, want %s", index, record.Kind, want[index])
		}
		if record.Seq != index+1 {
			t.Fatalf("record %d seq = %d, want %d", index, record.Seq, index+1)
		}
	}
	totals := collector.Totals()
	// Two plain + one streamed answer, all four calls reported usage; the sums come
	// from the reported figures only: 1+1+2+prompt of the tool paths.
	if totals.Calls != 4 || totals.Status != modelcall.StatusFullUsage {
		t.Fatalf("totals = %+v, want four calls with full usage", totals)
	}
	if totals.Dispatches != 4 {
		t.Fatalf("dispatches = %d, want all four calls to have reached the transport", totals.Dispatches)
	}
	// The transport saw the same number of requests the records claim: one per
	// exported call, none double-counted by the shared internal body.
	if hits.Load() != 4 {
		t.Fatalf("transport hits = %d, want 4", hits.Load())
	}
}

// TestRollUpOfSeveralCalls covers the three reporting states a group can be in,
// because a partial group must never be printed as a total.
func TestRollUpOfSeveralCalls(t *testing.T) {
	collector, ctx := observing(t)
	replies := []string{
		completionWith(`{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}`),
		completionWith(`{"prompt_tokens":10}`),
		completionWith(""),
	}
	var next atomic.Int64
	client, _, _ := scriptedClient(t, 2*time.Second, func(w http.ResponseWriter, r *http.Request) {
		index := min(int(next.Add(1))-1, len(replies)-1)
		_, _ = io.WriteString(w, replies[index])
	})

	for range 3 {
		if _, err := client.Complete(ctx, chatRequest()); err != nil {
			t.Fatal(err)
		}
	}

	totals := collector.Totals()
	if totals.Status != modelcall.StatusPartialUsage {
		t.Fatalf("status = %s, want partial_usage", totals.Status)
	}
	if totals.Prompt.Value != 20 || totals.Prompt.Contributing != 2 || totals.Prompt.Missing != 1 {
		t.Fatalf("prompt roll-up = %+v, want 20 from two calls with one missing", totals.Prompt)
	}
	if totals.Total.Value != 15 || totals.Total.Contributing != 1 || totals.Total.Missing != 2 {
		t.Fatalf("total roll-up = %+v, want 15 from one call with two missing", totals.Total)
	}
	if totals.Prompt.Complete() {
		t.Fatal("a group with a missing call reported itself complete")
	}
}

// TestCollectorsDoNotLeakAcrossRequests: two scopes, two collectors, one client.
func TestCollectorsDoNotLeakAcrossRequests(t *testing.T) {
	client, _, _ := scriptedClient(t, 5*time.Second, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, completionWith(`{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}`))
	})

	var group sync.WaitGroup
	first := modelcall.New()
	second := modelcall.New()
	run := func(collector *modelcall.Collector, value int64) {
		group.Add(1)
		go func() {
			defer group.Done()
			ctx := modelcall.WithCollector(context.Background(), collector)
			for range 8 {
				if _, err := client.Complete(ctx, chatRequest()); err != nil {
					t.Errorf("Complete: %v", err)
				}
			}
			totals := collector.Totals()
			if totals.Calls != 8 {
				t.Errorf("collector %d holds %d calls, want its own 8", value, totals.Calls)
			}
			if totals.Prompt.Value != 24 {
				t.Errorf("collector %d prompt sum = %d, want 24 with nothing lost or doubled", value, totals.Prompt.Value)
			}
		}()
	}
	run(first, 1)
	run(second, 2)
	group.Wait()
}

// TestWithoutACollectorNothingChanges pins the compatibility promise: the same
// bytes go out and the same answer comes back when nobody is watching.
func TestWithoutACollectorNothingChanges(t *testing.T) {
	var bodies []string
	client, _, _ := scriptedClient(t, 2*time.Second, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(raw))
		_, _ = io.WriteString(w, completionWith(`{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}`))
	})

	// Unwatched calls.
	for range 2 {
		if _, err := client.Complete(context.Background(), chatRequest()); err != nil {
			t.Fatal(err)
		}
	}
	// The same call with a collector attached.
	collector := modelcall.New()
	if _, err := client.Complete(modelcall.WithCollector(context.Background(), collector), chatRequest()); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 3 {
		t.Fatalf("bodies = %d, want three requests", len(bodies))
	}
	if bodies[0] != bodies[1] || bodies[1] != bodies[2] {
		t.Fatalf("observation changed the request body:\n%q\n%q\n%q", bodies[0], bodies[1], bodies[2])
	}
	if len(collector.Snapshot()) != 1 {
		t.Fatalf("records = %d, want only the watched call", len(collector.Snapshot()))
	}

	// And the body still carries exactly the keys it carried before this package
	// existed: no new knob, no request for behaviour the provider did not offer.
	var payload map[string]any
	if err := json.Unmarshal([]byte(bodies[0]), &payload); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(payload))
	for key := range payload {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	// The shape a plain completion has always sent: the model, the messages, the
	// output budget, and the always-present stream flag. temperature and
	// enable_thinking stay off the wire when unset, and tools stay off when none
	// were offered.
	want := []string{"max_tokens", "messages", "model", "stream"}
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Fatalf("request keys = %v, want exactly %v (a plain completion offers no tools)", keys, want)
	}
	for _, body := range bodies {
		if strings.Contains(body, "stream_options") || strings.Contains(body, "cached_tokens") {
			t.Fatalf("this round requests no new provider behaviour, body = %s", body)
		}
	}
}
