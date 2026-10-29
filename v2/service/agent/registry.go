package agent

import (
	"context"

	"github.com/cloudwego/eino/components/tool"
)

// CompositeToolRegistry joins independent capability registries without
// allowing one feature to know how another feature creates per-run state.
type CompositeToolRegistry struct {
	Registries []ToolRegistry
}

func (r CompositeToolRegistry) Tools(ctx context.Context, run RunContext) ([]tool.BaseTool, error) {
	var result []tool.BaseTool
	for _, registry := range r.Registries {
		if registry == nil {
			continue
		}
		items, err := registry.Tools(ctx, run)
		if err != nil {
			return nil, err
		}
		result = append(result, items...)
	}
	return result, nil
}
