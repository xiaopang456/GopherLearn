package config

import (
	"fmt"
	"github.com/BurntSushi/toml"
	"strings"
)

// LoadExplicit retains source defaults and validation but never consults process
// environment or config/config.toml. The isolated entry always supplies a path.
//
// One exception exists, and it is deliberate: the rerank bearer token is a
// secret, so it is never written into a configuration file. When the reranker
// is enabled this loader reads exactly one environment variable,
// RAG_RERANK_AUTH_TOKEN, and nothing else. Whether the reranker runs at all,
// which provider serves it, where it listens, which model it serves and how
// long a call may take all stay in the file.
func LoadExplicit(path string) (*Config, error) {
	loaded, err := loadExplicitFile(path)
	if err != nil {
		return nil, err
	}
	if loaded.RagModelConfig.RagRerankEnabled {
		if err := applyRerankTokenEnv(&loaded.RagModelConfig); err != nil {
			return nil, err
		}
	}
	if err := loaded.RagModelConfig.Validate(); err != nil {
		return nil, err
	}
	if err := validateRemainder(loaded); err != nil {
		return nil, err
	}
	return loaded, nil
}

// LoadExplicitForSchema loads the same explicit configuration for the schema
// tool, and ignores the reranker entirely.
//
// Creating tables needs no rerank credential, no rerank endpoint and no rerank
// model, so a deployment that enables the reranker must not be blocked from
// creating its schema by a missing token. The rerank section of the file is
// neither read nor validated here; the running entry is what checks it.
func LoadExplicitForSchema(path string) (*Config, error) {
	loaded, err := loadExplicitFile(path)
	if err != nil {
		return nil, err
	}
	loaded.RagModelConfig.RagRerankEnabled = false
	if err := loaded.RagModelConfig.Validate(); err != nil {
		return nil, err
	}
	if err := validateRemainder(loaded); err != nil {
		return nil, err
	}
	return loaded, nil
}

// loadExplicitFile decodes the one explicit configuration file this entry is
// given. It reads nothing else: no default file, no environment.
func loadExplicitFile(path string) (*Config, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("isolated V2 requires an explicit configuration path")
	}
	loaded := defaultConfig()
	if _, err := toml.DecodeFile(path, loaded); err != nil {
		return nil, fmt.Errorf("load explicit V2 configuration: %w", err)
	}
	return loaded, nil
}

// validateRemainder validates the sections that do not depend on the reranker.
func validateRemainder(loaded *Config) error {
	if err := loaded.JwtConfig.Validate(); err != nil {
		return err
	}
	if err := loaded.KnowledgeConfig.Validate(); err != nil {
		return err
	}
	if err := loaded.AgentConfig.Validate(); err != nil {
		return err
	}
	return nil
}
