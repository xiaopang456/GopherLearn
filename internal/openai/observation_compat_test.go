package openai

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/model"
	"github.com/xiaopang456/GopherLearn/internal/modelcall"
)

// The exact response the review reported. An optional cache figure the client
// cannot read must not cost the learner their answer.
const cachedTokensAsStringBody = `{"model":"answered-model","choices":[{"message":{"role":"assistant","content":"回答"},"finish_reason":"stop"}],` +
	`"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":"64"}}}`

// completionWithUsage embeds any usage object verbatim so a test can name the
// shape the provider sent rather than describe a Go struct.
func completionWithUsage(usage string) string {
	return `{"model":"answered-model","choices":[{"message":{"role":"assistant","content":"回答"},"finish_reason":"stop"}]` +
		`,"usage":` + usage + `}`
}

// TestUnreadableCacheDetailNeverBreaksTheAnswer is the compatibility rule: the
// optional observation field is best effort, and every failure mode of it leaves
// the answer path exactly as it was.
func TestUnreadableCacheDetailNeverBreaksTheAnswer(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		usage      string
		wantCached modelcall.Field
	}{
		{"string instead of number", `{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":"64"}}`, modelcall.Field{}},
		{"bool instead of number", `{"prompt_tokens":10,"prompt_tokens_details":{"cached_tokens":true}}`, modelcall.Field{}},
		{"float", `{"prompt_tokens":10,"prompt_tokens_details":{"cached_tokens":6.5}}`, modelcall.Field{}},
		{"details is a string", `{"prompt_tokens":10,"prompt_tokens_details":"opaque"}`, modelcall.Field{}},
		{"details is an array", `{"prompt_tokens":10,"prompt_tokens_details":[{"cached_tokens":1}]}`, modelcall.Field{}},
		{"details is a number", `{"prompt_tokens":10,"prompt_tokens_details":7}`, modelcall.Field{}},
		{"details null", `{"prompt_tokens":10,"prompt_tokens_details":null}`, modelcall.Field{}},
		{"details absent", `{"prompt_tokens":10}`, modelcall.Field{}},
		{"inner field absent", `{"prompt_tokens":10,"prompt_tokens_details":{"other":1}}`, modelcall.Field{}},
		{"inner field null", `{"prompt_tokens":10,"prompt_tokens_details":{"cached_tokens":null}}`, modelcall.Field{}},
		{"valid integer", `{"prompt_tokens":10,"prompt_tokens_details":{"cached_tokens":64}}`, modelcall.Field{Value: 64, Reported: true}},
		{"valid zero", `{"prompt_tokens":10,"prompt_tokens_details":{"cached_tokens":0}}`, modelcall.Field{Value: 0, Reported: true}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			body := completionWithUsage(testCase.usage)

			// First without any collector: this is the production shape, and it
			// must not know or care that observation exists.
			plain, _, _ := scriptedClient(t, 2*time.Second, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, body)
			})
			completion, err := plain.Complete(context.Background(), chatRequest())
			if err != nil {
				t.Fatalf("no collector: Complete failed for an optional cache field: %v", err)
			}
			if completion.Content != "回答" {
				t.Fatalf("no collector: content = %q, want the answer untouched", completion.Content)
			}

			// Then with a collector: the three base figures keep their own
			// behaviour, and the cache figure is either reported or unknown - a
			// string is never coerced into a number.
			collector := modelcall.New()
			client, _, _ := scriptedClient(t, 2*time.Second, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, body)
			})
			watched, err := client.Complete(modelcall.WithCollector(context.Background(), collector), chatRequest())
			if err != nil {
				t.Fatalf("with collector: Complete failed: %v", err)
			}
			if watched.Content != completion.Content || watched.Usage != completion.Usage ||
				watched.FinishReason != completion.FinishReason || len(watched.ToolCalls) != len(completion.ToolCalls) {
				t.Fatalf("attaching a collector changed the answer: %+v vs %+v", watched, completion)
			}
			record := recorded(t, collector)
			if record.Usage.Cached != testCase.wantCached {
				t.Fatalf("cached = %+v, want %+v", record.Usage.Cached, testCase.wantCached)
			}
			if !record.Usage.Prompt.Reported || record.Usage.Prompt.Value != 10 {
				t.Fatalf("prompt = %+v, want the reported 10 unaffected by the cache field", record.Usage.Prompt)
			}
		})
	}
}

// TestCacheDetailConsistentAcrossAllFourPaths: a reader that only behaves on the
// plain path is not a fix.
func TestCacheDetailConsistentAcrossAllFourPaths(t *testing.T) {
	streamBroken := `{"choices":[{"index":0,"delta":{"content":"回答"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":"64"}}}`

	t.Run("Complete", func(t *testing.T) {
		collector, ctx := observing(t)
		client, _, _ := scriptedClient(t, 2*time.Second, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, cachedTokensAsStringBody)
		})
		if _, err := client.Complete(ctx, chatRequest()); err != nil {
			t.Fatal(err)
		}
		if recorded(t, collector).Usage.Cached.Reported {
			t.Fatal("a string cache figure was recorded as reported")
		}
	})

	t.Run("CompleteWithTools", func(t *testing.T) {
		collector, ctx := observing(t)
		toolBody := strings.Replace(cachedTokensAsStringBody, `"content":"回答"`, `"content":""`, 1)
		client, _, _ := scriptedClient(t, 2*time.Second, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, toolBody)
		})
		// An answer with no tool call is refused by the empty rule; what matters
		// here is that the refusal is the business rule, not a parse crash.
		_, err := client.CompleteWithTools(ctx, toolOfferRequest())
		if err != nil && !errors.Is(err, model.ErrEmptyCompletion) {
			t.Fatalf("error = %v, want the ordinary empty-answer rule", err)
		}
		record := recorded(t, collector)
		if record.Usage.Cached.Reported {
			t.Fatal("a string cache figure was recorded as reported")
		}
		if record.Usage.Total.Value != 15 {
			t.Fatalf("total = %+v, want the base figure kept", record.Usage.Total)
		}
	})

	t.Run("Stream", func(t *testing.T) {
		collector, ctx := observing(t)
		client, _, _ := scriptedClient(t, 2*time.Second, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, sse(streamBroken)+"data: [DONE]\n\n")
		})
		if _, err := client.Stream(ctx, chatRequest(), func(string) error { return nil }); err != nil {
			t.Fatalf("a streamed string cache figure broke the answer: %v", err)
		}
		if recorded(t, collector).Usage.Cached.Reported {
			t.Fatal("a string cache figure was recorded as reported on the stream path")
		}
	})

	t.Run("StreamWithTools", func(t *testing.T) {
		collector, ctx := observing(t)
		client, _, _ := scriptedClient(t, 2*time.Second, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, sse(streamBroken)+"data: [DONE]\n\n")
		})
		_, err := client.StreamWithTools(ctx, toolOfferRequest(), func(model.StreamDelta) error { return nil })
		if err != nil && !errors.Is(err, model.ErrEmptyCompletion) {
			t.Fatalf("error = %v, want the ordinary empty-answer rule, not a parse failure", err)
		}
		record := recorded(t, collector)
		if record.Kind != modelcall.KindStreamWithTools {
			t.Fatalf("kind = %s, want the streamed tool path", record.Kind)
		}
		if record.Usage.Cached.Reported {
			t.Fatal("a string cache figure was recorded as reported on the streamed tool path")
		}
	})
}

// TestBodyProblemsStillReport: making one optional field tolerant must not turn
// into a blanket tolerance for a body that was never readable.
func TestBodyProblemsStillReport(t *testing.T) {
	for _, testCase := range []struct {
		name string
		body string
		want error
	}{
		{"body is not JSON", `{"choices":`, model.ErrMalformedResponse},
		{"trailing content", `{"choices":[{"message":{"content":"x"},"finish_reason":"stop"}]} trailing`, model.ErrMalformedResponse},
		{"no choices", `{"choices":[],"usage":{"prompt_tokens":1}}`, model.ErrMalformedResponse},
		{"empty content", `{"choices":[{"message":{"content":""},"finish_reason":"stop"}]}`, model.ErrEmptyCompletion},
		{"bad tool call shape", `{"choices":[{"message":{"content":"","tool_calls":[{"id":"c1","type":"function"}]},"finish_reason":"tool_calls"}]}`, model.ErrMalformedResponse},
		{"base usage field is a string", `{"choices":[{"message":{"content":"x"},"finish_reason":"stop"}],"usage":{"prompt_tokens":"ten"}}`, model.ErrMalformedResponse},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			client, _, _ := scriptedClient(t, 2*time.Second, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, testCase.body)
			})
			_, err := client.Complete(context.Background(), chatRequest())
			if !errors.Is(err, testCase.want) {
				t.Fatalf("error = %v, want %v", err, testCase.want)
			}
		})
	}
}

// TestCancelledCallbackIsNotLabelledAsAConsumerFault: a client that walked away
// is a cancellation, not a rejected fragment.
func TestCancelledCallbackIsNotLabelledAsAConsumerFault(t *testing.T) {
	frames := sse(
		`{"choices":[{"index":0,"delta":{"content":"第一"},"finish_reason":""}],"usage":{"prompt_tokens":7,"completion_tokens":1,"total_tokens":8}}`,
	) + "data: [DONE]\n\n"
	toolFrames := sse(
		`{"choices":[{"index":0,"delta":{"content":"第一"},"finish_reason":""}],"usage":{"prompt_tokens":7,"completion_tokens":1,"total_tokens":8}}`,
	) + "data: [DONE]\n\n"

	for _, path := range []struct {
		name   string
		frames string
		call   func(ctx context.Context, client *Client, err error) error
	}{
		{
			name:   "Stream",
			frames: frames,
			call: func(ctx context.Context, client *Client, err error) error {
				_, callErr := client.Stream(ctx, chatRequest(), func(string) error { return err })
				return callErr
			},
		},
		{
			name:   "StreamWithTools",
			frames: toolFrames,
			call: func(ctx context.Context, client *Client, err error) error {
				_, callErr := client.StreamWithTools(ctx, toolOfferRequest(), func(model.StreamDelta) error { return err })
				return callErr
			},
		},
	} {
		for _, testCase := range []struct {
			name string
			err  error
			want modelcall.Outcome
		}{
			{"plain cancellation", context.Canceled, modelcall.OutcomeCancelled},
			{"wrapped cancellation", errors.Join(errors.New("browser hung up"), context.Canceled), modelcall.OutcomeCancelled},
			{"deadline", context.DeadlineExceeded, modelcall.OutcomeTimeout},
			{"wrapped timeout", errors.Join(errors.New("write stalled"), model.ErrTimeout), modelcall.OutcomeTimeout},
			{"ordinary consumer fault", errors.New("handler refused"), modelcall.OutcomeCallbackFailed},
		} {
			t.Run(path.name+"/"+testCase.name, func(t *testing.T) {
				collector, ctx := observing(t)
				client, _, _ := scriptedClient(t, 2*time.Second, func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, path.frames)
				})

				callErr := path.call(ctx, client, testCase.err)
				// The caller's error is untouched: labelling a call is not
				// re-deciding what the caller sees.
				if !errors.Is(callErr, testCase.err) {
					t.Fatalf("returned error = %v, want the consumer's own error %v preserved", callErr, testCase.err)
				}

				snapshot := collector.Snapshot()
				if len(snapshot) != 1 {
					t.Fatalf("records = %d, want exactly one per call: %+v", len(snapshot), snapshot)
				}
				record := snapshot[0]
				if record.Outcome != testCase.want {
					t.Fatalf("outcome = %s, want %s", record.Outcome, testCase.want)
				}
				if record.Usage.Total.Value != 8 {
					t.Fatalf("usage = %+v, want the figures read before the callback refused kept", record.Usage)
				}
			})
		}
	}
}
