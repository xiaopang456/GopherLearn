package agent

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	agentmodel "GopherAI/model"
	einoopenai "github.com/cloudwego/eino-ext/components/model/openai"
	modelcomponent "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// retryProbeModel is a chat model whose next answer is scripted, so a test can
// make the provider fail once and answer next time.
type retryProbeModel struct {
	mu       sync.Mutex
	calls    int
	choices  []string
	respond  func(ctx context.Context, call int) (*schema.Message, error)
	stream   func(ctx context.Context, call int) (*schema.StreamReader[*schema.Message], error)
	toolList [][]string
}

func (m *retryProbeModel) record(opts ...modelcomponent.Option) int {
	options := modelcomponent.GetCommonOptions(nil, opts...)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	choice := ""
	if options.ToolChoice != nil {
		choice = string(*options.ToolChoice)
	}
	m.choices = append(m.choices, choice)
	return m.calls
}

func (m *retryProbeModel) Generate(ctx context.Context, _ []*schema.Message, opts ...modelcomponent.Option) (*schema.Message, error) {
	call := m.record(opts...)
	if m.respond == nil {
		return schema.AssistantMessage("answer", nil), nil
	}
	return m.respond(ctx, call)
}

func (m *retryProbeModel) Stream(ctx context.Context, _ []*schema.Message, opts ...modelcomponent.Option) (*schema.StreamReader[*schema.Message], error) {
	call := m.record(opts...)
	if m.stream != nil {
		return m.stream(ctx, call)
	}
	message, err := m.respond(ctx, call)
	if err != nil {
		return nil, err
	}
	reader, writer := schema.Pipe[*schema.Message](1)
	go func() {
		defer writer.Close()
		writer.Send(message, nil)
	}()
	return reader, nil
}

func (m *retryProbeModel) WithTools(tools []*schema.ToolInfo) (modelcomponent.ToolCallingChatModel, error) {
	names := make([]string, 0, len(tools))
	for _, info := range tools {
		if info != nil {
			names = append(names, info.Name)
		}
	}
	m.mu.Lock()
	m.toolList = append(m.toolList, names)
	m.mu.Unlock()
	return m, nil
}

func (m *retryProbeModel) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func (m *retryProbeModel) recordedChoices() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.choices...)
}

// newRetryState builds the turn state a bounded model needs: the budget, the
// step repository the steps are written to, and the event sink.
func newRetryState(steps StepRepository, maxModelCalls int) *runState {
	return &runState{
		input:        RunContext{RunID: "run-retry", UserID: 7, UserName: "learner", SessionID: "session-retry", Question: "question"},
		sink:         DiscardEvents,
		steps:        steps,
		config:       RunnerConfig{MaxModelCalls: maxModelCalls, MaxToolCalls: 3},
		sourceKeys:   map[string]struct{}{},
		perToolCalls: map[string]int{},
		progress:     newToolProgressLedger(),
	}
}

func newRetryBounded(t *testing.T, model *retryProbeModel, maxModelCalls int) (*boundedModel, *memoryStepRepository, *runState) {
	t.Helper()
	steps := newMemoryStepRepository()
	state := newRetryState(steps, maxModelCalls)
	return &boundedModel{inner: model, state: state}, steps, state
}

// providerStatus builds the error the chat model returns for a status the
// provider answered with.
func providerStatus(status int) error {
	return &einoopenai.APIError{HTTPStatusCode: status, Message: "provider answered"}
}

// providerTransport builds the error the chat model returns for a connection
// that failed before the provider answered.
func providerTransport() error {
	return &url.Error{
		Op:  "Post",
		URL: "https://provider.invalid/v1/chat/completions",
		Err: &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED},
	}
}

func retrySummaries(steps *memoryStepRepository) []string {
	steps.mu.Lock()
	defer steps.mu.Unlock()
	summaries := make([]string, 0, len(steps.steps))
	for _, step := range steps.steps {
		summaries = append(summaries, step.ArgumentsSummary)
	}
	return summaries
}

func TestBoundedModelRetriesOnceWhenTheProviderDidNotProcessTheRequest(t *testing.T) {
	model := &retryProbeModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		if call == 1 {
			return nil, providerStatus(503)
		}
		return schema.AssistantMessage("answered after the retry", nil), nil
	}}
	bounded, steps, state := newRetryBounded(t, model, 4)

	message, err := bounded.Generate(context.Background(), []*schema.Message{schema.UserMessage("hi")})
	if err != nil {
		t.Fatalf("the retry should have produced an answer: %v", err)
	}
	if message.Content != "answered after the retry" {
		t.Fatalf("the answer must come from the second attempt, got %q", message.Content)
	}
	if model.callCount() != 2 {
		t.Fatalf("the provider must be asked twice, asked %d times", model.callCount())
	}
	if state.modelCalls != 2 {
		t.Fatalf("the retry is a model call of its own, modelCalls=%d", state.modelCalls)
	}
	summaries := retrySummaries(steps)
	if len(summaries) != 2 {
		t.Fatalf("both attempts must be recorded, got %v", summaries)
	}
	if !strings.Contains(summaries[1], "retry after status_503") {
		t.Fatalf("the second record must say it is a retry, got %q", summaries[1])
	}
	if strings.Contains(summaries[0], "retry") {
		t.Fatalf("the first record is not a retry, got %q", summaries[0])
	}
}

func TestBoundedModelGivesUpAfterTwoFailures(t *testing.T) {
	model := &retryProbeModel{respond: func(context.Context, int) (*schema.Message, error) {
		return nil, providerStatus(503)
	}}
	bounded, _, state := newRetryBounded(t, model, 4)

	_, err := bounded.Generate(context.Background(), []*schema.Message{schema.UserMessage("hi")})
	if err == nil {
		t.Fatal("two failures in a row must fail the generation")
	}
	if !errors.Is(err, ErrProviderTransport) {
		t.Fatalf("the failure must stay a provider fault, got %v", err)
	}
	if model.callCount() != 2 {
		t.Fatalf("the provider must be asked exactly twice, asked %d times", model.callCount())
	}
	if state.modelCalls != 2 {
		t.Fatalf("both attempts must be counted, modelCalls=%d", state.modelCalls)
	}
}

func TestBoundedModelDoesNotRetryAStreamThatBrokeAfterItStarted(t *testing.T) {
	model := &retryProbeModel{stream: func(context.Context, int) (*schema.StreamReader[*schema.Message], error) {
		reader, writer := schema.Pipe[*schema.Message](1)
		go func() {
			defer writer.Close()
			writer.Send(schema.AssistantMessage("half an answer", nil), nil)
			writer.Send(nil, errors.New("connection reset by peer"))
		}()
		return reader, nil
	}}
	bounded, _, _ := newRetryBounded(t, model, 4)

	stream, err := bounded.Stream(context.Background(), []*schema.Message{schema.UserMessage("hi")})
	if err != nil {
		t.Fatalf("the stream itself starts, so no error here: %v", err)
	}
	defer stream.Close()
	var sawError bool
	for {
		_, recvErr := stream.Recv()
		if recvErr != nil {
			sawError = true
			break
		}
	}
	if !sawError {
		t.Fatal("the broken stream must surface its error")
	}
	if model.callCount() != 1 {
		t.Fatalf("a stream that already produced content must not be repeated, asked %d times", model.callCount())
	}
}

func TestBoundedModelDoesNotRetryRateLimitedRequests(t *testing.T) {
	for _, status := range []int{400, 401, 403, 429} {
		model := &retryProbeModel{respond: func(context.Context, int) (*schema.Message, error) {
			return nil, providerStatus(status)
		}}
		bounded, _, _ := newRetryBounded(t, model, 4)

		if _, err := bounded.Generate(context.Background(), []*schema.Message{schema.UserMessage("hi")}); err == nil {
			t.Fatalf("status %d must fail the generation", status)
		}
		if model.callCount() != 1 {
			t.Fatalf("status %d must not be repeated, asked %d times", status, model.callCount())
		}
	}
}

func TestBoundedModelDoesNotRetryWhenTheTurnHasNoTimeLeft(t *testing.T) {
	model := &retryProbeModel{respond: func(context.Context, int) (*schema.Message, error) {
		return nil, providerStatus(503)
	}}
	bounded, _, _ := newRetryBounded(t, model, 4)

	ctx, cancel := context.WithTimeout(context.Background(), modelRetryMinRemaining/2)
	defer cancel()
	if _, err := bounded.Generate(ctx, []*schema.Message{schema.UserMessage("hi")}); err == nil {
		t.Fatal("the generation must fail when the turn has no time left")
	}
	if model.callCount() != 1 {
		t.Fatalf("a turn without time left must not retry, asked %d times", model.callCount())
	}
}

func TestBoundedModelKeepsTheProviderErrorWhenTheBudgetIsSpent(t *testing.T) {
	model := &retryProbeModel{respond: func(context.Context, int) (*schema.Message, error) {
		return nil, providerStatus(503)
	}}
	// One generation is allowed, and the failed attempt spends it.
	bounded, _, state := newRetryBounded(t, model, 1)

	_, err := bounded.Generate(context.Background(), []*schema.Message{schema.UserMessage("hi")})
	if err == nil {
		t.Fatal("the generation must fail")
	}
	if errors.Is(err, ErrModelCallLimit) {
		t.Fatalf("the budget error must not hide the provider failure, got %v", err)
	}
	if !errors.Is(err, ErrProviderTransport) {
		t.Fatalf("the failure must stay a provider fault, got %v", err)
	}
	if model.callCount() != 1 {
		t.Fatalf("a spent budget must not retry, asked %d times", model.callCount())
	}
	if state.modelCalls != 1 {
		t.Fatalf("only the first attempt may be counted, modelCalls=%d", state.modelCalls)
	}
}

func TestBoundedModelRetryAsksTheSameQuestionAsTheAttemptItReplaces(t *testing.T) {
	model := &retryProbeModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		if call == 1 {
			return nil, providerStatus(503)
		}
		return schema.AssistantMessage("answered", nil), nil
	}}
	bounded, _, state := newRetryBounded(t, model, 3)
	// The tool budget is spent, so this generation writes the answer rather
	// than asking for a tool. The retry has to keep that choice: a retry
	// that asked for a tool would be a different question.
	state.toolCalls = state.config.MaxToolCalls

	if _, err := bounded.Generate(context.Background(), []*schema.Message{schema.UserMessage("hi")}); err != nil {
		t.Fatalf("the retry should have produced an answer: %v", err)
	}
	choices := model.recordedChoices()
	if len(choices) != 2 {
		t.Fatalf("the provider must be asked twice, asked %d times", len(choices))
	}
	if choices[0] != choices[1] {
		t.Fatalf("the retry must repeat the tool choice, first %q then %q", choices[0], choices[1])
	}
	if choices[0] != string(schema.ToolChoiceForbidden) {
		t.Fatalf("a generation that writes the answer must not be allowed to call tools, got %q", choices[0])
	}
}

func TestBoundedModelRetryMayNotTakeTheGenerationThatWritesTheAnswer(t *testing.T) {
	model := &retryProbeModel{respond: func(context.Context, int) (*schema.Message, error) {
		return nil, providerStatus(503)
	}}
	// Two generations: the first one decides about tools, the second one
	// writes the answer. A retry of the first one would take the slot that
	// writes the answer, so the turn must fail with the provider's error
	// instead of losing its way to an answer.
	bounded, _, state := newRetryBounded(t, model, 2)

	_, err := bounded.Generate(context.Background(), []*schema.Message{schema.UserMessage("hi")})
	if err == nil {
		t.Fatal("the generation must fail")
	}
	if !errors.Is(err, ErrProviderTransport) {
		t.Fatalf("the failure must stay a provider fault, got %v", err)
	}
	if model.callCount() != 1 {
		t.Fatalf("the retry must not take the answering generation, asked %d times", model.callCount())
	}
	if state.modelCalls != 1 {
		t.Fatalf("only the first attempt may be counted, modelCalls=%d", state.modelCalls)
	}
}

func TestBoundedModelStopsWhenTheTurnIsCancelledDuringThePause(t *testing.T) {
	model := &retryProbeModel{respond: func(context.Context, int) (*schema.Message, error) {
		return nil, providerStatus(503)
	}}
	bounded, _, _ := newRetryBounded(t, model, 4)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	defer cancel()

	if _, err := bounded.Generate(ctx, []*schema.Message{schema.UserMessage("hi")}); err == nil {
		t.Fatal("a cancelled turn must fail")
	}
	if model.callCount() != 1 {
		t.Fatalf("a cancelled pause must not send a second request, asked %d times", model.callCount())
	}
}

func TestBoundedModelRetriesAConnectionThatFailedBeforeTheProviderAnswered(t *testing.T) {
	model := &retryProbeModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		if call == 1 {
			return nil, providerTransport()
		}
		return schema.AssistantMessage("answered after the retry", nil), nil
	}}
	bounded, steps, _ := newRetryBounded(t, model, 4)

	if _, err := bounded.Generate(context.Background(), []*schema.Message{schema.UserMessage("hi")}); err != nil {
		t.Fatalf("the retry should have produced an answer: %v", err)
	}
	if model.callCount() != 2 {
		t.Fatalf("the provider must be asked twice, asked %d times", model.callCount())
	}
	summaries := retrySummaries(steps)
	if len(summaries) != 2 || !strings.Contains(summaries[1], "retry after transport") {
		t.Fatalf("the second record must say it is a retry, got %v", summaries)
	}
}

// The retry must not change what a run record looks like when nothing failed:
// one generation, one record, no mention of a retry.
func TestBoundedModelRecordsOneGenerationWhenNothingFailed(t *testing.T) {
	model := &retryProbeModel{respond: func(context.Context, int) (*schema.Message, error) {
		return schema.AssistantMessage("answered", nil), nil
	}}
	bounded, steps, state := newRetryBounded(t, model, 4)

	if _, err := bounded.Generate(context.Background(), []*schema.Message{schema.UserMessage("hi")}); err != nil {
		t.Fatalf("the generation must succeed: %v", err)
	}
	if model.callCount() != 1 {
		t.Fatalf("a healthy call must be asked once, asked %d times", model.callCount())
	}
	if state.modelCalls != 1 {
		t.Fatalf("a healthy call spends one generation, modelCalls=%d", state.modelCalls)
	}
	summaries := retrySummaries(steps)
	if len(summaries) != 1 || strings.Contains(summaries[0], "retry") {
		t.Fatalf("a healthy call records one plain generation, got %v", summaries)
	}
}

// The step a retry writes is an ordinary step: it carries the run and the
// session like any other.
func TestRetryStepCarriesTheRun(t *testing.T) {
	model := &retryProbeModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		if call == 1 {
			return nil, providerStatus(503)
		}
		return schema.AssistantMessage("answered", nil), nil
	}}
	bounded, steps, _ := newRetryBounded(t, model, 4)

	if _, err := bounded.Generate(context.Background(), []*schema.Message{schema.UserMessage("hi")}); err != nil {
		t.Fatalf("the retry should have produced an answer: %v", err)
	}
	steps.mu.Lock()
	defer steps.mu.Unlock()
	if len(steps.steps) != 2 {
		t.Fatalf("both attempts must be recorded, got %d", len(steps.steps))
	}
	for index, step := range steps.steps {
		if step.RunID != "run-retry" {
			t.Fatalf("step %d must carry the run, got %q", index, step.RunID)
		}
		if step.Type != agentmodel.AgentStepPlan {
			t.Fatalf("step %d must be a plan step, got %q", index, step.Type)
		}
		if step.Sequence != index+1 {
			t.Fatalf("step %d must have the next sequence, got %d", index, step.Sequence)
		}
	}
}
