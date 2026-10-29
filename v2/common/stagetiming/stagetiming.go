// Package stagetiming is a tiny, dependency-free stopwatch for the document pipeline's sub-steps.
//
// A Timer travels in the context, so the code that owns a whole execution (the node's worker loop,
// the coordinator's publish branch) can ask every layer below it to add its share without any
// signature changing. All methods are safe on a nil Timer and from several goroutines, so a caller
// that did not install one simply records nothing. Only stage names and durations are kept: no ids,
// content or error text ever enter a Timer.
package stagetiming

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

type key struct{}

// Timer accumulates durations per stage name.
type Timer struct {
	mu     sync.Mutex
	stages map[string]time.Duration
}

func New() *Timer { return &Timer{stages: map[string]time.Duration{}} }

// With returns a context that carries t.
func With(ctx context.Context, t *Timer) context.Context {
	if t == nil {
		return ctx
	}
	return context.WithValue(ctx, key{}, t)
}

// From returns the Timer in ctx, or nil.
func From(ctx context.Context) *Timer {
	t, _ := ctx.Value(key{}).(*Timer)
	return t
}

// Add accumulates d under stage.
func (t *Timer) Add(stage string, d time.Duration) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.stages[stage] += d
	t.mu.Unlock()
}

// Start returns a function that, when called, adds the time since Start to stage. Typical use:
//
//	defer stagetiming.From(ctx).Start("seal")()
func (t *Timer) Start(stage string) func() {
	if t == nil {
		return func() {}
	}
	began := time.Now()
	return func() { t.Add(stage, time.Since(began)) }
}

// Millis is the accumulated time of stage in milliseconds (fractional: several stages take well
// under a millisecond-resolution tick of interest).
func (t *Timer) Millis(stage string) float64 {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return float64(t.stages[stage]) / float64(time.Millisecond)
}

// Line renders "<kind> execution=<id> <stage>_ms=<n> ..." for the given stages, in that order.
// The id is passed by the caller; extra is appended verbatim (already "key=value" pairs).
func (t *Timer) Line(kind, execution string, stages []string, extra string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s execution=%s", kind, execution)
	for _, stage := range stages {
		fmt.Fprintf(&b, " %s_ms=%.1f", stage, t.Millis(stage))
	}
	if extra != "" {
		b.WriteString(" " + extra)
	}
	return b.String()
}
