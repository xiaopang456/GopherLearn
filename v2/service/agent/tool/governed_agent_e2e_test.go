package agenttool

import (
	"encoding/json"
	"strings"
	"testing"

	"GopherAI/model"
	serviceagent "GopherAI/service/agent"
)

// The end-to-end loop, with a Fake Agent.
//
// v1.2's version of this file re-derived the label-to-identity mapping by
// calling ExpandCandidates a second time, which is what let the read chain ship
// broken: the production registry never held the dotted labels at all, and the
// test could not see that because it was checking a mapping it had built
// itself. Everything below resolves citations through the run's own
// RunContext.MemoryEvidence - the same object the server would check the answer
// against - and nothing here calls the governor.
//
//	question -> Agent calls memory_search -> real tool implementation retrieves
//	-> state repository loads governance -> slots expand -> the model receives
//	[E#.d#] -> the answer cites one -> the run registry maps it back to the
//	stable assertion id, its kind and its state.

// fakeAgent is a model that reads the observation and cites a slot from it.
type fakeAgent struct {
	// pick chooses which label the answer will cite, given what it was shown.
	pick func(output MemorySearchOutput) string
	// sentence renders the answer around the chosen label.
	sentence func(label string) string
	saw      MemorySearchOutput
	answer   string
}

func (a *fakeAgent) turn(t *testing.T, store *governedStore) (MemorySearchOutput, string, *serviceagent.MemoryEvidenceRegistry) {
	t.Helper()
	built, evidence := governedTool(t, store, true)
	output := runGovernedSearch(t, built)
	a.saw = output
	label := a.pick(output)
	a.answer = a.sentence(label)
	return output, a.answer, evidence
}

// The whole loop: the model cites a current decision, and the server resolves
// the label through the run registry to a stable identity carrying its state.
func TestFakeAgentCitesASlotAndTheServerResolvesIt(t *testing.T) {
	store := governedFixture()
	agent := &fakeAgent{
		pick: func(output MemorySearchOutput) string {
			for _, card := range output.Results {
				if card.AssertionState == string(model.AssertionCurrent) {
					return card.Ref
				}
			}
			return ""
		},
		sentence: func(label string) string {
			return "你现在用的是固定头部方案 [" + label + "]。"
		},
	}
	output, answer, evidence := agent.turn(t, store)

	if !strings.Contains(answer, "[E2.d1]") {
		t.Fatalf("the model did not cite the current decision: %q", answer)
	}
	if len(output.Results) == 0 {
		t.Fatal("the model was shown nothing")
	}

	// The deterministic server-side check the production path runs.
	outcome := serviceagent.CheckMemoryCitations(evidence, answer)
	if len(outcome.Cited) != 1 || outcome.Cited[0] != "E2.d1" {
		t.Fatalf("the slot citation was not read out of the answer: %+v", outcome.Cited)
	}
	if outcome.Failed() {
		t.Fatalf("a legitimate slot citation was refused: %+v", outcome.Reasons())
	}

	entry, err := evidence.Lookup("E2.d1")
	if err != nil {
		t.Fatalf("the cited label did not resolve: %v", err)
	}
	if entry.InternalID != governedID("ep-new|d1") {
		t.Fatalf("the label resolved to %q, want the stable identity of ep-new's decision", entry.InternalID)
	}
	if entry.Kind != serviceagent.MemoryEvidenceEpisodic {
		t.Fatalf("the label resolved as %s", entry.Kind)
	}
	if entry.Status != string(model.AssertionCurrent) {
		t.Fatalf("the resolved assertion reads as %q", entry.Status)
	}
}

// The same loop, with the model presenting a replaced decision as the present.
//
// The reference itself is legitimate - "which approach did I abandon" needs it
// - so the deterministic check passes it, and what the server has is the state
// on the record it resolved to. That is the fact a present-tense judgement is
// made from; whether this particular sentence claims the present is the
// natural-language half, and it is not decided here.
func TestFakeAgentPresentingReplacedContentIsResolvableAsReplaced(t *testing.T) {
	store := governedFixture()
	agent := &fakeAgent{
		pick: func(output MemorySearchOutput) string {
			for _, card := range output.Results {
				if card.AssertionState == string(model.AssertionReplaced) {
					return card.Ref
				}
			}
			return ""
		},
		sentence: func(label string) string {
			return "你现在用的是分隔符切分 [" + label + "]。"
		},
	}
	_, answer, evidence := agent.turn(t, store)

	if !strings.Contains(answer, "[E1.d1]") {
		t.Fatalf("the model did not cite the replaced decision: %q", answer)
	}
	outcome := serviceagent.CheckMemoryCitations(evidence, answer)
	if outcome.Failed() {
		t.Fatalf("a legitimate reference to abandoned work was refused: %+v", outcome.Reasons())
	}
	entry, err := evidence.Lookup("E1.d1")
	if err != nil {
		t.Fatalf("the cited label did not resolve: %v", err)
	}
	if entry.Status != string(model.AssertionReplaced) {
		t.Fatalf("the replaced decision resolves as %q, so a present-tense claim on it could not be caught", entry.Status)
	}
	if entry.InternalID != governedID("ep-old|d1") {
		t.Fatalf("the label resolved to %q", entry.InternalID)
	}
}

// A label the turn never issued is refused, and a whole-card reference is
// refused too: a governed turn shows no whole cards, so [E1] names nothing.
func TestFakeAgentInventingALabelIsRefused(t *testing.T) {
	built, evidence := governedTool(t, governedFixture(), true)
	runGovernedSearch(t, built)

	forged := serviceagent.CheckMemoryCitations(evidence, "你决定过某件事 [E9.d1]。")
	if len(forged.Invalid) != 1 || forged.Invalid[0] != "E9.d1" {
		t.Fatalf("a forged label was accepted: %+v", forged)
	}
	wholeCard := serviceagent.CheckMemoryCitations(evidence, "你决定过某件事 [E1]。")
	if len(wholeCard.Invalid) != 1 {
		t.Fatalf("a whole-card reference was accepted in a governed turn: %+v", wholeCard)
	}
}

// The control: with governance off, the same question produces the old
// whole-card observation, so nothing was changed underneath the existing modes.
func TestUngovernedControlKeepsTheOldObservation(t *testing.T) {
	store := governedFixture()
	built, evidence := governedTool(t, store, false)
	output := runGovernedSearch(t, built)

	if len(output.Results) != 2 {
		t.Fatalf("the ungoverned mode returned %d cards, want the two Episodes", len(output.Results))
	}
	for _, card := range output.Results {
		if strings.Contains(card.Ref, ".") {
			t.Fatalf("a slot reference leaked into the ungoverned mode: %q", card.Ref)
		}
		if _, err := evidence.Lookup(card.Ref); err != nil {
			t.Fatalf("the ungoverned card %s stopped resolving: %v", card.Ref, err)
		}
	}
	raw, _ := json.Marshal(output)
	if strings.Contains(string(raw), "assertion_state") {
		t.Fatal("governance fields leaked into the ungoverned observation")
	}
}
