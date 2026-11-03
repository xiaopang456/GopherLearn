// Package calls attributes model calls to the runs that caused them.
//
// The rule is identity, not a wall-clock window. A call belongs to the run
// whose identity it carries (its run id, or the session it was made in). A
// call that carries no identity, or an identity no run claims, is attributed to
// no run at all: it is counted on its own, so a missing identity can never be
// hidden by a neighbouring run's time window.
//
// Within one run, the side a call falls on is decided by the run's own answer
// boundary: a call the product sent before the answer came back is part of
// producing the answer, a call it sent afterwards is background work that
// happened because of that answer. The boundary is a property of the run, so
// two runs never compete for the same call.
package calls

import (
	"fmt"
	"sort"
	"time"
)

// The two sides of a run's cost.
const (
	// PhaseAnswer is the work that produced the answer.
	PhaseAnswer = "answer"
	// PhaseAfterAnswer is the work that started after the answer came back.
	PhaseAfterAnswer = "after_answer"
)

// Run is one evaluated run: the identity the product gave it, the moment its
// answer came back, and what the run itself reported about its own cost.
type Run struct {
	Arm       string
	QID       string
	Rep       int
	RunID     string
	SessionID string
	// AnsweredAt is when the answer came back. A zero value means the run
	// never answered; such a run has no "after the answer" side.
	AnsweredAt time.Time
	// ReportedModelCalls is what the run reported about itself. It is a
	// cross-check for an attribution, never a substitute for one.
	ReportedModelCalls *int
}

// Key identifies the run inside an evaluation.
func (r Run) Key() string { return fmt.Sprintf("%s|%s|%d", r.Arm, r.QID, r.Rep) }

// Usage is what a provider reported for one call. A field that was not
// reported stays nil: it is unknown, not zero.
type Usage struct {
	Prompt     *int64
	Completion *int64
	Total      *int64
	Cached     *int64
	Reasoning  *int64
}

// Call is one model call as a call ledger recorded it.
type Call struct {
	// At is when the product sent the request. A call that had not been sent
	// yet cannot belong to the answer it would have produced.
	At     time.Time
	Lane   string
	Model  string
	Stream bool
	// RunID and SessionID are the identity the ledger carried. Either may be
	// empty; both empty means the call cannot be attributed.
	RunID     string
	SessionID string
	// Phase is the phase the ledger claimed. It is only a claim: the phase
	// used for accounting is derived from the run's answer boundary.
	Phase string
	Usage *Usage
}

// Totals are the sums of one side of a run's cost. Only reported values are
// summed, and the calls that reported nothing are counted apart, so a missing
// number never reads as a zero.
type Totals struct {
	Calls          int
	ChatCalls      int
	Prompt         int64
	Completion     int64
	Total          int64
	Cached         int64
	Reasoning      int64
	UnmeteredCalls int
}

func (t *Totals) add(call Call) {
	t.Calls++
	if call.Lane == "chat" {
		t.ChatCalls++
	}
	usage := call.Usage
	if usage == nil {
		t.UnmeteredCalls++
		return
	}
	if usage.Prompt != nil {
		t.Prompt += *usage.Prompt
	}
	if usage.Completion != nil {
		t.Completion += *usage.Completion
	}
	if usage.Total != nil {
		t.Total += *usage.Total
	}
	if usage.Cached != nil {
		t.Cached += *usage.Cached
	}
	if usage.Reasoning != nil {
		t.Reasoning += *usage.Reasoning
	}
}

// RunCost is one run's cost, split by phase.
type RunCost struct {
	Run         Run
	Answer      Totals
	AfterAnswer Totals
	// ClaimedPhaseMismatch counts calls whose ledger phase disagreed with the
	// phase derived from the run's answer boundary.
	ClaimedPhaseMismatch int
}

// Attribution is the whole result: every run that was claimed, and everything
// that was not.
type Attribution struct {
	Runs []RunCost
	// Unattributed holds the calls no run claims: no identity, an unknown
	// identity, or an identity more than one run could answer for.
	Unattributed Totals
	// UnattributedCalls counts them.
	UnattributedCalls int
	// AmbiguousCalls counts the calls whose identity more than one run could
	// answer for. They are not attributed to either.
	AmbiguousCalls int
	// UnknownIdentityCalls counts the calls whose identity no run claims.
	UnknownIdentityCalls int
}

// Attribute assigns every call to the run that owns it.
//
// A call is matched by run id first, then by session id. A session id is only
// used when exactly one run was made in that session; when several runs share
// a session the call is ambiguous and is left unattributed rather than
// guessed.
func Attribute(runs []Run, calls []Call) (Attribution, error) {
	byRunID := map[string][]int{}
	bySession := map[string][]int{}
	for i, run := range runs {
		if run.RunID != "" {
			byRunID[run.RunID] = append(byRunID[run.RunID], i)
		}
		if run.SessionID != "" {
			bySession[run.SessionID] = append(bySession[run.SessionID], i)
		}
	}
	result := Attribution{Runs: make([]RunCost, len(runs))}
	for i, run := range runs {
		result.Runs[i] = RunCost{Run: run}
	}
	for _, call := range calls {
		index, state := owner(call, byRunID, bySession)
		switch state {
		case ownerNone:
			result.UnattributedCalls++
			result.UnknownIdentityCalls++
			result.Unattributed.add(call)
			continue
		case ownerAmbiguous:
			result.UnattributedCalls++
			result.AmbiguousCalls++
			result.Unattributed.add(call)
			continue
		}
		cost := &result.Runs[index]
		if call.At.IsZero() {
			// A call with no time cannot be placed on either side of the
			// answer. It stays with its run, but it is counted apart.
			cost.ClaimedPhaseMismatch++
			continue
		}
		phase := PhaseAnswer
		if !cost.Run.AnsweredAt.IsZero() && call.At.After(cost.Run.AnsweredAt) {
			phase = PhaseAfterAnswer
		}
		if call.Phase != "" && call.Phase != phase {
			cost.ClaimedPhaseMismatch++
		}
		if phase == PhaseAfterAnswer {
			cost.AfterAnswer.add(call)
		} else {
			cost.Answer.add(call)
		}
	}
	return result, nil
}

type ownerState int

const (
	ownerNone ownerState = iota
	ownerFound
	ownerAmbiguous
)

// owner finds the run a call belongs to, by identity alone.
func owner(call Call, byRunID, bySession map[string][]int) (int, ownerState) {
	if call.RunID != "" {
		matches := byRunID[call.RunID]
		if len(matches) == 1 {
			return matches[0], ownerFound
		}
		if len(matches) > 1 {
			return 0, ownerAmbiguous
		}
		// The call names a run nobody has. A session id would be a guess.
		return 0, ownerNone
	}
	if call.SessionID == "" {
		return 0, ownerNone
	}
	matches := bySession[call.SessionID]
	if len(matches) == 1 {
		return matches[0], ownerFound
	}
	if len(matches) > 1 {
		return 0, ownerAmbiguous
	}
	return 0, ownerNone
}

// Cost looks a run's cost up by its identity.
func (a Attribution) Cost(run Run) (RunCost, bool) {
	for _, cost := range a.Runs {
		if cost.Run.Key() == run.Key() {
			return cost, true
		}
	}
	return RunCost{}, false
}

// ArmRuns returns the runs of one arm, in a fixed order.
func (a Attribution) ArmRuns(arm string) []RunCost {
	var out []RunCost
	for _, cost := range a.Runs {
		if cost.Run.Arm == arm {
			out = append(out, cost)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Run.Key() < out[j].Run.Key() })
	return out
}
