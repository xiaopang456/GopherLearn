package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/BurntSushi/toml"
)

// Thinking is off until a deployment turns it on: a provider that reads the
// switch may charge for it, so the default is the one nobody has to pay for.
// The configuration file is the only place that can change it.
func TestAgentThinkingDefaultsToOffAndFollowsTheFile(t *testing.T) {
	if defaultConfig().AgentConfig.AgentThinkingEnabled {
		t.Fatal("agent thinking must default to off")
	}

	dir := t.TempDir()
	on := filepath.Join(dir, "on.toml")
	if err := os.WriteFile(on, []byte("[agentConfig]\nagentThinkingEnabled = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded := defaultConfig()
	if _, err := toml.DecodeFile(on, loaded); err != nil {
		t.Fatal(err)
	}
	if !loaded.AgentConfig.AgentThinkingEnabled {
		t.Fatal("agentThinkingEnabled = true did not load")
	}

	off := filepath.Join(dir, "off.toml")
	if err := os.WriteFile(off, []byte("[agentConfig]\nmaxToolCalls = 3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	unset := defaultConfig()
	if _, err := toml.DecodeFile(off, unset); err != nil {
		t.Fatal(err)
	}
	if unset.AgentConfig.AgentThinkingEnabled {
		t.Fatal("a configuration that says nothing about thinking turned it on")
	}
}
