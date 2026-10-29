package episoderelation

import (
	"testing"
	"time"

	episodedao "GopherAI/dao/episode"
	episodeassertdao "GopherAI/dao/episodeassert"
	"GopherAI/model"
	"GopherAI/service/episode"
)

// STABILIZE-065 Phase 2-C1: an abstained row must be invisible to memory.
//
// Persisting an abstention instead of writing nothing is only safe if the row
// cannot be mistaken for memory. Every production read that serves recall,
// governance or relations filters positively on EpisodeStatusActive, so the new
// status is excluded by construction - but "by construction" is what the
// cross-session boundary also was before this round, and it is exactly the kind
// of guarantee that quietly stops holding.
//
// So this drives the real repositories against a real store, with one abstained
// row and one active row side by side, and requires every read to return the
// active one and only the active one.
func TestAbstainedRowsAreInvisibleToEveryMemoryRead(t *testing.T) {
	db := openTestDB(t)
	owner := int64(900000000000000501)
	at := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)

	seedSession(t, db, owner, "sess-iso-active", 9501, "接口统一固定超时 30 秒", at)
	seedSession(t, db, owner, "sess-iso-abstain", 9502, "一次普通提问", at.AddDate(0, 0, 1))

	active := seedEpisode(t, db, owner, "ep-iso-active", "sess-iso-active", 9501,
		"接口统一固定超时 30 秒", at)

	// The abstained row: same owner, same shape, no assertions, and the status
	// this round introduced.
	abstained := model.EpisodeMemory{
		ID: "ep-iso-abstain", UserID: owner, SessionID: "sess-iso-abstain",
		Title:      "（未保存：无长期内容）",
		SearchText: "本次会话没有需要长期保存的内容。",
		Document: model.EpisodeDocument{
			Synopsis:      "本次会话没有需要长期保存的内容。",
			Topics:        []model.EpisodeTopic{},
			SourceHandles: []string{"abstain:sess-iso-abstain:9502"},
		},
		TopicKeys:           model.StringList{},
		SourceLastMessageID: 9502,
		LastMessageAt:       at.AddDate(0, 0, 1),
		Version:             1,
		Status:              model.EpisodeStatusAbstained,
		IndexStatus:         model.EpisodeIndexIndexed,
		AbstainReason:       model.AbstainNoDurableAssertion,
		CreatedAt:           at, UpdatedAt: at,
	}
	if err := db.Save(&abstained).Error; err != nil {
		t.Fatalf("seed the abstained row: %v", err)
	}

	episodes := episodedao.NewRepository(db)
	ctx := itCtx()
	ids := []string{active.ID, abstained.ID}

	// 1. Retrieval: the candidate loader the memory search reads through.
	candidates, err := episodes.LoadCandidates(ctx, owner, "sess-current", ids)
	if err != nil {
		t.Fatalf("load candidates: %v", err)
	}
	assertOnlyActive(t, "LoadCandidates", candidates, active.ID)

	// 2. Indexing: what the reindexer would push into the vector index.
	reindex, err := episodes.ListActiveForReindex(ctx, owner, "", 50)
	if err != nil {
		t.Fatalf("list for reindex: %v", err)
	}
	assertOnlyActive(t, "ListActiveForReindex", reindex, active.ID)

	// 3. Assertion state: the governed read chain's own episode loader.
	states, err := episodeassertdao.NewRepository(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := states.LoadEpisode(ctx, owner, abstained.ID); err == nil {
		t.Fatal("assertion governance was able to load an abstained episode")
	}
	if _, err := states.LoadEpisode(ctx, owner, active.ID); err != nil {
		t.Fatalf("assertion governance can no longer load an active episode: %v", err)
	}

	// 4. Relations: the write-side candidate source.
	source, err := NewCandidateRepository(db, DefaultHistoryLimit)
	if err != nil {
		t.Fatal(err)
	}
	found, err := source.ActiveAssertions(ctx, owner, "sess-current")
	if err != nil {
		t.Fatalf("active assertions: %v", err)
	}
	for _, candidate := range found {
		if candidate.EpisodeID == abstained.ID {
			t.Fatal("the relation candidate source offered an abstained episode")
		}
	}
	if len(found) == 0 {
		t.Fatal("the relation candidate source returned nothing at all, so this " +
			"test would pass even if the filter were removed")
	}

	// 5. Rendering: an abstained row carries no assertion for a card to show.
	if n := len(model.AssertionsOf(abstained)); n != 1 {
		t.Fatalf("an abstained row flattens to %d assertions; only its synopsis "+
			"should exist, and nothing citable", n)
	}
	for _, assertion := range model.AssertionsOf(abstained) {
		if assertion.Kind != model.AssertionSynopsis {
			t.Fatalf("an abstained row carries a %s assertion", assertion.Kind)
		}
	}
}

func assertOnlyActive(t *testing.T, what string, rows []model.EpisodeMemory, wantID string) {
	t.Helper()
	if len(rows) == 0 {
		t.Fatalf("%s returned nothing, so this check would pass even with the "+
			"status filter removed", what)
	}
	for _, row := range rows {
		if row.Status != model.EpisodeStatusActive {
			t.Fatalf("%s returned a %q row", what, row.Status)
		}
	}
	found := false
	for _, row := range rows {
		if row.ID == wantID {
			found = true
		}
	}
	if !found {
		t.Fatalf("%s did not return the active row", what)
	}
}

// The production projector and its new outcome type are referenced so this file
// fails to build if either is removed, rather than silently testing a status
// nothing writes any more.
var _ = episode.OutcomeAbstained
