package summaryconvert

import (
	"encoding/json"
	"strings"
	"testing"

	"GopherAI/model"
)

func TestClassifyNamesEveryStoredShape(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    Shape
	}{
		{name: "empty", content: "", want: ShapeEmpty},
		{name: "whitespace only", content: "   \n\t ", want: ShapeEmpty},
		{name: "prose", content: "用户想复习归并排序，已经讲完一轮。", want: ShapeProse},
		{name: "prose that mentions json", content: `上一轮摘要里出现过 { 这个字符，正文照旧`, want: ShapeProse},
		{
			name:    "current document",
			content: `{"schema_version":2,"current_goal":"复习归并排序","confirmed_decisions":[{"text":"用中文讲解","evidence_message_ids":[7]}],"completed_items":["讲完一轮"],"open_loops":[],"user_corrections":[],"key_errors":[],"references":[]}`,
			want:    ShapeCurrent,
		},
		{
			name:    "current document without a schema version",
			content: `{"current_goal":"复习归并排序","confirmed_decisions":[],"completed_items":[],"open_loops":[],"user_corrections":[],"key_errors":[],"references":[]}`,
			want:    ShapeCurrent,
		},
		{
			name:    "legacy document with string ids",
			content: `{"schema_version":2,"current_goal":"复习归并排序","confirmed_decisions":[{"text":"用中文讲解","evidence_message_ids":["7","8"]}],"completed_items":[],"open_loops":[],"user_corrections":[],"key_errors":[],"references":[],"legacy_text":"旧散文"}`,
			want:    ShapeLegacyJSON,
		},
		{
			// v2's own evidence list still accepts the older plain string
			// array, so a row in that shape is readable here and must not be
			// rewritten: its items carry no ids and grant no read-back, which
			// is what they already did.
			name:    "legacy v1 string arrays are already readable",
			content: `{"schema_version":2,"current_goal":"复习","confirmed_decisions":["用中文讲解"],"completed_items":[],"open_loops":[],"user_corrections":[],"key_errors":[],"references":[]}`,
			want:    ShapeCurrent,
		},
		{
			name:    "legacy document with no schema version",
			content: `{"current_goal":"复习","confirmed_decisions":[{"text":"用中文讲解","evidence_message_ids":["7"]}],"completed_items":[],"open_loops":[],"user_corrections":[],"key_errors":[],"references":[]}`,
			want:    ShapeLegacyJSON,
		},
		{
			name:    "unsupported schema version",
			content: `{"schema_version":3,"current_goal":"复习","confirmed_decisions":[],"completed_items":[],"open_loops":[],"user_corrections":[],"key_errors":[],"references":[]}`,
			want:    ShapeDamaged,
		},
		{
			name:    "field this schema does not know",
			content: `{"schema_version":2,"current_goal":"复习","mood":"good","confirmed_decisions":[],"completed_items":[],"open_loops":[],"user_corrections":[],"key_errors":[],"references":[]}`,
			want:    ShapeDamaged,
		},
		{
			name:    "trailing json",
			content: `{"current_goal":"复习","confirmed_decisions":[],"completed_items":[],"open_loops":[],"user_corrections":[],"key_errors":[],"references":[]} {"current_goal":"再来一份"}`,
			want:    ShapeDamaged,
		},
		{name: "broken json", content: `{"current_goal":`, want: ShapeDamaged},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := Classify(test.content); got != test.want {
				t.Fatalf("Classify()=%q, want %q for %s", got, test.want, test.content)
			}
		})
	}
}

// TestClassifySeparatesCurrentFromLegacy is the case the whole tool rests on:
// a document this build can already read must never be treated as legacy, or a
// conversion run would rewrite rows it should leave byte-for-byte alone.
func TestClassifySeparatesCurrentFromLegacy(t *testing.T) {
	current := `{"schema_version":2,"current_goal":"g","confirmed_decisions":[{"text":"t","evidence_message_ids":[7]}],"completed_items":[],"open_loops":[],"user_corrections":[],"key_errors":[],"references":[]}`
	if Classify(current) != ShapeCurrent {
		t.Fatalf("a schema v2 document with numeric ids must read as current")
	}
	legacy := strings.Replace(current, "[7]", `["7"]`, 1)
	if Classify(legacy) != ShapeLegacyJSON {
		t.Fatalf("the same document with a string citation must read as legacy, not current")
	}
}

func TestConvertLegacyDocumentKeepsTextAndCitations(t *testing.T) {
	content := `{"schema_version":2,"current_goal":" 复习归并排序 ","confirmed_decisions":[{"text":"用中文讲解","evidence_message_ids":["7"," 8 "]}],"completed_items":["讲完一轮",""],"open_loops":[{"text":"还要出练习题","evidence_message_ids":["9"]}],"user_corrections":[],"key_errors":["把复杂度说错了"],"references":["slides.pdf"],"legacy_text":"更早的散文"}`

	conversion, err := ConvertLegacyDocument(content, model.DefaultSummaryBudget())
	if err != nil {
		t.Fatalf("ConvertLegacyDocument() error: %v", err)
	}
	if conversion.ModelCalls != 0 {
		t.Fatalf("a mechanical conversion spent %d model calls, want 0", conversion.ModelCalls)
	}
	if conversion.LegacyTextRunes != len([]rune("更早的散文")) {
		t.Fatalf("legacy_text runes=%d, want the prose length", conversion.LegacyTextRunes)
	}
	if len(conversion.DroppedEvidenceIDs) != 0 {
		t.Fatalf("no citation was unexpressible, got %v", conversion.DroppedEvidenceIDs)
	}

	// The real reader is the oracle, not this package's own encoder.
	document, err := model.DecodeSessionSummaryDocument(conversion.Content)
	if err != nil {
		t.Fatalf("the converted document is not readable by this schema: %v\n%s", err, conversion.Content)
	}
	if document.SchemaVersion != model.SessionSummarySchemaV2 {
		t.Fatalf("schema version=%d, want %d", document.SchemaVersion, model.SessionSummarySchemaV2)
	}
	if document.CurrentGoal != "复习归并排序" {
		t.Fatalf("current_goal=%q, want the prose text trimmed", document.CurrentGoal)
	}
	if got := document.EvidenceMessageIDs(); len(got) != 3 || got[0] != 7 || got[1] != 8 || got[2] != 9 {
		t.Fatalf("evidence ids=%v, want the string citations mapped to 7,8,9", got)
	}
	if len(document.CompletedItems) != 1 || document.CompletedItems[0] != "讲完一轮" {
		t.Fatalf("completed_items=%v, want one trimmed non-empty item", document.CompletedItems)
	}
	if document.ConfirmedDecisions[0].Text != "用中文讲解" {
		t.Fatalf("the carried text changed: %q", document.ConfirmedDecisions[0].Text)
	}
}

func TestConvertLegacyDocumentReportsCitationsItCannotCarry(t *testing.T) {
	// A citation this schema cannot express is not quietly deleted: the item
	// keeps its text with no ids (which authorizes no read-back), and the id
	// itself comes back in the report.
	content := `{"schema_version":2,"current_goal":"复习","confirmed_decisions":[{"text":"用中文讲解","evidence_message_ids":["msg-7","0","8"]}],"completed_items":[],"open_loops":[],"user_corrections":[],"key_errors":[],"references":[]}`

	conversion, err := ConvertLegacyDocument(content, model.DefaultSummaryBudget())
	if err != nil {
		t.Fatalf("ConvertLegacyDocument() error: %v", err)
	}
	if strings.Join(conversion.DroppedEvidenceIDs, ",") != "msg-7,0" {
		t.Fatalf("dropped ids=%v, want the two that name no message here", conversion.DroppedEvidenceIDs)
	}
	document, err := model.DecodeSessionSummaryDocument(conversion.Content)
	if err != nil {
		t.Fatalf("converted document unreadable: %v", err)
	}
	if len(document.ConfirmedDecisions) != 1 {
		t.Fatalf("the item disappeared: %v", document.ConfirmedDecisions)
	}
	if document.ConfirmedDecisions[0].Text != "用中文讲解" {
		t.Fatalf("text lost with the citation: %q", document.ConfirmedDecisions[0].Text)
	}
	if got := document.EvidenceMessageIDs(); len(got) != 1 || got[0] != 8 {
		t.Fatalf("ids=%v, want only the citation that could be carried", got)
	}
}

func TestConvertLegacyDocumentRefusesWhatTheReaderWouldReject(t *testing.T) {
	// One item over the budget's item length is refused rather than written and
	// then skipped forever.
	long := strings.Repeat("归", model.SessionSummaryItemMaxRunes+1)
	content := `{"schema_version":2,"current_goal":"复习","confirmed_decisions":[{"text":"` + long + `","evidence_message_ids":["7"]}],"completed_items":[],"open_loops":[],"user_corrections":[],"key_errors":[],"references":[]}`

	if _, err := ConvertLegacyDocument(content, model.DefaultSummaryBudget()); err == nil {
		t.Fatal("a document the reader refuses was accepted")
	}

	prose := `{"schema_version":2,` // not a document at all
	if _, err := ConvertLegacyDocument(prose, model.DefaultSummaryBudget()); err == nil {
		t.Fatal("a non-document was converted")
	}
}

func TestConvertLegacyDocumentFitsTheDeploymentBudget(t *testing.T) {
	// The budget belongs to the deployment: a run that narrowed the group limit
	// has to refuse a document that overflows it, not trim the user's summary
	// down to whatever fits.
	content := `{"schema_version":2,"current_goal":"复习","confirmed_decisions":[{"text":"一","evidence_message_ids":["7"]},{"text":"二","evidence_message_ids":["8"]},{"text":"三","evidence_message_ids":["9"]}],"completed_items":[],"open_loops":[],"user_corrections":[],"key_errors":[],"references":[]}`

	tight := model.SummaryBudget{GroupItemLimit: 2, OpenLoopsItemLimit: 2, ItemEvidenceLimit: model.SessionSummaryEvidenceLimit, EvidenceIDLimit: 64}
	if _, err := ConvertLegacyDocument(content, tight); err == nil {
		t.Fatal("a document over the configured budget was accepted without trimming")
	}
	wide := model.DefaultSummaryBudget()
	conversion, err := ConvertLegacyDocument(content, wide)
	if err != nil {
		t.Fatalf("the default budget refused a document that fits: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(conversion.Content), &decoded); err != nil {
		t.Fatalf("converted content is not json: %v", err)
	}
	if got := len(decoded["confirmed_decisions"].([]any)); got != 3 {
		t.Fatalf("confirmed_decisions kept %d items, want all 3", got)
	}
}
