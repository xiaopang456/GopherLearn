package validate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiaopang456/GopherLearn/eval/internal/dataset"
)

// TestMultiEvidenceQuestionWithTwoQuestionMarksIsReported checks that a
// multi-evidence question which strings several small questions together is
// reported.
//
// A multi-evidence question is ONE question that needs two places to answer.
// Three small questions joined with question marks are three questions, and
// scoring them against one evidence set is muddy.
func TestMultiEvidenceQuestionWithTwoQuestionMarksIsReported(t *testing.T) {
	corpus := writeCorpus(t, map[string]string{"a.md": "正文一行\n第二行\n"})
	questions := []dataset.Question{
		{ID: "q1", Question: "如果 T1 持有 X 锁，T2 再请求会怎样？意向锁是什么级别？它指示什么？",
			Answerable: true, Type: "multi_evidence", Topic: "a",
			Evidence: []dataset.EvidenceSet{{span("a.md", 1, 2)}}, Split: "dev"},
	}
	report, err := Validate(corpus, questions, nil, Quota{}, DefaultParams())
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	// It is an error, not a warning: the question is dropped and counted
	// against the output rate.
	if got := rulesOf(report, SeverityError)[RuleMultiQuestion]; got != 1 {
		t.Errorf("multi-question findings = %d, want 1: %v", got, report.Findings)
	}
}

// TestMultiEvidenceQuestionWithOneQuestionMarkIsFine checks that a proper
// multi-evidence question is not reported.
func TestMultiEvidenceQuestionWithOneQuestionMarkIsFine(t *testing.T) {
	corpus := writeCorpus(t, map[string]string{"a.md": "正文一行\n第二行\n"})
	questions := []dataset.Question{
		{ID: "q1", Question: "如果 T1 持有 X 锁，T2 再请求会怎样，需要看哪两处说明？",
			Answerable: true, Type: "multi_evidence", Topic: "a",
			Evidence: []dataset.EvidenceSet{{span("a.md", 1, 2)}}, Split: "dev"},
	}
	report, err := Validate(corpus, questions, nil, Quota{}, DefaultParams())
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got := rulesOf(report, SeverityError)[RuleMultiQuestion]; got != 0 {
		t.Errorf("a proper multi-evidence question was reported: %v", report.Findings)
	}
}

// TestCrossDocQuestionNeedsTwoDocuments checks that a cross-document question
// whose evidence only reaches one document is an error.
//
// The point of such a question is that it cannot be answered from one place.
func TestCrossDocQuestionNeedsTwoDocuments(t *testing.T) {
	corpus := writeCorpus(t, map[string]string{
		"a.md": "正文一行\n第二行\n",
		"b.md": "别处的正文\n",
	})
	questions := []dataset.Question{
		{ID: "q1", Question: "两处说法分别是什么？", Answerable: true, Type: "multi_evidence", Topic: "a",
			Evidence: []dataset.EvidenceSet{{span("a.md", 1, 2)}}, Split: "dev"},
	}
	meta := []Meta{{ID: "q1", Paraphrase: "verbatim", Lang: "zh_qa_zh_source",
		Params: GenParams{BatchKind: BatchKindCrossDoc}}}
	report, err := Validate(corpus, questions, meta, Quota{}, DefaultParams())
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got := rulesOf(report, SeverityError)[RuleCrossDocEvidence]; got != 1 {
		t.Errorf("cross-doc findings = %d, want 1: %v", got, report.Findings)
	}

	// With two documents it passes.
	questions[0].Evidence = []dataset.EvidenceSet{{span("a.md", 1, 2), span("b.md", 1, 1)}}
	report, err = Validate(corpus, questions, meta, Quota{}, DefaultParams())
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got := rulesOf(report, SeverityError)[RuleCrossDocEvidence]; got != 0 {
		t.Errorf("a question with two documents was reported: %v", report.Findings)
	}
}

// TestBatchKindsMayDifferInTheirParameters checks that the fingerprint is
// compared within a batch kind.
//
// The multi-evidence and cross-document batches think longer than the
// ordinary ones, so the kinds legitimately differ; what must not happen is
// one kind disagreeing with itself.
func TestBatchKindsMayDifferInTheirParameters(t *testing.T) {
	corpus := writeCorpus(t, map[string]string{"a.md": "正文一行\n"})
	questions := []dataset.Question{
		{ID: "q1", Question: "问题一", Answerable: true, Type: "single_fact", Topic: "a",
			Evidence: []dataset.EvidenceSet{{span("a.md", 1, 1)}}, Split: "dev"},
		{ID: "q2", Question: "问题二", Answerable: true, Type: "multi_evidence", Topic: "a",
			Evidence: []dataset.EvidenceSet{{span("a.md", 1, 1)}}, Split: "dev"},
	}
	thinking := true
	meta := []Meta{
		{ID: "q1", Paraphrase: "verbatim", Lang: "zh_qa_zh_source", Model: "qwen3.8-max",
			Params: GenParams{Temperature: 0.7, TopP: 0.8, MaxTokens: 4096, Seed: 1, N: 1,
				EnableThinking: &thinking, ThinkingBudget: 2048, BatchKind: BatchKindDefault}},
		{ID: "q2", Paraphrase: "verbatim", Lang: "zh_qa_zh_source", Model: "qwen3.8-max",
			Params: GenParams{Temperature: 0.7, TopP: 0.8, MaxTokens: 4096, Seed: 1, N: 1,
				EnableThinking: &thinking, ThinkingBudget: 4096, BatchKind: BatchKindMultiEvidence}},
	}
	report, err := Validate(corpus, questions, meta, Quota{}, DefaultParams())
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got := rulesOf(report, SeverityError)[RuleMetaModelMismatch]; got != 0 {
		t.Errorf("two kinds with different settings were reported: %v", report.Findings)
	}

	// Two questions of the same kind with different settings are a defect.
	meta[1].Params.BatchKind = BatchKindDefault
	report, err = Validate(corpus, questions, meta, Quota{}, DefaultParams())
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got := rulesOf(report, SeverityError)[RuleMetaModelMismatch]; got != 1 {
		t.Errorf("one kind disagreeing with itself was not reported: %v", report.Findings)
	}
}

// TestUnanswerableQuestionThatNamesTheCorpusIsReported checks that an
// unanswerable question naming a corpus file is reported.
//
// A real user does not know how the corpus is cut up, and a question that
// names the file also hands the answerer the place to look.
func TestUnanswerableQuestionThatNamesTheCorpusIsReported(t *testing.T) {
	corpus := writeCorpus(t, map[string]string{
		"notes/gopherai-rag.md": "正文一行\n",
	})
	questions := []dataset.Question{
		{ID: "q1", Question: "在 notes/gopherai-rag.md 的 Content、Raw 部分，段落通常 Content == Raw；带 BOM 的文件呢？",
			Answerable: false, Type: "unanswerable", Topic: "a", Split: "dev"},
	}
	report, err := Validate(corpus, questions, nil, Quota{}, DefaultParams())
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got := rulesOf(report, SeverityWarning)[RuleUnanswerableMetaLeak]; got != 1 {
		t.Errorf("meta-leak findings = %d, want 1: %v", got, report.Findings)
	}
}

// TestUnanswerableQuestionThatSoundsLikeAUserIsFine checks that a question
// written the way a user would write it is not reported.
func TestUnanswerableQuestionThatSoundsLikeAUserIsFine(t *testing.T) {
	corpus := writeCorpus(t, map[string]string{
		"notes/gopherai-rag.md": "正文一行\n",
	})
	questions := []dataset.Question{
		{ID: "q1", Question: "Redis 的列表最多允许保存多少个元素？",
			Answerable: false, Type: "unanswerable", Topic: "a", Split: "dev"},
	}
	report, err := Validate(corpus, questions, nil, Quota{}, DefaultParams())
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got := rulesOf(report, SeverityWarning)[RuleUnanswerableMetaLeak]; got != 0 {
		t.Errorf("a question that sounds like a user was reported: %v", report.Findings)
	}
}

// TestExcludedTopicNearbyEvidenceIsReported checks that a question whose
// evidence lands next to an excluded knowledge point is reported.
func TestExcludedTopicNearbyEvidenceIsReported(t *testing.T) {
	corpus := writeCorpus(t, map[string]string{
		"notes/redis.md": "第一行\n第二行\n第三行\n第四行\n第五行\n第六行\n第七行\n第八行\n第九行\n第十行\n第十一行\n",
	})
	questions := []dataset.Question{
		{ID: "q1", Question: "这一段讲了什么？", Answerable: true, Type: "single_fact", Topic: "a",
			Evidence: []dataset.EvidenceSet{{span("notes/redis.md", 10, 11)}}, Split: "dev"},
	}
	params := DefaultParams()
	params.Excluded = ExcludedTopics{{
		File: "notes/redis.md", Line: 5, Radius: 10,
		Terms: []string{"单线程", "多线程"}, Name: "Redis 6 的 I/O 线程",
	}}
	report, err := Validate(corpus, questions, nil, Quota{}, params)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got := rulesOf(report, SeverityWarning)[RuleExcludedTopicNear]; got != 1 {
		t.Errorf("excluded-topic findings = %d, want 1: %v", got, report.Findings)
	}
}

// TestExcludedTopicNamedInTheQuestionIsReported checks the second way to
// reach an excluded topic: naming all of its words, wherever the evidence
// points.
func TestExcludedTopicNamedInTheQuestionIsReported(t *testing.T) {
	corpus := writeCorpus(t, map[string]string{
		"notes/redis.md": "第一行\n第二行\n",
		"other.md":       "别处的正文\n",
	})
	questions := []dataset.Question{
		{ID: "q1", Question: "Redis 到底是单线程还是多线程？", Answerable: true, Type: "single_fact", Topic: "a",
			Evidence: []dataset.EvidenceSet{{span("other.md", 1, 1)}}, Split: "dev"},
	}
	params := DefaultParams()
	params.Excluded = ExcludedTopics{{
		File: "notes/redis.md", Line: 5, Radius: 10,
		Terms: []string{"单线程", "多线程"}, Name: "Redis 6 的 I/O 线程",
	}}
	report, err := Validate(corpus, questions, nil, Quota{}, params)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got := rulesOf(report, SeverityWarning)[RuleExcludedTopicNear]; got != 1 {
		t.Errorf("excluded-topic findings = %d, want 1: %v", got, report.Findings)
	}
}

// TestExcludedTopicFarAwayIsFine checks that a question far from the
// excluded line and not naming its words is not reported.
func TestExcludedTopicFarAwayIsFine(t *testing.T) {
	corpus := writeCorpus(t, map[string]string{
		"notes/redis.md": "第一行\n",
		"other.md":       "别处的正文\n",
	})
	questions := []dataset.Question{
		{ID: "q1", Question: "Go 的 map 扩容是怎么做的？", Answerable: true, Type: "single_fact", Topic: "a",
			Evidence: []dataset.EvidenceSet{{span("other.md", 1, 1)}}, Split: "dev"},
	}
	params := DefaultParams()
	params.Excluded = ExcludedTopics{{
		File: "notes/redis.md", Line: 5, Radius: 10,
		Terms: []string{"单线程", "多线程"}, Name: "Redis 6 的 I/O 线程",
	}}
	report, err := Validate(corpus, questions, nil, Quota{}, params)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got := rulesOf(report, SeverityWarning)[RuleExcludedTopicNear]; got != 0 {
		t.Errorf("a question far from the excluded topic was reported: %v", report.Findings)
	}
}

// TestAlternativesFindAnotherPlace checks the candidate search.
//
// The point of an alternative evidence set is that the same question can be
// answered from two places, so the candidate has to come from a document the
// question does not already use.
func TestAlternativesFindAnotherPlace(t *testing.T) {
	docs := loadDocs(t, map[string]string{
		"notes/a.md": "Redis 的 AOF 是追加日志。\n别的话。\n",
		"docs/b.md":  "Redis 的 AOF 是追加日志，用于持久化。\n别的内容。\n",
	})
	questions := []dataset.Question{
		{ID: "q1", Question: "AOF 是什么？", Answerable: true, Type: "single_fact",
			Evidence: []dataset.EvidenceSet{{span("notes/a.md", 1, 1)}}},
	}
	meta := []Meta{{ID: "q1", Points: []Point{{Text: "AOF 是追加日志", Keywords: []string{"AOF 是追加日志"}}}}}
	alts := FindAlternatives(questions[0], meta[0], docs, 5)
	if len(alts) != 1 {
		t.Fatalf("alternatives = %d, want 1: %+v", len(alts), alts)
	}
	if alts[0].File != "docs/b.md" {
		t.Errorf("alternative file = %q, want docs/b.md", alts[0].File)
	}
}

// TestAlternativesSkipTheFilesTheQuestionUses checks that a candidate from a
// document the question already uses is not offered.
func TestAlternativesSkipTheFilesTheQuestionUses(t *testing.T) {
	docs := loadDocs(t, map[string]string{
		"notes/a.md": "Redis 的 AOF 是追加日志。\n第二行也是。\n",
	})
	questions := []dataset.Question{
		{ID: "q1", Question: "AOF 是什么？", Answerable: true, Type: "single_fact",
			Evidence: []dataset.EvidenceSet{{span("notes/a.md", 1, 1)}}},
	}
	meta := []Meta{{ID: "q1", Points: []Point{{Text: "AOF", Keywords: []string{"AOF 是追加日志"}}}}}
	if alts := FindAlternatives(questions[0], meta[0], docs, 5); len(alts) != 0 {
		t.Errorf("a candidate from the same document was offered: %+v", alts)
	}
}

// TestThinkingParametersAreInTheFingerprint checks that the thinking settings
// are part of the question set's fingerprint.
//
// They decide how long the model thinks before it answers, so two question
// sets produced with different thinking budgets are not the same set.
func TestThinkingParametersAreInTheFingerprint(t *testing.T) {
	thinking := true
	off := false
	a := GenParams{Temperature: 0.7, TopP: 0.8, MaxTokens: 4096, Seed: 1, N: 1,
		EnableThinking: &thinking, ThinkingBudget: 2048}
	b := GenParams{Temperature: 0.7, TopP: 0.8, MaxTokens: 4096, Seed: 1, N: 1,
		EnableThinking: &off, ThinkingBudget: 2048}
	c := GenParams{Temperature: 0.7, TopP: 0.8, MaxTokens: 4096, Seed: 1, N: 1,
		EnableThinking: &thinking, ThinkingBudget: 8192}
	if a.Fingerprint("qwen3.8-max") == b.Fingerprint("qwen3.8-max") {
		t.Error("turning the thinking off did not change the fingerprint")
	}
	if a.Fingerprint("qwen3.8-max") == c.Fingerprint("qwen3.8-max") {
		t.Error("a different thinking budget did not change the fingerprint")
	}
	if a.Fingerprint("qwen3.8-max") == a.Fingerprint("qwen3.7-max") {
		t.Error("a different model did not change the fingerprint")
	}
	if !strings.Contains(a.Fingerprint("qwen3.8-max"), "budget=2048") {
		t.Errorf("the fingerprint does not carry the thinking budget: %s", a.Fingerprint("qwen3.8-max"))
	}
}

// TestExcludedTopicsAreLoadedFromJSON checks the file format.
func TestExcludedTopicsAreLoadedFromJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "excluded.json")
	content := `[{"file":"notes/redis.md","line":71,"radius":10,"terms":["单线程","多线程"],"name":"Redis 6 的 I/O 线程"}]`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	topics, err := LoadExcludedTopics(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(topics) != 1 || topics[0].Line != 71 || len(topics[0].Terms) != 2 {
		t.Errorf("loaded topics = %+v, want one topic with line 71 and two terms", topics)
	}
}
