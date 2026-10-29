package memory

import (
	"reflect"
	"testing"
)

func TestNormalizeMemoryTagsUsesFixedVocabulary(t *testing.T) {
	got := NormalizeMemoryTags([]string{"RPC", "free-form", "rpc", "etcd", "watch", "security", "docker"})
	want := []string{"docker", "etcd", "rpc", "security", "watch"}
	if !reflect.DeepEqual([]string(got), want) {
		t.Fatalf("tags=%v want=%v", got, want)
	}
}

func TestGenerateQueryTagsUsesExactASCIIAndChineseAliases(t *testing.T) {
	got := GenerateQueryTags("解释 Go RPC 的并发与 JWT 鉴权", "etcd Watch 复习")
	for _, want := range []string{"go", "rpc", "concurrency", "security", "etcd"} {
		if !hasTag(got, want) {
			t.Fatalf("tags=%v missing=%s", got, want)
		}
	}
	if hasTag(GenerateQueryTags("google 搜索", ""), "go") {
		t.Fatal("substring google must not match go")
	}
}

func TestGenerateQueryTagsCoversMemoryExperimentTerms(t *testing.T) {
	tests := []struct {
		name string
		text string
		want []string
	}{
		{name: "session title", text: "Session 长标题写库失败", want: []string{"agent", "context"}},
		{name: "dream run", text: "Dream Run 租约过期后重新开放 Candidate", want: []string{"memory"}},
		{name: "rrf", text: "RRF 融合 BM25 与 Vector", want: []string{"rag", "embedding"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := GenerateQueryTags(tt.text, "")
			for _, want := range tt.want {
				if !hasTag(got, want) {
					t.Fatalf("tags=%v missing=%s", got, want)
				}
			}
		})
	}
}
