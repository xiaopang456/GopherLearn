package domain

import (
	"math"
	"sort"
	"strings"
	"time"
)

// ProjectTopicProgress turns a learner's recent completed attempts at one topic
// into the thing the next explanation reads.
//
// The input contract is narrow on purpose: only completed attempts, newest first,
// no more than LearningRecentAttemptWindow of them, ordered by the store rather
// than here. That ordering is part of the arithmetic - the weight of an attempt is
// its position in the list, so a caller that sorted differently gets a different
// mastery score from the same answers.
//
// Nothing in here is a judgement about the learner beyond the input: no model gets
// to propose a score, a level, or which weak points matter. The original project
// made the same choice and stated it in its grading prompt ("do not output a total
// score or a mastery decision"), and the reason survives restating - a model that
// rates both the answer and the person who gave it has no outside check left.
func ProjectTopicProgress(userID, topicKey, topic string, completed []LearningAttempt, now time.Time) TopicProgress {
	weighted, weights := 0, 0
	type weakness struct {
		text  string
		count int
		first int
	}
	seen := make(map[string]*weakness)
	order := make([]string, 0, len(completed))

	for index, attempt := range completed {
		// Newest first, so the most recent answer carries the largest weight and
		// the weight sum falls off linearly rather than by a tuned decay constant.
		weight := len(completed) - index
		weighted += attempt.EffectiveTotal * weight
		weights += weight

		for _, raw := range attempt.Grade.MissingPoints {
			text := strings.TrimSpace(raw)
			if text == "" {
				continue
			}
			key := strings.ToLower(strings.Join(strings.Fields(text), " "))
			if known, exists := seen[key]; exists {
				known.count++
				continue
			}
			seen[key] = &weakness{text: text, count: 1, first: index}
			order = append(order, key)
		}
	}

	mastery := 0
	if weights > 0 {
		mastery = int(math.Round(float64(weighted) / float64(weights)))
	}
	confidence := math.Min(1, float64(len(completed))/LearningConfidenceAttemptDivisor)

	level := TopicProgressDeveloping
	switch {
	case len(completed) < LearningReliableAttemptCount:
		// Fewer than three valid answers is not enough to describe anyone. The
		// number still goes in - it is what was measured - but the level says so.
		level = TopicProgressInsufficient
	case mastery >= LearningMasteryMasteredAt && confidence >= LearningMasteryConfidenceAt:
		level = TopicProgressMastered
	}

	// Counted first, then first-seen, so a repeatedly missing point rises and a
	// one-off never does. Truncation happens after sorting: which points survive is
	// the only decision made here, and it is made on frequency.
	ranked := make([]*weakness, 0, len(order))
	for _, key := range order {
		ranked = append(ranked, seen[key])
	}
	sort.SliceStable(ranked, func(left, right int) bool {
		if ranked[left].count != ranked[right].count {
			return ranked[left].count > ranked[right].count
		}
		return ranked[left].first < ranked[right].first
	})
	limit := min(MaxLearningWeakPoints, len(ranked))
	weakPoints := make([]string, 0, limit)
	for _, item := range ranked[:limit] {
		weakPoints = append(weakPoints, item.text)
	}

	progress := TopicProgress{
		UserID:     userID,
		TopicKey:   topicKey,
		Topic:      topic,
		Mastery:    mastery,
		Confidence: confidence,
		ValidCount: len(completed),
		Level:      level,
		WeakPoints: weakPoints,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if len(completed) > 0 {
		progress.LastAttempt = completed[0].CompletedAt
	}
	return progress
}
