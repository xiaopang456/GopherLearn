package memory

import (
	"GopherAI/internal/testdb"
	"GopherAI/model"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	mysqldriver "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestExplicitMemoryWriteDraftConfirmationAndDeletion(t *testing.T) {
	dsn := testdb.RequireMySQLDSN(t)
	db, err := gorm.Open(mysqldriver.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent), TranslateError: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.LongTermMemory{}, &model.MemoryRevision{}, &model.MemoryDraft{}, &model.MemoryCandidate{}); err != nil {
		t.Fatal(err)
	}
	repository := NewRepository(db)
	ctx := context.Background()
	now := time.Now().Truncate(time.Millisecond)
	userID := uniqueMemoryTestUserID()
	foreignUserID := userID + 1
	t.Cleanup(func() {
		db.Unscoped().Where("user_id IN ?", []int64{userID, foreignUserID}).Delete(&model.MemoryDraft{})
		db.Unscoped().Where("user_id IN ?", []int64{userID, foreignUserID}).Delete(&model.MemoryRevision{})
		db.Unscoped().Where("user_id IN ?", []int64{userID, foreignUserID}).Delete(&model.LongTermMemory{})
	})

	content := "用户希望先看整体调用链，再进入少量代码锚点。"
	params := ExplicitWriteParams{
		Memory:     model.LongTermMemory{UserID: userID, Scope: model.MemoryScopeGlobal, Subject: "讲解偏好", Content: content, StructuredJSON: `{}`, Tags: model.StringList{"learning"}, Durability: model.MemoryDurabilityStable, SourceRefs: model.StringList{"user_api:test"}, Confidence: 1, Importance: 8},
		RequestKey: "direct:" + uuid.NewString(), Reason: "用户在管理面板直接写入", Now: now,
	}
	first, err := repository.CreateUserMemory(ctx, params)
	if err != nil || first.Replay || !first.Memory.UserConfirmed || first.Revision.Actor != model.MemoryRevisionUser {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	replay, err := repository.CreateUserMemory(ctx, params)
	if err != nil || !replay.Replay || replay.Memory.ID != first.Memory.ID || replay.Revision.ID != first.Revision.ID {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	if foreign, err := repository.GetExplicitWrite(ctx, foreignUserID, params.RequestKey); err != nil || foreign != nil {
		t.Fatalf("foreign replay=%+v err=%v", foreign, err)
	}
	replacementContent := "用户现在希望先看异常边界，再回到整体调用链。"
	replaced, err := repository.CreateUserMemory(ctx, ExplicitWriteParams{
		Memory:         model.LongTermMemory{UserID: userID, Scope: model.MemoryScopeGlobal, Subject: "讲解偏好", Content: replacementContent, StructuredJSON: `{}`, Tags: model.StringList{"learning"}, Durability: model.MemoryDurabilityStable, SourceRefs: model.StringList{"user_api:replace"}, Confidence: 1, Importance: 8},
		TargetMemoryID: first.Memory.ID, RequestKey: "replace:" + uuid.NewString(), Reason: "用户明确选择替换", Now: now.Add(time.Second),
	})
	if err != nil || replaced.Revision.Action != model.MemoryDreamSupersede || replaced.Revision.BeforeSnapshot == "" || replaced.Revision.AfterSnapshot == "" {
		t.Fatalf("replacement=%+v err=%v", replaced, err)
	}
	var retired model.LongTermMemory
	if err := db.Where("id = ? AND user_id = ?", first.Memory.ID, userID).First(&retired).Error; err != nil || retired.Status != model.LongTermMemorySuperseded || replaced.Memory.Status != model.LongTermMemoryActive {
		t.Fatalf("retired=%+v replacement=%+v err=%v", retired, replaced.Memory, err)
	}

	draftContent := "用户在 RPC 粘包排查时先检查长度字段。"
	draft := &model.MemoryDraft{
		ID: uuid.NewString(), UserID: userID, SessionID: uuid.NewString(), RunID: uuid.NewString(), IdempotencyKey: "draft-proposal:" + uuid.NewString(),
		Scope: model.MemoryScopeTopic, Subject: "RPC 粘包排查", Content: draftContent, ContentHash: model.MemoryContentHash(draftContent), Tags: model.StringList{"rpc"}, TopicKey: "rpc-粘包",
		Durability: model.MemoryDurabilityStable, ConflictIDs: model.StringList{}, Status: model.MemoryDraftPending, CreatedAt: now, UpdatedAt: now,
	}
	stored, created, err := repository.UpsertDraft(ctx, draft)
	if err != nil || !created || stored.Status != model.MemoryDraftPending {
		t.Fatalf("draft=%+v created=%v err=%v", stored, created, err)
	}
	if _, _, err := repository.ResolveDraft(ctx, ResolveDraftParams{UserID: foreignUserID, DraftID: draft.ID, Action: "add", Reason: "foreign", Now: now.Add(time.Second)}); !errors.Is(err, ErrDraftNotFound) {
		t.Fatalf("foreign confirmation err=%v", err)
	}

	results := make([]*ExplicitWriteResult, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			_, results[index], errs[index] = repository.ResolveDraft(ctx, ResolveDraftParams{UserID: userID, DraftID: draft.ID, Action: "add", Reason: "用户确认草稿", Now: now.Add(time.Second)})
		}(i)
	}
	wg.Wait()
	for i := range results {
		if errs[i] != nil || results[i] == nil || results[i].Memory.ID == "" {
			t.Fatalf("concurrent confirmation[%d]=%+v err=%v", i, results[i], errs[i])
		}
	}
	if results[0].Memory.ID != results[1].Memory.ID || results[0].Replay == results[1].Replay {
		t.Fatalf("confirmations must share one memory and one replay: %#v %#v", results[0], results[1])
	}

	if _, err := repository.DeleteMemory(ctx, userID, first.Memory.ID, "用户删除个人偏好", now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	var leaked int64
	if err := db.Model(&model.MemoryRevision{}).Where("user_id = ? AND memory_id = ? AND (before_snapshot <> '' OR after_snapshot <> '')", userID, first.Memory.ID).Count(&leaked).Error; err != nil {
		t.Fatal(err)
	}
	if leaked != 0 {
		t.Fatalf("deleted memory leaked through %d revision snapshots", leaked)
	}
	var deleted model.LongTermMemory
	if err := db.Where("id = ? AND user_id = ?", first.Memory.ID, userID).First(&deleted).Error; err != nil {
		t.Fatal(err)
	}
	if deleted.Status != model.LongTermMemoryDeleted || deleted.Content != "" || deleted.Subject != "" {
		t.Fatalf("deleted memory retained body: %+v", deleted)
	}
}

func TestConcurrentGlobalWritesRejectStaleSemanticSnapshot(t *testing.T) {
	dsn := testdb.RequireMySQLDSN(t)
	db, err := gorm.Open(mysqldriver.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent), TranslateError: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.LongTermMemory{}, &model.MemoryRevision{}); err != nil {
		t.Fatal(err)
	}
	userID := uniqueMemoryTestUserID()
	now := time.Now().UTC().Truncate(time.Millisecond)
	repository := NewRepository(db)
	t.Cleanup(func() {
		db.Unscoped().Where("user_id = ?", userID).Delete(&model.MemoryRevision{})
		db.Unscoped().Where("user_id = ?", userID).Delete(&model.LongTermMemory{})
	})

	snapshot := GlobalSnapshotFingerprint(nil)
	params := func(content string) ExplicitWriteParams {
		return ExplicitWriteParams{
			Memory:     model.LongTermMemory{UserID: userID, Scope: model.MemoryScopeGlobal, Subject: "并发偏好", Content: content, StructuredJSON: `{}`, Durability: model.MemoryDurabilityStable, SourceRefs: model.StringList{"test:concurrent"}, Confidence: 1, Importance: 8},
			RequestKey: uuid.NewString(), Reason: "concurrent snapshot test", Now: now, GlobalSnapshotHash: snapshot,
		}
	}
	requests := []ExplicitWriteParams{params("回答先给结论。"), params("请先说最终结论。")}
	errs := make([]error, len(requests))
	var wg sync.WaitGroup
	for index := range requests {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = repository.CreateUserMemory(context.Background(), requests[i])
		}(index)
	}
	wg.Wait()
	successes, conflicts := 0, 0
	for _, writeErr := range errs {
		switch {
		case writeErr == nil:
			successes++
		case errors.Is(writeErr, ErrMemoryConflict):
			conflicts++
		default:
			t.Fatalf("unexpected write error: %v", writeErr)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("successes=%d conflicts=%d errors=%v", successes, conflicts, errs)
	}
	var active int64
	if err := db.Model(&model.LongTermMemory{}).Where("user_id = ? AND memory_scope = ? AND status = ?", userID, model.MemoryScopeGlobal, model.LongTermMemoryActive).Count(&active).Error; err != nil || active != 1 {
		t.Fatalf("active=%d err=%v", active, err)
	}
}
