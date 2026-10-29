package rag

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParserMarkdownRealNote(t *testing.T) {
	t.Parallel()

	content, err := os.ReadFile(filepath.Join("testdata", "code_note.md"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	blocks := ParseMarkdown(string(content))
	if len(blocks) != 7 {
		t.Fatalf("ParseMarkdown() returned %d blocks, want 7\n%+v", len(blocks), blocks)
	}

	wants := []struct {
		index     int
		typeName  BlockType
		path      []string
		startLine int
		endLine   int
	}{
		{0, BlockTypeHeading, []string{"RabbitMQ"}, 1, 1},
		{1, BlockTypeHeading, []string{"RabbitMQ", "autoAck 的风险"}, 3, 3},
		{2, BlockTypeParagraph, []string{"RabbitMQ", "autoAck 的风险"}, 5, 5},
		{3, BlockTypeCode, []string{"RabbitMQ", "autoAck 的风险"}, 7, 17},
		{4, BlockTypeParagraph, []string{"RabbitMQ", "autoAck 的风险"}, 19, 19},
		{5, BlockTypeHeading, []string{"RabbitMQ", "手动确认"}, 21, 21},
		{6, BlockTypeParagraph, []string{"RabbitMQ", "手动确认"}, 23, 23},
	}

	for _, want := range wants {
		got := blocks[want.index]
		if got.Type != want.typeName || !reflect.DeepEqual(got.TitlePath, want.path) || got.StartLine != want.startLine || got.EndLine != want.endLine {
			t.Errorf("block[%d] = type %q path %v lines %d-%d, want type %q path %v lines %d-%d", want.index, got.Type, got.TitlePath, got.StartLine, got.EndLine, want.typeName, want.path, want.startLine, want.endLine)
		}
	}
	if blocks[3].Language != "go" {
		t.Errorf("code language = %q, want go", blocks[3].Language)
	}
	if strings.Contains(blocks[3].Content, "```") {
		t.Errorf("code content should not contain fences: %q", blocks[3].Content)
	}
	if !strings.HasPrefix(blocks[3].Raw, "```go\n") || !strings.HasSuffix(blocks[3].Raw, "\n```") {
		t.Errorf("code raw does not preserve fences: %q", blocks[3].Raw)
	}
}

func TestParserMarkdownBoundaries(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		input  string
		assert func(*testing.T, []Block)
	}{
		{
			name:  "empty and blank documents",
			input: "\ufeff\r\n\r\n",
			assert: func(t *testing.T, blocks []Block) {
				if len(blocks) != 0 {
					t.Fatalf("got %d blocks, want none", len(blocks))
				}
			},
		},
		{
			name:  "consecutive headings and skipped level",
			input: "# Go\n## Web\n#### Gin\n内容\n## GORM\n",
			assert: func(t *testing.T, blocks []Block) {
				if len(blocks) != 5 {
					t.Fatalf("got %d blocks, want 5", len(blocks))
				}
				if got, want := blocks[3].TitlePath, []string{"Go", "Web", "Gin"}; !reflect.DeepEqual(got, want) {
					t.Errorf("paragraph path = %v, want %v", got, want)
				}
				if got, want := blocks[4].TitlePath, []string{"Go", "GORM"}; !reflect.DeepEqual(got, want) {
					t.Errorf("replacement heading path = %v, want %v", got, want)
				}
			},
		},
		{
			name:  "all ATX heading levels",
			input: "# L1 #\n## L2 ##\n### L3\n#### L4\n##### L5\n###### L6\n",
			assert: func(t *testing.T, blocks []Block) {
				if len(blocks) != 6 {
					t.Fatalf("got %d blocks, want 6", len(blocks))
				}
				for i, block := range blocks {
					if block.Type != BlockTypeHeading || block.Level != i+1 || len(block.TitlePath) != i+1 {
						t.Errorf("heading[%d] = %+v", i, block)
					}
				}
				if blocks[0].Content != "L1" || blocks[1].Content != "L2" {
					t.Errorf("optional closing hashes were not removed: %q, %q", blocks[0].Content, blocks[1].Content)
				}
			},
		},
		{
			name:  "languages and CRLF",
			input: "# 示例\r\n\r\n```sql\r\nSELECT 1;\r\n```\r\n\r\n~~~bash\r\necho ok\r\n~~~\r\n\r\n```\r\nplain\r\n```\r\n",
			assert: func(t *testing.T, blocks []Block) {
				if len(blocks) != 4 {
					t.Fatalf("got %d blocks, want 4", len(blocks))
				}
				for i, want := range []string{"sql", "bash", ""} {
					if got := blocks[i+1].Language; got != want {
						t.Errorf("code block %d language = %q, want %q", i, got, want)
					}
				}
				if blocks[2].StartLine != 7 || blocks[2].EndLine != 9 {
					t.Errorf("bash lines = %d-%d, want 7-9", blocks[2].StartLine, blocks[2].EndLine)
				}
			},
		},
		{
			name:  "unclosed fence consumes through EOF",
			input: "## Go\n```go\nfunc main() {\n}\n",
			assert: func(t *testing.T, blocks []Block) {
				if len(blocks) != 2 {
					t.Fatalf("got %d blocks, want 2", len(blocks))
				}
				got := blocks[1]
				if got.Type != BlockTypeCode || got.Language != "go" || got.StartLine != 2 || got.EndLine != 4 {
					t.Errorf("unclosed code = %+v", got)
				}
				if got.Raw != "```go\nfunc main() {\n}" {
					t.Errorf("raw = %q", got.Raw)
				}
			},
		},
		{
			name:  "hash without separator remains paragraph",
			input: "#not-a-heading\ntext\n",
			assert: func(t *testing.T, blocks []Block) {
				if len(blocks) != 1 || blocks[0].Type != BlockTypeParagraph || blocks[0].StartLine != 1 || blocks[0].EndLine != 2 {
					t.Fatalf("got %+v, want one two-line paragraph", blocks)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.assert(t, ParseMarkdown(tt.input))
		})
	}
}

func TestParserTextParagraphs(t *testing.T) {
	t.Parallel()

	blocks := ParseText("第一段第一行\r\n第二行\r\n\r\n# TXT 中不是标题\r\n\r\n```go\r\n不是代码块\r\n```\r\n")
	if len(blocks) != 3 {
		t.Fatalf("ParseText() returned %d blocks, want 3: %+v", len(blocks), blocks)
	}
	if blocks[0].StartLine != 1 || blocks[0].EndLine != 2 {
		t.Errorf("first paragraph lines = %d-%d, want 1-2", blocks[0].StartLine, blocks[0].EndLine)
	}
	for i, block := range blocks {
		if block.Type != BlockTypeParagraph || block.Level != 0 || block.Language != "" || len(block.TitlePath) != 0 {
			t.Errorf("block[%d] unexpectedly has Markdown structure: %+v", i, block)
		}
	}
	if blocks[1].Content != "# TXT 中不是标题" {
		t.Errorf("TXT Markdown-looking content changed: %q", blocks[1].Content)
	}
	if blocks[2].StartLine != 6 || blocks[2].EndLine != 8 {
		t.Errorf("third paragraph lines = %d-%d, want 6-8", blocks[2].StartLine, blocks[2].EndLine)
	}
}

func TestParserReturnsIndependentTitlePaths(t *testing.T) {
	t.Parallel()

	blocks := ParseMarkdown("# A\nfirst\n## B\nsecond\n")
	blocks[1].TitlePath[0] = "changed"
	if blocks[0].TitlePath[0] != "A" || blocks[2].TitlePath[0] != "A" || blocks[3].TitlePath[0] != "A" {
		t.Fatalf("title paths share mutable backing storage: %+v", blocks)
	}
}
