package summary

import (
	"strings"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/responder"
)

// OrdinaryBuild plans one refresh over the stored messages of one session.
//
// The conversation is read in append order, and only complete user→assistant pairs
// qualify. A user message with no answer behind it - the turn that failed, or the
// question being answered right now - is left out entirely: it is neither compressed
// into a summary nor presented as an answered exchange. Nothing is deleted from the
// store on account of it; this is a read-side decision.
//
// The pairs are taken from the oldest side of what is uncovered, so the summary always
// covers one contiguous stretch from the previous boundary forward. Trimming from the
// other end would leave a hole, and a summary with a hole in it cannot be extended
// again without guessing what fell in it.
func OrdinaryBuild(messages []domain.Message, boundaryMessageID string, policy Policy) Plan {
	policy = policy.Normalize()
	start := 0
	if boundary := strings.TrimSpace(boundaryMessageID); boundary != "" {
		index := ordinaryIndexOfMessage(messages, boundary)
		if index < 0 {
			return Plan{Skipped: SkipBoundaryMissing}
		}
		// The boundary itself is already summarized; resume after it.
		start = index + 1
	}

	pairs := ordinaryCompletePairs(messages[start:])
	for index := range pairs {
		pairs[index].question += start
		pairs[index].answer += start
	}

	plan := Plan{UncoveredMessages: len(pairs) * 2}
	if plan.UncoveredMessages < policy.TriggerMessages {
		plan.Skipped = SkipBelowThreshold
		return plan
	}
	compressPairs := len(pairs) - recentPairCount(policy.KeepRecentMessages)
	if compressPairs <= 0 {
		plan.Skipped = SkipNothingToCompress
		return plan
	}
	// Trim the newest compressed pairs until the set fits the input budget. Refusing
	// to send part of a pair is the point: the budget is measured over whole
	// exchanges, so the result is never a truncated turn.
	for compressPairs > 0 && !ordinaryFitsBudget(pairs[:compressPairs], messages, policy) {
		compressPairs--
	}
	if compressPairs <= 0 {
		plan.Skipped = SkipNothingToCompress
		return plan
	}

	plan.Compress = make([]responder.Turn, 0, compressPairs*2)
	for _, entry := range pairs[:compressPairs] {
		plan.Compress = append(plan.Compress,
			responder.Turn{Role: roleUser, Content: messages[entry.question].Content},
			responder.Turn{Role: roleAssistant, Content: messages[entry.answer].Content},
		)
	}
	plan.CompressedMessages = len(plan.Compress)
	plan.CoveredMessageID = messages[pairs[compressPairs-1].answer].ID
	return plan
}

// ordinaryCompletePairs walks stored messages in order and returns every user→assistant pair.
// An orphan steps forward by one message rather than skipping a whole window, so a
// failed turn in the middle does not hide the exchanges after it.
func ordinaryCompletePairs(messages []domain.Message) []messagePair {
	pairs := make([]messagePair, 0, len(messages)/2+1)
	for index := 0; index+1 < len(messages); {
		if messages[index].Role == roleUser && messages[index+1].Role == roleAssistant {
			pairs = append(pairs, messagePair{question: index, answer: index + 1})
			index += 2
			continue
		}
		index++
	}
	return pairs
}

func ordinaryFitsBudget(pairs []messagePair, messages []domain.Message, policy Policy) bool {
	if len(pairs)*2 > policy.MaxInputMessages {
		return false
	}
	used := 0
	for _, entry := range pairs {
		used += len([]rune(messages[entry.question].Content)) + len([]rune(messages[entry.answer].Content))
		if used > policy.MaxInputRunes {
			return false
		}
	}
	return true
}

func ordinaryIndexOfMessage(messages []domain.Message, id string) int {
	for index := range messages {
		if messages[index].ID == id {
			return index
		}
	}
	return -1
}
