package service

import (
	"strings"
	"unicode"
)

// The text measurements the learning workflow decides with. They are deliberately
// lexical rather than semantic, and they live in one file so that evidence coverage
// and question novelty can be seen to use the same yardstick.
//
// A model is not asked "is this a duplicate?" because that question has no wrong
// answer it would flag: a generator that repeats itself is exactly as confident about
// the repeat as about the new thing, and a judge that says "different" costs a call
// and can still be wrong. A rune bigram comparison can be tuned, it is deterministic
// across runs, and a test can state the boundary it draws. Semantic alias merging -
// treating "RDB 持久化" and "快照与追加日志" as one subject - is a different and more
// dangerous decision, and it is not made here.

// normalizeLearningText lower-cases and keeps letters and digits, so a comparison is
// about what two strings say rather than how they were punctuated.
func normalizeLearningText(value string) string {
	var builder strings.Builder
	for _, symbol := range strings.ToLower(strings.TrimSpace(value)) {
		if unicode.IsLetter(symbol) || unicode.IsDigit(symbol) {
			builder.WriteRune(symbol)
		}
	}
	return builder.String()
}

// learningBigrams splits normalized text into overlapping rune pairs. A single rune
// yields one pair of itself, so a one-character topic still has something to compare:
// an empty set here would make every short string equally unlike every other.
func learningBigrams(value string) []string {
	runes := []rune(value)
	switch len(runes) {
	case 0:
		return nil
	case 1:
		return []string{string(runes)}
	}
	pairs := make([]string, 0, len(runes)-1)
	for index := 0; index+1 < len(runes); index++ {
		pairs = append(pairs, string(runes[index:index+2]))
	}
	return pairs
}

// learningDice is the Sørensen–Dice coefficient over rune bigrams. It is symmetric,
// which makes it the right measure for "are these two questions the same question".
func learningDice(left, right string) float64 {
	left, right = normalizeLearningText(left), normalizeLearningText(right)
	switch {
	case left == "" || right == "":
		return 0
	case left == right:
		return 1
	}
	leftPairs, rightPairs := learningBigrams(left), learningBigrams(right)
	if len(leftPairs) == 0 || len(rightPairs) == 0 {
		return 0
	}
	counts := make(map[string]int, len(leftPairs))
	for _, pair := range leftPairs {
		counts[pair]++
	}
	shared := 0
	for _, pair := range rightPairs {
		if counts[pair] > 0 {
			shared++
			counts[pair]--
		}
	}
	return 2 * float64(shared) / float64(len(leftPairs)+len(rightPairs))
}

// learningCoverage asks a different question than learningDice: how much of `phrase`
// appears in `text`. That asymmetry is what makes it usable for weak points - a long
// chunk of notes "covers" a short gap the learner has, while Dice would score the
// same pair low because their lengths differ.
func learningCoverage(text, phrase string) float64 {
	text, phrase = normalizeLearningText(text), normalizeLearningText(phrase)
	if text == "" || phrase == "" {
		return 0
	}
	if strings.Contains(text, phrase) {
		return 1
	}
	present := make(map[string]struct{})
	for _, pair := range learningBigrams(text) {
		present[pair] = struct{}{}
	}
	need := learningBigrams(phrase)
	if len(need) == 0 {
		return 0
	}
	matched := 0
	for _, pair := range need {
		if _, exists := present[pair]; exists {
			matched++
		}
	}
	return float64(matched) / float64(len(need))
}

// learningMaxCoverage is how strongly any one of `phrases` is present in `text`.
func learningMaxCoverage(text string, phrases []string) float64 {
	best := 0.0
	for _, phrase := range phrases {
		if score := learningCoverage(text, phrase); score > best {
			best = score
		}
	}
	return best
}

// clipLearningRunes cuts value to limit runes and marks that it was cut. The ellipsis
// matters: a snapshot that stops mid-sentence must not read like a complete one.
func clipLearningRunes(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "…"
}
