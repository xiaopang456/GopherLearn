package rag

import (
	"regexp"
	"strings"
)

// DefaultLexicalWeight is the weight of the keyword channel in the order a retrieval returns
// when the reranker does not run (and in the order it falls back to when the reranker fails).
//
// With equal weights, a chunk found by both channels at middling ranks outscored the vector
// channel's first choices, and for a question in Chinese about an English source the keyword
// channel can only agree with the Chinese notes on the same topic. At 0.1 the keyword channel
// still lifts a chunk the vector channel found, but a chunk only it found cannot outrank any
// vector candidate. The candidate pool the reranker sees is not affected: it keeps equal weights.
const DefaultLexicalWeight = 0.1

// A word of the question counts as an identifier for the reserved slot when it is made of
// letters, digits, '_', '.' and '-', is at least three characters long, is not a common English
// function word, and looks like an identifier: it has a digit or one of '_' '.' '-', or a
// lower-case letter followed by an upper-case one, or it starts with two capitals (AOF, RDB).
// Plain words such as "agent" or "Prompt" do not count.
var identifierToken = regexp.MustCompile(`[A-Za-z0-9_.-]+`)
var identifierShape = regexp.MustCompile(`[0-9_.-]|[a-z][A-Z]|^[A-Z]{2,}`)

var identifierStopWords = map[string]struct{}{}

func init() {
	for _, word := range strings.Fields(`the and for are was were with this that from have has had not but you your how what
		when where which why who can will would should could does did its their them they than then there here into out
		about over also any all more most some such only other one two use used using via per may might our each both own
		same very just like while between`) {
		identifierStopWords[word] = struct{}{}
	}
	for word := range lexicalASCIIStopWords {
		identifierStopWords[word] = struct{}{}
	}
}

// QueryIdentifiers returns the identifier-like words of a question, in order of first
// appearance and without repeats.
func QueryIdentifiers(query string) []string {
	var found []string
	seen := map[string]struct{}{}
	for _, raw := range identifierToken.FindAllString(query, -1) {
		word := strings.Trim(raw, "._-")
		if len(word) < 3 {
			continue
		}
		if _, stop := identifierStopWords[strings.ToLower(word)]; stop {
			continue
		}
		if !identifierShape.MatchString(word) {
			continue
		}
		if _, repeated := seen[word]; repeated {
			continue
		}
		seen[word] = struct{}{}
		found = append(found, word)
	}
	return found
}

// ReserveIdentifierSlot keeps one place among the top K for a chunk that contains, verbatim,
// an identifier of the question. fused is the whole fused list in order (not only its top K).
//
// When the top K already holds such a chunk, or K < 3, or the question has no identifier, or no
// other candidate contains one, the top K is returned unchanged. Otherwise the candidate that
// contains the most of the question's identifiers (the earlier in fused order on a tie) takes the
// last place of the top K. The point is a question that names something exactly - a function, a
// setting, an error code - which the vector channel may not find.
func ReserveIdentifierSlot(query string, fused []RedisChunkSearchResult, topK int) []RedisChunkSearchResult {
	if topK > len(fused) {
		topK = len(fused)
	}
	top := append([]RedisChunkSearchResult(nil), fused[:topK]...)
	if topK < 3 {
		return top
	}
	identifiers := QueryIdentifiers(query)
	if len(identifiers) == 0 {
		return top
	}
	matches := func(candidate RedisChunkSearchResult) int {
		text := candidate.RawContent
		if text == "" {
			text = candidate.EmbeddingContent
		}
		count := 0
		for _, identifier := range identifiers {
			if strings.Contains(text, identifier) {
				count++
			}
		}
		return count
	}
	for _, candidate := range top {
		if matches(candidate) > 0 {
			return top
		}
	}
	best, bestCount := -1, 0
	for index := topK; index < len(fused); index++ {
		if count := matches(fused[index]); count > bestCount {
			best, bestCount = index, count
		}
	}
	if best < 0 {
		return top
	}
	top[topK-1] = fused[best]
	return top
}
