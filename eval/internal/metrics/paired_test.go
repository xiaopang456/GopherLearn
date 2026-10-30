package metrics

import (
	"math"
	"testing"
)

func TestMcNemarExactByHand(t *testing.T) {
	// n = b + c discordant pairs; two-sided p = 2 * P(X <= min(b, c)), X ~ Bin(n, 1/2).
	cases := []struct {
		d    Discordant
		want float64
	}{
		{Discordant{5, 0}, 2 * 1.0 / 32},               // 2 * (1/32)
		{Discordant{0, 5}, 2 * 1.0 / 32},               // symmetric
		{Discordant{8, 2}, 2 * (1.0 + 10 + 45) / 1024}, // 2 * 56/1024
		{Discordant{4, 4}, 1},                          // 2 * P(X<=4) of Bin(8) = 2*163/256 > 1 -> capped
		{Discordant{1, 0}, 1},                          // 2 * (1/2) = 1
		{Discordant{0, 0}, 1},                          // nothing to test
		{Discordant{10, 0}, 2 * 1.0 / 1024},            // 2 * 2^-10
		{Discordant{3, 1}, 2 * (1.0 + 4) / 16},         // 2 * 5/16
	}
	for _, c := range cases {
		if got := McNemarExact(c.d); math.Abs(got-c.want) > 1e-12 {
			t.Errorf("McNemarExact(%+v) = %v, want %v", c.d, got, c.want)
		}
	}
}

func TestCountDiscordant(t *testing.T) {
	a := []bool{true, true, false, false, true}
	b := []bool{true, false, true, false, false}
	got := CountDiscordant(a, b)
	if got.AOnly != 2 || got.BOnly != 1 {
		t.Fatalf("%+v, want A-only 2 (questions 2 and 5), B-only 1 (question 3)", got)
	}
}

func TestMinDetectableDifferenceByHand(t *testing.T) {
	// n = 100, 10 discordant pairs: p_d = 0.1, (1.959964 + 0.841621) * sqrt(0.1/100).
	got, ok := MinDetectableHitDifference(100, Discordant{7, 3})
	want := 2.801585 * math.Sqrt(0.001)
	if !ok || math.Abs(got-want) > 1e-6 {
		t.Errorf("hit MDE = %v (%v), want %v", got, ok, want)
	}
	if _, ok := MinDetectableHitDifference(100, Discordant{}); ok {
		t.Error("with no disagreement the effect size is undefined")
	}
	if _, ok := MinDetectableHitDifference(0, Discordant{1, 0}); ok {
		t.Error("n = 0 has no MDE")
	}

	// sd = 0.2, n = 50: 2.801585 * 0.2 / sqrt(50).
	mean, ok := MinDetectableMeanDifference(50, 0.2)
	if want := 2.801585 * 0.2 / math.Sqrt(50); !ok || math.Abs(mean-want) > 1e-6 {
		t.Errorf("mean MDE = %v, want %v", mean, want)
	}
	if _, ok := MinDetectableMeanDifference(50, 0); ok {
		t.Error("zero spread has no MDE")
	}
	if _, ok := MinDetectableMeanDifference(1, 0.5); ok {
		t.Error("one question has no MDE")
	}
}

func TestCompareMeanAndSDByHand(t *testing.T) {
	// differences A-B: 0.5, 0.25, -0.25, 0 -> mean 0.125
	pairs := []Pair{{"d1", 1, 0.5}, {"d1", 0.75, 0.5}, {"d2", 0.25, 0.5}, {"d2", 0.5, 0.5}}
	d := Compare(pairs, 1, 500)
	if d.N != 4 || d.Clusters != 2 || math.Abs(d.Mean-0.125) > 1e-12 {
		t.Fatalf("%+v", d)
	}
	// sample variance: ((0.375)^2 + (0.125)^2 + (-0.375)^2 + (-0.125)^2) / 3
	want := math.Sqrt((0.140625 + 0.015625 + 0.140625 + 0.015625) / 3)
	if math.Abs(d.SD-want) > 1e-12 {
		t.Errorf("SD = %v, want %v", d.SD, want)
	}
}

func TestCompareIntervalProperties(t *testing.T) {
	// Every difference identical: no sampling variability, so the interval is the point.
	same := []Pair{{"a", 1, 0.5}, {"b", 1, 0.5}, {"c", 1, 0.5}, {"d", 1, 0.5}}
	d := Compare(same, 7, 1000)
	if !d.CI.OK || math.Abs(d.CI.Low-0.5) > 1e-12 || math.Abs(d.CI.High-0.5) > 1e-12 {
		t.Errorf("a constant difference must give a degenerate interval: %+v", d.CI)
	}

	// Many documents, A clearly better: the interval excludes zero.
	var better []Pair
	for i := 0; i < 20; i++ {
		better = append(better, Pair{Cluster: string(rune('a' + i)), A: 1, B: 0.25 + float64(i%3)*0.05})
	}
	d = Compare(better, 7, 2000)
	if !d.CI.OK || d.CI.Low <= 0 || d.CI.Low > d.Mean || d.CI.High < d.Mean {
		t.Errorf("A is better everywhere: mean %v CI %+v", d.Mean, d.CI)
	}

	// Differences of both signs: the interval straddles zero.
	var mixed []Pair
	for i := 0; i < 20; i++ {
		a, b := 1.0, 0.0
		if i%2 == 1 {
			a, b = 0, 1
		}
		mixed = append(mixed, Pair{Cluster: string(rune('a' + i)), A: a, B: b})
	}
	d = Compare(mixed, 7, 2000)
	if !d.CI.OK || d.CI.Low >= 0 || d.CI.High <= 0 {
		t.Errorf("a balanced difference must straddle zero: %+v", d.CI)
	}
}

func TestCompareIsDeterministicAndSeeded(t *testing.T) {
	var pairs []Pair
	for i := 0; i < 15; i++ {
		pairs = append(pairs, Pair{Cluster: string(rune('a' + i%6)), A: float64(i%4) / 3, B: float64(i%3) / 2})
	}
	first := Compare(pairs, 42, 1000)
	if again := Compare(pairs, 42, 1000); again != first {
		t.Fatal("the same data and seed gave different results")
	}
	if other := Compare(pairs, 43, 1000); other.CI == first.CI {
		t.Error("a different seed gave the identical interval; the seed is not used")
	}
	// Pair order must not matter.
	reversed := make([]Pair, len(pairs))
	for i, p := range pairs {
		reversed[len(pairs)-1-i] = p
	}
	if got := Compare(reversed, 42, 1000); got != first {
		t.Errorf("the result depends on the order of the questions: %+v vs %+v", got, first)
	}
}

// Resampling documents rather than questions widens the interval when questions
// in one document are alike.
func TestClusterBootstrapIsWiderThanAQuestionBootstrap(t *testing.T) {
	var clustered, independent []Pair
	for doc := 0; doc < 6; doc++ {
		effect := 1.0
		if doc%2 == 1 {
			effect = -1
		}
		for q := 0; q < 10; q++ {
			clustered = append(clustered, Pair{Cluster: string(rune('a' + doc)), A: effect, B: 0})
			independent = append(independent, Pair{Cluster: string(rune('a'+doc)) + string(rune('0'+q)), A: effect, B: 0})
		}
	}
	c := Compare(clustered, 5, 2000).CI
	i := Compare(independent, 5, 2000).CI
	if !(c.High-c.Low > i.High-i.Low) {
		t.Errorf("clustered width %v, independent width %v; the document effect must widen the interval", c.High-c.Low, i.High-i.Low)
	}
}

func TestCompareEdgeCases(t *testing.T) {
	if d := Compare(nil, 1, 100); d.N != 0 || d.CI.OK {
		t.Errorf("%+v", d)
	}
	one := Compare([]Pair{{"a", 1, 0}, {"a", 0, 0}}, 1, 100)
	if one.CI.OK || one.Clusters != 1 || one.Mean != 0.5 {
		t.Errorf("a single document cannot be bootstrapped: %+v", one)
	}
}
