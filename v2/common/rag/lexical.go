package rag

import (
	"sort"
	"strings"
	"unicode"
)

const (
	rrfRankConstant = 60
	maxLexicalTerms = 24
)

var lexicalQuestionWords = []string{
	"为什么", "是什么", "有什么", "怎么样", "怎么做", "请问", "哪些", "什么",
	"如何", "是否", "能不能", "可不可以", "这个", "那个", "它们", "它", "分别",
	"其中", "里面", "的话", "时候", "问题", "以及", "还是", "就是", "并且", "同时",
	"可以", "不能", "需要", "使用", "会不会", "会", "的", "了", "吗", "呢", "和", "与",
}

var lexicalASCIIStopWords = map[string]struct{}{
	"a": {}, "an": {}, "and": {}, "are": {}, "can": {}, "do": {}, "does": {},
	"how": {}, "in": {}, "is": {}, "of": {}, "or": {}, "the": {}, "to": {},
	"what": {}, "when": {}, "where": {}, "which": {}, "why": {},
}

// ExtractLexicalTerms returns a bounded, deterministic set of safe search
// terms. ASCII identifiers keep dots/underscores for exact code and config
// lookups; Chinese runs add short n-grams after question words are removed.
func ExtractLexicalTerms(query string) []string {
	query = strings.TrimSpace(query)
	if query == "" {
		return []string{}
	}
	terms := make([]string, 0, maxLexicalTerms)
	seen := make(map[string]struct{}, maxLexicalTerms)
	add := func(term string) {
		term = strings.Trim(term, "._:/-")
		if len([]rune(term)) < 2 || len(terms) >= maxLexicalTerms {
			return
		}
		key := strings.ToLower(term)
		if _, stop := lexicalASCIIStopWords[key]; stop {
			return
		}
		if _, exists := seen[key]; exists {
			return
		}
		seen[key] = struct{}{}
		terms = append(terms, term)
	}

	runes := []rune(query)
	// Code identifiers are usually the strongest lexical signal and must not
	// be pushed out by a long Chinese question that reaches the term cap.
	for start := 0; start < len(runes); {
		if isASCIIIdentifierRune(runes[start]) {
			end := start + 1
			for end < len(runes) && isASCIIIdentifierRune(runes[end]) {
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
			addChineseTerms(string(runes[start:end]), add)
			start = end
			continue
		}
		start++
	}
	return terms
}

func addChineseTerms(value string, add func(string)) {
	for _, word := range lexicalQuestionWords {
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

func isASCIIIdentifierRune(value rune) bool {
	return value <= unicode.MaxASCII && (unicode.IsLetter(value) || unicode.IsDigit(value) || strings.ContainsRune("._:/-", value))
}

// FuseRRF combines independently ranked vector and lexical results. RRF uses
// only ranks, so incomparable cosine distance and BM25 scores are never added
// directly. FusionScore is normalized to [0,1] for display and diagnostics.
// RRFRankConstant is the k of reciprocal-rank fusion, shared by the knowledge
// channel and the episodic reader so a plan can name one number for both.
func RRFRankConstant() int { return rrfRankConstant }

func FuseRRF(vectorResults, lexicalResults []RedisChunkSearchResult, topK int) []RedisChunkSearchResult {
	if topK <= 0 {
		return []RedisChunkSearchResult{}
	}
	type fused struct {
		candidate RedisChunkSearchResult
		score     float64
	}
	byID := make(map[string]*fused, len(vectorResults)+len(lexicalResults))
	identity := func(candidate RedisChunkSearchResult) string {
		if candidate.RedisKey != "" {
			return candidate.RedisKey
		}
		return candidate.DocID + ":" + candidate.ChunkID
	}
	for index, candidate := range vectorResults {
		key := identity(candidate)
		entry := &fused{candidate: candidate}
		entry.candidate.VectorRank = index + 1
		entry.score = 1 / float64(rrfRankConstant+index+1)
		byID[key] = entry
	}
	for index, candidate := range lexicalResults {
		key := identity(candidate)
		entry, exists := byID[key]
		if !exists {
			entry = &fused{candidate: candidate}
			byID[key] = entry
		} else {
			entry.candidate.LexicalScore = candidate.LexicalScore
		}
		entry.candidate.LexicalRank = index + 1
		entry.score += 1 / float64(rrfRankConstant+index+1)
	}
	channelCount := 1.0
	if len(lexicalResults) > 0 {
		channelCount = 2
	}
	maximum := channelCount / float64(rrfRankConstant+1)
	entries := make([]fused, 0, len(byID))
	for _, entry := range byID {
		entry.candidate.FusionScore = entry.score / maximum
		switch {
		case entry.candidate.VectorRank > 0 && entry.candidate.LexicalRank > 0:
			entry.candidate.MatchKind = "vector+lexical"
		case entry.candidate.LexicalRank > 0:
			entry.candidate.MatchKind = "lexical"
		default:
			entry.candidate.MatchKind = "vector"
		}
		entries = append(entries, *entry)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].score != entries[j].score {
			return entries[i].score > entries[j].score
		}
		leftVector, rightVector := nonZeroRank(entries[i].candidate.VectorRank), nonZeroRank(entries[j].candidate.VectorRank)
		if leftVector != rightVector {
			return leftVector < rightVector
		}
		leftLexical, rightLexical := nonZeroRank(entries[i].candidate.LexicalRank), nonZeroRank(entries[j].candidate.LexicalRank)
		if leftLexical != rightLexical {
			return leftLexical < rightLexical
		}
		return identity(entries[i].candidate) < identity(entries[j].candidate)
	})
	if topK > len(entries) {
		topK = len(entries)
	}
	results := make([]RedisChunkSearchResult, topK)
	for index := 0; index < topK; index++ {
		results[index] = entries[index].candidate
	}
	return results
}

func nonZeroRank(value int) int {
	if value == 0 {
		return int(^uint(0) >> 1)
	}
	return value
}
