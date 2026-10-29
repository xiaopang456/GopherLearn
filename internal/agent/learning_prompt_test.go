package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/xiaopang456/GopherLearn/internal/contextwindow"
	"github.com/xiaopang456/GopherLearn/internal/model"
	"github.com/xiaopang456/GopherLearn/internal/responder"
)

// masteryHistory is the same transcript for both prompt below, so the only thing that can
// differ between them is the mastery note.
func masteryHistory() []responder.Turn {
	return []responder.Turn{
		{Role: model.RoleUser, Content: "上一问"},
		{Role: model.RoleAssistant, Content: "上一答"},
	}
}

// buildPrompt asks the one assembly point a learning-agent turn now has about its
// opening messages. It replaced a harness-local helper that built the same layout
// without measuring it; keeping the layout in responder.PromptMessages and the budget
// in contextwindow.Build is what stops the two from drifting apart.
func buildPrompt(t *testing.T, request responder.Request) []model.Message {
	t.Helper()
	messages, _, err := contextwindow.Build(contextwindow.Parts{
		SystemPrompt: SystemPrompt + learningContextNote(request.TopicMastery),
		Summary:      request.Summary,
		History:      request.History,
		Question:     request.Content,
	}, contextwindow.DefaultConfig())
	if err != nil {
		t.Fatalf("the prompt was refused: %v", err)
	}
	return messages
}

// TestMasteryNoteReachesOnlyTheAgentPrompt pins the two halves of one decision: the
// learning agent is told how well the learner has been doing, and the note is placed where
// a model reads it as server-written context rather than as something the user said.
func TestMasteryNoteReachesOnlyTheAgentPrompt(t *testing.T) {
	const note = "在《Redis 持久化》这个主题上，学习者最近的回答方向正确但还不完整。"
	noteless := buildPrompt(t, responder.Request{
		Content: "讲讲虚拟节点。",
		History: masteryHistory(),
	})
	withNote := buildPrompt(t, responder.Request{
		Content:      "讲讲虚拟节点。",
		History:      masteryHistory(),
		TopicMastery: note,
	})

	if strings.Contains(noteless[0].Content, "[学习状态]") {
		t.Fatal("an empty note still changed the prompt")
	}
	if noteless[0].Content != SystemPrompt {
		t.Fatal("the agent's system prompt must be exactly the constant when nothing is known")
	}
	if !strings.Contains(withNote[0].Content, note) || !strings.Contains(withNote[0].Content, "[学习状态]") {
		t.Fatalf("the note never reached the prompt:\n%s", withNote[0].Content)
	}
	// A blank note is the same as no note: a trailing marker with nothing after it
	// would tell the model to expect a judgement that is not there.
	blank := buildPrompt(t, responder.Request{
		Content:      "讲讲虚拟节点。",
		History:      masteryHistory(),
		TopicMastery: "   ",
	})
	if blank[0].Content != SystemPrompt {
		t.Fatalf("whitespace became prompt content:\n%s", blank[0].Content)
	}

	// The note goes in the system message and nowhere else, so the conversation the
	// learner wrote is untouched by it - and the shared prompt layout, which the chat and
	// knowledge strategies depend on byte for byte, is not what carries it.
	if len(withNote) != len(noteless) {
		t.Fatalf("the note changed the message count: %d vs %d", len(withNote), len(noteless))
	}
	for index := range withNote {
		if index == 0 {
			continue
		}
		if withNote[index].Content != noteless[index].Content {
			t.Fatalf("the note changed transcript message %d:\n%q\n%q", index, noteless[index].Content, withNote[index].Content)
		}
	}
}

// TestHarnessSendsTheMasteryNoteToTheModel runs the whole turn so the assertion is about
// what a model is actually handed, not about a helper in isolation.
func TestHarnessSendsTheMasteryNoteToTheModel(t *testing.T) {
	// The capability is registered but never called: the first reply is prose, so what
	// this test compares is the request the loop opened with, not a tool round trip.
	searchTool := func() *recordingTool {
		return &recordingTool{name: "knowledge_search", observations: []Observation{{Text: "not used", Found: true}}}
	}
	chat := &scriptedToolModel{replies: []model.Completion{answerReply("可以先说结论，再补一个反例。")}}
	harness := newHarness(t, chat, Budget{}, searchTool())

	request := baseRequest()
	request.TopicMastery = "内部判断：可以从边界情形开始。"
	if _, _, err := harness.Run(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if len(chat.requests) != 1 {
		t.Fatalf("want one model call, got %d", len(chat.requests))
	}
	first := chat.requests[0].Messages[0]
	if first.Role != model.RoleSystem || first.Content != SystemPrompt {
		t.Fatalf("the first model message is not the policy system prompt:\n%q", first.Content)
	}

	if !strings.Contains(contentsOf(chat.requests[0].Messages), request.TopicMastery) {
		t.Fatalf("the independently budgeted mastery block was not sent:\n%q", contentsOf(chat.requests[0].Messages))
	}
	plain := &scriptedToolModel{replies: []model.Completion{answerReply("可以先说结论，再补一个反例。")}}
	withoutNote := request
	withoutNote.TopicMastery = ""
	if _, _, err := newHarness(t, plain, Budget{}, searchTool()).Run(context.Background(), withoutNote); err != nil {
		t.Fatal(err)
	}
	if len(plain.requests) != 1 {
		t.Fatalf("want one model call, got %d", len(plain.requests))
	}
	if got := plain.requests[0].Messages[0].Content; got != SystemPrompt {
		t.Fatalf("a turn without a note still carries one:\n%q", got)
	}
	noted, unnoted := chat.requests[0].Messages, plain.requests[0].Messages
	if len(noted) != len(unnoted)+1 {
		t.Fatalf("the independently budgeted mastery block changed message count by more than one: %d vs %d", len(noted), len(unnoted))
	}
	if !strings.Contains(contentsOf(noted), request.TopicMastery) {
		t.Fatal("the mastery block is absent from the noted turn")
	}
}
