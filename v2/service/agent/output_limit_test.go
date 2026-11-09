package agent

import (
	"context"
	"testing"

	"GopherAI/model"

	modelcomponent "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// outputLimitModel records the output limit every call was sent with. A call
// that names no limit records zero, because that is what the provider sees:
// there is no limit on the request.
type outputLimitModel struct {
	limits  []int
	respond func(context.Context, int) (*schema.Message, error)
}

func (m *outputLimitModel) record(opts ...modelcomponent.Option) {
	options := modelcomponent.GetCommonOptions(nil, opts...)
	limit := 0
	if options.MaxTokens != nil {
		limit = *options.MaxTokens
	}
	m.limits = append(m.limits, limit)
}

func (m *outputLimitModel) Generate(ctx context.Context, _ []*schema.Message, opts ...modelcomponent.Option) (*schema.Message, error) {
	m.record(opts...)
	return m.respond(ctx, len(m.limits))
}

func (m *outputLimitModel) Stream(ctx context.Context, _ []*schema.Message, opts ...modelcomponent.Option) (*schema.StreamReader[*schema.Message], error) {
	m.record(opts...)
	message, err := m.respond(ctx, len(m.limits))
	if err != nil {
		return nil, err
	}
	if message != nil && message.ResponseMeta == nil {
		message.ResponseMeta = &schema.ResponseMeta{}
	}
	if message != nil && message.ResponseMeta.FinishReason == "" {
		message.ResponseMeta.FinishReason = "stop"
	}
	reader, writer := schema.Pipe[*schema.Message](1)
	go func() {
		defer writer.Close()
		writer.Send(message, nil)
	}()
	return reader, nil
}

func (m *outputLimitModel) WithTools([]*schema.ToolInfo) (modelcomponent.ToolCallingChatModel, error) {
	return m, nil
}

// TestTheAgentTurnSendsTheConfiguredReplyBudget is the half of the request
// budget that used to be missing: the window kept a reserve for the reply and
// the generation left the reply length to the provider's default. The two
// numbers are the same one, so the request and the window that measured it
// cannot drift apart.
func TestTheAgentTurnSendsTheConfiguredReplyBudget(t *testing.T) {
	fake := &outputLimitModel{respond: func(_ context.Context, _ int) (*schema.Message, error) {
		return schema.AssistantMessage("final answer", nil), nil
	}}
	config := DefaultRunnerConfig()
	config.OutputReserveTokens = 3210
	runner := mustRunner(t, fake, StaticToolRegistry{}, newMemoryStepRepository(), config)

	if _, err := runner.Run(context.Background(), testRunContext(), newEventCollector()); err != nil {
		t.Fatal(err)
	}
	if len(fake.limits) == 0 {
		t.Fatal("the turn made no model call")
	}
	for index, limit := range fake.limits {
		if limit != 3210 {
			t.Fatalf("call %d carried an output limit of %d, want the configured 3210", index+1, limit)
		}
	}
}

// TestARunnerWithoutAReserveSendsNoLimit says the other half out loud: a
// runner that was built without a reply budget sends a request without a limit
// rather than inventing one. The number is then the provider's, and a provider
// that changes its default changes this deployment's bill.
func TestARunnerWithoutAReserveSendsNoLimit(t *testing.T) {
	fake := &outputLimitModel{respond: func(_ context.Context, _ int) (*schema.Message, error) {
		return schema.AssistantMessage("final answer", nil), nil
	}}
	config := DefaultRunnerConfig()
	config.OutputReserveTokens = 0
	runner := mustRunner(t, fake, StaticToolRegistry{}, newMemoryStepRepository(), config)

	if _, err := runner.Run(context.Background(), testRunContext(), newEventCollector()); err != nil {
		t.Fatal(err)
	}
	for index, limit := range fake.limits {
		if limit != 0 {
			t.Fatalf("call %d carried an output limit of %d, want none", index+1, limit)
		}
	}
}

// TestTheSummaryRequestCarriesItsOwnOutputLimit is the compaction request. The
// generator measures the document the model returned against its own ceiling,
// so the request has to ask for that same ceiling: a request that asks for
// more can be answered with a document this generator then refuses.
func TestTheSummaryRequestCarriesItsOwnOutputLimit(t *testing.T) {
	fake := &outputLimitModel{respond: func(_ context.Context, _ int) (*schema.Message, error) {
		return schema.AssistantMessage(`{"schema_version":2,"current_goal":"复习","confirmed_decisions":[],"completed_items":[],"open_loops":[],"user_corrections":[],"key_errors":[],"references":[]}`, nil), nil
	}}
	generator, err := NewModelSummaryGenerator(fake, 8000, 1500, model.DefaultSummaryBudget())
	if err != nil {
		t.Fatal(err)
	}
	messages := []model.Message{{ID: 1, IsUser: true, Content: "帮我复习归并排序"}}
	if _, err := generator.GenerateSummary(context.Background(), "", messages); err != nil {
		t.Fatal(err)
	}
	if len(fake.limits) != 1 {
		t.Fatalf("the summary made %d calls, want one", len(fake.limits))
	}
	if fake.limits[0] != 1500 {
		t.Fatalf("the summary request carried an output limit of %d, want its own ceiling 1500", fake.limits[0])
	}
}
