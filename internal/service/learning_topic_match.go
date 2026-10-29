package service

import (
	"github.com/xiaopang456/GopherLearn/internal/domain"
)

// Deciding whether a question is about a practised topic, without asking a model.
//
// This is the gate in front of the mastery note. Without it the note is chosen by
// recency, which means a learner who drilled Redis yesterday and asks about Kafka today
// gets told - by the server, in the position of authority - that they are weak at
// something they did not mention, and the agent pitches its answer to that instead. A
// wrong topic is worse than no topic, so every rule here errs toward silence.
//
// The shape is the one this project's own measurements settled: score the best-covered
// *part* of a topic name rather than the average over all of it, because learners drop
// the qualifier and name the concept ("Redis 缓存" asked about as 缓存穿透), and treat a
// single character as no evidence at all, because one Han rune or a two-letter acronym
// matches nearly everything. Matching is lexical on purpose: it is reproducible, it costs
// nothing, it cannot be talked into answering by text inside a document, and its boundary
// is something a test can state. Semantic alias merging - deciding that "RDB 快照" and
// "持久化" are one subject - is still not done anywhere in this workflow.
type learningTopicMatcher struct {
	normalized string
	// words holds maximal ASCII letter-or-digit runs, so an ASCII part of a topic name
	// only matches as a whole word. Substring matching there is how "go" is found inside
	// "google" and "rpc" inside "srpc".
	words map[string]struct{}
	// bigrams holds adjacent pairs from each non-ASCII run of the question, which is how
	// a Chinese topic name is compared without a segmenter's dictionary.
	bigrams map[string]struct{}
}

// newLearningTopicMatcher prepares one question for comparison against many stored
// topics. Normalising the question is the expensive half and depends only on the
// question, so it happens here rather than once per candidate.
func newLearningTopicMatcher(question string) learningTopicMatcher {
	normalized := domain.NormalizeLearningTopicKey(question)
	matcher := learningTopicMatcher{
		normalized: normalized,
		words:      make(map[string]struct{}),
		bigrams:    make(map[string]struct{}),
	}
	if normalized == "" {
		return matcher
	}
	for _, part := range splitLearningTopicParts(normalized) {
		if isLearningASCIIPart(part) {
			matcher.words[part] = struct{}{}
			continue
		}
		for _, pair := range learningBigrams(part) {
			matcher.bigrams[pair] = struct{}{}
		}
	}
	return matcher
}

// coverage scores how much of one topic key the question reproduces, 0 to 1.
func (m learningTopicMatcher) coverage(topicKey string) float64 {
	if m.normalized == "" {
		return 0
	}
	key := domain.NormalizeLearningTopicKey(topicKey)
	if key == "" {
		return 0
	}
	// Exact equality after normalisation is the strongest signal there is, and it is the
	// case where the question *is* the topic - a learner who typed the subject and
	// nothing else. Checked before the part walk so a one-part key cannot score lower
	// than a multi-part one for the same text.
	if key == m.normalized {
		return 1
	}
	best := 0.0
	for _, part := range splitLearningTopicParts(key) {
		if len([]rune(part)) < 2 {
			continue
		}
		score := 0.0
		if isLearningASCIIPart(part) {
			if _, whole := m.words[part]; whole {
				score = 1
			}
		} else {
			need := learningBigrams(part)
			matched := 0
			for _, pair := range need {
				if _, exists := m.bigrams[pair]; exists {
					matched++
				}
			}
			if len(need) > 0 {
				score = float64(matched) / float64(len(need))
			}
		}
		if score > best {
			best = score
		}
	}
	return best
}

// match scores a topic key and applies the floor to it, together.
//
// These are deliberately one function rather than a measurement one caller ranks and a
// threshold another caller checks: with two separate expressions, a caller can rank with
// `> 0` and still gate at `>= 0.5`, look correct on every test, and quietly load a topic
// the question barely touched. Whatever decides "is this the subject" decides it once.
func (m learningTopicMatcher) match(topicKey string) (float64, bool) {
	score := m.coverage(topicKey)
	return score, score >= domain.LearningTopicMatchMinCoverage
}

// splitLearningTopicParts breaks a normalised value at its separators and at every
// boundary between ASCII and non-ASCII, so "redis-持久化" and "rpc粘包处理" both yield
// two parts. The separators left by NormalizeLearningTopicKey are dropped here because
// the caller's job is comparing terms, not reconstructing the key.
func splitLearningTopicParts(value string) []string {
	parts := make([]string, 0, 4)
	var current []rune
	currentASCII := false
	flush := func() {
		if len(current) > 0 {
			parts = append(parts, string(current))
			current = current[:0]
		}
	}
	for _, symbol := range value {
		if symbol == '-' || symbol == '_' {
			flush()
			continue
		}
		isASCII := symbol < 128
		if len(current) > 0 && isASCII != currentASCII {
			flush()
		}
		currentASCII = isASCII
		current = append(current, symbol)
	}
	flush()
	return parts
}

func isLearningASCIIPart(value string) bool {
	for _, symbol := range value {
		if symbol >= 128 {
			return false
		}
	}
	return true
}
