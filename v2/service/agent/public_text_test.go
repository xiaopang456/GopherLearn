package agent

import (
	"context"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"strings"
	"sync/atomic"
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

func TestMalformedToolCallIsNeverPublished(t *testing.T) {
	const markup = `<｜｜DSML｜｜ calls>
<｜｜DSML｜｜ invoke name="knowledge_search">
<｜｜DSML｜｜ parameter name="query" string="true">Redis 持久化 RDB AOF 学习顺序</｜｜DSML｜｜ parameter>
<｜｜DSML｜｜ parameter name="max_results" string="false">5</｜｜DSML｜｜ parameter>
</｜｜DSML｜｜ invoke>
</｜｜DSML｜｜ calls>`
	for _, tc := range []struct {
		name, text string
		calls      int
	}{{"budget available", markup, 4}, {"budget exhausted", markup, 1}, {"inside prose", "前文" + markup + "后文", 4}} {
		t.Run(tc.name, func(t *testing.T) {
			var observed []PublicText
			ctx := WithPublicTextObserver(context.Background(), func(_ context.Context, e PublicText) error { observed = append(observed, e); return nil })
			c := &publicModelCall{observer: func(_ context.Context, e PublicText) error { observed = append(observed, e); return nil }}
			runes := []rune(tc.text)
			for i := 0; i < len(runes); i += 3 {
				end := i + 3
				if end > len(runes) {
					end = len(runes)
				}
				if err := c.accept(ctx, schema.AssistantMessage(string(runes[i:end]), nil)); err != nil {
					t.Fatal(err)
				}
			}
			if err := c.end(ctx, false); err != nil {
				t.Fatal(err)
			}
			foundFallback := false
			for _, e := range observed {
				if e.Content == malformedToolCallAnswer {
					foundFallback = true
				}
			}
			if !foundFallback {
				t.Fatalf("observed=%v", observed)
			}
			fake := &scriptedChatModel{respond: func(context.Context, int) (*schema.Message, error) { return schema.AssistantMessage(tc.text, nil), nil }}
			config := DefaultRunnerConfig()
			config.MaxModelCalls = tc.calls
			var count atomic.Int32
			runner := mustRunner(t, fake, StaticToolRegistry{Registered: []tool.BaseTool{mustFakeTool(t, &count)}}, newMemoryStepRepository(), config)
			result, err := runner.Run(ctx, testRunContext(), DiscardEvents)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(result.Answer, "｜｜DSML｜｜") || strings.TrimSpace(result.Answer) == "" {
				t.Fatalf("answer=%q", result.Answer)
			}
			for _, e := range observed {
				if strings.Contains(e.Content, "｜｜DSML｜｜") {
					t.Fatalf("published=%q", e.Content)
				}
			}
		})
	}
}
