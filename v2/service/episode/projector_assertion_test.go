package episode

import (
	"context"
	"strings"
	"testing"
	"time"

	episodedao "GopherAI/dao/episode"
	"GopherAI/model"
)

// STABILIZE-058 v1.2, group A: the real write chain.
//
// v1.1 built stable identity, reconciliation and an order-independent matcher,
// and then left them where only tests could reach them: Projector.Project
// generated a document and saved it directly. Every property v1.1 claimed was
// therefore true of a helper function and false of the system, which is a
// worse position than not having the helper - the freeze said "stable" while
// production still wrote position-numbered documents.
//
// These tests drive Projector.Project, the function that actually persists.

type assertLoader struct{ evidence ProjectionEvidence }

func (s assertLoader) Load(context.Context, int64, string, uint) (ProjectionEvidence, error) {
	return s.evidence, nil
}

type assertGenerator struct{ document model.EpisodeDocument }

func (s assertGenerator) Generate(context.Context, ProjectionEvidence) (ProjectionOutcome, error) {
	return Generated(s.document), nil
}

type assertRepository struct {
	existing *model.EpisodeMemory
	saved    *model.EpisodeMemory
}

func (s *assertRepository) GetBySession(context.Context, int64, string) (*model.EpisodeMemory, error) {
	if s.existing == nil {
		return nil, episodedao.ErrEpisodeNotFound
	}
	return s.existing, nil
}

func (s *assertRepository) Save(_ context.Context, episode *model.EpisodeMemory, _ uint64) error {
	clone := *episode
	s.saved = &clone
	return nil
}

var projectionClock = time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)

func assertMessage(id uint, content string, offset time.Duration) model.Message {
	return model.Message{ID: id, SessionID: "s-1", UserName: "learner", Content: content,
		IsUser: true, CreatedAt: projectionClock.Add(offset)}
}

func evidenceFor(existing *model.EpisodeMemory, messages ...model.Message) ProjectionEvidence {
	return ProjectionEvidence{
		UserID:   7,
		Session:  model.Session{ID: "s-1", UserID: 7, UserName: "learner", Mode: model.SessionModeAgent, Title: "会话"},
		Messages: messages, Existing: existing,
		TargetMessageID: messages[len(messages)-1].ID,
		TargetMessageAt: messages[len(messages)-1].CreatedAt,
	}
}

func assertDocument(decisions, openLoops, corrections []model.EpisodeEvidence) model.EpisodeDocument {
	return model.EpisodeDocument{
		Synopsis: "讨论了消息边界", Topics: []model.EpisodeTopic{{Key: "framing", Label: "framing"}},
		Decisions: decisions, OpenLoops: openLoops, UserCorrections: corrections,
		SourceHandles: []string{"message:1"},
	}
}

func assertEntry(text string, ids ...uint) model.EpisodeEvidence {
	return model.EpisodeEvidence{Text: text, EvidenceMessageIDs: ids}
}

func projectWith(t *testing.T, existing *model.EpisodeMemory, generated model.EpisodeDocument,
	messages ...model.Message) (*model.EpisodeMemory, error) {
	t.Helper()
	repository := &assertRepository{existing: existing}
	projector, err := NewProjector(assertLoader{evidence: evidenceFor(existing, messages...)},
		assertGenerator{document: generated}, repository, allowAll())
	if err != nil {
		t.Fatal(err)
	}
	return projector.Project(context.Background(), 7, "s-1", messages[len(messages)-1].ID)
}

func savedIDs(episode *model.EpisodeMemory) map[string]string {
	out := map[string]string{}
	for _, group := range [][]model.EpisodeEvidence{
		episode.Document.Decisions, episode.Document.OpenLoops, episode.Document.UserCorrections} {
		for _, item := range group {
			out[item.Text] = item.ID
		}
	}
	return out
}

// A1: every slot of a first projection is persisted with a server-issued id.
func TestFirstProjectionIssuesStableIdentities(t *testing.T) {
	episode, err := projectWith(t, nil,
		assertDocument([]model.EpisodeEvidence{assertEntry("用固定头部", 1)},
			[]model.EpisodeEvidence{assertEntry("半包没写", 2)},
			[]model.EpisodeEvidence{assertEntry("纠正：不是缓冲区问题", 2)}),
		assertMessage(1, "先说边界", 0), assertMessage(2, "再说半包", time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for text, id := range savedIDs(episode) {
		if id == "" {
			t.Errorf("%q was persisted with no identity", text)
			continue
		}
		if !model.ValidAssertionID(id) {
			t.Errorf("%q carries %q, which this server never issued", text, id)
		}
		if model.IsLegacyAssertionID(id) {
			t.Errorf("%q got a legacy derived identity on a first projection", text)
		}
	}
}

// A9: identities must be assigned before the Episode is saved, not afterwards.
func TestIdentitiesAreAssignedBeforeSave(t *testing.T) {
	repository := &assertRepository{}
	projector, err := NewProjector(
		assertLoader{evidence: evidenceFor(nil, assertMessage(1, "内容", 0))},
		assertGenerator{document: assertDocument([]model.EpisodeEvidence{assertEntry("决定", 1)}, nil, nil)},
		repository, allowAll())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := projector.Project(context.Background(), 7, "s-1", 1); err != nil {
		t.Fatal(err)
	}
	if repository.saved == nil {
		t.Fatal("nothing was saved")
	}
	if id := repository.saved.Document.Decisions[0].ID; !model.ValidAssertionID(id) {
		t.Fatalf("the row reached the repository carrying %q", id)
	}
}

// A2, A3: inserting, reordering and deleting must not move a survivor's id.
func TestReprojectionKeepsIdentitiesThroughStructuralEdits(t *testing.T) {
	first, err := projectWith(t, nil,
		assertDocument([]model.EpisodeEvidence{assertEntry("第一条", 1), assertEntry("第二条", 2), assertEntry("第三条", 3)}, nil, nil),
		assertMessage(1, "一", 0), assertMessage(2, "二", time.Hour), assertMessage(3, "三", 2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	before := savedIDs(first)

	// A new decision first, the middle one deleted, the rest reordered.
	second, err := projectWith(t, first,
		assertDocument([]model.EpisodeEvidence{assertEntry("新加的", 4), assertEntry("第三条", 3), assertEntry("第一条", 1)}, nil, nil),
		assertMessage(1, "一", 0), assertMessage(2, "二", time.Hour), assertMessage(3, "三", 2*time.Hour),
		assertMessage(4, "四", 3*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	after := savedIDs(second)

	for _, text := range []string{"第一条", "第三条"} {
		if before[text] != after[text] {
			t.Errorf("%q moved from %s to %s across a reprojection", text, before[text], after[text])
		}
	}
	if after["新加的"] == "" || after["新加的"] == before["第一条"] {
		t.Error("the new fact did not get an identity of its own")
	}
}

// A4: the same fact reworded keeps its identity when its evidence identifies it.
func TestRewordingKeepsIdentityWhenEvidenceIsUnique(t *testing.T) {
	first, err := projectWith(t, nil,
		assertDocument([]model.EpisodeEvidence{assertEntry("用固定头部", 1)}, nil, nil),
		assertMessage(1, "一", 0))
	if err != nil {
		t.Fatal(err)
	}
	// The projector skips work when nothing new has arrived, so the reprojection
	// has to advance the watermark the way a real session would.
	second, err := projectWith(t, first,
		assertDocument([]model.EpisodeEvidence{assertEntry("采用固定长度头部划定边界", 1)}, nil, nil),
		assertMessage(1, "一", 0), assertMessage(2, "二", time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if savedIDs(first)["用固定头部"] != savedIDs(second)["采用固定长度头部划定边界"] {
		t.Fatal("a reworded fact lost its identity")
	}
}

// A5: ambiguity must fail the projection, not resolve itself by position.
func TestAmbiguousReprojectionFailsClosed(t *testing.T) {
	first, err := projectWith(t, nil,
		assertDocument([]model.EpisodeEvidence{assertEntry("方案甲", 1), assertEntry("方案乙", 1)}, nil, nil),
		assertMessage(1, "一", 0))
	if err != nil {
		t.Fatal(err)
	}
	_, err = projectWith(t, first,
		assertDocument([]model.EpisodeEvidence{assertEntry("改写后的甲", 1), assertEntry("改写后的乙", 1)}, nil, nil),
		assertMessage(1, "一", 0), assertMessage(2, "二", time.Hour))
	if err == nil {
		t.Fatal("an ambiguous reprojection was resolved rather than refused")
	}
}

// A6: a governed assertion must still name the same fact afterwards.
func TestGovernedAssertionStillNamesItsFact(t *testing.T) {
	first, err := projectWith(t, nil,
		assertDocument([]model.EpisodeEvidence{assertEntry("旧方案：分隔符", 1), assertEntry("新方案：固定头部", 2)}, nil, nil),
		assertMessage(1, "一", 0), assertMessage(2, "二", time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	governed := savedIDs(first)["旧方案：分隔符"]

	second, err := projectWith(t, first,
		assertDocument([]model.EpisodeEvidence{assertEntry("先复述成因", 3), assertEntry("旧方案：分隔符", 1),
			assertEntry("新方案：固定头部", 2)}, nil, nil),
		assertMessage(1, "一", 0), assertMessage(2, "二", time.Hour), assertMessage(3, "三", 2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range second.Document.Decisions {
		if item.ID == governed && item.Text != "旧方案：分隔符" {
			t.Fatalf("the governed identity now names %q", item.Text)
		}
	}
	if orphans := model.OrphanedGovernance([]string{governed}, model.AssertionsOf(*second)); len(orphans) != 0 {
		t.Fatalf("the governed assertion was orphaned: %v", orphans)
	}
}

// A8: an identity the model returns must be one this server issued for this
// episode and this kind, exactly once.
func TestReturnedIdentitiesAreValidated(t *testing.T) {
	first, err := projectWith(t, nil,
		assertDocument([]model.EpisodeEvidence{assertEntry("决定一", 1)}, []model.EpisodeEvidence{assertEntry("未完成", 2)}, nil),
		assertMessage(1, "一", 0), assertMessage(2, "二", time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	issued := savedIDs(first)["决定一"]
	openLoop := savedIDs(first)["未完成"]

	forged := assertEntry("决定一", 1)
	forged.ID = "ea_000000000000000000000000"
	duplicateA, duplicateB := assertEntry("决定一", 1), assertEntry("决定二", 2)
	duplicateA.ID, duplicateB.ID = issued, issued
	crossKind := assertEntry("决定一", 1)
	crossKind.ID = openLoop

	for name, decisions := range map[string][]model.EpisodeEvidence{
		"unknown identity":    {forged},
		"duplicated identity": {duplicateA, duplicateB},
		"cross-kind identity": {crossKind},
	} {
		if _, err := projectWith(t, first, assertDocument(decisions, nil, nil),
			assertMessage(1, "一", 0), assertMessage(2, "二", time.Hour),
			assertMessage(3, "三", 2*time.Hour)); err == nil {
			t.Errorf("%s: the projection was accepted", name)
		}
	}
}

// The model never supplies a time. It is derived from the messages an
// assertion cites, and the latest of them is when the assertion was complete.
func TestOccurredAtComesFromEvidenceMessages(t *testing.T) {
	episode, err := projectWith(t, nil,
		assertDocument([]model.EpisodeEvidence{assertEntry("跨两条消息的决定", 1, 2)},
			[]model.EpisodeEvidence{assertEntry("只由第一条支撑", 1)}, nil),
		assertMessage(1, "一", 0), assertMessage(2, "二", 3*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	decision := episode.Document.Decisions[0]
	openLoop := episode.Document.OpenLoops[0]
	if !decision.OccurredAt.Equal(projectionClock.Add(3 * time.Hour).UTC()) {
		t.Fatalf("the decision was dated %s, want its latest evidence", decision.OccurredAt)
	}
	if !openLoop.OccurredAt.Equal(projectionClock.UTC()) {
		t.Fatalf("the open loop was dated %s, want its own evidence", openLoop.OccurredAt)
	}
	if decision.OccurredAt.Equal(openLoop.OccurredAt) {
		t.Fatal("two assertions in one card shared a timestamp; card-level dating is back")
	}
}

// Legacy documents keep working: a prior with no identities is reconciled by
// fingerprint, and nothing is silently declared current.
func TestLegacyPriorWithoutIdentitiesStillReconciles(t *testing.T) {
	legacy := &model.EpisodeMemory{
		ID: "ep-legacy", UserID: 7, SessionID: "s-1", Title: "旧卡", SearchText: "旧卡",
		Document:            assertDocument([]model.EpisodeEvidence{assertEntry("旧决定", 1)}, nil, nil),
		SourceLastMessageID: 1, LastMessageAt: projectionClock, Version: 1,
		Status: model.EpisodeStatusActive, IndexStatus: model.EpisodeIndexIndexed,
	}
	updated, err := projectWith(t, legacy,
		assertDocument([]model.EpisodeEvidence{assertEntry("旧决定", 1), assertEntry("新决定", 2)}, nil, nil),
		assertMessage(1, "一", 0), assertMessage(2, "二", time.Hour))
	if err != nil {
		t.Fatalf("a legacy prior broke reprojection: %v", err)
	}
	ids := savedIDs(updated)
	if !model.ValidAssertionID(ids["旧决定"]) || !model.ValidAssertionID(ids["新决定"]) {
		t.Fatalf("identities after a legacy reprojection: %v", ids)
	}
	if !strings.HasPrefix(ids["旧决定"], "lg_") {
		t.Fatalf("the legacy assertion should keep its derived identity, got %q", ids["旧决定"])
	}
}
