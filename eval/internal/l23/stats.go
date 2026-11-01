package l23

import (
	"fmt"
	"sort"

	"github.com/xiaopang456/GopherLearn/eval/internal/metrics"
)

// Layer names.
const (
	LayerL2 = "l2"
	LayerL3 = "l3"
)

// Options are the settings of a report.
type Options struct {
	// Baseline is the arm the others are compared with. Empty is allowed only for one arm.
	Baseline  string
	Seed      int64
	Resamples int
}

// PassK is pass^k: the chance that k trials of one question, chosen from its repeats,
// all succeed, averaged over the questions that have at least k repeats.
type PassK struct {
	K         int     `json:"k"`
	Value     float64 `json:"value"`
	Questions int     `json:"questions"`
	LeftOut   int     `json:"left_out"`
}

// AssertionRate counts the verdicts of one assertion over a layer's trials.
type AssertionRate struct {
	Name       string `json:"name"`
	Evaluated  int    `json:"evaluated"`
	Pass       int    `json:"pass"`
	Fail       int    `json:"fail"`
	NeedsJudge int    `json:"needs_judge"`
	Error      int    `json:"error"`
}

// ArmLayer is one arm's result on one layer.
type ArmLayer struct {
	Arm   string `json:"arm"`
	Layer string `json:"layer"`
	// Questions and Trials: a trial is one (question, repeat).
	Questions  int `json:"questions"`
	Trials     int `json:"trials"`
	Repeats    int `json:"repeats"`
	Pass       int `json:"pass"`
	Fail       int `json:"fail"`
	NeedsJudge int `json:"needs_judge"`
	Error      int `json:"error"`
	// Pass1Strict counts every trial that did not pass as unsuccessful. Pass1Decided
	// leaves out the trials the structure could not decide (needs_judge) and the ones the
	// service did not answer (error): pass / (pass + fail).
	Pass1Strict  float64         `json:"pass_at_1_strict"`
	Pass1Decided *float64        `json:"pass_at_1_decided"`
	PassK        []PassK         `json:"pass_k"`
	Classes      map[string]int  `json:"classes"`
	Tiers        map[string]int  `json:"tiers,omitempty"`
	Assertions   []AssertionRate `json:"assertions"`
}

// Comparison is one arm against the baseline on one layer, over the questions both ran
// with the same number of repeats. A question counts as a success only if every repeat
// passed.
type Comparison struct {
	Layer          string             `json:"layer"`
	Arm            string             `json:"arm"`
	Baseline       string             `json:"baseline"`
	Questions      int                `json:"questions"`
	Clusters       int                `json:"clusters"`
	Repeats        int                `json:"repeats"`
	LeftOut        int                `json:"left_out"`
	ArmOnly        int                `json:"arm_only"`
	BaselineOnly   int                `json:"baseline_only"`
	McNemarP       float64            `json:"mcnemar_p"`
	RateDifference metrics.Difference `json:"rate_difference"`
	AllKMDE        *float64           `json:"all_k_min_detectable"`
	RateMDE        *float64           `json:"rate_min_detectable"`
}

// CostRow is the average cost of an arm's trials, from what the runs reported.
type CostRow struct {
	Arm              string   `json:"arm"`
	Trials           int      `json:"trials"`
	MeanModelCalls   *float64 `json:"mean_model_calls"`
	MeanToolCalls    *float64 `json:"mean_tool_calls"`
	MedianDurationMs *float64 `json:"median_duration_ms"`
}

// Stats is everything a report shows.
type Stats struct {
	Arms        []string     `json:"arms"`
	ArmLayers   []ArmLayer   `json:"arm_layers"`
	Comparisons []Comparison `json:"comparisons"`
	Costs       []CostRow    `json:"costs"`
	// Envs is the set of environments the trials ran in.
	Envs []string `json:"envs"`
}

type trial struct {
	verdict string
	outcome Outcome
}

// Compute works out the statistics of a set of outcomes.
func Compute(outcomes []Outcome, opts Options) (*Stats, error) {
	if len(outcomes) == 0 {
		return nil, fmt.Errorf("no outcomes to report")
	}
	armSet, envSet := map[string]bool{}, map[string]bool{}
	for _, o := range outcomes {
		armSet[o.Arm], envSet[o.Env] = true, true
	}
	stats := &Stats{Arms: sortedKeys(armSet), Envs: sortedKeys(envSet)}
	if opts.Baseline != "" && !armSet[opts.Baseline] {
		return nil, fmt.Errorf("baseline arm %q has no outcomes (arms: %v)", opts.Baseline, stats.Arms)
	}
	if len(stats.Arms) > 1 && opts.Baseline == "" {
		return nil, fmt.Errorf("there are %d arms %v; say which one is the baseline", len(stats.Arms), stats.Arms)
	}

	for _, arm := range stats.Arms {
		for _, layer := range []string{LayerL2, LayerL3} {
			if al, ok := armLayer(outcomes, arm, layer); ok {
				stats.ArmLayers = append(stats.ArmLayers, al)
			}
		}
		stats.Costs = append(stats.Costs, cost(outcomes, arm))
	}
	for _, arm := range stats.Arms {
		if arm == opts.Baseline {
			continue
		}
		for _, layer := range []string{LayerL2, LayerL3} {
			if cmp, ok := compare(outcomes, layer, arm, opts); ok {
				stats.Comparisons = append(stats.Comparisons, cmp)
			}
		}
	}
	return stats, nil
}

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// byQuestion groups an arm's trials on a layer by question, repeats in order.
func byQuestion(outcomes []Outcome, arm, layer string) (map[string][]trial, []string) {
	groups := map[string][]trial{}
	for _, o := range outcomes {
		if o.Arm != arm {
			continue
		}
		if l := o.Layer(layer); l != nil {
			groups[o.QID] = append(groups[o.QID], trial{verdict: l.Verdict, outcome: o})
		}
	}
	ids := make([]string, 0, len(groups))
	for id := range groups {
		sort.Slice(groups[id], func(i, j int) bool { return groups[id][i].outcome.Rep < groups[id][j].outcome.Rep })
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return groups, ids
}

func successes(trials []trial) int {
	n := 0
	for _, t := range trials {
		if t.verdict == Pass {
			n++
		}
	}
	return n
}

// passAllK is C(c, k) / C(n, k): the chance that k trials drawn from n, c of which
// succeeded, all succeeded.
func passAllK(c, n, k int) float64 {
	if c < k {
		return 0
	}
	p := 1.0
	for i := 0; i < k; i++ {
		p *= float64(c-i) / float64(n-i)
	}
	return p
}

func armLayer(outcomes []Outcome, arm, layer string) (ArmLayer, bool) {
	groups, ids := byQuestion(outcomes, arm, layer)
	if len(ids) == 0 {
		return ArmLayer{}, false
	}
	al := ArmLayer{Arm: arm, Layer: layer, Questions: len(ids), Classes: map[string]int{}}
	rates := map[string]*AssertionRate{}
	for _, id := range ids {
		trials := groups[id]
		al.Repeats = max(al.Repeats, len(trials))
		for _, t := range trials {
			al.Trials++
			l := t.outcome.Layer(layer)
			switch t.verdict {
			case Pass:
				al.Pass++
			case Fail:
				al.Fail++
			case NeedsJudge:
				al.NeedsJudge++
			case Error:
				al.Error++
			default:
				return ArmLayer{}, false
			}
			if t.verdict != Pass && l.Class != "" {
				al.Classes[l.Class]++
			}
			if l.Tier != "" {
				if al.Tiers == nil {
					al.Tiers = map[string]int{}
				}
				al.Tiers[l.Tier]++
			}
			for _, a := range l.Assertions {
				if a.Informational {
					continue
				}
				r := rates[a.Name]
				if r == nil {
					r = &AssertionRate{Name: a.Name}
					rates[a.Name] = r
				}
				r.Evaluated++
				switch a.Verdict {
				case Pass:
					r.Pass++
				case Fail:
					r.Fail++
				case NeedsJudge:
					r.NeedsJudge++
				case Error:
					r.Error++
				}
			}
		}
	}
	al.Pass1Strict = float64(al.Pass) / float64(al.Trials)
	if decided := al.Pass + al.Fail; decided > 0 {
		v := float64(al.Pass) / float64(decided)
		al.Pass1Decided = &v
	}
	ks := []int{al.Repeats}
	if al.Repeats > 2 {
		ks = append(ks, 2)
	}
	for _, k := range ks {
		pk := PassK{K: k}
		var sum float64
		for _, id := range ids {
			trials := groups[id]
			if len(trials) < k {
				pk.LeftOut++
				continue
			}
			pk.Questions++
			sum += passAllK(successes(trials), len(trials), k)
		}
		if pk.Questions > 0 {
			pk.Value = sum / float64(pk.Questions)
		}
		al.PassK = append(al.PassK, pk)
	}
	names := make([]string, 0, len(rates))
	for name := range rates {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		al.Assertions = append(al.Assertions, *rates[name])
	}
	return al, true
}

func compare(outcomes []Outcome, layer, arm string, opts Options) (Comparison, bool) {
	a, aIDs := byQuestion(outcomes, arm, layer)
	b, _ := byQuestion(outcomes, opts.Baseline, layer)
	cmp := Comparison{Layer: layer, Arm: arm, Baseline: opts.Baseline}
	var pairs []metrics.Pair
	var armWins, baseWins []bool
	for _, id := range aIDs {
		ta, tb := a[id], b[id]
		if len(tb) == 0 || len(ta) != len(tb) {
			cmp.LeftOut++
			continue
		}
		n := len(ta)
		cmp.Repeats = max(cmp.Repeats, n)
		ca, cb := successes(ta), successes(tb)
		pairs = append(pairs, metrics.Pair{Cluster: ta[0].outcome.Cluster, A: float64(ca) / float64(n), B: float64(cb) / float64(n)})
		armWins = append(armWins, ca == n)
		baseWins = append(baseWins, cb == n)
	}
	if len(pairs) == 0 {
		return Comparison{}, false
	}
	cmp.Questions = len(pairs)
	cmp.RateDifference = metrics.Compare(pairs, opts.Seed, opts.Resamples)
	cmp.Clusters = cmp.RateDifference.Clusters
	d := metrics.CountDiscordant(armWins, baseWins)
	cmp.ArmOnly, cmp.BaselineOnly = d.AOnly, d.BOnly
	cmp.McNemarP = metrics.McNemarExact(d)
	if v, ok := metrics.MinDetectableHitDifference(cmp.Questions, d); ok {
		cmp.AllKMDE = &v
	}
	if v, ok := metrics.MinDetectableMeanDifference(cmp.Questions, cmp.RateDifference.SD); ok {
		cmp.RateMDE = &v
	}
	return cmp, true
}

func cost(outcomes []Outcome, arm string) CostRow {
	row := CostRow{Arm: arm}
	var models, tools, durations []float64
	for _, o := range outcomes {
		if o.Arm != arm {
			continue
		}
		row.Trials++
		if o.Cost.ModelCalls != nil {
			models = append(models, *o.Cost.ModelCalls)
		}
		if o.Cost.ToolCalls != nil {
			tools = append(tools, *o.Cost.ToolCalls)
		}
		if o.Cost.DurationMs != nil {
			durations = append(durations, *o.Cost.DurationMs)
		}
	}
	row.MeanModelCalls, row.MeanToolCalls, row.MedianDurationMs = mean(models), mean(tools), median(durations)
	return row
}

func mean(values []float64) *float64 {
	if len(values) == 0 {
		return nil
	}
	var sum float64
	for _, v := range values {
		sum += v
	}
	m := sum / float64(len(values))
	return &m
}

func median(values []float64) *float64 {
	if len(values) == 0 {
		return nil
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	n := len(sorted)
	m := sorted[n/2]
	if n%2 == 0 {
		m = (sorted[n/2-1] + sorted[n/2]) / 2
	}
	return &m
}
