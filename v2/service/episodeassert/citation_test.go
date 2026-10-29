package episodeassert

import (
	"strings"
	"testing"

	"GopherAI/model"
)

// presentIf is the deterministic claim classifier the tests use. Production may
// supply something cleverer; the citation rules must not differ because of it.
func presentIf(markers ...string) ClaimClassifier {
	return func(sentence string) ClaimKind {
		for _, marker := range markers {
			if strings.Contains(sentence, marker) {
				return ClaimPresent
			}
		}
		return ClaimHistory
	}
}

// registryWith issues one card's slots, applying the given states.
//
// States are keyed by slot rather than by identity, because a test is about
// "the first decision" and the identity is deliberately opaque.
func registryWith(t *testing.T, states map[string]model.AssertionState) *Registry {
	t.Helper()
	source := episode("ep-a", 7, 20, []string{"用分隔符切分", "改回固定头部"}, []string{"Body 未做"}, []string{"租约要续"})
	slotOf := map[string]string{
		assertionID("ep-a", model.AssertionDecision, 1):   "d1",
		assertionID("ep-a", model.AssertionDecision, 2):   "d2",
		assertionID("ep-a", model.AssertionOpenLoop, 1):   "o1",
		assertionID("ep-a", model.AssertionCorrection, 1): "c1",
	}
	items := model.AssertionsOf(source)
	for index := range items {
		if state, ok := states[slotOf[items[index].ID]]; ok {
			items[index].State = state
		}
	}
	registry := NewRegistry()
	if _, err := registry.Issue("E12", items); err != nil {
		t.Fatal(err)
	}
	return registry
}

// The four slot shapes must all be issuable and addressable.
func TestRegistryIssuesOneLabelPerSlot(t *testing.T) {
	registry := registryWith(t, nil)
	for _, label := range []string{"E12.s", "E12.d1", "E12.d2", "E12.o1", "E12.c1"} {
		if _, ok := registry.Lookup(label); !ok {
			t.Errorf("%s was not issued", label)
		}
	}
	if _, ok := registry.Lookup("E12.d3"); ok {
		t.Error("a slot that does not exist was issued")
	}
}

// A synopsis cannot establish that the learner decided or finished anything.
func TestSynopsisCannotAssertADecision(t *testing.T) {
	registry := registryWith(t, map[string]model.AssertionState{"s": model.AssertionCurrent})
	result := ValidateAnswer("你现在用的是固定头部方案 [E12.s]。", registry,
		presentIf("现在"), nil, true)
	if len(result.SynopsisAsAssertion) != 1 {
		t.Fatalf("the synopsis claim was accepted: %+v", result)
	}
	if !strings.Contains(result.Answer, NeutralisedSentence) {
		t.Fatal("the unsupported sentence was not neutralised")
	}
}

// A replaced assertion can carry history and must not carry the present.
func TestReplacedAssertionSupportsHistoryButNotPresent(t *testing.T) {
	registry := registryWith(t, map[string]model.AssertionState{
		"d1": model.AssertionReplaced, "d2": model.AssertionCurrent})

	history := ValidateAnswer("你当时尝试过用分隔符切分 [E12.d1]。", registry, presentIf("现在"), nil, true)
	if len(history.ReplacedAsPresent) != 0 || strings.Contains(history.Answer, NeutralisedSentence) {
		t.Fatalf("a legitimate historical use of a replaced assertion was refused: %+v", history)
	}

	present := ValidateAnswer("你现在用的是分隔符切分 [E12.d1]。", registry, presentIf("现在"), nil, true)
	if len(present.ReplacedAsPresent) != 1 {
		t.Fatalf("a replaced assertion was allowed to state the present: %+v", present)
	}
}

// Legacy data may say "this was recorded" and may not say "this still holds".
func TestUnknownAssertionCannotStateThePresent(t *testing.T) {
	registry := registryWith(t, nil) // every slot defaults to unknown
	history := ValidateAnswer("你当时记录过 Body 编解码还没做 [E12.o1]。", registry, presentIf("现在", "仍然"), nil, true)
	if strings.Contains(history.Answer, NeutralisedSentence) {
		t.Fatalf("legacy data was refused as history: %+v", history)
	}
	present := ValidateAnswer("Body 编解码现在仍然没做 [E12.o1]。", registry, presentIf("现在", "仍然"), nil, true)
	if len(present.UnknownAsPresent) != 1 {
		t.Fatalf("unknown legacy data was allowed to state the present: %+v", present)
	}
}

// A current assertion is the only thing that establishes the present on its own.
func TestCurrentAssertionEstablishesThePresent(t *testing.T) {
	registry := registryWith(t, map[string]model.AssertionState{"d2": model.AssertionCurrent})
	result := ValidateAnswer("你现在用的是固定头部方案 [E12.d2]。", registry, presentIf("现在"), nil, true)
	if len(result.ReplacedAsPresent)+len(result.UnknownAsPresent)+len(result.SynopsisAsAssertion) != 0 {
		t.Fatalf("a current decision was refused: %+v", result)
	}
	if strings.Contains(result.Answer, NeutralisedSentence) {
		t.Fatal("a supported sentence was neutralised")
	}
}

// One bad sentence must not destroy the good ones. This is the difference
// between a usable answer and a blank one.
func TestOnlyTheFailingSentenceIsNeutralised(t *testing.T) {
	registry := registryWith(t, map[string]model.AssertionState{
		"d1": model.AssertionReplaced, "d2": model.AssertionCurrent})
	answer := "你当时试过分隔符 [E12.d1]。你现在用的是分隔符 [E12.d1]。你现在用的是固定头部 [E12.d2]。"
	result := ValidateAnswer(answer, registry, presentIf("现在"), nil, true)

	if strings.Count(result.Answer, NeutralisedSentence) != 1 {
		t.Fatalf("expected exactly one neutralised sentence, got: %q", result.Answer)
	}
	if !strings.Contains(result.Answer, "你当时试过分隔符 [E12.d1]。") {
		t.Fatal("the valid historical sentence was removed")
	}
	if !strings.Contains(result.Answer, "你现在用的是固定头部 [E12.d2]。") {
		t.Fatal("the valid present-tense sentence was removed")
	}
}

// Every rejection shape the design promises.
func TestInvalidVersionMismatchedAndSlotlessReferences(t *testing.T) {
	registry := registryWith(t, map[string]model.AssertionState{"d2": model.AssertionCurrent})

	forged := ValidateAnswer("你决定过某件事 [E99.d1]。", registry, presentIf("现在"), nil, true)
	if len(forged.InvalidRefs) != 1 || forged.InvalidRefs[0] != "E99.d1" {
		t.Fatalf("a forged reference was accepted: %+v", forged)
	}

	slotless := ValidateAnswer("你决定过某件事 [E12]。", registry, presentIf("现在"), nil, true)
	if len(slotless.KindMismatch) != 1 {
		t.Fatalf("a slotless reference was accepted: %+v", slotless)
	}

	moved := ValidateAnswer("你现在用的是固定头部 [E12.d2]。", registry, presentIf("现在"),
		CurrentEpisodeVersions{"ep-a": 9}, true)
	if len(moved.VersionMismatch) != 1 {
		t.Fatalf("a citation of a moved card was accepted: %+v", moved)
	}
}

// Citing anything in a turn that retrieved nothing is the shape of an invented
// memory, and it is reported separately from a merely wrong citation.
func TestCitedAfterEmptyIsReportedSeparately(t *testing.T) {
	registry := registryWith(t, map[string]model.AssertionState{"d2": model.AssertionCurrent})
	empty := ValidateAnswer("你当时决定过 [E12.d2]。", registry, presentIf("现在"), nil, false)
	if !empty.CitedAfterEmpty {
		t.Fatal("a citation after an empty search was not flagged")
	}
	nonEmpty := ValidateAnswer("你当时决定过 [E12.d2]。", registry, presentIf("现在"), nil, true)
	if nonEmpty.CitedAfterEmpty {
		t.Fatal("a citation after a productive search was flagged")
	}
	noCitation := ValidateAnswer("没有找到相关记录。", registry, presentIf("现在"), nil, false)
	if noCitation.CitedAfterEmpty {
		t.Fatal("an honest no-record answer was flagged")
	}
}

// The judge boundary: strip exactly what was issued, and leave forgeries where
// the deterministic check can still see them. BUG-AGENT-154.
func TestStripRemovesOnlyIssuedLabels(t *testing.T) {
	registry := registryWith(t, nil)
	answer := "真实引用 [E12.d1]，伪造引用 [E99.d1]，文档引用 [1]，普通括号 [备注]。"
	stripped := StripIssuedRefs(answer, registry)

	if strings.Contains(stripped, "[E12.d1]") {
		t.Fatal("an issued label survived stripping; the judge can still tell the arms apart")
	}
	for _, keep := range []string{"[E99.d1]", "[1]", "[备注]"} {
		if !strings.Contains(stripped, keep) {
			t.Fatalf("%s was removed; deleting a forged reference hides the failure the checker must catch", keep)
		}
	}
}

// A malformed card number must be refused at issue time.
func TestMalformedCardIsRefused(t *testing.T) {
	registry := NewRegistry()
	if _, err := registry.Issue("12", nil); err == nil {
		t.Fatal("a card without the E prefix was accepted")
	}
	if _, err := registry.Issue("E12345", nil); err == nil {
		t.Fatal("an over-long card number was accepted")
	}
}

// Without a classifier the state-dependent checks must not silently pass.
//
// A report showing zero replaced-as-present violations because nothing was
// examined is worse than one showing none at all: it reads as evidence.
func TestPresentTenseChecksAreReportedAsSkippedWhenUnavailable(t *testing.T) {
	registry := registryWith(t, map[string]model.AssertionState{"d1": model.AssertionReplaced})
	result := ValidateAnswer("你现在用的是分隔符切分 [E12.d1]。", registry, nil, nil, true)

	if result.PresentTenseCheckRan {
		t.Fatal("the present-tense check claimed to have run without a classifier")
	}
	if len(result.ReplacedAsPresent) != 0 {
		t.Fatal("a check that cannot run must not produce findings")
	}
	// The deterministic checks still run: they need no language judgement.
	forged := ValidateAnswer("某个说法 [E99.d1]。", registry, nil, nil, true)
	if len(forged.InvalidRefs) != 1 {
		t.Fatal("the deterministic reference check must run regardless of the classifier")
	}
	// With a classifier, the state checks run and are reported as having run.
	withClassifier := ValidateAnswer("你现在用的是分隔符切分 [E12.d1]。", registry,
		presentIf("现在"), nil, true)
	if !withClassifier.PresentTenseCheckRan || len(withClassifier.ReplacedAsPresent) != 1 {
		t.Fatalf("the state check did not run with a classifier: %+v", withClassifier)
	}
}

// Production must not ship a claim classifier. This is asserted rather than
// promised, because the tempting fix for the test above is a keyword table.
func TestNoDefaultClassifierExists(t *testing.T) {
	result := ValidateAnswer("你现在用的是分隔符切分 [E12.d1]。",
		registryWith(t, map[string]model.AssertionState{"d1": model.AssertionReplaced}), nil, nil, true)
	for _, sentence := range result.Sentences {
		if sentence.Claim != ClaimUndetermined {
			t.Fatalf("a sentence was classified as %q with no classifier supplied", sentence.Claim)
		}
	}
}

// The reportable verdict must distinguish "no violations" from "not examined".
//
// A Pilot artefact printing replaced_used_as_present: 0 with no classifier
// would be reporting a measurement nobody took, and a reader cannot tell that
// from a clean result. So the verdict is a string, not an inferred zero.
func TestPresentTenseVerdictSeparatesCleanFromNotEvaluated(t *testing.T) {
	registry := registryWith(t, map[string]model.AssertionState{"d1": model.AssertionReplaced})

	skipped := ValidateAnswer("你现在用的是分隔符切分 [E12.d1]。", registry, nil, nil, true)
	if skipped.PresentTenseVerdict != PresentTenseNotEvaluated {
		t.Fatalf("verdict without a classifier = %q", skipped.PresentTenseVerdict)
	}
	violated := ValidateAnswer("你现在用的是分隔符切分 [E12.d1]。", registry, presentIf("现在"), nil, true)
	if violated.PresentTenseVerdict != PresentTenseViolated {
		t.Fatalf("verdict on a violation = %q", violated.PresentTenseVerdict)
	}
	clean := ValidateAnswer("你当时试过分隔符切分 [E12.d1]。", registry, presentIf("现在"), nil, true)
	if clean.PresentTenseVerdict != PresentTenseClean {
		t.Fatalf("verdict on a clean answer = %q", clean.PresentTenseVerdict)
	}
}
