package config

import "testing"

// The applied mode reads the new state back, and the parts that read it back
// are not here yet. Offering it would write segments that nothing reads and
// call it the applied mode, so it is refused until the read side lands.
func TestTheAppliedModeIsRefusedUntilTheReadSideExists(t *testing.T) {
	conf := defaultConfig()
	conf.SummarySegmentMode = "on"
	if err := conf.AgentConfig.Validate(); err == nil {
		t.Fatal("the applied mode was accepted before the read side exists")
	}
	conf.SummarySegmentMode = "shadow"
	if err := conf.AgentConfig.Validate(); err != nil {
		t.Fatalf("the shadow mode was refused: %v", err)
	}
	conf.SummarySegmentMode = "off"
	if err := conf.AgentConfig.Validate(); err != nil {
		t.Fatalf("the off mode was refused: %v", err)
	}
}

// The mode is off by default: the tables and the worker exist, but nothing
// reads the segments yet.
func TestTheSegmentModeIsOffByDefault(t *testing.T) {
	if mode := defaultConfig().SummarySegmentMode; mode != "off" {
		t.Fatalf("the default mode is %q, want off", mode)
	}
}
