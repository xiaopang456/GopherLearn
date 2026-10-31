package validate

import (
	"encoding/json"
	"testing"

	"github.com/xiaopang456/GopherLearn/eval/internal/dataset"
)

func TestWhyEachNeededPresence(t *testing.T) {
	q := dataset.Question{ID: "q", Type: "multi_evidence"}
	for _, tc := range []struct {
		name string
		m    Meta
		want int
	}{
		{"absent", Meta{}, 1},
		{"empty", Meta{WhyEachNeeded: []string{"  ", ""}}, 1},
		{"present", Meta{WhyEachNeeded: []string{"the first span supplies a missing detail"}}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &Report{}
			checkWhyEachNeeded(r, q, tc.m)
			if len(r.Findings) != tc.want {
				t.Fatalf("got %d findings, want %d", len(r.Findings), tc.want)
			}
		})
	}
	var m Meta
	if err := json.Unmarshal([]byte(`{"id":"q","why_each_needed":["first","second"]}`), &m); err != nil || len(m.WhyEachNeeded) != 2 {
		t.Fatalf("sidecar parsing: %+v %v", m, err)
	}
	r := &Report{}
	checkWhyEachNeeded(r, dataset.Question{Type: "single_fact"}, Meta{})
	if len(r.Findings) != 0 {
		t.Fatal("single-fact question requires no explanations")
	}
}

func TestChineseQuestionLanguage(t *testing.T) {
	for _, tc := range []struct {
		text, lang string
		want       int
	}{
		{"Redis 的 AOF 每秒刷盘有什么风险？", "zh_qa_en_source", 0},
		{"在 ALFWorld 实验中 ReAct 的成功率是多少？", "zh_qa_en_source", 0},
		{"What is the Redis AOF default?", "zh_qa_en_source", 1},
		{"在 Redis what is the default size?", "zh_qa_en_source", 1},
		{"What is the default?", "en_qa_en_source", 0},
	} {
		r := &Report{}
		checkQuestionLanguage(r, dataset.Question{ID: "q", Question: tc.text}, Meta{Lang: tc.lang})
		if len(r.Findings) != tc.want {
			t.Errorf("%q: got %d, want %d", tc.text, len(r.Findings), tc.want)
		}
	}
}

func TestKeywordsPerAlternativeEvidenceSet(t *testing.T) {
	corpus := writeCorpus(t, map[string]string{"a.md": "写时复制\n", "b.md": "copy-on-write\n", "c.md": "unrelated\n"})
	q := dataset.Question{ID: "q", Question: "使用哪种机制？", Answerable: true, Split: "dev", Evidence: []dataset.EvidenceSet{{span("a.md", 1, 1)}, {span("b.md", 1, 1)}}}
	m := Meta{ID: "q", Author: "e4", Points: []Point{{Text: "写时复制", Keywords: []string{"写时复制", "copy-on-write"}}}}
	r, err := Validate(corpus, []dataset.Question{q}, []Meta{m}, Quota{}, DefaultParams())
	if err != nil {
		t.Fatal(err)
	}
	if rulesOf(r, SeverityError)[RuleNuggetMissing] != 0 {
		t.Fatalf("bilingual alternatives rejected: %v", r.Findings)
	}
	if rulesOf(r, SeverityWarning)[RuleMetaModelMissing] != 0 {
		t.Fatal("manual question requires model parameters")
	}
	q.Evidence = append(q.Evidence, dataset.EvidenceSet{span("c.md", 1, 1)})
	r, err = Validate(corpus, []dataset.Question{q}, []Meta{m}, Quota{}, DefaultParams())
	if err != nil {
		t.Fatal(err)
	}
	if rulesOf(r, SeverityError)[RuleNuggetMissing] != 1 {
		t.Fatalf("missing alternative group not caught: %v", r.Findings)
	}
}
