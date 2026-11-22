package agenttool

import (
	"sort"
	"strings"

	"GopherAI/model"
	serviceagent "GopherAI/service/agent"
	"GopherAI/service/episode"
)

// retrievalQuestion is the question retrieval words are read from.
//
// On a clarification continuation the request's Question is the learner's
// reply ("p2c-07"), and the question they were actually asking is the one the
// clarification was asked about. That original question travels on the run.
func retrievalQuestion(run serviceagent.RunContext) string {
	if original := strings.TrimSpace(run.OriginalQuestion); original != "" {
		return original
	}
	return strings.TrimSpace(run.Question)
}

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

// selectTail chooses the tail with the learner's question first.
//
// Order of the word sources:
//
//  1. the question's own terms (the original question on a continuation);
//  2. the tokenizer's cut of that question;
//  3. the model's tool query — the question's words may not occur in the
//     candidate bodies at all, and the model's query is what still names the
//     subject;
//  4. no words: newest messages only.
func selectTail(messages []model.Message, question, modelQuery string, limit int) []model.Message {
	primary := tailSelectionTerms(question, "")
	if tailTermsMatch(messages, primary) {
		return selectSegmentTail(messages, primary, limit)
	}
	if fallback := thinCardFallbackTerms(question); len(fallback) > 0 && tailTermsMatch(messages, fallback) {
		return selectSegmentTail(messages, fallback, limit)
	}
	if modelTerms := tailSelectionTerms("", modelQuery); len(modelTerms) > 0 && tailTermsMatch(messages, modelTerms) {
		return selectSegmentTail(messages, modelTerms, limit)
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

// selectSegmentTail chooses the evidence for the answer within one budget.
//
// Selection order and display order are separate:
//
//   - DIRECT hits first: a message that mentions a term of the question beats
//     a neighbour that was only pulled in by one. Among direct hits the newer
//     message wins, because a later message may be the update of an earlier
//     one and an oldest-first budget would let the earlier matches crowd it
//     out.
//   - Then their immediate neighbours (a proposal usually comes with the
//     confirmation beside it), also newer first.
//   - Then the newest messages fill any remaining budget.
//
// The selected evidence is finally returned oldest first, so the model reads
// it as a timeline: the old value, then the update.
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
	direct := make([]int, 0, limit)
	for index, message := range messages {
		content := strings.ToLower(message.Content)
		for _, term := range normalized {
			if strings.Contains(content, term) {
				direct = append(direct, index)
				break
			}
		}
	}
	sort.Sort(sort.Reverse(sort.IntSlice(direct)))
	selected := make(map[int]struct{}, limit)
	for _, index := range direct {
		if len(selected) >= limit {
			break
		}
		selected[index] = struct{}{}
	}
	// Neighbours of the direct hits, newer first.
	neighbours := make([]int, 0, len(direct)*2)
	seenNeighbour := make(map[int]struct{}, len(direct)*2)
	for _, index := range direct {
		for _, delta := range []int{-1, 1} {
			candidate := index + delta
			if candidate < 0 || candidate >= len(messages) {
				continue
			}
			if _, already := selected[candidate]; already {
				continue
			}
			if _, duplicate := seenNeighbour[candidate]; duplicate {
				continue
			}
			seenNeighbour[candidate] = struct{}{}
			neighbours = append(neighbours, candidate)
		}
	}
	sort.Sort(sort.Reverse(sort.IntSlice(neighbours)))
	for _, index := range neighbours {
		if len(selected) >= limit {
			break
		}
		selected[index] = struct{}{}
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
