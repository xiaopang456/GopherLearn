package rag

import (
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
)

func TestBuildRAGPromptWithoutDocumentsRefusesUngroundedAnswer(t *testing.T) {
	t.Parallel()

	const query = "RabbitMQ autoAck=true 有什么风险？"
	if got := BuildRAGPrompt(query, nil); got != NoGroundingAnswer {
		t.Fatalf("BuildRAGPrompt() = %q, want refusal %q", got, NoGroundingAnswer)
	}
}

func TestBuildRAGPromptBaselineCharacterization(t *testing.T) {
	t.Parallel()

	docs := []*schema.Document{
		{
			ID:      "doc_1",
			Content: "autoAck=true 时，消息交付给消费者后会立即确认。",
			MetaData: map[string]any{
				"source": "work/day12.md",
			},
		},
	}

	got := BuildRAGPrompt("autoAck 有什么风险？", docs)

	for _, want := range []string{
		"[来源 1]",
		"autoAck=true 时，消息交付给消费者后会立即确认。",
		"用户问题：autoAck 有什么风险？",
		"day12.md",
		"不得把模型常识伪装成用户笔记内容",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("BuildRAGPrompt() does not contain %q\nprompt:\n%s", want, got)
		}
	}

	if strings.Contains(got, "work/day12.md") {
		t.Errorf("prompt leaked a source path instead of a safe file name: %s", got)
	}
}
