package rag

import (
	"strings"
	"testing"
)

func TestContextBuilderGroundsPromptAndMapsStableSources(t *testing.T) {
	builder, err := NewContextBuilder(ContextConfig{MaxContextTokens: 180, MaxSnippetRunes: 40})
	if err != nil {
		t.Fatal(err)
	}
	results := []RetrievalResult{
		{ChunkID: "a", DocID: "doc-a", DocVersion: 2, DocumentName: "/app/data/knowledge/73/doc-a/source-v2.md", TitlePath: []string{"Gin", "路由"}, StartLine: 10, EndLine: 12, ChunkIndex: 3, RawContent: "我习惯把 RouterGroup 理解为共享前缀和中间件。", ContentHash: "same", TokenEstimate: 50, Score: 0.91234567},
		{ChunkID: "duplicate", DocID: "doc-copy", DocVersion: 1, DocumentName: "copy.md", RawContent: "重复内容", ContentHash: "same", TokenEstimate: 20, Score: 0.8},
		{ChunkID: "overlap", DocID: "doc-overlap", DocVersion: 1, DocumentName: "overlap.md", RawContent: "我习惯把 RouterGroup 理解为共享前缀和中间件。补充", ContentHash: "near-copy", TokenEstimate: 50, Score: 0.79},
		{ChunkID: "b", DocID: "doc-b", DocVersion: 1, DocumentName: `C:\notes\redis.md`, TitlePath: []string{"Redis", "TTL"}, StartLine: 20, EndLine: 24, ChunkIndex: 1, RawContent: "EX 参数用来设置过期时间。", ContentHash: "other", TokenEstimate: 50, Score: 0.8},
		{ChunkID: "over-budget", DocID: "doc-c", DocVersion: 1, DocumentName: "mq.md", RawContent: "不应进入上下文", TokenEstimate: 100, Score: 0.7},
	}

	grounding := builder.Build("我在笔记里怎么理解路由组和 TTL？", results)
	if !grounding.HasEvidence || len(grounding.Sources) != 2 {
		t.Fatalf("grounding = %+v", grounding)
	}
	if grounding.Sources[0].Citation != 1 || grounding.Sources[0].FileName != "source-v2.md" || grounding.Sources[1].Citation != 2 || grounding.Sources[1].FileName != "redis.md" {
		t.Fatalf("stable sources = %+v", grounding.Sources)
	}
	if grounding.Sources[0].Score != 0.912346 || strings.Contains(grounding.Sources[0].FileName, "/app/") {
		t.Fatalf("safe source = %+v", grounding.Sources[0])
	}
	for _, expected := range []string{"[来源 1]", "[来源 2]", "共享前缀", "EX 参数", "每个事实结论后", "来源互相冲突", "用户问题：我在笔记里怎么理解路由组和 TTL？"} {
		if !strings.Contains(grounding.Prompt, expected) {
			t.Fatalf("prompt does not contain %q:\n%s", expected, grounding.Prompt)
		}
	}
	for _, unwanted := range []string{"重复内容", "补充", "不应进入上下文", "/app/data/knowledge", `C:\notes`} {
		if strings.Contains(grounding.Prompt, unwanted) {
			t.Fatalf("prompt leaked or exceeded budget with %q:\n%s", unwanted, grounding.Prompt)
		}
	}
}

func TestContextBuilderKeepsSplitCodeFenceComplete(t *testing.T) {
	builder, _ := NewContextBuilder(DefaultContextConfig())
	grounding := builder.Build("这段代码是什么？", []RetrievalResult{{
		ChunkID: "code", DocID: "doc", DocVersion: 1, DocumentName: "main.go.md",
		ChunkType: string(ChunkTypeCode), CodeLanguage: "go", RawContent: "func main() {\n\tfmt.Println(\"hi\")\n}", TokenEstimate: 20,
	}})
	if strings.Count(grounding.Prompt, "```") != 2 || !strings.Contains(grounding.Prompt, "```go") {
		t.Fatalf("code fence is incomplete:\n%s", grounding.Prompt)
	}
}

func TestContextBuilderNoEvidenceUsesDeterministicRefusal(t *testing.T) {
	builder, _ := NewContextBuilder(DefaultContextConfig())
	grounding := builder.Build("未知问题", nil)
	if grounding.HasEvidence || len(grounding.Sources) != 0 || grounding.Prompt != NoGroundingAnswer {
		t.Fatalf("empty grounding = %+v", grounding)
	}
}
