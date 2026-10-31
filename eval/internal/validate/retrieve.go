package validate

import (
	"fmt"
	"sort"
	"strings"

	"github.com/xiaopang456/GopherLearn/eval/internal/dataset"
)

// Retrieved is one place in the corpus that might answer a question.
//
// It is what the harness's own keyword channel would find for the question:
// the same corpus, the same terms, no model involved. The reviewer reads these
// and decides whether the corpus knows the answer.
type Retrieved struct {
	ID string `json:"id"`
	// File and the line range of the passage.
	File  string `json:"file"`
	Start int    `json:"start"`
	End   int    `json:"end"`
	// Matched are the terms of the question that the passage contains.
	Matched []string `json:"matched"`
	// Score ranks the passages: more terms, and longer terms, rank higher.
	Score int `json:"score"`
	// Question is the question the passage was retrieved for.
	Question string `json:"question"`
	// Answerable is the reviewer's verdict. It is empty in the output file:
	// the tool does not judge, it hands the passages over.
	Answerable string `json:"answerable,omitempty"`
}

// RetrieveForQuestion returns the passages that best match the question.
//
// The terms come from the question itself: the ASCII identifiers and the
// Chinese word groups the product's own extractor produces. Only the terms
// that are rare enough in the corpus count, so a passage that happens to
// contain "the" or "可以" does not come out on top.
//
// This is the **keyword channel** of the harness, not the vector one: it
// needs no embedding service and no model, so it can be run on every
// question as often as needed.
func RetrieveForQuestion(q dataset.Question, docs map[string]*Document, limit int) []Retrieved {
	if limit <= 0 {
		limit = 8
	}
	terms := retrieveTerms(q.Question)
	if len(terms) == 0 {
		return nil
	}
	distinctive := distinctiveKeywords(terms, docs, maxKeywordDocFreq, maxKeywordDocuments)
	if len(distinctive) == 0 {
		return nil
	}
	var out []Retrieved
	for file, doc := range docs {
		for n := 1; n <= len(doc.Lines); n++ {
			line := strings.ToLower(doc.Line(n))
			if strings.TrimSpace(line) == "" {
				continue
			}
			var matched []string
			score := 0
			for _, term := range distinctive {
				if strings.Contains(line, term) {
					matched = append(matched, term)
					score += len([]rune(term))
				}
			}
			if len(matched) == 0 {
				continue
			}
			out = append(out, Retrieved{
				ID: q.ID, File: file, Start: n, End: n,
				Matched: matched, Score: score, Question: q.Question,
			})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		if len(out[i].Matched) != len(out[j].Matched) {
			return len(out[i].Matched) > len(out[j].Matched)
		}
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		return out[i].Start < out[j].Start
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// retrieveTerms pulls the search terms out of a question.
//
// The ASCII identifiers and the Chinese word groups of at least two characters
// are what a question is *about*. The question words ("是什么"、"怎么"…) are
// dropped, they are everywhere in the corpus.
func retrieveTerms(question string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(term string) {
		term = strings.TrimSpace(term)
		if term == "" || seen[strings.ToLower(term)] {
			return
		}
		seen[strings.ToLower(term)] = true
		out = append(out, strings.ToLower(term))
	}
	for _, word := range questionWords {
		question = strings.ReplaceAll(question, word, " ")
	}
	for _, field := range strings.FieldsFunc(question, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			r >= 0x4e00 && r <= 0x9fff || r == '_' || r == '.' || r == '-' || r == '/')
	}) {
		add(field)
	}
	return out
}

// questionWords are the words that carry no signal about what a question is
// about. The product's own extractor drops these too.
var questionWords = []string{
	"是什么", "为什么", "有什么", "怎么样", "怎么做", "请问", "哪些", "什么",
	"如何", "是否", "能不能", "可不可以", "这个", "那个", "它们", "它", "分别",
	"其中", "里面", "的话", "时候", "问题", "以及", "还是", "就是", "并且", "同时",
	"可以", "不能", "需要", "使用", "会不会", "会", "的", "了", "吗", "呢", "和", "与",
	// The interrogatives that survive the first list: they are everywhere in
	// the corpus and would pull every passage with a question mark onto the
	// list.
	"哪种", "哪个", "哪些个", "多少", "怎么", "怎样", "如何样", "哪些方面",
	"？", "?", "。", "，", "、", "：", "；",
}

// FormatRetrieved renders the retrieved passages for the terminal.
func FormatRetrieved(items []Retrieved) string {
	if len(items) == 0 {
		return "  （没检索到任何段落）\n"
	}
	out := ""
	for _, r := range items {
		out += fmt.Sprintf("  %-8s %-46s %d-%d  [%s]\n",
			r.ID, r.File, r.Start, r.End, strings.Join(r.Matched, "、"))
	}
	return out
}
