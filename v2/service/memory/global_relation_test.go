package memory

import (
	"encoding/json"
	"strings"

	memorydao "GopherAI/dao/memory"
	"GopherAI/model"
	"context"
	"errors"
	"testing"
	"time"
)

func TestGlobalRelationClassifierAcceptsOnlySuppliedTarget(t *testing.T) {
	chat := &memoryChat{response: toolMessage(globalConflictTool, `{"relation":"duplicate","target_memory_id":"m1","reason":"same preference"}`)}
	classifier, _ := NewGlobalRelationClassifier(chat)
	result, err := classifier.Classify(context.Background(), ExplicitMemoryInput{Scope: model.MemoryScopeGlobal, Subject: "回答", Content: "先给结论"}, []model.LongTermMemory{{ID: "m1", Scope: model.MemoryScopeGlobal, Subject: "结构", Content: "结论优先", Status: model.LongTermMemoryActive}})
	if err != nil || result.Relation != GlobalRelationDuplicate || result.TargetMemoryID != "m1" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	chat.response = toolMessage(globalConflictTool, `{"relation":"duplicate","target_memory_id":"foreign","reason":"invented"}`)
	if _, err = classifier.Classify(context.Background(), ExplicitMemoryInput{Scope: model.MemoryScopeGlobal, Subject: "回答", Content: "先给结论"}, []model.LongTermMemory{{ID: "m1", Scope: model.MemoryScopeGlobal, Subject: "结构", Content: "结论优先", Status: model.LongTermMemoryActive}}); err == nil {
		t.Fatal("expected invented target rejection")
	}
}

func TestGlobalRelationClassifierIndependentAndStrictSchema(t *testing.T) {
	chat := &memoryChat{response: toolMessage(globalIndependentTool, `{"reason":"different durable preference"}`)}
	classifier, _ := NewGlobalRelationClassifier(chat)
	result, err := classifier.Classify(context.Background(), ExplicitMemoryInput{Scope: model.MemoryScopeGlobal, Subject: "语言", Content: "使用中文"}, []model.LongTermMemory{{ID: "m1", Scope: model.MemoryScopeGlobal, Subject: "排版", Content: "使用列表", Status: model.LongTermMemoryActive}})
	if err != nil || result.Relation != GlobalRelationIndependent {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	chat.response = toolMessage(globalIndependentTool, `{"reason":"different","action":"write"}`)
	if _, err = classifier.Classify(context.Background(), ExplicitMemoryInput{Scope: model.MemoryScopeGlobal, Subject: "语言", Content: "使用中文"}, []model.LongTermMemory{{ID: "m1", Scope: model.MemoryScopeGlobal, Subject: "排版", Content: "使用列表", Status: model.LongTermMemoryActive}}); err == nil {
		t.Fatal("expected unknown authority field rejection")
	}
}

type globalExplicitStub struct{ exact []model.LongTermMemory }

func (*globalExplicitStub) CreateUserMemory(context.Context, memorydao.ExplicitWriteParams) (*memorydao.ExplicitWriteResult, error) {
	return nil, nil
}
func (s *globalExplicitStub) FindExplicitConflicts(context.Context, int64, model.MemoryScope, string, string, []string, int) ([]model.LongTermMemory, error) {
	return append([]model.LongTermMemory(nil), s.exact...), nil
}
func (*globalExplicitStub) UpsertDraft(context.Context, *model.MemoryDraft) (*model.MemoryDraft, bool, error) {
	return nil, false, nil
}
func (*globalExplicitStub) GetDraft(context.Context, int64, string) (*model.MemoryDraft, error) {
	return nil, memorydao.ErrDraftNotFound
}
func (*globalExplicitStub) ListDrafts(context.Context, int64, model.MemoryDraftStatus, int) ([]model.MemoryDraft, error) {
	return nil, nil
}
func (*globalExplicitStub) ResolveDraft(context.Context, memorydao.ResolveDraftParams) (*model.MemoryDraft, *memorydao.ExplicitWriteResult, error) {
	return nil, nil, nil
}
func (*globalExplicitStub) ArchiveMemory(context.Context, int64, string, string, time.Time) (*model.LongTermMemory, *model.MemoryRevision, error) {
	return nil, nil, nil
}
func (*globalExplicitStub) GetExplicitWrite(context.Context, int64, string) (*memorydao.ExplicitWriteResult, error) {
	return nil, nil
}
func (*globalExplicitStub) FindDraftByContent(context.Context, int64, model.MemoryScope, string, string, string, ...model.MemoryDraftStatus) (*model.MemoryDraft, error) {
	return nil, nil
}
func (*globalExplicitStub) ApplyDraftEdit(context.Context, memorydao.DraftEditParams) (*model.MemoryDraft, error) {
	return nil, nil
}
func (*globalExplicitStub) GetActiveByID(context.Context, int64, string) (*model.LongTermMemory, error) {
	return nil, memorydao.ErrMemoryNotFound
}
func (*globalExplicitStub) FindMemoriesByIDs(context.Context, int64, []string) ([]model.LongTermMemory, error) {
	return nil, nil
}
func (*globalExplicitStub) FindBySourceSession(context.Context, int64, string) ([]model.LongTermMemory, error) {
	return nil, nil
}

type globalSourceStub struct {
	rows []model.LongTermMemory
	err  error
}

func (s globalSourceStub) ListAllActive(context.Context, int64, model.MemoryScope, float64, int) ([]model.LongTermMemory, error) {
	return append([]model.LongTermMemory(nil), s.rows...), s.err
}

func (s globalSourceStub) GetActiveByID(_ context.Context, _ int64, memoryID string) (*model.LongTermMemory, error) {
	for _, memory := range s.rows {
		if memory.ID == memoryID {
			copy := memory
			return &copy, nil
		}
	}
	return nil, memorydao.ErrMemoryNotFound
}

type globalResolverStub struct {
	result GlobalRelationResult
	err    error
	calls  int
}

func (s *globalResolverStub) Classify(context.Context, ExplicitMemoryInput, []model.LongTermMemory) (GlobalRelationResult, error) {
	s.calls++
	return s.result, s.err
}

func TestExplicitGlobalConflictBoundaryIsBoundedAndFailClosed(t *testing.T) {
	active := model.LongTermMemory{ID: "m1", Scope: model.MemoryScopeGlobal, Subject: "回答", Content: "先给结论", ContentHash: model.MemoryContentHash("先给结论"), Status: model.LongTermMemoryActive}
	input := ExplicitMemoryInput{Scope: model.MemoryScopeGlobal, Subject: "顺序", Content: "请先说最终结论"}
	resolver := &globalResolverStub{result: GlobalRelationResult{Relation: GlobalRelationDuplicate, TargetMemoryID: active.ID, Reason: "paraphrase"}}
	service := &GovernanceService{explicit: &globalExplicitStub{}, globalSource: globalSourceStub{rows: []model.LongTermMemory{active}}, globalRelations: resolver}
	conflicts, snapshot, _, _, err := service.explicitConflicts(context.Background(), 7, input)
	if err != nil || len(conflicts) != 1 || conflicts[0].ID != active.ID || snapshot == "" || resolver.calls != 1 {
		t.Fatalf("conflicts=%+v snapshot=%q calls=%d err=%v", conflicts, snapshot, resolver.calls, err)
	}

	resolver.err = errors.New("provider unavailable")
	if _, _, _, _, err = service.explicitConflicts(context.Background(), 7, input); err == nil {
		t.Fatal("provider failure must close the write boundary")
	}

	rows := make([]model.LongTermMemory, GlobalMemoryHardLimit+1)
	for index := range rows {
		rows[index] = active
		rows[index].ID = string(rune('a' + index))
	}
	resolver.err, resolver.calls = nil, 0
	service.globalSource = globalSourceStub{rows: rows}
	if _, _, _, _, err = service.explicitConflicts(context.Background(), 7, input); !errors.Is(err, ErrGlobalMemoryCapacity) || resolver.calls != 0 {
		t.Fatalf("capacity err=%v classifier_calls=%d", err, resolver.calls)
	}
}

func TestExplicitGlobalExactDuplicateSkipsClassifierButBindsSnapshot(t *testing.T) {
	active := model.LongTermMemory{ID: "m1", Scope: model.MemoryScopeGlobal, Subject: "回答", Content: "先给结论", ContentHash: model.MemoryContentHash("先给结论"), Status: model.LongTermMemoryActive}
	resolver := &globalResolverStub{}
	service := &GovernanceService{explicit: &globalExplicitStub{exact: []model.LongTermMemory{active}}, globalSource: globalSourceStub{rows: []model.LongTermMemory{active}}, globalRelations: resolver}
	conflicts, snapshot, _, _, err := service.explicitConflicts(context.Background(), 7, ExplicitMemoryInput{Scope: model.MemoryScopeGlobal, Subject: "回答", Content: active.Content})
	if err != nil || len(conflicts) != 1 || snapshot == "" || resolver.calls != 0 {
		t.Fatalf("conflicts=%+v snapshot=%q calls=%d err=%v", conflicts, snapshot, resolver.calls, err)
	}
}

// TestGlobalRelationClassifierCarriesClassAndScopeRules is the DED-02 input
// audit: what the trial run handed the classifier was a payload with no
// profile class and rules with no scope definition, so a same-scope
// paraphrase could drift to independent. This locks the fixed contract: the
// candidate and every old row travel with their profile_class, and the rules
// define duplicate scope-first - same scope plus same instruction is a
// duplicate whatever the wording or the example.
func TestGlobalRelationClassifierCarriesClassAndScopeRules(t *testing.T) {
	chat := &memoryChat{response: toolMessage(globalIndependentTool, `{"reason":"different scope"}`)}
	classifier, _ := NewGlobalRelationClassifier(chat)
	incoming := ExplicitMemoryInput{
		Scope: model.MemoryScopeGlobal, Subject: "讲解偏好",
		Content:      "讲新概念时先举一个我熟悉的例子，再讲正式定义。",
		ProfileClass: model.ProfileClassTeachingPreference,
	}
	existing := []model.LongTermMemory{
		{ID: "m-teach", Scope: model.MemoryScopeGlobal, Subject: "讲解偏好", Content: "讲新东西先用熟悉的例子再给定义。", ProfileClass: model.ProfileClassTeachingPreference, Status: model.LongTermMemoryActive},
		{ID: "m-goal", Scope: model.MemoryScopeGlobal, Subject: "目标", Content: "2027 年春招目标岗位：Go 后端开发", ProfileClass: model.ProfileClassGoal, Status: model.LongTermMemoryActive},
	}
	if _, err := classifier.Classify(context.Background(), incoming, existing); err != nil {
		t.Fatalf("classify: %v", err)
	}
	if len(chat.messages) != 2 {
		t.Fatalf("messages=%d, want system plus user", len(chat.messages))
	}
	var payload struct {
		ProposedMemory struct {
			Subject      string `json:"subject"`
			Content      string `json:"content"`
			ProfileClass string `json:"profile_class"`
		} `json:"proposed_memory"`
		ActiveMemories []struct {
			ID           string `json:"id"`
			Subject      string `json:"subject"`
			Content      string `json:"content"`
			ProfileClass string `json:"profile_class"`
		} `json:"active_memories"`
	}
	if err := json.Unmarshal([]byte(chat.messages[1].Content), &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload.ProposedMemory.Content != incoming.Content || payload.ProposedMemory.ProfileClass != string(model.ProfileClassTeachingPreference) {
		t.Fatalf("proposed=%+v, want the candidate with its class", payload.ProposedMemory)
	}
	if len(payload.ActiveMemories) != 2 {
		t.Fatalf("active=%d, want both rows as supplied", len(payload.ActiveMemories))
	}
	classes := map[string]string{}
	for _, row := range payload.ActiveMemories {
		classes[row.ID] = row.ProfileClass
		if row.ProfileClass == "" {
			t.Fatalf("row %s carries no profile class", row.ID)
		}
	}
	if classes["m-teach"] != string(model.ProfileClassTeachingPreference) || classes["m-goal"] != string(model.ProfileClassGoal) {
		t.Fatalf("classes=%v, want each row's own slot", classes)
	}
	// The rules themselves: duplicate is scope-first, independence is
	// different-scope-first, and neither collapses into the other.
	rules := chat.messages[0].Content
	for _, fragment := range []string{
		"same applicability scope",
		"Different wording, a different example or a different order never makes it distinct",
		"genuinely different scope",
		"a shared scope alone does not make a duplicate",
	} {
		if !strings.Contains(rules, fragment) {
			t.Fatalf("prompt rules missing %q", fragment)
		}
	}
}
