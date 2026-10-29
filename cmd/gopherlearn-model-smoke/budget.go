package main

import (
	"context"
	"fmt"

	"github.com/xiaopang456/GopherLearn/internal/model"
)

// metered puts the hard request ceiling on the path to the provider. It counts
// attempts rather than successes, because a request that fails has already been
// billed, and it decides before calling the inner model so that a request over the
// cap never reaches a socket at all.
//
// There is deliberately no retry logic here or below it: one user-visible attempt
// per counted request. The cap is process-local on purpose - it is a guard against
// this command misbehaving, not an accounting system.
type metered struct {
	inner    model.ChatModel
	limit    int
	requests int
	failures int
}

func newMetered(inner model.ChatModel, limit int) *metered {
	if limit <= 0 {
		limit = requestCap
	}
	return &metered{inner: inner, limit: limit}
}

var _ model.ChatModel = (*metered)(nil)

// reserve takes the next slot or refuses before anything is sent.
func (m *metered) reserve() error {
	if m.requests >= m.limit {
		return fmt.Errorf("%w: %d of %d already used", ErrRequestCapExceeded, m.requests, m.limit)
	}
	m.requests++
	return nil
}

func (m *metered) Complete(ctx context.Context, request model.Request) (model.Completion, error) {
	if err := m.reserve(); err != nil {
		return model.Completion{}, err
	}
	completion, err := m.inner.Complete(ctx, request)
	if err != nil {
		m.failures++
		return model.Completion{}, err
	}
	return completion, nil
}

func (m *metered) Stream(ctx context.Context, request model.Request, onDelta model.DeltaHandler) (model.Completion, error) {
	if err := m.reserve(); err != nil {
		return model.Completion{}, err
	}
	completion, err := m.inner.Stream(ctx, request, onDelta)
	if err != nil {
		m.failures++
		return model.Completion{}, err
	}
	return completion, nil
}

// CompleteWithTools keeps the meter in front of the tool-calling path as well. A
// wrapper that forwarded only Complete and Complete-without-tools would let a loop
// spend calls the budget never saw, and silently turning the inner model into a
// non-tool model would change what a turn can do.
func (m *metered) CompleteWithTools(ctx context.Context, request model.Request) (model.Completion, error) {
	toolModel, ok := m.inner.(model.ToolChatModel)
	if !ok {
		return model.Completion{}, fmt.Errorf("%w: the wrapped client cannot call tools", model.ErrToolsUnsupported)
	}
	if err := m.reserve(); err != nil {
		return model.Completion{}, err
	}
	completion, err := toolModel.CompleteWithTools(ctx, request)
	if err != nil {
		m.failures++
		return model.Completion{}, err
	}
	return completion, nil
}

var _ model.ToolChatModel = (*metered)(nil)
