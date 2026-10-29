package agent

import (
	"GopherAI/common/rag"
	agentdao "GopherAI/dao/agent"
	messagedao "GopherAI/dao/message"
	sessiondao "GopherAI/dao/session"
	"GopherAI/model"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

var (
	ErrServiceNotConfigured      = errors.New("agent service is not configured")
	ErrAgentSessionRequired      = errors.New("session is not in agent mode")
	ErrInvalidRunRequest         = errors.New("invalid agent run request")
	ErrRunInProgress             = agentdao.ErrRunInProgress
	ErrRunRequestConflict        = agentdao.ErrRunRequestConflict
	ErrRunRequestFinished        = agentdao.ErrRunRequestFinished
	ErrRunTemporarilyUnavailable = agentdao.ErrRunTemporarilyUnavailable
	ErrMessageLimitReached       = agentdao.ErrMessageLimitReached
)

type SessionRepository interface {
	GetByIDAndUserID(context.Context, string, int64) (*model.Session, error)
}

type RunRepository interface {
	StepRepository
	AcquireRun(context.Context, *model.AgentRun) (*agentdao.RunAcquisition, error)
	RenewRunLease(context.Context, int64, string, string, string, time.Time, time.Time) error
	CompleteRun(context.Context, int64, string, string, string, uint64, *model.Message, string, string) (*model.AgentRun, *model.Message, error)
	TransitionLeasedRun(context.Context, int64, string, string, string, uint64, model.AgentRunStatus, string, string) (*model.AgentRun, error)
}

type MessageStore interface {
	Create(context.Context, *model.Message) error
}

type MemoryEventPublisher interface {
	PublishAgentRun(context.Context, int64, string, string, string, int, bool) error
}

type MemoryEventPublisherFunc func(context.Context, int64, string, string, string, int, bool) error

func (f MemoryEventPublisherFunc) PublishAgentRun(ctx context.Context, userID int64, sessionID, runID, question string, toolCalls int, draftProposed bool) error {
	return f(ctx, userID, sessionID, runID, question, toolCalls, draftProposed)
}

var DiscardMemoryEvents MemoryEventPublisher = MemoryEventPublisherFunc(func(context.Context, int64, string, string, string, int, bool) error { return nil })

type EpisodeEventPublisher interface {
	PublishConversationCompleted(context.Context, int64, string, uint) error
}

type EpisodeEventPublisherFunc func(context.Context, int64, string, uint) error

func (f EpisodeEventPublisherFunc) PublishConversationCompleted(ctx context.Context, userID int64, sessionID string, latestMessageID uint) error {
	return f(ctx, userID, sessionID, latestMessageID)
}

var DiscardEpisodeEvents EpisodeEventPublisher = EpisodeEventPublisherFunc(func(context.Context, int64, string, uint) error { return nil })

type EpisodeShadowObserver interface {
	Observe(context.Context, int64, string, string) error
}
type EpisodeShadowObserverFunc func(context.Context, int64, string, string) error

func (f EpisodeShadowObserverFunc) Observe(ctx context.Context, userID int64, sessionID, question string) error {
	return f(ctx, userID, sessionID, question)
}

var DiscardEpisodeShadow EpisodeShadowObserver = EpisodeShadowObserverFunc(func(context.Context, int64, string, string) error { return nil })

type EpisodeSessionHistoryReader interface {
	HasSessionHistory(context.Context, int64, string) (bool, error)
}

type EpisodeSessionHistoryReaderFunc func(context.Context, int64, string) (bool, error)

func (f EpisodeSessionHistoryReaderFunc) HasSessionHistory(ctx context.Context, userID int64, sessionID string) (bool, error) {
	return f(ctx, userID, sessionID)
}

type DatabaseMessageStore struct{}

func (DatabaseMessageStore) Create(ctx context.Context, message *model.Message) error {
	_, err := messagedao.DefaultRepository().Create(ctx, message)
	return err
}

type Service struct {
	sessions       SessionRepository
	runs           RunRepository
	messages       MessageStore
	runner         *Runner
	memory         MemoryEventPublisher
	episodes       EpisodeEventPublisher
	shadow         EpisodeShadowObserver
	episodeHistory EpisodeSessionHistoryReader

	// leaseDuration and leaseHeartbeat are the run lease parameters. They are
	// fields rather than constants at the call site so the lease-loss path can
	// be driven in a test without waiting out a real ten second heartbeat; no
	// production caller changes them.
	leaseDuration  time.Duration
	leaseHeartbeat time.Duration
}

type RunRequest struct {
	UserID          int64
	UserName        string
	SessionID       string
	Question        string
	RequestID       string
	KnowledgePolicy rag.KnowledgePolicy
	EnableThinking  bool
	// TopicConsent is what the user authorised this answer to read.
	//
	// It is the raw client submission, not a decision: the ticket is verified
	// and the keys are re-checked against it before anything personal is read.
	// An absent or invalid consent is ordinary chat without topic
	// personalisation, never a reason to fail the turn.
	TopicConsent TopicConsentSubmission
}

// TopicConsentSubmission is the browser's half of the read-side confirmation.
type TopicConsentSubmission struct {
	// Ticket is the signed suggestion this selection belongs to.
	Ticket string `json:"ticket"`
	// SelectedTopicKeys are the topics the user ticked, at most two. An empty
	// slice with a valid ticket is a real answer - "use none of them" - and is
	// not the same as no ticket at all.
	SelectedTopicKeys []string `json:"selectedTopicKeys"`
}

// Empty reports whether the user submitted nothing at all.
func (s TopicConsentSubmission) Empty() bool {
	return strings.TrimSpace(s.Ticket) == "" && len(s.SelectedTopicKeys) == 0
}

type RunResult struct {
	RunID           string              `json:"runId"`
	Answer          string              `json:"answer"`
	ToolCalls       int                 `json:"toolCalls"`
	ModelCalls      int                 `json:"modelCalls"`
	Sources         []rag.Source        `json:"sources"`
	AnswerBasis     rag.AnswerBasis     `json:"answerBasis"`
	KnowledgeStatus rag.KnowledgeStatus `json:"knowledgeStatus"`
	KnowledgeReason rag.KnowledgeReason `json:"knowledgeReason"`
	ContextBudget   BudgetBreakdown     `json:"contextBudget"`
	MemoryDraft     *MemoryDraftCard    `json:"memoryDraft,omitempty"`
	Replayed        bool                `json:"replayed,omitempty"`
	// UsedTopicKeys names the topics whose personal state actually reached the
	// model, so the user can see what their confirmation did. It carries keys
	// and never note text: the answer already shows whatever it used.
	UsedTopicKeys []string `json:"usedTopicKeys,omitempty"`
}

func NewService(sessions SessionRepository, runs RunRepository, messages MessageStore, runner *Runner) (*Service, error) {
	if sessions == nil || runs == nil || messages == nil || runner == nil {
		return nil, fmt.Errorf("agent service dependencies must not be nil")
	}
	return &Service{
		sessions: sessions, runs: runs, messages: messages, runner: runner,
		memory: DiscardMemoryEvents, episodes: DiscardEpisodeEvents, shadow: DiscardEpisodeShadow,
		leaseDuration: defaultRunLeaseDuration, leaseHeartbeat: defaultRunHeartbeatInterval,
	}, nil
}

func (s *Service) SetMemoryPublisher(publisher MemoryEventPublisher) {
	if publisher == nil {
		publisher = DiscardMemoryEvents
	}
	s.memory = publisher
}

func (s *Service) SetEpisodePublisher(publisher EpisodeEventPublisher) {
	if publisher == nil {
		publisher = DiscardEpisodeEvents
	}
	s.episodes = publisher
}

func (s *Service) SetEpisodeShadowObserver(observer EpisodeShadowObserver) {
	if observer == nil {
		observer = DiscardEpisodeShadow
	}
	s.shadow = observer
}

// SetEpisodeSessionHistoryReader enables the pre-persistence history snapshot
// required by inject_enabled routing. It is intentionally separate from the
// Shadow observer so production runs never execute the same recall twice.
func (s *Service) SetEpisodeSessionHistoryReader(reader EpisodeSessionHistoryReader) {
	s.episodeHistory = reader
}

func NewDatabaseService(runner *Runner) (*Service, error) {
	return NewService(sessiondao.DefaultRepository(), agentdao.DefaultRepository(), DatabaseMessageStore{}, runner)
}

func (s *Service) Run(ctx context.Context, request RunRequest, sink EventSink) (RunResult, error) {
	if err := validateRunRequest(request); err != nil {
		return RunResult{}, err
	}
	if sink == nil {
		sink = DiscardEvents
	}
	owned, err := s.sessions.GetByIDAndUserID(ctx, request.SessionID, request.UserID)
	if err != nil {
		return RunResult{}, err
	}
	if owned.Mode != model.SessionModeAgent {
		return RunResult{}, ErrAgentSessionRequired
	}
	requestKey := strings.TrimSpace(request.RequestID)
	digestInput := request.Question
	if request.KnowledgePolicy != "" {
		digestInput += "\x00knowledge_policy=" + string(request.KnowledgePolicy)
	}
	digestInput += fmt.Sprintf("\x00enable_thinking=%t", request.EnableThinking)
	inputDigest := sha256.Sum256([]byte(digestInput))
	now := time.Now()
	leaseToken := uuid.NewString()
	leaseEnds := now.Add(defaultRunLeaseDuration)
	run := &model.AgentRun{
		ID: uuid.NewString(), UserID: owned.UserID, SessionID: owned.ID,
		ExecutionKind: model.AgentRunExecutionChat,
		Status:        model.AgentRunRunning, Version: 1, StartedAt: now,
		InputSummary: truncate(request.Question, 1000), RequestKey: &requestKey,
		InputHash: hex.EncodeToString(inputDigest[:]), LeaseToken: leaseToken,
		LeaseExpiresAt: &leaseEnds, HeartbeatAt: &now,
	}
	acquisition, err := s.runs.AcquireRun(ctx, run)
	if err != nil {
		err = normalizeAcquisitionError(err)
		result := RunResult{}
		if acquisition != nil && acquisition.Run != nil {
			result.RunID = acquisition.Run.ID
		}
		_ = sendAcquisitionFailure(ctx, sink, result.RunID, err)
		return result, err
	}
	if acquisition == nil || acquisition.Run == nil {
		return RunResult{}, fmt.Errorf("acquire agent run returned no run")
	}
	if !acquisition.Created && !acquisition.Resumed {
		return replayRun(ctx, sink, acquisition.Run)
	}
	run = acquisition.Run
	runCtx, keeper := startRunLeaseKeeper(ctx, s.runs, run, s.leaseDuration, s.leaseHeartbeat)
	defer keeper.Stop()
	currentSessionHasHistory := false
	episodeRecallReady := false
	if s.episodeHistory != nil {
		historyCtx, cancelHistory := context.WithTimeout(context.WithoutCancel(runCtx), 2*time.Second)
		currentSessionHasHistory, err = s.episodeHistory.HasSessionHistory(historyCtx, owned.UserID, owned.ID)
		cancelHistory()
		if err != nil {
			log.Printf("episode history snapshot failed: session=%s error_type=%T", owned.ID, err)
			currentSessionHasHistory = false
		} else {
			episodeRecallReady = true
		}
	}
	shadowCtx, cancelShadow := context.WithTimeout(context.WithoutCancel(runCtx), 2*time.Second)
	if err := s.shadow.Observe(shadowCtx, owned.UserID, owned.ID, request.Question); err != nil {
		log.Printf("episode shadow recall failed: session=%s error_type=%T", owned.ID, err)
	}
	cancelShadow()

	runID, userRole := run.ID, model.MessageAgentRunRoleUser
	if err := s.messages.Create(runCtx, &model.Message{
		SessionID: owned.ID, UserName: owned.UserName, Content: request.Question, IsUser: true,
		AgentRunID: &runID, AgentRunRole: &userRole,
		AnswerBasis: rag.AnswerBasisConversation, KnowledgeStatus: rag.KnowledgeStatusNotRequired, KnowledgeReason: rag.KnowledgeReasonNone,
	}); err != nil {
		return RunResult{RunID: run.ID}, s.failRun(runCtx, run, sink, "message_persistence_failed", err)
	}
	if err := sendEvent(runCtx, sink, Event{Type: EventRunStarted, RunID: run.ID}); err != nil {
		return RunResult{RunID: run.ID}, s.finishFailedRun(runCtx, run, sink, model.AgentRunCancelled, "client_disconnected", err)
	}

	runnerResult, runErr := s.runner.Run(runCtx, RunContext{
		RunID: run.ID, LeaseToken: run.LeaseToken, InitialStepSequence: acquisition.NextStepSequence,
		UserID: owned.UserID, UserName: owned.UserName,
		SessionID: owned.ID, Question: request.Question, KnowledgePolicy: request.KnowledgePolicy,
		EnableThinking:           request.EnableThinking,
		CurrentSessionHasHistory: currentSessionHasHistory, EpisodeRecallReady: episodeRecallReady,
		RequestID: request.RequestID, TopicConsent: request.TopicConsent,
	}, sink)
	if len(runnerResult.EpisodeIDs) > 0 {
		log.Printf("episode_context_injected run=%s session=%s episode_ids=%v tokens=%d", run.ID, run.SessionID, runnerResult.EpisodeIDs, runnerResult.ContextBudget.EpisodeMemory)
	}
	result := RunResult{
		RunID: run.ID, Answer: runnerResult.Answer,
		ToolCalls: runnerResult.ToolCalls, ModelCalls: runnerResult.ModelCalls,
		Sources:     append([]rag.Source{}, runnerResult.Sources...),
		AnswerBasis: runnerResult.AnswerBasis, KnowledgeStatus: runnerResult.KnowledgeStatus, KnowledgeReason: runnerResult.KnowledgeReason,
		ContextBudget: runnerResult.ContextBudget,
		MemoryDraft:   cloneMemoryDraftCard(runnerResult.MemoryDraft),
		UsedTopicKeys: append([]string{}, runnerResult.UsedTopicKeys...),
	}
	if leaseErr := keeper.Err(); leaseErr != nil {
		// A run whose lease ended must still end visibly. Returning here left
		// the row for the reaper to mark lease_expired and left the client with
		// a stream that simply stopped: in an S-LAT-2 load run every one of 32
		// killed runs reached the client as stream_ended, with no terminal event
		// and no error code to act on.
		return result, s.finishLeaseLostRun(runCtx, run, sink, keeper.Reason(), leaseErr)
	}
	if runErr != nil {
		reason, status := classifyRunError(runCtx, runErr)
		terminalErr := s.finishFailedRun(runCtx, run, sink, status, reason, runErr)
		publishCtx, cancelPublish := context.WithTimeout(context.WithoutCancel(runCtx), 2*time.Second)
		_ = s.memory.PublishAgentRun(publishCtx, run.UserID, run.SessionID, run.ID, request.Question, runnerResult.ToolCalls, runnerResult.MemoryDraft != nil)
		cancelPublish()
		return result, terminalErr
	}
	assistantMessage := &model.Message{
		SessionID: owned.ID, UserName: owned.UserName, Content: runnerResult.Answer, IsUser: false,
		Sources:        model.MessageSources(append([]rag.Source{}, runnerResult.Sources...)),
		EpisodeSources: append(model.EpisodeSourceRefs(nil), runnerResult.EpisodeSources...),
		AnswerBasis:    runnerResult.AnswerBasis, KnowledgeStatus: runnerResult.KnowledgeStatus, KnowledgeReason: runnerResult.KnowledgeReason,
	}
	encodedResult, err := json.Marshal(result)
	if err != nil {
		return result, s.failRun(runCtx, run, sink, "result_encoding_failed", err)
	}
	completed, persistedAssistant, err := s.runs.CompleteRun(runCtx, run.UserID, run.SessionID, run.ID, run.LeaseToken, run.Version, assistantMessage, truncate(runnerResult.Answer, 1000), string(encodedResult))
	if err != nil {
		return result, fmt.Errorf("complete agent run: %w", err)
	}
	run.Version = completed.Version
	keeper.Stop()
	episodeCtx, cancelEpisode := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	if err := s.episodes.PublishConversationCompleted(episodeCtx, run.UserID, run.SessionID, persistedAssistant.ID); err != nil {
		log.Printf("episode projection enqueue failed: session=%s error_type=%T", run.SessionID, err)
	}
	cancelEpisode()
	publishCtx, cancelPublish := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	_ = s.memory.PublishAgentRun(publishCtx, run.UserID, run.SessionID, run.ID, request.Question, runnerResult.ToolCalls, runnerResult.MemoryDraft != nil)
	cancelPublish()
	if err := sendEvent(ctx, sink, Event{
		Type: EventRunCompleted, RunID: run.ID, TerminationReason: "answered", UsedTopicKeys: append([]string{}, runnerResult.UsedTopicKeys...),
		Content:     runnerResult.Answer,
		Sources:     append([]rag.Source{}, runnerResult.Sources...),
		AnswerBasis: runnerResult.AnswerBasis, KnowledgeStatus: runnerResult.KnowledgeStatus, KnowledgeReason: runnerResult.KnowledgeReason,
		ContextBudget: &runnerResult.ContextBudget,
		MemoryDraft:   cloneMemoryDraftCard(runnerResult.MemoryDraft),
	}); err != nil {
		return result, err
	}
	return result, nil
}

func normalizeAcquisitionError(err error) error {
	if err == nil || errors.Is(err, ErrRunInProgress) || errors.Is(err, ErrRunRequestConflict) ||
		errors.Is(err, ErrRunRequestFinished) || errors.Is(err, ErrMessageLimitReached) ||
		errors.Is(err, ErrRunTemporarilyUnavailable) || errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("%w: %v", ErrRunTemporarilyUnavailable, err)
}

func (s *Service) failRun(ctx context.Context, run *model.AgentRun, sink EventSink, reason string, cause error) error {
	return s.finishFailedRun(ctx, run, sink, model.AgentRunFailed, reason, cause)
}

func (s *Service) finishFailedRun(
	ctx context.Context,
	run *model.AgentRun,
	sink EventSink,
	status model.AgentRunStatus,
	reason string,
	cause error,
) error {
	transitionCtx := context.WithoutCancel(ctx)
	transitionCtx, cancel := context.WithTimeout(transitionCtx, 3*time.Second)
	defer cancel()
	if _, err := s.runs.TransitionLeasedRun(
		transitionCtx, run.UserID, run.SessionID, run.ID, run.LeaseToken, run.Version,
		status, reason, "",
	); err != nil {
		return fmt.Errorf("agent run failed (%v); persist terminal state: %w", cause, err)
	}
	_ = sink.Send(transitionCtx, Event{
		Type: EventRunFailed, RunID: run.ID, Message: publicRunError(reason), TerminationReason: reason,
	})
	return cause
}

// finishLeaseLostRun ends a run whose lease could not be held.
//
// The terminal write is best effort and deliberately still fenced: it carries
// this executor's lease token and version, so if the run was genuinely taken
// over or already reaped, the write is refused rather than overwriting whatever
// the new owner recorded. The client event is not conditional on that write —
// whoever owns the row, this stream is over, and the caller waiting on it has
// to be told.
func (s *Service) finishLeaseLostRun(
	ctx context.Context,
	run *model.AgentRun,
	sink EventSink,
	reason string,
	cause error,
) error {
	transitionCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()

	if _, err := s.runs.TransitionLeasedRun(
		transitionCtx, run.UserID, run.SessionID, run.ID, run.LeaseToken, run.Version,
		model.AgentRunFailed, reason, "",
	); err != nil {
		// Expected when the lease really is gone: the row no longer matches this
		// token. Logged, not escalated — the run is over either way, and the
		// event below is what the client needs.
		log.Printf("[agent-lease] terminal state not written: run=%s reason=%s error_type=%T",
			run.ID, reason, err)
	}
	_ = sink.Send(transitionCtx, Event{
		Type: EventRunFailed, RunID: run.ID, Message: publicRunError(reason), TerminationReason: reason,
	})
	return fmt.Errorf("agent execution lease: %w", cause)
}

// classifyRunError is the service's view of a failed run: a reason string for
// the client and a status for the row.
//
// It reads ClassifyRunFailure rather than repeating its switch, so the
// evaluator and this path cannot drift into disagreeing about what an error
// was. The reason strings and statuses below are unchanged and deliberately so:
// they reach users and persisted rows, and STABILIZE-062 has no business
// moving them.
func classifyRunError(parent context.Context, err error) (string, model.AgentRunStatus) {
	switch ClassifyRunFailure(parent, err) {
	case RunFailureCancelled:
		return "cancelled", model.AgentRunCancelled
	case RunFailureTurnDeadline:
		return "timeout", model.AgentRunTimeout
	case RunFailureToolCallLimit:
		return "tool_call_limit", model.AgentRunFailed
	case RunFailureModelCallLimit:
		return "model_call_limit", model.AgentRunFailed
	case RunFailureRepeatedToolCall:
		return "repeated_tool_call", model.AgentRunFailed
	case RunFailureUnknownTool:
		return "unknown_tool", model.AgentRunFailed
	case RunFailureGraphStepLimit:
		return "graph_step_limit", model.AgentRunFailed
	case RunFailureIncompleteModelStream:
		return "incomplete_model_stream", model.AgentRunFailed
	default:
		return "runner_failed", model.AgentRunFailed
	}
}

func publicRunError(reason string) string {
	switch reason {
	case "cancelled", "client_disconnected":
		return "本次 Agent 运行已取消"
	case "timeout":
		return "本次 Agent 运行超时"
	case "tool_call_limit", "model_call_limit", "graph_step_limit", "repeated_tool_call", "unknown_tool":
		return "Agent 达到安全执行边界，本次任务未完成"
	case "incomplete_model_stream":
		return "模型响应中途结束，本次答案未保存，请重试"
	case runLeaseReasonLost, runLeaseReasonRenew:
		// Deliberately not "the server is broken": from the user's side this is
		// a run that has to be asked again, and the question itself was saved.
		return "本次 Agent 运行未能保持执行权，请重试"
	default:
		return "Agent 运行失败"
	}
}

func validateRunRequest(request RunRequest) error {
	if request.UserID <= 0 || strings.TrimSpace(request.UserName) == "" ||
		strings.TrimSpace(request.SessionID) == "" || strings.TrimSpace(request.Question) == "" ||
		!model.ValidRequestKey(request.RequestID) ||
		(request.KnowledgePolicy != "" && !request.KnowledgePolicy.Valid()) {
		return ErrInvalidRunRequest
	}
	return nil
}

func replayRun(ctx context.Context, sink EventSink, run *model.AgentRun) (RunResult, error) {
	var result RunResult
	if err := json.Unmarshal([]byte(run.ResultJSON), &result); err != nil {
		return RunResult{RunID: run.ID}, fmt.Errorf("decode replayable agent result: %w", err)
	}
	result.RunID = run.ID
	normalizeReplayedAnswerState(&result)
	result.Replayed = true
	if err := sendEvent(ctx, sink, Event{Type: EventRunStarted, RunID: run.ID}); err != nil {
		return result, err
	}
	if err := sendEvent(ctx, sink, Event{
		Type: EventRunCompleted, RunID: run.ID, TerminationReason: "answered", Content: result.Answer,
		UsedTopicKeys: append([]string{}, result.UsedTopicKeys...),
		Sources:       append([]rag.Source{}, result.Sources...), ContextBudget: &result.ContextBudget,
		AnswerBasis: result.AnswerBasis, KnowledgeStatus: result.KnowledgeStatus, KnowledgeReason: result.KnowledgeReason,
		MemoryDraft: cloneMemoryDraftCard(result.MemoryDraft),
	}); err != nil {
		return result, err
	}
	return result, nil
}

func normalizeReplayedAnswerState(result *RunResult) {
	if result == nil {
		return
	}
	if result.AnswerBasis.Valid() && result.KnowledgeStatus.Valid() && result.KnowledgeReason.Valid() {
		return
	}
	if len(result.Sources) > 0 {
		result.AnswerBasis = rag.AnswerBasisPersonalKnowledge
		result.KnowledgeStatus = rag.KnowledgeStatusGrounded
		result.KnowledgeReason = rag.KnowledgeReasonNone
		return
	}
	result.AnswerBasis = rag.AnswerBasisLegacyUnspecified
	result.KnowledgeStatus = rag.KnowledgeStatusLegacyUnspecified
	result.KnowledgeReason = rag.KnowledgeReasonNone
}

func sendAcquisitionFailure(ctx context.Context, sink EventSink, runID string, cause error) error {
	reason := "server_busy"
	message := "Agent 服务暂时繁忙，请稍后重试"
	if errors.Is(cause, ErrRunRequestConflict) {
		reason = "request_conflict"
		message = "该请求编号已被其他内容使用"
	} else if errors.Is(cause, ErrRunInProgress) {
		reason = "run_in_progress"
		message = "当前会话已有 Agent 任务正在执行"
	} else if errors.Is(cause, ErrRunRequestFinished) {
		reason = "request_finished"
		message = "该请求已经结束，不能重复执行"
	} else if errors.Is(cause, ErrMessageLimitReached) {
		reason = "message_limit_reached"
		message = "当前会话消息已达到上限，请新建会话继续"
	}
	return sink.Send(ctx, Event{Type: EventRunFailed, RunID: runID, Message: message, TerminationReason: reason})
}

var defaultService struct {
	sync.RWMutex
	service *Service
}

func SetDefaultService(service *Service) error {
	if service == nil {
		return fmt.Errorf("default agent service must not be nil")
	}
	defaultService.Lock()
	defaultService.service = service
	defaultService.Unlock()
	return nil
}

func DefaultService() (*Service, error) {
	defaultService.RLock()
	service := defaultService.service
	defaultService.RUnlock()
	if service == nil {
		return nil, ErrServiceNotConfigured
	}
	return service, nil
}
