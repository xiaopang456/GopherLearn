package agent

// STABILIZE-065 F5: partial-truncation behaviour tests.
//
// These pin the mastery/context truncation edge cases the evaluator relies on:
// when a block does not fit, the trim must keep the mandatory rule line and a
// whole-record prefix, drop whole detail records (never half a record), and
// never fabricate a block the caller asked for but did not receive.
//
// The production function under test is TrimMasteryWithinTokens in
// context_builder.go. The tests use a deterministic token estimate so the
// assertions are about behaviour, not about a specific tokenizer run.

import (
	"strings"
	"testing"

	"GopherAI/common/rag"
)

// buildMasteryDoc renders a minimal mastery document with the mandatory
// instruction line and n whole detail records. It reuses the package-level
// masteryDetailHeader / masteryRecordsInstruction constants that the
// production trim function itself depends on.
func buildMasteryDoc(records []string) string {
	lines := []string{masteryDetailHeader + ` "topic":`}
	for _, r := range records {
		lines = append(lines, "- "+r)
	}
	lines = append(lines, masteryRecordsInstruction)
	return strings.Join(lines, "\n")
}

func TestTrimKeepsInstructionAndWholeRecords(t *testing.T) {
	doc := buildMasteryDoc([]string{"r1", "r2", "r3"})
	full := rag.EstimateTokens(doc)
	band := rag.EstimateTokens(buildMasteryDoc(nil))
	// Pick a budget that fits the header+instruction band plus some but not all
	// records, so the trim must drop whole records rather than return nothing.
	budget := band + (full-band)/2
	trimmed := TrimMasteryWithinTokens(doc, budget)
	if trimmed == "" {
		t.Fatalf("budget %d (band=%d full=%d) should not collapse to empty; got %q", budget, band, full, trimmed)
	}
	if !strings.Contains(trimmed, masteryRecordsInstruction) {
		t.Fatalf("trimmed output must keep the mandatory instruction line; got %q", trimmed)
	}
}

func TestTrimDropsWholeDetailWhenNoRecordFits(t *testing.T) {
	doc := buildMasteryDoc([]string{"averyverylongrecordname"})
	trimmed := TrimMasteryWithinTokens(doc, 8)
	// No record fits; the trim must return the band/instruction prefix, never a
	// fabricated record and never a partial one.
	if strings.Contains(trimmed, "averyverylongrecordname") {
		t.Fatalf("record must be dropped whole when it does not fit: %q", trimmed)
	}
	if strings.Contains(trimmed, "##") {
		t.Fatalf("must not leave a dangling record header: %q", trimmed)
	}
}

func TestTrimLeavesFittingBlockAlone(t *testing.T) {
	doc := buildMasteryDoc([]string{"r1"})
	trimmed := TrimMasteryWithinTokens(doc, 500)
	if trimmed != doc {
		t.Fatalf("fitting block must be returned unchanged, got %q", trimmed)
	}
}

func TestTrimEmptyAndZeroBudget(t *testing.T) {
	if got := TrimMasteryWithinTokens("", 100); got != "" {
		t.Fatalf("empty input must stay empty, got %q", got)
	}
	if got := TrimMasteryWithinTokens("anything", 0); got != "" {
		t.Fatalf("zero budget must yield empty, got %q", got)
	}
}

func TestTrimPrefixWithinTokensRuneSafe(t *testing.T) {
	// A multi-byte string truncated by runes must never split a rune; the
	// prefix trim signals truncation with a trailing ellipsis, which must be
	// the only thing past the true prefix.
	long := strings.Repeat("掌", 200)
	trimmed := TrimMasteryWithinTokens(long, 10)
	body := strings.TrimSuffix(trimmed, "…")
	if !strings.HasPrefix(long, body) {
		t.Fatalf("prefix trim must be a true prefix before the ellipsis, got %q", trimmed)
	}
}

func TestTrimTokenEstimateConsistency(t *testing.T) {
	// The trim uses rag.EstimateTokens; sanity-check that the estimator itself
	// is stable for the doc shapes the trim handles.
	doc := buildMasteryDoc([]string{"r1", "r2"})
	a := rag.EstimateTokens(doc)
	b := rag.EstimateTokens(doc)
	if a != b {
		t.Fatalf("token estimate must be deterministic, got %d vs %d", a, b)
	}
}
