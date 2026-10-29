package agent

import (
	"context"
	"errors"
	"github.com/cloudwego/eino/schema"
	"io"
)

// PublicText is an observation of Content at the existing model boundary. It
// cannot represent reasoning, tool arguments, prompts, or tool results.
type PublicText struct {
	RunID       string
	Round       int
	Phase       string
	Content     string
	Kind        string
	Publication string // model_stream or generate_snapshot; never an inferred token count
}
type PublicTextObserver func(context.Context, PublicText) error
type publicTextKey struct{}

// ValidateRunRequestForAssembly exposes the original validation for transports
// that must reject malformed requests before committing streaming headers.
func ValidateRunRequestForAssembly(request RunRequest) error { return validateRunRequest(request) }

// WithPublicTextObserver explicitly opts this request into public observations.
// Native V2 and non-streaming callers do not set it and retain the original path.
func WithPublicTextObserver(ctx context.Context, observer PublicTextObserver) context.Context {
	if observer == nil {
		return ctx
	}
	return context.WithValue(ctx, publicTextKey{}, observer)
}

type publicModelCall struct {
	observer    PublicTextObserver
	runID       string
	round       int
	publication string
	opened      bool
	tools       bool
}

func newPublicModelCall(ctx context.Context, state *runState, publication string) *publicModelCall {
	observer, _ := ctx.Value(publicTextKey{}).(PublicTextObserver)
	if observer == nil {
		return nil
	}
	state.mu.Lock()
	round := state.modelCalls
	state.mu.Unlock()
	return &publicModelCall{observer: observer, runID: state.input.RunID, round: round, publication: publication}
}
func (c *publicModelCall) send(ctx context.Context, phase, content, kind string) error {
	return c.observer(ctx, PublicText{RunID: c.runID, Round: c.round, Phase: phase, Content: content, Kind: kind, Publication: c.publication})
}
func (c *publicModelCall) accept(ctx context.Context, message *schema.Message) error {
	if message == nil || (message.Role != "" && message.Role != schema.Assistant) {
		return nil
	}
	if len(message.ToolCalls) > 0 {
		c.tools = true
	}
	if message.Content == "" {
		return nil
	}
	if !c.opened {
		if err := c.send(ctx, "agent.text.started", "", ""); err != nil {
			return err
		}
		c.opened = true
	}
	return c.send(ctx, "agent.text.delta", message.Content, "")
}
func (c *publicModelCall) end(ctx context.Context, failed bool) error {
	if !c.opened {
		return nil
	}
	kind := "answer_candidate"
	if c.tools {
		kind = "process"
	}
	if failed {
		kind = "failed"
	}
	return c.send(ctx, "agent.text.ended", "", kind)
}

func observePublicStream(ctx context.Context, source *schema.StreamReader[*schema.Message], call *publicModelCall) *schema.StreamReader[*schema.Message] {
	if call == nil || source == nil {
		return source
	}
	reader, writer := schema.Pipe[*schema.Message](1)
	go func() {
		defer writer.Close()
		defer source.Close()
		for {
			message, err := source.Recv()
			if errors.Is(err, io.EOF) {
				if endErr := call.end(ctx, false); endErr != nil {
					writer.Send(nil, endErr)
				}
				return
			}
			if err != nil {
				if endErr := call.end(ctx, true); endErr != nil {
					writer.Send(nil, endErr)
				} else {
					writer.Send(nil, err)
				}
				return
			}
			if err := call.accept(ctx, message); err != nil {
				writer.Send(nil, err)
				return
			}
			if writer.Send(message, nil) {
				return
			}
		}
	}()
	return reader
}
