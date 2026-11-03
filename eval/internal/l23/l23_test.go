package l23

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiaopang456/GopherLearn/eval/internal/calls"
)

// The golden file is written by the Node scorer (eval/l2l3/test/e2e.test.mjs) from two
// scripted arms, "good" and "flawed", played against the synthetic corpus. Reading it
// here is the contract test: if either side changes the outcome schema, one of the two
// suites fails.
const fixtureDir = "../../l2l3/testdata"

func loadFixture(t *testing.T) ([]Outcome, Meta) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(fixtureDir, "outcomes.fixture.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	outcomes, err := ReadOutcomes(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	metaRaw, err := os.ReadFile(filepath.Join(fixtureDir, "outcomes.fixture.meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	var meta Meta
	if err := json.Unmarshal(metaRaw, &meta); err != nil {
		t.Fatal(err)
	}
	return outcomes, meta
}

func near(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("%s = %v, want %v", name, got, want)
	}
}

func find(t *testing.T, stats *Stats, arm, layer string) ArmLayer {
	t.Helper()
	for _, al := range stats.ArmLayers {
		if al.Arm == arm && al.Layer == layer {
			return al
		}
	}
	t.Fatalf("no result for %s %s", arm, layer)
	return ArmLayer{}
}

func passK(t *testing.T, al ArmLayer, k int) PassK {
	t.Helper()
	for _, p := range al.PassK {
		if p.K == k {
			return p
		}
	}
	t.Fatalf("no pass^%d for %s %s", k, al.Arm, al.Layer)
	return PassK{}
}

func fixtureStats(t *testing.T) (*Stats, Meta) {
	t.Helper()
	outcomes, meta := loadFixture(t)
	stats, err := Compute(outcomes, Options{Baseline: "good", Seed: 11, Resamples: 500})
	if err != nil {
		t.Fatal(err)
	}
	return stats, meta
}

func TestTheFixtureHasTheShapeTheScorerWrites(t *testing.T) {
	outcomes, meta := loadFixture(t)
	if len(outcomes) != 48 || meta.Outcomes != 48 || meta.Records != 48 {
		t.Fatalf("%d outcomes, meta %+v", len(outcomes), meta)
	}
	if len(meta.Arms) != 2 || meta.Arms[0] != "flawed" || meta.Arms[1] != "good" || meta.CorpusHash == "" || meta.DatasetSHA256 == nil {
		t.Fatalf("meta = %+v", meta)
	}
	for _, o := range outcomes {
		if o.Layers.L2 == nil && o.Layers.L3 == nil {
			t.Fatalf("%s/%s has no layer", o.Arm, o.QID)
		}
	}
}

// By hand, from fake/scenarios.mjs (3 repeats per question).
//
// good, L2: 5 questions x 3, every trial passes.
// flawed, L2: neap 0/3, bar 0/3, wick 2/3 (+1 error), fuel 2/3 (+1 fail), tungsten 0/3 (pending)
//
//	pass 4, fail 7, needs_judge 3, error 1 of 15; strict 4/15; decided 4/11;
//	pass^3 = 0; pass^2 = (C(2,2)/C(3,2) + C(2,2)/C(3,2)) / 5 = (1/3 + 1/3) / 5
func TestSuccessRatesAndPassKByHand(t *testing.T) {
	stats, _ := fixtureStats(t)

	good := find(t, stats, "good", LayerL2)
	if good.Questions != 5 || good.Trials != 15 || good.Pass != 15 || good.Repeats != 3 {
		t.Fatalf("good L2 = %+v", good)
	}
	near(t, "good pass@1", good.Pass1Strict, 1)
	near(t, "good pass^3", passK(t, good, 3).Value, 1)

	flawed := find(t, stats, "flawed", LayerL2)
	if flawed.Pass != 4 || flawed.Fail != 7 || flawed.NeedsJudge != 3 || flawed.Error != 1 || flawed.Trials != 15 {
		t.Fatalf("flawed L2 = %+v", flawed)
	}
	near(t, "flawed pass@1 strict", flawed.Pass1Strict, 4.0/15)
	if flawed.Pass1Decided == nil {
		t.Fatal("no decided rate")
	}
	near(t, "flawed pass@1 decided", *flawed.Pass1Decided, 4.0/11)
	near(t, "flawed pass^3", passK(t, flawed, 3).Value, 0)
	near(t, "flawed pass^2", passK(t, flawed, 2).Value, (1.0/3+1.0/3)/5)
	if passK(t, flawed, 3).Questions != 5 || passK(t, flawed, 3).LeftOut != 0 {
		t.Errorf("pass^3 questions = %+v", passK(t, flawed, 3))
	}

	// flawed, L3: neap 3/3, bar 3/3, wick 2/3 (+ error), fuel 3/3, chat 0/3, mem 0/3, hist 3/3
	l3 := find(t, stats, "flawed", LayerL3)
	if l3.Questions != 7 || l3.Trials != 21 || l3.Pass != 14 || l3.Fail != 6 || l3.Error != 1 || l3.NeedsJudge != 0 {
		t.Fatalf("flawed L3 = %+v", l3)
	}
	near(t, "L3 pass@1", l3.Pass1Strict, 14.0/21)
	near(t, "L3 decided", *l3.Pass1Decided, 14.0/20)
	near(t, "L3 pass^3", passK(t, l3, 3).Value, 4.0/7)
	near(t, "L3 pass^2", passK(t, l3, 2).Value, (4+1.0/3)/7)
}

func TestFailureClassesAreCountedApart(t *testing.T) {
	stats, _ := fixtureStats(t)
	l2 := find(t, stats, "flawed", LayerL2)
	want := map[string]int{"citation_miss": 3, "refused": 3, "run_failed": 1, "citation_out_of_range": 1, "grounded_answer": 3}
	for class, n := range want {
		if l2.Classes[class] != n {
			t.Errorf("L2 class %s = %d, want %d (all: %v)", class, l2.Classes[class], n, l2.Classes)
		}
	}
	l3 := find(t, stats, "flawed", LayerL3)
	if l3.Classes["forbidden_tool_used"] != 3 || l3.Classes["missing_required_tool"] != 3 || l3.Classes["run_failed"] != 1 {
		t.Errorf("L3 classes = %v", l3.Classes)
	}
	good := find(t, stats, "good", LayerL2)
	if len(good.Classes) != 0 || good.Tiers["abstained"] != 3 {
		t.Errorf("good: classes %v tiers %v; the unanswerable question is an abstention in the 0 tier", good.Classes, good.Tiers)
	}
	if find(t, stats, "flawed", LayerL2).Tiers["pending"] != 3 {
		t.Error("the grounded answer to the unanswerable question must be pending")
	}
}

func TestAssertionRatesLeaveOutInformationalChecks(t *testing.T) {
	stats, _ := fixtureStats(t)
	l2 := find(t, stats, "flawed", LayerL2)
	byName := map[string]AssertionRate{}
	for _, a := range l2.Assertions {
		byName[a.Name] = a
	}
	if _, ok := byName["citation_coverage"]; ok {
		t.Error("citation_coverage is a number, not a check, and must not appear as a pass rate")
	}
	hits := byName["citation_hits_gold"]
	// answerable trials that reached the citation checks: neap 3 + bar 3 (empty citations) + wick 2 + fuel 3 = 11
	if hits.Evaluated != 11 || hits.Fail != 3+3+1 || hits.Pass != 4 {
		t.Errorf("citation_hits_gold = %+v", hits)
	}
}

// Paired, from the per-question success rates and the "every repeat succeeded" outcome.
//
// L2: good succeeds on all 5 questions; flawed on none -> baseline-only 5, McNemar p = 2 * (1/2)^5.
// per-question rates, flawed minus good: neap -1, bar -1, wick -1/3, fuel -1/3, tungsten -1 -> mean -11/15
// L3: good on all 7; flawed all-repeats on neap, bar, fuel, hist -> baseline-only 3 (wick, chat, mem), p = 2 * (1/2)^3
// rates: flawed 1, 1, 2/3, 1, 0, 0, 1 vs 1 -> mean -(1/3)
func TestPairedComparisonByHand(t *testing.T) {
	stats, _ := fixtureStats(t)
	if len(stats.Comparisons) != 2 {
		t.Fatalf("%d comparisons", len(stats.Comparisons))
	}
	for _, c := range stats.Comparisons {
		if c.Arm != "flawed" || c.Baseline != "good" || c.Repeats != 3 || c.LeftOut != 0 {
			t.Fatalf("%+v", c)
		}
		switch c.Layer {
		case LayerL2:
			if c.Questions != 5 || c.ArmOnly != 0 || c.BaselineOnly != 5 {
				t.Errorf("L2 = %+v", c)
			}
			near(t, "L2 McNemar", c.McNemarP, 2.0/32)
			near(t, "L2 mean rate difference", c.RateDifference.Mean, -11.0/15)
			if !c.RateDifference.CI.OK || c.RateDifference.CI.High > 0 {
				t.Errorf("flawed is worse everywhere, so the interval must stay below zero: %+v", c.RateDifference.CI)
			}
			if c.Clusters != 3 {
				t.Errorf("clusters = %d, want 3: the tide document, the lantern document, and the unanswerable question's own topic", c.Clusters)
			}
		case LayerL3:
			if c.Questions != 7 || c.ArmOnly != 0 || c.BaselineOnly != 3 {
				t.Errorf("L3 = %+v", c)
			}
			near(t, "L3 McNemar", c.McNemarP, 2.0/8)
			near(t, "L3 mean rate difference", c.RateDifference.Mean, -1.0/3)
		default:
			t.Fatalf("layer %q", c.Layer)
		}
	}
}

func TestBaselineRules(t *testing.T) {
	outcomes, _ := loadFixture(t)
	if _, err := Compute(outcomes, Options{Seed: 1, Resamples: 100}); err == nil || !strings.Contains(err.Error(), "which one is the baseline") {
		t.Errorf("two arms without a baseline: %v", err)
	}
	if _, err := Compute(outcomes, Options{Baseline: "ghost", Seed: 1, Resamples: 100}); err == nil || !strings.Contains(err.Error(), "no outcomes") {
		t.Errorf("an unknown baseline: %v", err)
	}
	var one []Outcome
	for _, o := range outcomes {
		if o.Arm == "good" {
			one = append(one, o)
		}
	}
	stats, err := Compute(one, Options{Seed: 1, Resamples: 100})
	if err != nil || len(stats.Comparisons) != 0 {
		t.Errorf("one arm needs no baseline: %v %+v", err, stats)
	}
	if _, err := Compute(nil, Options{}); err == nil {
		t.Error("no outcomes must be an error")
	}
}

func TestQuestionsWithTooFewRepeatsAreLeftOutOfPassKAndSaidSo(t *testing.T) {
	outcomes, _ := loadFixture(t)
	var trimmed []Outcome
	for _, o := range outcomes {
		if o.Arm == "good" && o.QID == "q-bar" && o.Rep == 2 {
			continue // q-bar now has two repeats
		}
		trimmed = append(trimmed, o)
	}
	stats, err := Compute(trimmed, Options{Baseline: "good", Seed: 1, Resamples: 100})
	if err != nil {
		t.Fatal(err)
	}
	good := find(t, stats, "good", LayerL2)
	if pk := passK(t, good, 3); pk.Questions != 4 || pk.LeftOut != 1 {
		t.Errorf("pass^3 = %+v: q-bar has only two repeats and is left out, and the report says so", pk)
	}
	for _, c := range stats.Comparisons {
		if c.Layer == LayerL2 && (c.LeftOut != 1 || c.Questions != 4) {
			t.Errorf("a question with unequal repeats is left out of the pairing: %+v", c)
		}
	}
}

func TestOutcomesAreReadStrictly(t *testing.T) {
	for name, line := range map[string]string{
		"wrong schema":  `{"schema":"outcome.v2","arm":"a","qid":"q","rep":0,"env":"fake","cluster":"c","category":"x","layers":{"l2":null,"l3":null},"cost":{}}`,
		"unknown field": `{"schema":"outcome.v1","arm":"a","qid":"q","rep":0,"env":"fake","cluster":"c","category":"x","layers":{"l2":null,"l3":null},"cost":{},"surprise":1}`,
		"not json":      `{nope`,
	} {
		if _, err := ReadOutcomes(strings.NewReader(line + "\n")); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if _, err := ReadOutcomes(strings.NewReader("\n\n")); err != nil {
		t.Errorf("blank lines are fine: %v", err)
	}
}

func TestMarkdownStatesWhatEachColumnMeans(t *testing.T) {
	stats, meta := fixtureStats(t)
	md := Markdown(stats, meta, Options{Baseline: "good", Seed: 11, Resamples: 500})
	for _, want := range []string{
		"# L2 / L3 评测报告",
		"**离线替身环境（fake）**",
		"单次成功率（待判与错误按失败）",
		"单次成功率（只算已判定）",
		"**pass^k**",
		"k=3：0.571（7 题）",
		"k=2：0.619（7 题）",
		"## 表 3　失败、待判与错误的归类",
		"| flawed | L3 工具调用层 | forbidden_tool_used | 3 |",
		"无答案题档位：如实说明没有证据（0 档）",
		"**这里不给 +1 / −1 档，那要等判分。**",
		"| flawed vs good | L2 回答层 | 5 / 3 | 3 |",
		"McNemar",
		"最小可检测效应",
		"后台任务（记忆抽取、摘要）的调用",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("report lacks %q", want)
		}
	}
	if strings.Contains(md, "citation_coverage") {
		t.Error("the informational coverage number is not a pass rate")
	}
}

func TestARealRunHasNoOfflineBanner(t *testing.T) {
	outcomes, meta := loadFixture(t)
	for i := range outcomes {
		outcomes[i].Env = "real"
	}
	stats, err := Compute(outcomes, Options{Baseline: "good", Seed: 1, Resamples: 100})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(Markdown(stats, meta, Options{Baseline: "good"}), "离线替身环境") {
		t.Error("a real run must not carry the offline banner")
	}
}

func TestStatsSerialise(t *testing.T) {
	stats, _ := fixtureStats(t)
	data, err := JSON(stats)
	if err != nil || !strings.Contains(string(data), `"pass_at_1_strict"`) || !strings.Contains(string(data), `"mcnemar_p"`) {
		t.Fatalf("%v %s", err, data[:min(len(data), 200)])
	}
}

// bookedCalls builds a call ledger for the fixture: one call before the answer
// and one after it for every run of the "good" arm, plus a call that carries
// no identity at all.
func bookedCalls(t *testing.T, outcomes []Outcome) (*calls.Attribution, int) {
	t.Helper()
	answered := time.Date(2026, 11, 2, 11, 0, 10, 0, time.UTC)
	prompt := int64(100)
	var runs []calls.Run
	var list []calls.Call
	for _, o := range outcomes {
		if o.Arm != "good" {
			continue
		}
		id := fmt.Sprintf("run-%s-%d", o.QID, o.Rep)
		runs = append(runs, calls.Run{Arm: o.Arm, QID: o.QID, Rep: o.Rep, RunID: id, SessionID: "s-" + id, AnsweredAt: answered})
		list = append(list, calls.Call{At: answered.Add(-time.Second), Lane: "chat", RunID: id, Usage: &calls.Usage{Prompt: &prompt}})
		list = append(list, calls.Call{At: answered.Add(time.Second), Lane: "chat", RunID: id, Usage: &calls.Usage{Prompt: &prompt}})
	}
	list = append(list, calls.Call{At: answered, Lane: "chat"})
	attribution, err := calls.Attribute(runs, list)
	if err != nil {
		t.Fatal(err)
	}
	return &attribution, len(runs)
}

func TestTheReportSplitsTheCostOfTheAnswerFromTheCostAfterIt(t *testing.T) {
	outcomes, meta := loadFixture(t)
	attribution, runs := bookedCalls(t, outcomes)
	opts := Options{Baseline: "good", Seed: 11, Resamples: 500, Attribution: attribution}
	stats, err := Compute(outcomes, opts)
	if err != nil {
		t.Fatal(err)
	}
	md := Markdown(stats, meta, opts)
	for _, want := range []string{
		"## 表 6　代价（按运行归账：回答本身 / 回答之后的后台）",
		"| good | 回答本身 |",
		"| good | 回答之后的后台 |",
		"**不用墙钟时间窗**",
		"无法归账",
		"情景记忆投影",
		"现有评测测不到情景记忆投影的代价",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("report lacks %q", want)
		}
	}
	for _, c := range stats.Costs {
		if c.Arm != "good" {
			continue
		}
		if c.AttributedRuns != runs {
			t.Errorf("attributed runs = %d, want %d", c.AttributedRuns, runs)
		}
		if c.MeanAnswerCalls == nil || *c.MeanAnswerCalls != 1 {
			t.Errorf("mean answer calls = %v, want 1", c.MeanAnswerCalls)
		}
		if c.MeanAfterAnswerCalls == nil || *c.MeanAfterAnswerCalls != 1 {
			t.Errorf("mean after-answer calls = %v, want 1", c.MeanAfterAnswerCalls)
		}
		if c.MeanAnswerPromptTokens == nil || *c.MeanAnswerPromptTokens != 100 {
			t.Errorf("mean answer prompt tokens = %v, want 100", c.MeanAnswerPromptTokens)
		}
	}
	if stats.Attribution == nil || stats.Attribution.UnattributedCalls != 1 {
		t.Fatalf("the call without identity was not counted apart: %+v", stats.Attribution)
	}
	if stats.Attribution.UnknownIdentityCalls != 1 {
		t.Errorf("unknown identity calls = %d, want 1", stats.Attribution.UnknownIdentityCalls)
	}
	data, err := JSON(stats)
	if err != nil || !strings.Contains(string(data), `"call_attribution"`) {
		t.Fatalf("%v the json does not carry the attribution", err)
	}
}

func TestWithoutACallLedgerTheReportKeepsOnlyWhatTheRunsReported(t *testing.T) {
	outcomes, meta := loadFixture(t)
	stats, err := Compute(outcomes, Options{Baseline: "good", Seed: 11, Resamples: 500})
	if err != nil {
		t.Fatal(err)
	}
	md := Markdown(stats, meta, Options{Baseline: "good", Seed: 11, Resamples: 500})
	if strings.Contains(md, "## 表 6") {
		t.Error("the report shows a cost split without a call ledger")
	}
	if stats.Attribution != nil {
		t.Errorf("attribution without a call ledger: %+v", stats.Attribution)
	}
}
