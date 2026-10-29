// Package topicmatch decides whether a question names a topic.
//
// Two places in this project need that decision and both feed the same pool of
// topic keys: the mastery provider, whose candidates are the topics the learner
// has been quizzed on, and the memory retriever, whose candidates are the topics
// the learner has stored memories under. Whichever produces a key, the key is
// merged into one list and used to fetch topic memories.
//
// They had different answers to the same question. Scored on the same
// forty-six hand-labelled cases, the coverage matcher below reached 0.941 while
// the retriever's whole-name containment reached 0.294 — and not one of the
// cases it caught was a case the coverage matcher missed. Its strictness bought
// nothing: it was a strict subset. What differs between the two callers is the
// candidate pool, which is worth keeping; the matching rule is not.
package topicmatch

import (
	"GopherAI/model"
	"regexp"
	"unicode/utf8"
)

// DefaultMinCoverage is the share of one part of a topic name that a question
// must reproduce before the topic counts as named.
//
// Half is a judgement, not a measurement: loading the wrong topic is worse than
// loading none, because it tells the model the learner is weak at something
// they did not ask about.
const DefaultMinCoverage = 0.5

var asciiWordPattern = regexp.MustCompile(`[a-z0-9]+`)

// Question is a question prepared once and matched against many topic keys.
// The normalisation and bigram work depends only on the question, so hoisting
// it out of the loop keeps a scan over hundreds of topics cheap.
type Question struct {
	normalized string
	words      map[string]struct{}
	bigrams    map[string]struct{}
}

func NewQuestion(text string) Question {
	normalized := model.NormalizeLearningTopicKey(text)
	words := make(map[string]struct{})
	for _, word := range asciiWordPattern.FindAllString(normalized, -1) {
		words[word] = struct{}{}
	}
	return Question{normalized: normalized, words: words, bigrams: HanBigrams(normalized)}
}

// Empty reports whether the question carried nothing to match on.
func (q Question) Empty() bool { return q.normalized == "" }

// Names reports whether the question reproduces enough of the topic key.
func (q Question) Names(topicKey string) bool {
	return q.Coverage(topicKey) >= DefaultMinCoverage
}

// Coverage scores the best-covered part of the topic key, not the average over
// all of them.
//
// Learners drop the qualifier and name the concept: someone quizzed on
// "Redis 缓存" asks about 缓存穿透, and someone quizzed on "JWT 鉴权" asks about
// 鉴权. Averaging over the whole name would reject both, because the half they
// omitted counts against them. One part fully present is real evidence; the
// rest of the name is context they had no reason to repeat.
func (q Question) Coverage(topicKey string) float64 {
	best := 0.0
	for _, part := range SplitByScript(model.NormalizeLearningTopicKey(topicKey)) {
		if utf8.RuneCountInString(part) < 2 {
			// A single character, Han or ASCII, is too common to be evidence.
			continue
		}
		if IsASCIISegment(part) {
			// Whole-word only, so that "go" is not found inside "google".
			if _, exists := q.words[part]; exists {
				best = 1
			}
			continue
		}
		bigrams := HanBigrams(part)
		if len(bigrams) == 0 {
			continue
		}
		hit := 0
		for bigram := range bigrams {
			if _, exists := q.bigrams[bigram]; exists {
				hit++
			}
		}
		if coverage := float64(hit) / float64(len(bigrams)); coverage > best {
			best = coverage
		}
	}
	return best
}

// SplitByScript breaks a normalised key at separators and at every boundary
// between ASCII and Han, so that `rpc粘包处理` yields "rpc" and "粘包处理".
func SplitByScript(value string) []string {
	var parts []string
	var current []rune
	currentASCII := false
	flush := func() {
		if len(current) > 0 {
			parts = append(parts, string(current))
			current = current[:0]
		}
	}
	for _, r := range value {
		if r == '-' || r == '_' || r == ' ' {
			flush()
			continue
		}
		isASCII := r <= 127
		if len(current) > 0 && isASCII != currentASCII {
			flush()
		}
		currentASCII = isASCII
		current = append(current, r)
	}
	flush()
	return parts
}

// HanBigrams returns every adjacent pair of Han characters, which is how a
// topic name is compared without depending on a segmenter's dictionary.
func HanBigrams(value string) map[string]struct{} {
	out := map[string]struct{}{}
	var run []rune
	emit := func() {
		for i := 0; i+1 < len(run); i++ {
			out[string(run[i:i+2])] = struct{}{}
		}
		run = run[:0]
	}
	for _, r := range value {
		if r > 127 {
			run = append(run, r)
			continue
		}
		emit()
	}
	emit()
	return out
}

func IsASCIISegment(value string) bool {
	for _, r := range value {
		if r > 127 {
			return false
		}
	}
	return true
}
