package rag

import (
	"reflect"
	"testing"
)

func TestExtractLexicalTermsMixedChineseAndCode(t *testing.T) {
	terms := ExtractLexicalTerms("GetOrCreateAIHelper 为什么不能只加 sync.RWMutex 和 RLock？")
	for _, expected := range []string{"GetOrCreateAIHelper", "sync.RWMutex", "RLock"} {
		if !containsString(terms, expected) {
			t.Fatalf("terms %v do not contain %q", terms, expected)
		}
	}
	if len(terms) > maxLexicalTerms {
		t.Fatalf("term count = %d", len(terms))
	}
}

func TestExtractLexicalTermsIsDeterministicAndBounded(t *testing.T) {
	query := "服务发现为什么在 Get 和 Watch 之间存在事件空窗，config.toml 应该如何配置？"
	first := ExtractLexicalTerms(query)
	second := ExtractLexicalTerms(query)
	if !reflect.DeepEqual(first, second) || len(first) == 0 || len(first) > maxLexicalTerms {
		t.Fatalf("terms are not deterministic and bounded: %v / %v", first, second)
	}
}

func TestExtractLexicalTermsPrioritizesTrailingCodeIdentifier(t *testing.T) {
	query := "这是一个非常长的中文问题用于产生大量二元词并验证达到数量上限之后仍然不会丢掉末尾的重要代码符号 GetOrCreateAIHelper"
	terms := ExtractLexicalTerms(query)
	if len(terms) > maxLexicalTerms || !containsString(terms, "GetOrCreateAIHelper") {
		t.Fatalf("prioritized terms = %v", terms)
	}
}

func TestFuseRRFPromotesCandidateFoundByBothChannels(t *testing.T) {
	vector := []RedisChunkSearchResult{
		{RedisKey: "a", DocID: "doc", ChunkID: "a", Distance: 0.1},
		{RedisKey: "b", DocID: "doc", ChunkID: "b", Distance: 0.2},
	}
	lexical := []RedisChunkSearchResult{
		{RedisKey: "b", DocID: "doc", ChunkID: "b", LexicalScore: 8},
		{RedisKey: "c", DocID: "doc", ChunkID: "c", LexicalScore: 7},
	}
	results := FuseRRF(vector, lexical, 3)
	if len(results) != 3 || results[0].RedisKey != "b" {
		t.Fatalf("FuseRRF() = %+v", results)
	}
	if results[0].MatchKind != "vector+lexical" || results[0].VectorRank != 2 || results[0].LexicalRank != 1 || results[0].FusionScore <= results[1].FusionScore {
		t.Fatalf("fused trace = %+v", results[0])
	}
}

func TestFuseRRFVectorOnlyKeepsOrder(t *testing.T) {
	vector := []RedisChunkSearchResult{{RedisKey: "a"}, {RedisKey: "b"}}
	results := FuseRRF(vector, nil, 2)
	if len(results) != 2 || results[0].RedisKey != "a" || results[1].RedisKey != "b" || results[0].FusionScore != 1 {
		t.Fatalf("vector-only fusion = %+v", results)
	}
}
