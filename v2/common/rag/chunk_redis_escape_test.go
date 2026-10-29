package rag

import (
	"strings"
	"testing"

	redisCommon "GopherAI/common/redis"
)

// The knowledge search once escaped every non-ASCII rune before putting it in a
// TAG clause, so `粘包` was sent as `\粘\包` while the indexer had stored it
// unescaped. The clause could never match, and because the text-field clauses
// cannot match Han either — RediSearch's analyzer does not segment it — the
// whole lexical channel returned nothing for Chinese. Hybrid retrieval silently
// ran vector-only, with no error and no degradation banner.
//
// It was not caught because every lexical test used an ASCII identifier, which
// the broken escape left untouched. These tests use Han deliberately.

func TestTagEscapeLeavesHanIntact(t *testing.T) {
	for _, value := range []string{"粘包", "熔断", "分页", "网络编程"} {
		if got := redisCommon.EscapeTagValue(value); got != value {
			t.Errorf("EscapeTagValue(%q) = %q; Han is not special to RediSearch and "+
				"escaping it makes the tag unmatchable against the unescaped stored value", value, got)
		}
	}
}

// The characters that genuinely are special must still be escaped, or a
// document id would break the query it is embedded in.
func TestTagEscapeStillEscapesSpecialCharacters(t *testing.T) {
	documentID := "48679742-78a4-4c23-9583-b7b6ce76cf23"
	escaped := redisCommon.EscapeTagValue(documentID)
	if !strings.Contains(escaped, `\-`) {
		t.Errorf("EscapeTagValue(%q) = %q; the hyphens in a UUID must stay escaped", documentID, escaped)
	}
	if strings.ContainsAny(strings.ReplaceAll(escaped, `\-`, ""), `-`) {
		t.Errorf("an unescaped hyphen survived in %q", escaped)
	}
}

// The query side must produce exactly what the index side stored. This is the
// asymmetry that caused the defect: HanBigramTagValue writes bigrams joined by
// commas with no escaping at all, so anything the query escapes differently can
// never match.
func TestQueryBigramsMatchStoredBigramsByteForByte(t *testing.T) {
	content := "粘包 是 TCP 流式传输的常见问题，需要在应用层做拆包。"
	stored := HanBigramTagValue(content)
	storedSet := map[string]bool{}
	for _, bigram := range strings.Split(stored, ",") {
		storedSet[bigram] = true
	}

	queryTerms := HanBigramQueryTerms(ExtractLexicalTerms("粘包"))
	if len(queryTerms) == 0 {
		t.Fatal("a Han query produced no bigram terms")
	}

	matched := false
	for _, bigram := range queryTerms {
		escaped := redisCommon.EscapeTagValue(bigram)
		if escaped != bigram {
			t.Errorf("query bigram %q is sent as %q but stored as %q; the two can never match",
				bigram, escaped, bigram)
		}
		if storedSet[escaped] {
			matched = true
		}
	}
	if !matched {
		t.Errorf("none of the query bigrams %v appear in the stored tag value %q", queryTerms, stored)
	}
}
