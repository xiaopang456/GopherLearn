package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/agent"
	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/model"
	"github.com/xiaopang456/GopherLearn/internal/openai"
	"github.com/xiaopang456/GopherLearn/internal/repository"
	"github.com/xiaopang456/GopherLearn/internal/responder"
)

// These cases drive a real agent loop through the message service, so what is asserted is the
// event stream an SSE client is written from - not a double that has been told what the loop
// would produce. The regression they cover is a provider that narrates inside a tool round:
// that text is now shown while it is being generated, labelled as what it turns out to be, and
// never confirmed as the answer.

// roundModel is a tool-capable streaming provider replayed one round at a time. A round that
// asks for a tool narrates first, which is the shape a real model writes when it says "let me
// look that up" before it calls anything.
type roundModel struct {
	rounds  []scriptRound
	calls   int
	request []model.Request
	// hold runs inside the provider's own stream, at the gate point of a round that sets one.
	// A test that blocks in it is what lets another goroutine read the frames a client already
	// holds while generation is still running - nothing here judges real-time delivery by
	// counting chunks after the turn is over, which a re-chunked answer would also pass.
	hold func()
}

type scriptRound struct {
	deltas    []string
	toolNames []string
	gateAfter int
	// breakAfter fails the round after that many fragments were handed over, which is how a
	// provider whose stream dies mid-answer is simulated.
	breakAfter int
}

func (m *roundModel) Complete(context.Context, model.Request) (model.Completion, error) {
	return model.Completion{}, errors.New("test model: the plain completion path must not be used")
}

func (m *roundModel) Stream(context.Context, model.Request, model.DeltaHandler) (model.Completion, error) {
	return model.Completion{}, errors.New("test model: the tool-free stream path must not be used")
}

func (m *roundModel) CompleteWithTools(context.Context, model.Request) (model.Completion, error) {
	return model.Completion{}, errors.New("test model: a streamed turn must not be re-requested without streaming")
}

func (m *roundModel) StreamWithTools(ctx context.Context, request model.Request,
	onDelta model.StreamDeltaHandler,
) (model.Completion, error) {
	m.calls++
	m.request = append(m.request, request)
	if m.calls > len(m.rounds) {
		return model.Completion{}, errors.New("test model: the script ran out of rounds")
	}
	round := m.rounds[m.calls-1]
	var content strings.Builder
	for index, fragment := range round.deltas {
		if err := onDelta(model.StreamDelta{Content: fragment}); err != nil {
			return model.Completion{}, err
		}
		content.WriteString(fragment)
		if round.gateAfter > 0 && index+1 == round.gateAfter && m.hold != nil {
			m.hold()
			if err := ctx.Err(); err != nil {
				return model.Completion{}, err
			}
		}
		if round.breakAfter > 0 && index+1 == round.breakAfter {
			return model.Completion{}, model.ErrUnavailable
		}
	}
	var calls []model.ToolCall
	for index, name := range round.toolNames {
		if err := onDelta(model.StreamDelta{ToolCall: true}); err != nil {
			return model.Completion{}, err
		}
		calls = append(calls, model.ToolCall{ID: "call-" + strconv.Itoa(index), Name: name,
			Arguments: `{"query":"fsync"}`})
	}
	if len(calls) > 0 {
		return model.Completion{Content: content.String(), ToolCalls: calls,
			FinishReason: model.FinishToolCalls}, nil
	}
	if strings.TrimSpace(content.String()) == "" {
		return model.Completion{}, model.ErrEmptyCompletion
	}
	return model.Completion{Content: content.String(), FinishReason: model.FinishStop}, nil
}

// countedTool is the capability a decision round asks for, plus the record of whether the turn
// executed it.
type countedTool struct {
	name  string
	calls int
}

func (t *countedTool) Name() string        { return t.name }
func (t *countedTool) Description() string { return "test capability" }
func (t *countedTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","additionalProperties":false}`)
}

func (t *countedTool) Execute(ctx context.Context, _ agent.Principal, _ json.RawMessage) (agent.Observation, error) {
	if err := ctx.Err(); err != nil {
		return agent.Observation{}, err
	}
	t.calls++
	return agent.Observation{Text: "工具原文，含 secret-marker", Summary: "检索到 1 条资料", Found: true}, nil
}

// agentTurn installs the real harness behind the real responder router, so the service cannot
// be told it has steps by a double that only pretends to.
func agentTurn(t *testing.T, rounds ...scriptRound) (
	*MessageService, *repository.InMemoryRepository, string, string, *roundModel, *countedTool,
) {
	t.Helper()
	search := &countedTool{name: agent.KnowledgeSearchName}
	registry, err := agent.NewRegistry(search)
	if err != nil {
		t.Fatal(err)
	}
	chat := &roundModel{rounds: rounds}
	harness, err := agent.New(chat, registry, agent.Budget{})
	if err != nil {
		t.Fatal(err)
	}
	assistant, err := responder.NewStrategyResponder(harness)
	if err != nil {
		t.Fatal(err)
	}
	service, store, userID, sessionID := newMessageFixture(t, assistant, domain.SessionModeLearningAgent)
	return service, store, userID, sessionID, chat, search
}

func eventsNamed(events []StreamEvent, name string) []StreamEvent {
	var seen []StreamEvent
	for _, event := range events {
		if event.Name == name {
			seen = append(seen, event)
		}
	}
	return seen
}

// blockContent joins one block's published fragments as a client would.
func blockContent(events []StreamEvent, blockID string) string {
	var written strings.Builder
	for _, event := range events {
		if event.Name == "agent.text.delta" && event.BlockID == blockID {
			written.WriteString(event.Content)
		}
	}
	return written.String()
}

// TestTextBlocksStreamWhileTheModelIsStillWriting is the real-time claim on the wire this
// service produces: the provider is held inside its own stream, and the frames already
// delivered contain the text it wrote so far. The gate is synchronous with the frame hand-off,
// so what the client holds at that instant is a fact rather than a race.
func TestTextBlocksStreamWhileTheModelIsStillWriting(t *testing.T) {
	service, _, userID, sessionID, chat, _ := agentTurn(t,
		scriptRound{deltas: []string{"TCP 是字节流，", "不会保留消息边界", "。"}, gateAfter: 2},
	)
	arrived := make(chan StreamEvent, 32)
	done := make(chan error, 1)
	held := make(chan struct{})
	released := make(chan struct{})
	chat.hold = func() {
		held <- struct{}{}
		<-released
	}

	go func() {
		done <- service.StreamSteps(context.Background(), userID, sessionID, "RPC 的消息边界？",
			domain.KnowledgeOptions{}, func(event StreamEvent) error {
				arrived <- event
				return nil
			})
	}()

	select {
	case <-held:
	case <-time.After(3 * time.Second):
		t.Fatal("the provider never reached its gate")
	}
	var seen []StreamEvent
drain:
	for {
		select {
		case event := <-arrived:
			seen = append(seen, event)
		default:
			break drain
		}
	}
	var growing []string
	for _, event := range eventsNamed(seen, "agent.text.delta") {
		growing = append(growing, event.Content)
	}
	if strings.Join(growing, "") != "TCP 是字节流，不会保留消息边界" {
		t.Fatalf("text delivered while the model was still writing = %v", growing)
	}
	if len(eventsNamed(seen, "agent.text.ended")) != 0 {
		t.Fatal("a block ended before its provider had finished writing it")
	}
	if len(eventsNamed(seen, "message.delta")) != 0 {
		t.Fatalf("the turn also streamed %d message.delta frames beside its blocks",
			len(eventsNamed(seen, "message.delta")))
	}
	close(released)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case event := <-arrived:
			seen = append(seen, event)
		default:
			// Everything leaves through this callback, so once the turn has returned the
			// client's view is complete.
			completed := eventsNamed(seen, "message.completed")
			if len(completed) != 1 {
				t.Fatalf("%d completed frames, want exactly one: %v", len(completed), eventNames(seen))
			}
			final := completed[0].FinalBlockID
			if final == "" || blockContent(seen, final) != completed[0].Content {
				t.Fatalf("completed names block %q holding %q, content %q", final,
					blockContent(seen, final), completed[0].Content)
			}
			if completed[0].RunID == "" {
				t.Fatal("a completed frame lost the run it belongs to")
			}
			if got := eventsNamed(seen, "agent.text.ended"); len(got) != 1 ||
				got[0].Kind != "answer_candidate" || got[0].BlockID != final {
				t.Fatalf("blocks ended = %v, want the named final block confirmed as a candidate", got)
			}
			return
		}
	}
}

// TestNarratingAgentTurnConfirmsOnlyTheAnswerBlock is the full turn a real provider produces -
// narration, a tool call, then the answer - as the event list the browser is written from.
func TestNarratingAgentTurnConfirmsOnlyTheAnswerBlock(t *testing.T) {
	service, store, userID, sessionID, chat, search := agentTurn(t,
		scriptRound{deltas: []string{"我先查一下你的", "RPC 笔记。"}, toolNames: []string{agent.KnowledgeSearchName}},
		scriptRound{deltas: []string{"TCP 是字节流，", "不会保留消息边界", "。"}},
	)

	var events []StreamEvent
	if err := service.StreamSteps(context.Background(), userID, sessionID,
		"RPC 为什么没有消息边界？", domain.KnowledgeOptions{}, func(event StreamEvent) error {
			events = append(events, event)
			return nil
		}); err != nil {
		t.Fatalf("an agent turn that narrates before its tool call failed: %v", err)
	}
	if search.calls != 1 {
		t.Fatalf("the tool ran %d times, want once", search.calls)
	}
	if chat.calls != 2 {
		t.Fatalf("the model ran %d times, want one decision round plus the answer", chat.calls)
	}
	want := []string{
		"message.started",
		"step_started", "agent.text.started", "agent.text.delta", "agent.text.delta", "agent.text.ended",
		"step_started", "tool_started", "tool_result",
		"step_started", "agent.text.started", "agent.text.delta", "agent.text.delta", "agent.text.delta",
		"agent.text.ended", "message.completed",
	}
	if !reflect.DeepEqual(eventNames(events), want) {
		t.Fatalf("event order = %v,\nwant %v", eventNames(events), want)
	}
	kinds := eventsNamed(events, "agent.text.ended")
	if len(kinds) != 2 || kinds[0].Kind != "process" || kinds[1].Kind != "answer_candidate" {
		t.Fatalf("block kinds = %v, want a process note then a candidate answer", kinds)
	}
	// The round number belongs to the frame that opens a block, where the call it came from is
	// decided; the closing frame is about what the text turned out to be.
	opened := eventsNamed(events, "agent.text.started")
	if len(opened) != 2 || opened[0].Round != 1 || opened[0].BlockID != "blk-1" ||
		opened[1].Round != 2 || opened[1].BlockID != "blk-2" {
		t.Fatalf("blocks opened = %v, want blk-1 from round 1 and blk-2 from round 2", opened)
	}
	if len(eventsNamed(events, "message.delta")) != 0 {
		t.Fatal("the same text was also sent through the legacy answer channel")
	}
	completed := events[len(events)-1]
	if completed.Content != "TCP 是字节流，不会保留消息边界。" {
		t.Fatalf("completed content = %q", completed.Content)
	}
	if blockContent(events, completed.FinalBlockID) != completed.Content {
		t.Fatalf("final block %q holds %q, not the confirmed %q", completed.FinalBlockID,
			blockContent(events, completed.FinalBlockID), completed.Content)
	}
	if blockContent(events, kinds[0].BlockID) != "我先查一下你的RPC 笔记。" {
		t.Fatalf("the process note was rewritten: %q", blockContent(events, kinds[0].BlockID))
	}
	// What the learner read is what the record holds, byte for byte.
	messages, err := store.ListMessages(context.Background(), userID, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[1].Content != completed.Content {
		t.Fatalf("stored turn = %+v, want the answer the client streamed", messages)
	}
	for _, event := range events {
		if strings.Contains(event.Content, "secret-marker") || strings.Contains(completed.Content, "secret-marker") {
			t.Fatalf("tool internals leaked into the text stream: %+v", event)
		}
	}
}

// TestFailedAgentTurnKeepsItsTextButConfirmsNothing covers the failure direction end to end:
// text already on the screen stays, the block says it failed, and no completed frame ever
// names an answer.
func TestFailedAgentTurnKeepsItsTextButConfirmsNothing(t *testing.T) {
	service, store, userID, sessionID, _, search := agentTurn(t,
		scriptRound{deltas: []string{"写了一半", "后面没了"}, breakAfter: 1},
	)

	var events []StreamEvent
	err := service.StreamSteps(context.Background(), userID, sessionID, "问题", domain.KnowledgeOptions{},
		func(event StreamEvent) error {
			events = append(events, event)
			return nil
		})
	if !errors.Is(err, ErrInternal) {
		t.Fatalf("error = %v, want the internal failure a broken stream is", err)
	}
	if eventNames(events)[len(events)-1] != "message.failed" {
		t.Fatalf("the turn did not end in a failure frame: %v", eventNames(events))
	}
	ended := eventsNamed(events, "agent.text.ended")
	if len(ended) != 1 || ended[0].Kind != "failed" {
		t.Fatalf("blocks ended = %v, want one frame labelled failed", ended)
	}
	if blockContent(events, "blk-1") != "写了一半" {
		t.Fatalf("the text a client had already read was rewritten: %q", blockContent(events, "blk-1"))
	}
	if len(eventsNamed(events, "message.completed")) != 0 {
		t.Fatal("a broken stream confirmed an answer")
	}
	if search.calls != 0 {
		t.Fatalf("a broken stream still ran a tool %d time(s)", search.calls)
	}
	messages, err := store.ListMessages(context.Background(), userID, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].Role != "user" {
		t.Fatalf("a failed turn stored a record: %+v", messages)
	}
}

// TestCancelledAgentTurnStopsAtTheFrameThatCouldNotBeDelivered proves the rule on the path
// where text is written as it arrives: the frame a hung-up client cannot receive is what stops
// the loop, before the tool and before the next model call.
func TestCancelledAgentTurnStopsAtTheFrameThatCouldNotBeDelivered(t *testing.T) {
	service, store, userID, sessionID, chat, search := agentTurn(t,
		scriptRound{deltas: []string{"我先查一下", "再回答。"}, toolNames: []string{agent.KnowledgeSearchName}},
		scriptRound{deltas: []string{"不该到这里"}},
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var delivered int
	err := service.StreamSteps(ctx, userID, sessionID, "问题", domain.KnowledgeOptions{},
		func(StreamEvent) error {
			delivered++
			if delivered == 3 {
				// The client hangs up on the block's first text frame.
				cancel()
				return errors.New("connection reset")
			}
			return nil
		})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want the caller's cancellation", err)
	}
	if chat.calls != 1 {
		t.Fatalf("the model was asked %d times after the client hung up", chat.calls)
	}
	if search.calls != 0 {
		t.Fatalf("a cancelled turn still ran a tool %d time(s)", search.calls)
	}
	messages, err := store.ListMessages(context.Background(), userID, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].Role != "user" {
		t.Fatalf("a cancelled turn stored: %+v", messages)
	}
}

// TestPlainAgentStreamStillUsesTheLegacyAnswerChannel keeps the contract of a turn that asked
// for no progress: buffered answer text, message.delta frames, and no block to confirm.
func TestPlainAgentStreamStillUsesTheLegacyAnswerChannel(t *testing.T) {
	service, store, userID, sessionID, _, search := agentTurn(t,
		scriptRound{deltas: []string{"让我想想。"}, toolNames: []string{agent.KnowledgeSearchName}},
		scriptRound{deltas: []string{"答案", "来了。"}},
	)

	var events []StreamEvent
	if err := service.Stream(context.Background(), userID, sessionID,
		"问题", domain.KnowledgeOptions{}, func(event StreamEvent) error {
			events = append(events, event)
			return nil
		}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(eventNames(events),
		[]string{"message.started", "message.delta", "message.delta", "message.completed"}) {
		t.Fatalf("event order = %v", eventNames(events))
	}
	if search.calls != 1 {
		t.Fatalf("the tool ran %d times, want once", search.calls)
	}
	completed := events[len(events)-1]
	if completed.Content != "答案来了。" || completed.FinalBlockID != "" {
		t.Fatalf("completed = %+v, want the buffered answer with no block named", completed)
	}
	var streamed strings.Builder
	for _, event := range eventsNamed(events, "message.delta") {
		streamed.WriteString(event.Delta)
	}
	if streamed.String() != completed.Content {
		t.Fatalf("deltas %q are not the confirmed %q", streamed.String(), completed.Content)
	}
	for index, event := range events {
		if event.RunID != "" || event.BlockID != "" || event.Kind != "" || event.Round != 0 {
			t.Fatalf("frame %d carries step or block fields on a plain stream: %+v", index+1, event)
		}
	}
	messages, err := store.ListMessages(context.Background(), userID, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[1].Content != completed.Content {
		t.Fatalf("stored turn = %+v", messages)
	}
}

// blockOnlyResponder publishes text blocks and names an answer block, without any of the loop's
// own reasons for doing so. It exists to be caught lying: the two guards in the message service
// are about what happens when the block a turn confirms is not the text that was shown, and no
// honest strategy can produce that state.
type blockOnlyResponder struct {
	// published is what the turn hands to the progress channel.
	published []responder.Step
	// answer is what it returns once the text is out.
	answer responder.Answer
	calls  int
}

func (f *blockOnlyResponder) Respond(context.Context, responder.Request) (responder.Answer, error) {
	return f.answer, nil
}

func (f *blockOnlyResponder) Stream(_ context.Context, _ responder.Request, emit func(string) error) (responder.Answer, error) {
	if err := emit(f.answer.Content); err != nil {
		return responder.Answer{}, err
	}
	return f.answer, nil
}

func (f *blockOnlyResponder) SupportsSteps(responder.Request) bool { return true }

func (f *blockOnlyResponder) StreamSteps(_ context.Context, _ responder.Request, _ func(string) error,
	onStep responder.StepSink,
) (responder.Answer, error) {
	f.calls++
	for _, step := range f.published {
		if err := onStep(step); err != nil {
			return responder.Answer{}, err
		}
	}
	return f.answer, nil
}

func shownThenConfirmed(steps []responder.Step, shown, confirmed string) []responder.Step {
	return append(steps,
		responder.Step{Phase: responder.TextStarted, BlockID: shown, Round: 1},
		responder.Step{Phase: responder.TextDelta, BlockID: shown, Content: confirmed},
		responder.Step{Phase: responder.TextEnded, BlockID: shown, Kind: responder.TextKindCandidate},
	)
}

// TestConfirmedBlockMustBeAFinishedCandidate is the status half of the same rule. Text
// equality alone would confirm a note about a tool call, a round whose stream broke, or a block
// that was still being written - each of them can hold the same characters as the answer, and
// none of them is one. The reviewer's probe named all three and all three were stored; the
// service now asks how a block ended before it asks what it said.
func TestConfirmedBlockMustBeAFinishedCandidate(t *testing.T) {
	for _, kind := range []string{"", "process", "failed"} {
		name := kind
		if name == "" {
			name = "never-ended"
		}
		t.Run(name, func(t *testing.T) {
			published := []responder.Step{
				{Phase: responder.TextStarted, BlockID: "blk-1", Round: 1},
				{Phase: responder.TextDelta, BlockID: "blk-1", Content: "SAME TEXT"},
			}
			if kind != "" {
				published = append(published, responder.Step{Phase: responder.TextEnded, BlockID: "blk-1",
					Kind: responder.TextKind(kind)})
			}
			assistant := &blockOnlyResponder{
				published: published,
				answer:    responder.Answer{Content: "SAME TEXT", FinalBlockID: "blk-1"},
			}
			service, store, userID, sessionID := newMessageFixture(t, assistant, domain.SessionModeLearningAgent)

			var events []StreamEvent
			err := service.StreamSteps(context.Background(), userID, sessionID, "问题", domain.KnowledgeOptions{},
				func(event StreamEvent) error {
					events = append(events, event)
					return nil
				})
			if !errors.Is(err, ErrAnswerBlockDisagrees) {
				t.Fatalf("error = %v, want a %q block refused as an answer", err, kind)
			}
			if len(eventsNamed(events, "message.completed")) != 0 {
				t.Fatal("a block that never finished as a candidate was confirmed")
			}
			messages, readErr := store.ListMessages(context.Background(), userID, sessionID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if len(messages) != 1 || messages[0].Role != "user" {
				t.Fatalf("a refused confirmation still wrote a record: %+v", messages)
			}
		})
	}
}

// TestTruncatedWireBecomesNeitherAnAnswerNorARecord runs the whole chain a real turn uses - a
// local stand-in provider, the real OpenAI reader, the real harness, this service - against the
// two ways a provider stops early: the body simply ends, and the provider reports it ran out of
// output room. Both used to reach `message.completed` with a half sentence, because the parser
// recorded the missing end as a finish reason rather than as a failure.
func TestTruncatedWireBecomesNeitherAnAnswerNorARecord(t *testing.T) {
	for _, tail := range []struct {
		name  string
		frame string
	}{
		{"closed_without_marker", ""},
		{"stopped_at_output_limit", `{"choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`},
	} {
		t.Run(tail.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, sseChunk("PARTIAL ANSWER")+tail.frame)
			}))
			defer upstream.Close()
			client, err := openai.New(openai.Config{BaseURL: upstream.URL, APIKey: "review.fake.key",
				Model: "review-local"})
			if err != nil {
				t.Fatal(err)
			}
			search := &countedTool{name: agent.KnowledgeSearchName}
			registry, err := agent.NewRegistry(search)
			if err != nil {
				t.Fatal(err)
			}
			harness, err := agent.New(client, registry, agent.Budget{})
			if err != nil {
				t.Fatal(err)
			}
			assistant, err := responder.NewStrategyResponder(harness)
			if err != nil {
				t.Fatal(err)
			}
			service, store, userID, sessionID := newMessageFixture(t, assistant, domain.SessionModeLearningAgent)

			var events []StreamEvent
			streamErr := service.StreamSteps(context.Background(), userID, sessionID, "问题",
				domain.KnowledgeOptions{}, func(event StreamEvent) error {
					events = append(events, event)
					return nil
				})
			if !errors.Is(streamErr, ErrInternal) {
				t.Fatalf("error = %v, want the internal failure a cut-off stream is", streamErr)
			}
			// The text the provider did send is still on the wire - as a block that says it
			// failed, which is the only honest label for a prefix.
			if got := blockContent(events, "blk-1"); got != "PARTIAL ANSWER" {
				t.Fatalf("already published text = %q, want it kept for the learner", got)
			}
			ended := eventsNamed(events, "agent.text.ended")
			if len(ended) != 1 || ended[0].Kind != "failed" {
				t.Fatalf("blocks ended = %v, want the cut-off round closed as failed", ended)
			}
			if len(eventsNamed(events, "message.completed")) != 0 {
				t.Fatal("a truncated stream was confirmed as an answer")
			}
			if eventNames(events)[len(events)-1] != "message.failed" {
				t.Fatalf("last event = %q, want the failure frame", eventNames(events)[len(events)-1])
			}
			messages, readErr := store.ListMessages(context.Background(), userID, sessionID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if len(messages) != 1 || messages[0].Role != "user" {
				t.Fatalf("a truncated turn stored: %+v", messages)
			}
		})
	}
}

// sseChunk renders one provider delta the way the wire sends it.
func sseChunk(content string) string {
	return `data: {"choices":[{"index":0,"delta":{"content":"` + content + `"}}]}` + "\n\n"
}

// TestPlainChatCannotStoreATruncatedAnswer either: the tool-free chat stream is the same
// protocol with the same ways to stop early, and a direct answer has no block frames to carry
// the bad news, so it must fail instead of storing a prefix.
func TestPlainChatCannotStoreATruncatedAnswer(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseChunk("半句"))
	}))
	defer upstream.Close()
	client, err := openai.New(openai.Config{BaseURL: upstream.URL, APIKey: "review.fake.key",
		Model: "review-local"})
	if err != nil {
		t.Fatal(err)
	}
	direct, err := responder.NewModelStrategy(domain.AnswerStrategyDirectChat, client, responder.DirectSystemPrompt)
	if err != nil {
		t.Fatal(err)
	}
	assistant, err := responder.NewStrategyResponder(direct)
	if err != nil {
		t.Fatal(err)
	}
	service, store, userID, sessionID := newMessageFixture(t, assistant, domain.SessionModeChat)

	var events []StreamEvent
	if err := service.Stream(context.Background(), userID, sessionID, "问题", domain.KnowledgeOptions{},
		func(event StreamEvent) error {
			events = append(events, event)
			return nil
		}); !errors.Is(err, ErrInternal) {
		t.Fatalf("error = %v, want a cut-off chat stream refused", err)
	}
	if len(eventsNamed(events, "message.completed")) != 0 {
		t.Fatal("a truncated chat answer was confirmed")
	}
	messages, readErr := store.ListMessages(context.Background(), userID, sessionID)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(messages) != 1 || messages[0].Role != "user" {
		t.Fatalf("a truncated chat turn stored: %+v", messages)
	}
}

// TestConfirmedAnswerMustBeTheTextThatWasShown is the guard in the direction a strategy can fail
// in rather than only refuse to: it names a block, and the text it returns is not that block's.
// Nothing is stored and nothing is confirmed, because a receipt for words nobody read is worse
// than no receipt.
func TestConfirmedAnswerMustBeTheTextThatWasShown(t *testing.T) {
	assistant := &blockOnlyResponder{
		published: shownThenConfirmed(nil, "blk-1", "屏幕上的一句话"),
		answer:    responder.Answer{Content: "屏幕上没有这句话", FinalBlockID: "blk-1"},
	}
	service, store, userID, sessionID := newMessageFixture(t, assistant, domain.SessionModeLearningAgent)

	var events []StreamEvent
	err := service.StreamSteps(context.Background(), userID, sessionID, "问题", domain.KnowledgeOptions{},
		func(event StreamEvent) error {
			events = append(events, event)
			return nil
		})
	if !errors.Is(err, ErrAnswerBlockDisagrees) {
		t.Fatalf("error = %v, want the block disagreement the service is guarding", err)
	}
	names := eventNames(events)
	if names[len(names)-1] != "message.failed" {
		t.Fatalf("event order = %v, want the turn to end in a failure frame", names)
	}
	if len(eventsNamed(events, "message.completed")) != 0 {
		t.Fatal("a confirmed answer that nobody was shown still reached a completed frame")
	}
	messages, err := store.ListMessages(context.Background(), userID, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].Role != "user" {
		t.Fatalf("the guard ran after the write: %+v", messages)
	}
}

// TestConfirmedAnswerMustSurviveTheWrite covers the other half: a store that kept something
// other than what it was handed. The completed frame is written from the stored row, so without
// the second guard the client would confirm text that exists only in the database.
func TestConfirmedAnswerMustSurviveTheWrite(t *testing.T) {
	underlying := repository.NewInMemoryRepository()
	ctx := context.Background()
	user, err := underlying.CreateUser(ctx, &domain.User{Email: "writer@example.com", DisplayName: "Writer"})
	if err != nil {
		t.Fatal(err)
	}
	session, err := underlying.CreateSession(ctx, &domain.Session{
		UserID: user.ID, Title: "Agent", Mode: domain.SessionModeLearningAgent})
	if err != nil {
		t.Fatal(err)
	}
	assistant := &blockOnlyResponder{
		published: shownThenConfirmed(nil, "blk-1", "屏幕上的一句话"),
		answer:    responder.Answer{Content: "屏幕上的一句话", FinalBlockID: "blk-1"},
	}
	service := NewMessageService(rewroteOnWrite{underlying}, assistant, WithCollectionAccess(forbiddenAccess{}))

	var events []StreamEvent
	err = service.StreamSteps(ctx, user.ID, session.ID, "问题", domain.KnowledgeOptions{},
		func(event StreamEvent) error {
			events = append(events, event)
			return nil
		})
	if !errors.Is(err, ErrAnswerBlockDisagrees) {
		t.Fatalf("error = %v, want the stored text to be refused, not confirmed", err)
	}
	if len(eventsNamed(events, "message.completed")) != 0 {
		t.Fatalf("the client was handed a completed frame for text it never saw: %v", eventNames(events))
	}
}

// TestACandidateBlockIsConfirmedEvenWhenTheStrategyForgetsToNameIt pins the inference the
// receipt depends on: a turn that showed exactly one answer candidate and returned text that is
// that candidate gets its block named on the completed frame anyway. The alternative - a
// completed frame with no address, on a turn where the client has been growing several blocks -
// would leave the browser unable to tell which paragraph it just confirmed.
func TestACandidateBlockIsConfirmedEvenWhenTheStrategyForgetsToNameIt(t *testing.T) {
	assistant := &blockOnlyResponder{
		published: shownThenConfirmed(nil, "blk-1", "屏幕上的一句话"),
		answer:    responder.Answer{Content: "屏幕上的一句话"},
	}
	service, store, userID, sessionID := newMessageFixture(t, assistant, domain.SessionModeLearningAgent)

	var events []StreamEvent
	if err := service.StreamSteps(context.Background(), userID, sessionID, "问题", domain.KnowledgeOptions{},
		func(event StreamEvent) error {
			events = append(events, event)
			return nil
		}); err != nil {
		t.Fatal(err)
	}
	completed := eventsNamed(events, "message.completed")
	if len(completed) != 1 || completed[0].FinalBlockID != "blk-1" {
		t.Fatalf("completed = %v, want the one candidate block named", completed)
	}
	if blockContent(events, "blk-1") != completed[0].Content {
		t.Fatalf("confirmed %q is not the block's %q", completed[0].Content, blockContent(events, "blk-1"))
	}
	messages, err := store.ListMessages(context.Background(), userID, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[1].Content != completed[0].Content {
		t.Fatalf("stored turn = %+v", messages)
	}
}

// rewroteOnWrite stands in for the storage layer quietly keeping a different sentence: the one
// change to an answer after it was streamed that a learner could otherwise never notice.
type rewroteOnWrite struct {
	repository.Repository
}

func (s rewroteOnWrite) AppendMessage(ctx context.Context, userID, sessionID string,
	message *domain.Message,
) (*domain.Message, error) {
	stored := *message
	stored.Content += "（服务端补充）"
	return s.Repository.AppendMessage(ctx, userID, sessionID, &stored)
}
