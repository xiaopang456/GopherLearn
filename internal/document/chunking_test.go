package document

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/xiaopang456/GopherLearn/internal/domain"
)

func TestChunkRejectsEmptyContent(t *testing.T) {
	for _, value := range []string{"", "   ", "\n\n \t\n"} {
		if _, err := Chunk(value, DefaultOptions()); !errors.Is(err, ErrEmptyDocument) {
			t.Errorf("content %q error = %v, want ErrEmptyDocument", value, err)
		}
	}
}

func TestChunkKeepsHeadingWithItsBody(t *testing.T) {
	text := "# 标题\n\n第一段。\n\n## 小节\n\n第二段。\n"
	chunks, err := Chunk(text, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 2 {
		t.Fatalf("chunks = %d, want one per section: %#v", len(chunks), chunks)
	}
	if chunks[0].Section != "标题" || !strings.HasPrefix(chunks[0].Text, "# 标题") {
		t.Fatalf("first chunk lost its heading: %#v", chunks[0])
	}
	if chunks[1].Section != "标题 / 小节" || !strings.HasPrefix(chunks[1].Text, "## 小节") {
		t.Fatalf("nested section path or heading lost: %#v", chunks[1])
	}
	for index, chunk := range chunks {
		if chunk.Order != index+1 {
			t.Errorf("chunk %d has order %d", index, chunk.Order)
		}
		if chunk.StartLine <= 0 || chunk.EndLine < chunk.StartLine {
			t.Errorf("chunk %d has an impossible line span %#v", index, chunk)
		}
	}
}

func TestChunkHoldingDeeperHeadingLevelsResetsThePath(t *testing.T) {
	text := "# A\n\none\n\n### C\n\ntwo\n\n# B\n\nthree\n"
	chunks, err := Chunk(text, Options{TargetRunes: 40, MaxRunes: 60})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"A", "A / C", "B"}
	got := make([]string, 0, len(chunks))
	for _, chunk := range chunks {
		got = append(got, chunk.Section)
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("section paths = %v, want %v (full chunks %#v)", got, want, chunks)
	}
}

func TestChunkKeepsCodeBlockWholeAndSeparate(t *testing.T) {
	text := "说明文字。\n\n```go\nfunc main() {\n\tprintln(\"hi\")\n}\n```\n\n收尾文字。\n"
	chunks, err := Chunk(text, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	var code *domain.DocumentChunk
	for index := range chunks {
		if strings.Contains(chunks[index].Text, "func main()") {
			code = &chunks[index]
		}
	}
	if code == nil {
		t.Fatalf("code block was lost: %#v", chunks)
	}
	if strings.Contains(code.Text, "说明文字") || strings.Contains(code.Text, "收尾文字") {
		t.Fatalf("code block merged with prose: %q", code.Text)
	}
	if !strings.HasPrefix(code.Text, "```go") || !strings.HasSuffix(code.Text, "```") {
		t.Fatalf("code block lost its fences: %q", code.Text)
	}
}

func TestChunkSplitsOnlyOversizedUnits(t *testing.T) {
	longCode := "```python\n" + strings.Repeat("print(1)\n", 40) + "```"
	chunks, err := Chunk(longCode, Options{TargetRunes: 60, MaxRunes: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) < 2 {
		t.Fatalf("an oversized code block produced %d chunks, want it split", len(chunks))
	}
	joined := strings.Join(chunksOf(chunks), "")
	if !strings.Contains(joined, "print(1)") {
		t.Fatal("split dropped code content")
	}
	for _, chunk := range chunks {
		if utf8.RuneCountInString(chunk.Text) > 100 {
			t.Fatalf("chunk exceeds the cap: %d runes", utf8.RuneCountInString(chunk.Text))
		}
	}
	// Every split piece is still whole lines, so a reader never sees half a call.
	for _, chunk := range chunks {
		for _, line := range strings.Split(chunk.Text, "\n") {
			if line != "" && !strings.HasPrefix(line, "print(1") && line != "```python" && line != "```" {
				t.Fatalf("split cut a line in half: %q", line)
			}
		}
	}

	paragraph := strings.Repeat("长", 500)
	pieces, err := Chunk(paragraph, Options{TargetRunes: 120, MaxRunes: 200})
	if err != nil {
		t.Fatal(err)
	}
	if len(pieces) != 3 {
		t.Fatalf("a 500-rune paragraph split into %d pieces at cap 200, want 3", len(pieces))
	}
	if strings.Join(chunksOf(pieces), "") != paragraph {
		t.Fatal("paragraph split lost or reordered content")
	}

	// A small document must not be chopped up at all.
	small, err := Chunk("# 标题\n\n很短的一段。\n", DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if len(small) != 1 {
		t.Fatalf("a short document became %d chunks, want 1", len(small))
	}
}

func TestChunkAcceptsChineseAndMixedText(t *testing.T) {
	text := "# Redis 持久化\n\nAOF 以追加写记录命令，fsync 可选 everysec。\n\nRDB 通过 fork 生成快照。\n"
	chunks, err := Chunk(text, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 1 {
		t.Fatalf("chunks = %d, want the two paragraphs merged inside one section", len(chunks))
	}
	if chunks[0].Section != "Redis 持久化" {
		t.Fatalf("section = %q", chunks[0].Section)
	}
	for _, want := range []string{"AOF", "fsync", "everysec", "fork", "快照"} {
		if !strings.Contains(chunks[0].Text, want) {
			t.Fatalf("merged chunk dropped %q: %q", want, chunks[0].Text)
		}
	}
}

func TestChunkHonoursExplicitLineEndingsAndFenceLength(t *testing.T) {
	crlf := strings.ReplaceAll("# 标题\r\n\r\n正文。\r\n", "\r\n", "\n")
	if _, err := Chunk(crlf, DefaultOptions()); err != nil {
		t.Fatal(err)
	}
	windows, err := Chunk("# 标题\r\n\r\n正文。\r\n", DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if len(windows) != 1 || !strings.Contains(windows[0].Text, "正文") {
		t.Fatalf("CRLF input chunked wrong: %#v", windows)
	}

	// A longer fence must not be closed by a shorter run inside the block.
	nested := "````md\n```sh\necho hi\n```\nclosing fence only at the end\n````\n"
	nestedChunks, err := Chunk(nested, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if len(nestedChunks) != 1 || !strings.Contains(nestedChunks[0].Text, "closing fence only at the end") {
		t.Fatalf("nested fence broke the block apart: %#v", nestedChunks)
	}
}

func TestChunkRejectsOversizedContent(t *testing.T) {
	oversized := strings.Repeat("a", MaximumContentBytes+1)
	if _, err := Chunk(oversized, DefaultOptions()); !errors.Is(err, ErrContentTooLarge) {
		t.Fatalf("error = %v, want ErrContentTooLarge", err)
	}
}

func TestOutlineMatchesChunkerUnits(t *testing.T) {
	text := "# A\n\nbody\n\n# B\n\nmore\n"
	units := Outline(text)
	chunks, err := Chunk(text, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if len(units) != len(chunks) {
		t.Fatalf("outline reported %d units but chunking produced %d", len(units), len(chunks))
	}
}

func chunksOf(chunks []domain.DocumentChunk) []string {
	values := make([]string, 0, len(chunks))
	for _, chunk := range chunks {
		values = append(values, chunk.Text)
	}
	return values
}
