package config

import "testing"

// A deployment that never sends the board or the segments must not be rejected
// because of their budgets.
//
// The defect this guards: the four block budgets became configuration with the
// shipped numbers as defaults, and they were added to the "leaves no room for
// required context" sum unconditionally. Every existing deployment with a tight
// input target and the layered mode off - the shipped default - then failed to
// load, having changed nothing.
func TestTheBlockBudgetsOnlyCountWhenTheModeReadsThem(t *testing.T) {
	conf := defaultConfig()
	// A target that fits the five always-sent budgets and not the four blocks:
	// 800 + 1200 + 2000 + 900 + 1000 = 5900 against 13000 - 6500 = 6500.
	conf.ContextInputTargetTokens = 13000
	conf.SummarySegmentMode = "off"

	if err := conf.AgentConfig.Validate(); err != nil {
		t.Fatalf("a mode-off deployment with a tight input target was refused: %v", err)
	}
	conf.SummarySegmentMode = "shadow"
	if err := conf.AgentConfig.Validate(); err != nil {
		t.Fatalf("the shadow mode does not read the blocks and must load: %v", err)
	}

	// The same numbers with the mode that does read them: now the budgets are
	// part of the window and the configuration is genuinely too small.
	conf.SummarySegmentMode = "on"
	if err := conf.AgentConfig.Validate(); err == nil {
		t.Fatal("the applied mode accepted budgets that leave no room for required context")
	}
}

// With the mode on and room to spare, the same configuration loads: the refusal
// above is about the sum, not about the mode.
func TestTheAppliedModeLoadsWhenTheBudgetsFit(t *testing.T) {
	conf := defaultConfig()
	conf.SummarySegmentMode = "on"
	// Room for the five, the four blocks, the system prompt and the question.
	conf.ContextInputTargetTokens = 20000
	conf.ContextModelWindowTokens = 32768

	if err := conf.AgentConfig.Validate(); err != nil {
		t.Fatalf("a configuration with room was refused: %v", err)
	}
}

// The check that the budgets leave room for the required context belongs to
// every mode.
//
// The budgets that reach the window in the other modes are the same ones, so a
// configuration whose own budgets leave no room is refused whether the layered
// summary is on, off or in shadow. Only the terms of the sum depend on the
// mode - the check itself does not.
func TestTheCommonBudgetCheckAppliesInEveryMode(t *testing.T) {
	for _, mode := range []string{"on", "off", "shadow"} {
		t.Run(mode, func(t *testing.T) {
			conf := defaultConfig()
			conf.SummarySegmentMode = mode
			// The five budgets that reach the window in every mode:
			// 800 + 1200 + 2000 + 900 + 1000 = 5900 against 12000 - 6500 = 5500.
			conf.ContextInputTargetTokens = 12000

			if err := conf.AgentConfig.Validate(); err == nil {
				t.Fatalf("the %s mode accepted budgets that leave no room for required context", mode)
			}
		})
	}
}
