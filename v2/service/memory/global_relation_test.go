package memory

import (
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
	conflicts, snapshot, _, err := service.explicitConflicts(context.Background(), 7, input)
	if err != nil || len(conflicts) != 1 || conflicts[0].ID != active.ID || snapshot == "" || resolver.calls != 1 {
		t.Fatalf("conflicts=%+v snapshot=%q calls=%d err=%v", conflicts, snapshot, resolver.calls, err)
	}

	resolver.err = errors.New("provider unavailable")
	if _, _, _, err = service.explicitConflicts(context.Background(), 7, input); err == nil {
		t.Fatal("provider failure must close the write boundary")
	}

	rows := make([]model.LongTermMemory, GlobalMemoryHardLimit+1)
	for index := range rows {
		rows[index] = active
		rows[index].ID = string(rune('a' + index))
	}
	resolver.err, resolver.calls = nil, 0
	service.globalSource = globalSourceStub{rows: rows}
	if _, _, _, err = service.explicitConflicts(context.Background(), 7, input); !errors.Is(err, ErrGlobalMemoryCapacity) || resolver.calls != 0 {
		t.Fatalf("capacity err=%v classifier_calls=%d", err, resolver.calls)
	}
}

func TestExplicitGlobalExactDuplicateSkipsClassifierButBindsSnapshot(t *testing.T) {
	active := model.LongTermMemory{ID: "m1", Scope: model.MemoryScopeGlobal, Subject: "回答", Content: "先给结论", ContentHash: model.MemoryContentHash("先给结论"), Status: model.LongTermMemoryActive}
	resolver := &globalResolverStub{}
	service := &GovernanceService{explicit: &globalExplicitStub{exact: []model.LongTermMemory{active}}, globalSource: globalSourceStub{rows: []model.LongTermMemory{active}}, globalRelations: resolver}
	conflicts, snapshot, _, err := service.explicitConflicts(context.Background(), 7, ExplicitMemoryInput{Scope: model.MemoryScopeGlobal, Subject: "回答", Content: active.Content})
	if err != nil || len(conflicts) != 1 || snapshot == "" || resolver.calls != 0 {
		t.Fatalf("conflicts=%+v snapshot=%q calls=%d err=%v", conflicts, snapshot, resolver.calls, err)
	}
}
