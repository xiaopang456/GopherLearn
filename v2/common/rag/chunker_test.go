package rag

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestChunkRealNoteKeepsExplanationAndCodeTogether(t *testing.T) {
	t.Parallel()

	content, err := os.ReadFile(filepath.Join("testdata", "code_note.md"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	chunker := mustChunker(t, DefaultChunkerConfig())
	chunks, err := chunker.Chunk(ChunkInput{
		DocID: "doc-rabbit", DocVersion: 1, SourceFile: "rabbitmq.md",
		Blocks: ParseMarkdown(string(content)),
	})
	if err != nil {
		t.Fatalf("Chunk() error: %v", err)
	}
	if len(chunks) != 2 {
		t.Fatalf("Chunk() returned %d chunks, want 2: %+v", len(chunks), chunks)
	}

	first := chunks[0]
	if first.Type != ChunkTypeMixed || first.CodeLanguage != "go" {
		t.Errorf("first chunk type/language = %q/%q, want mixed/go", first.Type, first.CodeLanguage)
	}
	if !reflect.DeepEqual(first.TitlePath, []string{"RabbitMQ", "autoAck 的风险"}) {
		t.Errorf("first title path = %v", first.TitlePath)
	}
	for _, want := range []string{"收货回执", "channel.Consume", "autoAck=true"} {
		if !strings.Contains(first.Content, want) {
			t.Errorf("first chunk does not keep related explanation/code/conclusion %q", want)
		}
	}
	if strings.Count(first.RawContent, "```go") != 1 || strings.Count(first.RawContent, "```") != 2 {
		t.Errorf("normal code fence was split or damaged:\n%s", first.RawContent)
	}
	if first.StartLine != 1 || first.EndLine != 19 || first.TokenEstimate > 512 {
		t.Errorf("first lines/tokens = %d-%d/%d", first.StartLine, first.EndLine, first.TokenEstimate)
	}
	if !strings.Contains(first.EmbeddingContent, "文件: rabbitmq.md") ||
		!strings.Contains(first.EmbeddingContent, "标题: RabbitMQ > autoAck 的风险") ||
		!strings.Contains(first.EmbeddingContent, "channel.Consume") {
		t.Errorf("embedding content misses retrieval context:\n%s", first.EmbeddingContent)
	}
	if chunks[1].Type != ChunkTypeProse || !reflect.DeepEqual(chunks[1].TitlePath, []string{"RabbitMQ", "手动确认"}) {
		t.Errorf("second chunk = %+v", chunks[1])
	}
}

func TestChunkLongCodeUsesLineBoundariesAndOverlap(t *testing.T) {
	t.Parallel()

	codeLines := make([]string, 24)
	for i := range codeLines {
		codeLines[i] = fmt.Sprintf("result%02d := service.Call(ctx, request%02d)", i, i)
	}
	markdown := "# Client\n\n```go\n" + strings.Join(codeLines, "\n") + "\n```\n"
	config := ChunkerConfig{MaxTokens: 130, OverlapTokens: 25, MaxChunks: 100}
	chunker := mustChunker(t, config)
	chunks, err := chunker.Chunk(ChunkInput{
		DocID: "doc-code", DocVersion: 3, SourceFile: "client.md", Blocks: ParseMarkdown(markdown),
	})
	if err != nil {
		t.Fatalf("Chunk() error: %v", err)
	}
	if len(chunks) < 2 {
		t.Fatalf("long code returned %d chunk, want multiple", len(chunks))
	}

	known := make(map[string]struct{}, len(codeLines))
	for _, line := range codeLines {
		known[line] = struct{}{}
	}
	for i, chunk := range chunks {
		if chunk.Type != ChunkTypeCode || chunk.CodeLanguage != "go" {
			t.Errorf("chunk[%d] type/language = %q/%q", i, chunk.Type, chunk.CodeLanguage)
		}
		if chunk.TokenEstimate > config.MaxTokens {
			t.Errorf("chunk[%d] estimate = %d, max %d", i, chunk.TokenEstimate, config.MaxTokens)
		}
		if chunk.PartIndex != i+1 || chunk.PartTotal != len(chunks) {
			t.Errorf("chunk[%d] part = %d/%d, want %d/%d", i, chunk.PartIndex, chunk.PartTotal, i+1, len(chunks))
		}
		for _, line := range chunkCodeLines(chunk) {
			if _, ok := known[line]; !ok {
				t.Errorf("chunk[%d] cut inside a source line: %q", i, line)
			}
		}
	}
	if !chunksShareContentLine(chunks[0], chunks[1]) {
		t.Errorf("adjacent code chunks do not contain configured line overlap")
	}
	if chunks[0].StartLine != 3 || chunks[len(chunks)-1].EndLine != 28 {
		t.Errorf("split source range = %d-%d, want 3-28", chunks[0].StartLine, chunks[len(chunks)-1].EndLine)
	}
}

func TestChunkOversizedCodeLineReturnsExplicitError(t *testing.T) {
	t.Parallel()

	chunker := mustChunker(t, ChunkerConfig{MaxTokens: 50, OverlapTokens: 5, MaxChunks: 10})
	_, err := chunker.Chunk(ChunkInput{
		DocID: "doc-minified", DocVersion: 1, SourceFile: "generated.go",
		Blocks: ParseMarkdown("```go\n" + strings.Repeat("identifier", 100) + "\n```\n"),
	})
	if !errors.Is(err, ErrCodeLineTooLong) {
		t.Fatalf("Chunk() error = %v, want ErrCodeLineTooLong", err)
	}
}

func TestChunkLongTextHasHardBudgetAndPartMetadata(t *testing.T) {
	t.Parallel()

	chunker := mustChunker(t, ChunkerConfig{MaxTokens: 55, OverlapTokens: 5, MaxChunks: 100})
	chunks, err := chunker.Chunk(ChunkInput{
		DocID: "doc-text", DocVersion: 1, SourceFile: "redis.txt",
		Blocks: ParseText(strings.Repeat("这是我对Redis过期策略的个人理解。", 30)),
	})
	if err != nil {
		t.Fatalf("Chunk() error: %v", err)
	}
	if len(chunks) < 2 {
		t.Fatalf("long text returned %d chunk, want multiple", len(chunks))
	}
	for i, chunk := range chunks {
		if chunk.TokenEstimate > 55 || chunk.PartIndex != i+1 || chunk.PartTotal != len(chunks) {
			t.Errorf("chunk[%d] tokens/part = %d %d/%d", i, chunk.TokenEstimate, chunk.PartIndex, chunk.PartTotal)
		}
		if chunk.StartLine != 1 || chunk.EndLine != 1 {
			t.Errorf("split single-line text lost source line: %d-%d", chunk.StartLine, chunk.EndLine)
		}
	}
}

func TestChunkStableIDsAndHashes(t *testing.T) {
	t.Parallel()

	input := ChunkInput{
		DocID: "doc-stable", DocVersion: 2, SourceFile: "gorm.md",
		Blocks: ParseMarkdown("# GORM\n\n## 查询\n\n使用 `First` 查询第一条记录。\n"),
	}
	config := DefaultChunkerConfig()
	chunker := mustChunker(t, config)
	first, err := chunker.Chunk(input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := chunker.Chunk(input)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("same input/config produced different chunks\nfirst: %+v\nsecond: %+v", first, second)
	}
	if len(first) != 1 || len(first[0].ID) != len("chunk_")+32 || len(first[0].ContentHash) != 64 {
		t.Fatalf("unexpected stable identity: %+v", first)
	}

	changed := config
	changed.MaxTokens++
	other, err := mustChunker(t, changed).Chunk(input)
	if err != nil {
		t.Fatal(err)
	}
	if other[0].ID == first[0].ID {
		t.Errorf("configuration change should change stable ID namespace")
	}
}

func TestChunkRejectsInvalidConfigInputAndCount(t *testing.T) {
	t.Parallel()

	badConfigs := []ChunkerConfig{
		{MaxTokens: 0, OverlapTokens: 0, MaxChunks: 1},
		{MaxTokens: 10, OverlapTokens: 10, MaxChunks: 1},
		{MaxTokens: 10, OverlapTokens: 1, MaxChunks: 0},
	}
	for _, config := range badConfigs {
		if _, err := NewChunker(config); !errors.Is(err, ErrInvalidChunkerConfig) {
			t.Errorf("NewChunker(%+v) error = %v", config, err)
		}
	}

	chunker := mustChunker(t, ChunkerConfig{MaxTokens: 100, OverlapTokens: 5, MaxChunks: 1})
	_, err := chunker.Chunk(ChunkInput{
		DocID: "doc-limit", DocVersion: 1, SourceFile: "note.md",
		Blocks: ParseMarkdown("# A\ntext\n# B\ntext\n"),
	})
	if !errors.Is(err, ErrTooManyChunks) {
		t.Fatalf("Chunk() error = %v, want ErrTooManyChunks", err)
	}
	_, err = chunker.Chunk(ChunkInput{DocVersion: 1, SourceFile: "note.md"})
	if !errors.Is(err, ErrInvalidChunkInput) {
		t.Fatalf("invalid input error = %v", err)
	}
}

func mustChunker(t *testing.T, config ChunkerConfig) *Chunker {
	t.Helper()
	chunker, err := NewChunker(config)
	if err != nil {
		t.Fatalf("NewChunker() error: %v", err)
	}
	return chunker
}

func chunksShareContentLine(left, right Chunk) bool {
	seen := make(map[string]struct{})
	for _, line := range chunkCodeLines(left) {
		seen[line] = struct{}{}
	}
	for _, line := range chunkCodeLines(right) {
		if _, ok := seen[line]; ok {
			return true
		}
	}
	return false
}

func chunkCodeLines(chunk Chunk) []string {
	lines := strings.Split(chunk.Content, "\n")
	if len(lines) >= 2 && strings.HasPrefix(lines[0], "```") && lines[len(lines)-1] == "```" {
		return lines[1 : len(lines)-1]
	}
	return lines
}
