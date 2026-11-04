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
//
// The thinking switch is not checked here, because it is not checked anywhere:
// every dialect works with it. What this loader skips is the reranker, in the
// same way - this loader creates tables for a configuration the running entry
// would refuse, and preparing a database is not the same as agreeing to run
// it.
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
	metadata, err := toml.DecodeFile(path, loaded)
	if err != nil {
		return nil, fmt.Errorf("load explicit V2 configuration: %w", err)
	}
	if err := refuseRetiredSettings(metadata); err != nil {
		return nil, err
	}
	return loaded, nil
}

// retiredSettings names the file keys this build stopped reading, and says what
// replaces each one. They are refused by name instead of being ignored.
//
// The reason is that the TOML decoder is tolerant: a key nothing decodes into
// starts the process anyway, and the operator is left reading a file that says one
// thing while the program does another. A base URL nobody builds a client from, or
// an embedding key sitting in a section the embedder no longer reads, is exactly
// the failure that would surface as a bill or a silent fallback rather than an
// error. So a stale line stops startup, and the message names the line to change.
var retiredSettings = map[string]string{
	"ragModelConfig.baseUrl": "ragModelConfig.baseUrl was removed: no client is built from it. " +
		"The learning Agent's chat address comes from isolatedEntry.chatBaseURL. Delete this line.",
	"isolatedEntry.embeddingKey": "isolatedEntry.embeddingKey moved to ragModelConfig.embeddingApiKey, " +
		"beside the embedding model name and address it belongs to. Move the key there and delete this line.",
}

// refuseRetiredSettings checks the keys the document actually contains, not the
// ones the decoder failed to place: a retired key can sit in a section this struct
// never reads at all, and an "undecoded" report would hide it inside the section
// name.
func refuseRetiredSettings(metadata toml.MetaData) error {
	for _, key := range metadata.Keys() {
		if refusal, retired := retiredSettings[key.String()]; retired {
			return fmt.Errorf("retired configuration setting: %s", refusal)
		}
	}
	return nil
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
