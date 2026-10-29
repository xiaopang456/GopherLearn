package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/model"
	"github.com/xiaopang456/GopherLearn/internal/responder"
)

// scriptedToolModel replays a prepared sequence of completions and records every
// request it received, so a test can see exactly what the loop put back on the wire.
type scriptedToolModel struct {
	replies  []model.Completion
	requests []model.Request
	// repeatLast keeps answering with the final reply once the script runs out, which
	// is how a model that never stops asking for tools is simulated.
	repeatLast bool
	// varyRepeat rewrites the repeated call's query with the round number. The
	// loop refuses an exact consecutive repeat - same tool, same canonical
	// arguments - the way the reference project does, so a script that wants to
	// exercise the budgets past round two must keep asking with fresh
	// arguments, which is what a looping model actually does.
	varyRepeat bool
	// block holds a call open long enough for the turn's deadline to matter, which is
	// the only way to test a time budget without sleeping in the assertion.
	block time.Duration
	err   error
}

func (m *scriptedToolModel) CompleteWithTools(ctx context.Context, request model.Request) (model.Completion, error) {
	if err := ctx.Err(); err != nil {
		return model.Completion{}, err
	}
	if m.block > 0 {
		select {
		case <-time.After(m.block):
		case <-ctx.Done():
			return model.Completion{}, ctx.Err()
		}
	}
	if m.err != nil {
		return model.Completion{}, m.err
	}
	m.requests = append(m.requests, request)
	index := len(m.requests) - 1
	if index >= len(m.replies) {
		if !m.repeatLast {
			return model.Completion{}, errors.New("test script ran out of replies")
		}
		index = len(m.replies) - 1
	}
	reply := m.replies[index]
	if m.repeatLast && m.varyRepeat && index == len(m.replies)-1 {
		// Same tool, fresh arguments: a consecutive repeat with identical
		// arguments is refused before it runs, so the budget drills rewrite the
		// first string argument per round.
		varying := make([]model.ToolCall, len(reply.ToolCalls))
		for i, call := range reply.ToolCalls {
			varying[i] = call
			varying[i].Arguments = varyArguments(call.Arguments, len(m.requests))
		}
		reply.ToolCalls = varying
	}
	return reply, nil
}

// varyArguments rewrites one free-text argument of the call with a round
// suffix, keeping every declared field and enum value intact. The preferred
// keys are the free-text ones the tools declare; a call without any falls back
// to its longest string value.
func varyArguments(raw string, round int) string {
	var fields map[string]any
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		return raw
	}
	for _, preferred := range []string{"query", "content", "text"} {
		if text, ok := fields[preferred].(string); ok {
			fields[preferred] = fmt.Sprintf("%s-%d", text, round)
			return marshalArguments(fields, raw)
		}
	}
	longestKey, longest := "", ""
	for key, value := range fields {
		if text, ok := value.(string); ok && len(text) > len(longest) {
			longestKey, longest = key, text
		}
	}
	if longestKey == "" {
		return raw
	}
	fields[longestKey] = fmt.Sprintf("%s-%d", longest, round)
	return marshalArguments(fields, raw)
}

func marshalArguments(fields map[string]any, raw string) string {
	encoded, err := json.Marshal(fields)
	if err != nil {
		return raw
	}
	return string(encoded)
}

func (m *scriptedToolModel) Complete(context.Context, model.Request) (model.Completion, error) {
	return model.Completion{}, errors.New("the harness must not use the tool-free completion path")
}

func (m *scriptedToolModel) Stream(context.Context, model.Request, model.DeltaHandler) (model.Completion, error) {
	return model.Completion{}, errors.New("the harness must not stream its decision calls")
}

var _ model.ToolChatModel = (*scriptedToolModel)(nil)

func toolCallReply(id, name, arguments string) model.Completion {
	return model.Completion{
		FinishReason: model.FinishToolCalls,
		ToolCalls:    []model.ToolCall{{ID: id, Name: name, Arguments: arguments}},
	}
}

func answerReply(content string) model.Completion {
	return model.Completion{Content: content, FinishReason: model.FinishStop}
}

// recordingTool is a server-side capability a test controls: what it returns, or the
// error it fails with, plus a record of who asked.
type recordingTool struct {
	name         string
	observations []Observation
	failWith     error
	calls        []recordedCall
}

type recordedCall struct {
	principal Principal
	arguments string
}

func (t *recordingTool) Name() string        { return t.name }
func (t *recordingTool) Description() string { return "test capability" }
func (t *recordingTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","additionalProperties":false}`)
}

func (t *recordingTool) Execute(ctx context.Context, principal Principal, arguments json.RawMessage) (Observation, error) {
	if err := ctx.Err(); err != nil {
		return Observation{}, err
	}
	if t.failWith != nil {
		return Observation{}, t.failWith
	}
	t.calls = append(t.calls, recordedCall{principal: principal, arguments: string(arguments)})
	index := len(t.calls) - 1
	if index >= len(t.observations) {
		index = len(t.observations) - 1
	}
	return t.observations[index], nil
}

func sourceFor(chunkID string) domain.Source {
	return domain.Source{
		SourceID:     "src-" + chunkID,
		DocumentID:   "doc-" + chunkID,
		CollectionID: "col-owner",
		ChunkID:      chunkID,
		Title:        "aof.md",
		Snippet:      "AOF 的 fsync 策略…",
	}
}

func newHarness(t *testing.T, chat model.ToolChatModel, budget Budget, tools ...Tool) *Harness {
	t.Helper()
	registry, err := NewRegistry(tools...)
	if err != nil {
		t.Fatal(err)
	}
	harness, err := New(chat, registry, budget)
	if err != nil {
		t.Fatal(err)
	}
	return harness
}

func baseRequest() responder.Request {
	return responder.Request{
		UserID:         "user-owner",
		SessionID:      "session-1",
		Content:        "AOF 的 fsync 策略可以选哪些？",
		SessionMode:    domain.SessionModeLearningAgent,
		AnswerStrategy: domain.AnswerStrategyAgent,
		History:        []responder.Turn{{Role: model.RoleUser, Content: "上一轮的问题"}, {Role: model.RoleAssistant, Content: "上一轮的回答"}},
	}
}

func TestHarnessRunsAToolAndAnswersWithRealCitations(t *testing.T) {
	chat := &scriptedToolModel{replies: []model.Completion{
		toolCallReply("call-1", "knowledge_search", `{"query":"AOF fsync"}`),
		answerReply("可以选 always、everysec 或 no。"),
	}}
	search := &recordingTool{name: "knowledge_search", observations: []Observation{{
		Text:    ObservationLabel + " knowledge_search：检索到 1 条资料\n[1] aof.md › AOF 持久化\nfsync 可选 always、everysec、no。",
		Sources: []domain.Source{sourceFor("chunk-1")},
		Found:   true,
	}}}
	harness := newHarness(t, chat, Budget{}, search)

	answer, spent, err := harness.Run(context.Background(), baseRequest())
	if err != nil {
		t.Fatal(err)
	}
	if answer.Content != "可以选 always、everysec 或 no。" {
		t.Fatalf("answer = %q", answer.Content)
	}
	if len(answer.Sources) != 1 || answer.Sources[0].SourceID != "src-chunk-1" {
		t.Fatalf("citations = %+v, want the one the tool actually retrieved", answer.Sources)
	}
	if spent.ModelCalls != 2 || spent.ToolCalls != 1 || spent.Steps != 2 || spent.ToolRounds != 1 {
		t.Fatalf("usage = %+v", spent)
	}
	// What went back to the model is the transcript a tool loop needs: the call it
	// made, then the result attributed to that call, in that order.
	if len(chat.requests) != 2 {
		t.Fatalf("model saw %d requests", len(chat.requests))
	}
	second := chat.requests[1]
	if second.Messages[0].Role != model.RoleSystem || second.Messages[0].Content != SystemPrompt {
		t.Fatalf("the agent system prompt is not first: %+v", second.Messages[0])
	}
	var assistantTurn, toolTurn = -1, -1
	for index, message := range second.Messages {
		if message.Role == model.RoleAssistant && len(message.ToolCalls) > 0 {
			assistantTurn = index
		}
		if message.Role == model.RoleTool {
			toolTurn = index
		}
	}
	if assistantTurn < 0 || toolTurn != assistantTurn+1 {
		t.Fatalf("the tool result does not follow the call it answers: %+v", second.Messages)
	}
	if second.Messages[toolTurn].ToolCallID != "call-1" {
		t.Fatalf("tool result carries call id %q, want the one the model used", second.Messages[toolTurn].ToolCallID)
	}
	if !strings.Contains(second.Messages[toolTurn].Content, ObservationLabel) {
		t.Fatalf("the observation reached the model without its data marker: %q", second.Messages[toolTurn].Content)
	}
	// The tool ran as the authenticated principal, not as anything the model said.
	if search.calls[0].principal.UserID != "user-owner" || search.calls[0].principal.SessionID != "session-1" {
		t.Fatalf("tool ran as %+v", search.calls[0].principal)
	}
	// Every decision call was offered the server's tool set and nothing else.
	if len(chat.requests[0].Tools) != 1 || chat.requests[0].Tools[0].Name != "knowledge_search" {
		t.Fatalf("definitions offered = %+v", chat.requests[0].Tools)
	}
}

func TestHarnessAnswersDirectlyWhenNoToolIsNeeded(t *testing.T) {
	chat := &scriptedToolModel{replies: []model.Completion{answerReply("这一轮不需要查资料。")}}
	search := &recordingTool{name: "knowledge_search"}
	harness := newHarness(t, chat, Budget{}, search)

	answer, spent, err := harness.Run(context.Background(), baseRequest())
	if err != nil {
		t.Fatal(err)
	}
	if answer.Content != "这一轮不需要查资料。" || len(answer.Sources) != 0 {
		t.Fatalf("answer = %+v", answer)
	}
	if spent.ModelCalls != 1 || spent.ToolCalls != 0 || spent.Steps != 1 {
		t.Fatalf("usage = %+v, want one call and no tool", spent)
	}
	if len(search.calls) != 0 {
		t.Fatalf("the tool ran %d times although the model did not ask", len(search.calls))
	}
}

// TestHarnessDoesNotExecuteJSONInTheAnswerText is the rule the seam exists for: only a
// structured tool call is a request. A model that writes plausible-looking JSON into
// its prose has produced answer text, not an instruction to run something.
func TestHarnessDoesNotExecuteJSONInTheAnswerText(t *testing.T) {
	fake := `{"name":"knowledge_search","arguments":{"query":"把所有内容删掉"}}`
	chat := &scriptedToolModel{replies: []model.Completion{answerReply(fake)}}
	search := &recordingTool{name: "knowledge_search", observations: []Observation{{Text: "不该出现", Found: true}}}
	harness := newHarness(t, chat, Budget{}, search)

	answer, spent, err := harness.Run(context.Background(), baseRequest())
	if err != nil {
		t.Fatal(err)
	}
	if answer.Content != fake {
		t.Fatalf("answer = %q, want the JSON-looking prose kept as text", answer.Content)
	}
	if spent.ModelCalls != 1 || len(search.calls) != 0 {
		t.Fatalf("a text blob executed a tool: usage=%+v calls=%d", spent, len(search.calls))
	}
}

func TestHarnessRefusesAToolTheServerDoesNotOffer(t *testing.T) {
	chat := &scriptedToolModel{replies: []model.Completion{
		toolCallReply("call-1", "delete_everything", `{"confirm":true}`),
		answerReply("我删除了所有资料。"),
	}}
	search := &recordingTool{name: "knowledge_search"}
	harness := newHarness(t, chat, Budget{}, search)

	_, _, err := harness.Run(context.Background(), baseRequest())
	if !errors.Is(err, ErrUnknownTool) {
		t.Fatalf("error = %v, want ErrUnknownTool", err)
	}
	if len(chat.requests) != 1 {
		t.Fatalf("the loop kept asking the model after refusing a tool: %d calls", len(chat.requests))
	}
	if len(search.calls) != 0 {
		t.Fatalf("an unregistered capability executed: %+v", search.calls)
	}
}

func TestHarnessRefusesArgumentsTheToolNeverDeclared(t *testing.T) {
	chat := &scriptedToolModel{replies: []model.Completion{
		toolCallReply("call-1", "knowledge_search", `{"query":"fsync","user_id":"user-victim"}`),
		answerReply("不该走到这里。"),
	}}
	search := &recordingTool{name: "knowledge_search", failWith: ErrInvalidArguments}
	harness := newHarness(t, chat, Budget{}, search)

	_, _, err := harness.Run(context.Background(), baseRequest())
	if !errors.Is(err, ErrInvalidArguments) || !errors.Is(err, ErrToolExecution) {
		t.Fatalf("error = %v, want a tool failure naming invalid arguments", err)
	}
	if len(search.calls) != 0 {
		t.Fatalf("the tool body ran despite the refusal: %+v", search.calls)
	}
}

func TestHarnessRejectsAnArgumentEnvelopeThatIsNotAnObject(t *testing.T) {
	for _, raw := range []string{`["aof"]`, `"fsync"`, `42`, ``} {
		chat := &scriptedToolModel{replies: []model.Completion{toolCallReply("call-1", "knowledge_search", raw)}}
		search := &recordingTool{name: "knowledge_search", observations: []Observation{{Text: "x", Found: true}}}
		harness := newHarness(t, chat, Budget{}, search)
		_, _, err := harness.Run(context.Background(), baseRequest())
		if !errors.Is(err, ErrInvalidArguments) {
			t.Fatalf("arguments %q produced %v, want a refusal", raw, err)
		}
		if len(search.calls) != 0 {
			t.Fatalf("arguments %q reached the tool body", raw)
		}
	}
}

func TestHarnessStopsWhenTheModelCallBudgetRunsOut(t *testing.T) {
	chat := &scriptedToolModel{
		replies:    []model.Completion{toolCallReply("call-1", "knowledge_search", `{"query":"fsync"}`)},
		repeatLast: true, varyRepeat: true,
	}
	search := &recordingTool{name: "knowledge_search", observations: []Observation{{Text: "资料", Found: true}}}
	harness := newHarness(t, chat, Budget{MaxModelCalls: 2, MaxToolCalls: 50, MaxSteps: 50}, search)

	_, spent, err := harness.Run(context.Background(), baseRequest())
	if !errors.Is(err, ErrModelCallBudget) {
		t.Fatalf("error = %v, want ErrModelCallBudget", err)
	}
	if spent.ModelCalls != 2 {
		t.Fatalf("the loop made %d model calls under a budget of 2", spent.ModelCalls)
	}
}

func TestHarnessStopsWhenTheToolCallBudgetRunsOut(t *testing.T) {
	chat := &scriptedToolModel{replies: []model.Completion{{
		FinishReason: model.FinishToolCalls,
		ToolCalls: []model.ToolCall{
			{ID: "a", Name: "knowledge_search", Arguments: `{"query":"one"}`},
			{ID: "b", Name: "knowledge_search", Arguments: `{"query":"two"}`},
			{ID: "c", Name: "knowledge_search", Arguments: `{"query":"three"}`},
		},
	}}}
	search := &recordingTool{name: "knowledge_search", observations: []Observation{{Text: "资料", Found: true}}}
	harness := newHarness(t, chat, Budget{MaxModelCalls: 8, MaxToolCalls: 2, MaxSteps: 24}, search)

	_, spent, err := harness.Run(context.Background(), baseRequest())
	if !errors.Is(err, ErrToolCallBudget) {
		t.Fatalf("error = %v, want ErrToolCallBudget", err)
	}
	if spent.ToolCalls != 2 || len(search.calls) != 2 {
		t.Fatalf("spent %d tool calls, executed %d", spent.ToolCalls, len(search.calls))
	}
}

func TestHarnessStopsWhenTheStepBudgetRunsOut(t *testing.T) {
	chat := &scriptedToolModel{
		replies:    []model.Completion{toolCallReply("call-1", "knowledge_search", `{"query":"fsync"}`)},
		repeatLast: true, varyRepeat: true,
	}
	search := &recordingTool{name: "knowledge_search", observations: []Observation{{Text: "资料", Found: true}}}
	harness := newHarness(t, chat, Budget{MaxModelCalls: 20, MaxToolCalls: 20, MaxSteps: 3}, search)

	_, spent, err := harness.Run(context.Background(), baseRequest())
	if !errors.Is(err, ErrStepBudget) {
		t.Fatalf("error = %v, want ErrStepBudget", err)
	}
	if spent.Steps != 3 {
		t.Fatalf("the loop reported %d steps under a budget of 3", spent.Steps)
	}
	if spent.ModelCalls != 3 {
		t.Fatalf("the loop made %d model calls under a step budget of 3", spent.ModelCalls)
	}
}

func TestHarnessDeadlineAndCancellationAreDistinct(t *testing.T) {
	search := &recordingTool{name: "knowledge_search", observations: []Observation{{Text: "资料", Found: true}}}

	expired := &scriptedToolModel{replies: []model.Completion{answerReply("不该到这里")}, block: 25 * time.Millisecond}
	harness := newHarness(t, expired, Budget{Deadline: 2 * time.Millisecond}, search)
	_, _, err := harness.Run(context.Background(), baseRequest())
	if !errors.Is(err, ErrDeadline) {
		t.Fatalf("error = %v, want ErrDeadline", err)
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		t.Fatalf("the harness budget must not read as a bare context error: %v", err)
	}
	if !strings.Contains(err.Error(), "2ms") {
		t.Fatalf("the failure does not name the budget it hit: %v", err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	client := &scriptedToolModel{replies: []model.Completion{answerReply("不该到这里")}}
	harness = newHarness(t, client, Budget{Deadline: time.Minute}, search)
	_, _, err = harness.Run(canceled, baseRequest())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want the caller's cancellation", err)
	}
	if errors.Is(err, ErrDeadline) {
		t.Fatalf("a hung-up client must not be reported as the harness running out of time: %v", err)
	}
	if len(client.requests) != 0 {
		t.Fatal("a cancelled turn still asked the model something")
	}
}

func TestHarnessTreatsABlankFinalAnswerAsFailure(t *testing.T) {
	chat := &scriptedToolModel{replies: []model.Completion{answerReply("   ")}}
	harness := newHarness(t, chat, Budget{}, &recordingTool{name: "knowledge_search"})
	if _, _, err := harness.Run(context.Background(), baseRequest()); !errors.Is(err, model.ErrEmptyCompletion) {
		t.Fatalf("error = %v, want the blank answer refused", err)
	}
}

func TestHarnessNeedsAPrincipalAndAQuestion(t *testing.T) {
	chat := &scriptedToolModel{replies: []model.Completion{answerReply("x")}}
	harness := newHarness(t, chat, Budget{}, &recordingTool{name: "knowledge_search"})

	if _, _, err := harness.Run(context.Background(), responder.Request{Content: "问题"}); !errors.Is(err, ErrNoPrincipal) {
		t.Fatalf("error = %v, want ErrNoPrincipal", err)
	}
	if _, _, err := harness.Run(context.Background(), responder.Request{UserID: "u"}); !errors.Is(err, model.ErrInvalidRequest) {
		t.Fatalf("error = %v, want an empty question refused", err)
	}
	if len(chat.requests) != 0 {
		t.Fatal("a refused turn still reached the model")
	}
}

func TestHarnessDeduplicatesCitationsAcrossRounds(t *testing.T) {
	shared := sourceFor("chunk-1")
	chat := &scriptedToolModel{replies: []model.Completion{
		toolCallReply("a", "knowledge_search", `{"query":"first"}`),
		toolCallReply("b", "knowledge_search", `{"query":"second"}`),
		answerReply("两轮检索指向同一份资料。"),
	}}
	search := &recordingTool{name: "knowledge_search", observations: []Observation{
		{Text: "资料一", Sources: []domain.Source{shared}, Found: true},
		{Text: "资料二", Sources: []domain.Source{shared, sourceFor("chunk-2")}, Found: true},
	}}
	harness := newHarness(t, chat, Budget{}, search)

	answer, _, err := harness.Run(context.Background(), baseRequest())
	if err != nil {
		t.Fatal(err)
	}
	if len(answer.Sources) != 2 {
		t.Fatalf("citations = %+v, want the repeated one collapsed", answer.Sources)
	}
	if answer.Sources[0].SourceID != "src-chunk-1" || answer.Sources[1].SourceID != "src-chunk-2" {
		t.Fatalf("citation order is not first-seen order: %+v", answer.Sources)
	}
}

// TestStreamMatchesRespondAndIsNotTokenStreaming pins the honest shape of the SSE path:
// same content, same citations, and every delta a slice of an answer that was already
// finished - no tool text, no arguments, nothing streamed out mid-loop.
func TestStreamMatchesRespondAndIsNotTokenStreaming(t *testing.T) {
	build := func() (*scriptedToolModel, *Harness) {
		chat := &scriptedToolModel{replies: []model.Completion{
			toolCallReply("call-1", "knowledge_search", `{"query":"fsync"}`),
			answerReply("AOF 的 fsync 可以选 always、everysec 或 no。"),
		}}
		search := &recordingTool{name: "knowledge_search", observations: []Observation{{
			Text:    ObservationLabel + " knowledge_search：内部工具原文，含 secret-marker",
			Sources: []domain.Source{sourceFor("chunk-1")},
			Found:   true,
		}}}
		return chat, newHarness(t, chat, Budget{}, search)
	}

	syncChat, syncHarness := build()
	synced, spentSync, err := syncHarness.Run(context.Background(), baseRequest())
	if err != nil {
		t.Fatal(err)
	}
	if len(syncChat.requests) != 2 {
		t.Fatalf("Run asked the model %d times, want one tool round and one answer", len(syncChat.requests))
	}
	// Respond is the same loop through the responder.Strategy face.
	respondChat, respondHarness := build()
	answered, err := respondHarness.Respond(context.Background(), baseRequest())
	if err != nil {
		t.Fatal(err)
	}
	if answered.Content != synced.Content || len(answered.Sources) != len(synced.Sources) {
		t.Fatalf("Respond and Run disagree: %+v vs %+v", answered, synced)
	}
	if len(respondChat.requests) != len(syncChat.requests) {
		t.Fatalf("Respond spent a different number of calls: %d vs %d", len(respondChat.requests), len(syncChat.requests))
	}

	streamChat, streamHarness := build()
	streamed, spentStream, err := streamHarness.Run(context.Background(), baseRequest())
	if err != nil {
		t.Fatal(err)
	}
	if len(streamChat.requests) != 2 {
		t.Fatalf("the streamed script asked the model %d times", len(streamChat.requests))
	}
	if synced.Content != streamed.Content || len(synced.Sources) != len(streamed.Sources) {
		t.Fatalf("two runs of the same script disagree: %+v vs %+v", synced, streamed)
	}
	if spentSync != spentStream {
		t.Fatalf("the same script spent different budgets twice: %+v vs %+v", spentSync, spentStream)
	}

	// A fresh harness, so the delta callback can see how far the loop had got at the
	// moment the client was first given answer text.
	observeChat, observeHarness := build()
	var deltas []string
	callsAtFirstDelta := -1
	streamAnswer, err := observeHarness.Stream(context.Background(), baseRequest(), func(delta string) error {
		if callsAtFirstDelta < 0 {
			callsAtFirstDelta = len(observeChat.requests)
		}
		deltas = append(deltas, delta)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(deltas, "") != streamAnswer.Content {
		t.Fatalf("the deltas do not reassemble the answer: %q vs %q", strings.Join(deltas, ""), streamAnswer.Content)
	}
	if streamAnswer.Content != streamed.Content {
		t.Fatalf("Stream and Run produced different answers: %q vs %q", streamAnswer.Content, streamed.Content)
	}
	if len(deltas) < 2 {
		t.Fatalf("the answer arrived in one piece, so the chunking claim is untested: %d deltas", len(deltas))
	}
	// Both decision calls were non-streaming completions, and the loop had finished
	// before the first byte of answer text was emitted: that is exactly what "the agent
	// turn is not token-streamed yet" means, and saying it in a test is what keeps the
	// code from being read as the opposite.
	if callsAtFirstDelta != 2 {
		t.Fatalf("the client saw a delta after %d model calls, want the loop finished at 2", callsAtFirstDelta)
	}
	for _, delta := range deltas {
		if strings.Contains(delta, "secret-marker") || strings.Contains(delta, ObservationLabel) {
			t.Fatalf("tool internals leaked into a user-visible delta: %q", delta)
		}
	}
	if len(observeChat.requests) != 2 {
		t.Fatalf("Stream re-ran the loop: %d calls", len(observeChat.requests))
	}
}

func TestHarnessStreamRefusesWithoutADeltaHandler(t *testing.T) {
	chat := &scriptedToolModel{replies: []model.Completion{answerReply("答案")}}
	harness := newHarness(t, chat, Budget{}, &recordingTool{name: "knowledge_search"})
	if _, err := harness.Stream(context.Background(), baseRequest(), nil); err == nil {
		t.Fatal("a streamed turn with no delta handler was accepted")
	}
}

func TestHarnessPropagatesAModelFailure(t *testing.T) {
	chat := &scriptedToolModel{err: model.ErrUnavailable}
	search := &recordingTool{name: "knowledge_search"}
	harness := newHarness(t, chat, Budget{}, search)
	if _, _, err := harness.Run(context.Background(), baseRequest()); !errors.Is(err, model.ErrUnavailable) {
		t.Fatalf("error = %v, want the provider failure to reach the caller", err)
	}
	if len(search.calls) != 0 {
		t.Fatal("a tool ran although the model never answered")
	}
}

func TestHarnessNeedsAToolCapableModelAndARegisteredTool(t *testing.T) {
	chat := &scriptedToolModel{replies: []model.Completion{answerReply("x")}}
	if _, err := New(nil, mustRegistry(t, &recordingTool{name: "knowledge_search"}), Budget{}); err == nil {
		t.Fatal("a harness with no model was accepted")
	}
	if _, err := New(chat, mustRegistry(t), Budget{}); err == nil {
		t.Fatal("a harness with no tools was accepted")
	}
}

func mustRegistry(t *testing.T, tools ...Tool) *Registry {
	t.Helper()
	registry, err := NewRegistry(tools...)
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func TestBudgetNormalizeDefaultsAndClamps(t *testing.T) {
	// The four defaults are a contract, so they are pinned as numbers rather than
	// compared against the constants that spell them out.
	if DefaultMaxModelCalls != 4 || DefaultMaxToolCalls != 3 || DefaultMaxSteps != 10 || DefaultDeadline != 45*time.Second {
		t.Fatalf("defaults moved: %d/%d/%d/%s", DefaultMaxModelCalls, DefaultMaxToolCalls, DefaultMaxSteps, DefaultDeadline)
	}
	defaults := Budget{}.Normalize()
	if defaults.MaxModelCalls != DefaultMaxModelCalls || defaults.MaxToolCalls != DefaultMaxToolCalls ||
		defaults.MaxSteps != DefaultMaxSteps || defaults.Deadline != DefaultDeadline {
		t.Fatalf("defaults = %+v", defaults)
	}
	clamped := Budget{MaxModelCalls: 100000, MaxToolCalls: -7, MaxSteps: 999999, Deadline: time.Hour * 48}.Normalize()
	if clamped.MaxModelCalls != MaxModelCallsCeiling || clamped.MaxToolCalls != DefaultMaxToolCalls ||
		clamped.MaxSteps != MaxStepsCeiling || clamped.Deadline != DeadlineCeiling {
		t.Fatalf("clamping failed: %+v", clamped)
	}
	// A tighter step budget is not quietly raised to match the call budget: it is the
	// setting that stops a model that never stops asking for tools.
	tighter := Budget{MaxModelCalls: 6, MaxSteps: 2}.Normalize()
	if tighter.MaxSteps != 2 || tighter.MaxModelCalls != 6 {
		t.Fatalf("the stricter step budget was not honored: %+v", tighter)
	}
}

func TestHarnessPromptKeepsHistoryAndAsksTheQuestionOnce(t *testing.T) {
	chat := &scriptedToolModel{replies: []model.Completion{answerReply("答案")}}
	harness := newHarness(t, chat, Budget{}, &recordingTool{name: "knowledge_search"})
	request := baseRequest()
	if _, _, err := harness.Run(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	messages := chat.requests[0].Messages
	if messages[0].Role != model.RoleSystem || messages[len(messages)-1].Content != request.Content {
		t.Fatalf("prompt layout wrong: %+v", messages)
	}
	var asked int
	for _, message := range messages {
		if message.Role == model.RoleUser && message.Content == request.Content {
			asked++
		}
	}
	if asked != 1 {
		t.Fatalf("the question appears %d times in the prompt", asked)
	}
	if len(messages) != 1+len(request.History)+1 {
		t.Fatalf("history did not reach the prompt: %+v", messages)
	}
}
