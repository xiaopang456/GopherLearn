package report

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"sort"

	"encoding/json"

	"github.com/xiaopang456/GopherLearn/eval/internal/config"
	"github.com/xiaopang456/GopherLearn/eval/internal/dataset"
	"github.com/xiaopang456/GopherLearn/eval/internal/metrics"
	"github.com/xiaopang456/GopherLearn/eval/internal/runner"
)

// key names one (cell, question) record.
type key struct{ cell, question string }

// ---- the signal an unanswerable question is described by -----------------------

// The raw signals a retrieval gives. The fused score the service returns is
// normalised, so a single-channel cell always shows 1.0 at the top and the number says
// nothing about how well the query matched; these three do.
const (
	SignalVectorDistance = "vector_distance" // cosine distance of the nearest chunk: lower is a stronger match
	SignalRerankScore    = "rerank_score"    // the reranker's score of the top result: higher is stronger
	SignalLexicalScore   = "lexical_score"   // the keyword channel's own score of the top result: higher is stronger
)

// SignalStats describes how a cell's signal differs between questions that have an
// answer and questions that do not. It is a description, not a threshold: nothing here
// decides when to refuse.
type SignalStats struct {
	Name string `json:"name"`
	// Stronger is "lower" or "higher": which direction means a better match.
	Stronger     string `json:"stronger"`
	Unanswerable int    `json:"unanswerable"`
	Answerable   int    `json:"answerable"`
	// UnanswerableMinMedMax is the signal of the best result of each unanswerable question.
	UnanswerableMinMedMax []float64 `json:"unanswerable_min_med_max,omitempty"`
	AnswerableMedian      *float64  `json:"answerable_median,omitempty"`
	// AUC is the chance that a randomly chosen answerable question has a stronger signal
	// than a randomly chosen unanswerable one (ties count half). 0.5 means the signal
	// does not separate them.
	AUC *float64 `json:"auc,omitempty"`
}

// signalKind picks the signal a cell is described by: the reranker's score where it
// ran, the keyword score for a keyword-only cell, and otherwise the vector distance.
func signalKind(meta runner.Record) string {
	switch {
	case meta.Method == "":
		return ""
	case meta.Method == config.MethodKeyword:
		return SignalLexicalScore
	case meta.RerankStatus == "applied":
		return SignalRerankScore
	default:
		return SignalVectorDistance
	}
}

// signalFor reads the signal of one question's best result. ok is false when the
// record does not carry it (a failed retrieval, no results, or a record written before
// the signal was kept).
func signalFor(kind string, rec runner.Record) (float64, bool) {
	if rec.ErrorKind != "" {
		return 0, false
	}
	switch kind {
	case SignalRerankScore:
		if len(rec.Results) == 0 {
			return 0, false
		}
		return rec.Results[0].RerankScore, true
	case SignalLexicalScore:
		if len(rec.Results) == 0 || rec.Results[0].LexicalScore <= 0 {
			return 0, false
		}
		return rec.Results[0].LexicalScore, true
	case SignalVectorDistance:
		// The nearest chunk the vector channel returned, wherever it ended up after fusion.
		best, found := 0.0, false
		for _, h := range append(append([]runner.Hit{}, rec.Pool...), rec.Results...) {
			if h.VectorRank > 0 && (!found || h.Distance < best) {
				best, found = h.Distance, true
			}
		}
		return best, found
	}
	return 0, false
}

func buildSignal(kind string, unanswerable, answerable []float64) *SignalStats {
	if kind == "" || (len(unanswerable) == 0 && len(answerable) == 0) {
		return nil
	}
	s := &SignalStats{Name: kind, Stronger: "higher", Unanswerable: len(unanswerable), Answerable: len(answerable)}
	if kind == SignalVectorDistance {
		s.Stronger = "lower"
	}
	if len(unanswerable) > 0 {
		sorted := append([]float64(nil), unanswerable...)
		sort.Float64s(sorted)
		s.UnanswerableMinMedMax = []float64{sorted[0], median(sorted), sorted[len(sorted)-1]}
	}
	if len(answerable) > 0 {
		sorted := append([]float64(nil), answerable...)
		sort.Float64s(sorted)
		m := median(sorted)
		s.AnswerableMedian = &m
	}
	if len(unanswerable) > 0 && len(answerable) > 0 {
		var wins float64
		for _, a := range answerable {
			for _, u := range unanswerable {
				switch {
				case a == u:
					wins += 0.5
				case (kind == SignalVectorDistance && a < u) || (kind != SignalVectorDistance && a > u):
					wins++
				}
			}
		}
		auc := wins / float64(len(answerable)*len(unanswerable))
		s.AUC = &auc
	}
	return s
}

// ---- splitting a run by what kind of question it was --------------------------

// The question properties a run can be split by. They live in the question sidecar, not
// in the question file, so a run is only split when that sidecar is supplied.
var GroupDimensions = []string{"lang", "paraphrase"}

// GroupStats are one cell's aggregates over the answerable questions that share one
// value of one property (for example every question asked in Chinese about an English
// source).
type GroupStats struct {
	Cell         string  `json:"cell"`
	Dimension    string  `json:"dimension"`
	Value        string  `json:"value"`
	Answerable   int     `json:"answerable"`
	Hits         int     `json:"hits"`
	HitRate      float64 `json:"hit_rate"`
	MeanCoverage float64 `json:"mean_coverage"`
	MRR          float64 `json:"mrr"`
}

// GroupComparison is a paired comparison restricted to one group.
type GroupComparison struct {
	Dimension string `json:"dimension"`
	Value     string `json:"value"`
	Comparison
}

var idPrefix = regexp.MustCompile(`^\s*\{\s*"id"\s*:\s*"([^"]*)"`)

// LoadGroups reads the properties of the wanted questions from a question sidecar.
//
// The sidecar of a rehearsal covers every question, including ones the caller must not
// read. A line whose id is not wanted is therefore skipped on its id alone: its body is
// not decoded, so nothing about it can reach a report. Every wanted question must be
// present and carry every dimension.
func LoadGroups(path string, wanted map[string]bool) (map[string]map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read the question sidecar: %w", err)
	}
	defer file.Close()
	groups := map[string]map[string]string{}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for line := 1; scanner.Scan(); line++ {
		match := idPrefix.FindSubmatch(scanner.Bytes())
		if match == nil || !wanted[string(match[1])] {
			continue
		}
		var row map[string]json.RawMessage
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			return nil, fmt.Errorf("sidecar line %d: %w", line, err)
		}
		id := string(match[1])
		values := map[string]string{}
		for _, dim := range GroupDimensions {
			var value string
			if raw, ok := row[dim]; !ok || json.Unmarshal(raw, &value) != nil || value == "" {
				return nil, fmt.Errorf("sidecar line %d: question %q has no usable %q", line, id, dim)
			}
			values[dim] = value
		}
		if _, dup := groups[id]; dup {
			return nil, fmt.Errorf("sidecar line %d: question %q appears twice", line, id)
		}
		groups[id] = values
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	missing := 0
	for id := range wanted {
		if _, ok := groups[id]; !ok {
			missing++
		}
	}
	if missing > 0 {
		return nil, fmt.Errorf("the question sidecar has no row for %d of the %d questions", missing, len(wanted))
	}
	return groups, nil
}

// addGroups computes the per-group aggregates and comparisons.
func (rep *Report) addGroups(in Input, info map[string]runner.Record, outcomes map[key]metrics.Outcome) {
	if len(in.Groups) == 0 {
		return
	}
	values := map[string][]string{}
	for _, dim := range GroupDimensions {
		seen := map[string]bool{}
		for _, q := range in.Questions {
			if v, ok := in.Groups[q.ID][dim]; ok && q.Answerable {
				seen[v] = true
			}
		}
		values[dim] = sortedStrings(seen)
	}
	for _, cell := range in.Manifest.Cells {
		for _, dim := range GroupDimensions {
			for _, value := range values[dim] {
				gs := GroupStats{Cell: cell, Dimension: dim, Value: value}
				var coverage, rr float64
				for _, q := range in.Questions {
					out, have := outcomes[key{cell, q.ID}]
					if !q.Answerable || !have || in.Groups[q.ID][dim] != value {
						continue
					}
					gs.Answerable++
					coverage += out.Coverage
					rr += out.ReciprocalRank
					if out.Hit {
						gs.Hits++
					}
				}
				if gs.Answerable == 0 {
					continue
				}
				n := float64(gs.Answerable)
				gs.HitRate, gs.MeanCoverage, gs.MRR = float64(gs.Hits)/n, coverage/n, rr/n
				rep.ByGroup = append(rep.ByGroup, gs)
			}
		}
	}
	for _, dim := range GroupDimensions {
		for _, value := range values[dim] {
			dim, value := dim, value
			include := func(q dataset.Question) bool { return in.Groups[q.ID][dim] == value }
			for _, pair := range comparisonPairs(in.Manifest.Cells, info) {
				cmp, ok := compare(in, pair[0], pair[1], func(cell, question string) (metrics.Outcome, bool) {
					out, have := outcomes[key{cell, question}]
					return out, have
				}, include)
				if ok {
					rep.GroupComparisons = append(rep.GroupComparisons, GroupComparison{Dimension: dim, Value: value, Comparison: cmp})
				}
			}
		}
	}
}

func sortedStrings(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
