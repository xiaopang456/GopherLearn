package isolated

import (
	"testing"

	agentService "GopherAI/service/agent"
)

// The block header is charged in two places, and the two places have to
// charge the same number.
//
// The configuration raises the block's budget to what one whole segment
// costs, header included; the renderer takes the header off the budget before
// it renders a segment. If the two numbers are not the same number, the block
// is raised to fit a segment the renderer cannot fit, and the segment is
// dropped with nothing said about it.
//
// The header's size is restated in the config package because that package
// cannot import the agent's. This is where the two copies are held together.
func TestTheConfigurationAndTheRendererChargeTheSameHeader(t *testing.T) {
	agentConfig := testSettings(t).Config.AgentConfig
	agentConfig.SummaryAssistantContent = "on"
	agentConfig.ContextMaxSegmentTokens = 300
	agentConfig.ContextMaxResidentSegmentTokens = 370
	agentConfig.ContextMaxRetrievedSegmentTokens = 370

	want := agentConfig.EffectiveSegmentTokens() + agentService.SummarySegmentBlockHeaderTokens()
	if got := agentConfig.EffectiveResidentSegmentTokens(); got != want {
		t.Fatalf("the resident block is raised to %d tokens, a segment and its header cost %d",
			got, want)
	}
	if got := agentConfig.EffectiveRetrievedSegmentTokens(); got != want {
		t.Fatalf("the retrieved block is raised to %d tokens, a segment and its header cost %d",
			got, want)
	}
}
