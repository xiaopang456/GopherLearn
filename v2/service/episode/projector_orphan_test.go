package episode

import (
	"context"
	"errors"
	"testing"
	"time"

	"GopherAI/model"
)

// STABILIZE-058 v1.3: a re-projection may not orphan a governance relation.
//
// v1.2 computed the orphans and then logged them, which is the shape of a check
// without being one. The state table would still hold "this decision was
// replaced by that one" while the card no longer contained "this decision", so
// the relation resolved to nothing, and nothing is exactly what an invisible
// failure looks like: no error, no orphan visible in any read, and a governed
// answer quietly missing the fact it was supposed to qualify.
//
// The rule chosen here is fail-closed at the point of writing, so the previous
// Episode version stays intact and the relation stays resolvable. The narrower
// alternative - keeping a tombstone row for the vanished assertion - is a data
// model change, and this round does not make one.

// governanceIndex is the minimal read-only view, with the two things a relation
// can point at: an assertion's own state, and an assertion named as the
// replacement by some other one.
type governanceIndex struct {
	governed map[string]struct{}
	fail     error
	reads    int
}

func (g *governanceIndex) GovernedAssertions(_ context.Context, _ int64,
	_ string) (map[string]struct{}, error) {
	g.reads++
	if g.fail != nil {
		return nil, g.fail
	}
	out := map[string]struct{}{}
	for id := range g.governed {
		out[id] = struct{}{}
	}
	return out, nil
}

// countingRepository records how many times a projection reached the database.
type countingRepository struct {
	existing *model.EpisodeMemory
	saved    *model.EpisodeMemory
	saves    int
}

func (r *countingRepository) GetBySession(context.Context, int64, string) (*model.EpisodeMemory, error) {
	if r.existing == nil {
		return nil, nil
	}
	return r.existing, nil
}

func (r *countingRepository) Save(_ context.Context, episode *model.EpisodeMemory, _ uint64) error {
	clone := *episode
	r.saved, r.saves = &clone, r.saves+1
	return nil
}

// governedProjection re-projects an existing card under a governance index.
func governedProjection(t *testing.T, existing *model.EpisodeMemory, index GovernanceIndex,
	generated model.EpisodeDocument, messages ...model.Message) (*countingRepository, error) {
	t.Helper()
	repository := &countingRepository{existing: existing}
	projector, err := NewProjector(assertLoader{evidence: evidenceFor(existing, messages...)},
		assertGenerator{document: generated}, repository, allowAll())
	if err != nil {
		t.Fatal(err)
	}
	if index != nil {
		projector.UseGovernanceIndex(index)
	}
	_, err = projector.Project(context.Background(), 7, "s-1", messages[len(messages)-1].ID)
	return repository, err
}

// firstCard is one projection with two decisions, and the identity of the one
// the tests then govern.
func firstCard(t *testing.T) (*model.EpisodeMemory, map[string]string) {
	t.Helper()
	first, err := projectWith(t, nil,
		assertDocument([]model.EpisodeEvidence{assertEntry("旧方案：分隔符", 1), assertEntry("新方案：固定头部", 2)}, nil, nil),
		assertMessage(1, "一", 0), assertMessage(2, "二", time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return first, savedIDs(first)
}

// 1: a governed assertion disappearing fails the projection, and nothing is
// saved.
func TestGovernedAssertionDeletedFailsTheProjection(t *testing.T) {
	first, ids := firstCard(t)
	index := &governanceIndex{governed: map[string]struct{}{ids["旧方案：分隔符"]: {}}}
	version := first.Version

	repository, err := governedProjection(t, first, index,
		assertDocument([]model.EpisodeEvidence{assertEntry("新方案：固定头部", 2)}, nil, nil),
		assertMessage(1, "一", 0), assertMessage(2, "二", time.Hour), assertMessage(3, "三", 2*time.Hour))
	if err == nil {
		t.Fatal("a governed assertion vanished and the projection succeeded")
	}
	if !errors.Is(err, ErrOrphanedGovernance) {
		t.Fatalf("the failure was not reported as an orphan: %v", err)
	}
	if repository.saves != 0 {
		t.Fatalf("the projection wrote %d times after refusing", repository.saves)
	}
	if first.Version != version {
		t.Fatalf("the previous Episode version moved to %d", first.Version)
	}
}

// 2: an assertion another relation names as its replacement is governed too,
// even with no state row of its own.
func TestReplacementTargetDeletedFailsTheProjection(t *testing.T) {
	first, ids := firstCard(t)
	// Nothing governs 新方案 directly; it is what 旧方案 was replaced by, which
	// is recorded on the other row and returned by the index.
	index := &governanceIndex{governed: map[string]struct{}{ids["新方案：固定头部"]: {}}}

	repository, err := governedProjection(t, first, index,
		assertDocument([]model.EpisodeEvidence{assertEntry("旧方案：分隔符", 1)}, nil, nil),
		assertMessage(1, "一", 0), assertMessage(2, "二", time.Hour), assertMessage(3, "三", 2*time.Hour))
	if err == nil {
		t.Fatal("a replacement target vanished and the projection succeeded")
	}
	if repository.saves != 0 {
		t.Fatalf("the projection wrote %d times after refusing", repository.saves)
	}
}

// 3: an ordinary, ungoverned assertion may still disappear. The learner's own
// words stop supporting facts all the time, and refusing that would freeze
// every card the moment one relation existed anywhere.
func TestUngovernedAssertionMayDisappear(t *testing.T) {
	first, ids := firstCard(t)
	index := &governanceIndex{governed: map[string]struct{}{ids["新方案：固定头部"]: {}}}

	repository, err := governedProjection(t, first, index,
		assertDocument([]model.EpisodeEvidence{assertEntry("新方案：固定头部", 2)}, nil, nil),
		assertMessage(1, "一", 0), assertMessage(2, "二", time.Hour), assertMessage(3, "三", 2*time.Hour))
	if err != nil {
		t.Fatalf("an ungoverned deletion was refused: %v", err)
	}
	if repository.saves != 1 {
		t.Fatalf("the projection saved %d times", repository.saves)
	}
	if got := savedIDs(repository.saved)["新方案：固定头部"]; got != ids["新方案：固定头部"] {
		t.Fatalf("the surviving governed assertion changed identity to %s", got)
	}
}

// 4: inserting, reordering and rewording leave the governed relation pointing
// at the same stable identity.
func TestGovernedIdentitySurvivesStructuralEdits(t *testing.T) {
	first, ids := firstCard(t)
	governed := ids["旧方案：分隔符"]
	index := &governanceIndex{governed: map[string]struct{}{governed: {}}}

	repository, err := governedProjection(t, first, index,
		assertDocument([]model.EpisodeEvidence{
			assertEntry("先复述成因", 3),
			assertEntry("新方案：固定头部", 2),
			assertEntry("旧方案改写：按分隔符切分", 1),
		}, nil, nil),
		assertMessage(1, "一", 0), assertMessage(2, "二", time.Hour), assertMessage(3, "三", 2*time.Hour))
	if err != nil {
		t.Fatalf("a structural edit was refused: %v", err)
	}
	after := savedIDs(repository.saved)
	if after["旧方案改写：按分隔符切分"] != governed {
		t.Fatalf("the governed relation now points at %s, not %s",
			after["旧方案改写：按分隔符切分"], governed)
	}
	if after["先复述成因"] == governed {
		t.Fatal("the vanished fact was re-attached to a new one in the same position")
	}
}

// 5: a refusal is repeatable. Retrying produces the same failure, no version
// change and no half-written state, which is what makes the refusal safe to
// leave in place while the cause is fixed.
func TestRefusedProjectionRetriesCleanly(t *testing.T) {
	first, ids := firstCard(t)
	index := &governanceIndex{governed: map[string]struct{}{ids["旧方案：分隔符"]: {}}}
	document := assertDocument([]model.EpisodeEvidence{assertEntry("新方案：固定头部", 2)}, nil, nil)
	messages := []model.Message{assertMessage(1, "一", 0), assertMessage(2, "二", time.Hour),
		assertMessage(3, "三", 2*time.Hour)}

	version := first.Version
	for attempt := 1; attempt <= 2; attempt++ {
		repository, err := governedProjection(t, first, index, document, messages...)
		if !errors.Is(err, ErrOrphanedGovernance) {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
		if repository.saves != 0 || repository.saved != nil {
			t.Fatalf("attempt %d wrote something", attempt)
		}
		if first.Version != version {
			t.Fatalf("attempt %d moved the Episode version to %d", attempt, first.Version)
		}
	}
	if index.reads != 2 {
		t.Fatalf("the governance index was read %d times across two attempts", index.reads)
	}
}

// A store failure is a projection failure. An empty map read as "nothing is
// governed" would turn this whole check fail-open exactly when the database is
// unwell, which is the failure mode the check exists to prevent.
func TestGovernanceIndexFailureFailsTheProjection(t *testing.T) {
	first, _ := firstCard(t)
	index := &governanceIndex{fail: context.DeadlineExceeded}

	repository, err := governedProjection(t, first, index,
		assertDocument([]model.EpisodeEvidence{assertEntry("新方案：固定头部", 2)}, nil, nil),
		assertMessage(1, "一", 0), assertMessage(2, "二", time.Hour), assertMessage(3, "三", 2*time.Hour))
	if err == nil {
		t.Fatal("an unreadable governance index produced a successful projection")
	}
	if repository.saves != 0 {
		t.Fatal("something was saved after the index failed")
	}
}

// Without an index attached, projection behaves exactly as every mode before
// this one measured it: the deletion goes through.
func TestProjectionWithoutGovernanceIsUnchanged(t *testing.T) {
	first, _ := firstCard(t)
	repository, err := governedProjection(t, first, nil,
		assertDocument([]model.EpisodeEvidence{assertEntry("新方案：固定头部", 2)}, nil, nil),
		assertMessage(1, "一", 0), assertMessage(2, "二", time.Hour), assertMessage(3, "三", 2*time.Hour))
	if err != nil {
		t.Fatalf("an ungoverned mode's projection was refused: %v", err)
	}
	if repository.saves != 1 {
		t.Fatalf("the projection saved %d times", repository.saves)
	}
}
