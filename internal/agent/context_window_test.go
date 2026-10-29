package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/xiaopang456/GopherLearn/internal/contextwindow"
	"github.com/xiaopang456/GopherLearn/internal/model"
	"github.com/xiaopang456/GopherLearn/internal/responder"
)

// A tight window is what makes these assertions have teeth: with the shipped defaults
// a scripted turn is far too small to ever press against anything, and a check that is
// never reached cannot be shown to work.
func tightWindow(t *testing.T) contextwindow.Config {
	t.Helper()
	config, err := contextwindow.Config{
		WindowTokens:        contextwindow.MinWindowTokens,
		InputTargetTokens:   2048,
		OutputReserveTokens: 256,
		SummaryMaxTokens:    contextwindow.MinSummaryMaxTokens,
		HistoryMaxTokens:    contextwindow.MinHistoryMaxTokens,
		HistoryMaxMessages:  12,
		HistoryMaxRunes:     6000,
		HistoryFetchLimit:   16,
		EvidenceMaxTokens:   contextwindow.MinEvidenceMaxTokens,
	}.Resolve()
	if err != nil {
		t.Fatalf("the test window does not resolve: %v", err)
	}
	return config
}

func newWindowedHarness(t *testing.T, chat model.ToolChatModel, budget Budget,
	config contextwindow.Config, tools ...Tool,
) *Harness {
	t.Helper()
	registry, err := NewRegistry(tools...)
	if err != nil {
		t.Fatal(err)
	}
	harness, err := New(chat, registry, budget, WithContextWindow(config, 256))
	if err != nil {
		t.Fatal(err)
	}
	return harness
}

// estimatedInput is what the fake provider was actually handed, measured the way the
// budget measures it. Every assertion here reads the captured request rather than a
// counter the loop kept about itself, because the claim under test is about bytes on
// the wire.
func estimatedInput(t *testing.T, request model.Request) int {
	t.Helper()
	return contextwindow.RequestTokens(request)
}

// TestAgentHistoryAnswersToEveryCeilingAtOnce is the regression for the window not
// growing. The reference budget is what the same session was shown before the migration:
// twelve messages and six thousand runes, over whole exchanges, newest first, stopping at
// the first pair that does not fit. The estimated-token ceiling was added on top of that,
// so the binding limit can only ever be tighter. Each case names which ceiling binds, and
// a case where the agent shows more than the old window would is the bug this test exists
// to catch - six thousand estimated tokens is about twenty-four thousand runes of English
// text, so a token cap alone would have quietly widened the window for exactly the
// transcripts that are largest.
func TestAgentHistoryAnswersToEveryCeilingAtOnce(t *testing.T) {
	cases := []struct {
		name string
		// config overrides the default budget; empty means the shipped defaults.
		config  contextwindow.Config
		pairs   [][]string
		wantRun int
		binds   string
	}{
		{
			name: "three long english exchanges",
			// This is the case a token cap alone gets wrong. The old window kept one
			// pair because four thousand runes of English cost barely a thousand
			// tokens; a ceiling expressed only in tokens would have taken all three.
			pairs:   pairsOf(3, 2000, "english text that costs a quarter token per rune"),
			wantRun: 1,
			binds:   "runes",
		},
		{
			name:    "a tightened rune ceiling",
			config:  windowWithRunes(500),
			pairs:   pairsOf(10, 100, "abc"),
			wantRun: 2,
			binds:   "tightened runes",
		},
		{
			name:    "chinese exchanges cost the same in both units",
			pairs:   pairsOf(6, 1000, "记"),
			wantRun: 2,
			binds:   "tokens, the tighter of the two here",
		},
		{
			name:    "twenty short exchanges",
			pairs:   pairsOf(20, 8, "ab"),
			wantRun: 6,
			binds:   "the twelve-message ceiling",
		},
		{
			// The negative control for the first case: the same three exchanges with
			// room in the rune ceiling all go through. Without it, the first case
			// could be passing because of something other than the rune limit.
			name:    "the same english exchanges with room for them",
			config:  windowWithRunes(24000),
			pairs:   pairsOf(3, 2000, "english text that costs a quarter token per rune"),
			wantRun: 3,
			binds:   "only the token ceiling, which these fit",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			config := testCase.config
			if config.WindowTokens == 0 {
				config = contextwindow.DefaultConfig()
				config.HistoryMaxTokens = 6000
				config.HistoryMaxMessages = 12
				config.HistoryMaxRunes = 6000
			}
			chat := &scriptedToolModel{replies: []model.Completion{answerReply("回答。")}}
			harness := newWindowedHarness(t, chat, Budget{}, config,
				&recordingTool{name: MemorySearchName, observations: []Observation{{Text: "unused"}}})
			request := baseRequest()
			request.History = nil
			for _, pair := range testCase.pairs {
				request.History = append(request.History,
					responder.Turn{Role: model.RoleUser, Content: pair[0]},
					responder.Turn{Role: model.RoleAssistant, Content: pair[1]},
				)
			}
			if _, _, err := harness.Run(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			if len(chat.requests) != 1 {
				t.Fatalf("%d calls, want one", len(chat.requests))
			}
			got := chat.requests[0].Messages
			// Everything before the final user message, minus a leading system message
			// and any summary block, is the history the window carried.
			history := make([]model.Message, 0, len(got))
			for _, message := range got {
				if message.Role == model.RoleSystem || strings.HasPrefix(message.Content, responder.SummaryLabel) {
					continue
				}
				history = append(history, message)
			}
			// The last message is the current question, not history.
			if len(history) > 0 {
				history = history[:len(history)-1]
			}
			if len(history)/2 != testCase.wantRun {
				t.Fatalf("the window carried %d exchanges, want %d (bound: %s)",
					len(history)/2, testCase.wantRun, testCase.binds)
			}
			runes := 0
			for _, message := range history {
				runes += len([]rune(message.Content))
			}
			limits := config
			if runes > limits.HistoryMaxRunes {
				t.Fatalf("the window carried %d runes over a ceiling of %d", runes, limits.HistoryMaxRunes)
			}
			if len(history) > limits.HistoryMaxMessages {
				t.Fatalf("the window carried %d messages over a ceiling of %d",
					len(history), limits.HistoryMaxMessages)
			}
			if tokens := contextwindow.RequestTokens(model.Request{Messages: history}); tokens > limits.HistoryMaxTokens {
				t.Fatalf("the window carried %d estimated tokens over a ceiling of %d",
					tokens, limits.HistoryMaxTokens)
			}
			// And the exchanges kept are the newest ones, still in order.
			if len(history) > 0 {
				last := history[len(history)-1]
				want := testCase.pairs[len(testCase.pairs)-1][1]
				if last.Content != want {
					t.Fatalf("the newest exchange is not in the window: %q", firstRunes(last.Content, 24))
				}
			}
		})
	}
}

// TestAgentHistoryDropsAnUnansweredQuestionWithoutRepacking covers the failed turn that
// left a question with no answer: it must not enter the window, and its presence must not
// shift the pairings after it.
func TestAgentHistoryDropsAnUnansweredQuestionWithoutRepacking(t *testing.T) {
	chat := &scriptedToolModel{replies: []model.Completion{answerReply("回答。")}}
	harness := newWindowedHarness(t, chat, Budget{}, contextwindow.DefaultConfig(),
		&recordingTool{name: MemorySearchName, observations: []Observation{{Text: "unused"}}})
	request := baseRequest()
	request.History = []responder.Turn{
		{Role: model.RoleUser, Content: "甲问"},
		{Role: model.RoleAssistant, Content: "甲答"},
		{Role: model.RoleUser, Content: "乙问没人答"},
		{Role: model.RoleUser, Content: "丙问"},
		{Role: model.RoleAssistant, Content: "丙答"},
	}
	if _, _, err := harness.Run(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	var carried []string
	for _, message := range chat.requests[0].Messages {
		carried = append(carried, message.Content)
	}
	joined := strings.Join(carried, "\n")
	if strings.Contains(joined, "乙问没人答") {
		t.Fatalf("an unanswered question reached the model, inviting it to answer the same thing twice:\n%s", joined)
	}
	if !strings.Contains(joined, "甲问") || !strings.Contains(joined, "丙答") {
		t.Fatalf("a complete exchange was lost:\n%s", joined)
	}
	if strings.Count(joined, "丙问") != 1 {
		t.Fatal("the current question or a historical one was duplicated")
	}
}

// windowWithRunes moves only the rune ceiling, so the case that fails on it cannot be
// passing or failing because of a token ceiling someone also had to raise.
func windowWithRunes(runes int) contextwindow.Config {
	config := contextwindow.DefaultConfig()
	config.HistoryMaxTokens = 6000
	config.HistoryMaxMessages = 12
	config.HistoryMaxRunes = runes
	return config
}

// pairsOf builds n user/assistant exchanges whose text is roughly `runes` long.
func pairsOf(n, runes int, seed string) [][]string {
	pairs := make([][]string, 0, n)
	for index := 1; index <= n; index++ {
		pairs = append(pairs, []string{
			fmt.Sprintf("q%d %s", index, filler(runes, seed)),
			fmt.Sprintf("a%d %s", index, filler(runes, seed)),
		})
	}
	return pairs
}

func filler(runes int, seed string) string {
	if len(seed) == 0 {
		seed = "x"
	}
	var builder strings.Builder
	for len([]rune(builder.String())) < runes {
		builder.WriteString(seed)
	}
	text := builder.String()
	return string([]rune(text)[:runes])
}

func firstRunes(text string, count int) string {
	runes := []rune(text)
	if len(runes) <= count {
		return text
	}
	return string(runes[:count])
}

func TestAgentRefusesBeforeTheFirstCallWhenTheQuestionIsTooBig(t *testing.T) {
	chat := &scriptedToolModel{replies: []model.Completion{answerReply("不该发生")}}
	harness := newWindowedHarness(t, chat, Budget{}, tightWindow(t),
		&recordingTool{name: MemorySearchName, observations: []Observation{{Text: "不该发生", Found: true}}})
	request := baseRequest()
	request.Content = strings.Repeat("超", 2000)
	_, _, err := harness.Run(context.Background(), request)
	if !errors.Is(err, ErrContextBudget) || !errors.Is(err, contextwindow.ErrRequiredExceedsBudget) {
		t.Fatalf("err = %v, want a required-parts context refusal", err)
	}
	if len(chat.requests) != 0 {
		t.Fatalf("%d provider calls after a refusal: a turn that cannot fit must not be billed to find out",
			len(chat.requests))
	}
}

// The opening prompt is not the only one a turn sends. Each later call carries every
// observation it has already fed back, so the only check that bounds the turn is one
// taken on the request about to go out.
func TestAgentChecksEveryCallAndStopsWithoutPayingForAnother(t *testing.T) {
	bulky := Observation{
		Text:  ObservationLabel + " " + MemorySearchName + "：" + strings.Repeat("资料", 1600),
		Found: true,
	}
	chat := &scriptedToolModel{replies: []model.Completion{
		toolCallReply("call-1", MemorySearchName, `{"query":"记忆"}`),
		answerReply("不该被生成"),
	}, repeatLast: true}
	// The tool here is one that never consults the turn's allowance, which is what a
	// capability outside this package looks like. The loop's own per-call measurement
	// is then the only thing between the learner and a request past the window: a
	// budget whose sole guard is a tool's courtesy is not a budget.
	harness := newWindowedHarness(t, chat, Budget{MaxModelCalls: 4, MaxToolCalls: 4, MaxSteps: 8},
		tightWindow(t), &recordingTool{name: MemorySearchName, observations: []Observation{bulky}})
	_, usage, err := harness.Run(context.Background(), baseRequest())
	if !errors.Is(err, ErrContextBudget) || !errors.Is(err, contextwindow.ErrAssembledExceedsTarget) {
		t.Fatalf("err = %v, want an input-budget refusal on the second call", err)
	}
	if len(chat.requests) != 1 {
		t.Fatalf("%d calls reached the provider, want the one that produced the oversized observation",
			len(chat.requests))
	}
	if usage.ModelCalls != 1 {
		t.Fatalf("the refused call was still counted: %d", usage.ModelCalls)
	}
	// What was sent stays inside the window; what would not have been sent never was.
	// The oversized observation reached the server side transcript but no request, so
	// the turn ended rather than billing the learner for a call that must fail.
	for _, sent := range chat.requests {
		if estimatedInput(t, sent) > tightWindow(t).InputTargetTokens {
			t.Fatalf("a request of %d estimated tokens went out against a target of %d:\n%s",
				estimatedInput(t, sent), tightWindow(t).InputTargetTokens, rolesOf(sent.Messages))
		}
	}
	if strings.Contains(contentsOf(chat.requests[0].Messages), "资料资料") {
		t.Fatal("the oversized observation went out on a second call anyway")
	}
}

func TestAgentOpeningRequestStaysInsideTheWindow(t *testing.T) {
	config := tightWindow(t)
	chat := &scriptedToolModel{replies: []model.Completion{answerReply("直接回答。")}}
	harness := newWindowedHarness(t, chat, Budget{}, config,
		&recordingTool{name: MemorySearchName, observations: []Observation{{Text: "unused"}}})
	long := strings.Repeat("经", 400)
	request := baseRequest()
	request.Summary = strings.Repeat("摘", 5000)
	request.History = []responder.Turn{
		{Role: model.RoleUser, Content: long}, {Role: model.RoleAssistant, Content: long},
		{Role: model.RoleUser, Content: long}, {Role: model.RoleAssistant, Content: long},
	}
	_, usage, err := harness.Run(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(chat.requests) != 1 {
		t.Fatalf("%d calls, want one", len(chat.requests))
	}
	got := estimatedInput(t, chat.requests[0])
	if got > config.InputTargetTokens-config.EvidenceMaxTokens {
		t.Fatalf("the captured request estimates %d tokens, past the %d the window leaves for it",
			got, config.InputTargetTokens-config.EvidenceMaxTokens)
	}
	if usage.Context.AssembledTokens != got {
		t.Fatalf("the receipt says %d and the provider received %d", usage.Context.AssembledTokens, got)
	}
	if !usage.Context.SummaryTruncated {
		t.Fatal("a summary of 5000 CJK characters reached the model uncut inside a 128-token cap")
	}
	if usage.Context.HistoryDropped == 0 {
		t.Fatal("the window carried every exchange although the budget cannot hold them")
	}
	// The schemas are part of what a call carries, so they belong in what is measured.
	if len(chat.requests[0].Tools) == 0 {
		t.Fatal("the captured request holds no tool offers")
	}
}

func TestAgentSharesOneEvidenceAllowanceAcrossTwoCalls(t *testing.T) {
	config := mustResolve(contextwindow.Config{
		WindowTokens: contextwindow.MinWindowTokens, InputTargetTokens: 2048,
		OutputReserveTokens: 256, SummaryMaxTokens: contextwindow.MinSummaryMaxTokens,
		HistoryMaxTokens: contextwindow.MinHistoryMaxTokens, HistoryFetchLimit: 16,
		// One allowance for the whole turn, smaller than two per-call caps: a model
		// that searches twice must not send two full caps.
		EvidenceMaxTokens: 300,
	})
	entry := memoryEntry("全局记忆", strings.Repeat("长", 200), "mem-1")
	reader := &scriptedMemories{entries: []MemoryEntry{entry}}
	// The second ask carries a trailing space: an exact consecutive repeat is
	// refused before it runs, while the ledger's semantic identity - not the
	// fingerprint - is what recognises this as the same question and dedups
	// the record's body.
	chat := &scriptedToolModel{replies: []model.Completion{
		toolCallReply("call-1", MemorySearchName, `{"query":"记忆"}`),
		toolCallReply("call-2", MemorySearchName, `{"query":"记忆 "}`),
		answerReply("两次检索都读到了。"),
	}}
	harness := newWindowedHarness(t, chat, Budget{MaxModelCalls: 4, MaxToolCalls: 4, MaxSteps: 8}, config,
		newMemoryTool(t, reader, MemorySearchOptions{MaxEntries: 1, EntryRunes: 200, ObservationRunes: 4000}))
	_, usage, err := harness.Run(context.Background(), baseRequest())
	if err != nil {
		t.Fatal(err)
	}
	if len(chat.requests) != 3 {
		t.Fatalf("%d calls, want two search rounds and an answer", len(chat.requests))
	}
	// The second search hit the same record, so its body is not sent again. The
	// message list is one longer than the call index: the third request is the one
	// carrying the second observation.
	if first := chat.requests[1].Messages[len(chat.requests[1].Messages)-1].Content; strings.Contains(first, "已经完整给出") {
		t.Fatalf("the first search was treated as a repeat:\n%s", first)
	}
	second := chat.requests[2].Messages[len(chat.requests[2].Messages)-1].Content
	if !strings.Contains(second, "已经完整给出") {
		t.Fatalf("the repeated evidence was resent in full:\n%s", second)
	}
	if strings.Contains(second, "长长长长") {
		t.Fatalf("the repeat carried the body anyway:\n%s", second)
	}
	if usage.Context.EvidenceUsed > 300 {
		t.Fatalf("the turn spent %d against an allowance of 300", usage.Context.EvidenceUsed)
	}
	if usage.Context.EvidenceUsed == 0 {
		t.Fatal("the turn spent nothing, so the allowance was never consulted")
	}
}

func TestAgentStopsChargingEvidenceOnceTheAllowanceIsGone(t *testing.T) {
	config := mustResolve(contextwindow.Config{
		WindowTokens: contextwindow.MinWindowTokens, InputTargetTokens: 2048,
		OutputReserveTokens: 256, SummaryMaxTokens: contextwindow.MinSummaryMaxTokens,
		HistoryMaxTokens: contextwindow.MinHistoryMaxTokens, HistoryFetchLimit: 16,
		EvidenceMaxTokens: 128,
	})
	reader := &scriptedMemories{entries: []MemoryEntry{
		memoryEntry("全局记忆", strings.Repeat("长", 200), "mem-1"),
	}}
	chat := &scriptedToolModel{replies: []model.Completion{
		toolCallReply("call-1", MemorySearchName, `{"query":"记忆"}`),
		answerReply("没有找到依据。"),
	}}
	harness := newWindowedHarness(t, chat, Budget{}, config,
		newMemoryTool(t, reader, MemorySearchOptions{MaxEntries: 1, EntryRunes: 200, ObservationRunes: 4000}))
	_, _, err := harness.Run(context.Background(), baseRequest())
	if err != nil {
		t.Fatal(err)
	}
	observation := chat.requests[1].Messages[len(chat.requests[1].Messages)-1].Content
	if !strings.Contains(observation, "本轮资料额度已用完") {
		t.Fatalf("a refused passage was reported as a search that found nothing:\n%s", observation)
	}
	// No passage reached the model, so no citation may attach to the turn.
	if len(chat.requests[1].Messages) == 0 {
		t.Fatal("no observation was fed back at all")
	}
}

// A turn that stopped for want of context must not leave the transcript holding a tool
// request nobody answered. The loop refuses to *send*, so whatever it already assembled
// stays well-formed, and nothing is invented to close a call.
func TestToolProtocolSurvivesAContextRefusal(t *testing.T) {
	config := tightWindow(t)
	chat := &scriptedToolModel{replies: []model.Completion{
		{FinishReason: model.FinishToolCalls, ToolCalls: []model.ToolCall{
			{ID: "call-a", Name: MemorySearchName, Arguments: `{"query":"甲"}`},
			{ID: "call-b", Name: MemorySearchName, Arguments: `{"query":"乙"}`},
		}},
		answerReply("不该到达"),
	}, repeatLast: true}
	reader := &scriptedMemories{entries: []MemoryEntry{
		memoryEntry("甲", "第一条", "mem-1"), memoryEntry("乙", "第二条", "mem-2"),
	}}
	harness := newWindowedHarness(t, chat, Budget{MaxModelCalls: 4}, config,
		newMemoryTool(t, reader, MemorySearchOptions{MaxEntries: 1, ObservationRunes: 100}))
	_, _, err := harness.Run(context.Background(), baseRequest())
	if err != nil && !errors.Is(err, ErrContextBudget) {
		t.Fatalf("err = %v", err)
	}
	for _, request := range chat.requests {
		answered := map[string]bool{}
		for _, message := range request.Messages {
			if message.Role == model.RoleTool {
				if strings.TrimSpace(message.ToolCallID) == "" {
					t.Fatal("a tool message carries no call id")
				}
				answered[message.ToolCallID] = true
			}
		}
		for _, message := range request.Messages {
			for _, call := range message.ToolCalls {
				if !answered[call.ID] {
					t.Fatalf("call %q was sent without its result in the same request:\n%s",
						call.ID, rolesOf(request.Messages))
				}
			}
		}
	}
}

func TestConcurrentTurnsDoNotShareOneEvidenceAllowance(t *testing.T) {
	config := mustResolve(contextwindow.Config{
		WindowTokens: contextwindow.MinWindowTokens, InputTargetTokens: 4096,
		OutputReserveTokens: 256, SummaryMaxTokens: contextwindow.MinSummaryMaxTokens,
		HistoryMaxTokens: 1024, HistoryFetchLimit: 16, EvidenceMaxTokens: 400,
	})
	var wg sync.WaitGroup
	results := make([]int, 4)
	for index := range results {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			reader := &scriptedMemories{entries: []MemoryEntry{
				memoryEntry("并发", strings.Repeat("长", 200), "mem-concurrent"),
			}}
			chat := &scriptedToolModel{replies: []model.Completion{
				toolCallReply("call-1", MemorySearchName, `{"query":"记忆"}`),
				answerReply("各自读到了。"),
			}}
			harness := newWindowedHarness(t, chat, Budget{}, config,
				newMemoryTool(t, reader, MemorySearchOptions{MaxEntries: 1, EntryRunes: 200, ObservationRunes: 4000}))
			request := baseRequest()
			request.UserID = "user-" + string(rune('a'+index))
			_, usage, err := harness.Run(context.Background(), request)
			if err != nil {
				t.Errorf("turn %d: %v", index, err)
				return
			}
			// Every turn gets its whole allowance, not whatever the others left.
			results[index] = usage.Context.EvidenceUsed
		}(index)
	}
	wg.Wait()
	for index, used := range results {
		if used < 200 {
			t.Fatalf("turn %d spent %d evidence tokens while four turns ran at once", index, used)
		}
	}
}

func TestCancelledTurnDoesNotAskTheModelOrAToolAgain(t *testing.T) {
	config := tightWindow(t)
	cancelTool := &cancellingTool{name: MemorySearchName,
		observation: Observation{Text: ObservationLabel + " 一条", Found: true}}
	ctx, cancel := context.WithCancel(context.Background())
	cancelTool.cancel = cancel
	chat := &scriptedToolModel{replies: []model.Completion{
		toolCallReply("call-1", MemorySearchName, `{"query":"记忆"}`),
		answerReply("不该到达"),
	}, repeatLast: true}
	harness := newWindowedHarness(t, chat, Budget{MaxModelCalls: 4}, config, cancelTool)
	_, _, err := harness.Run(ctx, baseRequest())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the caller's cancellation", err)
	}
	if len(chat.requests) != 1 {
		t.Fatalf("%d calls after the caller hung up, want the one that asked for the cancelled work",
			len(chat.requests))
	}
}

type cancellingTool struct {
	name        string
	observation Observation
	calls       int
	cancel      context.CancelFunc
}

func (t *cancellingTool) Name() string                { return t.name }
func (t *cancellingTool) Description() string         { return "test capability" }
func (t *cancellingTool) Parameters() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t *cancellingTool) Execute(context.Context, Principal, json.RawMessage) (Observation, error) {
	t.calls++
	t.cancel()
	return t.observation, nil
}

func mustResolve(config contextwindow.Config) contextwindow.Config {
	resolved, err := config.Resolve()
	if err != nil {
		panic(err)
	}
	return resolved
}

func contentsOf(messages []model.Message) string {
	var builder strings.Builder
	for _, message := range messages {
		builder.WriteString(message.Content)
		builder.WriteString("\n")
	}
	return builder.String()
}

func rolesOf(messages []model.Message) string {
	names := make([]string, 0, len(messages))
	for _, message := range messages {
		names = append(names, message.Role)
	}
	return strings.Join(names, " ")
}
