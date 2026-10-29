// Package summary rolls one session's older conversation into a compact summary, so a
// long conversation keeps its goals, decisions and open questions after those messages
// leave the recent window - without resending the whole transcript every turn.
//
// The scope is deliberately narrow. A summary belongs to exactly one session and one
// user; nothing here reads across sessions, and there is no per-topic or per-user
// memory. That is Phase 5A: a bounded rolling context, not long-term memory.
//
// The package holds two things: the policy and planning, which are pure and decide
// what may be compressed, and the summarizer, which turns those messages into text
// through the same model seam every other answer path uses.
package summary

import "time"

// Policy bounds when a session summary is refreshed and how much may be handed to the
// model that writes it.
//
// Two units are used and they are not interchangeable. Messages and runes are what this
// project counts exactly over stored rows; the token figures are estimates from
// internal/contextwindow, of the same kind as the one figure that is actually sent to a
// provider. A trigger that fires on them is a threshold on a local measurement, never a
// claim about a model's tokenizer.
type Policy struct {
	// TriggerMessages is how many complete, not-yet-covered messages must pile up
	// before a refresh is worth a model call. Below it, the recent window still
	// holds the conversation and summarizing would cost a call to lose detail.
	TriggerMessages int

	// TriggerTokens is the second, independent way a refresh becomes due: a handful
	// of enormous answers never reaches a message count while already filling a
	// window. It is counted over the same exchanges the planner could compress.
	TriggerTokens int

	// KeepRecentMessages is how many of the newest complete messages stay out of the
	// summary, so the model always sees the verbatim end of the conversation. The
	// boundary is rounded up to a whole user→assistant pair, never mid-turn: a
	// question whose answer was folded into the summary while its answer stayed
	// verbatim reads as a broken conversation.
	KeepRecentMessages int

	// MaxInputMessages and MaxInputRunes bound what one generation is shown. The
	// compress set is trimmed from its newest end, so the summary always covers a
	// contiguous stretch starting at the previous boundary - a summary with a hole
	// in the middle cannot be re-extended honestly.
	MaxInputMessages int
	MaxInputRunes    int

	// MaxInputTokens bounds the same thing in the unit a model is billed in, over the
	// whole payload rather than only the new material: the previous summary travels in
	// the same request, and so do the labels framing it. Measuring only the new
	// exchanges is how a long session's summarizing request keeps growing while its
	// configured input cap looks respected.
	MaxInputTokens int

	// MaxOutputTokens is the hard cap sent to the model.
	MaxOutputTokens int

	// Timeout caps the summarizing call itself.
	Timeout time.Duration

	// StageBudget caps what one turn may spend on compacting. It is not extra time:
	// the refresh happens inside the turn's own deadline, before the answer is
	// generated, so a refresh bounded only by its own timeout can spend most of the
	// learner's budget rewriting the past and leave nothing for the reply.
	StageBudget time.Duration

	// MaxDeadlineShare is the largest fraction of the turn's remaining time the
	// refresh may take, as a divisor: 3 means at most a third. A fixed stage budget is
	// the right cap for a turn that has just started and the wrong one for a turn that
	// has four seconds left.
	MaxDeadlineShare int

	// FailureCooldown is how long a session stops attempting a refresh after one
	// failed. Without it, every turn of a busy session pays for a fresh call to a
	// model that is already known to be refusing.
	FailureCooldown time.Duration
}

// Defaults chosen to match the behaviour the reference project documents: twelve
// uncovered messages before a refresh, the last eight messages kept verbatim.
const (
	DefaultTriggerMessages    = 12
	DefaultTriggerTokens      = 6000
	DefaultKeepRecentMessages = 8
	DefaultMaxInputMessages   = 60
	DefaultMaxInputRunes      = 12000
	DefaultMaxInputTokens     = 8000
	DefaultMaxOutputTokens    = 2000
	DefaultSummaryTimeout     = 20 * time.Second
	DefaultSummaryStageBudget = 8 * time.Second
	// DefaultMaxDeadlineShare keeps a late turn's refresh proportional rather than
	// fixed, so a request nearly out of time does not spend most of what is left on
	// the step that is only an optimization over history that is still on disk.
	DefaultMaxDeadlineShare = 3
	// MinSummaryStageBudget is the least time worth handing the summarizer. Below it
	// the call cannot finish, so spending the turn's remaining time on it buys nothing
	// and the honest answer is to skip and say so.
	MinSummaryStageBudget  = 1500 * time.Millisecond
	DefaultFailureCooldown = 30 * time.Second
)

// DefaultPolicy is the policy a deployment runs with unless it configures otherwise.
func DefaultPolicy() Policy {
	return Policy{}.Normalize()
}

// Normalize replaces unusable values with the defaults. A zero means "not
// configured", never "do nothing": a deployment that forgets to set a threshold must
// not end up summarizing every turn, and one that sets KeepRecentMessages below the
// trigger would compress the very messages it promised to keep verbatim.
func (p Policy) Normalize() Policy {
	if p.TriggerMessages <= 0 {
		p.TriggerMessages = DefaultTriggerMessages
	}
	if p.TriggerTokens <= 0 {
		p.TriggerTokens = DefaultTriggerTokens
	}
	if p.KeepRecentMessages <= 0 {
		p.KeepRecentMessages = DefaultKeepRecentMessages
	}
	if p.MaxInputMessages <= 0 {
		p.MaxInputMessages = DefaultMaxInputMessages
	}
	if p.MaxInputRunes <= 0 {
		p.MaxInputRunes = DefaultMaxInputRunes
	}
	if p.MaxInputTokens <= 0 {
		p.MaxInputTokens = DefaultMaxInputTokens
	}
	if p.MaxOutputTokens <= 0 {
		p.MaxOutputTokens = DefaultMaxOutputTokens
	}
	if p.Timeout <= 0 {
		p.Timeout = DefaultSummaryTimeout
	}
	if p.StageBudget <= 0 {
		p.StageBudget = DefaultSummaryStageBudget
	}
	if p.MaxDeadlineShare <= 0 {
		p.MaxDeadlineShare = DefaultMaxDeadlineShare
	}
	if p.FailureCooldown <= 0 {
		p.FailureCooldown = DefaultFailureCooldown
	}
	// The kept tail has to fit inside the trigger, or a refresh would be asked to
	// compress messages that the policy also says must stay verbatim.
	if p.KeepRecentMessages >= p.TriggerMessages {
		p.KeepRecentMessages = p.TriggerMessages / 2
	}
	// A stage budget longer than the call's own timeout is not a longer stage: the
	// timeout is the outer bound, so the stage inherits the smaller figure instead of
	// promising time it can never use.
	if p.StageBudget > p.Timeout {
		p.StageBudget = p.Timeout
	}
	return p
}

// recentPairCount converts the kept-message count into whole pairs, rounding up.
// Rounding up is the conservative direction: it keeps more of the conversation
// verbatim and compresses less, which is the failure mode that costs detail rather
// than the one that costs honesty.
func recentPairCount(keepMessages int) int {
	pairs := keepMessages / 2
	if keepMessages%2 != 0 {
		pairs++
	}
	if pairs < 1 {
		pairs = 1
	}
	return pairs
}
