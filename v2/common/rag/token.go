package rag

import "unicode"

// EstimateTokens is a deterministic, model-independent budget estimator. CJK
// characters and punctuation count individually, while ASCII words use a
// four-characters-per-token approximation. It is not a model tokenizer.
func EstimateTokens(text string) int {
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
			length := i - start
			tokens += (length + 3) / 4
		default:
			tokens++
			i++
		}
	}
	return tokens
}
