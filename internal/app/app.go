// Package app turns environment configuration into the answer strategies a
// process runs on. It is separated from cmd/gopherlearn so that a second binary -
// the provider smoke check - can build exactly the same assistant the server
// builds. A smoke run that assembled its own copy of the wiring would prove that
// its own copy works, which is not the question.
package app

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/agent"
	"github.com/xiaopang456/GopherLearn/internal/contextwindow"
	"github.com/xiaopang456/GopherLearn/internal/devmodel"
	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/knowledge"
	"github.com/xiaopang456/GopherLearn/internal/model"
	"github.com/xiaopang456/GopherLearn/internal/openai"
	"github.com/xiaopang456/GopherLearn/internal/repository"
	"github.com/xiaopang456/GopherLearn/internal/responder"
	"github.com/xiaopang456/GopherLearn/internal/retrieval"
	"github.com/xiaopang456/GopherLearn/internal/service"
	"github.com/xiaopang456/GopherLearn/internal/summary"
)

// Provider selection. echo keeps the process free of any external call, which is
// the right default for development and the only safe default when nobody set the
// variable: a backend that silently starts calling a paid endpoint because a key
// appeared in the environment is not a backend anyone should deploy.
type Provider string

// MemoryReadMode selects the complete production memory-read path. legacy_v2 is
// the only mode enabled by this migration stage; experimental tool-driven modes
// are intentionally not silently inferred from optional collaborators.
type MemoryReadMode string

const (
	MemoryReadLegacyV2 MemoryReadMode = "legacy_v2"
)

const (
	// ProviderEcho answers deterministically and never dials a provider.
	ProviderEcho Provider = "echo"
	// ProviderOpenAI completes turns through an OpenAI-compatible endpoint.
	ProviderOpenAI Provider = "openai_compatible"
)

// Environment variable names owned by this package.
const (
	EnvProvider           = "GOPHERLEARN_AI_PROVIDER"
	EnvContextMaxMessages = "GOPHERLEARN_CONTEXT_MAX_MESSAGES"
	EnvContextMaxRunes    = "GOPHERLEARN_CONTEXT_MAX_RUNES"
	EnvAgentMaxModelCalls = "GOPHERLEARN_AGENT_MAX_MODEL_CALLS"
	EnvAgentMaxToolCalls  = "GOPHERLEARN_AGENT_MAX_TOOL_CALLS"
	EnvAgentMaxSteps      = "GOPHERLEARN_AGENT_MAX_STEPS"
	EnvAgentDeadline      = "GOPHERLEARN_AGENT_DEADLINE"
	// The learning agent's context window, in estimated tokens. The declared window
	// is a budget this process holds itself to, not a measured model capacity: it says
	// how much this server is willing to send, and nothing here proves what the model
	// behind the endpoint accepts.
	EnvWindowTokens         = "GOPHERLEARN_CONTEXT_WINDOW_TOKENS"
	EnvInputTargetTokens    = "GOPHERLEARN_CONTEXT_INPUT_TARGET_TOKENS"
	EnvOutputReserveTokens  = "GOPHERLEARN_CONTEXT_OUTPUT_RESERVE_TOKENS"
	EnvSummaryInjectTokens  = "GOPHERLEARN_CONTEXT_SUMMARY_MAX_TOKENS"
	EnvHistoryBudgetTokens  = "GOPHERLEARN_CONTEXT_HISTORY_MAX_TOKENS"
	EnvHistoryFetchLimit    = "GOPHERLEARN_CONTEXT_HISTORY_FETCH_LIMIT"
	EnvEvidenceBudgetTokens = "GOPHERLEARN_CONTEXT_EVIDENCE_MAX_TOKENS"
	// The rolling summary limits. Messages and runes are counted over stored rows; the
	// token figure bounds what a refresh actually sends, including the previous
	// summary it is revising.
	EnvSummaryTriggerMessages    = "GOPHERLEARN_SUMMARY_TRIGGER_MESSAGES"
	EnvSummaryTriggerTokens      = "GOPHERLEARN_SUMMARY_TRIGGER_TOKENS"
	EnvSummaryKeepRecentMessages = "GOPHERLEARN_SUMMARY_KEEP_RECENT_MESSAGES"
	EnvSummaryMaxInputTokens     = "GOPHERLEARN_SUMMARY_MAX_INPUT_TOKENS"
	EnvSummaryMaxOutputTokens    = "GOPHERLEARN_SUMMARY_MAX_OUTPUT_TOKENS"
	// The summary's share of the turn's clock. Timeout bounds the call, StageBudget
	// bounds the stage, and FailureCooldown decides how long a session stops trying
	// after a refresh was refused.
	EnvSummaryTimeout         = "GOPHERLEARN_SUMMARY_TIMEOUT"
	EnvSummaryStageBudget     = "GOPHERLEARN_SUMMARY_STAGE_BUDGET"
	EnvSummaryFailureCooldown = "GOPHERLEARN_SUMMARY_FAILURE_COOLDOWN"
	// EnvLocalLearningModel turns on the scripted question-and-grade model for the quiz
	// workflow. It is a local development switch, not a provider: nothing dials out, and
	// the questions say they came from a script. It is refused as soon as a real
	// provider is configured, so the only way to run it is deliberately.
	EnvLocalLearningModel = "GOPHERLEARN_LOCAL_LEARNING_MODEL"
	// EnvTaskMetrics switches the internal task metering on. It is off unless it is
	// named: a deployment that forgets the variable must not start writing a log line
	// per turn, and a typo in its value must not look like it is measuring something.
	EnvTaskMetrics = "GOPHERLEARN_TASK_METRICS"
)

// TaskMetrics is the resolved value of EnvTaskMetrics.
type TaskMetrics string

const (
	// TaskMetricsOff records nothing and costs nothing.
	TaskMetricsOff TaskMetrics = "off"
	// TaskMetricsLog writes one structured JSON line per user operation to the
	// process log. It is a measurement stream, not a business record.
	TaskMetricsLog TaskMetrics = "log"
)

var (
	// ErrUnsupportedProvider reports a GOPHERLEARN_AI_PROVIDER value that is
	// neither echo nor openai_compatible.
	ErrUnsupportedProvider = errors.New("unsupported " + EnvProvider + " value")
	// ErrUnsupportedTaskMetrics reports a GOPHERLEARN_TASK_METRICS value that is
	// neither off nor log.
	ErrUnsupportedTaskMetrics = errors.New("unsupported " + EnvTaskMetrics + " value")
	// ErrNoModelClient reports openai_compatible mode in a binary that cannot
	// build the client, which is a wiring fault rather than a configuration one.
	ErrNoModelClient = errors.New("no openai-compatible model client is configured")
	// ErrNoToolCapableModel reports a model that cannot carry tool calls in the one
	// provider mode that promises a learning agent. It is a startup failure: the
	// alternative is a learning turn answered by an echoed placeholder while the
	// configuration claims a real provider.
	ErrNoToolCapableModel = errors.New("openai_compatible needs a model that implements model.ToolChatModel")
	// ErrInvalidContextLimit reports an unparseable or non-positive budget.
	ErrInvalidContextLimit = errors.New(EnvContextMaxMessages + " and " + EnvContextMaxRunes + " must be positive integers")
	// ErrInvalidWindowLimit reports a context-window figure that is not a positive
	// integer. It is refused by name rather than defaulted: a budget silently replaced
	// by a default is a number the startup line would then report as the operator's.
	ErrInvalidWindowLimit = errors.New("the " + EnvWindowTokens + " family must be positive integers")
	// ErrInvalidSummaryLimit reports a summary figure that is not a positive integer.
	ErrInvalidSummaryLimit = errors.New("the summary trigger and input limits must be positive integers")
	// ErrInvalidSummaryDuration reports an unparsable or non-positive summary duration.
	ErrInvalidSummaryDuration = errors.New(EnvSummaryTimeout + ", " + EnvSummaryStageBudget + " and " +
		EnvSummaryFailureCooldown + " must be Go durations such as 20s, and positive")
	// ErrInvalidAgentDeadline reports an unparseable agent time budget.
	ErrInvalidAgentDeadline = errors.New(EnvAgentDeadline + " must be a Go duration such as 45s")
	// ErrUnsupportedLocalLearningModel reports a GOPHERLEARN_LOCAL_LEARNING_MODEL value
	// that names no scripted model this build carries.
	ErrUnsupportedLocalLearningModel = errors.New("unsupported " + EnvLocalLearningModel + " value")
	// ErrLocalLearningModelNeedsEcho reports the switch being on while a real provider
	// is configured. It is a startup failure rather than a precedence rule: a deployment
	// whose configuration says "openai_compatible" must never answer a learner's quiz
	// with a script, and quietly choosing either side would make the configuration lie.
	ErrLocalLearningModelNeedsEcho = errors.New(EnvLocalLearningModel +
		" is a local development switch and cannot be combined with " + EnvProvider + "=openai_compatible or with provider settings")
	// ErrNoLocalLearningModel reports the switch being on in a binary that cannot build
	// the scripted model - a wiring fault, the same kind ErrNoModelClient reports.
	ErrNoLocalLearningModel = errors.New("no local learning model constructor is configured")
)

// Config is the resolved assistant configuration.
type Config struct {
	Provider Provider
	Model    openai.Config
	Context  service.ContextBudget
	// Agent bounds one learning-agent turn. It applies in openai_compatible mode,
	// where the harness runs; echo mode has no loop to bound.
	Agent agent.Budget
	// Summary bounds when the session's rolling summary is refreshed and how much a
	// refresh may show the model. It applies to every answer strategy: the summary is
	// one shared recollection of the conversation, not a per-mode memory.
	Summary summary.Policy
	// LocalLearningModel names the scripted question-and-grade model for the quiz
	// workflow, and is empty unless GOPHERLEARN_LOCAL_LEARNING_MODEL asked for one. It
	// never answers a conversation turn: the strategies stay exactly as the provider
	// mode left them, which is what makes the switch safe to leave on in a
	// development process.
	LocalLearningModel string
	// MemoryReadMode is deliberately explicit so optional memory collaborators
	// cannot turn a legacy_v2 assembly into a hybrid experimental tool surface.
	MemoryReadMode MemoryReadMode
	// TaskMetrics decides whether one user operation produces one measurement
	// record. It defaults to off, and it changes no business outcome: the same
	// calls are made, the same frames are written, the same errors come back.
	TaskMetrics TaskMetrics
}

// Options injects the client construction path so the startup decision can be
// tested without a provider.
type Options struct {
	NewModel func(config openai.Config) (model.ChatModel, error)
	// DecorateTool wraps each tool before it is registered, for a caller that has to
	// observe executions it does not own - the one-shot smoke check counts how many times
	// the real tool was entered. Production leaves this nil, and a wrapper must not change
	// what the tool does: it is a mirror, not a policy.
	DecorateTool func(tool agent.Tool) agent.Tool
	// Memory installs the second agent tool, memory_search, over a read-only recall. Nil
	// means no memory tool is registered: an agent turn then has knowledge retrieval and
	// nothing else. The reader cannot be influenced by the model - it takes the
	// authenticated user, the session and the query text, and decides scope on the server.
	Memory MemoryRecall
	// MemoryProposer installs the third agent tool, propose_memory, which stores a pending
	// draft. It is a separate collaborator from Memory because reading what the user
	// confirmed and writing a candidate about this conversation are two different
	// permissions, and only the second one can put an unconfirmed claim into the database.
	//
	// The writer's signature cannot express confirmation, deletion or an owner other than
	// the principal, so nothing here can promote a draft: that stays the user's decision on
	// the memory page.
	MemoryProposer MemoryProposalWriter
	// HistoryReader installs the Agent history capability independently of the
	// Episode search tool. Its execution still requires a server-side turn grant;
	// registration does not imply that this turn has one.
	HistoryReader func(chat model.ToolChatModel) (agent.EpisodeHistoryReader, error)
	// Episodes is the optional model-driven episode_search setup retained for
	// non-legacy offline fixtures. Production legacy_v2 does not register it.
	Episodes func(chat model.ToolChatModel) (EpisodeTools, error)
	// NewLocalLearningModel builds the scripted quiz model named by
	// Config.LocalLearningModel. It is a separate injection point from NewModel on
	// purpose: the provider client answers conversations, this one only ever feeds the
	// quiz workflow, and a caller that wired the scripted model into the strategies
	// would be handing learners answers written by a placeholder.
	NewLocalLearningModel func(name string) (model.ChatModel, error)
	// Retrieval installs the stack that grounds a knowledge turn - the semantic mode's
	// embedder, vector index and cross-encoder, already wired together by AssembleRetrieval.
	//
	// Nil keeps the deterministic pipeline this process has always built, which is why the
	// default startup behaviour is unchanged rather than merely untested. It is injected
	// rather than assembled inside Assemble because one assembly point has to decide the
	// mode: a strategy that picked its own retriever could answer a learner's question from
	// a different index than the one the deployment believes it built.
	Retrieval retrieval.Retriever
}

// ProductionOptions wires the real OpenAI-compatible client, and the scripted quiz model
// that the local development switch can ask for. Neither is reachable without its own
// explicit configuration: a provider is built only in openai_compatible mode, and the
// script only when GOPHERLEARN_LOCAL_LEARNING_MODEL names it - which the configuration
// refuses alongside any provider setting.
func ProductionOptions() Options {
	return Options{
		NewModel: func(config openai.Config) (model.ChatModel, error) {
			return openai.New(config)
		},
		NewLocalLearningModel: func(name string) (model.ChatModel, error) {
			if name != devmodel.LearningModelName {
				return nil, fmt.Errorf("%w: %q", ErrUnsupportedLocalLearningModel, name)
			}
			return devmodel.NewLearning(), nil
		},
	}
}

// ConfigFromEnvironment reads provider selection and the context budget. The
// OpenAI variables are only required once that provider is chosen: an unset
// provider keeps a keyless local run working.
func ConfigFromEnvironment(lookup func(string) string) (Config, error) {
	if lookup == nil {
		lookup = func(string) string { return "" }
	}
	config := Config{Provider: ProviderEcho, Context: service.ContextBudget{}.Normalize()}

	provider := Provider(strings.ToLower(strings.TrimSpace(lookup(EnvProvider))))
	switch provider {
	case "", ProviderEcho:
		config.Provider = ProviderEcho
	case ProviderOpenAI:
		config.Provider = ProviderOpenAI
		modelConfig, err := openai.ConfigFromEnvironment(lookup)
		if err != nil {
			return Config{}, err
		}
		config.Model = modelConfig
	default:
		return Config{}, fmt.Errorf("%w: %q", ErrUnsupportedProvider, string(provider))
	}

	// The scripted quiz model is read after the provider, because whether it is lawful
	// depends on what was just chosen. Anything that names a real provider - the mode
	// itself, or a single provider setting left in the environment - rules it out.
	local := strings.ToLower(strings.TrimSpace(lookup(EnvLocalLearningModel)))
	switch local {
	case "":
	case devmodel.LearningModelName:
		if config.Provider != ProviderEcho || providerSettingsPresent(lookup) {
			return Config{}, ErrLocalLearningModelNeedsEcho
		}
		config.LocalLearningModel = local
	default:
		return Config{}, fmt.Errorf("%w: %q", ErrUnsupportedLocalLearningModel, local)
	}

	messages, err := positiveInt(lookup(EnvContextMaxMessages))
	if err != nil {
		return Config{}, err
	}
	runes, err := positiveInt(lookup(EnvContextMaxRunes))
	if err != nil {
		return Config{}, err
	}
	if messages > 0 {
		config.Context.MaxMessages = messages
	}
	if runes > 0 {
		config.Context.MaxRunes = runes
	}

	// The agent's context window. Every figure is read as an integer or the startup
	// stops; nothing is quietly rounded, because these numbers decide how much of a
	// learner's conversation reaches a model.
	window := contextwindow.Config{}
	if window.WindowTokens, err = positiveIntIn(lookup(EnvWindowTokens), ErrInvalidWindowLimit); err != nil {
		return Config{}, err
	}
	if window.InputTargetTokens, err = positiveIntIn(lookup(EnvInputTargetTokens), ErrInvalidWindowLimit); err != nil {
		return Config{}, err
	}
	if window.OutputReserveTokens, err = positiveIntIn(lookup(EnvOutputReserveTokens), ErrInvalidWindowLimit); err != nil {
		return Config{}, err
	}
	if window.SummaryMaxTokens, err = positiveIntIn(lookup(EnvSummaryInjectTokens), ErrInvalidWindowLimit); err != nil {
		return Config{}, err
	}
	if window.HistoryMaxTokens, err = positiveIntIn(lookup(EnvHistoryBudgetTokens), ErrInvalidWindowLimit); err != nil {
		return Config{}, err
	}
	if window.HistoryFetchLimit, err = positiveIntIn(lookup(EnvHistoryFetchLimit), ErrInvalidWindowLimit); err != nil {
		return Config{}, err
	}
	if window.EvidenceMaxTokens, err = positiveIntIn(lookup(EnvEvidenceBudgetTokens), ErrInvalidWindowLimit); err != nil {
		return Config{}, err
	}
	// The agent's history keeps the two ceilings this project already had, read from the
	// same environment variables the chat and knowledge windows use. Carrying them into
	// the budget instead of applying them separately is what makes one selection answer
	// to count, size and token estimate at once - and what stops a token cap, which is
	// wider than a rune cap for Latin-script text, from widening the window.
	if messages > 0 {
		window.HistoryMaxMessages = messages
	}
	if runes > 0 {
		window.HistoryMaxRunes = runes
	}
	resolvedWindow, err := window.Resolve()
	if err != nil {
		return Config{}, err
	}
	config.Context.Window = resolvedWindow
	// The provider's own output limit has to fit inside the reserve the window set
	// aside for a reply. Checked here because this is the only place that holds both
	// figures, and a window whose reserve is smaller than the limit it will actually
	// send is a budget that cancels itself.
	if err := resolvedWindow.CheckOutputCap(config.Model.MaxOutputTokens); err != nil {
		return Config{}, err
	}

	modelCalls, err := positiveInt(lookup(EnvAgentMaxModelCalls))
	if err != nil {
		return Config{}, err
	}
	toolCalls, err := positiveInt(lookup(EnvAgentMaxToolCalls))
	if err != nil {
		return Config{}, err
	}
	steps, err := positiveInt(lookup(EnvAgentMaxSteps))
	if err != nil {
		return Config{}, err
	}
	deadline, err := agentDeadline(lookup(EnvAgentDeadline))
	if err != nil {
		return Config{}, err
	}
	// Normalize clamps as well as defaults, so an operator who sets 100000 model
	// calls gets the ceiling instead of an agent that bills all night.
	config.Agent = agent.Budget{
		MaxModelCalls: modelCalls,
		MaxToolCalls:  toolCalls,
		MaxSteps:      steps,
		Deadline:      deadline,
	}.Normalize()
	// An agent turn spends one clock. The refresh and the loop draw from the same
	// deadline rather than each starting a fresh one, so a summary that took eight
	// seconds cannot also hand the answer the full forty-five.
	config.Context.AgentTurnDeadline = config.Agent.Deadline

	trigger, err := positiveIntIn(lookup(EnvSummaryTriggerMessages), ErrInvalidSummaryLimit)
	if err != nil {
		return Config{}, err
	}
	triggerTokens, err := positiveIntIn(lookup(EnvSummaryTriggerTokens), ErrInvalidSummaryLimit)
	if err != nil {
		return Config{}, err
	}
	keepRecent, err := positiveIntIn(lookup(EnvSummaryKeepRecentMessages), ErrInvalidSummaryLimit)
	if err != nil {
		return Config{}, err
	}
	inputTokens, err := positiveIntIn(lookup(EnvSummaryMaxInputTokens), ErrInvalidSummaryLimit)
	if err != nil {
		return Config{}, err
	}
	outputTokens, err := positiveIntIn(lookup(EnvSummaryMaxOutputTokens), ErrInvalidSummaryLimit)
	if err != nil {
		return Config{}, err
	}
	stageTimeout, err := summaryDuration(lookup(EnvSummaryTimeout))
	if err != nil {
		return Config{}, err
	}
	stageBudget, err := summaryDuration(lookup(EnvSummaryStageBudget))
	if err != nil {
		return Config{}, err
	}
	cooldown, err := summaryDuration(lookup(EnvSummaryFailureCooldown))
	if err != nil {
		return Config{}, err
	}
	config.Summary = summary.Policy{
		TriggerMessages:    trigger,
		TriggerTokens:      triggerTokens,
		KeepRecentMessages: keepRecent,
		MaxInputTokens:     inputTokens,
		MaxOutputTokens:    outputTokens,
		Timeout:            stageTimeout,
		StageBudget:        stageBudget,
		FailureCooldown:    cooldown,
	}.Normalize()

	// Task metering is opt-in, and a value that is neither name stops startup: a
	// typo left to default quietly to off would be a deployment that believes it is
	// measuring something while it measures nothing.
	switch metrics := TaskMetrics(strings.ToLower(strings.TrimSpace(lookup(EnvTaskMetrics)))); metrics {
	case "", TaskMetricsOff:
		config.TaskMetrics = TaskMetricsOff
	case TaskMetricsLog:
		config.TaskMetrics = TaskMetricsLog
	default:
		return Config{}, fmt.Errorf("%w: %q", ErrUnsupportedTaskMetrics, strings.TrimSpace(lookup(EnvTaskMetrics)))
	}
	return config, nil
}

// agentDeadline parses the turn's time budget. An unparsable value stops startup
// rather than defaulting, because a silently ignored deadline is the one budget whose
// absence cannot be seen in a log line about call counts.
func agentDeadline(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		return 0, ErrInvalidAgentDeadline
	}
	return value, nil
}

// providerSettingsPresent reports whether any provider variable was given, including one
// left behind by a different deployment. It is the second half of the rule that keeps the
// scripted quiz model out of a real-provider process: the mode alone is not enough,
// because a process can hold a key and still run echo - and then quietly answer a quiz
// from a script while an operator believes the notes went to a model.
func providerSettingsPresent(lookup func(string) string) bool {
	for _, name := range []string{
		openai.EnvBaseURL, openai.EnvAPIKey, openai.EnvModel, openai.EnvEnableThinking,
		openai.EnvTemperature, openai.EnvMaxOutputTokens, openai.EnvTimeout,
	} {
		if strings.TrimSpace(lookup(name)) != "" {
			return true
		}
	}
	return false
}

func positiveInt(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return 0, ErrInvalidContextLimit
	}
	return value, nil
}

// positiveIntIn reads one optional positive integer and reports it under the family's
// own error, so a bad window figure is refused as a window problem rather than as the
// generic context-limit one. Absent means unset, which Resolve fills with its default.
func positiveIntIn(raw string, refusal error) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%w: got %q", refusal, strings.TrimSpace(raw))
	}
	return value, nil
}

// summaryDuration reads one optional duration. An unparsable value stops startup: a
// silently ignored timeout is the one budget whose absence cannot be spotted in a line
// about token counts.
func summaryDuration(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%w: got %q", ErrInvalidSummaryDuration, raw)
	}
	return value, nil
}

// NewRetrievalPipeline builds the offline retrieval stack: BM25 plus the
// deterministic feature-hash vector scorer, fused by reciprocal rank. A real
// embedder, Redis and reranking are still not part of this.
func NewRetrievalPipeline(store knowledge.Store) *retrieval.Pipeline {
	return retrieval.NewPipeline(
		knowledge.NewChunkSource(store),
		[]retrieval.Scorer{
			retrieval.NewLexicalScorer(retrieval.BM25Options{}),
			retrieval.NewVectorScorer(retrieval.NewDeterministicEmbedder(0), retrieval.VectorOptions{}),
		},
		retrieval.Options{},
	)
}

// New installs the answer strategies and reports what the process will actually do
// when a user asks a question.
//
// echo mode keeps the deterministic placeholder responder and the extractive
// knowledge generator, so a keyless startup behaves exactly as it did before any model
// existed. Its learning_agent is an explicit placeholder: the startup line says the
// process contacts no provider.
//
// openai_compatible mode routes direct_chat and knowledge_qa through one shared
// ChatModel and runs learning_agent through the Agent Harness. A model that cannot carry
// tool calls stops startup instead of leaving the agent on echo: answering a learning
// question with an echoed placeholder, while the deployment claims a real provider, is
// the silent downgrade this function exists to make impossible.
func Assemble(store knowledge.Store, config Config, opts Options) (Assembly, error) {
	// A non-empty mode is the production contract. A zero mode remains accepted
	// for older offline assembly fixtures that exercise optional-tool behavior.
	if config.MemoryReadMode != "" && config.MemoryReadMode != MemoryReadLegacyV2 {
		return Assembly{}, fmt.Errorf("unsupported memory read mode %q", config.MemoryReadMode)
	}

	groundedRetriever := opts.Retrieval
	if groundedRetriever == nil {
		groundedRetriever = NewRetrievalPipeline(store)
	}
	titles := service.NewKnowledgeService(store)

	if config.Provider == ProviderOpenAI {
		// ConfigFromEnvironment already refuses this combination. The check is here
		// because Assemble takes a Config a caller built by hand, and a scripted quiz
		// model reaching a process that named a real provider is exactly the silent swap
		// this function exists to make impossible.
		if config.LocalLearningModel != "" {
			return Assembly{}, ErrLocalLearningModelNeedsEcho
		}
		if opts.NewModel == nil {
			return Assembly{}, ErrNoModelClient
		}
		chat, err := opts.NewModel(config.Model)
		if err != nil {
			return Assembly{}, err
		}
		toolModel, ok := chat.(model.ToolChatModel)
		if !ok {
			return Assembly{}, fmt.Errorf("%w: configured provider %T", ErrNoToolCapableModel, chat)
		}
		direct, err := responder.NewModelStrategy(domain.AnswerStrategyDirectChat, chat, responder.DirectSystemPrompt)
		if err != nil {
			return Assembly{}, err
		}
		generator, err := responder.NewModelGenerator(chat, responder.KnowledgeSystemPrompt)
		if err != nil {
			return Assembly{}, err
		}
		grounded := responder.NewKnowledgeStrategy(groundedRetriever, titles, generator)

		search, err := agent.NewKnowledgeSearch(groundedRetriever, titles, agent.KnowledgeSearchOptions{})
		if err != nil {
			return Assembly{}, err
		}
		var tool agent.Tool = search
		if opts.DecorateTool != nil {
			tool = opts.DecorateTool(search)
		}
		legacyMode := config.MemoryReadMode == MemoryReadLegacyV2
		// Explicit production legacy_v2 has no memory_search. Zero-mode retains
		// the historical offline assembly matrix until its fixtures migrate.
		tools := []agent.Tool{tool}
		if !legacyMode && opts.Memory != nil {
			memoryTool, err := agent.NewMemorySearch(newMemoryReader(opts.Memory), agent.MemorySearchOptions{})
			if err != nil {
				return Assembly{}, err
			}
			var memory agent.Tool = memoryTool
			if opts.DecorateTool != nil {
				memory = opts.DecorateTool(memoryTool)
			}
			tools = append(tools, memory)
		}
		if opts.MemoryProposer != nil {
			proposalTool, err := agent.NewProposeMemory(newMemoryProposer(opts.MemoryProposer), agent.ProposeMemoryOptions{})
			if err != nil {
				return Assembly{}, err
			}
			var proposal agent.Tool = proposalTool
			if opts.DecorateTool != nil {
				proposal = opts.DecorateTool(proposalTool)
			}
			tools = append(tools, proposal)
		}
		// Episode search is not part of the legacy_v2 V2 tool surface. The
		// Episode setup may still provide the server-owned history reader for
		// read_history_message, whose registration is independent of grants.
		if opts.HistoryReader != nil {
			reader, err := opts.HistoryReader(toolModel)
			if err != nil {
				return Assembly{}, err
			}
			historyTool, err := agent.NewReadHistoryMessage(reader, agent.HistoryReadOptions{})
			if err != nil {
				return Assembly{}, err
			}
			var history agent.Tool = historyTool
			if opts.DecorateTool != nil {
				history = opts.DecorateTool(historyTool)
			}
			tools = append(tools, history)
		}
		if opts.Episodes != nil {
			episodeTools, err := opts.Episodes(toolModel)
			if err != nil {
				return Assembly{}, err
			}
			if opts.HistoryReader == nil {
				historyTool, err := agent.NewReadHistoryMessage(episodeTools.HistoryReader, agent.HistoryReadOptions{})
				if err != nil {
					return Assembly{}, err
				}
				var history agent.Tool = historyTool
				if opts.DecorateTool != nil {
					history = opts.DecorateTool(historyTool)
				}
				tools = append(tools, history)
			}
			if !legacyMode {
				searchTool, err := agent.NewEpisodeSearch(episodeTools.Searcher, agent.EpisodeSearchOptions{})
				if err != nil {
					return Assembly{}, err
				}
				var search agent.Tool = searchTool
				if opts.DecorateTool != nil {
					search = opts.DecorateTool(searchTool)
				}
				tools = append(tools, search)
			}
		}
		registry, err := agent.NewRegistry(tools...)
		if err != nil {
			return Assembly{}, err
		}
		harness, err := agent.New(toolModel, registry, config.Agent,
			agent.WithContextWindow(config.Context.Window, config.Model.MaxOutputTokens))
		if err != nil {
			return Assembly{}, err
		}
		budget := harness.Budget()
		window := harness.Window()
		// The same chat model writes session summaries. They are a separate model call
		// and are deliberately outside the harness budget below: a summary is not an
		// agent step, and charging it there would make the agent's ceiling depend on
		// how long the conversation has been running.
		summarizer, err := summary.NewModelSummarizer(chat, config.Summary)
		if err != nil {
			return Assembly{}, err
		}
		startupLine := config.Model.Summary() + " agent=harness tools=" + strings.Join(harness.ToolNames(), ",") +
			" max_model_calls=" + strconv.Itoa(budget.MaxModelCalls) +
			" max_tool_calls=" + strconv.Itoa(budget.MaxToolCalls) +
			" max_steps=" + strconv.Itoa(budget.MaxSteps) +
			" deadline=" + budget.Deadline.String() +
			" window_tokens=" + strconv.Itoa(window.WindowTokens) +
			" input_target_tokens=" + strconv.Itoa(window.InputTargetTokens) +
			" output_reserve_tokens=" + strconv.Itoa(window.OutputReserveTokens) +
			" summary_inject_tokens=" + strconv.Itoa(window.SummaryMaxTokens) +
			" history_tokens=" + strconv.Itoa(window.HistoryMaxTokens) +
			" history_messages=" + strconv.Itoa(window.HistoryMaxMessages) +
			" history_runes=" + strconv.Itoa(window.HistoryMaxRunes) +
			" evidence_tokens=" + strconv.Itoa(window.EvidenceMaxTokens) +
			" summary_trigger_messages=" + strconv.Itoa(config.Summary.TriggerMessages) +
			" summary_trigger_tokens=" + strconv.Itoa(config.Summary.TriggerTokens) +
			" summary_keep_recent_messages=" + strconv.Itoa(config.Summary.KeepRecentMessages) +
			" summary_max_input_tokens=" + strconv.Itoa(config.Summary.MaxInputTokens) +
			" summary_max_output_tokens=" + strconv.Itoa(config.Summary.MaxOutputTokens) +
			" summary_stage_budget=" + config.Summary.StageBudget.String() +
			" summary_failure_cooldown=" + config.Summary.FailureCooldown.String()
		assistant, err := responder.NewStrategyResponder(direct, grounded, harness)
		if err != nil {
			return Assembly{}, err
		}
		return Assembly{
			Assistant:     assistant,
			Model:         chat,
			Summaries:     summarizer,
			SummaryPolicy: config.Summary,
			StartupLine:   startupLine,
		}, nil
	}

	agentStrategy, err := responder.NewEchoStrategy(domain.AnswerStrategyAgent)
	if err != nil {
		return Assembly{}, err
	}
	direct, err := responder.NewEchoStrategy(domain.AnswerStrategyDirectChat)
	if err != nil {
		return Assembly{}, err
	}
	grounded := responder.NewKnowledgeStrategy(groundedRetriever, titles, responder.ExcerptGenerator{})
	assistant, err := responder.NewStrategyResponder(direct, grounded, agentStrategy)
	if err != nil {
		return Assembly{}, err
	}
	// No summarizer, and no summary policy either: an echo "model" repeats the text it
	// is given, so a summary written by it would be a placeholder presented as
	// understanding. Turns here are answered from the recent window alone.
	assembly := Assembly{
		Assistant: assistant,
		Summaries: nil,
		StartupLine: "provider=echo (no provider calls; answers are deterministic placeholders; " +
			"learning_agent is a placeholder echo; session summaries are disabled, context is the recent window)",
	}
	if config.LocalLearningModel == "" {
		return assembly, nil
	}
	// The scripted quiz model changes one thing and one thing only: whether the quiz
	// workflow has a model behind it. Chat and knowledge answers still come from the
	// placeholders above, and Model stays nil so the agent harness is still unbuildable.
	if opts.NewLocalLearningModel == nil {
		return Assembly{}, ErrNoLocalLearningModel
	}
	scripted, err := opts.NewLocalLearningModel(config.LocalLearningModel)
	if err != nil {
		return Assembly{}, err
	}
	assembly.LearningModel = scripted
	assembly.StartupLine += " local_learning_model=" + config.LocalLearningModel +
		" (scripted quiz questions and grades; NOT a provider; chat answers are still placeholders)"
	return assembly, nil
}

// Assembly is what a binary needs to serve conversations: the assistant that answers
// turns, and the pieces that maintain the rolling context those turns are answered
// from. Returning them together is what keeps "the single assembly point" true - a
// command that assembled the summarizer separately could disagree with the strategies
// about what the session remembers.
type Assembly struct {
	Assistant responder.Responder
	// Model is the chat client this assembly built, exposed for the features that need a
	// model but are not an answer strategy. It is not a second way to answer a turn - that
	// is Assistant's job, and going around it would skip the strategy resolution, the
	// citation rules and the context budget. A build with no configured provider leaves it
	// nil, and every consumer must treat nil as "this feature is off".
	Model model.ChatModel
	// LearningModel is the scripted model a local development build may offer the quiz
	// workflow when no provider was configured. It is nil in every other case, and it is
	// never the model an answer strategy uses: a deployment with this set still answers
	// conversations, grounds knowledge turns and runs agents exactly as its provider mode
	// dictates - in echo mode, from placeholders.
	LearningModel model.ChatModel
	// MemorySupply is the server-side learning-agent context supplier. It is
	// intentionally separate from the model/tool assembly and is nil when memory
	// context injection was not configured.
	MemorySupply *service.MemoryContextSupply
	// EpisodeRecall is the installed context recall provider, if episodic memory
	// is enabled for learning-agent turns.
	EpisodeRecall service.EpisodeContextProvider
	// Summaries is nil when rolling summaries are disabled. The service treats that
	// as "never refresh", not as "summarize with nothing".
	Summaries     summary.ConversationSummarizer
	SummaryPolicy summary.Policy
	StartupLine   string
}

// New keeps the assistant-only view of Assemble for callers that neither configure nor
// serve summaries.
func New(store knowledge.Store, config Config, opts Options) (responder.Responder, string, error) {
	assembly, err := Assemble(store, config, opts)
	if err != nil {
		return nil, "", err
	}
	return assembly.Assistant, assembly.StartupLine, err
}

// NewMessageService is the one place the conversation service is assembled with
// the knowledge gate and the context budget, so no binary can quietly run a turn
// without either of them.
func NewMessageService(store repository.Repository, assistant responder.Responder, knowledgeService *service.KnowledgeService, budget service.ContextBudget, options ...service.MessageServiceOption) *service.MessageService {
	wired := []service.MessageServiceOption{
		service.WithCollectionAccess(knowledgeService),
		service.WithConversationBudget(budget.Normalize()),
	}
	// The active-quiz task state is wired whenever the store can answer it,
	// unconditionally, exactly as the reference project did: the block costs
	// one bounded read per agent turn and answers empty when no quiz is open.
	if reader, ok := store.(service.ActiveLearningReader); ok {
		wired = append(wired, service.WithLearningTaskState(reader))
	}
	return service.NewMessageService(store, assistant, append(wired, options...)...)
}

// SummaryOptions is the assembly's own summary wiring, so a binary passes one value
// rather than reaching into the policy by hand.
func (a Assembly) SummaryOptions() []service.MessageServiceOption {
	if a.Summaries == nil {
		return nil
	}
	return []service.MessageServiceOption{service.WithRollingSummary(a.Summaries, a.SummaryPolicy)}
}
