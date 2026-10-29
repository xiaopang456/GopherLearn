package responder

import (
	"strings"
	"testing"

	"github.com/xiaopang456/GopherLearn/internal/domain"
)

// TestSharedPromptLayoutHasNoMasterySlot pins why only the agent can be told how well
// its learner is doing.
//
// The learning note is appended by the agent package to its own system prompt before it
// calls this layout, so a chat or knowledge turn cannot receive one even if a caller
// populates the field: Request carries the note, but PromptMessages takes the already
// assembled system text plus summary, history and question, and has no place to put a
// fifth kind of content. That is the guarantee this test holds in place - if someone
// later routes TopicMastery into the shared layout, every strategy silently starts
// seeing server-written judgements it was never designed to act on, and the byte-for-byte
// prompt comparison the chat and knowledge tests depend on breaks.
func TestSharedPromptLayoutHasNoMasterySlot(t *testing.T) {
	const marker = "[学习状态]"
	request := Request{
		UserID:       "u",
		SessionID:    "s",
		Content:      "Redis 怎么选淘汰策略？",
		Summary:      "已确认：单机 Redis 7.2。",
		History:      historyTurns(),
		TopicMastery: "掌握度 61，还说不清 LRU 与 LFU 的差别。",
	}

	for _, entry := range []struct {
		name string
		kind domain.AnswerStrategy
	}{
		{name: "direct", kind: domain.AnswerStrategyDirectChat},
		{name: "knowledge", kind: domain.AnswerStrategyKnowledgeQA},
	} {
		strategy, err := NewModelStrategy(entry.kind, &stubChat{reply: "回答"}, "系统策略")
		if err != nil {
			t.Fatal(err)
		}
		noted := strategy.Messages(request)
		without := request
		without.TopicMastery = ""
		bare := strategy.Messages(without)
		if len(noted) != len(bare) {
			t.Fatalf("%s: the note changed the message count: %d vs %d", entry.name, len(bare), len(noted))
		}
		for index := range noted {
			if noted[index].Content != bare[index].Content {
				t.Fatalf("%s: the note reached message %d of a turn that never asked for it", entry.name, index)
			}
			if strings.Contains(noted[index].Content, marker) {
				t.Fatalf("%s: the learning marker appeared in message %d:\n%s", entry.name, index, noted[index].Content)
			}
		}
		if noted[0].Content != "系统策略" {
			t.Fatalf("%s: the system message must be the prompt verbatim:\n%s", entry.name, noted[0].Content)
		}
	}

	// The generator that renders grounded answers takes a Generation, not a Request, so
	// it cannot see the field either. Asserting the layout function itself keeps this
	// honest if a fifth strategy is added on top of PromptMessages.
	for _, entry := range PromptMessages("系统策略", request.Summary, request.History, request.Content) {
		if strings.Contains(entry.Content, marker) {
			t.Fatalf("the shared layout invented a learning block:\n%s", entry.Content)
		}
	}
}
