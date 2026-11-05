package isolated

import (
	agentService "GopherAI/service/agent"
	"context"
	"fmt"
	"github.com/cloudwego/eino/components/tool"
)

type checkedRegistry struct{ inner agentService.ToolRegistry }

func (r checkedRegistry) Tools(ctx context.Context, run agentService.RunContext) ([]tool.BaseTool, error) {
	items, err := r.inner.Tools(ctx, run)
	if err != nil {
		return nil, err
	}
	names := map[string]bool{}
	capabilities := map[agentService.MemoryToolCapability]bool{}
	for _, item := range items {
		if item == nil {
			return nil, fmt.Errorf("nil assembled tool")
		}
		info, err := item.Info(ctx)
		if err != nil {
			return nil, err
		}
		if info == nil {
			return nil, fmt.Errorf("tool info missing")
		}
		capability := agentService.CapabilityOf(info.Name)
		if capability == "" {
			return nil, fmt.Errorf("unknown assembled tool %q", info.Name)
		}
		if names[info.Name] || capabilities[capability] {
			return nil, fmt.Errorf("duplicate assembled tool %q", info.Name)
		}
		names[info.Name] = true
		capabilities[capability] = true
	}
	return items, nil
}

// checkedProbingRegistry is checkedRegistry plus the optional server-side
// pre-retrieval.
//
// The capability is exposed only when the wrapped registry really has it. A
// wrapper that always claimed it would turn "this deployment has no
// pre-retrieval" into an error, and the run would have to tell apart "nobody
// here can probe" from "the probe failed" - two different situations that both
// end in the forced document search, but only one of them is a mistake.
type checkedProbingRegistry struct{ inner agentService.ToolRegistry }

func (r checkedProbingRegistry) Tools(ctx context.Context, run agentService.RunContext) ([]tool.BaseTool, error) {
	return checkedRegistry{r.inner}.Tools(ctx, run)
}

// ProbeRelevance hands the pre-retrieval to the registry that can answer it.
//
// The questions and the threshold travel unchanged: the server decided what
// counts as relevant before the registry was asked, and a wrapper that
// changed either of them would decide the question's relevance on the
// registry's behalf.
func (r checkedProbingRegistry) ProbeRelevance(ctx context.Context, run agentService.RunContext, question string, threshold float64) (agentService.RelevanceOutcome, error) {
	prober, ok := r.inner.(agentService.RelevanceProber)
	if !ok {
		return agentService.RelevanceOutcome{}, agentService.ErrNoRelevanceProber
	}
	return prober.ProbeRelevance(ctx, run, question, threshold)
}

// registryForRunner wraps the assembled registry for the runner.
//
// The runner reads two things off the registry: the tool table, which the
// checks in checkedRegistry protect, and the optional pre-retrieval, which a
// wrapper that forgets to forward removes silently. The integrated entry lost
// the pre-retrieval that way: the runner asked the wrapper for a relevance
// prober, the wrapper had none, and every turn fell back to the forced
// document search - without a line of log saying so, and without the feature
// ever being exercised in production.
//
// The capability survives the wrapping only when the assembled registry has
// it, so a deployment that cannot probe keeps the behaviour it had.
func registryForRunner(inner agentService.ToolRegistry) agentService.ToolRegistry {
	if _, ok := inner.(agentService.RelevanceProber); ok {
		return checkedProbingRegistry{inner}
	}
	return checkedRegistry{inner}
}
