// Package model defines the conversation-model boundary. It is deliberately
// free of any provider SDK type: services, responders and the HTTP layer speak
// roles, messages and token counts, and only the adapter that dials a provider
// knows what a wire format looks like. Swapping providers, adding a different
// model, or running with no model at all must not ripple past this package.
package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Roles accepted in a Request. The set is closed on purpose: an adapter that
// silently forwards a role the provider does not know produces an answer no
// local test can predict.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	// RoleTool carries one tool's result back to the model. It exists only for the
	// agent loop; the two chat strategies never produce it.
	RoleTool = "tool"
)

var (
	// ErrNotConfigured reports a model that cannot be built or reached with the
	// configuration it was given. It is a startup or wiring fault, never a bad
	// user request.
	ErrNotConfigured = errors.New("model: not configured")
	// ErrUnavailable reports a transport-level failure: the provider could not
	// be reached at all.
	ErrUnavailable = errors.New("model: provider unavailable")
	// ErrProviderStatus reports a response that is not 2xx. The status code is
	// part of the message; the response body never is.
	ErrProviderStatus = errors.New("model: provider returned an error status")
	// ErrMalformedResponse reports a body or event stream the adapter cannot
	// read: invalid JSON, a non-SSE content type for a streaming call, or events
	// whose shape does not match the documented protocol.
	ErrMalformedResponse = errors.New("model: provider response is malformed")
	// ErrEmptyCompletion reports a provider that answered successfully with no
	// text. Treating it as a successful empty answer would store an empty
	// assistant message, so it is a failure instead.
	ErrEmptyCompletion = errors.New("model: provider returned an empty answer")
	// ErrTruncatedStream reports a stream that stopped before the provider said it
	// was finished, or that the provider stopped because it hit its own output
	// limit. Either way the text it delivered is a prefix, and a prefix is not an
	// answer: it cannot be stored, cited from, or confirmed to a learner who
	// already watched part of it arrive.
	ErrTruncatedStream = errors.New("model: provider stream ended before the answer was complete")
	// ErrTimeout reports a provider that did not answer within the configured
	// budget. It is distinguished from cancellation because an operator set the
	// first deadline and a caller triggered the second.
	ErrTimeout = errors.New("model: provider request timed out")
	// ErrInvalidRequest reports a Request this adapter cannot send, such as a
	// message list with no user turn. It is a programming fault in a strategy.
	ErrInvalidRequest = errors.New("model: request cannot be sent")
)

// ToolDefinition is one capability the server is willing to run. The registry, not
// the model, decides what exists: a model can ask for a tool by any name it likes,
// and an unknown name is a failure rather than something to guess at.
type ToolDefinition struct {
	Name        string
	Description string
	// Parameters is a JSON Schema document this package wrote for the provider. A
	// model never supplies it, and the server re-validates the arguments anyway -
	// a schema the provider enforces loosely is not an authorization boundary.
	Parameters json.RawMessage
}

// ToolCall is a model's request to run one tool. Arguments carries the raw JSON text
// the model produced and is only ever interpreted by the server-side tool, which
// rejects anything that does not match its own schema.
type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// Message is one turn of the conversation as the model will see it.
type Message struct {
	Role    string
	Content string
	// ToolCalls belongs to an assistant message that asked for tools.
	ToolCalls []ToolCall
	// ToolCallID belongs to a RoleTool message and names the call it answers.
	ToolCallID string
}

// ToolChoice tells the provider whether it may ask for tools on this call.
// It is a closed set rather than a free-form string: the two values this
// system sends are the two an agent loop needs, and anything else a provider
// supports is negotiated nowhere.
const (
	// ToolChoiceUnset leaves the decision to the provider. It is the zero
	// value, so every existing request keeps its behaviour.
	ToolChoiceUnset = ""
	// ToolChoiceNone forbids tool calls on this request: the model must answer
	// from what it already has. The loop sends it when the tool budget is
	// spent - MaxToolCalls bounds executions, not the ability to synthesize a
	// final answer from the evidence already collected. A provider that
	// violates it is still blocked by the loop's own hard budget check.
	ToolChoiceNone = "none"
)

// Request is one completion call. Generation knobs live here rather than on the
// adapter so a caller can see exactly what a turn asked for.
type Request struct {
	// Messages is the full prompt in the order it should be sent: at most one
	// leading system message, prior turns, then the current user turn.
	Messages []Message
	// Tools are the server-owned definitions offered to this call. An adapter that
	// cannot do tool calling must fail loudly rather than drop them, because the
	// agent loop would otherwise read the model's plain prose as a tool outcome.
	Tools []ToolDefinition
	// Temperature is sent only when greater than zero. Zero means "do not
	// override the provider default", not "deterministic", because providers
	// disagree about what 0 means.
	Temperature float64
	// MaxOutputTokens bounds the reply length. Zero defers to the provider.
	MaxOutputTokens int
	// ToolChoice states whether this call may ask for tools. It is sent only
	// when set; an adapter that cannot express it on the wire must fail the
	// request rather than silently allow calls the caller forbade.
	ToolChoice string
}

// FinishReason reports why the model stopped. Values are normalized: a provider
// that names its own reasons still maps onto this closed set, so callers can
// distinguish "the answer ended" from "the answer was cut off".
type FinishReason string

const (
	// FinishStop means the model finished its answer.
	FinishStop FinishReason = "stop"
	// FinishLength means the answer hit the output-token budget and is truncated.
	FinishLength FinishReason = "length"
	// FinishContentFilter means the provider withheld content.
	FinishContentFilter FinishReason = "content_filter"
	// FinishToolCalls means the model stopped in order to ask for tools, so its
	// text - if any - is not the answer for this turn.
	FinishToolCalls FinishReason = "tool_calls"
	// FinishOther covers a reason this package does not model.
	FinishOther FinishReason = "other"
)

// NormalizeFinishReason maps a provider's spelling onto the closed set.
func NormalizeFinishReason(value string) FinishReason {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "":
		return FinishOther
	case "stop", "end_turn", "finish":
		return FinishStop
	case "length", "max_tokens", "token_limit":
		return FinishLength
	case "content_filter", "sensitive", "recycle":
		return FinishContentFilter
	case "tool_calls", "tool_use", "function_call":
		return FinishToolCalls
	default:
		return FinishOther
	}
}

// Usage counts tokens for one call. Providers are not required to report it, so
// a zero Usage is normal and must never be treated as an error.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
}

// Completion is a model's answer.
type Completion struct {
	Content string
	// ToolCalls is non-empty when the model asked to use tools instead of
	// answering. It comes from the provider's structured tool-call field, never
	// from text that happens to look like JSON: a model that writes a JSON object
	// in its prose has asked for nothing.
	ToolCalls []ToolCall
	// Model names the model that answered, taken from the response when the
	// provider reports it. It is a diagnostic label, never a secret.
	Model        string
	FinishReason FinishReason
	Usage        Usage
}

// DeltaHandler receives one streamed piece of answer text. Returning an error
// aborts the call, which is how a disconnected client stops paying for tokens.
type DeltaHandler func(string) error

// ChatModel completes and streams chat requests. Both paths must exist on one
// interface so a strategy can pick its transport-per-need without knowing which
// provider sits behind it.
//
// It deliberately knows nothing about tools: an agent loop is an extra capability,
// and neither chat strategy has to reason about a model that cannot offer one.
type ChatModel interface {
	Complete(ctx context.Context, request Request) (Completion, error)
	// Stream calls onDelta for each piece of answer text and returns the
	// assembled completion. Implementations must return ErrEmptyCompletion when
	// the stream produced no text, and must not emit the protocol's completion
	// marker as answer text.
	Stream(ctx context.Context, request Request, onDelta DeltaHandler) (Completion, error)
}

// ErrToolsUnsupported reports a request that offered tools to a model wrapper that
// cannot carry them. It exists so an agent turn fails loudly instead of receiving a
// plain completion in which no tool was ever offered.
var ErrToolsUnsupported = errors.New("model: this chat model cannot call tools")

// ToolChatModel is a ChatModel that can offer server-owned tool definitions and
// report the model's structured tool calls. A caller asserts for it rather than
// adding a method to ChatModel, so every existing implementation - test doubles,
// the metered smoke wrapper, a future provider without tool support - keeps
// compiling and is refused by name when it is wired into an agent loop.
type ToolChatModel interface {
	ChatModel
	// CompleteWithTools sends request.Tools and returns whatever tool calls the
	// model asked for. It must not invoke them: deciding and executing are
	// separate steps, and the executor is the caller.
	CompleteWithTools(ctx context.Context, request Request) (Completion, error)
}

// StreamDelta is one fragment of a streamed assistant turn, as the transport saw it.
//
// Content and ToolCall are never both set: they describe two different things a provider
// can be doing mid-turn, and the caller has to tell them apart to decide what a client may
// see. Without the second one, a caller that must not publish a tool round's narration learns
// that the round was a tool round only after the stream ended - by which time the text has
// already reached the browser.
type StreamDelta struct {
	// Content is text the provider produced, in arrival order.
	Content string
	// ToolCall marks the arrival of a NEW tool request in this same stream. The adapter
	// assembles the call and returns it; this is only the signal that the turn has become
	// a tool round.
	ToolCall bool
}

// StreamDeltaHandler receives one fragment. Returning an error abandons the stream, which is
// how a client that hung up stops a provider mid-generation.
type StreamDeltaHandler func(StreamDelta) error

// ToolStreamChatModel is implemented by a tool-capable model that can also stream a
// turn while it decides. It is a separate optional face rather than a method on
// ToolChatModel for the same reason ToolChatModel is separate from ChatModel: doubles,
// the metered smoke wrapper and any provider without streaming tool support keep
// compiling and are refused by name, while an agent loop that has this face can publish
// answer text as the provider produces it instead of after.
//
// The contract of onDelta here is narrower than Stream's, and the difference matters:
// this method reports what the provider sent, in arrival order, and leaves it to the
// caller to decide which of that text is an answer. A call that ends with tool calls may
// have streamed content beside them, and a caller that published all of it would show a
// model's drafting as the assistant's final word.
type ToolStreamChatModel interface {
	ToolChatModel
	// StreamWithTools offers request.Tools, forwards each content fragment to onDelta
	// as it arrives, and returns the assembled completion - answer text and any tool
	// calls whose fragments were joined by index. It must not execute a tool.
	//
	// It must not report ErrEmptyCompletion for a turn that asked for tools and said
	// nothing else: an empty content beside a tool call is the normal shape of that
	// turn, not a failed generation.
	StreamWithTools(ctx context.Context, request Request, onDelta StreamDeltaHandler) (Completion, error)
}

// Validate checks the invariants every adapter depends on, so a strategy cannot
// send a prompt with nothing in it.
func (r Request) Validate() error {
	var userTurns int
	for index, message := range r.Messages {
		switch strings.TrimSpace(message.Role) {
		case RoleSystem:
			if index != 0 {
				return fmt.Errorf("%w: the system message must come first", ErrInvalidRequest)
			}
		case RoleUser, RoleAssistant:
		case RoleTool:
			// A tool result without the call it answers is unattributable, and a
			// provider would either reject the prompt or misread it as prose.
			if strings.TrimSpace(message.ToolCallID) == "" {
				return fmt.Errorf("%w: a tool result needs the tool call id it answers", ErrInvalidRequest)
			}
		default:
			return ErrInvalidRequest
		}
		if message.Role == RoleUser {
			userTurns++
		}
	}
	if userTurns == 0 {
		return ErrInvalidRequest
	}
	if r.Temperature < 0 {
		return ErrInvalidRequest
	}
	if r.MaxOutputTokens < 0 {
		return ErrInvalidRequest
	}
	for _, tool := range r.Tools {
		if strings.TrimSpace(tool.Name) == "" {
			return fmt.Errorf("%w: a tool definition needs a name", ErrInvalidRequest)
		}
	}
	return nil
}

// LastUserContent returns the text of the final user turn, which is the question
// this request asks about. It reports false when there is none.
func (r Request) LastUserContent() (string, bool) {
	for index := len(r.Messages) - 1; index >= 0; index-- {
		if r.Messages[index].Role == RoleUser {
			return r.Messages[index].Content, true
		}
	}
	return "", false
}
