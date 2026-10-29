package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// rerankConfiguration writes an explicit configuration file that enables the
// reranker and returns its path.
func rerankConfiguration(t *testing.T, extra string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	content := `[jwtConfig]
key = "a-jwt-secret-of-at-least-thirty-two-bytes"

[ragModelConfig]
embeddingModel = "qwen3.7-text-embedding"
embeddingBaseUrl = "https://embedding.example.invalid/v1"
chatModelName = "qwen3.8-max"
baseUrl = "https://chat.example.invalid/v1"
dimension = 1024
rerankEnabled = true
rerankEndpoint = "https://rerank.example.invalid/v1/rerank"
rerankProvider = "dashscope"
rerankModel = "qwen3-rerank"
` + extra
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestLoadExplicitReadsOnlyTheRerankTokenFromTheEnvironment proves the formal
// entry reads the rerank credential and nothing else: the token arrives from
// the environment, and every other rerank setting stays in the file.
func TestLoadExplicitReadsOnlyTheRerankTokenFromTheEnvironment(t *testing.T) {
	path := rerankConfiguration(t, "")
	t.Setenv("RAG_RERANK_AUTH_TOKEN", strings.Repeat("k", 32))
	// The other rerank variables must not reach the explicit entry: the
	// endpoint, the provider and the model are deployment settings and belong
	// in the file.
	t.Setenv("RAG_RERANK_ENDPOINT", "https://elsewhere.example.invalid/v1/rerank")
	t.Setenv("RAG_RERANK_ENABLED", "false")
	t.Setenv("RAG_RERANK_PROVIDER", "local")
	loaded, err := LoadExplicit(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.RagModelConfig.RagRerankToken != strings.Repeat("k", 32) {
		t.Fatalf("token = %q", loaded.RagModelConfig.RagRerankToken)
	}
	if loaded.RagModelConfig.RagRerankEndpoint != "https://rerank.example.invalid/v1/rerank" {
		t.Fatalf("endpoint = %q", loaded.RagModelConfig.RagRerankEndpoint)
	}
	if loaded.RagModelConfig.RagRerankProvider != RerankProviderDashScope {
		t.Fatalf("provider = %q", loaded.RagModelConfig.RagRerankProvider)
	}
	if !loaded.RagModelConfig.RagRerankEnabled {
		t.Fatal("rerank must stay enabled when the file says so")
	}
}

// TestLoadExplicitWithoutATokenExplainsWhatToDo proves the missing credential
// is reported by name, with the reason it is not in the file.
func TestLoadExplicitWithoutATokenExplainsWhatToDo(t *testing.T) {
	path := rerankConfiguration(t, "")
	t.Run("the variable is absent", func(t *testing.T) {
		t.Setenv("RAG_RERANK_AUTH_TOKEN", "placeholder")
		if err := os.Unsetenv("RAG_RERANK_AUTH_TOKEN"); err != nil {
			t.Fatal(err)
		}
		_, err := LoadExplicit(path)
		if err == nil {
			t.Fatal("expected a missing token error")
		}
		if !strings.Contains(err.Error(), "RAG_RERANK_AUTH_TOKEN") {
			t.Fatalf("error %q does not name the variable", err)
		}
		if !strings.Contains(err.Error(), "environment") {
			t.Fatalf("error %q does not say where the token comes from", err)
		}
	})
	t.Run("the variable is empty", func(t *testing.T) {
		t.Setenv("RAG_RERANK_AUTH_TOKEN", "")
		_, err := LoadExplicit(path)
		if err == nil {
			t.Fatal("expected a missing token error")
		}
		if !strings.Contains(err.Error(), "RAG_RERANK_AUTH_TOKEN") {
			t.Fatalf("error %q does not name the variable", err)
		}
		if !strings.Contains(err.Error(), "environment") {
			t.Fatalf("error %q does not say where the token comes from", err)
		}
	})
}

// TestLoadExplicitIgnoresTheTokenWhenTheRerankerIsOff proves a deployment
// that does not use the reranker needs no credential at all.
func TestLoadExplicitIgnoresTheTokenWhenTheRerankerIsOff(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	content := `[jwtConfig]
key = "a-jwt-secret-of-at-least-thirty-two-bytes"

[ragModelConfig]
embeddingModel = "qwen3.7-text-embedding"
embeddingBaseUrl = "https://embedding.example.invalid/v1"
chatModelName = "qwen3.8-max"
baseUrl = "https://chat.example.invalid/v1"
dimension = 1024
rerankEnabled = false
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RAG_RERANK_AUTH_TOKEN", "")
	loaded, err := LoadExplicit(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.RagModelConfig.RagRerankEnabled {
		t.Fatal("rerank must stay off when the file says so")
	}
}

// TestLoadExplicitForSchemaIgnoresTheReranker proves the schema tool is not
// gated by the reranker: creating tables needs no rerank credential and no
// rerank setting, so a deployment that enables the reranker must still be
// able to create its schema.
func TestLoadExplicitForSchemaIgnoresTheReranker(t *testing.T) {
	path := rerankConfiguration(t, "")
	t.Setenv("RAG_RERANK_AUTH_TOKEN", "")
	loaded, err := LoadExplicitForSchema(path)
	if err != nil {
		t.Fatalf("schema load refused a rerank configuration: %v", err)
	}
	if loaded.RagModelConfig.RagRerankEnabled {
		t.Fatal("the schema loader must not run with the reranker enabled")
	}
	// The running entry is the one that checks the reranker.
	if _, err := LoadExplicit(path); err == nil {
		t.Fatal("expected the running entry to refuse a missing rerank token")
	}
}

// TestLoadExplicitForSchemaIgnoresAnUnknownRerankProvider proves the schema
// tool validates no rerank setting at all, not even the provider name.
func TestLoadExplicitForSchemaIgnoresAnUnknownRerankProvider(t *testing.T) {
	path := rerankConfiguration(t, "")
	t.Setenv("RAG_RERANK_AUTH_TOKEN", strings.Repeat("k", 32))
	if _, err := LoadExplicitForSchema(path); err != nil {
		t.Fatalf("schema load refused an unknown rerank provider: %v", err)
	}
}

func TestRerankProviderValidation(t *testing.T) {
	base := defaultConfig().RagModelConfig
	base.RagRerankEnabled = true
	base.RagRerankToken = strings.Repeat("k", 32)

	t.Run("a hosted provider needs a model", func(t *testing.T) {
		cfg := base
		cfg.RagRerankProvider = RerankProviderDashScope
		cfg.RagRerankModel = ""
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "rerankModel") {
			t.Fatalf("error = %v, want a named model error", err)
		}
	})
	t.Run("an unknown provider is refused", func(t *testing.T) {
		cfg := base
		cfg.RagRerankProvider = "elsewhere"
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "rerankProvider") {
			t.Fatalf("error = %v, want a named provider error", err)
		}
	})
	t.Run("the local provider needs no model", func(t *testing.T) {
		cfg := base
		cfg.RagRerankProvider = RerankProviderLocal
		cfg.RagRerankModel = ""
		if err := cfg.Validate(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("a negative timeout is refused", func(t *testing.T) {
		cfg := base
		cfg.RagRerankTimeoutMs = -1
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "rerankTimeoutMs") {
			t.Fatalf("error = %v, want a named timeout error", err)
		}
	})
}

func TestRerankTimeoutResolution(t *testing.T) {
	cfg := defaultConfig().RagModelConfig
	if got := cfg.RagRerankTimeout(); got != defaultLocalRerankTimeout {
		t.Fatalf("local default timeout = %s, want %s", got, defaultLocalRerankTimeout)
	}
	cfg.RagRerankProvider = RerankProviderDashScope
	if got := cfg.RagRerankTimeout(); got != defaultHostedRerankTimeout {
		t.Fatalf("hosted default timeout = %s, want %s", got, defaultHostedRerankTimeout)
	}
	cfg.RagRerankTimeoutMs = 45000
	if got := cfg.RagRerankTimeout(); got != 45*time.Second {
		t.Fatalf("configured timeout = %s, want 45s", got)
	}
}
