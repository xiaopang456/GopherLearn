package config

import (
	"testing"

	"github.com/BurntSushi/toml"
)

func TestMemoryFinalizeIsOnByDefaultAndCanBeDisabled(t *testing.T) {
	loaded := defaultConfig()
	if !loaded.AgentConfig.MemoryFinalizeEnabled {
		t.Fatal("automatic memory proposals should be enabled by default")
	}
	if _, err := toml.Decode("[agentConfig]\nmemoryFinalizeEnabled = false\n", loaded); err != nil {
		t.Fatal(err)
	}
	if loaded.AgentConfig.MemoryFinalizeEnabled {
		t.Fatal("explicitly disabling automatic memory proposals should override the default")
	}
}
