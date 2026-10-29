package config

import (
	"os"
	"strings"
	"testing"

	"GopherAI/common/memoryrecall"
)

// The seventh mode does not become anybody's default by existing either, and no
// deployment sample may name it.
func TestAgentMemoryToolV2IsNotADefaultAnywhere(t *testing.T) {
	if memoryrecall.DefaultReadMode() != memoryrecall.ModeLegacyV2 {
		t.Fatalf("default read mode = %s", memoryrecall.DefaultReadMode())
	}
	if defaultConfig().AgentConfig.MemoryReadMode != string(memoryrecall.ModeLegacyV2) {
		t.Fatalf("default config mode = %q", defaultConfig().AgentConfig.MemoryReadMode)
	}
	for _, path := range []string{"config.docker.toml", "config.toml"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if strings.Contains(string(raw), string(memoryrecall.ModeAgentMemoryToolV2)) {
			t.Fatalf("%s names a mode it does not run", path)
		}
	}
}

// v2 registers the same tool at the same budget, so it is validated by the same
// rules: incomplete must not start, and a complete configuration under a mode
// that cannot use it must not start either.
func TestAgentMemoryToolV2ObeysTheSameConfigurationRulesAsV1(t *testing.T) {
	complete := memoryToolConfig(t)
	complete.MemoryReadMode = string(memoryrecall.ModeAgentMemoryToolV2)
	if err := complete.Validate(); err != nil {
		t.Fatalf("a complete v2 configuration was rejected: %v", err)
	}

	incomplete := defaultConfig().AgentConfig
	incomplete.MemoryReadMode = string(memoryrecall.ModeAgentMemoryToolV2)
	if err := incomplete.Validate(); err == nil {
		t.Fatal("agent_memory_tool_v2 started with no memoryTool settings")
	}

	// The wider run budget belongs to the tool modes and to nothing else.
	narrow := complete
	narrow.MaxToolCalls, narrow.MaxModelCalls, narrow.MaxGraphSteps = 3, 4, 10
	if err := narrow.Validate(); err == nil {
		t.Fatal("agent_memory_tool_v2 accepted the pre-STABILIZE-033 budget")
	}
	wide := complete
	wide.MaxToolCalls, wide.MaxModelCalls, wide.MaxGraphSteps = 8, 9, 20
	if err := wide.Validate(); err == nil {
		t.Fatal("agent_memory_tool_v2 accepted a budget wider than 6/7/16")
	}
}

// An unknown mode still stops startup rather than falling back to something.
//
// The example used to be "agent_memory_tool_v3", which STABILIZE-040 made real.
// A test whose unknown value can become known is a test that stops testing
// without failing, so the value here is one no mode will ever take.
func TestUnknownModeStillFailsClosedAfterV2(t *testing.T) {
	const neverAMode = "agent_memory_tool_v99"
	conf := defaultConfig().AgentConfig
	conf.MemoryReadMode = neverAMode
	if err := conf.Validate(); err == nil {
		t.Fatal("an unknown read mode started")
	}
	if _, err := memoryrecall.ParseReadMode(neverAMode); err == nil {
		t.Fatal("an unknown read mode parsed")
	}
	for _, mode := range memoryrecall.ModeNames() {
		if !mode.Valid() {
			t.Fatalf("mode %s is listed and invalid", mode)
		}
	}
	// Exactly one mode bootstraps topic memory, and every other predicate stays
	// where the modes that were measured left it.
	bootstrapping := 0
	for _, mode := range memoryrecall.ModeNames() {
		if mode.UsesAuthoritativeTopicBootstrap() {
			bootstrapping++
			if mode != memoryrecall.ModeAgentMemoryToolV2 {
				t.Fatalf("mode %s bootstraps topic memory", mode)
			}
			if mode.UsesReadPlan() || mode.UsesTopicResolver() || mode.UsesSemanticTopicLayer() ||
				mode.UsesModelPlanner() || mode.UsesRecallCycle() {
				t.Fatalf("mode %s builds an older read chain", mode)
			}
			if !mode.UsesMemorySearchTool() {
				t.Fatalf("mode %s does not register memory_search", mode)
			}
		}
	}
	if bootstrapping != 1 {
		t.Fatalf("%d modes bootstrap topic memory", bootstrapping)
	}
}
