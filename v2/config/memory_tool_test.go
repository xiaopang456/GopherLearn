package config

import (
	"os"
	"strings"
	"testing"

	"GopherAI/common/memoryrecall"
)

// memoryToolConfig is a complete, valid agent_memory_tool_v1 configuration.
func memoryToolConfig(t *testing.T) AgentConfig {
	t.Helper()
	conf := defaultConfig().AgentConfig
	conf.MemoryReadMode = string(memoryrecall.ModeAgentMemoryToolV1)
	conf.MemoryToolMaxCalls = 2
	conf.MemoryToolTopK = 5
	conf.MemoryToolCandidateDepth = 20
	conf.MemoryToolMinConfidence = 0.6
	conf.MemoryToolTimeoutMS = 3000
	conf.MaxToolCalls, conf.MaxModelCalls, conf.MaxGraphSteps = 6, 7, 16
	if err := conf.Validate(); err != nil {
		t.Fatalf("the reference memory-tool configuration must validate: %v", err)
	}
	return conf
}

// The sixth mode does not become anybody's default by existing, and neither the
// shipped defaults nor the deployment sample may carry its settings.
func TestMemoryToolIsNotADefaultAnywhere(t *testing.T) {
	base := defaultConfig().AgentConfig
	if base.MemoryReadMode != string(memoryrecall.ModeLegacyV2) {
		t.Fatalf("default mode = %q", base.MemoryReadMode)
	}
	if base.MemoryToolMaxCalls != 0 || base.MemoryToolTopK != 0 || base.MemoryToolCandidateDepth != 0 ||
		base.MemoryToolMinConfidence != 0 || base.MemoryToolTimeoutMS != 0 {
		t.Fatalf("the memory tool has a production default: %+v", base)
	}
	if base.MaxToolCalls != 3 || base.MaxModelCalls != 4 || base.MaxGraphSteps != 10 {
		t.Fatalf("the shipped run budget moved: %d/%d/%d", base.MaxToolCalls, base.MaxModelCalls, base.MaxGraphSteps)
	}
	for _, path := range []string{"config.docker.toml", "config.toml"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if strings.Contains(string(raw), "memoryTool") || strings.Contains(string(raw), "agent_memory_tool_v1") {
			t.Fatalf("%s carries settings for a mode it does not run", path)
		}
	}
}

// Both directions. An incomplete mode must not start, and a complete
// configuration under a mode that cannot use it is a retrieval budget nobody
// reviewed, sitting live for the day someone flips a string.
func TestMemoryToolConfigurationFailsClosedBothWays(t *testing.T) {
	incomplete := defaultConfig().AgentConfig
	incomplete.MemoryReadMode = string(memoryrecall.ModeAgentMemoryToolV1)
	if err := incomplete.Validate(); err == nil {
		t.Fatal("agent_memory_tool_v1 started with no memoryTool settings")
	}

	stray := defaultConfig().AgentConfig
	stray.MemoryToolTopK = 5
	if err := stray.Validate(); err == nil {
		t.Fatal("legacy_v2 accepted a memoryTool setting")
	}

	strayBudget := defaultConfig().AgentConfig
	strayBudget.MaxToolCalls, strayBudget.MaxModelCalls, strayBudget.MaxGraphSteps = 6, 7, 16
	if err := strayBudget.Validate(); err == nil {
		t.Fatal("legacy_v2 accepted the wider run budget")
	}
}

// Every bound is checked, and the ones that decide how much of a learner's
// history one call may reach are checked in both directions.
func TestMemoryToolBoundsAreEnforced(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*AgentConfig)
	}{
		{name: "three searches", mutate: func(c *AgentConfig) { c.MemoryToolMaxCalls = 3 }},
		{name: "no search", mutate: func(c *AgentConfig) { c.MemoryToolMaxCalls = 0 }},
		{name: "top_k too large", mutate: func(c *AgentConfig) { c.MemoryToolTopK = 7 }},
		{name: "depth too shallow", mutate: func(c *AgentConfig) { c.MemoryToolCandidateDepth = 2 }},
		{name: "depth unbounded", mutate: func(c *AgentConfig) { c.MemoryToolCandidateDepth = 500 }},
		{name: "confidence floor removed", mutate: func(c *AgentConfig) { c.MemoryToolMinConfidence = 0 }},
		{name: "confidence above one", mutate: func(c *AgentConfig) { c.MemoryToolMinConfidence = 1.5 }},
		{name: "timeout too small", mutate: func(c *AgentConfig) { c.MemoryToolTimeoutMS = 10 }},
		{name: "timeout too large", mutate: func(c *AgentConfig) { c.MemoryToolTimeoutMS = 60000 }},
		{name: "tool budget narrowed", mutate: func(c *AgentConfig) { c.MaxToolCalls = 3 }},
		{name: "model budget narrowed", mutate: func(c *AgentConfig) { c.MaxModelCalls = 4 }},
		{name: "graph budget narrowed", mutate: func(c *AgentConfig) { c.MaxGraphSteps = 10 }},
		{name: "deadline widened", mutate: func(c *AgentConfig) { c.DeadlineSeconds = 90 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			conf := memoryToolConfig(t)
			test.mutate(&conf)
			if err := conf.Validate(); err == nil {
				t.Fatalf("%s was accepted", test.name)
			}
		})
	}
}

// The STABILIZE-036 window is an experiment behind an explicit setting. Absent
// means the shipped window, an unknown value stops startup rather than falling
// back to it, and no other mode may carry it.
func TestWindowPolicyIsExperimentalAndFailsClosed(t *testing.T) {
	if base := defaultConfig().AgentConfig; strings.TrimSpace(base.MemoryToolWindowPolicy) != "" {
		t.Fatalf("the window experiment has a production default: %q", base.MemoryToolWindowPolicy)
	}

	for _, policy := range []string{"", "w0_fixed", "token_bounded_mixed_v1"} {
		conf := memoryToolConfig(t)
		conf.MemoryToolWindowPolicy = policy
		if err := conf.Validate(); err != nil {
			t.Fatalf("policy %q was refused: %v", policy, err)
		}
	}

	// A misspelled experiment that quietly ran the baseline would report the
	// baseline's behaviour under the experiment's name.
	conf := memoryToolConfig(t)
	conf.MemoryToolWindowPolicy = "token_bounded_mixed_v2"
	if err := conf.Validate(); err == nil {
		t.Fatal("an unknown window policy was accepted")
	}

	stray := defaultConfig().AgentConfig
	stray.MemoryToolWindowPolicy = "token_bounded_mixed_v1"
	if err := stray.Validate(); err == nil {
		t.Fatal("legacy_v2 accepted the window experiment")
	}
}

// The planner and the tool are different modes, and neither may borrow the
// other's settings.
func TestMemoryToolAndPlannerSettingsDoNotMix(t *testing.T) {
	conf := memoryToolConfig(t)
	conf.MemoryPlannerModel = "qwen-plus"
	conf.MemoryPlannerBaseURL = "https://dashscope.example.invalid/compatible-mode/v1"
	if err := conf.Validate(); err == nil {
		t.Fatal("agent_memory_tool_v1 accepted planner settings")
	}
}
