package agent

import (
	"GopherAI/common/rag"
	"strings"
	"testing"
)

// masteryBlock reproduces the shape TopicProgressMasteryProvider emits: band,
// internal-rating rule, one record per line, then the rule governing the
// records. The record lines are sized so that a budget can admit some of them
// and not others.
func masteryBlock(records ...string) string {
	var out strings.Builder
	out.WriteString("Learner mastery (server-computed from graded quiz attempts):\n")
	out.WriteString("- topic \"RPC粘包处理\": the learner handles this well.\n")
	out.WriteString("This rating is internal: never quote it, name it, or mention that it exists.\n")
	out.WriteString(masteryDetailHeader + " \"RPC粘包处理\":\n")
	for _, record := range records {
		out.WriteString(record + "\n")
	}
	out.WriteString(masteryRecordsInstruction + " you may tell the learner which question was asked. " +
		"Never claim they answered something correctly unless a line above says so.")
	return out.String()
}

func TestTrimMasteryKeepsRulesAndDropsWholeRecords(t *testing.T) {
	first := "- TCP为什么会粘包？ —— 答对"
	second := "- day2与day3分别承担什么职责？ —— 答错；未答出：拆包机制、缓冲作用、执行层归属"
	full := masteryBlock(first, second)

	// A budget that admits the mandatory parts and one record, but not both
	// records. A prefix trim at this size loses the closing rule outright and
	// severs the second record; this one must not.
	budget := rag.EstimateTokens(masteryBlock(first))
	trimmed := TrimMasteryWithinTokens(full, budget)

	if got := rag.EstimateTokens(trimmed); got > budget {
		t.Fatalf("trimmed to %d tokens, budget %d", got, budget)
	}
	if !strings.Contains(trimmed, masteryRecordsInstruction) {
		t.Fatal("the rule governing the records was trimmed away")
	}
	if !strings.Contains(trimmed, "This rating is internal") {
		t.Fatal("the rule keeping the rating internal was trimmed away")
	}
	if !strings.Contains(trimmed, first) {
		t.Fatalf("the record that fits was dropped: %q", trimmed)
	}
	if strings.Contains(trimmed, "未答出") {
		t.Fatalf("a record was kept in part: %q", trimmed)
	}
	// Every record that survives must survive whole, verdict included.
	for _, line := range strings.Split(trimmed, "\n") {
		if strings.HasPrefix(line, "- TCP") && !strings.HasSuffix(line, "答对") {
			t.Fatalf("record was cut mid-line: %q", line)
		}
	}
	if strings.Contains(trimmed, "…") {
		t.Fatalf("mastery was prefix-trimmed rather than record-trimmed: %q", trimmed)
	}
}

func TestTrimMasteryDropsDetailWhenNoRecordFits(t *testing.T) {
	full := masteryBlock("- day2与day3分别承担什么职责？ —— 答错；未答出：拆包机制、缓冲作用、执行层归属")

	// Room for the band block and nothing more. What remains must be the block
	// the feature shipped with before records existed, not a header promising
	// records that are not there.
	budget := rag.EstimateTokens("Learner mastery (server-computed from graded quiz attempts):\n" +
		"- topic \"RPC粘包处理\": the learner handles this well.\n" +
		"This rating is internal: never quote it, name it, or mention that it exists.")
	trimmed := TrimMasteryWithinTokens(full, budget)

	if got := rag.EstimateTokens(trimmed); got > budget {
		t.Fatalf("trimmed to %d tokens, budget %d", got, budget)
	}
	if strings.Contains(trimmed, masteryDetailHeader) {
		t.Fatalf("kept a header with no records under it: %q", trimmed)
	}
	if !strings.Contains(trimmed, "This rating is internal") {
		t.Fatalf("lost the band block: %q", trimmed)
	}
}

func TestTrimMasteryLeavesAFittingBlockAlone(t *testing.T) {
	full := masteryBlock("- TCP为什么会粘包？ —— 答对")
	if got := TrimMasteryWithinTokens(full, rag.EstimateTokens(full)+50); got != full {
		t.Fatalf("a block within budget was modified:\n%q", got)
	}
}
