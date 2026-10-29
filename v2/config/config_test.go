package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInfrastructureEnvironmentOverridesDockerValues(t *testing.T) {
	cfg := &Config{
		MysqlConfig: MysqlConfig{MysqlHost: "old-mysql", MysqlPort: 3306, MysqlUser: "root", MysqlPassword: "old", MysqlDatabaseName: "old", MysqlCharset: "utf8"},
		RedisConfig: RedisConfig{RedisHost: "old-redis", RedisPort: 6379, RedisPassword: "old", RedisDb: 0},
		Rabbitmq:    Rabbitmq{RabbitmqHost: "old-rabbit", RabbitmqPort: 5672, RabbitmqUsername: "root", RabbitmqPassword: "old", RabbitmqVhost: "/"},
	}
	t.Setenv("MYSQL_HOST", "mysql")
	t.Setenv("MYSQL_PORT", "3307")
	t.Setenv("MYSQL_USER", "gopherai")
	t.Setenv("MYSQL_PASSWORD", "mysql-secret")
	t.Setenv("MYSQL_DATABASE", "GopherAI")
	t.Setenv("MYSQL_CHARSET", "utf8mb4")
	t.Setenv("REDIS_HOST", "redis-vector")
	t.Setenv("REDIS_PORT", "6380")
	t.Setenv("REDIS_PASSWORD", "redis-secret")
	t.Setenv("REDIS_DB", "2")
	t.Setenv("RABBITMQ_HOST", "rabbitmq")
	t.Setenv("RABBITMQ_PORT", "5673")
	t.Setenv("RABBITMQ_USERNAME", "gopherai")
	t.Setenv("RABBITMQ_PASSWORD", "rabbit-secret")
	t.Setenv("RABBITMQ_VHOST", "/gopherai")

	if err := applyInfrastructureEnv(cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.MysqlHost != "mysql" || cfg.MysqlPort != 3307 || cfg.MysqlUser != "gopherai" || cfg.MysqlPassword != "mysql-secret" || cfg.MysqlDatabaseName != "GopherAI" || cfg.MysqlCharset != "utf8mb4" {
		t.Fatalf("mysql overrides not applied: %+v", cfg.MysqlConfig)
	}
	if cfg.RedisHost != "redis-vector" || cfg.RedisPort != 6380 || cfg.RedisPassword != "redis-secret" || cfg.RedisDb != 2 {
		t.Fatalf("redis overrides not applied: %+v", cfg.RedisConfig)
	}
	if cfg.RabbitmqHost != "rabbitmq" || cfg.RabbitmqPort != 5673 || cfg.RabbitmqUsername != "gopherai" || cfg.RabbitmqPassword != "rabbit-secret" || cfg.RabbitmqVhost != "/gopherai" {
		t.Fatalf("rabbitmq overrides not applied: %+v", cfg.Rabbitmq)
	}
}

func TestInfrastructureEnvironmentRejectsUnsafeValuesWithoutEchoingSecrets(t *testing.T) {
	t.Run("invalid port", func(t *testing.T) {
		t.Setenv("MYSQL_PORT", "not-a-port")
		err := applyInfrastructureEnv(&Config{})
		if err == nil || !strings.Contains(err.Error(), "MYSQL_PORT") || strings.Contains(err.Error(), "not-a-port") {
			t.Fatalf("expected redacted named port error, got %v", err)
		}
	})
	t.Run("empty secret", func(t *testing.T) {
		t.Setenv("REDIS_PASSWORD", "")
		err := applyInfrastructureEnv(&Config{})
		if err == nil || !strings.Contains(err.Error(), "REDIS_PASSWORD") {
			t.Fatalf("expected named empty secret error, got %v", err)
		}
	})
	t.Run("db outside supported range", func(t *testing.T) {
		t.Setenv("REDIS_DB", "16")
		err := applyInfrastructureEnv(&Config{})
		if err == nil || !strings.Contains(err.Error(), "REDIS_DB") {
			t.Fatalf("expected named db error, got %v", err)
		}
	})
}

func TestJWTConfigRequiresEnvironmentGradeSecret(t *testing.T) {
	tests := []struct {
		name string
		edit func(*JwtConfig)
	}{
		{"empty secret", func(c *JwtConfig) { c.Key = "" }},
		{"short secret", func(c *JwtConfig) { c.Key = "too-short" }},
		{"retired secret", func(c *JwtConfig) { c.Key = "GopherAI-v1" }},
		{"year-long token", func(c *JwtConfig) { c.ExpireDuration = 8760 }},
		{"empty issuer", func(c *JwtConfig) { c.Issuer = "" }},
		{"empty subject", func(c *JwtConfig) { c.Subject = "" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := JwtConfig{Key: strings.Repeat("aB3!", 8), ExpireDuration: 24, Issuer: "gopherai", Subject: "user"}
			test.edit(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatal("expected unsafe JWT config to be rejected")
			}
		})
	}
}

func TestJWTEnvironmentOverridesRetiredFileValues(t *testing.T) {
	t.Setenv("JWT_SECRET", strings.Repeat("s3cure!", 6))
	t.Setenv("JWT_EXPIRE_HOURS", "12")
	t.Setenv("JWT_ISSUER", "gopherai-test")
	t.Setenv("JWT_SUBJECT", "learner")
	cfg := JwtConfig{Key: "GopherAI-v1", ExpireDuration: 8760, Issuer: "old", Subject: "old"}
	if err := applyJWTEnv(&cfg); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.ExpireDuration != 12 || cfg.Issuer != "gopherai-test" || cfg.Subject != "learner" || cfg.Key == "GopherAI-v1" {
		t.Fatalf("JWT environment overrides not applied: %+v", cfg)
	}
}

func TestJWTEnvironmentRejectsInvalidDuration(t *testing.T) {
	t.Setenv("JWT_EXPIRE_HOURS", "one-day")
	cfg := JwtConfig{}
	if err := applyJWTEnv(&cfg); err == nil || !strings.Contains(err.Error(), "JWT_EXPIRE_HOURS") {
		t.Fatalf("expected named duration error, got %v", err)
	}
}

func TestRagRerankEnvironmentAndValidation(t *testing.T) {
	cfg := defaultConfig().RagModelConfig
	if cfg.RagRerankEnabled || cfg.RagRerankEndpoint != "http://host.docker.internal:18081" {
		t.Fatalf("unexpected rerank defaults: %+v", cfg)
	}
	t.Setenv("RAG_RERANK_ENABLED", "true")
	t.Setenv("RAG_RERANK_ENDPOINT", "http://host.docker.internal:18082")
	t.Setenv("RAG_RERANK_AUTH_TOKEN", strings.Repeat("x", 32))
	if err := applyRagEnv(&cfg); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if !cfg.RagRerankEnabled || cfg.RagRerankEndpoint != "http://host.docker.internal:18082" {
		t.Fatalf("rerank overrides not applied: %+v", cfg)
	}
}

func TestRagChatAndEmbeddingEnvironmentAreIndependent(t *testing.T) {
	cfg := defaultConfig().RagModelConfig
	t.Setenv("OPENAI_MODEL_NAME", "qwen3.8-max")
	t.Setenv("OPENAI_BASE_URL", "https://chat.example/v1")
	t.Setenv("EMBEDDING_MODEL_NAME", "qwen3.7-text-embedding")
	t.Setenv("EMBEDDING_BASE_URL", "https://embedding.example/v1")
	if err := applyRagEnv(&cfg); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.RagChatModelName != "qwen3.8-max" || cfg.RagBaseUrl != "https://chat.example/v1" {
		t.Fatalf("chat override mismatch: %+v", cfg)
	}
	if cfg.RagEmbeddingModel != "qwen3.7-text-embedding" || cfg.RagEmbeddingBaseURL != "https://embedding.example/v1" || cfg.RagDimension != 1024 {
		t.Fatalf("embedding override mismatch: %+v", cfg)
	}
}

func TestRagProviderEnvironmentRejectsEmptyValue(t *testing.T) {
	t.Setenv("EMBEDDING_BASE_URL", "")
	err := applyRagEnv(&RagModelConfig{})
	if err == nil || !strings.Contains(err.Error(), "EMBEDDING_BASE_URL") {
		t.Fatalf("expected named empty embedding URL error, got %v", err)
	}
}

func TestRagRerankRejectsInvalidEnvironmentAndMissingSecret(t *testing.T) {
	t.Run("invalid boolean", func(t *testing.T) {
		t.Setenv("RAG_RERANK_ENABLED", "sometimes")
		if err := applyRagEnv(&RagModelConfig{}); err == nil || !strings.Contains(err.Error(), "RAG_RERANK_ENABLED") {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("missing token", func(t *testing.T) {
		cfg := defaultConfig().RagModelConfig
		cfg.RagRerankEnabled = true
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "RAG_RERANK_AUTH_TOKEN") {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestDefaultKnowledgeConfig(t *testing.T) {
	got := defaultConfig().KnowledgeConfig
	if got.StorageDir != "data/knowledge" || got.MaxFileSizeBytes != 5*mib ||
		got.MaxDocumentsPerUser != 100 || got.MaxTotalSizeBytes != 50*mib {
		t.Fatalf("unexpected knowledge limits: %+v", got)
	}
	if got.WorkerCount != 2 || got.QueueSize != 100 || got.ScanIntervalSeconds != 5 || got.IndexTimeoutSeconds != 300 {
		t.Fatalf("unexpected worker defaults: %+v", got)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("defaults must be valid: %v", err)
	}
}

func TestKnowledgeConfigRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name string
		edit func(*KnowledgeConfig)
		want string
	}{
		{"empty storage", func(c *KnowledgeConfig) { c.StorageDir = "" }, "storageDir"},
		{"zero file limit", func(c *KnowledgeConfig) { c.MaxFileSizeBytes = 0 }, "maxFileSizeBytes"},
		{"zero document limit", func(c *KnowledgeConfig) { c.MaxDocumentsPerUser = 0 }, "maxDocumentsPerUser"},
		{"total below file", func(c *KnowledgeConfig) { c.MaxTotalSizeBytes = c.MaxFileSizeBytes - 1 }, "maxTotalSizeBytes"},
		{"zero workers", func(c *KnowledgeConfig) { c.WorkerCount = 0 }, "workerCount"},
		{"queue below workers", func(c *KnowledgeConfig) { c.QueueSize = c.WorkerCount - 1 }, "queueSize"},
		{"zero scan interval", func(c *KnowledgeConfig) { c.ScanIntervalSeconds = 0 }, "scanIntervalSeconds"},
		{"zero index timeout", func(c *KnowledgeConfig) { c.IndexTimeoutSeconds = 0 }, "indexTimeoutSeconds"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := defaultConfig().KnowledgeConfig
			tt.edit(&cfg)
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate() error = %v, want field %q", err, tt.want)
			}
		})
	}
}

func TestKnowledgeEnvironmentOverrideRejectsNonInteger(t *testing.T) {
	t.Setenv("KNOWLEDGE_WORKER_COUNT", "many")
	cfg := defaultConfig().KnowledgeConfig
	err := applyKnowledgeEnv(&cfg)
	if err == nil || !strings.Contains(err.Error(), "KNOWLEDGE_WORKER_COUNT") {
		t.Fatalf("expected named environment error, got %v", err)
	}
}

func TestDefaultAgentConfigAndEnvironmentOverrides(t *testing.T) {
	got := defaultConfig().AgentConfig
	if got.MaxToolCalls != 3 || got.MaxModelCalls != 4 || got.MaxGraphSteps != 10 || got.DeadlineSeconds != 45 ||
		got.KnowledgeDefaultTopK != 5 || got.KnowledgeMaxTopK != 8 || got.KnowledgeMaxResultTokens != 700 ||
		got.KnowledgeMaxObservationTokens != 2200 || got.KnowledgeMaxRunTokens != 6500 ||
		got.DocumentReadMaxLines != 240 || got.DocumentReadMaxBytes != 32768 || got.DocumentReadMaxObservationTokens != 3000 ||
		got.ContextModelWindowTokens != 32768 || got.ContextInputTargetTokens != 28000 || got.ContextOutputReserveTokens != 4000 ||
		got.ContextMaxGlobalUserMemoryTokens != 800 || got.ContextMaxTopicUserMemoryTokens != 1200 ||
		got.ContextMaxSummaryTokens != 2000 || got.ContextMaxMasteryTokens != 900 || got.ContextMaxEpisodeTokens != 1000 || got.EpisodeRecallMode != "shadow_only" || got.ContextHistoryFetchLimit != 128 ||
		got.SummaryTriggerMessages != 12 || got.SummaryTriggerTokens != 6000 || got.SummaryRetainMessages != 8 ||
		got.SummaryMaxInputTokens != 8000 || got.SummaryFetchLimit != 512 || got.SummaryTimeoutSeconds != 20 || got.SummaryFailureCooldownSeconds != 30 ||
		got.EpisodeWorkerCount != 1 || got.EpisodeQueueSize != 32 || got.EpisodeQuietWindowSeconds != 600 ||
		got.EpisodeMaxInputTokens != 12000 || got.EpisodeMaxMessages != 4096 {
		t.Fatalf("unexpected agent defaults: %+v", got)
	}
	t.Setenv("AGENT_MAX_TOOL_CALLS", "2")
	t.Setenv("AGENT_DEADLINE_SECONDS", "30")
	t.Setenv("AGENT_KNOWLEDGE_DEFAULT_TOP_K", "4")
	t.Setenv("AGENT_KNOWLEDGE_MAX_RUN_TOKENS", "6000")
	t.Setenv("AGENT_DOCUMENT_READ_MAX_LINES", "120")
	t.Setenv("AGENT_DOCUMENT_READ_MAX_BYTES", "16384")
	t.Setenv("AGENT_DOCUMENT_READ_MAX_OBSERVATION_TOKENS", "2000")
	t.Setenv("AGENT_CONTEXT_INPUT_TARGET_TOKENS", "18000")
	t.Setenv("AGENT_CONTEXT_HISTORY_FETCH_LIMIT", "96")
	t.Setenv("AGENT_CONTEXT_MAX_EPISODE_TOKENS", "900")
	t.Setenv("AGENT_EPISODE_RECALL_MODE", "inject_enabled")
	t.Setenv("AGENT_EPISODE_QUIET_WINDOW_SECONDS", "120")
	t.Setenv("AGENT_EPISODE_MAX_MESSAGES", "2048")
	if err := applyAgentEnv(&got); err != nil {
		t.Fatal(err)
	}
	if got.MaxToolCalls != 2 || got.DeadlineSeconds != 30 || got.KnowledgeDefaultTopK != 4 || got.KnowledgeMaxRunTokens != 6000 ||
		got.DocumentReadMaxLines != 120 || got.DocumentReadMaxBytes != 16384 || got.DocumentReadMaxObservationTokens != 2000 ||
		got.ContextInputTargetTokens != 18000 || got.ContextHistoryFetchLimit != 96 || got.ContextMaxEpisodeTokens != 900 || got.EpisodeRecallMode != "inject_enabled" || got.EpisodeQuietWindowSeconds != 120 || got.EpisodeMaxMessages != 2048 {
		t.Fatalf("agent overrides not applied: %+v", got)
	}
	if err := got.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestAgentConfigRejectsUnsafeLimits(t *testing.T) {
	tests := []struct {
		name string
		edit func(*AgentConfig)
	}{
		{"tools", func(c *AgentConfig) { c.MaxToolCalls = 9 }},
		{"models", func(c *AgentConfig) { c.MaxModelCalls = 0 }},
		{"graph", func(c *AgentConfig) { c.MaxGraphSteps = 33 }},
		{"deadline", func(c *AgentConfig) { c.DeadlineSeconds = 121 }},
		{"knowledge top k", func(c *AgentConfig) { c.KnowledgeDefaultTopK = c.KnowledgeMaxTopK + 1 }},
		{"knowledge result", func(c *AgentConfig) { c.KnowledgeMaxResultTokens = 0 }},
		{"knowledge observation", func(c *AgentConfig) { c.KnowledgeMaxObservationTokens = 9000 }},
		{"knowledge run", func(c *AgentConfig) { c.KnowledgeMaxRunTokens = c.KnowledgeMaxObservationTokens - 1 }},
		{"document lines", func(c *AgentConfig) { c.DocumentReadMaxLines = 0 }},
		{"document bytes", func(c *AgentConfig) { c.DocumentReadMaxBytes = 0 }},
		{"document observation", func(c *AgentConfig) { c.DocumentReadMaxObservationTokens = c.KnowledgeMaxRunTokens + 1 }},
		{"context model window", func(c *AgentConfig) { c.ContextModelWindowTokens = 1000 }},
		{"context input", func(c *AgentConfig) { c.ContextInputTargetTokens = c.ContextModelWindowTokens }},
		{"context output", func(c *AgentConfig) { c.ContextOutputReserveTokens = c.ContextModelWindowTokens }},
		{"context history", func(c *AgentConfig) { c.ContextHistoryFetchLimit = 0 }},
		{"episode recall mode", func(c *AgentConfig) { c.EpisodeRecallMode = "always" }},
		{"episode context budget", func(c *AgentConfig) { c.ContextMaxEpisodeTokens = 799 }},
		{"summary messages", func(c *AgentConfig) { c.SummaryTriggerMessages = 1 }},
		{"summary tokens", func(c *AgentConfig) { c.SummaryTriggerTokens = 0 }},
		{"summary retained", func(c *AgentConfig) { c.SummaryRetainMessages = c.SummaryTriggerMessages }},
		{"summary input", func(c *AgentConfig) { c.SummaryMaxInputTokens = c.ContextInputTargetTokens + 1 }},
		{"summary fetch", func(c *AgentConfig) { c.SummaryFetchLimit = 1 }},
		{"summary timeout", func(c *AgentConfig) { c.SummaryTimeoutSeconds = 0 }},
		{"summary cooldown", func(c *AgentConfig) { c.SummaryFailureCooldownSeconds = 0 }},
		{"episode workers", func(c *AgentConfig) { c.EpisodeWorkerCount = 0 }},
		{"episode queue", func(c *AgentConfig) { c.EpisodeQueueSize = 0 }},
		{"episode scan", func(c *AgentConfig) { c.EpisodeScanIntervalSeconds = 0 }},
		{"episode lease", func(c *AgentConfig) { c.EpisodeLeaseSeconds = c.EpisodeTaskTimeoutSeconds }},
		{"episode attempts", func(c *AgentConfig) { c.EpisodeMaxAttempts = 0 }},
		{"episode input", func(c *AgentConfig) { c.EpisodeMaxInputTokens = 0 }},
		{"episode messages", func(c *AgentConfig) { c.EpisodeMaxMessages = 0 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := defaultConfig().AgentConfig
			test.edit(&got)
			if err := got.Validate(); err == nil {
				t.Fatal("expected unsafe agent config to be rejected")
			}
		})
	}
}

// The run manifest reads configuration through the same two narrow accessors the
// product's own seams use. If either one ever disagrees with a full Load, the
// manifest would be recording a model or a budget the server never sends.
// The run manifest reads its configuration through the same narrow accessors the
// product's own seams use, so a manifest can never record a model or a budget the
// server would not actually send. These are the values that claim rests on.
func TestReadOnlyAccessorsReportWhatTheRunWillUse(t *testing.T) {
	// The accessors read a file. This repository ships none, so the test
	// writes the configuration it reads. The assertions are about the values
	// the accessors report, not about a file that happens to exist.
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[ragModelConfig]\n"), 0o600); err != nil {
		t.Fatalf("write configuration: %v", err)
	}
	rag, err := LoadRagEmbedding(path)
	if err != nil {
		t.Fatalf("rag load: %v", err)
	}
	if rag.RagEmbeddingModel == "" || rag.RagEmbeddingBaseURL == "" || rag.RagDimension <= 0 ||
		rag.RagChatModelName == "" || rag.RagBaseUrl == "" {
		t.Fatalf("the RAG accessor left a field unset: %+v", rag)
	}
	budget, err := LoadEpisodeTokenBudget(path)
	if err != nil {
		t.Fatalf("episode budget load: %v", err)
	}
	if budget != 1000 {
		t.Fatalf("episodic context budget %d, want the product default 1000", budget)
	}
	t.Setenv("AGENT_CONTEXT_MAX_EPISODE_TOKENS", "1100")
	overridden, err := LoadEpisodeTokenBudget(path)
	if err != nil {
		t.Fatalf("episode budget with env: %v", err)
	}
	if overridden != 1100 {
		t.Fatalf("env override produced %d, want 1100", overridden)
	}
	t.Setenv("AGENT_CONTEXT_MAX_EPISODE_TOKENS", "50")
	if _, err := LoadEpisodeTokenBudget(path); err == nil {
		t.Fatal("an out-of-range budget must be refused, not carried into a run")
	}
	if _, err := LoadEpisodeTokenBudget("does-not-exist.toml"); err == nil {
		t.Fatal("a missing config file must not yield a budget")
	}
}
