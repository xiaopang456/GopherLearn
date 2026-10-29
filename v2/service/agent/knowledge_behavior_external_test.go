package agent_test

import (
	"GopherAI/common/rag"
	"GopherAI/model"
	serviceagent "GopherAI/service/agent"
	agenttool "GopherAI/service/agent/tool"
	serviceknowledge "GopherAI/service/knowledge"
	"context"
	"fmt"
	"sync"
	"testing"

	modelcomponent "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

func TestKnowledgeAgentSimpleReformulatedAndEmptyEvidenceBehaviors(t *testing.T) {
	tests := []struct {
		name        string
		queries     []string
		results     map[string][]rag.RetrievalResult
		answer      string
		wantAnswer  string
		wantSources int
		wantModels  int
		policy      rag.KnowledgePolicy
	}{
		{
			name: "simple question searches once", queries: []string{"Redis TTL"}, answer: "TTL answer [1]", wantAnswer: "TTL answer [1]", wantSources: 1, wantModels: 2,
			results: map[string][]rag.RetrievalResult{"Redis TTL": {evidenceResult("redis", "ttl")}},
		},
		{
			name: "complex question rewrites query", queries: []string{"etcd Watch", "etcd Watch Revision 断线恢复"}, answer: "combined answer [1][2]", wantAnswer: "combined answer [1][2]", wantSources: 2, wantModels: 3,
			results: map[string][]rag.RetrievalResult{
				"etcd Watch":               {evidenceResult("etcd", "watch")},
				"etcd Watch Revision 断线恢复": {evidenceResult("rpc", "revision")},
			},
		},
		{
			name: "empty evidence cannot become a fabricated answer", queries: []string{"不存在的私人知识"}, answer: "笔记里写了虚构结论 [1]", wantAnswer: serviceagent.NoGroundingEvidenceAnswer, wantSources: 0, wantModels: 1,
			results: map[string][]rag.RetrievalResult{},
			policy:  rag.KnowledgeOnly,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			retriever := &behaviorRetriever{results: test.results}
			registry, err := agenttool.NewRegistry(retriever, agenttool.DefaultKnowledgeSearchConfig())
			if err != nil {
				t.Fatal(err)
			}
			chatModel := &behaviorModel{queries: test.queries, answer: test.answer}
			runner, err := serviceagent.NewRunner(
				chatModel, registry, &behaviorSteps{}, serviceagent.MinimalContextBuilder{}, serviceagent.DefaultRunnerConfig(),
			)
			if err != nil {
				t.Fatal(err)
			}
			result, err := runner.Run(context.Background(), serviceagent.RunContext{
				RunID: "run", UserID: 77, UserName: "learner", SessionID: "session", Question: "question", KnowledgePolicy: test.policy,
			}, serviceagent.DiscardEvents)
			if err != nil {
				t.Fatal(err)
			}
			if result.Answer != test.wantAnswer || result.ToolCalls != len(test.queries) || result.ModelCalls != test.wantModels {
				t.Fatalf("result = %#v", result)
			}
			if len(result.Sources) != test.wantSources {
				t.Fatalf("sources = %#v, want %d", result.Sources, test.wantSources)
			}
			if got := retriever.recordedQueries(); fmt.Sprint(got) != fmt.Sprint(test.queries) {
				t.Fatalf("queries = %#v, want %#v", got, test.queries)
			}
			for _, userID := range retriever.recordedUsers() {
				if userID != 77 {
					t.Fatalf("retrieval user id = %d, want 77", userID)
				}
			}
		})
	}
}

func TestKnowledgeAgentSearchesThenReadsBoundedSourceBeforeAnswering(t *testing.T) {
	retriever := &behaviorRetriever{results: map[string][]rag.RetrievalResult{
		"RPC Serve 实现": {{
			DocID: "rpc-note", DocVersion: 2, DocumentName: "note.md", ChunkID: "serve-summary",
			ChunkIndex: 4, ChunkType: string(rag.ChunkTypeCode), RawContent: "func Serve()", StartLine: 5, EndLine: 6,
		}},
	}}
	reader := &behaviorSourceReader{source: serviceknowledge.ActiveDocumentSource{
		DocumentID: "rpc-note", Version: 2, DocumentName: "note.md", FileType: "md",
		Content: "# RPC\n说明\n\n```go\nfunc Serve() {\n\tlisten()\n}\n```\n结尾",
	}}
	registry, err := agenttool.NewKnowledgeRegistry(retriever, reader, agenttool.DefaultKnowledgeSearchConfig(), agenttool.DefaultDocumentReadConfig())
	if err != nil {
		t.Fatal(err)
	}
	model := &sequenceBehaviorModel{messages: []*schema.Message{
		schema.AssistantMessage("", []schema.ToolCall{{ID: "search", Function: schema.FunctionCall{Name: agenttool.KnowledgeSearchName, Arguments: `{"query":"RPC Serve 实现"}`}}}),
		schema.AssistantMessage("", []schema.ToolCall{{ID: "read", Function: schema.FunctionCall{Name: agenttool.DocumentReadName, Arguments: `{"document_id":"rpc-note","document_version":2,"start_line":5,"end_line":6}`}}}),
		schema.AssistantMessage("Serve 的完整实现会调用 listen [2]。", nil),
	}}
	runner, err := serviceagent.NewRunner(model, registry, &behaviorSteps{}, serviceagent.MinimalContextBuilder{}, serviceagent.DefaultRunnerConfig())
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Run(context.Background(), serviceagent.RunContext{
		RunID: "run", UserID: 77, UserName: "learner", SessionID: "session", Question: "Serve 如何实现？",
	}, serviceagent.DiscardEvents)
	if err != nil {
		t.Fatal(err)
	}
	if result.ToolCalls != 2 || result.ModelCalls != 3 || len(result.Sources) != 1 || result.Sources[0].Citation != 2 ||
		result.Sources[0].StartLine != 4 || result.Sources[0].EndLine != 8 {
		t.Fatalf("result = %#v", result)
	}
	if reader.userID != 77 || reader.calls != 1 {
		t.Fatalf("reader user=%d calls=%d", reader.userID, reader.calls)
	}
}

func evidenceResult(docID, chunkID string) rag.RetrievalResult {
	return rag.RetrievalResult{
		DocID: docID, DocVersion: 1, DocumentName: docID + ".md",
		ChunkID: chunkID, ChunkIndex: 1, ChunkType: string(rag.ChunkTypeProse),
		RawContent: docID + " evidence", Score: 0.8,
	}
}

type behaviorRetriever struct {
	mu      sync.Mutex
	results map[string][]rag.RetrievalResult
	queries []string
	users   []int64
}

func (r *behaviorRetriever) Retrieve(ctx context.Context, userID int64, query string, _ int) ([]rag.RetrievalResult, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.queries = append(r.queries, query)
	r.users = append(r.users, userID)
	return append([]rag.RetrievalResult(nil), r.results[query]...), nil
}

func (r *behaviorRetriever) recordedQueries() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.queries...)
}

func (r *behaviorRetriever) recordedUsers() []int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int64(nil), r.users...)
}

type behaviorModel struct {
	mu      sync.Mutex
	calls   int
	queries []string
	answer  string
}

type behaviorSourceReader struct {
	source serviceknowledge.ActiveDocumentSource
	userID int64
	calls  int
}

func (r *behaviorSourceReader) ReadActiveSource(_ context.Context, userID int64, _ string, _ int) (serviceknowledge.ActiveDocumentSource, error) {
	r.userID = userID
	r.calls++
	return r.source, nil
}

type sequenceBehaviorModel struct {
	mu       sync.Mutex
	messages []*schema.Message
	calls    int
}

func (m *sequenceBehaviorModel) next() *schema.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	message := m.messages[m.calls]
	m.calls++
	return message
}

func (m *sequenceBehaviorModel) Generate(context.Context, []*schema.Message, ...modelcomponent.Option) (*schema.Message, error) {
	return m.next(), nil
}

func (m *sequenceBehaviorModel) Stream(context.Context, []*schema.Message, ...modelcomponent.Option) (*schema.StreamReader[*schema.Message], error) {
	reader, writer := schema.Pipe[*schema.Message](1)
	writer.Send(withBehaviorFinishReason(m.next()), nil)
	writer.Close()
	return reader, nil
}

func (m *sequenceBehaviorModel) WithTools([]*schema.ToolInfo) (modelcomponent.ToolCallingChatModel, error) {
	return m, nil
}

func (m *behaviorModel) next() *schema.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	if m.calls <= len(m.queries) {
		query := m.queries[m.calls-1]
		return schema.AssistantMessage("", []schema.ToolCall{{
			ID: fmt.Sprintf("call-%d", m.calls), Function: schema.FunctionCall{
				Name: agenttool.KnowledgeSearchName, Arguments: fmt.Sprintf(`{"query":%q}`, query),
			},
		}})
	}
	return schema.AssistantMessage(m.answer, nil)
}

func (m *behaviorModel) Generate(context.Context, []*schema.Message, ...modelcomponent.Option) (*schema.Message, error) {
	return m.next(), nil
}

func (m *behaviorModel) Stream(context.Context, []*schema.Message, ...modelcomponent.Option) (*schema.StreamReader[*schema.Message], error) {
	reader, writer := schema.Pipe[*schema.Message](1)
	writer.Send(withBehaviorFinishReason(m.next()), nil)
	writer.Close()
	return reader, nil
}

func withBehaviorFinishReason(message *schema.Message) *schema.Message {
	if message == nil {
		return nil
	}
	if message.ResponseMeta == nil {
		message.ResponseMeta = &schema.ResponseMeta{}
	}
	message.ResponseMeta.FinishReason = "stop"
	if len(message.ToolCalls) > 0 {
		message.ResponseMeta.FinishReason = "tool_calls"
	}
	return message
}

func (m *behaviorModel) WithTools([]*schema.ToolInfo) (modelcomponent.ToolCallingChatModel, error) {
	return m, nil
}

type behaviorSteps struct {
	mu    sync.Mutex
	steps []model.AgentStep
}

func (s *behaviorSteps) AppendStep(_ context.Context, _ string, step *model.AgentStep) error {
	s.mu.Lock()
	s.steps = append(s.steps, *step)
	s.mu.Unlock()
	return nil
}
