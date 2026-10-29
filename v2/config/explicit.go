package config

import (
	"fmt"
	"github.com/BurntSushi/toml"
	"strings"
)

// LoadExplicit retains source defaults and validation but never consults process
// environment or config/config.toml. The isolated entry always supplies a path.
func LoadExplicit(path string) (*Config, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("isolated V2 requires an explicit configuration path")
	}
	loaded := defaultConfig()
	if _, err := toml.DecodeFile(path, loaded); err != nil {
		return nil, fmt.Errorf("load explicit V2 configuration: %w", err)
	}
	if err := loaded.RagModelConfig.Validate(); err != nil {
		return nil, err
	}
	if err := loaded.JwtConfig.Validate(); err != nil {
		return nil, err
	}
	if err := loaded.KnowledgeConfig.Validate(); err != nil {
		return nil, err
	}
	if err := loaded.AgentConfig.Validate(); err != nil {
		return nil, err
	}
	return loaded, nil
}
