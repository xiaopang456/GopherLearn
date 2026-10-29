package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"GopherAI/common/memoryrecall"

	"github.com/BurntSushi/toml"
)

// Rerank provider names. The value is written in the configuration file as
// ragModelConfig.rerankProvider and selects which service shape the rerank
// client speaks.
//
// The vocabulary lives here because the configuration file is what names it;
// the clients that implement these shapes read the same names.
const (
	// RerankProviderLocal speaks to a locally served text-embeddings-inference
	// server: one local HTTP address answering /health, /info and /rerank.
	RerankProviderLocal = "local"
	// RerankProviderDashScope speaks to a hosted rerank endpoint that takes a
	// model name, a query and a document list, and answers with relevance
	// scores.
	RerankProviderDashScope = "dashscope"
)

// Default bounds for one rerank call when the configuration does not set one.
// A CPU-only local server needs seconds rather than milliseconds: the
// reference deployment answered a candidate batch in about thirteen seconds
// on CPU. A hosted endpoint answers in well under that: measured over five
// calls with twenty-four candidates, the median was 0.52s and the slowest
// 0.89s, so five seconds leaves room for jitter and load without letting a
// hung request hold a turn open.
const (
	defaultLocalRerankTimeout  = 30 * time.Second
	defaultHostedRerankTimeout = 5 * time.Second
)

// ErrRerankConfig marks a rerank configuration problem. The message never
// carries the endpoint, the token or any other configuration value: it names
// what is wrong and where to fix it.
var ErrRerankConfig = errors.New("invalid rerank configuration")

type MainConfig struct {
	Port    int    `toml:"port"`
	AppName string `toml:"appName"`
	Host    string `toml:"host"`
}

// EmailConfig addresses the SMTP server that delivers the registration captcha.
//
// Host and Port are optional: when they are empty the historical default
// (smtp.qq.com:587) is used, so an existing deployment keeps working. A
// deployment that runs its own relay - or a local capture server used to
// verify the registration path - points them at it.
//
// The mail path is needed only by "send me a captcha". Nothing else in the
// application reads this section, so leaving it unset does not stop the
// server from running or from serving the accounts it already has.
type EmailConfig struct {
	Authcode string `toml:"authcode"`
	Email    string `toml:"email"`
	// Host is the SMTP host. Empty means the default host.
	Host string `toml:"host"`
	// Port is the SMTP port. Zero means the default port.
	Port int `toml:"port"`
}

type RedisConfig struct {
	RedisPort     int    `toml:"port"`
	RedisDb       int    `toml:"db"`
	RedisHost     string `toml:"host"`
	RedisPassword string `toml:"password"`
}

type MysqlConfig struct {
	MysqlPort         int    `toml:"port"`
	MysqlHost         string `toml:"host"`
	MysqlUser         string `toml:"user"`
	MysqlPassword     string `toml:"password"`
	MysqlDatabaseName string `toml:"databaseName"`
	MysqlCharset      string `toml:"charset"`
}

type JwtConfig struct {
	ExpireDuration int    `toml:"expire_duration"`
	Issuer         string `toml:"issuer"`
	Subject        string `toml:"subject"`
	Key            string `toml:"key"`
}

type Rabbitmq struct {
	RabbitmqPort     int    `toml:"port"`
	RabbitmqHost     string `toml:"host"`
	RabbitmqUsername string `toml:"username"`
	RabbitmqPassword string `toml:"password"`
	RabbitmqVhost    string `toml:"vhost"`
}

type RagModelConfig struct {
	RagEmbeddingModel   string `toml:"embeddingModel"`
	RagEmbeddingBaseURL string `toml:"embeddingBaseUrl"`
	RagChatModelName    string `toml:"chatModelName"`
	RagDocDir           string `toml:"docDir"`
	RagBaseUrl          string `toml:"baseUrl"`
	RagDimension        int    `toml:"dimension"`
	RagRerankEnabled    bool   `toml:"rerankEnabled"`
	// RagRerankProvider selects the rerank service shape: RerankProviderLocal
	// for a locally served cross-encoder, RerankProviderDashScope for a hosted
	// endpoint. An empty value means the local provider.
	RagRerankProvider string `toml:"rerankProvider"`
	// RagRerankEndpoint is the service address. A local provider takes the
	// root of a text-embeddings-inference server; a hosted provider takes the
	// full rerank URL.
	RagRerankEndpoint string `toml:"rerankEndpoint"`
	// RagRerankModel names the model a hosted provider should serve. The local
	// provider serves one frozen model and ignores this field.
	RagRerankModel string `toml:"rerankModel"`
	// RagRerankTimeoutMs bounds one rerank call. Zero means the provider
	// default.
	RagRerankTimeoutMs int `toml:"rerankTimeoutMs"`
	// RagRerankToken is the bearer credential. It is never read from the
	// configuration file: it comes from RAG_RERANK_AUTH_TOKEN in the
	// environment, because it is a secret.
	RagRerankToken string `toml:"-"`
}

type VoiceServiceConfig struct {
	VoiceServiceApiKey    string `toml:"voiceServiceApiKey"`
	VoiceServiceSecretKey string `toml:"voiceServiceSecretKey"`
}

// KnowledgeConfig contains product limits and worker-pool settings for the
// personal knowledge base. Byte units avoid ambiguity at enforcement sites.
type KnowledgeConfig struct {
	StorageDir          string `toml:"storageDir"`
	MaxFileSizeBytes    int64  `toml:"maxFileSizeBytes"`
	MaxDocumentsPerUser int    `toml:"maxDocumentsPerUser"`
	MaxTotalSizeBytes   int64  `toml:"maxTotalSizeBytes"`
	WorkerCount         int    `toml:"workerCount"`
	QueueSize           int    `toml:"queueSize"`
	ScanIntervalSeconds int    `toml:"scanIntervalSeconds"`
	IndexTimeoutSeconds int    `toml:"indexTimeoutSeconds"`
	// ExecutionMode selects who indexes documents. This build ships exactly one
	// mode: "" or "legacy", the in-process worker pool that parses, chunks,
	// embeds and stores a document inside the backend process.
	//
	// The key is still read so that a configuration carried over from a build
	// that offered a remote executor fails loudly instead of being silently
	// downgraded to local processing.
	ExecutionMode string `toml:"executionMode"`
}

// Execution modes accepted by KnowledgeConfig.ExecutionMode.
const (
	ExecutionModeLegacy = "legacy"
)

// CandidateMode reports whether documents are processed by the candidate chain.
func (c KnowledgeConfig) CandidateMode() bool { return false }

type AgentConfig struct {
	MaxToolCalls                     int    `toml:"maxToolCalls"`
	MaxModelCalls                    int    `toml:"maxModelCalls"`
	MaxGraphSteps                    int    `toml:"maxGraphSteps"`
	DeadlineSeconds                  int    `toml:"deadlineSeconds"`
	KnowledgeDefaultTopK             int    `toml:"knowledgeDefaultTopK"`
	KnowledgeMaxTopK                 int    `toml:"knowledgeMaxTopK"`
	KnowledgeMaxResultTokens         int    `toml:"knowledgeMaxResultTokens"`
	KnowledgeMaxObservationTokens    int    `toml:"knowledgeMaxObservationTokens"`
	KnowledgeMaxRunTokens            int    `toml:"knowledgeMaxRunTokens"`
	DocumentReadMaxLines             int    `toml:"documentReadMaxLines"`
	DocumentReadMaxBytes             int    `toml:"documentReadMaxBytes"`
	DocumentReadMaxObservationTokens int    `toml:"documentReadMaxObservationTokens"`
	ContextModelWindowTokens         int    `toml:"contextModelWindowTokens"`
	ContextInputTargetTokens         int    `toml:"contextInputTargetTokens"`
	ContextOutputReserveTokens       int    `toml:"contextOutputReserveTokens"`
	ContextMaxGlobalUserMemoryTokens int    `toml:"contextMaxGlobalUserMemoryTokens"`
	ContextMaxTopicUserMemoryTokens  int    `toml:"contextMaxTopicUserMemoryTokens"`
	ContextMaxSummaryTokens          int    `toml:"contextMaxSummaryTokens"`
	ContextMaxMasteryTokens          int    `toml:"contextMaxMasteryTokens"`
	ContextMaxEpisodeTokens          int    `toml:"contextMaxEpisodeTokens"`
	EpisodeRecallMode                string `toml:"episodeRecallMode"`
	// MemoryReadMode selects the whole long-term memory read path.
	//
	// STABILIZE-031: the unit of choice is a complete mode, not a component
	// switch, and the default is legacy_v2 until a brand-new blind set says
	// otherwise. A development gate that did not pass is not a reason to ship.
	MemoryReadMode string `toml:"memoryReadMode"`
	// MemoryTopicStandout and MemoryTopicMargin are the semantic topic gate.
	//
	// They have no production default on purpose. Enabling
	// episode_v3_semantic without supplying both is a startup failure, because
	// a number that stays in a Default*Config() is a production default no
	// matter what the comment above it says (BUG-AGENT-140).
	MemoryTopicStandout float64 `toml:"memoryTopicStandout"`
	MemoryTopicMargin   float64 `toml:"memoryTopicMargin"`
	// TopicConsentSecret signs the read-side confirmation ticket of
	// agent_memory_user_confirmed_v1.
	//
	// It has no default, for the reason MemoryTopicStandout has none: a signing
	// key that lives in Default*Config() is a shipped key no matter what the
	// comment above it says, and a shipped signing key is the same as no
	// signature at all. Enabling the mode without supplying one is a startup
	// failure.
	TopicConsentSecret string `toml:"topicConsentSecret"`
	// TopicConsentTTLMinutes bounds how long a suggestion stays usable. Zero
	// means the package default.
	TopicConsentTTLMinutes int `toml:"topicConsentTTLMinutes"`
	// TopicConsentMaxCandidates bounds the suggestion list. Zero means the
	// package default.
	TopicConsentMaxCandidates int `toml:"topicConsentMaxCandidates"`
	// The MemoryPlanner* fields configure model_planned_v1 and nothing else.
	//
	// They are separate from the chat and embedding identities on purpose. The
	// 2026-09-02 environment pre-check found that the shipped chat endpoint is a
	// deployment bound to one model, where asking for the planner model by name
	// returns 404 - so reusing OPENAI_BASE_URL here would not be a shortcut, it
	// would be a different service. Reusing EMBEDDING_API_KEY as a chat
	// credential is refused for the same reason in the other direction.
	//
	// There is no API key field. The key is read from the process environment at
	// wiring time and never lands in a TOML file, a default, a log line or an
	// artefact; an operator may point it at the same value they use elsewhere,
	// but that is a property of their machine, not of this configuration.
	//
	// Like the semantic gate above, these have no production default and must be
	// absent unless the mode that uses them is selected. A number that stays in a
	// Default*Config() is a production default whatever the comment says
	// (BUG-AGENT-140), and here it would also be a credential read by a mode
	// nobody chose.
	MemoryPlannerModel          string `toml:"memoryPlannerModel"`
	MemoryPlannerBaseURL        string `toml:"memoryPlannerBaseURL"`
	MemoryPlannerTimeoutMS      int    `toml:"memoryPlannerTimeoutMs"`
	MemoryVerifierTimeoutMS     int    `toml:"memoryVerifierTimeoutMs"`
	MemoryPlannerMaxAttempts    int    `toml:"memoryPlannerMaxAttempts"`
	MemoryPlannerMaxConcurrency int    `toml:"memoryPlannerMaxConcurrency"`
	// The MemoryTool* fields configure the agent memory tool modes and nothing
	// else.
	//
	// They have no production default for the same reason the planner fields do
	// not (BUG-AGENT-140): a number that survives in a Default*Config() is a
	// production default whatever the comment above it says, and these numbers
	// decide how much of a learner's personal history one tool call may reach.
	// Under any other mode they must be absent, so that flipping one string
	// cannot bring a retrieval budget nobody reviewed into service.
	MemoryToolMaxCalls       int     `toml:"memoryToolMaxCalls"`
	MemoryToolTopK           int     `toml:"memoryToolTopK"`
	MemoryToolCandidateDepth int     `toml:"memoryToolCandidateDepth"`
	MemoryToolMinConfidence  float64 `toml:"memoryToolMinConfidence"`
	MemoryToolTimeoutMS      int     `toml:"memoryToolTimeoutMs"`
	// MemoryToolWindowPolicy selects the STABILIZE-036 experiment. Empty means
	// the shipped window. It is a separate setting rather than a mode of its
	// own because it changes only how much of an already-selected candidate set
	// one observation carries, and a mode would have implied a second read path
	// to keep measured.
	MemoryToolWindowPolicy        string `toml:"memoryToolWindowPolicy"`
	ContextHistoryFetchLimit      int    `toml:"contextHistoryFetchLimit"`
	SummaryTriggerMessages        int    `toml:"summaryTriggerMessages"`
	SummaryTriggerTokens          int    `toml:"summaryTriggerTokens"`
	SummaryRetainMessages         int    `toml:"summaryRetainMessages"`
	SummaryMaxInputTokens         int    `toml:"summaryMaxInputTokens"`
	SummaryFetchLimit             int    `toml:"summaryFetchLimit"`
	SummaryTimeoutSeconds         int    `toml:"summaryTimeoutSeconds"`
	SummaryStageBudgetSeconds     int    `toml:"summaryStageBudgetSeconds"`
	SummaryFailureCooldownSeconds int    `toml:"summaryFailureCooldownSeconds"`
	// EpisodeRelationMode selects the episodic write-side relation pass:
	// "off", "shadow" or "apply".
	//
	// The default is off, and that is a decision rather than an oversight.
	// Shadow records what the classifier decided and changes no learner's
	// memory; apply changes states, and nobody has yet measured this
	// classifier on real sessions. A deployment opts in.
	EpisodeRelationMode             string `toml:"episodeRelationMode"`
	EpisodeRelationMaxCandidates    int    `toml:"episodeRelationMaxCandidates"`
	EpisodeRelationPromptVersion    string `toml:"episodeRelationPromptVersion"`
	EpisodeWorkerCount              int    `toml:"episodeWorkerCount"`
	EpisodeQueueSize                int    `toml:"episodeQueueSize"`
	EpisodeScanIntervalSeconds      int    `toml:"episodeScanIntervalSeconds"`
	EpisodeReconcileIntervalSeconds int    `toml:"episodeReconcileIntervalSeconds"`
	EpisodeTaskTimeoutSeconds       int    `toml:"episodeTaskTimeoutSeconds"`
	EpisodeLeaseSeconds             int    `toml:"episodeLeaseSeconds"`
	EpisodeRecoveryIntervalSeconds  int    `toml:"episodeRecoveryIntervalSeconds"`
	EpisodeRetryDelaySeconds        int    `toml:"episodeRetryDelaySeconds"`
	EpisodeQuietWindowSeconds       int    `toml:"episodeQuietWindowSeconds"`
	EpisodeMaxAttempts              int    `toml:"episodeMaxAttempts"`
	EpisodeMaxInputTokens           int    `toml:"episodeMaxInputTokens"`
	EpisodeMaxOutputTokens          int    `toml:"episodeMaxOutputTokens"`
	EpisodeMaxMessages              int    `toml:"episodeMaxMessages"`
}

type Config struct {
	EmailConfig        `toml:"emailConfig"`
	RedisConfig        `toml:"redisConfig"`
	MysqlConfig        `toml:"mysqlConfig"`
	JwtConfig          `toml:"jwtConfig"`
	MainConfig         `toml:"mainConfig"`
	Rabbitmq           `toml:"rabbitmqConfig"`
	RagModelConfig     `toml:"ragModelConfig"`
	VoiceServiceConfig `toml:"voiceServiceConfig"`
	KnowledgeConfig    `toml:"knowledgeConfig"`
	AgentConfig        `toml:"agentConfig"`
}

type RedisKeyConfig struct {
	CaptchaPrefix   string
	IndexName       string
	IndexNamePrefix string
}

var DefaultRedisKeyConfig = RedisKeyConfig{
	CaptchaPrefix:   "captcha:%s",
	IndexName:       "rag_docs:%s:idx",
	IndexNamePrefix: "rag_docs:%s:",
}

var config *Config

const mib int64 = 1024 * 1024

func defaultConfig() *Config {
	return &Config{JwtConfig: JwtConfig{
		ExpireDuration: 24,
		Issuer:         "gopherai",
		Subject:        "gopherai-user",
	}, RagModelConfig: RagModelConfig{
		RagEmbeddingModel: "qwen3.7-text-embedding", RagEmbeddingBaseURL: "https://dashscope.aliyuncs.com/compatible-mode/v1",
		RagChatModelName: "qwen3.8-max", RagBaseUrl: "https://dashscope.aliyuncs.com/compatible-mode/v1",
		RagDimension: 1024, RagRerankEnabled: false, RagRerankProvider: RerankProviderLocal, RagRerankEndpoint: "http://host.docker.internal:18081",
	}, KnowledgeConfig: KnowledgeConfig{
		StorageDir:          "data/knowledge",
		MaxFileSizeBytes:    5 * mib,
		MaxDocumentsPerUser: 100,
		MaxTotalSizeBytes:   50 * mib,
		WorkerCount:         2,
		QueueSize:           100,
		ScanIntervalSeconds: 5,
		IndexTimeoutSeconds: 300,
	}, AgentConfig: AgentConfig{
		MaxToolCalls: 3, MaxModelCalls: 4, MaxGraphSteps: 10, DeadlineSeconds: 45,
		KnowledgeDefaultTopK: 5, KnowledgeMaxTopK: 8, KnowledgeMaxResultTokens: 700,
		KnowledgeMaxObservationTokens: 2200, KnowledgeMaxRunTokens: 6500,
		DocumentReadMaxLines: 240, DocumentReadMaxBytes: 32 * 1024, DocumentReadMaxObservationTokens: 3000,
		ContextModelWindowTokens: 32768, ContextInputTargetTokens: 28000, ContextOutputReserveTokens: 4000,
		ContextMaxGlobalUserMemoryTokens: 800, ContextMaxTopicUserMemoryTokens: 1200, ContextMaxSummaryTokens: 2000, ContextMaxMasteryTokens: 900,
		ContextMaxEpisodeTokens: 1000, EpisodeRecallMode: "shadow_only",
		MemoryReadMode:           string(memoryrecall.DefaultReadMode()),
		ContextHistoryFetchLimit: 128,
		SummaryTriggerMessages:   12, SummaryTriggerTokens: 6000, SummaryRetainMessages: 8,
		SummaryMaxInputTokens: 8000, SummaryFetchLimit: 512, SummaryTimeoutSeconds: 20,
		SummaryStageBudgetSeconds: 8, SummaryFailureCooldownSeconds: 30,
		// Literals, not the service package's constants: config must not
		// import a service, and the service parses and validates this string
		// at assembly time rather than trusting it here.
		EpisodeRelationMode:          "off",
		EpisodeRelationMaxCandidates: 8,
		EpisodeRelationPromptVersion: "v1",
		EpisodeWorkerCount:           1, EpisodeQueueSize: 32, EpisodeScanIntervalSeconds: 3, EpisodeReconcileIntervalSeconds: 60,
		EpisodeTaskTimeoutSeconds: 30, EpisodeLeaseSeconds: 60, EpisodeRetryDelaySeconds: 15,
		EpisodeRecoveryIntervalSeconds: 30, EpisodeQuietWindowSeconds: 600,
		EpisodeMaxAttempts: 3, EpisodeMaxInputTokens: 12000, EpisodeMaxOutputTokens: 1800, EpisodeMaxMessages: 4096,
	}}
}

// InitConfig 初始化项目配置
func InitConfig() error {
	loaded, err := Load("config/config.toml")
	if err != nil {
		return err
	}
	config = loaded
	return nil
}

// LoadRagEmbedding reads only the RAG embedding parameters from a config file:
// defaults, TOML, the embedding environment overrides, then the RAG validation.
// It deliberately stops before the JWT and infrastructure sections, so an
// offline step that needs a model name, an endpoint and a dimension never has to
// hold or touch a credential it does not use.
func LoadRagEmbedding(path string) (RagModelConfig, error) {
	loaded := defaultConfig()
	if _, err := toml.DecodeFile(path, loaded); err != nil {
		return RagModelConfig{}, fmt.Errorf("load config %q: %w", path, err)
	}
	if err := applyRagEnv(&loaded.RagModelConfig); err != nil {
		return RagModelConfig{}, err
	}
	if err := loaded.RagModelConfig.Validate(); err != nil {
		return RagModelConfig{}, fmt.Errorf("invalid RAG config: %w", err)
	}
	return loaded.RagModelConfig, nil
}

// LoadEpisodeTokenBudget reads only the episodic context budget the product uses
// when it renders episodes into the window: defaults, TOML, the one environment
// override, then the same range validation. A harness that invents its own budget
// number measures a different prompt than the server would send.
func LoadEpisodeTokenBudget(path string) (int, error) {
	loaded := defaultConfig()
	if _, err := toml.DecodeFile(path, loaded); err != nil {
		return 0, fmt.Errorf("load config %q: %w", path, err)
	}
	if err := applyAgentEnv(&loaded.AgentConfig); err != nil {
		return 0, err
	}
	if err := loaded.AgentConfig.Validate(); err != nil {
		return 0, fmt.Errorf("invalid agent config: %w", err)
	}
	return loaded.AgentConfig.ContextMaxEpisodeTokens, nil
}

// Load builds a configuration in deterministic precedence order:
// safe defaults, TOML values, environment overrides, validation.
func Load(path string) (*Config, error) {
	loaded := defaultConfig()
	if _, err := toml.DecodeFile(path, loaded); err != nil {
		return nil, fmt.Errorf("load config %q: %w", path, err)
	}
	if err := applyInfrastructureEnv(loaded); err != nil {
		return nil, err
	}
	if err := applyRagEnv(&loaded.RagModelConfig); err != nil {
		return nil, err
	}
	if err := loaded.RagModelConfig.Validate(); err != nil {
		return nil, fmt.Errorf("invalid RAG config: %w", err)
	}
	if err := applyJWTEnv(&loaded.JwtConfig); err != nil {
		return nil, err
	}
	if err := loaded.JwtConfig.Validate(); err != nil {
		return nil, fmt.Errorf("invalid jwt config: %w", err)
	}
	if err := applyKnowledgeEnv(&loaded.KnowledgeConfig); err != nil {
		return nil, err
	}
	if err := loaded.KnowledgeConfig.Validate(); err != nil {
		return nil, fmt.Errorf("invalid knowledge config: %w", err)
	}
	if err := applyAgentEnv(&loaded.AgentConfig); err != nil {
		return nil, err
	}
	if err := loaded.AgentConfig.Validate(); err != nil {
		return nil, fmt.Errorf("invalid agent config: %w", err)
	}
	return loaded, nil
}

func applyRagEnv(c *RagModelConfig) error {
	if c == nil {
		return fmt.Errorf("RAG config must not be nil")
	}
	stringOverrides := []struct {
		name string
		set  func(string)
	}{
		{"OPENAI_MODEL_NAME", func(value string) { c.RagChatModelName = value }},
		{"OPENAI_BASE_URL", func(value string) { c.RagBaseUrl = value }},
		{"EMBEDDING_MODEL_NAME", func(value string) { c.RagEmbeddingModel = value }},
		{"EMBEDDING_BASE_URL", func(value string) { c.RagEmbeddingBaseURL = value }},
	}
	for _, override := range stringOverrides {
		if value, ok := os.LookupEnv(override.name); ok {
			value = strings.TrimSpace(value)
			if value == "" {
				return fmt.Errorf("%s must not be empty", override.name)
			}
			override.set(value)
		}
	}
	if value, ok := os.LookupEnv("RAG_RERANK_ENABLED"); ok {
		parsed, err := strconv.ParseBool(strings.TrimSpace(value))
		if err != nil {
			return fmt.Errorf("RAG_RERANK_ENABLED must be true or false")
		}
		c.RagRerankEnabled = parsed
	}
	if value, ok := os.LookupEnv("RAG_RERANK_ENDPOINT"); ok {
		value = strings.TrimSpace(value)
		if value == "" {
			return fmt.Errorf("RAG_RERANK_ENDPOINT must not be empty")
		}
		c.RagRerankEndpoint = value
	}
	return applyRerankTokenEnv(c)
}

// applyRerankTokenEnv reads the rerank bearer token from the environment.
//
// The token is the one credential that never lives in a configuration file: it
// is a secret, and the entry that owns the reranker needs it at runtime. Every
// other rerank setting - whether the reranker runs at all, which provider
// serves it, where it listens and which model it serves - stays in the
// configuration file.
//
// Validation errors name the variable but never echo its value.
func applyRerankTokenEnv(c *RagModelConfig) error {
	if c == nil {
		return fmt.Errorf("RAG config must not be nil")
	}
	if value, ok := os.LookupEnv("RAG_RERANK_AUTH_TOKEN"); ok {
		if value == "" {
			return fmt.Errorf("RAG_RERANK_AUTH_TOKEN must not be empty; set it in the environment, because the rerank token is never read from the configuration file")
		}
		c.RagRerankToken = value
	}
	return nil
}

func (c RagModelConfig) Validate() error {
	if strings.TrimSpace(c.RagChatModelName) == "" || strings.TrimSpace(c.RagBaseUrl) == "" {
		return fmt.Errorf("chat model name and base URL are required")
	}
	if strings.TrimSpace(c.RagEmbeddingModel) == "" || strings.TrimSpace(c.RagEmbeddingBaseURL) == "" || c.RagDimension <= 0 {
		return fmt.Errorf("embedding model name, base URL and positive dimension are required")
	}
	if !c.RagRerankEnabled {
		return nil
	}
	if strings.TrimSpace(c.RagRerankEndpoint) == "" {
		return fmt.Errorf("%w: rerankEndpoint is required when rerank is enabled", ErrRerankConfig)
	}
	switch strings.TrimSpace(c.RagRerankProvider) {
	case "", RerankProviderLocal:
		// A local server serves one frozen model, so the model name is optional.
	case RerankProviderDashScope:
		if strings.TrimSpace(c.RagRerankModel) == "" {
			return fmt.Errorf("%w: rerankModel is required when rerankProvider is %s", ErrRerankConfig, RerankProviderDashScope)
		}
	default:
		return fmt.Errorf("%w: unsupported rerankProvider; supported providers are %s and %s", ErrRerankConfig, RerankProviderLocal, RerankProviderDashScope)
	}
	if c.RagRerankTimeoutMs < 0 {
		return fmt.Errorf("%w: rerankTimeoutMs must not be negative", ErrRerankConfig)
	}
	if len([]byte(c.RagRerankToken)) < 32 {
		return fmt.Errorf("%w: RAG_RERANK_AUTH_TOKEN must contain at least 32 bytes; set it in the environment, because the rerank token is never read from the configuration file", ErrRerankConfig)
	}
	return nil
}

// RagRerankTimeout reports the bound for one rerank call.
//
// A configured value wins. Otherwise the bound follows the provider: a hosted
// endpoint answers in seconds, while a local cross-encoder on CPU needs a
// generous window or every call degrades into a timeout.
func (c RagModelConfig) RagRerankTimeout() time.Duration {
	if c.RagRerankTimeoutMs > 0 {
		return time.Duration(c.RagRerankTimeoutMs) * time.Millisecond
	}
	if strings.TrimSpace(c.RagRerankProvider) == RerankProviderDashScope {
		return defaultHostedRerankTimeout
	}
	return defaultLocalRerankTimeout
}

// applyInfrastructureEnv keeps deploy-time credentials out of the Docker TOML.
// Validation errors name the variable but never echo its potentially secret
// value.
func applyInfrastructureEnv(c *Config) error {
	if c == nil {
		return fmt.Errorf("infrastructure config must not be nil")
	}
	stringsOverrides := []struct {
		name   string
		secret bool
		set    func(string)
	}{
		{"MYSQL_HOST", false, func(value string) { c.MysqlHost = value }},
		{"MYSQL_USER", false, func(value string) { c.MysqlUser = value }},
		{"MYSQL_PASSWORD", true, func(value string) { c.MysqlPassword = value }},
		{"MYSQL_DATABASE", false, func(value string) { c.MysqlDatabaseName = value }},
		{"MYSQL_CHARSET", false, func(value string) { c.MysqlCharset = value }},
		{"REDIS_HOST", false, func(value string) { c.RedisHost = value }},
		{"REDIS_PASSWORD", true, func(value string) { c.RedisPassword = value }},
		{"RABBITMQ_HOST", false, func(value string) { c.RabbitmqHost = value }},
		{"RABBITMQ_USERNAME", false, func(value string) { c.RabbitmqUsername = value }},
		{"RABBITMQ_PASSWORD", true, func(value string) { c.RabbitmqPassword = value }},
		{"RABBITMQ_VHOST", false, func(value string) { c.RabbitmqVhost = value }},
	}
	for _, override := range stringsOverrides {
		value, ok := os.LookupEnv(override.name)
		if !ok {
			continue
		}
		if !override.secret {
			value = strings.TrimSpace(value)
		}
		if value == "" {
			return fmt.Errorf("%s must not be empty", override.name)
		}
		override.set(value)
	}

	integerOverrides := []struct {
		name string
		min  int
		max  int
		set  func(int)
	}{
		{"MYSQL_PORT", 1, 65535, func(value int) { c.MysqlPort = value }},
		{"REDIS_PORT", 1, 65535, func(value int) { c.RedisPort = value }},
		{"REDIS_DB", 0, 15, func(value int) { c.RedisDb = value }},
		{"RABBITMQ_PORT", 1, 65535, func(value int) { c.RabbitmqPort = value }},
	}
	for _, override := range integerOverrides {
		value, ok := os.LookupEnv(override.name)
		if !ok {
			continue
		}
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < override.min || parsed > override.max {
			return fmt.Errorf("%s must be an integer between %d and %d", override.name, override.min, override.max)
		}
		override.set(parsed)
	}
	return nil
}

func applyJWTEnv(c *JwtConfig) error {
	if c == nil {
		return fmt.Errorf("jwt config must not be nil")
	}
	if value, ok := os.LookupEnv("JWT_SECRET"); ok {
		c.Key = value
	}
	if value, ok := os.LookupEnv("JWT_ISSUER"); ok {
		c.Issuer = strings.TrimSpace(value)
	}
	if value, ok := os.LookupEnv("JWT_SUBJECT"); ok {
		c.Subject = strings.TrimSpace(value)
	}
	if value, ok := os.LookupEnv("JWT_EXPIRE_HOURS"); ok {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("invalid JWT_EXPIRE_HOURS=%q: must be an integer", value)
		}
		c.ExpireDuration = parsed
	}
	return nil
}

func (c JwtConfig) Validate() error {
	if len([]byte(c.Key)) < 32 {
		return fmt.Errorf("JWT_SECRET must contain at least 32 bytes")
	}
	if c.Key == "GopherAI-v1" {
		return fmt.Errorf("the retired default JWT secret is forbidden")
	}
	if c.ExpireDuration < 1 || c.ExpireDuration > 168 {
		return fmt.Errorf("expire_duration must be between 1 and 168 hours")
	}
	if strings.TrimSpace(c.Issuer) == "" || strings.TrimSpace(c.Subject) == "" {
		return fmt.Errorf("issuer and subject must not be empty")
	}
	return nil
}

func applyAgentEnv(c *AgentConfig) error {
	if value, ok := os.LookupEnv("AGENT_EPISODE_RECALL_MODE"); ok {
		c.EpisodeRecallMode = strings.TrimSpace(value)
	}
	if value, ok := os.LookupEnv("AGENT_MEMORY_READ_MODE"); ok {
		c.MemoryReadMode = strings.TrimSpace(value)
	}
	if value, ok := os.LookupEnv("AGENT_TOPIC_CONSENT_SECRET"); ok {
		c.TopicConsentSecret = strings.TrimSpace(value)
	}
	for name, set := range map[string]func(string){
		"MEMORY_PLANNER_MODEL":    func(value string) { c.MemoryPlannerModel = value },
		"MEMORY_PLANNER_BASE_URL": func(value string) { c.MemoryPlannerBaseURL = value },
	} {
		if value, ok := os.LookupEnv(name); ok {
			set(strings.TrimSpace(value))
		}
	}
	for _, override := range []struct {
		name string
		set  func(int)
	}{
		{"MEMORY_PLANNER_TIMEOUT_MS", func(value int) { c.MemoryPlannerTimeoutMS = value }},
		{"MEMORY_VERIFIER_TIMEOUT_MS", func(value int) { c.MemoryVerifierTimeoutMS = value }},
		{"MEMORY_PLANNER_MAX_ATTEMPTS", func(value int) { c.MemoryPlannerMaxAttempts = value }},
		{"MEMORY_PLANNER_MAX_CONCURRENCY", func(value int) { c.MemoryPlannerMaxConcurrency = value }},
		{"MEMORY_TOOL_MAX_CALLS", func(value int) { c.MemoryToolMaxCalls = value }},
		{"MEMORY_TOOL_TOP_K", func(value int) { c.MemoryToolTopK = value }},
		{"MEMORY_TOOL_CANDIDATE_DEPTH", func(value int) { c.MemoryToolCandidateDepth = value }},
		{"MEMORY_TOOL_TIMEOUT_MS", func(value int) { c.MemoryToolTimeoutMS = value }},
	} {
		raw, ok := os.LookupEnv(override.name)
		if !ok {
			continue
		}
		parsed, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil {
			return fmt.Errorf("%s must be an integer: %w", override.name, err)
		}
		override.set(parsed)
	}
	for _, override := range []struct {
		name string
		set  func(float64)
	}{
		{"AGENT_MEMORY_TOPIC_STANDOUT", func(value float64) { c.MemoryTopicStandout = value }},
		{"AGENT_MEMORY_TOPIC_MARGIN", func(value float64) { c.MemoryTopicMargin = value }},
		{"MEMORY_TOOL_MIN_CONFIDENCE", func(value float64) { c.MemoryToolMinConfidence = value }},
	} {
		raw, ok := os.LookupEnv(override.name)
		if !ok {
			continue
		}
		parsed, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
		if err != nil {
			return fmt.Errorf("%s must be a number: %w", override.name, err)
		}
		override.set(parsed)
	}
	overrides := []struct {
		name string
		set  func(int)
	}{
		{"AGENT_MAX_TOOL_CALLS", func(value int) { c.MaxToolCalls = value }},
		{"AGENT_MAX_MODEL_CALLS", func(value int) { c.MaxModelCalls = value }},
		{"AGENT_MAX_GRAPH_STEPS", func(value int) { c.MaxGraphSteps = value }},
		{"AGENT_DEADLINE_SECONDS", func(value int) { c.DeadlineSeconds = value }},
		{"AGENT_KNOWLEDGE_DEFAULT_TOP_K", func(value int) { c.KnowledgeDefaultTopK = value }},
		{"AGENT_KNOWLEDGE_MAX_TOP_K", func(value int) { c.KnowledgeMaxTopK = value }},
		{"AGENT_KNOWLEDGE_MAX_RESULT_TOKENS", func(value int) { c.KnowledgeMaxResultTokens = value }},
		{"AGENT_KNOWLEDGE_MAX_OBSERVATION_TOKENS", func(value int) { c.KnowledgeMaxObservationTokens = value }},
		{"AGENT_KNOWLEDGE_MAX_RUN_TOKENS", func(value int) { c.KnowledgeMaxRunTokens = value }},
		{"AGENT_DOCUMENT_READ_MAX_LINES", func(value int) { c.DocumentReadMaxLines = value }},
		{"AGENT_DOCUMENT_READ_MAX_BYTES", func(value int) { c.DocumentReadMaxBytes = value }},
		{"AGENT_DOCUMENT_READ_MAX_OBSERVATION_TOKENS", func(value int) { c.DocumentReadMaxObservationTokens = value }},
		{"AGENT_CONTEXT_MODEL_WINDOW_TOKENS", func(value int) { c.ContextModelWindowTokens = value }},
		{"AGENT_CONTEXT_INPUT_TARGET_TOKENS", func(value int) { c.ContextInputTargetTokens = value }},
		{"AGENT_CONTEXT_OUTPUT_RESERVE_TOKENS", func(value int) { c.ContextOutputReserveTokens = value }},
		{"AGENT_CONTEXT_MAX_GLOBAL_USER_MEMORY_TOKENS", func(value int) { c.ContextMaxGlobalUserMemoryTokens = value }},
		{"AGENT_CONTEXT_MAX_TOPIC_USER_MEMORY_TOKENS", func(value int) { c.ContextMaxTopicUserMemoryTokens = value }},
		{"AGENT_CONTEXT_MAX_SUMMARY_TOKENS", func(value int) { c.ContextMaxSummaryTokens = value }},
		{"AGENT_CONTEXT_MAX_MASTERY_TOKENS", func(value int) { c.ContextMaxMasteryTokens = value }},
		{"AGENT_CONTEXT_MAX_EPISODE_TOKENS", func(value int) { c.ContextMaxEpisodeTokens = value }},
		{"AGENT_CONTEXT_HISTORY_FETCH_LIMIT", func(value int) { c.ContextHistoryFetchLimit = value }},
		{"AGENT_SUMMARY_TRIGGER_MESSAGES", func(value int) { c.SummaryTriggerMessages = value }},
		{"AGENT_SUMMARY_TRIGGER_TOKENS", func(value int) { c.SummaryTriggerTokens = value }},
		{"AGENT_SUMMARY_RETAIN_MESSAGES", func(value int) { c.SummaryRetainMessages = value }},
		{"AGENT_SUMMARY_MAX_INPUT_TOKENS", func(value int) { c.SummaryMaxInputTokens = value }},
		{"AGENT_SUMMARY_FETCH_LIMIT", func(value int) { c.SummaryFetchLimit = value }},
		{"AGENT_SUMMARY_TIMEOUT_SECONDS", func(value int) { c.SummaryTimeoutSeconds = value }},
		{"AGENT_SUMMARY_STAGE_BUDGET_SECONDS", func(value int) { c.SummaryStageBudgetSeconds = value }},
		{"AGENT_SUMMARY_FAILURE_COOLDOWN_SECONDS", func(value int) { c.SummaryFailureCooldownSeconds = value }},
		{"AGENT_EPISODE_RELATION_MAX_CANDIDATES", func(value int) { c.EpisodeRelationMaxCandidates = value }},
		{"AGENT_EPISODE_WORKER_COUNT", func(value int) { c.EpisodeWorkerCount = value }},
		{"AGENT_EPISODE_QUEUE_SIZE", func(value int) { c.EpisodeQueueSize = value }},
		{"AGENT_EPISODE_SCAN_INTERVAL_SECONDS", func(value int) { c.EpisodeScanIntervalSeconds = value }},
		{"AGENT_EPISODE_RECONCILE_INTERVAL_SECONDS", func(value int) { c.EpisodeReconcileIntervalSeconds = value }},
		{"AGENT_EPISODE_TASK_TIMEOUT_SECONDS", func(value int) { c.EpisodeTaskTimeoutSeconds = value }},
		{"AGENT_EPISODE_LEASE_SECONDS", func(value int) { c.EpisodeLeaseSeconds = value }},
		{"AGENT_EPISODE_RECOVERY_INTERVAL_SECONDS", func(value int) { c.EpisodeRecoveryIntervalSeconds = value }},
		{"AGENT_EPISODE_RETRY_DELAY_SECONDS", func(value int) { c.EpisodeRetryDelaySeconds = value }},
		{"AGENT_EPISODE_QUIET_WINDOW_SECONDS", func(value int) { c.EpisodeQuietWindowSeconds = value }},
		{"AGENT_EPISODE_MAX_ATTEMPTS", func(value int) { c.EpisodeMaxAttempts = value }},
		{"AGENT_EPISODE_MAX_INPUT_TOKENS", func(value int) { c.EpisodeMaxInputTokens = value }},
		{"AGENT_EPISODE_MAX_OUTPUT_TOKENS", func(value int) { c.EpisodeMaxOutputTokens = value }},
		{"AGENT_EPISODE_MAX_MESSAGES", func(value int) { c.EpisodeMaxMessages = value }},
	}
	for _, override := range overrides {
		value, ok := os.LookupEnv(override.name)
		if !ok {
			continue
		}
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("invalid %s=%q: must be an integer", override.name, value)
		}
		override.set(parsed)
	}
	return nil
}

func (c AgentConfig) Validate() error {
	if c.MaxToolCalls < 0 || c.MaxToolCalls > 8 {
		return fmt.Errorf("maxToolCalls must be between 0 and 8")
	}
	if c.MaxModelCalls < 1 || c.MaxModelCalls > 10 {
		return fmt.Errorf("maxModelCalls must be between 1 and 10")
	}
	if c.MaxGraphSteps < 2 || c.MaxGraphSteps > 32 {
		return fmt.Errorf("maxGraphSteps must be between 2 and 32")
	}
	if c.DeadlineSeconds < 1 || c.DeadlineSeconds > 120 {
		return fmt.Errorf("deadlineSeconds must be between 1 and 120")
	}
	if c.KnowledgeDefaultTopK < 1 || c.KnowledgeMaxTopK < c.KnowledgeDefaultTopK || c.KnowledgeMaxTopK > 20 {
		return fmt.Errorf("knowledge top_k must satisfy 1 <= default <= max <= 20")
	}
	if c.KnowledgeMaxResultTokens < 32 || c.KnowledgeMaxResultTokens > 4000 {
		return fmt.Errorf("knowledgeMaxResultTokens must be between 32 and 4000")
	}
	if c.KnowledgeMaxObservationTokens < 256 || c.KnowledgeMaxObservationTokens > 8000 {
		return fmt.Errorf("knowledgeMaxObservationTokens must be between 256 and 8000")
	}
	if c.KnowledgeMaxRunTokens < c.KnowledgeMaxObservationTokens || c.KnowledgeMaxRunTokens > 20000 {
		return fmt.Errorf("knowledgeMaxRunTokens must be between the observation budget and 20000")
	}
	if c.MaxToolCalls > 0 && c.KnowledgeMaxRunTokens < c.MaxToolCalls*128 {
		return fmt.Errorf("knowledgeMaxRunTokens cannot reserve minimal observations for maxToolCalls")
	}
	if c.DocumentReadMaxLines < 20 || c.DocumentReadMaxLines > 1000 {
		return fmt.Errorf("documentReadMaxLines must be between 20 and 1000")
	}
	if c.DocumentReadMaxBytes < 1024 || c.DocumentReadMaxBytes > 256*1024 {
		return fmt.Errorf("documentReadMaxBytes must be between 1024 and 262144")
	}
	if c.DocumentReadMaxObservationTokens < 256 || c.DocumentReadMaxObservationTokens > 8000 {
		return fmt.Errorf("documentReadMaxObservationTokens must be between 256 and 8000")
	}
	if c.DocumentReadMaxObservationTokens > c.KnowledgeMaxRunTokens {
		return fmt.Errorf("documentReadMaxObservationTokens cannot exceed knowledgeMaxRunTokens")
	}
	if c.ContextModelWindowTokens < 8192 || c.ContextModelWindowTokens > 1_048_576 {
		return fmt.Errorf("contextModelWindowTokens must be between 8192 and 1048576")
	}
	if c.ContextInputTargetTokens < 2048 || c.ContextInputTargetTokens > c.ContextModelWindowTokens {
		return fmt.Errorf("contextInputTargetTokens must be between 2048 and contextModelWindowTokens")
	}
	if c.ContextOutputReserveTokens < 256 || c.ContextInputTargetTokens+c.ContextOutputReserveTokens > c.ContextModelWindowTokens {
		return fmt.Errorf("context input target plus output reserve exceeds model window")
	}
	if c.KnowledgeMaxRunTokens >= c.ContextInputTargetTokens-1024 {
		return fmt.Errorf("knowledgeMaxRunTokens must leave at least 1024 input tokens for required context")
	}
	if c.EpisodeRecallMode != "disabled" && c.EpisodeRecallMode != "shadow_only" && c.EpisodeRecallMode != "inject_enabled" {
		return fmt.Errorf("episodeRecallMode must be disabled, shadow_only or inject_enabled")
	}
	mode, err := memoryrecall.ParseReadMode(c.MemoryReadMode)
	if err != nil {
		return err
	}
	// The semantic gate has no production default. Enabling the mode without
	// both numbers must stop startup rather than pick something.
	if mode.UsesSemanticTopicLayer() {
		if c.MemoryTopicStandout < 1 || c.MemoryTopicStandout > 3 {
			return fmt.Errorf("memoryTopicStandout must be set between 1 and 3 when memoryReadMode is %s", mode)
		}
		if c.MemoryTopicMargin <= 0 || c.MemoryTopicMargin > 0.5 {
			return fmt.Errorf("memoryTopicMargin must be set above 0 and at most 0.5 when memoryReadMode is %s", mode)
		}
	} else if c.MemoryTopicStandout != 0 || c.MemoryTopicMargin != 0 {
		// Carrying gate numbers in a mode that cannot use them is how a value
		// nobody reviewed becomes live the day someone flips the mode.
		return fmt.Errorf("memoryTopicStandout and memoryTopicMargin must be unset unless memoryReadMode is episode_v3_semantic")
	}
	if err := c.validateMemoryPlanner(mode); err != nil {
		return err
	}
	if err := c.validateMemoryTool(mode); err != nil {
		return err
	}
	if err := c.validateTopicConsent(mode); err != nil {
		return err
	}
	if c.ContextMaxEpisodeTokens < 800 || c.ContextMaxEpisodeTokens > 1200 {
		return fmt.Errorf("contextMaxEpisodeTokens must be between 800 and 1200")
	}
	if c.ContextMaxGlobalUserMemoryTokens < 0 || c.ContextMaxTopicUserMemoryTokens < 0 || c.ContextMaxSummaryTokens < 0 || c.ContextMaxMasteryTokens < 0 ||
		c.ContextMaxGlobalUserMemoryTokens+c.ContextMaxTopicUserMemoryTokens+c.ContextMaxSummaryTokens+c.ContextMaxMasteryTokens+c.ContextMaxEpisodeTokens >= c.ContextInputTargetTokens-c.KnowledgeMaxRunTokens {
		return fmt.Errorf("context profile, topic note, episode, summary and mastery budgets leave no room for required context")
	}
	if c.ContextMaxMasteryTokens > 2000 {
		return fmt.Errorf("contextMaxMasteryTokens must not exceed 2000")
	}
	if c.ContextMaxSummaryTokens < 128 || c.ContextMaxSummaryTokens > 8000 {
		return fmt.Errorf("contextMaxSummaryTokens must be between 128 and 8000")
	}
	if c.ContextHistoryFetchLimit < 1 || c.ContextHistoryFetchLimit > 512 {
		return fmt.Errorf("contextHistoryFetchLimit must be between 1 and 512")
	}
	if c.SummaryTriggerMessages < 2 || c.SummaryTriggerMessages > 200 {
		return fmt.Errorf("summaryTriggerMessages must be between 2 and 200")
	}
	if c.SummaryTriggerTokens < 256 || c.SummaryTriggerTokens > 100000 {
		return fmt.Errorf("summaryTriggerTokens must be between 256 and 100000")
	}
	if c.SummaryRetainMessages < 1 || c.SummaryRetainMessages >= c.SummaryTriggerMessages {
		return fmt.Errorf("summaryRetainMessages must be positive and below summaryTriggerMessages")
	}
	if c.SummaryMaxInputTokens < 512 || c.SummaryMaxInputTokens > c.ContextInputTargetTokens {
		return fmt.Errorf("summaryMaxInputTokens must be between 512 and contextInputTargetTokens")
	}
	if c.SummaryFetchLimit < c.SummaryTriggerMessages || c.SummaryFetchLimit > 1024 {
		return fmt.Errorf("summaryFetchLimit must cover summaryTriggerMessages and be at most 1024")
	}
	if c.SummaryTimeoutSeconds < 1 || c.SummaryTimeoutSeconds > 120 {
		return fmt.Errorf("summaryTimeoutSeconds must be between 1 and 120")
	}
	// The stage budget is what one request may actually spend on compaction.
	// It has to stay under the model timeout, and well under the agent deadline
	// it is carved out of, or the summary can consume the answer's budget.
	if c.SummaryStageBudgetSeconds < 1 || c.SummaryStageBudgetSeconds > c.SummaryTimeoutSeconds {
		return fmt.Errorf("summaryStageBudgetSeconds must be between 1 and summaryTimeoutSeconds")
	}
	if c.SummaryStageBudgetSeconds*3 >= c.DeadlineSeconds {
		return fmt.Errorf("summaryStageBudgetSeconds must leave the agent deadline room for its own model calls")
	}
	if c.SummaryFailureCooldownSeconds < 1 || c.SummaryFailureCooldownSeconds > 600 {
		return fmt.Errorf("summaryFailureCooldownSeconds must be between 1 and 600")
	}
	if c.EpisodeWorkerCount < 1 || c.EpisodeWorkerCount > 16 || c.EpisodeQueueSize < c.EpisodeWorkerCount || c.EpisodeQueueSize > 1000 {
		return fmt.Errorf("episode worker and queue sizes are invalid")
	}
	if c.EpisodeScanIntervalSeconds < 1 || c.EpisodeScanIntervalSeconds > 300 || c.EpisodeReconcileIntervalSeconds < 5 || c.EpisodeReconcileIntervalSeconds > 3600 {
		return fmt.Errorf("episode scan intervals are invalid")
	}
	// Recovery has to run more often than a lease lasts, or a stranded job
	// waits most of a lease before anything looks at it.
	if c.EpisodeRecoveryIntervalSeconds < 5 || c.EpisodeRecoveryIntervalSeconds > c.EpisodeLeaseSeconds {
		return fmt.Errorf("episodeRecoveryIntervalSeconds must be between 5 and episodeLeaseSeconds")
	}
	if c.EpisodeTaskTimeoutSeconds < 1 || c.EpisodeTaskTimeoutSeconds > 120 || c.EpisodeLeaseSeconds <= c.EpisodeTaskTimeoutSeconds || c.EpisodeLeaseSeconds > 600 {
		return fmt.Errorf("episode task timeout and lease are invalid")
	}
	if c.EpisodeRetryDelaySeconds < 1 || c.EpisodeRetryDelaySeconds > 3600 || c.EpisodeQuietWindowSeconds < 0 || c.EpisodeQuietWindowSeconds > 86400 || c.EpisodeMaxAttempts < 1 || c.EpisodeMaxAttempts > 10 {
		return fmt.Errorf("episode retry, quiet window or attempt configuration is invalid")
	}
	if c.EpisodeMaxInputTokens < 1024 || c.EpisodeMaxInputTokens > 100000 || c.EpisodeMaxOutputTokens < 256 || c.EpisodeMaxOutputTokens > 8000 || c.EpisodeMaxMessages < 32 || c.EpisodeMaxMessages > 20000 {
		return fmt.Errorf("episode model or evidence limits are invalid")
	}
	return nil
}

func applyKnowledgeEnv(c *KnowledgeConfig) error {
	if value, ok := os.LookupEnv("KNOWLEDGE_STORAGE_DIR"); ok {
		c.StorageDir = value
	}
	integerOverrides := []struct {
		name string
		set  func(int64)
	}{
		{"KNOWLEDGE_MAX_FILE_SIZE_BYTES", func(v int64) { c.MaxFileSizeBytes = v }},
		{"KNOWLEDGE_MAX_DOCUMENTS_PER_USER", func(v int64) { c.MaxDocumentsPerUser = int(v) }},
		{"KNOWLEDGE_MAX_TOTAL_SIZE_BYTES", func(v int64) { c.MaxTotalSizeBytes = v }},
		{"KNOWLEDGE_WORKER_COUNT", func(v int64) { c.WorkerCount = int(v) }},
		{"KNOWLEDGE_QUEUE_SIZE", func(v int64) { c.QueueSize = int(v) }},
		{"KNOWLEDGE_SCAN_INTERVAL_SECONDS", func(v int64) { c.ScanIntervalSeconds = int(v) }},
		{"KNOWLEDGE_INDEX_TIMEOUT_SECONDS", func(v int64) { c.IndexTimeoutSeconds = int(v) }},
	}
	for _, override := range integerOverrides {
		value, ok := os.LookupEnv(override.name)
		if !ok {
			continue
		}
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid %s=%q: must be an integer", override.name, value)
		}
		override.set(parsed)
	}
	return nil
}

func (c KnowledgeConfig) Validate() error {
	if c.StorageDir == "" {
		return fmt.Errorf("storageDir must not be empty")
	}
	if c.MaxFileSizeBytes <= 0 {
		return fmt.Errorf("maxFileSizeBytes must be greater than zero")
	}
	if c.MaxDocumentsPerUser <= 0 {
		return fmt.Errorf("maxDocumentsPerUser must be greater than zero")
	}
	if c.MaxTotalSizeBytes < c.MaxFileSizeBytes {
		return fmt.Errorf("maxTotalSizeBytes must be at least maxFileSizeBytes")
	}
	if c.WorkerCount <= 0 {
		return fmt.Errorf("workerCount must be greater than zero")
	}
	if c.QueueSize < c.WorkerCount {
		return fmt.Errorf("queueSize must be at least workerCount")
	}
	if c.ScanIntervalSeconds <= 0 {
		return fmt.Errorf("scanIntervalSeconds must be greater than zero")
	}
	if c.IndexTimeoutSeconds <= 0 {
		return fmt.Errorf("indexTimeoutSeconds must be greater than zero")
	}
	return c.ValidateExecutionMode()
}

// ValidateExecutionMode checks only the execution mode, so a caller can refuse a
// misconfigured mode before it opens any connection.
//
// This build processes documents in-process. A configuration that still names a
// remote executor is refused on purpose: silently running it locally would
// answer with a different pipeline than the operator asked for.
func (c KnowledgeConfig) ValidateExecutionMode() error {
	switch c.ExecutionMode {
	case "", ExecutionModeLegacy:
		return nil
	default:
		return fmt.Errorf("executionMode must be empty or %q: this build processes documents in-process only", ExecutionModeLegacy)
	}
}

// Use installs an already-loaded configuration as the process-wide one.
//
// Load alone does not do this, so a command that loads an alternative file and
// then calls a package relying on GetConfig would silently run against
// config/config.toml instead. Operational commands need to point at a different
// host or port than the container defaults without editing the checked-in file.
func Use(loaded *Config) error {
	if loaded == nil {
		return fmt.Errorf("configuration must not be nil")
	}
	config = loaded
	return nil
}

func GetConfig() *Config {
	if config == nil {
		if err := InitConfig(); err != nil {
			panic(fmt.Sprintf("initialize config: %v", err))
		}
	}
	return config
}

// Current returns the configuration that has been installed, or nil when none
// has been installed yet. Unlike GetConfig it never panics and never reads
// config/config.toml, so a caller that runs before the configuration is
// installed can tell that from a configured-but-empty section.
func Current() *Config { return config }

// validateMemoryTool refuses an incomplete agent memory tool mode and refuses a
// configured memory tool under any other mode.
//
// Every agent memory tool mode registers memory_search and runs at the wider
// budget, so all of them are validated by the same rules. What separates them
// is what the ContextBuilder injects before the first model call and which
// second tool the run registers, neither of which is a configuration question
// and neither of which is decided here.
//
// The run budget is part of it. The agent memory tool modes are the only ones whose
// tool ceiling is six, and the six exists because six serial tool rounds need
// seven model generations; a deployment that selected the mode and left the
// budget at three would register a tool the model can call twice at most, then
// report the resulting recall as the method's. Every other mode must stay at
// the pre-STABILIZE-033 ceiling for the opposite reason: their measurements
// were taken there.
func (c AgentConfig) validateMemoryTool(mode memoryrecall.ReadMode) error {
	if !mode.UsesMemorySearchTool() {
		if c.MemoryToolMaxCalls != 0 || c.MemoryToolTopK != 0 || c.MemoryToolCandidateDepth != 0 ||
			c.MemoryToolMinConfidence != 0 || c.MemoryToolTimeoutMS != 0 ||
			strings.TrimSpace(c.MemoryToolWindowPolicy) != "" {
			return fmt.Errorf("memoryTool* settings must be unset unless memoryReadMode is an agent memory tool mode")
		}
		if c.MaxToolCalls > 3 || c.MaxModelCalls > 4 || c.MaxGraphSteps > 10 {
			return fmt.Errorf("maxToolCalls, maxModelCalls and maxGraphSteps must not exceed 3, 4 and 10 unless memoryReadMode is an agent memory tool mode")
		}
		return nil
	}
	if c.MemoryToolMaxCalls < 1 || c.MemoryToolMaxCalls > 2 {
		return fmt.Errorf("memoryToolMaxCalls must be 1 or 2 when memoryReadMode is %s", mode)
	}
	if c.MemoryToolTopK < 1 || c.MemoryToolTopK > 6 {
		return fmt.Errorf("memoryToolTopK must be between 1 and 6 when memoryReadMode is %s", mode)
	}
	if c.MemoryToolCandidateDepth < 5 || c.MemoryToolCandidateDepth > 100 {
		return fmt.Errorf("memoryToolCandidateDepth must be between 5 and 100 when memoryReadMode is %s", mode)
	}
	if c.MemoryToolMinConfidence <= 0 || c.MemoryToolMinConfidence > 1 {
		return fmt.Errorf("memoryToolMinConfidence must be above 0 and at most 1 when memoryReadMode is %s", mode)
	}
	if c.MemoryToolTimeoutMS < 200 || c.MemoryToolTimeoutMS > 10000 {
		return fmt.Errorf("memoryToolTimeoutMs must be between 200 and 10000 when memoryReadMode is %s", mode)
	}
	if c.MaxToolCalls != 6 || c.MaxModelCalls != 7 || c.MaxGraphSteps != 16 {
		return fmt.Errorf("%s runs at maxToolCalls 6, maxModelCalls 7 and maxGraphSteps 16", mode)
	}
	if c.DeadlineSeconds != 45 {
		return fmt.Errorf("%s keeps the 45 second run deadline", mode)
	}
	// An unknown policy stops startup rather than falling back to the shipped
	// window. A deployment that misspelled the experiment and got the baseline
	// would report the baseline's behaviour under the experiment's name.
	if !memoryrecall.WindowPolicy(strings.TrimSpace(c.MemoryToolWindowPolicy)).Valid() {
		return fmt.Errorf("memoryToolWindowPolicy must be empty, w0_fixed or token_bounded_mixed_v1")
	}
	return nil
}

// validateMemoryPlanner refuses an incomplete model_planned_v1 and refuses a
// configured planner under any other mode.
//
// Both directions matter. An incomplete configuration must stop startup rather
// than silently fall back to another mode, because a mode nobody selected
// running in production is exactly what STABILIZE-031 found. And a complete
// configuration under a mode that cannot use it is a credential and an endpoint
// sitting live for the day someone flips a string.
// validateTopicConsent keeps the signing key out of every mode that cannot use
// it, and refuses to start the one mode that needs it without one.
//
// Both halves matter. A missing key under the confirmed mode would mean either
// an unsigned ticket or a silently disabled confirmation, and a key configured
// under a mode that never verifies one is a secret sitting in production for no
// reason, waiting for the day somebody flips the mode without re-reading it.
func (c AgentConfig) validateTopicConsent(mode memoryrecall.ReadMode) error {
	if !mode.UsesUserConfirmedTopicContext() {
		if strings.TrimSpace(c.TopicConsentSecret) != "" || c.TopicConsentTTLMinutes != 0 || c.TopicConsentMaxCandidates != 0 {
			return fmt.Errorf("topicConsent* settings must be unset unless memoryReadMode is agent_memory_user_confirmed_v1")
		}
		return nil
	}
	if len(strings.TrimSpace(c.TopicConsentSecret)) < memoryrecall.TopicConsentMinSecretLength {
		return fmt.Errorf("topicConsentSecret must be at least %d characters when memoryReadMode is %s",
			memoryrecall.TopicConsentMinSecretLength, mode)
	}
	if c.TopicConsentTTLMinutes < 0 || c.TopicConsentTTLMinutes > 60 {
		return fmt.Errorf("topicConsentTTLMinutes must be between 0 and 60 when memoryReadMode is %s", mode)
	}
	if c.TopicConsentMaxCandidates < 0 || c.TopicConsentMaxCandidates > memoryrecall.TopicConsentMaxCandidates {
		return fmt.Errorf("topicConsentMaxCandidates must be between 0 and %d when memoryReadMode is %s",
			memoryrecall.TopicConsentMaxCandidates, mode)
	}
	return nil
}

func (c AgentConfig) validateMemoryPlanner(mode memoryrecall.ReadMode) error {
	if !mode.UsesModelPlanner() {
		if strings.TrimSpace(c.MemoryPlannerModel) != "" || strings.TrimSpace(c.MemoryPlannerBaseURL) != "" ||
			c.MemoryPlannerTimeoutMS != 0 || c.MemoryVerifierTimeoutMS != 0 ||
			c.MemoryPlannerMaxAttempts != 0 || c.MemoryPlannerMaxConcurrency != 0 {
			return fmt.Errorf("memoryPlanner* settings must be unset unless memoryReadMode is model_planned_v1")
		}
		return nil
	}
	if strings.TrimSpace(c.MemoryPlannerModel) == "" {
		return fmt.Errorf("memoryPlannerModel must be set when memoryReadMode is model_planned_v1")
	}
	if strings.TrimSpace(c.MemoryPlannerBaseURL) == "" {
		return fmt.Errorf("memoryPlannerBaseURL must be set when memoryReadMode is model_planned_v1")
	}
	if c.MemoryPlannerTimeoutMS < 200 || c.MemoryPlannerTimeoutMS > 30000 {
		return fmt.Errorf("memoryPlannerTimeoutMs must be between 200 and 30000")
	}
	if c.MemoryVerifierTimeoutMS < 200 || c.MemoryVerifierTimeoutMS > 30000 {
		return fmt.Errorf("memoryVerifierTimeoutMs must be between 200 and 30000")
	}
	if c.MemoryPlannerMaxAttempts < 1 || c.MemoryPlannerMaxAttempts > 2 {
		return fmt.Errorf("memoryPlannerMaxAttempts must be 1 or 2")
	}
	if c.MemoryPlannerMaxConcurrency < 1 || c.MemoryPlannerMaxConcurrency > 64 {
		return fmt.Errorf("memoryPlannerMaxConcurrency must be between 1 and 64")
	}
	// The key is the one setting that never appears in configuration. It is
	// checked here so an incomplete deployment fails at startup rather than on
	// the first learner question.
	if strings.TrimSpace(os.Getenv("MEMORY_PLANNER_API_KEY")) == "" {
		return fmt.Errorf("MEMORY_PLANNER_API_KEY must be set in the environment when memoryReadMode is model_planned_v1")
	}
	return nil
}
