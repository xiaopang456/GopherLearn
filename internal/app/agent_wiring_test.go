package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/agent"
	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/knowledge"
	"github.com/xiaopang456/GopherLearn/internal/model"
	"github.com/xiaopang456/GopherLearn/internal/openai"
	"github.com/xiaopang456/GopherLearn/internal/repository"
	"github.com/xiaopang456/GopherLearn/internal/responder"
	"github.com/xiaopang456/GopherLearn/internal/service"
	"github.com/xiaopang456/GopherLearn/internal/summary"
)

// toolChat is a provider stand-in that can be offered tools. It answers the first
// decision call with a tool request and every later call with a finished answer, which
// is the smallest script that exercises a whole loop.
type toolChat struct {
	recordingChat
	replies     []model.Completion
	calls       int
	seenTools   [][]model.ToolDefinition
	toolRequest []model.Request
	failSecond  bool
}

func (c *toolChat) CompleteWithTools(ctx context.Context, request model.Request) (model.Completion, error) {
	if err := ctx.Err(); err != nil {
		return model.Completion{}, err
	}
	c.calls++
	c.seenTools = append(c.seenTools, request.Tools)
	// The transcript of each decision call is recorded separately: `requests` counts the
	// plain completion path, and a test asserts it stays empty for an agent turn.
	c.toolRequest = append(c.toolRequest, request)
	if c.failSecond && c.calls > 1 {
		return model.Completion{}, model.ErrUnavailable
	}
	index := c.calls - 1
	if index >= len(c.replies) {
		index = len(c.replies) - 1
	}
	return c.replies[index], nil
}

var _ model.ToolChatModel = (*toolChat)(nil)

func searchReply(query string) model.Completion {
	return model.Completion{FinishReason: model.FinishToolCalls, ToolCalls: []model.ToolCall{{
		ID: "call-1", Name: KnowledgeSearchToolName, Arguments: `{"query":"` + query + `"}`,
	}}}
}

// KnowledgeSearchToolName duplicates the constant here rather than importing it, so a
// rename inside the agent package has to be noticed by two files.
const KnowledgeSearchToolName = "knowledge_search"

func agentFixture(t *testing.T) (knowledge.Store, *service.KnowledgeService, string, string) {
	t.Helper()
	ctx := context.Background()
	store := knowledge.NewMemoryStore()
	ingest := service.NewKnowledgeService(store)
	owned, err := store.CreateCollection(ctx, &domain.Collection{UserID: "user-1", Title: "Redis 手册"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ingest.AddDocument(ctx, "user-1", owned.ID, service.DocumentInput{
		Title: "aof.md", Format: string(domain.DocumentFormatMarkdown),
		Content: "# AOF 持久化\n\nAOF 的 fsync 策略可以选 always、everysec 或 no。\n",
	}); err != nil {
		t.Fatal(err)
	}
	foreign, err := store.CreateCollection(ctx, &domain.Collection{UserID: "user-2", Title: "别人的手册"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ingest.AddDocument(ctx, "user-2", foreign.ID, service.DocumentInput{
		Title: "private.md", Format: string(domain.DocumentFormatMarkdown),
		Content: "# 别人的 AOF\n\n别人的 fsync 策略同样是 always、everysec 或 no。\n",
	}); err != nil {
		t.Fatal(err)
	}
	return store, ingest, owned.ID, foreign.ID
}

func newAgentAssistant(t *testing.T, store knowledge.Store, chat model.ToolChatModel, budget agent.Budget) (responder.Responder, string) {
	t.Helper()
	config, err := ConfigFromEnvironment(lookup(openAIEnvironment()))
	if err != nil {
		t.Fatal(err)
	}
	config.Agent = budget
	assistant, summary, err := New(store, config, Options{NewModel: func(openai.Config) (model.ChatModel, error) {
		return chat, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	return assistant, summary
}

func agentTranscript(t *testing.T) (repository.Repository, string, string) {
	t.Helper()
	ctx := context.Background()
	repo := repository.NewInMemoryRepository()
	user, err := repo.CreateUser(ctx, &domain.User{Email: "learner@example.test", DisplayName: "learner", PasswordHash: "hash-not-used"})
	if err != nil {
		t.Fatal(err)
	}
	session, err := repo.CreateSession(ctx, &domain.Session{UserID: user.ID, Title: "学习会话", Mode: domain.SessionModeLearningAgent})
	if err != nil {
		t.Fatal(err)
	}
	return repo, user.ID, session.ID
}

func TestOpenAIWithAToolCapableModelInstallsTheHarness(t *testing.T) {
	store, _, _, _ := agentFixture(t)
	chat := &toolChat{replies: []model.Completion{
		searchReply("AOF fsync 策略"),
		{Content: "可以选 always、everysec 或 no。", FinishReason: model.FinishStop},
	}}
	assistant, summary := newAgentAssistant(t, store, chat, agent.Budget{})

	if !strings.Contains(summary, "agent=harness") || !strings.Contains(summary, KnowledgeSearchToolName) {
		t.Fatalf("startup summary does not say what the agent will do: %q", summary)
	}
	for _, want := range []string{"max_model_calls=4", "max_tool_calls=3", "max_steps=10", "deadline=45s"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("summary omits %q: %s", want, summary)
		}
	}
	dispatcher, ok := assistant.(*responder.StrategyResponder)
	if !ok {
		t.Fatalf("assistant is %T", assistant)
	}
	// An agent turn with no authenticated identity is refused by the harness itself. If
	// the old placeholder were still installed, this would come back as an echo of the
	// question instead.
	_, err := dispatcher.Respond(context.Background(), responder.Request{
		Content: "问题", AnswerStrategy: domain.AnswerStrategyAgent,
	})
	if !errors.Is(err, agent.ErrNoPrincipal) {
		t.Fatalf("error = %v, want the harness's own refusal", err)
	}
	// And a legitimate one runs the loop, including a tool call.
	if _, err := dispatcher.Respond(context.Background(), responder.Request{
		UserID: "user-1", Content: "AOF 的 fsync 策略可以选哪些？", AnswerStrategy: domain.AnswerStrategyAgent,
	}); err != nil {
		t.Fatal(err)
	}
	if chat.calls != 2 {
		t.Fatalf("the agent turn spent %d model calls, want the loop's two", chat.calls)
	}
	if len(chat.seenTools) != 2 || len(chat.seenTools[0]) != 1 || chat.seenTools[0][0].Name != KnowledgeSearchToolName {
		t.Fatalf("the loop did not offer the registered tool: %+v", chat.seenTools)
	}
}

// TestAgentTurnThroughTheProductionAssembly is the end-to-end proof of this phase: a
// learning_agent session in openai mode runs the loop, searches only the caller's
// collection, and stores the answer with the citations the tool earned.
func TestAgentTurnThroughTheProductionAssembly(t *testing.T) {
	ctx := context.Background()
	store, ingest, ownedID, _ := agentFixture(t)
	chat := &toolChat{replies: []model.Completion{
		searchReply("AOF fsync 策略"),
		{Content: "AOF 的 fsync 可以选 always、everysec 或 no。", FinishReason: model.FinishStop},
	}}
	assistant, _ := newAgentAssistant(t, store, chat, agent.Budget{})
	repo, userID, sessionID := agentTranscript(t)
	messages := NewMessageService(repo, assistant, ingest, service.ContextBudget{}.Normalize())

	pair, err := messages.Send(ctx, userID, sessionID, "AOF 的 fsync 策略可以选哪些？", domain.KnowledgeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if pair.Assistant.Content != "AOF 的 fsync 可以选 always、everysec 或 no。" {
		t.Fatalf("answer = %q", pair.Assistant.Content)
	}
	if len(pair.Assistant.Sources) == 0 {
		t.Fatalf("a turn whose tool retrieved evidence stored no citations: %+v", pair.Assistant)
	}
	for _, source := range pair.Assistant.Sources {
		if source.CollectionID != ownedID {
			t.Fatalf("citation points outside the caller's collection: %+v", source)
		}
		if source.SourceID != "src-"+source.ChunkID {
			t.Fatalf("citation is not derived from a retrieved chunk: %+v", source)
		}
	}
	if chat.calls != 2 {
		t.Fatalf("the turn spent %d model calls, want one decision and one answer", chat.calls)
	}
	// Citations survive a history read, exactly as a grounded chat turn's do.
	stored, err := repo.ListMessages(ctx, userID, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 2 || len(stored[1].Sources) != len(pair.Assistant.Sources) {
		t.Fatalf("history lost the agent's citations: %+v", stored)
	}
}

// TestAgentStreamAndSendAgreeThroughTheService checks the SSE face of the same turn:
// the client gets the same final content, the citations ride the terminal event, and
// nothing from inside the loop is presented as answer text.
func TestAgentStreamAndSendAgreeThroughTheService(t *testing.T) {
	ctx := context.Background()
	store, ingest, _, _ := agentFixture(t)
	script := func() *toolChat {
		return &toolChat{replies: []model.Completion{
			searchReply("AOF fsync 策略"),
			{Content: "AOF 的 fsync 可以选 always、everysec 或 no。", FinishReason: model.FinishStop},
		}}
	}

	syncChat := script()
	syncAssistant, _ := newAgentAssistant(t, store, syncChat, agent.Budget{})
	syncRepo, syncUser, syncSession := agentTranscript(t)
	sent, err := NewMessageService(syncRepo, syncAssistant, ingest, service.ContextBudget{}.Normalize()).
		Send(ctx, syncUser, syncSession, "AOF 的 fsync 策略可以选哪些？", domain.KnowledgeOptions{})
	if err != nil {
		t.Fatal(err)
	}

	streamChat := script()
	streamAssistant, _ := newAgentAssistant(t, store, streamChat, agent.Budget{})
	streamRepo, streamUser, streamSession := agentTranscript(t)
	var events []service.StreamEvent
	var deltas []string
	if err := NewMessageService(streamRepo, streamAssistant, ingest, service.ContextBudget{}.Normalize()).
		Stream(ctx, streamUser, streamSession, "AOF 的 fsync 策略可以选哪些？", domain.KnowledgeOptions{}, func(event service.StreamEvent) error {
			events = append(events, event)
			if event.Name == "message.delta" {
				deltas = append(deltas, event.Delta)
			}
			return nil
		}); err != nil {
		t.Fatal(err)
	}

	if strings.Join(deltas, "") != sent.Assistant.Content {
		t.Fatalf("the streamed deltas say something else: %q vs %q", strings.Join(deltas, ""), sent.Assistant.Content)
	}
	if events[0].Name != "message.started" {
		t.Fatalf("first event = %q", events[0].Name)
	}
	last := events[len(events)-1]
	if last.Name != "message.completed" || last.Content != sent.Assistant.Content || len(last.Sources) == 0 {
		t.Fatalf("terminal event = %+v", last)
	}
	for _, event := range events[:len(events)-1] {
		if event.Name == "message.delta" && (strings.Contains(event.Delta, "fsync 策略可以选哪些？") && len(event.Delta) > 16) {
			t.Fatalf("a delta carried more than a chunk: %q", event.Delta)
		}
		if event.Name == "message.delta" && (strings.Contains(event.Delta, agent.ObservationLabel) || strings.Contains(event.Delta, KnowledgeSearchToolName)) {
			t.Fatalf("tool internals leaked into a delta: %q", event.Delta)
		}
	}
	if syncChat.calls != streamChat.calls {
		t.Fatalf("SSE spent %d calls where the turn spent %d", streamChat.calls, syncChat.calls)
	}
}

func TestAgentTurnCannotReachAnotherUsersCollection(t *testing.T) {
	ctx := context.Background()
	store, ingest, _, foreignID := agentFixture(t)
	chat := &toolChat{replies: []model.Completion{
		{FinishReason: model.FinishToolCalls, ToolCalls: []model.ToolCall{{
			ID: "call-1", Name: KnowledgeSearchToolName, Arguments: `{"query":"fsync","collection_ids":["` + foreignID + `"]}`,
		}}},
		{Content: "我读到了别人的资料。", FinishReason: model.FinishStop},
	}}
	assistant, _ := newAgentAssistant(t, store, chat, agent.Budget{})
	repo, userID, sessionID := agentTranscript(t)

	if _, err := NewMessageService(repo, assistant, ingest, service.ContextBudget{}.Normalize()).
		Send(ctx, userID, sessionID, "AOF 的 fsync 策略", domain.KnowledgeOptions{}); !errors.Is(err, service.ErrInternal) {
		t.Fatalf("error = %v, want the turn to fail rather than answer", err)
	}
	stored, err := repo.ListMessages(ctx, userID, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	// The question stays as fact; the answer that would have leaked never existed.
	if len(stored) != 1 || stored[0].Role != "user" {
		t.Fatalf("a failed agent turn wrote: %+v", stored)
	}
}

// TestAgentTurnRefusesASummaryBoundaryThatNoLongerExists is the agent face of the same
// integrity rule, checked through the production assembly rather than a stand-in
// responder: the summarizer and the loop share one recording model, so a zero count on
// both of its entry points proves the turn asked the provider for nothing at all - not
// a summary, and not an answer out of a context the service cannot describe.
func TestAgentTurnRefusesASummaryBoundaryThatNoLongerExists(t *testing.T) {
	ctx := context.Background()
	store, ingest, _, _ := agentFixture(t)
	repo, userID, sessionID := agentTranscript(t)
	for index := 1; index <= 3; index++ {
		for _, entry := range []struct{ role, content string }{
			{"user", "旧问题"}, {"assistant", "旧回答"},
		} {
			if _, err := repo.AppendMessage(ctx, userID, sessionID, &domain.Message{Role: entry.role, Content: entry.content}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := repo.CommitSessionSummary(ctx, &domain.SessionSummary{
		SessionID: sessionID, UserID: userID, Content: "边界已丢失", CoveredMessageID: "message-deleted-long-ago",
	}, 0); err != nil {
		t.Fatal(err)
	}

	chat := &toolChat{replies: []model.Completion{
		searchReply("AOF fsync 策略"),
		{Content: "可以选 always、everysec 或 no。", FinishReason: model.FinishStop},
	}}
	assistant, _ := newAgentAssistant(t, store, chat, agent.Budget{})
	// A policy under which a refresh is due, so "no call" cannot be explained away by
	// the threshold: the boundary is checked before any model is asked.
	policy := summary.Policy{TriggerMessages: 4, KeepRecentMessages: 2}
	summarizer, err := summary.NewModelSummarizer(chat, policy)
	if err != nil {
		t.Fatal(err)
	}
	messages := NewMessageService(repo, assistant, ingest, service.ContextBudget{}.Normalize(),
		service.WithRollingSummary(summarizer, policy))

	if _, err := messages.Send(ctx, userID, sessionID, "AOF 的 fsync 策略可以选哪些？", domain.KnowledgeOptions{}); !errors.Is(err, service.ErrSummaryBoundaryMissing) {
		t.Fatalf("error = %v, want the boundary-missing integrity failure", err)
	}
	if chat.calls != 0 || len(chat.requests) != 0 {
		t.Fatalf("the loop made %d tool-decision calls and plain completions were %d, want 0 and 0", chat.calls, len(chat.requests))
	}
	stored, err := repo.ListMessages(ctx, userID, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 7 || stored[len(stored)-1].Role != "user" {
		t.Fatalf("stored = %+v, want six seeded messages plus the unanswered question", stored)
	}
}

func TestEchoModeKeepsTheAgentOnThePlaceholder(t *testing.T) {
	store, _, _, _ := agentFixture(t)
	config, err := ConfigFromEnvironment(lookup(nil))
	if err != nil {
		t.Fatal(err)
	}
	chat := &toolChat{replies: []model.Completion{{Content: "不该被调用"}}}
	assistant, summary, err := New(store, config, Options{NewModel: func(openai.Config) (model.ChatModel, error) {
		return chat, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(summary, "provider=echo") {
		t.Fatalf("summary = %q", summary)
	}
	answer, err := assistant.Respond(context.Background(), responder.Request{
		UserID: "user-1", Content: "问题", AnswerStrategy: domain.AnswerStrategyAgent,
	})
	if err != nil {
		t.Fatal(err)
	}
	if answer.Content != "问题" || len(answer.Sources) != 0 {
		t.Fatalf("echo mode changed the agent answer: %+v", answer)
	}
	if chat.calls != 0 {
		t.Fatal("echo mode reached a model")
	}
}

// TestOpenAIWithANonToolModelRefusesToStart is the no-silent-downgrade rule: a process
// that claims a real provider must not answer learning questions with an echoed
// placeholder because its model cannot carry tool calls.
func TestOpenAIWithANonToolModelRefusesToStart(t *testing.T) {
	store, _, _, _ := agentFixture(t)
	config, err := ConfigFromEnvironment(lookup(openAIEnvironment()))
	if err != nil {
		t.Fatal(err)
	}
	chat := &recordingChat{}
	assistant, summary, err := New(store, config, Options{NewModel: func(openai.Config) (model.ChatModel, error) {
		return chat, nil
	}})
	if !errors.Is(err, ErrNoToolCapableModel) {
		t.Fatalf("error = %v, want the stable startup refusal", err)
	}
	if assistant != nil || summary != "" {
		t.Fatalf("a refused wiring still returned an assistant: %T %q", assistant, summary)
	}
	if len(chat.requests) != 0 {
		t.Fatal("the refused model was used anyway")
	}
}

func TestAgentBudgetComesFromTheEnvironment(t *testing.T) {
	values := map[string]string{
		EnvAgentMaxModelCalls: "3",
		EnvAgentMaxToolCalls:  "2",
		EnvAgentMaxSteps:      "5",
		EnvAgentDeadline:      "90s",
	}
	config, err := ConfigFromEnvironment(lookup(values))
	if err != nil {
		t.Fatal(err)
	}
	if config.Agent.MaxModelCalls != 3 || config.Agent.MaxToolCalls != 2 || config.Agent.MaxSteps != 5 ||
		config.Agent.Deadline != 90*time.Second {
		t.Fatalf("budget = %+v", config.Agent)
	}

	// An absurd request is clamped rather than honored: this is the one knob that
	// decides how much a single message can cost.
	clamped, err := ConfigFromEnvironment(lookup(map[string]string{EnvAgentMaxModelCalls: "100000"}))
	if err != nil {
		t.Fatal(err)
	}
	if clamped.Agent.MaxModelCalls != agent.MaxModelCallsCeiling {
		t.Fatalf("model-call ceiling not applied: %+v", clamped.Agent)
	}

	for _, bad := range []map[string]string{
		{EnvAgentMaxModelCalls: "zero"},
		{EnvAgentMaxToolCalls: "-1"},
		{EnvAgentMaxSteps: "0"},
		{EnvAgentDeadline: "soon"},
		{EnvAgentDeadline: "-5s"},
	} {
		merged := openAIEnvironment()
		for key, value := range bad {
			merged[key] = value
		}
		if _, err := ConfigFromEnvironment(lookup(merged)); err == nil {
			t.Fatalf("agent budget %v was accepted", bad)
		}
	}
}

// TestChatStrategiesAreUntouchedByTheAgent checks the "keep existing behaviour" half of
// this phase at the assembly level: a chat turn still goes through the plain, tool-free
// completion path, and a grounded turn still retrieves and cites as it did.
func TestChatStrategiesAreUntouchedByTheAgent(t *testing.T) {
	ctx := context.Background()
	store, _, ownedID, _ := agentFixture(t)
	config, err := ConfigFromEnvironment(lookup(openAIEnvironment()))
	if err != nil {
		t.Fatal(err)
	}
	chat := &toolChat{replies: []model.Completion{{Content: "模型回答", FinishReason: model.FinishStop}}}
	assistant, _, err := New(store, config, Options{NewModel: func(openai.Config) (model.ChatModel, error) {
		return chat, nil
	}})
	if err != nil {
		t.Fatal(err)
	}

	direct, err := assistant.Respond(ctx, responder.Request{
		UserID: "user-1", Content: "随便聊聊", AnswerStrategy: domain.AnswerStrategyDirectChat,
	})
	if err != nil {
		t.Fatal(err)
	}
	if direct.Content != "模型回答" || len(direct.Sources) != 0 {
		t.Fatalf("direct answer = %+v", direct)
	}
	grounded, err := assistant.Respond(ctx, responder.Request{
		UserID: "user-1", Content: "AOF 的 fsync 策略可以选哪些？", AnswerStrategy: domain.AnswerStrategyKnowledgeQA,
		KnowledgeEnabled: true, KnowledgeCollectionIDs: []string{ownedID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if grounded.Content != "模型回答" || len(grounded.Sources) == 0 {
		t.Fatalf("grounded answer = %+v", grounded)
	}
	if grounded.Sources[0].CollectionID != ownedID {
		t.Fatalf("grounded citation = %+v", grounded.Sources)
	}
	// Neither chat call was offered a tool: only the agent loop asks for that, and a
	// chat prompt that carried tool definitions would change what the model says.
	for index, request := range chat.requests {
		if len(request.Tools) != 0 {
			t.Fatalf("chat call %d was offered %d tools", index+1, len(request.Tools))
		}
	}
	if len(chat.seenTools) != 0 {
		t.Fatalf("the tool path was used %d times by chat turns", len(chat.seenTools))
	}
}
