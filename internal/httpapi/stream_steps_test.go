package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/responder"
)

// These are the bytes a browser sees. The service tests prove the frames are produced in
// the right order; what is proved here is that the writer puts each one on its own
// `event:` + `data:` pair, that a step frame carries only what a step may carry, and that
// a chat turn's stream is the same sequence of bytes it was before any of this existed.

func agentStepFrames() []responder.Step {
	return []responder.Step{
		{Phase: responder.StepStarted, Sequence: 1, StepType: responder.StepTypePlan, Message: "model generation 1"},
		{Phase: responder.StepStarted, Sequence: 2, StepType: responder.StepTypeTool, Message: "tool call 1"},
		{Phase: responder.ToolStarted, Sequence: 2, ToolName: "memory_search"},
		{Phase: responder.ToolResult, Sequence: 2, ToolName: "memory_search", Content: "读到 2 条记忆", DurationMillis: 7},
		// The confirmation rides on the same sequence as the call that produced it, which is
		// what the loop does: the card belongs to that step.
		{
			Phase:    responder.WaitingConfirmation,
			Sequence: 2,
			Message:  "本回合生成了 1 条待确认记忆草稿，请在当前回复下方的确认卡片中处理。",
			Draft: &responder.MemoryDraft{
				DraftID: "mem-draft-1", Scope: domain.MemoryScopeTopic,
				TopicKey: "go-concurrency", Content: "用户在学 Go 的内存模型",
			},
		},
		{Phase: responder.StepStarted, Sequence: 3, StepType: responder.StepTypePlan, Message: "model generation 2"},
	}
}

func streamBody(t *testing.T, server *Server, token, sessionID, body string) *flushRecorder {
	t.Helper()
	recorder := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/"+sessionID+"/stream", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	server.ServeHTTP(recorder, request)
	return recorder
}

// frames splits a stream body into its "event: name" + data pairs, in arrival order.
func frames(body string) [][2]string {
	parts := strings.Split(strings.TrimSuffix(body, "\n\n"), "\n\n")
	out := make([][2]string, 0, len(parts))
	for _, part := range parts {
		lines := strings.Split(part, "\n")
		if len(lines) != 2 || !strings.HasPrefix(lines[0], "event: ") || !strings.HasPrefix(lines[1], "data: ") {
			panic("malformed SSE frame: " + part)
		}
		out = append(out, [2]string{strings.TrimPrefix(lines[0], "event: "), strings.TrimPrefix(lines[1], "data: ")})
	}
	return out
}

func TestAgentStepStreamPutsProgressOnTheWire(t *testing.T) {
	assistant := apiResponder{steps: agentStepFrames()}
	server, userID, _, _ := newAPIFixture(t, assistant)
	token := issueTestToken(t, server, userID)
	sessionID := createSession(t, server, token, domain.SessionModeLearningAgent)

	recorder := streamBody(t, server, token, sessionID, `{"content":"question","stream_steps":true}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	seen := frames(body)
	names := make([]string, 0, len(seen))
	for _, frame := range seen {
		names = append(names, frame[0])
	}
	want := []string{
		"message.started",
		"step_started", "step_started", "tool_started", "tool_result", "waiting_confirmation", "step_started",
		"message.delta", "message.delta", "message.completed",
	}
	if strings.Join(names, " ") != strings.Join(want, " ") {
		t.Fatalf("frame order = %v, want %v\nbody:\n%s", names, want, body)
	}
	// Every frame is flushed as it is written, which is the only reason a viewer sees a
	// tool finish while the turn is still running.
	if recorder.flushes != len(seen) {
		t.Fatalf("%d frames but %d flushes", len(seen), recorder.flushes)
	}
	// One run id, on every frame, and nothing else that could identify a row or a person.
	var runID string
	for index, frame := range seen {
		if !strings.Contains(frame[1], `"run_id":"run-`) {
			t.Fatalf("frame %d (%s) carries no run id: %s", index+1, frame[0], frame[1])
		}
		current := frame[1][strings.Index(frame[1], `"run_id":"`)+len(`"run_id":"`):]
		current = current[:strings.Index(current, `"`)]
		if runID == "" {
			runID = current
		} else if current != runID {
			t.Fatalf("frame %d switched run id from %q to %q", index+1, runID, current)
		}
	}
	for _, frame := range seen {
		switch frame[0] {
		case "tool_started":
			if !strings.Contains(frame[1], `"tool_name":"memory_search"`) || !strings.Contains(frame[1], `"sequence":2`) {
				t.Fatalf("tool_started frame = %s", frame[1])
			}
			if strings.Contains(frame[1], `"step_type"`) {
				t.Fatalf("the source protocol names the step kind only on step_started: %s", frame[1])
			}
		case "tool_result":
			if !strings.Contains(frame[1], `"content":"读到 2 条记忆"`) || !strings.Contains(frame[1], `"duration_millis":7`) {
				t.Fatalf("tool_result frame = %s", frame[1])
			}
		case "step_started":
			if !strings.Contains(frame[1], `"step_type":"plan"`) && !strings.Contains(frame[1], `"step_type":"tool"`) {
				t.Fatalf("step_started frame without a kind: %s", frame[1])
			}
		}
	}
	// The answer's own frames are the ones the contract already had.
	completed := seen[len(seen)-1]
	if !strings.Contains(completed[1], `"content":"safe answer"`) || !strings.Contains(completed[1], `"message_id":`) {
		t.Fatalf("completed frame = %s", completed[1])
	}
	// Nothing a step says may be a stored row, a person, or a tool's own material. The
	// draft card is the one place an id appears, and the fields that make a draft
	// checkable - who owns it, where it came from, how certain it claims to be, when it was
	// written - are still not on the wire.
	for _, forbidden := range []string{"memory_id", "source_id", "session_id", "user_id", "doc_id",
		"collection_id", "document_id", "snippet", `"query"`, "secret-marker",
		"source_message_ids", "confidence", "created_at", "updated_at", "superseded_at", `"status"`} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("the step stream leaked %q:\n%s", forbidden, body)
		}
	}
}

// TestWaitingConfirmationFrameCarriesTheCardAndNothingElse pins the object a browser
// receives, key by key: the frame keeps the four step keys it already had and adds
// `memory_draft`, and the card itself is exactly the four fields a decision needs. An
// unread frame is not a decision, so this is the shape the card is built from.
func TestWaitingConfirmationFrameCarriesTheCardAndNothingElse(t *testing.T) {
	assistant := apiResponder{steps: agentStepFrames()}
	server, userID, _, _ := newAPIFixture(t, assistant)
	token := issueTestToken(t, server, userID)
	sessionID := createSession(t, server, token, domain.SessionModeLearningAgent)

	recorder := streamBody(t, server, token, sessionID, `{"content":"question","stream_steps":true}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	// One frame may carry a card. A step_started that quietly grew one would be a second
	// place for the same decision to appear.
	if got := strings.Count(body, `"memory_draft"`); got != 1 {
		t.Fatalf("%d frames carry a memory_draft, want exactly 1:\n%s", got, body)
	}
	var seen string
	for _, frame := range frames(body) {
		if frame[0] != "waiting_confirmation" {
			continue
		}
		seen = frame[1]
	}
	if seen == "" {
		t.Fatalf("no waiting_confirmation frame in:\n%s", body)
	}
	envelope := jsonKeys(t, seen)
	if got := strings.Join(envelope.keys, " "); got != "memory_draft message run_id sequence" {
		t.Fatalf("frame keys = %s, want the step keys plus one card", got)
	}
	card := jsonKeys(t, string(envelope.values["memory_draft"]))
	if got := strings.Join(card.keys, " "); got != "content draft_id scope topic_key" {
		t.Fatalf("card keys = %s, want exactly what a decision needs: %s", got, card.raw)
	}
	if got := string(card.values["draft_id"]); got != `"mem-draft-1"` {
		t.Fatalf("draft_id = %s", got)
	}
	if got := string(card.values["scope"]); got != `"topic"` {
		t.Fatalf("scope = %s", got)
	}
	if got := string(card.values["topic_key"]); got != `"go-concurrency"` {
		t.Fatalf("topic_key = %s", got)
	}
	if got := string(card.values["content"]); got != `"用户在学 Go 的内存模型"` {
		t.Fatalf("content = %s", got)
	}
}

// TestDraftCardOmitsATopicKeyTheScopeDoesNotUse: a global draft has no subject, and an
// empty string on the wire would render one anyway - "主题记忆：" followed by nothing.
func TestDraftCardOmitsATopicKeyTheScopeDoesNotUse(t *testing.T) {
	assistant := apiResponder{steps: []responder.Step{{
		Phase:    responder.WaitingConfirmation,
		Sequence: 1,
		Message:  "本回合生成了 1 条待确认记忆草稿，请在当前回复下方的确认卡片中处理。",
		Draft: &responder.MemoryDraft{
			DraftID: "mem-draft-2", Scope: domain.MemoryScopeGlobal, Content: "用户偏好先看代码",
		},
	}}}
	server, userID, _, _ := newAPIFixture(t, assistant)
	token := issueTestToken(t, server, userID)
	sessionID := createSession(t, server, token, domain.SessionModeLearningAgent)

	recorder := streamBody(t, server, token, sessionID, `{"content":"question","stream_steps":true}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	var seen string
	for _, frame := range frames(recorder.Body.String()) {
		if frame[0] == "waiting_confirmation" {
			seen = frame[1]
		}
	}
	if seen == "" {
		t.Fatalf("no waiting_confirmation frame in:\n%s", recorder.Body.String())
	}
	if strings.Contains(seen, "topic_key") {
		t.Fatalf("a global draft was given a subject: %s", seen)
	}
	card := jsonKeys(t, string(jsonKeys(t, seen).values["memory_draft"]))
	if got := strings.Join(card.keys, " "); got != "content draft_id scope" {
		t.Fatalf("card keys = %s, want %s: %s", got, "content draft_id scope", card.raw)
	}
}

// keyset is a JSON object's keys in the order the encoder wrote them, with their raw
// values, so a test can name the whole surface rather than spot-check strings inside it.
type keyset struct {
	keys   []string
	values map[string]json.RawMessage
	raw    string
}

func jsonKeys(t *testing.T, encoded string) keyset {
	t.Helper()
	var ordered []string
	decoder := json.NewDecoder(strings.NewReader(encoded))
	if tok, err := decoder.Token(); err != nil || tok != json.Delim('{') {
		t.Fatalf("not a JSON object: %s", encoded)
	}
	set := keyset{values: map[string]json.RawMessage{}, raw: encoded}
	for decoder.More() {
		name, err := decoder.Token()
		if err != nil {
			t.Fatalf("reading a key from %s: %v", encoded, err)
		}
		key, ok := name.(string)
		if !ok {
			t.Fatalf("non-string key in %s", encoded)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			t.Fatalf("reading %s from %s: %v", key, encoded, err)
		}
		ordered = append(ordered, key)
		set.values[key] = value
	}
	set.keys = ordered
	return set
}

func TestStreamStepsRefusedOnAChatTurnBeforeAnySSE(t *testing.T) {
	assistant := apiResponder{steps: agentStepFrames()}
	server, userID, _, sessionID := newAPIFixture(t, assistant)
	token := issueTestToken(t, server, userID)

	recorder := streamBody(t, server, token, sessionID, `{"content":"question","stream_steps":true}`)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), `"code":"invalid_request"`) {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	// A refusal is a plain JSON answer: it must not have committed to a stream, and it
	// must not have reached the model at all.
	if strings.HasPrefix(recorder.Header().Get("Content-Type"), "text/event-stream") ||
		strings.Contains(recorder.Body.String(), "event: ") {
		t.Fatalf("refused step stream committed an SSE response: headers=%v body=%s",
			recorder.Header(), recorder.Body.String())
	}
	select {
	case request := <-assistant.streamRequest:
		t.Fatalf("a refused step stream still reached the responder: %#v", request)
	default:
	}
}

func TestChatAndKnowledgeStreamBytesDoNotMentionSteps(t *testing.T) {
	assistant := apiResponder{}
	server, userID, _, sessionID := newAPIFixture(t, assistant)
	token := issueTestToken(t, server, userID)

	// The same turn, asked for the way a client asks today: three frames, no run id, no
	// step keys, no new header. This is the byte-for-byte claim the golden file also pins.
	recorder := streamBody(t, server, token, sessionID, `{"content":"question"}`)
	body := recorder.Body.String()
	want := "event: message.started\ndata: {}\n\n" +
		"event: message.delta\ndata: {\"delta\":\"safe \"}\n\n" +
		"event: message.delta\ndata: {\"delta\":\"answer\"}\n\n" +
		"event: message.completed\ndata: {\"content\":\"safe answer\",\"message_id\":\""
	if !strings.HasPrefix(body, want) {
		t.Fatalf("chat stream =\n%s\nwant prefix\n%s", body, want)
	}
	for _, forbidden := range []string{"run_id", "step_started", "tool_started", "tool_result",
		"waiting_confirmation", "step_type", "tool_name", "duration_millis", "termination_reason", "sequence",
		"memory_draft", "draft_id"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("a plain chat stream mentioned %q:\n%s", forbidden, body)
		}
	}
}
