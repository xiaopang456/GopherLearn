package main

import (
 "net/http"
 "time"
 "GopherAI/common/memoryrecall"
 "GopherAI/config"
 agentService "GopherAI/service/agent"
 agentTool "GopherAI/service/agent/tool"
)

const (
	httpReadHeaderTimeout = 5 * time.Second
	httpReadTimeout       = 30 * time.Second
	httpIdleTimeout       = 60 * time.Second
	httpMaxHeaderBytes    = 1 << 20
)

func newHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: httpReadHeaderTimeout,
		ReadTimeout:       httpReadTimeout,
		IdleTimeout:       httpIdleTimeout,
		MaxHeaderBytes:    httpMaxHeaderBytes,
		// WriteTimeout intentionally remains zero. Agent and legacy chat stream
		// SSE responses for longer than a fixed socket write deadline; their
		// model/tool work already has request-scoped deadlines and cancellation.
	}
}


func agentRunnerConfig(mode memoryrecall.ReadMode, agentConf config.AgentConfig) agentService.RunnerConfig {
	runnerConfig := agentService.RunnerConfig{
		MaxToolCalls: agentConf.MaxToolCalls, MaxModelCalls: agentConf.MaxModelCalls,
		MaxGraphSteps: agentConf.MaxGraphSteps,
		Deadline:      time.Duration(agentConf.DeadlineSeconds) * time.Second,
	}
	if mode.UsesMemorySearchTool() {
		runnerConfig.PerTool = agentService.AgentMemoryToolRunnerConfig().PerTool
		runnerConfig.PerTool.MemorySearch = agentConf.MemoryToolMaxCalls
		// A mode that registers memory_search but is not studying the model's
		// first tool choice keeps the server's knowledge-first rule
		// (STABILIZE-056). The four agent_memory_tool identities are studying
		// exactly that, so they keep deciding it themselves.
		runnerConfig.ForcesKnowledgeSearchWithMemoryTool = !mode.DefersFirstToolChoiceToModel()
	}
	if mode.UsesTopicMemoryReadTool() {
		topicConfig := agentService.AgentMemoryToolV3RunnerConfig()
		if mode.UsesTopicMemoryReadObservationV4() {
			topicConfig = agentService.AgentMemoryToolV4RunnerConfig()
		}
		runnerConfig.PerTool.TopicMemoryRead = topicConfig.PerTool.TopicMemoryRead
	}
	// read_history_message gains a per-capability ceiling only where this round
	// widened what it can reach: two calls, which is one readout of the summary's
	// evidence and one of an episode's. Every older mode keeps the budget its
	// published measurements were taken under - bounded by the whole-run total
	// alone - so the new capability stays the only variable. It is clamped to that
	// total because RunnerConfig.Validate rejects a per-capability limit above it.
	// v2 reads the summary's evidence back; v3 reads the summary's and an episode's.
	// Neither v1's nor v2's answer moves: the predicate is exactly the identities that
	// may read evidence back, and both get the same ceiling.
	if mode.ReadsEvidenceBack() {
		runnerConfig.PerTool.HistoryRead = min(agentService.DefaultHistoryReadCalls, runnerConfig.MaxToolCalls)
	}
	// STABILIZE-061. The surface and the ordering rule travel together with the
	// mode rather than with the deployment's tuning knobs: a config file that
	// could rename the model's tools without changing the mode would make the
	// mode's identity stop describing what the model saw.
	if mode.UsesExplicitMemoryActionNames() {
		runnerConfig.ToolSurface = agentService.SurfaceExplicitActions
		if mode.UsesExplicitKnowledgeSourceName() {
			runnerConfig.ToolSurface = agentService.SurfaceExplicitSources
		}
	}
	if mode.OrdersMemoryDraftBeforeMemoryRead() {
		runnerConfig.ForcesMemoryDraftBeforeMemoryRead = true
	}
	return runnerConfig
}

// buildTopicMemoryReadRegistry keeps the application wiring under the same
// mode predicate the ContextBuilder and runner budget use. STABILIZE-040's
// evaluator constructed this registry directly; without the application-side
// branch a configured v3/v4 process would advertise the tool in its prompt
// while never registering it.
func buildTopicMemoryReadRegistry(mode memoryrecall.ReadMode,
	provider agentService.AuthoritativeTopicMemoryProvider,
	snapshotProvider agentService.AuthoritativeTopicMemorySnapshotProvider) (agentService.ToolRegistry, error) {
	if !mode.UsesTopicMemoryReadTool() {
		return nil, nil
	}
	if mode.UsesTopicMemoryReadObservationV4() {
		return agentTool.NewTopicMemoryReadV4Registry(snapshotProvider, agentTool.DefaultTopicMemoryReadConfig())
	}
	return agentTool.NewTopicMemoryReadRegistry(provider, agentTool.DefaultTopicMemoryReadConfig())
}

