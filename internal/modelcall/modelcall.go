// Package modelcall records the model calls one request actually made.
//
// It exists because the counters that already live in the agent loop answer a
// different question: they report how much of a turn's budget was consumed, and
// they move before the call is issued. Neither the budget nor a provider's own
// accounting can tell you whether an HTTP request left the process, whether the
// provider reported token usage, or what happened to a call that failed after it
// had already been billed. This package records those three things per call.
//
// Two rules hold the numbers honest:
//
//   - A reported zero and a missing figure are different facts, so every usage
//     number carries whether the provider sent it (see Field). Nothing here
//     defaults an absent field to zero, sums prompt and completion to invent a
//     total, or folds cached tokens into any total.
//   - A record is committed once per exported model-method call, whatever the
//     outcome. Work that was paid for and then discarded still happened, so a
//     call that parsed usage and then failed on an empty answer keeps its usage.
//
// Nothing is logged on its own, and no value here is a cost: without a price
// source and complete coverage of every call type, the only honest cost figure
// is unknown.
package modelcall

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/model"
)

// Kind names the exported model method a call went through.
type Kind string

const (
	KindComplete          Kind = "complete"
	KindCompleteWithTools Kind = "complete_with_tools"
	KindStream            Kind = "stream"
	KindStreamWithTools   Kind = "stream_with_tools"
)

// Outcome classifies how a call ended. It labels the record only: the error a
// caller receives is untouched by this package.
type Outcome string

const (
	// OutcomeOK is a call that produced a usable completion.
	OutcomeOK Outcome = "ok"
	// OutcomeRefused is a call rejected before any HTTP request was built, such
	// as an invalid argument combination.
	OutcomeRefused Outcome = "refused"
	// OutcomeTransport is a request that was handed to the transport and failed
	// there: connection reset, DNS, or a deadline the transport reported.
	OutcomeTransport Outcome = "transport"
	// OutcomeProviderError is a non-2xx answer or an error frame.
	OutcomeProviderError Outcome = "provider_error"
	// OutcomeMalformed is an answer that could not be parsed or contradicted its
	// contract, including a stream that ended before its terminal frame.
	OutcomeMalformed Outcome = "malformed"
	// OutcomeEmpty is a provider answer that carried neither text nor a tool call.
	OutcomeEmpty Outcome = "empty"
	// OutcomeCancelled is a caller cancellation.
	OutcomeCancelled Outcome = "cancelled"
	// OutcomeTimeout is this client's own bound on one call expiring.
	OutcomeTimeout Outcome = "timeout"
	// OutcomeCallbackFailed is a consumer rejection: the answer arrived, and the
	// handler for it refused to take it.
	OutcomeCallbackFailed Outcome = "callback_failed"
	// OutcomeOther is anything the taxonomy above does not name.
	OutcomeOther Outcome = "other"
)

// Field is one numeric value plus whether the provider actually sent it.
//
// Reported is the whole point: a zero with Reported=false must never be summed,
// compared or printed as if it were a measurement, and a zero with Reported=true
// is a real report that should not be silently dropped.
type Field struct {
	Value    int64 `json:"value"`
	Reported bool  `json:"reported"`
}

// Present reports whether the provider sent this field at all.
func (f Field) Present() bool { return f.Reported }

// Usage holds the token figures a provider may report for one call.
//
// Cached counts prompt tokens the provider says it served from cache. It is a
// subset of Prompt, never an addition to any total, and it is kept separate so a
// cache discount can never be misread as fewer tokens.
type Usage struct {
	Prompt     Field `json:"prompt"`
	Completion Field `json:"completion"`
	Total      Field `json:"total"`
	Cached     Field `json:"cached"`
}

// AnyPresent reports whether the provider sent at least one usage figure.
func (u Usage) AnyPresent() bool {
	return u.Prompt.Reported || u.Completion.Reported || u.Total.Reported || u.Cached.Reported
}

// AllPresent reports whether the three figures a cost estimate needs were all
// sent. Cached is excluded on purpose: providers report it inconsistently, and a
// missing cache figure does not make the token totals partial.
func (u Usage) AllPresent() bool {
	return u.Prompt.Reported && u.Completion.Reported && u.Total.Reported
}

// Call is one exported model-method call, from entry to exit.
//
// The fields are deliberately narrow: no prompt, no answer text, no tool name or
// arguments, no endpoint, no credential, no provider error body. Those are either
// user data or secrets, and neither belongs in a measurement record.
type Call struct {
	// Seq is this call's position within its collector, starting at 1. It is an
	// internal correlation number, not a user-visible identifier.
	Seq int `json:"seq"`
	// Kind is the exported method the call went through.
	Kind Kind `json:"kind"`
	// Purpose is why the server made this call, read from the context label that
	// was in force at the call site. An unlabelled call says so rather than
	// borrowing a neighbour's total.
	Purpose Purpose `json:"purpose"`
	// Outcome classifies how the call ended.
	Outcome Outcome `json:"outcome"`
	// DurationMillis measures this client's own span for the call: entry until
	// exit, which includes waiting on the provider. It is not the provider's
	// reported processing time and not a user-perceived latency.
	DurationMillis int64 `json:"duration_millis"`
	// HTTPDispatched says whether a request was handed to the transport at least
	// once. A refused validation and a transport failure differ exactly here.
	HTTPDispatched bool `json:"http_dispatched"`
	// Usage is the last usage figure set for this call. For a stream it is the
	// most recent frame's value, because providers send cumulative snapshots
	// rather than per-frame increments; UsageFrames says how many arrived.
	Usage Usage `json:"usage"`
	// UsageFrames counts frames that carried usage. It is diagnostic: it says the
	// provider repeated a snapshot, never that tokens should be added again.
	UsageFrames int `json:"usage_frames"`
}

// Collector gathers calls for one scope - a request, a turn, a test - and is the
// only place sequence numbers come from.
//
// Collectors are never shared across scopes by this package: a caller that wants
// per-user totals has to pass its own collector through context, so a missing
// collector cannot quietly merge two requests' records.
type Collector struct {
	mu     sync.Mutex
	calls  []Call
	issued int
	now    func() time.Time
}

// New returns an empty collector.
func New() *Collector { return &Collector{now: time.Now} }

// withClock lets a test own time so durations are asserted instead of bounded.
func (c *Collector) withClock(now func() time.Time) *Collector {
	if c != nil && now != nil {
		c.now = now
	}
	return c
}

// Begin starts recording one call and returns nil when there is nothing to
// record into. Every method on the returned handle accepts a nil receiver, so a
// call path stays identical whether or not a collector is attached.
func (c *Collector) Begin(ctx context.Context, kind Kind) *Handle {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// issued only ever grows, so two concurrent calls cannot be handed the same
	// sequence number, and a committed record cannot make a later one reuse it.
	c.issued++
	return &Handle{
		collector: c,
		call:      Call{Seq: c.issued, Kind: kind, Purpose: PurposeFromContext(ctx)},
		started:   c.currentTime(),
	}
}

func (c *Collector) currentTime() time.Time {
	if c.now == nil {
		return time.Now()
	}
	return c.now()
}

// Handle is one call under construction. It is written from the goroutine making
// the model call and sealed exactly once.
type Handle struct {
	collector *Collector
	call      Call
	started   time.Time
	sealed    bool
}

// Dispatch records that a request reached the transport. Safe to call for every
// attempt within one call: the first one marks the call dispatched.
func (h *Handle) Dispatch() {
	if h == nil {
		return
	}
	h.call.HTTPDispatched = true
}

// NoteUsage records a usage snapshot from this call. A stream that reports the
// same cumulative figure in several frames therefore stays one entry per call:
// the newest snapshot wins and the frame count says how often it repeated.
func (h *Handle) NoteUsage(usage Usage) {
	if h == nil {
		return
	}
	if !usage.AnyPresent() {
		return
	}
	h.call.Usage = usage
	h.call.UsageFrames++
}

// Complete seals the call as a usable answer.
func (h *Handle) Complete() { h.Seal(OutcomeOK) }

// Fail seals the call with whatever outcome the error maps to.
func (h *Handle) Fail(err error) {
	if h == nil {
		return
	}
	h.Seal(outcomeOf(err))
}

// Seal finishes the call and commits its record. Calling it twice is a no-op, so
// a deferred safety net cannot duplicate an entry that an earlier path already
// wrote.
func (h *Handle) Seal(outcome Outcome) {
	if h == nil || h.sealed {
		return
	}
	h.sealed = true
	if outcome == "" {
		outcome = OutcomeOther
	}
	h.call.Outcome = outcome
	h.call.DurationMillis = millisSince(h.started, h.collector.currentTime())
	h.collector.commit(h.call)
}

func millisSince(start, end time.Time) int64 {
	if end.Before(start) {
		return 0
	}
	return end.Sub(start).Milliseconds()
}

func (c *Collector) commit(call Call) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, call)
}

// Snapshot returns a copy of every call recorded so far. Later calls cannot
// change what a caller already took, so a report built from a snapshot cannot be
// rewritten underneath itself.
func (c *Collector) Snapshot() []Call {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Call, len(c.calls))
	copy(out, c.calls)
	return out
}

// TotalsStatus says how much of a group of calls reported usage. The distinction
// between "no calls happened" and "calls happened and reported nothing" matters
// to every later comparison, so both are named instead of both reading as zero.
type TotalsStatus string

const (
	StatusNoCalls       TotalsStatus = "no_calls"
	StatusFullUsage     TotalsStatus = "full_usage"
	StatusPartialUsage  TotalsStatus = "partial_usage"
	StatusNoUsageReport TotalsStatus = "no_usage_reported"
)

// Sum is a total plus how many calls contributed to it, so a partial sum can
// never be read as a complete one.
type Sum struct {
	Value        int64 `json:"value"`
	Contributing int   `json:"contributing"`
	Missing      int   `json:"missing"`
}

// Totals is a roll-up of a set of calls.
type Totals struct {
	Calls        int                       `json:"calls"`
	ByKind       map[Kind]int              `json:"by_kind"`
	ByOutcome    map[Outcome]int           `json:"by_outcome"`
	ByPurpose    map[Purpose]PurposeTotals `json:"by_purpose"`
	Dispatches   int                       `json:"dispatches"`
	Dispatchless int                       `json:"dispatchless"`
	Status       TotalsStatus              `json:"status"`
	Prompt       Sum                       `json:"prompt"`
	Completion   Sum                       `json:"completion"`
	Total        Sum                       `json:"total"`
	Cached       Sum                       `json:"cached"`
}

// Add folds one field into a sum, skipping anything the provider did not report.
func (s *Sum) add(f Field) {
	if f.Reported {
		s.Value += f.Value
		s.Contributing++
		return
	}
	s.Missing++
}

// Complete reports whether every call in the group reported this figure.
func (s Sum) Complete() bool { return s.Missing == 0 && s.Contributing > 0 }

// Totals rolls up a snapshot. It never derives a value the provider did not
// report: total_tokens is not computed from prompt plus completion, and cached
// tokens are reported beside the totals rather than inside them.
func (c *Collector) Totals() Totals { return totalsOf(c.Snapshot()) }

// Totals rolls up a snapshot the same way Collector.Totals does, so a caller can
// re-check numbers from a snapshot it already holds without reaching back.
func TotalsOf(calls []Call) Totals { return totalsOf(calls) }

// accumulator folds calls into a roll-up. It is shared by the whole-task total
// and each labelled phase so the two cannot drift apart in what "reported" means.
type accumulator struct {
	calls        int
	dispatches   int
	dispatchless int
	reporting    int
	complete     int
	prompt       Sum
	completion   Sum
	total        Sum
	cached       Sum
}

func (a *accumulator) add(call Call) {
	a.calls++
	if call.HTTPDispatched {
		a.dispatches++
	} else {
		a.dispatchless++
	}
	a.prompt.add(call.Usage.Prompt)
	a.completion.add(call.Usage.Completion)
	a.total.add(call.Usage.Total)
	a.cached.add(call.Usage.Cached)
	if call.Usage.AnyPresent() {
		a.reporting++
	}
	if call.Usage.AllPresent() {
		a.complete++
	}
}

func (a accumulator) status() TotalsStatus {
	switch {
	case a.calls == 0:
		return StatusNoCalls
	case a.complete == a.calls:
		return StatusFullUsage
	case a.reporting == 0:
		return StatusNoUsageReport
	default:
		return StatusPartialUsage
	}
}

func totalsOf(calls []Call) Totals {
	totals := Totals{
		Calls:     len(calls),
		ByKind:    map[Kind]int{},
		ByOutcome: map[Outcome]int{},
		ByPurpose: map[Purpose]PurposeTotals{},
	}
	all := accumulator{}
	byPurpose := map[Purpose][]Call{}
	for _, call := range calls {
		totals.ByKind[call.Kind]++
		outcome := call.Outcome
		if outcome == "" {
			outcome = OutcomeOther
		}
		totals.ByOutcome[outcome]++
		all.add(call)
		purpose := call.Purpose
		if purpose == "" {
			purpose = PurposeUnlabelled
		}
		byPurpose[purpose] = append(byPurpose[purpose], call)
	}
	for purpose, group := range byPurpose {
		part := accumulator{}
		for _, call := range group {
			part.add(call)
		}
		totals.ByPurpose[purpose] = PurposeTotals{
			Calls: part.calls, Dispatches: part.dispatches, Status: part.status(),
			Prompt: part.prompt, Completion: part.completion, Total: part.total, Cached: part.cached,
		}
	}
	totals.Dispatches = all.dispatches
	totals.Dispatchless = all.dispatchless
	totals.Prompt = all.prompt
	totals.Completion = all.completion
	totals.Total = all.total
	totals.Cached = all.cached
	totals.Status = all.status()
	return totals
}

type contextKey struct{}

// WithCollector attaches a collector to a context for the duration of one scope.
func WithCollector(ctx context.Context, collector *Collector) context.Context {
	if ctx == nil || collector == nil {
		return ctx
	}
	return context.WithValue(ctx, contextKey{}, collector)
}

// FromContext returns the collector attached to ctx, if any. A missing collector
// is normal: every call path has to behave identically without one.
func FromContext(ctx context.Context) (*Collector, bool) {
	if ctx == nil {
		return nil, false
	}
	collector, ok := ctx.Value(contextKey{}).(*Collector)
	return collector, ok
}

// outcomeOf maps an existing error onto a record label. It reads the error
// taxonomy the model package already defines and changes nothing about it: the
// caller still receives exactly the error it received before, and a label that
// cannot be matched says so rather than guessing.
func outcomeOf(err error) Outcome {
	switch {
	case err == nil:
		return OutcomeOK
	case errors.Is(err, context.Canceled):
		return OutcomeCancelled
	case errors.Is(err, context.DeadlineExceeded):
		return OutcomeTimeout
	case errors.Is(err, model.ErrInvalidRequest), errors.Is(err, model.ErrNotConfigured),
		errors.Is(err, model.ErrToolsUnsupported):
		return OutcomeRefused
	case errors.Is(err, model.ErrProviderStatus):
		return OutcomeProviderError
	case errors.Is(err, model.ErrUnavailable):
		return OutcomeTransport
	case errors.Is(err, model.ErrEmptyCompletion):
		return OutcomeEmpty
	case errors.Is(err, model.ErrTruncatedStream):
		return OutcomeMalformed
	case errors.Is(err, model.ErrTimeout):
		return OutcomeTimeout
	case errors.Is(err, model.ErrMalformedResponse):
		return OutcomeMalformed
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return OutcomeTimeout
	}
	return OutcomeOther
}
