package validate

import "testing"

func TestMetaEnums(t *testing.T) {
	tests := []struct {
		para, lang string
		want       int
	}{
		{"verbatim", "zh_qa_zh_source", 0}, {"rephrased", "zh_qa_en_source", 0}, {"colloquial", "zh_qa_zh_source", 0},
		{"a rewritten question?", "zh_qa_zh_source", 1}, {"", "zh_qa_en_source", 1},
		{"verbatim", "english", 1}, {"verbatim", "", 1}, {"bad", "bad", 2},
	}
	for _, tc := range tests {
		r := &Report{}
		checkMetaEnums(r, "q", Meta{Paraphrase: tc.para, Lang: tc.lang})
		if len(r.Findings) != tc.want {
			t.Fatalf("%q/%q: got %d findings, want %d", tc.para, tc.lang, len(r.Findings), tc.want)
		}
		for _, f := range r.Findings {
			if f.Severity != SeverityError || f.ID != "q" {
				t.Fatalf("unexpected finding: %+v", f)
			}
		}
	}
}
