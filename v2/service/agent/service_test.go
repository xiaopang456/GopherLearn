package agent

import (
	"GopherAI/common/rag"
	agentdao "GopherAI/dao/agent"
	sessiondao "GopherAI/dao/session"
	"GopherAI/model"
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"
)

func TestServiceCompletesRunAndPersistsOnlyPublicFacts(t *testing.T) {
	chatModel := &scriptedChatModel{respond: func(_ context.Context, _ int) (*schema.Message, error) {
		return schema.AssistantMessage("完成答案", nil), nil
	}}
	repository := newMemoryRunRepository(agentTestSession())
	runner := mustRunner(t, chatModel, StaticToolRegistry{}, repository, DefaultRunnerConfig())
	messages := &memoryMessageStore{}
	service, err := NewService(repository, repository, messages, runner)
	if err != nil {
		t.Fatal(err)
	}
	shadowCalls := 0
	service.SetEpisodeShadowObserver(EpisodeShadowObserverFunc(func(_ context.Context, userID int64, sessionID, question string) error {
		shadowCalls++
		if userID != 7 || sessionID != "session-1" || question == "" || len(messages.messages) != 0 {
			t.Fatalf("shadow boundary user=%d session=%s messages=%d", userID, sessionID, len(messages.messages))
		}
		return nil
	}))
	var projectedMessageID uint
	service.SetEpisodePublisher(EpisodeEventPublisherFunc(func(_ context.Context, userID int64, sessionID string, messageID uint) error {
		if userID != 7 || sessionID != "session-1" {
			t.Fatalf("episode owner=%d session=%s", userID, sessionID)
		}
		projectedMessageID = messageID
		return errors.New("episode queue unavailable")
	}))
	events := newEventCollector()
	result, err := service.Run(context.Background(), agentTestRequest(), events)
	if err != nil {
		t.Fatal(err)
	}
	if result.Answer != "完成答案" || result.RunID == "" {
		t.Fatalf("result = %#v", result)
	}
	if result.ContextBudget.Assembled == 0 || result.ContextBudget.CurrentQuestion == 0 {
		t.Fatalf("context budget was not propagated: %+v", result.ContextBudget)
	}
	if repository.status != model.AgentRunCompleted || repository.reason != "answered" {
		t.Fatalf("terminal state = %s reason=%s", repository.status, repository.reason)
	}
	if projectedMessageID != 2 {
		t.Fatalf("episode projected message id=%d", projectedMessageID)
	}
	if shadowCalls != 1 {
		t.Fatalf("shadow calls=%d", shadowCalls)
	}
	persisted := persistedTestMessages(repository, messages)
	if len(persisted) != 2 || !persisted[0].IsUser || persisted[1].IsUser {
		t.Fatalf("messages = %#v", persisted)
	}
	if persisted[0].AnswerBasis != rag.AnswerBasisConversation || persisted[0].KnowledgeStatus != rag.KnowledgeStatusNotRequired ||
		persisted[1].AnswerBasis != rag.AnswerBasisConversation || persisted[1].KnowledgeStatus != rag.KnowledgeStatusNotRequired {
		t.Fatalf("persisted answer states = %#v", persisted)
	}
	if !events.ordered(EventRunStarted, EventStepStarted, EventAnswerDelta, EventRunCompleted) {
		t.Fatalf("event order = %#v", events.events)
	}
	completedEvent := events.events[len(events.events)-1]
	if completedEvent.ContextBudget == nil || completedEvent.ContextBudget.Assembled != result.ContextBudget.Assembled {
		t.Fatalf("completion budget = %#v, result = %+v", completedEvent.ContextBudget, result.ContextBudget)
	}
	for _, step := range repository.steps {
		if step.Type == model.AgentStepPlan && step.ResultSummary != "" {
			t.Fatalf("plan step must not persist hidden reasoning: %#v", step)
		}
	}
}

func TestServiceRejectsIncompleteModelStreamBeforeAssistantPersistence(t *testing.T) {
	chatModel := &scriptedChatModel{
		omitFinishReason: true,
		respond: func(_ context.Context, _ int) (*schema.Message, error) {
			return schema.AssistantMessage("只有一半的回答", nil), nil
		},
	}
	repository := newMemoryRunRepository(agentTestSession())
	runner := mustRunner(t, chatModel, StaticToolRegistry{}, repository, DefaultRunnerConfig())
	messages := &memoryMessageStore{}
	service, err := NewService(repository, repository, messages, runner)
	if err != nil {
		t.Fatal(err)
	}
	events := newEventCollector()
	result, err := service.Run(context.Background(), agentTestRequest(), events)
	if !errors.Is(err, ErrIncompleteModelStream) {
		t.Fatalf("result=%+v error=%v, want ErrIncompleteModelStream", result, err)
	}
	if repository.status != model.AgentRunFailed || repository.reason != "incomplete_model_stream" {
		t.Fatalf("terminal state=%s reason=%s", repository.status, repository.reason)
	}
	if repository.assistant != nil {
		t.Fatalf("partial assistant message was persisted: %#v", repository.assistant)
	}
	persisted := persistedTestMessages(repository, messages)
	if len(persisted) != 1 || !persisted[0].IsUser {
		t.Fatalf("persisted messages=%#v", persisted)
	}
	if len(events.events) == 0 || events.events[len(events.events)-1].TerminationReason != "incomplete_model_stream" {
		t.Fatalf("events=%#v", events.events)
	}
}

func TestServiceClassifiesUnexpectedAcquisitionFailureAsServerBusy(t *testing.T) {
	chatModel := &scriptedChatModel{respond: func(_ context.Context, _ int) (*schema.Message, error) {
		return schema.AssistantMessage("must not run", nil), nil
	}}
	repository := newMemoryRunRepository(agentTestSession())
	repository.acquireErr = errors.New("database unavailable")
	runner := mustRunner(t, chatModel, StaticToolRegistry{}, repository, DefaultRunnerConfig())
	service, err := NewService(repository, repository, &memoryMessageStore{}, runner)
	if err != nil {
		t.Fatal(err)
	}
	events := newEventCollector()
	_, err = service.Run(context.Background(), agentTestRequest(), events)
	if !errors.Is(err, ErrRunTemporarilyUnavailable) {
		t.Fatalf("error=%v, want ErrRunTemporarilyUnavailable", err)
	}
	if chatModel.CallCount() != 0 {
		t.Fatalf("model calls=%d, want 0", chatModel.CallCount())
	}
	if len(events.events) != 1 || events.events[0].TerminationReason != "server_busy" {
		t.Fatalf("events=%#v", events.events)
	}
}

func TestServiceReplaysCompletedRequestWithoutRepeatingSideEffects(t *testing.T) {
	chatModel := &scriptedChatModel{respond: func(_ context.Context, _ int) (*schema.Message, error) {
		return schema.AssistantMessage("只生成一次", nil), nil
	}}
	repository := newMemoryRunRepository(agentTestSession())
	runner := mustRunner(t, chatModel, StaticToolRegistry{}, repository, DefaultRunnerConfig())
	messages := &memoryMessageStore{}
	service, err := NewService(repository, repository, messages, runner)
	if err != nil {
		t.Fatal(err)
	}
	shadowCalls := 0
	service.SetEpisodeShadowObserver(EpisodeShadowObserverFunc(func(context.Context, int64, string, string) error {
		shadowCalls++
		return nil
	}))
	request := agentTestRequest()
	first, err := service.Run(context.Background(), request, DiscardEvents)
	if err != nil {
		t.Fatal(err)
	}
	events := newEventCollector()
	replay, err := service.Run(context.Background(), request, events)
	if err != nil {
		t.Fatal(err)
	}
	if !replay.Replayed || replay.RunID != first.RunID || replay.Answer != first.Answer {
		t.Fatalf("replay=%+v first=%+v", replay, first)
	}
	if replay.AnswerBasis != first.AnswerBasis || replay.KnowledgeStatus != first.KnowledgeStatus || replay.KnowledgeReason != first.KnowledgeReason {
		t.Fatalf("replayed answer state=%+v first=%+v", replay, first)
	}
	if chatModel.CallCount() != 1 || len(persistedTestMessages(repository, messages)) != 2 || shadowCalls != 1 {
		t.Fatalf("model=%d messages=%d shadow=%d", chatModel.CallCount(), len(persistedTestMessages(repository, messages)), shadowCalls)
	}
	if !events.ordered(EventRunStarted, EventRunCompleted) {
		t.Fatalf("replay events=%#v", events.events)
	}
	conflict := request
	conflict.Question = "不同内容"
	result, err := service.Run(context.Background(), conflict, DiscardEvents)
	if !errors.Is(err, ErrRunRequestConflict) || result.RunID != first.RunID {
		t.Fatalf("conflict result=%+v err=%v", result, err)
	}
	policyConflict := request
	policyConflict.KnowledgePolicy = rag.KnowledgeOnly
	result, err = service.Run(context.Background(), policyConflict, DiscardEvents)
	if !errors.Is(err, ErrRunRequestConflict) || result.RunID != first.RunID {
		t.Fatalf("policy conflict result=%+v err=%v", result, err)
	}
	thinkingConflict := request
	thinkingConflict.EnableThinking = true
	result, err = service.Run(context.Background(), thinkingConflict, DiscardEvents)
	if !errors.Is(err, ErrRunRequestConflict) || result.RunID != first.RunID {
		t.Fatalf("thinking conflict result=%+v err=%v", result, err)
	}
}

type capturingContextBuilder struct{ input RunContext }

func (b *capturingContextBuilder) Build(ctx context.Context, input RunContext) (ContextWindow, error) {
	b.input = input
	return (MinimalContextBuilder{}).Build(ctx, input)
}

type provenanceContextBuilder struct{ sources model.EpisodeSourceRefs }

func (b provenanceContextBuilder) Build(ctx context.Context, input RunContext) (ContextWindow, error) {
	window, err := (MinimalContextBuilder{}).Build(ctx, input)
	window.EpisodeIDs = []string{"episode-b"}
	window.EpisodeSources = append(model.EpisodeSourceRefs(nil), b.sources...)
	return window, err
}

func TestServicePersistsEpisodeSourcesOnAssistantMessage(t *testing.T) {
	repository := newMemoryRunRepository(agentTestSession())
	messages := &memoryMessageStore{}
	sources := model.EpisodeSourceRefs{{EpisodeID: "episode-a", Version: 2}, {EpisodeID: "episode-b", Version: 3}}
	runner, err := NewRunner(&scriptedChatModel{respond: func(context.Context, int) (*schema.Message, error) {
		return schema.AssistantMessage("recalled answer", nil), nil
	}}, StaticToolRegistry{}, repository, provenanceContextBuilder{sources: sources}, DefaultRunnerConfig())
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(repository, repository, messages, runner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.Run(context.Background(), agentTestRequest(), DiscardEvents); err != nil {
		t.Fatal(err)
	}
	if repository.assistant == nil || !reflect.DeepEqual(repository.assistant.EpisodeSources, sources) {
		t.Fatalf("assistant provenance=%+v", repository.assistant)
	}
	if len(messages.messages) != 1 || len(messages.messages[0].EpisodeSources) != 0 {
		t.Fatalf("user message must not carry episode provenance: %+v", messages.messages)
	}
}

func TestServiceCapturesEpisodeHistoryBeforePersistingQuestion(t *testing.T) {
	repository := newMemoryRunRepository(agentTestSession())
	messages := &memoryMessageStore{}
	builder := &capturingContextBuilder{}
	runner, err := NewRunner(&scriptedChatModel{respond: func(context.Context, int) (*schema.Message, error) {
		return schema.AssistantMessage("done", nil), nil
	}}, StaticToolRegistry{}, repository, builder, DefaultRunnerConfig())
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(repository, repository, messages, runner)
	if err != nil {
		t.Fatal(err)
	}
	service.SetEpisodeSessionHistoryReader(EpisodeSessionHistoryReaderFunc(func(_ context.Context, userID int64, sessionID string) (bool, error) {
		if userID != 7 || sessionID != "session-1" || len(messages.messages) != 0 {
			t.Fatalf("history snapshot crossed persistence boundary: user=%d session=%s messages=%d", userID, sessionID, len(messages.messages))
		}
		return true, nil
	}))
	request := agentTestRequest()
	request.KnowledgePolicy = rag.KnowledgeOnly
	if _, err = service.Run(context.Background(), request, DiscardEvents); err != nil {
		t.Fatal(err)
	}
	if !builder.input.EpisodeRecallReady || !builder.input.CurrentSessionHasHistory || builder.input.KnowledgePolicy != rag.KnowledgeOnly {
		t.Fatalf("run context lost history snapshot: %+v", builder.input)
	}
}

func TestServiceRejectsInvalidKnowledgePolicyBeforeCreatingRun(t *testing.T) {
	repository := newMemoryRunRepository(agentTestSession())
	runner := mustRunner(t, &scriptedChatModel{respond: func(context.Context, int) (*schema.Message, error) {
		return schema.AssistantMessage("unused", nil), nil
	}}, StaticToolRegistry{}, repository, DefaultRunnerConfig())
	service, err := NewService(repository, repository, &memoryMessageStore{}, runner)
	if err != nil {
		t.Fatal(err)
	}
	request := agentTestRequest()
	request.KnowledgePolicy = rag.KnowledgePolicy("unsafe_fallback")
	if _, err := service.Run(context.Background(), request, DiscardEvents); !errors.Is(err, ErrInvalidRunRequest) {
		t.Fatalf("error = %v", err)
	}
	if repository.run != nil {
		t.Fatal("invalid policy created a run")
	}
}

func TestServiceDoesNotCompleteOrSaveAssistantAfterStreamDisconnect(t *testing.T) {
	chatModel := &scriptedChatModel{respond: func(_ context.Context, _ int) (*schema.Message, error) {
		return schema.AssistantMessage("半截答案不应入库", nil), nil
	}}
	repository := newMemoryRunRepository(agentTestSession())
	runner := mustRunner(t, chatModel, StaticToolRegistry{}, repository, DefaultRunnerConfig())
	messages := &memoryMessageStore{}
	service, err := NewService(repository, repository, messages, runner)
	if err != nil {
		t.Fatal(err)
	}
	sink := EventSinkFunc(func(_ context.Context, event Event) error {
		if event.Type == EventAnswerDelta {
			return errors.New("connection closed")
		}
		return nil
	})
	_, err = service.Run(context.Background(), agentTestRequest(), sink)
	if !errors.Is(err, ErrEventDelivery) {
		t.Fatalf("error = %v, want ErrEventDelivery", err)
	}
	if repository.status != model.AgentRunCancelled {
		t.Fatalf("status = %s, want cancelled", repository.status)
	}
	if len(messages.messages) != 1 || !messages.messages[0].IsUser {
		t.Fatalf("messages = %#v; assistant answer must not be saved", messages.messages)
	}
}

func TestServicePersistsAgentSourcesWithAssistantMessage(t *testing.T) {
	chatModel := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		if call == 1 {
			return schema.AssistantMessage("", []schema.ToolCall{{
				ID: "source-call", Function: schema.FunctionCall{Name: "evidence", Arguments: `{}`},
			}}), nil
		}
		return schema.AssistantMessage("基于笔记回答 [1]", nil), nil
	}}
	evidenceTool, err := toolutils.InferTool("evidence", "return evidence", func(context.Context, *struct{}) (*structuredEvidenceOutput, error) {
		return &structuredEvidenceOutput{Summary: "找到证据", Results: []structuredEvidenceResult{{
			SourceID: "doc:v1:chunk", Citation: 1, DocumentID: "doc", DocumentVersion: 1,
			DocumentName: "note.md", ChunkID: "chunk", ChunkIndex: 2, Content: "evidence",
		}}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	repository := newMemoryRunRepository(agentTestSession())
	runner := mustRunner(t, chatModel, StaticToolRegistry{Registered: []tool.BaseTool{evidenceTool}}, repository, DefaultRunnerConfig())
	messages := &memoryMessageStore{}
	service, err := NewService(repository, repository, messages, runner)
	if err != nil {
		t.Fatal(err)
	}
	events := newEventCollector()
	result, err := service.Run(context.Background(), agentTestRequest(), events)
	if err != nil {
		t.Fatal(err)
	}
	persisted := persistedTestMessages(repository, messages)
	if len(result.Sources) != 1 || len(persisted) != 2 || len(persisted[1].Sources) != 1 {
		t.Fatalf("result sources=%#v messages=%#v", result.Sources, persisted)
	}
	if persisted[1].Sources[0].DocID != "doc" || persisted[0].Sources != nil {
		t.Fatalf("persisted sources=%#v", persisted)
	}
	if result.AnswerBasis != rag.AnswerBasisPersonalKnowledge || result.KnowledgeStatus != rag.KnowledgeStatusGrounded ||
		persisted[1].AnswerBasis != result.AnswerBasis || persisted[1].KnowledgeStatus != result.KnowledgeStatus {
		t.Fatalf("grounded states result=%+v persisted=%+v", result, persisted[1])
	}
	events.mu.Lock()
	defer events.mu.Unlock()
	completedHasFinalSources := false
	for _, event := range events.events {
		if event.Type == EventRunCompleted && event.Content == result.Answer && len(event.Sources) == 1 && event.Sources[0].DocID == "doc" &&
			event.AnswerBasis == rag.AnswerBasisPersonalKnowledge && event.KnowledgeStatus == rag.KnowledgeStatusGrounded {
			completedHasFinalSources = true
		}
	}
	if !completedHasFinalSources {
		t.Fatalf("run_completed missing final sources: %#v", events.events)
	}
}

func TestServicePersistsAndReplaysGeneralKnowledgeFallbackState(t *testing.T) {
	chatModel := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		if call == 1 {
			return schema.AssistantMessage("", []schema.ToolCall{{ID: "source-call", Function: schema.FunctionCall{Name: "evidence", Arguments: `{}`}}}), nil
		}
		return schema.AssistantMessage("TCP 提供可靠字节流。", nil), nil
	}}
	evidenceTool, err := toolutils.InferTool("evidence", "return no evidence", func(context.Context, *struct{}) (*structuredEvidenceOutput, error) {
		return &structuredEvidenceOutput{Status: "no_candidates", Reason: rag.KnowledgeReasonNoCandidate, Summary: "none", Results: []structuredEvidenceResult{}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	repository := newMemoryRunRepository(agentTestSession())
	runner := mustRunner(t, chatModel, StaticToolRegistry{Registered: []tool.BaseTool{evidenceTool}}, repository, DefaultRunnerConfig())
	service, err := NewService(repository, repository, &memoryMessageStore{}, runner)
	if err != nil {
		t.Fatal(err)
	}
	events := newEventCollector()
	request := agentTestRequest()
	result, err := service.Run(context.Background(), request, events)
	if err != nil {
		t.Fatal(err)
	}
	if result.AnswerBasis != rag.AnswerBasisGeneralKnowledge || result.KnowledgeStatus != rag.KnowledgeStatusNoEvidence || result.KnowledgeReason != rag.KnowledgeReasonNoCandidate || len(result.Sources) != 0 {
		t.Fatalf("result = %+v", result)
	}
	if repository.assistant == nil || repository.assistant.AnswerBasis != result.AnswerBasis || repository.assistant.KnowledgeStatus != result.KnowledgeStatus || repository.assistant.KnowledgeReason != result.KnowledgeReason {
		t.Fatalf("assistant = %+v", repository.assistant)
	}
	replayEvents := newEventCollector()
	replay, err := service.Run(context.Background(), request, replayEvents)
	if err != nil {
		t.Fatal(err)
	}
	if !replay.Replayed || replay.AnswerBasis != result.AnswerBasis || replay.KnowledgeStatus != result.KnowledgeStatus || replay.KnowledgeReason != result.KnowledgeReason || chatModel.CallCount() != 2 {
		t.Fatalf("replay=%+v model_calls=%d", replay, chatModel.CallCount())
	}
}

func TestServiceDoesNotPersistAssistantOutsideAtomicCompletion(t *testing.T) {
	repository := newMemoryRunRepository(agentTestSession())
	repository.completeErr = errors.New("commit failed")
	runner := mustRunner(t, &scriptedChatModel{respond: func(context.Context, int) (*schema.Message, error) {
		return schema.AssistantMessage("must roll back", nil), nil
	}}, StaticToolRegistry{}, repository, DefaultRunnerConfig())
	messages := &memoryMessageStore{}
	service, err := NewService(repository, repository, messages, runner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Run(context.Background(), agentTestRequest(), DiscardEvents); !errors.Is(err, repository.completeErr) {
		t.Fatalf("run error=%v, want commit error", err)
	}
	if len(messages.messages) != 1 || !messages.messages[0].IsUser || repository.assistant != nil {
		t.Fatalf("user messages=%#v assistant=%#v", messages.messages, repository.assistant)
	}
	if repository.status != model.AgentRunRunning {
		t.Fatalf("run status=%s, want running because commit rolled back", repository.status)
	}
}

func TestServicePersistsTimeoutStatus(t *testing.T) {
	chatModel := &scriptedChatModel{respond: func(ctx context.Context, _ int) (*schema.Message, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	repository := newMemoryRunRepository(agentTestSession())
	config := DefaultRunnerConfig()
	config.Deadline = 15 * time.Millisecond
	runner := mustRunner(t, chatModel, StaticToolRegistry{}, repository, config)
	service, err := NewService(repository, repository, &memoryMessageStore{}, runner)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Run(context.Background(), agentTestRequest(), newEventCollector())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want deadline exceeded", err)
	}
	if repository.status != model.AgentRunTimeout || repository.reason != "timeout" {
		t.Fatalf("terminal state = %s reason=%s", repository.status, repository.reason)
	}
}

func TestServicePersistsCancellationStatus(t *testing.T) {
	chatModel := &scriptedChatModel{respond: func(ctx context.Context, _ int) (*schema.Message, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	repository := newMemoryRunRepository(agentTestSession())
	runner := mustRunner(t, chatModel, StaticToolRegistry{}, repository, DefaultRunnerConfig())
	service, err := NewService(repository, repository, &memoryMessageStore{}, runner)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	_, err = service.Run(ctx, agentTestRequest(), newEventCollector())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context canceled", err)
	}
	if repository.status != model.AgentRunCancelled || repository.reason != "cancelled" {
		t.Fatalf("terminal state = %s reason=%s", repository.status, repository.reason)
	}
}

func TestServiceRejectsNonAgentAndCrossUserSession(t *testing.T) {
	chatModel := &scriptedChatModel{respond: func(_ context.Context, _ int) (*schema.Message, error) {
		return schema.AssistantMessage("answer", nil), nil
	}}
	for _, test := range []struct {
		name    string
		session model.Session
		request RunRequest
		want    error
	}{
		{name: "non-agent", session: func() model.Session { s := agentTestSession(); s.Mode = model.SessionModeRAG; return s }(), request: agentTestRequest(), want: ErrAgentSessionRequired},
		{name: "cross-user", session: agentTestSession(), request: func() RunRequest { r := agentTestRequest(); r.UserID = 999; return r }(), want: sessiondao.ErrSessionNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository := newMemoryRunRepository(test.session)
			runner := mustRunner(t, chatModel, StaticToolRegistry{}, repository, DefaultRunnerConfig())
			service, err := NewService(repository, repository, &memoryMessageStore{}, runner)
			if err != nil {
				t.Fatal(err)
			}
			_, err = service.Run(context.Background(), test.request, DiscardEvents)
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
			if repository.run != nil {
				t.Fatal("rejected request must not create a run")
			}
		})
	}
}

func agentTestSession() model.Session {
	return model.Session{ID: "session-1", UserID: 7, UserName: "learner", Mode: model.SessionModeAgent}
}

func agentTestRequest() RunRequest {
	return RunRequest{UserID: 7, UserName: "learner", SessionID: "session-1", Question: "解释 Context 取消", RequestID: "request-0001"}
}

type memoryRunRepository struct {
	mu           sync.Mutex
	session      model.Session
	run          *model.AgentRun
	steps        []model.AgentStep
	status       model.AgentRunStatus
	reason       string
	acquireCalls int
	assistant    *model.Message
	completeErr  error
	acquireErr   error
	leaseLost    bool
}

func newMemoryRunRepository(session model.Session) *memoryRunRepository {
	return &memoryRunRepository{session: session}
}

func (r *memoryRunRepository) GetByIDAndUserID(_ context.Context, sessionID string, userID int64) (*model.Session, error) {
	if r.session.ID != sessionID || r.session.UserID != userID {
		return nil, sessiondao.ErrSessionNotFound
	}
	copyOfSession := r.session
	return &copyOfSession, nil
}

func (r *memoryRunRepository) CreateRun(_ context.Context, run *model.AgentRun) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	copyOfRun := *run
	r.run = &copyOfRun
	r.status = run.Status
	return nil
}

func (r *memoryRunRepository) AcquireRun(_ context.Context, run *model.AgentRun) (*agentdao.RunAcquisition, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.acquireCalls++
	if r.acquireErr != nil {
		return nil, r.acquireErr
	}
	if r.run != nil && r.run.RequestKey != nil && run.RequestKey != nil && *r.run.RequestKey == *run.RequestKey {
		copyOfRun := *r.run
		if r.run.SessionID != run.SessionID || r.run.InputHash != run.InputHash {
			return &agentdao.RunAcquisition{Run: &copyOfRun}, agentdao.ErrRunRequestConflict
		}
		if r.status == model.AgentRunCompleted && r.run.ResultJSON != "" {
			return &agentdao.RunAcquisition{Run: &copyOfRun}, nil
		}
		return &agentdao.RunAcquisition{Run: &copyOfRun}, agentdao.ErrRunInProgress
	}
	copyOfRun := *run
	r.run = &copyOfRun
	r.status = run.Status
	return &agentdao.RunAcquisition{Run: &copyOfRun, Created: true}, nil
}

// loseLease makes every later renewal report that the run is no longer ours,
// which is what the DAO returns once the row no longer matches the token.
func (r *memoryRunRepository) loseLease() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.leaseLost = true
}

func (r *memoryRunRepository) RenewRunLease(_ context.Context, _ int64, _, _, leaseToken string, now, leaseEnds time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.leaseLost || r.run == nil || r.run.LeaseToken != leaseToken || r.status != model.AgentRunRunning {
		return agentdao.ErrRunLeaseLost
	}
	r.run.HeartbeatAt, r.run.LeaseExpiresAt = &now, &leaseEnds
	return nil
}

func (r *memoryRunRepository) CompleteRun(
	_ context.Context, _ int64, _, _, leaseToken string, _ uint64, assistant *model.Message, answer, resultJSON string,
) (*model.AgentRun, *model.Message, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.run == nil || r.status != model.AgentRunRunning || r.run.LeaseToken != leaseToken {
		return nil, nil, errors.New("state conflict")
	}
	if r.completeErr != nil {
		return nil, nil, r.completeErr
	}
	r.status = model.AgentRunCompleted
	r.reason = "answered"
	r.run.Status = model.AgentRunCompleted
	r.run.AnswerSummary = answer
	r.run.ResultJSON = resultJSON
	r.run.Version++
	copyOfAssistant := *assistant
	copyOfAssistant.ID = 2
	r.assistant = &copyOfAssistant
	copyOfRun := *r.run
	return &copyOfRun, &copyOfAssistant, nil
}

func (r *memoryRunRepository) TransitionLeasedRun(
	_ context.Context, _ int64, _, _, leaseToken string, _ uint64,
	to model.AgentRunStatus, reason, _ string,
) (*model.AgentRun, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.run == nil || r.status != model.AgentRunRunning || r.run.LeaseToken != leaseToken {
		return nil, agentdao.ErrRunLeaseLost
	}
	r.status, r.reason, r.run.Status = to, reason, to
	r.run.Version++
	copyOfRun := *r.run
	return &copyOfRun, nil
}

func (r *memoryRunRepository) AppendStep(_ context.Context, _ string, step *model.AgentStep) error {
	r.mu.Lock()
	r.steps = append(r.steps, *step)
	r.mu.Unlock()
	return nil
}

func (r *memoryRunRepository) TransitionRun(
	_ context.Context, _ int64, _, _ string, _ uint64,
	from, to model.AgentRunStatus, reason, _ string,
) (*model.AgentRun, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.run == nil || r.status != from {
		return nil, errors.New("state conflict")
	}
	r.status = to
	r.reason = reason
	r.run.Status = to
	r.run.Version++
	copyOfRun := *r.run
	return &copyOfRun, nil
}

type memoryMessageStore struct {
	mu       sync.Mutex
	messages []model.Message
}

func (s *memoryMessageStore) Create(_ context.Context, message *model.Message) error {
	s.mu.Lock()
	if message.AgentRunID != nil && message.AgentRunRole != nil {
		for _, existing := range s.messages {
			if existing.AgentRunID != nil && existing.AgentRunRole != nil && *existing.AgentRunID == *message.AgentRunID && *existing.AgentRunRole == *message.AgentRunRole {
				s.mu.Unlock()
				return nil
			}
		}
	}
	if message.ID == 0 {
		message.ID = uint(len(s.messages) + 1)
	}
	s.messages = append(s.messages, *message)
	s.mu.Unlock()
	return nil
}

func persistedTestMessages(repository *memoryRunRepository, messages *memoryMessageStore) []model.Message {
	messages.mu.Lock()
	persisted := append([]model.Message(nil), messages.messages...)
	messages.mu.Unlock()
	repository.mu.Lock()
	if repository.assistant != nil {
		persisted = append(persisted, *repository.assistant)
	}
	repository.mu.Unlock()
	return persisted
}

func (c *eventCollector) ordered(types ...EventType) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	position := 0
	for _, event := range c.events {
		if position < len(types) && event.Type == types[position] {
			position++
		}
	}
	return position == len(types)
}

// BUG-AGENT-103. A run whose lease ends must still end visibly: a terminal row
// and a run_failed event. Before the fix the service returned the lease error
// directly, so the row was left for the reaper to mark lease_expired and the
// client saw a stream that simply stopped.
func TestServiceWritesTerminalStateAndFailureEventWhenTheLeaseIsLost(t *testing.T) {
	chatModel := &scriptedChatModel{respond: func(ctx context.Context, _ int) (*schema.Message, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	repository := newMemoryRunRepository(agentTestSession())
	runner := mustRunner(t, chatModel, StaticToolRegistry{}, repository, DefaultRunnerConfig())
	service, err := NewService(repository, repository, &memoryMessageStore{}, runner)
	if err != nil {
		t.Fatal(err)
	}
	// A heartbeat short enough to fire while the model is still blocked. The
	// repository answers a renewal for an unknown token with ErrRunLeaseLost,
	// which is the definite-loss case, so this is one attempt and no retry.
	service.leaseHeartbeat = 5 * time.Millisecond
	service.leaseDuration = 200 * time.Millisecond
	repository.loseLease()

	events := newEventCollector()
	_, err = service.Run(context.Background(), agentTestRequest(), events)
	if err == nil || !errors.Is(err, agentdao.ErrRunLeaseLost) {
		t.Fatalf("error = %v, want the lease failure", err)
	}
	if repository.status != model.AgentRunFailed || repository.reason != "lease_lost" {
		t.Errorf("terminal state = %s reason = %q, want failed/lease_lost",
			repository.status, repository.reason)
	}
	if !events.contains(EventRunFailed) {
		t.Error("no run_failed event was sent; the client would see the stream simply stop")
	}
}
