// Package report turns a run's raw records into the evaluation's tables.
//
// Scoring is offline and repeatable: it reads a run directory and the question
// file, calls no service, and produces the same bytes every time.
package report

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/xiaopang456/GopherLearn/eval/internal/config"
	"github.com/xiaopang456/GopherLearn/eval/internal/dataset"
	"github.com/xiaopang456/GopherLearn/eval/internal/matrix"
	"github.com/xiaopang456/GopherLearn/eval/internal/metrics"
	"github.com/xiaopang456/GopherLearn/eval/internal/runner"
)

// Input is everything scoring needs.
type Input struct {
	Manifest runner.Manifest
	// Summary is the run's summary.json; nil when the run left none (it was killed).
	Summary   *runner.Summary
	Records   []runner.Record
	Questions []dataset.Question
	Threshold float64
	Seed      int64
	Resamples int
	// Groups maps a question id to its properties (see GroupDimensions). Nil means the
	// run is not split.
	Groups map[string]map[string]string
}

// CellStats are the aggregates of one cell over the answerable questions.
type CellStats struct {
	Cell   string `json:"cell"`
	Method string `json:"method"`
	Rule   string `json:"rule,omitempty"`
	TopK   int    `json:"top_k"`

	// Answerable is the number of answerable questions with a record in the cell.
	// Missing counts the questions of either kind without one (an incomplete run).
	Answerable int `json:"answerable"`
	Missing    int `json:"missing"`
	// Errors is the number of answerable questions whose retrieval failed.
	Errors int `json:"errors"`

	Hits         int     `json:"hits"`
	HitRate      float64 `json:"hit_rate"`
	MeanCoverage float64 `json:"mean_coverage"`
	MeanPurity   float64 `json:"mean_purity"`
	MRR          float64 `json:"mrr"`

	PoolHits    int `json:"pool_hits"`
	Normal      int `json:"normal"`
	Misranked   int `json:"misranked"`
	NotRecalled int `json:"not_recalled"`

	// Unanswerable questions are not scored; they are described by a raw signal.
	Unanswerable      int          `json:"unanswerable"`
	UnanswerableEmpty int          `json:"unanswerable_empty"`
	Signal            *SignalStats `json:"signal,omitempty"`
}

// Comparison is one paired comparison of a challenger cell against a baseline.
type Comparison struct {
	Challenger string `json:"challenger"`
	Baseline   string `json:"baseline"`
	N          int    `json:"n"`
	Clusters   int    `json:"clusters"`

	Hit         metrics.Difference `json:"hit_difference"`
	Coverage    metrics.Difference `json:"coverage_difference"`
	MRR         metrics.Difference `json:"mrr_difference"`
	Discordant  metrics.Discordant `json:"discordant"`
	McNemarP    float64            `json:"mcnemar_p"`
	HitMDE      *float64           `json:"hit_min_detectable"`
	CoverageMDE *float64           `json:"coverage_min_detectable"`
}

// TypeStats are the aggregates of one cell over the answerable questions of one
// question type (single_fact, comparison, multi_evidence ...). A multi-evidence
// question is covered only to the extent that all of its evidence was retrieved,
// so FullCoverage — every line of the best evidence set — is reported beside the
// thresholded hit.
type TypeStats struct {
	Cell         string  `json:"cell"`
	Type         string  `json:"type"`
	Answerable   int     `json:"answerable"`
	Hits         int     `json:"hits"`
	FullCoverage int     `json:"full_coverage"`
	MeanCoverage float64 `json:"mean_coverage"`
	MRR          float64 `json:"mrr"`
}

// Report is the scored run.
type Report struct {
	Input       Input
	Cells       []CellStats
	ByType      []TypeStats
	Comparisons []Comparison
	// ByGroup and GroupComparisons are filled only when the run is split (Input.Groups).
	ByGroup          []GroupStats
	GroupComparisons []GroupComparison
	// Incomplete is true when some (cell, question) has no record.
	Incomplete bool
	// Answerable and Unanswerable count the split's questions.
	Answerable   int
	Unanswerable int
}

// cellInfo parses a cell id back into its parts.
func cellInfo(records []runner.Record) map[string]runner.Record {
	first := map[string]runner.Record{}
	for _, r := range records {
		if _, ok := first[r.Cell]; !ok {
			first[r.Cell] = r
		}
	}
	return first
}

// Build scores the records.
func Build(in Input) (*Report, error) {
	known := map[string]dataset.Question{}
	for _, q := range in.Questions {
		known[q.ID] = q
	}
	byKey := map[key]runner.Record{}
	for _, r := range in.Records {
		if _, ok := known[r.QuestionID]; !ok {
			return nil, fmt.Errorf("record for question %q, which the question file does not have in the %q split", r.QuestionID, in.Manifest.Split)
		}
		k := key{r.Cell, r.QuestionID}
		if _, dup := byKey[k]; dup {
			return nil, fmt.Errorf("two records for question %q in cell %q", r.QuestionID, r.Cell)
		}
		byKey[k] = r
	}
	info := cellInfo(in.Records)

	rep := &Report{Input: in}
	for _, q := range in.Questions {
		if q.Answerable {
			rep.Answerable++
		} else {
			rep.Unanswerable++
		}
	}

	outcomes := map[key]metrics.Outcome{}
	for _, cell := range in.Manifest.Cells {
		meta, ok := info[cell]
		if !ok {
			// A cell with no record at all: the run stopped before it. Report it empty.
			meta = runner.Record{Cell: cell}
		}
		st := CellStats{Cell: cell, Method: meta.Method, Rule: meta.Rule, TopK: meta.TopK}
		var coverage, purity, rr float64
		var answerableSignal, unansweredSignal []float64
		kind := signalKind(meta)
		typeAgg := map[string]*TypeStats{}
		typeCov, typeRR := map[string]float64{}, map[string]float64{}
		for _, q := range in.Questions {
			rec, have := byKey[key{cell, q.ID}]
			if !q.Answerable {
				if !have {
					st.Missing++
					continue
				}
				st.Unanswerable++
				if len(rec.Results) == 0 {
					st.UnanswerableEmpty++
				}
				if v, ok := signalFor(kind, rec); ok {
					unansweredSignal = append(unansweredSignal, v)
				}
				continue
			}
			if !have {
				st.Missing++
				continue
			}
			st.Answerable++
			if v, ok := signalFor(kind, rec); ok {
				answerableSignal = append(answerableSignal, v)
			}
			out := metrics.Evaluate(q.Evidence, spans(rec.Results), spans(rec.Pool), rec.ErrorKind != "", in.Threshold)
			outcomes[key{cell, q.ID}] = out
			ts := typeAgg[q.Type]
			if ts == nil {
				ts = &TypeStats{Cell: cell, Type: q.Type}
				typeAgg[q.Type] = ts
			}
			ts.Answerable++
			typeCov[q.Type] += out.Coverage
			typeRR[q.Type] += out.ReciprocalRank
			if out.Hit {
				ts.Hits++
			}
			if out.Coverage >= 1-1e-9 {
				ts.FullCoverage++
			}
			coverage += out.Coverage
			purity += out.Purity
			rr += out.ReciprocalRank
			if out.Hit {
				st.Hits++
			}
			if out.PoolHit {
				st.PoolHits++
			}
			switch out.Class {
			case metrics.ClassNormal:
				st.Normal++
			case metrics.ClassMisranked:
				st.Misranked++
			case metrics.ClassNotRecalled:
				st.NotRecalled++
			case metrics.ClassError:
				st.Errors++
			}
		}
		if st.Answerable > 0 {
			n := float64(st.Answerable)
			st.HitRate, st.MeanCoverage, st.MeanPurity, st.MRR = float64(st.Hits)/n, coverage/n, purity/n, rr/n
		}
		if st.Missing > 0 {
			rep.Incomplete = true
		}
		st.Signal = buildSignal(kind, unansweredSignal, answerableSignal)
		rep.Cells = append(rep.Cells, st)
		types := make([]string, 0, len(typeAgg))
		for t := range typeAgg {
			types = append(types, t)
		}
		sort.Strings(types)
		for _, t := range types {
			ts := typeAgg[t]
			ts.MeanCoverage = typeCov[t] / float64(ts.Answerable)
			ts.MRR = typeRR[t] / float64(ts.Answerable)
			rep.ByType = append(rep.ByType, *ts)
		}
	}

	for _, pair := range comparisonPairs(in.Manifest.Cells, info) {
		cmp, ok := compare(in, pair[0], pair[1], func(cell, question string) (metrics.Outcome, bool) {
			out, have := outcomes[key{cell, question}]
			return out, have
		}, nil)
		if ok {
			rep.Comparisons = append(rep.Comparisons, cmp)
		}
	}
	rep.addGroups(in, info, outcomes)
	return rep, nil
}

func spans(hits []runner.Hit) []metrics.Span {
	out := make([]metrics.Span, len(hits))
	for i, h := range hits {
		out[i] = metrics.Span{File: h.File, Start: h.Start, End: h.End}
	}
	return out
}

func median(sorted []float64) float64 {
	n := len(sorted)
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}

// comparisonPairs lists the challenger/baseline cell pairs. The baseline is plain
// hybrid at the same K. A production-rule rerank cell above the production limit
// is skipped: it was bypassed, so it is hybrid by construction.
func comparisonPairs(cells []string, info map[string]runner.Record) [][2]string {
	have := map[string]bool{}
	for _, c := range cells {
		have[c] = true
	}
	var pairs [][2]string
	for _, c := range cells {
		r, ok := info[c]
		if !ok || r.Method == config.MethodHybrid {
			continue
		}
		base := fmt.Sprintf("%s/k=%d", config.MethodHybrid, r.TopK)
		if !have[base] {
			continue
		}
		if matrix.IsRerankMethod(r.Method) && r.Rule == matrix.RuleProduction && r.RerankStatus == "bypassed" {
			continue
		}
		pairs = append(pairs, [2]string{c, base})
		// Vector+rerank is also set against hybrid+rerank under the same rule and K: with a
		// reranker deciding the order, does the keyword channel add anything?
		if r.Method == config.MethodVectorRerank {
			other := fmt.Sprintf("%s/%s/k=%d", config.MethodHybridRerank, r.Rule, r.TopK)
			if have[other] {
				pairs = append(pairs, [2]string{c, other})
			}
		}
	}
	return pairs
}

// compare pairs two cells over the answerable questions both have a record for; include,
// when set, narrows it to a group of questions.
func compare(in Input, challenger, baseline string, outcome func(cell, question string) (metrics.Outcome, bool), include func(dataset.Question) bool) (Comparison, bool) {
	var hit, cov, mrr []metrics.Pair
	var a, b []bool
	for _, q := range in.Questions {
		if !q.Answerable || (include != nil && !include(q)) {
			continue
		}
		oa, okA := outcome(challenger, q.ID)
		ob, okB := outcome(baseline, q.ID)
		if !okA || !okB {
			continue
		}
		cluster := q.Evidence[0][0].File
		hit = append(hit, metrics.Pair{Cluster: cluster, A: boolFloat(oa.Hit), B: boolFloat(ob.Hit)})
		cov = append(cov, metrics.Pair{Cluster: cluster, A: oa.Coverage, B: ob.Coverage})
		mrr = append(mrr, metrics.Pair{Cluster: cluster, A: oa.ReciprocalRank, B: ob.ReciprocalRank})
		a, b = append(a, oa.Hit), append(b, ob.Hit)
	}
	if len(hit) == 0 {
		return Comparison{}, false
	}
	cmp := Comparison{Challenger: challenger, Baseline: baseline, N: len(hit)}
	cmp.Hit = metrics.Compare(hit, in.Seed, in.Resamples)
	cmp.Coverage = metrics.Compare(cov, in.Seed, in.Resamples)
	cmp.MRR = metrics.Compare(mrr, in.Seed, in.Resamples)
	cmp.Clusters = cmp.Hit.Clusters
	cmp.Discordant = metrics.CountDiscordant(a, b)
	cmp.McNemarP = metrics.McNemarExact(cmp.Discordant)
	if v, ok := metrics.MinDetectableHitDifference(cmp.N, cmp.Discordant); ok {
		cmp.HitMDE = &v
	}
	if v, ok := metrics.MinDetectableMeanDifference(cmp.N, cmp.Coverage.SD); ok {
		cmp.CoverageMDE = &v
	}
	return cmp, true
}

func boolFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// JSON is the machine-readable form of the aggregates.
func (r *Report) JSON() ([]byte, error) {
	payload := struct {
		RunID            string            `json:"run_id"`
		Environment      string            `json:"environment"`
		Split            string            `json:"split"`
		Threshold        float64           `json:"hit_threshold"`
		Seed             int64             `json:"bootstrap_seed"`
		Resamples        int               `json:"bootstrap_resamples"`
		Incomplete       bool              `json:"incomplete"`
		Answerable       int               `json:"answerable"`
		Unanswer         int               `json:"unanswerable"`
		Cells            []CellStats       `json:"cells"`
		ByType           []TypeStats       `json:"by_type"`
		ByGroup          []GroupStats      `json:"by_group,omitempty"`
		Comparisons      []Comparison      `json:"comparisons"`
		GroupComparisons []GroupComparison `json:"group_comparisons,omitempty"`
	}{
		RunID: r.Input.Manifest.RunID, Environment: r.Input.Manifest.Environment, Split: r.Input.Manifest.Split,
		Threshold: r.Input.Threshold, Seed: r.Input.Seed, Resamples: r.Input.Resamples,
		Incomplete: r.Incomplete, Answerable: r.Answerable, Unanswer: r.Unanswerable,
		Cells: r.Cells, ByType: r.ByType, ByGroup: r.ByGroup, Comparisons: r.Comparisons, GroupComparisons: r.GroupComparisons,
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// Score reads a run directory and the inputs it was run on, scores it, writes
// report.md and scores.json into a directory named for the scoring settings, and
// returns the report and that directory. Scoring the same run with the same
// settings rewrites the same bytes.
func Score(runDir string, cfg *config.Config, corpus *dataset.Corpus, ds *dataset.Dataset, groupsPath string) (*Report, string, error) {
	var manifest runner.Manifest
	raw, err := os.ReadFile(filepath.Join(runDir, runner.ManifestFile))
	if err != nil {
		return nil, "", fmt.Errorf("read the run manifest: %w", err)
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return nil, "", fmt.Errorf("parse the run manifest: %w", err)
	}
	if manifest.Dataset.Version != ds.Version {
		return nil, "", fmt.Errorf("the question file changed since this run (run used version %s, file is %s); scoring it would mix two question sets",
			short(manifest.Dataset.Version), short(ds.Version))
	}
	if manifest.Corpus.Hash != corpus.Hash() {
		return nil, "", fmt.Errorf("the corpus changed since this run (run used %s, manifest says %s)", short(manifest.Corpus.Hash), short(corpus.Hash()))
	}
	file, err := os.Open(filepath.Join(runDir, runner.RecordsFile))
	if err != nil {
		return nil, "", fmt.Errorf("read the run records: %w", err)
	}
	defer file.Close()
	records, err := runner.ReadRecords(file)
	if err != nil {
		return nil, "", err
	}
	var summary *runner.Summary
	if raw, err := os.ReadFile(filepath.Join(runDir, runner.SummaryFile)); err == nil {
		var s runner.Summary
		if json.Unmarshal(raw, &s) == nil {
			summary = &s
		}
	}
	questions := ds.Filter(manifest.Split)
	var groups map[string]map[string]string
	groupsKey := ""
	if groupsPath != "" {
		wanted := make(map[string]bool, len(questions))
		for _, q := range questions {
			wanted[q.ID] = true
		}
		if groups, err = LoadGroups(groupsPath, wanted); err != nil {
			return nil, "", err
		}
		encoded, _ := json.Marshal(groups) // a map of strings: Marshal cannot fail, and orders the keys
		groupKey := sha256.Sum256(encoded)
		groupsKey = hex.EncodeToString(groupKey[:4])
	}
	rep, err := Build(Input{
		Manifest: manifest, Summary: summary, Records: records, Questions: questions, Groups: groups,
		Threshold: cfg.Metrics.HitThreshold, Seed: cfg.Metrics.BootstrapSeed, Resamples: cfg.Metrics.BootstrapResamples,
	})
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%v|%d|%d|%s", ds.Version, cfg.Metrics.HitThreshold, cfg.Metrics.BootstrapSeed, cfg.Metrics.BootstrapResamples, groupsKey)))
	outDir := filepath.Join(runDir, "scores-"+hex.EncodeToString(sum[:4]))
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		return nil, "", err
	}
	data, err := rep.JSON()
	if err != nil {
		return nil, "", err
	}
	if err := os.WriteFile(filepath.Join(outDir, "scores.json"), data, 0o600); err != nil {
		return nil, "", err
	}
	if err := os.WriteFile(filepath.Join(outDir, "report.md"), []byte(rep.Markdown()), 0o600); err != nil {
		return nil, "", err
	}
	return rep, outDir, nil
}

func short(hash string) string {
	if len(hash) > 12 {
		return hash[:12]
	}
	return hash
}
