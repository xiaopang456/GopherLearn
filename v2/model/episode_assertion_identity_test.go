package model

import (
	"testing"
	"time"
)

// STABILIZE-058 v1.1: the identity defect, reproduced before it is fixed.
//
// v1 derived an assertion id from its position in the document -
// <episodeID>:d1, :d2, ... - and called that stable. The test that was supposed
// to prove stability only ever rewrote the text while keeping the count and the
// order, which is the one edit position numbering survives.
//
// Every test below is an edit a projector really performs. Under positional
// numbering each of them silently re-points a recorded governance relation at a
// different fact, which is worse than losing it: a replacement relation would
// go on being displayed while meaning something else.

func evidence(text string, messageIDs ...uint) EpisodeEvidence {
	return EpisodeEvidence{Text: text, EvidenceMessageIDs: messageIDs}
}

func identityEpisode(decisions []EpisodeEvidence, openLoops ...EpisodeEvidence) EpisodeMemory {
	return EpisodeMemory{
		ID: "ep-x", UserID: 7, SessionID: "s-1", Title: "t", SearchText: "t",
		Document: EpisodeDocument{
			Synopsis: "概述", Decisions: decisions, OpenLoops: openLoops,
			SourceHandles: []string{"h"},
		},
		Status: EpisodeStatusActive, IndexStatus: EpisodeIndexIndexed,
		Version: 1, LastMessageAt: time.Now(),
	}
}

// idOf finds the identity a given text currently carries.
func idOf(items []Assertion, text string) string {
	for _, item := range items {
		if item.Text == text {
			return item.ID
		}
	}
	return ""
}

// 1. A newly extracted decision lands at the front of the list. The decisions
// that were already there must keep their identities.
func TestInsertingADecisionDoesNotShiftExistingIdentities(t *testing.T) {
	before := AssertionsOf(identityEpisode([]EpisodeEvidence{
		evidence("用固定头部", 10), evidence("加魔数校验", 20)}))
	after := AssertionsOf(identityEpisode([]EpisodeEvidence{
		evidence("先确认粘包成因", 5), evidence("用固定头部", 10), evidence("加魔数校验", 20)}))

	for _, text := range []string{"用固定头部", "加魔数校验"} {
		if idOf(before, text) != idOf(after, text) {
			t.Errorf("%q changed identity from %s to %s when a decision was inserted before it",
				text, idOf(before, text), idOf(after, text))
		}
	}
}

// 2. The projector emits the same two decisions in the other order. Their
// identities must not swap, or a relation recorded against one now points at
// the other.
func TestReorderingDecisionsDoesNotSwapIdentities(t *testing.T) {
	before := AssertionsOf(identityEpisode([]EpisodeEvidence{
		evidence("用固定头部", 10), evidence("加魔数校验", 20)}))
	after := AssertionsOf(identityEpisode([]EpisodeEvidence{
		evidence("加魔数校验", 20), evidence("用固定头部", 10)}))

	for _, text := range []string{"用固定头部", "加魔数校验"} {
		if idOf(before, text) != idOf(after, text) {
			t.Errorf("%q changed identity from %s to %s when the order changed",
				text, idOf(before, text), idOf(after, text))
		}
	}
}

// 3. A decision is dropped. The ones after it must not move up into its number.
func TestDeletingADecisionDoesNotPromoteTheOthers(t *testing.T) {
	before := AssertionsOf(identityEpisode([]EpisodeEvidence{
		evidence("第一条", 10), evidence("第二条", 20), evidence("第三条", 30)}))
	after := AssertionsOf(identityEpisode([]EpisodeEvidence{
		evidence("第一条", 10), evidence("第三条", 30)}))

	if idOf(before, "第三条") != idOf(after, "第三条") {
		t.Errorf("第三条 moved from %s to %s when 第二条 was deleted",
			idOf(before, "第三条"), idOf(after, "第三条"))
	}
}

// 4. The same fact, reworded by the projector, must keep its identity.
//
// Inheritance across a rewording is the reconciler's job, because it is the
// only step that has the prior assertions to match against: kind plus the cited
// messages identify the fact when the wording no longer does. A bare read of a
// legacy document cannot do it and must not pretend to - it would have to guess.
func TestRewordingTheSameFactKeepsItsIdentity(t *testing.T) {
	prior := AssertionsOf(identityEpisode([]EpisodeEvidence{evidence("用固定头部", 10)}))
	governed := idOf(prior, "用固定头部")

	after, err := ReconcileAssertionIdentities(prior, identityEpisode([]EpisodeEvidence{
		evidence("采用固定长度头部来划定边界", 10)}))
	if err != nil {
		t.Fatalf("a resolvable rewording was refused: %v", err)
	}
	if got := idOf(after, "采用固定长度头部来划定边界"); got != governed {
		t.Errorf("a reworded fact changed identity from %s to %s", governed, got)
	}
}

// 5. Two decisions cite exactly the same messages, so nothing distinguishes
// them but their text. When the text also changes, the reconciler cannot know
// which is which, and it must fail closed rather than guess by position.
func TestAmbiguousEvidenceMustFailClosedRatherThanGuess(t *testing.T) {
	prior := identityEpisode([]EpisodeEvidence{
		evidence("方案甲", 10, 11), evidence("方案乙", 10, 11)})
	priorAssertions := AssertionsOf(prior)

	rewritten := identityEpisode([]EpisodeEvidence{
		evidence("改写后的甲", 10, 11), evidence("改写后的乙", 10, 11)})

	inherited, err := ReconcileAssertionIdentities(priorAssertions, rewritten)
	if err == nil {
		t.Fatalf("ambiguous reconciliation was resolved by guessing: %v",
			func() []string {
				out := []string{}
				for _, item := range inherited {
					out = append(out, item.ID+"="+item.Text)
				}
				return out
			}())
	}
}

// 6. A recorded relation must never end up pointing at a different fact. This
// is the consequence the other five tests are about, stated directly.
func TestReprojectionMustNotRepointAGovernanceRelation(t *testing.T) {
	prior := identityEpisode([]EpisodeEvidence{
		evidence("旧方案：分隔符", 10), evidence("新方案：固定头部", 20)})
	priorAssertions := AssertionsOf(prior)
	governed := idOf(priorAssertions, "旧方案：分隔符")

	// The projector re-emits the same two facts with a third inserted first.
	reprojected := identityEpisode([]EpisodeEvidence{
		evidence("先复述粘包成因", 5), evidence("旧方案：分隔符", 10), evidence("新方案：固定头部", 20)})
	after, err := ReconcileAssertionIdentities(priorAssertions, reprojected)
	if err != nil {
		t.Fatalf("a resolvable reprojection was refused: %v", err)
	}
	if got := idOf(after, "旧方案：分隔符"); got != governed {
		t.Fatalf("the governed assertion %s now names %q instead", governed, textOfID(after, governed))
	}
	if textOfID(after, governed) != "旧方案：分隔符" {
		t.Fatalf("relation target %s silently became %q", governed, textOfID(after, governed))
	}
}

func textOfID(items []Assertion, id string) string {
	for _, item := range items {
		if item.ID == id {
			return item.Text
		}
	}
	return ""
}
