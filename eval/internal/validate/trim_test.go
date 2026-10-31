package validate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiaopang456/GopherLearn/eval/internal/dataset"
)

// loadDocs writes a corpus of documents and returns them keyed by file name.
func loadDocs(t *testing.T, files map[string]string) map[string]*Document {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "files")
	paths := map[string]string{}
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		paths[name] = path
	}
	docs, err := LoadDocuments(paths)
	if err != nil {
		t.Fatalf("load documents: %v", err)
	}
	return docs
}

// TestTrimCutsTheHeadingsAndBlanksOffTheEdges checks that the headings and
// the blank lines at the edges of an evidence span are cut off.
//
// A model that points at a range of lines tends to include the heading above
// the paragraph or the blank line after it. Those lines carry no content, so
// the program cuts them instead of sending the question back to the model.
func TestTrimCutsTheHeadingsAndBlanksOffTheEdges(t *testing.T) {
	docs := loadDocs(t, map[string]string{
		"a.md": "# 小节标题\n\n正文第一行。\n正文第二行。\n\n## 下一节\n",
	})
	questions := []dataset.Question{
		{ID: "q1", Question: "正文说了什么", Answerable: true, Type: "single_fact",
			Evidence: []dataset.EvidenceSet{{span("a.md", 1, 6)}}},
	}
	trimmed, records := TrimEvidence(questions, docs)
	got := trimmed[0].Evidence[0][0]
	if got.Start != 3 || got.End != 4 {
		t.Errorf("trimmed span = %d-%d, want 3-4 (the two body lines)", got.Start, got.End)
	}
	if len(records) != 1 {
		t.Fatalf("records = %d, want 1", len(records))
	}
	if records[0].BeforeStart != 1 || records[0].BeforeEnd != 6 {
		t.Errorf("record before = %d-%d, want 1-6", records[0].BeforeStart, records[0].BeforeEnd)
	}
	// Lines 1, 2, 5 and 6 were cut.
	if len(records[0].TrimmedLines) != 4 {
		t.Errorf("trimmed lines = %v, want four lines", records[0].TrimmedLines)
	}
}

// TestTrimLeavesTheMiddleAlone checks that a heading in the middle of a span
// is not cut away.
//
// The heading in the middle is a real signal: the validator warns about it,
// because a heading ties the evidence to the chunking. Trimming it silently
// would hide that.
func TestTrimLeavesTheMiddleAlone(t *testing.T) {
	docs := loadDocs(t, map[string]string{
		"a.md": "# 小节标题\n正文第一行。\n## 中间的标题\n正文第二行。\n",
	})
	questions := []dataset.Question{
		{ID: "q1", Question: "正文说了什么", Answerable: true, Type: "single_fact",
			Evidence: []dataset.EvidenceSet{{span("a.md", 1, 5)}}},
	}
	trimmed, _ := TrimEvidence(questions, docs)
	got := trimmed[0].Evidence[0][0]
	// Lines 1 (the heading) and 5 (the trailing blank) come off; the heading
	// in the middle (line 3) stays.
	if got.Start != 2 || got.End != 4 {
		t.Errorf("trimmed span = %d-%d, want 2-4 (the heading in the middle stays)", got.Start, got.End)
	}
}

// TestTrimKeepsASpanThatHoldsNothingButHeadings checks that a span with
// nothing but headings and blank lines is left as it was.
//
// Cutting it would produce an empty range, and the defect would disappear
// instead of being reported.
func TestTrimKeepsASpanThatHoldsNothingButHeadings(t *testing.T) {
	docs := loadDocs(t, map[string]string{
		"a.md": "# 小节标题\n\n## 下一节\n",
	})
	questions := []dataset.Question{
		{ID: "q1", Question: "标题是什么", Answerable: true, Type: "single_fact",
			Evidence: []dataset.EvidenceSet{{span("a.md", 1, 3)}}},
	}
	trimmed, records := TrimEvidence(questions, docs)
	got := trimmed[0].Evidence[0][0]
	if got.Start != 1 || got.End != 3 {
		t.Errorf("trimmed span = %d-%d, want 1-3 (the span must stay as it was)", got.Start, got.End)
	}
	if len(records) != 1 || !records[0].Empty {
		t.Errorf("records = %+v, want one empty record", records)
	}
}

// TestSimilarPairsAreListedForTheReviewer checks the band between "close" and
// "duplicate".
//
// Two questions that differ a little are not duplicates: one of them must not
// be dropped. But the pair is worth a human look, so it is listed.
func TestSimilarPairsAreListedForTheReviewer(t *testing.T) {
	questions := []dataset.Question{
		{ID: "q1", Question: "Redis 的过期删除策略有哪几种？"},
		{ID: "q2", Question: "Redis 的过期删除策略有几种？"},
		{ID: "q3", Question: "Go 的 map 为什么不是并发安全的？"},
	}
	pairs := FindSimilarPairs(questions, 0.6, 0.75)
	if len(pairs) != 1 {
		t.Fatalf("pairs = %d, want 1: %+v", len(pairs), pairs)
	}
	if pairs[0].A != "q1" || pairs[0].B != "q2" {
		t.Errorf("pair = %s-%s, want q1-q2", pairs[0].A, pairs[0].B)
	}
	if pairs[0].Similarity < 0.6 || pairs[0].Similarity >= 0.75 {
		t.Errorf("similarity = %.3f, want it in [0.60, 0.75)", pairs[0].Similarity)
	}
}

// TestDuplicatesAreNotListedAsSimilarPairs checks that the band and the
// duplicate check do not overlap.
func TestDuplicatesAreNotListedAsSimilarPairs(t *testing.T) {
	questions := []dataset.Question{
		{ID: "q1", Question: "Redis 的持久化方式有哪些？"},
		{ID: "q2", Question: "Redis 的持久化方式有哪些?"},
	}
	if pairs := FindSimilarPairs(questions, 0.6, 0.75); len(pairs) != 0 {
		t.Errorf("a duplicate was listed as a similar pair: %+v", pairs)
	}
}

// TestSimilarPairsAreReportedInTheRun checks that the run mentions the pairs,
// so a reviewer knows the list exists.
func TestSimilarPairsAreReportedInTheRun(t *testing.T) {
	corpus := writeCorpus(t, map[string]string{"a.md": "正文一行\n"})
	questions := []dataset.Question{
		{ID: "q1", Question: "Redis 的过期删除策略有哪几种？", Answerable: true, Type: "single_fact",
			Evidence: []dataset.EvidenceSet{{span("a.md", 1, 1)}}, Split: "dev"},
		{ID: "q2", Question: "Redis 的过期删除策略有几种？", Answerable: true, Type: "single_fact",
			Evidence: []dataset.EvidenceSet{{span("a.md", 1, 1)}}, Split: "dev"},
	}
	report, err := Validate(corpus, questions, nil, Quota{}, DefaultParams())
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got := rulesOf(report, SeverityWarning)[RuleSimilarPair]; got != 1 {
		t.Errorf("similar pair findings = %d, want 1: %v", got, report.Findings)
	}
	if !report.OK() {
		t.Errorf("the run failed over a pair that is only similar: %v", report.Findings)
	}
}

// TestModelAndParametersMustBeTheSame checks that a set whose questions were
// not all produced the same way is rejected.
//
// Every question records the model and the generation parameters it came
// from. A set that mixes them cannot be reproduced and its questions cannot
// be compared with each other.
func TestModelAndParametersMustBeTheSame(t *testing.T) {
	corpus := writeCorpus(t, map[string]string{"a.md": "正文一行\n"})
	questions := []dataset.Question{
		{ID: "q1", Question: "问题一", Answerable: true, Type: "single_fact", Topic: "go",
			Evidence: []dataset.EvidenceSet{{span("a.md", 1, 1)}}, Split: "dev"},
		{ID: "q2", Question: "问题二", Answerable: true, Type: "single_fact", Topic: "go",
			Evidence: []dataset.EvidenceSet{{span("a.md", 1, 1)}}, Split: "dev"},
	}
	meta := []Meta{
		{ID: "q1", Paraphrase: "verbatim", Lang: "zh_qa_zh_source",
			Model: "qwen3.8-max", Params: GenParams{Temperature: 0.7, TopP: 0.8, MaxTokens: 4096, Seed: 20261031}},
		{ID: "q2", Paraphrase: "verbatim", Lang: "zh_qa_zh_source",
			Model: "qwen3.8-max", Params: GenParams{Temperature: 0.7, TopP: 0.8, MaxTokens: 4096, Seed: 20261031}},
	}
	report, err := Validate(corpus, questions, meta, Quota{}, DefaultParams())
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got := rulesOf(report, SeverityError)[RuleMetaModelMismatch]; got != 0 {
		t.Errorf("a consistent set was rejected: %v", report.Findings)
	}
	if got := rulesOf(report, SeverityWarning)[RuleMetaModelMissing]; got != 0 {
		t.Errorf("a set that records the model was reported as missing it: %v", report.Findings)
	}

	// A different seed is a different generation and has to be reported.
	meta[1].Params.Seed = 7
	report, err = Validate(corpus, questions, meta, Quota{}, DefaultParams())
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got := rulesOf(report, SeverityError)[RuleMetaModelMismatch]; got != 1 {
		t.Errorf("a mixed set was accepted: %v", report.Findings)
	}
}

// TestMissingModelIsOnlyAWarning checks that a question without a recorded
// model is reported but does not fail the run.
func TestMissingModelIsOnlyAWarning(t *testing.T) {
	corpus := writeCorpus(t, map[string]string{"a.md": "正文一行\n"})
	questions := []dataset.Question{
		{ID: "q1", Question: "问题一", Answerable: true, Type: "single_fact", Topic: "go",
			Evidence: []dataset.EvidenceSet{{span("a.md", 1, 1)}}, Split: "dev"},
	}
	meta := []Meta{{ID: "q1", Paraphrase: "verbatim", Lang: "zh_qa_zh_source"}}
	report, err := Validate(corpus, questions, meta, Quota{}, DefaultParams())
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got := rulesOf(report, SeverityWarning)[RuleMetaModelMissing]; got != 1 {
		t.Errorf("missing model findings = %d, want 1: %v", got, report.Findings)
	}
	if !report.OK() {
		t.Errorf("a missing model failed the run: %v", report.Findings)
	}
}

// TestLostParametersAreMissingNotAMismatch checks that a question which
// records the model but loses its parameters is reported as missing, not as
// "produced in a different way".
//
// The second combination would be "the model with zero parameters", which is
// not a different way of producing a question, it is a question whose
// parameters were lost on the way.
func TestLostParametersAreMissingNotAMismatch(t *testing.T) {
	corpus := writeCorpus(t, map[string]string{"a.md": "正文一行\n"})
	questions := []dataset.Question{
		{ID: "q1", Question: "问题一", Answerable: true, Type: "single_fact", Topic: "go",
			Evidence: []dataset.EvidenceSet{{span("a.md", 1, 1)}}, Split: "dev"},
		{ID: "q2", Question: "问题二", Answerable: true, Type: "single_fact", Topic: "go",
			Evidence: []dataset.EvidenceSet{{span("a.md", 1, 1)}}, Split: "dev"},
	}
	meta := []Meta{
		{ID: "q1", Paraphrase: "verbatim", Lang: "zh_qa_zh_source",
			Model: "qwen3.8-max", Params: GenParams{Temperature: 0.7, TopP: 0.8, MaxTokens: 4096, Seed: 20261031, N: 1}},
		// The model survived, the parameters did not.
		{ID: "q2", Paraphrase: "verbatim", Lang: "zh_qa_zh_source", Model: "qwen3.8-max"},
	}
	report, err := Validate(corpus, questions, meta, Quota{}, DefaultParams())
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got := rulesOf(report, SeverityError)[RuleMetaModelMismatch]; got != 0 {
		t.Errorf("lost parameters were reported as a mismatch: %v", report.Findings)
	}
	if got := rulesOf(report, SeverityWarning)[RuleMetaModelMissing]; got != 1 {
		t.Errorf("lost parameters were not reported as missing: %v", report.Findings)
	}
}

// TestTrimRecordsAreWritten checks that the trim log is written as JSONL.
func TestTrimRecordsAreWritten(t *testing.T) {
	records := []TrimRecord{
		{ID: "q1", File: "a.md", BeforeStart: 1, BeforeEnd: 4, AfterStart: 2, AfterEnd: 3, TrimmedLines: []int{1, 4}},
	}
	path := filepath.Join(t.TempDir(), "trim.jsonl")
	if err := WriteTrimRecords(path, records); err != nil {
		t.Fatalf("write records: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read records: %v", err)
	}
	if !strings.Contains(string(raw), `"id":"q1"`) || !strings.Contains(string(raw), `"trimmed_lines":[1,4]`) {
		t.Errorf("trim log = %s, want the id and the cut lines", raw)
	}
}
