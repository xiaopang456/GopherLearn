package service

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/summary"
)

// The two ways a refresh that was due can end without a model call. Both fail the turn
// closed, because a summary that should have been written and was not leaves the
// conversation longer than what the summary covers, and answering from that context
// would be answering from a past this service cannot describe.
//
// They are separate sentinels because they need different answers from an operator: a
// cooldown is a model that has been refusing, and a skipped stage is a turn that came
// in too late to afford the optimization.
var (
	ErrSummaryCoolingDown  = fmt.Errorf("%w: summary_failure_cooldown", ErrInternal)
	ErrSummaryStageSkipped = fmt.Errorf("%w: summary_stage_budget_exhausted", ErrInternal)
	// ErrSummaryStageTimedOut reports a refresh that was attempted and then ran past its
	// share of the turn while the caller was still waiting. It is a different event from
	// the turn's own deadline expiring, and reporting the one as the other would tell an
	// operator to lengthen the turn when the thing to look at is the summarizing model.
	ErrSummaryStageTimedOut = fmt.Errorf("%w: summary_stage_timed_out", ErrInternal)
)

// summaryGuard holds the per-session record of failed refreshes.
//
// It is keyed by user and session together: a failure in one conversation must not stop
// another user's session from summarizing, and a key made of session id alone would let a
// guesser poison someone else's sessions. The map is bounded and self-cleaning rather than
// a cache with an eviction policy - it holds a timestamp per active session, and the
// honest scope of a cooldown is "this process, this session, for a little while".
type summaryGuard struct {
	mu        sync.Mutex
	cooldowns map[string]time.Time
	cooldown  time.Duration
	now       func() time.Time
	// capacity bounds how many sessions may be cooling down at once. When it is full
	// the entry that would expire first is dropped, so the cost of pressure is one
	// session retrying a model that may still refuse - never an unbounded map.
	capacity int
}

const summaryGuardCapacity = 512

func newSummaryGuard(policy summary.Policy) *summaryGuard {
	return &summaryGuard{
		cooldowns: make(map[string]time.Time),
		cooldown:  policy.FailureCooldown,
		now:       time.Now,
		capacity:  summaryGuardCapacity,
	}
}

func summaryGuardKey(userID, sessionID string) string {
	return userID + "\x00" + sessionID
}

// coolingDown reports whether this session is still inside its cooldown, and forgets
// the entry once it has passed. Expired entries are removed on read rather than by a
// sweeper: nothing here deserves a goroutine that runs when no one is summarizing.
func (g *summaryGuard) coolingDown(key string) bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	until, ok := g.cooldowns[key]
	if !ok {
		return false
	}
	if !g.now().Before(until) {
		delete(g.cooldowns, key)
		return false
	}
	return true
}

func (g *summaryGuard) recordFailure(key string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	for stored, until := range g.cooldowns {
		if !now.Before(until) {
			delete(g.cooldowns, stored)
		}
	}
	if _, exists := g.cooldowns[key]; !exists && len(g.cooldowns) >= g.capacity {
		soonest, soonestUntil := "", time.Time{}
		for stored, until := range g.cooldowns {
			if soonest == "" || until.Before(soonestUntil) {
				soonest, soonestUntil = stored, until
			}
		}
		delete(g.cooldowns, soonest)
	}
	g.cooldowns[key] = now.Add(g.cooldown)
}

// clear ends a cooldown the moment a refresh has worked, so one recovered call restores
// the session instead of waiting out a timer.
func (g *summaryGuard) clear(key string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.cooldowns, key)
}

// applyTurnDeadline gives an agent turn one clock. The refresh and the loop spend from
// it in that order, so a summary that took the whole budget cannot also hand the loop a
// fresh one, and the stage budget below has a real remaining time to divide.
//
// Only an agent turn is wrapped. Every other strategy keeps the caller's context exactly
// as it arrived, which is the difference between changing one path and changing all of
// them while nobody asked.
func (s *MessageService) applyTurnDeadline(ctx context.Context, strategy domain.AnswerStrategy) (context.Context, context.CancelFunc) {
	if s.agentTurnDeadline <= 0 || strategy != domain.AnswerStrategyAgent || ctx == nil {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, s.agentTurnDeadline)
}

// stageBudget is how long this turn may spend compacting: the configured stage budget,
// never longer than the summarizing call's own timeout, and never more than the share
// of the turn's remaining time the policy allows.
//
// It reports false when what is left is too little to be worth a call. The turn's
// deadline is the one that was set before the refresh began, so the summary and the
// answer are spending the same budget rather than each getting a fresh one.
func stageBudget(ctx context.Context, policy summary.Policy) (time.Duration, bool) {
	budget := policy.StageBudget
	if policy.Timeout < budget {
		budget = policy.Timeout
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return budget, true
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return 0, false
	}
	share := remaining / time.Duration(policy.MaxDeadlineShare)
	if share >= budget {
		return budget, true
	}
	// The floor applies only when the turn is what squeezed the stage. A deliberately
	// small StageBudget is an operator's decision and is honored; a sliver left over
	// from a late request is not worth a call that will be cancelled before it answers.
	if share < summary.MinSummaryStageBudget {
		return 0, false
	}
	return share, true
}
