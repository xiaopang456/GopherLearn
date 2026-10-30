package metrics

import (
	"math"
	"testing"

	"github.com/xiaopang456/GopherLearn/eval/internal/dataset"
)

func set(spans ...Span) dataset.EvidenceSet { return dataset.EvidenceSet(spans) }
func sp(file string, start, end int) Span   { return Span{File: file, Start: start, End: end} }

func near(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("%s = %v, want %v", name, got, want)
	}
}

// The fixture below was worked out by hand.
//
// Gold: a.md lines 10-14 (5 lines).
// Results, in rank order:
//
//	r1 a.md 1-5     lines 1..5
//	r2 a.md 12-16   lines 12..16
//	r3 a.md 3-11    lines 3..11   (overlaps r1 on 3-5)
//
// top 1: L = 1..5                    -> covered 0 of 5 -> coverage 0,   purity 0,    RR 0
// top 2: L = 1..5 + 12..16 (10)      -> covered 12,13,14 -> 3/5 = 0.6,   purity 3/10, RR 1/2
// top 3: L = 1..16 (16 lines: the overlap 3-5 counts once)
//
//	-> covered 10..14 = 5/5 = 1.0, purity 5/16, RR 1/2
func TestSingleSetByHand(t *testing.T) {
	gold := []dataset.EvidenceSet{set(sp("a.md", 10, 14))}
	results := []Span{sp("a.md", 1, 5), sp("a.md", 12, 16), sp("a.md", 3, 11)}

	cases := []struct {
		k                    int
		coverage, purity, rr float64
		hit                  bool
	}{
		{1, 0, 0, 0, false},
		{2, 0.6, 0.3, 0.5, true},
		{3, 1.0, 5.0 / 16, 0.5, true},
	}
	for _, c := range cases {
		out := Evaluate(gold, results[:c.k], nil, false, 0.5)
		near(t, "coverage", out.Coverage, c.coverage)
		near(t, "purity", out.Purity, c.purity)
		near(t, "reciprocal rank", out.ReciprocalRank, c.rr)
		if out.Hit != c.hit {
			t.Errorf("top %d: hit = %v, want %v", c.k, out.Hit, c.hit)
		}
	}
}

// The two criteria differ on purpose: a result that shares one line with the
// evidence earns a reciprocal rank without making the question a hit.
func TestMRRAndHitUseDifferentCriteria(t *testing.T) {
	gold := []dataset.EvidenceSet{set(sp("a.md", 1, 10))}
	out := Evaluate(gold, []Span{sp("a.md", 10, 12)}, nil, false, 0.5)
	near(t, "coverage", out.Coverage, 0.1) // one gold line of ten
	near(t, "reciprocal rank", out.ReciprocalRank, 1)
	if out.Hit {
		t.Error("a 10% cover is not a hit at threshold 0.5")
	}

	// The threshold is inclusive: exactly half the lines is a hit.
	out = Evaluate(gold, []Span{sp("a.md", 1, 5)}, nil, false, 0.5)
	near(t, "coverage", out.Coverage, 0.5)
	if !out.Hit {
		t.Error("coverage equal to the threshold must be a hit")
	}
	if Evaluate(gold, []Span{sp("a.md", 1, 5)}, nil, false, 0.51).Hit {
		t.Error("the threshold is a parameter, not a constant")
	}
}

// Alternative sets: the best one counts, all spans of a set are required, and a
// tie goes to the set listed first.
func TestAlternativeSetsByHand(t *testing.T) {
	s1 := set(sp("a.md", 1, 2), sp("b.md", 1, 2)) // 4 lines, two spans
	s2 := set(sp("c.md", 5, 8))                   // 4 lines
	gold := []dataset.EvidenceSet{s1, s2}

	// a 1-2 and c 5-6: S1 covered 2/4, S2 covered 2/4 -> tie -> S1.
	out := Evaluate(gold, []Span{sp("a.md", 1, 2), sp("c.md", 5, 6)}, nil, false, 0.5)
	near(t, "tie coverage", out.Coverage, 0.5)
	near(t, "tie purity", out.Purity, 2.0/4) // S1's 2 covered lines of the 4 retrieved
	near(t, "tie RR", out.ReciprocalRank, 1)

	// c 5-8 covers S2 completely, and S1 not at all.
	out = Evaluate(gold, []Span{sp("c.md", 5, 8)}, nil, false, 0.5)
	near(t, "second set coverage", out.Coverage, 1)
	near(t, "second set purity", out.Purity, 1)

	// One span of a two-span set is half of it: partial credit, not all-or-nothing.
	out = Evaluate([]dataset.EvidenceSet{s1}, []Span{sp("a.md", 1, 2)}, nil, false, 0.5)
	near(t, "one of two spans", out.Coverage, 0.5)
	if !out.Hit {
		t.Error("covering half of a two-span set meets a 0.5 threshold")
	}
	out = Evaluate([]dataset.EvidenceSet{s1}, []Span{sp("a.md", 1, 1)}, nil, false, 0.5)
	near(t, "a quarter", out.Coverage, 0.25)
}

func TestLinesOfDifferentFilesNeverMix(t *testing.T) {
	gold := []dataset.EvidenceSet{set(sp("a.md", 5, 5))}
	out := Evaluate(gold, []Span{sp("b.md", 5, 5)}, nil, false, 0.5)
	if out.Coverage != 0 || out.ReciprocalRank != 0 || out.Hit {
		t.Fatalf("line 5 of b.md was credited to a.md: %+v", out)
	}
}

func TestEmptyResultsAndPurityGuard(t *testing.T) {
	gold := []dataset.EvidenceSet{set(sp("a.md", 1, 3))}
	out := Evaluate(gold, nil, nil, false, 0.5)
	if out.Coverage != 0 || out.Purity != 0 || out.ReciprocalRank != 0 || out.Hit || out.Class != ClassNotRecalled {
		t.Fatalf("no results: %+v", out)
	}
}

func TestReciprocalRankIsTheFirstUsefulResult(t *testing.T) {
	gold := []dataset.EvidenceSet{set(sp("a.md", 10, 12)), set(sp("b.md", 1, 1))}
	results := []Span{sp("a.md", 1, 2), sp("a.md", 5, 6), sp("b.md", 1, 4), sp("a.md", 10, 10)}
	near(t, "RR", ReciprocalRank(gold, results), 1.0/3) // third result touches the second set
	near(t, "none", ReciprocalRank(gold, results[:2]), 0)
	near(t, "empty", ReciprocalRank(gold, nil), 0)
}

// The classification separates "retrieved and ranked out" from "never retrieved".
func TestClassification(t *testing.T) {
	gold := []dataset.EvidenceSet{set(sp("a.md", 1, 10))}
	pool := []Span{sp("x.md", 1, 5), sp("a.md", 1, 10), sp("y.md", 1, 5)}

	good := Evaluate(gold, []Span{sp("a.md", 1, 10)}, pool, false, 0.5)
	if good.Class != ClassNormal || !good.PoolHit {
		t.Errorf("hit: %+v", good)
	}
	ranked := Evaluate(gold, []Span{sp("x.md", 1, 5)}, pool, false, 0.5)
	if ranked.Class != ClassMisranked || !ranked.PoolHit || ranked.Hit {
		t.Errorf("evidence in the pool but not in the top K must be misranked: %+v", ranked)
	}
	near(t, "pool coverage", ranked.PoolCoverage, 1)
	missed := Evaluate(gold, []Span{sp("x.md", 1, 5)}, []Span{sp("x.md", 1, 5), sp("y.md", 1, 5)}, false, 0.5)
	if missed.Class != ClassNotRecalled || missed.PoolHit {
		t.Errorf("evidence nowhere in the pool must be not-recalled: %+v", missed)
	}
	failed := Evaluate(gold, []Span{sp("a.md", 1, 10)}, pool, true, 0.5)
	if failed.Class != ClassError || failed.Hit || failed.Coverage != 0 {
		t.Errorf("a failed retrieval must score zero and be its own class: %+v", failed)
	}
}

// A pool coverage is the same measure applied to the candidate list.
func TestPoolCoverageUsesTheSameMeasure(t *testing.T) {
	gold := []dataset.EvidenceSet{set(sp("a.md", 1, 4))}
	out := Evaluate(gold, nil, []Span{sp("a.md", 1, 1), sp("a.md", 3, 3)}, false, 0.5)
	near(t, "pool coverage", out.PoolCoverage, 0.5) // lines 1 and 3 of 4
	if !out.PoolHit || out.Class != ClassMisranked {
		t.Errorf("%+v", out)
	}
}
