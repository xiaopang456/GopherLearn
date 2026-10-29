package modelcall

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func countingSink(records *[]TaskRecord, mu *sync.Mutex) Sink {
	return func(record TaskRecord) error {
		mu.Lock()
		defer mu.Unlock()
		*records = append(*records, record)
		return nil
	}
}

func TestDisabledPublisherIsInert(t *testing.T) {
	var writes int
	publisher := NewPublisher(false, func(TaskRecord) error { writes++; return nil })
	if publisher.Enabled() {
		t.Fatal("a disabled publisher reported itself enabled")
	}
	ctx, handle := publisher.Start(context.Background(), "message_send")
	if handle != nil {
		t.Fatal("a disabled publisher handed out a handle")
	}
	if collector, ok := FromContext(ctx); ok || collector != nil {
		t.Fatal("a disabled publisher attached a collector")
	}
	handle.NoteFirstEvent()
	handle.NoteFirstText("block")
	handle.NoteAnswerStored(true)
	if handle.DeliveryFailed() {
		t.Fatal("a nil handle reported a delivery failure")
	}
	handle.Seal(OutcomeOK, "", "")
	if writes != 0 {
		t.Fatalf("sink saw %d writes with metering off, want none", writes)
	}
}

// TestOneTaskWritesExactlyOneRecord: the deferred seal is a safety net, not a
// second line.
func TestOneTaskWritesExactlyOneRecord(t *testing.T) {
	var records []TaskRecord
	var mu sync.Mutex
	publisher := NewPublisher(true, countingSink(&records, &mu))
	_, handle := publisher.Start(context.Background(), "quiz_start")

	handle.Seal(OutcomeOK, "", "")
	handle.Seal(OutcomeTimeout, "model", "timeout")

	if len(records) != 1 {
		t.Fatalf("records = %d, want exactly one per task", len(records))
	}
	if records[0].Outcome != OutcomeOK || records[0].FailureStage != "" {
		t.Fatalf("record = %+v, want the first seal to win", records[0])
	}
	if records[0].TaskID == "" || !strings.HasPrefix(records[0].TaskID, "quiz_start-") {
		t.Fatalf("task id = %q, want the operation named inside it", records[0].TaskID)
	}
	if records[0].Cost != "unknown" {
		t.Fatalf("cost = %q, want unknown", records[0].Cost)
	}
}

func TestStreamTimingsAreRecordedOnceAndNeverInvented(t *testing.T) {
	now := time.Unix(0, 0)
	var records []TaskRecord
	var mu sync.Mutex
	publisher := NewPublisher(true, countingSink(&records, &mu)).WithClock(func() time.Time {
		return now
	})
	_, handle := publisher.Start(context.Background(), "message_stream")

	now = now.Add(120 * time.Millisecond)
	handle.NoteFirstEvent()
	now = now.Add(80 * time.Millisecond)
	handle.NoteFirstEvent()
	now = now.Add(400 * time.Millisecond)
	handle.NoteFirstText("block-a")
	handle.NoteFirstText("block-b")
	now = now.Add(100 * time.Millisecond)
	// The first text came from block-a and the turn confirmed block-a, so the
	// "was that the answer?" flag can be answered honestly at seal time.
	handle.NoteConfirmedBlock("block-a")
	handle.Seal(OutcomeOK, "", "")

	if len(records) != 1 {
		t.Fatalf("records = %d, want 1", len(records))
	}
	record := records[0]
	if record.FirstEventMillis == nil || *record.FirstEventMillis != 120 {
		t.Fatalf("first event = %v, want the first successful write only", record.FirstEventMillis)
	}
	if record.FirstTextMillis == nil || *record.FirstTextMillis != 600 {
		t.Fatalf("first text = %v, want 600 (120 + 80 + 400), i.e. the first non-empty one", *record.FirstTextMillis)
	}
	if !record.FirstTextIsFinalAnswer {
		t.Fatal("the confirmed block matched the first text block, so the flag must say so")
	}
	if record.DurationMillis != 700 {
		t.Fatalf("duration = %d, want the operation span up to the seal read", record.DurationMillis)
	}
}

// TestDurationStopsBeforeTheObservationsOwnTail pins the boundary the field
// documents: duration_millis is read at the first statement of Seal, so neither
// the cost of writing the record nor anything the caller does afterwards belongs
// to it. A number that folded the observation's own tail in would be measuring
// the meter rather than the operation.
func TestDurationStopsBeforeTheObservationsOwnTail(t *testing.T) {
	now := time.Unix(0, 0)
	var records []TaskRecord
	var mu sync.Mutex
	publisher := NewPublisher(true, func(record TaskRecord) error {
		// The sink runs synchronously inside Seal, after the duration was read.
		now = now.Add(500 * time.Millisecond)
		mu.Lock()
		defer mu.Unlock()
		records = append(records, record)
		return nil
	}).WithClock(func() time.Time { return now })

	_, handle := publisher.Start(context.Background(), "message_send")
	now = now.Add(90 * time.Millisecond)
	handle.Seal(OutcomeOK, "", "")
	now = now.Add(1 * time.Second)

	if len(records) != 1 {
		t.Fatalf("records = %d, want 1", len(records))
	}
	if records[0].DurationMillis != 90 {
		t.Fatalf("duration = %d, want 90: the sink write and the post-seal tail are not the operation's span",
			records[0].DurationMillis)
	}
}

// TestFirstTextFromNarrationIsNotTheAnswer is the reason first-text must not be
// reported as answer TTFT on an agent turn.
func TestFirstTextFromNarrationIsNotTheAnswer(t *testing.T) {
	var records []TaskRecord
	var mu sync.Mutex
	publisher := NewPublisher(true, countingSink(&records, &mu))
	_, handle := publisher.Start(context.Background(), "message_agent_stream")
	handle.NoteFirstText("narration-block")
	handle.NoteConfirmedBlock("answer-block")
	handle.Seal(OutcomeOK, "", "")

	record := records[0]
	if record.FirstTextMillis == nil {
		t.Fatal("the first public text was not recorded")
	}
	if record.FirstTextIsFinalAnswer {
		t.Fatal("a narration block must not be claimed as the confirmed answer block")
	}
}

func TestSinkFailureIsCountedWithoutItsText(t *testing.T) {
	publisher := NewPublisher(true, func(TaskRecord) error {
		return errors.New("the disk is full and the message quotes the prompt")
	})
	_, handle := publisher.Start(context.Background(), "message_send")
	handle.Seal(OutcomeOK, "", "")

	stats := publisher.Stats()
	if stats.Published != 0 || stats.Failed != 1 {
		t.Fatalf("stats = %+v, want one failed write and no published record", stats)
	}
	if stats.LastFailure != "sink_error" {
		t.Fatalf("last failure = %q, want the controlled category only", stats.LastFailure)
	}
	if strings.Contains(stats.LastFailure, "prompt") || strings.Contains(stats.LastFailure, "disk") {
		t.Fatalf("last failure leaked sink text: %q", stats.LastFailure)
	}
}

func TestSinkPanicIsContained(t *testing.T) {
	publisher := NewPublisher(true, func(TaskRecord) error { panic("sink exploded") })
	_, handle := publisher.Start(context.Background(), "message_send")
	handle.Seal(OutcomeOK, "", "")
	if stats := publisher.Stats(); stats.Failed != 1 || stats.LastFailure != "sink_panic" {
		t.Fatalf("stats = %+v, want the panic counted as sink_panic", stats)
	}
}

func TestMissingSinkIsCounted(t *testing.T) {
	publisher := NewPublisher(true, nil)
	_, handle := publisher.Start(context.Background(), "message_send")
	handle.Seal(OutcomeOK, "", "")
	if stats := publisher.Stats(); stats.Failed != 1 || stats.LastFailure != "sink_missing" {
		t.Fatalf("stats = %+v, want a missing sink reported rather than silently dropped", stats)
	}
}

// TestRecordSerialisesNullsNotZeros is the wire shape promise: an absent
// measurement is null, so a reader cannot mistake it for a zero.
func TestRecordSerialisesNullsNotZeros(t *testing.T) {
	var buffer bytes.Buffer
	publisher := NewPublisher(true, NewJSONSink(&buffer))
	_, handle := publisher.Start(context.Background(), "quiz_answer")
	handle.Seal(OutcomeOK, "", "")

	encoded := buffer.String()
	if !strings.Contains(encoded, `"first_event_millis":null`) {
		t.Fatalf("record JSON lost a null timing: %s", encoded)
	}
	if !strings.Contains(encoded, `"first_text_millis":null`) {
		t.Fatalf("record JSON lost a null first-text timing: %s", encoded)
	}
	if !strings.Contains(encoded, `"answer_stored":null`) || !strings.Contains(encoded, `"delivered":null`) {
		t.Fatalf("record JSON turned absent facts into zeros: %s", encoded)
	}
	if strings.Contains(encoded, `"cost":0`) || !strings.Contains(encoded, `"cost":"unknown"`) {
		t.Fatalf("record JSON claimed a cost: %s", encoded)
	}

	var decoded map[string]any
	if err := json.Unmarshal([]byte(encoded), &decoded); err != nil {
		t.Fatalf("the sink wrote something that is not JSON: %v", err)
	}
	// The forbidden families: prompt text, answer text, tool arguments, endpoint,
	// credential. Nothing here carries any of them.
	for key := range decoded {
		switch key {
		case "task_id", "operation", "outcome", "failure_stage", "reason_code", "duration_millis",
			"first_event_millis", "first_text_millis", "first_text_is_final_answer",
			"answer_stored", "delivered", "cost", "calls", "totals":
		default:
			t.Fatalf("record gained an unexpected top-level field %q", key)
		}
	}
}

// TestConcurrentTasksAreIsolatedAndUnique: twelve operations through one
// publisher, each with its own calls.
func TestConcurrentTasksAreIsolatedAndUnique(t *testing.T) {
	var records []TaskRecord
	var mu sync.Mutex
	publisher := NewPublisher(true, countingSink(&records, &mu))

	var group sync.WaitGroup
	for index := range 12 {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			ctx, handle := publisher.Start(context.Background(), "message_send")
			collector, ok := FromContext(ctx)
			if !ok {
				t.Errorf("task %d got no collector on its context", index)
				handle.Seal(OutcomeOther, "test", "")
				return
			}
			for range 3 {
				call := collector.Begin(ctx, KindComplete)
				call.Dispatch()
				call.NoteUsage(Usage{Prompt: Field{Value: 1, Reported: true}})
				call.Complete()
			}
			// A second task started inside this one must not see its calls.
			innerCtx, inner := publisher.Start(ctx, "nested")
			if innerCollector, ok := FromContext(innerCtx); ok && innerCollector == collector {
				t.Errorf("the nested task reused the outer collector")
			}
			inner.Seal(OutcomeOK, "", "")
			handle.Seal(OutcomeOK, "", "")
		}(index)
	}
	group.Wait()

	if len(records) != 24 {
		t.Fatalf("records = %d, want 12 tasks plus 12 nested ones", len(records))
	}
	seen := map[string]bool{}
	for _, record := range records {
		if seen[record.TaskID] {
			t.Fatalf("task id %q appeared twice", record.TaskID)
		}
		seen[record.TaskID] = true
		if strings.HasPrefix(record.Operation, "message_send") {
			if record.Totals.Calls != 3 || record.Totals.Prompt.Value != 3 {
				t.Fatalf("task %s totals = %+v, want its own three calls and no leakage",
					record.TaskID, record.Totals)
			}
		}
		if record.Operation == "nested" && record.Totals.Status != StatusNoCalls {
			t.Fatalf("nested task status = %s, want no_calls", record.Totals.Status)
		}
	}
}

func TestPurposeDefaultsToUnlabelled(t *testing.T) {
	collector := New()
	call := collector.Begin(context.Background(), KindComplete)
	call.Complete()
	if got := collector.Snapshot()[0].Purpose; got != PurposeUnlabelled {
		t.Fatalf("purpose = %q, want unlabelled rather than a guessed phase", got)
	}
	labelled := collector.Begin(WithPurpose(context.Background(), PurposeSummary), KindComplete)
	labelled.Complete()
	if got := collector.Snapshot()[1].Purpose; got != PurposeSummary {
		t.Fatalf("purpose = %q, want summary", got)
	}
	totals := collector.Totals()
	if totals.ByPurpose[PurposeSummary].Calls != 1 || totals.ByPurpose[PurposeUnlabelled].Calls != 1 {
		t.Fatalf("by-purpose = %+v, want one of each", totals.ByPurpose)
	}
}
