package hybrid

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"testing"

	"GopherAI/model"
)

// JSON citations encode an unheaded chunk with null or an empty title array.
func baselineValidTitlePath(source map[string]any) bool {
	value, exists := source["title_path"]
	if !exists {
		return false
	}
	if value == nil {
		return true
	}
	items, ok := value.([]any)
	if !ok {
		return false
	}
	for _, item := range items {
		if _, ok := item.(string); !ok {
			return false
		}
	}
	return true
}

func TestKnowledgeBaselineVerifierRejectsMalformedSourceContract(t *testing.T) {
	healthy := baselineHealthySource("target", 1, 0, 1, 2, nil)
	expectation := baselineRetrievalExpectation{Allowed: []baselineExpectedDocument{{DocID: "target", Version: 1}}, MustInclude: []string{"target"}}
	check := func(data map[string]any) []string {
		sources, problems := baselineSourcesOf(data)
		return append(problems, baselineVerifySources(expectation, sources).Problems...)
	}
	if problems := check(map[string]any{"sources": []any{healthy}}); len(problems) != 0 {
		t.Fatalf("healthy: %v", problems)
	}
	for _, field := range []string{"doc_id", "doc_version", "chunk_index", "start_line", "end_line", "title_path"} {
		t.Run("missing_"+field, func(t *testing.T) {
			source := baselineCloneSource(healthy)
			delete(source, field)
			want := field
			if field == "doc_version" {
				want = "document version"
			}
			if problems := check(map[string]any{"sources": []any{source}}); !baselineContainsPhrase(problems, want) {
				t.Fatalf("missing %s: %v", field, problems)
			}
		})
	}
	for _, field := range []string{"doc_version", "chunk_index", "start_line", "end_line"} {
		for _, value := range []struct {
			name  string
			value any
		}{
			{"fraction", 1.5}, {"negative", -1}, {"string", "1"}, {"bool", true}, {"null", nil},
			{"nan", math.NaN()}, {"infinity", math.Inf(1)}, {"overflow", math.Ldexp(1, strconv.IntSize-1)}, {"json_overflow", json.Number("9223372036854775808")},
		} {
			t.Run(field+"_"+value.name, func(t *testing.T) {
				source := baselineCloneSource(healthy)
				source[field] = value.value
				want := field
				if field == "doc_version" {
					want = "document version"
				}
				if problems := check(map[string]any{"sources": []any{source}}); !baselineContainsPhrase(problems, want) {
					t.Fatalf("invalid %s accepted or wrong reason: %v", field, problems)
				}
			})
		}
	}
	for _, value := range []struct {
		name  string
		value any
	}{{"null", nil}, {"string", "malformed"}, {"number", 1.0}, {"array", []any{}}} {
		t.Run("later_"+value.name, func(t *testing.T) {
			sources, problems := baselineSourcesOf(map[string]any{"sources": []any{healthy, value.value}})
			if len(sources) != 2 || !baselineContainsPhrase(problems, "source 1 is not an object") {
				t.Fatalf("bad later source discarded: %v", problems)
			}
			if !baselineVerifySources(expectation, sources).Failed() {
				t.Fatal("malformed slot accepted by verifier")
			}
		})
	}
	for _, value := range []any{"bad", map[string]any{}, []map[string]any{healthy}} {
		if _, problems := baselineSourcesOf(map[string]any{"sources": value}); !baselineContainsPhrase(problems, "sources is missing or is not an array") {
			t.Fatalf("wrong sources shape accepted: %T", value)
		}
	}
	if _, problems := baselineSourcesOf(nil); len(problems) == 0 {
		t.Fatal("missing sources accepted")
	}
	sources, problems := baselineSourcesOf(map[string]any{"sources": []any{}})
	if len(problems) != 0 || baselineVerifySources(baselineRetrievalExpectation{ExpectNoSources: true}, sources).Failed() {
		t.Fatal("legitimate empty array rejected")
	}
	sources, problems = baselineSourcesOf(map[string]any{"sources": nil})
	if len(problems) != 0 || baselineVerifySources(baselineRetrievalExpectation{ExpectNoSources: true}, sources).Failed() {
		t.Fatal("legitimate null empty result rejected")
	}
	if !baselineContainsPhrase(baselineVerifySources(expectation, sources).Problems, "returned no source") {
		t.Fatal("null sources accepted where hits are required")
	}
	delete(healthy, "chunk_index")
	if problems := baselineLocationProblems(newBaselineChunkStore(), 1, []map[string]any{healthy}); !baselineContainsPhrase(problems, "missing or invalid chunk_index") {
		t.Fatalf("missing index accepted with empty store: %v", problems)
	}
}

func TestKnowledgeBaselineEmptyOwnerRequiresRetrievalEvidence(t *testing.T) {
	healthy := baselineTolerantRetrieval{Status: 500, Refusal: "agent_model_failed", ToolSteps: []model.AgentStep{{ArgumentsSummary: `{"query":"q"}`, ResultSummary: "个人知识库未找到匹配证据。"}}}
	if problems := baselineEmptyOwnerProblems(healthy, "q"); len(problems) != 0 {
		t.Fatalf("healthy observation: %v", problems)
	}
	for _, test := range []struct {
		name   string
		mutate func(*baselineTolerantRetrieval)
		want   string
	}{
		{"no_retrieval", func(r *baselineTolerantRetrieval) { r.ToolSteps = nil }, "no pre-generation"},
		{"infra_failure", func(r *baselineTolerantRetrieval) { r.Refusal = "database_failed" }, "unexpected empty-owner HTTP failure"},
		{"tool_failure", func(r *baselineTolerantRetrieval) { r.ToolSteps[0].ErrorMessage = "failed" }, "not a completed empty retrieval"},
		{"foreign_source", func(r *baselineTolerantRetrieval) { r.ToolSteps[0].SourceIDs = []string{"owner:v1:chunk"} }, "not a completed empty retrieval"},
		{"wrong_query", func(r *baselineTolerantRetrieval) { r.ToolSteps[0].ArgumentsSummary = `{"query":"other"}` }, "requested query"},
		{"unavailable", func(r *baselineTolerantRetrieval) {
			r.ToolSteps[0].ResultSummary = "个人知识库检索暂时不可用。"
		}, "not a completed empty retrieval"},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := healthy
			result.ToolSteps = append([]model.AgentStep(nil), healthy.ToolSteps...)
			test.mutate(&result)
			if problems := baselineEmptyOwnerProblems(result, "q"); !baselineContainsPhrase(problems, test.want) {
				t.Fatalf("wrong rejection: %v", problems)
			}
		})
	}
}

// A failed HTTP turn carries no sources contract. Only the known fixture model
// failure is tolerated, with independent evidence of completed retrieval.
func baselineEmptyOwnerProblems(result baselineTolerantRetrieval, query string) []string {
	problems := append([]string(nil), result.Problems...)
	if result.Status != 200 && (result.Status != 500 || result.Refusal != "agent_model_failed") {
		problems = append(problems, "unexpected empty-owner HTTP failure")
	}
	if result.Status == 200 && result.Data == nil {
		problems = append(problems, "successful empty-owner response has no data")
	}
	if len(result.ToolSteps) == 0 {
		problems = append(problems, "no pre-generation knowledge retrieval observation")
	}
	for position, step := range result.ToolSteps {
		var arguments map[string]any
		if err := json.Unmarshal([]byte(step.ArgumentsSummary), &arguments); err != nil || arguments["query"] != query {
			problems = append(problems, fmt.Sprintf("tool observation %d does not match the requested query", position))
		}
		if step.ErrorMessage != "" || step.ResultSummary != "个人知识库未找到匹配证据。" || len(step.SourceIDs) != 0 {
			problems = append(problems, fmt.Sprintf("tool observation %d is not a completed empty retrieval", position))
		}
	}
	return problems
}
