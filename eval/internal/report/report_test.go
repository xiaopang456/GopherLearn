package report

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"GopherAI/common/rag"

	"github.com/xiaopang456/GopherLearn/eval/internal/config"
	"github.com/xiaopang456/GopherLearn/eval/internal/dataset"
	"github.com/xiaopang456/GopherLearn/eval/internal/fake"
	"github.com/xiaopang456/GopherLearn/eval/internal/pathguard"
	"github.com/xiaopang456/GopherLearn/eval/internal/runner"
)

func span(file string, start, end int) dataset.Span {
	return dataset.Span{File: file, Start: start, End: end}
}

func question(id, file string, start, end int) dataset.Question {
	return dataset.Question{ID: id, Question: id, Answerable: true, Split: dataset.SplitDev,
		Evidence: []dataset.EvidenceSet{{span(file, start, end)}}}
}

func hit(file string, start, end int, score float64) runner.Hit {
	return runner.Hit{File: file, Start: start, End: end, Score: score}
}

func rec(cell, q string, results, pool []runner.Hit, kind string) runner.Record {
	method, rule, k := "hybrid", "", 3
	switch {
	case strings.HasPrefix(cell, "vector"):
		method = "vector"
	case strings.HasPrefix(cell, "hybrid+rerank/production"):
		method, rule = "hybrid+rerank", "production"
	}
	if strings.HasSuffix(cell, "k=12") {
		k = 12
	}
	return runner.Record{QuestionID: q, Cell: cell, Method: method, Rule: rule, TopK: k, Results: results, Pool: pool, ErrorKind: kind}
}

func near(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("%s = %v, want %v", name, got, want)
	}
}

// handInput is a small run worked out by hand; the expected numbers in the tests
// below are computed from the data in the comments.
func handInput(environment string) Input {
	questions := []dataset.Question{
		question("q1", "a.md", 1, 10), question("q2", "a.md", 20, 29),
		question("q3", "b.md", 1, 4), question("q4", "b.md", 10, 13),
		{ID: "u1", Question: "u1", Answerable: false, Split: dataset.SplitDev},
	}
	hybrid := []runner.Record{
		rec("hybrid/k=3", "q1", []runner.Hit{hit("a.md", 1, 6, 0.9)}, []runner.Hit{hit("a.md", 1, 10, 0.9)}, ""),
		rec("hybrid/k=3", "q2", []runner.Hit{hit("a.md", 1, 5, 0.8)}, []runner.Hit{hit("a.md", 1, 5, 0.8), hit("a.md", 20, 29, 0.1)}, ""),
		rec("hybrid/k=3", "q3", []runner.Hit{hit("b.md", 1, 2, 0.7)}, []runner.Hit{hit("b.md", 1, 4, 0.7)}, ""),
		rec("hybrid/k=3", "q4", []runner.Hit{hit("b.md", 1, 3, 0.6), hit("a.md", 1, 2, 0.5)}, nil, ""),
		rec("hybrid/k=3", "u1", []runner.Hit{hit("a.md", 1, 1, 0.2)}, nil, ""),
	}
	vector := []runner.Record{
		rec("vector/k=3", "q1", []runner.Hit{hit("a.md", 1, 4, 0.5)}, []runner.Hit{hit("a.md", 1, 4, 0.5)}, ""),
		rec("vector/k=3", "q2", []runner.Hit{hit("a.md", 20, 24, 0.4)}, []runner.Hit{hit("a.md", 20, 24, 0.4)}, ""),
		rec("vector/k=3", "q3", []runner.Hit{hit("b.md", 1, 4, 0.3)}, []runner.Hit{hit("b.md", 1, 4, 0.3)}, ""),
		rec("vector/k=3", "q4", nil, nil, "unavailable:embedding_unavailable"),
		rec("vector/k=3", "u1", []runner.Hit{hit("a.md", 1, 1, 0.1)}, nil, ""),
	}
	// The same results again under other cell names.
	copyAs := func(cell, status string) []runner.Record {
		var out []runner.Record
		for _, r := range hybrid {
			c := rec(cell, r.QuestionID, r.Results, r.Pool, "")
			c.RerankStatus = status
			out = append(out, c)
		}
		return out
	}
	var records []runner.Record
	records = append(records, hybrid...)
	records = append(records, vector...)
	records = append(records, copyAs("hybrid+rerank/production/k=3", "applied")...)
	records = append(records, copyAs("hybrid/k=12", "bypassed")...)
	records = append(records, copyAs("hybrid+rerank/production/k=12", "bypassed")...)

	var manifest runner.Manifest
	manifest.Environment, manifest.Split, manifest.RunID = environment, dataset.SplitDev, "run-1"
	manifest.Cells = []string{"hybrid/k=3", "vector/k=3", "hybrid+rerank/production/k=3", "hybrid/k=12", "hybrid+rerank/production/k=12"}
	manifest.Rerank.Attached, manifest.Rerank.Model = true, "fake/x"
	return Input{Manifest: manifest, Records: records, Questions: questions, Threshold: 0.5, Seed: 11, Resamples: 500}
}

func cellByID(r *Report, id string) CellStats {
	for _, c := range r.Cells {
		if c.Cell == id {
			return c
		}
	}
	return CellStats{}
}

// Hybrid, by hand (see the comments in handInput):
//
//	q1 covers 6 of 10 lines -> 0.6, hit, RR 1, purity 6/6
//	q2 covers 0 of 10       -> 0,   miss, RR 0; pool holds a.md 20-29 -> misranked
//	q3 covers 2 of 4        -> 0.5, hit (inclusive), RR 1, purity 2/2
//	q4 covers 0 of 4        -> 0,   miss; empty pool -> not recalled
//
// hit rate 2/4, mean coverage (0.6+0+0.5+0)/4 = 0.275, MRR (1+0+1+0)/4 = 0.5,
// mean purity (1+0+1+0)/4 = 0.5, pool hits q1, q2, q3 = 3.
func TestHybridCellByHand(t *testing.T) {
	rep, err := Build(handInput(runner.EnvReal))
	if err != nil {
		t.Fatal(err)
	}
	c := cellByID(rep, "hybrid/k=3")
	if c.Answerable != 4 || c.Hits != 2 || c.PoolHits != 3 || c.Normal != 2 || c.Misranked != 1 || c.NotRecalled != 1 || c.Errors != 0 || c.Missing != 0 {
		t.Fatalf("counts = %+v", c)
	}
	near(t, "hit rate", c.HitRate, 0.5)
	near(t, "mean coverage", c.MeanCoverage, 0.275)
	near(t, "MRR", c.MRR, 0.5)
	near(t, "mean purity", c.MeanPurity, 0.5)
	if rep.Answerable != 4 || rep.Unanswerable != 1 || rep.Incomplete {
		t.Errorf("answerable %d unanswerable %d incomplete %v", rep.Answerable, rep.Unanswerable, rep.Incomplete)
	}
}

// Vector, by hand:
//
//	q1 covers 4 of 10 -> 0.4, no hit, but RR 1 (it shares lines); pool also 0.4 -> not recalled
//	q2 covers 5 of 10 -> 0.5, hit; q3 covers 4 of 4 -> 1, hit
//	q4 failed         -> 0, class error
//
// hit rate 2/4, mean coverage (0.4+0.5+1+0)/4 = 0.475, MRR (1+1+1+0)/4 = 0.75.
func TestVectorCellByHandKeepsTheFailure(t *testing.T) {
	rep, _ := Build(handInput(runner.EnvReal))
	c := cellByID(rep, "vector/k=3")
	if c.Answerable != 4 || c.Hits != 2 || c.Errors != 1 || c.NotRecalled != 1 || c.Normal != 2 || c.Misranked != 0 {
		t.Fatalf("counts = %+v", c)
	}
	near(t, "mean coverage", c.MeanCoverage, 0.475)
	near(t, "MRR", c.MRR, 0.75)
	// MRR counts a result that shares one line even when the question is not a hit.
	if c.MRR <= c.HitRate {
		t.Errorf("MRR %v must exceed hit rate %v here: q1 earned a reciprocal rank without being a hit", c.MRR, c.HitRate)
	}
}

// vector - hybrid, paired over the four answerable questions:
//
//	hit:      q1 0-1, q2 1-0, q3 1-1, q4 0-0  -> A-only 1 (q2), B-only 1 (q1), mean 0, McNemar p = 1
//	coverage: -0.2, +0.5, +0.5, 0            -> mean 0.2
//	MRR:      0, +1, 0, 0                    -> mean 0.25
func TestPairedComparisonByHand(t *testing.T) {
	rep, _ := Build(handInput(runner.EnvReal))
	var cmp *Comparison
	for i := range rep.Comparisons {
		if rep.Comparisons[i].Challenger == "vector/k=3" {
			cmp = &rep.Comparisons[i]
		}
	}
	if cmp == nil {
		t.Fatalf("no vector comparison among %d", len(rep.Comparisons))
	}
	if cmp.Baseline != "hybrid/k=3" || cmp.N != 4 || cmp.Clusters != 2 {
		t.Fatalf("%+v", cmp)
	}
	if cmp.Discordant.AOnly != 1 || cmp.Discordant.BOnly != 1 {
		t.Errorf("discordant = %+v", cmp.Discordant)
	}
	near(t, "McNemar p", cmp.McNemarP, 1)
	near(t, "hit difference", cmp.Hit.Mean, 0)
	near(t, "coverage difference", cmp.Coverage.Mean, 0.2)
	near(t, "MRR difference", cmp.MRR.Mean, 0.25)
	if !cmp.Hit.CI.OK || cmp.Hit.CI.Low > cmp.Hit.Mean || cmp.Hit.CI.High < cmp.Hit.Mean {
		t.Errorf("hit CI %+v", cmp.Hit.CI)
	}
}

func TestComparisonPairsSkipBypassedCellsAndNeedABaseline(t *testing.T) {
	rep, _ := Build(handInput(runner.EnvReal))
	got := map[string]bool{}
	for _, c := range rep.Comparisons {
		got[c.Challenger+" vs "+c.Baseline] = true
	}
	for _, want := range []string{"vector/k=3 vs hybrid/k=3", "hybrid+rerank/production/k=3 vs hybrid/k=3"} {
		if !got[want] {
			t.Errorf("missing comparison %q (have %v)", want, got)
		}
	}
	if got["hybrid+rerank/production/k=12 vs hybrid/k=12"] {
		t.Error("a bypassed production cell is hybrid by construction and must not be compared")
	}
	if len(got) != 2 {
		t.Errorf("comparisons = %v", got)
	}
	// Identical results: no difference, no discordance, and the MDE is undefined.
	for _, c := range rep.Comparisons {
		if c.Challenger == "hybrid+rerank/production/k=3" && (c.Hit.Mean != 0 || c.Discordant.AOnly+c.Discordant.BOnly != 0 || c.HitMDE != nil || c.McNemarP != 1) {
			t.Errorf("identical cells: %+v", c)
		}
	}
}

func TestUnanswerableScoresAreOnlyDescribed(t *testing.T) {
	rep, _ := Build(handInput(runner.EnvReal))
	h := cellByID(rep, "hybrid/k=3")
	if h.Unanswerable != 1 || h.UnanswerableEmpty != 0 || len(h.UnanswerableTopOne) != 3 {
		t.Fatalf("%+v", h)
	}
	near(t, "unanswerable top-1", h.UnanswerableTopOne[1], 0.2)
	if h.AnswerableTopOneMed == nil {
		t.Fatal("no answerable median")
	}
	// answerable top-1 scores for hybrid: 0.9, 0.8, 0.7, 0.6 -> median 0.75
	near(t, "answerable median", *h.AnswerableTopOneMed, 0.75)
}

// ---- the labels the reader depends on --------------------------------------

func TestMarkdownStatesTheTwoCriteriaSeparately(t *testing.T) {
	rep, _ := Build(handInput(runner.EnvReal))
	md := rep.Markdown()
	for _, want := range []string{
		"命中@K（覆盖率 ≥ 0.5）",
		"MRR（首个至少共有 1 行的结果）",
		"**命中@K 的口径**",
		"**MRR 的口径**",
		"**至少共有 1 行**",
		"一个结果只要与证据共有 1 行就能让该题的 MRR 大于 0，却不一定使该题命中",
		"## 表 1　汇总",
		"## 表 2　候选召回与失败分类",
		"**排得不好**",
		"**召回不到**",
		"## 表 3　配对比较",
		"cluster bootstrap",
		"McNemar",
		"最小可检测效应",
		"## 表 4　不可答题",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("report lacks %q", want)
		}
	}
	// Both criteria also appear in the paired table's header.
	line := lineContaining(md, "| 挑战者 |")
	if !strings.Contains(line, "命中率差（覆盖率 ≥ 0.5）") || !strings.Contains(line, "MRR 差（至少共有 1 行）") {
		t.Errorf("the paired table header does not name both criteria: %s", line)
	}
	if strings.Contains(md, "离线替身环境") {
		t.Error("a real run must not carry the offline banner")
	}
}

func TestMarkdownFollowsTheThreshold(t *testing.T) {
	in := handInput(runner.EnvReal)
	in.Threshold = 0.6
	rep, _ := Build(in)
	md := rep.Markdown()
	if !strings.Contains(md, "命中@K（覆盖率 ≥ 0.6）") || strings.Contains(md, "覆盖率 ≥ 0.5") {
		t.Error("the header must show the configured threshold, not a hard-coded one")
	}
	// At 0.6, q3 (coverage exactly 0.5) is no longer a hit; q1 (0.6) still is.
	near(t, "hybrid hit rate at 0.6", cellByID(rep, "hybrid/k=3").HitRate, 0.25)
	in.Threshold = 1
	rep, _ = Build(in)
	if !strings.Contains(rep.Markdown(), "覆盖率 ≥ 1）") {
		t.Error("a threshold of 1 must render as 1")
	}
}

func TestOfflineBannerAndIncompleteNotice(t *testing.T) {
	rep, _ := Build(handInput(runner.EnvFake))
	if md := rep.Markdown(); !strings.Contains(md, "离线替身环境（fake）") || !strings.Contains(md, "不得引用") {
		t.Error("an offline run must say its numbers are not quality results")
	}

	in := handInput(runner.EnvReal)
	in.Records = in.Records[1:] // drop hybrid/k=3 q1
	rep, _ = Build(in)
	c := cellByID(rep, "hybrid/k=3")
	if !rep.Incomplete || c.Missing != 1 || c.Answerable != 3 {
		t.Fatalf("incomplete = %v, cell %+v", rep.Incomplete, c)
	}
	if md := rep.Markdown(); !strings.Contains(md, "运行不完整") {
		t.Error("an incomplete run must be flagged in the report")
	}
}

func TestBuildRejectsInconsistentRecords(t *testing.T) {
	in := handInput(runner.EnvReal)
	in.Records = append(in.Records, rec("hybrid/k=3", "ghost", nil, nil, ""))
	if _, err := Build(in); err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Errorf("an unknown question id: %v", err)
	}
	in = handInput(runner.EnvReal)
	in.Records = append(in.Records, in.Records[0])
	if _, err := Build(in); err == nil || !strings.Contains(err.Error(), "two records") {
		t.Errorf("a duplicate record: %v", err)
	}
}

func TestJSONCarriesTheSameAggregates(t *testing.T) {
	rep, _ := Build(handInput(runner.EnvReal))
	data, err := rep.JSON()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"hit_threshold": 0.5`, `"bootstrap_seed": 11`, `"cell": "vector/k=3"`, `"mcnemar_p"`, `"misranked"`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("scores.json lacks %s", want)
		}
	}
}

func lineContaining(text, part string) string {
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, part) {
			return line
		}
	}
	return ""
}

// ---- scoring a real run directory ------------------------------------------

func runFake(t *testing.T) (*config.Config, *dataset.Corpus, *dataset.Dataset, string) {
	t.Helper()
	root := t.TempDir()
	if err := pathguard.Check("temp directory", root); err != nil {
		t.Skipf("temp directory is inside a git working tree: %v", err)
	}
	corpus, err := dataset.LoadCorpus(filepath.Join("..", "..", "testdata", "corpus.json"))
	if err != nil {
		t.Fatal(err)
	}
	ds, err := dataset.LoadDataset(filepath.Join("..", "..", "testdata", "dataset.jsonl"), corpus)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Paths = config.Paths{CorpusManifest: filepath.Join(root, "corpus.json"), DatasetFile: filepath.Join(root, "dataset.jsonl"), ResultsDir: filepath.Join(root, "results")}
	cfg.Services = config.Services{ProductConfig: filepath.Join(root, "product.toml.local"), EvalUserID: 900001}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	embedder, backend := fake.NewEmbedder(64), fake.NewBackend(64)
	env := runner.Env{
		Kind: runner.EnvFake, Embedder: embedder, Repository: backend, Searcher: backend, Reranker: fake.NewReranker(),
		Dimension: 64, EmbeddingModel: "fake", RerankModel: "fake/overlap",
		Chunker: rag.ChunkerConfig{MaxTokens: 90, OverlapTokens: 8, MaxChunks: 2000}, MaxSourceBytes: 1 << 20,
	}
	result, err := runner.Execute(context.Background(), cfg, corpus, ds, env, runner.Options{Split: dataset.SplitDev})
	if err != nil {
		t.Fatal(err)
	}
	return cfg, corpus, ds, result.Dir
}

func TestScoreWritesDeterministicFilesAndLabelsTheRun(t *testing.T) {
	cfg, corpus, ds, runDir := runFake(t)
	rep, out, err := Score(runDir, cfg, corpus, ds)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Incomplete || len(rep.Cells) != 22 || len(rep.Comparisons) == 0 {
		t.Fatalf("cells %d comparisons %d incomplete %v", len(rep.Cells), len(rep.Comparisons), rep.Incomplete)
	}
	read := func(name string) string {
		data, err := os.ReadFile(filepath.Join(out, name))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	md, js := read("report.md"), read("scores.json")
	if !strings.Contains(md, "离线替身环境（fake）") || !strings.Contains(md, "命中@K（覆盖率 ≥ 0.5）") {
		t.Error("report.md lacks the offline banner or the hit criterion")
	}

	// Scoring is repeatable: the same run and settings rewrite the same bytes.
	if _, out2, err := Score(runDir, cfg, corpus, ds); err != nil || out2 != out || read("report.md") != md || read("scores.json") != js {
		t.Fatalf("scoring twice gave different output (err %v)", err)
	}
	// A different setting goes to a different directory and leaves the first alone.
	cfg.Metrics.HitThreshold = 0.8
	if _, out3, err := Score(runDir, cfg, corpus, ds); err != nil || out3 == out || read("report.md") != md {
		t.Fatalf("a new threshold must not overwrite the earlier report (err %v)", err)
	}
}

func TestScoreRefusesAChangedQuestionFileOrCorpus(t *testing.T) {
	cfg, corpus, ds, runDir := runFake(t)

	edited, err := dataset.ParseDataset([]byte(`{"id":"only","question":"q","answerable":false,"split":"dev"}`+"\n"), corpus)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Score(runDir, cfg, corpus, edited); err == nil || !strings.Contains(err.Error(), "question file changed") {
		t.Errorf("a changed question file: %v", err)
	}

	other := *corpus
	other.Documents = append([]dataset.Document(nil), corpus.Documents[:2]...)
	if _, _, err := Score(runDir, cfg, &other, ds); err == nil || !strings.Contains(err.Error(), "corpus changed") {
		t.Errorf("a changed corpus: %v", err)
	}

	if _, _, err := Score(filepath.Join(filepath.Dir(runDir), "no-such-run"), cfg, corpus, ds); err == nil {
		t.Error("a missing run directory must fail")
	}
}

func TestScoreFlagsAnIncompleteRun(t *testing.T) {
	cfg, corpus, ds, runDir := runFake(t)
	path := filepath.Join(runDir, runner.RecordsFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if err := os.WriteFile(path, []byte(strings.Join(lines[:len(lines)-10], "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rep, _, err := Score(runDir, cfg, corpus, ds)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Incomplete {
		t.Fatal("a run with missing records must be reported as incomplete")
	}
}
