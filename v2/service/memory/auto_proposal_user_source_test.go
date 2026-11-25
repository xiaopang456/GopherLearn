package memory

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"GopherAI/model"
)

// 本文件是「用户原话足以支撑草稿」来源校验的零付费回归：
//   - 用户先说、助手复述；助手解释后、用户用自己的话完整复述 —— 两条都走
//     用户来源，助手引用被丢弃，不进入 assistant_messages，也不能给主题点名；
//   - 助手新增条件被裁掉，只保留用户说过的值；
//   - 助手另提新事项、用户只说「嗯/对」仍走严格采纳校验；
//   - 草稿标题、正文、来源、主题归属与确认前不变量一并断言。

const userSourceAt = "2026-11-24T21:00:00+08:00"

func userSourceTime(t *testing.T) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339, userSourceAt)
	if err != nil {
		t.Fatal(err)
	}
	return at
}

func userSourceExtract(t *testing.T, boards FinalizeBoardSource, messages []SessionMessage, proposal SessionRawProposal) FinalizeResult {
	t.Helper()
	service := newFinalizeService(t, boards, fakeFinalizeMessages{messages: messages},
		nil, &fakeFinalizeFingerprints{known: map[string]struct{}{}}, fakeFinalizeGlobalMind{}, nil,
		&scriptedProposer{proposals: []SessionRawProposal{proposal}},
		&scriptedRelations{result: GlobalRelationResult{Relation: GlobalRelationIndependent, Reason: "user source probe"}}, 10)
	result, err := service.Extract(context.Background(), FinalizeRequest{UserID: 7, Cursor: FinalizeCursor{SessionID: "S-dev-user-source"}})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	return result
}

// 用户先说、助手复述，且白板没有额外采纳记录：用户原话足以支撑，走用户
// 来源，出卡并丢弃助手引用。
func TestAutoProposalUserSourceRestatementWithoutBoard(t *testing.T) {
	at := userSourceTime(t)
	messages := []SessionMessage{
		{ID: 1, IsUser: true, Content: "以后都用 Go 1.23。", CreatedAt: at},
		{ID: 2, IsUser: false, Content: "好，以后都用 Go 1.23。", CreatedAt: at.Add(time.Minute)},
	}
	proposal := SessionRawProposal{
		Scope: model.MemoryScopeGlobal, Class: model.ProfileClassConstraint, Subject: "Go 版本",
		Durability: model.MemoryDurabilityStable, Content: "以后都用 Go 1.23。",
		EvidenceIDs: []uint{1}, StatementEvidenceID: 1, AssistantIDs: []uint{2},
	}
	result := userSourceExtract(t, fakeFinalizeBoards{}, messages, proposal)
	if len(result.Proposals) != 1 || len(result.Skipped) != 0 {
		t.Fatalf("proposals=%d skipped=%+v, want the user-source card", len(result.Proposals), result.Skipped)
	}
	candidate := result.Proposals[0]
	if candidate.Subject != "以后都用 Go 1.23" {
		t.Fatalf("subject=%q, want the title from the user's words", candidate.Subject)
	}
	if candidate.Content != "以后都用 Go 1.23。" {
		t.Fatalf("content=%q, want the user's words", candidate.Content)
	}
	if len(candidate.AssistantUtterances) != 0 {
		t.Fatalf("assistant refs=%+v, want the redundant refs dropped", candidate.AssistantUtterances)
	}
	if len(candidate.Evidence) != 1 || candidate.Evidence[0].MessageID != 1 {
		t.Fatalf("evidence=%+v, want the user's message", candidate.Evidence)
	}
	if candidate.TopicKey != "" {
		t.Fatalf("topic=%q, want an unattributed global card", candidate.TopicKey)
	}
}

// 助手解释后，用户用自己的话完整复述：用户来源成立，不需要白板采纳证明。
func TestAutoProposalUserSourceFullRestatementAfterAssistant(t *testing.T) {
	at := userSourceTime(t)
	messages := []SessionMessage{
		{ID: 1, IsUser: true, Content: "队列的消费顺序怎么理解？", CreatedAt: at},
		{ID: 2, IsUser: false, Content: "（讲解：先入先出，先到先处理。）", CreatedAt: at.Add(time.Minute)},
		{ID: 3, IsUser: true, Content: "我理解队列就是任务排队，先到先处理。", CreatedAt: at.Add(2 * time.Minute)},
	}
	proposal := SessionRawProposal{
		Scope: model.MemoryScopeGlobal, Class: model.ProfileClassConstraint, Subject: "队列理解",
		Durability: model.MemoryDurabilityStable, Content: "队列就是任务排队，先到先处理。",
		EvidenceIDs: []uint{3}, StatementEvidenceID: 3, AssistantIDs: []uint{2},
	}
	result := userSourceExtract(t, fakeFinalizeBoards{}, messages, proposal)
	if len(result.Proposals) != 1 || len(result.Skipped) != 0 {
		t.Fatalf("proposals=%d skipped=%+v, want the user's restatement card", len(result.Proposals), result.Skipped)
	}
	candidate := result.Proposals[0]
	if candidate.Content != "我理解队列就是任务排队，先到先处理。" {
		t.Fatalf("content=%q, want the user's restatement", candidate.Content)
	}
	if candidate.Subject != "我理解队列就是任务排队" {
		t.Fatalf("subject=%q, want the first user clause", candidate.Subject)
	}
	if len(candidate.AssistantUtterances) != 0 {
		t.Fatalf("assistant refs=%+v, want the explanation dropped", candidate.AssistantUtterances)
	}
}

// 助手在同一事项里加了用户没采纳的条件：用户来源只保留用户说过的部分。
func TestAutoProposalUserSourceDropsAssistantCondition(t *testing.T) {
	at := userSourceTime(t)
	messages := []SessionMessage{
		{ID: 1, IsUser: true, Content: "以后都用 Go 1.23。", CreatedAt: at},
		{ID: 2, IsUser: false, Content: "建议同时禁用泛型。", CreatedAt: at.Add(time.Minute)},
	}
	proposal := SessionRawProposal{
		Scope: model.MemoryScopeGlobal, Class: model.ProfileClassConstraint, Subject: "Go 版本，同时禁用泛型",
		Durability: model.MemoryDurabilityStable, Content: "以后都用 Go 1.23，禁用泛型。",
		EvidenceIDs: []uint{1}, StatementEvidenceID: 1, AssistantIDs: []uint{2},
	}
	result := userSourceExtract(t, fakeFinalizeBoards{}, messages, proposal)
	if len(result.Proposals) != 1 || len(result.Skipped) != 0 {
		t.Fatalf("proposals=%d skipped=%+v, want the user-only card", len(result.Proposals), result.Skipped)
	}
	candidate := result.Proposals[0]
	if candidate.Content != "以后都用 Go 1.23。" || candidate.Subject != "以后都用 Go 1.23" {
		t.Fatalf("subject=%q content=%q, want only what the learner said", candidate.Subject, candidate.Content)
	}
	if strings.Contains(candidate.Subject, "禁用泛型") || strings.Contains(candidate.Content, "禁用泛型") {
		t.Fatalf("subject=%q content=%q, want the assistant condition out", candidate.Subject, candidate.Content)
	}
	if len(candidate.AssistantUtterances) != 0 {
		t.Fatalf("assistant refs=%+v, want the unadopted condition's message dropped", candidate.AssistantUtterances)
	}
}

// 助手另提新事项、用户没有采纳：用户来源不成立，严格采纳校验也不成立 → 零卡。
func TestAutoProposalUserSourceAssistantNewMatterRefused(t *testing.T) {
	at := userSourceTime(t)
	messages := []SessionMessage{
		{ID: 1, IsUser: true, Content: "以后都用 Go 1.23。", CreatedAt: at},
		{ID: 2, IsUser: false, Content: "建议把 Redis 升到 7.4。", CreatedAt: at.Add(time.Minute)},
	}
	proposal := SessionRawProposal{
		Scope: model.MemoryScopeGlobal, Class: model.ProfileClassConstraint, Subject: "Redis 版本",
		Durability: model.MemoryDurabilityStable, Content: "Redis 升级到 7.4。",
		EvidenceIDs: []uint{1}, StatementEvidenceID: 1, AssistantIDs: []uint{2},
	}
	result := userSourceExtract(t, fakeFinalizeBoards{}, messages, proposal)
	if len(result.Proposals) != 0 {
		t.Fatalf("proposals=%+v, want no card for the unadopted new matter", result.Proposals)
	}
	if len(result.Skipped) != 1 || result.Skipped[0].Reason != FinalizeSkipAdoptionUnproven {
		t.Fatalf("skipped=%+v, want adoption_unproven", result.Skipped)
	}
}

// 用户只说「嗯/对」、具体内容来自助手：即使白板记录了采纳对，仍走明确采纳
// 校验；这样的回话不是明确采纳，零卡。
func TestAutoProposalUserSourceNonCommittalStillRefused(t *testing.T) {
	at := userSourceTime(t)
	document := model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV3,
		Constraints: []model.SessionWhiteboardEntry{{
			Key: "pool", Text: "连接池设为 50", Value: "50",
			SourceMessageIDs: []uint{2, 3}, Origin: model.SessionWhiteboardOriginAssistantAccepted, UpdatedAtMessageID: 3,
		}},
	}
	board := marshalFinalizeBoard(t, document)
	for _, reply := range []string{"嗯。", "对。"} {
		messages := []SessionMessage{
			{ID: 1, IsUser: true, Content: "连接池怎么定？", CreatedAt: at},
			{ID: 2, IsUser: false, Content: "建议把连接池设为 50。", CreatedAt: at.Add(time.Minute)},
			{ID: 3, IsUser: true, Content: reply, CreatedAt: at.Add(2 * time.Minute)},
		}
		proposal := SessionRawProposal{
			Scope: model.MemoryScopeGlobal, Class: model.ProfileClassConstraint, Subject: "连接池",
			Durability: model.MemoryDurabilityStable, Content: "连接池设为 50。",
			EvidenceIDs: []uint{3}, StatementEvidenceID: 3, AssistantIDs: []uint{2},
		}
		result := userSourceExtract(t, fakeFinalizeBoards{content: &board}, messages, proposal)
		if len(result.Proposals) != 0 {
			t.Fatalf("reply %q produced a card: %+v", reply, result.Proposals)
		}
		if len(result.Skipped) != 1 || result.Skipped[0].Reason != FinalizeSkipAdoptionUnproven {
			t.Fatalf("reply %q skipped=%+v, want adoption_unproven", reply, result.Skipped)
		}
	}
}

// 用户用自己的话支撑理解，助手只在旁边建议归到某个主题：助手引用被丢弃，
// 主题不能被它点名，留空待用户选择。
func TestAutoProposalUserSourceTopicCannotBeNamedByAssistant(t *testing.T) {
	at := userSourceTime(t)
	messages := []SessionMessage{
		{ID: 1, IsUser: true, Content: "我理解队列就是任务排队，先到先处理。", CreatedAt: at},
		{ID: 2, IsUser: false, Content: "这条理解可以归到 Kafka 主题。", CreatedAt: at.Add(time.Minute)},
	}
	topics := fakeFinalizeCatalogue{topics: []Topic{{Key: "kafka", Name: "Kafka"}}}
	proposal := SessionRawProposal{
		Scope: model.MemoryScopeTopic, Kind: FinalizeKindUnderstanding, TopicKey: "kafka", TopicUnique: false,
		Subject: "队列理解", Durability: model.MemoryDurabilityStable,
		Content:     "队列就是任务排队，先到先处理。",
		EvidenceIDs: []uint{1}, StatementEvidenceID: 1, AssistantIDs: []uint{2},
	}
	service := newFinalizeService(t, fakeFinalizeBoards{}, fakeFinalizeMessages{messages: messages},
		topics, &fakeFinalizeFingerprints{known: map[string]struct{}{}}, fakeFinalizeGlobalMind{},
		fakeFinalizeTopicMind{byTopic: map[string][]model.LongTermMemory{}},
		&scriptedProposer{proposals: []SessionRawProposal{proposal}},
		&scriptedRelations{result: GlobalRelationResult{Relation: GlobalRelationIndependent, Reason: "no related entries"}}, 10)
	result, err := service.Extract(context.Background(), FinalizeRequest{UserID: 7, Cursor: FinalizeCursor{SessionID: "S-dev-user-topic"}})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(result.Proposals) != 1 {
		t.Fatalf("proposals=%d skipped=%+v, want the user's understanding", len(result.Proposals), result.Skipped)
	}
	candidate := result.Proposals[0]
	if !candidate.TopicPending || candidate.TopicKey != "" {
		t.Fatalf("candidate=%+v, want the assistant's suggestion not to name the topic", candidate)
	}
	if len(candidate.AssistantUtterances) != 0 {
		t.Fatalf("assistant refs=%+v, want the suggestion dropped", candidate.AssistantUtterances)
	}
	if candidate.Content != "我理解队列就是任务排队，先到先处理。" {
		t.Fatalf("content=%q, want the user's understanding", candidate.Content)
	}
}

// 普通求解提问、用户没有采纳助手答案：提问本身不是用户陈述，零卡。
func TestAutoProposalUserSourceQuestionNotAdoptedIsRefused(t *testing.T) {
	at := userSourceTime(t)
	messages := []SessionMessage{
		{ID: 1, IsUser: true, Content: "优雅关闭时 context 和 WaitGroup 怎么配合？", CreatedAt: at},
		{ID: 2, IsUser: false, Content: "优雅关闭时用 context 通知退出，再等 WaitGroup 归零。", CreatedAt: at.Add(time.Minute)},
	}
	proposal := SessionRawProposal{
		Scope: model.MemoryScopeGlobal, Class: model.ProfileClassConstraint, Subject: "优雅关闭",
		Durability: model.MemoryDurabilityStable, Content: "优雅关闭时用 context 通知退出，再等 WaitGroup 归零。",
		EvidenceIDs: []uint{1}, StatementEvidenceID: 1, AssistantIDs: []uint{2},
	}
	result := userSourceExtract(t, fakeFinalizeBoards{}, messages, proposal)
	if len(result.Proposals) != 0 {
		t.Fatalf("proposals=%+v, want no card for an unadopted answer", result.Proposals)
	}
	if len(result.Skipped) != 1 || result.Skipped[0].Reason != FinalizeSkipAdoptionUnproven {
		t.Fatalf("skipped=%+v, want adoption_unproven", result.Skipped)
	}
}

// 只有一次性操作证据：即使候选省掉「今天先」、标成 stable，也必须零卡。
func TestAutoProposalUserSourceTemporaryOnlyIsRefused(t *testing.T) {
	at := userSourceTime(t)
	messages := []SessionMessage{
		{ID: 1, IsUser: true, Content: "今天先用 Go 1.22 跑一下旧项目。", CreatedAt: at},
		{ID: 2, IsUser: false, Content: "好，今天先用 1.22 跑。", CreatedAt: at.Add(time.Minute)},
	}
	proposal := SessionRawProposal{
		Scope: model.MemoryScopeGlobal, Class: model.ProfileClassConstraint, Subject: "Go 版本",
		Durability: model.MemoryDurabilityStable, Content: "用 Go 1.22 跑旧项目。",
		EvidenceIDs: []uint{1}, StatementEvidenceID: 1, AssistantIDs: []uint{2},
	}
	result := userSourceExtract(t, fakeFinalizeBoards{}, messages, proposal)
	if len(result.Proposals) != 0 {
		t.Fatalf("proposals=%+v, want the one-off operation kept out", result.Proposals)
	}
	if len(result.Skipped) != 1 || result.Skipped[0].Reason != FinalizeSkipAdoptionUnproven {
		t.Fatalf("skipped=%+v, want adoption_unproven", result.Skipped)
	}
}

// 正例：从今天起固定用 16 是长期决定，不能被一次性操作规则误拦。
func TestAutoProposalUserSourceStartTimeFixedStillCards(t *testing.T) {
	at := userSourceTime(t)
	messages := []SessionMessage{
		{ID: 1, IsUser: true, Content: "从今天起固定用 16 线程。", CreatedAt: at},
		{ID: 2, IsUser: false, Content: "好，从今天起固定用 16 线程。", CreatedAt: at.Add(time.Minute)},
	}
	proposal := SessionRawProposal{
		Scope: model.MemoryScopeGlobal, Class: model.ProfileClassConstraint, Subject: "线程数",
		Durability: model.MemoryDurabilityStable, Content: "固定用 16 线程。",
		EvidenceIDs: []uint{1}, StatementEvidenceID: 1, AssistantIDs: []uint{2},
	}
	result := userSourceExtract(t, fakeFinalizeBoards{}, messages, proposal)
	if len(result.Proposals) != 1 || len(result.Skipped) != 0 {
		t.Fatalf("proposals=%d skipped=%+v, want the standing decision", len(result.Proposals), result.Skipped)
	}
	candidate := result.Proposals[0]
	if candidate.Content != "从今天起固定用 16 线程。" || candidate.Subject != "从今天起固定用 16 线程" {
		t.Fatalf("subject=%q content=%q, want the user's standing decision", candidate.Subject, candidate.Content)
	}
	if len(candidate.AssistantUtterances) != 0 {
		t.Fatalf("assistant refs=%+v, want the redundant refs dropped", candidate.AssistantUtterances)
	}
}

// 正例：带「以后」的偏好请求不是普通求解提问，不能被提问过滤误拦。
func TestAutoProposalUserSourcePreferenceQuestionStillCards(t *testing.T) {
	at := userSourceTime(t)
	messages := []SessionMessage{
		{ID: 1, IsUser: true, Content: "可不可以以后先给最小示例？", CreatedAt: at},
		{ID: 2, IsUser: false, Content: "可以，以后先给最小示例。", CreatedAt: at.Add(time.Minute)},
	}
	proposal := SessionRawProposal{
		Scope: model.MemoryScopeGlobal, Class: model.ProfileClassTeachingPreference, Subject: "示例顺序",
		Durability: model.MemoryDurabilityStable, Content: "以后先给最小示例。",
		EvidenceIDs: []uint{1}, StatementEvidenceID: 1, AssistantIDs: []uint{2},
	}
	result := userSourceExtract(t, fakeFinalizeBoards{}, messages, proposal)
	if len(result.Proposals) != 1 || len(result.Skipped) != 0 {
		t.Fatalf("proposals=%d skipped=%+v, want the preference card", len(result.Proposals), result.Skipped)
	}
	candidate := result.Proposals[0]
	if candidate.Content != "可不可以以后先给最小示例？" || candidate.Subject != "可不可以以后先给最小示例" {
		t.Fatalf("subject=%q content=%q, want the user's preference request", candidate.Subject, candidate.Content)
	}
	if len(candidate.AssistantUtterances) != 0 {
		t.Fatalf("assistant refs=%+v, want the redundant refs dropped", candidate.AssistantUtterances)
	}
}

// 长期词只能帮助判断适用期限，不能把「怎么配合」这类技术求解变成用户决定：
// 即使问句里有「以后」，这个分句也不能支撑草稿。
func TestAutoProposalUserSourceTechnicalQuestionWithStandingWordIsRefused(t *testing.T) {
	at := userSourceTime(t)
	messages := []SessionMessage{
		{ID: 1, IsUser: true, Content: "以后生产环境里 context 和 WaitGroup 怎么配合？", CreatedAt: at},
		{ID: 2, IsUser: false, Content: "生产环境里用 context 通知退出，再等 WaitGroup 归零。", CreatedAt: at.Add(time.Minute)},
	}
	proposal := SessionRawProposal{
		Scope: model.MemoryScopeGlobal, Class: model.ProfileClassConstraint, Subject: "优雅关闭",
		Durability: model.MemoryDurabilityStable, Content: "生产环境里用 context 通知退出，再等 WaitGroup 归零。",
		EvidenceIDs: []uint{1}, StatementEvidenceID: 1, AssistantIDs: []uint{2},
	}
	result := userSourceExtract(t, fakeFinalizeBoards{}, messages, proposal)
	if len(result.Proposals) != 0 {
		t.Fatalf("proposals=%+v, want the technical question refused", result.Proposals)
	}
	if len(result.Skipped) != 1 || result.Skipped[0].Reason != FinalizeSkipAdoptionUnproven {
		t.Fatalf("skipped=%+v, want adoption_unproven", result.Skipped)
	}
}

// 同一条消息里同时有偏好和提问：分句级判断只保留有独立依据的偏好分句；
// 以提问分句为来源的技术候选零卡。
func TestAutoProposalUserSourceMixedPreferenceAndQuestion(t *testing.T) {
	at := userSourceTime(t)
	messages := []SessionMessage{
		{ID: 1, IsUser: true, Content: "以后先给最小示例，另外生产环境里 context 和 WaitGroup 怎么配合？", CreatedAt: at},
		{ID: 2, IsUser: false, Content: "好，以后先给最小示例。生产环境里用 context 通知退出，再等 WaitGroup 归零。", CreatedAt: at.Add(time.Minute)},
	}
	preference := SessionRawProposal{
		Scope: model.MemoryScopeGlobal, Class: model.ProfileClassTeachingPreference, Subject: "示例顺序",
		Durability: model.MemoryDurabilityStable, Content: "以后先给最小示例。",
		EvidenceIDs: []uint{1}, StatementEvidenceID: 1, AssistantIDs: []uint{2},
	}
	result := userSourceExtract(t, fakeFinalizeBoards{}, messages, preference)
	if len(result.Proposals) != 1 || len(result.Skipped) != 0 {
		t.Fatalf("proposals=%d skipped=%+v, want the preference clause only", len(result.Proposals), result.Skipped)
	}
	candidate := result.Proposals[0]
	if candidate.Content != "以后先给最小示例" || candidate.Subject != "以后先给最小示例" {
		t.Fatalf("subject=%q content=%q, want only the preference clause", candidate.Subject, candidate.Content)
	}
	if strings.Contains(candidate.Content, "context") || strings.Contains(candidate.Content, "怎么") {
		t.Fatalf("content=%q, want the question clause kept out", candidate.Content)
	}
	if len(candidate.AssistantUtterances) != 0 {
		t.Fatalf("assistant refs=%+v, want the redundant refs dropped", candidate.AssistantUtterances)
	}

	technical := SessionRawProposal{
		Scope: model.MemoryScopeGlobal, Class: model.ProfileClassConstraint, Subject: "优雅关闭",
		Durability: model.MemoryDurabilityStable, Content: "生产环境里用 context 通知退出，再等 WaitGroup 归零。",
		EvidenceIDs: []uint{1}, StatementEvidenceID: 1, AssistantIDs: []uint{2},
	}
	technicalResult := userSourceExtract(t, fakeFinalizeBoards{}, messages, technical)
	if len(technicalResult.Proposals) != 0 {
		t.Fatalf("proposals=%+v, want the technical question refused", technicalResult.Proposals)
	}
	if len(technicalResult.Skipped) != 1 || technicalResult.Skipped[0].Reason != FinalizeSkipAdoptionUnproven {
		t.Fatalf("skipped=%+v, want adoption_unproven", technicalResult.Skipped)
	}
}

// 走到草稿入口的字段与不变量：标题/正文来自用户原话，来源里没有助手消息，
// 主题留空；确认前正式记忆、修订记录零变化，掌握度逐字段一致。
func TestAutoProposalUserSourceDraftFieldsAndInvariants(t *testing.T) {
	acc := newFinalizeAcceptance(t)
	ctx := context.Background()
	sessionID := "fin-acc-long"
	at := userSourceTime(t)
	stored := acc.seedSession(t, ctx, sessionID, nil, []SessionMessage{
		{IsUser: true, Content: "以后都用 Go 1.23。", CreatedAt: at},
		{IsUser: false, Content: "好，以后都用 Go 1.23。", CreatedAt: at.Add(time.Minute)},
	})
	lastAttempt := at.Add(-24 * time.Hour)
	if err := acc.db.Create(&model.TopicProgress{
		UserID: acc.userID, TopicKey: "go-basics", Topic: "Go 基础", MasteryScore: 73,
		Confidence: 0.7, ValidAttempts: 4, Level: model.TopicProgressDeveloping,
		WeakPoints: model.StringList{"版本边界"}, LastAttemptAt: &lastAttempt,
		CreatedAt: at, UpdatedAt: at,
	}).Error; err != nil {
		t.Fatal(err)
	}
	var progressBefore model.TopicProgress
	if err := acc.db.Where("user_id = ? AND topic_key = ?", acc.userID, "go-basics").First(&progressBefore).Error; err != nil {
		t.Fatal(err)
	}
	memoriesBefore := countRows(t, acc, &model.LongTermMemory{})
	revisionsBefore := countRows(t, acc, &model.MemoryRevision{})
	acc.proposer.proposals = []SessionRawProposal{{
		Scope: model.MemoryScopeGlobal, Class: model.ProfileClassConstraint, Subject: "Go 版本",
		Durability: model.MemoryDurabilityStable, Content: "以后都用 Go 1.23。",
		EvidenceIDs: []uint{stored[0].ID}, StatementEvidenceID: stored[0].ID, AssistantIDs: []uint{stored[1].ID},
	}}
	outcome, err := acc.runner.RunSession(ctx, acc.userID, sessionID)
	if err != nil {
		t.Fatalf("run session: %v", err)
	}
	if len(outcome.Proposals) != 1 || len(outcome.Persisted) != 1 {
		t.Fatalf("proposals=%d persisted=%v skipped=%+v, want one card", len(outcome.Proposals), outcome.Persisted, outcome.Skipped)
	}
	candidate := outcome.Proposals[0]
	if len(candidate.AssistantUtterances) != 0 {
		t.Fatalf("assistant refs=%+v, want the redundant refs dropped", candidate.AssistantUtterances)
	}
	drafts := acc.pendingDrafts(ctx)
	if len(drafts) != 1 {
		t.Fatalf("drafts=%d, want one pending card", len(drafts))
	}
	draft := drafts[0]
	if draft.Subject != "以后都用 Go 1.23" {
		t.Fatalf("draft subject=%q, want the user's title", draft.Subject)
	}
	if draft.Content != "以后都用 Go 1.23。" {
		t.Fatalf("draft content=%q, want the user's words", draft.Content)
	}
	if len(draft.AssistantMessages) != 0 {
		t.Fatalf("draft assistant messages=%v, want none", draft.AssistantMessages)
	}
	if len(draft.EvidenceMessages) != 1 || draft.EvidenceMessages[0] != fmt.Sprintf("m%d", stored[0].ID) {
		t.Fatalf("draft evidence=%v, want the user's message", draft.EvidenceMessages)
	}
	if draft.TopicKey != "" {
		t.Fatalf("draft topic=%q, want an unattributed global card", draft.TopicKey)
	}
	if draft.ProfileClass != model.ProfileClassConstraint {
		t.Fatalf("draft class=%q, want the constraint class", draft.ProfileClass)
	}
	if got := countRows(t, acc, &model.LongTermMemory{}); got != memoriesBefore {
		t.Fatalf("memories=%d, want %d before confirmation", got, memoriesBefore)
	}
	if got := countRows(t, acc, &model.MemoryRevision{}); got != revisionsBefore {
		t.Fatalf("revisions=%d, want %d before confirmation", got, revisionsBefore)
	}
	var progressAfter model.TopicProgress
	if err := acc.db.Where("user_id = ? AND topic_key = ?", acc.userID, "go-basics").First(&progressAfter).Error; err != nil {
		t.Fatal(err)
	}
	if progressAfter.MasteryScore != progressBefore.MasteryScore ||
		progressAfter.Confidence != progressBefore.Confidence ||
		progressAfter.ValidAttempts != progressBefore.ValidAttempts ||
		progressAfter.Level != progressBefore.Level ||
		!reflect.DeepEqual(progressAfter.WeakPoints, progressBefore.WeakPoints) ||
		!progressAfter.LastAttemptAt.Equal(*progressBefore.LastAttemptAt) ||
		!progressAfter.UpdatedAt.Equal(progressBefore.UpdatedAt) {
		t.Fatalf("progress changed before confirmation: before=%+v after=%+v", progressBefore, progressAfter)
	}
}
