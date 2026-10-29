package rag

import (
	"fmt"
	"strings"

	"github.com/cloudwego/eino/schema"
)

// BuildRAGPrompt 构建包含检索文档的提示词
func BuildRAGPrompt(query string, docs []*schema.Document) string {
	if len(docs) == 0 {
		return NoGroundingAnswer
	}

	blocks := make([]string, 0, len(docs))
	for i, doc := range docs {
		fileName := "未命名文档"
		if source, ok := doc.MetaData["source"].(string); ok {
			fileName = safeFileName(source)
		}
		blocks = append(blocks, fmt.Sprintf("[来源 %d] 文件=%s\n%s", i+1, fileName, strings.TrimSpace(doc.Content)))
	}
	return buildGroundedPrompt(strings.TrimSpace(query), strings.Join(blocks, "\n\n"))
}
