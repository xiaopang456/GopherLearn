package rag

import (
	"strings"
	"unicode"
)

// maxIndexedBigrams bounds one document's bigram tag value. A long episode
// synopsis yields a few hundred distinct pairs; the cap exists so a pathological
// document cannot turn one hash field into megabytes.
const maxIndexedBigrams = 600

// HanBigrams returns the distinct adjacent Han character pairs in a text.
//
// RediSearch tokenizes Chinese with a dictionary, and that dictionary does not
// contain this project's vocabulary. Measured on the shipped index: the term
// list holds `粘` and `包` as separate single-character terms and no `粘包` at
// all, so a search for 粘包 matches nothing while 粘 matches every document
// containing it. 半包, 拆包 and the rest fail the same way. The knowledge base
// index has the identical blind spot, which is why the lexical half of hybrid
// retrieval has been contributing nothing for Chinese technical terms.
//
// Bigrams sidestep the dictionary: any term of two or more Han characters
// contains at least one indexed pair, whether or not a segmenter knows the word.
//
// They must be stored in a TAG field rather than a TEXT one. A TEXT field is
// re-tokenized on the way in, and the Chinese tokenizer splits `粘包` straight
// back into two characters — measured, not assumed. Dropping the Chinese
// tokenizer instead would fix Chinese and break ASCII, because `PacketBuffer`
// embedded in a run of Han characters is only reachable through that tokenizer.
// The two field types together cover both.
func HanBigrams(value string) []string {
	runes := []rune(value)
	seen := make(map[string]struct{}, len(runes))
	bigrams := make([]string, 0, len(runes))
	for index := 0; index+1 < len(runes) && len(bigrams) < maxIndexedBigrams; index++ {
		if !unicode.Is(unicode.Han, runes[index]) || !unicode.Is(unicode.Han, runes[index+1]) {
			continue
		}
		pair := string(runes[index : index+2])
		if _, exists := seen[pair]; exists {
			continue
		}
		seen[pair] = struct{}{}
		bigrams = append(bigrams, pair)
	}
	return bigrams
}

// HanBigramTagValue renders bigrams for a comma-separated TAG field. Han
// characters need no tag escaping, and restricting the pairs to Han-only means
// no separator or punctuation can appear inside a value.
func HanBigramTagValue(value string) string {
	return strings.Join(HanBigrams(value), ",")
}

// HanBigramQueryTerms keeps only the Han bigrams of an already-extracted term
// list, which is what the TAG field can match. ExtractLexicalTerms emits ASCII
// identifiers, whole Han runs and Han bigrams together; the first two are for
// the tokenized text field and would never match a tag.
func HanBigramQueryTerms(terms []string) []string {
	out := make([]string, 0, len(terms))
	seen := make(map[string]struct{}, len(terms))
	for _, term := range terms {
		runes := []rune(term)
		if len(runes) != 2 || !unicode.Is(unicode.Han, runes[0]) || !unicode.Is(unicode.Han, runes[1]) {
			continue
		}
		if _, exists := seen[term]; exists {
			continue
		}
		seen[term] = struct{}{}
		out = append(out, term)
	}
	return out
}
