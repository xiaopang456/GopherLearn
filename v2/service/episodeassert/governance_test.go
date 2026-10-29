package episodeassert

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"GopherAI/model"
)

// STABILIZE-058 Phase A1: the deterministic read chain.
//
// Everything here runs without a network, a model or a vector. What it pins is
// the part of the claim that is decidable by construction: that a replaced
// decision stays readable as history and cannot pass as the present, that
// governing one slot leaves its neighbours alone, that legacy data reads as
// unknown rather than current, and that a relation which would be dishonest is
// refused at write time rather than discovered at read time.

var fixedNow = time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)

// memoryStore is an in-memory Store with the same refusals the real one owes.
type memoryStore struct {
	episodes map[string]model.EpisodeMemory
	states   map[string]model.EpisodeAssertionState
	// messageTimes dates evidence. An id that is absent is undatable, which is
	// how a legacy assertion with no resolvable time is represented.
	messageTimes map[uint]time.Time
	saves        int
	failStates   bool
}

func newMemoryStore() *memoryStore {
	return &memoryStore{episodes: map[string]model.EpisodeMemory{},
		states: map[string]model.EpisodeAssertionState{}, messageTimes: map[uint]time.Time{}}
}

func (s *memoryStore) MessageTimes(_ context.Context, _ int64, ids []uint) (map[uint]time.Time, error) {
	out := map[uint]time.Time{}
	for _, id := range ids {
		if at, ok := s.messageTimes[id]; ok {
			out[id] = at
		}
	}
	return out, nil
}

func (s *memoryStore) LoadStates(_ context.Context, userID int64, ids []string) (map[string]model.EpisodeAssertionState, error) {
	if s.failStates {
		return nil, errors.New("assertion state store is unavailable")
	}
	out := map[string]model.EpisodeAssertionState{}
	for _, id := range ids {
		if state, ok := s.states[id]; ok && state.UserID == userID {
			out[id] = state
		}
	}
	return out, nil
}

func (s *memoryStore) LoadEpisode(_ context.Context, userID int64, episodeID string) (model.EpisodeMemory, error) {
	episode, ok := s.episodes[episodeID]
	if !ok {
		return model.EpisodeMemory{}, errors.New("episode not found")
	}
	if episode.UserID != userID {
		return model.EpisodeMemory{}, errors.New("episode belongs to another learner")
	}
	if episode.Status != model.EpisodeStatusActive {
		return model.EpisodeMemory{}, errors.New("episode is not active")
	}
	return episode, nil
}

func (s *memoryStore) SaveState(_ context.Context, state model.EpisodeAssertionState, expected uint64) error {
	existing, ok := s.states[state.AssertionID]
	current := uint64(0)
	if ok {
		current = existing.Version
	}
	if current != expected {
		return ErrVersionConflict
	}
	s.states[state.AssertionID] = state
	s.saves++
	return nil
}

// assertionID is the deterministic identity a fixture slot carries, so a test
// can name it without reaching into the document.
func assertionID(episodeID string, kind model.AssertionKind, ordinal int) string {
	return model.DeterministicAssertionID(fmt.Sprintf("%s|%s|%d", episodeID, kind, ordinal))
}

// messageIDFor gives every fixture slot its own evidence message, so assertions
// inside one card can be dated differently.
func messageIDFor(episodeID string, kind model.AssertionKind, ordinal int) uint {
	sum := 0
	for _, r := range episodeID + string(kind) {
		sum = sum*31 + int(r)
	}
	if sum < 0 {
		sum = -sum
	}
	return uint(sum%100000)*10 + uint(ordinal)
}

func episode(id string, userID int64, daysAgo int, decisions, openLoops, corrections []string) model.EpisodeMemory {
	toEvidence := func(kind model.AssertionKind, texts []string) []model.EpisodeEvidence {
		out := make([]model.EpisodeEvidence, 0, len(texts))
		for index, text := range texts {
			out = append(out, model.EpisodeEvidence{
				ID: assertionID(id, kind, index+1), Text: text,
				EvidenceMessageIDs: []uint{messageIDFor(id, kind, index+1)},
			})
		}
		return out
	}
	return model.EpisodeMemory{
		ID: id, UserID: userID, SessionID: "sess-" + id,
		Title: id, SearchText: id,
		Document: model.EpisodeDocument{
			Synopsis: "会话概述 " + id, Decisions: toEvidence(model.AssertionDecision, decisions),
			OpenLoops:       toEvidence(model.AssertionOpenLoop, openLoops),
			UserCorrections: toEvidence(model.AssertionCorrection, corrections),
			SourceHandles:   []string{"h:" + id},
		},
		Status: model.EpisodeStatusActive, IndexStatus: model.EpisodeIndexIndexed,
		Version: 1, LastMessageAt: fixedNow.AddDate(0, 0, -daysAgo),
	}
}

// dateFixtures gives every seeded slot a message time consistent with its
// card's age, so assertion-level ordering is real rather than assumed.
func dateFixtures(store *memoryStore) {
	for _, ep := range store.episodes {
		for _, group := range []struct {
			kind    model.AssertionKind
			entries []model.EpisodeEvidence
		}{
			{model.AssertionDecision, ep.Document.Decisions},
			{model.AssertionOpenLoop, ep.Document.OpenLoops},
			{model.AssertionCorrection, ep.Document.UserCorrections},
		} {
			for _, entry := range group.entries {
				for _, id := range entry.EvidenceMessageIDs {
					store.messageTimes[id] = ep.LastMessageAt
				}
			}
		}
	}
}

func governorWith(t *testing.T, store *memoryStore) *Governor {
	t.Helper()
	dateFixtures(store)
	governor, err := NewGovernor(store)
	if err != nil {
		t.Fatal(err)
	}
	governor.UseClock(func() time.Time { return fixedNow })
	return governor
}

// Legacy data has no rows, and must therefore read as unknown - not current.
func TestLegacyAssertionsReadAsUnknownNotCurrent(t *testing.T) {
	store := newMemoryStore()
	old := episode("ep-old", 7, 40, []string{"用固定头部"}, []string{"Body 未做"}, nil)
	store.episodes[old.ID] = old
	governor := governorWith(t, store)

	items, err := governor.Resolve(context.Background(), 7, old)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("resolved %d assertions, want synopsis + decision + open loop", len(items))
	}
	for _, item := range items {
		if item.State != model.AssertionUnknown {
			t.Fatalf("%s read as %s; legacy data must read as unknown", item.ID, item.State)
		}
	}
}

// Governing one slot must not touch its neighbours. This is the whole reason
// the state is per-assertion rather than per-Episode.
func TestReplacingOneDecisionLeavesTheOpenLoopAlone(t *testing.T) {
	store := newMemoryStore()
	older := episode("ep-a", 7, 30, []string{"用分隔符切分"}, []string{"半包还没写"}, nil)
	newer := episode("ep-b", 7, 10, []string{"改回固定头部并加魔数"}, nil, nil)
	store.episodes[older.ID], store.episodes[newer.ID] = older, newer
	governor := governorWith(t, store)

	if err := governor.MarkReplaced(context.Background(), ReplaceRequest{
		UserID: 7, SourceEpisodeID: "ep-a", SourceAssertionID: assertionID("ep-a", model.AssertionDecision, 1),
		TargetEpisodeID: "ep-b", TargetAssertionID: assertionID("ep-b", model.AssertionDecision, 1)}); err != nil {
		t.Fatal(err)
	}
	items, err := governor.Resolve(context.Background(), 7, older)
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]model.AssertionState{}
	for _, item := range items {
		states[item.ID] = item.State
	}
	if states[assertionID("ep-a", model.AssertionDecision, 1)] != model.AssertionReplaced {
		t.Fatalf("the replaced decision reads as %s", states[assertionID("ep-a", model.AssertionDecision, 1)])
	}
	if states[assertionID("ep-a", model.AssertionOpenLoop, 1)] != model.AssertionUnknown {
		t.Fatalf("the open loop was collaterally changed to %s", states[assertionID("ep-a", model.AssertionOpenLoop, 1)])
	}
	if states[model.LegacyAssertionID("ep-a", model.AssertionSynopsis, nil, "会话概述 ep-a")] != model.AssertionUnknown {
		t.Fatalf("the synopsis was collaterally changed to %s", states[model.LegacyAssertionID("ep-a", model.AssertionSynopsis, nil, "会话概述 ep-a")])
	}
}

// A replaced assertion must remain readable, and must name its replacement, so
// "which approach did I abandon" can be answered with evidence.
func TestReplacedAssertionStaysReadableAndNamesItsReplacement(t *testing.T) {
	store := newMemoryStore()
	older := episode("ep-a", 7, 30, []string{"用分隔符切分"}, nil, nil)
	newer := episode("ep-b", 7, 10, []string{"改回固定头部"}, nil, nil)
	store.episodes[older.ID], store.episodes[newer.ID] = older, newer
	governor := governorWith(t, store)
	if err := governor.MarkReplaced(context.Background(), ReplaceRequest{
		UserID: 7, SourceEpisodeID: "ep-a", SourceAssertionID: assertionID("ep-a", model.AssertionDecision, 1),
		TargetEpisodeID: "ep-b", TargetAssertionID: assertionID("ep-b", model.AssertionDecision, 1)}); err != nil {
		t.Fatal(err)
	}
	items, _ := governor.Resolve(context.Background(), 7, older)
	for _, item := range items {
		if item.ID != assertionID("ep-a", model.AssertionDecision, 1) {
			continue
		}
		if item.Text == "" {
			t.Fatal("a replaced assertion must keep its text; history needs it")
		}
		if !item.State.CanEstablishHistory() {
			t.Fatal("a replaced assertion must still be usable as history")
		}
		if item.State.CanEstablishPresent() {
			t.Fatal("a replaced assertion must not establish the present")
		}
		if item.ReplacedByAssertionID != assertionID("ep-b", model.AssertionDecision, 1) {
			t.Fatalf("replacement = %q", item.ReplacedByAssertionID)
		}
		return
	}
	t.Fatal("the replaced assertion disappeared from the read")
}

// Every integrity rule, each stated as its own refusal.
func TestRelationIntegrityRefusals(t *testing.T) {
	store := newMemoryStore()
	mine := episode("ep-a", 7, 30, []string{"旧决定"}, nil, nil)
	newer := episode("ep-b", 7, 10, []string{"新决定"}, nil, nil)
	earlier := episode("ep-early", 7, 60, []string{"更早的决定"}, nil, nil)
	theirs := episode("ep-other", 8, 10, []string{"别人的决定"}, nil, nil)
	archived := episode("ep-arch", 7, 20, []string{"归档的决定"}, nil, nil)
	archived.Status = model.EpisodeStatusArchived
	for _, item := range []model.EpisodeMemory{mine, newer, earlier, theirs, archived} {
		store.episodes[item.ID] = item
	}
	governor := governorWith(t, store)

	for name, request := range map[string]ReplaceRequest{
		"self loop": {UserID: 7, SourceEpisodeID: "ep-a", SourceAssertionID: assertionID("ep-a", model.AssertionDecision, 1),
			TargetEpisodeID: "ep-a", TargetAssertionID: assertionID("ep-a", model.AssertionDecision, 1)},
		"target missing": {UserID: 7, SourceEpisodeID: "ep-a", SourceAssertionID: assertionID("ep-a", model.AssertionDecision, 1),
			TargetEpisodeID: "ep-nope", TargetAssertionID: assertionID("ep-nope", model.AssertionDecision, 1)},
		"slot missing": {UserID: 7, SourceEpisodeID: "ep-a", SourceAssertionID: assertionID("ep-a", model.AssertionDecision, 1),
			TargetEpisodeID: "ep-b", TargetAssertionID: assertionID("ep-b", model.AssertionDecision, 9)},
		"cross user": {UserID: 7, SourceEpisodeID: "ep-a", SourceAssertionID: assertionID("ep-a", model.AssertionDecision, 1),
			TargetEpisodeID: "ep-other", TargetAssertionID: assertionID("ep-other", model.AssertionDecision, 1)},
		"target older": {UserID: 7, SourceEpisodeID: "ep-a", SourceAssertionID: assertionID("ep-a", model.AssertionDecision, 1),
			TargetEpisodeID: "ep-early", TargetAssertionID: assertionID("ep-early", model.AssertionDecision, 1)},
		"target not readable": {UserID: 7, SourceEpisodeID: "ep-a", SourceAssertionID: assertionID("ep-a", model.AssertionDecision, 1),
			TargetEpisodeID: "ep-arch", TargetAssertionID: assertionID("ep-arch", model.AssertionDecision, 1)},
		"malformed source": {UserID: 7, SourceAssertionID: "not-an-id", TargetAssertionID: "ep-b:d1"},
		"synopsis with number": {UserID: 7, SourceEpisodeID: "ep-a", SourceAssertionID: model.LegacyAssertionID("ep-a", model.AssertionSynopsis, nil, "会话概述 ep-a"),
			TargetEpisodeID: "ep-b", TargetAssertionID: assertionID("ep-b", model.AssertionDecision, 1)},
	} {
		if err := governor.MarkReplaced(context.Background(), request); err == nil {
			t.Errorf("%s: the relation was accepted", name)
		} else if !errors.Is(err, ErrRelationRejected) {
			t.Errorf("%s: rejected with the wrong class: %v", name, err)
		}
	}
	if store.saves != 0 {
		t.Fatalf("%d refused relations were written anyway", store.saves)
	}
}

// A chain is fine; a loop is not.
func TestChainIsAllowedAndCycleIsRefused(t *testing.T) {
	store := newMemoryStore()
	first := episode("ep-1", 7, 40, []string{"一"}, nil, nil)
	second := episode("ep-2", 7, 30, []string{"二"}, nil, nil)
	third := episode("ep-3", 7, 20, []string{"三"}, nil, nil)
	for _, item := range []model.EpisodeMemory{first, second, third} {
		store.episodes[item.ID] = item
	}
	governor := governorWith(t, store)

	if err := governor.MarkReplaced(context.Background(), ReplaceRequest{
		UserID: 7, SourceEpisodeID: "ep-1", SourceAssertionID: assertionID("ep-1", model.AssertionDecision, 1),
		TargetEpisodeID: "ep-2", TargetAssertionID: assertionID("ep-2", model.AssertionDecision, 1)}); err != nil {
		t.Fatalf("a straight chain must be allowed: %v", err)
	}
	if err := governor.MarkReplaced(context.Background(), ReplaceRequest{
		UserID: 7, SourceEpisodeID: "ep-2", SourceAssertionID: assertionID("ep-2", model.AssertionDecision, 1),
		TargetEpisodeID: "ep-3", TargetAssertionID: assertionID("ep-3", model.AssertionDecision, 1)}); err != nil {
		t.Fatalf("extending the chain must be allowed: %v", err)
	}
	// ep-3 -> ep-1 would close 1 -> 2 -> 3 -> 1. Making ep-1 the most recent
	// card removes the time objection, so the only thing left to refuse it is
	// the cycle walk - which is the point of the test.
	store.episodes["ep-1"] = episode("ep-1", 7, 5, []string{"一"}, nil, nil)
	dateFixtures(store)
	err := governor.MarkReplaced(context.Background(), ReplaceRequest{
		UserID: 7, SourceEpisodeID: "ep-3", SourceAssertionID: assertionID("ep-3", model.AssertionDecision, 1),
		TargetEpisodeID: "ep-1", TargetAssertionID: assertionID("ep-1", model.AssertionDecision, 1)})
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("the cycle was not refused: %v", err)
	}
}

// Optimistic concurrency: a stale writer loses and is told to re-read.
func TestConcurrentGovernanceIsVersionChecked(t *testing.T) {
	store := newMemoryStore()
	older := episode("ep-a", 7, 30, []string{"旧决定"}, nil, nil)
	newer := episode("ep-b", 7, 10, []string{"新决定"}, nil, nil)
	store.episodes[older.ID], store.episodes[newer.ID] = older, newer
	governor := governorWith(t, store)

	if err := governor.MarkReplaced(context.Background(), ReplaceRequest{
		UserID: 7, SourceEpisodeID: "ep-a", SourceAssertionID: assertionID("ep-a", model.AssertionDecision, 1),
		TargetEpisodeID: "ep-b", TargetAssertionID: assertionID("ep-b", model.AssertionDecision, 1)}); err != nil {
		t.Fatal(err)
	}
	// A second writer that still believes there is no row.
	err := governor.MarkCurrent(context.Background(), 7, "ep-a", assertionID("ep-a", model.AssertionDecision, 1), 0)
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("a stale write was accepted: %v", err)
	}
	// Re-reading and writing with the right version succeeds.
	if err := governor.MarkCurrent(context.Background(), 7, "ep-a", assertionID("ep-a", model.AssertionDecision, 1), 1); err != nil {
		t.Fatalf("a correctly versioned write was refused: %v", err)
	}
}

// There is no bulk "mark everything current". The absence is the safeguard, so
// it is asserted rather than assumed.
func TestNoConstructorCanMintCurrentForLegacyData(t *testing.T) {
	item := model.Assertion{ID: "ep-a:d1", EpisodeID: "ep-a", UserID: 7,
		Kind: model.AssertionDecision, OccurredAt: fixedNow}
	if state := model.NewAssertionState(item); state.State != model.AssertionUnknown {
		t.Fatalf("the constructor produced %s; only unknown is allowed", state.State)
	}
}

// Assertion ids must survive a re-projection that rewrites the text but keeps
// the order, or every recorded relation is orphaned.
func TestAssertionIDsAreStableAcrossReprojection(t *testing.T) {
	before := episode("ep-a", 7, 30, []string{"原文"}, []string{"未完成"}, []string{"纠正"})
	after := episode("ep-a", 7, 30, []string{"重写后的原文"}, []string{"重写后的未完成"}, []string{"重写后的纠正"})
	ids := func(source model.EpisodeMemory) []string {
		out := []string{}
		for _, item := range model.AssertionsOf(source) {
			out = append(out, item.ID)
		}
		return out
	}
	first, second := ids(before), ids(after)
	if len(first) != 4 || len(second) != 4 {
		t.Fatalf("expected four slots, got %d and %d", len(first), len(second))
	}
	for index := range first {
		if first[index] != second[index] {
			t.Fatalf("slot %d changed identity: %s -> %s", index, first[index], second[index])
		}
	}
}

// An identity must be one this server issued. Nothing is parsed out of it,
// because v1.1 identities are opaque by design.
func TestOnlyServerIssuedIdentitiesAreAccepted(t *testing.T) {
	issued, err := model.NewAssertionID()
	if err != nil {
		t.Fatal(err)
	}
	for _, valid := range []string{issued, model.DeterministicAssertionID("seed"),
		model.LegacyAssertionID("ep-x", model.AssertionDecision, []uint{1}, "文本")} {
		if !model.ValidAssertionID(valid) {
			t.Errorf("%q was rejected", valid)
		}
	}
	// The positional shape v1 used is no longer an identity at all, so an old
	// relation cannot be revived by writing one by hand.
	for _, invalid := range []string{"", "ep-x", "ep-x:d1", "ea_", "ea_zzzz", "lg_short"} {
		if model.ValidAssertionID(invalid) {
			t.Errorf("%q was accepted as an assertion id", invalid)
		}
	}
	if !model.IsLegacyAssertionID(model.LegacyAssertionID("ep-x", model.AssertionDecision, nil, "t")) {
		t.Error("a derived identity did not report itself as legacy")
	}
	if model.IsLegacyAssertionID(issued) {
		t.Error("an issued identity reported itself as legacy")
	}
}

// A row whose fields contradict each other must never reach the table.
func TestAssertionStateSelfValidation(t *testing.T) {
	base := model.EpisodeAssertionState{
		AssertionID: assertionID("ep-a", model.AssertionDecision, 1), UserID: 7, EpisodeID: "ep-a",
		Kind: model.AssertionDecision, State: model.AssertionCurrent, OccurredAt: fixedNow, Version: 1,
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("a well-formed row was refused: %v", err)
	}
	unissuedIdentity := base
	unissuedIdentity.AssertionID = "ep-a:d1"
	invalidKind := base
	invalidKind.Kind = "nonsense"
	replacedWithoutTarget := base
	replacedWithoutTarget.State = model.AssertionReplaced
	currentWithTarget := base
	currentWithTarget.ReplacedByAssertionID = assertionID("ep-b", model.AssertionDecision, 1)
	currentWithTarget.ReplacedByEpisodeID = "ep-b"
	unissuedTarget := base
	unissuedTarget.State = model.AssertionReplaced
	unissuedTarget.ReplacedByAssertionID, unissuedTarget.ReplacedByEpisodeID = "ep-b:d1", "ep-b"

	for name, row := range map[string]model.EpisodeAssertionState{
		"identity this server never issued": unissuedIdentity,
		"invalid kind":                      invalidKind,
		"replaced without target":           replacedWithoutTarget,
		"current with target":               currentWithTarget,
		"replacement never issued":          unissuedTarget,
	} {
		if err := row.Validate(); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}
