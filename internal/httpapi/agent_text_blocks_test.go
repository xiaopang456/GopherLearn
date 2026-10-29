package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/xiaopang456/GopherLearn/internal/agent"
	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/responder"
)

// The frames below are what a browser parses. The service tests prove the events are produced
// in the right order; what is proved here is the wire shape of the three text-block frames,
// that each carries only its own keys, and that the keys do not appear anywhere a turn that
// never published a block is written - which is the whole compatibility argument for adding
// them to one shared frame writer.

// blockResponder is the agent harness as the HTTP layer sees it: a strategy that reports
// progress and publishes its answer as text blocks rather than as message.delta. It is written
// out here rather than wired from internal/agent so these assertions are about bytes, and the
// service-level test with the real loop stays a separate claim.
type blockResponder struct {
	// narration is the tool round's own text; answer is the final round's text. Both arrive as
	// fragments, because that is the only shape a streamed turn has.
	narration []string
	answer    []string
	// draft is the row this turn stored, and the only reason it may offer a card.
	draft *responder.MemoryDraft
	// finalBlockID is the block the answer came out of, exactly as the loop names it.
	finalBlockID string
	// chunks counts calls to the legacy answer channel.
	chunks int
}

func (f blockResponder) Strategy() domain.AnswerStrategy { return domain.AnswerStrategyAgent }

func (f blockResponder) Respond(context.Context, responder.Request) (responder.Answer, error) {
	return responder.Answer{Content: strings.Join(f.answer, "")}, nil
}

// Stream is the turn with no progress frames: buffered answer text, message.delta only. It is
// kept distinct on purpose, so a test that asks for one shape cannot be served the other.
func (f blockResponder) Stream(_ context.Context, _ responder.Request, emit func(string) error) (responder.Answer, error) {
	for _, chunk := range f.answer {
		f.chunks++
		if err := emit(chunk); err != nil {
			return responder.Answer{}, err
		}
	}
	return responder.Answer{Content: strings.Join(f.answer, "")}, nil
}

func (f blockResponder) SupportsSteps(responder.Request) bool { return true }

func (f blockResponder) StreamSteps(_ context.Context, _ responder.Request, emit func(string) error,
	onStep responder.StepSink,
) (responder.Answer, error) {
	answer := strings.Join(f.answer, "")
	send := func(step responder.Step) error { return onStep(step) }
	if err := send(responder.Step{Phase: responder.StepStarted, Sequence: 1,
		StepType: responder.StepTypePlan, Message: "model generation 1"}); err != nil {
		return responder.Answer{}, err
	}
	if err := f.publishBlock(send, "blk-1", 1, f.narration, responder.TextKindProcess); err != nil {
		return responder.Answer{}, err
	}
	if err := send(responder.Step{Phase: responder.StepStarted, Sequence: 2,
		StepType: responder.StepTypeTool, Message: "tool call 1"}); err != nil {
		return responder.Answer{}, err
	}
	if err := send(responder.Step{Phase: responder.ToolStarted, Sequence: 2,
		ToolName: agent.KnowledgeSearchName}); err != nil {
		return responder.Answer{}, err
	}
	if err := send(responder.Step{Phase: responder.ToolResult, Sequence: 2,
		ToolName: agent.KnowledgeSearchName, Content: "检索到 1 条资料", DurationMillis: 12}); err != nil {
		return responder.Answer{}, err
	}
	if f.draft != nil {
		// The card rides on the step that produced it, with the same sequence, and nothing else
		// about this turn changes because it is there.
		if err := send(responder.Step{Phase: responder.WaitingConfirmation, Sequence: 2,
			Message: "本回合生成了 1 条待确认记忆草稿，请在当前回复下方的确认卡片中处理。",
			Draft:   f.draft}); err != nil {
			return responder.Answer{}, err
		}
	}
	if err := send(responder.Step{Phase: responder.StepStarted, Sequence: 3,
		StepType: responder.StepTypePlan, Message: "model generation 2"}); err != nil {
		return responder.Answer{}, err
	}
	if err := f.publishBlock(send, "blk-2", 2, f.answer, responder.TextKindCandidate); err != nil {
		return responder.Answer{}, err
	}
	// One channel per turn: the answer left as blocks, so the delta handler stays untouched.
	return responder.Answer{Content: answer, FinalBlockID: f.finalBlockID}, nil
}

func (f blockResponder) publishBlock(send func(responder.Step) error, blockID string, round int,
	fragments []string, kind responder.TextKind,
) error {
	if len(fragments) == 0 {
		return nil
	}
	if err := send(responder.Step{Phase: responder.TextStarted, BlockID: blockID, Round: round}); err != nil {
		return err
	}
	for _, fragment := range fragments {
		if err := send(responder.Step{Phase: responder.TextDelta, BlockID: blockID, Content: fragment}); err != nil {
			return err
		}
	}
	return send(responder.Step{Phase: responder.TextEnded, BlockID: blockID, Kind: kind})
}

func blockTurn() blockResponder {
	return blockResponder{
		narration:    []string{"我先查一下你的", "RPC 笔记。"},
		answer:       []string{"TCP 是字节流，", "不会保留消息边界。"},
		finalBlockID: "blk-2",
	}
}

func streamAgentTurn(t *testing.T, assistant responder.Responder) (*flushRecorder, string) {
	t.Helper()
	server, userID, _, _ := newAPIFixture(t, assistant)
	token := issueTestToken(t, server, userID)
	sessionID := createSession(t, server, token, domain.SessionModeLearningAgent)
	recorder := streamBody(t, server, token, sessionID, `{"content":"question","stream_steps":true}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	return recorder, sessionID
}

// TestAgentTextBlockFramesCarryOnlyTheirOwnKeys pins the three new frames key by key. An extra
// key on a text frame is how an internal counter or a tool's own material reaches a page, so
// the sets below are closed lists rather than a spot check.
func TestAgentTextBlockFramesCarryOnlyTheirOwnKeys(t *testing.T) {
	recorder, _ := streamAgentTurn(t, blockTurn())
	seen := frames(recorder.Body.String())
	names := make([]string, 0, len(seen))
	for _, frame := range seen {
		names = append(names, frame[0])
	}
	want := []string{
		"message.started",
		"step_started",
		"agent.text.started", "agent.text.delta", "agent.text.delta", "agent.text.ended",
		"step_started", "tool_started", "tool_result",
		"step_started",
		"agent.text.started", "agent.text.delta", "agent.text.delta", "agent.text.ended",
		"message.completed",
	}
	if strings.Join(names, " ") != strings.Join(want, " ") {
		t.Fatalf("frame order = %v,\nwant %v\nbody:\n%s", names, want, recorder.Body.String())
	}
	// Every frame is flushed as it is written, which is the only reason text grows while the
	// provider is still producing it.
	if recorder.flushes != len(seen) {
		t.Fatalf("%d frames but %d flushes", len(seen), recorder.flushes)
	}
	for _, frame := range seen {
		keys := strings.Join(jsonKeys(t, frame[1]).keys, " ")
		switch frame[0] {
		case "agent.text.started":
			if keys != "block_id round run_id" {
				t.Fatalf("agent.text.started keys = %s, want the block's name, its round and the run: %s",
					keys, frame[1])
			}
		case "agent.text.delta":
			if keys != "block_id content run_id" {
				t.Fatalf("agent.text.delta keys = %s, want one fragment of one block: %s", keys, frame[1])
			}
		case "agent.text.ended":
			if keys != "block_id kind run_id" {
				t.Fatalf("agent.text.ended keys = %s, want the block and what it turned out to be: %s",
					keys, frame[1])
			}
		case "message.completed":
			if keys != "content final_block_id message_id run_id" {
				t.Fatalf("message.completed keys = %s, want the confirmed text and the block it came from: %s",
					keys, frame[1])
			}
		}
	}
	bodies := map[string]string{}
	for _, frame := range seen {
		if frame[0] == "agent.text.started" {
			if _, taken := bodies["started"]; !taken {
				bodies["started"] = frame[1]
			} else {
				bodies["second"] = frame[1]
			}
		}
	}
	if !strings.Contains(bodies["started"], `"round":1`) ||
		!strings.Contains(bodies["started"], `"block_id":"blk-1"`) {
		t.Fatalf("first block opened as %s, want blk-1 from round 1", bodies["started"])
	}
	if !strings.Contains(bodies["second"], `"round":2`) ||
		!strings.Contains(bodies["second"], `"block_id":"blk-2"`) {
		t.Fatalf("second block opened as %s, want blk-2 from round 2", bodies["second"])
	}
	completed := seen[len(seen)-1][1]
	if !strings.Contains(completed, `"final_block_id":"blk-2"`) {
		t.Fatalf("the completed frame does not name the answer block: %s", completed)
	}
	// The legacy channel stays silent on this path: the same characters would otherwise reach
	// the browser twice, once as a delta and once as a block.
	if strings.Contains(recorder.Body.String(), "event: message.delta") {
		t.Fatalf("a blocks turn also sent message.delta:\n%s", recorder.Body.String())
	}
}

// TestWaitingConfirmationCardIsUnchangedBesideTextBlocks keeps the 12A receipt intact after the
// block keys were added to the same writer: the card frame gains nothing, and the draft it
// addresses is still the only identifier on the wire.
func TestWaitingConfirmationCardIsUnchangedBesideTextBlocks(t *testing.T) {
	assistant := blockTurn()
	assistant.draft = &responder.MemoryDraft{
		DraftID: "mem-draft-7", Scope: domain.MemoryScopeTopic, TopicKey: "go-concurrency",
		Content: "用户在学 Go 的并发内存模型",
	}
	recorder, _ := streamAgentTurn(t, assistant)
	body := recorder.Body.String()
	var card string
	for _, frame := range frames(body) {
		if frame[0] == "waiting_confirmation" {
			card = frame[1]
		}
	}
	if card == "" {
		t.Fatalf("no waiting_confirmation frame in:\n%s", body)
	}
	if got := strings.Join(jsonKeys(t, card).keys, " "); got != "memory_draft message run_id sequence" {
		t.Fatalf("card frame keys = %s, want the four step keys it has always had: %s", got, card)
	}
	inner := jsonKeys(t, string(jsonKeys(t, card).values["memory_draft"]))
	if got := strings.Join(inner.keys, " "); got != "content draft_id scope topic_key" {
		t.Fatalf("card keys = %s: %s", got, inner.raw)
	}
	if got := strings.Count(body, `"memory_draft"`); got != 1 {
		t.Fatalf("%d frames carry a card, want exactly one", got)
	}
}

// TestTextBlockKeysStayOffPlainChatBytes is the compatibility claim for a turn that never asked
// for progress: the same server, the same writer, and none of the new keys - and none of the
// step keys either - appear in its body.
func TestTextBlockKeysStayOffPlainChatBytes(t *testing.T) {
	assistant := blockTurn()
	server, userID, _, sessionID := newAPIFixture(t, responder.Responder(assistant))
	token := issueTestToken(t, server, userID)
	recorder := streamBody(t, server, token, sessionID, `{"content":"question"}`)
	body := recorder.Body.String()
	if !strings.Contains(body, "event: message.delta") {
		t.Fatalf("a plain agent stream stopped using the answer channel it has always used:\n%s", body)
	}
	for _, forbidden := range []string{"block_id", "final_block_id", "agent.text", "round", "kind",
		"run_id", "step_started", "tool_name", "sequence"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("a plain agent stream mentioned %q:\n%s", forbidden, body)
		}
	}
}
