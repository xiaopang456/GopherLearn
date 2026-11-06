package conversation

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"GopherAI/config"
)

func writeScaledConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "scaled.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write the configuration: %v", err)
	}
	return path
}

// A file that says nothing about a field must leave it alone.
//
// The whole point of the overlay is that a scaled run states what it changes;
// scoring a budget the file never mentions would make every run's numbers depend
// on defaults nobody wrote down.
func TestAScaledConfigChangesOnlyTheKeysItNames(t *testing.T) {
	path := writeScaledConfig(t, `
[agentConfig]
contextInputTargetTokens = 9700
contextMaxWhiteboardTokens = 480
`)
	target := config.AgentConfig{ContextMaxTopicUserMemoryTokens: 1200, ContextMaxSummaryTokens: 2000}

	if err := applyScaledAgentConfig(&target, path); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if target.ContextInputTargetTokens != 9700 {
		t.Fatalf("the input target is %d, want 9700", target.ContextInputTargetTokens)
	}
	if target.ContextMaxWhiteboardTokens != 480 {
		t.Fatalf("the board budget is %d, want 480", target.ContextMaxWhiteboardTokens)
	}
	if target.ContextMaxTopicUserMemoryTokens != 1200 || target.ContextMaxSummaryTokens != 2000 {
		t.Fatalf("a field the file does not name was changed: topic=%d summary=%d",
			target.ContextMaxTopicUserMemoryTokens, target.ContextMaxSummaryTokens)
	}
}

// A key written as zero means zero, not "keep the default".
//
// This is the case that turns a scaled run into a different experiment: three
// groups are compared on what they switch off, and "off" has to be expressible.
func TestAScaledConfigCanSwitchABudgetOff(t *testing.T) {
	path := writeScaledConfig(t, `
[agentConfig]
contextMaxGlobalUserMemoryTokens = 0
contextMaxMasteryTokens = 0
episodeRecallMode = "disabled"
`)
	target := config.AgentConfig{
		ContextMaxGlobalUserMemoryTokens: 800,
		ContextMaxMasteryTokens:          900,
		EpisodeRecallMode:                "shadow_only",
	}

	if err := applyScaledAgentConfig(&target, path); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if target.ContextMaxGlobalUserMemoryTokens != 0 || target.ContextMaxMasteryTokens != 0 {
		t.Fatalf("a budget written as zero was left at its default: global=%d mastery=%d",
			target.ContextMaxGlobalUserMemoryTokens, target.ContextMaxMasteryTokens)
	}
	if target.EpisodeRecallMode != "disabled" {
		t.Fatalf("the episode mode is %q", target.EpisodeRecallMode)
	}
}

// A misspelled key is refused. A run that quietly ignored it would report
// numbers for a configuration nobody wrote.
func TestAScaledConfigRefusesAKeyItDoesNotSet(t *testing.T) {
	path := writeScaledConfig(t, `
[agentConfig]
contextMaxWhitebordTokens = 480
`)
	var target config.AgentConfig
	err := applyScaledAgentConfig(&target, path)
	if err == nil {
		t.Fatal("a misspelled key was accepted")
	}
	if !strings.Contains(err.Error(), "does not set") {
		t.Fatalf("error = %v, want it to name the key as not part of this run", err)
	}
}

// No file means no overlay, which is the product's own configuration.
func TestAScaledConfigIsOptional(t *testing.T) {
	var target config.AgentConfig
	if err := applyScaledAgentConfig(&target, ""); err != nil {
		t.Fatalf("an absent configuration failed: %v", err)
	}
}

// The harness builds two arms, and refuses anything else instead of pretending.
//
// A mode it cannot assemble is a valid product configuration and an invalid
// input here. It used to be overwritten to the layered mode, which spent the
// segment-writing calls first and then rendered empty blocks from budgets that
// belonged to another arm - a wrong answer that looked like a result.
func TestTheHarnessRefusesAnArmItCannotBuild(t *testing.T) {
	for _, mode := range []string{"on", "off"} {
		if err := requireSupportedArm(mode); err != nil {
			t.Fatalf("the %q arm was refused: %v", mode, err)
		}
	}
	for _, mode := range []string{"shadow", ""} {
		err := requireSupportedArm(mode)
		if err == nil {
			t.Fatalf("mode %q was accepted", mode)
		}
		if !strings.Contains(err.Error(), "builds two arms") {
			t.Fatalf("mode %q produced %v, want an error naming the arms", mode, err)
		}
	}
}

// A run that names no configuration is this harness measuring its own arm.
//
// Without this, every existing invocation would break: the product's default
// mode is off, and refusing it would make the tool unusable unless the caller
// also wrote a file that restates the harness's own purpose.
func TestARunWithoutAConfigurationRunsTheHarnessArm(t *testing.T) {
	mode, err := resolveArm("off", false)
	if err != nil {
		t.Fatalf("a run with no file was refused: %v", err)
	}
	if mode != "on" {
		t.Fatalf("the run uses mode %q, want the harness arm", mode)
	}
	// A file that names either arm is honoured as written.
	for _, want := range []string{"on", "off"} {
		if mode, err := resolveArm(want, true); err != nil || mode != want {
			t.Fatalf("a file naming %q produced %q, %v", want, mode, err)
		}
	}
	// A file that names a mode this harness cannot build is refused, not
	// overridden.
	if _, err := resolveArm("shadow", true); err == nil {
		t.Fatal("a file naming an unbuilt arm was overridden instead of refused")
	}
}
