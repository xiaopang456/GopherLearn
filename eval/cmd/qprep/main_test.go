package main

import "testing"

// TestPackCallsWithoutMixingKeepsDocumentsApart checks that a document never
// shares a call with another one when mixing is off.
func TestPackCallsWithoutMixingKeepsDocumentsApart(t *testing.T) {
	take := map[string]int{"a.md": 3, "b.md": 4}
	calls, mixed := packCalls(take, []string{"a.md", "b.md"}, 4, false)
	if calls != 2 {
		t.Errorf("calls = %d, want 2 (three passages of a.md need a call of their own)", calls)
	}
	if mixed != 0 {
		t.Errorf("mixed calls = %d, want 0", mixed)
	}
}

// TestPackCallsWithMixingFillsTheLeftovers checks that the leftovers of a
// document are topped up with the next document, and that one call never
// spans more than two documents.
func TestPackCallsWithMixingFillsTheLeftovers(t *testing.T) {
	take := map[string]int{"a.md": 3, "b.md": 4}
	calls, mixed := packCalls(take, []string{"a.md", "b.md"}, 4, true)
	if calls != 2 {
		t.Errorf("calls = %d, want 2 (the three passages of a.md are topped up)", calls)
	}
	if mixed != 1 {
		t.Errorf("mixed calls = %d, want 1", mixed)
	}
}

// TestPackCallsNeverSpansThreeDocuments checks the two-document limit.
func TestPackCallsNeverSpansThreeDocuments(t *testing.T) {
	take := map[string]int{"a.md": 1, "b.md": 1, "c.md": 1}
	calls, mixed := packCalls(take, []string{"a.md", "b.md", "c.md"}, 4, true)
	if calls != 2 {
		t.Errorf("calls = %d, want 2 (a call holds at most two documents)", calls)
	}
	if mixed != 1 {
		t.Errorf("mixed calls = %d, want 1", mixed)
	}
}
