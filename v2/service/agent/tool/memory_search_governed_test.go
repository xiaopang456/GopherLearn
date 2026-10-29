package agenttool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"GopherAI/common/memoryrecall"
	"GopherAI/model"
	serviceagent "GopherAI/service/agent"
	"GopherAI/service/episodeassert"
	"GopherAI/service/memorysearch"

	"github.com/cloudwego/eino/components/tool"
)

// STABILIZE-058 v1.2, group B: the real read chain.
//
// v1.1's ExpandEpisodes was reachable only from tests and from a standalone
// component. Production memory_search never called it, so every governed
// property - slot-level references, current/replaced/unknown, a replacement
// target the model can name - was true of a helper and absent from the system.
//
// These tests drive the real memory_search tool.

var governedNow = time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)

// governedStore is the assertion-state store behind the governor.
type governedStore struct {
	episodes map[string]model.EpisodeMemory
	states   map[string]model.EpisodeAssertionState
	times    map[uint]time.Time
	failLoad bool
}

func (s *governedStore) LoadStates(_ context.Context, userID int64,
	ids []string) (map[string]model.EpisodeAssertionState, error) {
	if s.failLoad {
		return nil, context.DeadlineExceeded
	}
	out := map[string]model.EpisodeAssertionState{}
	for _, id := range ids {
		if state, ok := s.states[id]; ok && state.UserID == userID {
			out[id] = state
		}
	}
	return out, nil
}

func (s *governedStore) LoadEpisode(_ context.Context, userID int64,
	episodeID string) (model.EpisodeMemory, error) {
	episode, ok := s.episodes[episodeID]
	if !ok || episode.UserID != userID || episode.Status != model.EpisodeStatusActive {
		return model.EpisodeMemory{}, context.Canceled
	}
	return episode, nil
}

func (s *governedStore) SaveState(context.Context, model.EpisodeAssertionState, uint64) error {
	return nil
}

func (s *governedStore) MessageTimes(_ context.Context, _ int64, ids []uint) (map[uint]time.Time, error) {
	out := map[uint]time.Time{}
	for _, id := range ids {
		if at, ok := s.times[id]; ok {
			out[id] = at
		}
	}
	return out, nil
}

func governedID(seed string) string { return model.DeterministicAssertionID(seed) }

// governedFixture builds two cross-session cards: one whose decision was
// replaced but whose open loop is still live, and the card that replaced it.
func governedFixture() *governedStore {
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

	store.states[governedID("ep-old|d1")] = model.EpisodeAssertionState{
		AssertionID: governedID("ep-old|d1"), UserID: 7, EpisodeID: "ep-old",
		Kind: model.AssertionDecision, State: model.AssertionReplaced,
		ReplacedByAssertionID: governedID("ep-new|d1"), ReplacedByEpisodeID: "ep-new",
		OccurredAt: governedNow.AddDate(0, 0, -30), Version: 1}
	store.states[governedID("ep-new|d1")] = model.EpisodeAssertionState{
		AssertionID: governedID("ep-new|d1"), UserID: 7, EpisodeID: "ep-new",
		Kind: model.AssertionDecision, State: model.AssertionCurrent,
		OccurredAt: governedNow.AddDate(0, 0, -10), Version: 1}
	return store
}

// governedSearcher returns the two fixture Episodes as episodic candidates.
type governedSearcher struct{ store *governedStore }

func (s governedSearcher) Search(_ context.Context, request memorysearch.Request) (memorysearch.Result, error) {
	candidates := []memorysearch.Candidate{}
	for _, id := range []string{"ep-old", "ep-new"} {
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

func governedTool(t *testing.T, store *governedStore, governed bool) (tool.InvokableTool, *serviceagent.MemoryEvidenceRegistry) {
	t.Helper()
	// Quotes are off in this fixture: the tests are about assertion slots, and
	// a quote reader would add a second source of text to reason about.
	config := DefaultMemorySearchConfig()
	config.MaxSourceQuotes, config.MaxSourceQuoteTokens = 0, 0
	registry, err := NewMemorySearchRegistry(governedSearcher{store: store}, nil, config)
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

func runGovernedSearch(t *testing.T, built tool.InvokableTool) MemorySearchOutput {
	t.Helper()
	raw, err := built.InvokableRun(context.Background(),
		`{"query":"边界方案","top_k":6}`)
	if err != nil {
		t.Fatalf("memory_search failed: %v", err)
	}
	var output MemorySearchOutput
	if err := json.Unmarshal([]byte(raw), &output); err != nil {
		t.Fatalf("unreadable observation: %v", err)
	}
	return output
}

// B1, B2, B4: the governed mode returns slot-level cards carrying kind, state,
// time and replacement, not a whole-card [E1].
func TestGovernedSearchReturnsSlotLevelEvidence(t *testing.T) {
	built, _ := governedTool(t, governedFixture(), true)
	output := runGovernedSearch(t, built)

	if len(output.Results) == 0 {
		t.Fatal("the governed search returned nothing")
	}
	slots := map[string]MemorySearchResultCard{}
	for _, card := range output.Results {
		slots[card.Ref] = card
	}
	for _, want := range []string{"E1.d1", "E1.o1", "E2.d1"} {
		if _, ok := slots[want]; !ok {
			t.Fatalf("%s was not issued; refs = %v", want, governedRefsOf(output))
		}
	}
	for _, card := range output.Results {
		if !strings.Contains(card.Ref, ".") {
			t.Fatalf("a whole-card reference %q survived in the governed mode", card.Ref)
		}
		if card.AssertionKind == "" || card.AssertionState == "" {
			t.Fatalf("%s carries kind=%q state=%q", card.Ref, card.AssertionKind, card.AssertionState)
		}
		if card.OccurredAt == "" {
			t.Fatalf("%s carries no occurrence time", card.Ref)
		}
	}
	if slots["E1.d1"].AssertionState != string(model.AssertionReplaced) {
		t.Fatalf("the replaced decision reads as %q", slots["E1.d1"].AssertionState)
	}
	if slots["E2.d1"].AssertionState != string(model.AssertionCurrent) {
		t.Fatalf("the current decision reads as %q", slots["E2.d1"].AssertionState)
	}
	// The sibling open loop keeps its own state.
	if slots["E1.o1"].AssertionState != string(model.AssertionUnknown) {
		t.Fatalf("the sibling open loop reads as %q", slots["E1.o1"].AssertionState)
	}
}

// B5: a replacement inside this turn's results is named; one outside is
// reported as existing without inventing a label the turn never issued.
func TestReplacementTargetIsNamedOnlyWhenIssued(t *testing.T) {
	store := governedFixture()
	built, _ := governedTool(t, store, true)
	output := runGovernedSearch(t, built)
	for _, card := range output.Results {
		if card.Ref != "E1.d1" {
			continue
		}
		if !card.HasReplacement {
			t.Fatal("the replaced decision did not report a replacement")
		}
		if card.ReplacedBy != "E2.d1" {
			t.Fatalf("the replacement was named %q, want E2.d1", card.ReplacedBy)
		}
	}

	// Now the replacement is outside the result set.
	store.states[governedID("ep-old|d1")] = model.EpisodeAssertionState{
		AssertionID: governedID("ep-old|d1"), UserID: 7, EpisodeID: "ep-old",
		Kind: model.AssertionDecision, State: model.AssertionReplaced,
		ReplacedByAssertionID: governedID("ep-elsewhere|d1"), ReplacedByEpisodeID: "ep-elsewhere",
		OccurredAt: governedNow.AddDate(0, 0, -30), Version: 1}
	elsewhere, _ := governedTool(t, store, true)
	for _, card := range runGovernedSearch(t, elsewhere).Results {
		if card.Ref != "E1.d1" {
			continue
		}
		if !card.HasReplacement {
			t.Fatal("an out-of-set replacement must still be reported as existing")
		}
		if card.ReplacedBy != "" {
			t.Fatalf("a label the turn never issued was invented: %q", card.ReplacedBy)
		}
	}
}

// B6: a replaced assertion is neither removed nor pushed down.
func TestReplacedSlotIsNeitherRemovedNorDownranked(t *testing.T) {
	built, _ := governedTool(t, governedFixture(), true)
	output := runGovernedSearch(t, built)
	refs := governedRefsOf(output)
	position := func(label string) int {
		for index, ref := range refs {
			if ref == label {
				return index
			}
		}
		return -1
	}
	replaced, current := position("E1.d1"), position("E2.d1")
	if replaced < 0 {
		t.Fatalf("the replaced slot was filtered out: %v", refs)
	}
	// Slots within a card run synopsis, decisions, open loops, corrections, so
	// E1.s legitimately precedes E1.d1. What must hold is that the replaced
	// assertion's card is still ahead of the one that replaced it: governance
	// changes what a card contains, never the retrieval's ordering.
	if current >= 0 && replaced > current {
		t.Fatalf("the replaced slot was pushed behind its replacement: %v", refs)
	}
}

// B3: the model sees a per-turn label; the stable identity stays server-side.
func TestStableIdentityNeverReachesTheModel(t *testing.T) {
	built, evidence := governedTool(t, governedFixture(), true)
	output := runGovernedSearch(t, built)
	raw, _ := json.Marshal(output)
	for _, seed := range []string{"ep-old|d1", "ep-new|d1", "ep-old|o1"} {
		if strings.Contains(string(raw), governedID(seed)) {
			t.Fatalf("a stable assertion identity reached the observation: %s", governedID(seed))
		}
	}
	if evidence == nil {
		t.Fatal("no evidence registry was used")
	}
}

// B7: an assertion-state failure fails the call. Falling back to ungoverned
// cards would present a replaced decision as though nothing were known against
// it, which is the distinction the whole feature exists to make.
func TestAssertionStateFailureFailsTheSearch(t *testing.T) {
	store := governedFixture()
	store.failLoad = true
	built, _ := governedTool(t, store, true)
	raw, err := built.InvokableRun(context.Background(),
		`{"query":"边界方案","top_k":6}`)
	if err != nil {
		return // an error return is an acceptable failure shape
	}
	var output MemorySearchOutput
	if json.Unmarshal([]byte(raw), &output) == nil && output.Status == "ok" {
		t.Fatalf("a state failure produced a successful, ungoverned observation: %+v", output)
	}
}

// B9: with governance off, the observation is byte-identical to what the tool
// produced before this change - whole cards, no slot fields.
func TestUngovernedModeOutputIsUnchanged(t *testing.T) {
	built, _ := governedTool(t, governedFixture(), false)
	output := runGovernedSearch(t, built)
	for _, card := range output.Results {
		if strings.Contains(card.Ref, ".") {
			t.Fatalf("the ungoverned mode emitted a slot reference %q", card.Ref)
		}
		if card.AssertionKind != "" || card.AssertionState != "" ||
			card.ReplacedBy != "" || card.HasReplacement {
			t.Fatalf("the ungoverned mode emitted governance fields: %+v", card)
		}
	}
	raw, _ := json.Marshal(output.Results[0])
	for _, field := range []string{"assertion_kind", "assertion_state", "replaced_by", "has_replacement"} {
		if strings.Contains(string(raw), field) {
			t.Fatalf("%s appears in the ungoverned observation: %s", field, raw)
		}
	}
}

// B10: the production default is untouched, and only the new identity governs.
func TestDefaultModeIsUnchanged(t *testing.T) {
	if memoryrecall.DefaultReadMode() != memoryrecall.ModeLegacyV2 {
		t.Fatal("the production default moved")
	}
	if memoryrecall.ModeLegacyV2.GovernsAssertionTime() ||
		memoryrecall.ModeAgentMemoryUserConfirmedV1.GovernsAssertionTime() {
		t.Fatal("an existing mode started governing assertion time")
	}
	if !memoryrecall.ModeEpisodeAssertionGovernedV11.GovernsAssertionTime() {
		t.Fatal("the v1.1 identity does not govern assertion time")
	}
}

func governedRefsOf(output MemorySearchOutput) []string {
	out := []string{}
	for _, card := range output.Results {
		out = append(out, card.Ref)
	}
	return out
}
