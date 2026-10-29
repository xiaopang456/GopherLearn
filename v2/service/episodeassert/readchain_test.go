package episodeassert

import (
	"context"
	"testing"

	"GopherAI/common/memoryrecall"
	"GopherAI/model"
)

// The read chain, and the mode isolation around it.

func governedFixture(t *testing.T) (*Governor, *memoryStore, []model.EpisodeMemory) {
	t.Helper()
	store := newMemoryStore()
	older := episode("ep-a", 7, 30, []string{"用分隔符切分"}, []string{"半包还没写"}, nil)
	newer := episode("ep-b", 7, 10, []string{"改回固定头部"}, nil, []string{"分隔符会和负载冲突"})
	legacy := episode("ep-c", 7, 50, []string{"令牌桶限流"}, nil, nil)
	for _, card := range []model.EpisodeMemory{older, newer, legacy} {
		store.episodes[card.ID] = card
	}
	governor := governorWith(t, store)
	if err := governor.MarkReplaced(context.Background(), ReplaceRequest{
		UserID:          7,
		SourceEpisodeID: "ep-a", SourceAssertionID: assertionID("ep-a", model.AssertionDecision, 1),
		TargetEpisodeID: "ep-b", TargetAssertionID: assertionID("ep-b", model.AssertionDecision, 1),
	}); err != nil {
		t.Fatal(err)
	}
	if err := governor.MarkCurrent(context.Background(), 7, "ep-b",
		assertionID("ep-b", model.AssertionDecision, 1), 0); err != nil {
		t.Fatal(err)
	}
	return governor, store, []model.EpisodeMemory{older, newer, legacy}
}

func slotOf(result ReadChainResult, label string) (GovernedSlot, bool) {
	for _, card := range result.Cards {
		for _, slot := range card.Slots {
			if slot.Label == label {
				return slot, true
			}
		}
	}
	return GovernedSlot{}, false
}

// The whole chain, end to end: every state present, every slot labelled, the
// replacement named, and times carried from the assertions.
func TestExpandEpisodesProducesGovernedSlots(t *testing.T) {
	governor, _, episodes := governedFixture(t)
	result, err := governor.ExpandEpisodes(context.Background(), 7, episodes)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Cards) != 3 {
		t.Fatalf("expanded %d cards, want 3", len(result.Cards))
	}

	replaced, ok := slotOf(result, "E1.d1")
	if !ok || replaced.State != model.AssertionReplaced {
		t.Fatalf("E1.d1 = %+v", replaced)
	}
	if replaced.Text == "" {
		t.Fatal("a replaced assertion must keep its text; history needs it")
	}
	if replaced.ReplacedByLabel != "E2.d1" {
		t.Fatalf("the replacement was reported as %q, want E2.d1", replaced.ReplacedByLabel)
	}
	if replaced.OccurredAt == nil {
		t.Fatal("a datable assertion came back without its time")
	}

	current, _ := slotOf(result, "E2.d1")
	if current.State != model.AssertionCurrent {
		t.Fatalf("E2.d1 = %s", current.State)
	}
	// The sibling open loop on the replaced card keeps its own state.
	sibling, _ := slotOf(result, "E1.o1")
	if sibling.State != model.AssertionUnknown {
		t.Fatalf("E1.o1 = %s", sibling.State)
	}
	// Legacy data with no governance is unknown, not current.
	legacy, _ := slotOf(result, "E3.d1")
	if legacy.State != model.AssertionUnknown {
		t.Fatalf("E3.d1 = %s", legacy.State)
	}

	counts := result.StateCounts()
	if counts[model.AssertionReplaced] != 1 || counts[model.AssertionCurrent] != 1 {
		t.Fatalf("state counts = %v", counts)
	}
}

// Replaced assertions are neither filtered nor down-ranked. "Which approach did
// I abandon" needs them, and a card's order is the retrieval's answer.
func TestReplacedSlotsAreNeitherFilteredNorReordered(t *testing.T) {
	governor, _, episodes := governedFixture(t)
	result, err := governor.ExpandEpisodes(context.Background(), 7, episodes)
	if err != nil {
		t.Fatal(err)
	}
	if result.Cards[0].Card != "E1" || result.Cards[1].Card != "E2" {
		t.Fatal("card numbering did not follow the search order")
	}
	if _, ok := slotOf(result, "E1.d1"); !ok {
		t.Fatal("the replaced assertion was filtered out of the tool result")
	}
}

// The Registry maps a per-turn label to a stable identity, and the identity
// never appears in what the model sees.
func TestRegistryKeepsTheStableIdentityServerSide(t *testing.T) {
	governor, _, episodes := governedFixture(t)
	result, err := governor.ExpandEpisodes(context.Background(), 7, episodes)
	if err != nil {
		t.Fatal(err)
	}
	reference, ok := result.Registry.Lookup("E1.d1")
	if !ok {
		t.Fatal("E1.d1 was not issued")
	}
	if reference.AssertionID != assertionID("ep-a", model.AssertionDecision, 1) {
		t.Fatalf("the label maps to %q", reference.AssertionID)
	}
	// The rendered slots carry the label and the text, never the identity.
	for _, card := range result.Cards {
		for _, slot := range card.Slots {
			if slot.Label == "" {
				t.Fatal("a slot was rendered without a label")
			}
			if slot.Text == reference.AssertionID {
				t.Fatal("a stable identity reached the rendered result")
			}
		}
	}
}

// Two turns number cards independently; the identity behind them does not move.
func TestLabelsArePerTurnAndIdentitiesAreNot(t *testing.T) {
	governor, _, episodes := governedFixture(t)
	first, err := governor.ExpandEpisodes(context.Background(), 7, episodes)
	if err != nil {
		t.Fatal(err)
	}
	// A later turn retrieves the same cards in the other order.
	reordered := []model.EpisodeMemory{episodes[1], episodes[0], episodes[2]}
	second, err := governor.ExpandEpisodes(context.Background(), 7, reordered)
	if err != nil {
		t.Fatal(err)
	}
	firstRef, _ := first.Registry.Lookup("E1.d1")
	secondRef, _ := second.Registry.Lookup("E2.d1")
	if firstRef.AssertionID != secondRef.AssertionID {
		t.Fatalf("the same fact carried %s then %s", firstRef.AssertionID, secondRef.AssertionID)
	}
	if firstRef.Label == secondRef.Label {
		t.Fatal("labels are supposed to be per-turn")
	}
}

// A state-store failure must fail the call. Falling back to ungoverned cards
// would present every replaced decision as though nothing were known against
// it, which is the distinction this feature exists to make.
func TestReadChainDoesNotFallBackToUngovernedCards(t *testing.T) {
	governor, store, episodes := governedFixture(t)
	store.failStates = true
	if _, err := governor.ExpandEpisodes(context.Background(), 7, episodes); err == nil {
		t.Fatal("a state store failure produced a successful, ungoverned result")
	}
}

// Mode isolation: only the v1.1 identity governs assertion time, and every
// other property of the mode it extends is inherited unchanged.
func TestOnlyTheNewIdentityGovernsAssertionTime(t *testing.T) {
	governed := memoryrecall.ModeEpisodeAssertionGovernedV11
	for _, mode := range []memoryrecall.ReadMode{
		memoryrecall.ModeLegacyV2, memoryrecall.ModeEpisodeV3, memoryrecall.ModeEpisodeV3Lexical,
		memoryrecall.ModeEpisodeV3Semantic, memoryrecall.ModeModelPlannedV1,
		memoryrecall.ModeAgentMemoryToolV1, memoryrecall.ModeAgentMemoryToolV2,
		memoryrecall.ModeAgentMemoryToolV3, memoryrecall.ModeAgentMemoryToolV4,
		memoryrecall.ModeAgentMemoryUserConfirmedV1,
	} {
		if mode.GovernsAssertionTime() {
			t.Errorf("%s governs assertion time and must not", mode)
		}
	}
	if !governed.GovernsAssertionTime() {
		t.Fatal("the v1.1 identity does not govern assertion time")
	}
	if !governed.Valid() {
		t.Fatal("the v1.1 identity is not a valid mode")
	}
	// It inherits the user-confirmed mode's properties, so memory_search stays
	// Episode-only and the topic confirmation chain still applies.
	if !governed.RestrictsMemorySearchToEpisodic() {
		t.Fatal("memory_search must remain Episode-only in the new mode")
	}
	if !governed.UsesUserConfirmedTopicContext() {
		t.Fatal("the topic user-confirmation chain must still apply")
	}
	if !governed.UsesMemorySearchTool() {
		t.Fatal("the new mode must still register memory_search")
	}
	if governed.AutoInjectsTopicAndEpisodeMemory() {
		t.Fatal("the new mode must not auto-inject memory")
	}
	if memoryrecall.DefaultReadMode() != memoryrecall.ModeLegacyV2 {
		t.Fatal("the production default must remain legacy_v2")
	}
}
