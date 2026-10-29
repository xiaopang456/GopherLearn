package modelcall

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/model"
)

func field(value int64) Field { return Field{Value: value, Reported: true} }

func call(kind Kind, outcome Outcome, usage Usage) Call {
	return Call{Kind: kind, Outcome: outcome, HTTPDispatched: true, Usage: usage}
}

// TestAbsentIsNotZero is the whole reason this package exists: a provider that
// sent nothing must not arrive as a measurement of nothing.
func TestAbsentIsNotZero(t *testing.T) {
	absent := Usage{}
	if absent.AnyPresent() || absent.AllPresent() {
		t.Fatal("an empty Usage reads as reported")
	}
	explicitZero := Usage{
		Prompt:     field(0),
		Completion: field(0),
		Total:      field(0),
	}
	if !explicitZero.AnyPresent() || !explicitZero.AllPresent() {
		t.Fatal("reported zeros were collapsed into absent")
	}

	totals := TotalsOf([]Call{{Usage: absent}, {Usage: explicitZero}})
	if totals.Prompt.Contributing != 1 || totals.Prompt.Missing != 1 {
		t.Fatalf("prompt roll-up = %+v, want the zeros counted and the absent marked missing", totals.Prompt)
	}
	if totals.Status != StatusPartialUsage {
		t.Fatalf("status = %s, want partial_usage: one call reported, one did not", totals.Status)
	}
}

// TestTotalsNeverInventsATotal: prompt and completion were reported, total was
// not, so the total stays missing rather than being added up by us.
func TestTotalsNeverInventsATotal(t *testing.T) {
	totals := TotalsOf([]Call{{Usage: Usage{Prompt: field(10), Completion: field(4)}}})
	if totals.Total.Contributing != 0 || totals.Total.Value != 0 {
		t.Fatalf("total_tokens invented as %+v, want no contribution and no value", totals.Total)
	}
	if totals.Total.Missing != 1 {
		t.Fatalf("total missing = %d, want 1", totals.Total.Missing)
	}
	if totals.Prompt.Value != 10 || totals.Prompt.Contributing != 1 {
		t.Fatalf("prompt sum = %+v, want 10 from one call", totals.Prompt)
	}
	if totals.Status != StatusPartialUsage {
		t.Fatalf("status = %s, want partial_usage", totals.Status)
	}
}

// TestCachedIsNeverAddedToATotal: a cache hit discounts tokens already counted in
// prompt, so it must sit beside the totals, not inside them.
func TestCachedIsNeverAddedToATotal(t *testing.T) {
	totals := TotalsOf([]Call{{Usage: Usage{
		Prompt: field(100), Completion: field(20), Total: field(120), Cached: field(64),
	}}})
	if totals.Prompt.Value != 100 {
		t.Fatalf("prompt = %d, want the reported 100 with cache kept separate", totals.Prompt.Value)
	}
	if totals.Total.Value != 120 {
		t.Fatalf("total = %d, want 120 rather than cache folded in", totals.Total.Value)
	}
	if totals.Cached.Value != 64 || totals.Cached.Contributing != 1 {
		t.Fatalf("cached = %+v, want 64 reported once", totals.Cached)
	}
}

// TestCachedAbsentIsNotZero keeps the discount invisible rather than invented.
func TestCachedAbsentIsNotZero(t *testing.T) {
	totals := TotalsOf([]Call{{Usage: Usage{Prompt: field(50), Completion: field(5), Total: field(55)}}})
	if totals.Cached.Contributing != 0 || totals.Cached.Missing != 1 {
		t.Fatalf("cached = %+v, want absent and counted as missing", totals.Cached)
	}
	// Absent cache does not make the token totals partial: providers report it
	// inconsistently, and the three figures that cost money are complete here.
	if totals.Status != StatusFullUsage {
		t.Fatalf("status = %s, want full_usage when only cache is missing", totals.Status)
	}
}

func TestTotalsStatuses(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		calls []Call
		want  TotalsStatus
	}{
		{"no_calls", nil, StatusNoCalls},
		{"none_reported", []Call{
			{Kind: KindComplete, Outcome: OutcomeOK, HTTPDispatched: true},
			{Kind: KindStream, Outcome: OutcomeOK, HTTPDispatched: true},
		}, StatusNoUsageReport},
		{"partial", []Call{
			{Kind: KindComplete, Usage: Usage{Prompt: field(1), Completion: field(1), Total: field(2)}},
			{Kind: KindComplete},
		}, StatusPartialUsage},
		{"full", []Call{
			{Kind: KindComplete, Usage: Usage{Prompt: field(1), Completion: field(1), Total: field(2)}},
			{Kind: KindComplete, Usage: Usage{Prompt: field(2), Completion: field(2), Total: field(4)}},
		}, StatusFullUsage},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := TotalsOf(testCase.calls).Status; got != testCase.want {
				t.Fatalf("status = %s, want %s", got, testCase.want)
			}
		})
	}
}

// TestNoCallsIsNotZeroCallsReported guards the reading that an empty collector
// proves nothing was billed. It proves nothing was recorded.
func TestNoCallsIsNotZeroCallsReported(t *testing.T) {
	empty := New().Totals()
	if empty.Status != StatusNoCalls || empty.Calls != 0 {
		t.Fatalf("empty totals = %+v, want no_calls with zero calls", empty)
	}
	if empty.Prompt.Complete() {
		t.Fatal("an empty group claimed a complete prompt total")
	}
	silent := TotalsOf([]Call{{Outcome: OutcomeOK, HTTPDispatched: true}})
	if silent.Status != StatusNoUsageReport {
		t.Fatalf("status = %s, want no_usage_reported so a silent group cannot look like no_calls", silent.Status)
	}
}

func TestDispatchBoundaryIsRecorded(t *testing.T) {
	totals := TotalsOf([]Call{
		{Kind: KindComplete, Outcome: OutcomeRefused},
		{Kind: KindComplete, Outcome: OutcomeTransport, HTTPDispatched: true},
	})
	if totals.Dispatches != 1 || totals.Dispatchless != 1 {
		t.Fatalf("dispatches = %d/%d, want one dispatched and one refused", totals.Dispatches, totals.Dispatchless)
	}
}

func TestSnapshotDoesNotChangeUnderneathItsHolder(t *testing.T) {
	collector := New()
	handle := collector.Begin(context.Background(), KindComplete)
	handle.Dispatch()
	handle.NoteUsage(Usage{Prompt: field(1)})
	handle.Complete()

	snapshot := collector.Snapshot()
	later := collector.Begin(context.Background(), KindStream)
	later.NoteUsage(Usage{Prompt: field(99)})
	later.Complete()

	if len(snapshot) != 1 {
		t.Fatalf("snapshot length = %d after a later call, want the taken copy to stay put", len(snapshot))
	}
	if snapshot[0].Usage.Prompt.Value != 1 {
		t.Fatal("a later write changed an already-taken snapshot")
	}
	if len(collector.Snapshot()) != 2 {
		t.Fatal("the collector itself stopped recording")
	}
}

func TestHandleSealsExactlyOnce(t *testing.T) {
	collector := New()
	handle := collector.Begin(context.Background(), KindComplete)
	handle.Complete()
	handle.Fail(context.Canceled)
	handle.Seal(OutcomeTimeout)

	snapshot := collector.Snapshot()
	if len(snapshot) != 1 {
		t.Fatalf("records = %d, want one call recorded once regardless of later seals", len(snapshot))
	}
	if snapshot[0].Outcome != OutcomeOK {
		t.Fatalf("outcome = %s, want the first seal to win", snapshot[0].Outcome)
	}
}

func TestNoteUsageIgnoresAnEmptySnapshot(t *testing.T) {
	collector := New()
	handle := collector.Begin(context.Background(), KindStream)
	handle.NoteUsage(Usage{Prompt: field(7), Completion: field(2), Total: field(9)})
	// A later frame that carries a usage object with nothing in it must not clear
	// what was already learned about this call.
	handle.NoteUsage(Usage{})
	handle.Complete()

	record := collector.Snapshot()[0]
	if record.Usage.Prompt.Value != 7 || record.UsageFrames != 1 {
		t.Fatalf("record = %+v, want the earlier figure kept and one counted frame", record)
	}
}

func TestConcurrentCallsDoNotContaminateEachOther(t *testing.T) {
	collector := New()
	var group sync.WaitGroup
	handles := make([]*Handle, 32)
	for index := range handles {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			handle := collector.Begin(context.Background(), KindComplete)
			handles[index] = handle
			handle.Dispatch()
			handle.NoteUsage(Usage{Prompt: field(int64(index)), Completion: field(int64(index)), Total: field(int64(2 * index))})
			handle.Complete()
		}(index)
	}
	group.Wait()

	snapshot := collector.Snapshot()
	if len(snapshot) != len(handles) {
		t.Fatalf("records = %d, want %d", len(snapshot), len(handles))
	}
	seen := map[int]bool{}
	for _, record := range snapshot {
		if seen[record.Seq] {
			t.Fatalf("sequence %d appeared twice", record.Seq)
		}
		seen[record.Seq] = true
	}
	totals := collector.Totals()
	if totals.Status != StatusFullUsage || totals.Prompt.Contributing != len(handles) {
		t.Fatalf("totals = %+v, want every concurrent call represented", totals)
	}
	// 0+1+...+31 = 496 per field, and no call's figure leaked into another.
	if totals.Prompt.Value != 496 || totals.Total.Value != 992 {
		t.Fatalf("sums = %d/%d, want 496/992", totals.Prompt.Value, totals.Total.Value)
	}
}

func TestDurationsComeFromTheInjectedClock(t *testing.T) {
	now := time.Unix(0, 0)
	collector := New().withClock(func() time.Time { return now })
	handle := collector.Begin(context.Background(), KindComplete)
	now = now.Add(1500 * time.Millisecond)
	handle.Complete()

	if got := collector.Snapshot()[0].DurationMillis; got != 1500 {
		t.Fatalf("duration = %dms, want 1500 measured against the clock it was given", got)
	}
}

func TestNilCollectorAndNilHandleAreInert(t *testing.T) {
	// A request with no collector must not allocate, panic, or change shape.
	var collector *Collector
	if collector.Begin(context.Background(), KindComplete) != nil {
		t.Fatal("a nil collector handed out a handle")
	}
	if _, ok := FromContext(context.Background()); ok {
		t.Fatal("an unattached context reported a collector")
	}
	var handle *Handle
	handle.Dispatch()
	handle.NoteUsage(Usage{Prompt: field(1)})
	handle.Complete()
	handle.Fail(context.Canceled)
	handle.Seal(OutcomeOK)
	if collector.Snapshot() != nil || collector.Totals().Calls != 0 {
		t.Fatal("a nil collector produced records")
	}
	WithCollector(context.Background(), nil)
}

func TestOutcomeOf(t *testing.T) {
	for _, testCase := range []struct {
		name string
		err  error
		want Outcome
	}{
		{"nil", nil, OutcomeOK},
		{"cancelled", context.Canceled, OutcomeCancelled},
		{"caller_deadline", context.DeadlineExceeded, OutcomeTimeout},
		{"refused", model.ErrInvalidRequest, OutcomeRefused},
		{"refused_wrapped", fmt.Errorf("%w: no messages", model.ErrInvalidRequest), OutcomeRefused},
		{"empty", model.ErrEmptyCompletion, OutcomeEmpty},
		{"truncated", model.ErrTruncatedStream, OutcomeMalformed},
		{"timeout", model.ErrTimeout, OutcomeTimeout},
		{"provider", model.ErrProviderStatus, OutcomeProviderError},
		{"provider_wrapped", fmt.Errorf("openai: status 503: %w", model.ErrProviderStatus), OutcomeProviderError},
		{"unavailable", model.ErrUnavailable, OutcomeTransport},
		{"malformed", model.ErrMalformedResponse, OutcomeMalformed},
		{"unknown", errors.New("something else"), OutcomeOther},
	} {
		if got := outcomeOf(testCase.err); got != testCase.want {
			t.Fatalf("%s: outcomeOf(%v) = %s, want %s", testCase.name, testCase.err, got, testCase.want)
		}
	}
}

// TestRecordedShapeCarriesNoContent is a whitelist guard: a record may gain a
// number or an enum, but prompt text, an answer, tool arguments, an endpoint or a
// credential must never become part of measurement by someone adding a field.
func TestRecordedShapeCarriesNoContent(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		sample any
		want   []string
	}{
		{
			name:   "Call",
			sample: Call{},
			want:   []string{"DurationMillis", "HTTPDispatched", "Kind", "Outcome", "Purpose", "Seq", "Usage", "UsageFrames"},
		},
		{"Usage", Usage{}, []string{"Cached", "Completion", "Prompt", "Total"}},
		{"Field", Field{}, []string{"Reported", "Value"}},
		{"Totals", Totals{}, []string{
			"ByKind", "ByOutcome", "ByPurpose", "Calls", "Cached", "Completion", "Dispatches", "Dispatchless",
			"Prompt", "Status", "Total",
		}},
		{"Sum", Sum{}, []string{"Contributing", "Missing", "Value"}},
	} {
		fields := fieldNames(testCase.sample)
		sort.Strings(testCase.want)
		if !reflect.DeepEqual(fields, testCase.want) {
			t.Fatalf("%s fields = %v, want exactly %v", testCase.name, fields, testCase.want)
		}
	}
}

func fieldNames(sample any) []string {
	value := reflect.TypeOf(sample)
	names := make([]string, 0, value.NumField())
	for index := range value.NumField() {
		names = append(names, value.Field(index).Name)
	}
	sort.Strings(names)
	return names
}

// TestSequencesAreNeverReused covers the identity rule: one allocator, so no two
// records can share a Seq, and a call that has begun but not finished cannot make
// the next one borrow its number.
func TestSequencesAreNeverReused(t *testing.T) {
	collector := New()

	first := collector.Begin(context.Background(), KindComplete)
	second := collector.Begin(context.Background(), KindStream)

	// Nothing is visible until it is sealed, because a half-finished call has no
	// outcome and no duration yet.
	if snapshot := collector.Snapshot(); len(snapshot) != 0 {
		t.Fatalf("snapshot = %+v, want an unfinished call kept out of it", snapshot)
	}

	second.Complete()
	if snapshot := collector.Snapshot(); len(snapshot) != 1 || snapshot[0].Seq != 2 {
		t.Fatalf("snapshot = %+v, want only the sealed call with its own sequence", snapshot)
	}

	third := collector.Begin(context.Background(), KindComplete)
	if third.call.Seq == 2 || third.call.Seq == 1 {
		t.Fatalf("third call reused sequence %d", third.call.Seq)
	}
	first.Fail(nil)
	third.Complete()

	var seen []int
	for _, record := range collector.Snapshot() {
		seen = append(seen, record.Seq)
	}
	slices.Sort(seen)
	if len(seen) != 3 || seen[0] != 1 || seen[1] != 2 || seen[2] != 3 {
		t.Fatalf("sequences = %v, want 1,2,3 exactly once each", seen)
	}
}

// TestConcurrentInterleavedCallsKeepUniqueSequences begins calls from many
// goroutines, holds some open, and seals them in the opposite order: the one
// allocator still has to hand out distinct numbers.
func TestConcurrentInterleavedCallsKeepUniqueSequences(t *testing.T) {
	collector := New()
	const calls = 64

	handles := make([]*Handle, calls)
	var opening sync.WaitGroup
	for index := range handles {
		opening.Add(1)
		go func(index int) {
			defer opening.Done()
			handles[index] = collector.Begin(context.Background(), KindComplete)
		}(index)
	}
	opening.Wait()

	// Every open handle holds a number the rest have never seen.
	issued := map[int]bool{}
	for _, handle := range handles {
		if issued[handle.call.Seq] {
			t.Fatalf("sequence %d was handed out twice", handle.call.Seq)
		}
		issued[handle.call.Seq] = true
	}

	var sealing sync.WaitGroup
	for index := range handles {
		sealing.Add(1)
		go func(index int) {
			defer sealing.Done()
			if index%2 == 0 {
				handles[index].Complete()
				return
			}
			handles[index].Fail(context.Canceled)
		}(index)
	}
	sealing.Wait()

	snapshot := collector.Snapshot()
	if len(snapshot) != calls {
		t.Fatalf("records = %d, want %d", len(snapshot), calls)
	}
	seen := map[int]bool{}
	for _, record := range snapshot {
		if seen[record.Seq] {
			t.Fatalf("record for sequence %d appeared twice", record.Seq)
		}
		seen[record.Seq] = true
	}
	if len(seen) != calls {
		t.Fatalf("distinct sequences = %d, want %d", len(seen), calls)
	}
	totals := collector.Totals()
	if totals.ByOutcome[OutcomeOK] != calls/2 || totals.ByOutcome[OutcomeCancelled] != calls/2 {
		t.Fatalf("outcome split = %+v, want half ok and half cancelled", totals.ByOutcome)
	}
}

// TestSealedCallKeepsItsOwnIdentityAcrossSnapshots: sealing order must not
// reorder or renumber what a reader already took.
func TestSealedCallKeepsItsOwnIdentityAcrossSnapshots(t *testing.T) {
	collector := New()
	first := collector.Begin(context.Background(), KindComplete)
	second := collector.Begin(context.Background(), KindStream)
	second.Complete()
	first.Complete()

	taken := collector.Snapshot()
	if taken[0].Seq != 2 || taken[0].Kind != KindStream {
		t.Fatalf("first committed record = %+v, want the call that sealed first, not the call that began first", taken[0])
	}
	if taken[1].Seq != 1 || taken[1].Kind != KindComplete {
		t.Fatalf("second committed record = %+v, want the earlier sequence number kept with its own call", taken[1])
	}
}
