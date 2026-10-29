package memorysearch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	episodedao "GopherAI/dao/episode"
	memorydao "GopherAI/dao/memory"
	"GopherAI/model"
)

// The fixture clock. Every eligibility question below is asked against it
// rather than against the wall clock, so an expiry test cannot start passing
// because a day went by.
var fixtureNow = time.Date(2026, 9, 3, 9, 0, 0, 0, time.UTC)

type fakeMemoryIndex struct {
	vector      []memorydao.MemorySearchResult
	lexical     []memorydao.MemorySearchResult
	vectorErr   error
	lexicalErr  error
	vectorCalls atomic.Int32
	textCalls   atomic.Int32
}

func (f *fakeMemoryIndex) SearchByVector(context.Context, int64, []float32, int) ([]memorydao.MemorySearchResult, error) {
	f.vectorCalls.Add(1)
	return f.vector, f.vectorErr
}

func (f *fakeMemoryIndex) SearchByText(context.Context, int64, []string, int) ([]memorydao.MemorySearchResult, error) {
	f.textCalls.Add(1)
	return f.lexical, f.lexicalErr
}

type fakeMemoryLoader struct {
	rows []model.LongTermMemory
	err  error
	// asked records the ids the fact source was given, so a test can prove the
	// reload happened rather than assume it.
	asked []string
}

func (f *fakeMemoryLoader) LoadByIDs(_ context.Context, userID int64, ids []string) ([]model.LongTermMemory, error) {
	f.asked = append(f.asked, ids...)
	if f.err != nil {
		return nil, f.err
	}
	out := make([]model.LongTermMemory, 0, len(f.rows))
	wanted := map[string]struct{}{}
	for _, id := range ids {
		wanted[id] = struct{}{}
	}
	for _, row := range f.rows {
		if _, ok := wanted[row.ID]; !ok {
			continue
		}
		// The production loader filters by owner and by "in force"; the fake
		// does the owner half only, so the re-check in the searcher is the
		// thing under test rather than a duplicate of the fake.
		if row.UserID != userID {
			continue
		}
		out = append(out, row)
	}
	return out, nil
}

type fakeEpisodeIndex struct {
	vector     []episodedao.EpisodeSearchResult
	lexical    []episodedao.EpisodeSearchResult
	exact      []episodedao.EpisodeSearchResult
	vectorErr  error
	lexicalErr error
}

func (f *fakeEpisodeIndex) SearchByVector(context.Context, int64, string, []float32, int) ([]episodedao.EpisodeSearchResult, error) {
	return f.vector, f.vectorErr
}

func (f *fakeEpisodeIndex) SearchByText(context.Context, int64, string, []string, int) ([]episodedao.EpisodeSearchResult, error) {
	return f.lexical, f.lexicalErr
}

func (f *fakeEpisodeIndex) SearchExactTopics(context.Context, int64, string, []string, int) ([]episodedao.EpisodeSearchResult, error) {
	return f.exact, nil
}

type fakeEpisodeStore struct {
	rows      []model.EpisodeMemory
	byTime    []model.EpisodeMemory
	loadErr   error
	timeErr   error
	timeCalls atomic.Int32
	loadCalls atomic.Int32
}

func (f *fakeEpisodeStore) LoadCandidates(_ context.Context, userID int64, sessionID string, ids []string) ([]model.EpisodeMemory, error) {
	f.loadCalls.Add(1)
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	wanted := map[string]struct{}{}
	for _, id := range ids {
		wanted[id] = struct{}{}
	}
	out := make([]model.EpisodeMemory, 0, len(ids))
	for _, row := range f.rows {
		if _, ok := wanted[row.ID]; !ok {
			continue
		}
		// The production loader also applies status, the current session and
		// the transitive source closure. The fake applies owner and closure
		// only, so the searcher's own re-check stays the thing under test.
		if row.UserID != userID || row.SessionID == sessionID {
			continue
		}
		out = append(out, row)
	}
	return out, nil
}

func (f *fakeEpisodeStore) ListByTime(context.Context, int64, string, time.Time, time.Time, int) ([]model.EpisodeMemory, error) {
	f.timeCalls.Add(1)
	return f.byTime, f.timeErr
}

type fakeEmbedder struct {
	calls atomic.Int32
	err   error
}

func (f *fakeEmbedder) EmbedQuery(context.Context, string) ([]float32, error) {
	f.calls.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	return []float32{0.1, 0.2, 0.3}, nil
}

func memoryRow(id string, userID int64, mutate func(*model.LongTermMemory)) model.LongTermMemory {
	row := model.LongTermMemory{
		ID: id, UserID: userID, Scope: model.MemoryScopeTopic, Subject: "subject " + id,
		Content: "content " + id, TopicKey: "topic-" + id, Confidence: 0.9,
		Status: model.LongTermMemoryActive, UpdatedAt: fixtureNow.Add(-time.Hour),
	}
	if mutate != nil {
		mutate(&row)
	}
	return row
}

func episodeRow(id string, userID int64, sessionID string, mutate func(*model.EpisodeMemory)) model.EpisodeMemory {
	row := model.EpisodeMemory{
		ID: id, UserID: userID, SessionID: sessionID, Title: "title " + id, Version: 3,
		Status: model.EpisodeStatusActive, IndexStatus: model.EpisodeIndexIndexed,
		LastMessageAt: fixtureNow.Add(-48 * time.Hour),
		Document: model.EpisodeDocument{
			Synopsis:  "synopsis " + id,
			Decisions: []model.EpisodeEvidence{{Text: "decision " + id, EvidenceMessageIDs: []uint{11, 12}}},
		},
	}
	if mutate != nil {
		mutate(&row)
	}
	return row
}

func hits(ids ...string) []memorydao.MemorySearchResult {
	out := make([]memorydao.MemorySearchResult, 0, len(ids))
	for index, id := range ids {
		out = append(out, memorydao.MemorySearchResult{MemoryID: id, Rank: index, Distance: 0.2})
	}
	return out
}

func episodeHits(ids ...string) []episodedao.EpisodeSearchResult {
	out := make([]episodedao.EpisodeSearchResult, 0, len(ids))
	for index, id := range ids {
		out = append(out, episodedao.EpisodeSearchResult{
			EpisodeID: id, Rank: index, EmbeddingVersion: episodedao.CurrentEpisodeEmbeddingVersion,
		})
	}
	return out
}

type harness struct {
	memoryIndex  *fakeMemoryIndex
	memoryLoader *fakeMemoryLoader
	episodeIndex *fakeEpisodeIndex
	episodeStore *fakeEpisodeStore
	embedder     *fakeEmbedder
	searcher     *Searcher
}

func newHarness(t *testing.T, mutate func(*harness)) *harness {
	t.Helper()
	h := &harness{
		memoryIndex:  &fakeMemoryIndex{},
		memoryLoader: &fakeMemoryLoader{},
		episodeIndex: &fakeEpisodeIndex{},
		episodeStore: &fakeEpisodeStore{},
		embedder:     &fakeEmbedder{},
	}
	if mutate != nil {
		mutate(h)
	}
	searcher, err := NewSearcher(h.memoryIndex, h.memoryLoader, h.episodeIndex, h.episodeStore, h.embedder, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	h.searcher = searcher
	return h
}

func (h *harness) search(t *testing.T, request Request) Result {
	t.Helper()
	if request.UserID == 0 {
		request.UserID = 1
	}
	if request.SessionID == "" {
		request.SessionID = "current-session"
	}
	if request.Query == "" {
		request.Query = "学习者对消息边界的理解"
	}
	if request.Now.IsZero() {
		request.Now = fixtureNow
	}
	result, err := h.searcher.Search(context.Background(), request)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	return result
}

func ids(result Result) []string {
	out := make([]string, 0, len(result.Candidates))
	for _, candidate := range result.Candidates {
		out = append(out, string(candidate.Kind)+":"+candidate.ID)
	}
	return out
}

// One question, one vector. A mixed question used to be the case that quietly
// paid for two identical embeddings, or lost a channel because someone switched
// one off to avoid paying twice.
func TestBothPathsShareOneQueryEmbedding(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		h.memoryIndex.vector = hits("m-1")
		h.memoryLoader.rows = []model.LongTermMemory{memoryRow("m-1", 1, nil)}
		h.episodeIndex.vector = episodeHits("e-1")
		h.episodeStore.rows = []model.EpisodeMemory{episodeRow("e-1", 1, "other-session", nil)}
	})
	result := h.search(t, Request{})
	if result.Status != StatusOK || len(result.Candidates) != 2 {
		t.Fatalf("status=%s candidates=%v", result.Status, ids(result))
	}
	if got := h.embedder.calls.Load(); got != 1 {
		t.Fatalf("embedding calls = %d, want 1", got)
	}
	if result.EmbeddingCalls != 1 {
		t.Fatalf("reported embedding calls = %d, want 1", result.EmbeddingCalls)
	}
}

// Both kinds are represented before rank decides the rest. A mixed question
// needs one of each far more than it needs the two best of one.
func TestMergeGivesEachKindItsFirstSlot(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		h.memoryIndex.vector = hits("m-1", "m-2", "m-3", "m-4")
		h.memoryLoader.rows = []model.LongTermMemory{
			memoryRow("m-1", 1, nil), memoryRow("m-2", 1, nil), memoryRow("m-3", 1, nil), memoryRow("m-4", 1, nil),
		}
		h.episodeIndex.vector = episodeHits("e-9")
		h.episodeStore.rows = []model.EpisodeMemory{episodeRow("e-9", 1, "other", nil)}
	})
	result := h.search(t, Request{TopK: 2})
	if got := ids(result); len(got) != 2 || got[0] != "semantic:m-1" || got[1] != "episodic:e-9" {
		t.Fatalf("merged = %v", got)
	}
}

// The same question twice produces the same cards in the same order. Without
// it, nothing downstream — a duplicate check, an evaluation, a bug report — can
// be reproduced.
func TestMergeIsStableAcrossRuns(t *testing.T) {
	first := ""
	for attempt := 0; attempt < 8; attempt++ {
		h := newHarness(t, func(h *harness) {
			h.memoryIndex.vector = hits("m-b", "m-a")
			h.memoryIndex.lexical = hits("m-a", "m-b")
			h.memoryLoader.rows = []model.LongTermMemory{memoryRow("m-a", 1, nil), memoryRow("m-b", 1, nil)}
		})
		got := fmt.Sprint(ids(h.search(t, Request{Kinds: []Kind{KindSemantic}})))
		if attempt == 0 {
			first = got
			continue
		}
		if got != first {
			t.Fatalf("ordering drifted: %s then %s", first, got)
		}
	}
}

// Everything the fact source refuses is dropped and counted. The index is a
// candidate layer; MySQL decides what is real.
func TestReloadRejectsEveryIneligibleRecord(t *testing.T) {
	expired := fixtureNow.Add(-time.Hour)
	tests := []struct {
		name   string
		mutate func(*model.LongTermMemory)
	}{
		{name: "another account", mutate: func(m *model.LongTermMemory) { m.UserID = 999 }},
		{name: "archived", mutate: func(m *model.LongTermMemory) { m.Status = model.LongTermMemoryArchived }},
		{name: "superseded", mutate: func(m *model.LongTermMemory) { m.Status = model.LongTermMemorySuperseded }},
		{name: "deleted", mutate: func(m *model.LongTermMemory) { m.Status = model.LongTermMemoryDeleted }},
		{name: "expired", mutate: func(m *model.LongTermMemory) {
			m.Durability = model.MemoryDurabilityTimeBound
			m.ValidUntil = &expired
		}},
		{name: "below confidence", mutate: func(m *model.LongTermMemory) { m.Confidence = 0.1 }},
		{name: "global scope", mutate: func(m *model.LongTermMemory) { m.Scope = model.MemoryScopeGlobal }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := newHarness(t, func(h *harness) {
				h.memoryIndex.vector = hits("m-1")
				h.memoryLoader.rows = []model.LongTermMemory{memoryRow("m-1", 1, test.mutate)}
			})
			result := h.search(t, Request{Kinds: []Kind{KindSemantic}})
			if len(result.Candidates) != 0 {
				t.Fatalf("%s reached the model: %v", test.name, ids(result))
			}
			if result.Status != StatusNoEvidence {
				t.Fatalf("status = %s, want no_evidence", result.Status)
			}
			if result.Diagnostics.ReloadRejections != 1 {
				t.Fatalf("reload rejections = %d, want 1", result.Diagnostics.ReloadRejections)
			}
		})
	}
}

// A Redis entry claiming another owner or the asking session is refused by the
// reload, not by the index that produced it.
func TestForgedIndexEntriesDoNotSurviveTheReload(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		h.episodeIndex.vector = episodeHits("e-foreign", "e-current", "e-ok")
		h.episodeStore.rows = []model.EpisodeMemory{
			episodeRow("e-foreign", 42, "other", nil),
			episodeRow("e-current", 1, "current-session", nil),
			episodeRow("e-ok", 1, "past-session", nil),
		}
	})
	result := h.search(t, Request{Kinds: []Kind{KindEpisodic}})
	if got := ids(result); len(got) != 1 || got[0] != "episodic:e-ok" {
		t.Fatalf("candidates = %v", got)
	}
	if result.Diagnostics.ReloadRejections != 2 {
		t.Fatalf("reload rejections = %d, want 2", result.Diagnostics.ReloadRejections)
	}
}

// An index entry written under a superseded embedding version was ranked in a
// space the query's vector does not live in.
func TestSupersededEmbeddingVersionEntriesAreIgnored(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		stale := episodeHits("e-stale")
		stale[0].EmbeddingVersion = episodedao.CurrentEpisodeEmbeddingVersion - 1
		h.episodeIndex.vector = stale
		h.episodeStore.rows = []model.EpisodeMemory{episodeRow("e-stale", 1, "other", nil)}
	})
	result := h.search(t, Request{Kinds: []Kind{KindEpisodic}})
	if len(result.Candidates) != 0 {
		t.Fatalf("stale-version entry reached the model: %v", ids(result))
	}
	if h.episodeStore.loadCalls.Load() != 0 {
		t.Fatalf("a stale entry still cost a reload")
	}
}

// No vector, no answer. A lexical channel on its own returns the neighbouring
// subject as readily as the right one, and this design refuses to call that a
// degraded result.
func TestVectorOutageDoesNotDegradeToLexicalOnly(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		h.embedder.err = errors.New("embedding provider down")
		h.memoryIndex.lexical = hits("m-1")
		h.memoryLoader.rows = []model.LongTermMemory{memoryRow("m-1", 1, nil)}
		h.episodeIndex.lexical = episodeHits("e-1")
		h.episodeStore.rows = []model.EpisodeMemory{episodeRow("e-1", 1, "other", nil)}
	})
	result := h.search(t, Request{})
	if result.Status != StatusUnavailable {
		t.Fatalf("status = %s, want unavailable", result.Status)
	}
	if len(result.Candidates) != 0 {
		t.Fatalf("lexical-only candidates were returned: %v", ids(result))
	}
	if result.Reason != ReasonEmbeddingUnavailable {
		t.Fatalf("reason = %s", result.Reason)
	}
}

// An outage must not read as an empty memory: the two license completely
// different sentences about the learner.
func TestIndexOutageIsUnavailableRatherThanNoEvidence(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		h.memoryIndex.vectorErr = errors.New("redis down")
		h.episodeIndex.vectorErr = errors.New("redis down")
	})
	result := h.search(t, Request{})
	if result.Status != StatusUnavailable {
		t.Fatalf("status = %s, want unavailable", result.Status)
	}
	empty := newHarness(t, nil)
	if got := empty.search(t, Request{}); got.Status != StatusNoEvidence {
		t.Fatalf("empty memory reported %s, want no_evidence", got.Status)
	}
}

// "Nothing matched, so here is the most recent session" is the fabrication this
// layer exists to refuse.
func TestNoArbitraryRecentEpisodeFallback(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		h.episodeStore.byTime = []model.EpisodeMemory{episodeRow("e-recent", 1, "other", nil)}
		h.episodeStore.rows = []model.EpisodeMemory{episodeRow("e-recent", 1, "other", nil)}
	})
	result := h.search(t, Request{Kinds: []Kind{KindEpisodic}})
	if len(result.Candidates) != 0 {
		t.Fatalf("a recent episode was returned without being asked for: %v", ids(result))
	}
	if h.episodeStore.timeCalls.Load() != 0 {
		t.Fatalf("the time channel ran without an explicit temporal scope")
	}
}

// The time channel is a MySQL read, so an explicit "latest" still works when
// Redis does not — and says it is degraded.
func TestExplicitLatestUsesTheTimeChannelAndDeclaresDegradation(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		h.embedder.err = errors.New("embedding down")
		h.episodeStore.byTime = []model.EpisodeMemory{episodeRow("e-latest", 1, "other", nil)}
		h.episodeStore.rows = []model.EpisodeMemory{episodeRow("e-latest", 1, "other", nil)}
	})
	result := h.search(t, Request{Kinds: []Kind{KindEpisodic}, Temporal: Temporal{Mode: TemporalLatest}})
	if result.Status != StatusOK || len(result.Candidates) != 1 {
		t.Fatalf("status=%s candidates=%v", result.Status, ids(result))
	}
	if !result.Degraded {
		t.Fatalf("a vector-less result was not marked degraded")
	}
}

// A cancelled request stops. Continuing to call external services after the
// caller has gone spends budget on an answer nobody will read.
func TestCancelledContextStopsWithoutCallingProviders(t *testing.T) {
	h := newHarness(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := h.searcher.Search(ctx, Request{UserID: 1, SessionID: "s", Query: "q", Now: fixtureNow})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusUnavailable || result.Reason != ReasonCancelled {
		t.Fatalf("result = %#v", result)
	}
	if h.embedder.calls.Load() != 0 {
		t.Fatalf("a cancelled search still bought an embedding")
	}
}

// An episode's card carries its structured projection, never its source
// handles: those are a write-time audit trail the model cannot act on.
func TestEpisodeCardCarriesProjectionWithoutSourceHandles(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		h.episodeIndex.vector = episodeHits("e-1")
		h.episodeStore.rows = []model.EpisodeMemory{episodeRow("e-1", 1, "other", func(e *model.EpisodeMemory) {
			e.Document.SourceHandles = []string{"message:169", "knowledge:secret"}
		})}
	})
	result := h.search(t, Request{Kinds: []Kind{KindEpisodic}})
	if len(result.Candidates) != 1 {
		t.Fatalf("candidates = %v", ids(result))
	}
	card := result.Candidates[0]
	if !strings.Contains(card.Content, "synopsis e-1") || !strings.Contains(card.Content, "decision e-1") {
		t.Fatalf("card content = %q", card.Content)
	}
	if strings.Contains(card.Content, "message:169") || strings.Contains(card.Content, "knowledge:") {
		t.Fatalf("source handles leaked into the card: %q", card.Content)
	}
	if len(card.EvidenceMessageIDs) != 2 {
		t.Fatalf("evidence ids = %v", card.EvidenceMessageIDs)
	}
}

// kinds restricts what is searched at all, not just what is returned.
func TestKindsRestrictWhichPathsRun(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		h.memoryIndex.vector = hits("m-1")
		h.memoryLoader.rows = []model.LongTermMemory{memoryRow("m-1", 1, nil)}
		h.episodeIndex.vector = episodeHits("e-1")
		h.episodeStore.rows = []model.EpisodeMemory{episodeRow("e-1", 1, "other", nil)}
	})
	result := h.search(t, Request{Kinds: []Kind{KindSemantic}})
	if got := ids(result); len(got) != 1 || got[0] != "semantic:m-1" {
		t.Fatalf("candidates = %v", got)
	}
	if h.episodeStore.loadCalls.Load() != 0 {
		t.Fatalf("the episode path ran for a semantic-only search")
	}
}

// Identity is the caller's, never the request's.
func TestSearchRefusesWithoutTrustedIdentity(t *testing.T) {
	h := newHarness(t, nil)
	for _, request := range []Request{
		{UserID: 0, SessionID: "s", Query: "q"},
		{UserID: 1, SessionID: "", Query: "q"},
		{UserID: 1, SessionID: "s", Query: "   "},
	} {
		if _, err := h.searcher.Search(context.Background(), request); err == nil {
			t.Fatalf("search accepted %#v", request)
		}
	}
}

// The reserve is what the final merge dropped, and it is never part of the
// window. A caller that ignores it must see exactly the candidates this layer
// has always returned, which is what makes STABILIZE-036 an added policy rather
// than a changed candidate layer.
func TestReserveHoldsWhatTheMergeDroppedAndNeverEntersTheWindow(t *testing.T) {
	semantic := []Candidate{
		{Kind: KindSemantic, ID: "s1", rank: 0}, {Kind: KindSemantic, ID: "s2", rank: 1},
		{Kind: KindSemantic, ID: "s3", rank: 2}, {Kind: KindSemantic, ID: "s4", rank: 3},
	}
	episodic := []Candidate{
		{Kind: KindEpisodic, ID: "e1", rank: 0}, {Kind: KindEpisodic, ID: "e2", rank: 1},
		{Kind: KindEpisodic, ID: "e3", rank: 2}, {Kind: KindEpisodic, ID: "e4", rank: 3},
	}
	final := interleave(semantic, episodic, 6)
	held := map[string]struct{}{}
	for _, candidate := range final {
		held[candidate.ID] = struct{}{}
	}
	kept := reserve(semantic, episodic, final)
	if len(kept) != 2 {
		t.Fatalf("reserve = %d records, want the two the merge dropped", len(kept))
	}
	for _, candidate := range kept {
		if _, inWindow := held[candidate.ID]; inWindow {
			t.Fatalf("%s is in both the window and the reserve", candidate.ID)
		}
	}
}

// A single-kind result has no reserve: its window is short because that kind ran
// out of eligible records, not because the merge dropped anything.
func TestSingleKindResultHasNoReserve(t *testing.T) {
	episodic := []Candidate{
		{Kind: KindEpisodic, ID: "e1", rank: 0}, {Kind: KindEpisodic, ID: "e2", rank: 1},
	}
	if kept := reserve(nil, episodic, interleave(nil, episodic, 6)); len(kept) != 0 {
		t.Fatalf("reserve = %v, want empty", kept)
	}
}
