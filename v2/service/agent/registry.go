package agent

import (
	"context"
	"errors"

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

// ProbeRelevance asks the registries whether any of them can run a
// server-side pre-retrieval.
//
// It is the same optional-capability pattern as the tool table itself: a
// registry that does not implement it has no pre-retrieval, and the run
// proceeds as it always did. The first registry that answers owns the
// question - there is one knowledge base per user, and two answers would mean
// two verdicts on the same material.
//
// A registry that holds no prober is an error rather than an empty answer,
// because an empty answer is a verdict: it would say the material could not be
// searched, which is not what "nobody here can search" means.
func (r CompositeToolRegistry) ProbeRelevance(ctx context.Context, run RunContext, question string, threshold float64) (RelevanceOutcome, error) {
	for _, registry := range r.Registries {
		if registry == nil {
			continue
		}
		prober, ok := registry.(RelevanceProber)
		if !ok {
			continue
		}
		return prober.ProbeRelevance(ctx, run, question, threshold)
	}
	return RelevanceOutcome{}, ErrNoRelevanceProber
}

// ErrNoRelevanceProber says no registry in the composition can run a
// server-side pre-retrieval.
var ErrNoRelevanceProber = errors.New("no registry provides a relevance prober")
