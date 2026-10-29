package agent

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/model"
	"github.com/xiaopang456/GopherLearn/internal/responder"
)

// gatedStreamModel is a provider that streams a tool-capable turn and can block inside the
// stream until the test lets it continue. The block is the whole point: it is the only way to
// read a client's view at a moment when the provider has not finished, which is what separates
// text that was published while it was being generated from a finished answer sliced
// afterwards.
//
// It refuses the non-streaming tool path, so a loop that "streamed" by asking the model twice
// fails here instead of looking correct.
type gatedStreamModel struct {
	rounds   []gatedRound
	calls    int
	requests []model.Request
	entered  chan struct{}
	release  chan struct{}
}

type gatedRound struct {
	// deltas are the content fragments in arrival order.
	deltas []string
	// toolNames asks for one tool per name.
	toolNames []string
	// toolAfterDeltas is how many fragments arrive before the tool request does. A real
	// provider narrating its decision uses a nonzero value here, and the turn must still
	// work: on the blocks path that text is kept and labelled a process note, and on the
	// plain path it is dropped before anybody read it.
	toolAfterDeltas int
	// gateAfter blocks the round once that many fragments have been handed over, so a test
	// can read what the client already holds while generation is still running. Only the
	// round a test is watching sets it; a round nobody releases would hang.
	gateAfter int
	// errAfterDeltas fails the round after that many fragments were handed over, which is
	// how a provider whose stream breaks mid-answer is simulated.
	errAfterDeltas int
}

func (m *gatedStreamModel) Complete(context.Context, model.Request) (model.Completion, error) {
	return model.Completion{}, errors.New("test model: the plain completion path must not be used")
}

func (m *gatedStreamModel) Stream(context.Context, model.Request, model.DeltaHandler) (model.Completion, error) {
	return model.Completion{}, errors.New("test model: the tool-free stream path must not be used")
}

func (m *gatedStreamModel) CompleteWithTools(context.Context, model.Request) (model.Completion, error) {
	return model.Completion{}, errors.New("test model: a streamed turn must not be re-requested without streaming")
}

func (m *gatedStreamModel) StreamWithTools(ctx context.Context, request model.Request,
	onDelta model.StreamDeltaHandler,
) (model.Completion, error) {
	m.calls++
	m.requests = append(m.requests, request)
	if m.calls > len(m.rounds) {
		return model.Completion{}, errors.New("test model: the script ran out of rounds")
	}
	round := m.rounds[m.calls-1]
	var content strings.Builder
	send := func(from, to int) error {
		for index := from; index < to && index < len(round.deltas); index++ {
			fragment := model.StreamDelta{Content: round.deltas[index]}
			if err := onDelta(fragment); err != nil {
				return err
			}
			content.WriteString(round.deltas[index])
			if round.gateAfter > 0 && index+1 == round.gateAfter {
				select {
				case m.entered <- struct{}{}:
				case <-ctx.Done():
					return ctx.Err()
				}
				select {
				case <-m.release:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			if round.errAfterDeltas > 0 && index+1 == round.errAfterDeltas {
				return model.ErrUnavailable
			}
		}
		return nil
	}
	if err := send(0, round.toolAfterDeltas); err != nil {
		return model.Completion{}, err
	}
	var calls []model.ToolCall
	for index, name := range round.toolNames {
		if err := onDelta(model.StreamDelta{ToolCall: true}); err != nil {
			return model.Completion{}, err
		}
		calls = append(calls, model.ToolCall{ID: "call-" + strconv.Itoa(index), Name: name,
			Arguments: `{"query":"fsync"}`})
	}
	if err := send(round.toolAfterDeltas, len(round.deltas)); err != nil {
		return model.Completion{}, err
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

func newGatedModel(rounds ...gatedRound) *gatedStreamModel {
	return &gatedStreamModel{rounds: rounds, entered: make(chan struct{}, 4), release: make(chan struct{}, 4)}
}

// recorder is the client's side of one turn: every answer delta and every frame of the
// progress channel, in arrival order.
type recorder struct {
	deltas []string
	steps  []responder.Step
}

func (r *recorder) emit(delta string) error {
	r.deltas = append(r.deltas, delta)
	return nil
}

func (r *recorder) step(step responder.Step) error {
	r.steps = append(r.steps, step)
	return nil
}

func (r *recorder) answer() string { return strings.Join(r.deltas, "") }

func (r *recorder) stepPhases() string {
	names := make([]string, 0, len(r.steps))
	for _, step := range r.steps {
		names = append(names, string(step.Phase))
	}
	return strings.Join(names, " ")
}

// fragmentsOf returns the text of one block, in the order its delta frames arrived.
func (r *recorder) fragmentsOf(blockID string) string {
	var written strings.Builder
	for _, step := range r.steps {
		if step.Phase == responder.TextDelta && step.BlockID == blockID {
			written.WriteString(step.Content)
		}
	}
	return written.String()
}

// kindsOfBlock reports the id and closing kind of every block the turn published, in order.
func (r *recorder) kindsOfBlock() []string {
	var seen []string
	for _, step := range r.steps {
		if step.Phase == responder.TextEnded {
			seen = append(seen, step.BlockID+":"+string(step.Kind))
		}
	}
	return seen
}

// TestStreamStepsPublishesTextWhileTheProviderIsStillWriting is the real-time claim, proved
// the only way it can be proved: the model is blocked inside its own stream, and the frames a
// client already holds contain answer text. A turn that waited for the round to end before
// showing anything would hold nothing at this moment, however many fragments the script has.
func TestStreamStepsPublishesTextWhileTheProviderIsStillWriting(t *testing.T) {
	chat := newGatedModel(gatedRound{
		deltas: []string{"TCP 是字节流，", "不会保留消息边界", "。"}, gateAfter: 2,
	})
	harness := newHarness(t, chat, Budget{}, &recordingTool{name: KnowledgeSearchName})

	arrived := make(chan responder.Step, 16)
	var emitted int
	done := make(chan error, 1)
	go func() {
		_, err := harness.StreamSteps(context.Background(), baseRequest(),
			func(string) error {
				emitted++
				return nil
			},
			func(step responder.Step) error {
				arrived <- step
				return nil
			})
		done <- err
	}()

	select {
	case <-chat.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("the stream never reached its gate")
	}
	var text []string
	var blockID string
	var announced int
collect:
	for {
		select {
		case step := <-arrived:
			switch step.Phase {
			case responder.StepStarted:
				announced++
			case responder.TextStarted:
				blockID = step.BlockID
			case responder.TextDelta:
				text = append(text, step.Content)
			}
		default:
			break collect
		}
	}
	if announced != 1 {
		t.Fatalf("%d step frames had been announced mid-round, want the running call alone", announced)
	}
	if blockID == "" {
		t.Fatal("no block had been opened while the provider was still writing")
	}
	if strings.Join(text, "") != "TCP 是字节流，不会保留消息边界" {
		t.Fatalf("text published mid-round = %v, want both fragments the provider had produced", text)
	}
	if emitted != 0 {
		t.Fatalf("the turn used the message.delta channel %d times beside its blocks", emitted)
	}

	chat.release <- struct{}{}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// Every frame left the loop through the sink, so once StreamSteps has returned the channel
	// holds the whole turn.
	var closed string
	for {
		select {
		case step := <-arrived:
			switch step.Phase {
			case responder.TextDelta:
				text = append(text, step.Content)
			case responder.TextEnded:
				closed = step.BlockID + ":" + string(step.Kind)
			}
		default:
			if strings.Join(text, "") != "TCP 是字节流，不会保留消息边界。" {
				t.Fatalf("the block's fragments = %q", strings.Join(text, ""))
			}
			if closed != blockID+":answer_candidate" {
				t.Fatalf("block %q closed as %q, want the candidate answer", blockID, closed)
			}
			if emitted != 0 {
				t.Fatalf("the turn used the message.delta channel %d times beside its blocks", emitted)
			}
			if chat.calls != 1 {
				t.Fatalf("the model was asked %d times, want one round published live", chat.calls)
			}
			return
		}
	}
}

// TestAnswerBlockIsNamedAndExactlyThePublishedText pins the three-way agreement the whole
// protocol buys: the fragments of the block the loop names as its answer, the answer the loop
// returns, and the text of the turn are the same bytes.
func TestAnswerBlockIsNamedAndExactlyThePublishedText(t *testing.T) {
	chat := newGatedModel(gatedRound{deltas: []string{"TCP 是字节流，", "不会保留消息边界", "。"}})
	harness := newHarness(t, chat, Budget{}, &recordingTool{name: KnowledgeSearchName})

	var client recorder
	answer, err := harness.StreamSteps(context.Background(), baseRequest(), client.emit, client.step)
	if err != nil {
		t.Fatal(err)
	}
	if answer.FinalBlockID == "" {
		t.Fatal("an answer published as a block named no block")
	}
	if client.fragmentsOf(answer.FinalBlockID) != answer.Content {
		t.Fatalf("block %s holds %q, answer %q", answer.FinalBlockID,
			client.fragmentsOf(answer.FinalBlockID), answer.Content)
	}
	if got := client.kindsOfBlock(); len(got) != 1 || got[0] != answer.FinalBlockID+":answer_candidate" {
		t.Fatalf("blocks = %v, want one candidate answer", got)
	}
	if len(client.deltas) != 0 {
		t.Fatalf("a blocks turn also sent %v on the answer channel", client.deltas)
	}
}

// TestToolRoundTextSurvivesAsAProcessBlock is the regression a real provider produced: the
// stream writes "let me look that up" and only then asks for a tool. On this path the text has
// already been shown, so the turn keeps it, labels the block a process note, and finishes.
func TestToolRoundTextSurvivesAsAProcessBlock(t *testing.T) {
	chat := newGatedModel(
		gatedRound{
			deltas:          []string{"我先查一下你的", "RPC 笔记。"},
			toolAfterDeltas: 2,
			toolNames:       []string{KnowledgeSearchName},
		},
		gatedRound{deltas: []string{"TCP 是字节流，", "不会保留消息边界。"}},
	)
	knowledge := &recordingTool{name: KnowledgeSearchName,
		observations: []Observation{{Text: "资料原文", Summary: "检索到 1 条资料", Found: true}}}
	harness := newHarness(t, chat, Budget{}, knowledge)

	var client recorder
	answer, err := harness.StreamSteps(context.Background(), baseRequest(), client.emit, client.step)
	if err != nil {
		t.Fatalf("a provider that narrates before calling a tool failed the turn: %v", err)
	}
	if len(knowledge.calls) != 1 {
		t.Fatalf("the tool ran %d times, want once", len(knowledge.calls))
	}
	got := client.kindsOfBlock()
	want := []string{"blk-1:process", "blk-2:answer_candidate"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("blocks = %v, want %v", got, want)
	}
	if client.fragmentsOf("blk-1") != "我先查一下你的RPC 笔记。" {
		t.Fatalf("the process note reached the client as %q", client.fragmentsOf("blk-1"))
	}
	if client.fragmentsOf("blk-2") != answer.Content || answer.FinalBlockID != "blk-2" {
		t.Fatalf("answer %q is not block 2 (%q), final=%q", answer.Content,
			client.fragmentsOf("blk-2"), answer.FinalBlockID)
	}
	if strings.Contains(client.fragmentsOf("blk-2"), "我先查一下") {
		t.Fatal("the two rounds' text was merged into one block")
	}
	if chat.calls != 2 {
		t.Fatalf("the model ran %d times, want the decision round plus the answer", chat.calls)
	}
}

// TestRoundsKeepTheirOwnBlocksAcrossTwoToolCalls covers the longer loop: three model calls, two
// of them decisions, and a block per call whose round number says which call it came from.
func TestRoundsKeepTheirOwnBlocksAcrossTwoToolCalls(t *testing.T) {
	chat := newGatedModel(
		gatedRound{deltas: []string{"先查知识库。"}, toolNames: []string{KnowledgeSearchName}},
		gatedRound{deltas: []string{"再查记忆。"}, toolNames: []string{MemorySearchName}},
		gatedRound{deltas: []string{"两轮", "都用上了。"}},
	)
	knowledge := &recordingTool{name: KnowledgeSearchName, observations: []Observation{{Text: "资料", Found: true}}}
	memory := &recordingTool{name: MemorySearchName, observations: []Observation{{Text: "记忆", Found: true}}}
	harness := newHarness(t, chat, Budget{MaxModelCalls: 4, MaxToolCalls: 3, MaxSteps: 10}, knowledge, memory)

	var client recorder
	answer, err := harness.StreamSteps(context.Background(), baseRequest(), client.emit, client.step)
	if err != nil {
		t.Fatal(err)
	}
	if len(knowledge.calls) != 1 || len(memory.calls) != 1 {
		t.Fatalf("knowledge=%d memory=%d, want each once across two rounds", len(knowledge.calls), len(memory.calls))
	}
	if chat.calls != 3 {
		t.Fatalf("the model ran %d times, want three rounds", chat.calls)
	}
	got := strings.Join(client.kindsOfBlock(), " ")
	want := "blk-1:process blk-2:process blk-3:answer_candidate"
	if got != want {
		t.Fatalf("blocks = %s, want %s", got, want)
	}
	var rounds []string
	for _, step := range client.steps {
		if step.Phase == responder.TextStarted {
			rounds = append(rounds, step.BlockID+"="+strconv.Itoa(step.Round))
		}
	}
	if strings.Join(rounds, " ") != "blk-1=1 blk-2=2 blk-3=3" {
		t.Fatalf("block rounds = %v", rounds)
	}
	if client.fragmentsOf("blk-3") != "两轮都用上了。" || answer.FinalBlockID != "blk-3" {
		t.Fatalf("answer=%q final=%q blocks=%v", answer.Content, answer.FinalBlockID, client.steps)
	}
}

// TestASilentRoundOpensNoBlock keeps the frame set honest in the other direction: a model call
// that says nothing publicly asks for a tool and produces no text block at all, so a client
// never has to render an empty paragraph to explain a round.
func TestASilentRoundOpensNoBlock(t *testing.T) {
	chat := newGatedModel(
		gatedRound{toolNames: []string{KnowledgeSearchName}},
		gatedRound{deltas: []string{"答案。"}},
	)
	knowledge := &recordingTool{name: KnowledgeSearchName, observations: []Observation{{Text: "资料", Found: true}}}
	harness := newHarness(t, chat, Budget{}, knowledge)

	var client recorder
	if _, err := harness.StreamSteps(context.Background(), baseRequest(), client.emit, client.step); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(client.kindsOfBlock(), " "); got != "blk-2:answer_candidate" {
		t.Fatalf("blocks = %s, want only the round that said something", got)
	}
	for _, step := range client.steps {
		if step.Phase == responder.TextStarted && step.BlockID != "blk-2" {
			t.Fatalf("a silent round announced block %q", step.BlockID)
		}
	}
}

// TestFailedRoundClosesItsBlockAsIncomplete covers the failure direction: text a learner has
// already watched stays, and the closing frame is what says it never became an answer.
func TestFailedRoundClosesItsBlockAsIncomplete(t *testing.T) {
	chat := newGatedModel(gatedRound{deltas: []string{"写了一半", "后面没了"}, errAfterDeltas: 1})
	knowledge := &recordingTool{name: KnowledgeSearchName, observations: []Observation{{Text: "资料", Found: true}}}
	harness := newHarness(t, chat, Budget{}, knowledge)

	var client recorder
	_, err := harness.StreamSteps(context.Background(), baseRequest(), client.emit, client.step)
	if !errors.Is(err, model.ErrUnavailable) {
		t.Fatalf("error = %v, want the provider failure", err)
	}
	if got := strings.Join(client.kindsOfBlock(), " "); got != "blk-1:failed" {
		t.Fatalf("blocks = %s, want the broken round closed as failed", got)
	}
	if client.fragmentsOf("blk-1") != "写了一半" {
		t.Fatalf("already published text was rewritten: %q", client.fragmentsOf("blk-1"))
	}
	if len(knowledge.calls) != 0 {
		t.Fatalf("a failed turn still ran a tool %d time(s)", len(knowledge.calls))
	}
}

// TestProgressSinkFailureStopsBeforeTheToolAndTheNextCall is the cancellation rule on the path
// where text is written as it arrives: the failed write is the notice that nobody is watching,
// and the loop must stop before spending a tool call or another model call.
func TestProgressSinkFailureStopsBeforeTheToolAndTheNextCall(t *testing.T) {
	chat := newGatedModel(gatedRound{
		deltas:          []string{"我先查一下", "再回答。"},
		toolAfterDeltas: 2,
		toolNames:       []string{KnowledgeSearchName},
	})
	knowledge := &recordingTool{name: KnowledgeSearchName, observations: []Observation{{Text: "资料", Found: true}}}
	harness := newHarness(t, chat, Budget{}, knowledge)

	seen := 0
	sink := func(responder.Step) error {
		seen++
		// The client hangs up at the second text frame of the round: the fragments before it
		// were delivered, and nothing after it may be paid for.
		if seen >= 3 {
			return context.Canceled
		}
		return nil
	}
	_, err := harness.StreamSteps(context.Background(), baseRequest(), func(string) error { return nil }, sink)
	if !errors.Is(err, ErrStepDelivery) || !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want an undeliverable step caused by the caller's cancellation", err)
	}
	if chat.calls != 1 {
		t.Fatalf("the model was asked %d times after the audience left", chat.calls)
	}
	if len(knowledge.calls) != 0 {
		t.Fatalf("a turn nobody was watching still ran a tool %d time(s)", len(knowledge.calls))
	}
}

// TestStepFramesAndBlocksStayInOrderOnOneChannel is the ordering claim: a process note lands
// before the tool it led to, and the answer note after the call that produced it, because both
// kinds of frame leave through the same sink.
func TestStepFramesAndBlocksStayInOrderOnOneChannel(t *testing.T) {
	chat := newGatedModel(
		gatedRound{deltas: []string{"查一下。"}, toolNames: []string{KnowledgeSearchName}},
		gatedRound{deltas: []string{"答案来了。"}},
	)
	knowledge := &recordingTool{name: KnowledgeSearchName,
		observations: []Observation{{Text: "资料", Found: true, Summary: "检索到 1 条资料"}}}
	harness := newHarness(t, chat, Budget{}, knowledge)

	var client recorder
	if _, err := harness.StreamSteps(context.Background(), baseRequest(), client.emit, client.step); err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"step_started", "agent.text.started", "agent.text.delta", "agent.text.ended",
		"step_started", "tool_started", "tool_result",
		"step_started", "agent.text.started", "agent.text.delta", "agent.text.ended",
	}, " ")
	if client.stepPhases() != want {
		t.Fatalf("frame order = %s,\nwant %s", client.stepPhases(), want)
	}
}

// TestNonStreamingModelOnTheProgressPathStillPublishesOneBlock is the honest fallback: a
// provider with no streamed tool turn is asked once per round, not twice, and its text still
// arrives as a block - filled in one piece, which is all the transport can offer.
func TestNonStreamingModelOnTheProgressPathStillPublishesOneBlock(t *testing.T) {
	chat := &scriptedToolModel{replies: []model.Completion{
		toolCallReply("call-1", KnowledgeSearchName, `{"query":"fsync"}`),
		answerReply("整体回答。"),
	}}
	knowledge := &recordingTool{name: KnowledgeSearchName, observations: []Observation{{Text: "资料", Found: true}}}
	harness := newHarness(t, chat, Budget{}, knowledge)

	var client recorder
	answer, err := harness.StreamSteps(context.Background(), baseRequest(), client.emit, client.step)
	if err != nil {
		t.Fatal(err)
	}
	if len(client.deltas) != 0 {
		t.Fatalf("the turn re-chunked the answer into %v beside its blocks", client.deltas)
	}
	if got := strings.Join(client.kindsOfBlock(), " "); got != "blk-2:answer_candidate" {
		t.Fatalf("blocks = %s, want one candidate answer for the round that answered", got)
	}
	if client.fragmentsOf("blk-2") != "整体回答。" || answer.FinalBlockID != "blk-2" {
		t.Fatalf("answer=%q final=%q", answer.Content, answer.FinalBlockID)
	}
	if len(chat.requests) != 2 {
		t.Fatalf("the model was asked %d times, want one decision and one answer", len(chat.requests))
	}
}

// --- the turn without progress frames: one buffered round, message deltas only ---

// TestNarrationThenAToolCallCompletesTheTurn is the same provider shape on the plain path,
// where no block frames exist to label a round. The narration is dropped before it is shown,
// which is the only way this contract can keep "the answer is what the client read" true.
func TestNarrationThenAToolCallCompletesTheTurn(t *testing.T) {
	chat := newGatedModel(
		gatedRound{
			deltas:          []string{"我先查一下资料", "再回答。"},
			toolAfterDeltas: 2,
			toolNames:       []string{KnowledgeSearchName},
		},
		gatedRound{deltas: []string{"AOF 可以选 everysec。"}},
	)
	knowledge := &recordingTool{name: KnowledgeSearchName,
		observations: []Observation{{Text: "资料原文", Summary: "检索到 1 条资料", Found: true}}}
	harness := newHarness(t, chat, Budget{}, knowledge)

	var client recorder
	answer, err := harness.Stream(context.Background(), baseRequest(), client.emit)
	if err != nil {
		t.Fatalf("a provider that narrates before calling a tool failed the turn: %v", err)
	}
	if len(knowledge.calls) != 1 {
		t.Fatalf("the tool ran %d times, want once", len(knowledge.calls))
	}
	for _, delta := range client.deltas {
		if strings.Contains(delta, "我先查一下") {
			t.Fatalf("a decision round's narration reached the client: %v", client.deltas)
		}
	}
	if client.answer() != answer.Content || answer.Content != "AOF 可以选 everysec。" {
		t.Fatalf("published %q, answer %q", client.answer(), answer.Content)
	}
	if answer.FinalBlockID != "" {
		t.Fatalf("a turn with no blocks named block %q", answer.FinalBlockID)
	}
	if chat.calls != 2 {
		t.Fatalf("the model ran %d times, want the decision round plus the answer", chat.calls)
	}
}

// TestRoundTextIsHeldUntilTheRoundEnds pins the plain path's rule: while the provider is still
// inside a call, the client holds nothing, because the call has not yet said whether it was an
// answer.
func TestRoundTextIsHeldUntilTheRoundEnds(t *testing.T) {
	chat := newGatedModel(gatedRound{deltas: []string{"AOF 的 fsync ", "可以选 everysec", "。"}, gateAfter: 2})
	harness := newHarness(t, chat, Budget{}, &recordingTool{name: KnowledgeSearchName})

	deltas := make(chan string, 8)
	done := make(chan error, 1)
	go func() {
		_, err := harness.Stream(context.Background(), baseRequest(), func(delta string) error {
			deltas <- delta
			return nil
		})
		done <- err
	}()

	select {
	case <-chat.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("the stream never reached its gate")
	}
	select {
	case early := <-deltas:
		t.Fatalf("the client was given %q while the model call was still open", early)
	default:
	}

	chat.release <- struct{}{}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var held []string
	for len(held) < 3 {
		select {
		case delta := <-deltas:
			held = append(held, delta)
		default:
			t.Fatalf("the answer arrived in %d publishes, want the round's 3 chunks: %v", len(held), held)
		}
	}
	if strings.Join(held, "") != "AOF 的 fsync 可以选 everysec。" {
		t.Fatalf("published text = %v", held)
	}
	if chat.calls != 1 {
		t.Fatalf("the model was asked %d times, want one call for one round", chat.calls)
	}
}

func TestToolRoundTextNeverReachesTheClient(t *testing.T) {
	chat := newGatedModel(
		gatedRound{deltas: []string{"我先查一下记忆。"}, toolNames: []string{KnowledgeSearchName}},
		gatedRound{deltas: []string{"真正的答案。"}},
	)
	knowledge := &recordingTool{name: KnowledgeSearchName, observations: []Observation{{Text: "资料", Found: true}}}
	harness := newHarness(t, chat, Budget{}, knowledge)

	var client recorder
	answer, err := harness.Stream(context.Background(), baseRequest(), client.emit)
	if err != nil {
		t.Fatal(err)
	}
	for _, delta := range client.deltas {
		if strings.Contains(delta, "我先查一下") {
			t.Fatalf("a decision round's narration was published: %v", client.deltas)
		}
	}
	if client.answer() != answer.Content {
		t.Fatalf("answer %q differs from the published deltas %v", answer.Content, client.deltas)
	}
	if len(knowledge.calls) != 1 {
		t.Fatalf("the tool ran %d times, want once", len(knowledge.calls))
	}
	if chat.calls != 2 {
		t.Fatalf("the model ran %d times, want the decision round plus the answer", chat.calls)
	}
}

func TestStreamStillRunsMultiRoundToolLoops(t *testing.T) {
	chat := newGatedModel(
		gatedRound{deltas: []string{"先查知识库。"}, toolNames: []string{KnowledgeSearchName}},
		gatedRound{deltas: []string{"再查记忆。"}, toolNames: []string{MemorySearchName}},
		gatedRound{deltas: []string{"两轮都用上了。"}},
	)
	knowledge := &recordingTool{name: KnowledgeSearchName, observations: []Observation{{Text: "资料", Found: true}}}
	memory := &recordingTool{name: MemorySearchName, observations: []Observation{{Text: "记忆", Found: true}}}
	harness := newHarness(t, chat, Budget{MaxModelCalls: 4, MaxToolCalls: 3, MaxSteps: 10}, knowledge, memory)

	var client recorder
	answer, err := harness.Stream(context.Background(), baseRequest(), client.emit)
	if err != nil {
		t.Fatal(err)
	}
	// Streaming did not shorten the loop: two tool rounds still ran and the third call
	// answered. A "stream once, then force a final" shortcut would fail right here.
	if len(knowledge.calls) != 1 || len(memory.calls) != 1 {
		t.Fatalf("knowledge=%d memory=%d, want each once across two rounds", len(knowledge.calls), len(memory.calls))
	}
	if chat.calls != 3 {
		t.Fatalf("the model ran %d times, want three rounds", chat.calls)
	}
	if client.answer() != answer.Content || answer.Content != "两轮都用上了。" {
		t.Fatalf("answer=%q published=%v", answer.Content, client.deltas)
	}
}

// TestSuppressionDoesNotSurviveIntoTheNextRound is the second regression the old publisher
// had: a round that asked for a tool silenced the call, and a sticky flag then kept the
// following answer from ever being published. Each call starts from scratch.
func TestSuppressionDoesNotSurviveIntoTheNextRound(t *testing.T) {
	chat := newGatedModel(
		gatedRound{deltas: []string{"查一下。"}, toolNames: []string{KnowledgeSearchName}},
		gatedRound{deltas: []string{"一句", "一句", "又一句。"}},
	)
	knowledge := &recordingTool{name: KnowledgeSearchName, observations: []Observation{{Text: "资料", Found: true}}}
	harness := newHarness(t, chat, Budget{}, knowledge)

	var client recorder
	answer, err := harness.Stream(context.Background(), baseRequest(), client.emit)
	if err != nil {
		t.Fatal(err)
	}
	if client.answer() != "一句一句又一句。" || client.answer() != answer.Content {
		t.Fatalf("after a tool round the answer was published as %q, answer=%q", client.answer(), answer.Content)
	}
	if len(client.deltas) != 3 {
		t.Fatalf("the final round's chunks were merged into %d frames: %v", len(client.deltas), client.deltas)
	}
}

// TestCancellationInsideAHeldRoundStopsBeforeTheTool is what buffering makes possible to lose:
// no write reached the client, so a hang-up cannot be noticed by a failed write. The loop must
// still refuse the tool it was about to execute and the model call after it.
func TestCancellationInsideAHeldRoundStopsBeforeTheTool(t *testing.T) {
	chat := newGatedModel(gatedRound{
		deltas: []string{"我先查一下", "再回答。"}, toolAfterDeltas: 2,
		toolNames: []string{KnowledgeSearchName}, gateAfter: 2,
	})
	knowledge := &recordingTool{name: KnowledgeSearchName, observations: []Observation{{Text: "资料", Found: true}}}
	harness := newHarness(t, chat, Budget{}, knowledge)

	ctx, cancel := context.WithCancel(context.Background())
	errs := make(chan error, 1)
	go func() {
		_, err := harness.Stream(ctx, baseRequest(), func(string) error { return ctx.Err() })
		errs <- err
	}()
	<-chat.entered
	cancel()
	if err := <-errs; !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want the caller's cancellation", err)
	}
	if chat.calls != 1 {
		t.Fatalf("the model was asked %d times after the client hung up, want this round only", chat.calls)
	}
	if len(knowledge.calls) != 0 {
		t.Fatalf("a cancelled turn still ran a tool %d time(s)", len(knowledge.calls))
	}
}

func TestStreamCancellationStopsEverythingWithoutFurtherSpend(t *testing.T) {
	// The client hangs up while the provider is still writing the answer, and the next round
	// of the script would have asked for a tool. Neither the tool nor a second model call may
	// happen: the write that cannot be delivered is what stops the loop.
	chat := newGatedModel(
		gatedRound{deltas: []string{"半句", "答案", "这一段没人会读到"}, gateAfter: 2},
		gatedRound{deltas: []string{"不该走到这里"}, toolNames: []string{KnowledgeSearchName}},
	)
	knowledge := &recordingTool{name: KnowledgeSearchName, observations: []Observation{{Text: "资料", Found: true}}}
	harness := newHarness(t, chat, Budget{}, knowledge)

	ctx, cancel := context.WithCancel(context.Background())
	errs := make(chan error, 1)
	go func() {
		// The delta handler checks the context the way the service's does: a client that
		// has gone away is discovered by the write, not by a separate stop signal.
		_, err := harness.Stream(ctx, baseRequest(), func(string) error { return ctx.Err() })
		errs <- err
	}()
	<-chat.entered
	cancel()
	chat.release <- struct{}{}
	if err := <-errs; !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want the caller's cancellation", err)
	}
	if chat.calls != 1 {
		t.Fatalf("the model was asked %d times after the client hung up", chat.calls)
	}
	if len(knowledge.calls) != 0 {
		t.Fatalf("a cancelled turn still ran a tool %d time(s)", len(knowledge.calls))
	}
}

// TestABrokenStreamPublishesNoPartialAnswer keeps the plain path's buffer honest in the failure
// direction: a provider whose stream dies mid-answer leaves its fragments unpublished, because
// the turn is going to end in a failure frame and half a sentence is not an answer.
func TestABrokenStreamPublishesNoPartialAnswer(t *testing.T) {
	chat := newGatedModel(gatedRound{deltas: []string{"写了一半", "后面没了"}, errAfterDeltas: 1})
	knowledge := &recordingTool{name: KnowledgeSearchName, observations: []Observation{{Text: "资料", Found: true}}}
	harness := newHarness(t, chat, Budget{}, knowledge)

	var client recorder
	_, err := harness.Stream(context.Background(), baseRequest(), client.emit)
	if !errors.Is(err, model.ErrUnavailable) {
		t.Fatalf("error = %v, want the provider failure", err)
	}
	if len(client.deltas) != 0 {
		t.Fatalf("a failed turn published %q", client.answer())
	}
	if len(knowledge.calls) != 0 {
		t.Fatalf("a failed turn still ran a tool %d time(s)", len(knowledge.calls))
	}
}

func TestStreamWithoutAStreamingModelStillAnswers(t *testing.T) {
	// The honest fallback on the plain path: a model with no streamed tool path answers in one
	// piece after the loop, re-chunked, and nothing pretends otherwise.
	chat := &scriptedToolModel{replies: []model.Completion{answerReply("整体回答。")}}
	harness := newHarness(t, chat, Budget{}, &recordingTool{name: KnowledgeSearchName})
	var client recorder
	answer, err := harness.Stream(context.Background(), baseRequest(), client.emit)
	if err != nil {
		t.Fatal(err)
	}
	if answer.Content != "整体回答。" || client.answer() != answer.Content {
		t.Fatalf("answer=%q published=%v", answer.Content, client.deltas)
	}
	if answer.FinalBlockID != "" {
		t.Fatalf("a turn that published no blocks named block %q", answer.FinalBlockID)
	}
}

func TestStreamedHarnessKeepsStepFramesAndAnswerTogether(t *testing.T) {
	chat := newGatedModel(
		gatedRound{deltas: []string{"查一下。"}, toolNames: []string{KnowledgeSearchName}},
		gatedRound{deltas: []string{"答案来了。"}},
	)
	knowledge := &recordingTool{name: KnowledgeSearchName,
		observations: []Observation{{Text: "资料", Found: true, Summary: "检索到 1 条资料"}}}
	harness := newHarness(t, chat, Budget{}, knowledge)

	var client recorder
	answer, err := harness.StreamSteps(context.Background(), baseRequest(), client.emit, client.step)
	if err != nil {
		t.Fatal(err)
	}
	if client.fragmentsOf(answer.FinalBlockID) != answer.Content {
		t.Fatalf("answer %q is not the published text of %q: %q", answer.Content,
			answer.FinalBlockID, client.fragmentsOf(answer.FinalBlockID))
	}
	want := "step_started step_started tool_started tool_result step_started"
	if got := stepOnlyPhases(client.steps); got != want {
		t.Fatalf("step frames = %q, want %q", got, want)
	}
}

// stepOnlyPhases reports the step protocol's own frames, with the text-block frames left out,
// so a change to the frames beside them cannot quietly reorder the ones a client folds on.
func stepOnlyPhases(steps []responder.Step) string {
	var names []string
	for _, step := range steps {
		switch step.Phase {
		case responder.TextStarted, responder.TextDelta, responder.TextEnded:
			continue
		}
		names = append(names, string(step.Phase))
	}
	return strings.Join(names, " ")
}
