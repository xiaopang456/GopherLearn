package agent

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
)

func at(value int) *int { return &value }

func fragment(idx *int, id, name, arguments string) *schema.Message {
	return &schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
		Index: idx, ID: id,
		Function: schema.FunctionCall{Name: name, Arguments: arguments},
	}}}
}

// The aggregator assembles before it judges, which is the whole point.
func TestAggregatorAssemblesThenValidates(t *testing.T) {
	aggregator := NewToolCallAggregator()
	// Mid-stream the call has no name yet. Asking now is asking about a thing
	// that does not exist, and this is exactly where STABILIZE-062 v1.1 asked.
	aggregator.Observe(fragment(at(0), "c1", "", `{"que`))
	if report := aggregator.Validate([]string{SearchPastSessionsToolName}); report.Ok() {
		t.Fatal("a half-delivered call was already judged well formed")
	}
	// The name arrives, and the same question now has a different, correct
	// answer.
	aggregator.Observe(fragment(at(0), "", SearchPastSessionsToolName, `ry":"x"}`))
	report := aggregator.Validate([]string{SearchPastSessionsToolName})
	if !report.Ok() {
		t.Fatalf("a complete, registered call was rejected: %v", report.Problems)
	}
	if report.Calls != 1 {
		t.Fatalf("the stream assembled to %d calls, want 1", report.Calls)
	}
	calls := aggregator.Calls()
	if calls[0].Arguments != `{"query":"x"}` {
		t.Fatalf("arguments = %q", calls[0].Arguments)
	}
}

// Every enumerated fault, one at a time.
func TestAggregatorReportsEachFault(t *testing.T) {
	table := []string{SaveMemoryDraftToolName, SearchPastSessionsToolName}
	for name, build := range map[string]struct {
		chunks []*schema.Message
		want   StreamToolCallProblem
	}{
		"never named": {[]*schema.Message{
			fragment(at(0), "c1", "", `{`), fragment(at(0), "", "", `}`)},
			StreamToolCallUnnamed},
		"two names": {[]*schema.Message{
			fragment(at(0), "c1", SaveMemoryDraftToolName, `{`),
			fragment(at(0), "", SearchPastSessionsToolName, `}`)},
			StreamToolCallNameConflict},
		"not registered": {[]*schema.Message{
			fragment(at(0), "c1", "knowledge_search", `{}`)},
			StreamToolCallUnregistered},
	} {
		t.Run(name, func(t *testing.T) {
			aggregator := NewToolCallAggregator()
			for _, chunk := range build.chunks {
				aggregator.Observe(chunk)
			}
			report := aggregator.Validate(table)
			if report.Ok() {
				t.Fatalf("%s was accepted", name)
			}
			found := false
			for _, problem := range report.Problems {
				if problem == build.want {
					found = true
				}
			}
			if !found {
				t.Fatalf("problems = %v, want %s", report.Problems, build.want)
			}
		})
	}
}

// An empty table means nothing was bound, so there is nothing to be off.
func TestAggregatorSkipsRegistrationWhenNothingIsBound(t *testing.T) {
	aggregator := NewToolCallAggregator()
	aggregator.Observe(fragment(at(0), "c1", "anything_at_all", `{}`))
	if report := aggregator.Validate(nil); !report.Ok() {
		t.Fatalf("an unbound copy reported %v", report.Problems)
	}
}

// The report carries codes and tool names, and nothing a provider wrote.
func TestAggregatorReportCarriesNoArgumentText(t *testing.T) {
	aggregator := NewToolCallAggregator()
	aggregator.Observe(fragment(at(0), "c1", "some_unregistered_tool",
		`{"secret":"sk-live-000","question":"记住我复习先默写"}`))
	report := aggregator.Validate([]string{SaveMemoryDraftToolName})
	rendered := strings.Join(report.UnregisteredNames, ",")
	for _, problem := range report.Problems {
		rendered += "," + string(problem)
	}
	for _, forbidden := range []string{"sk-live", "记住", "secret", "question"} {
		if strings.Contains(rendered, forbidden) {
			t.Errorf("the report carries %q: %s", forbidden, rendered)
		}
	}
	if len(report.UnregisteredNames) != 1 || report.UnregisteredNames[0] != "some_unregistered_tool" {
		t.Fatalf("the unregistered name was lost: %v", report.UnregisteredNames)
	}
}

// The old modes are untouched by any of this.
//
// Two halves. The names the legacy and explicit-action surfaces publish have
// not moved, and no production path calls the aggregator at all - it is an
// observation tool, so a mode's behaviour cannot depend on it.
func TestOldModesAreUnaffectedByTheAggregator(t *testing.T) {
	for surface, want := range map[ToolSurface]string{
		SurfaceLegacy:          "knowledge_search",
		SurfaceExplicitActions: "knowledge_search",
		SurfaceExplicitSources: SearchUploadedDocumentsToolName,
	} {
		if got := MustToolNameFor(CapabilityKnowledgeSearch, surface); got != want {
			t.Errorf("%s forces %s, want %s", surface, got, want)
		}
	}
	fileSet := token.NewFileSet()
	packages, err := parser.ParseDir(fileSet, ".", func(info fs.FileInfo) bool {
		return !strings.HasSuffix(info.Name(), "_test.go") &&
			info.Name() != "stream_tool_calls.go"
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, parsed := range packages {
		for path, file := range parsed.Files {
			ast.Inspect(file, func(node ast.Node) bool {
				identifier, ok := node.(*ast.Ident)
				if ok && (identifier.Name == "NewToolCallAggregator" ||
					identifier.Name == "ToolCallAggregator") {
					t.Errorf("%s references the aggregator; it is an observation "+
						"tool and no production path may depend on it", path)
				}
				return true
			})
		}
	}
}
