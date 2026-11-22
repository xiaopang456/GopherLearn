package agent

import (
	"strings"
	"testing"
)

// The summary and the unsummarized tail share one cap.
//
// The summary is the conversation's compressed body, the tail is the stretch
// no summary covers, and the order is fixed: the summary goes in first. A tail
// that does not fit is cut - it must never cost the summary its place, because
// that would trade evidence the conversation HAS for evidence it might have.

const lockedBudgetTestQuestion = "上次你给我排的那三道题里，第二道练的是什么？"

func lockedSessionWithTail(summary string, tailLines []string) *LockedSession {
	tail := strings.Join(tailLines, "\n")
	return &LockedSession{
		SessionID: "p2c-06", Title: "Evaluation conversation",
		Summary: summary, Tail: tail, TailMessages: len(tailLines),
		Evidence: summary + "\n" + tail,
	}
}

// The block stays byte for byte the same when everything fits.
//
// The published measurements were taken against these bytes; adding a way to
// append a tail must not move the block it appends to.
func TestTheLockedBlockIsUnchangedWhenEverythingFits(t *testing.T) {
	locked := lockedSessionWithTail("怀疑是全量重建，但没细看",
		[]string{"learner: 给我三个练习吧", "assistant: 三道：构建缓存、依赖裁剪、静态检查"})
	want := renderLockedSessionBlock(locked, lockedBudgetTestQuestion)
	got := renderLockedSessionBlockWithinBudget(locked, lockedBudgetTestQuestion, maxLockedSessionTokens)
	if got != want {
		t.Fatalf("the budget renderer changed the block that already fit:\nwant %q\n got %q", want, got)
	}
}

// A tail that does not fit is cut; the summary stays.
func TestTheTailIsCutToFitAndTheSummaryStays(t *testing.T) {
	summary := strings.Repeat("摘要事实：构建变慢的排查结论。", 16)
	firstTail := "learner: 第一条尾部原话，答案可能就在这里"
	lastTail := "assistant: 最后一条尾部原话，肯定放不下了"
	lines := []string{firstTail}
	for index := 0; index < 40; index++ {
		lines = append(lines, "learner: "+strings.Repeat("很长的尾部原话内容", 18))
	}
	lines = append(lines, lastTail)
	locked := lockedSessionWithTail(summary, lines)

	single := renderLockedSessionBlock(locked, lockedBudgetTestQuestion)
	if messageTokenCost(single) <= maxLockedSessionTokens {
		t.Fatalf("the fixture is not over the cap: %d", messageTokenCost(single))
	}
	block := renderLockedSessionBlockWithinBudget(locked, lockedBudgetTestQuestion, maxLockedSessionTokens)
	if block == "" {
		t.Fatal("a block whose summary fits was dropped")
	}
	if cost := messageTokenCost(block); cost > maxLockedSessionTokens {
		t.Fatalf("the block is %d tokens, over the %d cap", cost, maxLockedSessionTokens)
	}
	if !strings.Contains(block, "摘要事实") {
		t.Fatal("the summary did not survive the tail trim")
	}
	if !strings.Contains(block, firstTail) {
		t.Fatal("the first tail line that fits was dropped")
	}
	if strings.Contains(block, lastTail) {
		t.Fatal("a tail line that does not fit was kept")
	}
	if !strings.HasSuffix(block, lockedSessionAnswerRule) {
		t.Fatal("the block lost its answer rule")
	}
}

// A summary that does not fit even alone drops the block.
//
// The turn then falls back to what it had before the lock: the clarification
// block and the tools. That is the existing behaviour, and the tail must not
// resurrect a block the summary alone cannot carry.
func TestASummaryThatDoesNotFitDropsTheBlock(t *testing.T) {
	locked := lockedSessionWithTail(strings.Repeat("撑爆预算的摘要事实。", 400), []string{"learner: 尾部"})
	if got := renderLockedSessionBlockWithinBudget(locked, lockedBudgetTestQuestion,
		maxLockedSessionTokens); got != "" {
		t.Fatalf("an over-budget summary produced a block of %d bytes", len(got))
	}
}

// The fallback path (a conversation with no summary) keeps its own bound: its
// words are the evidence, and there is no tail to cut.
func TestTheNoSummaryEvidenceKeepsTheDropBehaviour(t *testing.T) {
	locked := &LockedSession{
		SessionID: "session-b", Title: "浔漳",
		Evidence: strings.Repeat("没有摘要时的原话。", 2000),
		Messages: 40,
	}
	if got := renderLockedSessionBlockWithinBudget(locked, lockedBudgetTestQuestion,
		maxLockedSessionTokens); got != "" {
		t.Fatalf("an over-budget word fallback produced a block of %d bytes", len(got))
	}
}
