package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/BurntSushi/toml"
)

// The three modes are the plan's: off writes nothing and reads the old rolling
// summary, shadow writes without being read, and on reads the new state back.
//
// The applied mode was refused while the read side did not exist: offering it
// would have written segments that nothing reads and called it the applied
// mode. The read side is here now, so the mode is accepted, and a name that
// is not one of the three is still refused.
func TestTheSegmentModeAcceptsTheThreeModes(t *testing.T) {
	conf := defaultConfig()
	conf.SummarySegmentMode = "on"
	if err := conf.AgentConfig.Validate(); err != nil {
		t.Fatalf("the applied mode was refused: %v", err)
	}
	conf.SummarySegmentMode = "shadow"
	if err := conf.AgentConfig.Validate(); err != nil {
		t.Fatalf("the shadow mode was refused: %v", err)
	}
	conf.SummarySegmentMode = "off"
	if err := conf.AgentConfig.Validate(); err != nil {
		t.Fatalf("the off mode was refused: %v", err)
	}
	conf.SummarySegmentMode = "applied"
	if err := conf.AgentConfig.Validate(); err == nil {
		t.Fatal("a mode that is not one of the three was accepted")
	}
}

// The mode is on by default.
//
// The write side, the read side and the fallback that goes with them are
// assembled together, so a deployment that says nothing gets the layered
// summary. The way back to the old rolling summary is a setting, not the
// absence of one.
func TestTheSegmentModeIsOnByDefault(t *testing.T) {
	if mode := defaultConfig().SummarySegmentMode; mode != "on" {
		t.Fatalf("the default mode is %q, want on", mode)
	}
}

// A configuration file that does not mention the mode must not turn it off.
//
// The mode is a setting, and a file that is silent about it keeps the product
// default instead of choosing one of its own. This is the property a
// deployment depends on when it upgrades: the layered summary starts without
// the file having to say so.
func TestAConfigurationThatDoesNotMentionTheModeKeepsItOn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "silent.toml")
	if err := os.WriteFile(path, []byte("[agentConfig]\nmaxToolCalls = 3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded := defaultConfig()
	if _, err := toml.DecodeFile(path, loaded); err != nil {
		t.Fatal(err)
	}
	if mode := loaded.AgentConfig.SummarySegmentMode; mode != "on" {
		t.Fatalf("a configuration that says nothing about the mode left it %q, want on", mode)
	}
}

// And a file that asks for off still gets the old rolling summary: the
// rollback is a line in the file, and it has to keep working after the
// default changed.
func TestAConfigurationThatTurnsTheModeOffGetsTheRollingSummary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "off.toml")
	if err := os.WriteFile(path, []byte("[agentConfig]\nsummarySegmentMode = \"off\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded := defaultConfig()
	if _, err := toml.DecodeFile(path, loaded); err != nil {
		t.Fatal(err)
	}
	if mode := loaded.AgentConfig.SummarySegmentMode; mode != "off" {
		t.Fatalf("the file asked for off and the mode is %q", mode)
	}
}
