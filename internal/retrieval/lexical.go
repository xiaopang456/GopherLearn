package retrieval

import (
	"context"
	"math"
	"strings"
	"unicode"

	"github.com/xiaopang456/GopherLearn/internal/domain"
)

// Tokenize splits text for both the lexical scorer and the deterministic
// embedding. Latin, digit and underscore runs become single lowercase terms.
//
// A CJK run becomes overlapping character bigrams, because Chinese has no
// spaces and a whole-run term would only ever match an identical sentence. Bigram
// membership is also what "the query and this chunk share something" means for
// BM25's admission rule, so single characters deliberately do not count: 量子 and
// 子进程 share 子 and nothing else, and treating that as a match would cite a
// Redis snapshot chunk for a physics question. A run of exactly one character is
// still a term, so a character that stands alone in the text stays findable; the
// cost is that a one-character Chinese query usually matches nothing, which is
// the right way for this stage to fail - an honest "nothing found" beats a
// citation bought with a single shared stroke.
func Tokenize(text string) []string {
	var tokens []string
	var latin strings.Builder
	var cjk []rune
	flushLatin := func() {
		if latin.Len() > 0 {
			tokens = append(tokens, strings.ToLower(latin.String()))
			latin.Reset()
		}
	}
	flushCJK := func() {
		switch len(cjk) {
		case 0:
		case 1:
			tokens = append(tokens, string(cjk))
		default:
			for index := 0; index+1 < len(cjk); index++ {
				tokens = append(tokens, string(cjk[index:index+2]))
			}
		}
		cjk = cjk[:0]
	}
	for _, value := range text {
		switch {
		case unicode.Is(unicode.Han, value):
			flushLatin()
			cjk = append(cjk, value)
		case value == '_' || unicode.IsLetter(value) || unicode.IsDigit(value):
			flushCJK()
			latin.WriteRune(value)
		default:
			flushLatin()
			flushCJK()
		}
	}
	flushLatin()
	flushCJK()
	return tokens
}

// BM25Options tunes the lexical scorer. Zero values select the usual Okapi
// constants.
type BM25Options struct {
	K1 float64
	B  float64
}

// LexicalScorer is Okapi BM25 over the candidate set it is handed.
type LexicalScorer struct {
	options BM25Options
}

func NewLexicalScorer(options BM25Options) *LexicalScorer {
	if options.K1 == 0 {
		options.K1 = 1.4
	}
	if options.B == 0 {
		options.B = 0.75
	}
	return &LexicalScorer{options: options}
}

// Name implements Scorer.
func (s *LexicalScorer) Name() string { return "lexical-bm25" }

// Admit implements Scorer. BM25 measures term overlap, so its zero point is a
// real answer rather than a weak signal: a chunk sharing no term with the query
// sums to exactly 0, and any chunk sharing one is strictly positive because the
// idf used here is always above zero. Cutting at 0 therefore means "shares at
// least one word or CJK bigram with the question", which needs no tuned constant
// and cannot be confused with the cosine cut the vector scorer uses.
func (s *LexicalScorer) Admit(score float64) bool {
	return isFinite(score) && score > 0
}

// Score implements Scorer. Inverse document frequency is computed inside the
// candidate set, which keeps scores reproducible for a given scope and query.
func (s *LexicalScorer) Score(_ context.Context, query string, candidates []domain.DocumentChunk) ([]Scored, error) {
	queryTerms := unique(Tokenize(query))
	documents := make([][]string, len(candidates))
	total := 0
	for index, chunk := range candidates {
		documents[index] = Tokenize(chunk.Text + "\n" + chunk.Section)
		total += len(documents[index])
	}
	average := 0.0
	if len(documents) > 0 {
		average = float64(total) / float64(len(documents))
	}

	documentFrequency := make(map[string]int, 64)
	for _, document := range documents {
		for _, term := range unique(document) {
			documentFrequency[term]++
		}
	}

	corpus := float64(len(documents))
	results := make([]Scored, 0, len(candidates))
	for index, chunk := range candidates {
		document := documents[index]
		if len(document) == 0 || average == 0 {
			results = append(results, Scored{Chunk: chunk})
			continue
		}
		frequencies := termFrequencies(document)
		score := 0.0
		for _, term := range queryTerms {
			frequency := frequencies[term]
			if frequency == 0 {
				continue
			}
			idf := math.Log(1 + (corpus-float64(documentFrequency[term])+0.5)/(float64(documentFrequency[term])+0.5))
			saturation := float64(frequency) + s.options.K1*(1-s.options.B+s.options.B*float64(len(document))/average)
			score += idf * (float64(frequency) * (s.options.K1 + 1)) / saturation
		}
		results = append(results, Scored{Chunk: chunk, Score: score})
	}
	return results, nil
}

func unique(values []string) []string {
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}

func termFrequencies(values []string) map[string]int {
	counts := make(map[string]int, len(values))
	for _, value := range values {
		counts[value]++
	}
	return counts
}
