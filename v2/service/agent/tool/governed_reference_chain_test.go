package agenttool

import (
	"errors"
	"strings"
	"testing"

	serviceagent "GopherAI/service/agent"
)

// STABILIZE-058 v1.3, group B: one mapping, end to end.
//
// v1.2 showed the model [E1.d1] and registered something else. governCards
// built a refs slice and never appended to it, discarded what Register handed
// back, and Register itself auto-numbers - so every slot went into the shared
// registry as a plain E#, the dotted label the model actually read was never
// registered at all, and the call record kept the old whole-card refs.
//
// The consequence is the one that matters: an answer citing [E1.d1] could not
// be resolved, while the invisible whole-card E# stayed citable. The model was
// shown one vocabulary and judged against another.

// B1: the label the model reads must resolve in the run's own registry.
func TestDottedRefResolvesInTheRunRegistry(t *testing.T) {
	built, evidence := governedTool(t, governedFixture(), true)
	output := runGovernedSearch(t, built)

	for _, card := range output.Results {
		entry, err := evidence.Lookup(card.Ref)
		if err != nil {
			t.Fatalf("%s was shown to the model and cannot be resolved: %v", card.Ref, err)
		}
		if !strings.HasPrefix(entry.InternalID, "ea_") && !strings.HasPrefix(entry.InternalID, "lg_") {
			t.Fatalf("%s resolves to %q, which is not a stable assertion identity", card.Ref, entry.InternalID)
		}
	}
}

// B2: the call record must name exactly what the observation showed.
func TestCallRefsEqualTheObservationRefs(t *testing.T) {
	built, evidence := governedTool(t, governedFixture(), true)
	output := runGovernedSearch(t, built)

	calls := evidence.Calls()
	if len(calls) != 1 {
		t.Fatalf("recorded %d calls, want 1", len(calls))
	}
	shown := map[string]struct{}{}
	for _, card := range output.Results {
		shown[card.Ref] = struct{}{}
	}
	if len(calls[0].Refs) != len(shown) {
		t.Fatalf("the call recorded %d refs for %d shown cards: %v",
			len(calls[0].Refs), len(shown), calls[0].Refs)
	}
	for _, ref := range calls[0].Refs {
		if _, ok := shown[ref.Ref]; !ok {
			t.Fatalf("the call recorded %q, which the model never saw", ref.Ref)
		}
		if ref.InternalID == "" || ref.Kind == "" {
			t.Fatalf("the call record for %s carries no identity or kind: %+v", ref.Ref, ref)
		}
	}
}

// B3: nothing invisible may stay citable.
func TestNoInvisibleWholeCardRefSurvives(t *testing.T) {
	built, evidence := governedTool(t, governedFixture(), true)
	output := runGovernedSearch(t, built)

	shown := map[string]struct{}{}
	for _, card := range output.Results {
		shown[card.Ref] = struct{}{}
	}
	for _, entry := range evidence.Entries() {
		if entry.Kind != serviceagent.MemoryEvidenceEpisodic {
			continue
		}
		if _, visible := shown[entry.Ref]; !visible {
			t.Fatalf("%s is registered and citable but was never shown to the model", entry.Ref)
		}
	}
}

// B4, B5, B6, B7: what the production citation check accepts and refuses.
func TestFinalCitationCheckAcceptsSlotsAndRefusesEverythingElse(t *testing.T) {
	built, evidence := governedTool(t, governedFixture(), true)
	output := runGovernedSearch(t, built)
	if len(output.Results) == 0 {
		t.Fatal("nothing was shown")
	}
	slot := output.Results[0].Ref // "E1.s"

	if _, err := evidence.Lookup(slot); err != nil {
		t.Fatalf("a shown slot was refused: %v", err)
	}
	// The old whole-card shape is no longer a reference at all.
	if _, err := evidence.Lookup("E1"); err == nil {
		t.Fatal("a whole-card reference resolved in the governed mode")
	}
	// A label this turn never issued.
	if _, err := evidence.Lookup("E9.d1"); err == nil {
		t.Fatal("a forged slot reference resolved")
	}
	// Changing the slot letter must not silently resolve to the same record.
	decision, ok := firstRefWithSuffix(output, ".d1")
	if !ok {
		t.Fatal("no decision slot was shown")
	}
	swapped := strings.Replace(decision, ".d1", ".o1", 1)
	entry, err := evidence.Lookup(swapped)
	if err == nil {
		original, _ := evidence.Lookup(decision)
		if entry.InternalID == original.InternalID {
			t.Fatalf("%s resolved to the same record as %s", swapped, decision)
		}
	} else if !errors.Is(err, serviceagent.ErrUnknownMemoryRef) &&
		!errors.Is(err, serviceagent.ErrMemoryKindMismatch) {
		t.Fatalf("%s failed with an unexpected error: %v", swapped, err)
	}
}

// B8: across two cards every label is unique and maps to its own assertion.
func TestLabelsAreUniqueAndDoNotCross(t *testing.T) {
	built, evidence := governedTool(t, governedFixture(), true)
	output := runGovernedSearch(t, built)

	seenLabels := map[string]struct{}{}
	seenIdentities := map[string]string{}
	for _, card := range output.Results {
		if _, dup := seenLabels[card.Ref]; dup {
			t.Fatalf("%s was issued twice", card.Ref)
		}
		seenLabels[card.Ref] = struct{}{}
		entry, err := evidence.Lookup(card.Ref)
		if err != nil {
			t.Fatalf("%s: %v", card.Ref, err)
		}
		if owner, dup := seenIdentities[entry.InternalID]; dup {
			t.Fatalf("%s and %s both map to %s", owner, card.Ref, entry.InternalID)
		}
		seenIdentities[entry.InternalID] = card.Ref
	}
	if len(seenLabels) < 4 {
		t.Fatalf("only %d slots across two cards: %v", len(seenLabels), seenLabels)
	}
}

func firstRefWithSuffix(output MemorySearchOutput, suffix string) (string, bool) {
	for _, card := range output.Results {
		if strings.HasSuffix(card.Ref, suffix) {
			return card.Ref, true
		}
	}
	return "", false
}
