package service

import (
	"regexp"
	"strings"
	"unicode"
)

// Deterministic text helpers for the episodic memory system, carried over from
// the original project's common text utilities. They are pure functions: the
// same input gives the same output on every machine, which is what lets a read
// plan, a search text and a topic key stay reproducible across processes.

const (
	episodeRRFRankConstant = 60
	maxEpisodeLexicalTerms = 24
)

var episodeLexicalQuestionWords = []string{
	"为什么", "是什么", "有什么", "怎么样", "怎么做", "请问", "哪些", "什么",
	"如何", "是否", "能不能", "可不可以", "这个", "那个", "它们", "它", "分别",
	"其中", "里面", "的话", "时候", "问题", "以及", "还是", "就是", "并且", "同时",
	"可以", "不能", "需要", "使用", "会不会", "会", "的", "了", "吗", "呢", "和", "与",
}

var episodeLexicalASCIIStopWords = map[string]struct{}{
	"a": {}, "an": {}, "and": {}, "are": {}, "can": {}, "do": {}, "does": {},
	"how": {}, "in": {}, "is": {}, "of": {}, "or": {}, "the": {}, "to": {},
	"what": {}, "when": {}, "where": {}, "which": {}, "why": {},
}

// extractEpisodeLexicalTerms returns a bounded, deterministic set of safe
// search terms. ASCII identifiers keep dots/underscores for exact code and
// config lookups; Chinese runs add short n-grams after question words are
// removed. Code identifiers are collected first so a long Chinese question
// cannot push them out at the term cap.
func extractEpisodeLexicalTerms(query string) []string {
	query = strings.TrimSpace(query)
	if query == "" {
		return []string{}
	}
	terms := make([]string, 0, maxEpisodeLexicalTerms)
	seen := make(map[string]struct{}, maxEpisodeLexicalTerms)
	add := func(term string) {
		term = strings.Trim(term, "._:/-")
		if len([]rune(term)) < 2 || len(terms) >= maxEpisodeLexicalTerms {
			return
		}
		key := strings.ToLower(term)
		if _, stop := episodeLexicalASCIIStopWords[key]; stop {
			return
		}
		if _, exists := seen[key]; exists {
			return
		}
		seen[key] = struct{}{}
		terms = append(terms, term)
	}

	runes := []rune(query)
	for start := 0; start < len(runes); {
		if isEpisodeASCIIIdentifierRune(runes[start]) {
			end := start + 1
			for end < len(runes) && isEpisodeASCIIIdentifierRune(runes[end]) {
				end++
			}
			add(string(runes[start:end]))
			start = end
			continue
		}
		start++
	}
	for start := 0; start < len(runes); {
		if unicode.Is(unicode.Han, runes[start]) {
			end := start + 1
			for end < len(runes) && unicode.Is(unicode.Han, runes[end]) {
				end++
			}
			episodeAddChineseTerms(string(runes[start:end]), add)
			start = end
			continue
		}
		start++
	}
	return terms
}

func episodeAddChineseTerms(value string, add func(string)) {
	for _, word := range episodeLexicalQuestionWords {
		value = strings.ReplaceAll(value, word, " ")
	}
	for _, part := range strings.Fields(value) {
		runes := []rune(part)
		if len(runes) < 2 {
			continue
		}
		add(part)
		if len(runes) <= 2 {
			continue
		}
		for index := 0; index+2 <= len(runes); index++ {
			add(string(runes[index : index+2]))
		}
	}
}

func isEpisodeASCIIIdentifierRune(value rune) bool {
	return value <= unicode.MaxASCII && (unicode.IsLetter(value) || unicode.IsDigit(value) || strings.ContainsRune("._:/-", value))
}

// estimateEpisodeTokens is a deterministic, model-independent budget estimator:
// CJK characters count individually, ASCII words use a four-characters-per-token
// approximation. It is not a model tokenizer, and every limit it feeds is named
// as an estimate.
func estimateEpisodeTokens(text string) int {
	runes := []rune(text)
	tokens := 0
	for i := 0; i < len(runes); {
		r := runes[i]
		switch {
		case r == '\n':
			tokens++
			i++
		case unicode.IsSpace(r):
			tokens++
			for i < len(runes) && runes[i] != '\n' && unicode.IsSpace(runes[i]) {
				i++
			}
		case r <= unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_'):
			start := i
			for i < len(runes) {
				current := runes[i]
				if current > unicode.MaxASCII || (!unicode.IsLetter(current) && !unicode.IsDigit(current) && current != '_') {
					break
				}
				i++
			}
			tokens += (i - start + 3) / 4
		default:
			tokens++
			i++
		}
	}
	return tokens
}

// episodeIdentifierPattern is the shape of a code-like token.
var episodeIdentifierPattern = regexp.MustCompile(`[A-Za-z][A-Za-z0-9._:/-]{2,}`)

// episodeStrongIdentifiers pulls the code-shaped tokens out of a question. The
// bar is deliberately high: a three-letter acronym is not an identifier, and
// promoting one would make the exact channel fire on questions that named
// nothing in particular. What qualifies is a digit, a separator, mixed case, or
// real length.
func episodeStrongIdentifiers(question string) []string {
	matches := episodeIdentifierPattern.FindAllString(question, -1)
	seen := map[string]struct{}{}
	result := make([]string, 0, len(matches))
	for _, match := range matches {
		match = strings.Trim(match, "._:/-")
		if !isEpisodeStrongIdentifier(match) {
			continue
		}
		key := strings.ToLower(match)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, match)
	}
	return result
}

func isEpisodeStrongIdentifier(value string) bool {
	if len(value) < 4 {
		return false
	}
	hasLower, hasUpper, hasDigit, hasSeparator := false, false, false, false
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
			hasLower = true
		case r >= 'A' && r <= 'Z':
			hasUpper = true
		case r >= '0' && r <= '9':
			hasDigit = true
		case strings.ContainsRune("._:/-", r):
			hasSeparator = true
		}
	}
	return hasDigit || hasSeparator || (hasLower && hasUpper) || len(value) >= 8
}

// containsAny reports whether value contains any of the needles.
func containsAny(value string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}
