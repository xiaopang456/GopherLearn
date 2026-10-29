package rag

import (
	"fmt"
	"strings"
)

// Chunk converts parser blocks into retrieval chunks without calling an
// embedding model or external storage.
func (c *Chunker) Chunk(input ChunkInput) ([]Chunk, error) {
	if err := validateChunkInput(input); err != nil {
		return nil, err
	}
	if len(input.Blocks) == 0 {
		return nil, nil
	}

	chunks := make([]Chunk, 0)
	buffer := make([]chunkUnit, 0)
	appendChunk := func(units []chunkUnit) error {
		if !unitsHaveBody(units) {
			return nil
		}
		chunk := c.makeChunk(input, len(chunks), units)
		if chunk.TokenEstimate > c.config.MaxTokens {
			return fmt.Errorf("%w: generated chunk uses %d tokens, maximum is %d", ErrInvalidChunkInput, chunk.TokenEstimate, c.config.MaxTokens)
		}
		if len(chunks) >= c.config.MaxChunks {
			return fmt.Errorf("%w: maximum is %d", ErrTooManyChunks, c.config.MaxChunks)
		}
		chunks = append(chunks, chunk)
		return nil
	}
	flush := func() error {
		err := appendChunk(buffer)
		buffer = buffer[:0]
		return err
	}

	for _, block := range input.Blocks {
		unit := unitFromBlock(block)
		if block.Type == BlockTypeHeading {
			if unitsHaveBody(buffer) {
				if err := flush(); err != nil {
					return nil, err
				}
			} else {
				buffer = replaceHeadingBranch(buffer, block.Level)
			}
			buffer = append(buffer, unit)
			continue
		}

		candidate := appendCopy(buffer, unit)
		if c.fits(input, candidate) {
			buffer = candidate
			continue
		}

		if err := flush(); err != nil {
			return nil, err
		}
		if c.fits(input, []chunkUnit{unit}) {
			buffer = append(buffer, unit)
			continue
		}

		parts, err := c.splitOversizedBlock(input, block)
		if err != nil {
			return nil, err
		}
		for _, part := range parts {
			if err := appendChunk([]chunkUnit{part}); err != nil {
				return nil, err
			}
		}
	}

	if err := flush(); err != nil {
		return nil, err
	}
	return chunks, nil
}

func validateChunkInput(input ChunkInput) error {
	if strings.TrimSpace(input.DocID) == "" {
		return fmt.Errorf("%w: DocID must not be empty", ErrInvalidChunkInput)
	}
	if input.DocVersion <= 0 {
		return fmt.Errorf("%w: DocVersion must be greater than zero", ErrInvalidChunkInput)
	}
	if strings.TrimSpace(input.SourceFile) == "" {
		return fmt.Errorf("%w: SourceFile must not be empty", ErrInvalidChunkInput)
	}
	for i, block := range input.Blocks {
		if block.Type != BlockTypeHeading && block.Type != BlockTypeParagraph && block.Type != BlockTypeCode {
			return fmt.Errorf("%w: block %d has unsupported type %q", ErrInvalidChunkInput, i, block.Type)
		}
		if block.StartLine <= 0 || block.EndLine < block.StartLine {
			return fmt.Errorf("%w: block %d has invalid lines %d-%d", ErrInvalidChunkInput, i, block.StartLine, block.EndLine)
		}
	}
	return nil
}

func (c *Chunker) fits(input ChunkInput, units []chunkUnit) bool {
	return len(units) > 0 && c.makeChunk(input, 0, units).TokenEstimate <= c.config.MaxTokens
}

func (c *Chunker) splitOversizedBlock(input ChunkInput, block Block) ([]chunkUnit, error) {
	if block.Type == BlockTypeCode {
		return c.splitCodeBlock(input, block)
	}
	if block.Type == BlockTypeParagraph {
		return c.splitTextBlock(input, block)
	}
	return nil, fmt.Errorf("%w: heading at lines %d-%d exceeds token budget", ErrInvalidChunkInput, block.StartLine, block.EndLine)
}

func (c *Chunker) splitTextBlock(input ChunkInput, block Block) ([]chunkUnit, error) {
	lines := strings.Split(block.Content, "\n")
	parts := make([]chunkUnit, 0)
	for start := 0; start < len(lines); {
		end := start
		for end < len(lines) {
			candidate := textPartUnit(block, lines, start, end+1)
			if !c.fits(input, []chunkUnit{candidate}) {
				break
			}
			end++
		}
		if end > start {
			parts = append(parts, textPartUnit(block, lines, start, end))
			start = end
			continue
		}

		lineParts, err := c.splitLongTextLine(input, block, lines[start], block.StartLine+start)
		if err != nil {
			return nil, err
		}
		parts = append(parts, lineParts...)
		start++
	}
	setPartNumbers(parts)
	return parts, nil
}

func (c *Chunker) splitLongTextLine(input ChunkInput, block Block, line string, sourceLine int) ([]chunkUnit, error) {
	runes := []rune(line)
	parts := make([]chunkUnit, 0)
	for start := 0; start < len(runes); {
		low, high, best := start+1, len(runes), start
		for low <= high {
			mid := low + (high-low)/2
			candidate := unitFromBlock(block)
			candidate.content = string(runes[start:mid])
			candidate.raw = candidate.content
			candidate.startLine, candidate.endLine = sourceLine, sourceLine
			if c.fits(input, []chunkUnit{candidate}) {
				best = mid
				low = mid + 1
			} else {
				high = mid - 1
			}
		}
		if best == start {
			return nil, fmt.Errorf("%w: metadata leaves no room for text at line %d", ErrInvalidChunkInput, sourceLine)
		}
		part := unitFromBlock(block)
		part.content = string(runes[start:best])
		part.raw = part.content
		part.startLine, part.endLine = sourceLine, sourceLine
		parts = append(parts, part)
		start = best
	}
	return parts, nil
}

func (c *Chunker) splitCodeBlock(input ChunkInput, block Block) ([]chunkUnit, error) {
	if block.Content == "" {
		return nil, fmt.Errorf("%w: empty code block metadata exceeds budget at line %d", ErrInvalidChunkInput, block.StartLine)
	}
	lines := strings.Split(block.Content, "\n")
	parts := make([]chunkUnit, 0)
	coveredUntil := 0
	for start := 0; start < len(lines); {
		end := start
		for end < len(lines) {
			candidate := codePartUnit(block, lines, start, end+1)
			if !c.fits(input, []chunkUnit{candidate}) {
				break
			}
			end++
		}
		if end == start {
			return nil, fmt.Errorf("%w at source line %d (maximum estimate %d)", ErrCodeLineTooLong, block.StartLine+1+start, c.config.MaxTokens)
		}
		if end <= coveredUntil {
			start = coveredUntil
			continue
		}
		parts = append(parts, codePartUnit(block, lines, start, end))
		coveredUntil = end
		if end == len(lines) {
			break
		}
		next := end
		overlap := 0
		for next > start+1 {
			lineTokens := EstimateTokens(lines[next-1])
			if overlap+lineTokens > c.config.OverlapTokens {
				break
			}
			overlap += lineTokens
			next--
		}
		start = next
	}
	setPartNumbers(parts)
	return parts, nil
}

func textPartUnit(block Block, lines []string, start, end int) chunkUnit {
	unit := unitFromBlock(block)
	unit.content = strings.Join(lines[start:end], "\n")
	unit.raw = unit.content
	unit.startLine = block.StartLine + start
	unit.endLine = block.StartLine + end - 1
	return unit
}

func codePartUnit(block Block, lines []string, start, end int) chunkUnit {
	unit := unitFromBlock(block)
	unit.content = strings.Join(lines[start:end], "\n")
	rawLines := strings.Split(block.Raw, "\n")
	closed := codeBlockHasClosingFence(rawLines)
	pieceRaw := make([]string, 0, end-start+2)
	if start == 0 && len(rawLines) > 0 {
		pieceRaw = append(pieceRaw, rawLines[0])
	}
	pieceRaw = append(pieceRaw, lines[start:end]...)
	if end == len(lines) && closed {
		pieceRaw = append(pieceRaw, rawLines[len(rawLines)-1])
	}
	unit.raw = strings.Join(pieceRaw, "\n")
	if start == 0 {
		unit.startLine = block.StartLine
	} else {
		unit.startLine = block.StartLine + 1 + start
	}
	unit.endLine = block.StartLine + end
	if end == len(lines) && closed {
		unit.endLine = block.EndLine
	}
	return unit
}

func codeBlockHasClosingFence(rawLines []string) bool {
	if len(rawLines) < 2 {
		return false
	}
	opening, ok := parseOpeningFence(rawLines[0])
	return ok && isClosingFence(rawLines[len(rawLines)-1], opening)
}

func setPartNumbers(parts []chunkUnit) {
	if len(parts) <= 1 {
		return
	}
	for i := range parts {
		parts[i].partIndex = i + 1
		parts[i].partTotal = len(parts)
	}
}

func unitsHaveBody(units []chunkUnit) bool {
	for _, unit := range units {
		if unit.typeName != BlockTypeHeading {
			return true
		}
	}
	return false
}

func replaceHeadingBranch(units []chunkUnit, level int) []chunkUnit {
	for len(units) > 0 {
		last := units[len(units)-1]
		if last.typeName != BlockTypeHeading || last.level < level {
			break
		}
		units = units[:len(units)-1]
	}
	return units
}

func appendCopy(units []chunkUnit, unit chunkUnit) []chunkUnit {
	result := make([]chunkUnit, len(units), len(units)+1)
	copy(result, units)
	return append(result, unit)
}
