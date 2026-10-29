package openai

import (
	"context"
	"errors"
	"io"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/model"
)

// stalledHandler opens an event stream, optionally emits a few events, then goes
// silent for holdBeforeReturn without ever sending [DONE]. The bounded hold keeps
// the test server's shutdown from waiting on a handler that will never be woken,
// which is exactly the situation the idle budget exists to escape.
func stalledHandler(events int, hold time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}
		flusher.Flush()
		for index := 0; index < events; index++ {
			_, _ = io.WriteString(w, sse(chunkDelta("片段")))
			flusher.Flush()
		}
		time.Sleep(hold)
	}
}

// TestStreamStallsAfterHeadersFailsAsTimeout is the leak this fix removes: a
// provider that sends response headers and then nothing at all used to hold the
// request open indefinitely.
func TestStreamStallsAfterHeadersFailsAsTimeout(t *testing.T) {
	client, _, hits := scriptedClientWith(t, Config{Timeout: 150 * time.Millisecond}, stalledHandler(0, 900*time.Millisecond))
	started := time.Now()
	completion, err := client.Stream(context.Background(), oneUserRequest(), func(string) error {
		t.Error("a stalled stream delivered answer text")
		return nil
	})
	if !errors.Is(err, model.ErrTimeout) {
		t.Fatalf("error = %v, want model.ErrTimeout", err)
	}
	if completion.Content != "" {
		t.Fatalf("completion = %#v, want nothing from a stalled stream", completion)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("the stalled call took %v, so the idle budget did not fire", elapsed)
	}
	if hits.Load() != 1 {
		t.Fatalf("one stalled stream cost %d calls, want no retry", hits.Load())
	}
}

// TestStreamStallsAfterFirstDeltaFailsAsTimeout covers the more convincing case:
// data flowed, then stopped. The deltas already handed to the caller stay handed
// over - the SSE contract has no way to unsend them - but no answer is reported,
// so nothing half-finished can be stored.
func TestStreamStallsAfterFirstDeltaFailsAsTimeout(t *testing.T) {
	client, _, _ := scriptedClientWith(t, Config{Timeout: 150 * time.Millisecond}, stalledHandler(2, 900*time.Millisecond))
	var received []string
	_, err := client.Stream(context.Background(), oneUserRequest(), func(delta string) error {
		received = append(received, delta)
		return nil
	})
	if !errors.Is(err, model.ErrTimeout) {
		t.Fatalf("error = %v, want model.ErrTimeout", err)
	}
	if len(received) == 0 {
		t.Fatal("the deltas delivered before the stall were withheld from the caller")
	}
}

// TestStreamThatKeepsTalkingIsNotCutOff is the other half of an idle budget: total
// duration must never matter, or a long answer would be killed mid-sentence.
func TestStreamThatKeepsTalkingIsNotCutOff(t *testing.T) {
	const idle = 200 * time.Millisecond
	const gap = 40 * time.Millisecond
	const events = 12
	client, _, hits := scriptedClientWith(t, Config{Timeout: idle}, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		flusher.Flush()
		for index := 0; index < events; index++ {
			_, _ = io.WriteString(w, sse(chunkDelta("段")))
			flusher.Flush()
			time.Sleep(gap)
		}
		_, _ = io.WriteString(w, sse(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`, "[DONE]"))
	})
	started := time.Now()
	var received int
	completion, err := client.Stream(context.Background(), oneUserRequest(), func(string) error {
		received++
		return nil
	})
	if err != nil {
		t.Fatalf("a stream that produced data every %v hit an idle budget of %v: %v", gap, idle, err)
	}
	if received != events {
		t.Fatalf("deltas = %d, want %d", received, events)
	}
	if completion.FinishReason != model.FinishStop {
		t.Fatalf("finish = %q", completion.FinishReason)
	}
	if elapsed := time.Since(started); elapsed < idle {
		t.Fatalf("the stream finished in %v, below the idle budget of %v, so the test proved nothing", elapsed, idle)
	}
	if hits.Load() != 1 {
		t.Fatalf("a long stream was re-dialed %d times", hits.Load())
	}
}

// TestCallerCancelWinsOverTheIdleBudget keeps the two failure kinds apart: a user
// hanging up is not an outage, and a service that reported a timeout for every
// disconnected browser would hide the real reason.
func TestCallerCancelWinsOverTheIdleBudget(t *testing.T) {
	client, _, _ := scriptedClientWith(t, Config{Timeout: 10 * time.Second}, stalledHandler(1, 900*time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(120 * time.Millisecond)
		cancel()
	}()
	_, err := client.Stream(ctx, oneUserRequest(), func(string) error { return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled, not a timeout", err)
	}
	if errors.Is(err, model.ErrTimeout) {
		t.Fatalf("a caller cancellation was classified as %v", err)
	}
}

// TestIdleTimeoutLeaksNoGoroutines runs the stall repeatedly and checks the process
// settles back down. Each stall arms a timer and cancels a derived context; a
// forgotten Stop or an abandoned body would show up here as drift.
func TestIdleTimeoutLeaksNoGoroutines(t *testing.T) {
	client, _, _ := scriptedClientWith(t, Config{Timeout: 60 * time.Millisecond}, stalledHandler(0, 120*time.Millisecond))
	warm := func() {
		if _, err := client.Stream(context.Background(), oneUserRequest(), func(string) error { return nil }); !errors.Is(err, model.ErrTimeout) {
			t.Fatalf("error = %v, want the stall to time out", err)
		}
	}
	warm()
	warm()
	time.Sleep(200 * time.Millisecond)
	baseline := runtime.NumGoroutine()

	for round := 0; round < 25; round++ {
		warm()
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baseline+2 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("goroutines grew from %d to %d across 25 stalled streams", baseline, runtime.NumGoroutine())
}

// TestIdleBudgetDisabledForNonPositiveTimeout documents the escape hatch the parser
// keeps: without a configured timeout the client must not invent one. Config
// validation keeps this reachable only through a hand-built Config, which is what
// a test harness does.
func TestIdleBudgetIsGuardedByTheConfiguredTimeout(t *testing.T) {
	body := sse(chunkDelta("一段"), `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`, "[DONE]")
	client, _, _ := scriptedClientWith(t, Config{}, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, body)
	})
	idle := newIdleReader(strings.NewReader(body), 0, func() { t.Error("a trip fired with no budget") })
	if _, err := client.consumeStream(context.Background(), idle, http.StatusOK, func(string) error { return nil }, nil); err != nil {
		t.Fatalf("error = %v, want the stream to complete with the idle watch off", err)
	}
	if idle.TimedOut() {
		t.Fatal("the idle watch reported a timeout it was never given")
	}
}
