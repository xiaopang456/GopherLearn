package model

import (
	"fmt"
	"strings"
	"testing"
)

func TestSessionSummaryDecoderSupportsLegacyAndV2Evidence(t *testing.T) {
	legacy, err := DecodeSessionSummaryDocument(`{"current_goal":"RPC","confirmed_decisions":["使用长度前缀"]}`)
	if err != nil {
		t.Fatal(err)
	}
	if legacy.SchemaVersion != 0 || len(legacy.ConfirmedDecisions) != 1 || len(legacy.ConfirmedDecisions[0].EvidenceMessageIDs) != 0 {
		t.Fatalf("legacy=%#v", legacy)
	}
	v2, err := DecodeSessionSummaryDocument(`{"schema_version":2,"current_goal":"RPC","confirmed_decisions":[{"text":"使用长度前缀","evidence_message_ids":[9,3]}],"open_loops":[{"text":"补并发测试","evidence_message_ids":[11]}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := v2.EvidenceMessageIDs(); len(got) != 3 || got[0] != 3 || got[1] != 9 || got[2] != 11 {
		t.Fatalf("evidence ids=%v", got)
	}
}

func TestSessionSummaryDecoderRejectsUnsafeEvidence(t *testing.T) {
	for _, payload := range []string{
		`null`,
		`{"schema_version":3,"current_goal":"x"}`,
		`{"schema_version":2,"confirmed_decisions":[{"text":"x","evidence_message_ids":[0]}]}`,
		`{"schema_version":2,"confirmed_decisions":[{"text":"x","evidence_message_ids":[1,1]}]}`,
		`{"schema_version":2,"confirmed_decisions":[{"text":"x","evidence_message_ids":[1],"extra":true}]}`,
		`{"schema_version":2,"unknown":true}`,
	} {
		if _, err := DecodeSessionSummaryDocument(payload); err == nil {
			t.Fatalf("payload should fail: %s", payload)
		}
	}
}

// The budget of a summary is a budget and not a validity rule: a document
// that outgrew it still decodes, and it is trimmed to the budget instead of
// being thrown away.

func loops(count int) SessionSummaryEvidenceList {
	items := make(SessionSummaryEvidenceList, 0, count)
	for i := 1; i <= count; i++ {
		items = append(items, SessionSummaryEvidence{Text: fmt.Sprintf("loop-%02d", i), EvidenceMessageIDs: []uint{uint(i)}})
	}
	return items
}

func TestSessionSummaryTrimKeepsTheNewestItems(t *testing.T) {
	document := SessionSummaryDocument{SchemaVersion: SessionSummarySchemaV2, CurrentGoal: "keep going", OpenLoops: loops(22)}
	trimmed, truncations := document.Truncate(DefaultSummaryBudget())
	if len(trimmed.OpenLoops) != 20 {
		t.Fatalf("open loops = %d, want 20", len(trimmed.OpenLoops))
	}
	if trimmed.OpenLoops[0].Text != "loop-03" || trimmed.OpenLoops[19].Text != "loop-22" {
		t.Fatalf("the wrong items survived: %s .. %s", trimmed.OpenLoops[0].Text, trimmed.OpenLoops[19].Text)
	}
	if len(truncations) != 1 || truncations[0].Kind != "items" || truncations[0].Group != SummaryGroupOpenLoops ||
		truncations[0].Kept != 20 || truncations[0].Dropped != 2 {
		t.Fatalf("truncations = %+v", truncations)
	}
	if err := trimmed.Validate(DefaultSummaryBudget()); err != nil {
		t.Fatalf("the trimmed document is not within the budget: %v", err)
	}
}

func TestSessionSummaryTrimKeepsTheTailOfAPlainGroup(t *testing.T) {
	items := make([]string, 0, 25)
	for i := 1; i <= 25; i++ {
		items = append(items, fmt.Sprintf("done-%02d", i))
	}
	document := SessionSummaryDocument{SchemaVersion: SessionSummarySchemaV2, CurrentGoal: "keep going", CompletedItems: items}
	trimmed, truncations := document.Truncate(DefaultSummaryBudget())
	if len(trimmed.CompletedItems) != 20 || trimmed.CompletedItems[0] != "done-06" {
		t.Fatalf("completed items = %d, first = %q", len(trimmed.CompletedItems), trimmed.CompletedItems[0])
	}
	if len(truncations) != 1 || truncations[0].Group != SummaryGroupCompletedItems || truncations[0].Dropped != 5 {
		t.Fatalf("truncations = %+v", truncations)
	}
}

func TestSessionSummaryTrimKeepsTheDocumentWithinTheCitationBudget(t *testing.T) {
	items := make(SessionSummaryEvidenceList, 0, 20)
	next := uint(1)
	for i := 0; i < 20; i++ {
		ids := []uint{next, next + 1, next + 2, next + 3}
		next += 4
		items = append(items, SessionSummaryEvidence{Text: fmt.Sprintf("loop-%02d", i), EvidenceMessageIDs: ids})
	}
	document := SessionSummaryDocument{SchemaVersion: SessionSummarySchemaV2, CurrentGoal: "keep going", OpenLoops: items}
	trimmed, truncations := document.Truncate(DefaultSummaryBudget())
	if cited := len(trimmed.EvidenceMessageIDs()); cited > SessionSummarySourceLimit {
		t.Fatalf("the document still cites %d messages, want at most %d", cited, SessionSummarySourceLimit)
	}
	if len(truncations) == 0 {
		t.Fatal("the trim was not reported")
	}
	if err := trimmed.Validate(DefaultSummaryBudget()); err != nil {
		t.Fatalf("the trimmed document is not within the budget: %v", err)
	}
}

func TestSessionSummaryTrimKeepsTheNewestCitationsOfOneItem(t *testing.T) {
	ids := make([]uint, 0, 40)
	for i := 1; i <= 40; i++ {
		ids = append(ids, uint(i))
	}
	document := SessionSummaryDocument{SchemaVersion: SessionSummarySchemaV2, CurrentGoal: "keep going",
		OpenLoops: SessionSummaryEvidenceList{{Text: "loop", EvidenceMessageIDs: ids}}}
	trimmed, truncations := document.Truncate(DefaultSummaryBudget())
	item := trimmed.OpenLoops[0]
	if len(item.EvidenceMessageIDs) != SessionSummaryEvidenceLimit {
		t.Fatalf("citations = %d, want %d", len(item.EvidenceMessageIDs), SessionSummaryEvidenceLimit)
	}
	if item.EvidenceMessageIDs[0] != 9 || item.EvidenceMessageIDs[len(item.EvidenceMessageIDs)-1] != 40 {
		t.Fatalf("the wrong citations survived: %v", item.EvidenceMessageIDs)
	}
	if len(truncations) != 1 || truncations[0].Kind != "evidence_ids" || truncations[0].Dropped != 8 {
		t.Fatalf("truncations = %+v", truncations)
	}
}

func TestSessionSummaryInsideTheBudgetIsUntouched(t *testing.T) {
	document := SessionSummaryDocument{SchemaVersion: SessionSummarySchemaV2, CurrentGoal: "keep going", OpenLoops: loops(20)}
	trimmed, truncations := document.Truncate(DefaultSummaryBudget())
	if len(trimmed.OpenLoops) != 20 || len(truncations) != 0 {
		t.Fatalf("a document inside the budget was changed: %d items, %+v", len(trimmed.OpenLoops), truncations)
	}
	for i, item := range trimmed.OpenLoops {
		if item.Text != document.OpenLoops[i].Text {
			t.Fatalf("item %d changed: %q", i, item.Text)
		}
	}
}

func TestSessionSummaryDecodesAnOverlongDocument(t *testing.T) {
	loops := make([]string, 0, 25)
	for i := 1; i <= 25; i++ {
		loops = append(loops, fmt.Sprintf(`{"text":"loop-%02d","evidence_message_ids":[%d]}`, i, i))
	}
	payload := fmt.Sprintf(`{"schema_version":2,"current_goal":"keep going","open_loops":[%s]}`, strings.Join(loops, ","))
	document, err := DecodeSessionSummaryDocument(payload)
	if err != nil {
		t.Fatalf("an overlong document must still decode: %v", err)
	}
	if len(document.OpenLoops) != 25 {
		t.Fatalf("open loops = %d, want 25", len(document.OpenLoops))
	}
	if err := document.Validate(DefaultSummaryBudget()); err == nil {
		t.Fatal("the document is out of the budget and must say so")
	}
}
