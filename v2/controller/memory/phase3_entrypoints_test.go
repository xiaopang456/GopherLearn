package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"GopherAI/common/code"
	"GopherAI/controller"
	memorydao "GopherAI/dao/memory"
	messagedao "GopherAI/dao/message"
	sessiondao "GopherAI/dao/session"
	"GopherAI/internal/testdb"
	"GopherAI/model"
	memoryservice "GopherAI/service/memory"

	"github.com/gin-gonic/gin"
	mysqldriver "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// 第三阶段批次 2「入口接线」验收。既有验收（v2/service/memory/
// phase3_batch2_acceptance_test.go）从 FinalizeRunner.RunSession 服务层直接
// 进入，已验证草稿落库、来源字段与水位规则；这里从触发侧的两个真实入口各跑
// 一次——学习端发出的 HTTP 收尾通知（POST /agent/sessions/:id/finalize，走
// 真实 FinalizeSession 控制器与默认 runner）和服务端空闲扫描兜底
//（FinalizeIdleSweeper，真实 ListDueFinalizeSessions 查询）——证明：
//   两条入口都落到待确认草稿，记忆页可见；
//   重复触发不重复出卡（通知重发、扫描反复执行都算重试）；
//   收尾时仍在生成的会话：已落库前缀照常出卡、水位停在当时最新消息，
//   生成完成后的后续触发补齐尾巴且不为已覆盖区间重复出卡。
// 提取模型用确定性替身（scriptedEntryProposer），其余全部真实实现，零模型调用。

type entryProbe struct {
	db         *gorm.DB
	repo       *memorydao.Repository
	governance *memoryservice.GovernanceService
	runner     *memoryservice.FinalizeRunner
	proposer   *scriptedEntryProposer
	userID     int64
	engine     *gin.Engine
}

type scriptedEntryProposer struct {
	proposals []memoryservice.SessionRawProposal
}

func (p *scriptedEntryProposer) Propose(ctx context.Context, input *memoryservice.SessionExtractionInput) ([]memoryservice.SessionRawProposal, error) {
	return p.proposals, nil
}

// independentRelations stubs the global classifier: every new proposal is
// independent, which is what these scenarios need (dedup behaviour itself is
// covered by the service-layer acceptance).
type independentRelations struct{}

func (r *independentRelations) Classify(ctx context.Context, incoming memoryservice.ExplicitMemoryInput, existing []model.LongTermMemory) (memoryservice.GlobalRelationResult, error) {
	return memoryservice.GlobalRelationResult{Relation: memoryservice.GlobalRelationIndependent, Reason: "entry acceptance stub"}, nil
}

func newEntryProbe(t *testing.T) *entryProbe {
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
	probe := &entryProbe{
		db:     db,
		repo:   repo,
		userID: time.Now().UnixNano() % 1_000_000_000,
	}
	probe.proposer = &scriptedEntryProposer{}
	governance, err := memoryservice.NewExplicitGovernanceService(repo, nil, memoryservice.DefaultDreamConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := governance.UseGlobalRelations(repo, &independentRelations{}); err != nil {
		t.Fatal(err)
	}
	if err := governance.UseTopicRelations(memoryservice.NewDeterministicTopicRelationResolver(nil)); err != nil {
		t.Fatal(err)
	}
	probe.governance = governance
	catalogue, err := memoryservice.NewTopicCatalogue(nil, memoryservice.MemoryTopicAdapter{Repository: repo}, 100)
	if err != nil {
		t.Fatal(err)
	}
	finalizeService, err := memoryservice.BuildSessionFinalizeService(
		sessiondao.NewRepository(db), messagedao.NewRepository(db), repo, catalogue,
		probe.proposer, &independentRelations{}, memoryservice.NewDeterministicTopicRelationResolver(nil))
	if err != nil {
		t.Fatal(err)
	}
	probe.runner, err = memoryservice.NewFinalizeRunner(finalizeService, governance, repo)
	if err != nil {
		t.Fatal(err)
	}
	// 生产装配同构：控制器读默认 runner（finalize）与默认 governance（记忆页）。
	if err := memoryservice.SetDefaultGovernanceService(governance); err != nil {
		t.Fatal(err)
	}
	if err := memoryservice.SetDefaultFinalizeRunner(probe.runner); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = memoryservice.SetDefaultFinalizeRunner(nil) })

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		c.Set("userID", probe.userID)
		c.Next()
	})
	// 与 v2/router/Agent.go 相同的路径与处理器——入口接线本身就是被测对象。
	engine.POST("/agent/sessions/:id/finalize", FinalizeSession)
	engine.GET("/agent/memories", List)
	probe.engine = engine

	t.Cleanup(func() {
		db.Unscoped().Where("user_id = ?", probe.userID).Delete(&model.MemoryDraft{})
		db.Unscoped().Where("user_id = ?", probe.userID).Delete(&model.MemoryRevision{})
		db.Unscoped().Where("user_id = ?", probe.userID).Delete(&model.LongTermMemory{})
		db.Unscoped().Where("user_id = ?", probe.userID).Delete(&model.SessionFinalizeProgress{})
		db.Unscoped().Where("user_id = ?", probe.userID).Delete(&model.SessionWhiteboard{})
		db.Unscoped().Where("user_id = ?", probe.userID).Delete(&model.Message{})
		db.Unscoped().Where("user_id = ?", probe.userID).Delete(&model.Session{})
		db.Unscoped().Where("user_id = ?", probe.userID).Delete(&model.TopicProgress{})
	})
	return probe
}

func (p *entryProbe) postFinalize(t *testing.T, sessionID string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/agent/sessions/"+sessionID+"/finalize", nil)
	rec := httptest.NewRecorder()
	p.engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("finalize http=%d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		controller.Response
		SessionID        string `json:"session_id"`
		CoveredMessageID uint   `json:"covered_message_id"`
		Advanced         bool   `json:"advanced"`
		NoNewContent     bool   `json:"no_new_content"`
		PersistedCount   int    `json:"persisted"`
		FailedCount      int    `json:"failed"`
		RetryPending     int    `json:"retry_pending"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode finalize response: %v", err)
	}
	if body.StatusCode != code.CodeSuccess {
		t.Fatalf("finalize code=%v msg=%q", body.StatusCode, body.StatusMsg)
	}
	raw := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &raw)
	raw["__persisted"] = float64(body.PersistedCount)
	raw["__failed"] = float64(body.FailedCount)
	raw["__retry_pending"] = float64(body.RetryPending)
	raw["__advanced"] = body.Advanced
	raw["__no_new_content"] = body.NoNewContent
	raw["__covered"] = float64(body.CoveredMessageID)
	return raw
}

func (p *entryProbe) expectCount(t *testing.T, body map[string]any, key string, want int) {
	t.Helper()
	got, _ := body[key].(float64)
	if int(got) != want {
		t.Fatalf("finalize %s=%v, want %d (body=%v)", key, body[key], want, body)
	}
}

// pendingViaList reads the memory page through the real List handler.
func (p *entryProbe) pendingViaList(t *testing.T) []map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/agent/memories", nil)
	rec := httptest.NewRecorder()
	p.engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list http=%d", rec.Code)
	}
	var body struct {
		PendingDrafts []map[string]any `json:"pendingDrafts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	return body.PendingDrafts
}

func (p *entryProbe) watermark(ctx context.Context, sessionID string) uint {
	value, err := p.repo.GetFinalizeProgress(ctx, p.userID, sessionID)
	if err != nil {
		return 0
	}
	return value
}

func (p *entryProbe) seedSession(t *testing.T, ctx context.Context, sessionID string, board *model.SessionWhiteboardDocument, rows []SessionSeedMessage) []model.Message {
	t.Helper()
	if err := p.db.WithContext(ctx).Create(&model.Session{
		ID: sessionID, UserID: p.userID, UserName: "finalize-entry", Mode: model.SessionModeNormal,
		Title: "入口验收会话", CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}).Error; err != nil {
		t.Fatalf("seed session: %v", err)
	}
	stored := make([]model.Message, 0, len(rows))
	for _, row := range rows {
		message := &model.Message{
			SessionID: sessionID, UserName: "finalize-entry", Content: row.Content, IsUser: row.IsUser,
			CreatedAt: row.CreatedAt,
		}
		if err := p.db.WithContext(ctx).Create(message).Error; err != nil {
			t.Fatalf("seed message: %v", err)
		}
		stored = append(stored, *message)
	}
	if board != nil {
		encoded, err := model.MarshalSessionWhiteboardDocument(*board)
		if err != nil {
			t.Fatalf("marshal board: %v", err)
		}
		if err := p.db.WithContext(ctx).Create(&model.SessionWhiteboard{
			UserID: p.userID, SessionID: sessionID, Content: encoded, Version: 1,
		}).Error; err != nil {
			t.Fatalf("seed board: %v", err)
		}
	}
	return stored
}

func (p *entryProbe) appendMessages(t *testing.T, ctx context.Context, sessionID string, rows []SessionSeedMessage) []model.Message {
	t.Helper()
	stored := make([]model.Message, 0, len(rows))
	for _, row := range rows {
		message := &model.Message{
			SessionID: sessionID, UserName: "finalize-entry", Content: row.Content, IsUser: row.IsUser,
			CreatedAt: row.CreatedAt,
		}
		if err := p.db.WithContext(ctx).Create(message).Error; err != nil {
			t.Fatalf("append message: %v", err)
		}
		stored = append(stored, *message)
	}
	return stored
}

func (p *entryProbe) updateBoard(t *testing.T, ctx context.Context, sessionID string, board *model.SessionWhiteboardDocument) {
	t.Helper()
	encoded, err := model.MarshalSessionWhiteboardDocument(*board)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.db.Model(&model.SessionWhiteboard{}).Where("user_id = ? AND session_id = ?", p.userID, sessionID).
		Update("content", encoded).Error; err != nil {
		t.Fatalf("update board: %v", err)
	}
}

type SessionSeedMessage struct {
	IsUser    bool
	Content   string
	CreatedAt time.Time
}

func entryBoardAt(messageID uint) *model.SessionWhiteboardDocument {
	return &model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV3,
		Constraints: []model.SessionWhiteboardEntry{{
			Key: "teaching-preference", Text: "讲新概念先举例再讲定义", Value: "先举例再讲定义",
			SourceMessageIDs: []uint{messageID}, Origin: model.SessionWhiteboardOriginUser, UpdatedAtMessageID: messageID,
		}},
	}
}

func TestFinalizeAcceptanceHTTPEntry(t *testing.T) {
	probe := newEntryProbe(t)
	ctx := context.Background()
	sessionID := "fin-entry-http"
	stored := probe.seedSession(t, ctx, sessionID, nil, []SessionSeedMessage{
		{IsUser: true, Content: "今天继续学 channel。", CreatedAt: time.Now().Add(-10 * time.Minute)},
		{IsUser: false, Content: "好。", CreatedAt: time.Now().Add(-9 * time.Minute)},
		{IsUser: true, Content: "以后讲新概念先举一个我熟悉的例子再讲定义，比如讲缓冲就用排队打比方。", CreatedAt: time.Now().Add(-8 * time.Minute)},
		{IsUser: false, Content: "明白。", CreatedAt: time.Now().Add(-7 * time.Minute)},
		{IsUser: true, Content: "我老是搞混 select 里 default 的作用，给我出两道题。", CreatedAt: time.Now().Add(-6 * time.Minute)},
	})
	probe.updateBoard(t, ctx, sessionID, entryBoardAt(stored[2].ID))

	probe.proposer.proposals = []memoryservice.SessionRawProposal{
		{
			Scope: model.MemoryScopeGlobal, Class: model.ProfileClassTeachingPreference, Subject: "讲解偏好",
			Durability:  model.MemoryDurabilityStable,
			Content:     "讲新概念时先举一个我熟悉的例子，再讲正式定义，例如讲缓冲就用排队打比方。",
			EvidenceIDs: []uint{stored[2].ID}, StatementEvidenceID: stored[2].ID,
		},
		{
			Scope: model.MemoryScopeTopic, Kind: "understanding", TopicKey: "go-basics",
			Subject: "select 易错点", Durability: model.MemoryDurabilityStable,
			Content:     "我老是搞混 select 里 default 的作用：default 是非阻塞的兜底分支。",
			EvidenceIDs: []uint{stored[4].ID}, StatementEvidenceID: stored[4].ID,
		},
	}

	// 第一次通知：两个候选落成待确认草稿，水位推进到最新消息。
	first := probe.postFinalize(t, sessionID)
	probe.expectCount(t, first, "__persisted", 2)
	probe.expectCount(t, first, "__failed", 0)
	probe.expectCount(t, first, "__retry_pending", 0)
	if first["__advanced"] != true {
		t.Fatalf("first finalize=%v, want advanced", first)
	}
	if got := probe.watermark(ctx, sessionID); got != stored[len(stored)-1].ID {
		t.Fatalf("watermark=%d, want %d", got, stored[len(stored)-1].ID)
	}

	// 记忆页可见：来源字段随草稿展示（S1 契约）。
	drafts := probe.pendingViaList(t)
	if len(drafts) != 2 {
		t.Fatalf("pending via list=%d, want the two cards on the memory page", len(drafts))
	}
	for _, d := range drafts {
		if d["session_id"] != sessionID {
			t.Fatalf("draft source session=%v, want %q", d["session_id"], sessionID)
		}
		if d["source"] != "对话提议" {
			t.Fatalf("draft source=%v, want 对话提议", d["source"])
		}
	}

	// 通知重发（fire-and-forget 契约）：水位已覆盖，无新内容，不出第二张卡。
	retry := probe.postFinalize(t, sessionID)
	probe.expectCount(t, retry, "__persisted", 0)
	if retry["__no_new_content"] != true || retry["__advanced"] != false {
		t.Fatalf("retry finalize=%v, want no-new-content without advancing", retry)
	}
	if got := probe.watermark(ctx, sessionID); got != stored[len(stored)-1].ID {
		t.Fatalf("watermark after retry=%d, want held at %d", got, stored[len(stored)-1].ID)
	}
	if cards := probe.pendingViaList(t); len(cards) != 2 {
		t.Fatalf("pending after retry=%d, want exactly the two original cards", len(cards))
	}
	var pendingCount int64
	if err := probe.db.Model(&model.MemoryDraft{}).Where("user_id = ? AND status = ?", probe.userID, model.MemoryDraftPending).
		Count(&pendingCount).Error; err != nil {
		t.Fatal(err)
	}
	if pendingCount != 2 {
		t.Fatalf("pending rows=%d in the database", pendingCount)
	}
}

// TestFinalizeAcceptanceLongSession: 长会话（150 条消息，候选分居头尾）——
// 收尾入口经读窗口覆盖全量落库消息，两个候选各出一张卡，水位落最新消息；
// 重复通知不重复出卡。
func TestFinalizeAcceptanceLongSession(t *testing.T) {
	probe := newEntryProbe(t)
	ctx := context.Background()
	sessionID := "fin-entry-long"
	base := time.Now().Add(-90 * time.Minute)
	rows := make([]SessionSeedMessage, 0, 150)
	for i := 0; i < 150; i++ {
		isUser := i%2 == 0
		content := fmt.Sprintf("流水消息 %d（不构成候选）。", i)
		if i == 30 {
			isUser, content = true, "以后讲新概念先举一个我熟悉的例子再讲定义，比如讲缓冲就用排队打比方。"
		}
		if i == 148 {
			isUser, content = true, "我老是搞混 select 里 default 的作用：default 是非阻塞的兜底分支。"
		}
		rows = append(rows, SessionSeedMessage{IsUser: isUser, Content: content, CreatedAt: base.Add(time.Duration(i) * time.Minute)})
	}
	// 先落一行占位白板（updateBoard 需要既有行才能改写内容）。
	stored := probe.seedSession(t, ctx, sessionID, entryBoardAt(1), rows)
	probe.updateBoard(t, ctx, sessionID, entryBoardAt(stored[30].ID))

	probe.proposer.proposals = []memoryservice.SessionRawProposal{
		{
			Scope: model.MemoryScopeGlobal, Class: model.ProfileClassTeachingPreference, Subject: "讲解偏好",
			Durability:  model.MemoryDurabilityStable,
			Content:     "讲新概念时先举一个我熟悉的例子，再讲正式定义，例如讲缓冲就用排队打比方。",
			EvidenceIDs: []uint{stored[30].ID}, StatementEvidenceID: stored[30].ID,
		},
		{
			Scope: model.MemoryScopeTopic, Kind: "understanding", TopicKey: "go-basics",
			Subject: "select 易错点", Durability: model.MemoryDurabilityStable,
			Content:     "我老是搞混 select 里 default 的作用：default 是非阻塞的兜底分支。",
			EvidenceIDs: []uint{stored[148].ID}, StatementEvidenceID: stored[148].ID,
		},
	}

	first := probe.postFinalize(t, sessionID)
	probe.expectCount(t, first, "__persisted", 2)
	probe.expectCount(t, first, "__failed", 0)
	if first["__advanced"] != true {
		t.Fatalf("long-session finalize=%v, want advanced", first)
	}
	if got := probe.watermark(ctx, sessionID); got != stored[len(stored)-1].ID {
		t.Fatalf("watermark=%d, want %d (长会话水位落最新消息)", got, stored[len(stored)-1].ID)
	}
	drafts := probe.pendingViaList(t)
	if len(drafts) != 2 {
		t.Fatalf("pending=%d, want two cards from the long session", len(drafts))
	}
	for _, d := range drafts {
		if d["session_id"] != sessionID || d["source"] != "对话提议" {
			t.Fatalf("long-session draft=%v", d)
		}
	}
	retry := probe.postFinalize(t, sessionID)
	probe.expectCount(t, retry, "__persisted", 0)
	if retry["__no_new_content"] != true {
		t.Fatalf("retry=%v, want no-new-content", retry)
	}
	if cards := probe.pendingViaList(t); len(cards) != 2 {
		t.Fatalf("pending after retry=%d, want exactly the two cards", len(cards))
	}
}

func TestFinalizeAcceptanceIdleSweeperEntry(t *testing.T) {
	probe := newEntryProbe(t)
	ctx := context.Background()
	sessionID := "fin-entry-idle"
	storedAt := time.Now().Add(-2 * time.Minute) // 早于 Idle 阈值，晚于 Lookback
	stored := probe.seedSession(t, ctx, sessionID, nil, []SessionSeedMessage{
		{IsUser: true, Content: "继续 channel。", CreatedAt: storedAt},
		{IsUser: false, Content: "好。", CreatedAt: storedAt.Add(time.Minute)},
		{IsUser: true, Content: "以后讲新概念先举一个我熟悉的例子再讲定义。", CreatedAt: storedAt.Add(2 * time.Minute)},
	})
	probe.updateBoard(t, ctx, sessionID, entryBoardAt(stored[2].ID))
	probe.proposer.proposals = []memoryservice.SessionRawProposal{{
		Scope: model.MemoryScopeGlobal, Class: model.ProfileClassTeachingPreference, Subject: "讲解偏好",
		Durability:  model.MemoryDurabilityStable,
		Content:     "讲新概念时先举一个我熟悉的例子，再讲正式定义。",
		EvidenceIDs: []uint{stored[2].ID}, StatementEvidenceID: stored[2].ID,
	}}

	// 短间隔扫描：Interval 50ms、Idle 5s——种子消息时间已越过阈值。
	sweeper, err := memoryservice.NewFinalizeIdleSweeper(probe.runner, memoryservice.FinalizeSweeperConfig{
		Interval: 50 * time.Millisecond, Idle: 5 * time.Second, Lookback: 24 * time.Hour, Batch: 50,
	})
	if err != nil {
		t.Fatal(err)
	}
	sweepCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := sweeper.Start(sweepCtx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sweeper.Stop)

	// 轮询等扫描把候选落成草稿（最多 6s）。
	deadline := time.Now().Add(6 * time.Second)
	drafts := probe.pendingViaList(t)
	for len(drafts) < 1 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		drafts = probe.pendingViaList(t)
	}
	if len(drafts) != 1 {
		t.Fatalf("pending via idle sweep=%d, want the one card", len(drafts))
	}
	if drafts[0]["session_id"] != sessionID || drafts[0]["source"] != "对话提议" {
		t.Fatalf("idle draft=%v, want provenance from the swept session", drafts[0])
	}
	newest := stored[len(stored)-1].ID
	// 草稿与水位是两次提交：草稿先落库、水位后落库。看到草稿的那一刻，
	// 水位可能还没写完——这里按草稿同样的方式等水位，追到它为止，断言的
	// 仍是同一件事：扫描之后水位停在最新消息。
	watermark := probe.watermark(ctx, sessionID)
	for watermark != newest && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		watermark = probe.watermark(ctx, sessionID)
	}
	if watermark != newest {
		t.Fatalf("watermark=%d after sweep, want %d", watermark, newest)
	}

	// 扫描继续跑几个周期：水位已覆盖最新消息，不重复出卡。
	time.Sleep(400 * time.Millisecond)
	if cards := probe.pendingViaList(t); len(cards) != 1 {
		t.Fatalf("pending after repeated sweeps=%d, want exactly the one card", len(cards))
	}
	var pendingCount int64
	if err := probe.db.Model(&model.MemoryDraft{}).Where("user_id = ? AND status = ?", probe.userID, model.MemoryDraftPending).
		Count(&pendingCount).Error; err != nil {
		t.Fatal(err)
	}
	if pendingCount != 1 {
		t.Fatalf("pending rows=%d in the database after repeated sweeps", pendingCount)
	}
}

func TestFinalizeAcceptanceStillGenerating(t *testing.T) {
	probe := newEntryProbe(t)
	ctx := context.Background()
	sessionID := "fin-entry-generating"
	prefix := []SessionSeedMessage{
		{IsUser: true, Content: "继续 channel。", CreatedAt: time.Now().Add(-10 * time.Minute)},
		{IsUser: false, Content: "好。", CreatedAt: time.Now().Add(-9 * time.Minute)},
		{IsUser: true, Content: "以后讲新概念先举一个我熟悉的例子再讲定义，比如讲缓冲就用排队打比方。", CreatedAt: time.Now().Add(-8 * time.Minute)},
	}
	stored := probe.seedSession(t, ctx, sessionID, nil, prefix)
	probe.updateBoard(t, ctx, sessionID, entryBoardAt(stored[2].ID))

	probe.proposer.proposals = []memoryservice.SessionRawProposal{{
		Scope: model.MemoryScopeGlobal, Class: model.ProfileClassTeachingPreference, Subject: "讲解偏好",
		Durability:  model.MemoryDurabilityStable,
		Content:     "讲新概念时先举一个我熟悉的例子，再讲正式定义，例如讲缓冲就用排队打比方。",
		EvidenceIDs: []uint{stored[2].ID}, StatementEvidenceID: stored[2].ID,
	}}

	// 收尾通知在生成期间到达：已落库前缀照常提取，水位停在 prefix 末尾——
	// 生成中的尾部不被越过。
	first := probe.postFinalize(t, sessionID)
	probe.expectCount(t, first, "__persisted", 1)
	probe.expectCount(t, first, "__failed", 0)
	if first["__advanced"] != true {
		t.Fatalf("prefix finalize=%v, want advanced", first)
	}
	if got := probe.watermark(ctx, sessionID); got != stored[len(stored)-1].ID {
		t.Fatalf("watermark=%d, want %d (生成中的尾部不被越过)", got, stored[len(stored)-1].ID)
	}

	// 生成完成：尾巴落库，替身补上 select 理解候选；后续触发只出尾巴的卡。
	tail := probe.appendMessages(t, ctx, sessionID, []SessionSeedMessage{
		{IsUser: true, Content: "我老是搞混 select 里 default 的作用。", CreatedAt: time.Now().Add(-1 * time.Minute)},
	})
	probe.proposer.proposals = []memoryservice.SessionRawProposal{{
		Scope: model.MemoryScopeTopic, Kind: "understanding", TopicKey: "go-basics",
		Subject: "select 易错点", Durability: model.MemoryDurabilityStable,
		Content:     "我老是搞混 select 里 default 的作用：default 是非阻塞的兜底分支。",
		EvidenceIDs: []uint{tail[0].ID}, StatementEvidenceID: tail[0].ID,
	}}
	second := probe.postFinalize(t, sessionID)
	probe.expectCount(t, second, "__persisted", 1)
	if second["__advanced"] != true {
		t.Fatalf("tail finalize=%v, want advanced", second)
	}
	drafts := probe.pendingViaList(t)
	if len(drafts) != 2 {
		t.Fatalf("pending=%d, want the prefix card plus the tail card (无重复)", len(drafts))
	}
	if got := probe.watermark(ctx, sessionID); got != tail[0].ID {
		t.Fatalf("watermark=%d, want %d", got, tail[0].ID)
	}

	// 重复通知（重试）与几个空闲扫描周期都不再出卡。
	retry := probe.postFinalize(t, sessionID)
	probe.expectCount(t, retry, "__persisted", 0)
	if retry["__no_new_content"] != true {
		t.Fatalf("retry=%v, want no-new-content", retry)
	}
	sweeper, err := memoryservice.NewFinalizeIdleSweeper(probe.runner, memoryservice.FinalizeSweeperConfig{
		Interval: 40 * time.Millisecond, Idle: 5 * time.Second, Lookback: 24 * time.Hour, Batch: 50,
	})
	if err != nil {
		t.Fatal(err)
	}
	sweepCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := sweeper.Start(sweepCtx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sweeper.Stop)
	time.Sleep(300 * time.Millisecond)
	if cards := probe.pendingViaList(t); len(cards) != 2 {
		t.Fatalf("pending=%d after retry and sweep, want exactly the two cards", len(cards))
	}
	var pendingCount int64
	if err := probe.db.Model(&model.MemoryDraft{}).Where("user_id = ? AND status = ?", probe.userID, model.MemoryDraftPending).
		Count(&pendingCount).Error; err != nil {
		t.Fatal(err)
	}
	if pendingCount != 2 {
		t.Fatalf("pending rows=%d in the database", pendingCount)
	}
}
