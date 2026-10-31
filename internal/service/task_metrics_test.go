package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/agent"
	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/model"
	"github.com/xiaopang456/GopherLearn/internal/modelcall"
	"github.com/xiaopang456/GopherLearn/internal/openai"
	"github.com/xiaopang456/GopherLearn/internal/repository"
	"github.com/xiaopang456/GopherLearn/internal/responder"
	"github.com/xiaopang456/GopherLearn/internal/summary"
)

// scriptedReply is one provider answer, described by what it says rather than by
// the wire shape the client happens to need.
type scriptedReply struct {
	content  string
	usage    string
	callID   string
	toolName string
	args     string
	// raw replaces the whole body, for a provider whose answer cannot be parsed at
	// all; status answers with an HTTP failure instead; stall keeps the handler
	// waiting until the request is cancelled, so a real client-side timeout is
	// produced by the transport, not asserted.
	raw    string
	status int
	delay  time.Duration
	// stall makes the handler block until the request context is done. It is
	// used instead of a fixed sleep: a fixed sleep of the same length as the
	// client timeout is a race, and on a loaded machine the response can win
	// it and the test fails for no reason.
	stall bool
}

type providerScript struct {
	t       *testing.T
	mu      sync.Mutex
	replies []scriptedReply
	served  int
	client  *openai.Client
}

func newProviderScript(t *testing.T, replies ...scriptedReply) *providerScript {
	t.Helper()
	return newProviderScriptWithTimeout(t, 2*time.Second, replies...)
}

// newProviderScriptWithTimeout is the same script with a client timeout the test
// controls, so a stalled provider can produce a real transport timeout instead of
// a manufactured error value.
func newProviderScriptWithTimeout(t *testing.T, timeout time.Duration, replies ...scriptedReply) *providerScript {
	t.Helper()
	script := &providerScript{t: t, replies: replies}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		wantsStream := strings.Contains(string(body), `"stream":true`)

		script.mu.Lock()
		index := script.served
		script.served++
		var reply scriptedReply
		if index < len(script.replies) {
			reply = script.replies[index]
		}
		script.mu.Unlock()
		if index >= len(script.replies) {
			t.Errorf("provider saw call %d but only %d replies were scripted", index+1, len(script.replies))
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"code":"script_exhausted"}}`))
			return
		}
		if reply.stall {
			// Wait for the client to give up. The request context is
			// cancelled when the client closes the connection, so this
			// returns only after the timeout has fired. A fixed sleep of
			// the same length as the timeout would be a race.
			<-r.Context().Done()
			return
		}
		if reply.delay > 0 {
			time.Sleep(reply.delay)
		}
		if reply.status != 0 {
			w.WriteHeader(reply.status)
			_, _ = io.WriteString(w, `{"error":{"code":"upstream","type":"server_error"}}`)
			return
		}
		if wantsStream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, reply.streamEvent())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, reply.completionBody())
	}))
	t.Cleanup(server.Close)
	client, err := openai.New(openai.Config{
		BaseURL: server.URL, APIKey: "sk-unit-test-key-0123456789", Model: "script-model", Timeout: timeout,
	})
	if err != nil {
		t.Fatal(err)
	}
	script.client = client
	return script
}

// scriptStatus, scriptRaw and scriptDelayed name the three faults a provider can
// show that are not about the answer's text.
func scriptStatus(status int) scriptedReply { return scriptedReply{status: status} }

func scriptRaw(body string) scriptedReply { return scriptedReply{raw: body} }

// scriptDelayed stalls the provider until the client gives up.
//
// It used to sleep for the length of the client timeout, which is a race:
// on a loaded machine the response could arrive just before the timeout
// fired, and the test failed although nothing was broken. Blocking until the
// request context is done makes the timeout the only possible outcome.
func scriptDelayed(timeout time.Duration) scriptedReply {
	return scriptedReply{content: "太迟了。", stall: true}
}

// completionBody is the non-streaming shape of one reply.
func (r scriptedReply) completionBody() string {
	if r.raw != "" {
		return r.raw
	}
	if r.toolName != "" {
		body := `{"model":"script-model","choices":[{"message":{"role":"assistant","content":"",` +
			`"tool_calls":[{"id":` + jsonString(r.callID) + `,"type":"function","function":{"name":` +
			jsonString(r.toolName) + `,"arguments":` + jsonString(r.args) + `}}]},"finish_reason":"tool_calls"}]}`
		return withUsage(body, r.usage)
	}
	body := `{"model":"script-model","choices":[{"message":{"role":"assistant","content":` +
		jsonString(r.content) + `},"finish_reason":"stop"}]}`
	return withUsage(body, r.usage)
}

// streamEvent is the same reply as one server-sent event plus its completion
// marker. A single chunk is enough here: the client's rules about truncation and
// finish reasons are covered in internal/openai.
func (r scriptedReply) streamEvent() string {
	if r.raw != "" {
		return "data: " + r.raw + "\n\ndata: [DONE]\n\n"
	}
	delta := `{"content":` + jsonString(r.content) + `}`
	finish := "stop"
	if r.toolName != "" {
		delta = `{"content":"","tool_calls":[{"index":0,"id":` + jsonString(r.callID) +
			`,"type":"function","function":{"name":` + jsonString(r.toolName) +
			`,"arguments":` + jsonString(r.args) + `}}]}`
		finish = "tool_calls"
	}
	chunk := withUsage(`{"model":"script-model","choices":[{"index":0,"delta":`+delta+
		`,"finish_reason":"`+finish+`"}]}`, r.usage)
	return "data: " + chunk + "\n\ndata: [DONE]\n\n"
}

func withUsage(body, usage string) string {
	if usage == "" {
		return body
	}
	return strings.TrimSuffix(body, "}") + `,"usage":` + usage + "}"
}

// scriptCompletion and scriptToolCall name the two replies a scenario needs.
func scriptCompletion(content, usage string) scriptedReply {
	return scriptedReply{content: content, usage: usage}
}

func scriptToolCall(callID, toolName, arguments, usage string) scriptedReply {
	return scriptedReply{callID: callID, toolName: toolName, args: arguments, usage: usage}
}

// dispatches counts the HTTP requests the provider actually received: the
// independent witness on what the record claims about calls.
func (p *providerScript) dispatches() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.served
}

func jsonString(value string) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

func usageBody(prompt, completion, total int) string {
	return fmt.Sprintf(`{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}`, prompt, completion, total)
}

// sinkRecorder is the in-memory stand-in for the JSON log.
type sinkRecorder struct {
	mu      sync.Mutex
	records []modelcall.TaskRecord
	fail    error
	panicOn bool
}

func (s *sinkRecorder) write(record modelcall.TaskRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.panicOn {
		panic("the log sink exploded")
	}
	if s.fail != nil {
		return s.fail
	}
	s.records = append(s.records, record)
	return nil
}

func (s *sinkRecorder) all() []modelcall.TaskRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]modelcall.TaskRecord, len(s.records))
	copy(out, s.records)
	return out
}

func (s *sinkRecorder) one(t *testing.T) modelcall.TaskRecord {
	t.Helper()
	records := s.all()
	if len(records) != 1 {
		t.Fatalf("records = %d, want exactly one per operation: %+v", len(records), records)
	}
	return records[0]
}

// probeTool is the server-owned capability an agent turn can ask for, so a test
// can produce a genuine two-call agent turn instead of pretending.
type probeTool struct {
	mu   sync.Mutex
	runs int
}

func (*probeTool) Name() string { return "probe_search" }

func (*probeTool) Description() string { return "server-owned probe used by measurement tests" }

func (*probeTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}

func (p *probeTool) Execute(_ context.Context, principal agent.Principal, _ json.RawMessage) (agent.Observation, error) {
	if strings.TrimSpace(principal.UserID) == "" {
		return agent.Observation{}, errors.New("a tool may not run without a principal")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.runs++
	return agent.Observation{Text: "探针取回两段可核对的材料。", Found: true, Summary: "2 条"}, nil
}

func (p *probeTool) executions() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.runs
}

func newAgentResponder(t *testing.T, chat model.ChatModel) (responder.Responder, *probeTool) {
	t.Helper()
	tool := &probeTool{}
	registry, err := agent.NewRegistry(tool)
	if err != nil {
		t.Fatal(err)
	}
	toolModel, ok := chat.(model.ToolChatModel)
	if !ok {
		t.Fatal("the scripted provider cannot carry tool calls")
	}
	harness, err := agent.New(toolModel, registry, agent.Budget{MaxModelCalls: 4, MaxToolCalls: 3, MaxSteps: 10}.Normalize())
	if err != nil {
		t.Fatal(err)
	}
	direct, err := responder.NewModelStrategy(domain.AnswerStrategyDirectChat, chat, "回答学生。")
	if err != nil {
		t.Fatal(err)
	}
	assistant, err := responder.NewStrategyResponder(direct, harness)
	if err != nil {
		t.Fatal(err)
	}
	return assistant, tool
}

// ctxCapturingResponder joins whatever task the caller is inside, which is how a
// nested helper is shown to reach the enclosing task instead of starting its own.
type ctxCapturingResponder struct {
	answer    string
	innerSeen chan *modelcall.TaskHandle
}

func (r *ctxCapturingResponder) Strategy(domain.SessionMode, domain.KnowledgeOptions) domain.AnswerStrategy {
	return domain.AnswerStrategyDirectChat
}

func (r *ctxCapturingResponder) Respond(ctx context.Context, _ responder.Request) (responder.Answer, error) {
	handle := modelcall.CurrentTask(ctx)
	r.innerSeen <- handle
	// A deeper helper writes through the handle it was handed. If that handle were
	// a different task, this note would land in another record and the line under
	// test here would stay null.
	handle.NoteFirstText("nested-block")
	return responder.Answer{Content: r.answer}, nil
}

func (r *ctxCapturingResponder) Stream(ctx context.Context, _ responder.Request, emit func(string) error) (responder.Answer, error) {
	return r.Respond(ctx, responder.Request{})
}

// metricsFixture wires one MessageService with task metering on or off.
type metricsFixture struct {
	service   *MessageService
	store     *repository.InMemoryRepository
	userID    string
	sessionID string
	agentID   string
	sink      *sinkRecorder
	publisher *modelcall.Publisher
	// framesSink collects the frame names a streamed turn wrote out, so one
	// fixture can be compared against itself with the switch flipped.
	framesSink []string
}

func newMetricsFixture(t *testing.T, assistant responder.Responder, enabled bool, options ...MessageServiceOption) *metricsFixture {
	t.Helper()
	ctx := t.Context()
	store := repository.NewInMemoryRepository()
	user, err := store.CreateUser(ctx, &domain.User{Email: "metered@example.com", Username: "metered"})
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.CreateSession(ctx, &domain.Session{UserID: user.ID, Title: "chat", Mode: domain.SessionModeChat})
	if err != nil {
		t.Fatal(err)
	}
	agentSession, err := store.CreateSession(ctx, &domain.Session{UserID: user.ID, Title: "agent", Mode: domain.SessionModeLearningAgent})
	if err != nil {
		t.Fatal(err)
	}
	sink := &sinkRecorder{}
	publisher := modelcall.NewPublisher(enabled, sink.write)
	options = append(options, WithCollectionAccess(openAccess{}), WithTaskMetrics(publisher))
	service := NewMessageService(store, assistant, options...)
	return &metricsFixture{
		service: service, store: store, userID: user.ID, sessionID: session.ID, agentID: agentSession.ID,
		sink: sink, publisher: publisher,
	}
}

// storeTurn seeds one answered exchange into one conversation. The session has to
// be named: a summary is a per-conversation recollection, so seeding the chat and
// then measuring an agent turn would leave the agent turn below threshold and stop
// testing the summary call at all.
func (f *metricsFixture) storeTurn(t *testing.T, sessionID, question, answer string) {
	t.Helper()
	ctx := t.Context()
	if _, err := f.store.AppendMessage(ctx, f.userID, sessionID, &domain.Message{Role: "user", Content: question}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.AppendMessage(ctx, f.userID, sessionID, &domain.Message{Role: "assistant", Content: answer}); err != nil {
		t.Fatal(err)
	}
}

// summaryPolicy is a threshold a short test conversation can actually cross. It is
// written already-consistent (keep below trigger) because Policy.Normalize runs again
// inside summary.Build: a keep that was raised to the default on the first pass would
// leave nothing to compress, and the test would silently stop exercising the summary
// call at all.
func summaryPolicy() summary.Policy {
	return summary.Policy{
		TriggerMessages: 4, KeepRecentMessages: 2, MaxInputMessages: 60, MaxInputRunes: 12000, MaxOutputTokens: 200,
	}.Normalize()
}

// quizMetricsFixture is newLearningFixture with a real provider client and task
// metering instead of the scripted model.
type quizMetricsFixture struct {
	service   *LearningService
	sink      *sinkRecorder
	userID    string
	agentChat string
}

func newQuizMetricsFixture(t *testing.T, chat model.ChatModel) *quizMetricsFixture {
	t.Helper()
	ctx := t.Context()
	store := repository.NewInMemoryRepository()
	user, err := store.CreateUser(ctx, &domain.User{Email: "learner@example.com", Username: "learner"})
	if err != nil {
		t.Fatal(err)
	}
	agentChat, err := store.CreateSession(ctx, &domain.Session{UserID: user.ID, Title: "A", Mode: domain.SessionModeLearningAgent})
	if err != nil {
		t.Fatal(err)
	}
	sink := &sinkRecorder{}
	service := NewLearningService(store, chat,
		&learningRecall{results: learningChunks(8)},
		&learningCatalog{titles: map[string]string{"doc-a": "Redis 笔记", "doc-b": "Go 笔记"}, collections: []string{"col-1"}},
		&learningCatalog{titles: map[string]string{"doc-a": "Redis 笔记", "doc-b": "Go 笔记"}, collections: []string{"col-1"}},
	).WithTaskMetrics(modelcall.NewPublisher(true, sink.write))
	return &quizMetricsFixture{service: service, sink: sink, userID: user.ID, agentChat: agentChat.ID}
}

// TestTaskRecordOwnsSummaryAndTwoAgentCalls is the headline case: one user
// operation, three model calls, two purposes, one roll-up.
func TestTaskRecordOwnsSummaryAndTwoAgentCalls(t *testing.T) {
	// The summarizer now answers in the structured schema-v2 document the producer
	// validates, so the fixture's first completion is one.
	script := newProviderScript(t,
		scriptCompletion(`{"schema_version":2,"current_goal":"在 RDB 与 AOF 之间做生产选型",`+
			`"confirmed_decisions":[],"completed_items":[],"open_loops":[],"user_corrections":[],"key_errors":[],"references":[]}`,
			usageBody(100, 10, 110)),
		scriptToolCall("call-1", "probe_search", "{}", usageBody(200, 20, 220)),
		scriptCompletion("RDB 恢复快，AOF 丢得少。", usageBody(300, 30, 330)),
	)
	assistant, tool := newAgentResponder(t, script.client)
	summarizer, err := summary.NewModelSummarizer(script.client, summaryPolicy())
	if err != nil {
		t.Fatal(err)
	}
	fixture := newMetricsFixture(t, assistant, true, WithRollingSummary(summarizer, summaryPolicy()))
	for _, pair := range [][2]string{
		{"RDB 会不会丢数据？", "两次快照之间的那段会。"},
		{"那 AOF 呢？", "丢得少，但文件更大、重放更慢。"},
		{"生产上怎么选？", "通常两者同时开。"},
		{"为什么？", "用 AOF 保完整性，用 RDB 加速冷启动。"},
	} {
		fixture.storeTurn(t, fixture.agentID, pair[0], pair[1])
	}

	if _, err := fixture.service.Send(t.Context(), fixture.userID, fixture.agentID, "那生产呢？",
		domain.KnowledgeOptions{}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	record := fixture.sink.one(t)
	if record.Operation != taskOperationMessageSend {
		t.Fatalf("operation = %s, want message_send", record.Operation)
	}
	if record.Outcome != modelcall.OutcomeOK || record.FailureStage != "" {
		t.Fatalf("record = %+v, want a clean success with no failure stage", record)
	}
	if len(record.Calls) != 3 {
		t.Fatalf("calls = %d, want summary + decision round + answer round: %+v", len(record.Calls), record.Calls)
	}
	purposes := map[modelcall.Purpose]int{}
	for index, call := range record.Calls {
		purposes[call.Purpose]++
		if call.Seq != index+1 {
			t.Fatalf("call %d has seq %d, want the calls kept in issue order", index, call.Seq)
		}
	}
	if purposes[modelcall.PurposeSummary] != 1 || purposes[modelcall.PurposeDialogue] != 2 {
		t.Fatalf("purposes = %v, want one summary and two dialogue rounds", purposes)
	}
	// The summary is never folded into the answer's totals, and the answer's
	// totals are both rounds rather than the last one.
	summaryPart := record.Totals.ByPurpose[modelcall.PurposeSummary]
	dialoguePart := record.Totals.ByPurpose[modelcall.PurposeDialogue]
	if summaryPart.Calls != 1 || summaryPart.Prompt.Value != 100 {
		t.Fatalf("summary roll-up = %+v, want its own 100 prompt tokens", summaryPart)
	}
	if dialoguePart.Calls != 2 || dialoguePart.Prompt.Value != 500 || dialoguePart.Total.Value != 550 {
		t.Fatalf("dialogue roll-up = %+v, want 500 prompt and 550 total across both rounds", dialoguePart)
	}
	if record.Totals.Prompt.Value != 600 || record.Totals.Completion.Value != 60 || record.Totals.Total.Value != 660 {
		t.Fatalf("task totals = %+v, want 600/60/660 across all three calls", record.Totals)
	}
	if record.Totals.Status != modelcall.StatusFullUsage || record.Totals.Dispatches != 3 {
		t.Fatalf("status/dispatches = %s/%d, want full_usage and three dispatches", record.Totals.Status, record.Totals.Dispatches)
	}
	if script.dispatches() != 3 {
		t.Fatalf("provider saw %d requests, want 3", script.dispatches())
	}
	if tool.executions() != 1 {
		t.Fatalf("tool ran %d times, want the one execution this turn asked for", tool.executions())
	}
	if record.Cost != "unknown" {
		t.Fatalf("cost = %q, want unknown while no price source exists", record.Cost)
	}
	// A non-streaming operation has no frames to time.
	if record.FirstEventMillis != nil || record.FirstTextMillis != nil {
		t.Fatalf("a non-streaming turn recorded stream timings: %+v", record)
	}
}

// TestNonStreamingTurnRecordsNullStreamTimings keeps "did not happen" separate
// from "took no time".
func TestNonStreamingTurnRecordsNullStreamTimings(t *testing.T) {
	script := newProviderScript(t, scriptCompletion("答案。", usageBody(7, 3, 10)))
	direct, err := responder.NewModelStrategy(domain.AnswerStrategyDirectChat, script.client, "回答。")
	if err != nil {
		t.Fatal(err)
	}
	assistant, err := responder.NewStrategyResponder(direct)
	if err != nil {
		t.Fatal(err)
	}
	fixture := newMetricsFixture(t, assistant, true)

	if _, err := fixture.service.Send(t.Context(), fixture.userID, fixture.sessionID, "问题", domain.KnowledgeOptions{}); err != nil {
		t.Fatal(err)
	}
	record := fixture.sink.one(t)
	if record.FirstEventMillis != nil || record.FirstTextMillis != nil {
		t.Fatalf("a non-streaming turn recorded stream timings: %+v", record)
	}
	if record.AnswerStored == nil || !*record.AnswerStored {
		t.Fatal("the stored answer was not recorded")
	}
	if record.Delivered != nil {
		t.Fatal("a non-streaming turn cannot claim delivery to a client it never streamed")
	}
	if record.Totals.ByPurpose[modelcall.PurposeDialogue].Calls != 1 {
		t.Fatalf("purposes = %+v, want the one call labelled dialogue", record.Totals.ByPurpose)
	}
}

// TestQuizGenerationRetriesAreAllRecorded: a rejected set still cost a call.
func TestQuizGenerationRetriesAreAllRecorded(t *testing.T) {
	valid := learningQuestions([]int{1, 2}, []int{3}, []int{4, 5})
	script := newProviderScript(t,
		scriptCompletion("not json at all", usageBody(50, 5, 55)),
		scriptCompletion("still not json", usageBody(50, 5, 55)),
		scriptCompletion(valid, usageBody(60, 6, 66)),
	)
	fixture := newQuizMetricsFixture(t, script.client)

	if _, err := fixture.service.Start(t.Context(), fixture.userID, fixture.agentChat, "Redis 持久化", nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	record := fixture.sink.one(t)
	if record.Operation != taskOperationQuizStart {
		t.Fatalf("operation = %s, want quiz_start", record.Operation)
	}
	if len(record.Calls) != 3 {
		t.Fatalf("calls = %d, want all three attempts recorded: %+v", len(record.Calls), record.Calls)
	}
	for _, call := range record.Calls {
		if call.Purpose != modelcall.PurposeQuizGeneration {
			t.Fatalf("call %+v, want every attempt labelled quiz_generation", call)
		}
	}
	if record.Totals.Prompt.Value != 160 || record.Totals.Total.Value != 176 {
		t.Fatalf("totals = %+v, want the retries included: prompt 160, total 176", record.Totals)
	}
	if script.dispatches() != 3 {
		t.Fatalf("provider saw %d requests, want 3", script.dispatches())
	}
	if record.FirstEventMillis != nil || record.FirstTextMillis != nil {
		t.Fatalf("a quiz has no stream to time: %+v", record)
	}
}

// TestFailedGenerationKeepsTheCallsItMade: an operation that produced nothing
// still consumed what it spent.
func TestFailedGenerationKeepsTheCallsItMade(t *testing.T) {
	script := newProviderScript(t,
		scriptCompletion("not json", usageBody(50, 5, 55)),
		scriptCompletion("still not json", usageBody(50, 5, 55)),
		scriptCompletion("not json either", usageBody(60, 6, 66)),
	)
	fixture := newQuizMetricsFixture(t, script.client)

	_, err := fixture.service.Start(t.Context(), fixture.userID, fixture.agentChat, "Redis 持久化", nil)
	if !errors.Is(err, ErrLearningGeneration) {
		t.Fatalf("error = %v, want the generation refusal", err)
	}
	record := fixture.sink.one(t)
	if record.Outcome == modelcall.OutcomeOK {
		t.Fatalf("outcome = %s, want a failed task", record.Outcome)
	}
	if record.FailureStage != stageGeneration {
		t.Fatalf("stage = %q, want %q", record.FailureStage, stageGeneration)
	}
	if record.ReasonCode != reasonGenerationRejected {
		t.Fatalf("reason = %q, want %q", record.ReasonCode, reasonGenerationRejected)
	}
	if len(record.Calls) != 3 || script.dispatches() != 3 {
		t.Fatalf("calls = %d, provider saw %d, want 3 and 3 after the rejected retries",
			len(record.Calls), script.dispatches())
	}
	if record.Totals.Prompt.Value != 160 {
		t.Fatalf("prompt total = %d, want all three rejected attempts included", record.Totals.Prompt.Value)
	}
}

// TestSummaryFailureKeepsItsOwnCall: the summary call happened even though the
// turn died on it.
func TestSummaryFailureKeepsItsOwnCall(t *testing.T) {
	script := newProviderScript(t, scriptedReply{})
	direct, err := responder.NewModelStrategy(domain.AnswerStrategyDirectChat, script.client, "回答。")
	if err != nil {
		t.Fatal(err)
	}
	assistant, err := responder.NewStrategyResponder(direct)
	if err != nil {
		t.Fatal(err)
	}
	summarizer, err := summary.NewModelSummarizer(script.client, summaryPolicy())
	if err != nil {
		t.Fatal(err)
	}
	fixture := newMetricsFixture(t, assistant, true, WithRollingSummary(summarizer, summaryPolicy()))
	for index := range 4 {
		fixture.storeTurn(t, fixture.sessionID, fmt.Sprintf("旧问题 %d", index), fmt.Sprintf("旧答案 %d", index))
	}

	if _, err := fixture.service.Send(t.Context(), fixture.userID, fixture.sessionID, "新问题",
		domain.KnowledgeOptions{}); !errors.Is(err, ErrInternal) {
		t.Fatalf("error = %v, want the turn to end as it always did", err)
	}
	record := fixture.sink.one(t)
	if record.FailureStage != stageSummary {
		t.Fatalf("stage = %q, want the summary phase named", record.FailureStage)
	}
	if len(record.Calls) != 1 || record.Calls[0].Purpose != modelcall.PurposeSummary {
		t.Fatalf("calls = %+v, want the summary call kept even though the turn failed", record.Calls)
	}
	if record.Calls[0].Outcome == modelcall.OutcomeOK {
		t.Fatalf("summary call outcome = %s, want a failure", record.Calls[0].Outcome)
	}
	if !record.Calls[0].HTTPDispatched {
		t.Fatal("a call that got a response body back was recorded as undispatched")
	}
}

// TestAnswerStoredButNotDelivered splits the two facts.
func TestAnswerStoredButNotDelivered(t *testing.T) {
	script := newProviderScript(t, scriptCompletion("答案文本。", usageBody(4, 2, 6)))
	direct, err := responder.NewModelStrategy(domain.AnswerStrategyDirectChat, script.client, "回答。")
	if err != nil {
		t.Fatal(err)
	}
	assistant, err := responder.NewStrategyResponder(direct)
	if err != nil {
		t.Fatal(err)
	}
	fixture := newMetricsFixture(t, assistant, true)

	streamErr := fixture.service.Stream(t.Context(), fixture.userID, fixture.sessionID, "问题",
		domain.KnowledgeOptions{}, func(event StreamEvent) error {
			// The terminal frame is the one that cannot leave the process.
			if event.Name == "message.completed" {
				return errors.New("connection closed")
			}
			return nil
		})
	if streamErr == nil {
		t.Fatal("a failed terminal frame was reported as a delivered turn")
	}
	record := fixture.sink.one(t)
	if record.AnswerStored == nil || !*record.AnswerStored {
		t.Fatalf("answer_stored = %v, want true: the row was written before the frame failed", record.AnswerStored)
	}
	if record.Delivered == nil || *record.Delivered {
		t.Fatalf("delivered = %v, want false: the learner never got the confirmation", record.Delivered)
	}
	if record.FailureStage != stageDeliver {
		t.Fatalf("stage = %q, want the delivery phase", record.FailureStage)
	}
	if record.Outcome == modelcall.OutcomeOK {
		t.Fatal("a turn whose terminal frame failed must not be recorded as a success")
	}
	stored, err := fixture.store.ListMessages(t.Context(), fixture.userID, fixture.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 2 {
		t.Fatalf("stored messages = %d, want the question and the answer both kept", len(stored))
	}
}

// TestConsumerRefusedTheFirstFrame: the client was gone before anything was sent,
// so the honest record is a delivery failure with no provider call at all - not a
// call that reported zero tokens.
func TestConsumerRefusedTheFirstFrame(t *testing.T) {
	script := newProviderScript(t, scriptCompletion("完整答案。", usageBody(9, 4, 13)))
	direct, err := responder.NewModelStrategy(domain.AnswerStrategyDirectChat, script.client, "回答。")
	if err != nil {
		t.Fatal(err)
	}
	assistant, err := responder.NewStrategyResponder(direct)
	if err != nil {
		t.Fatal(err)
	}
	fixture := newMetricsFixture(t, assistant, true)

	streamErr := fixture.service.Stream(t.Context(), fixture.userID, fixture.sessionID, "问题",
		domain.KnowledgeOptions{}, func(StreamEvent) error { return errors.New("browser hung up") })
	if streamErr == nil {
		t.Fatal("a consumer that refused every frame succeeded anyway")
	}
	record := fixture.sink.one(t)
	if record.Outcome != modelcall.OutcomeCallbackFailed {
		t.Fatalf("outcome = %s, want callback_failed", record.Outcome)
	}
	if record.Delivered == nil || *record.Delivered {
		t.Fatalf("delivered = %v, want false", record.Delivered)
	}
	if record.FirstEventMillis != nil || record.FirstTextMillis != nil {
		t.Fatalf("nothing left the process, yet timings were recorded: %+v", record)
	}
	if len(record.Calls) != 0 || record.Totals.Status != modelcall.StatusNoCalls {
		t.Fatalf("calls = %+v status = %s, want no calls: the provider was never asked",
			record.Calls, record.Totals.Status)
	}
	if script.dispatches() != 0 {
		t.Fatalf("provider saw %d requests, want none", script.dispatches())
	}
	if record.DurationMillis < 0 {
		t.Fatal("a refused turn lost its duration")
	}
}

// TestCancelledTurnRecordsCancellation checks that walking away is not a fault in
// the model, and that a turn which never dispatched is recorded as no calls.
func TestCancelledTurnRecordsCancellation(t *testing.T) {
	script := newProviderScript(t)
	direct, err := responder.NewModelStrategy(domain.AnswerStrategyDirectChat, script.client, "回答。")
	if err != nil {
		t.Fatal(err)
	}
	assistant, err := responder.NewStrategyResponder(direct)
	if err != nil {
		t.Fatal(err)
	}
	fixture := newMetricsFixture(t, assistant, true)

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := fixture.service.Send(cancelled, fixture.userID, fixture.sessionID, "问题",
		domain.KnowledgeOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want the cancellation returned", err)
	}
	record := fixture.sink.one(t)
	if record.Outcome != modelcall.OutcomeCancelled {
		t.Fatalf("outcome = %s, want cancelled", record.Outcome)
	}
	if record.ReasonCode != reasonCancelled {
		t.Fatalf("reason = %q, want %q", record.ReasonCode, reasonCancelled)
	}
	if len(record.Calls) != 0 || record.Totals.Status != modelcall.StatusNoCalls {
		t.Fatalf("calls = %+v status = %s, want a pre-dispatch cancellation as no calls at all",
			record.Calls, record.Totals.Status)
	}
	if record.DurationMillis < 0 {
		t.Fatal("a cancelled operation lost its duration")
	}
	if script.dispatches() != 0 {
		t.Fatalf("provider saw %d requests, want none", script.dispatches())
	}
}

// TestFirstEventAndFirstTextDiffer, including an empty fragment that is not text.
func TestFirstEventAndFirstTextDiffer(t *testing.T) {
	assistant := &fixedResponder{answer: "最终答案", chunks: []string{"", "首", "段"}}
	fixture := newMetricsFixture(t, assistant, true)

	var names []string
	err := fixture.service.Stream(t.Context(), fixture.userID, fixture.sessionID, "问题",
		domain.KnowledgeOptions{}, func(event StreamEvent) error {
			names = append(names, event.Name)
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if len(names) < 4 || names[0] != "message.started" {
		t.Fatalf("frames = %v, want the started frame first and the three deltas after it", names)
	}
	record := fixture.sink.one(t)
	if record.FirstEventMillis == nil {
		t.Fatal("no first-event time recorded")
	}
	if record.FirstTextMillis == nil {
		t.Fatal("no first-text time recorded")
	}
	if *record.FirstTextMillis < *record.FirstEventMillis {
		t.Fatalf("first text %d precedes first event %d", *record.FirstTextMillis, *record.FirstEventMillis)
	}
	if record.Totals.ByPurpose[modelcall.PurposeDialogue].Calls != 0 {
		t.Fatalf("a scripted responder made no provider call but recorded %+v", record.Totals.ByPurpose)
	}
	if record.AnswerStored == nil || !*record.AnswerStored {
		t.Fatal("the answer was not recorded as stored")
	}
	if record.Delivered == nil || !*record.Delivered {
		t.Fatal("the delivered flag was not set on a complete stream")
	}
}

// TestTasksDoNotLeakAcrossConcurrentOperations: one publisher, many tasks.
func TestTasksDoNotLeakAcrossConcurrentOperations(t *testing.T) {
	direct := &fixedResponder{answer: "答案", chunks: []string{"答案"}}
	fixture := newMetricsFixture(t, direct, true)

	var group sync.WaitGroup
	for index := range 12 {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			if _, err := fixture.service.Send(t.Context(), fixture.userID, fixture.sessionID,
				fmt.Sprintf("问题 %d", index), domain.KnowledgeOptions{}); err != nil {
				t.Errorf("Send %d: %v", index, err)
			}
		}(index)
	}
	group.Wait()

	records := fixture.sink.all()
	if len(records) != 12 {
		t.Fatalf("records = %d, want exactly one per operation", len(records))
	}
	seen := map[string]bool{}
	for _, record := range records {
		if record.TaskID == "" || seen[record.TaskID] {
			t.Fatalf("task id %q is empty or repeated across operations", record.TaskID)
		}
		seen[record.TaskID] = true
		if len(record.Calls) != 0 {
			t.Fatalf("task %s holds provider calls %+v, want none from a scripted responder", record.TaskID, record.Calls)
		}
		if record.Totals.Status != modelcall.StatusNoCalls {
			t.Fatalf("task %s status = %s, want no_calls", record.TaskID, record.Totals.Status)
		}
	}
}

// TestNestedCallsJoinTheTaskThatStartedThem: a deeper helper reaches the enclosing
// task rather than opening a competing one.
func TestNestedCallsJoinTheTaskThatStartedThem(t *testing.T) {
	responder := &ctxCapturingResponder{answer: "答案", innerSeen: make(chan *modelcall.TaskHandle, 1)}
	fixture := newMetricsFixture(t, responder, true)

	if _, err := fixture.service.Send(t.Context(), fixture.userID, fixture.sessionID, "问题",
		domain.KnowledgeOptions{}); err != nil {
		t.Fatal(err)
	}
	inner := <-responder.innerSeen
	if inner == nil {
		t.Fatal("a nested helper could not see the task it belongs to")
	}
	record := fixture.sink.one(t)
	// The note came from the responder, several frames below Send, and still
	// reached this task's own record: nesting joins the enclosing task rather than
	// opening a competing one.
	if record.FirstTextMillis == nil {
		t.Fatal("a nested helper's note did not reach the enclosing task record")
	}
	if record.FirstTextIsFinalAnswer {
		t.Fatal("a nested block id cannot be claimed as the confirmed answer block")
	}
	if record.Totals.Status != modelcall.StatusNoCalls {
		t.Fatalf("status = %s, want no provider calls from a scripted responder", record.Totals.Status)
	}
}

// TestIdempotentReplayRecordsNoCalls: a resubmitted answer must not look like a
// paid call that reported nothing.
func TestIdempotentReplayRecordsNoCalls(t *testing.T) {
	script := newProviderScript(t,
		scriptCompletion(learningQuestions([]int{1, 2}, []int{3}, []int{4, 5}), usageBody(60, 6, 66)),
		scriptCompletion(learningGradeReply(50, 20, 12), usageBody(70, 7, 77)),
	)
	fixture := newQuizMetricsFixture(t, script.client)

	view, err := fixture.service.Start(t.Context(), fixture.userID, fixture.agentChat, "Redis 持久化", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.Answer(t.Context(), fixture.userID, view.ID, "sub-1", 1, "RDB 快，AOF 全。"); err != nil {
		t.Fatal(err)
	}
	replay, err := fixture.service.Answer(t.Context(), fixture.userID, view.ID, "sub-1", 1, "RDB 快，AOF 全。")
	if err != nil {
		t.Fatalf("a replayed submission must stay a success: %v", err)
	}
	if replay.ID != view.ID {
		t.Fatalf("replay returned quiz %q, want the same one", replay.ID)
	}

	records := fixture.sink.all()
	if len(records) != 3 {
		t.Fatalf("records = %d, want start, answer and replay: %+v", len(records), records)
	}
	graded, replayRecord := records[1], records[2]
	if graded.Totals.ByPurpose[modelcall.PurposeQuizGrading].Calls != 1 {
		t.Fatalf("grading roll-up = %+v, want one graded call", graded.Totals.ByPurpose)
	}
	if graded.Totals.ByPurpose[modelcall.PurposeQuizGrading].Total.Value != 77 {
		t.Fatalf("grading tokens = %+v, want the 77 the provider reported", graded.Totals.ByPurpose)
	}
	if len(replayRecord.Calls) != 0 || replayRecord.Totals.Status != modelcall.StatusNoCalls {
		t.Fatalf("replay record = %+v, want zero calls: no model was asked again", replayRecord)
	}
	if replayRecord.Outcome != modelcall.OutcomeOK {
		t.Fatalf("replay outcome = %s, want the replay itself recorded as a success", replayRecord.Outcome)
	}
	if script.dispatches() != 2 {
		t.Fatalf("provider saw %d requests, want 2", script.dispatches())
	}
}

// TestSinkFailureCannotChangeBusinessResult: a broken log is a broken log.
func TestSinkFailureCannotChangeBusinessResult(t *testing.T) {
	direct := &fixedResponder{answer: "答案", chunks: []string{"答案"}}
	ctx := t.Context()
	store := repository.NewInMemoryRepository()
	user, err := store.CreateUser(ctx, &domain.User{Email: "a@example.com", Username: "a"})
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.CreateSession(ctx, &domain.Session{UserID: user.ID, Mode: domain.SessionModeChat})
	if err != nil {
		t.Fatal(err)
	}

	broken := &sinkRecorder{fail: errors.New("disk full")}
	brokenService := NewMessageService(store, direct, WithCollectionAccess(openAccess{}),
		WithTaskMetrics(modelcall.NewPublisher(true, broken.write)))
	pair, err := brokenService.Send(ctx, user.ID, session.ID, "问题", domain.KnowledgeOptions{})
	if err != nil {
		t.Fatalf("Send failed because a log line could not be written: %v", err)
	}
	if pair.Assistant.Content != "答案" {
		t.Fatalf("answer = %q, want the business result untouched", pair.Assistant.Content)
	}
	if len(broken.all()) != 0 {
		t.Fatal("a rejected record was kept anyway")
	}
	if stats := brokenService.metrics.Stats(); stats.Failed != 1 || stats.LastFailure != "sink_error" {
		t.Fatalf("stats = %+v, want the failure counted and categorised without its text", stats)
	}

	panicking := &sinkRecorder{panicOn: true}
	panickedService := NewMessageService(store, direct, WithCollectionAccess(openAccess{}),
		WithTaskMetrics(modelcall.NewPublisher(true, panicking.write)))
	if _, err := panickedService.Send(ctx, user.ID, session.ID, "问题二", domain.KnowledgeOptions{}); err != nil {
		t.Fatalf("a panicking sink reached the caller: %v", err)
	}
	if stats := panickedService.metrics.Stats(); stats.Failed != 1 || stats.LastFailure != "sink_panic" {
		t.Fatalf("stats = %+v, want a panic counted as sink_panic", stats)
	}
}

// TestSwitchOffLeavesEverythingUntouched is the promise for the default
// configuration, checked over the answer shapes a user can actually get: a plain
// turn, a streamed turn, and a streamed agent turn with steps. Same frames, same
// stored rows, same provider dispatches, same answer - and no log line at all
// when the switch is off.
func TestSwitchOffLeavesEverythingUntouched(t *testing.T) {
	type observation struct {
		frames      []string
		rows        int
		answer      string
		dispatches  int
		records     int
		recordCalls int
	}

	run := func(shape string, enabled bool) observation {
		t.Helper()
		var script *providerScript
		var assistant responder.Responder
		switch shape {
		case "chat":
			script = newProviderScript(t, scriptCompletion("答案文本。", usageBody(11, 5, 16)))
			direct, err := responder.NewModelStrategy(domain.AnswerStrategyDirectChat, script.client, "回答。")
			if err != nil {
				t.Fatal(err)
			}
			assistant, err = responder.NewStrategyResponder(direct)
			if err != nil {
				t.Fatal(err)
			}
		case "chat_stream":
			script = newProviderScript(t, scriptCompletion("答案文本。", usageBody(11, 5, 16)))
			direct, err := responder.NewModelStrategy(domain.AnswerStrategyDirectChat, script.client, "回答。")
			if err != nil {
				t.Fatal(err)
			}
			assistant, err = responder.NewStrategyResponder(direct)
			if err != nil {
				t.Fatal(err)
			}
		case "agent_stream":
			script = newProviderScript(t,
				scriptToolCall("call-1", "probe_search", "{}", usageBody(20, 4, 24)),
				scriptCompletion("答案文本。", usageBody(30, 6, 36)),
			)
			var err error
			assistant, _ = newAgentResponder(t, script.client)
			if err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatalf("unknown shape %q", shape)
		}

		fixture := newMetricsFixture(t, assistant, enabled)
		ctx := t.Context()
		sessionID := fixture.sessionID
		var err error
		switch shape {
		case "chat":
			_, err = fixture.service.Send(ctx, fixture.userID, sessionID, "问题", domain.KnowledgeOptions{})
		case "chat_stream":
			err = fixture.service.Stream(ctx, fixture.userID, sessionID, "问题", domain.KnowledgeOptions{},
				func(event StreamEvent) error {
					fixture.framesSink = append(fixture.framesSink, event.Name)
					return nil
				})
		case "agent_stream":
			sessionID = fixture.agentID
			err = fixture.service.StreamSteps(ctx, fixture.userID, sessionID, "问题", domain.KnowledgeOptions{},
				func(event StreamEvent) error {
					fixture.framesSink = append(fixture.framesSink, event.Name)
					return nil
				})
		}
		if err != nil {
			t.Fatalf("shape %s enabled=%v: %v", shape, enabled, err)
		}
		rows, err := fixture.store.ListMessages(ctx, fixture.userID, sessionID)
		if err != nil {
			t.Fatal(err)
		}
		result := observation{
			frames:     fixture.framesSink,
			rows:       len(rows),
			records:    len(fixture.sink.all()),
			dispatches: script.dispatches(),
		}
		if len(rows) > 0 {
			result.answer = rows[len(rows)-1].Content
		}
		if records := fixture.sink.all(); len(records) > 0 {
			result.recordCalls = records[0].Totals.Calls
		}
		return result
	}

	for _, shape := range []string{"chat", "chat_stream", "agent_stream"} {
		off := run(shape, false)
		on := run(shape, true)
		if fmt.Sprint(off.frames) != fmt.Sprint(on.frames) {
			t.Fatalf("%s: frames differ with metering on: %v vs %v", shape, off.frames, on.frames)
		}
		if off.rows != on.rows || off.rows != 2 {
			t.Fatalf("%s: stored rows = %d off / %d on, want both 2", shape, off.rows, on.rows)
		}
		if off.answer != on.answer || on.answer == "" {
			t.Fatalf("%s: stored answer differs with metering on: %q vs %q", shape, off.answer, on.answer)
		}
		if off.dispatches != on.dispatches {
			t.Fatalf("%s: provider dispatches = %d off / %d on, want identical", shape, off.dispatches, on.dispatches)
		}
		if off.records != 0 {
			t.Fatalf("%s: records with the switch off = %d, want none", shape, off.records)
		}
		if on.records != 1 {
			t.Fatalf("%s: records with the switch on = %d, want exactly one", shape, on.records)
		}
		if on.recordCalls != on.dispatches {
			t.Fatalf("%s: the record claims %d calls but the provider saw %d",
				shape, on.recordCalls, on.dispatches)
		}
	}
}

// BenchmarkMeteringOverhead measures the cost of the observation itself on one
// offline task, with the switch off and on. It is an in-process micro-benchmark
// over a scripted responder: it says what this code costs here, and nothing about
// production latency, provider time or any optimisation benefit.
func BenchmarkMeteringOverhead(b *testing.B) {
	for _, enabled := range []bool{false, true} {
		name := "metering=off"
		if enabled {
			name = "metering=on"
		}
		b.Run(name, func(b *testing.B) {
			ctx := context.Background()
			store := repository.NewInMemoryRepository()
			user, err := store.CreateUser(ctx, &domain.User{Email: "bench@example.com", Username: "bench"})
			if err != nil {
				b.Fatal(err)
			}
			session, err := store.CreateSession(ctx, &domain.Session{UserID: user.ID, Mode: domain.SessionModeChat})
			if err != nil {
				b.Fatal(err)
			}
			direct := &fixedResponder{answer: "答案", chunks: []string{"答案"}}
			service := NewMessageService(store, direct, WithCollectionAccess(openAccess{}),
				WithTaskMetrics(modelcall.NewPublisher(enabled, func(modelcall.TaskRecord) error { return nil })))
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if _, err := service.Send(ctx, user.ID, session.ID, "问题", domain.KnowledgeOptions{}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
