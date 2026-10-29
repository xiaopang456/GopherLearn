package episodeassert

import (
	"context"
	"errors"
	"strings"
	"testing"

	"GopherAI/model"
)

// The full replacement matrix, every cell asserted.
//
// v1 parsed the target's kind and then ignored it, so an open loop could
// "replace" a synopsis. A relation nobody can act on is worse than none: it
// renders, it looks authoritative, and downstream cannot tell it from a real
// one.
func TestReplacementMatrixCoversEveryPair(t *testing.T) {
	kinds := []model.AssertionKind{model.AssertionSynopsis, model.AssertionDecision,
		model.AssertionOpenLoop, model.AssertionCorrection}
	allowed := map[string]bool{
		"decision>decision":     true,
		"decision>correction":   true,
		"correction>correction": true,
	}
	for _, source := range kinds {
		for _, target := range kinds {
			key := string(source) + ">" + string(target)
			if got := ReplacementAllowed(source, target); got != allowed[key] {
				t.Errorf("%s: allowed=%v, want %v", key, got, allowed[key])
			}
		}
	}
}

// The two refusals worth stating in their own right.
func TestSynopsisAndOpenLoopCannotParticipateInReplacement(t *testing.T) {
	store := newMemoryStore()
	older := episode("ep-a", 7, 30, []string{"旧决定"}, []string{"未完成一"}, nil)
	newer := episode("ep-b", 7, 10, []string{"新决定"}, []string{"未完成二"}, []string{"纠正"})
	store.episodes[older.ID], store.episodes[newer.ID] = older, newer
	governor := governorWith(t, store)

	synopsisID := func(episodeID string) string {
		return model.LegacyAssertionID(episodeID, model.AssertionSynopsis, nil, "会话概述 "+episodeID)
	}
	for name, request := range map[string]ReplaceRequest{
		"synopsis as source": {UserID: 7,
			SourceEpisodeID: "ep-a", SourceAssertionID: synopsisID("ep-a"),
			TargetEpisodeID: "ep-b", TargetAssertionID: assertionID("ep-b", model.AssertionDecision, 1)},
		"synopsis as target": {UserID: 7,
			SourceEpisodeID: "ep-a", SourceAssertionID: assertionID("ep-a", model.AssertionDecision, 1),
			TargetEpisodeID: "ep-b", TargetAssertionID: synopsisID("ep-b")},
		"open loop as source": {UserID: 7,
			SourceEpisodeID: "ep-a", SourceAssertionID: assertionID("ep-a", model.AssertionOpenLoop, 1),
			TargetEpisodeID: "ep-b", TargetAssertionID: assertionID("ep-b", model.AssertionDecision, 1)},
		"open loop as target": {UserID: 7,
			SourceEpisodeID: "ep-a", SourceAssertionID: assertionID("ep-a", model.AssertionDecision, 1),
			TargetEpisodeID: "ep-b", TargetAssertionID: assertionID("ep-b", model.AssertionOpenLoop, 1)},
		"correction replaced by decision": {UserID: 7,
			SourceEpisodeID: "ep-b", SourceAssertionID: assertionID("ep-b", model.AssertionCorrection, 1),
			TargetEpisodeID: "ep-b", TargetAssertionID: assertionID("ep-b", model.AssertionDecision, 1)},
	} {
		err := governor.MarkReplaced(context.Background(), request)
		if err == nil {
			t.Errorf("%s: the relation was accepted", name)
			continue
		}
		if !errors.Is(err, ErrRelationRejected) {
			t.Errorf("%s: rejected with the wrong class: %v", name, err)
		}
	}
	if store.saves != 0 {
		t.Fatalf("%d refused relations were written anyway", store.saves)
	}
}

// The three allowed shapes must actually work.
func TestAllowedReplacementShapesAreAccepted(t *testing.T) {
	for name, shape := range map[string]struct {
		sourceKind, targetKind model.AssertionKind
	}{
		"decision replaced by decision":     {model.AssertionDecision, model.AssertionDecision},
		"decision replaced by correction":   {model.AssertionDecision, model.AssertionCorrection},
		"correction replaced by correction": {model.AssertionCorrection, model.AssertionCorrection},
	} {
		t.Run(name, func(t *testing.T) {
			store := newMemoryStore()
			older := episode("ep-a", 7, 30, []string{"旧决定"}, nil, []string{"旧纠正"})
			newer := episode("ep-b", 7, 10, []string{"新决定"}, nil, []string{"新纠正"})
			store.episodes[older.ID], store.episodes[newer.ID] = older, newer
			governor := governorWith(t, store)

			if err := governor.MarkReplaced(context.Background(), ReplaceRequest{
				UserID:          7,
				SourceEpisodeID: "ep-a", SourceAssertionID: assertionID("ep-a", shape.sourceKind, 1),
				TargetEpisodeID: "ep-b", TargetAssertionID: assertionID("ep-b", shape.targetKind, 1),
			}); err != nil {
				t.Fatalf("an allowed shape was refused: %v", err)
			}
		})
	}
}

// Two decisions inside one session share the card's timestamp, so card-level
// ordering cannot separate them. Assertion times can, and must be what the rule
// compares.
func TestAssertionsInOneCardCarryTheirOwnTimes(t *testing.T) {
	store := newMemoryStore()
	card := episode("ep-a", 7, 20, []string{"先定的", "后定的"}, nil, nil)
	store.episodes[card.ID] = card
	governor := governorWith(t, store)

	// The two decisions are dated separately: the second was said later.
	first := messageIDFor("ep-a", model.AssertionDecision, 1)
	second := messageIDFor("ep-a", model.AssertionDecision, 2)
	store.messageTimes[first] = fixedNow.AddDate(0, 0, -20)
	store.messageTimes[second] = fixedNow.AddDate(0, 0, -19)

	items, err := governor.ResolveWithTimes(context.Background(), 7, card)
	if err != nil {
		t.Fatal(err)
	}
	times := map[string]bool{}
	for _, item := range items {
		if item.Kind == model.AssertionDecision {
			times[item.OccurredAt.String()] = true
			if !item.TimeKnown {
				t.Fatalf("%s has no resolvable time", item.ID)
			}
		}
	}
	if len(times) != 2 {
		t.Fatalf("two decisions in one card shared a timestamp: %v", times)
	}

	// Within-card ordering now works, which card-level times could never do.
	if err := governor.MarkReplaced(context.Background(), ReplaceRequest{
		UserID:          7,
		SourceEpisodeID: "ep-a", SourceAssertionID: assertionID("ep-a", model.AssertionDecision, 1),
		TargetEpisodeID: "ep-a", TargetAssertionID: assertionID("ep-a", model.AssertionDecision, 2),
	}); err != nil {
		t.Fatalf("a later decision in the same card could not replace an earlier one: %v", err)
	}
	// And the reverse is refused.
	err = governor.MarkReplaced(context.Background(), ReplaceRequest{
		UserID:          7,
		SourceEpisodeID: "ep-a", SourceAssertionID: assertionID("ep-a", model.AssertionDecision, 2),
		TargetEpisodeID: "ep-a", TargetAssertionID: assertionID("ep-a", model.AssertionDecision, 1),
	})
	if err == nil || !strings.Contains(err.Error(), "older") {
		t.Fatalf("an earlier decision was allowed to replace a later one: %v", err)
	}
}

// An assertion whose evidence cannot be dated must not be ordered at all.
func TestUndatableAssertionCannotBeOrdered(t *testing.T) {
	store := newMemoryStore()
	older := episode("ep-a", 7, 30, []string{"旧决定"}, nil, nil)
	newer := episode("ep-b", 7, 10, []string{"新决定"}, nil, nil)
	store.episodes[older.ID], store.episodes[newer.ID] = older, newer
	governor := governorWith(t, store)
	// The source's evidence is undatable, as a legacy record's would be.
	delete(store.messageTimes, messageIDFor("ep-a", model.AssertionDecision, 1))

	err := governor.MarkReplaced(context.Background(), ReplaceRequest{
		UserID:          7,
		SourceEpisodeID: "ep-a", SourceAssertionID: assertionID("ep-a", model.AssertionDecision, 1),
		TargetEpisodeID: "ep-b", TargetAssertionID: assertionID("ep-b", model.AssertionDecision, 1),
	})
	if err == nil || !strings.Contains(err.Error(), "resolvable time") {
		t.Fatalf("an undatable assertion was ordered anyway: %v", err)
	}
}

// A store failure must surface. Returning an empty state map would silently
// downgrade every replaced assertion to unknown, and unknown is permissive
// enough to be quoted as history.
func TestStateStoreFailureIsNotDowngradedToUnknown(t *testing.T) {
	store := newMemoryStore()
	card := episode("ep-a", 7, 20, []string{"决定"}, nil, nil)
	store.episodes[card.ID] = card
	governor := governorWith(t, store)
	store.failStates = true

	if _, err := governor.Resolve(context.Background(), 7, card); err == nil {
		t.Fatal("a state store failure was reported as a successful read")
	}
}
