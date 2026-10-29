package memorywindow

import (
	"strings"
	"testing"

	"GopherAI/common/rag"
)

func ranked(pairs ...any) []Ranked {
	out := []Ranked{}
	for i := 0; i < len(pairs); i += 2 {
		out = append(out, Ranked{Key: pairs[i].(string), Rank: pairs[i+1].(int)})
	}
	return out
}

func keys(in []Ranked) []string {
	out := []string{}
	for _, item := range in {
		out = append(out, item.Key)
	}
	return out
}

func TestInterleaveGivesEachKindOneSlotThenRanks(t *testing.T) {
	semantic := ranked("s0", 0, "s1", 1, "s2", 2, "s3", 3)
	episodic := ranked("e0", 4, "e1", 5, "e2", 6, "e3", 7)
	got := strings.Join(keys(Interleave(semantic, episodic, 6)), ",")
	if got != "s0,e0,s1,s2,s3,e1" {
		t.Fatalf("interleave order = %q", got)
	}
}

func TestInterleaveBreaksRankTiesTowardSemantic(t *testing.T) {
	got := keys(Interleave(ranked("s0", 0, "s1", 3), ranked("e0", 1, "e1", 3), 4))
	if strings.Join(got, ",") != "s0,e0,s1,e1" {
		t.Fatalf("tie order = %v", got)
	}
}

// A top_k of one skips the guaranteed-slot branch, so the base window is not a
// prefix of the eight-card sequence. Reserve must still be exactly "everything
// the sequence holds that the base window does not".
func TestReserveFiltersRatherThanContinues(t *testing.T) {
	semantic := ranked("s0", 2, "s1", 3)
	episodic := ranked("e0", 0, "e1", 1)
	base := keys(Interleave(semantic, episodic, 1))
	if strings.Join(base, ",") != "e0" {
		t.Fatalf("base window = %v", base)
	}
	// The full sequence is s0,e0,e1,s1: both kinds take a guaranteed slot, then
	// rank decides. Removing the base window leaves the rest in that order.
	got := keys(Reserve(semantic, episodic, base, 8))
	if strings.Join(got, ",") != "s0,e1,s1" {
		t.Fatalf("reserve = %v", got)
	}
}

func TestReserveIsEmptyWhenTheBaseWindowAlreadyHoldsEverything(t *testing.T) {
	semantic := ranked("s0", 0)
	episodic := ranked("e0", 1)
	base := keys(Interleave(semantic, episodic, 6))
	if got := Reserve(semantic, episodic, base, 8); len(got) != 0 {
		t.Fatalf("reserve = %v, want empty", keys(got))
	}
}

func TestDynamicLimitRefusesToWidenAnEmptyObservation(t *testing.T) {
	if got := DynamicLimit(0, 1.35, 2800); got != 0 {
		t.Fatalf("limit for an empty observation = %d, want 0", got)
	}
}

func TestDynamicLimitFloorsGrowthAndHonoursTheAbsoluteCap(t *testing.T) {
	if got := DynamicLimit(300, 1.35, 2800); got != 405 {
		t.Fatalf("limit = %d, want 405", got)
	}
	if got := DynamicLimit(2600, 1.35, 2800); got != 2800 {
		t.Fatalf("limit = %d, want the absolute cap", got)
	}
	// Growth can never take the budget below what the base window already
	// spends, or preserving the base window would be impossible.
	if got := DynamicLimit(500, 0.5, 2800); got != 500 {
		t.Fatalf("limit = %d, want the base itself", got)
	}
}

func TestFitShortensABaseCardToWhatIsLeft(t *testing.T) {
	body := strings.Repeat("alpha beta ", 40)
	budget := Budget{MaxCardTokens: 320, MaxObservationTokens: 100}
	got, truncated, cost, ok := Fit(body, 0, 90, budget)
	if !ok || !truncated {
		t.Fatalf("ok=%v truncated=%v", ok, truncated)
	}
	if cost > 10 || cost != rag.EstimateTokens(got) {
		t.Fatalf("cost = %d for %d tokens of body", cost, rag.EstimateTokens(got))
	}
}

func TestFitRefusesWhenTheObservationIsFull(t *testing.T) {
	budget := Budget{MaxCardTokens: 320, MaxObservationTokens: 100}
	if _, _, _, ok := Fit("anything", 0, 100, budget); ok {
		t.Fatal("a full observation still accepted a card")
	}
}

func TestFitWholeDropsACardThatWouldHaveToBeSqueezed(t *testing.T) {
	body := strings.Repeat("alpha beta ", 40)
	budget := Budget{MaxCardTokens: 320, MaxObservationTokens: 2800}
	full := rag.EstimateTokens(strings.TrimSpace(body))
	// One token short of the card's full cost: Fit would shorten it, FitWhole
	// must refuse it outright.
	if _, _, _, ok := FitWhole(body, 0, 0, full-1, budget); ok {
		t.Fatal("an appended card was squeezed into a budget it does not fit")
	}
	if _, _, cost, ok := FitWhole(body, 0, 0, full, budget); !ok || cost != full {
		t.Fatalf("ok=%v cost=%d want %d", ok, cost, full)
	}
}

func TestFitWholeChargesForQuotesBeforeAccepting(t *testing.T) {
	budget := Budget{MaxCardTokens: 320, MaxObservationTokens: 2800}
	body := "short body"
	cost := rag.EstimateTokens(body)
	if _, _, _, ok := FitWhole(body, 50, 0, cost+49, budget); ok {
		t.Fatal("quotes were not charged against the budget")
	}
	if _, _, got, ok := FitWhole(body, 50, 0, cost+50, budget); !ok || got != cost+50 {
		t.Fatalf("ok=%v cost=%d", ok, got)
	}
}

func TestFitWholeStillRespectsThePerCardCap(t *testing.T) {
	body := strings.Repeat("alpha beta ", 400)
	budget := Budget{MaxCardTokens: 320, MaxObservationTokens: 2800}
	got, truncated, cost, ok := FitWhole(body, 0, 0, 2800, budget)
	if !ok || !truncated {
		t.Fatalf("ok=%v truncated=%v", ok, truncated)
	}
	if cost > 320 || rag.EstimateTokens(got) != cost {
		t.Fatalf("cost = %d, want at most the per-card cap", cost)
	}
}

func TestFitWholeNeverExceedsTheAbsoluteObservationCap(t *testing.T) {
	budget := Budget{MaxCardTokens: 320, MaxObservationTokens: 100}
	body := strings.Repeat("alpha beta ", 40)
	if _, _, _, ok := FitWhole(body, 0, 0, 5000, budget); ok {
		t.Fatal("a per-call limit above the absolute cap was honoured")
	}
}
