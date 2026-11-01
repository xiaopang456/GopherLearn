package validate

import (
	"os"
	"regexp"
	"strings"
)

// LineKind classifies one source line, using the same rules the product's
// Markdown parser uses so that "what is a heading" means the same thing here
// and in the chunker.
type LineKind int

const (
	// LineContent is a line that carries text: a candidate for evidence.
	LineContent LineKind = iota
	// LineBlank is whitespace only.
	LineBlank
	// LineImage is a line that carries nothing but an image link.
	LineImage
	// LineHeading is an ATX heading ("# ..." through "###### ...").
	LineHeading
	// LineRule is a horizontal rule.
	LineRule
	// LineFence is a code fence marker.
	LineFence
)

func (k LineKind) String() string {
	switch k {
	case LineBlank:
		return "blank"
	case LineImage:
		return "image-only"
	case LineHeading:
		return "heading"
	case LineRule:
		return "horizontal-rule"
	case LineFence:
		return "code-fence"
	default:
		return "content"
	}
}

var (
	// A Markdown image alone on a line: ![](url) or ![alt](url).
	mdImageOnly = regexp.MustCompile(`^\s*!\[[^\]]*\]\([^)]*\)\s*$`)
	// An HTML <img> alone on a line.
	htmlImageOnly = regexp.MustCompile(`^\s*<img\b[^>]*>\s*$`)
	// A horizontal rule alone on a line. Go's regexp has no backreferences, so
	// the three markers are spelled out.
	ruleOnly = regexp.MustCompile(`^\s*(?:-{3,}|\*{3,}|_{3,})\s*$`)
	// A code fence marker alone on a line.
	fenceOnly = regexp.MustCompile("^\\s*(`{3,}|~{3,}).*$")
)

// ClassifyLine decides what a source line is.
//
// The heading rule is a copy of the product's parseATXHeading: up to three
// leading spaces, one to six '#', and then whitespace. "##x" is not a heading,
// and neither is a setext underline, because the product does not treat either
// as one.
//
// ClassifyLine looks at one line and nothing else. Use ClassifyLines for a
// whole document: a line inside a fenced code block is code for the product,
// and only ClassifyLines knows where the fences are.
func ClassifyLine(raw string) LineKind {
	line := strings.TrimRight(raw, "\r")
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return LineBlank
	}
	if isATXHeading(line) {
		return LineHeading
	}
	if mdImageOnly.MatchString(line) || htmlImageOnly.MatchString(line) {
		return LineImage
	}
	if ruleOnly.MatchString(line) {
		return LineRule
	}
	if fenceOnly.MatchString(line) {
		return LineFence
	}
	return LineContent
}

// ClassifyLines classifies every line of a document, tracking code fences.
//
// The product's Markdown parser only looks for headings outside fenced code
// blocks: a "# comment" inside a code block is code, not a heading. Classifying
// line by line would call it a heading, and an evidence span that covers only
// a code block would then look like it holds no body text at all.
//
// The fence rules are the product's: up to three leading spaces, at least
// three of the same marker, and the closing fence has to be at least as long as
// the opening one with nothing after it. A fence that is never closed swallows
// the rest of the document, exactly as it does for the product.
func ClassifyLines(lines []string) []LineKind {
	kinds := make([]LineKind, len(lines))
	var open *codeFence
	for i, raw := range lines {
		line := strings.TrimRight(raw, "\r")
		if open != nil {
			if isClosingFence(line, *open) {
				open = nil
				kinds[i] = LineFence
				continue
			}
			kinds[i] = LineContent
			continue
		}
		if fence, ok := parseOpeningFence(line); ok {
			open = &fence
			kinds[i] = LineFence
			continue
		}
		kinds[i] = ClassifyLine(line)
	}
	return kinds
}

// codeFence is an open fenced code block.
type codeFence struct {
	marker   byte
	length   int
	language string
}

// parseOpeningFence copies the product's parseOpeningFence.
func parseOpeningFence(line string) (codeFence, bool) {
	trimmed := trimUpToThreeSpaces(line)
	if len(trimmed) < 3 || (trimmed[0] != '`' && trimmed[0] != '~') {
		return codeFence{}, false
	}
	marker := trimmed[0]
	length := countPrefix(trimmed, marker)
	if length < 3 {
		return codeFence{}, false
	}
	info := strings.TrimSpace(trimmed[length:])
	if marker == '`' && strings.ContainsRune(info, '`') {
		return codeFence{}, false
	}
	language := ""
	if fields := strings.Fields(info); len(fields) > 0 {
		language = fields[0]
	}
	return codeFence{marker: marker, length: length, language: language}, true
}

// isClosingFence copies the product's isClosingFence.
func isClosingFence(line string, opening codeFence) bool {
	trimmed := trimUpToThreeSpaces(line)
	length := countPrefix(trimmed, opening.marker)
	return length >= opening.length && strings.TrimSpace(trimmed[length:]) == ""
}

// countPrefix counts the leading occurrences of one byte.
func countPrefix(value string, marker byte) int {
	count := 0
	for count < len(value) && value[count] == marker {
		count++
	}
	return count
}

func isATXHeading(line string) bool {
	trimmed := trimUpToThreeSpaces(line)
	if trimmed == "" || trimmed[0] != '#' {
		return false
	}
	level := 0
	for level < len(trimmed) && level < 6 && trimmed[level] == '#' {
		level++
	}
	if level == 0 {
		return false
	}
	if level < len(trimmed) && trimmed[level] != ' ' && trimmed[level] != '\t' {
		return false
	}
	return true
}

func trimUpToThreeSpaces(line string) string {
	spaces := 0
	for spaces < len(line) && spaces < 3 && line[spaces] == ' ' {
		spaces++
	}
	return line[spaces:]
}

// Document is one corpus document with its lines classified.
type Document struct {
	File  string
	Lines []string
	Kinds []LineKind
}

// Line returns the text of a 1-based line, or "" when out of range.
func (d *Document) Line(n int) string {
	if n < 1 || n > len(d.Lines) {
		return ""
	}
	return d.Lines[n-1]
}

// Kind returns the classification of a 1-based line.
func (d *Document) Kind(n int) LineKind {
	if n < 1 || n > len(d.Kinds) {
		return LineBlank
	}
	return d.Kinds[n-1]
}

// LoadDocuments reads every corpus document and classifies its lines.
//
// Lines are numbered the way evidence spans number them: 1-based, "\n" ends a
// line, "\r\n" counts as one terminator, and a final line without a terminator
// still counts. That is the same convention the evaluation's dataset loader
// uses, so a line number that passes validation is a line number the run can
// use.
func LoadDocuments(paths map[string]string) (map[string]*Document, error) {
	docs := make(map[string]*Document, len(paths))
	for file, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		lines := splitLines(string(raw))
		doc := &Document{File: file, Lines: make([]string, len(lines)), Kinds: make([]LineKind, len(lines))}
		for i, line := range lines {
			doc.Lines[i] = strings.TrimRight(line, "\r")
		}
		doc.Kinds = ClassifyLines(doc.Lines)
		docs[file] = doc
	}
	return docs, nil
}

// splitLines splits on "\n" and drops the empty tail a trailing newline
// leaves behind, which is how the product's line counter numbers lines.
func splitLines(text string) []string {
	lines := strings.Split(text, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// CountCorpusLines counts the lines of every corpus document, for the
// document-frequency filter the unanswerable check needs.
func CountCorpusLines(docs map[string]*Document) int {
	total := 0
	for _, doc := range docs {
		total += len(doc.Lines)
	}
	return total
}

// scanLines calls fn for every line of every document, with the file and the
// 1-based line number.
func scanLines(docs map[string]*Document, fn func(file string, line int, text string)) {
	for file, doc := range docs {
		for i, text := range doc.Lines {
			fn(file, i+1, text)
		}
	}
}
