package isolated

import (
	"context"
	"strings"
	"testing"

	modelcomponent "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// The segment writer's model has to come from this entry's configuration.
//
// This entry refuses environment credentials on purpose: the operator names the
// key, the model and the endpoint in the settings file. A segment writer built
// from OPENAI_* would either refuse to start on a deployment that never set
// them, or - worse - start and quietly spend a different account against a
// different endpoint than every other call in the process.
func TestTheSegmentWriterUsesTheInjectedModel(t *testing.T) {
	s := testSettings(t)
	s.Config.AgentConfig.SummarySegmentMode = "shadow"

	asked := 0
	assembly, err := buildSummarySegmentAssembly(context.Background(), s.Config,
		func(context.Context) (modelcomponent.ToolCallingChatModel, error) {
			asked++
			return &answerStub{}, nil
		})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !assembly.Enabled() {
		t.Fatal("the assembly is disabled with the mode on")
	}
	if asked != 1 {
		t.Fatalf("the injected factory was asked %d times, want 1", asked)
	}
}

// A factory that is not wired is a startup error, not a silent fallback.
//
// Falling back to the process environment here would be the same defect one
// layer down: the deployment would run, and the segments would be written by a
// model nobody configured.
func TestTheSegmentWriterRefusesToGuessAModel(t *testing.T) {
	s := testSettings(t)
	s.Config.AgentConfig.SummarySegmentMode = "shadow"

	_, err := buildSummarySegmentAssembly(context.Background(), s.Config, nil)
	if err == nil {
		t.Fatal("the assembly was built without a model factory")
	}
	if !strings.Contains(err.Error(), "must be explicit") {
		t.Fatalf("error = %v, want it to name the missing factory", err)
	}
}

// The off mode never asks for the model: it has no writer, so it must not
// fail startup over a factory it will not use.
func TestTheOffModeDoesNotAskForTheSegmentModel(t *testing.T) {
	s := testSettings(t)
	s.Config.AgentConfig.SummarySegmentMode = "off"

	assembly, err := buildSummarySegmentAssembly(context.Background(), s.Config, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if assembly != nil {
		t.Fatal("the off mode built an assembly")
	}
}

// answerStub satisfies the chat-model interface without answering anything.
type answerStub struct{}

func (s *answerStub) WithTools([]*schema.ToolInfo) (modelcomponent.ToolCallingChatModel, error) {
	return s, nil
}

func (s *answerStub) Generate(context.Context, []*schema.Message,
	...modelcomponent.Option) (*schema.Message, error) {
	return schema.AssistantMessage("{}", nil), nil
}

func (s *answerStub) Stream(context.Context, []*schema.Message,
	...modelcomponent.Option) (*schema.StreamReader[*schema.Message], error) {
	return nil, nil
}
