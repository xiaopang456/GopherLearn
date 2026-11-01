package report

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiaopang456/GopherLearn/eval/internal/dataset"
	"github.com/xiaopang456/GopherLearn/eval/internal/runner"
)

func vh(file string, start, end int, rank int, distance float64) runner.Hit {
	return runner.Hit{File: file, Start: start, End: end, VectorRank: rank, Distance: distance}
}

func TestSignalKindFollowsTheMethod(t *testing.T) {
	cases := []struct {
		meta runner.Record
		want string
	}{
		{runner.Record{Method: "vector", RerankStatus: "bypassed"}, SignalVectorDistance},
		{runner.Record{Method: "keyword", RerankStatus: "bypassed"}, SignalLexicalScore},
		{runner.Record{Method: "hybrid", RerankStatus: "bypassed"}, SignalVectorDistance},
		{runner.Record{Method: "hybrid+rerank", RerankStatus: "applied"}, SignalRerankScore},
		// A rerank cell the product did not rerank is hybrid, so it reads the vector distance.
		{runner.Record{Method: "hybrid+rerank", RerankStatus: "bypassed"}, SignalVectorDistance},
		{runner.Record{}, ""},
	}
	for _, c := range cases {
		if got := signalKind(c.meta); got != c.want {
			t.Errorf("signalKind(%+v) = %q, want %q", c.meta, got, c.want)
		}
	}
}

func TestSignalForReadsTheRawNumberNotTheFusedScore(t *testing.T) {
	// The fused Score is 1.0 everywhere here and must play no part.
	pool := []runner.Hit{{File: "x.md", Start: 1, End: 2, Score: 1, LexicalRank: 1}, vh("a.md", 1, 2, 2, 0.41), vh("b.md", 1, 2, 1, 0.33)}
	rec := runner.Record{Results: []runner.Hit{{File: "x.md", Start: 1, End: 2, Score: 1, RerankScore: 0.77, LexicalScore: 4.5}}, Pool: pool}

	if v, ok := signalFor(SignalVectorDistance, rec); !ok || v != 0.33 {
		t.Errorf("vector distance = %v, %v; want the nearest chunk the vector channel returned (0.33), skipping the keyword-only hit", v, ok)
	}
	if v, ok := signalFor(SignalRerankScore, rec); !ok || v != 0.77 {
		t.Errorf("rerank = %v %v", v, ok)
	}
	if v, ok := signalFor(SignalLexicalScore, rec); !ok || v != 4.5 {
		t.Errorf("lexical = %v %v", v, ok)
	}
	// A record that predates the keyword score, a failed call and an empty result carry no signal.
	if _, ok := signalFor(SignalLexicalScore, runner.Record{Results: []runner.Hit{{Score: 1}}}); ok {
		t.Error("an old record has no keyword score; it must not be read as zero")
	}
	if _, ok := signalFor(SignalRerankScore, runner.Record{ErrorKind: "failed", Results: rec.Results}); ok {
		t.Error("a failed retrieval has no signal")
	}
	if _, ok := signalFor(SignalRerankScore, runner.Record{}); ok {
		t.Error("no results, no signal")
	}
	if _, ok := signalFor(SignalVectorDistance, runner.Record{Pool: []runner.Hit{{File: "x.md", LexicalRank: 1}}}); ok {
		t.Error("a pool with no vector hit has no distance")
	}
}

// By hand. Vector distance, lower is stronger:
//
//	answerable 0.2, 0.3, 0.4      unanswerable 0.35, 0.5
//	0.2 beats both (2), 0.3 beats both (2), 0.4 beats 0.5 only (1)  -> 5 of 6
//
// Rerank score, higher is stronger:
//
//	answerable 0.9, 0.8           unanswerable 0.9, 0.3
//	0.9 ties 0.9 (half) and beats 0.3, 0.8 loses to 0.9 and beats 0.3  -> 2.5 of 4
func TestSeparabilityByHand(t *testing.T) {
	s := buildSignal(SignalVectorDistance, []float64{0.35, 0.5}, []float64{0.4, 0.2, 0.3})
	if s.Stronger != "lower" || s.Unanswerable != 2 || s.Answerable != 3 {
		t.Fatalf("%+v", s)
	}
	near(t, "AUC", *s.AUC, 5.0/6)
	near(t, "answerable median", *s.AnswerableMedian, 0.3)
	near(t, "unanswerable min", s.UnanswerableMinMedMax[0], 0.35)
	near(t, "unanswerable median", s.UnanswerableMinMedMax[1], 0.425)
	near(t, "unanswerable max", s.UnanswerableMinMedMax[2], 0.5)

	r := buildSignal(SignalRerankScore, []float64{0.9, 0.3}, []float64{0.9, 0.8})
	if r.Stronger != "higher" {
		t.Fatalf("%+v", r)
	}
	near(t, "rerank AUC", *r.AUC, 2.5/4)

	// A signal that does not separate the two kinds sits at one half.
	near(t, "no separation", *buildSignal(SignalRerankScore, []float64{0.5, 0.5}, []float64{0.5, 0.5}).AUC, 0.5)
	// With nothing to compare against there is no AUC, and with nothing at all no signal.
	if only := buildSignal(SignalRerankScore, nil, []float64{0.5}); only.AUC != nil || only.UnanswerableMinMedMax != nil {
		t.Errorf("%+v", only)
	}
	if buildSignal(SignalRerankScore, nil, nil) != nil || buildSignal("", []float64{1}, []float64{1}) != nil {
		t.Error("no samples, or no kind, means no signal")
	}
}

func TestACellIsDescribedByItsOwnSignal(t *testing.T) {
	gold := dataset.EvidenceSet{span("a.md", 1, 10)}
	questions := []dataset.Question{
		typed("t1", "single_fact", gold), typed("t2", "single_fact", gold),
		{ID: "u1", Question: "u1", Answerable: false, Split: dataset.SplitDev, Type: "unanswerable"},
		{ID: "u2", Question: "u2", Answerable: false, Split: dataset.SplitDev, Type: "unanswerable"},
	}
	vec := func(q string, d float64) runner.Record {
		return rec("vector/k=3", q, []runner.Hit{vh("a.md", 1, 10, 1, d)}, []runner.Hit{vh("a.md", 1, 10, 1, d)}, "")
	}
	rr := func(q string, score float64) runner.Record {
		r := rec("hybrid+rerank/production/k=3", q, []runner.Hit{{File: "a.md", Start: 1, End: 10, Score: 1, RerankScore: score}}, nil, "")
		r.RerankStatus = "applied"
		return r
	}
	records := []runner.Record{
		vec("t1", 0.2), vec("t2", 0.3), vec("u1", 0.35), vec("u2", 0.5),
		rr("t1", 0.9), rr("t2", 0.8), rr("u1", 0.9), rr("u2", 0.3),
	}
	var manifest runner.Manifest
	manifest.Environment, manifest.Split, manifest.RunID = runner.EnvReal, dataset.SplitDev, "run-signal"
	manifest.Cells = []string{"vector/k=3", "hybrid+rerank/production/k=3"}
	rep, err := Build(Input{Manifest: manifest, Records: records, Questions: questions, Threshold: 0.5, Seed: 1, Resamples: 100})
	if err != nil {
		t.Fatal(err)
	}
	v := cellByID(rep, "vector/k=3").Signal
	if v == nil || v.Name != SignalVectorDistance || v.Answerable != 2 || v.Unanswerable != 2 {
		t.Fatalf("vector signal = %+v", v)
	}
	near(t, "vector AUC", *v.AUC, 1) // both answerable are nearer than both unanswerable
	r := cellByID(rep, "hybrid+rerank/production/k=3").Signal
	if r == nil || r.Name != SignalRerankScore {
		t.Fatalf("rerank signal = %+v", r)
	}
	near(t, "rerank AUC", *r.AUC, (0.5+1+0+1)/4.0) // 0.9 ties 0.9 and beats 0.3; 0.8 loses to 0.9 and beats 0.3

	md := rep.Markdown()
	for _, want := range []string{
		"## 表 4　不可答题的信号分布（仅描述）",
		"向量距离（越小越强）",
		"重排分数（越大越强）",
		"不用检索服务返回的融合分数",
		"不据此定拒答阈值",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("report lacks %q", want)
		}
	}
}

// ---- splitting by group -----------------------------------------------------

// Two cells, four questions. "en" = asked in Chinese about an English source.
//
//	q1 en verbatim   : vector hit, hybrid miss
//	q2 en rephrased  : vector hit, hybrid miss
//	q3 zh verbatim   : both hit
//	q4 zh colloquial : both miss
func groupInput() Input {
	goldA := dataset.EvidenceSet{span("a.md", 1, 10)}
	goldB := dataset.EvidenceSet{span("b.md", 1, 10)}
	questions := []dataset.Question{typed("q1", "single_fact", goldA), typed("q2", "single_fact", goldA), typed("q3", "single_fact", goldA), typed("q4", "single_fact", goldB)}
	good := []runner.Hit{hit("a.md", 1, 10, 0.9)}
	bad := []runner.Hit{hit("x.md", 1, 5, 0.9)}
	records := []runner.Record{
		rec("vector/k=3", "q1", good, nil, ""), rec("hybrid/k=3", "q1", bad, nil, ""),
		rec("vector/k=3", "q2", good, nil, ""), rec("hybrid/k=3", "q2", bad, nil, ""),
		rec("vector/k=3", "q3", good, nil, ""), rec("hybrid/k=3", "q3", good, nil, ""),
		rec("vector/k=3", "q4", bad, nil, ""), rec("hybrid/k=3", "q4", bad, nil, ""),
	}
	var manifest runner.Manifest
	manifest.Environment, manifest.Split, manifest.RunID = runner.EnvReal, dataset.SplitDev, "run-groups"
	manifest.Cells = []string{"vector/k=3", "hybrid/k=3"}
	groups := map[string]map[string]string{
		"q1": {"lang": "zh_qa_en_source", "paraphrase": "verbatim"},
		"q2": {"lang": "zh_qa_en_source", "paraphrase": "rephrased"},
		"q3": {"lang": "zh_qa_zh_source", "paraphrase": "verbatim"},
		"q4": {"lang": "zh_qa_zh_source", "paraphrase": "colloquial"},
	}
	return Input{Manifest: manifest, Records: records, Questions: questions, Groups: groups, Threshold: 0.5, Seed: 3, Resamples: 200}
}

func groupRow(rep *Report, cell, dim, value string) (GroupStats, bool) {
	for _, g := range rep.ByGroup {
		if g.Cell == cell && g.Dimension == dim && g.Value == value {
			return g, true
		}
	}
	return GroupStats{}, false
}

func TestGroupRowsByHand(t *testing.T) {
	rep, err := Build(groupInput())
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		cell, dim, value string
		n, hits          int
	}{
		{"vector/k=3", "lang", "zh_qa_en_source", 2, 2},
		{"hybrid/k=3", "lang", "zh_qa_en_source", 2, 0},
		{"vector/k=3", "lang", "zh_qa_zh_source", 2, 1},
		{"hybrid/k=3", "lang", "zh_qa_zh_source", 2, 1},
		{"vector/k=3", "paraphrase", "verbatim", 2, 2},
		{"hybrid/k=3", "paraphrase", "verbatim", 2, 1},
		{"vector/k=3", "paraphrase", "rephrased", 1, 1},
		{"hybrid/k=3", "paraphrase", "rephrased", 1, 0},
		{"vector/k=3", "paraphrase", "colloquial", 1, 0},
	}
	for _, c := range cases {
		g, ok := groupRow(rep, c.cell, c.dim, c.value)
		if !ok || g.Answerable != c.n || g.Hits != c.hits {
			t.Errorf("%s %s=%s: %+v (found %v), want n=%d hits=%d", c.cell, c.dim, c.value, g, ok, c.n, c.hits)
		}
	}
	g, _ := groupRow(rep, "vector/k=3", "lang", "zh_qa_zh_source")
	near(t, "mean coverage", g.MeanCoverage, 0.5)
	near(t, "MRR", g.MRR, 0.5)
}

// vector - hybrid inside each language group.
//
//	zh_qa_en_source: q1, q2 -> vector-only 2, hybrid-only 0, McNemar p = 2 * (1/2)^2 = 0.5, mean hit difference +1
//	zh_qa_zh_source: q3, q4 -> no disagreement, p = 1, mean difference 0
func TestGroupComparisonsByHand(t *testing.T) {
	rep, _ := Build(groupInput())
	find := func(dim, value string) GroupComparison {
		for _, c := range rep.GroupComparisons {
			if c.Dimension == dim && c.Value == value && c.Challenger == "vector/k=3" {
				return c
			}
		}
		t.Fatalf("no comparison for %s=%s", dim, value)
		return GroupComparison{}
	}
	en := find("lang", "zh_qa_en_source")
	if en.N != 2 || en.Discordant.AOnly != 2 || en.Discordant.BOnly != 0 || en.Baseline != "hybrid/k=3" {
		t.Fatalf("%+v", en)
	}
	near(t, "en McNemar", en.McNemarP, 0.5)
	near(t, "en hit difference", en.Hit.Mean, 1)
	zh := find("lang", "zh_qa_zh_source")
	if zh.N != 2 || zh.Discordant.AOnly != 0 || zh.Discordant.BOnly != 0 {
		t.Fatalf("%+v", zh)
	}
	near(t, "zh McNemar", zh.McNemarP, 1)
	near(t, "zh hit difference", zh.Hit.Mean, 0)
	// The whole-run comparison is unchanged by the split.
	whole := rep.Comparisons[0]
	if whole.N != 4 || whole.Discordant.AOnly != 2 {
		t.Fatalf("%+v", whole)
	}
}

func TestGroupTablesAreInTheReportOnlyWhenTheRunIsSplit(t *testing.T) {
	rep, _ := Build(groupInput())
	md := rep.Markdown()
	for _, want := range []string{
		"## 表 6　按语言与表述类型拆分",
		"| vector/k=3 | 语言 | zh_qa_en_source | 2 | 1.000 (2/2) |",
		"## 表 7　各组内的配对比较",
		"| vector/k=3 | 3 | 语言 | zh_qa_en_source | 2 | +1.000",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("report lacks %q", want)
		}
	}
	in := groupInput()
	in.Groups = nil
	plain, _ := Build(in)
	if strings.Contains(plain.Markdown(), "表 6") || len(plain.ByGroup) != 0 {
		t.Error("an unsplit run must not carry group tables")
	}
}

func TestLoadGroupsDecodesOnlyTheQuestionsAskedFor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sidecar.jsonl")
	body := strings.Join([]string{
		`{"id":"q1","lang":"zh_qa_en_source","paraphrase":"verbatim","points":[{"text":"x"}]}`,
		// A question that is not asked for: its body is not even valid JSON, so a decoder would fail on it.
		`{"id":"held-back-1","lang":"zh_qa_zh_source", this is not json at all`,
		`{"id":"q2","paraphrase":"colloquial","lang":"zh_qa_zh_source"}`,
		`not a record at all`,
		``,
	}, "\n")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	groups, err := LoadGroups(path, map[string]bool{"q1": true, "q2": true})
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 2 || groups["q1"]["lang"] != "zh_qa_en_source" || groups["q2"]["paraphrase"] != "colloquial" {
		t.Fatalf("%v", groups)
	}
	if _, leaked := groups["held-back-1"]; leaked {
		t.Fatal("a question that was not asked for was read")
	}
	// Only the two dimensions are kept, nothing else of the row.
	if len(groups["q1"]) != 2 {
		t.Errorf("q1 carries %v", groups["q1"])
	}
}

func TestLoadGroupsRefusesAnIncompleteSidecar(t *testing.T) {
	write := func(lines ...string) string {
		p := filepath.Join(t.TempDir(), "s.jsonl")
		if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	want := map[string]bool{"q1": true, "q2": true}
	cases := map[string]string{
		"a missing row":       write(`{"id":"q1","lang":"a","paraphrase":"b"}`),
		"a missing dimension": write(`{"id":"q1","lang":"a"}`, `{"id":"q2","lang":"a","paraphrase":"b"}`),
		"an empty value":      write(`{"id":"q1","lang":"","paraphrase":"b"}`, `{"id":"q2","lang":"a","paraphrase":"b"}`),
		"a duplicate row":     write(`{"id":"q1","lang":"a","paraphrase":"b"}`, `{"id":"q1","lang":"a","paraphrase":"b"}`, `{"id":"q2","lang":"a","paraphrase":"b"}`),
		"a broken wanted row": write(`{"id":"q1","lang":"a",`, `{"id":"q2","lang":"a","paraphrase":"b"}`),
	}
	for name, path := range cases {
		if _, err := LoadGroups(path, want); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if _, err := LoadGroups(filepath.Join(t.TempDir(), "absent.jsonl"), want); err == nil {
		t.Error("a missing sidecar must fail")
	}
}
