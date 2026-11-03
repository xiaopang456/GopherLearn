package conversation

import (
	"errors"
	"fmt"
	"testing"
)

// The product's own log prints the type of the error but not its text, so the
// diagnosis keeps the whole error and names the step it came from.
func TestSummaryFailureStageNamesTheStep(t *testing.T) {
	for _, tc := range []struct {
		text string
		want string
	}{
		{"generate rolling summary: connection reset by peer", "model_call"},
		{"rolling summary input exceeds token limit", "length_check"},
		{"rolling summary exceeds output token limit", "length_check"},
		{"invalid session summary goal", "length_check"},
		{"decode rolling summary: session summary has too many evidence items", "structure_validation"},
		{"decode rolling summary: invalid character 'x'", "output_parse"},
		{"decode previous rolling summary: invalid character 'x'", "output_parse"},
		{"rolling summary must use schema version 2", "structure_validation"},
		{"rolling summary cited non-user or unavailable message 42", "structure_validation"},
		{"rolling summary evidence: text is required", "structure_validation"},
		{"summary model returned no durable session state", "empty_output"},
	} {
		if got := summaryStage(tc.text); got != tc.want {
			t.Fatalf("stage(%q) = %s, want %s", tc.text, got, tc.want)
		}
	}
}

func TestErrorChainKeepsEveryLayer(t *testing.T) {
	inner := errors.New("invalid character 'x'")
	middle := fmt.Errorf("decode rolling summary: %w", inner)
	outer := fmt.Errorf("compaction unavailable: %w", middle)
	chain := errorChain(outer)
	if len(chain) != 3 {
		t.Fatalf("chain = %v", chain)
	}
	if chain[0] != "compaction unavailable: decode rolling summary: invalid character 'x'" || chain[2] != "invalid character 'x'" {
		t.Fatalf("chain = %v", chain)
	}
}
