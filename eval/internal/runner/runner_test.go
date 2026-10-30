package runner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/embedding"

	"GopherAI/common/rag"

	"github.com/xiaopang456/GopherLearn/eval/internal/config"
	"github.com/xiaopang456/GopherLearn/eval/internal/dataset"
	"github.com/xiaopang456/GopherLearn/eval/internal/fake"
	"github.com/xiaopang456/GopherLearn/eval/internal/pathguard"
	"github.com/xiaopang456/GopherLearn/eval/internal/safety"
)

const (
	testUser = int64(900001)
	testDim  = 64
)

var fixedClock = func() time.Time { return time.Date(2026, 10, 30, 8, 0, 0, 0, time.UTC) }

func loadFixture(t *testing.T) (*dataset.Corpus, *dataset.Dataset) {
	t.Helper()
	corpus, err := dataset.LoadCorpus(filepath.Join("..", "..", "testdata", "corpus.json"))
	if err != nil {
		t.Fatal(err)
	}
	ds, err := dataset.LoadDataset(filepath.Join("..", "..", "testdata", "dataset.jsonl"), corpus)
	if err != nil {
		t.Fatal(err)
	}
	return corpus, ds
}

// testConfig returns a validated configuration whose paths sit in a fresh temp
// directory. methods and topK default to the full matrix when nil.
func testConfig(t *testing.T, methods []string, topK []int, limits config.Limits) *config.Config {
	t.Helper()
	root := t.TempDir()
	if err := pathguard.Check("temp directory", root); err != nil {
		t.Skipf("temp directory is inside a git working tree: %v", err)
	}
	cfg := config.Default()
	cfg.Paths = config.Paths{
		CorpusManifest: filepath.Join(root, "corpus.json"), DatasetFile: filepath.Join(root, "dataset.jsonl"),
		ResultsDir: filepath.Join(root, "results"),
	}
	cfg.Services = config.Services{ProductConfig: filepath.Join(root, "product.toml.local"), EvalUserID: testUser}
	if methods != nil {
		cfg.Matrix.Methods = methods
	}
	if topK != nil {
		cfg.Matrix.TopK = topK
	}
	cfg.Limits = limits
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

type testEnv struct {
	Env
	embedder *fake.Embedder
	backend  *fake.Backend
	reranker *fake.Reranker
}

// newEnv returns an offline environment. Chunks are small so each fixture
// document yields several, and the retrieval pool is larger than a TopK.
func newEnv(t *testing.T) *testEnv {
	t.Helper()
	e := &testEnv{embedder: fake.NewEmbedder(testDim), backend: fake.NewBackend(testDim), reranker: fake.NewReranker()}
	e.Env = Env{
		Kind: EnvFake, Embedder: e.embedder, Repository: e.backend, Searcher: e.backend, Reranker: e.reranker,
		Dimension: testDim, EmbeddingModel: "synthetic-embedding", RerankModel: "fake/fake-rerank",
		Chunker: rag.ChunkerConfig{MaxTokens: 90, OverlapTokens: 8, MaxChunks: 2000}, MaxSourceBytes: 1 << 20,
	}
	return e
}

func execute(t *testing.T, cfg *config.Config, env Env, split string) (*Result, []Record) {
	t.Helper()
	corpus, ds := loadFixture(t)
	result, err := Execute(context.Background(), cfg, corpus, ds, env, Options{Split: split, Now: fixedClock})
	if err != nil {
		t.Fatal(err)
	}
	return result, readRecords(t, result.Dir)
}

func readRecords(t *testing.T, dir string) []Record {
	t.Helper()
	file, err := os.Open(filepath.Join(dir, RecordsFile))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	records, err := ReadRecords(file)
	if err != nil {
		t.Fatal(err)
	}
	return records
}

func byCell(records []Record, cell string) []Record {
	var out []Record
	for _, r := range records {
		if r.Cell == cell {
			out = append(out, r)
		}
	}
	return out
}

func spans(hits []Hit) [][3]any {
	out := make([][3]any, len(hits))
	for i, h := range hits {
		out[i] = [3]any{h.File, h.Start, h.End}
	}
	return out
}

// ---- the full matrix -------------------------------------------------------

func TestExecuteRunsEveryMethodAtEveryTopK(t *testing.T) {
	cfg := testConfig(t, nil, nil, config.Limits{})
	env := newEnv(t)
	result, records := execute(t, cfg, env.Env, dataset.SplitDev)

	const devQuestions, cells = 5, 22 // 3 plain methods x 5 + rerank 5 production + 2 forced
	if len(records) != devQuestions*cells || result.Records != len(records) || result.Errors != 0 || !result.Complete {
		t.Fatalf("%d records (result %+v), want %d, no errors, complete", len(records), result, devQuestions*cells)
	}
	seen := map[string]bool{}
	for _, r := range records {
		seen[r.Method] = true
		if len(r.Pool) > rag.RerankCandidateK {
			t.Errorf("%s %s: pool of %d exceeds %d", r.QuestionID, r.Cell, len(r.Pool), rag.RerankCandidateK)
		}
		if len(r.Results) > r.TopK {
			t.Errorf("%s %s: %d results for topK %d", r.QuestionID, r.Cell, len(r.Results), r.TopK)
		}
		if len(r.Results) == 0 && r.Method != config.MethodKeyword {
			t.Errorf("%s %s: no results", r.QuestionID, r.Cell)
		}
		for i, h := range r.Results {
			if h.Rank != i+1 || h.File == "" || h.Start < 1 || h.End < h.Start || h.Version != 1 {
				t.Errorf("%s %s: bad hit %+v", r.QuestionID, r.Cell, h)
			}
		}
	}
	for _, method := range []string{config.MethodVector, config.MethodKeyword, config.MethodHybrid, config.MethodHybridRerank} {
		if !seen[method] {
			t.Errorf("method %s never ran", method)
		}
	}
	// Sealed questions stay untouched unless asked for.
	for _, r := range records {
		if r.QuestionID == "q-graft-when" {
			t.Fatal("a sealed question ran in a dev run")
		}
	}
	// The unanswerable question is retrieved too: its score distribution is needed.
	if got := byCell(records, "hybrid/k=5"); len(got) != devQuestions || got[len(got)-1].QuestionID != "q-tungsten" {
		t.Errorf("hybrid/k=5 records: %d, last %v", len(got), got[len(got)-1].QuestionID)
	}
}

func TestExecuteSelectsTheSplit(t *testing.T) {
	cfg := testConfig(t, []string{config.MethodHybrid}, []int{5}, config.Limits{})
	_, records := execute(t, cfg, newEnv(t).Env, dataset.SplitSealed)
	if len(records) != 1 || records[0].QuestionID != "q-graft-when" {
		t.Fatalf("sealed records = %+v", records)
	}
	corpus, ds := loadFixture(t)
	if _, err := Execute(context.Background(), testConfig(t, nil, nil, config.Limits{}), corpus, ds, newEnv(t).Env, Options{Split: "nope", Now: fixedClock}); err == nil {
		t.Fatal("an unknown split must be refused")
	}
}

// ---- what each option does -------------------------------------------------

func TestVectorCellsNeverUseTheLexicalChannel(t *testing.T) {
	env := newEnv(t)
	_, records := execute(t, testConfig(t, []string{config.MethodVector}, []int{3, 12}, config.Limits{}), env.Env, dataset.SplitDev)
	for _, s := range env.backend.Searches() {
		if s.Kind != "vector" {
			t.Fatalf("a vector-only run issued a %s search", s.Kind)
		}
	}
	for _, r := range records {
		for _, h := range r.Results {
			if h.LexicalRank != 0 || h.MatchKind != "vector" {
				t.Fatalf("%s: hit %+v came from the lexical channel", r.Cell, h)
			}
		}
	}
}

func TestKeywordCellsNeverEmbedAQuery(t *testing.T) {
	env := newEnv(t)
	result, records := execute(t, testConfig(t, []string{config.MethodKeyword}, []int{3, 12}, config.Limits{}), env.Env, dataset.SplitDev)
	for _, s := range env.backend.Searches() {
		if s.Kind != "text" {
			t.Fatalf("a keyword-only run issued a %s search", s.Kind)
		}
	}
	plan := result.Estimate
	if env.embedder.Calls() != plan.IndexEmbeddingCalls || plan.QueryEmbeddingCalls != 0 || result.EmbeddingCalls != plan.IndexEmbeddingCalls {
		t.Errorf("embedder calls %d, want only the %d indexing calls", env.embedder.Calls(), plan.IndexEmbeddingCalls)
	}
	for _, r := range records {
		if r.EmbeddingCalls != 0 {
			t.Fatalf("%s %s caused %d embedding calls", r.QuestionID, r.Cell, r.EmbeddingCalls)
		}
		for _, h := range r.Results {
			if h.VectorRank != 0 || h.MatchKind != "lexical" {
				t.Fatalf("%s: hit %+v came from the vector channel", r.Cell, h)
			}
		}
	}
}

func TestRerankRulesAndForceOff(t *testing.T) {
	cfg := testConfig(t, []string{config.MethodHybrid, config.MethodHybridRerank}, []int{3, 8, 12, 20}, config.Limits{})
	env := newEnv(t)
	_, records := execute(t, cfg, env.Env, dataset.SplitDev)

	status := func(cell string) map[string]bool {
		out := map[string]bool{}
		for _, r := range byCell(records, cell) {
			out[r.RerankStatus] = true
		}
		return out
	}
	// A reranker is attached for the whole run, yet the plain hybrid cells must
	// not use it.
	for _, cell := range []string{"hybrid/k=3", "hybrid/k=8", "hybrid/k=12", "hybrid/k=20"} {
		if s := status(cell); !s["bypassed"] || len(s) != 1 {
			t.Errorf("%s: rerank statuses %v, want only bypassed", cell, s)
		}
	}
	// The production rule reranks up to k=8 and bypasses above.
	for cell, want := range map[string]string{
		"hybrid+rerank/production/k=3": "applied", "hybrid+rerank/production/k=8": "applied",
		"hybrid+rerank/production/k=12": "bypassed", "hybrid+rerank/production/k=20": "bypassed",
		"hybrid+rerank/forced/k=12": "applied", "hybrid+rerank/forced/k=20": "applied",
	} {
		if s := status(cell); !s[want] || len(s) != 1 {
			t.Errorf("%s: rerank statuses %v, want only %s", cell, s, want)
		}
	}
	// Candidate depth follows the product formula.
	for cell, want := range map[string]int{
		"hybrid/k=3": 30, "hybrid/k=12": 72, "hybrid/k=20": 100,
		"hybrid+rerank/production/k=3": 48, "hybrid+rerank/production/k=12": 72, "hybrid+rerank/forced/k=12": 48,
	} {
		for _, r := range byCell(records, cell) {
			if r.EffectiveDepth != want {
				t.Errorf("%s: effective depth %d, want %d", cell, r.EffectiveDepth, want)
			}
		}
	}
	// A bypassed production cell returns what plain hybrid returns.
	for i, bypassed := range byCell(records, "hybrid+rerank/production/k=12") {
		plain := byCell(records, "hybrid/k=12")[i]
		if !reflect.DeepEqual(spans(bypassed.Results), spans(plain.Results)) {
			t.Errorf("%s: bypassed production results differ from hybrid", bypassed.QuestionID)
		}
	}
	// Forced reranking at k=12 is a prefix of forced reranking at k=20: both
	// rerank the same pool, which is what lets one rerank call serve both cells.
	for i, small := range byCell(records, "hybrid+rerank/forced/k=12") {
		large := byCell(records, "hybrid+rerank/forced/k=20")[i]
		if len(large.Results) < len(small.Results) || !reflect.DeepEqual(spans(small.Results), spans(large.Results[:len(small.Results)])) {
			t.Errorf("%s: forced k=12 is not a prefix of forced k=20", small.QuestionID)
		}
		if len(small.Pool) > rag.RerankCandidateK || !reflect.DeepEqual(spans(small.Pool), spans(large.Pool)) {
			t.Errorf("%s: forced cells saw different pools", small.QuestionID)
		}
	}
	// The pool is the fused, pre-rerank list: the same for a reranked cell and a
	// plain one of the same depth.
	if !reflect.DeepEqual(spans(byCell(records, "hybrid+rerank/production/k=12")[0].Pool), spans(byCell(records, "hybrid/k=12")[0].Pool)) {
		t.Error("the pool of a bypassed cell differs from the plain hybrid pool")
	}
}

// ---- paid-call accounting --------------------------------------------------

func TestOneQueryEmbeddingAndOneRerankPerQuestion(t *testing.T) {
	env := newEnv(t)
	result, records := execute(t, testConfig(t, nil, nil, config.Limits{}), env.Env, dataset.SplitDev)
	const questions = 5

	var embedSum, rerankSum int
	for _, r := range records {
		embedSum += r.EmbeddingCalls
		rerankSum += r.RerankCalls
		if r.EmbeddingCalls > 1 || r.RerankCalls > 1 {
			t.Errorf("%s %s made %d embedding and %d rerank calls", r.QuestionID, r.Cell, r.EmbeddingCalls, r.RerankCalls)
		}
	}
	if embedSum != questions || rerankSum != questions {
		t.Fatalf("records account for %d query embeddings and %d reranks, want one each per question (%d)", embedSum, rerankSum, questions)
	}
	if env.reranker.Calls() != questions || env.reranker.HealthCalls() != 1 {
		t.Errorf("reranker saw %d rerank and %d health calls, want %d and 1", env.reranker.Calls(), env.reranker.HealthCalls(), questions)
	}
	if env.embedder.Calls() != result.Estimate.IndexEmbeddingCalls+questions {
		t.Errorf("embedder calls %d, want %d indexing + %d queries", env.embedder.Calls(), result.Estimate.IndexEmbeddingCalls, questions)
	}
}

func TestPlanEqualsWhatTheRunSpends(t *testing.T) {
	cfg := testConfig(t, nil, nil, config.Limits{})
	env := newEnv(t)
	corpus, ds := loadFixture(t)
	plan, err := MakePlan(context.Background(), PlanInput{
		Config: cfg, Corpus: corpus, Questions: ds.Filter(dataset.SplitDev), Chunker: env.Chunker, MaxSourceBytes: env.MaxSourceBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, _ := execute(t, cfg, env.Env, dataset.SplitDev)
	if result.EmbeddingCalls != plan.EmbeddingCalls || result.RerankCalls != plan.RerankCalls {
		t.Errorf("plan: %d embedding, %d rerank calls; the run spent %d and %d", plan.EmbeddingCalls, plan.RerankCalls, result.EmbeddingCalls, result.RerankCalls)
	}
	if plan.Questions != 5 || plan.Cells != 22 || plan.Retrievals != 110 || plan.DistinctQueries != 5 {
		t.Errorf("plan shape = %+v", plan)
	}
	if env.embedder.Calls() != plan.EmbeddingCalls || env.reranker.Calls()+env.reranker.HealthCalls() != plan.RerankCalls {
		t.Errorf("the fakes saw %d embedding and %d rerank calls", env.embedder.Calls(), env.reranker.Calls()+env.reranker.HealthCalls())
	}
}

func TestPlanCountsDistinctQuestionTextsAndHonoursAnExistingIndex(t *testing.T) {
	cfg := testConfig(t, []string{config.MethodHybrid}, []int{5}, config.Limits{})
	corpus, _ := loadFixture(t)
	questions := []dataset.Question{{ID: "a", Question: "same text"}, {ID: "b", Question: "same text"}, {ID: "c", Question: "other"}}
	chunker := rag.ChunkerConfig{MaxTokens: 90, OverlapTokens: 8, MaxChunks: 2000}
	fresh, err := MakePlan(context.Background(), PlanInput{Config: cfg, Corpus: corpus, Questions: questions, Chunker: chunker, MaxSourceBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	reused, _ := MakePlan(context.Background(), PlanInput{Config: cfg, Corpus: corpus, Questions: questions, Chunker: chunker, MaxSourceBytes: 1 << 20, IndexExists: true})
	if fresh.Questions != 3 || fresh.DistinctQueries != 2 || fresh.QueryEmbeddingCalls != 2 {
		t.Errorf("duplicates must share one embedding: %+v", fresh)
	}
	if fresh.IndexEmbeddingCalls == 0 || reused.IndexEmbeddingCalls != 0 || reused.EmbeddingCalls != 2 {
		t.Errorf("fresh index %d calls, reused %d, reused total %d", fresh.IndexEmbeddingCalls, reused.IndexEmbeddingCalls, reused.EmbeddingCalls)
	}
	if fresh.RerankCalls != 0 {
		t.Errorf("a hybrid-only matrix needs no rerank calls, plan says %d", fresh.RerankCalls)
	}
}

func TestLimitsFailClosed(t *testing.T) {
	corpus, ds := loadFixture(t)
	probe := newEnv(t)
	probeCfg := testConfig(t, nil, nil, config.Limits{})
	need, err := MakePlan(context.Background(), PlanInput{Config: probeCfg, Corpus: corpus, Questions: ds.Filter(dataset.SplitDev), Chunker: probe.Chunker, MaxSourceBytes: probe.MaxSourceBytes})
	if err != nil {
		t.Fatal(err)
	}

	for name, limits := range map[string]config.Limits{
		"no limits at all (the default)":  {},
		"embedding limit one too low":     {MaxEmbeddingCalls: need.EmbeddingCalls - 1, MaxRerankCalls: need.RerankCalls},
		"rerank limit one too low":        {MaxEmbeddingCalls: need.EmbeddingCalls, MaxRerankCalls: need.RerankCalls - 1},
		"only the embedding limit is set": {MaxEmbeddingCalls: 1000},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testConfig(t, nil, nil, limits)
			env := newEnv(t)
			env.Paid = true
			_, err := Execute(context.Background(), cfg, corpus, ds, env.Env, Options{Split: dataset.SplitDev, Now: fixedClock})
			if !errors.Is(err, ErrOverLimit) {
				t.Fatalf("err = %v, want ErrOverLimit", err)
			}
			if env.embedder.Calls() != 0 || env.reranker.Calls() != 0 || env.reranker.HealthCalls() != 0 || env.backend.Len() != 0 {
				t.Fatal("a refused run made calls or wrote an index")
			}
			if _, statErr := os.Stat(cfg.Paths.ResultsDir); !os.IsNotExist(statErr) {
				t.Fatal("a refused run left a results directory behind")
			}
		})
	}

	t.Run("limits equal to the plan are enough, and are spent exactly", func(t *testing.T) {
		cfg := testConfig(t, nil, nil, config.Limits{MaxEmbeddingCalls: need.EmbeddingCalls, MaxRerankCalls: need.RerankCalls})
		env := newEnv(t)
		env.Paid = true
		result, err := Execute(context.Background(), cfg, corpus, ds, env.Env, Options{Split: dataset.SplitDev, Now: fixedClock})
		if err != nil || !result.Complete {
			t.Fatalf("err %v, result %+v", err, result)
		}
		if result.EmbeddingCalls != need.EmbeddingCalls || result.RerankCalls != need.RerankCalls {
			t.Errorf("spent %d and %d, plan %d and %d", result.EmbeddingCalls, result.RerankCalls, need.EmbeddingCalls, need.RerankCalls)
		}
	})

	t.Run("an offline run needs no limits", func(t *testing.T) {
		cfg := testConfig(t, nil, nil, config.Limits{})
		if _, err := Execute(context.Background(), cfg, corpus, ds, newEnv(t).Env, Options{Split: dataset.SplitDev, Now: fixedClock}); err != nil {
			t.Fatal(err)
		}
	})
}

func TestBudgetRefusesTheCallPastTheLimit(t *testing.T) {
	budget := NewBudget(true, 2, 1)
	embedder := fake.NewEmbedder(8)
	counting := countingEmbedder{inner: embedder, budget: budget}
	for i := 0; i < 2; i++ {
		if _, err := counting.EmbedStrings(context.Background(), []string{"x"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := counting.EmbedStrings(context.Background(), []string{"x"}); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("err = %v, want ErrBudgetExhausted", err)
	}
	if embedder.Calls() != 2 {
		t.Fatalf("the refused call reached the provider: %d calls", embedder.Calls())
	}

	reranker := newCachingReranker(fake.NewReranker(), budget)
	if _, err := reranker.Rerank(context.Background(), "q", []string{"a", "b"}); err != nil {
		t.Fatal(err)
	}
	if _, err := reranker.Rerank(context.Background(), "q", []string{"a", "b"}); err != nil {
		t.Fatal("a cache hit must not spend the budget: ", err)
	}
	if _, err := reranker.Rerank(context.Background(), "another", []string{"a", "b"}); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("err = %v", err)
	}
	if err := reranker.Health(context.Background()); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("the startup check is a call and is charged to the budget: %v", err)
	}
	if e, r := budget.Used(); e != 2 || r != 1 {
		t.Fatalf("used %d and %d", e, r)
	}

	unlimited := NewBudget(false, 0, 0)
	for i := 0; i < 3; i++ {
		if err := unlimited.spendEmbedding(); err != nil {
			t.Fatal("an offline budget must only count: ", err)
		}
	}
}

func TestCachesHandOutCopiesAndDoNotRememberCancellation(t *testing.T) {
	budget := NewBudget(false, 0, 0)
	cache := newCachingReranker(fake.NewReranker(), budget)
	first, _ := cache.Rerank(context.Background(), "q", []string{"alpha", "beta"})
	first[0].Score = 999 // the retrieval service sorts and mutates what it receives
	second, _ := cache.Rerank(context.Background(), "q", []string{"alpha", "beta"})
	if second[0].Score == 999 {
		t.Fatal("the cache handed out its own slice")
	}

	embedder := newCachingEmbedder(fake.NewEmbedder(8), budget)
	v1, _ := embedder.EmbedStrings(context.Background(), []string{"hello"})
	v1[0][0] = 999
	v2, _ := embedder.EmbedStrings(context.Background(), []string{"hello"})
	if v2[0][0] == 999 {
		t.Fatal("the embedding cache handed out its own slice")
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := embedder.EmbedStrings(cancelled, []string{"fresh"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if _, err := embedder.EmbedStrings(context.Background(), []string{"fresh"}); err != nil {
		t.Fatalf("a cancelled call was remembered as a failure: %v", err)
	}
}

// ---- degradation and failure retention -------------------------------------

func TestRerankerDegradationIsRecordedAndDoesNotStopTheRun(t *testing.T) {
	cfg := testConfig(t, []string{config.MethodHybrid, config.MethodHybridRerank}, []int{3, 5}, config.Limits{})
	env := newEnv(t)
	env.reranker.FailRerank(fake.Unavailable())
	result, records := execute(t, cfg, env.Env, dataset.SplitDev)
	if !result.Complete || result.Errors != 0 {
		t.Fatalf("a degraded reranker must not fail the run: %+v", result)
	}
	for _, cell := range []string{"hybrid+rerank/production/k=3", "hybrid+rerank/production/k=5"} {
		plain := byCell(records, strings.TrimPrefix(strings.Replace(cell, "+rerank/production", "", 1), ""))
		for i, r := range byCell(records, cell) {
			if r.RerankStatus != "fallback" {
				t.Errorf("%s %s: status %q, want fallback", r.QuestionID, cell, r.RerankStatus)
			}
			if !reflect.DeepEqual(spans(r.Results), spans(plain[i].Results)) {
				t.Errorf("%s %s: a fallback must keep the fused order", r.QuestionID, cell)
			}
		}
	}
	if env.reranker.Calls() != 5 {
		t.Errorf("%d rerank calls; a failed question must be remembered, not retried per cell", env.reranker.Calls())
	}
}

func TestRerankerFailureIsKeptAsARecordAndNothingIsRetried(t *testing.T) {
	cfg := testConfig(t, []string{config.MethodHybrid, config.MethodHybridRerank}, []int{3, 5}, config.Limits{})
	env := newEnv(t)
	env.reranker.FailRerank(errors.New("bad gateway"))
	result, records := execute(t, cfg, env.Env, dataset.SplitDev)

	if !result.Complete || result.Errors != 10 { // 5 questions x 2 rerank cells
		t.Fatalf("errors = %d, complete = %v; want the 10 rerank records to carry the failure and the run to finish", result.Errors, result.Complete)
	}
	for _, r := range records {
		failed := r.Method == config.MethodHybridRerank
		if failed != (r.ErrorKind != "") {
			t.Errorf("%s %s: error kind %q", r.QuestionID, r.Cell, r.ErrorKind)
		}
		if failed && (r.ErrorKind != "retrieval_failed" || len(r.Results) != 0) {
			t.Errorf("%s %s: kind %q, %d results", r.QuestionID, r.Cell, r.ErrorKind, len(r.Results))
		}
		if !failed && len(r.Results) == 0 {
			t.Errorf("%s %s lost its results to another cell's failure", r.QuestionID, r.Cell)
		}
	}
	if env.reranker.Calls() != 5 {
		t.Errorf("%d rerank calls for 5 questions: failures were retried", env.reranker.Calls())
	}
}

// failAfter lets the first n embedding calls through, then fails.
type failAfter struct {
	inner embedding.Embedder
	n     int
	calls int
}

func (f *failAfter) EmbedStrings(ctx context.Context, texts []string, opts ...embedding.Option) ([][]float64, error) {
	f.calls++
	if f.calls > f.n {
		return nil, errors.New("embedding provider unavailable")
	}
	return f.inner.EmbedStrings(ctx, texts, opts...)
}

func TestQueryEmbeddingFailureIsKeptAndNotRetried(t *testing.T) {
	cfg := testConfig(t, []string{config.MethodKeyword, config.MethodVector}, []int{3, 5}, config.Limits{})
	env := newEnv(t)
	corpus, ds := loadFixture(t)
	plan, err := MakePlan(context.Background(), PlanInput{Config: cfg, Corpus: corpus, Questions: ds.Filter(dataset.SplitDev), Chunker: env.Chunker, MaxSourceBytes: env.MaxSourceBytes})
	if err != nil {
		t.Fatal(err)
	}
	flaky := &failAfter{inner: env.embedder, n: plan.IndexEmbeddingCalls}
	env.Env.Embedder = flaky
	result, records := execute(t, cfg, env.Env, dataset.SplitDev)

	if !result.Complete || result.Errors != 10 { // 5 questions x 2 vector cells
		t.Fatalf("errors = %d, complete = %v", result.Errors, result.Complete)
	}
	for _, r := range records {
		failed := r.Method == config.MethodVector
		if failed && r.ErrorKind != "unavailable:embedding_unavailable" {
			t.Errorf("%s %s: kind %q", r.QuestionID, r.Cell, r.ErrorKind)
		}
		if !failed && (r.ErrorKind != "" || len(r.Results) == 0) {
			t.Errorf("keyword %s %s: kind %q, %d results; a vector failure must not touch keyword cells", r.QuestionID, r.Cell, r.ErrorKind, len(r.Results))
		}
	}
	if flaky.calls != plan.IndexEmbeddingCalls+5 {
		t.Errorf("%d embedding calls; each question is tried once (%d indexing + 5 queries)", flaky.calls, plan.IndexEmbeddingCalls)
	}
}

func TestIndexingFailureStopsBeforeAnyRetrieval(t *testing.T) {
	cfg := testConfig(t, nil, nil, config.Limits{})
	env := newEnv(t)
	env.embedder.Fail(errors.New("provider down"))
	corpus, ds := loadFixture(t)
	result, err := Execute(context.Background(), cfg, corpus, ds, env.Env, Options{Split: dataset.SplitDev, Now: fixedClock})
	if err == nil || !strings.Contains(err.Error(), "build the index") {
		t.Fatalf("err = %v", err)
	}
	if result == nil || result.Complete || result.Records != 0 || env.backend.Len() != 0 {
		t.Fatalf("result %+v, %d chunks stored", result, env.backend.Len())
	}
	summary, readErr := os.ReadFile(filepath.Join(result.Dir, SummaryFile))
	if readErr != nil || !strings.Contains(string(summary), `"complete": false`) || !strings.Contains(string(summary), "indexing failed") {
		t.Fatalf("the failed run must say so: %s %v", summary, readErr)
	}
}

// ---- determinism and files -------------------------------------------------

func TestRunsAreDeterministicApartFromTimings(t *testing.T) {
	cfg := testConfig(t, nil, nil, config.Limits{})
	_, first := execute(t, cfg, newEnv(t).Env, dataset.SplitDev)
	cfg.Paths.ResultsDir = filepath.Join(filepath.Dir(cfg.Paths.ResultsDir), "results-2")
	_, second := execute(t, cfg, newEnv(t).Env, dataset.SplitDev)
	for _, records := range [][]Record{first, second} {
		for i := range records {
			records[i].ElapsedMs = 0
		}
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("two runs over the same inputs produced different records")
	}
}

func TestARunDirectoryIsNeverOverwritten(t *testing.T) {
	cfg := testConfig(t, []string{config.MethodHybrid}, []int{5}, config.Limits{})
	corpus, ds := loadFixture(t)
	first, err := Execute(context.Background(), cfg, corpus, ds, newEnv(t).Env, Options{Split: dataset.SplitDev, Now: fixedClock})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(first.Dir, RecordsFile))

	_, err = Execute(context.Background(), cfg, corpus, ds, newEnv(t).Env, Options{Split: dataset.SplitDev, Now: fixedClock})
	if err == nil || !strings.Contains(err.Error(), "never overwritten") {
		t.Fatalf("err = %v", err)
	}
	after, _ := os.ReadFile(filepath.Join(first.Dir, RecordsFile))
	if string(before) != string(after) {
		t.Fatal("the first run's records changed")
	}
}

func TestRunFilesRecordTheProvenanceAndHoldNoDocumentText(t *testing.T) {
	cfg := testConfig(t, nil, nil, config.Limits{})
	result, _ := execute(t, cfg, newEnv(t).Env, dataset.SplitDev)

	var manifest Manifest
	raw := mustRead(t, filepath.Join(result.Dir, ManifestFile))
	if err := jsonUnmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	corpus, ds := loadFixture(t)
	switch {
	case manifest.Environment != EnvFake || manifest.Split != dataset.SplitDev:
		t.Errorf("environment %q split %q", manifest.Environment, manifest.Split)
	case manifest.Corpus.Hash != corpus.Hash() || manifest.Dataset.Version != ds.Version || manifest.Dataset.Questions != 5:
		t.Errorf("corpus/dataset provenance: %+v %+v", manifest.Corpus, manifest.Dataset)
	case manifest.ConfigDigest != cfg.Digest() || len(manifest.Fingerprint) != 64 || len(manifest.Cells) != 22:
		t.Errorf("digest/fingerprint/cells: %s %s %d", manifest.ConfigDigest, manifest.Fingerprint, len(manifest.Cells))
	case manifest.Embedding.Model != "synthetic-embedding" || manifest.Embedding.Dimension != testDim || manifest.Embedding.ContentVersion != rag.EmbeddingContentVersion:
		t.Errorf("embedding: %+v", manifest.Embedding)
	case !manifest.Rerank.Attached || manifest.Rerank.Model != "fake/fake-rerank":
		t.Errorf("rerank: %+v", manifest.Rerank)
	case manifest.HitThreshold != 0.5 || manifest.IndexDecision != "build" || manifest.Commit == "":
		t.Errorf("threshold %v decision %s commit %q", manifest.HitThreshold, manifest.IndexDecision, manifest.Commit)
	}
	if !strings.HasSuffix(filepath.Base(result.Dir), cfg.Digest()[:8]) || !strings.HasPrefix(filepath.Base(result.Dir), "20261030T080000Z-") {
		t.Errorf("run directory name %q", filepath.Base(result.Dir))
	}

	// Chunk text, question text and file contents must not reach the results.
	for _, name := range []string{ManifestFile, RecordsFile, SummaryFile} {
		text := string(mustRead(t, filepath.Join(result.Dir, name)))
		for _, forbidden := range []string{"sandbar", "Grey Point", "paraffin", "scion", "keel boat", "boiling point"} {
			if strings.Contains(text, forbidden) {
				t.Errorf("%s contains document or question text %q", name, forbidden)
			}
		}
	}
}

func TestExecuteRefusesAResultsDirectoryInsideAWorktree(t *testing.T) {
	cfg := testConfig(t, nil, nil, config.Limits{})
	repo := filepath.Dir(cfg.Paths.ResultsDir)
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	corpus, ds := loadFixture(t)
	env := newEnv(t)
	_, err := Execute(context.Background(), cfg, corpus, ds, env.Env, Options{Split: dataset.SplitDev, Now: fixedClock})
	if !errors.Is(err, pathguard.ErrInsideWorktree) || env.embedder.Calls() != 0 {
		t.Fatalf("err = %v, %d embedding calls", err, env.embedder.Calls())
	}
	if _, statErr := os.Stat(cfg.Paths.ResultsDir); !os.IsNotExist(statErr) {
		t.Fatal("results directory was created inside a worktree")
	}
}

// ---- the Redis guard -------------------------------------------------------

type recordingGuard struct {
	decision safety.Decision
	checkErr error
	events   []string
	backend  *fake.Backend
	storesAt map[string]int
}

func (g *recordingGuard) Check(context.Context, safety.Fingerprint, bool) (safety.Decision, error) {
	g.events = append(g.events, "check")
	return g.decision, g.checkErr
}
func (g *recordingGuard) Begin(context.Context, safety.Fingerprint) error {
	g.events = append(g.events, "begin")
	g.storesAt["begin"] = g.backend.StoreCalls()
	return nil
}
func (g *recordingGuard) Commit(context.Context, safety.Fingerprint) error {
	g.events = append(g.events, "commit")
	g.storesAt["commit"] = g.backend.StoreCalls()
	return nil
}

func TestAForeignDatabaseStopsTheRunBeforeAnythingIsSpentOrWritten(t *testing.T) {
	cfg := testConfig(t, nil, nil, config.Limits{})
	env := newEnv(t)
	env.Guard = &recordingGuard{checkErr: safety.ErrForeignDatabase, backend: env.backend, storesAt: map[string]int{}}
	corpus, ds := loadFixture(t)
	_, err := Execute(context.Background(), cfg, corpus, ds, env.Env, Options{Split: dataset.SplitDev, Now: fixedClock})
	if !errors.Is(err, safety.ErrForeignDatabase) {
		t.Fatalf("err = %v", err)
	}
	if env.embedder.Calls() != 0 || env.backend.Len() != 0 {
		t.Fatal("a refused run spent calls or wrote chunks")
	}
	if _, statErr := os.Stat(cfg.Paths.ResultsDir); !os.IsNotExist(statErr) {
		t.Fatal("a refused run left a results directory behind")
	}
}

func TestTheDatabaseIsClaimedBeforeTheFirstWriteAndCommittedAfterTheLast(t *testing.T) {
	cfg := testConfig(t, []string{config.MethodHybrid}, []int{5}, config.Limits{})
	env := newEnv(t)
	guard := &recordingGuard{decision: safety.Build, backend: env.backend, storesAt: map[string]int{}}
	env.Guard = guard
	result, _ := execute(t, cfg, env.Env, dataset.SplitDev)
	if !reflect.DeepEqual(guard.events, []string{"check", "begin", "commit"}) {
		t.Fatalf("guard events = %v", guard.events)
	}
	if guard.storesAt["begin"] != 0 || guard.storesAt["commit"] != 3 || result.IndexDecision != "build" {
		t.Errorf("stores at begin %d, at commit %d (want 0 and 3, one per document)", guard.storesAt["begin"], guard.storesAt["commit"])
	}
}

func TestAMatchingIndexIsReusedWithoutIndexingCalls(t *testing.T) {
	cfg := testConfig(t, []string{config.MethodHybrid}, []int{5}, config.Limits{})
	env := newEnv(t)
	first, _ := execute(t, cfg, env.Env, dataset.SplitDev)
	indexCalls := env.embedder.Calls() - 5

	// A second run over the same backend, where the guard says the index matches.
	cfg.Paths.ResultsDir = filepath.Join(filepath.Dir(cfg.Paths.ResultsDir), "results-2")
	env.Guard = &recordingGuard{decision: safety.Reuse, backend: env.backend, storesAt: map[string]int{}}
	storesBefore := env.backend.StoreCalls()
	second, records := execute(t, cfg, env.Env, dataset.SplitDev)
	if second.IndexDecision != "reuse" || env.backend.StoreCalls() != storesBefore {
		t.Fatalf("decision %s, %d new stores", second.IndexDecision, env.backend.StoreCalls()-storesBefore)
	}
	if second.EmbeddingCalls != 5 || second.Estimate.IndexEmbeddingCalls != 0 || indexCalls != first.Estimate.IndexEmbeddingCalls {
		t.Errorf("second run spent %d embedding calls (plan: %d indexing); first run indexed with %d", second.EmbeddingCalls, second.Estimate.IndexEmbeddingCalls, indexCalls)
	}
	for _, r := range records {
		if len(r.Results) == 0 {
			t.Fatalf("%s: a reused index returned nothing", r.QuestionID)
		}
		if r.Results[0].File == "" {
			t.Fatalf("%s: results did not map back to files", r.QuestionID)
		}
	}
}

func TestExecuteRejectsABadEnvironmentAndAMissingReranker(t *testing.T) {
	corpus, ds := loadFixture(t)
	env := newEnv(t)
	env.Kind = "staging"
	if _, err := Execute(context.Background(), testConfig(t, nil, nil, config.Limits{}), corpus, ds, env.Env, Options{Split: dataset.SplitDev, Now: fixedClock}); err == nil {
		t.Fatal("an unknown environment kind must be refused")
	}
	env = newEnv(t)
	env.Reranker = nil
	_, err := Execute(context.Background(), testConfig(t, nil, nil, config.Limits{}), corpus, ds, env.Env, Options{Split: dataset.SplitDev, Now: fixedClock})
	if err == nil || !strings.Contains(err.Error(), "no reranker") {
		t.Fatalf("err = %v", err)
	}
}

func TestErrorKindIsABoundedCode(t *testing.T) {
	for err, want := range map[error]string{
		&rag.RetrievalUnavailableError{Reason: rag.RetrievalReasonVector, Cause: errors.New("secret detail")}: "unavailable:vector_unavailable",
		ErrBudgetExhausted:                           "budget_exhausted",
		rag.ErrInvalidRetrievalRequest:               "invalid_request",
		errors.New("anything with sk-secret inside"): "failed",
	} {
		if got := errorKind(err); got != want {
			t.Errorf("errorKind(%v) = %q, want %q", err, got, want)
		}
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func jsonUnmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }
