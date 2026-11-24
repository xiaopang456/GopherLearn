package memory

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"GopherAI/model"
)

// 本文件是复核反例一、反例二补修的零付费回归。
//
// 反例一：白板把用户先说的话记成 origin=user，只能证明消息先后，不能证明
// 用户采纳了助手后来加的内容。三个开发样例：
//  1. 用户决定、助手复述：正常出卡，且草稿内容由用户原话支撑；
//  2. 助手另提新事项（Redis 升级）：零卡，记 adoption_unproven；
//  3. 助手在同一事项里加用户没说过的条件：出卡，但草稿不带该条件。
//
// 反例二：提取侧先截前 10 条全局画像，草稿入口却比较最多 30 条，同一事项的
// 旧值排在第 11 条时两层判定范围不一致。修复后两层看同一批生效条目，草稿
// 钉住正确旧值；同值重述仍判重，跨类不同事项仍并存。确认前零正式写入、
// 旧条目状态/修订记录/掌握度都不变。

const (
	boundaryUserStatement = "以后都用 Go 1.23。"
	boundaryCardContent   = "以后都用 Go 1.23。"
)

func learnerFirstMessages(assistantReply string) []SessionMessage {
	at := time.Date(2026, 11, 24, 20, 1, 0, 0, time.FixedZone("CST", 8*3600))
	return []SessionMessage{
		{ID: 1, IsUser: true, Content: boundaryUserStatement, CreatedAt: at},
		{ID: 2, IsUser: false, Content: assistantReply, CreatedAt: at.Add(time.Minute)},
	}
}

func learnerFirstProposal(content string) SessionRawProposal {
	return SessionRawProposal{
		Scope: model.MemoryScopeGlobal, Class: model.ProfileClassConstraint, Subject: "Go 版本",
		Durability: model.MemoryDurabilityStable, Content: content,
		EvidenceIDs: []uint{1}, StatementEvidenceID: 1, AssistantIDs: []uint{2},
	}
}

func extractLearnerFirstMessages(t *testing.T, messages []SessionMessage, proposal SessionRawProposal) FinalizeResult {
	t.Helper()
	document := model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV3,
		Constraints: []model.SessionWhiteboardEntry{{
			Key: "go-version", Text: "以后都用 Go 1.23", Value: "1.23",
			SourceMessageIDs: []uint{messages[0].ID}, Origin: model.SessionWhiteboardOriginUser, UpdatedAtMessageID: messages[0].ID,
		}},
	}
	board := marshalFinalizeBoard(t, document)
	service := newFinalizeService(t, fakeFinalizeBoards{content: &board}, fakeFinalizeMessages{messages: messages},
		nil, &fakeFinalizeFingerprints{known: map[string]struct{}{}}, fakeFinalizeGlobalMind{}, nil,
		&scriptedProposer{proposals: []SessionRawProposal{proposal}},
		&scriptedRelations{result: GlobalRelationResult{Relation: GlobalRelationIndependent, Reason: "boundary probe"}}, 10)
	result, err := service.Extract(context.Background(), FinalizeRequest{UserID: 7, Cursor: FinalizeCursor{SessionID: "S-dev-boundary"}})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	return result
}

func extractLearnerFirst(t *testing.T, assistantReply, proposalContent string) FinalizeResult {
	t.Helper()
	return extractLearnerFirstMessages(t, learnerFirstMessages(assistantReply), learnerFirstProposal(proposalContent))
}

// 样例一：用户决定、助手复述 → 正常出卡。草稿内容由用户原话支撑，助手的
// 复述只作为来源记录，不把助手的改写当成用户原话。
func TestAutoProposalAdoptionRestatementStillCards(t *testing.T) {
	result := extractLearnerFirst(t, "好，以后都用 Go 1.23。", "用户以后都用 Go 1.23。")
	if len(result.Proposals) != 1 || len(result.Skipped) != 0 {
		t.Fatalf("proposals=%d skipped=%+v, want one card", len(result.Proposals), result.Skipped)
	}
	candidate := result.Proposals[0]
	if candidate.Content != boundaryCardContent {
		t.Fatalf("content=%q, want the learner's own statement %q", candidate.Content, boundaryCardContent)
	}
	// 助手只是复述、学习者没有明确采纳：复述不进入 assistant_messages，也
	// 不能作为主题点名证据。
	if len(candidate.AssistantUtterances) != 0 {
		t.Fatalf("assistant provenance=%v, want the unadopted restatement dropped", candidate.AssistantUtterances)
	}
	if len(candidate.Evidence) != 1 || candidate.Evidence[0].Role != model.SessionWhiteboardOriginUser {
		t.Fatalf("evidence=%v, want the learner's message", candidate.Evidence)
	}
}

// 样例二：助手另提新事项。用户没说过 Redis，白板 origin=user 只覆盖 Go 那句，
// 时间先后不能证明用户采纳了 Redis 提议 → 零 Redis 卡，记 adoption_unproven。
func TestAutoProposalAdoptionAssistantNewMatterIsRefused(t *testing.T) {
	result := extractLearnerFirst(t, "建议顺便把 Redis 升到 7.4。", "Redis 升级到 7.4。")
	if len(result.Proposals) != 0 {
		t.Fatalf("proposals=%+v, want no Redis card", result.Proposals)
	}
	if len(result.Skipped) != 1 || result.Skipped[0].Reason != FinalizeSkipAdoptionUnproven {
		t.Fatalf("skipped=%+v, want adoption_unproven", result.Skipped)
	}
	if result.Skipped[0].Subject != "Go 版本" && result.Skipped[0].Subject != "Redis 版本" {
		t.Fatalf("skip subject=%q, want the refused candidate recorded", result.Skipped[0].Subject)
	}
}

// 样例三：助手在同一事项里加了用户没说过的限制。卡可以出（用户自己的决定
// 仍然有效），但草稿只写用户原话，不携带助手新增的限制。
func TestAutoProposalAdoptionAssistantAddedConditionIsDropped(t *testing.T) {
	result := extractLearnerFirst(t, "好，以后都用 Go 1.23，但不要用泛型。", "以后都用 Go 1.23，禁用泛型。")
	if len(result.Proposals) != 1 || len(result.Skipped) != 0 {
		t.Fatalf("proposals=%d skipped=%+v, want the learner's card", len(result.Proposals), result.Skipped)
	}
	content := result.Proposals[0].Content
	if strings.Contains(content, "泛型") || strings.Contains(content, "禁用") {
		t.Fatalf("content=%q, want no assistant-added condition", content)
	}
	if content != boundaryCardContent {
		t.Fatalf("content=%q, want the learner's own statement %q", content, boundaryCardContent)
	}
}

// 样例四：同一条用户消息里既有长期决定、又有一次性临时操作。长期决定出卡，
// 但草稿只保留支撑这个事项的片段：临时设置不能借同一条消息进入卡片。
func TestAutoProposalAdoptionTemporaryOperationStaysOutOfTheCard(t *testing.T) {
	at := time.Date(2026, 11, 24, 20, 1, 0, 0, time.FixedZone("CST", 8*3600))
	messages := []SessionMessage{
		{ID: 1, IsUser: true, Content: "以后都用 Go 1.23。今天先用 1.22 跑通旧项目。", CreatedAt: at},
		{ID: 2, IsUser: false, Content: "好，以后都用 Go 1.23。", CreatedAt: at.Add(time.Minute)},
	}
	result := extractLearnerFirstMessages(t, messages, learnerFirstProposal("以后都用 Go 1.23。"))
	if len(result.Proposals) != 1 || len(result.Skipped) != 0 {
		t.Fatalf("proposals=%d skipped=%+v, want only the long-term decision", len(result.Proposals), result.Skipped)
	}
	candidate := result.Proposals[0]
	if candidate.Content != boundaryCardContent {
		t.Fatalf("content=%q, want only the part supporting the long-term decision %q", candidate.Content, boundaryCardContent)
	}
	for _, leak := range []string{"1.22", "今天", "旧项目", "跑通"} {
		if strings.Contains(candidate.Content, leak) {
			t.Fatalf("content=%q, want the temporary operation kept out", candidate.Content)
		}
	}
	if candidate.Durability != model.MemoryDurabilityStable {
		t.Fatalf("durability=%s, want the standing decision", candidate.Durability)
	}
}

// 反例：同一事项同时有长期值和临时值。两句用词接近，都过字符相似度门槛，
// 但临时那句的值（1.22）和适用范围（今天）都不属于长期决定，不能进卡。
func TestAutoProposalAdoptionLongTermValueExcludesTemporaryValue(t *testing.T) {
	at := time.Date(2026, 11, 24, 20, 1, 0, 0, time.FixedZone("CST", 8*3600))
	messages := []SessionMessage{
		{ID: 1, IsUser: true, Content: "以后项目都用 Go 1.23。今天这个项目先用 Go 1.22。", CreatedAt: at},
		{ID: 2, IsUser: false, Content: "好，以后都按 Go 1.23 来。", CreatedAt: at.Add(time.Minute)},
	}
	result := extractLearnerFirstMessages(t, messages, learnerFirstProposal("以后项目都用 Go 1.23。"))
	if len(result.Proposals) != 1 || len(result.Skipped) != 0 {
		t.Fatalf("proposals=%d skipped=%+v, want the long-term card", len(result.Proposals), result.Skipped)
	}
	candidate := result.Proposals[0]
	if candidate.Content != "以后项目都用 Go 1.23。" {
		t.Fatalf("content=%q, want only the long-term value", candidate.Content)
	}
	for _, leak := range []string{"1.22", "今天"} {
		if strings.Contains(candidate.Content, leak) {
			t.Fatalf("content=%q, want the temporary value and scope kept out", candidate.Content)
		}
	}
}

// 适用范围也必须核对：临时那句的版本和长期决定相同（都是 1.23），但「今天」
// 表明它是一次性操作，仍然不能进长期决定的卡。
func TestAutoProposalAdoptionTemporaryScopeExcludesSameValue(t *testing.T) {
	at := time.Date(2026, 11, 24, 20, 1, 0, 0, time.FixedZone("CST", 8*3600))
	messages := []SessionMessage{
		{ID: 1, IsUser: true, Content: "以后都用 Go 1.23。今天先用 Go 1.23 跑一下。", CreatedAt: at},
		{ID: 2, IsUser: false, Content: "好，以后都用 Go 1.23。", CreatedAt: at.Add(time.Minute)},
	}
	result := extractLearnerFirstMessages(t, messages, learnerFirstProposal("以后都用 Go 1.23。"))
	if len(result.Proposals) != 1 || len(result.Skipped) != 0 {
		t.Fatalf("proposals=%d skipped=%+v, want the long-term card", len(result.Proposals), result.Skipped)
	}
	if result.Proposals[0].Content != boundaryCardContent {
		t.Fatalf("content=%q, want only the long-term scope %q", result.Proposals[0].Content, boundaryCardContent)
	}
	for _, leak := range []string{"今天", "跑一下"} {
		if strings.Contains(result.Proposals[0].Content, leak) {
			t.Fatalf("content=%q, want the temporary scope kept out", result.Proposals[0].Content)
		}
	}
}

// 反例：用户在助手之后发言，不等于采纳。普通收尾、转题、未表态都必须拒绝；
// 能明确指回提议的「就按你刚说的方案」才是采纳。
func TestAutoProposalAdoptionLateReplyNeedsAgreement(t *testing.T) {
	at := time.Date(2026, 11, 24, 20, 1, 0, 0, time.FixedZone("CST", 8*3600))
	proposal := SessionRawProposal{
		Scope: model.MemoryScopeGlobal, Class: model.ProfileClassConstraint, Subject: "Redis 版本",
		Durability: model.MemoryDurabilityStable, Content: "Redis 升级到 7.4。",
		EvidenceIDs: []uint{3}, StatementEvidenceID: 3, AssistantIDs: []uint{2},
	}
	extract := func(reply string, withAcceptedBoard bool) FinalizeResult {
		messages := []SessionMessage{
			{ID: 1, IsUser: true, Content: "Redis 缓存怎么配比较好？", CreatedAt: at},
			{ID: 2, IsUser: false, Content: "建议把 Redis 升到 7.4，能少踩几个持久化配置的坑。", CreatedAt: at.Add(time.Minute)},
			{ID: 3, IsUser: true, Content: reply, CreatedAt: at.Add(2 * time.Minute)},
		}
		var boards FinalizeBoardSource = fakeFinalizeBoards{}
		if withAcceptedBoard {
			document := model.SessionWhiteboardDocument{
				SchemaVersion: model.SessionWhiteboardSchemaV3,
				OpenItems: []model.SessionWhiteboardEntry{{
					Key: "redis-version", Text: "升到 Redis 7.4", Value: "7.4",
					SourceMessageIDs: []uint{2, 3}, Origin: model.SessionWhiteboardOriginAssistantAccepted, UpdatedAtMessageID: 3,
				}},
			}
			content := marshalFinalizeBoard(t, document)
			boards = fakeFinalizeBoards{content: &content}
		}
		service := newFinalizeService(t, boards, fakeFinalizeMessages{messages: messages},
			nil, &fakeFinalizeFingerprints{known: map[string]struct{}{}}, fakeFinalizeGlobalMind{}, nil,
			&scriptedProposer{proposals: []SessionRawProposal{proposal}},
			&scriptedRelations{result: GlobalRelationResult{Relation: GlobalRelationIndependent, Reason: "no related entries"}}, 10)
		result, err := service.Extract(context.Background(), FinalizeRequest{UserID: 7, Cursor: FinalizeCursor{SessionID: "S-dev-late-reply"}})
		if err != nil {
			t.Fatalf("extract: %v", err)
		}
		return result
	}
	for _, reply := range []string{"今天到这。", "那再讲讲别的吧。", "嗯，我再想想。"} {
		result := extract(reply, false)
		if len(result.Proposals) != 0 {
			t.Fatalf("reply %q produced a card: %+v", reply, result.Proposals)
		}
		if len(result.Skipped) != 1 || result.Skipped[0].Reason != FinalizeSkipAdoptionUnproven {
			t.Fatalf("reply %q skipped=%+v, want adoption_unproven", reply, result.Skipped)
		}
	}
	// 即使白板误记了采纳，收尾话本身也必须读起来像采纳：记录和原话要一致。
	if result := extract("今天到这。", true); len(result.Proposals) != 0 {
		t.Fatalf("a mislabeled board let the closing through: %+v", result.Proposals)
	}
	// 正例：明确指回提议的采纳，且白板记录了这对证据。
	result := extract("就按你刚说的方案。", true)
	if len(result.Proposals) != 1 || len(result.Skipped) != 0 {
		t.Fatalf("adoption reply: proposals=%d skipped=%+v, want the card", len(result.Proposals), result.Skipped)
	}
	if len(result.Proposals[0].AssistantUtterances) != 1 || result.Proposals[0].AssistantUtterances[0].MessageID != 2 {
		t.Fatalf("assistant provenance=%+v, want the adopted proposal kept", result.Proposals[0].AssistantUtterances)
	}
}

// 反例：助手给出两个方案，学习者明确选第二种。只有被选中的那条提议能出
// 卡；引用第一条时白板没有那条采纳记录，不能凭「就按」放行。
func TestAutoProposalAdoptionSelectsTheCitedProposal(t *testing.T) {
	at := time.Date(2026, 11, 24, 20, 1, 0, 0, time.FixedZone("CST", 8*3600))
	messages := []SessionMessage{
		{ID: 1, IsUser: true, Content: "Redis 缓存版本怎么定？", CreatedAt: at},
		{ID: 2, IsUser: false, Content: "建议升到 Redis 7.4。", CreatedAt: at.Add(time.Minute)},
		{ID: 3, IsUser: false, Content: "也可以保留 Redis 7.2。", CreatedAt: at.Add(2 * time.Minute)},
		{ID: 4, IsUser: true, Content: "就按第二种方案。", CreatedAt: at.Add(3 * time.Minute)},
	}
	// 白板记录的是第二种方案被采纳：助手 m03 提议、学习者 m04 接受。
	document := model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV3,
		OpenItems: []model.SessionWhiteboardEntry{{
			Key: "redis-version", Text: "保留 Redis 7.2", Value: "7.2",
			SourceMessageIDs: []uint{3, 4}, Origin: model.SessionWhiteboardOriginAssistantAccepted, UpdatedAtMessageID: 4,
		}},
	}
	board := marshalFinalizeBoard(t, document)
	first := SessionRawProposal{
		Scope: model.MemoryScopeGlobal, Class: model.ProfileClassConstraint, Subject: "Redis 版本",
		Durability: model.MemoryDurabilityStable, Content: "Redis 升级到 7.4。",
		EvidenceIDs: []uint{4}, StatementEvidenceID: 4, AssistantIDs: []uint{2},
	}
	second := SessionRawProposal{
		Scope: model.MemoryScopeGlobal, Class: model.ProfileClassConstraint, Subject: "Redis 版本",
		Durability: model.MemoryDurabilityStable, Content: "保留 Redis 7.2。",
		EvidenceIDs: []uint{4}, StatementEvidenceID: 4, AssistantIDs: []uint{3},
	}
	service := newFinalizeService(t, fakeFinalizeBoards{content: &board}, fakeFinalizeMessages{messages: messages},
		nil, &fakeFinalizeFingerprints{known: map[string]struct{}{}}, fakeFinalizeGlobalMind{}, nil,
		&scriptedProposer{proposals: []SessionRawProposal{first, second}},
		&scriptedRelations{result: GlobalRelationResult{Relation: GlobalRelationIndependent, Reason: "no related entries"}}, 10)
	result, err := service.Extract(context.Background(), FinalizeRequest{UserID: 7, Cursor: FinalizeCursor{SessionID: "S-dev-selected"}})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(result.Proposals) != 1 || result.Proposals[0].Content != "保留 Redis 7.2。" {
		t.Fatalf("proposals=%+v, want only the cited, chosen proposal", result.Proposals)
	}
	if len(result.Skipped) != 1 || result.Skipped[0].Reason != FinalizeSkipAdoptionUnproven {
		t.Fatalf("skipped=%+v, want the first proposal refused", result.Skipped)
	}
}

// 正例：从今天起长期使用 → 今天是开始时间，适用期限是长期；不能因为命中
// 「今天」就把长期决定拒掉。
func TestAutoProposalAdoptionStartTimeIsStandingScope(t *testing.T) {
	at := time.Date(2026, 11, 24, 20, 1, 0, 0, time.FixedZone("CST", 8*3600))
	messages := []SessionMessage{
		{ID: 1, IsUser: true, Content: "从今天起长期使用 Go 1.23。", CreatedAt: at},
		{ID: 2, IsUser: false, Content: "好，从今天起长期按 Go 1.23 来。", CreatedAt: at.Add(time.Minute)},
	}
	result := extractLearnerFirstMessages(t, messages, learnerFirstProposal("长期使用 Go 1.23。"))
	if len(result.Proposals) != 1 || len(result.Skipped) != 0 {
		t.Fatalf("proposals=%d skipped=%+v, want the standing decision", len(result.Proposals), result.Skipped)
	}
	if result.Proposals[0].Content != "从今天起长期使用 Go 1.23。" {
		t.Fatalf("content=%q, want the learner's start-time decision", result.Proposals[0].Content)
	}
	if result.Proposals[0].Durability != model.MemoryDurabilityStable {
		t.Fatalf("durability=%s, want the standing decision", result.Proposals[0].Durability)
	}
}

// 反例：今天只用一次 → 临时操作，不能支撑长期决定。
func TestAutoProposalAdoptionOneOffScopeIsRefused(t *testing.T) {
	at := time.Date(2026, 11, 24, 20, 1, 0, 0, time.FixedZone("CST", 8*3600))
	messages := []SessionMessage{
		{ID: 1, IsUser: true, Content: "今天只用一次 Go 1.23。", CreatedAt: at},
		{ID: 2, IsUser: false, Content: "好，今天就按 Go 1.23 来。", CreatedAt: at.Add(time.Minute)},
	}
	result := extractLearnerFirstMessages(t, messages, learnerFirstProposal("长期使用 Go 1.23。"))
	if len(result.Proposals) != 0 {
		t.Fatalf("proposals=%+v, want the one-off scope refused", result.Proposals)
	}
	if len(result.Skipped) != 1 || result.Skipped[0].Reason != FinalizeSkipAdoptionUnproven {
		t.Fatalf("skipped=%+v, want adoption_unproven", result.Skipped)
	}
}

// 反例：助手单方面建议把这条理解归到 RPC，学习者没有点名、也没有采纳这条
// 建议。主题必须留空待用户选择；助手的建议不能作为点名证据。
func TestAutoProposalAdoptionAssistantTopicSuggestionStaysPending(t *testing.T) {
	at := time.Date(2026, 11, 24, 20, 1, 0, 0, time.FixedZone("CST", 8*3600))
	messages := []SessionMessage{
		{ID: 1, IsUser: true, Content: "我理解队列就是任务排队，先到先处理。", CreatedAt: at},
		{ID: 2, IsUser: false, Content: "这条理解可以归到 RPC 主题。", CreatedAt: at.Add(time.Minute)},
	}
	board := topicUnderstandingBoard(t, 1)
	topics := fakeFinalizeCatalogue{topics: []Topic{{Key: "rpc", Name: "RPC"}}}
	service := newFinalizeService(t, fakeFinalizeBoards{content: &board}, fakeFinalizeMessages{messages: messages},
		topics, &fakeFinalizeFingerprints{known: map[string]struct{}{}}, fakeFinalizeGlobalMind{},
		fakeFinalizeTopicMind{byTopic: map[string][]model.LongTermMemory{}},
		&scriptedProposer{proposals: []SessionRawProposal{{
			Scope: model.MemoryScopeTopic, Kind: FinalizeKindUnderstanding, TopicKey: "rpc", TopicUnique: false,
			Subject: "队列理解", Durability: model.MemoryDurabilityStable,
			Content:     "队列就是任务排队，先到先处理。",
			EvidenceIDs: []uint{1}, StatementEvidenceID: 1, AssistantIDs: []uint{2},
		}}},
		&scriptedRelations{result: GlobalRelationResult{Relation: GlobalRelationIndependent, Reason: "no related entries"}}, 10)
	result, err := service.Extract(context.Background(), FinalizeRequest{UserID: 7, Cursor: FinalizeCursor{SessionID: "S-dev-topic-suggestion"}})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(result.Proposals) != 1 {
		t.Fatalf("proposals=%d skipped=%+v, want the understanding with an empty attribution", len(result.Proposals), result.Skipped)
	}
	candidate := result.Proposals[0]
	if !candidate.TopicPending || candidate.TopicKey != "" {
		t.Fatalf("candidate=%+v, want the assistant's suggestion not to name the topic", candidate)
	}
	if len(candidate.AssistantUtterances) != 0 {
		t.Fatalf("assistant provenance=%v, want the unadopted suggestion dropped", candidate.AssistantUtterances)
	}
}

// 正例：学习者自己点名 RPC → 仍按点名预填主题。
func TestAutoProposalAdoptionLearnerNamedTopicStillPrefills(t *testing.T) {
	at := time.Date(2026, 11, 24, 20, 1, 0, 0, time.FixedZone("CST", 8*3600))
	messages := []SessionMessage{
		{ID: 1, IsUser: true, Content: "我在学 RPC 框架，我理解它就是像调本地函数一样调远程服务。", CreatedAt: at},
	}
	board := topicUnderstandingBoard(t, 1)
	topics := fakeFinalizeCatalogue{topics: []Topic{{Key: "rpc", Name: "RPC"}}}
	service := newFinalizeService(t, fakeFinalizeBoards{content: &board}, fakeFinalizeMessages{messages: messages},
		topics, &fakeFinalizeFingerprints{known: map[string]struct{}{}}, fakeFinalizeGlobalMind{},
		fakeFinalizeTopicMind{byTopic: map[string][]model.LongTermMemory{}},
		&scriptedProposer{proposals: []SessionRawProposal{{
			Scope: model.MemoryScopeTopic, Kind: FinalizeKindUnderstanding, TopicKey: "rpc", TopicUnique: false,
			Subject: "RPC 理解", Durability: model.MemoryDurabilityStable,
			Content:     "RPC 框架就是像调本地函数一样调远程服务。",
			EvidenceIDs: []uint{1}, StatementEvidenceID: 1,
		}}},
		&scriptedRelations{result: GlobalRelationResult{Relation: GlobalRelationIndependent, Reason: "no related entries"}}, 10)
	result, err := service.Extract(context.Background(), FinalizeRequest{UserID: 7, Cursor: FinalizeCursor{SessionID: "S-dev-topic-named"}})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(result.Proposals) != 1 {
		t.Fatalf("proposals=%d skipped=%+v, want the understanding card", len(result.Proposals), result.Skipped)
	}
	candidate := result.Proposals[0]
	if candidate.TopicPending || candidate.TopicKey != "rpc" {
		t.Fatalf("candidate=%+v, want the learner-named topic prefilled", candidate)
	}
}

func topicUnderstandingBoard(t *testing.T, messageID uint) string {
	t.Helper()
	document := model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV3,
		Constraints: []model.SessionWhiteboardEntry{{
			Key: "understanding", Text: "我的理解", Value: "理解",
			SourceMessageIDs: []uint{messageID}, Origin: model.SessionWhiteboardOriginUser, UpdatedAtMessageID: messageID,
		}},
	}
	return marshalFinalizeBoard(t, document)
}

// 反例二主用例：10 条无关画像 + 第 11 条同一事项旧值（跨类别），走完整
// Handoff → ProposeDraft。两层都必须看见旧值，最终草稿钉住它；确认前正式
// 记忆、旧条目状态、修订记录和掌握度都不变。
func TestAutoProposalRangeAlignmentLinksOldValueBeyondTen(t *testing.T) {
	acc := newFinalizeAcceptance(t)
	ctx := context.Background()
	sessionID := "fin-acc-long"
	stored := acc.seedSession(t, ctx, sessionID, nil, baseFinalizeMessages())
	// 旧值按「背景」存；随后 10 条无关「目标」画像的 updated_at 都更新，按
	// 生效清单的排序旧值正好排到第 11 条。
	old := seedProfileEntry(t, acc, "Go 版本", "当前项目用 Go 1.22。", model.ProfileClassBackground)
	for i := 0; i < 10; i++ {
		seedProfileEntry(t, acc, fmt.Sprintf("无关画像%d", i), fmt.Sprintf("无关的画像内容%d。", i), model.ProfileClassGoal)
	}
	if err := acc.db.Create(&model.TopicProgress{
		UserID: acc.userID, TopicKey: "go-basics", Topic: "Go 基础", MasteryScore: 80,
		Confidence: 0.8, ValidAttempts: 6, Level: model.TopicProgressDeveloping,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}).Error; err != nil {
		t.Fatal(err)
	}
	memoriesBefore := countRows(t, acc, &model.LongTermMemory{})
	revisionsBefore := countRows(t, acc, &model.MemoryRevision{})
	acc.global.inner = &deterministicGlobalClassifier{rules: []stubGlobalRule{
		{contains: "1.23", relation: "replacement", targetHint: "1.22"},
	}}
	acc.proposer.proposals = []SessionRawProposal{{
		Scope: model.MemoryScopeGlobal, Class: model.ProfileClassConstraint, Subject: "Go 版本",
		Durability: model.MemoryDurabilityStable, Content: "当前项目用 Go 1.23。",
		EvidenceIDs: []uint{stored[2].ID}, StatementEvidenceID: stored[2].ID,
	}}
	outcome, err := acc.runner.RunSession(ctx, acc.userID, sessionID)
	if err != nil {
		t.Fatalf("run session: %v", err)
	}
	if len(outcome.Persisted) != 1 || len(outcome.Proposals) != 1 {
		t.Fatalf("persisted=%v proposals=%d skipped=%+v, want one card", outcome.Persisted, len(outcome.Proposals), outcome.Skipped)
	}
	candidate := outcome.Proposals[0]
	if candidate.Relation.Relation != GlobalRelationReplacement || candidate.Relation.TargetMemoryID != old.ID {
		t.Fatalf("relation=%+v, want replacement of the 11th entry %s", candidate.Relation, old.ID)
	}
	relatedHasOld := false
	for _, row := range candidate.RelatedOld {
		if row.ID == old.ID {
			relatedHasOld = true
		}
	}
	if !relatedHasOld {
		t.Fatalf("related=%+v, want the 11th old value in the extraction range", candidate.RelatedOld)
	}
	// 两层看见的是同一批生效条目：提取侧判定与草稿入口判定都包含第 11 条。
	if len(acc.global.saw) < 2 {
		t.Fatalf("classifier calls=%d, want extraction and draft-entry judgements", len(acc.global.saw))
	}
	for call, rows := range acc.global.saw {
		if !memoryRowsContain(rows, old.ID) {
			t.Fatalf("judgement %d did not see the 11th entry: %v", call, memoryIDs(rows))
		}
	}
	drafts := acc.pendingDrafts(ctx)
	if len(drafts) != 1 {
		t.Fatalf("drafts=%d, want one pending card", len(drafts))
	}
	if !stringListContains(drafts[0].ConflictIDs, old.ID) {
		t.Fatalf("draft conflicts=%v, want the 11th old value pinned", drafts[0].ConflictIDs)
	}
	// 不变量：确认前零正式写入、旧条目仍生效、无修订记录新增、掌握度不变。
	if got := countRows(t, acc, &model.LongTermMemory{}); got != memoriesBefore {
		t.Fatalf("memories=%d, want %d (no formal write before confirmation)", got, memoriesBefore)
	}
	if got := seedProfileStatus(t, acc, old.ID); got != model.LongTermMemoryActive {
		t.Fatalf("old status=%s, want active before confirmation", got)
	}
	if got := countRows(t, acc, &model.MemoryRevision{}); got != revisionsBefore {
		t.Fatalf("revisions=%d, want %d (untouched)", got, revisionsBefore)
	}
	var progress model.TopicProgress
	if err := acc.db.Where("user_id = ? AND topic_key = ?", acc.userID, "go-basics").First(&progress).Error; err != nil {
		t.Fatal(err)
	}
	if progress.MasteryScore != 80 {
		t.Fatalf("mastery=%d, want 80 (untouched)", progress.MasteryScore)
	}
}

// 同值重述仍判重跳过：旧值排在 11 条画像里时，提取侧先用哈希查重挡下，
// 不再落到草稿入口才判重。
func TestAutoProposalRangeAlignmentSameValueStillDeduplicated(t *testing.T) {
	acc := newFinalizeAcceptance(t)
	ctx := context.Background()
	sessionID := "fin-acc-long"
	stored := acc.seedSession(t, ctx, sessionID, nil, baseFinalizeMessages())
	old := seedProfileEntry(t, acc, "Go 版本", "当前项目用 Go 1.22。", model.ProfileClassBackground)
	for i := 0; i < 10; i++ {
		seedProfileEntry(t, acc, fmt.Sprintf("无关同值画像%d", i), fmt.Sprintf("无关的同值画像内容%d。", i), model.ProfileClassGoal)
	}
	memoriesBefore := countRows(t, acc, &model.LongTermMemory{})
	revisionsBefore := countRows(t, acc, &model.MemoryRevision{})
	acc.proposer.proposals = []SessionRawProposal{{
		Scope: model.MemoryScopeGlobal, Class: model.ProfileClassConstraint, Subject: "Go 版本",
		Durability: model.MemoryDurabilityStable, Content: "当前项目用 Go 1.22。",
		EvidenceIDs: []uint{stored[2].ID}, StatementEvidenceID: stored[2].ID,
	}}
	outcome, err := acc.runner.RunSession(ctx, acc.userID, sessionID)
	if err != nil {
		t.Fatalf("run session: %v", err)
	}
	if len(outcome.Proposals) != 0 || len(outcome.Persisted) != 0 {
		t.Fatalf("proposals=%d persisted=%v, want the duplicate skipped", len(outcome.Proposals), outcome.Persisted)
	}
	if len(outcome.Skipped) != 1 || outcome.Skipped[0].Reason != FinalizeSkipDuplicateExisting {
		t.Fatalf("skipped=%+v, want duplicate_existing at the extraction range", outcome.Skipped)
	}
	if drafts := acc.pendingDrafts(ctx); len(drafts) != 0 {
		t.Fatalf("drafts=%+v, want no card for the duplicate", drafts)
	}
	if got := countRows(t, acc, &model.LongTermMemory{}); got != memoriesBefore {
		t.Fatalf("memories=%d, want %d", got, memoriesBefore)
	}
	if got := seedProfileStatus(t, acc, old.ID); got != model.LongTermMemoryActive {
		t.Fatalf("old status=%s, want active", got)
	}
	if got := countRows(t, acc, &model.MemoryRevision{}); got != revisionsBefore {
		t.Fatalf("revisions=%d, want %d", got, revisionsBefore)
	}
}

// 跨类的不同事项仍可并存：第 11 条旧值现在进入提取侧判定范围，但判定为
// independent，草稿不钉旧值，两条都保持生效。
func TestAutoProposalRangeAlignmentCrossClassDifferentMattersCoexist(t *testing.T) {
	acc := newFinalizeAcceptance(t)
	ctx := context.Background()
	sessionID := "fin-acc-long"
	stored := acc.seedSession(t, ctx, sessionID, nil, baseFinalizeMessages())
	goal := seedProfileEntry(t, acc, "目标", "2027 年春招目标岗位：Go 后端开发", model.ProfileClassGoal)
	for i := 0; i < 10; i++ {
		seedProfileEntry(t, acc, fmt.Sprintf("无关并存画像%d", i), fmt.Sprintf("无关的并存画像内容%d。", i), model.ProfileClassGoal)
	}
	memoriesBefore := countRows(t, acc, &model.LongTermMemory{})
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
		t.Fatalf("persisted=%v proposals=%d skipped=%+v, want the preference card", outcome.Persisted, len(outcome.Proposals), outcome.Skipped)
	}
	candidate := outcome.Proposals[0]
	if candidate.Relation.Relation != GlobalRelationIndependent {
		t.Fatalf("relation=%+v, want independent coexistence", candidate.Relation)
	}
	if len(acc.global.saw) == 0 || !memoryRowsContain(acc.global.saw[0], goal.ID) {
		t.Fatalf("extraction range=%v, want the cross-class goal in view", acc.global.saw)
	}
	drafts := acc.pendingDrafts(ctx)
	if len(drafts) != 1 || len(drafts[0].ConflictIDs) != 0 {
		t.Fatalf("drafts=%+v, want no replacement target", drafts)
	}
	if got := seedProfileStatus(t, acc, goal.ID); got != model.LongTermMemoryActive {
		t.Fatalf("goal status=%s, want active (coexist)", got)
	}
	if got := countRows(t, acc, &model.LongTermMemory{}); got != memoriesBefore {
		t.Fatalf("memories=%d, want %d", got, memoriesBefore)
	}
}

func memoryRowsContain(rows []model.LongTermMemory, id string) bool {
	for _, row := range rows {
		if row.ID == id {
			return true
		}
	}
	return false
}

func memoryIDs(rows []model.LongTermMemory) []string {
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	return ids
}
