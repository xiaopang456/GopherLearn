package config

import (
	"strings"
	"testing"

	"GopherAI/common/memoryrecall"
)

const validConsentSecret = "stabilize050-config-test-secret-0123456789"

func confirmedAgentConfig() AgentConfig {
	config := defaultConfig().AgentConfig
	config.MemoryReadMode = string(memoryrecall.ModeAgentMemoryUserConfirmedV1)
	config.TopicConsentSecret = validConsentSecret
	// The mode registers memory_search for episodic history, so it inherits the
	// same tool budget every agent memory-tool mode is validated against.
	config.MemoryToolMaxCalls = 2
	config.MemoryToolTopK = 4
	config.MemoryToolCandidateDepth = 40
	config.MemoryToolMinConfidence = 0.5
	config.MemoryToolTimeoutMS = 2000
	config.MaxToolCalls = 6
	config.MaxModelCalls = 7
	config.MaxGraphSteps = 16
	config.DeadlineSeconds = 45
	return config
}

// The mode signs tickets, so it cannot start without a key. A missing one would
// mean either an unsigned ticket or a silently disabled confirmation.
func TestConfirmedModeRequiresASigningSecret(t *testing.T) {
	config := confirmedAgentConfig()
	config.TopicConsentSecret = ""
	if err := config.Validate(); err == nil {
		t.Fatal("the confirmed mode must not start without a signing secret")
	}
	config.TopicConsentSecret = strings.Repeat("a", memoryrecall.TopicConsentMinSecretLength-1)
	if err := config.Validate(); err == nil {
		t.Fatal("a short signing secret must be refused")
	}
}

// A key configured under a mode that never verifies one is a secret sitting in
// production for no reason, waiting for someone to flip the mode.
func TestOtherModesMustNotCarryAConsentSecret(t *testing.T) {
	config := defaultConfig().AgentConfig
	if config.MemoryReadMode != string(memoryrecall.ModeLegacyV2) {
		t.Fatalf("default mode = %s, want legacy_v2", config.MemoryReadMode)
	}
	config.TopicConsentSecret = validConsentSecret
	if err := config.Validate(); err == nil {
		t.Fatal("a consent secret under legacy_v2 must be refused")
	}
	config.TopicConsentSecret = ""
	config.TopicConsentTTLMinutes = 5
	if err := config.Validate(); err == nil {
		t.Fatal("a consent TTL under legacy_v2 must be refused")
	}
}

func TestConfirmedModeAcceptsAValidConfiguration(t *testing.T) {
	config := confirmedAgentConfig()
	if err := config.Validate(); err != nil {
		t.Fatalf("a valid confirmed configuration was refused: %v", err)
	}
	config.TopicConsentTTLMinutes = 10
	config.TopicConsentMaxCandidates = memoryrecall.TopicConsentMaxCandidates
	if err := config.Validate(); err != nil {
		t.Fatalf("valid bounds were refused: %v", err)
	}
}

func TestConfirmedModeRefusesOutOfRangeBounds(t *testing.T) {
	config := confirmedAgentConfig()
	config.TopicConsentTTLMinutes = 61
	if err := config.Validate(); err == nil {
		t.Fatal("an hour-plus ticket lifetime must be refused")
	}
	config = confirmedAgentConfig()
	config.TopicConsentMaxCandidates = memoryrecall.TopicConsentMaxCandidates + 1
	if err := config.Validate(); err == nil {
		t.Fatal("more candidates than the ceiling must be refused")
	}
}

// The shipped default must not move, and the mode must be parseable by name.
func TestDefaultStaysLegacyAndTheModeParses(t *testing.T) {
	if memoryrecall.DefaultReadMode() != memoryrecall.ModeLegacyV2 {
		t.Fatalf("default read mode = %s", memoryrecall.DefaultReadMode())
	}
	mode, err := memoryrecall.ParseReadMode("agent_memory_user_confirmed_v1")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if mode != memoryrecall.ModeAgentMemoryUserConfirmedV1 {
		t.Fatalf("mode = %s", mode)
	}
	if _, err := memoryrecall.ParseReadMode("agent_memory_user_confirmed_v2"); err == nil {
		t.Fatal("an unknown mode must fail closed")
	}
}
