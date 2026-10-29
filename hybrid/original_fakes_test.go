package hybrid

import (
	"context"
	"github.com/cloudwego/eino/components/embedding"
	modelcomponent "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"sync"
)

type scriptedChatModel struct {
	mu                   sync.Mutex
	calls                int
	forcedToolChoices    int
	forbiddenToolChoices int
	boundToolCalls       [][]string
	respond              func(context.Context, int) (*schema.Message, error)
	forbiddenAnswer      string
	omitFinishReason     bool
}

func (m *scriptedChatModel) next(ctx context.Context, opts ...modelcomponent.Option) (*schema.Message, error) {
	m.mu.Lock()
	m.calls++
	call := m.calls
	forbiddenAnswer := m.forbiddenAnswer
	m.mu.Unlock()
	options := modelcomponent.GetCommonOptions(nil, opts...)
	if forbiddenAnswer != "" && options.ToolChoice != nil && *options.ToolChoice == schema.ToolChoiceForbidden {
		return schema.AssistantMessage(forbiddenAnswer, nil), nil
	}
	return m.respond(ctx, call)
}

func (m *scriptedChatModel) Generate(ctx context.Context, _ []*schema.Message, opts ...modelcomponent.Option) (*schema.Message, error) {
	m.recordOptions(opts...)
	return m.next(ctx, opts...)
}

func (m *scriptedChatModel) Stream(ctx context.Context, _ []*schema.Message, opts ...modelcomponent.Option) (*schema.StreamReader[*schema.Message], error) {
	m.recordOptions(opts...)
	message, err := m.next(ctx, opts...)
	if err != nil {
		return nil, err
	}
	if message != nil && !m.omitFinishReason {
		if message.ResponseMeta == nil {
			message.ResponseMeta = &schema.ResponseMeta{}
		}
		if message.ResponseMeta.FinishReason == "" {
			message.ResponseMeta.FinishReason = "stop"
			if len(message.ToolCalls) > 0 {
				message.ResponseMeta.FinishReason = "tool_calls"
			}
		}
	}
	reader, writer := schema.Pipe[*schema.Message](1)
	writer.Send(message, nil)
	writer.Close()
	return reader, nil
}

func (m *scriptedChatModel) WithTools(tools []*schema.ToolInfo) (modelcomponent.ToolCallingChatModel, error) {
	names := make([]string, 0, len(tools))
	for _, info := range tools {
		if info != nil {
			names = append(names, info.Name)
		}
	}
	m.mu.Lock()
	m.boundToolCalls = append(m.boundToolCalls, names)
	m.mu.Unlock()
	return m, nil
}

func (m *scriptedChatModel) BoundToolCalls() [][]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([][]string, len(m.boundToolCalls))
	for index := range m.boundToolCalls {
		out[index] = append([]string(nil), m.boundToolCalls[index]...)
	}
	return out
}

func (m *scriptedChatModel) HasExclusiveToolBinding(name string) bool {
	for _, names := range m.BoundToolCalls() {
		if len(names) == 1 && names[0] == name {
			return true
		}
	}
	return false
}

func (m *scriptedChatModel) CallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func (m *scriptedChatModel) recordOptions(opts ...modelcomponent.Option) {
	options := modelcomponent.GetCommonOptions(nil, opts...)
	if options.ToolChoice == nil {
		return
	}
	m.mu.Lock()
	switch *options.ToolChoice {
	case schema.ToolChoiceForced:
		m.forcedToolChoices++
	case schema.ToolChoiceForbidden:
		m.forbiddenToolChoices++
	}
	m.mu.Unlock()
}

func (m *scriptedChatModel) ForcedToolChoiceCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.forcedToolChoices
}

func (m *scriptedChatModel) ForbiddenToolChoiceCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.forbiddenToolChoices
}

type retrievalFakeEmbedder struct {
	dimension      int
	calls          int
	err            error
	waitForContext bool
}

func (f *retrievalFakeEmbedder) EmbedStrings(ctx context.Context, texts []string, _ ...embedding.Option) ([][]float64, error) {
	f.calls++
	if f.waitForContext {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if f.err != nil {
		return nil, f.err
	}
	vectors := make([][]float64, len(texts))
	for index := range vectors {
		vectors[index] = make([]float64, f.dimension)
		vectors[index][0] = 1
	}
	return vectors, nil
}
