package memory

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	memorydao "GopherAI/dao/memory"
	"GopherAI/model"
)

// 本文件是自动提议专项三项修复的零付费回归。每项在修复前失败、修复后
// 通过（证据见交付说明），并共用三条不变量断言：
//   - 同值判重：同内容（含跨类别）在提取端或入口层被挡住，不产生第二张卡；
//   - 未经确认零正式写入：跑完收尾提取后 LongTermMemory 行数不变，候选只落
//     pending 草稿；
//   - 掌握度不变：TopicProgress 行在提取全程不被触碰。

func seedProfileEntry(t *testing.T, acc *finalizeAcceptance, subject, content string, class model.MemoryProfileClass) *model.LongTermMemory {
	t.Helper()
	written, err := acc.repo.CreateUserMemory(context.Background(), memorydao.ExplicitWriteParams{
		Memory: model.LongTermMemory{
			UserID: acc.userID, Scope: model.MemoryScopeGlobal, Subject: subject, Content: content,
			StructuredJSON: `{}`, Tags: model.StringList{"project"}, Durability: model.MemoryDurabilityStable,
			ProfileClass: class, SourceRefs: model.StringList{"user_api:seed-fix"}, Confidence: 1, Importance: 8,
			Status: model.LongTermMemoryActive, UserConfirmed: true,
		},
		RequestKey: fmt.Sprintf("seed:%d:%s:%s", acc.userID, subject, content),
		Reason:     "专项回归种子",
		Now:        time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return &written.Memory
}

func countRows(t *testing.T, acc *finalizeAcceptance, model any) int64 {
	t.Helper()
	var count int64
	if err := acc.db.Model(model).Where("user_id = ?", acc.userID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	return count
}

// Fix 1 回归：新旧值分类不同（旧「背景」、新「约束」），同一项目同一个版本
// 仍要关联到旧条目；跨类别的不同事项仍可并存。
func TestAutoProposalFix1CrossClassValueLinksOldEntry(t *testing.T) {
	acc := newFinalizeAcceptance(t)
	ctx := context.Background()
	sessionID := "fin-acc-crossclass"
	stored := acc.seedSession(t, ctx, sessionID, nil, baseFinalizeMessages())
	// 旧值按「背景」存，另一条目标类画像条目与候选无关。
	old := seedProfileEntry(t, acc, "Go 版本", "当前项目使用 Go 1.22。", model.ProfileClassBackground)
	seedProfileEntry(t, acc, "目标", "2027 年春招目标岗位：Go 后端开发", model.ProfileClassGoal)
	// 掌握度: 一行进度记录，提取全程不得变化。
	if err := acc.db.Create(&model.TopicProgress{
		UserID: acc.userID, TopicKey: "go-basics", Topic: "Go 基础", MasteryScore: 80,
		Confidence: 0.8, ValidAttempts: 6, Level: model.TopicProgressDeveloping,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}).Error; err != nil {
		t.Fatal(err)
	}
	memoriesBefore := countRows(t, acc, &model.LongTermMemory{})

	// 新值被归为「约束」：分类只影响类别，不影响它和旧值是不是同一事项。
	acc.global.inner = &deterministicGlobalClassifier{rules: []stubGlobalRule{
		{contains: "1.23", relation: "replacement", targetHint: "1.22"},
	}}
	acc.proposer.proposals = []SessionRawProposal{{
		Scope: model.MemoryScopeGlobal, Class: model.ProfileClassConstraint, Subject: "Go 版本",
		Durability: model.MemoryDurabilityStable, Content: "当前项目使用 Go 1.23。",
		EvidenceIDs: []uint{stored[2].ID}, StatementEvidenceID: stored[2].ID,
	}}
	outcome, err := acc.runner.RunSession(ctx, acc.userID, sessionID)
	if err != nil {
		t.Fatalf("run session: %v", err)
	}
	// 同一事项关联到正确旧条目：提取端判定为替换，草稿钉住旧值。
	if len(outcome.Persisted) != 1 || len(outcome.Proposals) != 1 {
		t.Fatalf("persisted=%v proposals=%d, want one card", outcome.Persisted, len(outcome.Proposals))
	}
	candidate := outcome.Proposals[0]
	if candidate.Relation.Relation != GlobalRelationReplacement || candidate.Relation.TargetMemoryID != old.ID {
		t.Fatalf("relation=%+v, want replacement of %s", candidate.Relation, old.ID)
	}
	relatedHasOld := false
	for _, row := range candidate.RelatedOld {
		if row.ID == old.ID {
			relatedHasOld = true
		}
	}
	if !relatedHasOld {
		t.Fatalf("related=%+v, want the background-class old value in the judgement range", candidate.RelatedOld)
	}
	// 判定视野是全部生效条目：旧值（背景）和无关目标类都在，类别不是边界。
	saw := acc.global.saw[len(acc.global.saw)-1]
	foundOld, foundOther := false, false
	for _, row := range saw {
		if row.ID == old.ID {
			foundOld = true
		}
		if row.ProfileClass == model.ProfileClassGoal {
			foundOther = true
		}
	}
	if !foundOld || !foundOther {
		t.Fatalf("classifier saw old=%v other=%v, want the class-blind range", foundOld, foundOther)
	}
	drafts := acc.pendingDrafts(ctx)
	if len(drafts) != 1 || !stringListContains(drafts[0].ConflictIDs, old.ID) {
		t.Fatalf("drafts=%+v, want one pending card pinned to the old value", drafts)
	}
	// 不变量：零正式写入（旧值仍生效、无新增记忆）与掌握度不变。
	if got := countRows(t, acc, &model.LongTermMemory{}); got != memoriesBefore {
		t.Fatalf("memories=%d, want %d (no formal write before confirmation)", got, memoriesBefore)
	}
	var progress model.TopicProgress
	if err := acc.db.Where("user_id = ? AND topic_key = ?", acc.userID, "go-basics").First(&progress).Error; err != nil {
		t.Fatal(err)
	}
	if progress.MasteryScore != 80 {
		t.Fatalf("mastery=%d, want 80 (untouched)", progress.MasteryScore)
	}
}

// Fix 1 对照：跨类别的不同事项不被误替换，各自并存。
func TestAutoProposalFix1CrossClassDifferentMattersCoexist(t *testing.T) {
	acc := newFinalizeAcceptance(t)
	ctx := context.Background()
	sessionID := "fin-acc-0001"
	stored := acc.seedSession(t, ctx, sessionID, nil, baseFinalizeMessages())
	goal := seedProfileEntry(t, acc, "目标", "2027 年春招目标岗位：Go 后端开发", model.ProfileClassGoal)
	acc.global.inner = &deterministicGlobalClassifier{} // 判定为 independent
	acc.proposer.proposals = []SessionRawProposal{{
		Scope: model.MemoryScopeGlobal, Class: model.ProfileClassTeachingPreference, Subject: "讲解偏好",
		Durability: model.MemoryDurabilityStable, Content: "讲新概念先举例再讲定义。",
		EvidenceIDs: []uint{stored[2].ID}, StatementEvidenceID: stored[2].ID,
	}}
	outcome, err := acc.runner.RunSession(ctx, acc.userID, sessionID)
	if err != nil {
		t.Fatalf("run session: %v", err)
	}
	if len(outcome.Persisted) != 1 || len(outcome.Proposals) != 1 {
		t.Fatalf("persisted=%v, want the preference card", outcome.Persisted)
	}
	if outcome.Proposals[0].Relation.Relation != GlobalRelationIndependent || outcome.Proposals[0].Relation.TargetMemoryID != "" {
		t.Fatalf("relation=%+v, want independent coexistence", outcome.Proposals[0].Relation)
	}
	drafts := acc.pendingDrafts(ctx)
	if len(drafts) != 1 || len(drafts[0].ConflictIDs) != 0 {
		t.Fatalf("drafts=%+v, want no replacement target", drafts)
	}
	if got := seedProfileStatus(t, acc, goal.ID); got != model.LongTermMemoryActive {
		t.Fatalf("goal status=%s, want active (coexist)", got)
	}
}

func seedProfileStatus(t *testing.T, acc *finalizeAcceptance, id string) model.LongTermMemoryStatus {
	t.Helper()
	var row model.LongTermMemory
	if err := acc.db.Where("user_id = ? AND id = ?", acc.userID, id).First(&row).Error; err != nil {
		t.Fatal(err)
	}
	return row.Status
}

// Fix 2 回归：用户先说的决定能作为证据；只有助手提议、用户未采纳仍拒绝。
func TestAutoProposalFix2LearnerFirstDecisionIsEvidence(t *testing.T) {
	acc := newFinalizeAcceptance(t)
	ctx := context.Background()
	sessionID := "fin-acc-answer"
	messages := []SessionMessage{
		{IsUser: true, Content: "以后方案都改成 B 版，示例也按 B 来。", CreatedAt: time.Date(2026, 11, 22, 20, 1, 0, 0, time.FixedZone("CST", 8*3600))},
		{IsUser: false, Content: "好，以后都按 B 版走，我记下了。", CreatedAt: time.Date(2026, 11, 22, 20, 2, 0, 0, time.FixedZone("CST", 8*3600))},
	}
	stored := acc.seedSession(t, ctx, sessionID, nil, messages)
	board := model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV3,
		Constraints: []model.SessionWhiteboardEntry{{
			Key: "plan-b", Text: "方案改成 B 版，示例按 B 来", Value: "B 版",
			SourceMessageIDs: []uint{stored[0].ID}, Origin: model.SessionWhiteboardOriginUser, UpdatedAtMessageID: stored[0].ID,
		}},
	}
	if err := acc.db.Create(&model.SessionWhiteboard{
		UserID: acc.userID, SessionID: sessionID, Content: marshalFinalizeBoard(t, board), Version: 1,
	}).Error; err != nil {
		t.Fatal(err)
	}
	memoriesBefore := countRows(t, acc, &model.LongTermMemory{})
	acc.global.inner = &deterministicGlobalClassifier{}
	acc.proposer.proposals = []SessionRawProposal{{
		Scope: model.MemoryScopeGlobal, Class: model.ProfileClassBackground, Subject: "方案版本",
		Durability: model.MemoryDurabilityStable, Content: "示例与方案都按 B 版。",
		EvidenceIDs: []uint{stored[0].ID}, StatementEvidenceID: stored[0].ID, AssistantIDs: []uint{stored[1].ID},
	}}
	outcome, err := acc.runner.RunSession(ctx, acc.userID, sessionID)
	if err != nil {
		t.Fatalf("run session: %v", err)
	}
	// 用户原话（白板归属为 user）足以支撑候选：出卡，不判 adoption_unproven。
	if len(outcome.Persisted) != 1 || len(outcome.Proposals) != 1 {
		t.Fatalf("persisted=%v skipped=%+v, want the learner-first card", outcome.Persisted, outcome.Skipped)
	}
	drafts := acc.pendingDrafts(ctx)
	if len(drafts) != 1 {
		t.Fatalf("drafts=%d, want one pending card", len(drafts))
	}
	// 来源契约保留两侧原话：用户决定 + 助手复述。
	if len(drafts[0].AssistantMessages) != 1 || len(drafts[0].EvidenceMessages) != 1 {
		t.Fatalf("draft provenance evidence=%v assistant=%v", drafts[0].EvidenceMessages, drafts[0].AssistantMessages)
	}
	// 不变量：未经确认零正式写入。
	if got := countRows(t, acc, &model.LongTermMemory{}); got != memoriesBefore {
		t.Fatalf("memories=%d, want %d", got, memoriesBefore)
	}
}

func TestAutoProposalFix2UnacceptedAssistantProposalStillRefused(t *testing.T) {
	acc := newFinalizeAcceptance(t)
	ctx := context.Background()
	sessionID := "fin-acc-topic"
	messages := []SessionMessage{
		{IsUser: true, Content: "方案怎么改比较好？", CreatedAt: time.Date(2026, 11, 22, 20, 1, 0, 0, time.FixedZone("CST", 8*3600))},
		{IsUser: false, Content: "建议把方案改成 B 版，示例也按 B 来。", CreatedAt: time.Date(2026, 11, 22, 20, 2, 0, 0, time.FixedZone("CST", 8*3600))},
	}
	stored := acc.seedSession(t, ctx, sessionID, nil, messages)
	// 白板只把这条助手建议记成 assistant 来源（未采纳），用户那句话不是
	// 学习者自己的决定。
	board := model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV3,
		OpenItems: []model.SessionWhiteboardEntry{{
			Key: "assistant-suggestion", Text: "建议改成 B 版", Value: "B 版",
			SourceMessageIDs: []uint{stored[1].ID}, Origin: model.SessionWhiteboardOriginAssistant, UpdatedAtMessageID: stored[1].ID,
		}},
	}
	if err := acc.db.Create(&model.SessionWhiteboard{
		UserID: acc.userID, SessionID: sessionID, Content: marshalFinalizeBoard(t, board), Version: 1,
	}).Error; err != nil {
		t.Fatal(err)
	}
	memoriesBefore := countRows(t, acc, &model.LongTermMemory{})
	acc.global.inner = &deterministicGlobalClassifier{}
	acc.proposer.proposals = []SessionRawProposal{{
		Scope: model.MemoryScopeGlobal, Class: model.ProfileClassBackground, Subject: "方案版本",
		Durability: model.MemoryDurabilityStable, Content: "示例与方案都按 B 版。",
		EvidenceIDs: []uint{stored[0].ID}, StatementEvidenceID: stored[0].ID, AssistantIDs: []uint{stored[1].ID},
	}}
	outcome, err := acc.runner.RunSession(ctx, acc.userID, sessionID)
	if err != nil {
		t.Fatalf("run session: %v", err)
	}
	if len(outcome.Persisted) != 0 || len(outcome.Proposals) != 0 {
		t.Fatalf("persisted=%v proposals=%d, want the unaccepted proposal refused", outcome.Persisted, len(outcome.Proposals))
	}
	if len(outcome.Skipped) != 1 || outcome.Skipped[0].Reason != FinalizeSkipAdoptionUnproven {
		t.Fatalf("skipped=%+v, want adoption_unproven", outcome.Skipped)
	}
	if got := countRows(t, acc, &model.LongTermMemory{}); got != memoriesBefore {
		t.Fatalf("memories=%d, want %d", got, memoriesBefore)
	}
}

// Fix 3 回归：先区分主题理解和全局规则，再决定归属。范围由工具决定，冗余的
// scope 参数不能覆盖它；多主题技术理解保留为主题草稿并留空归属。
func TestAutoProposalFix3ScopeComesFromTheToolNotTheArgument(t *testing.T) {
	ctx := context.Background()
	visible := map[uint]SessionMessage{1: {ID: 1, IsUser: true, Content: "我理解上下文取消会级联到子任务。"}}
	topicChat := &memoryChat{response: toolMessage(finalizeToolTopic,
		`{"scope":"global","kind":"understanding","subject":"上下文取消","proposed_content":"上下文取消会级联到子任务，这条理解适合多个主题。","durability":"stable","evidence_ids":[1],"statement_evidence_id":1,"topic_key":"","topic_unique":false}`)}
	topicProposer, err := NewModelSessionProposer(topicChat)
	if err != nil {
		t.Fatal(err)
	}
	proposals, err := topicProposer.Propose(ctx, &SessionExtractionInput{UserID: 7, SessionID: "S", Visible: visible})
	if err != nil || len(proposals) != 1 {
		t.Fatalf("topic proposal err=%v n=%d", err, len(proposals))
	}
	// 工具是语义决定：主题工具即使参数写成 global，也仍是主题理解。
	if proposals[0].Scope != model.MemoryScopeTopic {
		t.Fatalf("scope=%s, want topic from the tool, not the argument", proposals[0].Scope)
	}

	profileChat := &memoryChat{response: toolMessage(finalizeToolProfile,
		`{"scope":"topic","class":"teaching_preference","subject":"讲解偏好","proposed_content":"讲新概念先举例再讲定义。","durability":"stable","evidence_ids":[1],"statement_evidence_id":1}`)}
	profileProposer, err := NewModelSessionProposer(profileChat)
	if err != nil {
		t.Fatal(err)
	}
	profileProposals, err := profileProposer.Propose(ctx, &SessionExtractionInput{UserID: 7, SessionID: "S", Visible: visible})
	if err != nil || len(profileProposals) != 1 {
		t.Fatalf("profile proposal err=%v n=%d", err, len(profileProposals))
	}
	// 反向同理：正常画像不会被参数里的 topic 改成主题草稿。
	if profileProposals[0].Scope != model.MemoryScopeGlobal || profileProposals[0].Class != model.ProfileClassTeachingPreference {
		t.Fatalf("scope=%s class=%s, want the global preference kept", profileProposals[0].Scope, profileProposals[0].Class)
	}
	if !strings.Contains(finalizeProposerPrompt, "Decide what the thing is before deciding where it belongs") {
		t.Fatal("the extraction prompt lost the decide-kind-first rule")
	}
}

// Fix 3 对照（服务层）：多主题技术理解 → 主题草稿、归属留空、待用户选择；
// 全局偏好 → 画像，不会被改成主题草稿。
func TestAutoProposalFix3TopicPendingVersusGlobalPreference(t *testing.T) {
	board := ext01BoardPreferenceOnly(t)
	at := time.Date(2026, 11, 18, 20, 1, 0, 0, time.FixedZone("CST", 8*3600))
	messages := []SessionMessage{
		{ID: 1, IsUser: true, Content: "我理解上下文取消会级联到子任务，这条理解好几个主题都用得上。", CreatedAt: at},
	}
	service := newFinalizeService(t, fakeFinalizeBoards{content: &board}, fakeFinalizeMessages{messages: messages},
		nil, &fakeFinalizeFingerprints{known: map[string]struct{}{}}, fakeFinalizeGlobalMind{}, nil,
		&scriptedProposer{proposals: []SessionRawProposal{{
			Scope: model.MemoryScopeTopic, Kind: FinalizeKindUnderstanding, TopicKey: "", TopicUnique: false,
			Subject: "上下文取消", Durability: model.MemoryDurabilityStable,
			Content: "上下文取消会级联到子任务，适用多个主题。", EvidenceIDs: []uint{1}, StatementEvidenceID: 1,
		}}}, &scriptedRelations{result: GlobalRelationResult{Relation: GlobalRelationIndependent, Reason: "x"}}, 0)
	result, err := service.Extract(context.Background(), FinalizeRequest{UserID: 7, Cursor: FinalizeCursor{SessionID: "S-dev-fix3"}})
	if err != nil || len(result.Proposals) != 1 {
		t.Fatalf("topic pending: proposals=%d err=%v", len(result.Proposals), err)
	}
	pending := result.Proposals[0]
	if pending.Scope != model.MemoryScopeTopic || !pending.TopicPending || pending.TopicKey != "" {
		t.Fatalf("candidate=%+v, want a topic draft waiting for attribution", pending)
	}
	if pending.Class != "" {
		t.Fatalf("class=%q, a topic note carries no profile class", pending.Class)
	}

	// 对照：确实跨对话适用的全局偏好留在画像。
	globalService := newFinalizeService(t, fakeFinalizeBoards{content: &board}, fakeFinalizeMessages{messages: messages},
		nil, &fakeFinalizeFingerprints{known: map[string]struct{}{}}, fakeFinalizeGlobalMind{}, nil,
		&scriptedProposer{proposals: []SessionRawProposal{{
			Scope: model.MemoryScopeGlobal, Class: model.ProfileClassTeachingPreference,
			Subject: "讲解偏好", Durability: model.MemoryDurabilityStable,
			Content: "讲新概念先举例再讲定义。", EvidenceIDs: []uint{1}, StatementEvidenceID: 1,
		}}}, &scriptedRelations{result: GlobalRelationResult{Relation: GlobalRelationIndependent, Reason: "x"}}, 0)
	globalResult, err := globalService.Extract(context.Background(), FinalizeRequest{UserID: 7, Cursor: FinalizeCursor{SessionID: "S-dev-fix3"}})
	if err != nil || len(globalResult.Proposals) != 1 {
		t.Fatalf("global preference: proposals=%d err=%v", len(globalResult.Proposals), err)
	}
	preference := globalResult.Proposals[0]
	if preference.Scope != model.MemoryScopeGlobal || preference.Class != model.ProfileClassTeachingPreference || preference.TopicPending {
		t.Fatalf("candidate=%+v, want a normal profile preference", preference)
	}
}

func ext01BoardPreferenceOnly(t *testing.T) string {
	t.Helper()
	document := model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV3,
		Constraints: []model.SessionWhiteboardEntry{{
			Key: "teaching-preference", Text: "讲新概念先举例再讲定义", Value: "先举例再讲定义",
			SourceMessageIDs: []uint{1}, Origin: model.SessionWhiteboardOriginUser, UpdatedAtMessageID: 1,
		}},
	}
	return marshalFinalizeBoard(t, document)
}
