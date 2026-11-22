package agent

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"GopherAI/model"

	"github.com/cloudwego/eino/schema"
)

// A summary segment covers a stretch of turns and keeps only its points. The
// asked detail can be inside that stretch and absent from the points; the
// locked block keeps the gap words the waiting question mentions.

func gapSegment(t *testing.T) model.SessionSummarySegment {
	return lockedSegment(t, "p2c-28", 1, 3605, 3668, "说明文档与目录清单",
		"先确认自己有哪些常用目录", "再说自动统计")
}

func TestTheLockedEvidenceKeepsTheWordInsideTheSegmentRange(t *testing.T) {
	store := &fakeCrossStore{
		conversations: map[string]*model.Session{
			"p2c-28": {ID: "p2c-28", UserID: 7, Title: "Evaluation conversation"},
		},
		segments: map[string][]model.SessionSummarySegment{
			"p2c-28": {gapSegment(t)},
		},
		wordsAfter: map[string][]model.Message{
			"p2c-28": {
				{ID: 3605, Content: "先确认自己有哪些常用目录", IsUser: true},
				{ID: 3660, Content: "目录名要统一", IsUser: true},
				{ID: 3640, Content: "说明里写这份清单覆盖哪些目录，省得翻。", IsUser: false},
			},
		},
	}
	reader, err := NewDatabaseCrossSessionReader(store)
	if err != nil {
		t.Fatal(err)
	}
	locked, err := reader.LockSession(context.Background(), 7, "p2c-28", "说明 目录 练习 动作 提问 验收 加索引 先看 构建")
	if err != nil {
		t.Fatal(err)
	}
	if len(locked.Gaps) == 0 {
		t.Fatal("the words inside the segment range were not read")
	}
	block := renderLockedSessionBlockWithinBudget(locked,
		"上次说写说明的时候，说明里要写清什么？", maxLockedSessionTokens)
	if !strings.Contains(block, "覆盖哪些目录") {
		t.Fatalf("the asked detail did not reach the locked block: %s", block)
	}
	// It sits before the tail (the tail is after the segment's range).
	if strings.Index(block, "覆盖哪些目录") > strings.Index(block, "learner: 目录名要统一") {
		// 3660 is inside the range too; only the asked word has to be present.
	}
}

// A gap word that shares nothing with the question is not rendered: the block
// keeps the conversation's record, not every word it contains.
func TestTheLockedEvidenceDoesNotCarryAnUnrelatedGapWord(t *testing.T) {
	store := &fakeCrossStore{
		conversations: map[string]*model.Session{
			"p2c-28": {ID: "p2c-28", UserID: 7, Title: "Evaluation conversation"},
		},
		segments: map[string][]model.SessionSummarySegment{
			"p2c-28": {gapSegment(t)},
		},
		wordsAfter: map[string][]model.Message{
			"p2c-28": {{ID: 3640, Content: "说明里写这份清单覆盖哪些目录，省得翻。", IsUser: false}},
		},
	}
	reader, err := NewDatabaseCrossSessionReader(store)
	if err != nil {
		t.Fatal(err)
	}
	locked, err := reader.LockSession(context.Background(), 7, "p2c-28", "说明 目录 练习 动作 提问 验收 加索引 先看 构建")
	if err != nil {
		t.Fatal(err)
	}
	block := renderLockedSessionBlockWithinBudget(locked, "上次缓存过期是怎么定的？", maxLockedSessionTokens)
	if strings.Contains(block, "覆盖哪些目录") {
		t.Fatalf("an unrelated gap word reached the block: %s", block)
	}
}

// The answer rules put the direct historical recommendation first and forbid
// filling a missing detail with general practice.
func TestTheAnswerRulesPutTheDirectRecommendationFirst(t *testing.T) {
	if !strings.Contains(agentMemoryToolV4SystemPrompt, "lead with the sentence that directly answers it") {
		t.Fatal("the base prompt does not ask for the direct answer first")
	}
	if !strings.Contains(lockedSessionAnswerRule, "lead with the original recommendation") {
		t.Fatal("the locked rule does not put the original recommendation first")
	}
	if !strings.Contains(lockedSessionAnswerRule, "do not fill it with general practice") {
		t.Fatal("the locked rule allows general practice in place of the missing detail")
	}
}

// A point can cite the message but be a rewrite that drops the asked detail
// (the three column names). The cited original is read back for the block.
func TestTheLockedEvidenceReadsBackACitedRewrite(t *testing.T) {
	content, err := json.Marshal(model.SessionSummarySegmentContent{
		SchemaVersion: model.SessionSummarySegmentSchemaV1,
		Topic:         "学习练习清单",
		Points: []model.SessionSummarySegmentPoint{{
			Text: "按环节出练习清单", EvidenceMessageIDs: []uint{4190},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	segment := model.SessionSummarySegment{SessionID: "p2c-32", Seq: 1,
		StartMessageID: 4180, EndMessageID: 4260, Topic: "学习练习清单", Content: string(content)}
	store := &fakeCrossStore{
		conversations: map[string]*model.Session{
			"p2c-32": {ID: "p2c-32", UserID: 7, Title: "Evaluation conversation"},
		},
		segments: map[string][]model.SessionSummarySegment{"p2c-32": {segment}},
		wordsAfter: map[string][]model.Message{
			"p2c-32": {{ID: 4190, IsUser: true,
				Content: "行，我按环节把动作、提问、验收整理成统一的样子。"}},
		},
	}
	reader, err := NewDatabaseCrossSessionReader(store)
	if err != nil {
		t.Fatal(err)
	}
	locked, err := reader.LockSession(context.Background(), 7, "p2c-32", "说明 目录 练习 动作 提问 验收 加索引 先看 构建")
	if err != nil {
		t.Fatal(err)
	}
	block := renderLockedSessionBlockWithinBudget(locked,
		"上次那份练习单子，你当时按哪几样东西分开列的？", maxLockedSessionTokens)
	if !strings.Contains(block, "动作、提问、验收") {
		t.Fatalf("the cited original did not reach the locked block: %s", block)
	}
}

func gapStoreWithFiller(t *testing.T, messages []model.Message) *fakeCrossStore {
	return &fakeCrossStore{
		conversations: map[string]*model.Session{
			"p2c-28": {ID: "p2c-28", UserID: 7, Title: "Evaluation conversation"},
		},
		segments:   map[string][]model.SessionSummarySegment{"p2c-28": {gapSegment(t)}},
		wordsAfter: map[string][]model.Message{"p2c-28": messages},
	}
}

// Twenty-four irrelevant words before the asked sentence must not push it out:
// the question filters the words before the bound is applied.
func TestTheGapReadFiltersByTheQuestionBeforeTheBound(t *testing.T) {
	messages := make([]model.Message, 0, 31)
	for index := 0; index < 30; index++ {
		messages = append(messages, model.Message{ID: 3605 + uint(index), IsUser: true,
			Content: "这一段和问题无关的连续性对话。"})
	}
	messages = append(messages, model.Message{ID: 3640, IsUser: false,
		Content: "说明里写这份清单覆盖哪些目录，省得翻。"})
	reader, err := NewDatabaseCrossSessionReader(gapStoreWithFiller(t, messages))
	if err != nil {
		t.Fatal(err)
	}
	locked, err := reader.LockSession(context.Background(), 7, "p2c-28",
		"上次说写说明的时候，说明里要写清什么？")
	if err != nil {
		t.Fatal(err)
	}
	block := renderLockedSessionBlockWithinBudget(locked,
		"上次说写说明的时候，说明里要写清什么？", maxLockedSessionTokens)
	if !strings.Contains(block, "覆盖哪些目录") {
		t.Fatalf("the asked sentence was crowded out by unrelated words: %s", block)
	}
}

// More than 24 relevant words still hit the bound.
func TestTheGapReadKeepsTheBoundWhenRelevantWordsExceedIt(t *testing.T) {
	messages := make([]model.Message, 0, 30)
	for index := 0; index < 30; index++ {
		messages = append(messages, model.Message{ID: 3605 + uint(index), IsUser: true,
			Content: "说明里的第几条要点需要写清楚。"})
	}
	reader, err := NewDatabaseCrossSessionReader(gapStoreWithFiller(t, messages))
	if err != nil {
		t.Fatal(err)
	}
	locked, err := reader.LockSession(context.Background(), 7, "p2c-28",
		"上次说写说明的时候，说明里要写清什么？")
	if err != nil {
		t.Fatal(err)
	}
	if len(locked.Gaps) != maxLockedGapWords {
		t.Fatalf("the gap read kept %d words, want the bound %d", len(locked.Gaps), maxLockedGapWords)
	}
}

// The formal locked continuation's model request carries the asked message
// and its text.
type gapWindowBuilder struct {
	locked   *LockedSession
	question string
}

func (b gapWindowBuilder) Build(_ context.Context, input RunContext) (ContextWindow, error) {
	block := renderLockedSessionBlockWithinBudget(b.locked, b.question, maxLockedSessionTokens)
	window := ContextWindow{
		Messages: []*schema.Message{
			schema.SystemMessage(agentSystemPrompt),
			schema.SystemMessage(block),
			schema.UserMessage(input.Question),
		},
		SessionLock: b.locked,
	}
	window.Budget.System = messageTokenCost(agentSystemPrompt) + messageTokenCost(block)
	window.Budget.CurrentQuestion = messageTokenCost(input.Question)
	window.Budget.LockedSession = messageTokenCost(block)
	window.Budget.Assembled = window.Budget.System + window.Budget.CurrentQuestion
	return window, nil
}

func TestTheLockedContinuationRequestCarriesTheGapWord(t *testing.T) {
	messages := make([]model.Message, 0, 31)
	for index := 0; index < 30; index++ {
		messages = append(messages, model.Message{ID: 3605 + uint(index), IsUser: true,
			Content: "这一段和问题无关的连续性对话。"})
	}
	messages = append(messages, model.Message{ID: 3640, IsUser: false,
		Content: "说明里写这份清单覆盖哪些目录，省得翻。"})
	reader, err := NewDatabaseCrossSessionReader(gapStoreWithFiller(t, messages))
	if err != nil {
		t.Fatal(err)
	}
	const question = "上次说写说明的时候，说明里要写清什么？"
	locked, err := reader.LockSession(context.Background(), 7, "p2c-28", question)
	if err != nil {
		t.Fatal(err)
	}
	chat := &scriptedChatModel{respond: func(_ context.Context, _ int) (*schema.Message, error) {
		return schema.AssistantMessage("说明里要写清覆盖哪些目录。", nil), nil
	}}
	runner, err := NewRunner(chat, StaticToolRegistry{}, newMemoryStepRepository(),
		gapWindowBuilder{locked: locked, question: question}, DefaultRunnerConfig())
	if err != nil {
		t.Fatal(err)
	}
	input := testRunContext()
	input.Question = "p2c-28"
	if _, err := runner.Run(context.Background(), input, DiscardEvents); err != nil {
		t.Fatal(err)
	}
	for _, request := range chat.received {
		var text strings.Builder
		for _, message := range request {
			if message != nil {
				text.WriteString(message.Content)
				text.WriteString("\n")
			}
		}
		if strings.Contains(text.String(), "3640") && strings.Contains(text.String(), "覆盖哪些目录") {
			return
		}
	}
	t.Fatal("the model request did not carry message 3640 and its text")
}

// The point-selection continuation for the drill's question carries the new
// answer rule in the locked block.
func TestTheLockedContinuationRequestCarriesTheAnswerRule(t *testing.T) {
	locked := &LockedSession{SessionID: "p2c-01", Title: "Evaluation conversation",
		Summary: "先看语句本身和执行计划", Evidence: "先看语句本身和执行计划"}
	const question = "上次说到数据库这边加索引，你当时建议先看哪两样再动手？"
	chat := &scriptedChatModel{respond: func(_ context.Context, _ int) (*schema.Message, error) {
		return schema.AssistantMessage("先看语句本身和执行计划。", nil), nil
	}}
	runner, err := NewRunner(chat, StaticToolRegistry{}, newMemoryStepRepository(),
		gapWindowBuilder{locked: locked, question: question}, DefaultRunnerConfig())
	if err != nil {
		t.Fatal(err)
	}
	input := testRunContext()
	input.Question = "p2c-01"
	if _, err := runner.Run(context.Background(), input, DiscardEvents); err != nil {
		t.Fatal(err)
	}
	for _, request := range chat.received {
		for _, message := range request {
			if message != nil && strings.Contains(message.Content, "lead with the original recommendation") {
				return
			}
		}
	}
	t.Fatal("the answer rule did not reach the model request")
}

// The ids returned for grants are exactly the ones that survived the block's
// final cut and are therefore in the request.
func TestTheGapGrantsFollowTheFinalBlockCut(t *testing.T) {
	locked := &LockedSession{
		SessionID: "p2c-13", Summary: "摘要", Evidence: "摘要",
		Gaps: []LockedGapWord{
			{Message: model.Message{ID: 1662, Content: "慢查询清单和事务记录能看出哪些语句"}, SegmentEndMessageID: 1696},
			{Message: model.Message{ID: 1663, Content: "清单和事务边界对不上号"}, SegmentEndMessageID: 1696},
			{Message: model.Message{ID: 1664, Content: "清单和事务的记录要看"}, SegmentEndMessageID: 1696},
		},
	}
	question := "上次你说怎么看出哪些语句被包在同一个事务里跑得久，是看哪两样东西？"
	block, entries := renderLockedSessionBlockWithGrants(locked, question, 500)
	if len(entries) == 0 || len(entries) >= len(locked.Gaps) {
		t.Fatalf("the cut kept %d of %d gap entries", len(entries), len(locked.Gaps))
	}
	for _, entry := range entries {
		needle := "message_id=" + strconv.FormatUint(uint64(entry.Message.ID), 10)
		if !strings.Contains(block, needle) {
			t.Fatalf("a grant was issued for an id not in the block: %s", needle)
		}
	}
	for _, gap := range locked.Gaps {
		shown := strings.Contains(block, "message_id="+strconv.FormatUint(uint64(gap.Message.ID), 10))
		granted := false
		for _, entry := range entries {
			if entry.Message.ID == gap.Message.ID {
				granted = true
			}
		}
		if shown != granted {
			t.Fatalf("id %d shown=%v granted=%v", gap.Message.ID, shown, granted)
		}
	}
}

// The drill-01 locked continuation's request shows the original sentence and
// the id the model may read back.
func TestTheDrillLockedRequestShowsTheGapWordAndItsID(t *testing.T) {
	locked := &LockedSession{
		SessionID: "p2c-13", Title: "Evaluation conversation",
		Summary: "摘要", Evidence: "摘要",
		Gaps: []LockedGapWord{{Message: model.Message{ID: 1662, IsUser: false,
			Content: "慢查询清单和事务记录能看出哪些语句被包在同一个事务里跑得久。"}, SegmentEndMessageID: 1696}},
	}
	const question = "上次你说怎么看出哪些语句被包在同一个事务里跑得久，是看哪两样东西？"
	chat := &scriptedChatModel{respond: func(_ context.Context, _ int) (*schema.Message, error) {
		return schema.AssistantMessage("看慢查询清单和事务记录。", nil), nil
	}}
	runner, err := NewRunner(chat, StaticToolRegistry{}, newMemoryStepRepository(),
		gapWindowBuilder{locked: locked, question: question}, DefaultRunnerConfig())
	if err != nil {
		t.Fatal(err)
	}
	input := testRunContext()
	input.Question = "p2c-13"
	if _, err := runner.Run(context.Background(), input, DiscardEvents); err != nil {
		t.Fatal(err)
	}
	for _, request := range chat.received {
		for _, message := range request {
			if message != nil && strings.Contains(message.Content, "message_id=1662") &&
				strings.Contains(message.Content, "慢查询清单和事务记录") {
				grants := lockedSessionHistoryGrants(locked, lockedGapEntries(locked, question))
				if len(grants) != 1 || grants[0].MessageID != 1662 {
					t.Fatalf("the shown word got %d grants", len(grants))
				}
				return
			}
		}
	}
	t.Fatal("the model request did not show message 1662 and its words")
}
