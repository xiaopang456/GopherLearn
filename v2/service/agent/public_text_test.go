package agent

import (
	"context"
	"github.com/cloudwego/eino/schema"
	"testing"
)

func TestPublicGenerateIsOneSnapshotAndOptInOnly(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		ctx := context.Background()
		var observed []PublicText
		if enabled {
			ctx = WithPublicTextObserver(ctx, func(_ context.Context, e PublicText) error { observed = append(observed, e); return nil })
		}
		inner := &scriptedChatModel{respond: func(context.Context, int) (*schema.Message, error) {
			return schema.AssistantMessage("complete public answer", nil), nil
		}}
		state := &runState{input: testRunContext(), config: DefaultRunnerConfig(), sink: DiscardEvents, steps: newMemoryStepRepository()}
		bound := &boundedModel{inner: inner, state: state}
		answer, err := bound.Generate(ctx, []*schema.Message{schema.UserMessage("question")})
		if err != nil || answer.Content != "complete public answer" || state.modelCalls != 1 {
			t.Fatalf("original Generate changed: %v %v", answer, err)
		}
		if !enabled {
			if len(observed) != 0 {
				t.Fatal("native opt-out publishes text")
			}
			continue
		}
		if len(observed) != 3 || observed[1].Content != answer.Content || observed[1].Publication != "generate_snapshot" || observed[2].Kind != "answer_candidate" {
			t.Fatalf("Generate claimed fragmented tokens: %v", observed)
		}
	}
}

func TestPublicObservationIgnoresPrivateMessageFieldsAndToolContent(t *testing.T) {
	var seen []PublicText
	c := &publicModelCall{publication: "model_stream", observer: func(_ context.Context, e PublicText) error { seen = append(seen, e); return nil }}
	ctx := context.Background()
	if err := c.accept(ctx, &schema.Message{Role: schema.Tool, Content: "private evidence payload"}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 0 {
		t.Fatal("tool evidence leaked")
	}
	if err := c.accept(ctx, &schema.Message{Role: schema.Assistant, Content: "public", Extra: map[string]any{"reasoning_content": "private reasoning"}, ToolCalls: []schema.ToolCall{{Function: schema.FunctionCall{Arguments: "private arguments"}}}}); err != nil {
		t.Fatal(err)
	}
	if err := c.end(ctx, false); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 3 || seen[1].Content != "public" || seen[2].Kind != "process" {
		t.Fatalf("privacy/process seam %v", seen)
	}
}
