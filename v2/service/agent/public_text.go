package agent

import (
	"context"
	"errors"
	"io"
	"strings"
	"unicode"

	"github.com/cloudwego/eino/schema"
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
	pending     string
	malformed   bool
}

const malformedToolCallAnswer = "这一轮模型未能正常调用工具，请重试。"

// The tool-call protocol of the provider leaks into Content when the model
// boundary cannot turn a call into a structured ToolCall: the markup arrives
// as ordinary text. The filter must not depend on one exact spelling of it.
//
// Two shapes have been seen in the records: the doubled bar (｜｜DSML｜｜) and
// the single bar (｜DSML｜). The bar and the angle bracket belong to the
// provider's markup, not to this program, so ASCII forms are accepted too.
var toolProtocolBars = []string{"｜", "|"}

// toolProtocolMarkerPatterns enumerates the complete marker shapes the filter
// recognises. The streaming path also uses them as the set of prefixes it may
// hold back, which is what keeps a marker split across chunks from leaking.
var toolProtocolMarkerPatterns = buildToolProtocolMarkerPatterns()

func buildToolProtocolMarkerPatterns() []string {
	patterns := make([]string, 0, 108)
	for _, bar := range toolProtocolBars {
		for before := 1; before <= 3; before++ {
			for after := 1; after <= 3; after++ {
				core := strings.Repeat(bar, before) + "DSML" + strings.Repeat(bar, after)
				patterns = append(patterns,
					core,
					"<"+core,
					"</"+core,
					strings.Repeat(bar, before)+"dsml"+strings.Repeat(bar, after),
					"<"+strings.Repeat(bar, before)+"dsml"+strings.Repeat(bar, after),
				)
			}
		}
	}
	return patterns
}

func isToolProtocolBar(r rune) bool {
	return r == '｜' || r == '|'
}

func isToolProtocolLetter(r rune, want rune) bool {
	return unicode.ToUpper(r) == want
}

// hasToolProtocol reports whether text carries the protocol's marker word.
//
// It is the word DSML with at least one vertical bar on each side, in either
// bar width and either letter case. Requiring a bar on both sides is what
// keeps ordinary prose that happens to contain the four letters from being
// rewritten as a failure message.
func hasToolProtocol(text string) bool {
	runes := []rune(text)
	for i := 0; i+len("DSML") <= len(runes); i++ {
		if !isToolProtocolLetter(runes[i], 'D') ||
			!isToolProtocolLetter(runes[i+1], 'S') ||
			!isToolProtocolLetter(runes[i+2], 'M') ||
			!isToolProtocolLetter(runes[i+3], 'L') {
			continue
		}
		barsBefore := 0
		for j := i - 1; j >= 0 && isToolProtocolBar(runes[j]); j-- {
			barsBefore++
		}
		barsAfter := 0
		for j := i + 4; j < len(runes) && isToolProtocolBar(runes[j]); j++ {
			barsAfter++
		}
		if barsBefore > 0 && barsAfter > 0 {
			return true
		}
	}
	return false
}

// toolProtocolHoldback returns how many bytes at the end of text may still
// become a protocol marker, so the streaming filter can keep them from being
// published before the next chunk says what they are.
func toolProtocolHoldback(text string) int {
	if text == "" {
		return 0
	}
	runes := []rune(text)
	limit := 0
	for _, pattern := range toolProtocolMarkerPatterns {
		if length := len([]rune(pattern)); length > limit {
			limit = length
		}
	}
	if limit > len(runes) {
		limit = len(runes)
	}
	for n := limit; n >= 1; n-- {
		suffix := string(runes[len(runes)-n:])
		for _, pattern := range toolProtocolMarkerPatterns {
			if len(suffix) < len(pattern) && strings.HasPrefix(pattern, suffix) {
				return len(suffix)
			}
		}
	}
	return 0
}

// isIncompleteToolProtocol reports whether a stream that ended left a piece of
// the protocol's markup in the buffer. A single trailing bar is ordinary text
// and is published; a bar plus a letter of the marker word is not.
func isIncompleteToolProtocol(text string) bool {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return false
	}
	held := toolProtocolHoldback(trimmed)
	if held <= 0 || held > len(trimmed) {
		return false
	}
	suffix := trimmed[len(trimmed)-held:]
	if len([]rune(suffix)) < 2 {
		return false
	}
	for _, r := range suffix {
		if isToolProtocolBar(r) {
			return true
		}
	}
	return false
}

func safeModelText(text string) string {
	if hasToolProtocol(text) {
		return malformedToolCallAnswer
	}
	return text
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
func (c *publicModelCall) emit(ctx context.Context, content string) error {
	if content == "" {
		return nil
	}
	if !c.opened {
		if err := c.send(ctx, "agent.text.started", "", ""); err != nil {
			return err
		}
		c.opened = true
	}
	return c.send(ctx, "agent.text.delta", content, "")
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
	if c.malformed {
		return nil
	}
	c.pending += message.Content
	if hasToolProtocol(c.pending) {
		c.pending = ""
		c.malformed = true
		return c.emit(ctx, malformedToolCallAnswer)
	}
	hold := toolProtocolHoldback(c.pending)
	ready := c.pending[:len(c.pending)-hold]
	c.pending = c.pending[len(c.pending)-hold:]
	return c.emit(ctx, ready)
}
func (c *publicModelCall) end(ctx context.Context, failed bool) error {
	if !c.malformed && c.pending != "" {
		if hasToolProtocol(c.pending) || isIncompleteToolProtocol(c.pending) {
			// The stream ended with protocol markup, or with a piece of it that
			// never became a complete marker. Either way that text is not an
			// answer, and publishing it would show the learner the transport.
			c.pending = ""
			c.malformed = true
			if err := c.emit(ctx, malformedToolCallAnswer); err != nil {
				return err
			}
		}
	}
	if err := c.emit(ctx, c.pending); err != nil {
		return err
	}
	c.pending = ""
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
