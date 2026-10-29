package config

import (
	"os"
	"strings"
	"testing"

	"GopherAI/common/memoryrecall"
)

// The eighth mode does not become anybody's default by existing, and no
// deployment sample may name it.
//
// This is the same test the seventh mode got, and it is repeated rather than
// generalised on purpose: "no new mode ships by accident" is a claim about each
// mode, and a loop over ModeNames would pass on the day someone forgot to add
// one to the list.
func TestAgentMemoryToolV3IsNotADefaultAnywhere(t *testing.T) {
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
		if strings.Contains(string(raw), string(memoryrecall.ModeAgentMemoryToolV3)) {
			t.Fatalf("%s names a mode it does not run", path)
		}
	}
}

// v3 registers memory_search at the same budget, so it is validated by the same
// rules. The second tool it also registers is not a configuration setting: it
// has no query, no top-k and no candidate depth to configure, and a deployment
// that could tune it could tune away the one thing that makes it narrow.
func TestAgentMemoryToolV3ObeysTheSameConfigurationRulesAsV1(t *testing.T) {
	complete := memoryToolConfig(t)
	complete.MemoryReadMode = string(memoryrecall.ModeAgentMemoryToolV3)
	if err := complete.Validate(); err != nil {
		t.Fatalf("a complete v3 configuration was rejected: %v", err)
	}

	incomplete := defaultConfig().AgentConfig
	incomplete.MemoryReadMode = string(memoryrecall.ModeAgentMemoryToolV3)
	if err := incomplete.Validate(); err == nil {
		t.Fatal("agent_memory_tool_v3 started with no memoryTool settings")
	}

	narrow := complete
	narrow.MaxToolCalls, narrow.MaxModelCalls, narrow.MaxGraphSteps = 3, 4, 10
	if err := narrow.Validate(); err == nil {
		t.Fatal("agent_memory_tool_v3 accepted the pre-STABILIZE-033 budget")
	}
	wide := complete
	wide.MaxToolCalls, wide.MaxModelCalls, wide.MaxGraphSteps = 8, 9, 20
	if err := wide.Validate(); err == nil {
		t.Fatal("agent_memory_tool_v3 accepted a budget wider than 6/7/16")
	}
	longer := complete
	longer.DeadlineSeconds = 60
	if err := longer.Validate(); err == nil {
		t.Fatal("agent_memory_tool_v3 accepted a longer deadline than the 45 seconds it was authorised for")
	}
}

// Two isolated experimental modes register the narrow tool: v3 keeps its
// frozen observation, while v4 exposes scope and completeness. Exactly one
// mode still bootstraps, and no tool mode may bootstrap as well.
func TestOnlyV3AndV4ReadTopicMemoryByToolAndNeitherBootstraps(t *testing.T) {
	reading, bootstrapping := 0, 0
	for _, mode := range memoryrecall.ModeNames() {
		if !mode.Valid() {
			t.Fatalf("mode %s is listed and invalid", mode)
		}
		if mode.UsesTopicMemoryReadTool() {
			reading++
			if mode != memoryrecall.ModeAgentMemoryToolV3 && mode != memoryrecall.ModeAgentMemoryToolV4 {
				t.Fatalf("mode %s registers topic_memory_read", mode)
			}
			if mode.UsesAuthoritativeTopicBootstrap() {
				t.Fatalf("mode %s both injects the topic and offers it as a tool", mode)
			}
			if !mode.UsesMemorySearchTool() {
				t.Fatalf("mode %s reaches topic memory but not episodes", mode)
			}
			if !mode.SharesIssuedRecordsAcrossMemoryTools() {
				t.Fatalf("mode %s registers two memory tools without sharing what they issued", mode)
			}
		}
		if mode.UsesAuthoritativeTopicBootstrap() {
			bootstrapping++
			if mode != memoryrecall.ModeAgentMemoryToolV2 {
				t.Fatalf("mode %s bootstraps topic memory", mode)
			}
		}
	}
	if reading != 2 || bootstrapping != 1 {
		t.Fatalf("%d modes read the topic by tool, %d bootstrap it", reading, bootstrapping)
	}
}

func TestAgentMemoryToolV4IsIsolatedAndNotADeploymentDefault(t *testing.T) {
	complete := memoryToolConfig(t)
	complete.MemoryReadMode = string(memoryrecall.ModeAgentMemoryToolV4)
	if err := complete.Validate(); err != nil {
		t.Fatalf("a complete v4 configuration was rejected: %v", err)
	}
	for _, path := range []string{"config.docker.toml", "config.toml"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if strings.Contains(string(raw), string(memoryrecall.ModeAgentMemoryToolV4)) {
			t.Fatalf("%s names experimental v4", path)
		}
	}
	if !memoryrecall.ModeAgentMemoryToolV4.UsesTopicMemoryReadObservationV4() ||
		memoryrecall.ModeAgentMemoryToolV3.UsesTopicMemoryReadObservationV4() {
		t.Fatal("v4 observation contract is not isolated from v3")
	}
}
