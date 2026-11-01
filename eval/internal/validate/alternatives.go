package validate

import (
	"fmt"
	"sort"
	"strings"

	"github.com/xiaopang456/GopherLearn/eval/internal/dataset"
)

// Alternative is a place in the corpus that could serve as a second evidence
// set for a question.
//
// The candidate has to come from a **different document** than the evidence
// the question already has: the point of an alternative evidence set is that
// the same question can be answered from two places, and two places in the
// same file are not two places.
type Alternative struct {
	ID string `json:"id"`
	// File and the line range of the candidate.
	File  string `json:"file"`
	Start int    `json:"start"`
	End   int    `json:"end"`
	// Matched are the required-point keywords the candidate contains.
	Matched []string `json:"matched"`
	// Score counts the matched keywords, longer keywords first.
	Score int `json:"score"`
	// Question is the question the candidate was found for.
	Question string `json:"question"`
}

// FindAlternatives searches the whole corpus for a second place that answers
// the same question.
//
// The search uses the question's own required-point keywords: they are the
// words that have to appear in the evidence for the answer to be checkable,
// so a line that contains them is a place where the answer can be read.
//
// Only the **distinctive** keywords are searched for. A keyword that turns up
// in hundreds of corpus lines ("read", "session", "客户端") says nothing about
// where the answer is, and matching it would drown the real candidates in
// noise.
//
// This is a **candidate list for the reviewer**, not a verdict: the tool says
// "this other paragraph contains the keywords too", and the reviewer decides
// whether it really answers the question.
func FindAlternatives(q dataset.Question, m Meta, docs map[string]*Document, limit int) []Alternative {
	if limit <= 0 {
		limit = 5
	}
	keywords := alternativeKeywords(m)
	if len(keywords) == 0 {
		return nil
	}
	// The files the question already uses: a second set has to come from
	// somewhere else.
	used := map[string]bool{}
	for _, set := range q.Evidence {
		for _, span := range set {
			used[span.File] = true
		}
	}
	// Keep only the keywords that are rare enough to mean something.
	distinctive := distinctiveKeywords(keywords, docs, maxKeywordDocFreq, maxKeywordDocuments)
	if len(distinctive) == 0 {
		return nil
	}
	var out []Alternative
	for file, doc := range docs {
		if used[file] {
			continue
		}
		for n := 1; n <= len(doc.Lines); n++ {
			line := strings.ToLower(doc.Line(n))
			if strings.TrimSpace(line) == "" {
				continue
			}
			var matched []string
			score := 0
			for _, keyword := range distinctive {
				if strings.Contains(line, keyword) {
					matched = append(matched, keyword)
					score += len([]rune(keyword))
				}
			}
			if len(matched) == 0 {
				continue
			}
			out = append(out, Alternative{
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
	// One file per question should not fill the whole list: the point of an
	// alternative evidence set is that the same question can be answered from
	// a *different* place, and a reviewer wants to see which other files
	// could hold it.
	perFile := map[string]int{}
	var kept []Alternative
	for _, candidate := range out {
		if perFile[candidate.File] >= maxAlternativesPerFile {
			continue
		}
		perFile[candidate.File]++
		kept = append(kept, candidate)
	}
	out = kept
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// maxKeywordDocFreq is how many corpus lines a keyword may show up in and
// still count as distinctive. The same limit the unanswerable check uses.
const maxKeywordDocFreq = 120

// maxKeywordDocuments is how many different documents a keyword may show up
// in and still count as distinctive.
//
// A word that turns up all over the corpus ("read", "session", "release")
// says nothing about where the answer is. A technical term stays inside the
// handful of documents that talk about its topic, which is exactly where an
// alternative answer would be.
const maxKeywordDocuments = 6

// maxAlternativesPerFile is how many candidates one document may contribute to
// one question.
const maxAlternativesPerFile = 2

// distinctiveKeywords drops the keywords that are everywhere in the corpus.
func distinctiveKeywords(keywords []string, docs map[string]*Document, maxFreq int, maxDocs int) []string {
	lineFreq := map[string]int{}
	docFreq := map[string]map[string]bool{}
	for _, keyword := range keywords {
		lowered := strings.ToLower(keyword)
		if lowered == "" {
			continue
		}
		for file, doc := range docs {
			for n := 1; n <= len(doc.Lines); n++ {
				if !strings.Contains(strings.ToLower(doc.Line(n)), lowered) {
					continue
				}
				lineFreq[keyword]++
				if docFreq[keyword] == nil {
					docFreq[keyword] = map[string]bool{}
				}
				docFreq[keyword][file] = true
			}
		}
	}
	var out []string
	for _, keyword := range keywords {
		if lineFreq[keyword] == 0 || lineFreq[keyword] > maxFreq {
			continue
		}
		if len(docFreq[keyword]) > maxDocs {
			continue
		}
		out = append(out, strings.ToLower(keyword))
	}
	return out
}

// alternativeKeywords collects the keywords to search for.
//
// The required-point keywords come first: they are what the answer has to
// contain. The question's own words are added as a fallback, so a question
// without required points still gets candidates.
func alternativeKeywords(m Meta) []string {
	seen := map[string]bool{}
	var out []string
	add := func(keyword string) {
		keyword = strings.TrimSpace(keyword)
		if keyword == "" || seen[keyword] {
			return
		}
		seen[keyword] = true
		out = append(out, keyword)
	}
	for _, point := range m.Points {
		for _, keyword := range point.Keywords {
			add(keyword)
		}
	}
	return out
}

// FormatAlternatives renders the candidate list for the terminal.
func FormatAlternatives(alts []Alternative) string {
	if len(alts) == 0 {
		return "  （没有找到别的出处）\n"
	}
	out := ""
	for _, a := range alts {
		out += fmt.Sprintf("  %-8s %-46s %d-%d  [%s]\n",
			a.ID, a.File, a.Start, a.End, strings.Join(a.Matched, "、"))
	}
	return out
}
