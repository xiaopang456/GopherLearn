package conversation

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	modelcomponent "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// answerStub answers with fixed text and counts the calls it served.
type answerStub struct {
	content string
	calls   int
}

func (s *answerStub) WithTools([]*schema.ToolInfo) (modelcomponent.ToolCallingChatModel, error) {
	return s, nil
}

func (s *answerStub) Generate(context.Context, []*schema.Message,
	...modelcomponent.Option) (*schema.Message, error) {
	s.calls++
	return &schema.Message{Role: schema.Assistant, Content: s.content}, nil
}

func (s *answerStub) Stream(context.Context, []*schema.Message,
	...modelcomponent.Option) (*schema.StreamReader[*schema.Message], error) {
	return nil, nil
}

// A call that fails without a body must not report the previous answer.
//
// The failure this guards is silent and specific: a batch times out, the
// recorder still holds the batch before it, and the report files that earlier
// answer as the evidence for this failure. The evidence would then point at a
// body that parsed fine.
func TestRecordingChatModelDropsThePreviousAnswerOnAFailedCall(t *testing.T) {
	stub := &answerStub{content: `{"schema_version":1}`}
	recorder := &recordingChatModel{inner: stub}
	if _, err := recorder.Generate(context.Background(), []*schema.Message{schema.UserMessage("q")}); err != nil {
		t.Fatalf("generate: %v", err)
	}

	// The same recorder, now wrapped around a model that fails without
	// answering.
	recorder.inner = &failingStub{}
	if _, err := recorder.Generate(context.Background(), []*schema.Message{schema.UserMessage("q")}); err == nil {
		t.Fatal("the failing call reported no error")
	}
	if got := recorder.excerpt(); got != "" {
		t.Fatalf("excerpt = %q, want nothing: the call returned no body", got)
	}
}

type failingStub struct{}

func (f *failingStub) WithTools([]*schema.ToolInfo) (modelcomponent.ToolCallingChatModel, error) {
	return f, nil
}

func (f *failingStub) Generate(context.Context, []*schema.Message,
	...modelcomponent.Option) (*schema.Message, error) {
	return nil, errNoAnswer
}

func (f *failingStub) Stream(context.Context, []*schema.Message,
	...modelcomponent.Option) (*schema.StreamReader[*schema.Message], error) {
	return nil, nil
}

var errNoAnswer = errors.New("the provider answered nothing")

// waiting between attempts is the worker's RetryDelay, and a torn-down run
// must not sleep through it.
func TestWaitBeforeRetryHonoursTheContext(t *testing.T) {
	if err := waitBeforeRetry(context.Background(), 0); err != nil {
		t.Fatalf("a zero delay waited and failed: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitBeforeRetry(ctx, 30*time.Second); err == nil {
		t.Fatal("a cancelled run waited out the delay instead of stopping")
	}
}

// The body a failed batch answered is the only evidence that tells a truncated
// answer apart from a wrongly shaped one, so the wrapper has to keep it.
func TestRecordingChatModelKeepsTheLastAnswer(t *testing.T) {
	stub := &answerStub{content: `{"schema_version":1,`}
	recorder := &recordingChatModel{inner: stub}

	if _, err := recorder.Generate(context.Background(), []*schema.Message{schema.UserMessage("q")}); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if got := recorder.excerpt(); got != `{"schema_version":1,` {
		t.Fatalf("excerpt = %q, want the answer verbatim", got)
	}
	if stub.calls != 1 {
		t.Fatalf("inner model served %d calls, want 1", stub.calls)
	}
}

// A body that is not JSON can be very long - a model that ignores the format
// may answer with prose. The report holds a bounded head, not the whole thing.
func TestRecordingChatModelBoundsTheExcerpt(t *testing.T) {
	body := strings.Repeat("字", excerptForReport+50)
	recorder := &recordingChatModel{inner: &answerStub{content: body}}

	if _, err := recorder.Generate(context.Background(), []*schema.Message{schema.UserMessage("q")}); err != nil {
		t.Fatalf("generate: %v", err)
	}
	got := recorder.excerpt()
	if !strings.HasSuffix(got, " ...") {
		t.Fatalf("excerpt = %q, want a marked truncation", got[len(got)-8:])
	}
	if runes := []rune(strings.TrimSuffix(got, " ...")); len(runes) != excerptForReport {
		t.Fatalf("kept %d runes, want %d", len(runes), excerptForReport)
	}
}

// Binding tools returns a new wrapper. A recorder that handed the inner model
// back would lose the answer on every run that binds tools first.
func TestRecordingChatModelKeepsRecordingAfterWithTools(t *testing.T) {
	recorder := &recordingChatModel{inner: &answerStub{content: "second"}}

	bound, err := recorder.WithTools(nil)
	if err != nil {
		t.Fatalf("with tools: %v", err)
	}
	if _, err := bound.Generate(context.Background(), []*schema.Message{schema.UserMessage("q")}); err != nil {
		t.Fatalf("generate: %v", err)
	}
	next, ok := bound.(*recordingChatModel)
	if !ok {
		t.Fatalf("bound model is %T, want the recorder", bound)
	}
	if got := next.excerpt(); got != "second" {
		t.Fatalf("excerpt = %q, want the answer served after binding", got)
	}
}
