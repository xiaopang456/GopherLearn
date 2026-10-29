package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/xiaopang456/GopherLearn/internal/agent"
	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/knowledge"
	"github.com/xiaopang456/GopherLearn/internal/model"
	"github.com/xiaopang456/GopherLearn/internal/openai"
	"github.com/xiaopang456/GopherLearn/internal/repository"
	"github.com/xiaopang456/GopherLearn/internal/repository/repositorytest"
	"github.com/xiaopang456/GopherLearn/internal/responder"
	"github.com/xiaopang456/GopherLearn/internal/service"
)

// The quiz workflow is the first consumer of Assembly.Model, and it is the reason the
// field exists at all: cmd/gopherlearn builds a LearningService only when the assembly
// says a provider is configured. That makes one value load-bearing for a claim the
// README makes about echo mode - a keyless start contacts nobody and offers no
// generated quizzes - so the two directions of that claim are pinned here.
//
// What these tests do NOT do is assert that the service is wired in main.go. The binary's
// own startup path needs a provider to reach, and this package's job stops at reporting
// honestly whether one exists.

func echoAssembly(t *testing.T) Assembly {
	t.Helper()
	config, err := ConfigFromEnvironment(lookup(nil))
	if err != nil {
		t.Fatal(err)
	}
	assembly, err := Assemble(knowledge.NewMemoryStore(), config, Options{})
	if err != nil {
		t.Fatal(err)
	}
	return assembly
}

func TestEchoAssemblyReportsNoModelSoTheQuizWorkflowStaysOff(t *testing.T) {
	assembly := echoAssembly(t)
	if assembly.Model != nil {
		t.Fatalf("echo mode handed out a model of type %T", assembly.Model)
	}
	if assembly.Summaries != nil {
		t.Fatal("echo mode handed out a summarizer it cannot honestly run")
	}
	// The refusal is not just a nil the caller might ignore: a LearningService built over
	// the echo assembly's nil model cannot produce a quiz. This is the line between
	// "main.go chose not to build it" and "nothing can build it". The store here is a real
	// one, so the refusal cannot be attributed to the missing store instead.
	quiz := service.NewLearningService(repository.NewInMemoryRepository(), assembly.Model, nil, nil, nil)
	if _, err := quiz.Start(t.Context(), "user-1", "session-1", "Redis 持久化", nil); !errors.Is(err, service.ErrLearningModelNotConfigured) {
		t.Fatalf("start over an echo model = %v, want the wiring refusal", err)
	}
}

func TestConfiguredAssemblyHandsOutTheModelItBuilt(t *testing.T) {
	store, _, _, _ := agentFixture(t)
	chat := &toolChat{replies: []model.Completion{{Content: "回答", FinishReason: model.FinishStop}}}
	config, err := ConfigFromEnvironment(lookup(openAIEnvironment()))
	if err != nil {
		t.Fatal(err)
	}
	assembly, err := Assemble(store, config, Options{NewModel: func(openai.Config) (model.ChatModel, error) {
		return chat, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	// Identity, not just non-nilness: the quiz workflow must share the client the answer
	// strategies use. A second client built beside the assembly would bill separately and
	// could be configured differently from the one the startup line advertised.
	if assembly.Model != model.ChatModel(chat) {
		t.Fatalf("assembly model = %p, want the configured client %p", assembly.Model, chat)
	}
	if assembly.Summaries == nil {
		t.Fatal("a configured provider should still maintain session summaries")
	}
}

// TestFailedAssemblyCarriesNoModel covers the other direction of the same rule: every
// error path returns a zero Assembly, so a caller that skipped the error check still sees
// "no model" rather than a half-built client.
func TestFailedAssemblyCarriesNoModel(t *testing.T) {
	store, _, _, _ := agentFixture(t)
	config, err := ConfigFromEnvironment(lookup(openAIEnvironment()))
	if err != nil {
		t.Fatal(err)
	}

	// No construction path at all.
	assembly, err := Assemble(store, config, Options{})
	if !errors.Is(err, ErrNoModelClient) {
		t.Fatalf("error = %v, want the missing-client refusal", err)
	}
	if assembly.Model != nil || assembly.Assistant != nil {
		t.Fatalf("a refused assembly still handed out pieces: %+v", assembly)
	}

	// A provider that cannot be built.
	wanted := errors.New("provider unavailable")
	assembly, err = Assemble(store, config, Options{NewModel: func(openai.Config) (model.ChatModel, error) {
		return nil, wanted
	}})
	if !errors.Is(err, wanted) {
		t.Fatalf("error = %v, want the constructor's own failure", err)
	}
	if assembly.Model != nil {
		t.Fatal("a failed constructor produced a model")
	}

	// A client that cannot carry tool calls: the harness refuses to start, and the model
	// it was built from must not leak out either - otherwise a deployment would offer
	// generated quizzes on the same provider the startup check just rejected.
	plain := &recordingChat{}
	assembly, err = Assemble(store, config, Options{NewModel: func(openai.Config) (model.ChatModel, error) {
		return plain, nil
	}})
	if !errors.Is(err, ErrNoToolCapableModel) {
		t.Fatalf("error = %v, want the tool-capability refusal", err)
	}
	if assembly.Model != nil {
		t.Fatalf("a provider too weak for the agent was still handed to callers: %T", assembly.Model)
	}
}

// TestAssemblyModelIsNotASecondAnswerPath pins what the field is not. Exposing a chat
// client next to the responder makes it tempting to answer a turn with it directly, which
// would skip strategy resolution, the citation rules and the context budget. The
// assembly's own answer surface must stay the responder's.
func TestAssemblyModelIsNotASecondAnswerPath(t *testing.T) {
	store, _, _, _ := agentFixture(t)
	chat := &toolChat{replies: []model.Completion{
		{FinishReason: model.FinishToolCalls, ToolCalls: []model.ToolCall{{ID: "c1", Name: KnowledgeSearchToolName, Arguments: `{"query":"AOF"}`}}},
		{Content: "可以选 always、everysec 或 no。", FinishReason: model.FinishStop},
	}}
	config, err := ConfigFromEnvironment(lookup(openAIEnvironment()))
	if err != nil {
		t.Fatal(err)
	}
	config.Agent = agent.Budget{}
	assembly, err := Assemble(store, config, Options{NewModel: func(openai.Config) (model.ChatModel, error) {
		return chat, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if assembly.Assistant == nil {
		t.Fatal("a configured assembly must still expose exactly one answer path")
	}
	// Assembling alone asked no one anything. A model that was called during startup would
	// mean the process pays for a turn nobody requested.
	if chat.calls != 0 || len(chat.requests) != 0 {
		t.Fatalf("construction spent %d tool calls and %d completions", chat.calls, len(chat.requests))
	}

	// One turn through the assistant runs the loop: two decision calls, both through the
	// tool-capable path. That is the whole budget the turn used, so the exposed Model is
	// doing what it exists for - letting a non-strategy feature ask the same client - and
	// is not a second route by which a turn gets answered.
	dispatcher, ok := assembly.Assistant.(*responder.StrategyResponder)
	if !ok {
		t.Fatalf("assistant is %T", assembly.Assistant)
	}
	if _, err := dispatcher.Respond(t.Context(), responder.Request{
		UserID: "user-1", Content: "AOF 的 fsync 策略可以选哪些？", AnswerStrategy: domain.AnswerStrategyAgent,
	}); err != nil {
		t.Fatal(err)
	}
	if chat.calls != 2 {
		t.Fatalf("one agent turn spent %d model calls, want the loop's two", chat.calls)
	}
	if len(chat.requests) != 0 {
		t.Fatalf("the agent used the tool-free completion path %d times", len(chat.requests))
	}
}

func contentsOfMessages(messages []model.Message) string {
	var builder strings.Builder
	for _, message := range messages {
		builder.WriteString(message.Content)
	}
	return builder.String()
}

// TestMasteryNoteReachesTheAgentPromptOnlyForItsOwnTopic is the whole chain in one place:
// graded answers become a projection, the projection becomes prose about the topic the
// learner is asking about, and that prose becomes the agent's system prompt - and none of
// it happens for a question about something else.
//
// The end-to-end shape matters because each link alone can look correct. A matcher that
// never matches and a harness that drops the note produce the same observable silence, and
// only a test that drives a turn through the production assembly can tell "the gate closed"
// apart from "the wire was cut".
func TestMasteryNoteReachesTheAgentPromptOnlyForItsOwnTopic(t *testing.T) {
	ctx := context.Background()
	store, ingest, _, _ := agentFixture(t)
	chat := &toolChat{replies: []model.Completion{
		{Content: "先说结论，再补一个反例。", FinishReason: model.FinishStop},
		{Content: "分区迁移由控制器安排。", FinishReason: model.FinishStop},
	}}
	assistant, _ := newAgentAssistant(t, store, chat, agent.Budget{})
	repo, userID, sessionID := agentTranscript(t)

	quiz, questions := repositorytest.OpenLearningQuiz(t, repo, userID, sessionID, "Redis 持久化")
	for index := range domain.LearningQuestionCount {
		repositorytest.AnswerLearningQuestion(t, repo, quiz, questions[index],
			"sub-prompt-"+string(rune('a'+index)),
			domain.LearningGrade{Correctness: 40, Completeness: 15, Clarity: 9,
				Feedback: "答到了主要机制。", MissingPoints: []string{"没说出重写触发条件"}, Confidence: 0.8})
	}
	// The reader consults its store and nothing else, so every other collaborator is nil
	// here on purpose: a note that needed retrieval or a model to build would fail loudly.
	learning := service.NewLearningService(repo, nil, nil, nil, nil)
	messages := NewMessageService(repo, assistant, ingest, service.ContextBudget{}.Normalize(),
		service.WithMasteryContext(learning))

	if _, err := messages.Send(ctx, userID, sessionID, "Redis 持久化里 RDB 和 AOF 各会失去什么？", domain.KnowledgeOptions{}); err != nil {
		t.Fatal(err)
	}
	noted := chat.toolRequest[0].Messages[0]
	if noted.Role != model.RoleSystem || noted.Content != agent.SystemPrompt {
		t.Fatalf("a turn about the practised topic changed the policy prompt:\n%q", noted.Content)
	}
	if !strings.Contains(contentsOfMessages(chat.toolRequest[0].Messages), "Redis 持久化") {
		t.Fatalf("the mastery block never named its own topic:\n%q", chat.toolRequest[0].Messages)
	}

	// A question about a subject nobody drilled must reach the model with exactly the
	// constant system prompt. Not "a shorter note" - the prompt is the one an agent with
	// no history would get, so an unmatched question cannot be told apart from a new
	// account by anything downstream.
	if _, err := messages.Send(ctx, userID, sessionID, "Kafka 的分区再平衡是怎么触发的？", domain.KnowledgeOptions{}); err != nil {
		t.Fatal(err)
	}
	quiet := chat.toolRequest[1].Messages[0]
	if quiet.Content != agent.SystemPrompt {
		t.Fatalf("an off-topic question got a mastery note:\n%s", quiet.Content)
	}
	// One request per turn, both of them: matching a topic is lexical, so a question that
	// matches nothing costs no call, and a question that matches costs no extra one.
	if len(chat.toolRequest) != 2 || chat.calls != 2 {
		t.Fatalf("the two turns spent %d requests and %d calls, want one each", len(chat.toolRequest), chat.calls)
	}
}
