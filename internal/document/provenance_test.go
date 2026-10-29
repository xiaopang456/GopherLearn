package document

import (
	"fmt"
	"strings"
	"testing"

	"github.com/xiaopang456/GopherLearn/internal/domain"
)

// TestSplitPartsCiteOnlyTheirOwnLines is the migration oracle for the provenance
// rule v2 implements in common/rag/chunker.go: when one structural unit is too
// large and gets cut, each piece carries the lines it actually holds
// (textPartUnit sets startLine = block.StartLine + start), not the whole unit.
//
// Two properties are checked, and both have to be able to fail:
//
//   - tight: each chunk cites exactly the lines its text was taken from, and no
//     strictly smaller span around them does as well. See assertTight.
//   - covering: every non-blank source line falls inside some chunk's cited
//     range, so a document cannot lose a paragraph just because nothing pointed at
//     it.
func TestSplitPartsCiteOnlyTheirOwnLines(t *testing.T) {
	// Every line is unique and long enough that a split has to cross line
	// boundaries; repeats would make the tightness check ambiguous.
	var source strings.Builder
	source.WriteString("# 持久化\n\n")
	for line := 1; line <= 12; line++ {
		source.WriteString(fmt.Sprintf("第%d段说明：AOF 的 fsync 策略在第%d行上各有取舍，写在这里以便逐行核对。\n", line, line))
	}
	source.WriteString("\n```go\n")
	for line := 1; line <= 10; line++ {
		source.WriteString(fmt.Sprintf("func example%d() { log.Printf(\"第%d行日志，用于确认代码块切片同样逐行对应\") }\n", line, line))
	}
	source.WriteString("```\n")

	// A cap small enough that both a paragraph and a code block have to be cut.
	given := source.String()
	lines := strings.Split(NormalizeText(given), "\n")
	chunks, err := Chunk(given, Options{TargetRunes: 60, MaxRunes: 90})
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) < 6 {
		t.Fatalf("got %d chunks, want the small caps to force several splits", len(chunks))
	}

	cited := make(map[int]bool)
	for _, chunk := range chunks {
		if chunk.StartLine <= 0 || chunk.EndLine < chunk.StartLine || chunk.EndLine > len(lines) {
			t.Fatalf("chunk order %d cites lines %d-%d of a %d-line document", chunk.Order, chunk.StartLine, chunk.EndLine, len(lines))
		}
		assertTight(t, lines, chunk)
		for line := chunk.StartLine; line <= chunk.EndLine; line++ {
			cited[line] = true
		}
	}

	for index, line := range lines {
		if strings.TrimSpace(line) == "" || cited[index+1] {
			continue
		}
		t.Errorf("source line %d (%q) is cited by no chunk; the document lost content silently", index+1, line)
	}
}

// assertTight checks that a chunk cites exactly the lines its text came from.
//
// Each line of the chunk's text has to be found inside one of the cited source
// lines - found, not equalled, because a unit cut mid-way leaves a fragment of a
// source line at each edge - and neither end may be spare: dropping the first
// cited line or the last one has to break that correspondence. The second half is
// what catches over-attribution, which is what the pipeline did before this fix:
// every piece of a cut unit cited the whole unit, and a whole unit contains every
// piece, so a plain containment check would have approved it.
//
// Blank source lines are not compared: a unit's text normalises the blank run
// between a heading and the paragraph under it, exactly as v2 joins its blocks
// with a fixed separator.
func assertTight(t *testing.T, lines []string, chunk domain.DocumentChunk) {
	t.Helper()
	cited := lines[chunk.StartLine-1 : chunk.EndLine]
	if !linesHeldBy(cited, chunk.Text) {
		t.Errorf("chunk order %d cites lines %d-%d but its text is not drawn from them:\ncited: %q\ntext:  %q",
			chunk.Order, chunk.StartLine, chunk.EndLine, cited, chunk.Text)
		return
	}
	if chunk.EndLine == chunk.StartLine {
		return
	}
	if linesHeldBy(cited[1:], chunk.Text) {
		t.Errorf("chunk order %d cites line %d which holds nothing it shows (its text still fits in %d-%d)",
			chunk.Order, chunk.StartLine, chunk.StartLine+1, chunk.EndLine)
	}
	if linesHeldBy(cited[:len(cited)-1], chunk.Text) {
		t.Errorf("chunk order %d cites line %d which holds nothing it shows (its text still fits in %d-%d)",
			chunk.Order, chunk.EndLine, chunk.StartLine, chunk.EndLine-1)
	}
}

// linesHeldBy reports whether every non-blank line of text appears inside one of
// the given source lines.
func linesHeldBy(sourceLines []string, text string) bool {
	for _, piece := range strings.Split(text, "\n") {
		if strings.TrimSpace(piece) == "" {
			continue
		}
		found := false
		for _, line := range sourceLines {
			if strings.Contains(line, piece) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// TestSplitPartsOfOneBlockDoNotShareARange states the same defect in its plainest
// form: before the fix, every piece of a cut unit reported the whole unit, so two
// pieces that hold different text pointed at the same lines.
func TestSplitPartsOfOneBlockDoNotShareARange(t *testing.T) {
	var body strings.Builder
	body.WriteString("# 标题\n\n")
	for line := 1; line <= 8; line++ {
		body.WriteString(fmt.Sprintf("这一行讲的是第%d个要点，长度足够让整段超出切片上限。\n", line))
	}
	given := body.String()
	lines := strings.Split(given, "\n")
	chunks, err := Chunk(given, Options{TargetRunes: 40, MaxRunes: 50})
	if err != nil {
		t.Fatal(err)
	}
	// The document is one paragraph, so any two chunks must hold different text -
	// and therefore must not report an identical line span.
	if len(chunks) < 2 {
		t.Fatalf("got %d chunks, want the paragraph cut at least once", len(chunks))
	}
	seen := map[string][2]int{}
	for _, chunk := range chunks {
		span := [2]int{chunk.StartLine, chunk.EndLine}
		if previous, exists := seen[chunk.Text]; exists {
			t.Fatalf("two chunks with the same text %#v at %v and %v", chunk.Text, previous, span)
		}
		seen[chunk.Text] = span
	}
	// And no chunk may cite the whole document: the first one starts at the heading
	// and stops where its own text stops.
	last := chunks[len(chunks)-1]
	if last.EndLine != len(lines)-1 {
		t.Fatalf("the final chunk ends at line %d, want the document's last text line %d", last.EndLine, len(lines)-1)
	}
}
