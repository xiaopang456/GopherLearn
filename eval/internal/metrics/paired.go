package metrics

import (
	"math"
	"math/rand"
	"sort"
)

// Pair is one question's value under two cells. Cluster names the document the
// question's evidence sits in: questions about the same document are not
// independent, so the bootstrap resamples documents, not questions.
type Pair struct {
	Cluster string
	A, B    float64
}

// Interval is a 95% confidence interval. OK is false when it cannot be formed
// (fewer than two clusters).
type Interval struct {
	Low, High float64
	OK        bool
}

// Difference summarises mean(A - B) over paired questions.
type Difference struct {
	N        int
	Clusters int
	Mean     float64
	CI       Interval
	// SD is the standard deviation of the per-question differences.
	SD float64
}

// z-scores for a two-sided 5% test with 80% power.
const (
	zAlpha = 1.959964
	zPower = 0.841621
)

// Compare returns the mean paired difference A - B with a 95% percentile
// confidence interval from a cluster bootstrap: each of `resamples` draws picks as
// many clusters as there are, with replacement, and takes the mean difference of
// the questions in them. The generator is seeded, so the interval is a function
// of the data and the seed.
func Compare(pairs []Pair, seed int64, resamples int) Difference {
	d := Difference{N: len(pairs)}
	if len(pairs) == 0 {
		return d
	}
	// Clusters are visited in name order and each cluster's differences in value
	// order, so neither the order of the questions nor map iteration can change a
	// floating-point sum: the result is a function of the data alone.
	byCluster := map[string][]float64{}
	var order []string
	for _, p := range pairs {
		if _, ok := byCluster[p.Cluster]; !ok {
			order = append(order, p.Cluster)
		}
		byCluster[p.Cluster] = append(byCluster[p.Cluster], p.A-p.B)
	}
	sort.Strings(order)
	d.Clusters = len(order)
	clusterSum := make([]float64, len(order))
	clusterN := make([]int, len(order))
	var sum float64
	for i, name := range order {
		diffs := byCluster[name]
		sort.Float64s(diffs)
		for _, v := range diffs {
			clusterSum[i] += v
		}
		clusterN[i] = len(diffs)
		sum += clusterSum[i]
	}
	d.Mean = sum / float64(len(pairs))
	var squares float64
	for _, name := range order {
		for _, v := range byCluster[name] {
			squares += (v - d.Mean) * (v - d.Mean)
		}
	}
	if len(pairs) > 1 {
		d.SD = math.Sqrt(squares / float64(len(pairs)-1))
	}
	if len(order) < 2 {
		return d
	}

	rng := rand.New(rand.NewSource(seed))
	means := make([]float64, resamples)
	for r := range means {
		var total float64
		var n int
		for range order {
			i := rng.Intn(len(order))
			total += clusterSum[i]
			n += clusterN[i]
		}
		means[r] = total / float64(n)
	}
	sort.Float64s(means)
	d.CI = Interval{Low: quantile(means, 0.025), High: quantile(means, 0.975), OK: true}
	return d
}

// quantile reads a sorted sample with linear interpolation.
func quantile(sorted []float64, q float64) float64 {
	if len(sorted) == 1 {
		return sorted[0]
	}
	pos := q * float64(len(sorted)-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	return sorted[lo] + (sorted[hi]-sorted[lo])*(pos-float64(lo))
}

// Discordant counts the pairs on which two binary outcomes disagree.
type Discordant struct {
	// AOnly is the number of questions A hit and B missed; BOnly the reverse.
	AOnly, BOnly int
}

// CountDiscordant tallies hit/miss disagreement over paired outcomes.
func CountDiscordant(a, b []bool) Discordant {
	var d Discordant
	for i := range a {
		switch {
		case a[i] && !b[i]:
			d.AOnly++
		case !a[i] && b[i]:
			d.BOnly++
		}
	}
	return d
}

// McNemarExact returns the two-sided exact p-value of the McNemar test: under no
// difference each discordant pair is equally likely to favour either side, so the
// smaller count follows Binomial(b+c, 1/2). With no discordant pair there is
// nothing to test and the p-value is 1.
func McNemarExact(d Discordant) float64 {
	n := d.AOnly + d.BOnly
	if n == 0 {
		return 1
	}
	smaller := min(d.AOnly, d.BOnly)
	var tail float64
	for k := 0; k <= smaller; k++ {
		tail += math.Exp(logChoose(n, k) - float64(n)*math.Ln2)
	}
	return math.Min(1, 2*tail)
}

func logChoose(n, k int) float64 {
	a, _ := math.Lgamma(float64(n + 1))
	b, _ := math.Lgamma(float64(k + 1))
	c, _ := math.Lgamma(float64(n - k + 1))
	return a - b - c
}

// MinDetectableHitDifference is the smallest difference in hit rate that a
// paired comparison of n questions would detect with 80% power at the 5% level,
// given the share of questions on which the two cells disagree. It is the usual
// normal approximation for paired proportions, (z_a + z_b) * sqrt(p_d / n), and
// is a planning figure, not a test. It is not defined without disagreement.
func MinDetectableHitDifference(n int, d Discordant) (float64, bool) {
	if n == 0 || d.AOnly+d.BOnly == 0 {
		return 0, false
	}
	pd := float64(d.AOnly+d.BOnly) / float64(n)
	return (zAlpha + zPower) * math.Sqrt(pd/float64(n)), true
}

// MinDetectableMeanDifference is the same for a continuous per-question score
// whose paired differences have standard deviation sd.
func MinDetectableMeanDifference(n int, sd float64) (float64, bool) {
	if n < 2 || sd == 0 {
		return 0, false
	}
	return (zAlpha + zPower) * sd / math.Sqrt(float64(n)), true
}
