package agenttool

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"GopherAI/model"
	serviceagent "GopherAI/service/agent"
	"GopherAI/service/episodeassert"
	"GopherAI/service/memorysearch"

	"github.com/cloudwego/eino/components/tool"
)

// BUG-AGENT-217 (red first): two governed searches in one turn share one
// MemoryEvidenceRegistry, but the governor numbers its cards by each search's
// own candidate order. A second search whose ranking reorders the episodes
// issues E1 for an episode the first search had already issued as E2, and the
// turn-shared registry refuses the rebinding - tool_infrastructure with the
// governed_expansion component, exactly the v1.4.1/v1.4.2 signature.
//
// The tests pin the turn-stable rule: once a turn has issued a label for an
// episode's assertion, that label is the episode's label for the whole turn,
// whatever order a later search returns. A genuinely new episode gets a fresh
// number and never takes a used one; a real collision - two different
// assertions claiming one label - is still refused.

// reorderSearcher serves scripted candidate orders per query, the way two real
// queries with different vectors rank the same episodes differently.
type reorderSearcher struct {
	store  *governedStore
	orders map[string][]string
}

func (s reorderSearcher) Search(_ context.Context, request memorysearch.Request) (memorysearch.Result, error) {
	order, ok := s.orders[request.Query]
	if !ok {
		order = []string{"ep-old", "ep-new", "ep-third"}
	}
	candidates := []memorysearch.Candidate{}
	for _, id := range order {
		episode := s.store.episodes[id]
		candidates = append(candidates, memorysearch.Candidate{
			Kind: memorysearch.KindEpisodic, ID: episode.ID, Version: episode.Version,
			Subject: episode.Title, Content: memorysearch.RenderEpisodeContent(episode),
			OccurredAt: episode.LastMessageAt,
		})
	}
	return memorysearch.Result{Status: memorysearch.StatusOK, Reason: memorysearch.ReasonNone,
		Candidates: candidates}, nil
}

// reorderFixture builds three active cards: ep-old's decision was replaced by
// ep-new's, ep-third stands alone, and ep-old also carries a live open loop so
// two slot kinds can be checked across a reorder.
func reorderFixture() *governedStore {
	store := &governedStore{episodes: map[string]model.EpisodeMemory{},
		states: map[string]model.EpisodeAssertionState{}, times: map[uint]time.Time{}}

	card := func(id string, daysAgo int, decision, openLoop string, decisionMessage, loopMessage uint) model.EpisodeMemory {
		at := governedNow.AddDate(0, 0, -daysAgo)
		store.times[decisionMessage] = at
		document := model.EpisodeDocument{
			Synopsis: "概述 " + id, SourceHandles: []string{"h:" + id},
			Decisions: []model.EpisodeEvidence{{ID: governedID(id + "|d1"), Text: decision,
				EvidenceMessageIDs: []uint{decisionMessage}, OccurredAt: at}},
		}
		if openLoop != "" {
			store.times[loopMessage] = at.Add(time.Hour)
			document.OpenLoops = []model.EpisodeEvidence{{ID: governedID(id + "|o1"), Text: openLoop,
				EvidenceMessageIDs: []uint{loopMessage}, OccurredAt: at.Add(time.Hour)}}
		}
		episode := model.EpisodeMemory{ID: id, UserID: 7, SessionID: "sess-" + id,
			Title: id, SearchText: decision, Document: document,
			Status: model.EpisodeStatusActive, IndexStatus: model.EpisodeIndexIndexed,
			Version: 1, LastMessageAt: at}
		store.episodes[id] = episode
		return episode
	}
	card("ep-old", 30, "用分隔符切分", "半包还没写", 10, 11)
	card("ep-new", 10, "改回固定头部", "", 20, 0)
	card("ep-third", 5, "第三张卡的结论", "", 30, 0)

	store.states[governedID("ep-old|d1")] = model.EpisodeAssertionState{
		AssertionID: governedID("ep-old|d1"), UserID: 7, EpisodeID: "ep-old",
		Kind: model.AssertionDecision, State: model.AssertionReplaced,
		ReplacedByAssertionID: governedID("ep-new|d1"), ReplacedByEpisodeID: "ep-new",
		OccurredAt: governedNow.AddDate(0, 0, -30), Version: 1}
	store.states[governedID("ep-new|d1")] = model.EpisodeAssertionState{
		AssertionID: governedID("ep-new|d1"), UserID: 7, EpisodeID: "ep-new",
		Kind: model.AssertionDecision, State: model.AssertionCurrent,
		OccurredAt: governedNow.AddDate(0, 0, -10), Version: 1}
	store.states[governedID("ep-third|d1")] = model.EpisodeAssertionState{
		AssertionID: governedID("ep-third|d1"), UserID: 7, EpisodeID: "ep-third",
		Kind: model.AssertionDecision, State: model.AssertionCurrent,
		OccurredAt: governedNow.AddDate(0, 0, -5), Version: 1}
	return store
}

func reorderTool(t *testing.T, store *governedStore, orders map[string][]string,
	governed bool) (tool.InvokableTool, *serviceagent.MemoryEvidenceRegistry) {
	t.Helper()
	config := DefaultMemorySearchConfig()
	config.MaxSourceQuotes, config.MaxSourceQuoteTokens = 0, 0
	registry, err := NewMemorySearchRegistry(reorderSearcher{store: store, orders: orders},
		nil, config)
	if err != nil {
		t.Fatal(err)
	}
	registry.UseClock(func() time.Time { return governedNow })
	registry.UseEpisodicOnly(true)
	if governed {
		governor, err := episodeassert.NewGovernor(store)
		if err != nil {
			t.Fatal(err)
		}
		registry.UseAssertionGovernance(governor)
	}
	evidence := serviceagent.NewMemoryEvidenceRegistry()
	tools, err := registry.Tools(context.Background(), serviceagent.RunContext{
		RunID: "run-1", UserID: 7, UserName: "learner", SessionID: "current",
		Question: "我现在用的边界方案是哪个？", MemoryEvidence: evidence,
	})
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

func runReorderSearch(t *testing.T, built tool.InvokableTool, query string) MemorySearchOutput {
	t.Helper()
	raw, err := built.InvokableRun(context.Background(),
		`{"query":"`+query+`","top_k":6}`)
	if err != nil {
		t.Fatalf("memory_search failed: %v", err)
	}
	var output MemorySearchOutput
	if err := json.Unmarshal([]byte(raw), &output); err != nil {
		t.Fatalf("unreadable observation: %v", err)
	}
	return output
}

func slotRefsOf(output MemorySearchOutput) []string {
	refs := []string{}
	for _, card := range output.Results {
		refs = append(refs, card.Ref)
	}
	return refs
}

func assertRefBoundTo(t *testing.T, evidence *serviceagent.MemoryEvidenceRegistry,
	ref, wantAssertionID string) {
	t.Helper()
	entry, err := evidence.Lookup(ref)
	if err != nil {
		t.Fatalf("%s is not in the turn registry: %v", ref, err)
	}
	if entry.InternalID != wantAssertionID {
		t.Fatalf("%s is bound to %s, want %s", ref, entry.InternalID, wantAssertionID)
	}
}

// The red test: a second search whose ranking reverses the first search's
// episode order must succeed, and every episode must keep the label the turn
// already gave it.
func TestSecondSearchReorderKeepsTurnStableLabels(t *testing.T) {
	built, evidence := reorderTool(t, reorderFixture(), map[string][]string{
		"角度一": {"ep-old", "ep-new"},
		"角度二": {"ep-new", "ep-old"},
	}, true)

	runReorderSearch(t, built, "角度一")
	assertRefBoundTo(t, evidence, "E1.d1", governedID("ep-old|d1"))
	assertRefBoundTo(t, evidence, "E2.d1", governedID("ep-new|d1"))

	second := runReorderSearch(t, built, "角度二")
	// The second observation lists ep-new first, under ep-new's own turn label,
	// then ep-old under its. Nothing is renumbered by the new ranking.
	assertRefBoundTo(t, evidence, "E1.d1", governedID("ep-old|d1"))
	assertRefBoundTo(t, evidence, "E1.o1", governedID("ep-old|o1"))
	assertRefBoundTo(t, evidence, "E2.d1", governedID("ep-new|d1"))
	want := map[string]bool{"E2.s": true, "E2.d1": true,
		"E1.s": true, "E1.d1": true, "E1.o1": true}
	for _, ref := range slotRefsOf(second) {
		if !want[ref] {
			t.Fatalf("the reordered second search minted %s; labels must be turn-stable", ref)
		}
		delete(want, ref)
	}
	for ref := range want {
		t.Fatalf("the reordered second search lost %s", ref)
	}
}

// A second search that surfaces an episode the turn has never seen gives it a
// fresh number; it must not take E1, which the turn already spent on ep-old.
func TestSecondSearchNewEpisodeNeverReusesALabel(t *testing.T) {
	built, evidence := reorderTool(t, reorderFixture(), map[string][]string{
		"角度一": {"ep-old"},
		"角度二": {"ep-new"},
	}, true)

	runReorderSearch(t, built, "角度一")
	assertRefBoundTo(t, evidence, "E1.d1", governedID("ep-old|d1"))

	second := runReorderSearch(t, built, "角度二")
	refs := slotRefsOf(second)
	if len(refs) != 2 || refs[0] != "E2.s" || refs[1] != "E2.d1" {
		t.Fatalf("a new episode was issued %v; want a fresh [E2.s E2.d1] only", refs)
	}
	assertRefBoundTo(t, evidence, "E2.s",
		model.LegacyAssertionID("ep-new", model.AssertionSynopsis, nil, "概述 ep-new"))
	assertRefBoundTo(t, evidence, "E2.d1", governedID("ep-new|d1"))
}

// [old,new] then [new,old,third]: the two existing cards keep their numbers and
// the new card is numbered after everything the turn has issued.
func TestReorderWithNewEpisodeKeepsExistingNumbers(t *testing.T) {
	built, evidence := reorderTool(t, reorderFixture(), map[string][]string{
		"角度一": {"ep-old", "ep-new"},
		"角度二": {"ep-new", "ep-old", "ep-third"},
	}, true)

	runReorderSearch(t, built, "角度一")
	second := runReorderSearch(t, built, "角度二")
	assertRefBoundTo(t, evidence, "E3.s",
		model.LegacyAssertionID("ep-third", model.AssertionSynopsis, nil, "概述 ep-third"))
	assertRefBoundTo(t, evidence, "E3.d1", governedID("ep-third|d1"))
	want := []string{"E2.s", "E2.d1", "E1.s", "E1.d1", "E1.o1", "E3.s", "E3.d1"}
	got := slotRefsOf(second)
	if len(got) != len(want) {
		t.Fatalf("the reordered set is %v, want %v", got, want)
	}
	for i, ref := range want {
		if got[i] != ref {
			t.Fatalf("slot %d is %s, want %s", i, got[i], ref)
		}
	}
}

// The legitimate-reuse case BUG-AGENT-215 already covered, one step closer to
// production: the tool refuses an identical query, so a second search always
// arrives with different words - here returning the same episodes in the same
// order. Same labels, no error, no duplicate entries.
func TestRepeatedSearchReusesReferences(t *testing.T) {
	built, evidence := reorderTool(t, reorderFixture(), map[string][]string{
		"角度一": {"ep-old", "ep-new"},
		"角度二": {"ep-old", "ep-new"},
	}, true)

	first := runReorderSearch(t, built, "角度一")
	before := len(evidence.Entries())
	second := runReorderSearch(t, built, "角度二")
	if len(slotRefsOf(first)) == 0 || len(slotRefsOf(first)) != len(slotRefsOf(second)) {
		t.Fatalf("the repeated search changed the slot set: %v (status=%s reason=%s) then %v (status=%s reason=%s)",
			slotRefsOf(first), first.Status, first.Reason,
			slotRefsOf(second), second.Status, second.Reason)
	}
	for i, ref := range slotRefsOf(second) {
		if ref != slotRefsOf(first)[i] {
			t.Fatalf("slot %d moved from %s to %s", i, slotRefsOf(first)[i], ref)
		}
	}
	if len(evidence.Entries()) != before {
		t.Fatalf("a repeated search added entries: %d -> %d", before, len(evidence.Entries()))
	}
}

// Slot suffixes are part of the label: a decision stays .d and an open loop
// stays .o across a reorder, under the episode's own turn card number.
func TestSlotSuffixesSurviveReorder(t *testing.T) {
	built, evidence := reorderTool(t, reorderFixture(), map[string][]string{
		"角度一": {"ep-old", "ep-new"},
		"角度二": {"ep-new", "ep-old"},
	}, true)

	runReorderSearch(t, built, "角度一")
	second := runReorderSearch(t, built, "角度二")
	seen := map[string]string{}
	for _, card := range second.Results {
		if card.Ref == "E1.d1" || card.Ref == "E1.o1" {
			seen[card.AssertionKind] = card.Ref
		}
	}
	if seen["decision"] != "E1.d1" {
		t.Fatalf("the decision slot moved: %v", seen)
	}
	if seen["open_loop"] != "E1.o1" {
		t.Fatalf("the open-loop slot moved: %v", seen)
	}
	assertRefBoundTo(t, evidence, "E1.o1", governedID("ep-old|o1"))
}

// The replacement target is named with its turn-stable label: in the reordered
// search ep-new arrives first and the governor would locally call it E1, but
// the turn already knows it as E2, and that is what the model must read.
func TestReplacementLabelIsTurnStable(t *testing.T) {
	built, _ := reorderTool(t, reorderFixture(), map[string][]string{
		"角度一": {"ep-old", "ep-new"},
		"角度二": {"ep-new", "ep-old"},
	}, true)

	runReorderSearch(t, built, "角度一")
	second := runReorderSearch(t, built, "角度二")
	for _, card := range second.Results {
		if card.Ref != "E1.d1" {
			continue
		}
		if !card.HasReplacement {
			t.Fatal("the replaced decision lost its replacement flag")
		}
		if card.ReplacedBy != "E2.d1" {
			t.Fatalf("the replacement target read %q, want the turn-stable E2.d1",
				card.ReplacedBy)
		}
	}
}

// The legacy (non-governed) path auto-numbers whole cards and has no shared
// label space to collide with; its behaviour is untouched by the fix.
func TestLegacyModeTwoSearchesUnchanged(t *testing.T) {
	built, _ := reorderTool(t, reorderFixture(), map[string][]string{
		"角度一": {"ep-old", "ep-new"},
		"角度二": {"ep-new", "ep-old"},
	}, false)

	first := runReorderSearch(t, built, "角度一")
	second := runReorderSearch(t, built, "角度二")
	if len(first.Results) != 2 || first.Results[0].Ref != "E1" || first.Results[1].Ref != "E2" {
		t.Fatalf("the first legacy search changed shape: %v", slotRefsOf(first))
	}
	if len(second.Results) != 2 || second.Results[0].Ref != "E3" || second.Results[1].Ref != "E4" {
		t.Fatalf("the second legacy search changed shape: %v", slotRefsOf(second))
	}
}

// concurrentReorderSearcher holds both real search calls at the store boundary
// before releasing them together. The two tool invocations therefore enter the
// same turn's governed expansion concurrently, with opposite ranking orders.
type concurrentReorderSearcher struct {
	store   *governedStore
	ready   chan struct{}
	release chan struct{}
}

func (s *concurrentReorderSearcher) Search(ctx context.Context, request memorysearch.Request) (memorysearch.Result, error) {
	select {
	case s.ready <- struct{}{}:
	case <-ctx.Done():
		return memorysearch.Result{}, ctx.Err()
	}
	select {
	case <-s.release:
	case <-ctx.Done():
		return memorysearch.Result{}, ctx.Err()
	}
	order := []string{"ep-old", "ep-new"}
	if request.Query == "角度二" {
		order = []string{"ep-new", "ep-old"}
	}
	candidates := make([]memorysearch.Candidate, 0, len(order))
	for _, id := range order {
		episode := s.store.episodes[id]
		candidates = append(candidates, memorysearch.Candidate{
			Kind: memorysearch.KindEpisodic, ID: episode.ID, Version: episode.Version,
			Subject: episode.Title, Content: memorysearch.RenderEpisodeContent(episode),
			OccurredAt: episode.LastMessageAt,
		})
	}
	return memorysearch.Result{Status: memorysearch.StatusOK, Reason: memorysearch.ReasonNone,
		Candidates: candidates}, nil
}

func concurrentReorderTool(t *testing.T, store *governedStore,
	searcher *concurrentReorderSearcher) (tool.InvokableTool, *serviceagent.MemoryEvidenceRegistry) {
	t.Helper()
	config := DefaultMemorySearchConfig()
	config.MaxSourceQuotes, config.MaxSourceQuoteTokens = 0, 0
	registry, err := NewMemorySearchRegistry(searcher, nil, config)
	if err != nil {
		t.Fatal(err)
	}
	registry.UseClock(func() time.Time { return governedNow })
	registry.UseEpisodicOnly(true)
	governor, err := episodeassert.NewGovernor(store)
	if err != nil {
		t.Fatal(err)
	}
	registry.UseAssertionGovernance(governor)
	evidence := serviceagent.NewMemoryEvidenceRegistry()
	tools, err := registry.Tools(context.Background(), serviceagent.RunContext{
		RunID: "run-concurrent", UserID: 7, UserName: "learner", SessionID: "current",
		Question: "并发重排验证", MemoryEvidence: evidence,
	})
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

func TestConcurrentGovernedReorderUsesOneCardPerEpisode(t *testing.T) {
	store := reorderFixture()
	searcher := &concurrentReorderSearcher{store: store, ready: make(chan struct{}, 2), release: make(chan struct{})}
	built, evidence := concurrentReorderTool(t, store, searcher)
	outputs := make(chan MemorySearchOutput, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, query := range []string{"角度一", "角度二"} {
		query := query
		wg.Add(1)
		go func() {
			defer wg.Done()
			raw, err := built.InvokableRun(context.Background(),
				`{"query":"`+query+`","top_k":6}`)
			if err != nil {
				errs <- err
				return
			}
			var output MemorySearchOutput
			if err := json.Unmarshal([]byte(raw), &output); err != nil {
				errs <- err
				return
			}
			outputs <- output
		}()
	}
	<-searcher.ready
	<-searcher.ready
	close(searcher.release)
	wg.Wait()
	close(errs)
	close(outputs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent governed search failed: %v", err)
		}
	}
	if len(evidence.Entries()) != 5 {
		t.Fatalf("two overlapping searches should have five unique slot entries, got %d", len(evidence.Entries()))
	}
	aCard, aOK := evidence.EpisodeCard("ep-old", 1)
	bCard, bOK := evidence.EpisodeCard("ep-new", 1)
	if !aOK || !bOK || aCard == bCard {
		t.Fatalf("Episode cards are not unique and stable: %q/%q", aCard, bCard)
	}
	for _, output := range []MemorySearchOutput{<-outputs, <-outputs} {
		for _, card := range output.Results {
			if card.Ref != aCard+".s" && card.Ref != aCard+".d1" &&
				card.Ref != aCard+".o1" && card.Ref != bCard+".s" && card.Ref != bCard+".d1" {
				t.Fatalf("output carried an unregistered or drifting ref %q", card.Ref)
			}
		}
	}
}
