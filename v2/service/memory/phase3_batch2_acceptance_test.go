package memory

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	mysqldriver "gorm.io/driver/mysql"

	memorydao "GopherAI/dao/memory"
	messagedao "GopherAI/dao/message"
	sessiondao "GopherAI/dao/session"
	"GopherAI/internal/testdb"
	"GopherAI/model"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// 第三阶段批次 2「自动提议」验收：会话收尾 → 候选落库 → 记忆页可见的正式
// 链路。提取模型用确定性替身，其余每一层——白板读取、消息读取、指纹、
// 主题目录、比较范围、草稿入口、确认事务、水位表——都是真实实现，跑在
// 专用测试库上；全程零模型调用（真实模型评测按计划归批次 3/评测窗口）。
//
// 三态与水位规则一次验完：
//   - 已保存：候选过真实 ProposeDraft 落 pending 草稿，来源字段完整落库，
//     记忆页（PendingDrafts）可见，确认后随正式记忆保留；
//   - 明确跳过：提取端带原因的拒绝、草稿入口的查重与已忽略同源抑制，
//     终态，水位向前；
//   - 待重试：判定失败保留进度，水位不动，恢复后同进度重跑只出一张卡。
//   - 白板漏记对照（§6.1）：白板漏记但原话仍在时，补读窗口找回候选。

type finalizeAcceptance struct {
	db         *gorm.DB
	repo       *memorydao.Repository
	session    *sessiondao.Repository
	messages   *messagedao.Repository
	governance *GovernanceService
	runner     *FinalizeRunner
	proposer   *scriptedProposer
	topic      *flakyTopicResolver
	global     *recordingGlobalClassifier
	userID     int64
}

// flakyTopicResolver wraps the deterministic topic resolver with a failure
// switch, so the retry state can be driven without a second stand-in type.
type flakyTopicResolver struct {
	inner    *DeterministicTopicRelationResolver
	failNext bool
	saw      [][]model.LongTermMemory
}

func (r *flakyTopicResolver) Classify(ctx context.Context, incoming ExplicitMemoryInput, existing []model.LongTermMemory) (TopicRelationResult, error) {
	r.saw = append(r.saw, existing)
	if r.failNext {
		r.failNext = false
		return TopicRelationResult{}, errors.New("topic judgement unavailable")
	}
	return r.inner.Classify(ctx, incoming, existing)
}

// recordingGlobalClassifier wraps the deterministic global classifier and
// records the rows each judgement saw, which is how the 同类收窄 assertion
// reads the rule's own view.
type recordingGlobalClassifier struct {
	inner *deterministicGlobalClassifier
	saw   [][]model.LongTermMemory
}

func (c *recordingGlobalClassifier) Classify(ctx context.Context, incoming ExplicitMemoryInput, existing []model.LongTermMemory) (GlobalRelationResult, error) {
	c.saw = append(c.saw, existing)
	return c.inner.Classify(ctx, incoming, existing)
}

func newFinalizeAcceptance(t *testing.T) *finalizeAcceptance {
	t.Helper()
	dsn := testdb.RequireMySQLDSN(t)
	db, err := gorm.Open(mysqldriver.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent), TranslateError: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.LongTermMemory{}, &model.MemoryRevision{}, &model.MemoryDraft{}, &model.MemoryCandidate{},
		&model.TopicProgress{}, &model.Session{}, &model.Message{}, &model.SessionWhiteboard{}, &model.SessionFinalizeProgress{}); err != nil {
		t.Fatal(err)
	}
	repo := memorydao.NewRepository(db)
	acc := &finalizeAcceptance{
		db:       db,
		repo:     repo,
		session:  sessiondao.NewRepository(db),
		messages: messagedao.NewRepository(db),
		userID:   uniqueFinalizeUserID(),
	}
	acc.topic = &flakyTopicResolver{inner: NewDeterministicTopicRelationResolver(nil)}
	acc.global = &recordingGlobalClassifier{inner: &deterministicGlobalClassifier{}}
	global := acc.global
	governance, err := NewExplicitGovernanceService(repo, nil, DefaultDreamConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := governance.UseGlobalRelations(repo, global); err != nil {
		t.Fatal(err)
	}
	if err := governance.UseTopicRelations(acc.topic); err != nil {
		t.Fatal(err)
	}
	acc.governance = governance
	catalogue, err := NewTopicCatalogue(nil, MemoryTopicAdapter{Repository: repo}, 100)
	if err != nil {
		t.Fatal(err)
	}
	acc.proposer = &scriptedProposer{}
	finalizeService, err := BuildSessionFinalizeService(
		acc.session, acc.messages, repo, catalogue,
		acc.proposer, global, acc.topic)
	if err != nil {
		t.Fatal(err)
	}
	acc.runner, err = NewFinalizeRunner(finalizeService, governance, repo)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Unscoped().Where("user_id = ?", acc.userID).Delete(&model.MemoryDraft{})
		db.Unscoped().Where("user_id = ?", acc.userID).Delete(&model.MemoryRevision{})
		db.Unscoped().Where("user_id = ?", acc.userID).Delete(&model.LongTermMemory{})
		db.Unscoped().Where("user_id = ?", acc.userID).Delete(&model.SessionFinalizeProgress{})
		db.Unscoped().Where("user_id = ?", acc.userID).Delete(&model.SessionWhiteboard{})
		db.Unscoped().Where("user_id = ?", acc.userID).Delete(&model.Message{})
		db.Unscoped().Where("user_id = ?", acc.userID).Delete(&model.Session{})
		db.Unscoped().Where("user_id = ?", acc.userID).Delete(&model.TopicProgress{})
	})
	return acc
}

func uniqueFinalizeUserID() int64 {
	return time.Now().UnixNano() % 1_000_000_000
}

// seedSession inserts one session of the learner with its messages and its
// board, and returns the stored messages (with their real ids) plus the
// newest message id.
func (a *finalizeAcceptance) seedSession(t *testing.T, ctx context.Context, sessionID string, board *model.SessionWhiteboardDocument, rows []SessionMessage) []SessionMessage {
	t.Helper()
	if err := a.db.WithContext(ctx).Create(&model.Session{
		ID: sessionID, UserID: a.userID, UserName: "finalize-acc", Mode: model.SessionModeNormal,
		Title: "验收会话", CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}).Error; err != nil {
		t.Fatalf("seed session: %v", err)
	}
	stored := make([]SessionMessage, 0, len(rows))
	for _, row := range rows {
		message := &model.Message{
			SessionID: sessionID, UserName: "finalize-acc", Content: row.Content, IsUser: row.IsUser,
			CreatedAt: row.CreatedAt,
		}
		if err := a.db.WithContext(ctx).Create(message).Error; err != nil {
			t.Fatalf("seed message: %v", err)
		}
		stored = append(stored, SessionMessage{ID: message.ID, IsUser: row.IsUser, Content: row.Content, CreatedAt: row.CreatedAt})
	}
	if board != nil {
		encoded, err := model.MarshalSessionWhiteboardDocument(*board)
		if err != nil {
			t.Fatalf("marshal board: %v", err)
		}
		if err := a.db.WithContext(ctx).Create(&model.SessionWhiteboard{
			UserID: a.userID, SessionID: sessionID, Content: encoded, Version: 1,
		}).Error; err != nil {
			t.Fatalf("seed board: %v", err)
		}
	}
	return stored
}

func (a *finalizeAcceptance) progress(ctx context.Context, sessionID string) uint {
	value, err := a.repo.GetFinalizeProgress(ctx, a.userID, sessionID)
	if err != nil {
		return 0
	}
	return value
}

func (a *finalizeAcceptance) pendingDrafts(ctx context.Context) []model.MemoryDraft {
	drafts, err := a.governance.PendingDrafts(ctx, a.userID)
	if err != nil {
		return nil
	}
	return drafts
}

func baseFinalizeMessages() []SessionMessage {
	at := func(minute int) time.Time {
		return time.Date(2026, 11, 22, 20, minute, 0, 0, time.FixedZone("CST", 8*3600))
	}
	return []SessionMessage{
		{IsUser: true, Content: "今天继续学 channel，今晚时间不多。", CreatedAt: at(1)},
		{IsUser: false, Content: "好，按最小必要内容走。", CreatedAt: at(2)},
		{IsUser: true, Content: "以后你给我讲新的概念，都先举一个我熟悉的例子再讲定义，比如讲缓冲就用排队打比方。", CreatedAt: at(6)},
		{IsUser: false, Content: "明白，先讲无缓冲和有缓冲的区别。", CreatedAt: at(7)},
		{IsUser: true, Content: "我老是搞混 select 里 default 的作用，给我出两道题。", CreatedAt: at(36)},
	}
}

// TestFinalizeAcceptanceFullChain is the one-pass acceptance: three states,
// the watermark rules, the source fields end to end, and the §6.1 replay
// pair - all through the real draft entry on a real database.
func TestFinalizeAcceptanceFullChain(t *testing.T) {
	acc := newFinalizeAcceptance(t)
	ctx := context.Background()
	sessionID := "fin-acc-0001"

	// ---- 会话收尾 → 已保存：候选落 pending 草稿，来源字段完整，记忆页可见。
	preferenceAt := time.Date(2026, 11, 22, 20, 6, 0, 0, time.FixedZone("CST", 8*3600))
	board := &model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV3,
		Constraints: []model.SessionWhiteboardEntry{{
			Key: "teaching-preference", Text: "讲新概念先举例再讲定义", Value: "先举例再讲定义",
			Origin: model.SessionWhiteboardOriginUser, UpdatedAtMessageID: 3,
		}},
	}
	stored := acc.seedSession(t, ctx, sessionID, board, baseFinalizeMessages())
	if len(stored) != 5 {
		t.Fatalf("seeded messages=%d", len(stored))
	}
	// 白板条目的引用需要在拿到真实消息 id 之后落定。
	boardEntry := model.SessionWhiteboardEntry{
		Key: "teaching-preference", Text: "讲新概念先举例再讲定义", Value: "先举例再讲定义",
		SourceMessageIDs: []uint{stored[2].ID}, Origin: model.SessionWhiteboardOriginUser, UpdatedAtMessageID: stored[2].ID,
	}
	board.Constraints = []model.SessionWhiteboardEntry{boardEntry}
	encoded, err := model.MarshalSessionWhiteboardDocument(*board)
	if err != nil {
		t.Fatal(err)
	}
	if err := acc.db.Model(&model.SessionWhiteboard{}).Where("user_id = ? AND session_id = ?", acc.userID, sessionID).
		Update("content", encoded).Error; err != nil {
		t.Fatal(err)
	}

	acc.proposer.proposals = []SessionRawProposal{
		{
			Scope: model.MemoryScopeGlobal, Class: model.ProfileClassTeachingPreference, Subject: "讲解偏好",
			Durability:  model.MemoryDurabilityStable,
			Content:     "讲新概念时先举一个我熟悉的例子，再讲正式定义，例如讲缓冲就用排队打比方。",
			EvidenceIDs: []uint{stored[2].ID}, StatementEvidenceID: stored[2].ID,
		},
		{
			Scope: model.MemoryScopeTopic, Kind: FinalizeKindUnderstanding, TopicKey: "go-basics",
			Subject: "select 易错点", Durability: model.MemoryDurabilityStable,
			Content:     "我老是搞混 select 里 default 的作用：default 是非阻塞的兜底分支。",
			EvidenceIDs: []uint{stored[4].ID}, StatementEvidenceID: stored[4].ID,
		},
	}
	outcome, err := acc.runner.RunSession(ctx, acc.userID, sessionID)
	if err != nil {
		t.Fatalf("run session: %v", err)
	}
	if len(outcome.Persisted) != 2 || len(outcome.Failed) != 0 || len(outcome.Skipped) != 0 {
		t.Fatalf("persisted=%d failed=%+v skipped=%+v", len(outcome.Persisted), outcome.Failed, outcome.Skipped)
	}
	if !outcome.Advanced {
		t.Fatalf("advanced=false, want the watermark to move past full persistence")
	}
	newest := stored[len(stored)-1].ID
	if got := acc.progress(ctx, sessionID); got != newest {
		t.Fatalf("watermark=%d, want %d (已保存推进水位)", got, newest)
	}
	if outcome.Cursor.CoveredMessageID != newest {
		t.Fatalf("cursor=%+v, want covered=%d", outcome.Cursor, newest)
	}

	// 记忆页可见：PendingDrafts 是记忆页待确认列表的数据来源。
	drafts := acc.pendingDrafts(ctx)
	if len(drafts) != 2 {
		t.Fatalf("pending drafts=%d, want the two cards on the memory page", len(drafts))
	}
	var globalDraft, topicDraft *model.MemoryDraft
	for i := range drafts {
		switch drafts[i].Scope {
		case model.MemoryScopeGlobal:
			globalDraft = &drafts[i]
		case model.MemoryScopeTopic:
			topicDraft = &drafts[i]
		}
	}
	if globalDraft == nil || topicDraft == nil {
		t.Fatalf("draft scopes=%+v", drafts)
	}
	// 来源字段完整落库：画像类别、用户证据、主证据时间、来源会话。
	if globalDraft.ProfileClass != model.ProfileClassTeachingPreference {
		t.Fatalf("global draft class=%q", globalDraft.ProfileClass)
	}
	if len(globalDraft.EvidenceMessages) != 1 || globalDraft.EvidenceMessages[0] != fmt.Sprintf("m%d", stored[2].ID) {
		t.Fatalf("global draft evidence=%v", globalDraft.EvidenceMessages)
	}
	if globalDraft.StatementTime == nil || !globalDraft.StatementTime.Equal(preferenceAt) {
		t.Fatalf("global draft statement time=%v, want the primary evidence's time", globalDraft.StatementTime)
	}
	if globalDraft.SessionID != sessionID {
		t.Fatalf("global draft source session=%q", globalDraft.SessionID)
	}
	// 目录为空（该学习者还没有主题目录）：主题候选按计划规则待归属，
	// 由确认流程选择归属——这正是批次 1 的 pending-attribution 放宽。
	if topicDraft.TopicKey != "" || len(topicDraft.EvidenceMessages) != 1 {
		t.Fatalf("topic draft=%+v", topicDraft)
	}
	if topicDraft.StatementTime == nil {
		t.Fatalf("topic draft statement time missing")
	}

	// 主题候选先完成归属（批次 1 的编辑归属入口），再确认。
	attributed, err := acc.governance.EditDraft(ctx, EditDraftRequest{
		UserID: acc.userID, DraftID: topicDraft.ID, TopicKey: &[]string{"gorm"}[0],
	})
	if err != nil {
		t.Fatalf("attribute topic draft: %v", err)
	}
	if attributed.Draft == nil || attributed.Draft.TopicKey != "gorm" {
		t.Fatalf("attributed draft=%+v", attributed.Draft)
	}
	topicDraft = attributed.Draft

	// 确认后生效：来源字段随正式记忆保留（用户确认前正式记忆不变）。
	for _, draft := range []model.MemoryDraft{*globalDraft, *topicDraft} {
		confirmed, err := acc.governance.ConfirmDraft(ctx, ConfirmDraftRequest{
			UserID: acc.userID, DraftID: draft.ID, Action: "add", Reason: "验收确认",
		})
		if err != nil {
			t.Fatalf("confirm draft %s: %v", draft.ID, err)
		}
		if confirmed.Memory == nil || !confirmed.Memory.UserConfirmed {
			t.Fatalf("confirm result=%+v", confirmed)
		}
		if confirmed.Memory.ProfileClass != draft.ProfileClass ||
			len(confirmed.Memory.EvidenceMessages) != len(draft.EvidenceMessages) ||
			confirmed.Memory.SourceSession != draft.SessionID ||
			confirmed.Memory.StatementTime == nil {
			t.Fatalf("confirmed memory lost provenance: %+v", confirmed.Memory)
		}
	}
	if remaining := acc.pendingDrafts(ctx); len(remaining) != 0 {
		t.Fatalf("pending=%d after confirm, want the page empty", len(remaining))
	}

	// ---- 明确跳过（草稿入口查重）：与已生效条目同内容的候选被入口跳过，
	// 终态，水位向前。这条内容在本会话没有草稿（直接写入的种子），指纹层
	// 不认识它，只有入口的冲突规则认识。
	seedContent := "先看整体调用链，再进入少量代码锚点。"
	if _, err := acc.repo.CreateUserMemory(ctx, memorydao.ExplicitWriteParams{
		Memory: model.LongTermMemory{
			UserID: acc.userID, Scope: model.MemoryScopeGlobal, Subject: "讲解偏好", Content: seedContent,
			StructuredJSON: `{}`, Tags: model.StringList{"learning"}, Durability: model.MemoryDurabilityStable,
			SourceRefs: model.StringList{"user_api:seed"}, Confidence: 1, Importance: 8,
			Status: model.LongTermMemoryActive, UserConfirmed: true,
		},
		RequestKey: "finalize-acc-seed", Reason: "验收种子", Now: time.Now(),
	}); err != nil {
		t.Fatalf("seed active memory: %v", err)
	}
	acc.proposer.proposals = []SessionRawProposal{{
		Scope: model.MemoryScopeGlobal, Class: model.ProfileClassTeachingPreference, Subject: "讲解偏好",
		Durability: model.MemoryDurabilityStable,
		Content:    seedContent, EvidenceIDs: []uint{stored[2].ID}, StatementEvidenceID: stored[2].ID,
	}}
	// 造一条新消息把水位推前，这次触发才真的跑提取。
	skipMessage := acc.seedMessages(t, ctx, sessionID, []SessionMessage{
		{IsUser: true, Content: "这条也顺便说说，然后今天就到这。", CreatedAt: preferenceAt.Add(10 * time.Minute)},
	})
	skipOutcome, err := acc.runner.RunSession(ctx, acc.userID, sessionID)
	if err != nil {
		t.Fatalf("skip run: %v", err)
	}
	if skipOutcome.NoNewContent {
		t.Fatalf("the run must re-process only if progress moved; got no-new-content with a fresh proposal")
	}
	if len(skipOutcome.Skipped) != 1 || skipOutcome.Skipped[0].Reason != FinalizeSkipDuplicateExisting {
		t.Fatalf("skipped=%+v, want the free hash duplicate skip", skipOutcome.Skipped)
	}
	if len(skipOutcome.Persisted) != 0 {
		t.Fatalf("persisted=%d, want no card for the duplicate", len(skipOutcome.Persisted))
	}
	if !skipOutcome.Advanced {
		t.Fatalf("advanced=false, want the watermark to move past the explicit skip")
	}
	if got := acc.progress(ctx, sessionID); got != skipMessage[0].ID {
		t.Fatalf("watermark=%d, want %d (明确跳过也是终态)", got, skipMessage[0].ID)
	}
	// 入口层的同一条规则：绕过提取端直接对真实入口提案同一内容。
	if _, _, skipReason, err := acc.governance.ProposeDraft(ctx, DraftRequest{
		UserID: acc.userID, SessionID: sessionID, RunID: "fin-direct-dup",
		Input: ExplicitMemoryInput{
			Scope: model.MemoryScopeGlobal, Subject: "讲解偏好", Content: seedContent,
			Durability: model.MemoryDurabilityStable, IdempotencyKey: "direct-dup-check",
			ProfileClass: model.ProfileClassTeachingPreference,
		},
	}); err != nil || skipReason != SkipDuplicateExisting {
		t.Fatalf("entry duplicate: skip=%q err=%v", skipReason, err)
	}

	// ---- 同源重试只留一份：同一会话同一内容第二次触发被指纹层挡住。
	retryOutcome, err := acc.runner.RunSession(ctx, acc.userID, sessionID)
	if err != nil {
		t.Fatalf("retry run: %v", err)
	}
	if !retryOutcome.NoNewContent || retryOutcome.Advanced {
		t.Fatalf("retry=%+v, want no new content", retryOutcome)
	}

	// ---- 待重试：判定失败保留进度，水位不动；恢复后同进度重跑只出一张卡。
	newMessages := acc.seedMessages(t, ctx, sessionID, []SessionMessage{
		{IsUser: true, Content: "另外记一下：我复习 GORM 时老忘记 Unscoped 才能看到软删除的记录。", CreatedAt: preferenceAt.Add(30 * time.Minute)},
	})
	acc.proposer.proposals = []SessionRawProposal{{
		Scope: model.MemoryScopeTopic, Kind: FinalizeKindUnderstanding, TopicKey: "gorm",
		Subject: "GORM 软删除", Durability: model.MemoryDurabilityStable,
		Content:     "我老忘记 GORM 默认查询会过滤软删除记录，要用 Unscoped 才能看到。",
		EvidenceIDs: []uint{newMessages[0].ID}, StatementEvidenceID: newMessages[0].ID,
	}}
	acc.topic.failNext = true
	beforeRetry := acc.progress(ctx, sessionID)
	retryHeld, err := acc.runner.RunSession(ctx, acc.userID, sessionID)
	if err != nil {
		t.Fatalf("retry-held run: %v", err)
	}
	if len(retryHeld.RetryPending) != 1 || retryHeld.Advanced {
		t.Fatalf("retry-held=%+v, want the candidate held for retry", retryHeld)
	}
	if got := acc.progress(ctx, sessionID); got != beforeRetry {
		t.Fatalf("watermark=%d, want it held at %d (待重试保留进度)", got, beforeRetry)
	}
	if cards := acc.pendingDrafts(ctx); len(cards) != 0 {
		t.Fatalf("pending=%d, want no card while the judgement is pending", len(cards))
	}
	// 恢复后同进度重跑：判定成功，只出一张卡，水位推进。
	recovered, err := acc.runner.RunSession(ctx, acc.userID, sessionID)
	if err != nil {
		t.Fatalf("recovered run: %v", err)
	}
	if len(recovered.Persisted) != 1 || len(recovered.Skipped) != 0 || !recovered.Advanced {
		t.Fatalf("recovered=%+v", recovered)
	}
	if cards := acc.pendingDrafts(ctx); len(cards) != 1 {
		t.Fatalf("pending=%d after recovery, want exactly one card", len(cards))
	}
	if got := acc.progress(ctx, sessionID); got != newMessages[0].ID {
		t.Fatalf("watermark=%d, want %d", got, newMessages[0].ID)
	}

	// ---- 忽略同源抑制：学习者忽略这张卡后，同源重试不再出卡（提取端
	// 指纹层），入口层的 ignored_source 规则同样返回抑制。
	pending := acc.pendingDrafts(ctx)
	if _, err := acc.governance.ConfirmDraft(ctx, ConfirmDraftRequest{
		UserID: acc.userID, DraftID: pending[0].ID, Action: "cancel", Reason: "验收忽略",
	}); err != nil {
		t.Fatalf("cancel draft: %v", err)
	}
	acc.proposer.proposals = []SessionRawProposal{{
		Scope: model.MemoryScopeTopic, Kind: FinalizeKindUnderstanding, TopicKey: "gorm",
		Subject: "GORM 软删除", Durability: model.MemoryDurabilityStable,
		Content:     "我老忘记 GORM 默认查询会过滤软删除记录，要用 Unscoped 才能看到。",
		EvidenceIDs: []uint{newMessages[0].ID}, StatementEvidenceID: newMessages[0].ID,
	}}
	// 造一条新消息把水位推前，让这次触发真的跑提取。
	again := acc.seedMessages(t, ctx, sessionID, []SessionMessage{
		{IsUser: true, Content: "顺带把这条也记上：gorm 的 Unscoped。", CreatedAt: preferenceAt.Add(40 * time.Minute)},
	})
	ignoredOutcome, err := acc.runner.RunSession(ctx, acc.userID, sessionID)
	if err != nil {
		t.Fatalf("ignored run: %v", err)
	}
	if len(ignoredOutcome.Skipped) == 0 || len(ignoredOutcome.Persisted) != 0 {
		t.Fatalf("ignored=%+v, want the repeat suppressed with no card", ignoredOutcome)
	}
	if got := acc.progress(ctx, sessionID); got != again[0].ID {
		t.Fatalf("watermark=%d, want %d", got, again[0].ID)
	}
	// 入口层自己的规则也认识这个抑制：绕过提取端直接对真实入口提案。
	if _, _, skipReason, err := acc.governance.ProposeDraft(ctx, DraftRequest{
		UserID: acc.userID, SessionID: sessionID, RunID: "fin-direct",
		Input: ExplicitMemoryInput{
			Scope: model.MemoryScopeTopic, Subject: "GORM 软删除",
			Content:  "我老忘记 GORM 默认查询会过滤软删除记录，要用 Unscoped 才能看到。",
			TopicKey: "gorm", Durability: model.MemoryDurabilityStable, IdempotencyKey: "direct-ignored-check",
		},
	}); err != nil || skipReason != SkipIgnoredSource {
		t.Fatalf("entry ignored-source: skip=%q err=%v", skipReason, err)
	}

	// ---- 白板漏记对照（§6.1）：同一句偏好，白板漏记但原话仍在，补读窗口
	// 找回候选并可追溯到对应消息。
	missedSession := "fin-acc-missed"
	missedBoard := &model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV3,
		Plan: []model.SessionWhiteboardEntry{{
			Key: "practice", Text: "练两道题", State: "open",
			Origin: model.SessionWhiteboardOriginUser, UpdatedAtMessageID: 1,
		}},
	}
	missedStored := acc.seedSession(t, ctx, missedSession, missedBoard, baseFinalizeMessages())
	missedBoard.Plan[0].SourceMessageIDs = []uint{missedStored[0].ID}
	missedEncoded, err := model.MarshalSessionWhiteboardDocument(*missedBoard)
	if err != nil {
		t.Fatal(err)
	}
	if err := acc.db.Model(&model.SessionWhiteboard{}).Where("user_id = ? AND session_id = ?", acc.userID, missedSession).
		Update("content", missedEncoded).Error; err != nil {
		t.Fatal(err)
	}
	acc.proposer.proposals = []SessionRawProposal{{
		Scope: model.MemoryScopeGlobal, Class: model.ProfileClassTeachingPreference, Subject: "讲解偏好",
		Durability:  model.MemoryDurabilityStable,
		Content:     "讲新概念时先举一个我熟悉的例子，再讲正式定义。",
		EvidenceIDs: []uint{missedStored[2].ID}, StatementEvidenceID: missedStored[2].ID,
	}}
	missedOutcome, err := acc.runner.RunSession(ctx, acc.userID, missedSession)
	if err != nil {
		t.Fatalf("missed run: %v", err)
	}
	if len(missedOutcome.Persisted) != 1 {
		t.Fatalf("missed proposals=%d, want the bounded read-back to recover the candidate", len(missedOutcome.Persisted))
	}
	if missedOutcome.BoardMissing {
		t.Fatalf("board missing reported, want the whiteboard found")
	}
	if sawM03 := missedOutcome.Proposals[0].Evidence[0].MessageID == missedStored[2].ID; !sawM03 {
		t.Fatalf("candidate evidence=%+v, want traceable to the original message", missedOutcome.Proposals[0].Evidence)
	}
	if _, saw := acc.proposer.lastInput.Visible[missedStored[2].ID]; !saw {
		t.Fatalf("extraction request could not see the missed utterance, want the window to cover it")
	}

	// ---- 水位只能向前：旧值写不回去。
	if err := acc.repo.SaveFinalizeProgress(ctx, acc.userID, missedSession, 1); err != nil {
		t.Fatal(err)
	}
	if got := acc.progress(ctx, missedSession); got == 1 {
		t.Fatalf("watermark moved backwards, want it monotonic")
	}

	// ---- 全局同类收窄：判定只见同一画像槽位（或未标类别的存量行）。
	// 一条不同类别的画像条目（目标类）不进入教学偏好候选的判定视野。
	// 造一条新消息把水位推前，这次触发才真的跑提取。
	narrowMessage := acc.seedMessages(t, ctx, missedSession, []SessionMessage{
		{IsUser: true, Content: "还有，讲新东西最好配一道练习题。", CreatedAt: preferenceAt.Add(50 * time.Minute)},
	})
	if _, err := acc.repo.CreateUserMemory(ctx, memorydao.ExplicitWriteParams{
		Memory: model.LongTermMemory{
			UserID: acc.userID, Scope: model.MemoryScopeGlobal, Subject: "目标", Content: "2027 年春招目标岗位：Go 后端开发",
			StructuredJSON: `{}`, Tags: model.StringList{"project"}, Durability: model.MemoryDurabilityStable,
			ProfileClass: model.ProfileClassGoal,
			SourceRefs:   model.StringList{"user_api:seed-goal"}, Confidence: 1, Importance: 8,
			Status: model.LongTermMemoryActive, UserConfirmed: true,
		},
		RequestKey: "finalize-acc-seed-goal", Reason: "验收种子", Now: time.Now(),
	}); err != nil {
		t.Fatalf("seed goal memory: %v", err)
	}
	acc.proposer.proposals = []SessionRawProposal{{
		Scope: model.MemoryScopeGlobal, Class: model.ProfileClassTeachingPreference, Subject: "讲解偏好",
		Durability:  model.MemoryDurabilityStable,
		Content:     "讲新概念先举例，再给正式定义，例子要配一道练习题。",
		EvidenceIDs: []uint{missedStored[2].ID, narrowMessage[0].ID}, StatementEvidenceID: missedStored[2].ID,
	}}
	narrowed, err := acc.runner.RunSession(ctx, acc.userID, missedSession)
	if err != nil {
		t.Fatalf("narrowed run: %v", err)
	}
	if len(narrowed.Persisted) != 1 {
		t.Fatalf("narrowed=%+v, want the preference candidate proposed", narrowed)
	}
	if len(acc.global.saw) == 0 {
		t.Fatalf("global classifier was never called")
	}
	lastSaw := acc.global.saw[len(acc.global.saw)-1]
	for _, row := range lastSaw {
		if row.ProfileClass == model.ProfileClassGoal {
			t.Fatalf("classifier saw a goal-class row %+v, want same-class or unclassified rows only", row)
		}
	}
	foundOwnClass := false
	for _, row := range lastSaw {
		if row.ProfileClass == model.ProfileClassTeachingPreference {
			foundOwnClass = true
		}
	}
	if !foundOwnClass {
		t.Fatalf("classifier did not see the confirmed teaching preference row: %+v", lastSaw)
	}
}

// seedMessages appends messages to an existing session and returns them.
func (a *finalizeAcceptance) seedMessages(t *testing.T, ctx context.Context, sessionID string, rows []SessionMessage) []SessionMessage {
	t.Helper()
	stored := make([]SessionMessage, 0, len(rows))
	for _, row := range rows {
		message := &model.Message{
			SessionID: sessionID, UserName: "finalize-acc", Content: row.Content, IsUser: row.IsUser,
			CreatedAt: row.CreatedAt,
		}
		if err := a.db.WithContext(ctx).Create(message).Error; err != nil {
			t.Fatalf("seed message: %v", err)
		}
		stored = append(stored, SessionMessage{ID: message.ID, IsUser: row.IsUser, Content: row.Content, CreatedAt: row.CreatedAt})
	}
	return stored
}
