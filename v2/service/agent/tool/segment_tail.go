package agenttool

import (
	"strings"

	"GopherAI/model"
	"GopherAI/service/episode"
)

// tailSelectionTerms reads the words the tail is selected by.
//
// The LEARNER'S QUESTION comes first: the update has to be findable from the
// words the learner used, not from the words the model chose to search with.
// The model's query is the fallback, and the tokenizer's cut of each is the
// last resort.
func tailSelectionTerms(question, modelQuery string) []string {
	question = strings.TrimSpace(question)
	modelQuery = strings.TrimSpace(modelQuery)
	if question != "" {
		plan := episode.BuildReadPlan(episode.RouteRequest{Question: question})
		if len(plan.TopicTerms) > 0 {
			return plan.TopicTerms
		}
		if terms := thinCardFallbackTerms(question); len(terms) > 0 {
			return terms
		}
	}
	if modelQuery != "" {
		plan := episode.BuildReadPlan(episode.RouteRequest{Question: modelQuery})
		if len(plan.TopicTerms) > 0 {
			return plan.TopicTerms
		}
		if terms := thinCardFallbackTerms(modelQuery); len(terms) > 0 {
			return terms
		}
	}
	return nil
}

// selectTail chooses the tail with the learner's question first, and falls
// back to the tokenizer's cut of that question when the first terms match
// nothing.
//
// The question is the source of the words: the update has to be findable from
// what the learner asked, and the model's query is only the last fallback.
func selectTail(messages []model.Message, question, modelQuery string, limit int) []model.Message {
	primary := tailSelectionTerms(question, modelQuery)
	if tailTermsMatch(messages, primary) {
		return selectSegmentTail(messages, primary, limit)
	}
	if fallback := thinCardFallbackTerms(question); len(fallback) > 0 && tailTermsMatch(messages, fallback) {
		return selectSegmentTail(messages, fallback, limit)
	}
	return selectSegmentTail(messages, primary, limit)
}

// tailTermsMatch reports whether any message mentions one of the terms.
func tailTermsMatch(messages []model.Message, terms []string) bool {
	for _, term := range terms {
		term = strings.ToLower(strings.TrimSpace(term))
		if len([]rune(term)) < 2 {
			continue
		}
		for _, message := range messages {
			if strings.Contains(strings.ToLower(message.Content), term) {
				return true
			}
		}
	}
	return false
}

// selectSegmentTail chooses the messages after a summary's high-water mark
// that the answer needs.
//
// The summary stops before the conversation settles, and the words after it
// can be long. Keeping only the newest messages is wrong when the settlement
// itself is a little further back: the learner's "change it to 37 minutes" sits
// a few turns after the summary, while the newest turns are about something
// else. Keeping only the first messages is wrong for the same reason in the
// other direction.
//
// So the selection is: the scanned messages that mention the question's own
// terms, plus their immediate neighbour on each side (the update usually comes
// with a confirmation), then the newest messages as a fallback. Everything is
// returned oldest first, so the old proposal (in the segments) and its later
// replacement are visible in order.
func selectSegmentTail(messages []model.Message, terms []string, limit int) []model.Message {
	if limit <= 0 || len(messages) == 0 {
		return nil
	}
	normalized := make([]string, 0, len(terms))
	for _, term := range terms {
		term = strings.ToLower(strings.TrimSpace(term))
		if len([]rune(term)) >= 2 {
			normalized = append(normalized, term)
		}
	}
	matched := make(map[int]struct{}, limit)
	for index, message := range messages {
		content := strings.ToLower(message.Content)
		for _, term := range normalized {
			if strings.Contains(content, term) {
				matched[index] = struct{}{}
				if index > 0 {
					matched[index-1] = struct{}{}
				}
				if index+1 < len(messages) {
					matched[index+1] = struct{}{}
				}
				break
			}
		}
	}
	selected := make(map[int]struct{}, limit)
	for index := 0; index < len(messages) && len(selected) < limit; index++ {
		if _, ok := matched[index]; ok {
			selected[index] = struct{}{}
		}
	}
	for index := len(messages) - 1; index >= 0 && len(selected) < limit; index-- {
		selected[index] = struct{}{}
	}
	out := make([]model.Message, 0, len(selected))
	for index, message := range messages {
		if _, ok := selected[index]; ok {
			out = append(out, message)
		}
	}
	return out
}
