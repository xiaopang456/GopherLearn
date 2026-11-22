package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	memorydao "GopherAI/dao/memory"
	"GopherAI/internal/testdb"
	"GopherAI/model"
	memoryservice "GopherAI/service/memory"

	mysqldriver "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/cloudwego/eino/schema"
)

// 第三阶段批次 3「实际加载」验收：抓取真实模型请求，核对候选待确认、确认
// 生效、替换之后装载的内容，并用一条可观察的回答验证记忆确实被送达并影响
// 行为。零模型付费：模型是脚本替身，它读自己刚收到的请求来决定回答顺序。
//
// 两条路径分别核验：
//   - legacy_v2（默认部署配置，config.docker.toml）：画像每轮装载，主题笔记
//     按问题词面匹配装载（该模式没有用户选择概念）；
//   - agent_memory_user_confirmed_v1（带主题选择的模式）：未选主题无主题
//     记忆，选定后只装载该主题的「我的理解」与「做题掌握度」。
//
// 真实模型对回答质量与时间语义的评测归评测窗口（计划 §6.1.2）。

// batch3Relations is the global conflict seam: the seeded content is new or
// replaces one named row, and nothing else conflicts.
type batch3Relations struct{}

func (batch3Relations) Classify(_ context.Context, incoming memoryservice.ExplicitMemoryInput, existing []model.LongTermMemory) (memoryservice.GlobalRelationResult, error) {
	for _, row := range existing {
		if strings.Contains(incoming.Content, "先举例再说定义") && strings.Contains(row.Content, "先说定义再举例") {
			return memoryservice.GlobalRelationResult{
				Relation: memoryservice.GlobalRelationReplacement, TargetMemoryID: row.ID, Reason: "batch3 replacement stub",
			}, nil
		}
	}
	return memoryservice.GlobalRelationResult{Relation: memoryservice.GlobalRelationIndependent, Reason: "batch3 independent stub"}, nil
}

type batch3LegacyFixture struct {
	db         *gorm.DB
	repo       *memorydao.Repository
	governance *memoryservice.GovernanceService
	model      *scriptedChatModel
	runner     *Runner
	userID     int64
}

func newBatch3LegacyFixture(t *testing.T) *batch3LegacyFixture {
	t.Helper()
	dsn := testdb.RequireMySQLDSN(t)
	db, err := gorm.Open(mysqldriver.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent), TranslateError: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.LongTermMemory{}, &model.MemoryRevision{}, &model.MemoryDraft{}, &model.TopicProgress{}); err != nil {
		t.Fatal(err)
	}
	repo := memorydao.NewRepository(db)
	governance, err := memoryservice.NewExplicitGovernanceService(repo, nil, memoryservice.DefaultDreamConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := governance.UseGlobalRelations(repo, batch3Relations{}); err != nil {
		t.Fatal(err)
	}
	retriever, err := memoryservice.NewRetriever(repo, nil, nil, memoryservice.DefaultRetrieverConfig())
	if err != nil {
		t.Fatal(err)
	}
	mastery := newTestMasteryProvider(t, &stubProgressReader{}, &scopedActiveLearning{})
	builder, err := NewContextBuilder(DefaultContextConfig(), stubHistory{}, nil, mastery,
		MemoryProviderFunc(func(ctx context.Context, input RunContext) (MemoryRecall, error) {
			recalled, recallErr := retriever.RelevantMemories(ctx, memoryservice.RecallRequest{
				UserID: input.UserID, SessionID: input.SessionID, Question: input.Question,
				StrongTopicKeys: input.StrongTopicKeys,
			})
			if recallErr != nil {
				return MemoryRecall{}, recallErr
			}
			return MemoryRecall{Global: recalled.Global, Topic: recalled.Topic}, nil
		}), nil)
	if err != nil {
		t.Fatal(err)
	}
	chat := &scriptedChatModel{}
	chat.respond = func(_ context.Context, _ int) (*schema.Message, error) {
		request := batch3RequestText(chat.received[len(chat.received)-1])
		if strings.Contains(request, "先举一个我熟悉的例子") || strings.Contains(request, "先举例再说定义") {
			// 记忆被送达：回答按“先举例再讲定义”的顺序。
			return schema.AssistantMessage("例子：先把 channel 想成排队打饭；定义：有缓冲 channel 是带容量的队列。", nil), nil
		}
		// 没有该记忆：回答按“先定义后例子”的默认顺序。
		return schema.AssistantMessage("定义：有缓冲 channel 是带容量的队列；例子：排队打饭。", nil), nil
	}
	runner, err := NewRunner(chat, StaticToolRegistry{}, newMemoryStepRepository(), builder, DefaultRunnerConfig())
	if err != nil {
		t.Fatal(err)
	}
	fixture := &batch3LegacyFixture{
		db: db, repo: repo, governance: governance, model: chat, runner: runner,
		userID: time.Now().UnixNano() % 1_000_000_000,
	}
	t.Cleanup(func() {
		db.Unscoped().Where("user_id = ?", fixture.userID).Delete(&model.MemoryDraft{})
		db.Unscoped().Where("user_id = ?", fixture.userID).Delete(&model.MemoryRevision{})
		db.Unscoped().Where("user_id = ?", fixture.userID).Delete(&model.LongTermMemory{})
	})
	return fixture
}

func batch3RequestText(messages []*schema.Message) string {
	var builder strings.Builder
	for _, message := range messages {
		builder.WriteString(message.Content)
		builder.WriteString("\n")
	}
	return builder.String()
}

func (f *batch3LegacyFixture) run(t *testing.T, sessionID, question string) (string, string) {
	t.Helper()
	result, err := f.runner.Run(context.Background(), RunContext{
		RunID: "run-b3-" + sessionID, UserID: f.userID, UserName: "batch3",
		SessionID: sessionID, Question: question,
	}, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return batch3RequestText(f.model.received[len(f.model.received)-1]), result.Answer
}

// TestBatch3LegacyDefaultModelRequests covers the actually delivered default
// (legacy_v2, config.docker.toml): the profile loads every turn; a pending
// candidate loads nothing; confirmation loads the new value; replacement
// retires the old value; topic notes load only when the question names the
// topic (this mode has no selection concept).
func TestBatch3LegacyDefaultModelRequests(t *testing.T) {
	fixture := newBatch3LegacyFixture(t)
	ctx := context.Background()
	sessionID := "b3-legacy"

	preference := "讲新概念时先举一个我熟悉的例子，再讲正式定义。"
	draft, created, skip, err := fixture.governance.ProposeDraft(ctx, memoryservice.DraftRequest{
		UserID: fixture.userID, SessionID: sessionID, RunID: "run-b3-seed",
		Input: memoryservice.ExplicitMemoryInput{
			Scope: model.MemoryScopeGlobal, Subject: "讲解偏好", Content: preference,
			Durability: model.MemoryDurabilityStable, IdempotencyKey: "b3:preference",
			ProfileClass: model.ProfileClassTeachingPreference,
		},
	})
	if err != nil || draft == nil || !created || skip != "" {
		t.Fatalf("seed pending draft: draft=%+v created=%v skip=%q err=%v", draft, created, skip, err)
	}

	// 待确认：请求里不应出现候选。
	request, answer := fixture.run(t, sessionID, "今天学什么？")
	if strings.Contains(request, "先举一个我熟悉的例子") {
		t.Fatalf("a pending candidate reached the model request")
	}
	if !strings.HasPrefix(answer, "定义：") {
		t.Fatalf("answer=%q, want definition-first before confirmation", answer)
	}

	// 确认后：新值进入请求，回答顺序随之改变（记忆确实被使用）。
	if _, err := fixture.governance.ConfirmDraft(ctx, memoryservice.ConfirmDraftRequest{
		UserID: fixture.userID, DraftID: draft.ID, Action: "add", Reason: "批次3确认",
	}); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	request, answer = fixture.run(t, sessionID, "今天学什么？")
	if !strings.Contains(request, "先举一个我熟悉的例子") {
		t.Fatalf("the confirmed preference is missing from the model request: %s", request)
	}
	if !strings.HasPrefix(answer, "例子：") {
		t.Fatalf("answer=%q, want example-first after confirmation", answer)
	}

	// 替换：替换前请求里是旧值，替换后只有新值。
	oldContent := "讲新概念时先说定义再举例。"
	written, err := fixture.repo.CreateUserMemory(ctx, memorydao.ExplicitWriteParams{
		Memory: model.LongTermMemory{
			UserID: fixture.userID, Scope: model.MemoryScopeGlobal, Subject: "讲解偏好", Content: oldContent,
			StructuredJSON: `{}`, Tags: model.StringList{"preference"}, Durability: model.MemoryDurabilityStable,
			SourceRefs: model.StringList{"user_api:b3-old"}, Confidence: 1, Importance: 8,
			Status: model.LongTermMemoryActive, UserConfirmed: true,
		},
		RequestKey: "b3-old", Reason: "批次3旧值", Now: time.Now(),
	})
	if err != nil {
		t.Fatalf("seed old value: %v", err)
	}
	replacement := "讲新概念时先举例再说定义。"
	replaceDraft, _, skip, err := fixture.governance.ProposeDraft(ctx, memoryservice.DraftRequest{
		UserID: fixture.userID, SessionID: sessionID, RunID: "run-b3-replace",
		Input: memoryservice.ExplicitMemoryInput{
			Scope: model.MemoryScopeGlobal, Subject: "讲解偏好", Content: replacement,
			Durability: model.MemoryDurabilityStable, IdempotencyKey: "b3:replacement",
			ProfileClass: model.ProfileClassTeachingPreference,
		},
	})
	if err != nil || replaceDraft == nil || skip != "" {
		t.Fatalf("propose replacement: draft=%+v skip=%q err=%v", replaceDraft, skip, err)
	}
	request, _ = fixture.run(t, sessionID, "今天学什么？")
	if !strings.Contains(request, oldContent) || strings.Contains(request, replacement) {
		t.Fatalf("before replacement the request must hold the old value only")
	}
	if _, err := fixture.governance.ConfirmDraft(ctx, memoryservice.ConfirmDraftRequest{
		UserID: fixture.userID, DraftID: replaceDraft.ID, Action: "replace",
		TargetMemoryID: written.Memory.ID, Reason: "批次3替换",
	}); err != nil {
		t.Fatalf("confirm replacement: %v", err)
	}
	request, _ = fixture.run(t, sessionID, "今天学什么？")
	if strings.Contains(request, oldContent) {
		t.Fatalf("the superseded value is still loaded: %s", request)
	}
	if !strings.Contains(request, replacement) {
		t.Fatalf("the replacement value is missing from the model request")
	}

	// 主题（legacy 默认）：问题点出主题时装载主题笔记，否则不装载。
	topicNote := "select 的 default 分支用于非阻塞收发。"
	if _, err := fixture.repo.CreateUserMemory(ctx, memorydao.ExplicitWriteParams{
		Memory: model.LongTermMemory{
			UserID: fixture.userID, Scope: model.MemoryScopeTopic, TopicKey: "go-concurrency",
			Subject: "select 用法", Content: topicNote, StructuredJSON: `{}`,
			Tags: model.StringList{"concurrency"}, Durability: model.MemoryDurabilityStable,
			SourceRefs: model.StringList{"user_api:b3-topic"}, Confidence: 1, Importance: 8,
			Status: model.LongTermMemoryActive, UserConfirmed: true,
		},
		RequestKey: "b3-topic", Reason: "批次3主题", Now: time.Now(),
	}); err != nil {
		t.Fatalf("seed topic note: %v", err)
	}
	request, _ = fixture.run(t, sessionID, "今天学什么？")
	if strings.Contains(request, topicNote) {
		t.Fatalf("an unrelated question loaded the topic note")
	}
	request, _ = fixture.run(t, sessionID, "go-concurrency 里的 select 怎么用？")
	if !strings.Contains(request, topicNote) {
		t.Fatalf("the named topic note is missing from the model request: %s", request)
	}
}

// TestBatch3UserConfirmedSelectionModelRequests covers the selection-enabled
// mode at the model-request level: with the switch on but no topic selected,
// no topic memory and no mastery load; selecting a topic loads exactly that
// topic's understanding and mastery beside the profile.
func TestBatch3UserConfirmedSelectionModelRequests(t *testing.T) {
	fixture := newConfirmedFixture(t, nil)
	ticket, _, err := fixture.signer.Issue(11, "s-1", "req-b3", "登录态怎么校验？", confirmedCandidates())
	if err != nil {
		t.Fatal(err)
	}
	chat := &scriptedChatModel{respond: func(context.Context, int) (*schema.Message, error) {
		return schema.AssistantMessage("好的。", nil), nil
	}}
	runner, err := NewRunner(chat, StaticToolRegistry{}, newMemoryStepRepository(), fixture.builder, userConfirmedRunnerConfig())
	if err != nil {
		t.Fatal(err)
	}
	run := func(consent TopicConsentSubmission) string {
		t.Helper()
		if _, err := runner.Run(context.Background(), RunContext{
			RunID: "run-b3-confirmed", RequestID: "req-b3", UserID: 11, UserName: "batch3",
			SessionID: "s-1", Question: "登录态怎么校验？", TopicConsent: consent,
		}, nil); err != nil {
			t.Fatalf("run: %v", err)
		}
		return batch3RequestText(chat.received[len(chat.received)-1])
	}

	// 开关开而未选：无主题记忆、无掌握度；画像照常。
	unselected := run(TopicConsentSubmission{})
	if !strings.Contains(unselected, "先说结论") {
		t.Fatalf("the profile is missing from the request")
	}
	if strings.Contains(unselected, "签名只防篡改") || strings.Contains(unselected, "已入门") {
		t.Fatalf("topic memory or mastery loaded without a selection")
	}
	if strings.Contains(unselected, "这条不该出现") {
		t.Fatalf("the legacy topic path leaked into the confirmed mode")
	}

	// 选定 jwt-鉴权：只装载该主题的两部分，与画像并列。
	selected := run(TopicConsentSubmission{Ticket: ticket, SelectedTopicKeys: []string{"jwt-鉴权"}})
	if !strings.Contains(selected, "签名只防篡改") {
		t.Fatalf("the selected topic's understanding is missing from the request")
	}
	if !strings.Contains(selected, "已入门") {
		t.Fatalf("the selected topic's mastery is missing from the request")
	}
	if !strings.Contains(selected, "先说结论") {
		t.Fatalf("the profile is missing from the selected run")
	}
	if strings.Contains(selected, "这条不该出现") {
		t.Fatalf("another topic's memory leaked into the request")
	}
}
