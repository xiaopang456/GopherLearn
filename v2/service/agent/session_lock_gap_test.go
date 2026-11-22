package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"GopherAI/model"
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
	locked, err := reader.LockSession(context.Background(), 7, "p2c-28")
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
	locked, err := reader.LockSession(context.Background(), 7, "p2c-28")
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
	locked, err := reader.LockSession(context.Background(), 7, "p2c-32")
	if err != nil {
		t.Fatal(err)
	}
	block := renderLockedSessionBlockWithinBudget(locked,
		"上次那份练习单子，你当时按哪几样东西分开列的？", maxLockedSessionTokens)
	if !strings.Contains(block, "动作、提问、验收") {
		t.Fatalf("the cited original did not reach the locked block: %s", block)
	}
}
