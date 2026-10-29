package modelcall

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
)

// lineWriter keeps every Write call whole and records the lines it was given. The
// sink must not interleave two records, and it must not lose one under
// concurrency, so the writer itself is the witness.
type lineWriter struct {
	mu      sync.Mutex
	lines   []string
	fail    error
	panicOn bool
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.panicOn {
		panic("writer exploded mid-write")
	}
	if w.fail != nil {
		return 0, w.fail
	}
	w.lines = append(w.lines, strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

func (w *lineWriter) all() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]string, len(w.lines))
	copy(out, w.lines)
	return out
}

// TestConcurrentSinkWritesWholeRecords is the regression for the shared
// json.Encoder: concurrent Encode calls on a writer that returns an error raced on
// the encoder's sticky error field, and an unguarded encoder also lets two
// records interleave when the writer is not atomic per call.
func TestConcurrentSinkWritesWholeRecords(t *testing.T) {
	writer := &lineWriter{}
	publisher := NewPublisher(true, NewJSONSink(writer))

	var group sync.WaitGroup
	for index := range 32 {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			ctx, handle := publisher.Start(t.Context(), "message_send")
			collector, ok := FromContext(ctx)
			if !ok {
				t.Errorf("task %d had no collector", index)
				handle.Seal(OutcomeOther, "test", "")
				return
			}
			call := collector.Begin(ctx, KindComplete)
			call.Dispatch()
			call.NoteUsage(Usage{Total: Field{Value: int64(index), Reported: true}})
			call.Complete()
			handle.NoteAnswerStored(true)
			handle.Seal(OutcomeOK, "", "")
		}(index)
	}
	group.Wait()

	lines := writer.all()
	if len(lines) != 32 {
		t.Fatalf("lines = %d, want 32 whole records", len(lines))
	}
	seen := map[string]bool{}
	for _, line := range lines {
		if !strings.HasPrefix(line, "{") || !strings.HasSuffix(line, "}") {
			t.Fatalf("a line is not a whole JSON object, so two records interleaved: %.120s", line)
		}
		var decoded TaskRecord
		if err := json.Unmarshal([]byte(line), &decoded); err != nil {
			t.Fatalf("line did not parse: %v\n%.200s", err, line)
		}
		if decoded.TaskID == "" || seen[decoded.TaskID] {
			t.Fatalf("task id %q is empty or repeated across lines", decoded.TaskID)
		}
		seen[decoded.TaskID] = true
		if decoded.Totals.Calls != 1 {
			t.Fatalf("task %s reports %+v, want its own single call", decoded.TaskID, decoded.Calls)
		}
	}
	if stats := publisher.Stats(); stats.Published != 32 || stats.Failed != 0 {
		t.Fatalf("stats = %+v, want 32 published and no failures", stats)
	}
}

// TestConcurrentSinkErrorsAreCountedWithoutARace is the shape the reviewer
// reproduced under -race: every write fails, so the encoder's sticky error is
// touched by every goroutine at once.
func TestConcurrentSinkErrorsAreCountedWithoutARace(t *testing.T) {
	writer := &lineWriter{fail: errors.New("disk full")}
	publisher := NewPublisher(true, NewJSONSink(writer))

	var group sync.WaitGroup
	for range 24 {
		group.Add(1)
		go func() {
			defer group.Done()
			_, handle := publisher.Start(t.Context(), "message_send")
			handle.Seal(OutcomeOK, "", "")
		}()
	}
	group.Wait()

	if lines := writer.all(); len(lines) != 0 {
		t.Fatalf("the failing writer was handed %d records", len(lines))
	}
	stats := publisher.Stats()
	if stats.Published != 0 || stats.Failed != 24 || stats.LastFailure != "sink_error" {
		t.Fatalf("stats = %+v, want 24 failures counted and categorised", stats)
	}
}

// TestSinkWriterPanicIsIsolated checks the other half of the fix: the lock is
// released by defer even when the writer panics, so one bad line cannot wedge
// every later operation.
func TestSinkWriterPanicIsIsolated(t *testing.T) {
	writer := &lineWriter{panicOn: true}
	publisher := NewPublisher(true, NewJSONSink(writer))

	_, banged := publisher.Start(t.Context(), "message_send")
	banged.Seal(OutcomeOK, "", "")
	if stats := publisher.Stats(); stats.Failed != 1 || stats.LastFailure != "sink_panic" {
		t.Fatalf("stats = %+v, want the panic contained and categorised", stats)
	}

	// Same publisher, same sink: a later task must still reach the writer.
	writer.mu.Lock()
	writer.panicOn = false
	writer.mu.Unlock()
	_, after := publisher.Start(t.Context(), "message_send")
	after.Seal(OutcomeOK, "", "")
	written := writer.all()
	if len(written) != 1 {
		t.Fatalf("lines = %d, want the later record written after the panic", len(written))
	}
	if !strings.Contains(written[0], `"task_id"`) {
		t.Fatalf("the later line is not a task record: %.80s", written[0])
	}
}

// TestSinkWriterPanicWithPartialBytesStillReleasesLock covers a writer that takes
// bytes and then explodes: the encoder has consumed the record, and the next one
// must not be blocked behind a held lock.
func TestSinkWriterPanicWithPartialBytesStillReleasesLock(t *testing.T) {
	var buffer bytes.Buffer
	calls := 0
	writes := func(p []byte) (int, error) {
		calls++
		if calls == 1 {
			buffer.Write(p)
			panic("exploded after taking the bytes")
		}
		buffer.Write(p)
		return len(p), nil
	}
	sink := NewJSONSink(writerFunc(writes))
	publisher := NewPublisher(true, sink)

	_, first := publisher.Start(t.Context(), "message_send")
	first.Seal(OutcomeOK, "", "")
	_, second := publisher.Start(t.Context(), "message_send")
	second.Seal(OutcomeOK, "", "")

	if stats := publisher.Stats(); stats.Failed != 1 || stats.Published != 1 {
		t.Fatalf("stats = %+v, want the panic isolated and the next write to succeed", stats)
	}
	if got := strings.Count(buffer.String(), `"task_id"`); got != 2 {
		t.Fatalf("records in the buffer = %d, want both lines present: %s", got, buffer.String())
	}
}

// writerFunc adapts a function to io.Writer for the tests above.
type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
