package document

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// bom is the UTF-8 byte-order mark: the three bytes an editor on Windows puts at
// the head of a file. It is invisible in the text but not in the bytes, so left
// in place it stops a first line "# Title" from parsing as a title and makes the
// same notes hash differently depending on which editor saved them. It is built
// from its code point here rather than written as a literal, so that no invisible
// character ever enters this source file.
var bom = string(rune(0xFEFF))

// NormalizeText folds a document onto the text the pipeline indexes and compares:
// the BOM removed, CRLF and a lone CR turned into LF. Nothing else changes, so
// what a chunk quotes is still what the author wrote.
func NormalizeText(text string) string {
	text = strings.TrimPrefix(text, bom)
	text = strings.ReplaceAll(text, "\r\n", "\n")
	return strings.ReplaceAll(text, "\r", "\n")
}

// NormalizeContent is NormalizeText for bytes.
func NormalizeContent(content []byte) []byte {
	return []byte(NormalizeText(string(content)))
}

// ContentHash identifies a document by what it says. Two uploads that differ only
// in a BOM or in line endings are the same document to a reader, so the hash is
// taken over the normalized text - which is also what lets a rewrite that
// reproduced the old bytes be recognised without re-chunking it.
func ContentHash(content []byte) string {
	sum := sha256.Sum256(NormalizeContent(content))
	return hex.EncodeToString(sum[:])
}
