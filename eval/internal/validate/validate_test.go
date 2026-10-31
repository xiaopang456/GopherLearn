package validate

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiaopang456/GopherLearn/eval/internal/dataset"
)

// writeCorpus writes a corpus of documents and returns a loaded corpus.
func writeCorpus(t *testing.T, files map[string]string) *dataset.Corpus {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "files")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	type entry struct {
		name string
		sum  string
	}
	var entries []entry
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		entries = append(entries, entry{name: name})
	}
	var b strings.Builder
	b.WriteString(`{"name":"test","root":"files","documents":[`)
	for i, e := range entries {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"file":"` + e.name + `"}`)
	}
	b.WriteString("]}")
	manifest := filepath.Join(dir, "corpus.json")
	if err := os.WriteFile(manifest, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	corpus, err := dataset.LoadCorpus(manifest)
	if err != nil {
		t.Fatalf("load corpus: %v", err)
	}
	return corpus
}

func span(file string, start, end int) dataset.Span {
	return dataset.Span{File: file, Start: start, End: end}
}

func rulesOf(report *Report, severity Severity) map[string]int {
	out := map[string]int{}
	for _, f := range report.Findings {
		if f.Severity == severity {
			out[f.Rule]++
		}
	}
	return out
}

func TestClassifyLine(t *testing.T) {
	cases := []struct {
		line string
		want LineKind
	}{
		{"", LineBlank},
		{"   \t ", LineBlank},
		{"![](https://cdn.example/a.png)", LineImage},
		{"![](https://cdn.example/a.png)   ", LineImage},
		{`<img src="https://cdn.example/a.png" width="684" title="" />`, LineImage},
		{"# 标题", LineHeading},
		{"## <font style=\"color:red\">1.6 怎么拼字符串</font>", LineHeading},
		{"###### 六级标题", LineHeading},
		{"####### 七级不是标题", LineContent},
		{"#不是标题", LineContent},
		{"## 标题", LineHeading},
		{"---", LineRule},
		{"***", LineRule},
		{"```go", LineFence},
		{"普通正文内容", LineContent},
		{"- 列表项", LineContent},
	}
	for _, tc := range cases {
		if got := ClassifyLine(tc.line); got != tc.want {
			t.Errorf("ClassifyLine(%q) = %v, want %v", tc.line, got, tc.want)
		}
	}
}

func TestEvidenceMustNotBeImageOnlyOrBlank(t *testing.T) {
	corpus := writeCorpus(t, map[string]string{
		"a.md": "第一行正文\n\n![](https://cdn.example/x.png)\n末行正文\n",
	})
	questions := []dataset.Question{
		{ID: "q-image", Question: "图片说明了什么", Answerable: true,
			Evidence: []dataset.EvidenceSet{{span("a.md", 3, 3)}}, Split: "dev"},
		{ID: "q-blank", Question: "空行附近写了什么", Answerable: true,
			Evidence: []dataset.EvidenceSet{{span("a.md", 2, 2)}}, Split: "dev"},
		{ID: "q-ok", Question: "正文说了什么", Answerable: true,
			Evidence: []dataset.EvidenceSet{{span("a.md", 1, 1)}}, Split: "dev"},
	}
	report, err := Validate(corpus, questions, nil, Quota{}, DefaultParams())
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	errors := rulesOf(report, SeverityError)
	// An image line inside a span is only a warning: the span may also hold
	// body text, and a picture that is merely part of a paragraph is not a
	// defect. A span whose only content is an image is still an error, but
	// that is the no-content rule.
	if errors[RuleEvidenceImageOnly] != 0 {
		t.Errorf("image-only findings = %d, want 0 (the rule is a warning now)", errors[RuleEvidenceImageOnly])
	}
	if got := rulesOf(report, SeverityWarning)[RuleEvidenceImageOnly]; got != 1 {
		t.Errorf("image warnings = %d, want 1: %v", got, report.Findings)
	}
	// The image span has no body text either, and the blank span has none:
	// both are rejected by the no-content rule as well.
	if errors[RuleEvidenceNoContent] != 2 {
		t.Errorf("no-content findings = %d, want 2 (the image line and the blank line)", errors[RuleEvidenceNoContent])
	}
	// The good span must not be reported.
	for _, f := range report.Findings {
		if f.ID == "q-ok" && f.Severity == SeverityError {
			t.Errorf("the good question was reported: %s", f)
		}
	}
}

func TestEvidenceMustNotBeHeadingOnly(t *testing.T) {
	corpus := writeCorpus(t, map[string]string{
		"a.md": "# 标题\n正文一行\n## 二级标题\n",
	})
	questions := []dataset.Question{
		{ID: "q-heading", Question: "标题是什么", Answerable: true,
			Evidence: []dataset.EvidenceSet{{span("a.md", 1, 1)}}, Split: "dev"},
	}
	report, err := Validate(corpus, questions, nil, Quota{}, DefaultParams())
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if rulesOf(report, SeverityError)[RuleEvidenceNoContent] != 1 {
		t.Errorf("a heading-only span must be rejected, findings: %v", rulesOf(report, SeverityError))
	}
}

func TestEvidenceHeadingInsideSpanIsWarningOnly(t *testing.T) {
	corpus := writeCorpus(t, map[string]string{
		"a.md": "# 标题\n正文一行\n",
	})
	questions := []dataset.Question{
		{ID: "q-mixed", Question: "正文是什么", Answerable: true,
			Evidence: []dataset.EvidenceSet{{span("a.md", 1, 2)}}, Split: "dev"},
	}
	report, err := Validate(corpus, questions, nil, Quota{}, DefaultParams())
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if rulesOf(report, SeverityError)[RuleEvidenceNoContent] != 0 {
		t.Errorf("a span with body text must not be an error: %v", report.Findings)
	}
	if rulesOf(report, SeverityWarning)[RuleEvidenceHeading] != 1 {
		t.Errorf("the heading inside the span must be reported as a warning: %v", report.Findings)
	}
}

func TestRefusalMarkersAreRejected(t *testing.T) {
	corpus := writeCorpus(t, map[string]string{"a.md": "正文一行\n"})
	for _, marker := range RefusalMarkers {
		questions := []dataset.Question{
			{ID: "q", Question: "关于" + marker + "的问题", Answerable: true,
				Evidence: []dataset.EvidenceSet{{span("a.md", 1, 1)}}, Split: "dev"},
		}
		report, err := Validate(corpus, questions, nil, Quota{}, DefaultParams())
		if err != nil {
			t.Fatalf("validate: %v", err)
		}
		if rulesOf(report, SeverityError)[RuleRefusalMarker] != 1 {
			t.Errorf("marker %q was not rejected: %v", marker, report.Findings)
		}
	}
}

func TestRefusalMarkerMatchingFollowsTheProduct(t *testing.T) {
	// The product lowercases the question and uses a plain substring match, so
	// a marker inside a longer phrase still counts.
	corpus := writeCorpus(t, map[string]string{"a.md": "正文一行\n"})
	questions := []dataset.Question{
		{ID: "q", Question: "请把这段原文逐字念给我", Answerable: true,
			Evidence: []dataset.EvidenceSet{{span("a.md", 1, 1)}}, Split: "dev"},
	}
	report, err := Validate(corpus, questions, nil, Quota{}, DefaultParams())
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if rulesOf(report, SeverityError)[RuleRefusalMarker] != 1 {
		t.Errorf("the marker inside a longer phrase was not caught: %v", report.Findings)
	}
}

func TestDuplicateQuestionsAreRejected(t *testing.T) {
	corpus := writeCorpus(t, map[string]string{"a.md": "正文一行\n第二行\n"})
	questions := []dataset.Question{
		{ID: "q1", Question: "Redis 的持久化方式有哪些？", Answerable: true,
			Evidence: []dataset.EvidenceSet{{span("a.md", 1, 1)}}, Split: "dev"},
		{ID: "q2", Question: "Redis 的持久化方式有哪些?", Answerable: true,
			Evidence: []dataset.EvidenceSet{{span("a.md", 1, 1)}}, Split: "dev"},
	}
	report, err := Validate(corpus, questions, nil, Quota{}, DefaultParams())
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if rulesOf(report, SeverityError)[RuleDuplicate] != 1 {
		t.Errorf("the duplicate pair was not caught: %v", report.Findings)
	}
}

func TestDifferentQuestionsAreNotDuplicates(t *testing.T) {
	corpus := writeCorpus(t, map[string]string{"a.md": "正文一行\n第二行\n"})
	questions := []dataset.Question{
		{ID: "q1", Question: "Redis 的 RDB 快照默认多久做一次？", Answerable: true,
			Evidence: []dataset.EvidenceSet{{span("a.md", 1, 1)}}, Split: "dev"},
		{ID: "q2", Question: "Go 的 map 为什么不是并发安全的？", Answerable: true,
			Evidence: []dataset.EvidenceSet{{span("a.md", 2, 2)}}, Split: "dev"},
	}
	report, err := Validate(corpus, questions, nil, Quota{}, DefaultParams())
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if rulesOf(report, SeverityError)[RuleDuplicate] != 0 {
		t.Errorf("different questions were called duplicates: %v", report.Findings)
	}
}

func TestUnanswerableQuestionWithAMatchingCorpusLineIsRejected(t *testing.T) {
	corpus := writeCorpus(t, map[string]string{
		"a.md": "Redis 的 AOF 默认每秒钟 fsync 一次。\n别的内容。\n",
	})
	questions := []dataset.Question{
		{ID: "q", Question: "Redis 的 AOF 默认多久 fsync 一次？", Answerable: false, Split: "dev"},
	}
	report, err := Validate(corpus, questions, nil, Quota{}, DefaultParams())
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if rulesOf(report, SeverityError)[RuleUnanswerableHit] != 1 {
		t.Errorf("an unanswerable question with a matching line was not caught: %v", report.Findings)
	}
}

// TestUnanswerableQuestionWithoutAMatchPasses checks that a question whose
// keywords are nowhere in the corpus is not rejected.
func TestUnanswerableQuestionWithoutAMatchPasses(t *testing.T) {
	corpus := writeCorpus(t, map[string]string{
		"a.md": "Redis 的 AOF 默认每秒钟 fsync 一次。\n别的内容。\n",
	})
	questions := []dataset.Question{
		{ID: "q", Question: "PostgreSQL 的流复制槽在什么情况下会膨胀？", Answerable: false, Split: "dev"},
	}
	report, err := Validate(corpus, questions, nil, Quota{}, DefaultParams())
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if rulesOf(report, SeverityError)[RuleUnanswerableHit] != 0 {
		t.Errorf("an unanswerable question without a match was rejected: %v", report.Findings)
	}
}

// TestUnanswerableKeywordsSplitAcrossLinesAreNotAnAnswer pins the rule that
// made the old check unusable.
//
// The keyword extractor emits the whole Chinese run and every two-character
// slice of it, so a question about "默认值" also yields "认值", a fragment
// that turns up in unrelated lines. The old check counted a single keyword
// hit as an answer, so nearly every unanswerable question was rejected.
//
// Now a corpus line only counts as the answer when it carries *every*
// distinctive keyword. Keywords that sit on different lines are a suspect
// (warning), not an error.
func TestUnanswerableKeywordsSplitAcrossLinesAreNotAnAnswer(t *testing.T) {
	corpus := writeCorpus(t, map[string]string{
		"a.md": "配置项很重要。\n默认值的说明在附录。\n",
	})
	questions := []dataset.Question{
		{ID: "q", Question: "配置项的默认值是什么？", Answerable: false, Split: "dev"},
	}
	report, err := Validate(corpus, questions, nil, Quota{}, DefaultParams())
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got := rulesOf(report, SeverityError)[RuleUnanswerableHit]; got != 0 {
		t.Errorf("keywords on different lines were called an answer: %v", report.Findings)
	}
	if got := rulesOf(report, SeverityWarning)[RuleUnanswerableSuspect]; got != 1 {
		t.Errorf("the split keywords were not reported as a suspect: %v", report.Findings)
	}
}

// TestUnanswerableQuestionWithEveryKeywordOnOneLineIsRejected is the other
// half of the rule: when one corpus line carries every distinctive keyword,
// the question really is answered and must be rejected.
func TestUnanswerableQuestionWithEveryKeywordOnOneLineIsRejected(t *testing.T) {
	corpus := writeCorpus(t, map[string]string{
		"a.md": "配置项的默认值是 30。\n别的内容。\n",
	})
	questions := []dataset.Question{
		{ID: "q", Question: "配置项的默认值是什么？", Answerable: false, Split: "dev"},
	}
	report, err := Validate(corpus, questions, nil, Quota{}, DefaultParams())
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got := rulesOf(report, SeverityError)[RuleUnanswerableHit]; got != 1 {
		t.Errorf("a corpus line with every keyword was not caught: %v", report.Findings)
	}
}

// TestUnanswerableCommonKeywordIsNotDistinctive checks that a keyword that
// shows up all over the corpus cannot condemn a question.
func TestUnanswerableCommonKeywordIsNotDistinctive(t *testing.T) {
	var lines []string
	for i := 0; i < 200; i++ {
		lines = append(lines, "这一行提到了默认值，是第 "+string(rune('a'+i%26))+string(rune('a'+i/26))+" 行。")
	}
	corpus := writeCorpus(t, map[string]string{"a.md": strings.Join(lines, "\n") + "\n"})
	questions := []dataset.Question{
		{ID: "q", Question: "流复制槽的默认值是什么？", Answerable: false, Split: "dev"},
	}
	report, err := Validate(corpus, questions, nil, Quota{}, DefaultParams())
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got := rulesOf(report, SeverityError)[RuleUnanswerableHit]; got != 0 {
		t.Errorf("a question was rejected because of a very common keyword: %v", report.Findings)
	}
	if got := rulesOf(report, SeverityWarning)[RuleUnanswerableInconclusive]; got != 1 {
		t.Errorf("the question was not reported as inconclusive: %v", report.Findings)
	}
}

// TestUnanswerablePartialMatchIsOnlyASuspect checks that a partial keyword
// hit is reported for a human to look at, but does not fail the validation.
func TestUnanswerablePartialMatchIsOnlyASuspect(t *testing.T) {
	corpus := writeCorpus(t, map[string]string{
		"a.md": "配置项很重要。\n默认值的说明在附录。\n",
	})
	questions := []dataset.Question{
		{ID: "q", Question: "配置项的默认值是什么？", Answerable: false, Split: "dev"},
	}
	report, err := Validate(corpus, questions, nil, Quota{}, DefaultParams())
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got := rulesOf(report, SeverityError)[RuleUnanswerableHit]; got != 0 {
		t.Errorf("a partial match was called an answer: %v", report.Findings)
	}
	if got := rulesOf(report, SeverityWarning)[RuleUnanswerableSuspect]; got != 1 {
		t.Errorf("the partial match was not reported as a suspect: %v", report.Findings)
	}
}

// TestUnanswerableCommonIdentifierIsOnlyASuspect pins the rule that the
// second review round changed.
//
// A question that names a topic the corpus talks about a lot ("AOF 的压缩
// 策略默认值是多少", while the corpus only explains AOF in general) used to
// be rejected as "the corpus answers it", because some corpus line carries
// the identifier AOF and nothing else. Those questions are exactly the ones
// the plan asks for, so treating the identifier alone as the answer burned
// the generation budget on every unanswerable question.
func TestUnanswerableCommonIdentifierIsOnlyASuspect(t *testing.T) {
	var lines []string
	for i := 0; i < 70; i++ {
		lines = append(lines, fmt.Sprintf("这一行讲 AOF 的第 %d 个方面。", i))
	}
	corpus := writeCorpus(t, map[string]string{"a.md": strings.Join(lines, "\n") + "\n"})
	questions := []dataset.Question{
		{ID: "q", Question: "Redis 的 AOF 压缩策略默认是什么？", Answerable: false, Split: "dev"},
	}
	report, err := Validate(corpus, questions, nil, Quota{}, DefaultParams())
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got := rulesOf(report, SeverityError)[RuleUnanswerableHit]; got != 0 {
		t.Errorf("a common identifier was treated as the answer: %v", report.Findings)
	}
	if got := rulesOf(report, SeverityWarning)[RuleUnanswerableSuspect]; got != 1 {
		t.Errorf("the common identifier was not reported as a suspect: %v", report.Findings)
	}
}

// TestUnanswerableQuestionIsNeverSilent checks that a question the screen
// cannot decide is still reported.
//
// The plan promises that "nothing was reported" never means "checked and
// clean": a question whose keywords turn up nowhere near each other is
// inconclusive, and that is said out loud.
func TestUnanswerableQuestionIsNeverSilent(t *testing.T) {
	corpus := writeCorpus(t, map[string]string{"a.md": "AOF 相关内容。\n"})
	questions := []dataset.Question{
		{ID: "q", Question: "Redis 的 AOF 压缩策略默认是什么？", Answerable: false, Split: "dev"},
	}
	report, err := Validate(corpus, questions, nil, Quota{}, DefaultParams())
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(report.Findings) == 0 {
		t.Fatal("the question was not reported at all")
	}
	if got := rulesOf(report, SeverityWarning)[RuleUnanswerableInconclusive]; got != 1 {
		t.Errorf("the question was not reported as inconclusive: %v", report.Findings)
	}
}

func TestQuotaChecks(t *testing.T) {
	corpus := writeCorpus(t, map[string]string{"a.md": "正文一行\n第二行\n"})
	questions := []dataset.Question{
		{ID: "q1", Question: "问题一", Answerable: true, Type: "single_fact", Topic: "go",
			Evidence: []dataset.EvidenceSet{{span("a.md", 1, 1)}}, Split: "dev"},
		{ID: "q2", Question: "问题二", Answerable: false, Type: "unanswerable", Topic: "go", Split: "dev"},
	}
	meta := []Meta{
		{ID: "q1", Paraphrase: "verbatim", Lang: "zh_qa_zh_source"},
		{ID: "q2", Paraphrase: "rephrased", Lang: "zh_qa_zh_source"},
	}
	quota := Quota{}
	quota.Total.Min = 2
	quota.Types = map[string]int{"single_fact": 1, "unanswerable": 1}
	quota.Paraphrase = map[string]int{"verbatim": 1, "rephrased": 1}
	quota.Lang = map[string]int{"zh_qa_zh_source": 2}
	quota.PerTopic = map[string]int{"go": 2}
	quota.MinRephrasedRatio = 0.3
	quota.Split = map[string]int{"dev": 2, "sealed": 0}
	report, err := Validate(corpus, questions, meta, quota, DefaultParams())
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if !report.OK() {
		t.Errorf("a set that matches the quota was rejected: %v", report.Findings)
	}

	// Now break the rephrased floor and the type count.
	quota.MinRephrasedRatio = 0.9
	quota.Types = map[string]int{"single_fact": 5}
	report, err = Validate(corpus, questions, meta, quota, DefaultParams())
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if report.OK() {
		t.Error("a set that breaks the quota was accepted")
	}
	if rulesOf(report, SeverityError)[RuleQuotaRephrased] == 0 {
		t.Error("the rephrased floor was not checked")
	}
}

func TestSplitIsStratifiedAndProportional(t *testing.T) {
	var questions []dataset.Question
	var meta []Meta
	for i := 0; i < 100; i++ {
		id := "q" + string(rune('a'+i/10)) + string(rune('0'+i%10))
		kind := "single_fact"
		if i%2 == 0 {
			kind = "multi_evidence"
		}
		questions = append(questions, dataset.Question{
			ID: id, Question: "问题 " + id, Answerable: true, Type: kind, Topic: "go", Split: "dev"})
		meta = append(meta, Meta{ID: id, Paraphrase: "rephrased", Lang: "zh_qa_zh_source"})
	}
	result, err := Split(questions, meta, 42)
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	if len(result.Dev) != 60 || len(result.Sealed) != 40 {
		t.Errorf("split sizes = %d/%d, want 60/40", len(result.Dev), len(result.Sealed))
	}
	// Both kinds must appear on both sides.
	devKinds := map[string]int{}
	for _, id := range result.Dev {
		for _, q := range questions {
			if q.ID == id {
				devKinds[q.Type]++
			}
		}
	}
	if devKinds["single_fact"] == 0 || devKinds["multi_evidence"] == 0 {
		t.Errorf("the split did not stratify by type: %v", devKinds)
	}
	// The same seed must give the same split.
	again, err := Split(questions, meta, 42)
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	if strings.Join(again.Dev, ",") != strings.Join(result.Dev, ",") {
		t.Error("the split is not reproducible for the same seed")
	}
}

func TestNuggetKeywordMustBeNearTheEvidence(t *testing.T) {
	corpus := writeCorpus(t, map[string]string{
		"a.md": "第一段说 Redis 用 AOF。\n第二段说别的。\n",
	})
	questions := []dataset.Question{
		{ID: "q", Question: "Redis 用什么做持久化？", Answerable: true,
			Evidence: []dataset.EvidenceSet{{span("a.md", 1, 1)}}, Split: "dev"},
	}
	meta := []Meta{
		{ID: "q", Paraphrase: "verbatim", Lang: "zh_qa_zh_source",
			Points: []Point{{Text: "用 AOF 持久化", Keywords: []string{"AOF"}}}},
	}
	report, err := Validate(corpus, questions, meta, Quota{}, DefaultParams())
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if rulesOf(report, SeverityError)[RuleNuggetMissing] != 0 {
		t.Errorf("a present keyword was reported missing: %v", report.Findings)
	}

	meta[0].Points[0].Keywords = []string{"binlog"}
	report, err = Validate(corpus, questions, meta, Quota{}, DefaultParams())
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if rulesOf(report, SeverityError)[RuleNuggetMissing] != 1 {
		t.Errorf("an absent keyword was not reported: %v", report.Findings)
	}
}

// TestTopicFloorNeverExceedsTheTopicTarget checks that a topic the plan gives
// zero questions is not also required to have some.
//
// The plan lowers the quota of the documents that have too little material.
// A document planned with zero questions must be allowed to end up with zero
// questions, or the floor and the target contradict each other.
func TestTopicFloorNeverExceedsTheTopicTarget(t *testing.T) {
	corpus := writeCorpus(t, map[string]string{"a.md": "正文一行\n"})
	questions := []dataset.Question{
		{ID: "q1", Question: "问题一", Answerable: true, Type: "single_fact", Topic: "docs/a.md",
			Evidence: []dataset.EvidenceSet{{span("a.md", 1, 1)}}, Split: "dev"},
		{ID: "q2", Question: "问题二", Answerable: true, Type: "single_fact", Topic: "docs/b.md",
			Evidence: []dataset.EvidenceSet{{span("a.md", 1, 1)}}, Split: "dev"},
	}
	meta := []Meta{
		{ID: "q1", Paraphrase: "verbatim", Lang: "zh_qa_zh_source"},
		{ID: "q2", Paraphrase: "verbatim", Lang: "zh_qa_zh_source"},
	}
	quota := Quota{}
	quota.PerTopic = map[string]int{"docs/a.md": 0, "docs/b.md": 1}
	quota.PerTopicMin = 5
	report, err := Validate(corpus, questions, meta, quota, DefaultParams())
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got := rulesOf(report, SeverityError)[RuleQuotaTopic]; got != 0 {
		t.Errorf("the topic floor fought the topic target: %v", report.Findings)
	}
	// A topic that is below its own target is still reported, but only as a
	// warning.
	if got := rulesOf(report, SeverityWarning)[RuleQuotaTopic]; got != 1 {
		t.Errorf("the topic count drift was not reported: %v", report.Findings)
	}
}

// TestTopicFloorStillBitesBelowTheTarget checks that the cap on the floor does
// not turn the floor off for the topics that still have a target.
func TestTopicFloorStillBitesBelowTheTarget(t *testing.T) {
	corpus := writeCorpus(t, map[string]string{"a.md": "正文一行\n"})
	questions := []dataset.Question{
		{ID: "q1", Question: "问题一", Answerable: true, Type: "single_fact", Topic: "docs/a.md",
			Evidence: []dataset.EvidenceSet{{span("a.md", 1, 1)}}, Split: "dev"},
	}
	meta := []Meta{{ID: "q1", Paraphrase: "verbatim", Lang: "zh_qa_zh_source"}}
	quota := Quota{}
	quota.PerTopic = map[string]int{"docs/a.md": 5}
	quota.PerTopicMin = 5
	report, err := Validate(corpus, questions, meta, quota, DefaultParams())
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got := rulesOf(report, SeverityError)[RuleQuotaTopic]; got != 1 {
		t.Errorf("a topic below its target was not reported as an error: %v", report.Findings)
	}
}

// TestSplitGivesTheLeftoversToTheLargestFraction pins the largest remainder
// method: the dev slots that do not fit into a whole share go to the strata
// whose fractional part is the largest.
//
// The strata here are sized so that the floors leave two slots over. A share
// of four (fraction 0.4) must not receive one of them while two strata with
// a fraction of 0.8 miss out.
func TestSplitGivesTheLeftoversToTheLargestFraction(t *testing.T) {
	sizes := map[string]int{"comparison": 4, "multi_evidence": 3, "single_fact": 3}
	var questions []dataset.Question
	var meta []Meta
	for kind, n := range sizes {
		for i := 0; i < n; i++ {
			id := fmt.Sprintf("%s-%d", kind, i)
			questions = append(questions, dataset.Question{
				ID: id, Question: "问题 " + id, Answerable: true, Type: kind, Topic: "go", Split: "dev"})
			meta = append(meta, Meta{ID: id, Paraphrase: "rephrased", Lang: "zh_qa_zh_source"})
		}
	}
	result, err := Split(questions, meta, 42)
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	if len(result.Dev) != 6 || len(result.Sealed) != 4 {
		t.Fatalf("split sizes = %d/%d, want 6/4", len(result.Dev), len(result.Sealed))
	}
	devByKind := map[string]int{}
	for _, id := range result.Dev {
		for _, q := range questions {
			if q.ID == id {
				devByKind[q.Type]++
			}
		}
	}
	// The two strata with the fraction 0.8 get the leftover slots, so the
	// stratum with the fraction 0.4 keeps two dev questions, not three.
	for kind, want := range map[string]int{"comparison": 2, "multi_evidence": 2, "single_fact": 2} {
		if devByKind[kind] != want {
			t.Errorf("stratum %q has %d dev questions, want %d (dev by stratum: %v)",
				kind, devByKind[kind], want, devByKind)
		}
	}
}

// TestSplitKeepsTheTopicAndParaphraseMix checks that the split does not put
// every question of one topic or one paraphrase class on the same side.
func TestSplitKeepsTheTopicAndParaphraseMix(t *testing.T) {
	var questions []dataset.Question
	var meta []Meta
	topics := []string{"go", "redis"}
	classes := []string{"verbatim", "rephrased"}
	for _, topic := range topics {
		for _, class := range classes {
			for i := 0; i < 5; i++ {
				id := fmt.Sprintf("%s-%s-%d", topic, class, i)
				questions = append(questions, dataset.Question{
					ID: id, Question: "问题 " + id, Answerable: true, Type: "single_fact", Topic: topic, Split: "dev"})
				meta = append(meta, Meta{ID: id, Paraphrase: class, Lang: "zh_qa_zh_source"})
			}
		}
	}
	result, err := Split(questions, meta, 42)
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	devSet := map[string]bool{}
	for _, id := range result.Dev {
		devSet[id] = true
	}
	got := map[string]int{}
	for _, q := range questions {
		class := "verbatim"
		for _, m := range meta {
			if m.ID == q.ID {
				class = m.Paraphrase
			}
		}
		key := q.Topic + "|" + class
		if devSet[q.ID] {
			got[key]++
		}
	}
	// Four sub-groups of five, twelve dev questions: three each.
	for _, topic := range topics {
		for _, class := range classes {
			key := topic + "|" + class
			if got[key] != 3 {
				t.Errorf("sub-group %q has %d dev questions, want 3 (all: %v)", key, got[key], got)
			}
		}
	}
}

// TestQuotaSplitTolerance checks that a split count may drift by the stated
// tolerance without being reported.
func TestQuotaSplitTolerance(t *testing.T) {
	corpus := writeCorpus(t, map[string]string{"a.md": "正文一行\n"})
	questions := []dataset.Question{
		{ID: "q1", Question: "问题一", Answerable: true, Type: "single_fact", Topic: "go",
			Evidence: []dataset.EvidenceSet{{span("a.md", 1, 1)}}, Split: "dev"},
		{ID: "q2", Question: "问题二", Answerable: true, Type: "single_fact", Topic: "go",
			Evidence: []dataset.EvidenceSet{{span("a.md", 1, 1)}}, Split: "dev"},
	}
	meta := []Meta{
		{ID: "q1", Paraphrase: "verbatim", Lang: "zh_qa_zh_source"},
		{ID: "q2", Paraphrase: "verbatim", Lang: "zh_qa_zh_source"},
	}
	quota := Quota{}
	quota.Split = map[string]int{"dev": 2, "sealed": 1}
	quota.SplitTolerance = 1
	report, err := Validate(corpus, questions, meta, quota, DefaultParams())
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if rulesOf(report, SeverityWarning)[RuleQuotaSplit] != 0 {
		t.Errorf("a split inside the tolerance was reported: %v", report.Findings)
	}

	quota.SplitTolerance = 0
	report, err = Validate(corpus, questions, meta, quota, DefaultParams())
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if rulesOf(report, SeverityWarning)[RuleQuotaSplit] != 1 {
		t.Errorf("a split outside the tolerance was not reported: %v", report.Findings)
	}
}

// TestValidateWithoutAQuotaHasNoQuotaFindings checks that a run without a
// quota file does not claim that every type, topic and split is "not in the
// plan": without a plan there is nothing to compare against.
func TestValidateWithoutAQuotaHasNoQuotaFindings(t *testing.T) {
	corpus := writeCorpus(t, map[string]string{"a.md": "正文一行\n"})
	questions := []dataset.Question{
		{ID: "q1", Question: "问题一", Answerable: true, Type: "single_fact", Topic: "go",
			Evidence: []dataset.EvidenceSet{{span("a.md", 1, 1)}}, Split: "dev"},
	}
	report, err := Validate(corpus, questions, nil, Quota{}, DefaultParams())
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	for _, f := range report.Findings {
		if strings.HasPrefix(f.Rule, "quota.") {
			t.Errorf("a quota finding was reported without a quota: %s", f)
		}
	}
}

// TestHeadingInsideCodeFenceIsContent checks that a "#" line inside a fenced
// code block is code, not a heading.
//
// The product only looks for headings outside code fences, so a span that
// covers a code block has body text for the product. A line-by-line classifier
// called it a heading, and the span then looked like it had no body text at
// all, which rejected a perfectly good question.
func TestHeadingInsideCodeFenceIsContent(t *testing.T) {
	corpus := writeCorpus(t, map[string]string{
		"a.md": "```go\n// # 这不是标题，是注释\nfmt.Println(1)\n```\n",
	})
	questions := []dataset.Question{
		{ID: "q-code", Question: "代码块里做了什么", Answerable: true,
			Evidence: []dataset.EvidenceSet{{span("a.md", 2, 2)}}, Split: "dev"},
	}
	report, err := Validate(corpus, questions, nil, Quota{}, DefaultParams())
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if rulesOf(report, SeverityError)[RuleEvidenceNoContent] != 0 {
		t.Errorf("a line inside a code fence was treated as having no body text: %v", report.Findings)
	}
	if rulesOf(report, SeverityWarning)[RuleEvidenceHeading] != 0 {
		t.Errorf("a line inside a code fence was treated as a heading: %v", report.Findings)
	}
}

// TestThirdDuplicateIsCaught checks that a third question that is just as
// similar to the first one is reported as well.
//
// With a pair-wise comparison the first pair was marked as "seen" and the
// third question was never compared with anything, so it slipped through.
func TestThirdDuplicateIsCaught(t *testing.T) {
	corpus := writeCorpus(t, map[string]string{"a.md": "正文一行\n"})
	questions := []dataset.Question{
		{ID: "q1", Question: "Redis 的持久化方式有哪些？", Answerable: true,
			Evidence: []dataset.EvidenceSet{{span("a.md", 1, 1)}}, Split: "dev"},
		{ID: "q2", Question: "Redis 的持久化方式有哪些?", Answerable: true,
			Evidence: []dataset.EvidenceSet{{span("a.md", 1, 1)}}, Split: "dev"},
		{ID: "q3", Question: "Redis 的持久化方式有哪些！", Answerable: true,
			Evidence: []dataset.EvidenceSet{{span("a.md", 1, 1)}}, Split: "dev"},
	}
	report, err := Validate(corpus, questions, nil, Quota{}, DefaultParams())
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got := rulesOf(report, SeverityError)[RuleDuplicate]; got != 2 {
		t.Errorf("duplicate findings = %d, want 2 (the second and the third question): %v",
			got, report.Findings)
	}
}
