package document

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// The four tests below exist because v2 folds a document onto its text before it
// parses or hashes anything (common/rag/parser.go splitDocumentLines,
// service/knowledge/storage.go NormalizeContent), and this pipeline only did half
// of that: line endings yes, byte-order mark no.

func TestByteOrderMarkDoesNotCostTheFirstHeadingItsStructure(t *testing.T) {
	const chineseBody = "# Redis 持久化\n\nAOF 的 fsync 策略可以选 everysec。\n"
	const englishBody = "# Redis persistence\n\nAOF fsync can be always, everysec or no.\n"
	cases := map[string]string{
		"chinese": chineseBody,
		"english": englishBody,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			given := bomMarker + body
			chunks, err := Chunk(given, DefaultOptions())
			if err != nil {
				t.Fatal(err)
			}
			if len(chunks) == 0 {
				t.Fatal("a document that starts with a byte-order mark produced no chunks")
			}
			// The mark is not content: without it the first line is a title, and a
			// title is what every chunk under it is filed under.
			if chunks[0].Section == "" {
				t.Fatalf("section = %q, want the first heading to still be a heading: %#v", chunks[0].Section, chunks[0])
			}
			for index, chunk := range chunks {
				if strings.Contains(chunk.Text, bomMarker) {
					t.Errorf("chunk %d carries the byte-order mark as text: %q", index, chunk.Text)
				}
			}
			// Same document, one editor with a mark and one without: the chunks have
			// to be indistinguishable, or the mark silently changes what is indexed.
			without, err := Chunk(body, DefaultOptions())
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(chunks, without) {
				t.Errorf("chunks differ only because of the mark:\n with = %#v\nwithout = %#v", chunks, without)
			}
		})
	}
}

func TestFileThatIsOnlyAByteOrderMarkIsAnEmptyDocument(t *testing.T) {
	if _, err := Chunk(bomMarker, DefaultOptions()); !errors.Is(err, ErrEmptyDocument) {
		t.Fatalf("a file containing nothing but the mark = %v, want it treated as empty", err)
	}
	if _, err := Chunk(bomMarker+"\r\n", DefaultOptions()); !errors.Is(err, ErrEmptyDocument) {
		t.Fatalf("the mark plus a line ending = %v, want it treated as empty", err)
	}
	// The same refusal must still cover the plain cases this pipeline already had.
	for _, blank := range []string{"", "   \n\t\n", "\r\n\r\n"} {
		if _, err := Chunk(blank, DefaultOptions()); !errors.Is(err, ErrEmptyDocument) {
			t.Errorf("blank document %q = %v, want ErrEmptyDocument", blank, err)
		}
	}
}

func TestLineEndingsDoNotMoveTheTextOrTheRanges(t *testing.T) {
	const unix = "# 标题\n\n第一段。\n\n第二段。\n\n```go\nfmt.Println(\"行\")\n```\n"
	windows := strings.ReplaceAll(unix, "\n", "\r\n")
	classic := strings.ReplaceAll(unix, "\n", "\r")
	bomWindows := bomMarker + windows

	base, err := Chunk(unix, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	for name, given := range map[string]string{"crlf": windows, "lone cr": classic, "bom plus crlf": bomWindows} {
		t.Run(name, func(t *testing.T) {
			got, err := Chunk(given, DefaultOptions())
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, base) {
				t.Errorf("%s chunks = %#v, want them identical to the LF ones: %#v", name, got, base)
			}
			for index, chunk := range got {
				if strings.ContainsAny(chunk.Text, "\r") {
					t.Errorf("chunk %d still carries a carriage return: %q", index, chunk.Text)
				}
			}
		})
	}
}

func TestInteriorByteOrderMarkStaysAsText(t *testing.T) {
	// v2 trims the mark from the head of the document only. A mark in the middle is
	// a character the author put there, and dropping it would make a chunk quote
	// text that is not in the source at that position.
	given := "第一段。\n\n" + bomMarker + "第二段。\n"
	chunks, err := Chunk(given, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, chunk := range chunks {
		if strings.Contains(chunk.Text, bomMarker) {
			found = true
		}
	}
	if !found {
		t.Fatalf("no chunk kept the interior mark: %#v", chunks)
	}
}
