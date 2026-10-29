package agenttool

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"GopherAI/common/rag"
	serviceagent "GopherAI/service/agent"
	serviceknowledge "GopherAI/service/knowledge"

	"github.com/cloudwego/eino/components/tool"
)

func TestDocumentReadSchemaExposesOnlySafeSelectors(t *testing.T) {
	_, readTool := mustKnowledgeAndReadTools(t, &fakeRetriever{}, &fakeActiveSourceReader{}, 42, DefaultKnowledgeSearchConfig(), DefaultDocumentReadConfig())
	info, err := readTool.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	schema, err := info.ToJSONSchema()
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(schema)
	text := string(encoded)
	for _, field := range []string{"document_id", "document_version", "title_path", "start_line", "end_line"} {
		if !strings.Contains(text, `"`+field+`"`) {
			t.Fatalf("schema missing %s: %s", field, text)
		}
	}
	if strings.Contains(text, "user_id") || strings.Contains(text, "storage_path") || strings.Contains(text, "file_path") {
		t.Fatalf("unsafe field leaked in schema: %s", text)
	}
}

func TestDocumentReadRequiresSearchAndUsesTrustedUser(t *testing.T) {
	retriever := &fakeRetriever{
		resultsByQuery: map[string][]rag.RetrievalResult{
			"rpc": {
				{
					DocID: "doc-rpc", DocVersion: 1, DocumentName: "rpc.md", ChunkID: "chunk-1",
					ChunkType: "code", StartLine: 5, EndLine: 6, RawContent: "func Serve()", TitlePath: []string{"RPC"},
				},
			},
		},
	}
	reader := &fakeActiveSourceReader{source: serviceknowledge.ActiveDocumentSource{
		DocumentID: "doc-rpc", Version: 1, DocumentName: "rpc.md", FileType: "md",
		Content: "# RPC\nintro\n\n```go\nfunc Serve() {\n\treturn\n}\n```\ntail",
	}}
	searchTool, readTool := mustKnowledgeAndReadTools(t, retriever, reader, 42, DefaultKnowledgeSearchConfig(), DefaultDocumentReadConfig())

	direct := invokeDocumentRead(t, readTool, `{"document_id":"doc-rpc","document_version":1,"start_line":5,"end_line":6}`)
	if direct.Status != "retry_required" || direct.Error == nil || direct.Error.Code != "search_required" || reader.callCount() != 0 {
		t.Fatalf("direct read = %#v, calls=%d", direct, reader.callCount())
	}
	search := invokeKnowledgeTool(t, searchTool, `{"query":"rpc"}`)
	if len(search.Results) != 1 || search.Results[0].Citation != 1 {
		t.Fatalf("search = %#v", search)
	}
	read := invokeDocumentRead(t, readTool, `{"document_id":"doc-rpc","document_version":1,"start_line":5,"end_line":6,"user_id":999,"file_path":"../secret"}`)
	if read.Status != "ok" || len(read.Results) != 1 {
		t.Fatalf("read = %#v", read)
	}
	result := read.Results[0]
	if reader.lastUserID() != 42 || result.Citation != 2 || result.StartLine != 4 || result.EndLine != 8 || !result.RangeExpanded {
		t.Fatalf("safe expanded read = %#v, user=%d", result, reader.lastUserID())
	}
	if !strings.HasPrefix(result.Content, "```go\n") || !strings.HasSuffix(result.Content, "\n```") {
		t.Fatalf("code fence was split: %q", result.Content)
	}
	readAgain := invokeDocumentRead(t, readTool, `{"document_id":"doc-rpc","document_version":1,"start_line":4,"end_line":8}`)
	if len(readAgain.Results) != 1 || readAgain.Results[0].Citation != 2 || !readAgain.Results[0].AlreadySeen || readAgain.Results[0].Content != "" {
		t.Fatalf("repeat evidence = %#v", readAgain)
	}
}

func TestDocumentReadSupportsHeadingAndReturnsRetryableBounds(t *testing.T) {
	content := "# RPC\nintro\n## Codec\ncodec body\n```go\nfunc Encode() {}\n```\n## Server\nserver body\n"
	retriever := &fakeRetriever{
		resultsByQuery: map[string][]rag.RetrievalResult{
			"codec": {
				{
					DocID: "doc-rpc", DocVersion: 3, DocumentName: "rpc.md", ChunkID: "chunk-codec", RawContent: "codec", TitlePath: []string{"RPC", "Codec"},
				},
			},
		},
	}
	reader := &fakeActiveSourceReader{source: serviceknowledge.ActiveDocumentSource{DocumentID: "doc-rpc", Version: 3, DocumentName: "rpc.md", FileType: "md", Content: content}}
	searchTool, readTool := mustKnowledgeAndReadTools(t, retriever, reader, 7, DefaultKnowledgeSearchConfig(), DefaultDocumentReadConfig())
	_ = invokeKnowledgeTool(t, searchTool, `{"query":"codec"}`)

	heading := invokeDocumentRead(t, readTool, `{"document_id":"doc-rpc","document_version":3,"title_path":["RPC","Codec"]}`)
	if heading.Status != "ok" || heading.Results[0].StartLine != 3 || heading.Results[0].EndLine != 7 || !strings.Contains(heading.Results[0].Content, "func Encode") {
		t.Fatalf("heading output = %#v", heading)
	}
	outOfBounds := invokeDocumentRead(t, readTool, `{"document_id":"doc-rpc","document_version":3,"start_line":8,"end_line":80}`)
	if outOfBounds.Status != "retry_required" || outOfBounds.Error == nil || outOfBounds.Error.Code != "range_out_of_bounds" || outOfBounds.Error.DocumentLines != 9 {
		t.Fatalf("bounds output = %#v", outOfBounds)
	}
}

func TestDocumentReadReturnsStructuredRetryForLargeRange(t *testing.T) {
	lines := make([]string, 30)
	for index := range lines {
		lines[index] = "line"
	}
	retriever := &fakeRetriever{
		resultsByQuery: map[string][]rag.RetrievalResult{
			"large": {
				{DocID: "doc", DocVersion: 1, ChunkID: "chunk", RawContent: "line"},
			},
		},
	}
	reader := &fakeActiveSourceReader{source: serviceknowledge.ActiveDocumentSource{DocumentID: "doc", Version: 1, DocumentName: "large.txt", FileType: "txt", Content: strings.Join(lines, "\n")}}
	readConfig := DefaultDocumentReadConfig()
	readConfig.MaxLines = 20
	searchTool, readTool := mustKnowledgeAndReadTools(t, retriever, reader, 1, DefaultKnowledgeSearchConfig(), readConfig)
	_ = invokeKnowledgeTool(t, searchTool, `{"query":"large"}`)
	output := invokeDocumentRead(t, readTool, `{"document_id":"doc","document_version":1,"start_line":1,"end_line":30}`)
	if output.Status != "retry_required" || output.Error == nil || output.Error.Code != "range_too_large" || output.Error.SuggestedEnd != 20 {
		t.Fatalf("large output = %#v", output)
	}
}

func TestDocumentReadPropagatesReaderFailureAndCancellation(t *testing.T) {
	want := errors.New("source unavailable")
	retriever := &fakeRetriever{
		resultsByQuery: map[string][]rag.RetrievalResult{
			"x": {
				{DocID: "doc", DocVersion: 1, ChunkID: "chunk", RawContent: "x"},
			},
		},
	}
	reader := &fakeActiveSourceReader{err: want}
	searchTool, readTool := mustKnowledgeAndReadTools(t, retriever, reader, 1, DefaultKnowledgeSearchConfig(), DefaultDocumentReadConfig())
	_ = invokeKnowledgeTool(t, searchTool, `{"query":"x"}`)
	if _, err := readTool.InvokableRun(context.Background(), `{"document_id":"doc","document_version":1,"start_line":1,"end_line":1}`); !errors.Is(err, want) {
		t.Fatalf("reader error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := readTool.InvokableRun(ctx, `{"document_id":"doc","document_version":1,"start_line":1,"end_line":1}`); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error = %v", err)
	}
}

func mustKnowledgeAndReadTools(t *testing.T, retriever Retriever, reader ActiveSourceReader, userID int64, searchConfig KnowledgeSearchConfig, readConfig DocumentReadConfig) (tool.InvokableTool, tool.InvokableTool) {
	t.Helper()
	registry, err := NewKnowledgeRegistry(retriever, reader, searchConfig, readConfig)
	if err != nil {
		t.Fatal(err)
	}
	tools, err := registry.Tools(context.Background(), serviceagent.RunContext{UserID: userID, RunID: "run", SessionID: "session"})
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 2 {
		t.Fatalf("tools = %d, want 2", len(tools))
	}
	search, ok := tools[0].(tool.InvokableTool)
	if !ok {
		t.Fatal("search tool is not invokable")
	}
	read, ok := tools[1].(tool.InvokableTool)
	if !ok {
		t.Fatal("read tool is not invokable")
	}
	return search, read
}

func invokeDocumentRead(t *testing.T, readTool tool.InvokableTool, arguments string) DocumentReadOutput {
	t.Helper()
	raw, err := readTool.InvokableRun(context.Background(), arguments)
	if err != nil {
		t.Fatal(err)
	}
	var output DocumentReadOutput
	if err := json.Unmarshal([]byte(raw), &output); err != nil {
		t.Fatalf("decode output %q: %v", raw, err)
	}
	return output
}

type fakeActiveSourceReader struct {
	mu      sync.Mutex
	source  serviceknowledge.ActiveDocumentSource
	err     error
	userIDs []int64
}

func (f *fakeActiveSourceReader) ReadActiveSource(ctx context.Context, userID int64, documentID string, version int) (serviceknowledge.ActiveDocumentSource, error) {
	if err := ctx.Err(); err != nil {
		return serviceknowledge.ActiveDocumentSource{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.userIDs = append(f.userIDs, userID)
	if f.err != nil {
		return serviceknowledge.ActiveDocumentSource{}, f.err
	}
	if f.source.DocumentID != documentID || f.source.Version != version {
		return serviceknowledge.ActiveDocumentSource{}, serviceknowledge.ErrDocumentVersionNotActive
	}
	return f.source, nil
}

func (f *fakeActiveSourceReader) lastUserID() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.userIDs[len(f.userIDs)-1]
}

func (f *fakeActiveSourceReader) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.userIDs)
}
