package agent

import (
	"context"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
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

// readB3LeakedToolCall is the exact Content a B3 attempt received from the
// model on 2026-11-20, copied from
// r3-targeted-b/out/B3-r1/turn1/record.jsonl. It is the SINGLE-bar spelling of
// the protocol, which the old filter (doubled bar only) published verbatim as
// the learner's answer.
func readB3LeakedToolCall(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("testdata/b3-single-pipe-tool-call.txt")
	if err != nil {
		t.Fatalf("read the B3 raw output: %v", err)
	}
	return string(raw)
}

func TestSafeModelTextReplacesEverySpellingOfTheToolProtocol(t *testing.T) {
	raw := readB3LeakedToolCall(t)
	for _, tc := range []struct {
		name, text string
		replace    bool
	}{
		{"the B3 raw output (single bar)", raw, true},
		{"doubled bar", "<｜｜DSML｜｜ calls>", true},
		{"ASCII bar", "<|DSML| invoke name=\"x\">", true},
		{"lowercase marker word", "<｜dsml｜ invoke>", true},
		{"marker inside prose", "前文" + raw + "后文", true},
		{"the ordinary answer", "第二道是依赖裁剪。", false},
		{"the word without bars", "DSML 是一个协议名。", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := safeModelText(tc.text)
			if tc.replace {
				if got != malformedToolCallAnswer {
					t.Fatalf("a leaked protocol text was published: %q", got)
				}
				return
			}
			if got != tc.text {
				t.Fatalf("an ordinary answer was rewritten: %q -> %q", tc.text, got)
			}
		})
	}
}

func TestTheToolProtocolNeverLeaksAcrossChunkBoundaries(t *testing.T) {
	raw := readB3LeakedToolCall(t)
	cases := map[string]string{
		"the B3 raw output (single bar)": raw,
		"doubled bar":                    "<｜｜DSML｜｜ invoke name=\"x\">",
		"lowcased marker word":           "<｜dsml｜ invoke>",
		"marker inside prose":            "前文" + raw + "后文",
		"marker ends the stream":         "前文<｜DS",
	}
	for name, text := range cases {
		for _, size := range []int{1, 2, 3, 7} {
			t.Run(name, func(t *testing.T) {
				var observed []PublicText
				c := &publicModelCall{observer: func(_ context.Context, e PublicText) error {
					observed = append(observed, e)
					return nil
				}}
				runes := []rune(text)
				for i := 0; i < len(runes); i += size {
					end := i + size
					if end > len(runes) {
						end = len(runes)
					}
					if err := c.accept(context.Background(), schema.AssistantMessage(string(runes[i:end]), nil)); err != nil {
						t.Fatal(err)
					}
				}
				if err := c.end(context.Background(), false); err != nil {
					t.Fatal(err)
				}
				fallbacks := 0
				for _, e := range observed {
					if e.Content == malformedToolCallAnswer {
						fallbacks++
						continue
					}
					if strings.Contains(strings.ToUpper(e.Content), "DSML") {
						t.Fatalf("protocol text was published: %q", e.Content)
					}
				}
				if fallbacks != 1 {
					t.Fatalf("the failure message was published %d times, want once: %v", fallbacks, observed)
				}
			})
		}
	}
}

func TestAnOrdinaryTrailingBarIsStillPublished(t *testing.T) {
	var observed []PublicText
	c := &publicModelCall{observer: func(_ context.Context, e PublicText) error {
		observed = append(observed, e)
		return nil
	}}
	text := "第二道是依赖裁剪。"
	if err := c.accept(context.Background(), schema.AssistantMessage(text+"｜", nil)); err != nil {
		t.Fatal(err)
	}
	if err := c.end(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	var published strings.Builder
	for _, e := range observed {
		published.WriteString(e.Content)
	}
	if got := published.String(); got != text+"｜" {
		t.Fatalf("published %q, want the learner's text unchanged", got)
	}
}

func TestASnapshotEndingInAPartialMarkerIsNotPublished(t *testing.T) {
	var observed []PublicText
	c := &publicModelCall{observer: func(_ context.Context, e PublicText) error {
		observed = append(observed, e)
		return nil
	}}
	// A non-streaming generation can hand the whole text back at once, so the
	// partial marker sits in the buffer with prose in front of it.
	if err := c.accept(context.Background(), schema.AssistantMessage("前文<｜DS", nil)); err != nil {
		t.Fatal(err)
	}
	if err := c.end(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	fallbacks := 0
	for _, e := range observed {
		if e.Content == malformedToolCallAnswer {
			fallbacks++
			continue
		}
		if strings.Contains(strings.ToUpper(e.Content), "DSML") {
			t.Fatalf("protocol text was published: %q", e.Content)
		}
	}
	if fallbacks != 1 {
		t.Fatalf("the failure message was published %d times, want once: %v", fallbacks, observed)
	}
}

func TestTheLeakedToolCallIsAnAnswerFailureAndNotAStructuredCall(t *testing.T) {
	raw := readB3LeakedToolCall(t)
	fake := &scriptedChatModel{respond: func(context.Context, int) (*schema.Message, error) {
		// The provider hands the call back as Content, with no ToolCall. The
		// boundary must neither execute it nor show it.
		return schema.AssistantMessage(raw, nil), nil
	}}
	var count atomic.Int32
	config := DefaultRunnerConfig()
	runner := mustRunner(t, fake, StaticToolRegistry{Registered: []tool.BaseTool{mustFakeTool(t, &count)}}, newMemoryStepRepository(), config)
	result, err := runner.Run(context.Background(), testRunContext(), DiscardEvents)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToUpper(result.Answer), "DSML") {
		t.Fatalf("answer=%q leaks the protocol", result.Answer)
	}
	if !strings.Contains(result.Answer, malformedToolCallAnswer) {
		t.Fatalf("answer=%q, want the explicit failure message", result.Answer)
	}
	if count.Load() != 0 {
		t.Fatalf("a tool ran %d times from text the model wrote", count.Load())
	}
}
