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
