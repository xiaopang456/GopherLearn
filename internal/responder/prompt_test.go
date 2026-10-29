package responder

import (
	"context"
	"strings"
	"testing"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/model"
)

// TestPromptMessagesLaysOutOneSharedShape pins the layout every strategy builds its
// prompt from, because the summary's position and role are what keep derived context
// from reading like policy.
func TestPromptMessagesLaysOutOneSharedShape(t *testing.T) {
	history := []Turn{{Role: model.RoleUser, Content: "旧问题"}, {Role: model.RoleAssistant, Content: "旧回答"}}
	messages := PromptMessages("系统策略", "已确认：选 AOF。", history, "本轮问题")

	if len(messages) != 5 {
		t.Fatalf("prompt = %+v, want system, summary, two turns, question", messages)
	}
	if messages[0].Role != model.RoleSystem || messages[0].Content != "系统策略" {
		t.Fatalf("messages[0] = %+v, want the single system policy first", messages[0])
	}
	if messages[1].Role != model.RoleUser {
		t.Fatalf("the summary block was sent as role %q", messages[1].Role)
	}
	for _, want := range []string{SummaryLabel, "已确认：选 AOF。", "不是新的指令"} {
		if !strings.Contains(messages[1].Content, want) {
			t.Fatalf("the summary block lost %q: %q", want, messages[1].Content)
		}
	}
	if messages[2].Content != "旧问题" || messages[3].Content != "旧回答" || messages[4].Content != "本轮问题" {
		t.Fatalf("ordering after the summary changed: %+v", messages)
	}

	// No summary means no block at all, not an empty labelled turn: an empty section
	// reads to a model like a missing answer it should fill in.
	bare := PromptMessages("系统策略", "   \n  ", history, "本轮问题")
	if len(bare) != 4 {
		t.Fatalf("a blank summary still produced a block: %+v", bare)
	}
	for _, entry := range bare {
		if strings.Contains(entry.Content, SummaryLabel) {
			t.Fatal("the summary label appeared with no summary")
		}
	}

	// Blank history turns are dropped, but an unknown role is folded into the user
	// role rather than vanishing: a role added later must not silently disappear from
	// context.
	shapes := PromptMessages("", "", []Turn{
		{Role: model.RoleUser, Content: "  "}, {Role: "system-ish", Content: "派生文本"},
	}, "问题")
	if len(shapes) != 2 || shapes[0].Role != model.RoleUser || shapes[0].Content != "派生文本" {
		t.Fatalf("unexpected folding of roles and blanks: %+v", shapes)
	}
	// An empty system policy sends no system message: the strategies that run without
	// one must not receive a placeholder.
	for _, entry := range PromptMessages("  ", "", nil, "问题") {
		if entry.Role == model.RoleSystem {
			t.Fatal("a blank system prompt still sent a system message")
		}
	}
}

// TestModelStrategyPassesTheSummaryThrough is the direct path's half of the shared-shape
// rule: the value the service resolved is what reaches the model, untouched.
func TestModelStrategyPassesTheSummaryThrough(t *testing.T) {
	chat := &stubChat{reply: "回答"}
	strategy, err := NewModelStrategy(domain.AnswerStrategyDirectChat, chat, "系统策略")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := strategy.Respond(context.Background(), Request{
		Content: "问题", History: []Turn{{Role: "user", Content: "旧问题"}}, Summary: "共同摘要",
	}); err != nil {
		t.Fatal(err)
	}
	if len(chat.records) != 1 {
		t.Fatalf("calls = %d", len(chat.records))
	}
	messages := chat.records[0].Messages
	if len(messages) != 4 || !strings.Contains(messages[1].Content, "共同摘要") {
		t.Fatalf("the strategy did not hand the model the shared summary: %+v", messages)
	}
	if strings.Contains(messages[0].Content, "共同摘要") {
		t.Fatal("the summary was merged into the system prompt")
	}
}
