//go:build v2_real_gates

package hybrid

import (
	"GopherAI/model"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"strings"
	"sync"
	"testing"

	"gorm.io/gorm/schema"
)

type i0ModelTable struct {
	value any
	table string
}

// Frozen gate subset: same fifteen checks, using the original model names.
var i0RequiredTables = []i0ModelTable{
	{new(model.User), "users"}, {new(model.Session), "sessions"},
	{new(model.Message), "messages"}, {new(model.SessionSummary), "session_summaries"},
	{new(model.AgentRun), "agent_runs"}, {new(model.AgentStep), "agent_steps"},
	{new(model.KnowledgeDocument), "knowledge_documents"}, {new(model.LongTermMemory), "long_term_memories"},
	{new(model.MemoryDraft), "memory_drafts"}, {new(model.MemoryRevision), "memory_revisions"},
	{new(model.LearningSession), "learning_sessions"}, {new(model.LearningAttempt), "learning_attempts"},
	{new(model.TopicProgress), "topic_progresses"}, {new(model.EpisodeMemory), "episode_memories"},
	{new(model.EpisodeProjectionJob), "episode_projection_jobs"},
}

func validI0Structure(table, column, failure string) bool {
	known := false
	for _, row := range i0RequiredTables {
		if row.table == table {
			known = true
		}
	}
	if !known {
		return false
	}
	switch failure {
	case "missing_table":
		return column == ""
	case "missing_column":
		return table == "users" && column == "email_normalized"
	case "nonempty_or_query_failed":
		return column == "" && (table == "users" || table == "sessions" || table == "messages")
	}
	return false
}

func (d *i0Diagnostic) failStructure(err error, assertion, table, column, failure string) bool {
	expected := map[string]string{"missing_table": "original required table missing", "missing_column": "original generated email column missing", "nonempty_or_query_failed": "initializer unexpectedly populated original tables"}
	if assertion != expected[failure] || d.step != "structure" || !validI0Structure(table, column, failure) {
		return false
	}
	e := classifyI0(err)
	e.Step, e.Last, e.Result, e.Assertion = d.step, d.last, "failure", assertion
	e.CheckTable, e.CheckColumn, e.CheckFailure = table, column, failure
	d.emit(e)
	return true
}

func checkI0RequiredTables(hasTable func(any) bool, d *i0Diagnostic) bool {
	for _, row := range i0RequiredTables {
		if !hasTable(row.table) {
			d.failStructure(nil, "original required table missing", row.table, "", "missing_table")
			return false
		}
	}
	return true
}

func TestI0FrozenModelTableNames(t *testing.T) {
	// Full original AutoMigrate inventory; parsing opens no database.
	all := append([]i0ModelTable{}, i0RequiredTables...)
	all = append(all, i0ModelTable{new(model.EpisodeAssertionState), "episode_assertion_states"},
		i0ModelTable{new(model.EpisodeAssertionRelation), "episode_assertion_relations"},
		i0ModelTable{new(model.EpisodeRelationProposal), "episode_relation_proposals"},
		i0ModelTable{new(model.LearningQuestion), "learning_questions"},
		i0ModelTable{new(model.MemoryExtractionJob), "memory_extraction_jobs"},
		i0ModelTable{new(model.MemoryCandidate), "memory_candidates"},
		i0ModelTable{new(model.MemoryDreamRun), "memory_dream_runs"})
	models := map[string]bool{}
	for _, row := range all {
		s, err := schema.Parse(row.value, &sync.Map{}, schema.NamingStrategy{IdentifierMaxLength: 64})
		if err != nil {
			t.Fatal(err)
		}
		if s.Table != row.table {
			t.Fatalf("model %s: expected %s, parsed %s", s.Name, row.table, s.Table)
		}
		models[reflect.TypeOf(row.value).Elem().Name()] = true
		t.Logf("MODEL_TABLE %s %s", s.Name, s.Table)
	}
	if len(i0RequiredTables) != 15 || len(models) != 22 {
		t.Fatal("frozen coverage changed")
	}
	// Assert the inventory matches the actual original initializer, not a hand-built replacement.
	f, err := parser.ParseFile(token.NewFileSet(), "../v2/common/mysql/mysql.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	actual := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		c, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := c.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "AutoMigrate" {
			return true
		}
		for _, arg := range c.Args {
			v, ok := arg.(*ast.CallExpr)
			if !ok || len(v.Args) != 1 {
				t.Fatal("initializer inventory shape changed")
			}
			s, ok := v.Args[0].(*ast.SelectorExpr)
			if !ok {
				t.Fatal("initializer model shape changed")
			}
			actual[s.Sel.Name] = true
		}
		return true
	})
	if !reflect.DeepEqual(actual, models) {
		t.Fatal("original initializer model inventory drift")
	}
}

func TestI0MissingTableClosedDiagnostic(t *testing.T) {
	for _, missing := range i0RequiredTables {
		var rows []string
		d := newI0Diagnostic(func(v ...any) { rows = append(rows, v[0].(string)) })
		d.last = "initialize"
		d.begin("structure")
		if checkI0RequiredTables(func(v any) bool { return v.(string) != missing.table }, d) {
			t.Fatal("missing table accepted")
		}
		var e i0Evidence
		if json.Unmarshal([]byte(strings.TrimPrefix(rows[len(rows)-1], "I0_DIAG ")), &e) != nil {
			t.Fatal("invalid diagnostic")
		}
		if e.CheckTable != missing.table || e.CheckFailure != "missing_table" || e.Last != "initialize" {
			t.Fatal("missing exact check")
		}
	}
	for _, input := range [][3]string{{"private-dsn", "", "missing_table"}, {"users", "secret-column", "missing_column"}, {"users", "", "private-reason"}} {
		d := newI0Diagnostic(func(...any) { t.Fatal("unknown value emitted") })
		d.step = "structure"
		if d.failStructure(nil, "original required table missing", input[0], input[1], input[2]) {
			t.Fatal("unknown check accepted")
		}
	}
}
