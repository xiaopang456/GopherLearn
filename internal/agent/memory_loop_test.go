package agent

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/model"
	"github.com/xiaopang456/GopherLearn/internal/responder"
)

// These tests run the real memory tools rather than recordingTool. A stand-in can be made
// to return anything, so it can only prove that the harness moves a string from one place to
// another; what the next Provider run depends on is that *these* tools' observations,
// citations and refusals survive that trip - including the two ways a memory turn can end
// wrongly: a budget reported as "no memories", or a failed read reported as an empty one.

// runMemoryLoop drives one scripted turn over the real memory_search tool and returns what
// the model was shown alongside what the turn produced.
func runMemoryLoop(t *testing.T, reader *scriptedMemories, options MemorySearchOptions,
	budget Budget, replies ...model.Completion,
) (*scriptedToolModel, *MemorySearch, responder.Answer, Usage, error) {
	t.Helper()
	chat := &scriptedToolModel{replies: replies}
	tool := newMemoryTool(t, reader, options)
	harness := newHarness(t, chat, budget, tool)
	answer, spent, err := harness.Run(context.Background(), baseRequest())
	return chat, tool, answer, spent, err
}

// toolMessages lists the role=tool messages of one decision call, in transcript order.
func toolMessages(chat *scriptedToolModel, call int) []model.Message {
	if call-1 >= len(chat.requests) {
		return nil
	}
	var found []model.Message
	for _, message := range chat.requests[call-1].Messages {
		if message.Role == model.RoleTool {
			found = append(found, message)
		}
	}
	return found
}

// everyTranscript is everything the model was ever shown. A "this never reached the model"
// assertion has to look at all of it: reading only the last call would miss a message the
// loop put in earlier.
func everyTranscript(chat *scriptedToolModel) string {
	var builder strings.Builder
	for _, request := range chat.requests {
		for _, message := range request.Messages {
			builder.WriteString(string(message.Role))
			builder.WriteString("|")
			builder.WriteString(message.Content)
			builder.WriteString("\n")
		}
	}
	return builder.String()
}

// observationsAcrossTheTurn is the tool-result channel only, across every decision call. A
// claim that "the model was never told there were no memories" cannot be checked against the
// whole transcript, because the agent's own system prompt already instructs it not to
// invent: only a role=tool message can carry an observation, so that is the channel to read.
func observationsAcrossTheTurn(chat *scriptedToolModel) string {
	var builder strings.Builder
	for _, request := range chat.requests {
		for _, message := range request.Messages {
			if message.Role == model.RoleTool {
				builder.WriteString(message.Content)
				builder.WriteString("\n")
			}
		}
	}
	return builder.String()
}

func offeredNames(definitions []model.ToolDefinition) string {
	names := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		names = append(names, definition.Name)
	}
	return strings.Join(names, ",")
}

// TestMemorySearchLoopReturnsTheToolResultAsARoleToolMessage is the two-step contract the
// next real run is judged on: the model asks, the server executes, and the second request
// carries the result back in the shape a provider can pair with the call it answers.
func TestMemorySearchLoopReturnsTheToolResultAsARoleToolMessage(t *testing.T) {
	reader := &scriptedMemories{
		entries: []MemoryEntry{
			memoryEntry("全局记忆", "用户偏好中文回答", "mem-1"),
			memoryEntry("经历记录", "上一轮决定用 fsync everysec", "mem-2"),
		},
		scopeNote: "本轮检索范围：当前用户的全局记忆与本会话经历。",
	}
	chat, _, answer, spent, err := runMemoryLoop(t, reader, MemorySearchOptions{}, Budget{MaxModelCalls: 2},
		toolCallReply("call-mem-1", MemorySearchName, `{"query":"fsync 偏好"}`),
		answerReply("按你确认过的偏好，用中文解释 everysec。"))
	if err != nil {
		t.Fatalf("a legal two-step memory turn failed: %v", err)
	}
	if answer.Content == "" {
		t.Fatal("the turn ended without an answer")
	}
	if spent.ModelCalls != 2 || spent.ToolCalls != 1 || spent.ToolRounds != 1 {
		t.Fatalf("usage = %+v, want two model calls and one tool call", spent)
	}
	if reader.calls != 1 {
		t.Fatalf("the read ran %d times, want once per tool call", reader.calls)
	}
	// The read ran as the turn, not as anything the model's arguments could name.
	if len(reader.seen) != 1 || reader.seen[0] != (Principal{UserID: "user-owner", SessionID: "session-1"}) {
		t.Fatalf("recall ran as %+v", reader.seen)
	}
	if len(chat.requests) != 2 {
		t.Fatalf("the model was asked %d times, want the call plus the answer", len(chat.requests))
	}
	observed := toolMessages(chat, 2)
	if len(observed) != 1 {
		t.Fatalf("the second request carried %d tool messages, want exactly 1: %+v",
			len(observed), chat.requests[1].Messages)
	}
	if observed[0].ToolCallID != "call-mem-1" {
		t.Fatalf("tool result answers call %q, want the id the model used", observed[0].ToolCallID)
	}
	// A provider pairs a tool result with the call that precedes it; anything else is a
	// transcript that cannot be read as one round trip.
	callIndex, resultIndex := -1, -1
	for index, message := range chat.requests[1].Messages {
		if message.Role == model.RoleAssistant && len(message.ToolCalls) > 0 {
			callIndex = index
		}
		if message.Role == model.RoleTool {
			resultIndex = index
		}
	}
	if callIndex < 0 || resultIndex != callIndex+1 {
		t.Fatalf("the tool result does not follow the call it answers: %+v", chat.requests[1].Messages)
	}
	if second := chat.requests[1].Messages; second[callIndex].ToolCalls[0].Name != MemorySearchName {
		t.Fatalf("the transcript attributes the call to %q", second[callIndex].ToolCalls[0].Name)
	}
	if len(chat.requests[0].Messages) >= len(chat.requests[1].Messages) {
		t.Fatalf("the second request did not grow by the round trip: %d then %d messages",
			len(chat.requests[0].Messages), len(chat.requests[1].Messages))
	}
	for _, want := range []string{"用户偏好中文回答", "上一轮决定用 fsync everysec", "[1] 全局记忆", "[2] 经历记录"} {
		if !strings.Contains(observed[0].Content, want) {
			t.Fatalf("the observation lost %q on its way back to the model: %s", want, observed[0].Content)
		}
	}
	if !strings.HasPrefix(observed[0].Content, ObservationLabel+" "+MemorySearchName) {
		t.Fatalf("the result must identify which tool produced it: %s", observed[0].Content)
	}
	if transcript := everyTranscript(chat); strings.Contains(transcript, "mem-1") || strings.Contains(transcript, "src-") {
		t.Fatalf("internal identifiers reached the model: %s", transcript)
	}
	// Citations are the memories that were shown, in the order they were shown.
	if len(answer.Sources) != 2 || answer.Sources[0].SourceID != "src-mem-1" || answer.Sources[1].SourceID != "src-mem-2" {
		t.Fatalf("citations = %+v, want both recalled memories in order", answer.Sources)
	}
}

// TestMemorySearchLoopCitesOnlyWhatTheObservationShowed: the budget inside the tool can drop
// entries, and a dropped one must disappear from the citations as well - otherwise the answer
// claims grounding in a memory the model was never given.
func TestMemorySearchLoopCitesOnlyWhatTheObservationShowed(t *testing.T) {
	long := strings.Repeat("很长的一段记忆内容，", 40)
	reader := &scriptedMemories{entries: []MemoryEntry{
		memoryEntry("全局记忆", "第一条 fsync 记忆", "mem-1"),
		memoryEntry("全局记忆", long, "mem-2"),
		memoryEntry("全局记忆", "第三条 fsync 记忆", "mem-3"),
	}}
	chat, _, answer, _, err := runMemoryLoop(t, reader, MemorySearchOptions{ObservationRunes: 40, MaxEntries: 2},
		Budget{MaxModelCalls: 2},
		toolCallReply("call-mem-1", MemorySearchName, `{"query":"fsync 记忆"}`),
		answerReply("只按看得到的两条回答。"))
	if err != nil {
		t.Fatal(err)
	}
	shown := toolMessages(chat, 2)
	if len(shown) != 1 {
		t.Fatalf("want one observation in the transcript, got %d", len(shown))
	}
	blocks := strings.Count(shown[0].Content, "\n[")
	if blocks != len(answer.Sources) {
		t.Fatalf("%d memories were shown but %d were cited: %+v", blocks, len(answer.Sources), answer.Sources)
	}
	if blocks == 0 || blocks >= 3 {
		t.Fatalf("the budget trimmed nothing: blocks=%d text=%q", blocks, shown[0].Content)
	}
	if strings.Contains(shown[0].Content, "第三条 fsync 记忆") {
		t.Fatal("the entry the budget left out reached the model anyway")
	}
	for _, source := range answer.Sources {
		if source.SourceID == "src-mem-3" {
			t.Fatalf("a memory the model was never shown was cited: %+v", answer.Sources)
		}
	}
}

// TestMemoryToolFailureEndsTheTurnInsteadOfReportingNoMemories is the distinction this phase
// keeps insisting on, tested through the loop: a store that could not answer must produce no
// observation at all, because an observation saying "there is nothing here" would be a
// confident statement about the user built on a failure to look.
func TestMemoryToolFailureEndsTheTurnInsteadOfReportingNoMemories(t *testing.T) {
	reader := &scriptedMemories{err: errors.New("store unreachable")}
	chat, _, answer, spent, err := runMemoryLoop(t, reader, MemorySearchOptions{}, Budget{MaxModelCalls: 2},
		toolCallReply("call-mem-1", MemorySearchName, `{"query":"fsync"}`),
		answerReply("不该走到这里。"))
	if err == nil {
		t.Fatal("a failed read ended the turn as a success")
	}
	if !errors.Is(err, ErrMemoryLookup) || !errors.Is(err, ErrToolExecution) {
		t.Fatalf("error = %v, want a tool execution failure naming the lookup", err)
	}
	if !strings.Contains(err.Error(), "store unreachable") {
		t.Fatalf("the failure lost its cause for the caller: %v", err)
	}
	if answer.Content != "" || len(answer.Sources) != 0 {
		t.Fatalf("a failed read produced an answer: %+v", answer)
	}
	// The read was attempted, so this is a store failure and not an argument refusal.
	if reader.calls != 1 {
		t.Fatalf("the reader ran %d times", reader.calls)
	}
	if spent.ModelCalls != 1 || spent.ToolCalls != 1 {
		t.Fatalf("usage = %+v, want the turn to stop after the failed call", spent)
	}
	if len(chat.requests) != 1 {
		t.Fatalf("the model was asked again after the failure: %d requests", len(chat.requests))
	}
	transcript := observationsAcrossTheTurn(chat)
	if transcript != "" {
		t.Fatalf("a failed read handed the model an observation anyway: %s", transcript)
	}
}

// TestGenuineEmptyReadStillAnswersWithZeroCitations is the positive control for the test
// above: an honest "nothing matched" is not an error, the model does see it, and the answer
// it produces carries no citation. Without this pair the failure test would also pass on a
// tool that never returned anything at all.
func TestGenuineEmptyReadStillAnswersWithZeroCitations(t *testing.T) {
	reader := &scriptedMemories{scopeNote: "本轮检索范围：当前用户的全局记忆。"}
	chat, _, answer, spent, err := runMemoryLoop(t, reader, MemorySearchOptions{}, Budget{MaxModelCalls: 2},
		toolCallReply("call-mem-1", MemorySearchName, `{"query":"fsync"}`),
		answerReply("我这边没有可参考的记忆。"))
	if err != nil {
		t.Fatalf("an empty read must not fail the turn: %v", err)
	}
	if spent.ModelCalls != 2 || spent.ToolCalls != 1 {
		t.Fatalf("usage = %+v", spent)
	}
	shown := toolMessages(chat, 2)
	if len(shown) != 1 || !strings.Contains(shown[0].Content, "没有读到") {
		t.Fatalf("the model was not told the read came back empty: %+v", shown)
	}
	if !strings.Contains(shown[0].Content, "本轮检索范围：当前用户的全局记忆。") {
		t.Fatalf("an empty result must say what was searched: %s", shown[0].Content)
	}
	if !strings.Contains(shown[0].Content, "不要编造") {
		t.Fatalf("an empty result needs the explicit warning: %s", shown[0].Content)
	}
	if answer.Content == "" || len(answer.Sources) != 0 {
		t.Fatalf("answer = %+v, want prose with no citations", answer)
	}
}

// TestOneModelCallCannotFinishAMemoryRound is the budget half of the "do not fake an empty
// memory" rule: when the loop runs out of calls it must say so. A caller that read this as
// "the user has no memories" would be told the opposite of what happened, and would hide a
// lookup that already cost a read.
func TestOneModelCallCannotFinishAMemoryRound(t *testing.T) {
	entries := func() *scriptedMemories {
		return &scriptedMemories{
			entries:   []MemoryEntry{memoryEntry("全局记忆", "用户偏好中文回答", "mem-1")},
			scopeNote: "本轮检索范围：当前用户的全局记忆。",
		}
	}
	chat, _, answer, spent, err := runMemoryLoop(t, entries(), MemorySearchOptions{}, Budget{MaxModelCalls: 1},
		toolCallReply("call-mem-1", MemorySearchName, `{"query":"偏好"}`),
		answerReply("不该走到这里。"))
	if !errors.Is(err, ErrModelCallBudget) {
		t.Fatalf("error = %v, want the model-call budget", err)
	}
	if errors.Is(err, ErrMemoryLookup) {
		t.Fatalf("a budget must not read as a failed read: %v", err)
	}
	if !strings.Contains(err.Error(), "1 calls") {
		t.Fatalf("the failure does not name the budget it hit: %v", err)
	}
	if answer.Content != "" || len(answer.Sources) != 0 {
		t.Fatalf("a turn that ran out of budget returned an answer: %+v", answer)
	}
	if spent.ModelCalls != 1 {
		t.Fatalf("the loop made %d model calls under a budget of 1", spent.ModelCalls)
	}
	// The read really happened, so the only thing missing was the second call.
	if spent.ToolCalls != 1 {
		t.Fatalf("usage = %+v, want the executed read reported", spent)
	}
	if len(chat.requests) != 1 {
		t.Fatalf("the model was asked %d times under a budget of 1", len(chat.requests))
	}
	// The read's observation existed but was never sent, which is the difference between
	// "out of calls" and "the user has nothing": the model is not shown an empty-memory
	// result it would then repeat back as a fact about the user.
	if shown := observationsAcrossTheTurn(chat); shown != "" {
		t.Fatalf("an exhausted budget still put an observation on the wire: %s", shown)
	}

	// Same script, one more call: the turn succeeds. The only difference between the two
	// runs is the budget, which is what makes the failure above a budget failure rather than
	// a memory failure.
	secondChat, _, secondAnswer, secondSpent, err := runMemoryLoop(t, entries(), MemorySearchOptions{},
		Budget{MaxModelCalls: 2},
		toolCallReply("call-mem-1", MemorySearchName, `{"query":"偏好"}`),
		answerReply("按偏好回答。"))
	if err != nil {
		t.Fatalf("the same script under a budget of 2 failed: %v", err)
	}
	if secondAnswer.Content != "按偏好回答。" || len(secondAnswer.Sources) != 1 {
		t.Fatalf("answer = %+v", secondAnswer)
	}
	if secondSpent.ModelCalls != 2 || len(secondChat.requests) != 2 {
		t.Fatalf("usage = %+v requests = %d", secondSpent, len(secondChat.requests))
	}
}

// TestMemorySearchLoopRefusesForgedScopeWithoutReading puts the schema refusal inside a turn:
// an undeclared field ends the turn as a tool failure, the reader is never entered, and the
// model is not asked a second time to try the same privilege again.
func TestMemorySearchLoopRefusesForgedScopeWithoutReading(t *testing.T) {
	for _, arguments := range []string{
		`{"query":"fsync","user_id":"user-victim"}`,
		`{"query":"fsync","session_id":"session-victim"}`,
		`{"query":"fsync","topic_key":"redis"}`,
		`{"query":"fsync","scope":"global"}`,
		`{"query":"fsync","min_confidence":0}`,
		`{"query":"fsync","all_users":true}`,
	} {
		reader := &scriptedMemories{entries: []MemoryEntry{memoryEntry("全局记忆", "用户的 fsync 偏好", "mem-1")}}
		chat, _, answer, spent, err := runMemoryLoop(t, reader, MemorySearchOptions{}, Budget{MaxModelCalls: 2},
			toolCallReply("call-forge", MemorySearchName, arguments),
			answerReply("不该走到这里。"))
		if !errors.Is(err, ErrToolExecution) || !errors.Is(err, ErrUnknownArgumentField) {
			t.Fatalf("%s = %v, want a tool failure naming the undeclared field", arguments, err)
		}
		if reader.calls != 0 {
			t.Fatalf("%s reached the reader %d time(s)", arguments, reader.calls)
		}
		if len(chat.requests) != 1 || spent.ModelCalls != 1 {
			t.Fatalf("%s let the loop continue: usage=%+v requests=%d", arguments, spent, len(chat.requests))
		}
		if answer.Content != "" || len(answer.Sources) != 0 {
			t.Fatalf("%s produced an answer: %+v", arguments, answer)
		}
	}
	// Positive control: the same script with only the declared key reads once.
	reader := &scriptedMemories{entries: []MemoryEntry{memoryEntry("全局记忆", "用户的 fsync 偏好", "mem-1")}}
	if _, _, _, _, err := runMemoryLoop(t, reader, MemorySearchOptions{}, Budget{MaxModelCalls: 2},
		toolCallReply("call-ok", MemorySearchName, `{"query":"fsync"}`),
		answerReply("按偏好回答。")); err != nil {
		t.Fatalf("the legal arguments failed: %v", err)
	}
	if reader.calls != 1 {
		t.Fatalf("the authorized call reached the reader %d time(s)", reader.calls)
	}
}

// TestMemorySearchWithoutAPrincipalCannotReadAnything covers the no-principal refusal inside
// the registry, the only place a turn can be headless: the harness refuses an unauthenticated
// request before the loop, so a tool that trusted its principal would fail here rather than
// in production.
func TestMemorySearchWithoutAPrincipalCannotReadAnything(t *testing.T) {
	reader := &scriptedMemories{entries: []MemoryEntry{memoryEntry("全局记忆", "用户的 fsync 偏好", "mem-1")}}
	registry, err := NewRegistry(newMemoryTool(t, reader, MemorySearchOptions{}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.run(context.Background(), Principal{SessionID: "session-1"},
		model.ToolCall{ID: "call-1", Name: MemorySearchName, Arguments: `{"query":"fsync"}`},
	); !errors.Is(err, ErrToolExecution) || !errors.Is(err, ErrNoPrincipal) {
		t.Fatalf("headless call = %v, want a refusal naming the missing principal", err)
	}
	if reader.calls != 0 {
		t.Fatalf("a call with no user reached the reader %d time(s)", reader.calls)
	}
	// And the same tool with a principal does read, so the refusal above is not "nothing
	// ever works".
	if _, err := registry.run(context.Background(), Principal{UserID: "user-1", SessionID: "session-1"},
		model.ToolCall{ID: "call-2", Name: MemorySearchName, Arguments: `{"query":"fsync"}`},
	); err != nil {
		t.Fatalf("the same call with a principal failed: %v", err)
	}
	if reader.calls != 1 {
		t.Fatalf("the authorized call reached the reader %d time(s)", reader.calls)
	}
}

// TestProposalLoopStoresAPendingDraftUnderATwoCallBudget is the write side of the two-step
// contract: one write, one answer, and the record the writer received is built from the
// turn's identity plus the model's words in exactly the proportions the seam allows.
func TestProposalLoopStoresAPendingDraftUnderATwoCallBudget(t *testing.T) {
	proposer := &recordingProposer{outcome: MemoryProposalOutcome{Created: true}}
	chat := &scriptedToolModel{replies: []model.Completion{
		toolCallReply("call-prop", ProposeMemoryName, `{"scope":"topic","topic_key":"redis","content":"AOF 的 fsync 选 everysec"}`),
		answerReply("记下了，等你在记忆页面确认。"),
	}}
	harness := newHarness(t, chat, Budget{MaxModelCalls: 2}, newProposalTool(t, proposer))
	request := baseRequest()
	request.UserMessageID = "message-7"

	answer, spent, err := harness.Run(context.Background(), request)
	if err != nil {
		t.Fatalf("a two-call proposal turn failed: %v", err)
	}
	if spent.ModelCalls != 2 || spent.ToolCalls != 1 {
		t.Fatalf("usage = %+v", spent)
	}
	if answer.Content == "" {
		t.Fatal("the user lost their answer")
	}
	if len(answer.Sources) != 0 {
		t.Fatalf("a proposal was cited as evidence: %+v", answer.Sources)
	}
	if len(proposer.calls) != 1 {
		t.Fatalf("the writer ran %d times, want once", len(proposer.calls))
	}
	call := proposer.calls[0]
	// The three things the model may name came from its arguments.
	if call.proposal.Scope != domain.MemoryScopeTopic || call.proposal.TopicKey != "redis" ||
		call.proposal.Content != "AOF 的 fsync 选 everysec" {
		t.Fatalf("proposal = %+v", call.proposal)
	}
	// Who the draft belongs to, and which stored message it cites, came from the turn.
	if call.principal != (Principal{UserID: "user-owner", SessionID: "session-1", UserMessageID: "message-7"}) {
		t.Fatalf("writer ran as %+v", call.principal)
	}
	// What the model read back: a sentence about confirmation, with no identifiers it could
	// reuse as a handle.
	shown := toolMessages(chat, 2)
	if len(shown) != 1 {
		t.Fatalf("the proposal returned %d observations", len(shown))
	}
	if !strings.Contains(shown[0].Content, "确认") {
		t.Fatalf("the model must be told confirmation is not its decision: %s", shown[0].Content)
	}
	for _, forbidden := range []string{"message-7", "session-1", "user-owner", "mem-"} {
		if strings.Contains(shown[0].Content, forbidden) {
			t.Fatalf("the observation leaked %q: %s", forbidden, shown[0].Content)
		}
	}
	// A status, a confidence and a source list are not fields of this seam at all, so no
	// argument text can ask for them. Naming the fields keeps the claim checkable.
	if names := fieldNamesOf(MemoryProposal{}); strings.Join(names, ",") != "Content,Scope,TopicKey" {
		t.Fatalf("MemoryProposal carries %v, want only the three model-owned fields", names)
	}
}

// TestProposalLoopRefusesForgedIdentityWithoutWriting is the tool-level forgery rule inside
// a turn: a refusal must end the turn, not give a persistent model another call with which
// to retry the same write.
func TestProposalLoopRefusesForgedIdentityWithoutWriting(t *testing.T) {
	for _, arguments := range []string{
		`{"scope":"global","content":"用户偏好","user_id":"user-victim"}`,
		`{"scope":"global","content":"用户偏好","session_id":"session-victim"}`,
		`{"scope":"global","content":"用户偏好","source_message_ids":["message-1"]}`,
		`{"scope":"global","content":"用户偏好","status":"active"}`,
		`{"scope":"global","content":"用户偏好","confidence":1}`,
		`{"scope":"global","content":"用户偏好","confirm":true}`,
		`{"scope":"global","content":"用户偏好","skip_confirmation":true}`,
	} {
		proposer := &recordingProposer{outcome: MemoryProposalOutcome{Created: true}}
		chat := &scriptedToolModel{replies: []model.Completion{
			toolCallReply("call-prop", ProposeMemoryName, arguments),
			answerReply("不该走到这里。"),
		}}
		harness := newHarness(t, chat, Budget{MaxModelCalls: 2}, newProposalTool(t, proposer))
		request := baseRequest()
		request.UserMessageID = "message-7"
		answer, spent, err := harness.Run(context.Background(), request)
		if !errors.Is(err, ErrToolExecution) || !errors.Is(err, ErrUnknownArgumentField) {
			t.Fatalf("%s = %v, want an undeclared-field refusal", arguments, err)
		}
		if len(proposer.calls) != 0 {
			t.Fatalf("%s reached the writer %d time(s)", arguments, len(proposer.calls))
		}
		if spent.ModelCalls != 1 || len(chat.requests) != 1 {
			t.Fatalf("%s let the loop continue: usage=%+v requests=%d", arguments, spent, len(chat.requests))
		}
		if answer.Content != "" {
			t.Fatalf("%s produced an answer: %q", arguments, answer.Content)
		}
	}
	// Positive control: the same turn with only declared keys writes once and answers.
	proposer := &recordingProposer{outcome: MemoryProposalOutcome{Created: true}}
	chat := &scriptedToolModel{replies: []model.Completion{
		toolCallReply("call-prop", ProposeMemoryName, `{"scope":"global","content":"用户偏好先看代码"}`),
		answerReply("记下了。"),
	}}
	harness := newHarness(t, chat, Budget{MaxModelCalls: 2}, newProposalTool(t, proposer))
	request := baseRequest()
	request.UserMessageID = "message-7"
	if _, _, err := harness.Run(context.Background(), request); err != nil {
		t.Fatalf("the legal proposal failed: %v", err)
	}
	if len(proposer.calls) != 1 {
		t.Fatalf("the legal proposal wrote %d drafts", len(proposer.calls))
	}
}

// TestMemoryToolSurfaceIsIdenticalEveryRound is the "do not widen the offer" invariant at
// the loop level: the same definitions on every decision call, memory_search still declaring
// only its query, and no capability appearing because a round happened to want one.
func TestMemoryToolSurfaceIsIdenticalEveryRound(t *testing.T) {
	reader := &scriptedMemories{entries: []MemoryEntry{memoryEntry("全局记忆", "用户偏好中文", "mem-1")}}
	proposer := &recordingProposer{outcome: MemoryProposalOutcome{Created: true}}
	search := newMemoryTool(t, reader, MemorySearchOptions{})
	propose := newProposalTool(t, proposer)
	knowledge := &recordingTool{name: KnowledgeSearchName, observations: []Observation{{
		Text: ObservationLabel + " " + KnowledgeSearchName + "：检索到 1 条资料", Found: true,
	}}}
	chat := &scriptedToolModel{replies: []model.Completion{
		toolCallReply("call-1", MemorySearchName, `{"query":"偏好"}`),
		toolCallReply("call-2", ProposeMemoryName, `{"scope":"global","content":"用户偏好先看代码"}`),
		answerReply("按偏好回答。"),
	}}
	harness := newHarness(t, chat, Budget{}, knowledge, search, propose)
	request := baseRequest()
	request.UserMessageID = "message-7"
	if _, _, err := harness.Run(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if len(chat.requests) != 3 {
		t.Fatalf("the loop made %d decision calls, want three", len(chat.requests))
	}
	// Registration order is what production uses: knowledge first, then the two memory
	// doors. A round of the loop must not reorder or widen that offer.
	want := KnowledgeSearchName + "," + MemorySearchName + "," + ProposeMemoryName
	for index, sent := range chat.requests {
		if names := offeredNames(sent.Tools); names != want {
			t.Fatalf("decision call %d was offered %q, want %q", index+1, names, want)
		}
		for _, definition := range sent.Tools {
			if definition.Name != MemorySearchName {
				continue
			}
			if string(definition.Parameters) != string(search.Parameters()) {
				t.Fatalf("decision call %d was offered a different memory_search schema: %s",
					index+1, definition.Parameters)
			}
			var schema map[string]any
			if err := json.Unmarshal(definition.Parameters, &schema); err != nil {
				t.Fatalf("memory_search schema is not JSON: %v", err)
			}
			properties, _ := schema["properties"].(map[string]any)
			if len(properties) != 1 {
				t.Fatalf("memory_search advertises %d keys: %v", len(properties), properties)
			}
			if _, declared := properties["query"]; !declared {
				t.Fatalf("the only declared key must be query, got %v", properties)
			}
			if schema["additionalProperties"] != false {
				t.Fatalf("the schema must still refuse undeclared keys: %v", schema["additionalProperties"])
			}
		}
	}
	// Both doors were used once each, so the surface was not just advertised but exercised.
	if reader.calls != 1 || len(proposer.calls) != 1 || len(knowledge.calls) != 0 {
		t.Fatalf("reads=%d writes=%d knowledge=%d, want 1, 1 and untouched",
			reader.calls, len(proposer.calls), len(knowledge.calls))
	}
}

// fieldNamesOf lists a struct's exported field names, sorted, so a test can say "this seam
// has no such field" without reading a struct definition at review time.
func fieldNamesOf(value any) []string {
	structType := reflect.TypeOf(value)
	names := make([]string, 0, structType.NumField())
	for index := 0; index < structType.NumField(); index++ {
		field := structType.Field(index)
		if !field.IsExported() {
			continue
		}
		names = append(names, field.Name)
	}
	sort.Strings(names)
	return names
}
