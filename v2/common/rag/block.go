package rag

// BlockType describes the structural role of a parsed document block.
type BlockType string

const (
	BlockTypeHeading   BlockType = "heading"
	BlockTypeParagraph BlockType = "paragraph"
	BlockTypeCode      BlockType = "code"
)

// Block is the parser output consumed by the chunker in the next ingestion
// stage. Line numbers are one-based and inclusive. Raw preserves the source
// syntax while normalizing line endings to LF; Content removes heading/fence
// markers where appropriate.
type Block struct {
	Type      BlockType
	Level     int
	Language  string
	Content   string
	Raw       string
	TitlePath []string
	StartLine int
	EndLine   int
}
