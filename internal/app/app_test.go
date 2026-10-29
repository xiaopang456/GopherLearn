package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/knowledge"
	"github.com/xiaopang456/GopherLearn/internal/model"
	"github.com/xiaopang456/GopherLearn/internal/openai"
	"github.com/xiaopang456/GopherLearn/internal/responder"
	"github.com/xiaopang456/GopherLearn/internal/service"
)

const unitTestKey = "sk-startup-test-key-3f9a"

// recordingChat is the provider stand-in for the wiring tests: it proves which
// strategies reach a model, and what they send.
type recordingChat struct {
	requests []model.Request
}

func (c *recordingChat) Complete(_ context.Context, request model.Request) (model.Completion, error) {
	c.requests = append(c.requests, request)
	return model.Completion{Content: "模型回答", FinishReason: model.FinishStop}, nil
}

func (c *recordingChat) Stream(ctx context.Context, request model.Request, onDelta model.DeltaHandler) (model.Completion, error) {
	c.requests = append(c.requests, request)
	if err := onDelta("模型"); err != nil {
		return model.Completion{}, err
	}
	return model.Completion{Content: "模型", FinishReason: model.FinishStop}, nil
}

func openAIEnvironment() map[string]string {
	return map[string]string{
		EnvProvider:       "openai_compatible",
		openai.EnvBaseURL: "https://provider.invalid/v1",
		openai.EnvAPIKey:  unitTestKey,
		openai.EnvModel:   "startup-test-model",
	}
}

func lookup(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func TestAIProviderDefaultsToEchoAndNeedsNoKey(t *testing.T) {
	config, err := ConfigFromEnvironment(lookup(nil))
	if err != nil {
		t.Fatalf("a keyless environment refused to start: %v", err)
	}
	if config.Provider != ProviderEcho {
		t.Fatalf("provider = %q, want the echo default", config.Provider)
	}
	if config.Model.APIKey != "" || config.Model.Model != "" {
		t.Fatalf("echo mode pulled provider settings: %#v", config.Model)
	}
	if config.Context.MaxMessages != service.DefaultContextMaxMessages || config.Context.MaxRunes != service.DefaultContextMaxRunes {
		t.Fatalf("context budget = %#v, want the documented defaults", config.Context)
	}
	// An explicitly chosen echo is the same thing, and a stray key must not
	// promote the process into calling anyone.
	withStrayKey := map[string]string{EnvProvider: "echo", openai.EnvAPIKey: unitTestKey, openai.EnvBaseURL: "https://provider.invalid"}
	config, err = ConfigFromEnvironment(lookup(withStrayKey))
	if err != nil || config.Provider != ProviderEcho {
		t.Fatalf("explicit echo configuration = (%#v, %v)", config, err)
	}
}

func TestOpenAIModeRefusesToStartOnItsOwnMisconfiguration(t *testing.T) {
	for _, missing := range []string{openai.EnvBaseURL, openai.EnvAPIKey, openai.EnvModel} {
		values := openAIEnvironment()
		delete(values, missing)
		if _, err := ConfigFromEnvironment(lookup(values)); err == nil {
			t.Fatalf("openai_compatible mode started without %s", missing)
		}
	}
	for _, value := range []string{"openAI", "ollama", "echo ", "provider", ""} {
		values := openAIEnvironment()
		values[EnvProvider] = value
		config, err := ConfigFromEnvironment(lookup(values))
		switch strings.TrimSpace(value) {
		case "":
			// Unset or blank provider keeps the safe default.
			if err != nil || config.Provider != ProviderEcho {
				t.Fatalf("blank provider = (%#v, %v), want echo", config, err)
			}
		case "echo", "openai_compatible":
			if err != nil {
				t.Fatalf("provider %q rejected: %v", value, err)
			}
		default:
			if !errors.Is(err, ErrUnsupportedProvider) {
				t.Fatalf("provider %q error = %v, want ErrUnsupportedProvider", value, err)
			}
		}
	}
	// Case and surrounding space are tolerated, because operators type these into
	// a shell.
	values := openAIEnvironment()
	values[EnvProvider] = "  OpenAI_Compatible  "
	if config, err := ConfigFromEnvironment(lookup(values)); err != nil || config.Provider != ProviderOpenAI {
		t.Fatalf("normalized provider = (%#v, %v)", config, err)
	}
}

func TestContextBudgetComesFromTheEnvironment(t *testing.T) {
	values := openAIEnvironment()
	values[EnvContextMaxMessages] = "3"
	values[EnvContextMaxRunes] = "800"
	config, err := ConfigFromEnvironment(lookup(values))
	if err != nil {
		t.Fatal(err)
	}
	if config.Context.MaxMessages != 3 || config.Context.MaxRunes != 800 {
		t.Fatalf("budget = %#v, want the configured 3/800", config.Context)
	}
	for _, bad := range []map[string]string{
		{EnvContextMaxMessages: "0"},
		{EnvContextMaxMessages: "-2"},
		{EnvContextMaxRunes: "many"},
		{EnvContextMaxRunes: "0"},
	} {
		values := openAIEnvironment()
		for key, value := range bad {
			values[key] = value
		}
		if _, err := ConfigFromEnvironment(lookup(values)); !errors.Is(err, ErrInvalidContextLimit) {
			t.Fatalf("%v error = %v, want ErrInvalidContextLimit", bad, err)
		}
	}
}

func TestNewAssistantEchoModeKeepsEveryPlaceholder(t *testing.T) {
	store := knowledge.NewMemoryStore()
	assistant, summary, err := New(store, Config{Provider: ProviderEcho, Context: service.ContextBudget{}.Normalize()}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(summary, "provider=echo") || strings.Contains(summary, unitTestKey) {
		t.Fatalf("summary = %q", summary)
	}
	direct, err := assistant.Respond(context.Background(), responder.Request{
		Content: "问题", AnswerStrategy: domain.AnswerStrategyDirectChat,
	})
	if err != nil || direct.Content != "问题" {
		t.Fatalf("echo direct answer = (%#v, %v), want the unchanged placeholder", direct, err)
	}
}

// TestNewAssistantOpenAIUsesOneSharedChatModel is the装配 contract: both Chat
// strategies go through the same configured model, the agent strategy stays on
// echo, and the knowledge path keeps retrieving with citations attached by the
// program.
func TestNewAssistantOpenAIUsesOneSharedChatModel(t *testing.T) {
	store := knowledge.NewMemoryStore()
	ctx := context.Background()
	owned, err := store.CreateCollection(ctx, &domain.Collection{UserID: "user-1", Title: "手册"})
	if err != nil {
		t.Fatal(err)
	}
	chat := &toolChat{replies: []model.Completion{{Content: "学习回答", FinishReason: model.FinishStop}}}
	config, err := ConfigFromEnvironment(lookup(openAIEnvironment()))
	if err != nil {
		t.Fatal(err)
	}
	assistant, summary, err := New(store, config, Options{
		NewModel: func(openai.Config) (model.ChatModel, error) { return chat, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(summary, "startup-test-model") || strings.Contains(summary, unitTestKey) ||
		!strings.Contains(summary, "agent=harness") {
		t.Fatalf("summary = %q, want the model name, an honest agent note and no key", summary)
	}

	if _, err := assistant.Respond(ctx, responder.Request{
		UserID: "user-1", Content: "问题", History: []responder.Turn{{Role: "user", Content: "上一问"}},
		AnswerStrategy: domain.AnswerStrategyDirectChat,
	}); err != nil {
		t.Fatal(err)
	}
	if len(chat.requests) != 1 {
		t.Fatalf("direct_chat reached the model %d times", len(chat.requests))
	}
	first := chat.requests[0]
	if len(first.Messages) != 3 || first.Messages[0].Role != model.RoleSystem {
		t.Fatalf("direct prompt = %#v, want system + history + question", first.Messages)
	}

	// The agent strategy is no longer a placeholder: it runs the loop, which asks the
	// model through the tool-calling path rather than the plain completion path.
	agentAnswer, err := assistant.Respond(ctx, responder.Request{
		UserID: "user-1", Content: "任务", AnswerStrategy: domain.AnswerStrategyAgent,
	})
	if err != nil {
		t.Fatal(err)
	}
	if agentAnswer.Content != "学习回答" {
		t.Fatalf("agent answer = %+v", agentAnswer)
	}
	if len(chat.requests) != 1 {
		t.Fatalf("the agent turn used the tool-free completion path: %#v", chat.requests)
	}
	if chat.calls != 1 || len(chat.seenTools) != 1 || chat.seenTools[0][0].Name != KnowledgeSearchToolName {
		t.Fatalf("the agent loop did not run with the registered tool: calls=%d offers=%d", chat.calls, len(chat.seenTools))
	}

	// An owned but empty knowledge base answers without a model call at all, so a
	// provider outage cannot turn "nothing found" into an error page.
	grounded, err := assistant.Respond(ctx, responder.Request{
		UserID: "user-1", Content: "Redis 持久化", AnswerStrategy: domain.AnswerStrategyKnowledgeQA,
		KnowledgeEnabled: true, KnowledgeCollectionIDs: []string{owned.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if grounded.Content != responder.NoEvidenceAnswer || len(grounded.Sources) != 0 {
		t.Fatalf("empty knowledge base answered %#v", grounded)
	}
	if len(chat.requests) != 1 {
		t.Fatalf("a turn with no evidence asked the model anyway: %#v", chat.requests)
	}

	// With real content, the grounded answer comes from the model and the citations
	// come from retrieval.
	if _, err := store.CreateDocument(ctx, "user-1", &domain.Document{CollectionID: owned.ID, Title: "redis.md", Format: domain.DocumentFormatMarkdown},
		[]domain.DocumentChunk{{Text: "Redis 持久化：AOF 以追加写记录每条写命令，fsync 可选 everysec。", Section: "持久化"}}); err != nil {
		t.Fatal(err)
	}
	grounded, err = assistant.Respond(ctx, responder.Request{
		UserID: "user-1", Content: "Redis 持久化 AOF", AnswerStrategy: domain.AnswerStrategyKnowledgeQA,
		KnowledgeEnabled: true, KnowledgeCollectionIDs: []string{owned.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if grounded.Content != "模型回答" {
		t.Fatalf("grounded body = %q, want the model's own text", grounded.Content)
	}
	if len(grounded.Sources) != 1 || grounded.Sources[0].Title != "redis.md" {
		t.Fatalf("citations = %#v, want the one retrieved chunk", grounded.Sources)
	}
	groundedRequest := chat.requests[len(chat.requests)-1]
	evidence := groundedRequest.Messages[len(groundedRequest.Messages)-1].Content
	if !strings.Contains(evidence, "AOF 以追加写记录每条写命令") || !strings.Contains(evidence, "[1]") {
		t.Fatalf("the model was not given the retrieved passage as numbered evidence:\n%s", evidence)
	}
}

func TestNewAssistantRefusesAProviderItCannotBuild(t *testing.T) {
	config, err := ConfigFromEnvironment(lookup(openAIEnvironment()))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := New(knowledge.NewMemoryStore(), config, Options{}); !errors.Is(err, ErrNoModelClient) {
		t.Fatalf("error = %v, want ErrNoModelClient", err)
	}
	boom := errors.New("client build failed")
	if _, _, err := New(knowledge.NewMemoryStore(), config, Options{
		NewModel: func(openai.Config) (model.ChatModel, error) { return nil, boom },
	}); !errors.Is(err, boom) {
		t.Fatalf("error = %v, want the connector failure", err)
	}
}

func TestProductionAIOptionsBuildsARealClient(t *testing.T) {
	// The production connector must be able to build a usable client from the same
	// configuration the environment parser produced - without dialing anything.
	config, err := ConfigFromEnvironment(lookup(openAIEnvironment()))
	if err != nil {
		t.Fatal(err)
	}
	chat, err := ProductionOptions().NewModel(config.Model)
	if err != nil {
		t.Fatal(err)
	}
	if chat == nil {
		t.Fatal("no client was built")
	}
}

// echoAssistant installs the default provider mode, which is what a keyless
// local run gets. It is the same helper the server binary uses.
func echoAssistant(store knowledge.Store) (responder.Responder, error) {
	assistant, _, err := New(store, Config{Provider: ProviderEcho, Context: service.ContextBudget{}.Normalize()}, Options{})
	return assistant, err
}

// TestNewResponderInstallsEveryAnswerStrategy checks the startup wiring covers all
// three strategies and that none of them reaches a network.
func TestNewResponderInstallsEveryAnswerStrategy(t *testing.T) {
	assistant, err := echoAssistant(knowledge.NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	dispatcher, ok := assistant.(*responder.StrategyResponder)
	if !ok {
		t.Fatalf("New returned %T, want the strategy dispatcher", assistant)
	}
	installed := dispatcher.Strategies()
	want := []domain.AnswerStrategy{domain.AnswerStrategyAgent, domain.AnswerStrategyDirectChat, domain.AnswerStrategyKnowledgeQA}
	if len(installed) != len(want) {
		t.Fatalf("installed strategies = %v, want %v", installed, want)
	}
	for index, strategy := range want {
		if installed[index] != strategy {
			t.Fatalf("installed strategies = %v, want %v", installed, want)
		}
	}

	ctx := context.Background()
	direct, err := assistant.Respond(ctx, responder.Request{Content: "问题", AnswerStrategy: domain.AnswerStrategyDirectChat})
	if err != nil {
		t.Fatal(err)
	}
	if direct.Content != "问题" || len(direct.Sources) != 0 {
		t.Fatalf("direct_chat answer changed at startup wiring: %#v", direct)
	}
	agent, err := assistant.Respond(ctx, responder.Request{Content: "任务", AnswerStrategy: domain.AnswerStrategyAgent})
	if err != nil {
		t.Fatal(err)
	}
	if agent.Content != "任务" || len(agent.Sources) != 0 {
		t.Fatalf("the agent strategy must stay unimplemented and must not claim a tool run: %#v", agent)
	}
	// A collection the caller owns but has never filled answers honestly with no
	// citations, rather than inventing a source.
	shared := knowledge.NewMemoryStore()
	owned, err := shared.CreateCollection(ctx, &domain.Collection{UserID: "user-1", Title: "空手册"})
	if err != nil {
		t.Fatal(err)
	}
	emptyAssistant, err := echoAssistant(shared)
	if err != nil {
		t.Fatal(err)
	}
	grounded, err := emptyAssistant.Respond(ctx, responder.Request{
		UserID: "user-1", Content: "Redis 持久化", AnswerStrategy: domain.AnswerStrategyKnowledgeQA,
		KnowledgeEnabled: true, KnowledgeCollectionIDs: []string{owned.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(grounded.Sources) != 0 || !strings.Contains(grounded.Content, "没有检索到") {
		t.Fatalf("an empty knowledge base produced %#v", grounded)
	}
	// The same wiring refuses a scope the caller does not own, so even a
	// bypassed service-level gate cannot read another user's documents.
	if _, err := emptyAssistant.Respond(ctx, responder.Request{
		UserID: "user-2", Content: "Redis 持久化", AnswerStrategy: domain.AnswerStrategyKnowledgeQA,
		KnowledgeEnabled: true, KnowledgeCollectionIDs: []string{owned.ID},
	}); err == nil {
		t.Fatal("a grounded answer read a collection its owner was not")
	}
}
