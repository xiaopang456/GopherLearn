package agenttool

import (
	"GopherAI/common/rag"
	serviceagent "GopherAI/service/agent"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/cloudwego/eino/components/tool"
)

func TestKnowledgeSearchSchemaHasBoundedInputsAndNoUserID(t *testing.T) {
	searchTool := mustKnowledgeTool(t, &fakeRetriever{}, 42, DefaultKnowledgeSearchConfig())
	info, err := searchTool.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := info.ToJSONSchema()
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(encoded)
	if err != nil {
		t.Fatal(err)
	}
	schemaText := string(payload)
	for _, field := range []string{"query", "top_k", "document_ids", "content_types"} {
		if !strings.Contains(schemaText, `"`+field+`"`) {
			t.Fatalf("schema missing %s: %s", field, schemaText)
		}
	}
	if strings.Contains(schemaText, "user_id") || !strings.Contains(schemaText, `"required":["query"]`) {
		t.Fatalf("schema must require query and exclude user_id: %s", schemaText)
	}
}

func TestExplicitSourceSurfaceRenamesOnlyUploadedDocumentSearch(t *testing.T) {
	registry, err := NewRegistry(&fakeRetriever{}, DefaultKnowledgeSearchConfig())
	if err != nil {
		t.Fatal(err)
	}
	registry.UseToolSurface(serviceagent.SurfaceExplicitSources)
	tools, err := registry.Tools(context.Background(), serviceagent.RunContext{UserID: 42})
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 {
		t.Fatalf("tool count = %d", len(tools))
	}
	info, err := tools[0].Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != serviceagent.SearchUploadedDocumentsToolName {
		t.Fatalf("tool name = %s", info.Name)
	}
	for _, boundary := range []string{"uploaded documents", "does not search past conversations",
		"learning state", "never saves anything"} {
		if !strings.Contains(info.Desc, boundary) {
			t.Errorf("description does not state %q: %s", boundary, info.Desc)
		}
	}
	// The legacy registry remains byte-for-byte named as before.
	legacy, err := NewRegistry(&fakeRetriever{}, DefaultKnowledgeSearchConfig())
	if err != nil {
		t.Fatal(err)
	}
	legacyTools, err := legacy.Tools(context.Background(), serviceagent.RunContext{UserID: 42})
	if err != nil {
		t.Fatal(err)
	}
	legacyInfo, err := legacyTools[0].Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if legacyInfo.Name != KnowledgeSearchName || legacyInfo.Desc != knowledgeSearchDescription {
		t.Fatal("the explicit-source surface changed the legacy knowledge contract")
	}
}

func TestKnowledgeSearchUsesTrustedUserAndAppliesSafeFilters(t *testing.T) {
	retriever := &fakeRetriever{resultsByQuery: map[string][]rag.RetrievalResult{
		"watch revision": {
			{DocID: "doc-a", DocVersion: 1, DocumentName: "redis.md", ChunkID: "chunk-a", ChunkType: string(rag.ChunkTypeProse), RawContent: "redis fact"},
			{DocID: "doc-b", DocVersion: 2, DocumentName: `../private/rpc.md`, ChunkID: "chunk-b", ChunkIndex: 3, ChunkType: string(rag.ChunkTypeCode), RawContent: "watch code", TitlePath: []string{"etcd", "Watch"}, Symbols: []string{"Watch", "Revision"}, Score: 0.75},
		},
	}}
	searchTool := mustKnowledgeTool(t, retriever, 42, DefaultKnowledgeSearchConfig())
	output := invokeKnowledgeTool(t, searchTool, `{
		"query":"watch revision","top_k":2,"document_ids":["doc-b"],
		"content_types":["CODE"],"user_id":999
	}`)

	if got := retriever.lastUserID(); got != 42 {
		t.Fatalf("retriever user id = %d, want trusted 42", got)
	}
	if got := retriever.lastTopK(); got != maximumRetrievalTopK {
		t.Fatalf("filtered retrieval top_k = %d, want %d", got, maximumRetrievalTopK)
	}
	if len(output.Results) != 1 || output.Results[0].DocumentID != "doc-b" || output.Results[0].ContentType != "code" {
		t.Fatalf("filtered output = %#v", output.Results)
	}
	if output.Results[0].DocumentName != "rpc.md" || output.Results[0].Citation != 1 || output.Results[0].Content != "watch code" {
		t.Fatalf("safe structured result = %#v", output.Results[0])
	}
}

func TestKnowledgeSearchAcceptsExactUploadedFileNameAsDocumentFilter(t *testing.T) {
	retriever := &fakeRetriever{resultsByQuery: map[string][]rag.RetrievalResult{"Tool Schema": {
		{DocID: "doc-a", DocVersion: 1, DocumentName: "other.md", ChunkID: "chunk-a", ChunkType: string(rag.ChunkTypeProse), RawContent: "other"},
		{DocID: "doc-b", DocVersion: 1, DocumentName: "GopherAI-Agent-Learning-Notes-v2.md", ChunkID: "chunk-b", ChunkType: string(rag.ChunkTypeProse), RawContent: "Tool Schema ToolCall Tool Result"},
	}}}
	registry, err := NewRegistry(retriever, DefaultKnowledgeSearchConfig())
	if err != nil {
		t.Fatal(err)
	}
	tools, err := registry.Tools(context.Background(), serviceagent.RunContext{UserID: 7})
	if err != nil {
		t.Fatal(err)
	}
	search := tools[0].(tool.InvokableTool)
	encoded, err := search.InvokableRun(context.Background(), `{"query":"Tool Schema","document_ids":["gopherai-agent-learning-notes-v2.md"]}`)
	if err != nil {
		t.Fatal(err)
	}
	var output KnowledgeSearchOutput
	if err := json.Unmarshal([]byte(encoded), &output); err != nil {
		t.Fatal(err)
	}
	if len(output.Results) != 1 || output.Results[0].DocumentID != "doc-b" {
		t.Fatalf("results = %#v", output.Results)
	}
}

func TestKnowledgeSearchKeepsStableCitationsDeduplicatesContentAndBoundsTokens(t *testing.T) {
	longContent := strings.Repeat("并发取消与超时边界需要传播。", 200)
	retriever := &fakeRetriever{resultsByQuery: map[string][]rag.RetrievalResult{
		"first": {
			{DocID: "doc-a", DocVersion: 1, DocumentName: "a.md", ChunkID: "chunk-a", ChunkType: "prose", RawContent: longContent, Score: 0.8},
		},
		"refined": {
			{DocID: "doc-a", DocVersion: 1, DocumentName: "a.md", ChunkID: "chunk-a", ChunkType: "prose", RawContent: longContent, Score: 0.9},
			{DocID: "doc-b", DocVersion: 1, DocumentName: "b.md", ChunkID: "chunk-b", ChunkType: "code", RawContent: longContent, Score: 0.7},
		},
	}}
	config := DefaultKnowledgeSearchConfig()
	config.MaxPerResultTokens = 160
	config.MaxObservationTokens = 700
	config.MaxRunTokens = 1500
	config.MaxCalls = 2
	searchTool := mustKnowledgeTool(t, retriever, 7, config)

	firstRaw, first := invokeKnowledgeToolRaw(t, searchTool, `{"query":"first","top_k":2}`)
	secondRaw, second := invokeKnowledgeToolRaw(t, searchTool, `{"query":"refined","top_k":2}`)
	if rag.EstimateTokens(firstRaw) > config.MaxObservationTokens || rag.EstimateTokens(secondRaw) > config.MaxObservationTokens {
		t.Fatalf("observation exceeded call budget: first=%d second=%d", rag.EstimateTokens(firstRaw), rag.EstimateTokens(secondRaw))
	}
	if rag.EstimateTokens(firstRaw)+rag.EstimateTokens(secondRaw) > config.MaxRunTokens {
		t.Fatalf("run observations exceeded budget")
	}
	if len(first.Results) != 1 || first.Results[0].Citation != 1 || !first.Results[0].ContentTruncated {
		t.Fatalf("first result = %#v", first.Results)
	}
	if len(second.Results) != 2 || second.Results[0].Citation != 1 || !second.Results[0].AlreadySeen || second.Results[0].Content != "" {
		t.Fatalf("deduplicated result = %#v", second.Results)
	}
	if second.Results[1].Citation != 2 || second.Results[1].Content == "" {
		t.Fatalf("new citation = %#v", second.Results[1])
	}
	if second.RemainingRunTokens < 0 || first.ObservationTokens <= 0 || second.ObservationTokens <= 0 {
		t.Fatalf("budget metadata: first=%#v second=%#v", first, second)
	}
}

func TestKnowledgeSearchHandlesEmptyFailureCancellationAndInvalidInput(t *testing.T) {
	t.Run("empty is valid evidence miss", func(t *testing.T) {
		searchTool := mustKnowledgeTool(t, &fakeRetriever{}, 1, DefaultKnowledgeSearchConfig())
		output := invokeKnowledgeTool(t, searchTool, `{"query":"missing"}`)
		if output.Status != "no_candidates" || output.Reason != rag.KnowledgeReasonNoCandidate || len(output.Results) != 0 || !strings.Contains(output.Summary, "未找到") {
			t.Fatalf("empty output = %#v", output)
		}
	})

	t.Run("typed retrieval unavailability is structured", func(t *testing.T) {
		want := errors.New("redis unavailable")
		searchTool := mustKnowledgeTool(t, &fakeRetriever{err: &rag.RetrievalUnavailableError{Reason: rag.RetrievalReasonVector, Cause: want}}, 1, DefaultKnowledgeSearchConfig())
		output := invokeKnowledgeTool(t, searchTool, `{"query":"redis"}`)
		if output.Status != "unavailable" || output.Reason != rag.KnowledgeReasonVector || len(output.Results) != 0 || !strings.Contains(output.Summary, "暂时不可用") {
			t.Fatalf("unavailable output = %#v", output)
		}
	})

	t.Run("unexpected retrieval failure propagates", func(t *testing.T) {
		want := errors.New("redis unavailable")
		searchTool := mustKnowledgeTool(t, &fakeRetriever{err: want}, 1, DefaultKnowledgeSearchConfig())
		_, err := searchTool.InvokableRun(context.Background(), `{"query":"redis"}`)
		if !errors.Is(err, want) {
			t.Fatalf("error = %v, want wrapped retrieval failure", err)
		}
	})

	t.Run("cancellation propagates", func(t *testing.T) {
		searchTool := mustKnowledgeTool(t, &fakeRetriever{}, 1, DefaultKnowledgeSearchConfig())
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := searchTool.InvokableRun(ctx, `{"query":"redis"}`)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context canceled", err)
		}
	})

	for _, arguments := range []string{
		`{"query":""}`,
		`{"query":"x","top_k":99}`,
		`{"query":"x","content_types":["image"]}`,
	} {
		t.Run(fmt.Sprintf("invalid_%x", arguments), func(t *testing.T) {
			searchTool := mustKnowledgeTool(t, &fakeRetriever{}, 1, DefaultKnowledgeSearchConfig())
			_, err := searchTool.InvokableRun(context.Background(), arguments)
			if !errors.Is(err, ErrInvalidKnowledgeSearchRequest) {
				t.Fatalf("error = %v, want invalid request", err)
			}
		})
	}
}

func mustKnowledgeTool(t *testing.T, retriever Retriever, userID int64, config KnowledgeSearchConfig) tool.InvokableTool {
	t.Helper()
	registry, err := NewRegistry(retriever, config)
	if err != nil {
		t.Fatal(err)
	}
	tools, err := registry.Tools(context.Background(), serviceagent.RunContext{UserID: userID, RunID: "run", SessionID: "session"})
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 {
		t.Fatalf("tools = %d, want 1", len(tools))
	}
	invokable, ok := tools[0].(tool.InvokableTool)
	if !ok {
		t.Fatal("knowledge tool is not invokable")
	}
	return invokable
}

func invokeKnowledgeTool(t *testing.T, searchTool tool.InvokableTool, arguments string) KnowledgeSearchOutput {
	t.Helper()
	_, output := invokeKnowledgeToolRaw(t, searchTool, arguments)
	return output
}

func invokeKnowledgeToolRaw(t *testing.T, searchTool tool.InvokableTool, arguments string) (string, KnowledgeSearchOutput) {
	t.Helper()
	raw, err := searchTool.InvokableRun(context.Background(), arguments)
	if err != nil {
		t.Fatal(err)
	}
	var output KnowledgeSearchOutput
	if err := json.Unmarshal([]byte(raw), &output); err != nil {
		t.Fatalf("decode output %q: %v", raw, err)
	}
	return raw, output
}

type fakeRetriever struct {
	mu             sync.Mutex
	resultsByQuery map[string][]rag.RetrievalResult
	err            error
	userIDs        []int64
	topKs          []int
}

func (f *fakeRetriever) Retrieve(ctx context.Context, userID int64, query string, topK int) ([]rag.RetrievalResult, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.userIDs = append(f.userIDs, userID)
	f.topKs = append(f.topKs, topK)
	if f.err != nil {
		return nil, f.err
	}
	return append([]rag.RetrievalResult(nil), f.resultsByQuery[query]...), nil
}

func (f *fakeRetriever) lastUserID() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.userIDs[len(f.userIDs)-1]
}

func (f *fakeRetriever) lastTopK() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.topKs[len(f.topKs)-1]
}
