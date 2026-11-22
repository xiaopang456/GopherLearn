package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	memorydao "GopherAI/dao/memory"
	"GopherAI/internal/testdb"
	"GopherAI/model"

	mysqldriver "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// 第三阶段批次 1「草稿与确认」验收：规格
// tutorial/gopherlearn-optimization/r2-phase3-batch1-impl-spec.md 列出的 19 个
// 开发场景（P3D-EXT-05、P3D-TLD-06 的掌握度断言；P3D-DED-01..06；P3D-CNF-01..07；
// P3D-TLD-01..04）。固定候选 + 确定性替身，不需要任何模型调用；封存 12 场景
// 留给评测窗口。
//
// 判分对照 eval-data/phase3-memory-write/dev/expected/*.expected.json 的
// scoring.checks；本文件按场景 id 注释到每个断言块。

type stubGlobalRule struct {
	contains   string
	relation   string
	targetHint string
}

// deterministicGlobalClassifier mirrors the production classifier contract with
// keyword rules. It exists for acceptance and local verification only.
type deterministicGlobalClassifier struct {
	rules []stubGlobalRule
	calls int
}

func (c *deterministicGlobalClassifier) Classify(_ context.Context, incoming ExplicitMemoryInput, existing []model.LongTermMemory) (GlobalRelationResult, error) {
	c.calls++
	if len(existing) == 0 {
		// 与生产分类器一致：没有可对照的条目时直接独立。
		return GlobalRelationResult{Relation: GlobalRelationIndependent, Reason: "确定性替身：无既有条目"}, nil
	}
	proposed := strings.ToLower(incoming.Subject + "\n" + incoming.Content)
	for _, rule := range c.rules {
		if rule.contains != "" && !strings.Contains(proposed, strings.ToLower(rule.contains)) {
			continue
		}
		relation := GlobalRelation(rule.relation)
		if relation != GlobalRelationIndependent && relation != GlobalRelationDuplicate && relation != GlobalRelationReplacement && relation != GlobalRelationComplementary {
			return GlobalRelationResult{}, fmt.Errorf("stub relation %q invalid", rule.relation)
		}
		result := GlobalRelationResult{Relation: relation, Reason: "确定性替身判定"}
		if relation != GlobalRelationIndependent {
			hint := strings.ToLower(rule.targetHint)
			for _, memory := range existing {
				if hint == "" || strings.Contains(strings.ToLower(memory.Subject+"\n"+memory.Content), hint) {
					result.TargetMemoryID = memory.ID
					break
				}
			}
			if result.TargetMemoryID == "" {
				return GlobalRelationResult{}, fmt.Errorf("stub rule %q matched but found no target", rule.contains)
			}
		}
		return result, nil
	}
	return GlobalRelationResult{Relation: GlobalRelationIndependent, Reason: "确定性替身默认独立"}, nil
}

type acceptanceFixture struct {
	svc    *GovernanceService
	repo   *memorydao.Repository
	db     *gorm.DB
	global *deterministicGlobalClassifier
	topic  *DeterministicTopicRelationResolver
	userID int64
}

func newAcceptanceFixture(t *testing.T, globalRules []stubGlobalRule, topicRules []DeterministicTopicRule) *acceptanceFixture {
	t.Helper()
	dsn := testdb.RequireMySQLDSN(t)
	db, err := gorm.Open(mysqldriver.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent), TranslateError: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.LongTermMemory{}, &model.MemoryRevision{}, &model.MemoryDraft{}, &model.MemoryCandidate{}, &model.TopicProgress{}); err != nil {
		t.Fatal(err)
	}
	repo := memorydao.NewRepository(db)
	global := &deterministicGlobalClassifier{rules: globalRules}
	topic := NewDeterministicTopicRelationResolver(topicRules)
	svc, err := NewExplicitGovernanceService(repo, nil, DefaultDreamConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.UseGlobalRelations(repo, global); err != nil {
		t.Fatal(err)
	}
	if err := svc.UseTopicRelations(topic); err != nil {
		t.Fatal(err)
	}
	userID := time.Now().UnixNano()
	t.Cleanup(func() {
		db.Unscoped().Where("user_id = ?", userID).Delete(&model.MemoryDraft{})
		db.Unscoped().Where("user_id = ?", userID).Delete(&model.MemoryRevision{})
		db.Unscoped().Where("user_id = ?", userID).Delete(&model.LongTermMemory{})
		db.Unscoped().Where("user_id = ?", userID).Delete(&model.TopicProgress{})
	})
	return &acceptanceFixture{svc: svc, repo: repo, db: db, global: global, topic: topic, userID: userID}
}

func (f *acceptanceFixture) seed(t *testing.T, scope model.MemoryScope, topicKey, subject, content string) *model.LongTermMemory {
	t.Helper()
	result, err := f.svc.Create(context.Background(), CreateRequest{
		UserID: f.userID,
		Input: ExplicitMemoryInput{
			Scope: scope, Subject: subject, Content: content, TopicKey: topicKey,
			Durability: model.MemoryDurabilityStable, IdempotencyKey: fmt.Sprintf("seed:%s:%d", subject, time.Now().UnixNano()),
		},
		Reason: "验收种子数据",
	})
	if err != nil || result.Memory == nil {
		t.Fatalf("seed %s: err=%v", subject, err)
	}
	return result.Memory
}

func (f *acceptanceFixture) propose(t *testing.T, sessionID, runID string, scope model.MemoryScope, topicKey, subject, content string) (*model.MemoryDraft, bool, string) {
	t.Helper()
	draft, created, skip, err := f.svc.ProposeDraft(context.Background(), DraftRequest{
		UserID: f.userID, SessionID: sessionID, RunID: runID,
		Input: ExplicitMemoryInput{
			Scope: scope, Subject: subject, Content: content, TopicKey: topicKey,
			Durability: model.MemoryDurabilityStable, IdempotencyKey: fmt.Sprintf("propose:%s:%s:%d", sessionID, runID, time.Now().UnixNano()),
		},
	})
	if err != nil {
		t.Fatalf("propose %q: err=%v", subject, err)
	}
	return draft, created, skip
}

func (f *acceptanceFixture) confirm(t *testing.T, draftID, action, targetID, reason string) (ConfirmDraftResult, error) {
	t.Helper()
	return f.svc.ConfirmDraft(context.Background(), ConfirmDraftRequest{
		UserID: f.userID, DraftID: draftID, Action: action, TargetMemoryID: targetID, Reason: reason,
	})
}

func (f *acceptanceFixture) edit(t *testing.T, draftID string, apply func(*EditDraftRequest)) (EditDraftResult, error) {
	t.Helper()
	request := EditDraftRequest{UserID: f.userID, DraftID: draftID}
	apply(&request)
	return f.svc.EditDraft(context.Background(), request)
}

func (f *acceptanceFixture) memoryByID(t *testing.T, id string) *model.LongTermMemory {
	t.Helper()
	if strings.TrimSpace(id) == "" {
		t.Fatal("memoryByID called with empty id")
	}
	row, err := f.repo.GetActiveByID(context.Background(), f.userID, id)
	if err != nil {
		t.Fatalf("load %s: err=%v", id, err)
	}
	return row
}

// loadMemoryAny loads a row regardless of status (superseded rows included).
func (f *acceptanceFixture) loadMemoryAny(t *testing.T, id string) *model.LongTermMemory {
	t.Helper()
	var row model.LongTermMemory
	if err := f.db.Where("id = ? AND user_id = ?", id, f.userID).First(&row).Error; err != nil {
		t.Fatalf("load any %s: err=%v", id, err)
	}
	return &row
}

func (f *acceptanceFixture) revisionCount(t *testing.T, memoryID string) int64 {
	t.Helper()
	var count int64
	if err := f.db.Model(&model.MemoryRevision{}).Where("user_id = ? AND memory_id = ?", f.userID, memoryID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	return count
}

func strPointer(value string) *string { return &value }

// P3D-EXT-05 / P3D-TLD-06 的批次 1 检查项：确认、替换、忽略全流程不得触碰
// 做题掌握度（硬失败探针 mastery_modification）。
func TestPhase3Batch1MasteryUnchanged(t *testing.T) {
	f := newAcceptanceFixture(t, []stubGlobalRule{{contains: "1.23", relation: "replacement", targetHint: "1.22"}}, nil)
	ctx := context.Background()
	countProgress := func() int64 {
		var count int64
		if err := f.db.Model(&model.TopicProgress{}).Where("user_id = ?", f.userID).Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		return count
	}
	before := countProgress()
	target := f.seed(t, model.MemoryScopeGlobal, "", "背景", "当前项目使用 Go 1.22")
	draft, _, skip := f.propose(t, "S-dev-EXT-05", "run-1", model.MemoryScopeGlobal, "", "背景", "当前项目使用 Go 1.23")
	if draft == nil || skip != "" {
		t.Fatalf("propose draft=%v skip=%q", draft, skip)
	}
	if _, err := f.confirm(t, draft.ID, "replace", target.ID, "用户确认更新版本"); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	ignored, _, _ := f.propose(t, "S-dev-EXT-05", "run-2", model.MemoryScopeGlobal, "", "偏好", "示例代码带单元测试")
	if _, err := f.confirm(t, ignored.ID, "cancel", "", ""); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if after := countProgress(); after != before {
		t.Fatalf("mastery rows changed: before=%d after=%d", before, after)
	}
	_ = ctx
}

// P3D-DED-01：字面重复（与已有画像条目字节相同）直接跳过，不进待确认列表，
// 正式记忆不新增。
func TestPhase3Batch1Dedup01LiteralDuplicate(t *testing.T) {
	f := newAcceptanceFixture(t, nil, nil)
	existing := f.seed(t, model.MemoryScopeGlobal, "", "教学偏好", "讲新概念先举用户熟悉的例子，再讲正式定义")
	draft, created, skip := f.propose(t, "S-dev-DED-01", "run-1", model.MemoryScopeGlobal, "", "教学偏好", "讲新概念先举用户熟悉的例子，再讲正式定义")
	if draft != nil || created || skip != SkipDuplicateExisting {
		t.Fatalf("draft=%v created=%v skip=%q", draft, created, skip)
	}
	var draftCount int64
	f.db.Model(&model.MemoryDraft{}).Where("user_id = ?", f.userID).Count(&draftCount)
	if draftCount != 0 {
		t.Fatalf("pending duplicates leaked: %d", draftCount)
	}
	loaded := f.memoryByID(t, existing.ID)
	if loaded.Status != model.LongTermMemoryActive {
		t.Fatalf("existing status=%s", loaded.Status)
	}
}

// P3D-DED-02：语义重复（不同说法同一意思，替身判 duplicate）跳过并沿用。
func TestPhase3Batch1Dedup02SemanticDuplicate(t *testing.T) {
	f := newAcceptanceFixture(t, []stubGlobalRule{{contains: "熟悉的例子", relation: "duplicate", targetHint: "讲新概念"}}, nil)
	existing := f.seed(t, model.MemoryScopeGlobal, "", "教学偏好", "讲新概念先举用户熟悉的例子，再讲正式定义")
	draft, _, skip := f.propose(t, "S-dev-DED-02", "run-1", model.MemoryScopeGlobal, "", "教学偏好", "以后讲新东西先拿个我熟悉的例子开头，讲完例子再给正式定义")
	if draft != nil || skip != SkipDuplicateExisting {
		t.Fatalf("draft=%v skip=%q", draft, skip)
	}
	if f.global.calls == 0 {
		t.Fatal("classifier was not consulted")
	}
	loaded := f.memoryByID(t, existing.ID)
	if loaded.Status != model.LongTermMemoryActive {
		t.Fatalf("existing status=%s", loaded.Status)
	}
}

// P3D-DED-03：适用场景不同（替身判 independent）提议新增，与旧条目并存。
func TestPhase3Batch1Dedup03Coexist(t *testing.T) {
	f := newAcceptanceFixture(t, []stubGlobalRule{{contains: "验证步骤", relation: "independent"}}, nil)
	existing := f.seed(t, model.MemoryScopeGlobal, "", "教学偏好", "讲新概念先举用户熟悉的例子，再讲正式定义")
	draft, created, skip := f.propose(t, "S-dev-DED-03", "run-1", model.MemoryScopeGlobal, "", "排查偏好", "排查代码问题先给能自行运行的验证步骤，再定位")
	if draft == nil || !created || skip != "" {
		t.Fatalf("draft=%v created=%v skip=%q", draft, created, skip)
	}
	if len(draft.ConflictIDs) != 0 {
		t.Fatalf("independent proposal must not carry conflicts: %v", draft.ConflictIDs)
	}
	confirmed, err := f.confirm(t, draft.ID, "add", "", "用户确认并存")
	if err != nil || confirmed.Memory == nil {
		t.Fatalf("confirm err=%v", err)
	}
	if got := f.memoryByID(t, existing.ID); got.Status != model.LongTermMemoryActive {
		t.Fatalf("old entry status=%s", got.Status)
	}
	if got := f.memoryByID(t, confirmed.Memory.ID); got.Status != model.LongTermMemoryActive {
		t.Fatalf("new entry status=%s", got.Status)
	}
}

// P3D-DED-04：同源重试只留一份草稿；保存后再次提案按已生效条目跳过。
func TestPhase3Batch1Dedup04RetryCoalesce(t *testing.T) {
	f := newAcceptanceFixture(t, nil, nil)
	first, created, skip := f.propose(t, "S-dev-DED-04", "run-1", model.MemoryScopeTopic, "go-concurrency", "我的理解", "我学 channel 时老忘记有缓冲和无缓冲在接收方阻塞行为上的差别")
	if first == nil || !created || skip != "" {
		t.Fatalf("first draft=%v created=%v skip=%q", first, created, skip)
	}
	// 同一进度重试：同幂等键（实际由提取端稳定生成）→ 返回同一草稿。
	retrySameKey := DraftRequest{
		UserID: f.userID, SessionID: "S-dev-DED-04", RunID: "run-1",
		Input: ExplicitMemoryInput{Scope: model.MemoryScopeTopic, Subject: "我的理解", Content: first.Content, TopicKey: "go-concurrency",
			Durability: model.MemoryDurabilityStable, IdempotencyKey: first.IdempotencyKey},
	}
	coalesced, createdAgain, skipAgain, err := f.svc.ProposeDraft(context.Background(), retrySameKey)
	if err != nil || coalesced == nil || createdAgain || skipAgain != "" || coalesced.ID != first.ID {
		t.Fatalf("same-key retry draft=%v created=%v skip=%q err=%v", coalesced, createdAgain, skipAgain, err)
	}
	// 跨幂等键同内容：合并到已有 pending 草稿，不出现第二张卡。
	_, createdThird, skipThird := f.propose(t, "S-dev-DED-04", "run-2", model.MemoryScopeTopic, "go-concurrency", "我的理解", first.Content)
	if createdThird || skipThird != "" {
		t.Fatalf("cross-key retry created=%v skip=%q", createdThird, skipThird)
	}
	if _, err := f.confirm(t, first.ID, "add", "", "用户确认"); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	skipAfterSave := ""
	_, _, skipAfterSave = f.propose(t, "S-dev-DED-04", "run-3", model.MemoryScopeTopic, "go-concurrency", "我的理解", first.Content)
	if skipAfterSave != SkipDuplicateExisting {
		t.Fatalf("post-save re-propose skip=%q", skipAfterSave)
	}
}

// P3D-DED-05：新会话重新表达已忽略内容按正常流程提议；同源重试仍抑制。
func TestPhase3Batch1Dedup05IgnoreSuppressionIsSourceScoped(t *testing.T) {
	f := newAcceptanceFixture(t, nil, nil)
	content := "讲 channel 前先复习上一次的示例代码"
	old, created, skip := f.propose(t, "S-1101", "run-old", model.MemoryScopeGlobal, "", "教学偏好", content)
	if old == nil || !created || skip != "" {
		t.Fatalf("old draft=%v created=%v skip=%q", old, created, skip)
	}
	if _, err := f.confirm(t, old.ID, "cancel", "", ""); err != nil {
		t.Fatalf("ignore: %v", err)
	}
	// 新会话重新表达：不受历史忽略压制，正常起草稿。
	restated, createdNew, skipNew := f.propose(t, "S-dev-DED-05", "run-new", model.MemoryScopeGlobal, "", "教学偏好", content)
	if restated == nil || !createdNew || skipNew != "" {
		t.Fatalf("restated draft=%v created=%v skip=%q", restated, createdNew, skipNew)
	}
	if restated.ID == old.ID {
		t.Fatal("restated proposal must be a new draft, not the ignored one")
	}
	// 本次会话再次忽略后，同源重试被抑制。
	if _, err := f.confirm(t, restated.ID, "cancel", "", ""); err != nil {
		t.Fatalf("ignore again: %v", err)
	}
	_, _, skipRetry := f.propose(t, "S-dev-DED-05", "run-new-2", model.MemoryScopeGlobal, "", "教学偏好", content)
	if skipRetry != SkipIgnoredSource {
		t.Fatalf("same-source retry skip=%q", skipRetry)
	}
	// 历史草稿保持 ignored，不被复活。
	stored, err := f.repo.GetDraft(context.Background(), f.userID, old.ID)
	if err != nil || stored.Status != model.MemoryDraftCancelled {
		t.Fatalf("old draft status=%s err=%v", stored.Status, err)
	}
	var memoryCount int64
	f.db.Model(&model.LongTermMemory{}).Where("user_id = ?", f.userID).Count(&memoryCount)
	if memoryCount != 0 {
		t.Fatalf("ignored content leaked into memory: %d", memoryCount)
	}
}

// P3D-DED-06：跨主题相似条目不参与去重；各自主题各自保存。
func TestPhase3Batch1Dedup06CrossTopicNotCompared(t *testing.T) {
	f := newAcceptanceFixture(t, nil, nil)
	f.seed(t, model.MemoryScopeTopic, "redis-basic", "我的理解", "Redis 的 String 底层是 SDS，长度信息在头部，取 len 是 O(1)")
	draft, created, skip := f.propose(t, "S-dev-DED-06", "run-1", model.MemoryScopeTopic, "go-basics", "我的理解", "我容易把 Go 字符串头和 Redis SDS 的头结构记混；两者取长度都是 O(1)")
	if draft == nil || !created || skip != "" {
		t.Fatalf("draft=%v created=%v skip=%q", draft, created, skip)
	}
	if _, err := f.confirm(t, draft.ID, "add", "", "用户确认"); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	var count int64
	f.db.Model(&model.LongTermMemory{}).Where("user_id = ? AND memory_scope = ?", f.userID, model.MemoryScopeTopic).Count(&count)
	if count != 2 {
		t.Fatalf("topic notes=%d, want one per topic", count)
	}
}

// P3D-CNF-01：同一事项新值建议替换；编辑正文后按最终内容复核仍为替换；
// 替换与标已取代同事务；重复提交幂等。
func TestPhase3Batch1Cnf01ReplaceEditReplay(t *testing.T) {
	f := newAcceptanceFixture(t, []stubGlobalRule{{contains: "1.23", relation: "replacement", targetHint: "1.22"}}, nil)
	old := f.seed(t, model.MemoryScopeGlobal, "", "背景", "当前项目使用 Go 1.22，示例代码也按 1.22 给")
	draft, _, skip := f.propose(t, "S-dev-CNF-01", "run-1", model.MemoryScopeGlobal, "", "背景", "当前项目使用 Go 1.23，示例代码按 1.23 给")
	if draft == nil || skip != "" || len(draft.ConflictIDs) != 1 || draft.ConflictIDs[0] != old.ID {
		t.Fatalf("draft=%v skip=%q conflicts=%v", draft, skip, draft.ConflictIDs)
	}
	// 编辑正文补充限定，复核仍指向同一替换目标。
	edited, err := f.edit(t, draft.ID, func(request *EditDraftRequest) {
		request.Content = strPointer("当前项目使用 Go 1.23，示例代码按 1.23 给（含 CI 构建镜像）")
	})
	if err != nil || edited.Closed {
		t.Fatalf("edit err=%v closed=%v", err, edited.Closed)
	}
	if len(edited.Targets) != 1 || edited.Targets[0].ID != old.ID {
		t.Fatalf("edited targets=%+v", edited.Targets)
	}
	if edited.Draft.ContentHash != model.MemoryContentHash("当前项目使用 Go 1.23，示例代码按 1.23 给（含 CI 构建镜像）") {
		t.Fatal("edited content hash not refreshed")
	}
	result, err := f.confirm(t, draft.ID, "replace", old.ID, "用户明确确认替换版本")
	if err != nil || result.Memory == nil {
		t.Fatalf("confirm err=%v", err)
	}
	if got := f.loadMemoryAny(t, old.ID); got.Status != model.LongTermMemorySuperseded {
		t.Fatalf("old status=%s", got.Status)
	}
	if got := f.memoryByID(t, result.Memory.ID); got.Status != model.LongTermMemoryActive || !strings.Contains(got.Content, "CI 构建镜像") {
		t.Fatalf("new content=%q status=%s", got.Content, got.Status)
	}
	// supersede 修订挂在替换后的新条目上（MemoryID=新条目，BeforeSnapshot=旧条目）。
	if f.revisionCount(t, result.Memory.ID) != 1 {
		t.Fatalf("supersede revision count=%d, want 1", f.revisionCount(t, result.Memory.ID))
	}
	// 重复提交：返回同一结果，不产生第二条记录或修订。
	replay, replayErr := f.confirm(t, draft.ID, "replace", old.ID, "用户明确确认替换版本")
	if replayErr != nil || !replay.Replay || replay.Memory.ID != result.Memory.ID || replay.Revision.ID != result.Revision.ID {
		t.Fatalf("replay=%+v err=%v", replay, replayErr)
	}
	if f.revisionCount(t, result.Memory.ID) != 1 {
		t.Fatal("replay wrote an extra revision")
	}
}

// P3D-CNF-02：忽略不写入且不再提示；未确认草稿保持 pending；确认前正式记忆
// 保持原值。
func TestPhase3Batch1Cnf02IgnoreAndPending(t *testing.T) {
	f := newAcceptanceFixture(t, []stubGlobalRule{{contains: "1.23", relation: "replacement", targetHint: "1.22"}}, nil)
	old := f.seed(t, model.MemoryScopeGlobal, "", "背景", "当前项目使用 Go 1.22")
	preference, _, _ := f.propose(t, "S-dev-CNF-02", "run-1", model.MemoryScopeGlobal, "", "教学偏好", "示例代码尽量带单元测试，别只给实现")
	version, _, _ := f.propose(t, "S-dev-CNF-02", "run-1", model.MemoryScopeGlobal, "", "背景", "当前项目使用 Go 1.23")
	if _, err := f.confirm(t, preference.ID, "cancel", "", ""); err != nil {
		t.Fatalf("ignore: %v", err)
	}
	// 忽略后：无写入；pending 的替换草稿保持可见；正式记忆保持原值。
	var activeCount int64
	f.db.Model(&model.LongTermMemory{}).Where("user_id = ? AND status = ?", f.userID, model.LongTermMemoryActive).Count(&activeCount)
	if activeCount != 1 {
		t.Fatalf("ignored content written: active=%d", activeCount)
	}
	stored, err := f.repo.GetDraft(context.Background(), f.userID, version.ID)
	if err != nil || stored.Status != model.MemoryDraftPending {
		t.Fatalf("pending draft status=%s err=%v", stored.Status, err)
	}
	ignored, err := f.repo.GetDraft(context.Background(), f.userID, preference.ID)
	if err != nil || ignored.Status != model.MemoryDraftCancelled {
		t.Fatalf("ignored draft status=%s err=%v", ignored.Status, err)
	}
	loaded := f.memoryByID(t, old.ID)
	if !strings.Contains(loaded.Content, "1.22") {
		t.Fatalf("official memory changed: %q", loaded.Content)
	}
}

// P3D-CNF-03：迟到旧草稿的替换被阻止（目标不在当前冲突集），正式记忆保持
// 新值；放弃后无写入。
func TestPhase3Batch1Cnf03LateDraftBlocked(t *testing.T) {
	ctx := context.Background()
	f := newAcceptanceFixture(t, []stubGlobalRule{
		{contains: "1.23", relation: "replacement", targetHint: "1.22"},
		{contains: "固定在 go 1.22", relation: "replacement", targetHint: "1.23"},
	}, nil)
	oldValue := f.seed(t, model.MemoryScopeGlobal, "", "背景", "当前项目使用 Go 1.22")
	upgrade, _, _ := f.propose(t, "S-1118", "run-1", model.MemoryScopeGlobal, "", "背景", "当前项目使用 Go 1.23")
	upgradeResult, upgradeErr := f.confirm(t, upgrade.ID, "replace", oldValue.ID, "用户确认升级")
	if upgradeErr != nil || upgradeResult.Memory == nil {
		t.Fatalf("upgrade confirm err=%v", upgradeErr)
	}
	// 旧会话（2026-11-08 原话）的草稿现在才来确认：目标应是旧条目。
	late, _, lateSkip := f.propose(t, "S-1108", "run-2", model.MemoryScopeGlobal, "", "背景", "项目暂时固定在 Go 1.22")
	if late == nil || lateSkip != "" {
		t.Fatalf("late draft=%v skip=%q", late, lateSkip)
	}
	if len(late.ConflictIDs) == 0 {
		t.Fatalf("late draft conflicts=%v", late.ConflictIDs)
	}
	// 钉住的旧条目即使已被取代，也要能在用户归属校验下取回旧内容和保存时间，
	// 供用户复核迟到草稿；他人回查为空。
	history, historyErr := f.svc.MemoriesByIDs(ctx, f.userID, []string{oldValue.ID})
	if historyErr != nil || len(history) != 1 {
		t.Fatalf("pinned history rows=%d err=%v", len(history), historyErr)
	}
	if history[0].Status != model.LongTermMemorySuperseded || history[0].Content != oldValue.Content || history[0].UpdatedAt.IsZero() {
		t.Fatalf("pinned history row=%+v", history[0])
	}
	foreignHistory, foreignHistoryErr := f.svc.MemoriesByIDs(ctx, f.userID+998, []string{oldValue.ID})
	if foreignHistoryErr != nil || len(foreignHistory) != 0 {
		t.Fatalf("foreign history rows=%d err=%v", len(foreignHistory), foreignHistoryErr)
	}
	_, err := f.confirm(t, late.ID, "replace", oldValue.ID, "用户误点旧草稿")
	if !errors.Is(err, memorydao.ErrMemoryConflict) {
		t.Fatalf("late replace err=%v, want conflict block", err)
	}
	currentValue := f.memoryByID(t, upgradeResult.Memory.ID)
	if currentValue == nil || !strings.Contains(currentValue.Content, "1.23") || currentValue.Status != model.LongTermMemoryActive {
		t.Fatalf("current value disturbed: %+v", currentValue)
	}
	if _, err := f.confirm(t, late.ID, "cancel", "", ""); err != nil {
		t.Fatalf("abandon: %v", err)
	}
}

// P3D-CNF-04：确认前旧条目被手动编辑 → 事务内核验版本 → 409 + 最新内容 →
// 看过最新内容后带指纹重确认才替换。
func TestPhase3Batch1Cnf04VersionRecheck(t *testing.T) {
	f := newAcceptanceFixture(t, []stubGlobalRule{{contains: "1.23", relation: "replacement", targetHint: "1.22"}}, nil)
	old := f.seed(t, model.MemoryScopeGlobal, "", "背景", "当前项目使用 Go 1.22")
	draft, _, _ := f.propose(t, "S-1120", "run-1", model.MemoryScopeGlobal, "", "背景", "当前项目使用 Go 1.23")
	pinned := memorydao.PinnedConflictHash(*draft, old.ID)
	if pinned == "" {
		t.Fatal("draft did not pin the conflict hash")
	}
	// 确认前，用户在记忆页手动编辑旧条目（Correct）。
	if _, _, correctErr := f.svc.Correct(context.Background(), CorrectionRequest{
		UserID: f.userID, MemoryID: old.ID, Subject: "背景",
		Content: "当前项目使用 Go 1.22（仅本地开发环境；CI 已单独升级到 1.23）",
		Tags:    []string{"go"}, Reason: "用户手动修正环境说明",
	}); correctErr != nil {
		t.Fatalf("correct: %v", correctErr)
	}
	// 修订基线在 Correct 之后取：Correct 自己会写一条修订。
	revisionsBefore := f.revisionCount(t, old.ID)
	// 第一次确认：未带指纹 → 409 目标已修改，零写入。
	_, err := f.confirm(t, draft.ID, "replace", old.ID, "用户确认替换")
	if !errors.Is(err, memorydao.ErrMemoryTargetModified) {
		t.Fatalf("first confirm err=%v, want target modified", err)
	}
	if got := f.memoryByID(t, old.ID); got.Status != model.LongTermMemoryActive || !strings.Contains(got.Content, "仅本地开发环境") {
		t.Fatalf("official memory disturbed: %+v", got)
	}
	if f.revisionCount(t, old.ID) != revisionsBefore {
		t.Fatal("rejected confirm wrote a revision")
	}
	// 409 响应载荷：最新内容可见（与控制器抓取同一路径）。
	latest, err := f.svc.MemoriesByIDs(context.Background(), f.userID, []string{old.ID})
	if err != nil || len(latest) != 1 || !strings.Contains(latest[0].Content, "仅本地开发环境") {
		t.Fatalf("latest payload=%+v err=%v", latest, err)
	}
	// 重新确认：带最新指纹 → 替换针对 v2 生效，同事务标已取代。
	result, confirmErr := f.svc.ConfirmDraft(context.Background(), ConfirmDraftRequest{
		UserID: f.userID, DraftID: draft.ID, Action: "replace", TargetMemoryID: old.ID,
		Reason: "看过最新内容后仍统一到 1.23", ExpectedTargetHash: latest[0].ContentHash,
	})
	if confirmErr != nil || result.Memory == nil {
		t.Fatalf("re-confirm err=%v", confirmErr)
	}
	if got := f.loadMemoryAny(t, old.ID); got.Status != model.LongTermMemorySuperseded {
		t.Fatalf("old status=%s", got.Status)
	}
	if got := f.memoryByID(t, result.Memory.ID); got.Status != model.LongTermMemoryActive {
		t.Fatalf("new status=%s", got.Status)
	}
}

// P3D-CNF-05 烟测（请记住）：显式提议 → 草稿 → 确认 → 生效；来源引用可核；
// 同键重放不重复。
func TestPhase3Batch1Cnf05ExplicitRememberSmoke(t *testing.T) {
	f := newAcceptanceFixture(t, nil, nil)
	content := "我复习 Redis 主从复制的时候，总是记混 repl_backlog 环形缓冲区的作用"
	request := DraftRequest{
		UserID: f.userID, SessionID: "S-dev-CNF-05", RunID: "run-remember",
		Input: ExplicitMemoryInput{
			Scope: model.MemoryScopeTopic, Subject: "我的理解", Content: content, TopicKey: "redis-basic",
			Durability: model.MemoryDurabilityStable, IdempotencyKey: "agent:cnf05-fixed",
		},
	}
	draft, created, skip, err := f.svc.ProposeDraft(context.Background(), request)
	if err != nil || draft == nil || !created || skip != "" {
		t.Fatalf("propose draft=%v created=%v skip=%q err=%v", draft, created, skip, err)
	}
	if draft.TopicKey != "redis-basic" {
		t.Fatalf("topic prefill=%q", draft.TopicKey)
	}
	confirmed, confirmErr := f.confirm(t, draft.ID, "add", "", "用户在记忆页确认请记住内容")
	if confirmErr != nil || confirmed.Memory == nil {
		t.Fatalf("confirm err=%v", confirmErr)
	}
	loaded := f.memoryByID(t, confirmed.Memory.ID)
	if loaded.Status != model.LongTermMemoryActive {
		t.Fatalf("status=%s", loaded.Status)
	}
	if !strings.Contains(strings.Join([]string(loaded.SourceRefs), "|"), "memory_draft:"+draft.ID) {
		t.Fatalf("source refs=%v", loaded.SourceRefs)
	}
	// 确认后再提案同内容：内容已生效，走 duplicate 跳过分支，不产生第二份草稿
	// 或第二条记忆（pending 期间的同键重放才会返回原草稿）。
	replay, createdAgain, skipAgain, replayErr := f.svc.ProposeDraft(context.Background(), request)
	if replayErr != nil || createdAgain || replay != nil || skipAgain != SkipDuplicateExisting {
		t.Fatalf("re-propose draft=%v created=%v skip=%q err=%v", replay, createdAgain, skipAgain, replayErr)
	}
}

// P3D-CNF-06 烟测（手动录入）：手动条目即时生效；同内容自动提案按已生效
// 条目跳过。
func TestPhase3Batch1Cnf06ManualEntrySmoke(t *testing.T) {
	f := newAcceptanceFixture(t, nil, nil)
	created, err := f.svc.Create(context.Background(), CreateRequest{
		UserID: f.userID,
		Input: ExplicitMemoryInput{
			Scope: model.MemoryScopeGlobal, Subject: "教学偏好",
			Content:    "讲 Gin 校验失败时，先给一个最小可复现的 curl 例子再讲规则",
			Durability: model.MemoryDurabilityStable, IdempotencyKey: "manual:cnf06",
		},
		Reason: "用户在记忆页手动录入",
	})
	if err != nil || created.Memory == nil || !created.Memory.UserConfirmed {
		t.Fatalf("manual create err=%v confirmed=%v", err, created.Memory != nil && created.Memory.UserConfirmed)
	}
	if created.Memory.Status != model.LongTermMemoryActive {
		t.Fatalf("status=%s", created.Memory.Status)
	}
	// 收尾提取对同一内容不得重复提议（批次 1 用提案路径验证）。
	_, _, skip := f.propose(t, "S-dev-CNF-06", "run-1", model.MemoryScopeGlobal, "", "教学偏好", "讲 Gin 校验失败时，先给一个最小可复现的 curl 例子再讲规则")
	if skip != SkipDuplicateExisting {
		t.Fatalf("auto re-propose skip=%q", skip)
	}
}

// P3D-TLD-01：唯一主题预填，确认后保存到该主题。
func TestPhase3Batch1Tld01UniqueTopicPrefill(t *testing.T) {
	f := newAcceptanceFixture(t, nil, nil)
	draft, _, skip := f.propose(t, "S-dev-TLD-01", "run-1", model.MemoryScopeTopic, "go-basics", "我的理解", "把切片传进函数后 append，函数外部看不到新长度，已两次犯此错")
	if draft == nil || skip != "" || draft.TopicKey != "go-basics" {
		t.Fatalf("draft=%v skip=%q topic=%q", draft, skip, draft.TopicKey)
	}
	confirmed, confirmErr := f.confirm(t, draft.ID, "add", "", "用户确认")
	if confirmErr != nil || confirmed.Memory == nil {
		t.Fatalf("confirm err=%v", confirmErr)
	}
	loaded := f.memoryByID(t, confirmed.Memory.ID)
	if loaded.TopicKey != "go-basics" {
		t.Fatalf("saved topic=%q", loaded.TopicKey)
	}
}

// P3D-TLD-02：多主题歧义 → 待归属草稿；确认前必须归属；选定后按最终归属
// 复核；其他主题不受影响。
func TestPhase3Batch1Tld02TopicPendingAndChoice(t *testing.T) {
	f := newAcceptanceFixture(t, nil, []DeterministicTopicRule{{Contains: "分段", Relation: "independent"}})
	vectorNote := f.seed(t, model.MemoryScopeTopic, "vector-embed", "我的理解", "余弦相似度对归一化后的向量等价于点积")
	draft, created, skip := f.propose(t, "S-dev-TLD-02", "run-1", model.MemoryScopeTopic, "", "我的理解", "长文档整篇直接 embedding 检索不准；按标题分段后再嵌入明显变好")
	if draft == nil || !created || skip != "" {
		t.Fatalf("draft=%v created=%v skip=%q", draft, created, skip)
	}
	if strings.TrimSpace(draft.TopicKey) != "" {
		t.Fatalf("ambiguous draft must wait for attribution, topic=%q", draft.TopicKey)
	}
	// 未归属就确认 → 拒绝（确认接口不替用户选主题）。
	if _, err := f.confirm(t, draft.ID, "add", "", "用户确认"); !errors.Is(err, memorydao.ErrDraftAttributionRequired) {
		t.Fatalf("confirm without attribution err=%v", err)
	}
	// 用户选定 rag → 以最终归属复核冲突（rag 内无同义 → 新增）。
	edited, editErr := f.edit(t, draft.ID, func(request *EditDraftRequest) {
		request.TopicKey = strPointer("rag")
	})
	if editErr != nil || edited.Closed {
		t.Fatalf("edit err=%v closed=%v", editErr, edited.Closed)
	}
	if edited.Draft.TopicKey != "rag" {
		t.Fatalf("assigned topic=%q", edited.Draft.TopicKey)
	}
	confirmed, confirmErr := f.confirm(t, draft.ID, "add", "", "用户确认归到 RAG")
	if confirmErr != nil || confirmed.Memory == nil {
		t.Fatalf("confirm err=%v", confirmErr)
	}
	loaded := f.memoryByID(t, confirmed.Memory.ID)
	if loaded.TopicKey != "rag" {
		t.Fatalf("saved topic=%q", loaded.TopicKey)
	}
	if got := f.memoryByID(t, vectorNote.ID); got.TopicKey != "vector-embed" {
		t.Fatalf("other topic disturbed: %+v", got)
	}
}

// P3D-TLD-03：确认创建新主题——主题在用户确认后才存在（目录由记忆派生）。
func TestPhase3Batch1Tld03NewTopicAfterConfirm(t *testing.T) {
	f := newAcceptanceFixture(t, nil, nil)
	catalogueBefore, err := NewTopicCatalogue(nil, MemoryTopicAdapter{Repository: f.repo}, 100)
	if err != nil {
		t.Fatal(err)
	}
	topicsBefore, err := catalogueBefore.List(context.Background(), f.userID)
	if err != nil {
		t.Fatal(err)
	}
	for _, topic := range topicsBefore {
		if topic.Key == "etcd-discovery" {
			t.Fatal("topic must not exist before confirmation")
		}
	}
	draft, _, skip := f.propose(t, "S-dev-TLD-03", "run-1", model.MemoryScopeTopic, "etcd-discovery", "我的理解", "我容易忽略 watch 是从指定 revision 开始的，历史变更不会自动重放")
	if draft == nil || skip != "" {
		t.Fatalf("draft=%v skip=%q", draft, skip)
	}
	if _, err := f.confirm(t, draft.ID, "add", "", "用户确认并新建主题"); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	catalogueAfter, err := NewTopicCatalogue(nil, MemoryTopicAdapter{Repository: f.repo}, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	topicsAfter, listErr := catalogueAfter.List(context.Background(), f.userID)
	if listErr != nil {
		t.Fatal(listErr)
	}
	for _, topic := range topicsAfter {
		if topic.Key == "etcd-discovery" {
			found = true
		}
	}
	if !found {
		t.Fatal("confirmed topic missing from catalogue")
	}
}

// P3D-TLD-04：改归属后在最终主题下判一次语义；判同义 → 草稿自动结束，无需
// 用户放弃；无新增条目。
func TestPhase3Batch1Tld04TopicChangeSemanticDuplicateAutoClose(t *testing.T) {
	f := newAcceptanceFixture(t, nil, []DeterministicTopicRule{{Contains: "混淆切片共享底层", Relation: "duplicate", TargetHint: "深拷贝"}})
	tmGB01 := f.seed(t, model.MemoryScopeTopic, "go-basics", "我的理解", "我容易混淆切片共享底层数组和深拷贝")
	f.seed(t, model.MemoryScopeTopic, "go-concurrency", "我的理解", "select 的 default 分支用于非阻塞地尝试收发")
	draft, _, skip := f.propose(t, "S-dev-TLD-04", "run-1", model.MemoryScopeTopic, "go-concurrency", "我的理解", "并发 append 丢数据这个坑，根子还是我容易混淆切片共享底层数组和深拷贝这件事")
	if draft == nil || skip != "" {
		t.Fatalf("draft=%v skip=%q", draft, skip)
	}
	// 用户改归属为 go-basics 并保存：最终主题下判语义 → 同义 → 自动结束。
	edited, editErr := f.edit(t, draft.ID, func(request *EditDraftRequest) {
		request.TopicKey = strPointer("go-basics")
	})
	if editErr != nil {
		t.Fatalf("edit err=%v", editErr)
	}
	if !edited.Closed {
		t.Fatalf("expected auto-close, got closed=%v targets=%+v", edited.Closed, edited.Targets)
	}
	stored, err := f.repo.GetDraft(context.Background(), f.userID, draft.ID)
	if err != nil || stored.Status != model.MemoryDraftCancelled || stored.CancelReason != model.CancelReasonSemanticDuplicate {
		t.Fatalf("closed draft status=%s reason=%q err=%v", stored.Status, stored.CancelReason, err)
	}
	if len(edited.Targets) != 1 || edited.Targets[0].ID != tmGB01.ID {
		t.Fatalf("duplicate target=%+v", edited.Targets)
	}
	var count int64
	f.db.Model(&model.LongTermMemory{}).Where("user_id = ? AND topic_key = ?", f.userID, "go-basics").Count(&count)
	if count != 1 {
		t.Fatalf("go-basics notes=%d, want the original only", count)
	}
	if got := f.memoryByID(t, tmGB01.ID); got.Status != model.LongTermMemoryActive {
		t.Fatalf("existing note disturbed: %+v", got)
	}
}

// P3D-CNF-07：事务外预检通过后目标被另一标签页修改 → 事务内核对拦截 →
// 409 + 最新内容 → 正式记忆保持原值 → 重确认后才替换。
func TestPhase3Batch1Cnf07ConcurrentTargetEdit(t *testing.T) {
	f := newAcceptanceFixture(t, []stubGlobalRule{{contains: "1.23", relation: "replacement", targetHint: "1.22"}}, nil)
	old := f.seed(t, model.MemoryScopeGlobal, "", "背景", "当前项目使用 Go 1.22")
	draft, _, _ := f.propose(t, "S-1121", "run-1", model.MemoryScopeGlobal, "", "背景", "当前项目使用 Go 1.23")
	pinned := memorydao.PinnedConflictHash(*draft, old.ID)
	// 事务外预检（服务层快路径的读法）：此刻目标仍是用户看到的版本。
	seen, err := f.repo.GetActiveByID(context.Background(), f.userID, old.ID)
	if err != nil || seen.ContentHash != pinned {
		t.Fatalf("precheck hash drift: err=%v", err)
	}
	// 另一标签页在预检之后、确认事务拿行锁之前提交了修改。
	if _, _, correctErr := f.svc.Correct(context.Background(), CorrectionRequest{
		UserID: f.userID, MemoryID: old.ID, Subject: "背景",
		Content: "当前项目使用 Go 1.22（仅本地开发环境；CI 已单独升级到 1.23）",
		Tags:    []string{"go"}, Reason: "另一标签页的用户修正",
	}); correctErr != nil {
		t.Fatalf("concurrent correct: %v", correctErr)
	}
	// 修订基线在并发 Correct 之后取。
	revisionsBefore := f.revisionCount(t, old.ID)
	// 第一次确认：预检结果已过期，事务内锁行核验拦截，零写入。
	_, confirmErr := f.confirm(t, draft.ID, "replace", old.ID, "用户确认替换")
	if !errors.Is(confirmErr, memorydao.ErrMemoryTargetModified) {
		t.Fatalf("confirm err=%v, want target modified", confirmErr)
	}
	current := f.memoryByID(t, old.ID)
	if current.Status != model.LongTermMemoryActive || !strings.Contains(current.Content, "仅本地开发环境") {
		t.Fatalf("official memory disturbed: %+v", current)
	}
	if f.revisionCount(t, old.ID) != revisionsBefore {
		t.Fatal("rejected confirm wrote a revision")
	}
	stored, err := f.repo.GetDraft(context.Background(), f.userID, draft.ID)
	if err != nil || stored.Status != model.MemoryDraftPending {
		t.Fatalf("draft status=%s err=%v", stored.Status, err)
	}
	// 看过最新内容后携带最新指纹重试 → 替换成功，同事务标已取代。
	result, retryErr := f.svc.ConfirmDraft(context.Background(), ConfirmDraftRequest{
		UserID: f.userID, DraftID: draft.ID, Action: "replace", TargetMemoryID: old.ID,
		Reason: "看过最新内容后仍统一到 1.23", ExpectedTargetHash: current.ContentHash,
	})
	if retryErr != nil || result.Memory == nil {
		t.Fatalf("retry err=%v", retryErr)
	}
	if got := f.loadMemoryAny(t, old.ID); got.Status != model.LongTermMemorySuperseded || !strings.Contains(got.Content, "仅本地开发环境") {
		t.Fatalf("superseded row=%+v", got)
	}
	if got := f.memoryByID(t, result.Memory.ID); got.Status != model.LongTermMemoryActive {
		t.Fatalf("new status=%s", got.Status)
	}
}

// 自动候选字段契约（批次 1）：全局画像类别、用户证据消息、被采纳的助手消息、
// 支撑当前值的原话时间——候选写入草稿、读取可见、确认后随正式记忆保留；
// 待归属主题允许空 topic_key；旧草稿与手动路径零值兼容。
func TestPhase3Batch1CandidateProvenanceContract(t *testing.T) {
	f := newAcceptanceFixture(t, []stubGlobalRule{{contains: "1.23", relation: "replacement", targetHint: "1.22"}}, []DeterministicTopicRule{{Contains: "分段", Relation: "independent"}})
	ctx := context.Background()
	statementAt := time.Date(2026, 11, 20, 20, 6, 0, 0, time.Local)

	// 1) 候选 → 草稿：四字段完整落库。
	draftInput := DraftRequest{
		UserID: f.userID, SessionID: "S-dev-EXT-01", RunID: "run-1",
		Input: ExplicitMemoryInput{
			Scope: model.MemoryScopeGlobal, Subject: "教学偏好",
			Content:    "讲新概念先举用户熟悉的例子，再讲正式定义",
			Durability: model.MemoryDurabilityStable, IdempotencyKey: "contract:global",
			ProfileClass:     model.ProfileClassTeachingPreference,
			EvidenceMessages: []string{"m03", " m05 "}, AssistantMessages: []string{"m02"},
			StatementTime: &statementAt,
		},
	}
	draft, created, skip, err := f.svc.ProposeDraft(ctx, draftInput)
	if err != nil || draft == nil || !created || skip != "" {
		t.Fatalf("propose draft=%v created=%v skip=%q err=%v", draft, created, skip, err)
	}
	if draft.ProfileClass != model.ProfileClassTeachingPreference {
		t.Fatalf("draft class=%q", draft.ProfileClass)
	}
	if len(draft.EvidenceMessages) != 2 || draft.EvidenceMessages[1] != "m05" {
		t.Fatalf("evidence=%v", draft.EvidenceMessages)
	}
	if len(draft.AssistantMessages) != 1 || draft.AssistantMessages[0] != "m02" {
		t.Fatalf("assistant=%v", draft.AssistantMessages)
	}
	if draft.StatementTime == nil || !draft.StatementTime.Equal(statementAt) {
		t.Fatalf("statement time=%v", draft.StatementTime)
	}
	// 2) 待确认列表读取：字段可见（记忆页草稿卡展示来源与原话）。
	pending, err := f.svc.PendingDrafts(ctx, f.userID)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending=%d err=%v", len(pending), err)
	}
	if pending[0].ProfileClass != model.ProfileClassTeachingPreference || len(pending[0].EvidenceMessages) != 2 || pending[0].StatementTime == nil {
		t.Fatalf("pending provenance lost: %+v", pending[0])
	}
	// 3) 编辑正文：来源字段保留（编辑只改内容与归属）。
	edited, editErr := f.edit(t, draft.ID, func(request *EditDraftRequest) {
		request.Content = strPointer("讲新概念先举用户熟悉的例子，再讲正式定义；例子优先于推导")
	})
	if editErr != nil || edited.Closed {
		t.Fatalf("edit err=%v closed=%v", editErr, edited.Closed)
	}
	if edited.Draft.ProfileClass != model.ProfileClassTeachingPreference || len(edited.Draft.EvidenceMessages) != 2 || edited.Draft.StatementTime == nil {
		t.Fatalf("edited provenance lost: %+v", edited.Draft)
	}
	// 4) 确认：四字段随正式记忆保留。
	confirmed, confirmErr := f.confirm(t, draft.ID, "add", "", "用户确认")
	if confirmErr != nil || confirmed.Memory == nil {
		t.Fatalf("confirm err=%v", confirmErr)
	}
	memory := f.memoryByID(t, confirmed.Memory.ID)
	if memory.ProfileClass != model.ProfileClassTeachingPreference || len(memory.EvidenceMessages) != 2 || len(memory.AssistantMessages) != 1 || memory.StatementTime == nil || !memory.StatementTime.Equal(statementAt) {
		t.Fatalf("memory provenance lost: class=%q evidence=%v assistant=%v stmt=%v", memory.ProfileClass, memory.EvidenceMessages, memory.AssistantMessages, memory.StatementTime)
	}

	// 5) 替换流：新值带新的原话时间；被取代的旧行保留自己的旧来源。
	oldStatement := time.Date(2026, 10, 6, 20, 30, 0, 0, time.Local)
	oldSeed, seedErr := f.svc.Create(ctx, CreateRequest{
		UserID: f.userID,
		Input: ExplicitMemoryInput{
			Scope: model.MemoryScopeGlobal, Subject: "背景", Content: "当前项目使用 Go 1.22",
			Durability: model.MemoryDurabilityStable, IdempotencyKey: "contract:old",
			ProfileClass: model.ProfileClassBackground, EvidenceMessages: []string{"m12"}, StatementTime: &oldStatement,
		},
		Reason: "验收种子：带来源的旧值",
	})
	if seedErr != nil || oldSeed.Memory == nil {
		t.Fatalf("seed old: %v", seedErr)
	}
	newStatement := time.Date(2026, 11, 20, 20, 1, 0, 0, time.Local)
	newDraft, _, _, proposeErr := f.svc.ProposeDraft(ctx, DraftRequest{
		UserID: f.userID, SessionID: "S-dev-CNF-01", RunID: "run-2",
		Input: ExplicitMemoryInput{
			Scope: model.MemoryScopeGlobal, Subject: "背景", Content: "当前项目使用 Go 1.23",
			Durability: model.MemoryDurabilityStable, IdempotencyKey: "contract:new",
			ProfileClass: model.ProfileClassBackground, EvidenceMessages: []string{"m11"}, StatementTime: &newStatement,
		},
	})
	if proposeErr != nil || newDraft == nil {
		t.Fatalf("propose new: %v", proposeErr)
	}
	replaced, replaceErr := f.svc.ConfirmDraft(ctx, ConfirmDraftRequest{
		UserID: f.userID, DraftID: newDraft.ID, Action: "replace", TargetMemoryID: oldSeed.Memory.ID, Reason: "用户确认替换版本",
	})
	if replaceErr != nil || replaced.Memory == nil {
		t.Fatalf("replace err=%v", replaceErr)
	}
	newRow := f.memoryByID(t, replaced.Memory.ID)
	if newRow.StatementTime == nil || !newRow.StatementTime.Equal(newStatement) {
		t.Fatalf("new statement time=%v", newRow.StatementTime)
	}
	superseded := f.loadMemoryAny(t, oldSeed.Memory.ID)
	if superseded.Status != model.LongTermMemorySuperseded || superseded.StatementTime == nil || !superseded.StatementTime.Equal(oldStatement) {
		t.Fatalf("superseded row disturbed: status=%s stmt=%v", superseded.Status, superseded.StatementTime)
	}

	// 6) 待归属主题：空 topic_key 合法；证据字段保留；归属后确认随记忆保留。
	topicDraft, topicCreated, topicSkip, topicErr := f.svc.ProposeDraft(ctx, DraftRequest{
		UserID: f.userID, SessionID: "S-dev-TLD-02", RunID: "run-3",
		Input: ExplicitMemoryInput{
			Scope: model.MemoryScopeTopic, Subject: "我的理解",
			Content:    "长文档整篇直接 embedding 检索不准；按标题分段后再嵌入明显变好",
			Durability: model.MemoryDurabilityStable, IdempotencyKey: "contract:topic",
			EvidenceMessages: []string{"m01"}, AssistantMessages: []string{"m02-adoption"},
		},
	})
	if topicErr != nil || topicDraft == nil || !topicCreated || topicSkip != "" {
		t.Fatalf("topic propose err=%v created=%v skip=%q", topicErr, topicCreated, topicSkip)
	}
	if strings.TrimSpace(topicDraft.TopicKey) != "" || topicDraft.ProfileClass != "" || len(topicDraft.EvidenceMessages) != 1 {
		t.Fatalf("topic draft provenance: topic=%q class=%q evidence=%v", topicDraft.TopicKey, topicDraft.ProfileClass, topicDraft.EvidenceMessages)
	}
	topicEdited, topicEditErr := f.edit(t, topicDraft.ID, func(request *EditDraftRequest) {
		request.TopicKey = strPointer("rag")
	})
	if topicEditErr != nil || topicEdited.Closed || topicEdited.Draft.TopicKey != "rag" || len(topicEdited.Draft.EvidenceMessages) != 1 {
		t.Fatalf("topic assign edit err=%v closed=%v", topicEditErr, topicEdited.Closed)
	}
	topicConfirmed, topicConfirmErr := f.confirm(t, topicDraft.ID, "add", "", "用户确认归到 RAG")
	if topicConfirmErr != nil || topicConfirmed.Memory == nil {
		t.Fatalf("topic confirm err=%v", topicConfirmErr)
	}
	topicMemory := f.memoryByID(t, topicConfirmed.Memory.ID)
	if topicMemory.TopicKey != "rag" || topicMemory.ProfileClass != "" || len(topicMemory.EvidenceMessages) != 1 || len(topicMemory.AssistantMessages) != 1 {
		t.Fatalf("topic memory provenance: topic=%q class=%q evidence=%v assistant=%v", topicMemory.TopicKey, topicMemory.ProfileClass, topicMemory.EvidenceMessages, topicMemory.AssistantMessages)
	}

	// 7) 手动录入与存量兼容：手动路径不带来源字段 → 正式记忆零值；直接以旧
	// 形态落库的草稿（零值来源）确认不受影响。
	manual, manualErr := f.svc.Create(ctx, CreateRequest{
		UserID: f.userID,
		Input: ExplicitMemoryInput{
			Scope: model.MemoryScopeGlobal, Subject: "教学偏好", Content: "出题后先等用户说完思路再对答案",
			Durability: model.MemoryDurabilityStable, IdempotencyKey: "contract:manual",
		},
		Reason: "用户手动录入",
	})
	if manualErr != nil || manual.Memory == nil {
		t.Fatalf("manual create err=%v", manualErr)
	}
	if manual.Memory.ProfileClass != "" || len(manual.Memory.EvidenceMessages) != 0 || manual.Memory.StatementTime != nil {
		t.Fatalf("manual provenance must stay empty: %+v", manual.Memory)
	}
	legacyDraft := model.MemoryDraft{
		ID: "legacy-draft", UserID: f.userID, SessionID: "S-legacy", RunID: "run-legacy",
		IdempotencyKey: "contract:legacy", Scope: model.MemoryScopeGlobal, Subject: "背景", Content: "当前项目使用 Go 1.24",
		ContentHash: model.MemoryContentHash("当前项目使用 Go 1.24"), Tags: model.StringList{"go"},
		Durability: model.MemoryDurabilityStable, ConflictIDs: model.StringList{}, Status: model.MemoryDraftPending,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := f.db.Create(&legacyDraft).Error; err != nil {
		t.Fatal(err)
	}
	legacyConfirmed, legacyConfirmErr := f.confirm(t, legacyDraft.ID, "add", "", "存量草稿确认")
	if legacyConfirmErr != nil || legacyConfirmed.Memory == nil {
		t.Fatalf("legacy confirm err=%v", legacyConfirmErr)
	}
	if legacyConfirmed.Memory.ProfileClass != "" || len(legacyConfirmed.Memory.EvidenceMessages) != 0 {
		t.Fatalf("legacy memory provenance must stay empty: %+v", legacyConfirmed.Memory)
	}

	// 8) 契约校验：类别词表、主题范围禁带类别、证据列表上限。
	badClass := draftInput
	badClass.Input.IdempotencyKey = "contract:bad-class"
	badClass.Input.ProfileClass = "mood"
	if _, _, _, classErr := f.svc.ProposeDraft(ctx, badClass); classErr == nil {
		t.Fatal("unknown profile class must be rejected")
	}
	topicWithClass := DraftRequest{
		UserID: f.userID, SessionID: "S-dev-TLD-02", RunID: "run-4",
		Input: ExplicitMemoryInput{
			Scope: model.MemoryScopeTopic, Subject: "我的理解", Content: "主题范围不允许画像类别",
			Durability: model.MemoryDurabilityStable, IdempotencyKey: "contract:topic-class",
			ProfileClass: model.ProfileClassGoal,
		},
	}
	if _, _, _, classTopicErr := f.svc.ProposeDraft(ctx, topicWithClass); classTopicErr == nil {
		t.Fatal("topic scope must reject a profile class")
	}
	overflow := make([]string, 21)
	for i := range overflow {
		overflow[i] = fmt.Sprintf("m%02d", i)
	}
	overflowInput := draftInput
	overflowInput.Input.IdempotencyKey = "contract:overflow"
	overflowInput.Input.EvidenceMessages = overflow
	if _, _, _, overflowErr := f.svc.ProposeDraft(ctx, overflowInput); overflowErr == nil {
		t.Fatal("evidence list overflow must be rejected")
	}
}

// 来源契约补两处（批次 1）：确认后的正式记忆带来源会话，并可在用户归属校验
// 下回查；自动候选只有助手消息引用而缺用户证据时被拒绝。手动与存量留空兼容。
func TestPhase3Batch1SessionTracebackAndAssistantEvidenceRule(t *testing.T) {
	f := newAcceptanceFixture(t, nil, nil)
	ctx := context.Background()
	foreignUserID := f.userID + 999

	// 1) 确认写入保留来源会话；用户归属下的回查命中，他人回查为空。
	draft, created, skip, proposeErr := f.svc.ProposeDraft(ctx, DraftRequest{
		UserID: f.userID, SessionID: "S-trace", RunID: "run-1",
		Input: ExplicitMemoryInput{
			Scope: model.MemoryScopeGlobal, Subject: "教学偏好",
			Content:    "讲新概念先举用户熟悉的例子，再讲正式定义",
			Durability: model.MemoryDurabilityStable, IdempotencyKey: "trace:global",
			ProfileClass:     model.ProfileClassTeachingPreference,
			EvidenceMessages: []string{"m03"}, AssistantMessages: []string{"m02"},
		},
	})
	if proposeErr != nil || draft == nil || !created || skip != "" {
		t.Fatalf("propose err=%v created=%v skip=%q", proposeErr, created, skip)
	}
	confirmed, confirmErr := f.confirm(t, draft.ID, "add", "", "用户确认")
	if confirmErr != nil || confirmed.Memory == nil {
		t.Fatalf("confirm err=%v", confirmErr)
	}
	memory := f.memoryByID(t, confirmed.Memory.ID)
	if memory.SourceSession != "S-trace" {
		t.Fatalf("source session=%q", memory.SourceSession)
	}
	owned, ownedErr := f.svc.MemoriesBySession(ctx, f.userID, "S-trace")
	if ownedErr != nil || len(owned) != 1 || owned[0].ID != memory.ID {
		t.Fatalf("owner trace-back rows=%d err=%v", len(owned), ownedErr)
	}
	foreign, foreignErr := f.svc.MemoriesBySession(ctx, foreignUserID, "S-trace")
	if foreignErr != nil || len(foreign) != 0 {
		t.Fatalf("foreign trace-back rows=%d err=%v", len(foreign), foreignErr)
	}

	// 2) 助手消息引用缺用户证据：自动候选被拒绝，无草稿无写入。
	beforeDrafts, _ := f.svc.PendingDrafts(ctx, f.userID)
	_, _, _, assistantOnlyErr := f.svc.ProposeDraft(ctx, DraftRequest{
		UserID: f.userID, SessionID: "S-trace", RunID: "run-2",
		Input: ExplicitMemoryInput{
			Scope: model.MemoryScopeGlobal, Subject: "教学偏好",
			Content:    "学新章节前先画一张知识地图",
			Durability: model.MemoryDurabilityStable, IdempotencyKey: "trace:assistant-only",
			AssistantMessages: []string{"m09"},
		},
	})
	if assistantOnlyErr == nil {
		t.Fatal("assistant-only provenance must be rejected")
	}
	afterDrafts, _ := f.svc.PendingDrafts(ctx, f.userID)
	if len(afterDrafts) != len(beforeDrafts) {
		t.Fatalf("rejected candidate leaked a draft: before=%d after=%d", len(beforeDrafts), len(afterDrafts))
	}
	var memoryCount int64
	f.db.Model(&model.LongTermMemory{}).Where("user_id = ?", f.userID).Count(&memoryCount)
	if memoryCount != 1 {
		t.Fatalf("rejected candidate wrote memory: %d", memoryCount)
	}

	// 3) 手动录入不带来源字段：正式记忆来源为空，兼容不变。
	manual, manualErr := f.svc.Create(ctx, CreateRequest{
		UserID: f.userID,
		Input: ExplicitMemoryInput{
			Scope: model.MemoryScopeGlobal, Subject: "教学偏好", Content: "出题后先等用户说完思路再对答案",
			Durability: model.MemoryDurabilityStable, IdempotencyKey: "trace:manual",
		},
		Reason: "用户手动录入",
	})
	if manualErr != nil || manual.Memory == nil {
		t.Fatalf("manual create err=%v", manualErr)
	}
	manualRow := f.loadMemoryAny(t, manual.Memory.ID)
	if manualRow.SourceSession != "" {
		t.Fatalf("manual source session=%q", manualRow.SourceSession)
	}
	// 4) 存量形态（零来源字段）草稿确认不受新规则影响。
	legacyDraft := model.MemoryDraft{
		ID: "legacy-trace", UserID: f.userID, SessionID: "S-legacy", RunID: "run-legacy",
		IdempotencyKey: "trace:legacy", Scope: model.MemoryScopeGlobal, Subject: "背景", Content: "当前项目使用 Go 1.24",
		ContentHash: model.MemoryContentHash("当前项目使用 Go 1.24"), Tags: model.StringList{"go"},
		Durability: model.MemoryDurabilityStable, ConflictIDs: model.StringList{}, Status: model.MemoryDraftPending,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := f.db.Create(&legacyDraft).Error; err != nil {
		t.Fatal(err)
	}
	legacyConfirmed, legacyErr := f.confirm(t, legacyDraft.ID, "add", "", "存量草稿确认")
	if legacyErr != nil || legacyConfirmed.Memory == nil {
		t.Fatalf("legacy confirm err=%v", legacyErr)
	}
	if got := f.loadMemoryAny(t, legacyConfirmed.Memory.ID); got.SourceSession != "S-legacy" {
		// 存量草稿也有 session：确认带入是契约的自然结果，不破坏兼容。
		t.Fatalf("legacy source session=%q", got.SourceSession)
	}
}

// P3D-CNF-07 补充（事务内顺序）：锁定替换目标后优先核对目标版本——目标已修改
// 时返回 ErrMemoryTargetModified（409+最新内容）而不是被集合指纹的通用冲突
// 吞掉；指纹继续保护其他条目的变更。两个用例都在 DAO 事务层确定性复现。
func TestPhase3Batch1TxChecksVersionBeforeFingerprint(t *testing.T) {
	ctx := context.Background()

	// 用例一：目标被并发修改，集合指纹同样过期——版本核对优先，返回目标已修改。
	// 「仅本地开发环境」规则先行，替身对并发修正的分类是独立（其他条目存在时
	// Correct 也要过一次分类器）。
	f := newAcceptanceFixture(t, []stubGlobalRule{
		{contains: "仅本地开发环境", relation: "independent"},
		{contains: "1.23", relation: "replacement", targetHint: "1.22"},
	}, nil)
	target := f.seed(t, model.MemoryScopeGlobal, "", "背景", "当前项目使用 Go 1.22")
	f.seed(t, model.MemoryScopeGlobal, "", "教学偏好", "讲新概念先举用户熟悉的例子，再讲正式定义")
	draft, _, skip := f.propose(t, "S-1121", "run-1", model.MemoryScopeGlobal, "", "背景", "当前项目使用 Go 1.23")
	if draft == nil || skip != "" {
		t.Fatalf("draft=%v skip=%q", draft, skip)
	}
	rows, listErr := f.repo.ListAllActive(ctx, f.userID, model.MemoryScopeGlobal, 0, memorydao.GlobalMemorySnapshotLimit)
	if listErr != nil {
		t.Fatal(listErr)
	}
	staleSnapshot := memorydao.GlobalSnapshotFingerprint(rows)
	if _, _, correctErr := f.svc.Correct(ctx, CorrectionRequest{
		UserID: f.userID, MemoryID: target.ID, Subject: "背景",
		Content: "当前项目使用 Go 1.22（仅本地开发环境；CI 已单独升级到 1.23）",
		Tags:    []string{"go"}, Reason: "另一标签页的用户修正",
	}); correctErr != nil {
		t.Fatalf("concurrent correct: %v", correctErr)
	}
	_, _, resolveErr := f.repo.ResolveDraft(ctx, memorydao.ResolveDraftParams{
		UserID: f.userID, DraftID: draft.ID, Action: "replace", TargetMemoryID: target.ID,
		Reason: "用户确认替换", Now: time.Now().Add(time.Minute), GlobalSnapshotHash: staleSnapshot,
	})
	if !errors.Is(resolveErr, memorydao.ErrMemoryTargetModified) {
		t.Fatalf("tx resolve err=%v, want target modified (version check before fingerprint)", resolveErr)
	}
	stored, storedErr := f.repo.GetDraft(ctx, f.userID, draft.ID)
	if storedErr != nil || stored.Status != model.MemoryDraftPending {
		t.Fatalf("draft status=%s err=%v", stored.Status, storedErr)
	}
	current := f.loadMemoryAny(t, target.ID)
	if current.Status != model.LongTermMemoryActive || !strings.Contains(current.Content, "仅本地开发环境") {
		t.Fatalf("official memory disturbed: %+v", current)
	}

	// 用例二：目标未变、其他条目被并发修改——集合指纹仍拦截，通用冲突。
	g := newAcceptanceFixture(t, nil, nil)
	t2 := g.seed(t, model.MemoryScopeGlobal, "", "背景", "当前项目使用 Go 1.22")
	g.seed(t, model.MemoryScopeGlobal, "", "教学偏好", "讲新概念先举用户熟悉的例子，再讲正式定义")
	draft2, _, skip2 := g.propose(t, "S-ordering", "run-1", model.MemoryScopeGlobal, "", "教学偏好", "示例代码尽量带单元测试，别只给实现")
	if draft2 == nil || skip2 != "" {
		t.Fatalf("draft2=%v skip=%q", draft2, skip2)
	}
	rows2, listErr2 := g.repo.ListAllActive(ctx, g.userID, model.MemoryScopeGlobal, 0, memorydao.GlobalMemorySnapshotLimit)
	if listErr2 != nil {
		t.Fatal(listErr2)
	}
	staleSnapshot2 := memorydao.GlobalSnapshotFingerprint(rows2)
	if _, _, otherErr := g.svc.Correct(ctx, CorrectionRequest{
		UserID: g.userID, MemoryID: t2.ID, Subject: "背景",
		Content: "当前项目使用 Go 1.22（仅本地开发环境）",
		Tags:    []string{"go"}, Reason: "另一标签页改了其他条目",
	}); otherErr != nil {
		t.Fatalf("other correct: %v", otherErr)
	}
	// 目标未变：携带目标当前哈希 → 版本核对通过 → 指纹拦截其他条目的变更。
	_, _, otherResolveErr := g.repo.ResolveDraft(ctx, memorydao.ResolveDraftParams{
		UserID: g.userID, DraftID: draft2.ID, Action: "replace", TargetMemoryID: t2.ID,
		Reason: "用户确认替换", Now: time.Now().Add(time.Minute), GlobalSnapshotHash: staleSnapshot2,
		ExpectedTargetHash: model.MemoryContentHash(t2.Content),
	})
	if !errors.Is(otherResolveErr, memorydao.ErrMemoryConflict) {
		t.Fatalf("other-entry change err=%v, want fingerprint conflict", otherResolveErr)
	}
}
