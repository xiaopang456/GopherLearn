package config

import (
	"os"
	"strings"
	"testing"

	"GopherAI/common/memoryrecall"
)

// The default is the whole point of STABILIZE-031. If this ever flips, a read
// path whose development gate did not pass is live for every user.
func TestShippedDefaultReadModeIsLegacyV2(t *testing.T) {
	if got := defaultConfig().AgentConfig.MemoryReadMode; got != string(memoryrecall.ModeLegacyV2) {
		t.Fatalf("default memory read mode is %q, want legacy_v2", got)
	}
	if err := defaultConfig().AgentConfig.Validate(); err != nil {
		t.Fatalf("the shipped default must validate: %v", err)
	}
}

// The deployed compose configuration is a deployment sample, and the design
// requires every one of them to sit on legacy_v2.
func TestDeployedConfigPinsLegacyV2(t *testing.T) {
	raw, err := os.ReadFile("config.docker.toml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `memoryReadMode = "legacy_v2"`) {
		t.Fatal("config.docker.toml must pin memoryReadMode to legacy_v2")
	}
}

func TestUnknownReadModeRefusesToStart(t *testing.T) {
	conf := defaultConfig().AgentConfig
	conf.MemoryReadMode = "episode_v4"
	if err := conf.Validate(); err == nil {
		t.Fatal("an unknown mode must stop startup rather than fall back")
	}
}

// The semantic gate has no production default, so enabling the mode without
// both numbers must fail. A number that stays in a Default*Config() is a
// production default whatever the comment above it says.
func TestSemanticModeDemandsBothGateNumbers(t *testing.T) {
	base := defaultConfig().AgentConfig
	base.MemoryReadMode = string(memoryrecall.ModeEpisodeV3Semantic)

	if err := base.Validate(); err == nil {
		t.Fatal("episode_v3_semantic without a gate must not start")
	}

	onlyStandout := base
	onlyStandout.MemoryTopicStandout = 1.3
	if err := onlyStandout.Validate(); err == nil {
		t.Fatal("a standout without a margin must not start")
	}

	onlyMargin := base
	onlyMargin.MemoryTopicMargin = 0.06
	if err := onlyMargin.Validate(); err == nil {
		t.Fatal("a margin without a standout must not start")
	}

	complete := base
	complete.MemoryTopicStandout, complete.MemoryTopicMargin = 1.3, 0.06
	if err := complete.Validate(); err != nil {
		t.Fatalf("an explicitly configured gate must start: %v", err)
	}
}

// Gate numbers carried by a mode that cannot use them are how an unreviewed
// value goes live the day somebody flips the mode.
func TestLowerModesRefuseStrayGateNumbers(t *testing.T) {
	for _, mode := range []memoryrecall.ReadMode{
		memoryrecall.ModeLegacyV2, memoryrecall.ModeEpisodeV3, memoryrecall.ModeEpisodeV3Lexical,
	} {
		t.Run(string(mode), func(t *testing.T) {
			conf := defaultConfig().AgentConfig
			conf.MemoryReadMode = string(mode)
			conf.MemoryTopicStandout = 1.3
			if err := conf.Validate(); err == nil {
				t.Fatalf("%s must refuse a stray standout", mode)
			}
		})
	}
}

func TestReadModeEnvOverrideIsValidated(t *testing.T) {
	t.Setenv("AGENT_MEMORY_READ_MODE", string(memoryrecall.ModeEpisodeV3))
	conf := defaultConfig()
	if err := applyAgentEnv(&conf.AgentConfig); err != nil {
		t.Fatal(err)
	}
	if conf.AgentConfig.MemoryReadMode != string(memoryrecall.ModeEpisodeV3) {
		t.Fatalf("env override did not apply: %q", conf.AgentConfig.MemoryReadMode)
	}

	t.Setenv("AGENT_MEMORY_READ_MODE", "nonsense")
	broken := defaultConfig()
	if err := applyAgentEnv(&broken.AgentConfig); err != nil {
		t.Fatal(err)
	}
	if err := broken.AgentConfig.Validate(); err == nil {
		t.Fatal("an unknown mode from the environment must still be refused")
	}
}
