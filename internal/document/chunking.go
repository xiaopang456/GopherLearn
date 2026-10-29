// Package document turns uploaded Markdown or plain text into retrievable
// chunks. The split follows the document's own structure - headings, paragraphs
// and fenced code blocks - rather than a global fixed token window, because a
// window that ignores structure routinely cuts a table or a code block in half
// and then answers from a fragment that no longer means anything.
package document

import (
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/xiaopang456/GopherLearn/internal/domain"
)

var (
	// ErrEmptyDocument reports content with nothing to index.
	ErrEmptyDocument = errors.New("document: content is empty")
	// ErrContentTooLarge reports content beyond the ingestion cap.
	ErrContentTooLarge = errors.New("document: content exceeds the ingestion limit")
)

// Options bounds the chunker. TargetRunes groups small paragraphs together up
// to that size; MaxRunes is the hard cap after which an oversized unit - a long
// paragraph or a long code block - is split further.
type Options struct {
	TargetRunes int
	MaxRunes    int
}

// MaximumContentBytes bounds one stored document version.
const MaximumContentBytes = 2 << 20

func DefaultOptions() Options {
	return Options{TargetRunes: 700, MaxRunes: 1200}
}

func (o Options) withDefaults() Options {
	if o.TargetRunes <= 0 {
		o.TargetRunes = DefaultOptions().TargetRunes
	}
	if o.MaxRunes <= 0 {
		o.MaxRunes = DefaultOptions().MaxRunes
	}
	if o.MaxRunes < o.TargetRunes {
		o.MaxRunes = o.TargetRunes
	}
	return o
}

// Unit is one structural piece of the document before it becomes a chunk: the
// section it sits in, its verbatim text and the 1-based line span it covered.
type Unit struct {
	Section   string
	Text      string
	StartLine int
	EndLine   int
}

// Outline reports the document's structure without chunking it. The API surface
// uses it to confirm what was uploaded, and it doubles as the chunker's own
// diagnostic when a document does not split the way an author expects.
func Outline(text string) []Unit {
	units, _ := units(text, DefaultOptions())
	return units
}

// Chunk splits content into chunks ordered by appearance. Chunk ids are left
// empty here: identity and version numbering belong to the store, which can see
// the document the chunks are about to be filed under.
func Chunk(text string, opts Options) ([]domain.DocumentChunk, error) {
	parsed, err := units(text, opts)
	if err != nil {
		return nil, err
	}
	chunks := make([]domain.DocumentChunk, 0, len(parsed))
	for index, unit := range parsed {
		chunks = append(chunks, domain.DocumentChunk{
			Order:     index + 1,
			Section:   unit.Section,
			Text:      unit.Text,
			StartLine: unit.StartLine,
			EndLine:   unit.EndLine,
		})
	}
	return chunks, nil
}

const (
	kindParagraph = iota
	kindCode
)

type region struct {
	kind int
	// headingOnly marks a region that has collected heading lines but no body
	// yet. A heading must never become a chunk of its own, so such a region
	// survives a blank line and keeps collecting.
	headingOnly bool
	section     string
	lines       []string
	// numbers is the source line of each entry of lines. A region skips blank
	// lines while it collects, so the position of a line inside the region's text
	// is not its number in the document, and a provenance range built from the
	// offset alone would cite the wrong lines.
	numbers   []int
	startLine int
	endLine   int
}

func units(text string, opts Options) ([]Unit, error) {
	options := opts.withDefaults()
	normalized := NormalizeText(text)
	if strings.TrimSpace(normalized) == "" {
		return nil, ErrEmptyDocument
	}
	if len(normalized) > MaximumContentBytes {
		return nil, ErrContentTooLarge
	}
	lines := strings.Split(normalized, "\n")

	var (
		units       []Unit
		pending     *region
		heading     []headingLevel
		inCode      bool
		fence       string
		lastWasCode bool
	)
	flush := func() {
		if pending == nil {
			return
		}
		body := strings.TrimSpace(strings.Join(pending.lines, "\n"))
		start, end, section, kind, numbers := pending.startLine, pending.endLine, pending.section, pending.kind, pending.numbers
		pending = nil
		if body == "" {
			return
		}
		switch {
		case kind == kindCode:
			// A code block is one unit, or several capped units that keep whole
			// lines; it is never merged with prose around it.
			units = append(units, splitCode(section, numbers, strings.Split(body, "\n"), options.MaxRunes)...)
			lastWasCode = true
		case utf8.RuneCountInString(body) > options.MaxRunes:
			units = append(units, splitParagraph(section, numbers, body, options.MaxRunes)...)
			lastWasCode = false
		case !lastWasCode && len(units) > 0 && units[len(units)-1].Section == section &&
			utf8.RuneCountInString(units[len(units)-1].Text)+utf8.RuneCountInString(body) <= options.TargetRunes:
			// Consecutive short paragraphs in one section merge into a single
			// unit, so a document of one-line paragraphs does not become one
			// chunk each. Blank lines between them are fine; an intervening code
			// block is not, because it set lastWasCode.
			last := &units[len(units)-1]
			last.Text = last.Text + "\n\n" + body
			last.EndLine = end
		default:
			units = append(units, Unit{Section: section, Text: body, StartLine: start, EndLine: end})
			lastWasCode = false
		}
	}

	for index, line := range lines {
		lineNumber := index + 1
		if inCode {
			pending.lines = append(pending.lines, line)
			pending.numbers = append(pending.numbers, lineNumber)
			pending.endLine = lineNumber
			if strings.HasPrefix(strings.TrimSpace(line), fence) && strings.TrimSpace(line) != "" {
				inCode = false
				fence = ""
				flush()
			}
			continue
		}
		if marker, opening := fenceMarker(line); opening {
			flush()
			pending = &region{kind: kindCode, section: sectionPath(heading), lines: []string{line},
				numbers: []int{lineNumber}, startLine: lineNumber, endLine: lineNumber}
			inCode = true
			fence = marker
			continue
		}
		if level, title, isHeading := headingLevelOf(line); isHeading {
			flush()
			heading = pushHeading(heading, level, title)
			pending = &region{kind: kindParagraph, section: sectionPath(heading), lines: []string{line},
				numbers: []int{lineNumber}, startLine: lineNumber, endLine: lineNumber, headingOnly: true}
			continue
		}
		if strings.TrimSpace(line) == "" {
			if pending == nil || !pending.headingOnly {
				flush()
			}
			continue
		}
		if pending == nil {
			pending = &region{kind: kindParagraph, section: sectionPath(heading), startLine: lineNumber}
		}
		pending.lines = append(pending.lines, line)
		pending.numbers = append(pending.numbers, lineNumber)
		pending.endLine = lineNumber
		pending.headingOnly = false
	}
	flush()
	if len(units) == 0 {
		return nil, ErrEmptyDocument
	}
	return units, nil
}

type headingLevel struct {
	level int
	title string
}

func pushHeading(stack []headingLevel, level int, title string) []headingLevel {
	trimmed := make([]headingLevel, 0, len(stack)+1)
	for _, item := range stack {
		if item.level < level {
			trimmed = append(trimmed, item)
		}
	}
	return append(trimmed, headingLevel{level: level, title: title})
}

func sectionPath(stack []headingLevel) string {
	parts := make([]string, 0, len(stack))
	for _, item := range stack {
		parts = append(parts, item.title)
	}
	return strings.Join(parts, " / ")
}

func headingLevelOf(line string) (int, string, bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "#") {
		return 0, "", false
	}
	level := 0
	for level < len(trimmed) && trimmed[level] == '#' {
		level++
	}
	if level > 6 || level == len(trimmed) {
		return 0, "", false
	}
	if trimmed[level] != ' ' && trimmed[level] != '\t' {
		return 0, "", false
	}
	title := strings.TrimSpace(trimmed[level:])
	if title == "" {
		return 0, "", false
	}
	return level, title, true
}

// fenceMarker returns the opening marker of a fenced code block. The marker is
// kept so a nested run of backticks shorter than the fence does not close it.
// fenceMarker returns the opening marker of a fenced code block, measured as the
// whole leading run of the fence character. A four-backtick fence must not be
// recorded as three, or a nested ```sh line inside it would close the block.
func fenceMarker(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return "", false
	}
	character := trimmed[0]
	if character != '`' && character != '~' {
		return "", false
	}
	run := 0
	for run < len(trimmed) && trimmed[run] == character {
		run++
	}
	if run < 3 {
		return "", false
	}
	return strings.Repeat(string(character), run), true
}

// splitParagraph cuts an oversized paragraph on rune boundaries. It is a cap on
// one structural unit, not a global window: paragraphs are only ever split
// because they are individually too large to be a useful retrieval unit.
//
// Each part keeps the lines it actually holds. Citing the whole paragraph for
// every part would make a chunk point at text it does not contain, which is the
// one thing a provenance range must never do. numbers is the document line of
// each line of the unit's own text.
func splitParagraph(section string, numbers []int, body string, maxRunes int) []Unit {
	runes := []rune(body)
	if maxRunes <= 0 || len(runes) <= maxRunes {
		return []Unit{{Section: section, Text: body, StartLine: numbers[0], EndLine: numbers[len(numbers)-1]}}
	}
	lines := strings.Split(body, "\n")
	parts := make([]Unit, 0, len(runes)/maxRunes+1)
	for offset := 0; offset < len(runes); offset += maxRunes {
		limit := offset + maxRunes
		if limit > len(runes) {
			limit = len(runes)
		}
		partStart, partEnd := lineSpan(lines, numbers, offset, limit)
		parts = append(parts, Unit{
			Section: section, Text: string(runes[offset:limit]),
			StartLine: partStart, EndLine: partEnd,
		})
	}
	return parts
}

// lineSpan maps a half-open rune window of a unit's text onto the document lines
// it touched. lines are the unit's own lines and numbers the line each came from,
// so a rune offset's line is found by walking the cumulative lengths. A line's
// terminator counts as part of the line it ends.
func lineSpan(lines []string, numbers []int, from, to int) (int, int) {
	last := from
	if to-1 > last {
		last = to - 1
	}
	return numbers[lineIndexOf(lines, from)], numbers[lineIndexOf(lines, last)]
}

func lineIndexOf(lines []string, offset int) int {
	position := 0
	for index, line := range lines {
		end := position + utf8.RuneCountInString(line)
		if offset <= end || index == len(lines)-1 {
			return index
		}
		position = end + 1
	}
	return len(lines) - 1
}

// splitCode keeps whole lines together so a split code block never ends mid
// statement, and only breaks a single line when that line alone exceeds the cap.
func splitCode(section string, numbers []int, lines []string, maxRunes int) []Unit {
	if maxRunes <= 0 {
		return []Unit{{Section: section, Text: strings.Join(lines, "\n"),
			StartLine: numbers[0], EndLine: numbers[len(numbers)-1]}}
	}
	var (
		parts   []Unit
		current []string
		size    int
		from    int // index within lines of the first line current holds
	)
	// emit closes the run of whole lines gathered so far. to is the index of its
	// last line, so a unit always cites the lines it contains - including the
	// fence lines, which are part of the block's text.
	emit := func(to int) {
		if len(current) == 0 {
			return
		}
		parts = append(parts, Unit{
			Section: section, Text: strings.Join(current, "\n"),
			StartLine: numbers[from], EndLine: numbers[to],
		})
		current, size = nil, 0
	}
	for index, line := range lines {
		length := utf8.RuneCountInString(line) + 1
		if length > maxRunes {
			emit(index - 1)
			// One line too long to fit: its parts all sit on that line.
			parts = append(parts, splitParagraph(section, numbers[index:index+1], line, maxRunes)...)
			continue
		}
		if size+length > maxRunes {
			emit(index - 1)
		}
		if len(current) == 0 {
			from = index
		}
		current = append(current, line)
		size += length
	}
	emit(len(lines) - 1)
	if len(parts) == 0 {
		parts = append(parts, Unit{Section: section, Text: strings.Join(lines, "\n"),
			StartLine: numbers[0], EndLine: numbers[len(numbers)-1]})
	}
	return parts
}
