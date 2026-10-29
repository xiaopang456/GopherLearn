package agenttool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"GopherAI/model"
	serviceagent "GopherAI/service/agent"
	"GopherAI/service/episodeassert"
	"GopherAI/service/memorysearch"

	"github.com/cloudwego/eino/components/tool"
)

// STABILIZE-065 Phase 1.1.1, blocker 1: relations reach the model.
//
// Phase 1.1 gave the relation store a real schema, a real writer and a
// relation-aware resolver. Nothing read it. ResolveWithRelations and
// RenderRelationNote had tests that called them directly and no production
// caller at all, which is the same shape of defect STABILIZE-058 v1.2 found in
// ExpandEpisodes: a governed read that exists in a helper and is absent from
// the system.
//
// A supplement the database records and the model never sees is not a feature.
// It is a row.
//
// These tests drive the real memory_search tool.

var relationNow = time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)

// --- a relation reader that counts queries and records their scope ---

type recordingRelations struct {
	mu    sync.Mutex
	rows  []model.EpisodeAssertionRelation
	calls int
	asked [][]string
	fail  bool
}

func (r *recordingRelations) RelationsForAssertions(_ context.Context, userID int64,
	assertionIDs []string) ([]model.EpisodeAssertionRelation, error) {
	r.mu.Lock()
	r.calls++
	r.asked = append(r.asked, append([]string(nil), assertionIDs...))
	failing := r.fail
	r.mu.Unlock()
	if failing {
		return nil, fmt.Errorf("relation store unavailable")
	}
	wanted := map[string]struct{}{}
	for _, id := range assertionIDs {
		wanted[id] = struct{}{}
	}
	out := []model.EpisodeAssertionRelation{}
	for _, row := range r.rows {
		if row.UserID != userID {
			continue
		}
		_, source := wanted[row.SourceAssertionID]
		_, target := wanted[row.TargetAssertionID]
		if source || target {
			out = append(out, row)
		}
	}
	return out, nil
}

func (r *recordingRelations) Calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func (r *recordingRelations) LastAsked() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.asked) == 0 {
		return nil
	}
	return r.asked[len(r.asked)-1]
}

// --- the fixture: three cross-session decisions of one learner ---

// relationStore builds three Episodes, each with one decision, all current.
func relationStore(t *testing.T) (*governedStore, map[string]string) {
	t.Helper()
	store := &governedStore{episodes: map[string]model.EpisodeMemory{},
		states: map[string]model.EpisodeAssertionState{}, times: map[uint]time.Time{}}
	ids := map[string]string{}
	add := func(id, title string, daysAgo int, message uint, text string) {
		at := relationNow.AddDate(0, 0, -daysAgo)
		store.times[message] = at
		assertionID := governedID(id + "|d1")
		ids[id] = assertionID
		store.episodes[id] = model.EpisodeMemory{ID: id, UserID: 7,
			SessionID: "sess-" + id, Title: title, SearchText: text,
			Document: model.EpisodeDocument{Synopsis: "概述：" + title,
				SourceHandles: []string{"h:" + id},
				Decisions: []model.EpisodeEvidence{{ID: assertionID, Text: text,
					EvidenceMessageIDs: []uint{message}, OccurredAt: at}}},
			Status: model.EpisodeStatusActive, IndexStatus: model.EpisodeIndexIndexed,
			Version: 1, LastMessageAt: at}
		store.states[assertionID] = model.EpisodeAssertionState{
			AssertionID: assertionID, UserID: 7, EpisodeID: id,
			Kind: model.AssertionDecision, State: model.AssertionCurrent,
			OccurredAt: at, Version: 1}
	}
	// The titles are ordinary text. If an Episode's title were its id, a leak
	// test could not tell a card title from a leaked handle.
	add("ep-a", "超时策略", 60, 30, "接口统一固定超时 30 秒")
	add("ep-b", "存储与超时", 40, 31, "Redis 同时保存缓存和向量索引，超时策略不变")
	add("ep-c", "超时策略复述", 20, 32, "超时策略默认仍是按任务类型动态计算")
	return store, ids
}

// relationSearcher returns every Episode in the store, oldest first.
type relationSearcher struct{ store *governedStore }

func (s relationSearcher) Search(_ context.Context,
	_ memorysearch.Request) (memorysearch.Result, error) {
	ids := make([]string, 0, len(s.store.episodes))
	for id := range s.store.episodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	candidates := []memorysearch.Candidate{}
	for _, id := range ids {
		episode := s.store.episodes[id]
		candidates = append(candidates, memorysearch.Candidate{
			Kind: memorysearch.KindEpisodic, ID: episode.ID, Version: episode.Version,
			Subject: episode.Title, Content: memorysearch.RenderEpisodeContent(episode),
			OccurredAt: episode.LastMessageAt})
	}
	return memorysearch.Result{Status: memorysearch.StatusOK,
		Reason: memorysearch.ReasonNone, Candidates: candidates}, nil
}

// relationTool wires the real registry with governance and, optionally, the
// relation reader.
func relationTool(t *testing.T, store *governedStore, governed bool,
	relations episodeassert.RelationReader) (tool.InvokableTool, *serviceagent.MemoryEvidenceRegistry) {
	t.Helper()
	config := DefaultMemorySearchConfig()
	config.MaxSourceQuotes, config.MaxSourceQuoteTokens = 0, 0
	registry, err := NewMemorySearchRegistry(relationSearcher{store: store}, nil, config)
	if err != nil {
		t.Fatal(err)
	}
	registry.UseClock(func() time.Time { return relationNow })
	registry.UseEpisodicOnly(true)
	if governed {
		governor, gerr := episodeassert.NewGovernor(store)
		if gerr != nil {
			t.Fatal(gerr)
		}
		registry.UseAssertionGovernance(governor)
	}
	if relations != nil {
		registry.UseAssertionRelations(relations)
	}
	evidence := serviceagent.NewMemoryEvidenceRegistry()
	tools, err := registry.Tools(context.Background(), serviceagent.RunContext{
		RunID: "run-rel", UserID: 7, UserName: "learner", SessionID: "current",
		Question: "超时策略现在是什么？", MemoryEvidence: evidence})
	if err != nil {
		t.Fatal(err)
	}
	for _, built := range tools {
		info, _ := built.Info(context.Background())
		if info != nil && info.Name == "memory_search" {
			return built.(tool.InvokableTool), evidence
		}
	}
	t.Fatal("memory_search was not built")
	return nil, nil
}

func runRelationSearch(t *testing.T, built tool.InvokableTool) (MemorySearchOutput, string) {
	t.Helper()
	raw, err := built.InvokableRun(context.Background(), `{"query":"超时策略","top_k":6}`)
	if err != nil {
		t.Fatalf("memory_search failed: %v", err)
	}
	var output MemorySearchOutput
	if err := json.Unmarshal([]byte(raw), &output); err != nil {
		t.Fatalf("unreadable observation: %v", err)
	}
	return output, raw
}

func cardByRef(t *testing.T, output MemorySearchOutput, ref string) MemorySearchResultCard {
	t.Helper()
	for _, card := range output.Results {
		if card.Ref == ref {
			return card
		}
	}
	refs := []string{}
	for _, card := range output.Results {
		refs = append(refs, card.Ref)
	}
	t.Fatalf("%s was not issued; refs = %v", ref, refs)
	return MemorySearchResultCard{}
}

// --- 1. a supplement reaches the model, naming the other slot ---

func TestRelationSupplementReachesTheModel(t *testing.T) {
	store, ids := relationStore(t)
	relations := &recordingRelations{rows: []model.EpisodeAssertionRelation{{
		UserID: 7, Kind: model.RelationSupplements,
		SourceAssertionID: ids["ep-a"], SourceEpisodeID: "ep-a",
		TargetAssertionID: ids["ep-b"], TargetEpisodeID: "ep-b",
		SourceKind: model.AssertionDecision, TargetKind: model.AssertionDecision,
		Applied: true}}}
	built, _ := relationTool(t, store, true, relations)
	output, _ := runRelationSearch(t, built)

	earlier := cardByRef(t, output, "E1.d1")
	later := cardByRef(t, output, "E2.d1")
	if len(earlier.Relations) != 1 {
		t.Fatalf("the earlier decision carries %d relations: %+v",
			len(earlier.Relations), earlier.Relations)
	}
	if earlier.Relations[0].Kind != string(model.RelationSupplements) {
		t.Fatalf("the earlier decision reports %q", earlier.Relations[0].Kind)
	}
	if earlier.Relations[0].Ref != "E2.d1" {
		t.Fatalf("the supplement points at %q, not the slot in this window",
			earlier.Relations[0].Ref)
	}
	if earlier.Relations[0].Note == "" {
		t.Fatal("the supplement carries no sentence the model can read")
	}
	// Both ends must say so. "This was added to" and "this adds to that" are
	// different sentences, and a reader given only one of them cannot tell
	// which decision is the later one.
	if len(later.Relations) != 1 || later.Relations[0].Ref != "E1.d1" {
		t.Fatalf("the later decision carries %+v", later.Relations)
	}
	if later.Relations[0].Note == earlier.Relations[0].Note {
		t.Fatalf("both ends rendered the same sentence %q, so the direction "+
			"is invisible", later.Relations[0].Note)
	}
	// Both ends are still current: a supplement does not supersede.
	if earlier.AssertionState != string(model.AssertionCurrent) ||
		later.AssertionState != string(model.AssertionCurrent) {
		t.Fatalf("a supplement changed a state: %q / %q",
			earlier.AssertionState, later.AssertionState)
	}
}

// --- 2. an equivalence reaches the model ---

func TestRelationEquivalentReachesTheModel(t *testing.T) {
	store, ids := relationStore(t)
	relations := &recordingRelations{rows: []model.EpisodeAssertionRelation{{
		UserID: 7, Kind: model.RelationEquivalent,
		SourceAssertionID: ids["ep-b"], SourceEpisodeID: "ep-b",
		TargetAssertionID: ids["ep-c"], TargetEpisodeID: "ep-c",
		SourceKind: model.AssertionDecision, TargetKind: model.AssertionDecision,
		Applied: true}}}
	built, _ := relationTool(t, store, true, relations)
	output, _ := runRelationSearch(t, built)

	card := cardByRef(t, output, "E2.d1")
	if len(card.Relations) != 1 ||
		card.Relations[0].Kind != string(model.RelationEquivalent) {
		t.Fatalf("the restated decision carries %+v", card.Relations)
	}
	if card.Relations[0].Ref != "E3.d1" {
		t.Fatalf("the equivalence points at %q", card.Relations[0].Ref)
	}
	if !strings.Contains(card.Relations[0].Note, "同") &&
		!strings.Contains(card.Relations[0].Note, "相同") {
		t.Fatalf("the equivalence note %q does not say the two records say "+
			"the same thing", card.Relations[0].Note)
	}
}

// --- 3. a replacement is not rendered twice ---

func TestRelationReplacementIsNotRenderedTwice(t *testing.T) {
	store := governedFixture()
	relations := &recordingRelations{rows: []model.EpisodeAssertionRelation{{
		UserID: 7, Kind: model.RelationReplaces,
		SourceAssertionID: governedID("ep-old|d1"), SourceEpisodeID: "ep-old",
		TargetAssertionID: governedID("ep-new|d1"), TargetEpisodeID: "ep-new",
		SourceKind: model.AssertionDecision, TargetKind: model.AssertionDecision,
		Applied: true}}}
	built, _ := relationTool(t, store, true, relations)
	output, _ := runRelationSearch(t, built)

	// The searcher orders by Episode id, so "ep-new" is card E1 and "ep-old"
	// is card E2. The card numbers follow retrieval order, not chronology.
	superseded := cardByRef(t, output, "E2.d1")
	if superseded.ReplacedBy != "E1.d1" || !superseded.HasReplacement {
		t.Fatalf("the existing replacement rendering changed: %+v", superseded)
	}
	// The replaced end already says it was replaced, and names the slot. A
	// relation note repeating it would present one fact as two.
	for _, relation := range superseded.Relations {
		if relation.Kind == string(model.RelationReplaces) {
			t.Fatalf("the replaced decision renders its replacement twice: "+
				"replaced_by=%s and relation %+v", superseded.ReplacedBy, relation)
		}
	}
	// The successor says nothing today, so naming what it replaced is new
	// information rather than a repetition.
	successor := cardByRef(t, output, "E1.d1")
	found := false
	for _, relation := range successor.Relations {
		if relation.Kind == string(model.RelationReplaces) && relation.Ref == "E2.d1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the successor does not name what it replaced: %+v",
			successor.Relations)
	}
}

// --- 4. a target outside the window is stated, never cited ---

func TestRelationOutsideTheWindowIsNotCited(t *testing.T) {
	store, ids := relationStore(t)
	outside := governedID("ep-not-in-window|d1")
	relations := &recordingRelations{rows: []model.EpisodeAssertionRelation{{
		UserID: 7, Kind: model.RelationSupplements,
		SourceAssertionID: ids["ep-a"], SourceEpisodeID: "ep-a",
		TargetAssertionID: outside, TargetEpisodeID: "ep-not-in-window",
		SourceKind: model.AssertionDecision, TargetKind: model.AssertionDecision,
		Applied: true}}}
	built, evidence := relationTool(t, store, true, relations)
	output, raw := runRelationSearch(t, built)

	card := cardByRef(t, output, "E1.d1")
	if len(card.Relations) != 1 {
		t.Fatalf("the decision carries %+v", card.Relations)
	}
	if card.Relations[0].Ref != "" {
		t.Fatalf("a target outside the window was given the citable ref %q",
			card.Relations[0].Ref)
	}
	if card.Relations[0].Note == "" {
		t.Fatal("the relation outside the window was dropped entirely; its " +
			"existence is information even when its target cannot be cited")
	}
	// Nothing the turn never issued may resolve as a citation.
	if _, err := evidence.Lookup(card.Relations[0].Ref); err == nil {
		t.Fatal("an empty reference resolved")
	}
	if strings.Contains(raw, outside) {
		t.Fatalf("the out-of-window assertion id reached the model payload")
	}
}

// --- 5. no raw internal identity reaches the model ---

func TestRelationPayloadCarriesNoInternalIdentity(t *testing.T) {
	store, ids := relationStore(t)
	relations := &recordingRelations{rows: []model.EpisodeAssertionRelation{
		{UserID: 7, Kind: model.RelationSupplements,
			SourceAssertionID: ids["ep-a"], SourceEpisodeID: "ep-a",
			TargetAssertionID: ids["ep-b"], TargetEpisodeID: "ep-b",
			SourceKind: model.AssertionDecision, TargetKind: model.AssertionDecision,
			ID: 4242, Applied: true},
		{UserID: 7, Kind: model.RelationEquivalent,
			SourceAssertionID: ids["ep-b"], SourceEpisodeID: "ep-b",
			TargetAssertionID: ids["ep-c"], TargetEpisodeID: "ep-c",
			SourceKind: model.AssertionDecision, TargetKind: model.AssertionDecision,
			ID: 4243, Applied: true}}}
	built, _ := relationTool(t, store, true, relations)
	_, raw := runRelationSearch(t, built)

	for name, needle := range map[string]string{
		"assertion id":     ids["ep-a"],
		"assertion id (b)": ids["ep-b"],
		"assertion id (c)": ids["ep-c"],
		"episode id":       "ep-a",
		"episode id (b)":   "ep-b",
		"database row id":  "4242",
	} {
		if strings.Contains(raw, needle) {
			t.Fatalf("the model-visible payload leaks the %s %q:\n%s",
				name, needle, raw)
		}
	}
	// Session identity must not travel either.
	if strings.Contains(raw, "sess-ep-a") {
		t.Fatalf("the payload leaks a session id:\n%s", raw)
	}
	// The payload this test just scanned is also the documented example of a
	// relation-aware observation. Writing it from the test rather than pasting
	// it into a document means the example cannot drift away from what the
	// tool actually emits.
	writeRelationSample(t, raw)
}

// writeRelationSample records the model-visible observation as evidence.
func writeRelationSample(t *testing.T, raw string) {
	t.Helper()
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, []byte(raw), "", "  "); err != nil {
		t.Fatalf("the observation is not valid JSON: %v", err)
	}
	path := filepath.Join("..", "..", "..", "exp033", "stabilize065", "repro",
		"phase1-1-1-model-visible-relations.json")
	if err := os.WriteFile(path, pretty.Bytes(), 0o644); err != nil {
		t.Logf("could not record the sample observation: %v", err)
	}
}

// --- 6. another learner's relation never surfaces ---

func TestRelationForeignOwnerIsDropped(t *testing.T) {
	store, ids := relationStore(t)
	relations := &recordingRelations{rows: []model.EpisodeAssertionRelation{{
		// A row that names this learner's assertions but belongs to someone
		// else. The reader is asked for one owner; a row for anyone else is a
		// cross-account read, and dropping it here is the second check.
		UserID: 8, Kind: model.RelationSupplements,
		SourceAssertionID: ids["ep-a"], SourceEpisodeID: "ep-a",
		TargetAssertionID: ids["ep-b"], TargetEpisodeID: "ep-b",
		SourceKind: model.AssertionDecision, TargetKind: model.AssertionDecision,
		Applied: true}}}
	built, _ := relationTool(t, store, true, relations)
	output, _ := runRelationSearch(t, built)
	for _, card := range output.Results {
		if len(card.Relations) != 0 {
			t.Fatalf("%s carries another learner's relation: %+v",
				card.Ref, card.Relations)
		}
	}
}

// --- 7. a relation store failure fails the call, it does not answer "none" ---

func TestRelationStoreFailureFailsClosed(t *testing.T) {
	store, _ := relationStore(t)
	relations := &recordingRelations{fail: true}
	built, _ := relationTool(t, store, true, relations)
	raw, err := built.InvokableRun(context.Background(), `{"query":"超时策略","top_k":6}`)
	if err == nil {
		t.Fatalf("an unreachable relation store returned a successful "+
			"observation:\n%s", raw)
	}
	// "No relations" and "the relation store is down" must not look alike: the
	// first says the learner's decisions stand alone, and the model would say
	// so.
	if strings.Contains(strings.ToLower(err.Error()), "no relation") {
		t.Fatalf("the failure was reported as an absence: %v", err)
	}
}

// --- 8. one bounded query per search, scoped to this window ---

func TestRelationQueryIsBatchedAndScoped(t *testing.T) {
	store, ids := relationStore(t)
	relations := &recordingRelations{}
	built, _ := relationTool(t, store, true, relations)
	output, _ := runRelationSearch(t, built)

	if len(output.Results) < 3 {
		t.Fatalf("the window carried %d slots; the batching claim needs more "+
			"than one", len(output.Results))
	}
	if relations.Calls() != 1 {
		t.Fatalf("a %d-slot window issued %d relation queries; one per slot is "+
			"the N+1 this batching exists to avoid", len(output.Results),
			relations.Calls())
	}
	asked := relations.LastAsked()
	if len(asked) == 0 {
		t.Fatal("the relation query named no assertions, so it asked for the " +
			"learner's entire history")
	}
	if len(asked) > len(output.Results) {
		t.Fatalf("the query asked about %d assertions for a %d-slot window",
			len(asked), len(output.Results))
	}
	inWindow := map[string]struct{}{}
	for _, id := range []string{ids["ep-a"], ids["ep-b"], ids["ep-c"]} {
		inWindow[id] = struct{}{}
	}
	for _, id := range asked {
		if _, ok := inWindow[id]; !ok {
			// Synopsis slots carry derived ids, which are legitimately part of
			// this window. Anything else would mean the scope leaked.
			if strings.HasPrefix(id, "lg_") || strings.HasPrefix(id, "ea_") {
				continue
			}
			t.Fatalf("the query asked about %q, which is not in this window", id)
		}
	}
}

// --- 9. the ungoverned observation is byte-identical ---

func TestRelationsDoNotTouchTheUngovernedObservation(t *testing.T) {
	store, ids := relationStore(t)
	rows := []model.EpisodeAssertionRelation{{
		UserID: 7, Kind: model.RelationSupplements,
		SourceAssertionID: ids["ep-a"], SourceEpisodeID: "ep-a",
		TargetAssertionID: ids["ep-b"], TargetEpisodeID: "ep-b",
		SourceKind: model.AssertionDecision, TargetKind: model.AssertionDecision,
		Applied: true}}

	plain, _ := relationTool(t, store, false, nil)
	_, baseline := runRelationSearch(t, plain)

	relations := &recordingRelations{rows: rows}
	withReader, _ := relationTool(t, store, false, relations)
	_, observed := runRelationSearch(t, withReader)

	if observed != baseline {
		t.Fatalf("attaching a relation reader changed an ungoverned "+
			"observation:\n baseline %s\n observed %s", baseline, observed)
	}
	if relations.Calls() != 0 {
		t.Fatalf("an ungoverned mode issued %d relation queries",
			relations.Calls())
	}
	if strings.Contains(baseline, "relations") {
		t.Fatalf("the ungoverned observation carries a relations field:\n%s",
			baseline)
	}
}

// --- 10. governance without a reader is unchanged ---

func TestGovernedSearchWithoutRelationReaderIsUnchanged(t *testing.T) {
	store, _ := relationStore(t)
	withoutReader, _ := relationTool(t, store, true, nil)
	_, baseline := runRelationSearch(t, withoutReader)

	empty := &recordingRelations{}
	withEmptyReader, _ := relationTool(t, store, true, empty)
	_, observed := runRelationSearch(t, withEmptyReader)

	// A reader that returns nothing must produce the same bytes as no reader
	// at all. Otherwise "this learner has no relations yet" and "this
	// deployment has no relation store" would be different observations for
	// the same absence.
	if observed != baseline {
		t.Fatalf("a reader with no rows changed the observation:\n "+
			"baseline %s\n observed %s", baseline, observed)
	}
}
