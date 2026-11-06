package config

import "testing"

// The three modes are the plan's: off writes nothing, shadow writes without
// being read, and on reads the new state back.
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

// The mode is off by default: the tables and the worker exist, but nothing
// reads the segments yet.
func TestTheSegmentModeIsOffByDefault(t *testing.T) {
	if mode := defaultConfig().SummarySegmentMode; mode != "off" {
		t.Fatalf("the default mode is %q, want off", mode)
	}
}
