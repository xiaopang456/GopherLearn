package isolated

import (
	"context"
	"net/http"
	"testing"

	"GopherAI/common/memoryrecall"
	"GopherAI/config"
	agentService "GopherAI/service/agent"
)

// These three cases used to sit in the root package of this module, where they
// ran against copies of the wiring pasted into the test file. They are here
// now so that they read the wiring this entry actually runs.

func TestHTTPServerHasResourceTimeoutsWithoutBreakingSSE(t *testing.T) {
	server := newHTTPServer("127.0.0.1:0", http.NewServeMux())
	if server.ReadHeaderTimeout != httpReadHeaderTimeout || server.ReadTimeout != httpReadTimeout || server.IdleTimeout != httpIdleTimeout {
		t.Fatalf("unexpected HTTP timeouts: header=%v read=%v idle=%v", server.ReadHeaderTimeout, server.ReadTimeout, server.IdleTimeout)
	}
	if server.MaxHeaderBytes != httpMaxHeaderBytes {
		t.Fatalf("max header bytes=%d, want %d", server.MaxHeaderBytes, httpMaxHeaderBytes)
	}
	if server.WriteTimeout != 0 {
		t.Fatalf("write timeout=%v, want zero so a streamed answer is not cut off", server.WriteTimeout)
	}
}

func TestAgentMemoryToolV3ApplicationWiringIncludesTopicReadBudget(t *testing.T) {
	conf := config.AgentConfig{
		MaxToolCalls:       6,
		MaxModelCalls:      7,
		MaxGraphSteps:      16,
		DeadlineSeconds:    45,
		MemoryToolMaxCalls: 2,
	}
	got := agentRunnerConfig(memoryrecall.ModeAgentMemoryToolV3, conf, "")
	if got.PerTool.MemorySearch != 2 || got.PerTool.TopicMemoryRead != 1 {
		t.Fatalf("v3 per-tool limits = %#v", got.PerTool)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("v3 runner config is invalid: %v", err)
	}
	v4 := agentRunnerConfig(memoryrecall.ModeAgentMemoryToolV4, conf, "")
	if v4.PerTool != got.PerTool || v4.MaxToolCalls != got.MaxToolCalls ||
		v4.MaxModelCalls != got.MaxModelCalls || v4.MaxGraphSteps != got.MaxGraphSteps ||
		v4.Deadline != got.Deadline {
		t.Fatalf("v4 budget drifted from v3: v3=%#v v4=%#v", got, v4)
	}
}

func TestTheReplyBudgetReachesTheAgentRunner(t *testing.T) {
	// The window keeps this much for the reply, and every generation of the
	// turn is sent with it as the output limit. The two have to be the same
	// number, or the request is measured against a budget it was not sent.
	conf := config.AgentConfig{
		MaxToolCalls: 6, MaxModelCalls: 7, MaxGraphSteps: 16, DeadlineSeconds: 45,
		ContextOutputReserveTokens: 3600,
	}
	got := agentRunnerConfig(memoryrecall.ModeAgentMemoryToolV3, conf, "")
	if got.OutputReserveTokens != 3600 {
		t.Fatalf("the runner's reply budget = %d, want the deployment's 3600", got.OutputReserveTokens)
	}
}

func TestTopicMemoryReadRegistryIsBuiltOnlyForItsMode(t *testing.T) {
	provider := agentService.AuthoritativeTopicMemoryProviderFunc(
		func(context.Context, agentService.RunContext, []string) ([]agentService.TopicMemoryRecord, error) {
			return nil, nil
		})
	snapshotProvider := agentService.AuthoritativeTopicMemorySnapshotProviderFunc(
		func(context.Context, agentService.RunContext, []string) (agentService.AuthoritativeTopicMemorySnapshot, error) {
			return agentService.AuthoritativeTopicMemorySnapshot{}, nil
		})
	for _, mode := range memoryrecall.ModeNames() {
		got, err := buildTopicMemoryReadRegistry(mode, provider, snapshotProvider)
		if err != nil {
			t.Fatalf("mode %s: %v", mode, err)
		}
		if (got != nil) != mode.UsesTopicMemoryReadTool() {
			t.Fatalf("mode %s registry present = %t", mode, got != nil)
		}
	}
}

func TestTopicMemoryReadRegistryFailsClosedWithoutProvider(t *testing.T) {
	if _, err := buildTopicMemoryReadRegistry(memoryrecall.ModeAgentMemoryToolV3, nil, nil); err == nil {
		t.Fatal("v3 accepted a missing topic memory provider")
	}
	if _, err := buildTopicMemoryReadRegistry(memoryrecall.ModeAgentMemoryToolV4, nil, nil); err == nil {
		t.Fatal("v4 accepted a missing topic memory snapshot provider")
	}
}
