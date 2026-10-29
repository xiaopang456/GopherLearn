package config

import (
	"os"
	"strings"
	"testing"

	"GopherAI/common/memoryrecall"
)

// The shipped default is unchanged by this task. A mode that has not passed a
// blind run is not a reason to ship anything, and the fifth mode has not.
func TestTheShippedDefaultIsStillLegacyV2(t *testing.T) {
	if got := defaultConfig().AgentConfig.MemoryReadMode; got != string(memoryrecall.ModeLegacyV2) {
		t.Fatalf("the shipped default is %q, not legacy_v2", got)
	}
	if memoryrecall.DefaultReadMode() != memoryrecall.ModeLegacyV2 {
		t.Fatal("DefaultReadMode must still answer legacy_v2")
	}
	raw, err := os.ReadFile("config.docker.toml")
	if err != nil {
		t.Fatalf("read the deployment config: %v", err)
	}
	if !strings.Contains(string(raw), `memoryReadMode = "legacy_v2"`) {
		t.Fatal("config.docker.toml must pin memoryReadMode to legacy_v2")
	}
	if strings.Contains(string(raw), "memoryPlanner") || strings.Contains(string(raw), "MEMORY_PLANNER") {
		t.Fatal("the deployment sample must not carry planner settings for a mode it does not run")
	}
}

// No sample or default may carry a credential, an endpoint or a model name for
// this path. The key in particular has no configuration field at all.
func TestNoSampleCarriesAPlannerIdentity(t *testing.T) {
	base := defaultConfig().AgentConfig
	if base.MemoryPlannerModel != "" || base.MemoryPlannerBaseURL != "" ||
		base.MemoryPlannerTimeoutMS != 0 || base.MemoryVerifierTimeoutMS != 0 ||
		base.MemoryPlannerMaxAttempts != 0 || base.MemoryPlannerMaxConcurrency != 0 {
		t.Fatalf("the planner has a production default: %+v", base)
	}
	for _, path := range []string{"config.docker.toml", "config.toml"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, secret := range []string{"MEMORY_PLANNER_API_KEY", "memoryPlannerApiKey", "sk-"} {
			if strings.Contains(string(raw), secret) {
				t.Fatalf("%s carries %q", path, secret)
			}
		}
	}
}

func plannerConfig(t *testing.T) AgentConfig {
	t.Helper()
	conf := defaultConfig().AgentConfig
	conf.MemoryReadMode = string(memoryrecall.ModeModelPlannedV1)
	conf.MemoryPlannerModel = "qwen-plus"
	conf.MemoryPlannerBaseURL = "https://dashscope.example.invalid/compatible-mode/v1"
	conf.MemoryPlannerTimeoutMS = 2500
	conf.MemoryVerifierTimeoutMS = 2500
	conf.MemoryPlannerMaxAttempts = 2
	conf.MemoryPlannerMaxConcurrency = 8
	return conf
}

// An incomplete planned mode stops startup. Silently falling back to another
// mode is how a mode nobody selected ends up serving production.
func TestTheModelPlannedModeRefusesToStartIncomplete(t *testing.T) {
	t.Setenv("MEMORY_PLANNER_API_KEY", "test-value-not-a-real-key")
	if err := plannerConfig(t).Validate(); err != nil {
		t.Fatalf("a complete planned configuration must validate: %v", err)
	}
	for name, mutate := range map[string]func(*AgentConfig){
		"no model":       func(c *AgentConfig) { c.MemoryPlannerModel = "" },
		"no base url":    func(c *AgentConfig) { c.MemoryPlannerBaseURL = "" },
		"no timeout":     func(c *AgentConfig) { c.MemoryPlannerTimeoutMS = 0 },
		"no verifier":    func(c *AgentConfig) { c.MemoryVerifierTimeoutMS = 0 },
		"three attempts": func(c *AgentConfig) { c.MemoryPlannerMaxAttempts = 3 },
		"no concurrency": func(c *AgentConfig) { c.MemoryPlannerMaxConcurrency = 0 },
		"absurd timeout": func(c *AgentConfig) { c.MemoryPlannerTimeoutMS = 90000 },
	} {
		conf := plannerConfig(t)
		mutate(&conf)
		if err := conf.Validate(); err == nil {
			t.Fatalf("%s must stop startup", name)
		}
	}
}

func TestTheModelPlannedModeRefusesToStartWithoutItsKey(t *testing.T) {
	t.Setenv("MEMORY_PLANNER_API_KEY", "")
	if err := plannerConfig(t).Validate(); err == nil {
		t.Fatal("a missing MEMORY_PLANNER_API_KEY must stop startup")
	}
}

// The mirror rule, and the one BUG-AGENT-140 is actually about: settings for a
// mode that is not selected must not sit there live for the day someone flips a
// string. Under every lower mode the planner is not configured, so it cannot be
// constructed and cannot read a key.
func TestLowerModesMayNotCarryPlannerSettings(t *testing.T) {
	for _, mode := range []memoryrecall.ReadMode{
		memoryrecall.ModeLegacyV2, memoryrecall.ModeEpisodeV3,
		memoryrecall.ModeEpisodeV3Lexical, memoryrecall.ModeEpisodeV3Semantic,
	} {
		conf := plannerConfig(t)
		conf.MemoryReadMode = string(mode)
		if mode == memoryrecall.ModeEpisodeV3Semantic {
			conf.MemoryTopicStandout, conf.MemoryTopicMargin = 1.3, 0.06
		}
		if err := conf.Validate(); err == nil {
			t.Fatalf("%s must refuse to carry planner settings", mode)
		}
		if mode.UsesModelPlanner() {
			t.Fatalf("%s must not report that it uses the model planner", mode)
		}
	}
}

func TestEveryExistingModeStillParsesAndValidates(t *testing.T) {
	for _, mode := range []memoryrecall.ReadMode{
		memoryrecall.ModeLegacyV2, memoryrecall.ModeEpisodeV3,
		memoryrecall.ModeEpisodeV3Lexical, memoryrecall.ModeEpisodeV3Semantic,
	} {
		parsed, err := memoryrecall.ParseReadMode(string(mode))
		if err != nil || parsed != mode {
			t.Fatalf("mode %s no longer parses: %v", mode, err)
		}
		conf := defaultConfig().AgentConfig
		conf.MemoryReadMode = string(mode)
		if mode.UsesSemanticTopicLayer() {
			conf.MemoryTopicStandout, conf.MemoryTopicMargin = 1.3, 0.06
		}
		if err := conf.Validate(); err != nil {
			t.Fatalf("mode %s no longer validates: %v", mode, err)
		}
	}
	if _, err := memoryrecall.ParseReadMode("model_planned_v2"); err == nil {
		t.Fatal("an unknown mode must refuse to start")
	}
	if !memoryrecall.ModeModelPlannedV1.UsesModelPlanner() {
		t.Fatal("model_planned_v1 must report that it uses the model planner")
	}
	if memoryrecall.ModeModelPlannedV1.UsesReadPlan() ||
		memoryrecall.ModeModelPlannedV1.UsesTopicResolver() ||
		memoryrecall.ModeModelPlannedV1.UsesRecallCycle() {
		t.Fatal("model_planned_v1 is a complete alternative path and must stack on nothing")
	}
}
