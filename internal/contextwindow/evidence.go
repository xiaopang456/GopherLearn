package contextwindow

import (
	"context"
	"fmt"
	"sync"
)

// Unversioned is the version a record that has no version column reports. It is a
// value rather than zero because zero has to stay available for "this record is at
// version zero"; two entries that differ only in whether they were ever revised must
// not share a dedup key, and neither may be merged with a revision number.
const Unversioned int64 = -1

// EvidenceID is one displayable unit of evidence: a passage, a recalled record, an
// excerpt range. It is the unit the budget is spent in and the unit deduplication
// compares, because "did we already send this" only has an answer per unit.
//
// The key deliberately carries the display range. The same chunk shown whole and shown
// as its first two hundred runes is not the same evidence, and merging them would let
// a turn claim to have sent text it never sent.
type EvidenceID struct {
	// Kind separates the sources of evidence so a document id and a memory id that
	// happened to be spelled alike could not cancel each other's text.
	Kind string
	// Namespace is the scope the record lives in: a collection, a user, a session.
	Namespace string
	// RecordID is the record's own stable identifier.
	RecordID string
	// Version is the record's revision, or Unversioned.
	Version int64
	// DisplayStart and DisplayLimit describe the part of the record that is shown,
	// counted in runes of the record's text.
	DisplayStart int
	DisplayLimit int
}

// Key is the deduplication identity. It is a string so a caller can store it in a set
// without exposing the struct, and it quotes every text field so a separator cannot be
// forged inside an identifier.
func (id EvidenceID) Key() string {
	return fmt.Sprintf("%s|%q|%q|%d|%d:%d", id.Kind, id.Namespace, id.RecordID,
		id.Version, id.DisplayStart, id.DisplayLimit)
}

// Evidence is one turn's shared allowance for evidence bodies.
//
// It belongs to a single agent turn and nothing else. It is created on the turn's stack
// and dropped with it, so one user's spending cannot shrink another's and a restart or
// a cache cannot resurrect it. It is not a budget for the request as a whole: it
// counts what new evidence text this turn has introduced, while the request-wide
// estimate in CheckRequest counts everything still in front of the model, including
// the bodies sent one call ago.
//
// A tool that does not consult it is still bounded by its own per-call cap; the two
// limits are deliberately different things, and a per-tool cap is not a whole-turn
// ceiling.
type Evidence struct {
	mu       sync.Mutex
	max      int
	used     int
	reserved int
	bodies   map[string]bool
}

// NewEvidence returns an allowance of max estimated tokens.
func NewEvidence(max int) *Evidence {
	if max < 0 {
		max = 0
	}
	return &Evidence{max: max, bodies: make(map[string]bool)}
}

// Remaining reports what this turn may still introduce.
func (e *Evidence) Remaining() int {
	if e == nil {
		return 0
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.max - e.used - e.reserved
}

// Used reports what this turn has already introduced.
func (e *Evidence) Used() int {
	if e == nil {
		return 0
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.used
}

// BodySent reports whether this exact display unit already reached the model in this
// turn. A repeated hit may then be acknowledged without resending its text, which is
// cheaper and does not ask the model to read the same passage twice.
func (e *Evidence) BodySent(key string) bool {
	if e == nil {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.bodies[key]
}

// TryCharge spends tokens against the allowance and reports whether it fit. The check
// and the spend are one operation: charging after asking would let two calls of one
// round each see the same remaining figure and both spend it.
//
// A failed charge spends nothing, so the caller keeps its whole allowance for the
// entries after it. That is the direction the reference project chose for long-term
// memory - skip what does not fit rather than let one long record crowd out the short
// ones behind it - and it is what keeps a budget refusal from becoming an ordering
// preference.
//
// A nil allowance reports true and spends nothing. Outside an agent turn there is no
// shared allowance to spend, and a tool must not answer "found nothing" because nobody
// installed a budget; there its own per-call cap is the only limit, as it was before
// this package existed.
func (e *Evidence) TryCharge(tokens int) bool {
	if e == nil {
		return true
	}
	if tokens < 0 {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.used+e.reserved+tokens > e.max {
		return false
	}
	e.used += tokens
	return true
}

// Reserve returns a provisional charge. The reservation is committed only after
// the caller has completed its authorization handoff and successfully produced
// the observation. A failed authorization can therefore release it without
// disturbing earlier successful evidence.
func (e *Evidence) Reserve(tokens int) (*Reservation, bool) {
	if e == nil {
		return &Reservation{}, true
	}
	if tokens < 0 {
		return nil, false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.used+e.reserved+tokens > e.max {
		return nil, false
	}
	e.reserved += tokens
	return &Reservation{e: e, tokens: tokens}, true
}

type Reservation struct {
	e      *Evidence
	tokens int
	mu     sync.Mutex
	closed bool
}

func (r *Reservation) Commit() {
	if r == nil || r.e == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	// Lock order is Reservation.mu -> Evidence.mu for both commit and
	// release. All shared used/reserved mutations occur under Evidence.mu.
	r.e.mu.Lock()
	defer r.e.mu.Unlock()
	if r.e.reserved < r.tokens {
		return
	}
	r.e.reserved -= r.tokens
	r.e.used += r.tokens
	r.closed = true
}

func (r *Reservation) Release() {
	if r == nil || r.e == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.e.mu.Lock()
	defer r.e.mu.Unlock()
	if r.e.reserved < r.tokens {
		return
	}
	r.e.reserved -= r.tokens
	r.closed = true
}

func (e *Evidence) MarkBodySent(key string) {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.bodies == nil {
		e.bodies = make(map[string]bool)
	}
	e.bodies[key] = true
}

// evidenceContextKey carries the turn's allowance to the tools it runs.
type evidenceContextKey struct{}

// WithEvidence hands one turn's allowance to everything downstream of ctx. It is the
// interface a tool uses to share the budget: a tool that ignores it stays inside its
// own per-call cap, and a tool that honors it spends from the same allowance as its
// neighbours in the turn.
func WithEvidence(ctx context.Context, evidence *Evidence) context.Context {
	return context.WithValue(ctx, evidenceContextKey{}, evidence)
}

// EvidenceFrom returns the allowance this turn installed, or nil when there is none -
// which is the answer outside an agent loop, not a failure.
func EvidenceFrom(ctx context.Context) *Evidence {
	if ctx == nil {
		return nil
	}
	evidence, _ := ctx.Value(evidenceContextKey{}).(*Evidence)
	return evidence
}
