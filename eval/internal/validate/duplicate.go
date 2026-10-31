package validate

import (
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/xiaopang456/GopherLearn/eval/internal/dataset"
)

// checkDuplicates reports questions that are too close to each other.
//
// The comparison is on character n-grams of the normalized question text, which
// works for Chinese and English alike: two Chinese questions that use the same
// wording in a different order still share most of their trigrams.
//
// Normalization drops case, whitespace and punctuation, so a question that
// differs only in "？" versus "?" or in spacing is the same question.
func checkDuplicates(report *Report, questions []dataset.Question, params Params) {
	threshold := params.DuplicateThreshold
	if threshold <= 0 {
		threshold = 0.75
	}
	type entry struct {
		id    string
		grams map[string]struct{}
	}
	entries := make([]entry, 0, len(questions))
	for _, q := range questions {
		grams := trigrams(normalizeQuestion(q.Question))
		if len(grams) == 0 {
			continue
		}
		entries = append(entries, entry{id: q.ID, grams: grams})
	}
	// Every question is compared with the questions that are kept. A question
	// that is too close to a kept one is reported and dropped.
	//
	// Comparing against the kept list rather than against every pair matters:
	// with a pair-wise comparison, once two questions were found to be similar
	// both were marked as "seen", and a third question that was just as similar
	// was never compared with anything and slipped through.
	var kept []entry
	for _, e := range entries {
		duplicate := false
		for _, k := range kept {
			similarity := jaccard(k.grams, e.grams)
			if similarity < threshold {
				continue
			}
			report.Findings = append(report.Findings, Finding{
				Severity: SeverityError, Rule: RuleDuplicate, ID: e.id,
				Detail: fmt.Sprintf("too similar to %q (character-trigram Jaccard %.2f >= %.2f); keep one of them",
					k.id, similarity, threshold)})
			duplicate = true
			break
		}
		if !duplicate {
			kept = append(kept, e)
		}
	}
}

// normalizeQuestion lowercases the question and drops everything that carries
// no meaning for similarity: spaces and punctuation.
func normalizeQuestion(question string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(question)) {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r):
			b.WriteRune(r)
		case r > unicode.MaxASCII && !unicode.IsSpace(r) && !unicode.IsPunct(r):
			b.WriteRune(r)
		}
	}
	return b.String()
}

// trigrams returns the set of character three-grams of s. A string shorter
// than three characters contributes itself, so a short question still gets a
// comparable signature.
func trigrams(s string) map[string]struct{} {
	runes := []rune(s)
	out := map[string]struct{}{}
	if len(runes) == 0 {
		return out
	}
	if len(runes) < 3 {
		out[string(runes)] = struct{}{}
		return out
	}
	for i := 0; i+3 <= len(runes); i++ {
		out[string(runes[i:i+3])] = struct{}{}
	}
	return out
}

func jaccard(a, b map[string]struct{}) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	shared := 0
	for key := range a {
		if _, ok := b[key]; ok {
			shared++
		}
	}
	union := len(a) + len(b) - shared
	if union == 0 {
		return 0
	}
	return float64(shared) / float64(union)
}

// SortedTopics returns the topics of the questions in a stable order.
func SortedTopics(questions []dataset.Question) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, q := range questions {
		if q.Topic == "" {
			continue
		}
		if _, ok := seen[q.Topic]; ok {
			continue
		}
		seen[q.Topic] = struct{}{}
		out = append(out, q.Topic)
	}
	sort.Strings(out)
	return out
}
