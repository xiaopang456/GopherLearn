package document

import (
	"encoding/hex"
	"strings"
	"testing"
)

// bomMarker is the byte-order mark spelled as bytes, so this file needs no
// invisible character to test for one.
var bomMarker = string([]byte{0xEF, 0xBB, 0xBF})

func TestNormalizeTextFoldsEndingsAndLeadsOfFiles(t *testing.T) {
	cases := []struct {
		name  string
		given string
		want  string
	}{
		{name: "lf passes through", given: "第一行\n第二行\n", want: "第一行\n第二行\n"},
		{name: "crlf folds to lf", given: "first\r\nsecond\r\n", want: "first\nsecond\n"},
		{name: "lone cr folds to lf", given: "first\rsecond\r", want: "first\nsecond\n"},
		{name: "mixed endings fold alike", given: "a\r\nb\nc\rd", want: "a\nb\nc\nd"},
		{name: "leading bom is dropped", given: bomMarker + "# 标题\n", want: "# 标题\n"},
		{name: "bom plus crlf", given: bomMarker + "# Title\r\nbody\r\n", want: "# Title\nbody\n"},
		// v2 trims the mark from the head of the file only. A mark in the middle is
		// content the author can see in some editors, and silently deleting it would
		// make a chunk quote text that is not in the source.
		{name: "an interior bom stays", given: "a\n" + bomMarker + "b\n", want: "a\n" + bomMarker + "b\n"},
		{name: "an empty file stays empty", given: "", want: ""},
		{name: "a file that is only a bom becomes empty", given: bomMarker, want: ""},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			if got := NormalizeText(item.given); got != item.want {
				t.Errorf("NormalizeText(%q) = %q, want %q", item.given, got, item.want)
			}
		})
	}
}

func TestContentHashIdentifiesContentNotEncoding(t *testing.T) {
	const body = "# Redis 持久化\n\nAOF everysec。\n"
	windows := bomMarker + strings.ReplaceAll(body, "\n", "\r\n")
	unix := body
	linuxWithBom := bomMarker + body

	if hash, other := ContentHash([]byte(unix)), ContentHash([]byte(windows)); hash != other {
		t.Errorf("line endings changed the hash (%s vs %s), want one document", hash, other)
	}
	if ContentHash([]byte(linuxWithBom)) != ContentHash([]byte(unix)) {
		t.Error("the byte-order mark changed the hash, want it ignored")
	}
	// What the hash is for: refusing a second copy, and recognising a rewrite that
	// reproduced the old bytes. A trailing space is a content change to a reader
	// only in the loosest sense, but it is not what this identifies - a different
	// sentence has to hash differently, or an update would be reported as no change
	// and never indexed.
	if ContentHash([]byte(body+"补充一段。")) == ContentHash([]byte(unix)) {
		t.Error("different content produced the same hash")
	}
	if got := ContentHash([]byte(unix)); len(got) != 64 {
		t.Errorf("hash %q is %d characters, want a 64-character hex digest", got, len(got))
	} else if _, err := hex.DecodeString(got); err != nil {
		t.Errorf("hash %q is not hex: %v", got, err)
	}
	// The size the quota spends is deliberately not this number: the hash is over
	// normalized text, the byte count is what the client sent.
	if int64(len(unix)) == int64(len(windows)) {
		t.Fatal("the fixture has no raw-size difference to demonstrate")
	}
}
