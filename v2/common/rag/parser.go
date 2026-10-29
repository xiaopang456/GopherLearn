package rag

import (
	"strings"
)

type heading struct {
	level int
	title string
}

type fence struct {
	marker   byte
	length   int
	language string
}

// ParseMarkdown converts Markdown into ordered heading, paragraph and fenced
// code blocks. It intentionally handles only the structures required by the
// personal-notes ingestion pipeline; chunk sizing belongs to the next stage.
func ParseMarkdown(content string) []Block {
	lines := splitDocumentLines(content)
	if len(lines) == 0 {
		return nil
	}

	blocks := make([]Block, 0)
	headings := make([]heading, 0, 6)
	paragraphStart := -1
	paragraphLines := make([]string, 0)
	codeStart := -1
	codeLines := make([]string, 0)
	var openFence fence

	flushParagraph := func(endLine int) {
		if paragraphStart < 0 {
			return
		}
		text := strings.Join(paragraphLines, "\n")
		blocks = append(blocks, Block{
			Type:      BlockTypeParagraph,
			Content:   text,
			Raw:       text,
			TitlePath: headingPath(headings),
			StartLine: paragraphStart + 1,
			EndLine:   endLine,
		})
		paragraphStart = -1
		paragraphLines = paragraphLines[:0]
	}

	for i, line := range lines {
		if codeStart >= 0 {
			if isClosingFence(line, openFence) {
				rawLines := append([]string{lines[codeStart]}, codeLines...)
				rawLines = append(rawLines, line)
				blocks = append(blocks, Block{
					Type:      BlockTypeCode,
					Language:  openFence.language,
					Content:   strings.Join(codeLines, "\n"),
					Raw:       strings.Join(rawLines, "\n"),
					TitlePath: headingPath(headings),
					StartLine: codeStart + 1,
					EndLine:   i + 1,
				})
				codeStart = -1
				codeLines = codeLines[:0]
				openFence = fence{}
				continue
			}
			codeLines = append(codeLines, line)
			continue
		}

		if candidate, ok := parseOpeningFence(line); ok {
			flushParagraph(i)
			codeStart = i
			openFence = candidate
			continue
		}

		if level, title, ok := parseATXHeading(line); ok {
			flushParagraph(i)
			for len(headings) > 0 && headings[len(headings)-1].level >= level {
				headings = headings[:len(headings)-1]
			}
			headings = append(headings, heading{level: level, title: title})
			blocks = append(blocks, Block{
				Type:      BlockTypeHeading,
				Level:     level,
				Content:   title,
				Raw:       line,
				TitlePath: headingPath(headings),
				StartLine: i + 1,
				EndLine:   i + 1,
			})
			continue
		}

		if strings.TrimSpace(line) == "" {
			flushParagraph(i)
			continue
		}

		if paragraphStart < 0 {
			paragraphStart = i
		}
		paragraphLines = append(paragraphLines, line)
	}

	if codeStart >= 0 {
		rawLines := append([]string{lines[codeStart]}, codeLines...)
		blocks = append(blocks, Block{
			Type:      BlockTypeCode,
			Language:  openFence.language,
			Content:   strings.Join(codeLines, "\n"),
			Raw:       strings.Join(rawLines, "\n"),
			TitlePath: headingPath(headings),
			StartLine: codeStart + 1,
			EndLine:   len(lines),
		})
	} else {
		flushParagraph(len(lines))
	}

	return blocks
}

// ParseText splits plain text on blank lines. Markdown-looking syntax remains
// paragraph content because .txt files do not opt into Markdown semantics.
func ParseText(content string) []Block {
	lines := splitDocumentLines(content)
	if len(lines) == 0 {
		return nil
	}

	blocks := make([]Block, 0)
	start := -1
	paragraph := make([]string, 0)
	flush := func(endLine int) {
		if start < 0 {
			return
		}
		text := strings.Join(paragraph, "\n")
		blocks = append(blocks, Block{
			Type:      BlockTypeParagraph,
			Content:   text,
			Raw:       text,
			StartLine: start + 1,
			EndLine:   endLine,
		})
		start = -1
		paragraph = paragraph[:0]
	}

	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			flush(i)
			continue
		}
		if start < 0 {
			start = i
		}
		paragraph = append(paragraph, line)
	}
	flush(len(lines))
	return blocks
}

func splitDocumentLines(content string) []string {
	content = strings.TrimPrefix(content, "\ufeff")
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.ReplaceAll(content, "\r", "\n")
	if content == "" {
		return nil
	}
	lines := strings.Split(content, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func parseATXHeading(line string) (int, string, bool) {
	trimmed := trimUpToThreeSpaces(line)
	if trimmed == "" || trimmed[0] != '#' {
		return 0, "", false
	}
	level := 0
	for level < len(trimmed) && level < 6 && trimmed[level] == '#' {
		level++
	}
	if level == 0 || (level < len(trimmed) && trimmed[level] != ' ' && trimmed[level] != '\t') {
		return 0, "", false
	}
	title := strings.TrimSpace(trimmed[level:])
	if hash := strings.LastIndex(title, "#"); hash >= 0 {
		candidate := strings.TrimRight(title, "#")
		if candidate != title && (candidate == "" || candidate[len(candidate)-1] == ' ' || candidate[len(candidate)-1] == '\t') {
			title = strings.TrimSpace(candidate)
		}
	}
	return level, title, true
}

func parseOpeningFence(line string) (fence, bool) {
	trimmed := trimUpToThreeSpaces(line)
	if len(trimmed) < 3 || (trimmed[0] != '`' && trimmed[0] != '~') {
		return fence{}, false
	}
	marker := trimmed[0]
	length := countPrefix(trimmed, marker)
	if length < 3 {
		return fence{}, false
	}
	info := strings.TrimSpace(trimmed[length:])
	if marker == '`' && strings.ContainsRune(info, '`') {
		return fence{}, false
	}
	language := ""
	if fields := strings.Fields(info); len(fields) > 0 {
		language = fields[0]
	}
	return fence{marker: marker, length: length, language: language}, true
}

func isClosingFence(line string, opening fence) bool {
	trimmed := trimUpToThreeSpaces(line)
	length := countPrefix(trimmed, opening.marker)
	return length >= opening.length && strings.TrimSpace(trimmed[length:]) == ""
}

func trimUpToThreeSpaces(line string) string {
	spaces := 0
	for spaces < len(line) && spaces < 3 && line[spaces] == ' ' {
		spaces++
	}
	return line[spaces:]
}

func countPrefix(value string, marker byte) int {
	count := 0
	for count < len(value) && value[count] == marker {
		count++
	}
	return count
}

func headingPath(headings []heading) []string {
	if len(headings) == 0 {
		return nil
	}
	path := make([]string, len(headings))
	for i, item := range headings {
		path[i] = item.title
	}
	return path
}
