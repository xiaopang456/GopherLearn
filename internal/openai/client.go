package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/model"
	"github.com/xiaopang456/GopherLearn/internal/modelcall"
)

// errStreamIdle marks a read that was cut off because the provider went silent.
// It is internal to this package: what callers see is model.ErrTimeout.
var errStreamIdle = errors.New("openai: stream produced no data within the idle budget")

// maxResponseBytes bounds how much of a provider body this client will read. A
// runaway or malicious endpoint must not be able to make the process allocate
// without limit; a chat answer is far smaller than this.
const maxResponseBytes = 4 << 20

// Client is a model.ChatModel backed by an OpenAI-compatible /chat/completions
// endpoint.
type Client struct {
	config   Config
	endpoint string
	// completeClient bounds the whole call, including the body: a non-streaming
	// completion has no reason to stay open past the timeout.
	completeClient *http.Client
	// streamClient bounds only the wait for response headers. Once a stream is
	// open, cutting it off on a timer would truncate answers that are arriving
	// normally, so the caller's context is what governs its lifetime.
	streamClient *http.Client
}

var (
	_ model.ChatModel     = (*Client)(nil)
	_ model.ToolChatModel = (*Client)(nil)
)

// New builds a client. Configuration is validated here as well as at startup,
// because a client constructed by hand must not be able to send a key nowhere.
func New(config Config) (*Client, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	endpoint, err := config.endpoint()
	if err != nil {
		return nil, err
	}
	timeout := config.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	// Cloned so this client cannot be affected by, or contribute to, global
	// transport mutation elsewhere in the process.
	streamTransport := http.DefaultTransport.(*http.Transport).Clone()
	streamTransport.ResponseHeaderTimeout = timeout

	// The two clients keep two different contracts, so a wrapper is applied to
	// each one's own base transport instead of a single injected http.Client being
	// shared: the non-streaming call is bounded overall by Client.Timeout, while
	// the streaming call deliberately has no whole-stream deadline - only the
	// header wait above and the per-chunk idle bound inside the reader apply.
	complete := &http.Client{Timeout: timeout}
	stream := &http.Client{Transport: streamTransport}
	if config.WrapTransport != nil {
		wrap := config.WrapTransport.RoundTrip
		if wrap == nil {
			return nil, fmt.Errorf("%w: no function to wrap with", ErrInvalidWrapTransport)
		}
		if wrapped := wrap(http.DefaultTransport); wrapped == nil {
			return nil, fmt.Errorf("%w: non-streaming transport", ErrInvalidWrapTransport)
		} else {
			complete.Transport = wrapped
		}
		if wrapped := wrap(streamTransport); wrapped == nil {
			return nil, fmt.Errorf("%w: streaming transport", ErrInvalidWrapTransport)
		} else {
			stream.Transport = wrapped
		}
	}

	return &Client{
		config:         config,
		endpoint:       endpoint,
		completeClient: complete,
		streamClient:   stream,
	}, nil
}

// Model reports the configured model name. It is a label for logs and tests,
// never a credential.
func (c *Client) Model() string { return c.config.Model }

// wireMessage is the JSON shape of one chat turn.
type wireMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	// ToolCalls appears only on an assistant turn that asked for tools.
	ToolCalls []wireToolCall `json:"tool_calls,omitempty"`
	// ToolCallID ties a "tool" turn to the call it answers.
	ToolCallID string `json:"tool_call_id,omitempty"`
}

// wireToolCall is one tool request as the protocol carries it. Arguments stay a
// string here: the server-side tool parses and validates them, so an adapter must
// not reshape what the model wrote before that happens.
type wireToolCall struct {
	// Index is the slot a streamed fragment belongs to. A non-streaming response
	// carries one complete call per entry and sends no index, so this is a pointer:
	// absent means "not a fragment", and index 0 must stay distinguishable from no
	// index at all, because the first call is legitimately slot zero.
	Index    *int   `json:"index,omitempty"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// wireToolDefinition offers one server-owned capability to the model.
type wireToolDefinition struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

// toToolCalls turns the provider's structured calls into the seam type, and refuses a
// response this client could not answer correctly: a call with no name has nothing to
// run, a call with no id cannot have its result attributed, one id used twice would
// attribute one result to two requests, and a call that is not of type "function" is
// not something this client knows how to carry.
//
// There is no repair path on purpose. Synthesizing an id or defaulting a name turns a
// malformed vendor response into a legal-looking call, which is the one outcome an agent
// loop must never produce silently. A vendor that genuinely omits ids would need an
// explicit, operator-selected compatibility mode; none exists yet, because no such
// vendor has been reached, and an untested escape hatch is not a fallback - it is a
// second code path nobody exercises.
func toToolCalls(calls []wireToolCall) ([]model.ToolCall, error) {
	if len(calls) == 0 {
		return nil, nil
	}
	converted := make([]model.ToolCall, 0, len(calls))
	seen := make(map[string]bool, len(calls))
	for index, call := range calls {
		position := "call " + strconv.Itoa(index+1)
		if strings.TrimSpace(call.Type) != "function" {
			return nil, fmt.Errorf("%w: %s is not of type function", model.ErrMalformedResponse, position)
		}
		if strings.TrimSpace(call.Function.Name) == "" {
			return nil, fmt.Errorf("%w: %s names no tool", model.ErrMalformedResponse, position)
		}
		id := strings.TrimSpace(call.ID)
		if id == "" {
			return nil, fmt.Errorf("%w: %s carries no id to attribute its result to", model.ErrMalformedResponse, position)
		}
		if seen[id] {
			return nil, fmt.Errorf("%w: tool call id %q appears twice in one response", model.ErrMalformedResponse, id)
		}
		seen[id] = true
		converted = append(converted, model.ToolCall{
			ID: id,
			// The name and arguments go through unchanged, including a name this
			// server never offered: deciding whether that is valid is the harness's
			// job, and rewriting it here would hide what the model actually said.
			Name:      call.Function.Name,
			Arguments: call.Function.Arguments,
		})
	}
	return converted, nil
}

// wireRequest is the request body. EnableThinking is always sent: an operator who
// sets the flag must be able to see it take effect on the wire, and a field that
// disappears at zero makes the configuration unverifiable.
type wireRequest struct {
	Model       string        `json:"model"`
	Messages    []wireMessage `json:"messages"`
	Temperature float64       `json:"temperature,omitempty"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	// EnableThinking is a pointer so "the operator never set it" and "the operator
	// set it to false" stay different on the wire. It is a Qwen-style extension
	// rather than part of the OpenAI schema, so unset must send nothing.
	EnableThinking *bool `json:"enable_thinking,omitempty"`
	Stream         bool  `json:"stream"`
	// Tools is sent only when the caller offered definitions, so a plain chat
	// request keeps the exact byte shape it had before tools existed.
	Tools []wireToolDefinition `json:"tools,omitempty"`
	// ToolChoice is sent only when the caller stated one. It is a pointer so
	// "not stated" and "stated" stay different on the wire even before omitempty
	// is considered.
	ToolChoice *string `json:"tool_choice,omitempty"`
}

// wireUsage keeps each figure optional. A provider that omits a field, one that
// sends the object with nothing in it, and one that genuinely reports zero are
// three different facts, and collapsing them into an int would turn the first two
// into a measurement of zero tokens.
type wireUsage struct {
	PromptTokens     *int `json:"prompt_tokens"`
	CompletionTokens *int `json:"completion_tokens"`
	TotalTokens      *int `json:"total_tokens"`
	// PromptTokensDetails is kept as raw bytes instead of a typed field, and that
	// is a compatibility rule rather than laziness: the cache figure is an optional
	// observation extension, so a provider that sends it in a shape this client
	// cannot read must still get its answer through. A typed field here made
	// "cached_tokens":"64" fail the whole body as malformed - even for a request
	// with no collector attached at all.
	PromptTokensDetails json.RawMessage `json:"prompt_tokens_details,omitempty"`
}

type wireResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Role      string         `json:"role"`
			Content   string         `json:"content"`
			ToolCalls []wireToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *wireUsage `json:"usage"`
	Error *struct {
		Code string `json:"code"`
		Type string `json:"type"`
	} `json:"error"`
}

type wireChunk struct {
	Model   string `json:"model"`
	Choices []struct {
		Delta struct {
			Role    string `json:"role"`
			Content string `json:"content"`
			// Fragments of one call arrive across chunks, keyed by index: the id and
			// name on the first one, arguments appended after. StreamWithTools joins
			// them; Stream refuses a stream that asked for tools.
			ToolCalls []wireToolCall `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *wireUsage `json:"usage"`
	Error *struct {
		Code string `json:"code"`
		Type string `json:"type"`
	} `json:"error"`
}

// ErrStreamToolCalls reports a stream that asked for tools. The agent loop completes
// its turns without streaming, so this cannot be worked around by answering from the
// text beside the request: doing so would silently discard a tool the model asked to
// run, which is exactly the failure the tool-calling seam exists to avoid.
var ErrStreamToolCalls = errors.New("openai: this client does not replay tool calls from a stream")

// Complete performs one non-streaming completion with no tools offered. It is the
// path the two chat strategies use, and it stays that way: a request that carried
// tool definitions here would expect tool calls back from a method that does not
// promise them.
func (c *Client) Complete(ctx context.Context, request model.Request) (result model.Completion, err error) {
	handle := beginCall(ctx, modelcall.KindComplete)
	defer func() { sealWithErr(handle, err) }()
	if len(request.Tools) > 0 {
		return model.Completion{}, fmt.Errorf(
			"%w: tools were offered to a plain completion; use the tool-calling path", model.ErrInvalidRequest)
	}
	return c.complete(ctx, request, handle)
}

// CompleteWithTools performs one non-streaming completion while offering the
// request's server-owned tool definitions, and returns the model's structured tool
// calls. It never runs a tool: deciding is the model's job, validating and executing
// are the caller's.
func (c *Client) CompleteWithTools(ctx context.Context, request model.Request) (result model.Completion, err error) {
	handle := beginCall(ctx, modelcall.KindCompleteWithTools)
	defer func() { sealWithErr(handle, err) }()
	if len(request.Tools) == 0 {
		return model.Completion{}, fmt.Errorf("%w: a tool completion needs at least one server-owned tool", model.ErrInvalidRequest)
	}
	for _, tool := range request.Tools {
		if len(tool.Parameters) == 0 || !json.Valid(tool.Parameters) {
			return model.Completion{}, fmt.Errorf("%w: tool %q carries no valid parameter schema", model.ErrInvalidRequest, tool.Name)
		}
	}
	return c.complete(ctx, request, handle)
}

// complete is the shared body of the two non-streaming methods. It owns no record
// of its own on purpose: one exported call is one observation, and a wrapper that
// recorded again here would bill the same request twice.
func (c *Client) complete(ctx context.Context, request model.Request, handle *modelcall.Handle) (model.Completion, error) {
	payload, err := c.body(request, false)
	if err != nil {
		return model.Completion{}, err
	}
	response, err := c.send(ctx, c.completeClient, false, payload, handle)
	if err != nil {
		return model.Completion{}, err
	}
	defer response.Body.Close()
	if err := checkStatus(response); err != nil {
		return model.Completion{}, err
	}

	raw, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	if err != nil {
		return model.Completion{}, classifyTransportError(ctx, err)
	}
	var decoded wireResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		// The body is deliberately not included: it can echo the prompt.
		return model.Completion{}, fmt.Errorf("%w: completion body is not valid JSON", model.ErrMalformedResponse)
	}
	// Usage is taken before any answer rule runs. A turn that is about to be
	// rejected for saying nothing was still sent, and the provider's figures for
	// it stay in the record even though the answer does not.
	handle.NoteUsage(usageFields(decoded.Usage))
	if decoded.Error != nil && len(decoded.Choices) == 0 {
		return model.Completion{}, statusError(response.StatusCode, decoded.Error.Code, decoded.Error.Type)
	}
	if len(decoded.Choices) == 0 {
		return model.Completion{}, fmt.Errorf("%w: completion carried no choices", model.ErrMalformedResponse)
	}
	content := decoded.Choices[0].Message.Content
	// Only the provider's structured tool_calls field means the model asked to use a
	// tool. A JSON object that happens to appear in the text is answer text, and
	// treating it as a call is how an agent ends up executing a string a user typed
	// into a document.
	calls, err := toToolCalls(decoded.Choices[0].Message.ToolCalls)
	if err != nil {
		return model.Completion{}, err
	}
	if len(calls) == 0 && strings.TrimSpace(content) == "" {
		return model.Completion{}, model.ErrEmptyCompletion
	}
	return model.Completion{
		Content:   content,
		ToolCalls: calls,
		// The finish reason is reported as the provider stated it, normalized. A
		// caller that branches on tools must look at ToolCalls, not at this field:
		// providers disagree about naming "tool_calls" and some say "stop".
		Model:        fallback(decoded.Model, c.config.Model),
		FinishReason: model.NormalizeFinishReason(decoded.Choices[0].FinishReason),
		Usage:        usage(decoded.Usage),
	}, nil
}

// Stream consumes the provider's server-sent events and forwards each answer
// fragment to onDelta.
func (c *Client) Stream(ctx context.Context, request model.Request, onDelta model.DeltaHandler) (result model.Completion, err error) {
	handle := beginCall(ctx, modelcall.KindStream)
	defer func() { sealWithErr(handle, err) }()
	if onDelta == nil {
		return model.Completion{}, fmt.Errorf("%w: a stream needs a delta handler", model.ErrInvalidRequest)
	}
	if len(request.Tools) > 0 {
		// This path assembles answer text only, so offering it tools would let a
		// model ask for one and have the request vanish. The agent loop completes
		// its turns instead of streaming them.
		return model.Completion{}, fmt.Errorf("%w: tools cannot be offered to a streamed completion", model.ErrInvalidRequest)
	}
	payload, err := c.body(request, true)
	if err != nil {
		return model.Completion{}, err
	}
	// callCtx is what the request runs on, so a stalled provider can be abandoned
	// by this client without touching the caller's context: the caller must still
	// see context.Canceled for its own cancellation and ErrTimeout for ours.
	callCtx, cancelCall := context.WithCancel(ctx)
	defer cancelCall()
	response, err := c.send(callCtx, c.streamClient, true, payload, handle)
	if err != nil {
		return model.Completion{}, err
	}
	defer response.Body.Close()
	if err := checkStatus(response); err != nil {
		return model.Completion{}, err
	}
	if err := expectEventStream(response); err != nil {
		return model.Completion{}, err
	}
	idle := newIdleReader(response.Body, c.config.Timeout, cancelCall)
	defer idle.stop()
	return c.consumeStream(ctx, idle, response.StatusCode, onDelta, handle)
}

// StreamWithTools offers the request's tools and streams the turn: each answer
// fragment goes to onDelta as it arrives, and the tool calls the model asked for come
// back assembled from their fragments.
//
// Unlike Stream, a turn that says nothing and asks for a tool is a success here - that
// is the ordinary shape of a decision round. Unlike CompleteWithTools, the caller sees
// text before the call is over, which is what lets the agent loop publish an answer
// while the provider is still writing one. Nothing is executed here: deciding and
// running stay separate steps, and the executor is the caller.
func (c *Client) StreamWithTools(ctx context.Context, request model.Request,
	onDelta model.StreamDeltaHandler,
) (result model.Completion, err error) {
	handle := beginCall(ctx, modelcall.KindStreamWithTools)
	defer func() { sealWithErr(handle, err) }()
	if onDelta == nil {
		return model.Completion{}, fmt.Errorf("%w: a stream needs a delta handler", model.ErrInvalidRequest)
	}
	if len(request.Tools) == 0 {
		// The face exists for a tool round. A tool-less stream would duplicate
		// Stream's job under a different empty-content rule, and two nearly
		// identical paths are how one of them stops getting fixed.
		return model.Completion{}, fmt.Errorf("%w: a streamed tool turn needs tool definitions", model.ErrInvalidRequest)
	}
	payload, err := c.body(request, true)
	if err != nil {
		return model.Completion{}, err
	}
	// callCtx abandons a stalled provider without touching the caller's context, so
	// the caller still reads its own cancellation as context.Canceled and this
	// client's stall as ErrTimeout.
	callCtx, cancelCall := context.WithCancel(ctx)
	defer cancelCall()
	response, err := c.send(callCtx, c.streamClient, true, payload, handle)
	if err != nil {
		return model.Completion{}, err
	}
	defer response.Body.Close()
	if err := checkStatus(response); err != nil {
		return model.Completion{}, err
	}
	if err := expectEventStream(response); err != nil {
		return model.Completion{}, err
	}
	idle := newIdleReader(response.Body, c.config.Timeout, cancelCall)
	defer idle.stop()
	return c.consumeToolStream(ctx, idle, response.StatusCode, onDelta, handle)
}

// assembledCall is one tool call under construction. Arguments accumulate because the
// protocol splits them across chunks; id, type and name come from whichever fragment
// carried them, and a call that never received an id or a name is refused at the end
// rather than repaired here.
type assembledCall struct {
	index     int
	id        string
	callType  string
	name      string
	arguments strings.Builder
}

// consumeToolStream reads one streamed turn into answer text plus tool calls.
//
// The content fragments are handed to onDelta unchanged. That is the point of this
// method: whether text beside a tool request is part of the answer is a question about
// the turn, and the turn belongs to the caller. A client that decided it here would
// either withhold the final answer or publish a model's drafting, and it would do so
// without ever telling the caller which it chose.
// consumeToolStream writes into the handle its caller opened; the exported call,
// not the read loop, is the unit being counted.
func (c *Client) consumeToolStream(ctx context.Context, body io.Reader, status int,
	onDelta model.StreamDeltaHandler, handle *modelcall.Handle,
) (model.Completion, error) {
	var (
		builder   strings.Builder
		finish    model.FinishReason
		completed = model.Completion{Model: c.config.Model}
		sawDone   bool
		calls     []*assembledCall
		byIndex   = map[int]*assembledCall{}
	)
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 16*1024), maxResponseBytes)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return model.Completion{}, err
		}
		line := strings.TrimRight(scanner.Text(), "\r")
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		field, value, ok := strings.Cut(line, ":")
		if !ok || field != "data" {
			continue
		}
		value = strings.TrimPrefix(value, " ")
		if value == "[DONE]" {
			sawDone = true
			break
		}
		var chunk wireChunk
		if err := json.Unmarshal([]byte(value), &chunk); err != nil {
			return model.Completion{}, fmt.Errorf("%w: stream data is not valid JSON", model.ErrMalformedResponse)
		}
		if chunk.Error != nil {
			return model.Completion{}, statusError(status, chunk.Error.Code, chunk.Error.Type)
		}
		if chunk.Model != "" {
			completed.Model = chunk.Model
		}
		if chunk.Usage != nil {
			// Providers send a cumulative snapshot, so the newest one replaces
			// the previous figure rather than adding to it. UsageFrames says how
			// often it repeated; nothing sums the repeats.
			completed.Usage = usage(chunk.Usage)
			handle.NoteUsage(usageFields(chunk.Usage))
		}
		for _, choice := range chunk.Choices {
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				finish = model.NormalizeFinishReason(*choice.FinishReason)
			}
			for _, fragment := range choice.Delta.ToolCalls {
				slot, exists := c.slotFor(fragment, calls, byIndex)
				target, known := byIndex[slot]
				if !known || !exists {
					target = &assembledCall{index: slot}
					byIndex[slot] = target
					calls = append(calls, target)
					// The turn has become a tool round, and the caller is told at this
					// moment rather than at the end: whoever is publishing answer text
					// needs the chance to stop, not a post-mortem.
					if err := onDelta(model.StreamDelta{ToolCall: true}); err != nil {
						sealCallback(handle, err)
						return model.Completion{}, err
					}
				}
				if fragment.ID != "" {
					target.id = fragment.ID
				}
				if fragment.Type != "" {
					target.callType = fragment.Type
				}
				if fragment.Function.Name != "" {
					target.name += fragment.Function.Name
				}
				target.arguments.WriteString(fragment.Function.Arguments)
			}
			if choice.Delta.Content == "" {
				continue
			}
			if err := onDelta(model.StreamDelta{Content: choice.Delta.Content}); err != nil {
				sealCallback(handle, err)
				return model.Completion{}, err
			}
			builder.WriteString(choice.Delta.Content)
		}
	}
	if err := scanner.Err(); err != nil {
		return model.Completion{}, classifyReadError(ctx, err)
	}
	// Checked before anything is assembled into a result: a stream that stopped
	// early can hold a half-written tool call as easily as a half-written sentence,
	// and executing truncated arguments would run something the model never asked
	// for.
	if err := streamFinished(finish, sawDone); err != nil {
		return model.Completion{}, err
	}
	wire := make([]wireToolCall, 0, len(calls))
	for _, call := range calls {
		callType := call.callType
		if callType == "" {
			// "function" is the only type this protocol defines, and some vendors
			// leave it out of streamed fragments entirely. Filling it in costs
			// nothing a reader could have checked differently; rejecting a
			// complete, well-formed call for one absent redundant field would be
			// this client inventing a failure.
			callType = "function"
		}
		entry := wireToolCall{ID: call.id, Type: callType}
		entry.Function.Name = call.name
		entry.Function.Arguments = call.arguments.String()
		wire = append(wire, entry)
	}
	toolCalls, err := toToolCalls(wire)
	if err != nil {
		return model.Completion{}, err
	}
	completed.ToolCalls = toolCalls
	completed.Content = builder.String()
	completed.FinishReason = finish
	if completed.FinishReason == "" {
		if sawDone {
			completed.FinishReason = model.FinishStop
		} else {
			completed.FinishReason = model.FinishOther
		}
	}
	if strings.TrimSpace(completed.Content) == "" && len(toolCalls) == 0 {
		// Empty text is only acceptable when the model asked for something. With
		// neither text nor a request the turn produced nothing, and the caller has
		// to see that instead of explaining a blank answer.
		return model.Completion{}, model.ErrEmptyCompletion
	}
	return completed, nil
}

// slotFor decides which call a fragment belongs to. An explicit index always wins;
// a fragment that names no new call and carries no index continues the one already
// being built; anything else opens a fresh slot.
func (c *Client) slotFor(fragment wireToolCall, calls []*assembledCall, byIndex map[int]*assembledCall) (int, bool) {
	if fragment.Index != nil {
		slot := *fragment.Index
		_, known := byIndex[slot]
		return slot, known
	}
	if fragment.ID == "" && fragment.Function.Name == "" && len(calls) > 0 {
		// A bare continuation with no index. This is only unambiguous when one call
		// is being streamed at a time, which is what vendors that omit index do;
		// interleaving two calls without an index is not something this protocol
		// describes, and a call that ends up malformed is refused downstream
		// rather than guessed at here.
		return calls[len(calls)-1].index, true
	}
	return len(calls), false
}

// idleReader turns the configured timeout into a budget between reads rather than
// a budget for the whole call. Each read arms a timer that abandons the request if
// the provider has stayed silent for too long, and every returned read re-arms it,
// so a stream that keeps producing text can run as long as it needs to while a
// stream that stops producing anything cannot hold a request forever.
type idleReader struct {
	source   io.Reader
	timeout  time.Duration
	trip     func()
	mu       sync.Mutex
	timer    *time.Timer
	timedOut bool
}

func newIdleReader(source io.Reader, timeout time.Duration, trip func()) *idleReader {
	return &idleReader{source: source, timeout: timeout, trip: trip}
}

func (r *idleReader) Read(buffer []byte) (int, error) {
	if r.timeout <= 0 {
		return r.source.Read(buffer)
	}
	r.arm()
	count, err := r.source.Read(buffer)
	r.disarm()
	if err != nil && r.TimedOut() {
		return count, errStreamIdle
	}
	return count, err
}

// TimedOut reports whether the idle budget expired.
func (r *idleReader) TimedOut() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.timedOut
}

func (r *idleReader) arm() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.timer = time.AfterFunc(r.timeout, func() {
		r.mu.Lock()
		r.timedOut = true
		trip := r.trip
		r.mu.Unlock()
		// Cancelling the request is what unblocks the parked read; closing the
		// body happens on the way out of Stream either way.
		trip()
	})
}

func (r *idleReader) disarm() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.timer != nil && !r.timer.Stop() {
		// Stop reports false when the callback already ran or is running, so the
		// timeout stands.
		r.timedOut = true
	}
	r.timer = nil
}

// stop ends the idle watch, so no timer outlives the call. It is safe to call
// more than once.
func (r *idleReader) stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.timer != nil {
		if !r.timer.Stop() {
			// False means the callback already ran or is running, so the stall was
			// real and the error classification must say timeout.
			r.timedOut = true
		}
		r.timer = nil
	}
}

// consumeStream reads events from a body. It takes the reader rather than the
// response so the parser can be exercised on input that arrives one byte at a
// time, which is how a multi-byte character split across network reads is tested
// without depending on how a socket happens to be buffered.
func (c *Client) consumeStream(ctx context.Context, body io.Reader, status int, onDelta model.DeltaHandler, handle *modelcall.Handle) (model.Completion, error) {
	var (
		builder   strings.Builder
		finish    model.FinishReason
		completed = model.Completion{Model: c.config.Model}
		sawDone   bool
	)
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 16*1024), maxResponseBytes)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return model.Completion{}, err
		}
		line := strings.TrimRight(scanner.Text(), "\r")
		if line == "" || strings.HasPrefix(line, ":") {
			// A blank line terminates an event and a leading colon is a
			// keep-alive comment. Neither carries answer text.
			continue
		}
		field, value, ok := strings.Cut(line, ":")
		if !ok || field != "data" {
			// id:, event:, retry: and any future field are not part of the answer
			// contract this client depends on.
			continue
		}
		value = strings.TrimPrefix(value, " ")
		if value == "[DONE]" {
			// The protocol's completion marker, never answer text.
			sawDone = true
			break
		}
		var chunk wireChunk
		if err := json.Unmarshal([]byte(value), &chunk); err != nil {
			return model.Completion{}, fmt.Errorf("%w: stream data is not valid JSON", model.ErrMalformedResponse)
		}
		if chunk.Error != nil {
			return model.Completion{}, statusError(status, chunk.Error.Code, chunk.Error.Type)
		}
		if chunk.Model != "" {
			completed.Model = chunk.Model
		}
		if chunk.Usage != nil {
			// Providers send a cumulative snapshot, so the newest one replaces
			// the previous figure rather than adding to it. UsageFrames says how
			// often it repeated; nothing sums the repeats.
			completed.Usage = usage(chunk.Usage)
			handle.NoteUsage(usageFields(chunk.Usage))
		}
		for _, choice := range chunk.Choices {
			if len(choice.Delta.ToolCalls) > 0 {
				// Fail rather than answer from the text beside the request. A stream
				// that quietly swallowed a tool call would let the model report an
				// outcome it never received.
				return model.Completion{}, fmt.Errorf("%w: a streamed delta asked for tools", ErrStreamToolCalls)
			}
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				finish = model.NormalizeFinishReason(*choice.FinishReason)
			}
			if choice.Delta.Content == "" {
				continue
			}
			if err := onDelta(choice.Delta.Content); err != nil {
				sealCallback(handle, err)
				return model.Completion{}, err
			}
			builder.WriteString(choice.Delta.Content)
		}
	}
	if err := scanner.Err(); err != nil {
		return model.Completion{}, classifyReadError(ctx, err)
	}
	// A completed read loop clears the idle budget: whatever comes next is the
	// caller's business, not a stall.
	_ = ctx
	content := builder.String()
	if err := streamFinished(finish, sawDone); err != nil {
		return model.Completion{}, err
	}
	if strings.TrimSpace(content) == "" {
		return model.Completion{}, model.ErrEmptyCompletion
	}
	completed.Content = content
	completed.FinishReason = finish
	if completed.FinishReason == "" {
		// The read loop ended with the protocol's completion marker and no reason
		// named. Some vendors do exactly that, and their turn is a finished turn:
		// reported as stopped rather than as a reason the provider never gave.
		completed.FinishReason = model.FinishStop
	}
	return completed, nil
}

// body renders the request payload. Messages are forwarded in the order the
// strategy assembled them; nothing here rewrites or re-sorts the conversation.
//
// Generation knobs resolve in three steps, highest priority first:
//
//  1. the value this request carries;
//  2. the client's configured default (from the environment);
//  3. zero, which omits the field and leaves the provider's own default in force.
//
// A strategy that sets nothing therefore inherits the operator's configuration
// instead of silently dropping it - a knob that is parsed, logged and never sent
// is worse than no knob at all.
func (c *Client) body(request model.Request, stream bool) ([]byte, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	if len(request.Messages) == 0 {
		return nil, fmt.Errorf("%w: no messages", model.ErrInvalidRequest)
	}
	maxTokens := request.MaxOutputTokens
	if maxTokens <= 0 {
		maxTokens = c.config.MaxOutputTokens
	}
	temperature := request.Temperature
	if temperature <= 0 {
		temperature = c.config.Temperature
	}
	payload := wireRequest{
		Model:          c.config.Model,
		Messages:       make([]wireMessage, 0, len(request.Messages)),
		Temperature:    temperature,
		MaxTokens:      maxTokens,
		EnableThinking: c.config.EnableThinking,
		Stream:         stream,
	}
	// Tool choice is sent only when the caller stated it, so every request that
	// says nothing keeps the exact byte shape it had. "none" is the one value
	// this system sends: the loop uses it to close out a turn whose tool budget
	// is spent, and it must reach the wire as stated rather than be absorbed -
	// a call the server forbade but never told the provider about is one the
	// provider may still take.
	switch request.ToolChoice {
	case "":
	case model.ToolChoiceNone:
		if len(request.Tools) == 0 {
			return nil, fmt.Errorf("%w: tool_choice %q with no tools offered", model.ErrInvalidRequest, request.ToolChoice)
		}
		none := model.ToolChoiceNone
		payload.ToolChoice = &none
	default:
		return nil, fmt.Errorf("%w: unsupported tool choice %q", model.ErrInvalidRequest, request.ToolChoice)
	}
	for _, message := range request.Messages {
		rendered := wireMessage{Role: message.Role, Content: message.Content, ToolCallID: message.ToolCallID}
		for _, call := range message.ToolCalls {
			// The name and arguments the model produced go out unchanged. Validating
			// them is the server-side tool's job, and rewriting them here would mean
			// the transcript the model reads back is not what it asked for.
			toolCall := wireToolCall{ID: call.ID, Type: "function"}
			toolCall.Function.Name = call.Name
			toolCall.Function.Arguments = call.Arguments
			rendered.ToolCalls = append(rendered.ToolCalls, toolCall)
		}
		payload.Messages = append(payload.Messages, rendered)
	}
	for _, tool := range request.Tools {
		definition := wireToolDefinition{Type: "function"}
		definition.Function.Name = tool.Name
		definition.Function.Description = tool.Description
		definition.Function.Parameters = tool.Parameters
		payload.Tools = append(payload.Tools, definition)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("%w: cannot encode request: %v", model.ErrInvalidRequest, err)
	}
	return encoded, nil
}

// send issues the HTTP request. The API key is attached to the header only, and
// the caller's context governs cancellation of the in-flight call.
//
// A dispatch is recorded at the moment the request is handed to the transport,
// before its outcome is known: "a request left this process" is the boundary that
// matters when deciding whether a call could have been billed. A context already
// cancelled at entry, or a request that could not even be built, dispatches
// nothing and the record says so.
func (c *Client) send(ctx context.Context, client *http.Client, stream bool, payload []byte, handle *modelcall.Handle) (*http.Response, error) {
	if ctx == nil {
		return nil, errors.New("openai: context is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		// url.Error text carries the endpoint, never the key, but the error is
		// still normalized so callers see one taxonomy.
		return nil, classifyTransportError(ctx, err)
	}
	request.Header.Set("Authorization", "Bearer "+c.config.APIKey)
	request.Header.Set("Content-Type", "application/json")
	if stream {
		request.Header.Set("Accept", "text/event-stream")
	} else {
		request.Header.Set("Accept", "application/json")
	}
	request.ContentLength = int64(len(payload))

	handle.Dispatch()
	response, err := client.Do(request)
	if err != nil {
		return nil, classifyTransportError(ctx, err)
	}
	return response, nil
}

// checkStatus converts a non-2xx response into a stable error. The response body
// is read only far enough to pull a machine-readable code out of it, because a
// provider error can otherwise echo the prompt back.
func checkStatus(response *http.Response) error {
	if response.StatusCode >= 200 && response.StatusCode <= 299 {
		return nil
	}
	code, kind := probeErrorBody(response)
	return statusError(response.StatusCode, code, kind)
}

func statusError(status int, code, kind string) error {
	return &StatusError{Status: status, Code: code, Type: kind}
}

// StatusError is a non-2xx provider response, or a stream that carried an error
// frame. Only the numeric status and the two machine-readable identifiers are
// kept: an error message authored by a provider can echo the prompt back, so it
// must never become part of Go's error text either.
type StatusError struct {
	Status int
	Code   string
	Type   string
}

func (e *StatusError) Error() string {
	detail := "status " + strconv.Itoa(e.Status)
	if e.Type != "" {
		detail += " type " + e.Type
	}
	if e.Code != "" {
		detail += " code " + e.Code
	}
	return model.ErrProviderStatus.Error() + ": " + detail
}

func (e *StatusError) Unwrap() error { return model.ErrProviderStatus }

// ProviderStatusOf reports the HTTP status behind an ErrProviderStatus failure.
// A caller that must record why a request failed can store this number and
// nothing else from the response.
func ProviderStatusOf(err error) (int, bool) {
	var status *StatusError
	if !errors.As(err, &status) {
		return 0, false
	}
	return status.Status, true
}

func probeErrorBody(response *http.Response) (string, string) {
	if response.Body == nil {
		return "", ""
	}
	limited := io.LimitReader(response.Body, 8*1024)
	raw, err := io.ReadAll(limited)
	if err != nil || len(raw) == 0 {
		return "", ""
	}
	var decoded wireResponse
	if err := json.Unmarshal(raw, &decoded); err != nil || decoded.Error == nil {
		return "", ""
	}
	return decoded.Error.Code, decoded.Error.Type
}

func expectEventStream(response *http.Response) error {
	if response.Body == nil {
		return fmt.Errorf("%w: empty stream response", model.ErrMalformedResponse)
	}
	mediaType := response.Header.Get("Content-Type")
	base, _, _ := strings.Cut(mediaType, ";")
	if !strings.EqualFold(strings.TrimSpace(base), "text/event-stream") {
		return fmt.Errorf("%w: streaming response is %q, not text/event-stream", model.ErrMalformedResponse, strings.TrimSpace(mediaType))
	}
	return nil
}

// classifyTransportError maps a transport failure onto this package's error
// taxonomy while keeping caller cancellation distinguishable from a timeout: a
// caller hanging up is not an outage, and an outage is not a cancelled client.
func classifyTransportError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("%w: %v", model.ErrUnavailable, err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		if ctxErr := ctx.Err(); errors.Is(ctxErr, context.DeadlineExceeded) {
			return ctxErr
		}
		return fmt.Errorf("%w: %v", model.ErrTimeout, err)
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return fmt.Errorf("%w: %v", model.ErrTimeout, err)
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return fmt.Errorf("%w: %v", model.ErrUnavailable, urlErr.Op)
	}
	return fmt.Errorf("%w: %v", model.ErrUnavailable, err)
}

// streamFinished decides whether a stream that stopped delivering lines was ever
// told to stop.
//
// A provider ends a turn in one of two honest ways: it names a finish reason, or it
// sends the protocol's `[DONE]` marker. Anything else - a socket that simply closed,
// an idle read that ended the body, a turn the provider cut at its own output limit -
// means the text received so far is a prefix. A prefix cannot be stored as an answer,
// cited from, or confirmed to a learner who watched it arrive, so this is a failure and
// not a finish reason to report. Deciding it here, in the adapter that reads the wire,
// is what makes both stream paths honest: neither can hand a caller a half answer
// dressed as a whole one.
func streamFinished(finish model.FinishReason, sawDone bool) error {
	if finish == model.FinishLength {
		return fmt.Errorf("%w: the provider stopped at its output limit", model.ErrTruncatedStream)
	}
	if finish == "" && !sawDone {
		return fmt.Errorf("%w: the stream closed without a finish reason or the completion marker",
			model.ErrTruncatedStream)
	}
	return nil
}

// classifyReadError handles a stream that broke mid-body. Caller cancellation
// still wins over a read failure, because cancelling the request typically makes
// the body read error too.
func classifyReadError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return fmt.Errorf("%w: %v", model.ErrTimeout, err)
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("%w: stream ended mid-event", model.ErrMalformedResponse)
	}
	if errors.Is(err, errStreamIdle) {
		return fmt.Errorf("%w: %v", model.ErrTimeout, err)
	}
	return fmt.Errorf("%w: %v", model.ErrUnavailable, err)
}

// usage projects a provider's figures onto the domain type. It keeps the shape
// every existing consumer already reads: an absent figure stays zero, because
// model.Usage has no way to say otherwise. Callers that need to tell "absent"
// from "reported zero" read the observation record instead (usageFields).
func usage(payload *wireUsage) model.Usage {
	if payload == nil {
		return model.Usage{}
	}
	return model.Usage{
		PromptTokens:     derefInt(payload.PromptTokens),
		CompletionTokens: derefInt(payload.CompletionTokens),
		TotalTokens:      derefInt(payload.TotalTokens),
	}
}

// usageFields is the same payload read the honest way: each figure carries
// whether the provider sent it. Nothing is inferred here - no total computed from
// prompt plus completion, no cache figure merged into a total.
func usageFields(payload *wireUsage) modelcall.Usage {
	if payload == nil {
		return modelcall.Usage{}
	}
	reported := modelcall.Usage{
		Prompt:     fieldOf(payload.PromptTokens),
		Completion: fieldOf(payload.CompletionTokens),
		Total:      fieldOf(payload.TotalTokens),
		Cached:     cachedTokensOf(payload.PromptTokensDetails),
	}
	return reported
}

// cachedTokensOf pulls the one readable figure out of the optional details
// object. Anything it cannot interpret - a string, a boolean, a fraction, an
// array, a bare number, or a null - becomes "not reported". Coercing the text
// "64" into the number 64 would hand a report a cache measurement the provider
// never actually delivered in a readable form, so the honest answer is unknown.
func cachedTokensOf(raw []byte) modelcall.Field {
	if len(raw) == 0 {
		return modelcall.Field{}
	}
	var details struct {
		CachedTokens *int `json:"cached_tokens"`
	}
	// A null decodes cleanly into the zero struct, and a nil pointer is already
	// "not reported", so no special case is needed for it.
	if err := json.Unmarshal(raw, &details); err != nil {
		return modelcall.Field{}
	}
	return fieldOf(details.CachedTokens)
}

func fieldOf(value *int) modelcall.Field {
	if value == nil {
		return modelcall.Field{}
	}
	return modelcall.Field{Value: int64(*value), Reported: true}
}

func derefInt(value *int) int {
	if value == nil {
		return 0
	}
	return *value
}

// beginCall opens the observation record for one exported model-method call and
// returns nil when no collector was attached to ctx. A nil handle is accepted by
// every modelcall method, so an unobserved request runs the identical code path.
func beginCall(ctx context.Context, kind modelcall.Kind) *modelcall.Handle {
	collector, ok := modelcall.FromContext(ctx)
	if !ok {
		return nil
	}
	return collector.Begin(ctx, kind)
}

// sealCallback labels a call whose delta handler returned an error. The error the
// caller receives is untouched; only the record is decided here, and it is not
// decided as a consumer refusal when the consumer's failure was really the caller
// walking away. A browser that hung up mid-stream is a cancellation, and labelling
// it callback_failed would hide the one cause an operator most wants to count.
func sealCallback(handle *modelcall.Handle, err error) {
	if handle == nil {
		return
	}
	switch {
	case errors.Is(err, context.Canceled):
		handle.Seal(modelcall.OutcomeCancelled)
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, model.ErrTimeout):
		handle.Seal(modelcall.OutcomeTimeout)
	default:
		handle.Seal(modelcall.OutcomeCallbackFailed)
	}
}

// sealWithErr closes a record exactly once, classifying from the error the
// caller is already about to return. Sealing twice is a no-op, so a site that
// already named its own outcome - a rejected delta handler, for example - keeps
// it. The returned error is untouched either way: observation labels a call, it
// never rewrites what the caller sees.
func sealWithErr(handle *modelcall.Handle, err error) {
	if handle == nil {
		return
	}
	if err != nil {
		handle.Fail(err)
		return
	}
	handle.Complete()
}

func fallback(value, alternate string) string {
	if strings.TrimSpace(value) != "" {
		return value
	}
	return alternate
}
