package rag

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

type ChunkType string

const (
	ChunkTypeProse ChunkType = "prose"
	ChunkTypeCode  ChunkType = "code"
	ChunkTypeMixed ChunkType = "mixed"
)

var (
	ErrInvalidChunkerConfig = errors.New("invalid chunker config")
	ErrInvalidChunkInput    = errors.New("invalid chunk input")
	ErrCodeLineTooLong      = errors.New("code line exceeds chunk token budget")
	ErrTooManyChunks        = errors.New("document exceeds chunk count limit")
)

// ChunkerConfig is explicit so the same document and configuration always
// produce the same chunk sequence and IDs.
type ChunkerConfig struct {
	MaxTokens     int
	OverlapTokens int
	MaxChunks     int
}

func DefaultChunkerConfig() ChunkerConfig {
	return ChunkerConfig{MaxTokens: 512, OverlapTokens: 48, MaxChunks: 2000}
}

func (c ChunkerConfig) Validate() error {
	if c.MaxTokens <= 0 {
		return fmt.Errorf("%w: MaxTokens must be greater than zero", ErrInvalidChunkerConfig)
	}
	if c.OverlapTokens < 0 || c.OverlapTokens >= c.MaxTokens {
		return fmt.Errorf("%w: OverlapTokens must be non-negative and less than MaxTokens", ErrInvalidChunkerConfig)
	}
	if c.MaxChunks <= 0 {
		return fmt.Errorf("%w: MaxChunks must be greater than zero", ErrInvalidChunkerConfig)
	}
	return nil
}

type ChunkInput struct {
	DocID      string
	DocVersion int
	SourceFile string
	Blocks     []Block
}

// Chunk contains retrieval text and source metadata. PartIndex/PartTotal are
// one-based only when one oversized source block was split; otherwise both are
// zero.
type Chunk struct {
	ID               string
	Index            int
	Type             ChunkType
	DocID            string
	DocVersion       int
	SourceFile       string
	TitlePath        []string
	StartLine        int
	EndLine          int
	CodeLanguage     string
	Symbols          []string
	Content          string
	RawContent       string
	EmbeddingContent string
	ContentHash      string
	TokenEstimate    int
	PartIndex        int
	PartTotal        int
}

type Chunker struct {
	config ChunkerConfig
}

func NewChunker(config ChunkerConfig) (*Chunker, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &Chunker{config: config}, nil
}

type chunkUnit struct {
	typeName  BlockType
	level     int
	language  string
	content   string
	raw       string
	titlePath []string
	startLine int
	endLine   int
	partIndex int
	partTotal int
}

func unitFromBlock(block Block) chunkUnit {
	return chunkUnit{
		typeName: block.Type, level: block.Level, language: block.Language,
		content: block.Content, raw: block.Raw, titlePath: cloneStrings(block.TitlePath),
		startLine: block.StartLine, endLine: block.EndLine,
	}
}

func (c *Chunker) makeChunk(input ChunkInput, index int, units []chunkUnit) Chunk {
	chunk := Chunk{
		Index: index, DocID: input.DocID, DocVersion: input.DocVersion,
		SourceFile: input.SourceFile, StartLine: units[0].startLine,
		EndLine: units[len(units)-1].endLine,
	}

	rawParts := make([]string, 0, len(units))
	contentParts := make([]string, 0, len(units))
	hasCode, hasProse := false, false
	languages := make([]string, 0, 1)
	for _, unit := range units {
		rawParts = append(rawParts, unit.raw)
		switch unit.typeName {
		case BlockTypeCode:
			hasCode = true
			language := unit.language
			contentParts = append(contentParts, "```"+language+"\n"+unit.content+"\n```")
			if language != "" && !containsString(languages, language) {
				languages = append(languages, language)
			}
		case BlockTypeParagraph:
			hasProse = true
			contentParts = append(contentParts, unit.content)
		case BlockTypeHeading:
			contentParts = append(contentParts, unit.content)
		}
		if len(unit.titlePath) >= len(chunk.TitlePath) {
			chunk.TitlePath = cloneStrings(unit.titlePath)
		}
	}

	switch {
	case hasCode && hasProse:
		chunk.Type = ChunkTypeMixed
	case hasCode:
		chunk.Type = ChunkTypeCode
	default:
		chunk.Type = ChunkTypeProse
	}
	if len(languages) == 1 {
		chunk.CodeLanguage = languages[0]
	}
	if len(units) == 1 && units[0].partTotal > 1 {
		chunk.PartIndex, chunk.PartTotal = units[0].partIndex, units[0].partTotal
	}

	chunk.RawContent = strings.Join(rawParts, "\n\n")
	chunk.Content = strings.Join(contentParts, "\n\n")
	chunk.Symbols = ExtractSymbols(chunk.Content)
	if len(chunk.Symbols) > 32 {
		chunk.Symbols = chunk.Symbols[:32]
	}
	chunk.EmbeddingContent = buildEmbeddingContent(chunk)
	chunk.TokenEstimate = EstimateTokens(chunk.EmbeddingContent)
	contentSum := sha256.Sum256([]byte(chunk.RawContent))
	chunk.ContentHash = hex.EncodeToString(contentSum[:])
	chunk.ID = stableChunkID(chunk, c.config)
	return chunk
}

func buildEmbeddingContent(chunk Chunk) string {
	parts := []string{"文件: " + chunk.SourceFile}
	if len(chunk.TitlePath) > 0 {
		parts = append(parts, "标题: "+strings.Join(chunk.TitlePath, " > "))
	}
	if len(chunk.Symbols) > 0 {
		parts = append(parts, "符号: "+strings.Join(chunk.Symbols, ", "))
	}
	parts = append(parts, "", chunk.Content)
	return strings.Join(parts, "\n")
}

func stableChunkID(chunk Chunk, config ChunkerConfig) string {
	payload := strings.Join([]string{
		"chunker-v1", chunk.DocID, strconv.Itoa(chunk.DocVersion), strconv.Itoa(chunk.Index),
		chunk.SourceFile, strings.Join(chunk.TitlePath, "\x1f"),
		strconv.Itoa(chunk.StartLine), strconv.Itoa(chunk.EndLine), string(chunk.Type),
		chunk.CodeLanguage, chunk.ContentHash, strconv.Itoa(config.MaxTokens),
		strconv.Itoa(config.OverlapTokens), strconv.Itoa(config.MaxChunks),
	}, "\x1e")
	sum := sha256.Sum256([]byte(payload))
	return "chunk_" + hex.EncodeToString(sum[:16])
}

func cloneStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	return append([]string(nil), values...)
}

func containsString(values []string, value string) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}
