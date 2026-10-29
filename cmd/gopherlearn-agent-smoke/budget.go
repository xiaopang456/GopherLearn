package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/xiaopang456/GopherLearn/internal/agent"
	"github.com/xiaopang456/GopherLearn/internal/model"
	"github.com/xiaopang456/GopherLearn/internal/responder"
)

// attempt is one round trip between the harness and the model: exactly what was sent
// and exactly what came back. The record reads these to state structural facts about
// the tool-calling protocol - was the first response a tool call, did the result fed
// back carry the same call id - without storing any prompt, answer or tool output.
type attempt struct {
	role       string
	sent       model.Request
	completion model.Completion
	err        error
}

// metered puts the two-request ceiling on the path to the provider and keeps the
// transcript the checks need. It counts attempts, not successes: a request that failed
// was still sent and still billed.
//
// This is a deliberate second copy of the provider smoke check's wrapper rather than a
// shared package. The two commands freeze different budgets, record different facts, and
// the earlier one is already accepted evidence - folding them together would reopen
// files that this phase has no reason to touch.
type metered struct {
	inner    model.ChatModel
	limit    int
	requests int
	failures int
	attempts []attempt
}

func newMetered(inner model.ChatModel, limit int) *metered {
	if limit <= 0 {
		limit = requestCap
	}
	return &metered{inner: inner, limit: limit}
}

func (m *metered) reserve() error {
	if m.requests >= m.limit {
		return fmt.Errorf("%w: %d of %d already used", ErrRequestCapExceeded, m.requests, m.limit)
	}
	m.requests++
	return nil
}

func (m *metered) record(role string, request model.Request, completion model.Completion, err error) {
	if err != nil {
		m.failures++
	}
	m.attempts = append(m.attempts, attempt{role: role, sent: request, completion: completion, err: err})
}

func (m *metered) Complete(ctx context.Context, request model.Request) (model.Completion, error) {
	if err := m.reserve(); err != nil {
		return model.Completion{}, err
	}
	completion, err := m.inner.Complete(ctx, request)
	m.record("complete", request, completion, err)
	return completion, err
}

func (m *metered) Stream(ctx context.Context, request model.Request, onDelta model.DeltaHandler) (model.Completion, error) {
	if err := m.reserve(); err != nil {
		return model.Completion{}, err
	}
	completion, err := m.inner.Stream(ctx, request, onDelta)
	m.record("stream", request, completion, err)
	return completion, err
}

// CompleteWithTools keeps the ceiling in front of the path this smoke is about. A
// wrapper that dropped the method would make the harness refuse to start, which is the
// correct failure: no budget-free side channel around the cap.
func (m *metered) CompleteWithTools(ctx context.Context, request model.Request) (model.Completion, error) {
	toolModel, ok := m.inner.(model.ToolChatModel)
	if !ok {
		return model.Completion{}, fmt.Errorf("%w: the wrapped client cannot call tools", model.ErrToolsUnsupported)
	}
	if err := m.reserve(); err != nil {
		return model.Completion{}, err
	}
	completion, err := toolModel.CompleteWithTools(ctx, request)
	m.record("tool", request, completion, err)
	return completion, err
}

// firstToolCall returns the call the first response made, with the ordinal the checks
// use in messages.
func (m *metered) firstToolCall() (model.ToolCall, bool) {
	for _, entry := range m.attempts {
		if entry.role != "tool" || len(entry.completion.ToolCalls) == 0 {
			continue
		}
		return entry.completion.ToolCalls[0], true
	}
	return model.ToolCall{}, false
}

// toolResultInRequest returns the role=tool message carried by the request at index, if
// that request carried one. Index 1 is the second request - the answer turn - so this is
// how a check says "the model was actually handed the tool result" rather than "a tool
// result appears somewhere in the transcript".
func (m *metered) toolResultInRequest(index int) (model.Message, bool) {
	if index < 0 || index >= len(m.attempts) {
		return model.Message{}, false
	}
	for _, message := range m.attempts[index].sent.Messages {
		if message.Role == model.RoleTool {
			return message, true
		}
	}
	return model.Message{}, false
}

// toolResultRounds counts the requests that carried a tool result, which is how many
// rounds the loop finished. Compared against toolCallRounds it shows a call that went
// unanswered.
func (m *metered) toolResultRounds() int {
	rounds := 0
	for _, entry := range m.attempts {
		for _, message := range entry.sent.Messages {
			if message.Role == model.RoleTool {
				rounds++
				break
			}
		}
	}
	return rounds
}

// firstToolCallRound returns the calls the first response that asked for tools made, in
// order. The count is a fact worth recording on its own: a model that asked for three tools
// under a budget of one did not fit this run regardless of what the first call said.
func (m *metered) firstToolCallRound() []model.ToolCall {
	for _, entry := range m.attempts {
		if len(entry.completion.ToolCalls) > 0 {
			return entry.completion.ToolCalls
		}
	}
	return nil
}

// firstRoundTools returns the tool definitions this run offered to the model, so a check
// can ask "undeclared against what" without keeping a second copy of the schema.
func (m *metered) firstRoundTools() []model.ToolDefinition {
	for _, entry := range m.attempts {
		if len(entry.sent.Tools) > 0 {
			return entry.sent.Tools
		}
	}
	return nil
}

// toolCallRounds counts the responses that asked to run tools.
func (m *metered) toolCallRounds() int {
	var rounds int
	for _, entry := range m.attempts {
		if len(entry.completion.ToolCalls) > 0 {
			rounds++
		}
	}
	return rounds
}

// firstFailure reports the earliest round trip that failed, so a provider-side code can
// be attributed to the step that caused it.
func (m *metered) firstFailure() (attempt, bool) {
	for _, entry := range m.attempts {
		if entry.err != nil {
			return entry, true
		}
	}
	return attempt{}, false
}

// countedTool is a mirror around the real tool. The transcript cannot answer two questions
// this record has to state: whether the tool's Execute was entered at all, and whether it
// came back with a result. A refusal before Execute (an unknown name, an envelope that is
// not an object) and a refusal from inside the tool look identical from the outside, and
// the difference is the whole diagnosis.
type countedTool struct {
	inner       agent.Tool
	invocations int
	productions int
}

// wrap is the app.Options.DecorateTool hook: it remembers the tool it was handed and
// returns itself. Production passes no decorator at all, and this one changes nothing about
// what the tool does.
func (c *countedTool) wrap(tool agent.Tool) agent.Tool {
	c.inner = tool
	return c
}

func (c *countedTool) Name() string { return c.inner.Name() }

func (c *countedTool) Description() string { return c.inner.Description() }

func (c *countedTool) Parameters() json.RawMessage { return c.inner.Parameters() }

func (c *countedTool) Execute(ctx context.Context, principal agent.Principal, arguments json.RawMessage) (agent.Observation, error) {
	c.invocations++
	observation, err := c.inner.Execute(ctx, principal, arguments)
	if err == nil {
		c.productions++
	}
	return observation, err
}

var _ agent.Tool = (*countedTool)(nil)

// recordedResponder is a mirror around the assembled assistant. The message service folds
// any strategy failure into service.ErrInternal, which is the right thing to show an HTTP
// caller and useless for a post-mortem, so the chain is kept here - in this process only,
// never serialized, never printed. What passes through is unchanged.
type recordedResponder struct {
	inner responder.Responder
	cause error
}

func (r *recordedResponder) Respond(ctx context.Context, request responder.Request) (responder.Answer, error) {
	answer, err := r.inner.Respond(ctx, request)
	r.keep(err)
	return answer, err
}

func (r *recordedResponder) Stream(ctx context.Context, request responder.Request, emit func(string) error) (responder.Answer, error) {
	answer, err := r.inner.Stream(ctx, request, emit)
	r.keep(err)
	return answer, err
}

func (r *recordedResponder) keep(err error) {
	if err != nil {
		r.cause = errors.Join(r.cause, err)
	}
}

// chain is the composed failure the strategy reported, or nil when the turn answered.
func (r *recordedResponder) chain() error { return r.cause }

var _ responder.Responder = (*recordedResponder)(nil)
