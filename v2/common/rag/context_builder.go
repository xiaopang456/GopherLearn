package rag

import (
	"errors"
	"fmt"
	"math"
	"path"
	"strconv"
	"strings"
	"unicode"
)

const NoGroundingAnswer = "在你的个人知识库中没有找到足够依据，暂时无法根据个人笔记回答这个问题。"

var ErrInvalidContextConfig = errors.New("invalid RAG context config")

type ContextConfig struct {
	MaxContextTokens int
	MaxSnippetRunes  int
}

func DefaultContextConfig() ContextConfig {
	return ContextConfig{MaxContextTokens: 1800, MaxSnippetRunes: 240}
}

type Source struct {
	Citation   int      `json:"citation"`
	DocID      string   `json:"doc_id"`
	DocVersion int      `json:"doc_version"`
	FileName   string   `json:"file_name"`
	TitlePath  []string `json:"title_path"`
	StartLine  int      `json:"start_line"`
	EndLine    int      `json:"end_line"`
	ChunkIndex int      `json:"chunk_index"`
	Score      float64  `json:"score"`
	Snippet    string   `json:"snippet"`
}

type GroundedContext struct {
	Prompt      string
	Sources     []Source
	HasEvidence bool
}

type ContextBuilder struct {
	config ContextConfig
}

func NewContextBuilder(config ContextConfig) (*ContextBuilder, error) {
	if config.MaxContextTokens <= 0 || config.MaxSnippetRunes <= 0 {
		return nil, ErrInvalidContextConfig
	}
	return &ContextBuilder{config: config}, nil
}

func (b *ContextBuilder) Build(query string, results []RetrievalResult) GroundedContext {
	query = strings.TrimSpace(query)
	selected := make([]groundedChunk, 0, len(results))
	seen := make(map[string]struct{}, len(results))
	selectedContents := make([]string, 0, len(results))
	usedTokens := 0
	for _, result := range results {
		content := contextContent(result)
		if content == "" {
			continue
		}
		identity := groundingIdentity(result, content)
		if _, exists := seen[identity]; exists {
			continue
		}
		seen[identity] = struct{}{}
		if overlapsSelectedContent(content, selectedContents) {
			continue
		}
		cost := result.TokenEstimate
		if cost <= 0 {
			cost = EstimateTokens(content)
		}
		cost += 32 // stable allowance for citation and source metadata
		if usedTokens+cost > b.config.MaxContextTokens {
			continue
		}
		usedTokens += cost
		selected = append(selected, groundedChunk{result: result, content: content})
		selectedContents = append(selectedContents, content)
	}

	if len(selected) == 0 {
		return GroundedContext{Prompt: NoGroundingAnswer, Sources: []Source{}, HasEvidence: false}
	}

	sources := make([]Source, 0, len(selected))
	blocks := make([]string, 0, len(selected))
	for index, chunk := range selected {
		citation := index + 1
		source := sourceFromResult(citation, chunk.result, chunk.content, b.config.MaxSnippetRunes)
		sources = append(sources, source)
		blocks = append(blocks, formatGroundingBlock(source, chunk.content))
	}
	return GroundedContext{
		Prompt:      buildGroundedPrompt(query, strings.Join(blocks, "\n\n")),
		Sources:     sources,
		HasEvidence: true,
	}
}

func overlapsSelectedContent(content string, selected []string) bool {
	normalized := strings.Join(strings.Fields(content), " ")
	currentRunes := []rune(normalized)
	if len(currentRunes) == 0 {
		return true
	}
	for _, candidate := range selected {
		other := strings.Join(strings.Fields(candidate), " ")
		otherRunes := []rune(other)
		shorter, longer := len(currentRunes), len(otherRunes)
		if shorter > longer {
			shorter, longer = longer, shorter
		}
		if longer == 0 || float64(shorter)/float64(longer) < 0.8 {
			continue
		}
		if strings.Contains(normalized, other) || strings.Contains(other, normalized) {
			return true
		}
	}
	return false
}

type groundedChunk struct {
	result  RetrievalResult
	content string
}

func groundingIdentity(result RetrievalResult, content string) string {
	if result.ContentHash != "" {
		return "hash:" + result.ContentHash
	}
	if result.DocID != "" && result.ChunkID != "" {
		return "chunk:" + result.DocID + ":" + strconv.Itoa(result.DocVersion) + ":" + result.ChunkID
	}
	return "content:" + content
}

func contextContent(result RetrievalResult) string {
	content := strings.TrimSpace(result.RawContent)
	if content == "" {
		return ""
	}
	if result.ChunkType != string(ChunkTypeCode) {
		return content
	}
	language := safeFenceLanguage(result.CodeLanguage)
	fenceCount := strings.Count(content, "```")
	if fenceCount%2 == 0 && fenceCount > 0 {
		return content
	}
	if strings.HasPrefix(content, "```") {
		return content + "\n```"
	}
	if strings.HasSuffix(content, "```") {
		return "```" + language + "\n" + content
	}
	return "```" + language + "\n" + content + "\n```"
}

func safeFenceLanguage(language string) string {
	var result strings.Builder
	for _, character := range strings.TrimSpace(language) {
		if unicode.IsLetter(character) || unicode.IsDigit(character) || character == '+' || character == '#' || character == '-' || character == '_' {
			result.WriteRune(character)
		}
	}
	return result.String()
}

func sourceFromResult(citation int, result RetrievalResult, content string, maxSnippetRunes int) Source {
	return Source{
		Citation: citation, DocID: result.DocID, DocVersion: result.DocVersion,
		FileName: safeFileName(result.DocumentName), TitlePath: sanitizeTitlePath(result.TitlePath),
		StartLine: result.StartLine, EndLine: result.EndLine, ChunkIndex: result.ChunkIndex,
		Score: safeScore(result.Score), Snippet: safeSnippet(content, maxSnippetRunes),
	}
}

func safeFileName(value string) string {
	value = strings.ReplaceAll(strings.TrimSpace(value), "\\", "/")
	value = path.Base(value)
	if value == "" || value == "." || value == "/" {
		return "未命名文档"
	}
	return stripControl(value)
}

func sanitizeTitlePath(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(stripControl(value)); value != "" {
			result = append(result, value)
		}
	}
	return result
}

func safeScore(value float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0
	}
	if value < 0 {
		return 0
	}
	if value > 1 {
		return 1
	}
	return math.Round(value*1_000_000) / 1_000_000
}

func safeSnippet(content string, maxRunes int) string {
	content = strings.TrimSpace(stripControl(content))
	runes := []rune(content)
	if len(runes) <= maxRunes {
		return content
	}
	return strings.TrimSpace(string(runes[:maxRunes])) + "…"
}

func stripControl(value string) string {
	return strings.Map(func(character rune) rune {
		if character == '\n' || character == '\r' || character == '\t' || !unicode.IsControl(character) {
			return character
		}
		return -1
	}, value)
}

func formatGroundingBlock(source Source, content string) string {
	title := ""
	if len(source.TitlePath) > 0 {
		title = "；标题=" + strings.Join(source.TitlePath, " > ")
	}
	lines := ""
	if source.StartLine > 0 {
		lines = fmt.Sprintf("；行=%d-%d", source.StartLine, source.EndLine)
	}
	return fmt.Sprintf("[来源 %d] 文件=%s%s%s\n%s", source.Citation, source.FileName, title, lines, content)
}

func buildGroundedPrompt(query, contextText string) string {
	return fmt.Sprintf(`你正在回答“个人知识库”问题。必须遵守以下规则：
1. 只能依据下方编号来源回答，不得把模型常识伪装成用户笔记内容。
2. 每个事实结论后使用 [1]、[2] 这样的来源编号；编号必须与下方来源一致。
3. 尽量保留笔记中的个人表达和代码写法，不要擅自改写代码语义。
4. 如果来源互相冲突，明确指出冲突并分别引用，不要静默选择其中一个。
5. 如果这些来源不足以回答问题，明确说明“个人知识库中的现有依据不足”，不要猜测。

个人知识库来源：
%s

用户问题：%s

请给出有依据、可核对的回答：`, contextText, query)
}
