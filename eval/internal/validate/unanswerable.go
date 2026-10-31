package validate

import (
	"fmt"
	"sort"
	"strings"
	"unicode"

	"GopherAI/common/rag"

	"github.com/xiaopang456/GopherLearn/eval/internal/dataset"
)

// checkUnanswerable confirms that an unanswerable question really has no
// answer in the corpus.
//
// The keywords come from the product's own extractor
// (rag.ExtractLexicalTerms), so the check looks for exactly the terms the
// product's keyword channel would look for.
//
// A plain keyword count does not work on this corpus. The extractor emits a
// Chinese run and every two-character slice of it, so "默认值" also yields
// "认值", a fragment that crosses a word boundary and turns up in unrelated
// lines. With a loose frequency limit those fragments count as "distinctive
// keywords", and the rule degenerates into "this word appeared somewhere":
// every unanswerable question was rejected.
//
// The rule that survives the corpus:
//
//  1. a keyword that occurs in more than UnanswerableMaxDocFreq corpus lines
//     proves nothing and is dropped;
//  2. a corpus line that carries *every* distinctive keyword is treated as
//     the answer (error), as long as there are at least two of them: the
//     corpus talks about the whole question, not about one of its words;
//  3. a corpus line that carries a single ASCII identifier which is common in
//     the corpus (UnanswerableStrongTermMinDocFreq lines or more) is also
//     treated as the answer: the corpus covers that identifier thoroughly, so
//     a question about it is very likely answerable;
//  4. a corpus line that carries some of the keywords is only a suspect
//     (warning): the corpus talks about something nearby, and a human has to
//     look;
//  5. a question whose keywords are all absent or all too common cannot be
//     screened at all; that is reported too, so "not flagged" never silently
//     means "screened and clean".
//
// The candidate lines are reported with their file and line so a human can
// look at them.
func checkUnanswerable(report *Report, q dataset.Question, docs map[string]*Document, params Params) {
	if q.Answerable {
		return
	}
	terms := rag.ExtractLexicalTerms(q.Question)
	if len(terms) == 0 {
		report.Findings = append(report.Findings, Finding{
			Severity: SeverityWarning, Rule: RuleUnanswerableInconclusive, ID: q.ID,
			Detail: "no searchable keywords could be extracted, so the absence of an answer cannot be confirmed"})
		return
	}
	lowered := make([]string, len(terms))
	for i, t := range terms {
		lowered[i] = strings.ToLower(t)
	}
	total := CountCorpusLines(docs)
	if total == 0 {
		return
	}
	maxFreq := params.UnanswerableMaxDocFreq
	if maxFreq <= 0 {
		maxFreq = 120
	}
	strongFreq := params.UnanswerableStrongTermMinDocFreq
	if strongFreq <= 0 {
		strongFreq = 60
	}
	minDistinctive := params.UnanswerableMinDistinctive
	if minDistinctive <= 0 {
		minDistinctive = 2
	}

	// Document frequency per keyword.
	docFreq := make([]int, len(lowered))
	scanLines(docs, func(_ string, _ int, text string) {
		low := strings.ToLower(text)
		for i, term := range lowered {
			if term == "" {
				continue
			}
			if containsTerm(low, term) {
				docFreq[i]++
			}
		}
	})
	distinctive := make([]int, 0, len(lowered))
	for i, term := range lowered {
		if term == "" {
			continue
		}
		if docFreq[i] == 0 || docFreq[i] > maxFreq {
			continue
		}
		distinctive = append(distinctive, i)
	}
	if len(distinctive) == 0 {
		report.Findings = append(report.Findings, Finding{
			Severity: SeverityWarning, Rule: RuleUnanswerableInconclusive, ID: q.ID,
			Detail: fmt.Sprintf("every keyword of this question is absent from or too common in the corpus (%d lines); the absence of an answer cannot be confirmed either way",
				total)})
		return
	}

	var answers []corpusCandidate
	var suspects []corpusCandidate
	scanLines(docs, func(file string, line int, text string) {
		low := strings.ToLower(text)
		var matched []string
		score := 0
		for _, i := range distinctive {
			if containsTerm(low, lowered[i]) {
				matched = append(matched, terms[i])
				score += termWeight(terms[i])
			}
		}
		if len(matched) == 0 {
			return
		}
		c := corpusCandidate{file: file, line: line, matched: matched, score: score}
		switch {
		case len(matched) >= minDistinctive && len(matched) == len(distinctive):
			// The corpus line carries every distinctive keyword of the
			// question: that is the answer, not a coincidence.
			answers = append(answers, c)
		case len(matched) == 1 && isASCIITerm(matched[0]) && docFreq[indexOf(lowered, matched[0])] >= strongFreq:
			// One widely used identifier: the corpus covers that topic
			// thoroughly, so the question is very likely answerable.
			answers = append(answers, c)
		case len(matched) >= minDistinctive:
			suspects = append(suspects, c)
		}
	})
	sortCandidates(answers)
	sortCandidates(suspects)

	if len(answers) > 0 {
		report.Findings = append(report.Findings, Finding{
			Severity: SeverityError, Rule: RuleUnanswerableHit, ID: q.ID,
			File: answers[0].file, Line: answers[0].line,
			Detail: fmt.Sprintf("an unanswerable question is answered by %d corpus line(s); the closest: %s",
				len(answers), formatCandidates(answers))})
		return
	}
	if len(suspects) > 0 {
		report.Findings = append(report.Findings, Finding{
			Severity: SeverityWarning, Rule: RuleUnanswerableSuspect, ID: q.ID,
			File: suspects[0].file, Line: suspects[0].line,
			Detail: fmt.Sprintf("%d corpus line(s) carry some of this question's keywords but not all of them; the corpus may still know the answer: %s",
				len(suspects), formatCandidates(suspects))})
	}
}

func sortCandidates(candidates []corpusCandidate) {
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}
		if len(candidates[i].matched) != len(candidates[j].matched) {
			return len(candidates[i].matched) > len(candidates[j].matched)
		}
		if candidates[i].file != candidates[j].file {
			return candidates[i].file < candidates[j].file
		}
		return candidates[i].line < candidates[j].line
	})
}

// corpusCandidate is one corpus line that carries some of a question's
// keywords.
type corpusCandidate struct {
	file    string
	line    int
	matched []string
	score   int
}

// formatCandidates renders the first few candidate lines for the report.
func formatCandidates(candidates []corpusCandidate) string {
	shown := candidates
	if len(shown) > 5 {
		shown = shown[:5]
	}
	var details []string
	for _, c := range shown {
		details = append(details, fmt.Sprintf("%s:%d [%s]", c.file, c.line, strings.Join(c.matched, "、")))
	}
	return strings.Join(details, "; ")
}

// indexOf returns the position of a lowered term, or -1.
func indexOf(lowered []string, term string) int {
	for i, t := range lowered {
		if t == strings.ToLower(term) {
			return i
		}
	}
	return -1
}

// isStrongTerm reports whether a keyword is specific enough to be evidence on
// its own: an ASCII identifier of at least four characters, or a Chinese run
// of at least three.
//
// The weight is only used to rank the candidate lines, never to decide
// whether a line counts.
func isStrongTerm(term string) bool {
	runes := []rune(term)
	if len(runes) == 0 {
		return false
	}
	if isASCIITerm(term) {
		return len(term) >= 4
	}
	return len(runes) >= 3
}

// termWeight ranks a keyword: a specific term counts more than a two-character
// Chinese bigram, so a real answer outranks the bigram noise around it.
func termWeight(term string) int {
	if isStrongTerm(term) {
		return 2
	}
	return 1
}

// containsTerm reports whether a lowercased line contains the term.
//
// ASCII terms are matched on word boundaries, so "go" does not match inside
// "google". Han terms are matched as plain substrings, because Chinese has no
// word boundaries.
func containsTerm(loweredLine, loweredTerm string) bool {
	if loweredTerm == "" {
		return false
	}
	if isASCIITerm(loweredTerm) {
		return containsWord(loweredLine, loweredTerm)
	}
	return strings.Contains(loweredLine, loweredTerm)
}

func isASCIITerm(term string) bool {
	for _, r := range term {
		if r > unicode.MaxASCII || !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '.' && r != '_' && r != ':' && r != '/' && r != '-' {
			return false
		}
	}
	return true
}

func containsWord(haystack, needle string) bool {
	for offset := 0; offset+len(needle) <= len(haystack); offset++ {
		if haystack[offset:offset+len(needle)] != needle {
			continue
		}
		if offset > 0 && isWordByte(haystack[offset-1]) {
			continue
		}
		if offset+len(needle) < len(haystack) && isWordByte(haystack[offset+len(needle)]) {
			continue
		}
		return true
	}
	return false
}

func isWordByte(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9', b == '_':
		return true
	}
	return false
}
