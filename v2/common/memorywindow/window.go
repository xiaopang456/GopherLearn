// Package memorywindow holds the two decisions that decide what a memory
// observation finally contains: how the two memory kinds are merged into one
// ordered window, and whether one more card still fits in the token budget.
//
// It is a leaf package on purpose. Both the production tool and the offline
// evaluators need these answers, and an offline aggregator that re-implements
// them "closely enough" produces a token figure that describes nothing the
// model ever read. STABILIZE-035 measured its cost with a second, private copy
// of the rendering loop; this package exists so STABILIZE-036 cannot.
package memorywindow

import (
	"strings"

	"GopherAI/common/rag"
)

// The STABILIZE-036 method parameters. They are constants rather than settings
// because a number a deployment could choose is a number an experiment can be
// re-asked for until it flatters. A different value is a different method
// version, with its own freeze and its own gates.
const (
	// TokenBoundedGrowth is how much one observation may grow to carry evidence
	// the final merge would otherwise have dropped.
	TokenBoundedGrowth = 1.35
	// TokenBoundedAbsoluteTokens is the ceiling no widened observation passes,
	// whatever the base call spent.
	TokenBoundedAbsoluteTokens = 2800
	// TokenBoundedMaxCards is the widened window's card ceiling: the union of
	// both kinds' per-kind top lists, and no more.
	TokenBoundedMaxCards = 8
)

// Ranked is one candidate reduced to what merging needs: an identity, and its
// fused rank inside its own kind. Rank is comparable across kinds because both
// sides number from the top of their own RRF list.
type Ranked struct {
	Key  string
	Rank int
}

// Interleave merges the two kinds into one ordered window.
//
// Each kind keeps one guaranteed slot before rank decides the rest. Ranking
// purely by fused score would let whichever channel happens to score higher on
// this corpus take every slot, and a mixed question needs one of each far more
// than it needs the two best of one.
func Interleave(semantic, episodic []Ranked, limit int) []Ranked {
	out := make([]Ranked, 0, limit)
	seen := make(map[string]struct{}, limit)
	take := func(candidate Ranked) {
		if _, duplicate := seen[candidate.Key]; duplicate {
			return
		}
		seen[candidate.Key] = struct{}{}
		out = append(out, candidate)
	}
	if len(semantic) > 0 && len(episodic) > 0 && limit >= 2 {
		take(semantic[0])
		take(episodic[0])
		semantic, episodic = semantic[1:], episodic[1:]
	}
	for (len(semantic) > 0 || len(episodic) > 0) && len(out) < limit {
		switch {
		case len(episodic) == 0:
			take(semantic[0])
			semantic = semantic[1:]
		case len(semantic) == 0:
			take(episodic[0])
			episodic = episodic[1:]
		case semantic[0].Rank <= episodic[0].Rank:
			// Ties go to the semantic side by a fixed rule rather than by map
			// order, so the same question produces the same card list twice.
			take(semantic[0])
			semantic = semantic[1:]
		default:
			take(episodic[0])
			episodic = episodic[1:]
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// Reserve is the STABILIZE-036 append sequence: the same deterministic
// interleave of both per-kind lists, taken up to limit, with everything the
// base window already holds removed.
//
// It is defined as a filter over the full sequence rather than as a
// continuation of the base window, because a base window built with top_k == 1
// skips the guaranteed-slot branch and is therefore not a prefix of the
// eight-card sequence. Filtering is correct in both cases; continuing is not.
func Reserve(semantic, episodic []Ranked, base []string, limit int) []Ranked {
	held := make(map[string]struct{}, len(base))
	for _, key := range base {
		held[key] = struct{}{}
	}
	out := []Ranked{}
	for _, candidate := range Interleave(semantic, episodic, limit) {
		if _, already := held[candidate.Key]; already {
			continue
		}
		out = append(out, candidate)
	}
	return out
}

// Budget bounds one observation. Both numbers are part of the method identity
// and are written into every freeze that uses them.
type Budget struct {
	// MaxCardTokens is the per-card body cap.
	MaxCardTokens int
	// MaxObservationTokens is the ceiling on one whole observation.
	MaxObservationTokens int
}

// Fit renders one base-window card into what is left of the observation.
//
// A card whose body would not fit is allowed to be shortened, because the base
// window's job is to show the model everything the method selected; ok is false
// only when nothing survives, and the caller must then declare the observation
// truncated rather than emit a card with an empty body. quoteCost is charged on
// top of the body exactly as the tool charges it.
func Fit(content string, quoteCost int, used int, budget Budget) (body string, truncated bool, cost int, ok bool) {
	remaining := budget.MaxObservationTokens - used
	if remaining <= 0 {
		return "", false, 0, false
	}
	cardBudget := budget.MaxCardTokens
	if remaining < cardBudget {
		cardBudget = remaining
	}
	body, truncated = TrimToTokens(content, cardBudget)
	if body == "" {
		return "", truncated, 0, false
	}
	return body, truncated, rag.EstimateTokens(body) + quoteCost, true
}

// FitWhole renders one appended card, which may not be shrunk to fit.
//
// The difference from Fit is the one rule STABILIZE-036 froze before seeing any
// result: an extra card is a bonus, so it is rendered under the same per-card
// cap as every other card and then either fits whole or is dropped whole. The
// alternative — squeezing it into whatever is left — would let the budget buy
// a card body too short to check, which is the failure the base window's own
// empty-body guard already refuses.
func FitWhole(content string, quoteCost int, used int, limit int, budget Budget) (body string, truncated bool, cost int, ok bool) {
	if limit > budget.MaxObservationTokens {
		limit = budget.MaxObservationTokens
	}
	body, truncated = TrimToTokens(content, budget.MaxCardTokens)
	if body == "" {
		return "", truncated, 0, false
	}
	cost = rag.EstimateTokens(body) + quoteCost
	if used+cost > limit {
		return "", truncated, cost, false
	}
	return body, truncated, cost, true
}

// DynamicLimit is the STABILIZE-036 per-call ceiling: the base observation may
// grow by at most a third and a bit, and never past the absolute cap.
//
// A base of zero returns zero, which is what stops a failed or empty search
// from being widened: there is no evidence to protect the independence of.
func DynamicLimit(baseTokens int, growth float64, absolute int) int {
	if baseTokens <= 0 {
		return 0
	}
	limit := int(float64(baseTokens) * growth)
	if limit < baseTokens {
		limit = baseTokens
	}
	if limit > absolute {
		limit = absolute
	}
	return limit
}

// TrimToTokens shortens a body to fit a token budget, reporting whether it had
// to.
func TrimToTokens(value string, maxTokens int) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" || maxTokens <= 0 {
		return "", value != ""
	}
	if rag.EstimateTokens(value) <= maxTokens {
		return value, false
	}
	runes := []rune(value)
	low, high := 0, len(runes)
	for low < high {
		mid := (low + high + 1) / 2
		if rag.EstimateTokens(string(runes[:mid])) <= maxTokens {
			low = mid
		} else {
			high = mid - 1
		}
	}
	if low == 0 {
		return "", true
	}
	return strings.TrimSpace(string(runes[:low])), true
}
