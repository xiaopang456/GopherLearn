package agenttool

import (
	"strings"

	"GopherAI/model"
)

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
