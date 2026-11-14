package agent

import (
	"GopherAI/model"
	"context"
	"encoding/json"
	"fmt"
	modelcomponent "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"strings"
	"testing"
)

type multiRetentionModel struct {
	calls int
	combo bool
}

func (m *multiRetentionModel) Generate(_ context.Context, input []*schema.Message, _ ...modelcomponent.Option) (*schema.Message, error) {
	m.calls++
	user, _ := firstMessageIDs(input[1].Content)
	entries := make([]map[string]any, 6)
	for i := range entries {
		entries[i] = map[string]any{"key": fmt.Sprintf("k%02d_%d", m.calls, i), "text": fmt.Sprintf("value %d %d", m.calls, i), "value": fmt.Sprintf("%d-%d", m.calls, i), "source_message_ids": []int{user}, "updated_at_message_id": user}
	}
	if m.combo {
		key := fmt.Sprintf("extra%d", m.calls)
		if m.calls <= 2 {
			key = "shared"
		}
		entries[5] = map[string]any{"key": key, "text": fmt.Sprintf("version%d", m.calls), "value": "same", "source_message_ids": []int{user}, "updated_at_message_id": user}
	} else {
		entries[5] = map[string]any{"key": "shared", "text": "shared", "value": "same", "source_message_ids": []int{user}, "updated_at_message_id": user}
	}
	data := map[string]any{"schema_version": 1, "segment": map[string]any{"topic": "test", "points": []any{map[string]any{"text": "a point", "evidence_message_ids": []int{user}, "source": "user"}}}, "whiteboard": map[string]any{"schema_version": 1, "constraints": entries}}
	b, _ := json.Marshal(data)
	return schema.AssistantMessage(string(b), nil), nil
}
func (m *multiRetentionModel) Stream(context.Context, []*schema.Message, ...modelcomponent.Option) (*schema.StreamReader[*schema.Message], error) {
	return nil, fmt.Errorf("not used")
}
func (m *multiRetentionModel) WithTools([]*schema.ToolInfo) (modelcomponent.ToolCallingChatModel, error) {
	return m, nil
}
func TestSplitGeneratorReconcilesEarlierCapacityEvictions(t *testing.T) {
	stub := &multiRetentionModel{}
	generator, err := NewModelSegmentGenerator(stub, 1200, 2000)
	if err != nil {
		t.Fatal(err)
	}
	generator.UseAssistantContent(true)
	messages := longConversation(65)
	result, err := generator.GenerateSegment(context.Background(), SegmentGenerationInput{Messages: messages, FirstMessageID: 1, LastMessageID: messages[len(messages)-1].ID})
	if err != nil {
		t.Fatal(err)
	}
	if stub.calls < 5 {
		t.Fatalf("insufficient split calls: %d", stub.calls)
	}
	var early *WhiteboardEntryOutcome
	for i := range result.Diagnostics.Entries {
		d := &result.Diagnostics.Entries[i]
		if d.Key == "k01_0" {
			early = d
			break
		}
	}
	if early == nil || early.CallIndex != 1 || early.Outcome != "applied" || early.EntryRetention != "removed" || early.EntryRetentionReason != "item_capacity" {
		t.Fatalf("early diagnostic: %+v", early)
	}
	var shared []WhiteboardEntryOutcome
	for _, d := range result.Diagnostics.Entries {
		if d.Key == "shared" {
			shared = append(shared, d)
		}
	}
	if len(shared) < 3 || shared[0].EntryRetention != "removed" || shared[0].EntryRetentionReason != "updated_or_replaced" || shared[len(shared)-1].EntryRetention != "retained" {
		t.Fatalf("shared versions: %+v", shared)
	}
	for _, e := range result.Whiteboard.Constraints {
		if strings.HasPrefix(e.Key, "k01_") {
			t.Fatalf("earliest entry survived: %+v", e)
		}
	}
}

type multiAnchorModel struct{ calls int }

func (m *multiAnchorModel) Generate(_ context.Context, input []*schema.Message, _ ...modelcomponent.Option) (*schema.Message, error) {
	m.calls++
	user, _ := firstMessageIDs(input[1].Content)
	makeEntry := func(key, value string, ids []int) map[string]any {
		return map[string]any{"key": key, "text": "每天学习 " + value, "value": value, "source_message_ids": ids, "updated_at_message_id": user}
	}
	entries := []map[string]any{}
	if m.calls == 1 {
		entries = append(entries, makeEntry("resolved", "30页", []int{user, 99999}), makeEntry("expired", "31页", []int{user, 99999}))
	} else {
		if m.calls == 2 {
			entries = append(entries, makeEntry("resolved", "30页", []int{user}))
		}
		entries = append(entries, makeEntry(fmt.Sprintf("a%d", m.calls), "31页", []int{user, 99999}))
	}
	data := map[string]any{"schema_version": 1, "segment": map[string]any{"topic": "test", "points": []any{map[string]any{"text": "a point", "evidence_message_ids": []int{user}, "source": "user"}}}, "whiteboard": map[string]any{"schema_version": 1, "constraints": entries}}
	b, _ := json.Marshal(data)
	return schema.AssistantMessage(string(b), nil), nil
}
func (m *multiAnchorModel) Stream(context.Context, []*schema.Message, ...modelcomponent.Option) (*schema.StreamReader[*schema.Message], error) {
	return nil, fmt.Errorf("not used")
}
func (m *multiAnchorModel) WithTools([]*schema.ToolInfo) (modelcomponent.ToolCallingChatModel, error) {
	return m, nil
}
func TestSplitGeneratorReconcilesResolvedAndExpiredAnchors(t *testing.T) {
	stub := &multiAnchorModel{}
	generator, err := NewModelSegmentGenerator(stub, 1200, 2000)
	if err != nil {
		t.Fatal(err)
	}
	generator.UseAssistantContent(true)
	messages := longConversation(80)
	for i := range messages {
		if messages[i].IsUser {
			messages[i].Content += " 每天学习30页、31页。"
		}
	}
	result, err := generator.GenerateSegment(context.Background(), SegmentGenerationInput{Messages: messages, FirstMessageID: 1, LastMessageID: messages[len(messages)-1].ID})
	if err != nil {
		t.Fatal(err)
	}
	if stub.calls < 10 {
		t.Fatalf("insufficient split calls: %d", stub.calls)
	}
	rows := map[string]WhiteboardEntryOutcome{}
	for _, d := range result.Diagnostics.Entries {
		if d.CallIndex == 1 {
			rows[d.Key] = d
		}
	}
	if rows["resolved"].AnchorRetention != "removed" || rows["resolved"].AnchorRetentionReason != "anchor_resolved" {
		t.Fatalf("resolved: %+v", rows["resolved"])
	}
	if rows["expired"].AnchorRetention != "removed" || rows["expired"].AnchorRetentionReason != "anchor_capacity" {
		t.Fatalf("expired: %+v", rows["expired"])
	}
}

func TestEarlierVersionDoesNotReviveWhenSameEntryReturns(t *testing.T) {
	entry := model.SessionWhiteboardEntry{Key: "k", Text: "same", Value: "v", SourceMessageIDs: []uint{1}, UpdatedAtMessageID: 1}
	row := WhiteboardEntryOutcome{Key: "k", Outcome: "applied", EntryRetention: "retained", candidateEntry: &entry, candidateGroup: "constraints", candidateValue: "v"}
	prior := WhiteboardDiagnostics{Entries: []WhiteboardEntryOutcome{row}}
	empty := model.SessionWhiteboardDocument{}
	reconcilePriorCall(&prior, WhiteboardDiagnostics{removedEntries: []whiteboardRemoval{{entry: entry, group: "constraints", reason: "item_capacity"}}}, empty)
	board := model.SessionWhiteboardDocument{Constraints: []model.SessionWhiteboardEntry{entry}}
	reconcilePriorCall(&prior, WhiteboardDiagnostics{Entries: []WhiteboardEntryOutcome{row}}, board)
	if got := prior.Entries[0]; got.EntryRetention != "removed" || got.EntryRetentionReason != "item_capacity" {
		t.Fatalf("reintroduced entry revived old record: %+v", got)
	}
	prior = WhiteboardDiagnostics{Entries: []WhiteboardEntryOutcome{row}}
	reconcilePriorCall(&prior, WhiteboardDiagnostics{Entries: []WhiteboardEntryOutcome{row}}, board)
	if got := prior.Entries[0]; got.EntryRetention != "removed" || got.EntryRetentionReason != "updated_or_replaced" {
		t.Fatalf("identical replacement not recorded: %+v", got)
	}
}

func TestSplitGeneratorReplacedVersionAndEvictedVersionKeepDistinctReasons(t *testing.T) {
	stub := &multiRetentionModel{combo: true}
	g, err := NewModelSegmentGenerator(stub, 1200, 2000)
	if err != nil {
		t.Fatal(err)
	}
	g.UseAssistantContent(true)
	messages := longConversation(65)
	result, err := g.GenerateSegment(context.Background(), SegmentGenerationInput{Messages: messages, FirstMessageID: 1, LastMessageID: messages[len(messages)-1].ID})
	if err != nil {
		t.Fatal(err)
	}
	var rows []WhiteboardEntryOutcome
	for _, r := range result.Diagnostics.Entries {
		if r.Key == "shared" {
			rows = append(rows, r)
		}
	}
	if len(rows) != 2 || rows[0].EntryRetentionReason != "updated_or_replaced" || rows[1].EntryRetentionReason != "item_capacity" || rows[0].EntryRetention != "removed" || rows[1].EntryRetention != "removed" {
		t.Fatalf("rows=%+v", rows)
	}
}
func TestRemovalEventCopiesVersionEvidence(t *testing.T) {
	item := model.SessionWhiteboardEntry{Key: "k", Text: "B", Value: "v", Origin: "user", SourceMessageIDs: []uint{7}}
	before := boardWithGroup("constraints", []model.SessionWhiteboardEntry{item})
	d := WhiteboardDiagnostics{}
	markRemovedEntries(&d, before, model.SessionWhiteboardDocument{}, "item_capacity")
	item.SourceMessageIDs[0] = 9
	before.Constraints[0].SourceMessageIDs[0] = 8
	if len(d.removedEntries) != 1 || d.removedEntries[0].entry.SourceMessageIDs[0] != 7 {
		t.Fatalf("snapshot changed: %+v", d.removedEntries)
	}
}
