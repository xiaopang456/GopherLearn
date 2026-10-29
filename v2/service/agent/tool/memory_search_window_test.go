package agenttool

import (
	"strings"
	"testing"
	"time"

	"GopherAI/common/memoryrecall"
	"GopherAI/common/rag"
	episodedao "GopherAI/dao/episode"
	memorydao "GopherAI/dao/memory"
	"GopherAI/model"
	"GopherAI/service/memorysearch"
)

// body returns a body of roughly the requested token cost, so a test can state
// a budget in terms of cards rather than in terms of a magic number.
func body(label string, words int) string {
	return label + " " + strings.TrimSpace(strings.Repeat("alpha ", words))
}

// mixedFixture gives the searcher four eligible records of each kind, so the
// final merge has something to squeeze out.
func mixedFixture(h *toolHarness, semanticWords, episodeWords int) {
	rows := []model.LongTermMemory{}
	hits := []memorydao.MemorySearchResult{}
	for index, id := range []string{"s1", "s2", "s3", "s4"} {
		rows = append(rows, model.LongTermMemory{
			ID: id, UserID: 1, Scope: model.MemoryScopeTopic, Subject: "subject " + id,
			Content: body("semantic "+id, semanticWords), TopicKey: "topic", Confidence: 0.9,
			Status: model.LongTermMemoryActive, UpdatedAt: toolClock.Add(-time.Hour),
		})
		hits = append(hits, memorydao.MemorySearchResult{MemoryID: id, Rank: index})
	}
	h.memoryLoader.rows = rows
	h.memoryIndex.vector = hits

	episodes := []model.EpisodeMemory{}
	episodeHits := []episodedao.EpisodeSearchResult{}
	for index, id := range []string{"e1", "e2", "e3", "e4"} {
		episodes = append(episodes, model.EpisodeMemory{
			ID: id, UserID: 1, SessionID: "past-" + id, Title: "title " + id, Version: 2,
			Status: model.EpisodeStatusActive, IndexStatus: model.EpisodeIndexIndexed,
			LastMessageAt: toolClock.Add(-72 * time.Hour),
			Document: model.EpisodeDocument{
				Synopsis: body("episode "+id, episodeWords),
				Decisions: []model.EpisodeEvidence{
					{Text: "decision " + id, EvidenceMessageIDs: []uint{1}},
				},
			},
		})
		episodeHits = append(episodeHits, episodedao.EpisodeSearchResult{
			EpisodeID: id, Rank: index, EmbeddingVersion: episodedao.CurrentEpisodeEmbeddingVersion,
		})
	}
	h.episodeStore.rows = episodes
	h.episodeIndex.vector = episodeHits
}

// windowHarness is newToolHarness with a chosen window policy.
func windowHarness(t *testing.T, policy memoryrecall.WindowPolicy, mutate func(*toolHarness)) *toolHarness {
	t.Helper()
	h := newToolHarness(t, mutate)
	config := DefaultMemorySearchConfig()
	config.WindowPolicy = policy
	searcher, err := memorysearch.NewSearcher(h.memoryIndex, h.memoryLoader, h.episodeIndex,
		h.episodeStore, h.embedder, memorysearch.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	registry, err := NewMemorySearchRegistry(searcher, h.messages, config)
	if err != nil {
		t.Fatal(err)
	}
	registry.UseClock(func() time.Time { return toolClock })
	h.registry = registry
	return h
}

func refsOf(output MemorySearchOutput) []string {
	out := []string{}
	for _, card := range output.Results {
		out = append(out, card.Ref)
	}
	return out
}

// The shipped policy is unchanged by anything STABILIZE-036 added. The reserve
// is computed on every mixed call, so this is the test that says computing it
// is not the same as using it.
func TestFixedPolicyIgnoresTheReserve(t *testing.T) {
	h := windowHarness(t, WindowPolicyFixed, func(h *toolHarness) {})
	mixedFixture(h, 4, 4)
	invokable, _ := h.build(t)
	output := call(t, invokable, `{"query":"what did I decide about pooling"}`)
	if output.Status != statusOK {
		t.Fatalf("status = %s", output.Status)
	}
	if len(output.Results) != 5 {
		t.Fatalf("cards = %d, want the merged window of five", len(output.Results))
	}
}

// The widened window appends whole cards after the base window, leaves the base
// window's cards and their order alone, and continues the run's ref numbering.
func TestTokenBoundedPolicyAppendsAfterTheBaseWindow(t *testing.T) {
	base := windowHarness(t, WindowPolicyFixed, nil)
	mixedFixture(base, 4, 4)
	baseTool, _ := base.build(t)
	baseline := call(t, baseTool, `{"query":"what did I decide about pooling"}`)

	h := windowHarness(t, WindowPolicyTokenBounded, nil)
	mixedFixture(h, 4, 4)
	invokable, evidence := h.build(t)
	output := call(t, invokable, `{"query":"what did I decide about pooling"}`)

	if len(output.Results) <= len(baseline.Results) {
		t.Fatalf("cards = %d, want more than the base window's %d",
			len(output.Results), len(baseline.Results))
	}
	for index, card := range baseline.Results {
		if output.Results[index].Ref != card.Ref || output.Results[index].Content != card.Content {
			t.Fatalf("base card %d changed: %+v vs %+v", index, output.Results[index], card)
		}
	}
	// Every card the model can see must be resolvable, appended ones included.
	for _, ref := range refsOf(output) {
		if _, err := evidence.Lookup(ref); err != nil {
			t.Fatalf("ref %s does not resolve: %v", ref, err)
		}
	}
}

// The budget is derived from what this call actually spent and is never
// exceeded, and the window never exceeds eight cards.
func TestTokenBoundedPolicyStaysInsideTheDerivedBudget(t *testing.T) {
	base := windowHarness(t, WindowPolicyFixed, nil)
	mixedFixture(base, 4, 4)
	baseTool, _ := base.build(t)
	baseline := call(t, baseTool, `{"query":"what did I decide about pooling"}`)

	h := windowHarness(t, WindowPolicyTokenBounded, nil)
	mixedFixture(h, 4, 4)
	invokable, _ := h.build(t)
	output := call(t, invokable, `{"query":"what did I decide about pooling"}`)

	limit := int(float64(baseline.ObservationTokens) * 1.35)
	if output.ObservationTokens > limit {
		t.Fatalf("observation = %d tokens, budget = %d", output.ObservationTokens, limit)
	}
	if output.ObservationTokens > 2800 {
		t.Fatalf("observation = %d tokens, over the absolute cap", output.ObservationTokens)
	}
	if len(output.Results) > 8 {
		t.Fatalf("cards = %d, over the card cap", len(output.Results))
	}
}

// A single-kind request is never widened: a short window there means that kind
// ran out of records, not that the merge dropped anything.
func TestTokenBoundedPolicyLeavesSingleKindRequestsAlone(t *testing.T) {
	fixed := windowHarness(t, WindowPolicyFixed, nil)
	mixedFixture(fixed, 4, 4)
	fixedTool, _ := fixed.build(t)
	baseline := call(t, fixedTool, `{"query":"my sessions","kinds":["episodic"]}`)

	h := windowHarness(t, WindowPolicyTokenBounded, nil)
	mixedFixture(h, 4, 4)
	invokable, _ := h.build(t)
	output := call(t, invokable, `{"query":"my sessions","kinds":["episodic"]}`)

	if len(output.Results) != len(baseline.Results) ||
		output.ObservationTokens != baseline.ObservationTokens {
		t.Fatalf("a single-kind call was widened: %d cards / %d tokens vs %d / %d",
			len(output.Results), output.ObservationTokens,
			len(baseline.Results), baseline.ObservationTokens)
	}
	for index, card := range baseline.Results {
		if output.Results[index].Ref != card.Ref {
			t.Fatalf("card %d differs", index)
		}
	}
}

// An appended card is carried whole or not at all: the observation budget may
// refuse it, but it may never shorten it. The per-card cap still applies, so
// "whole" means the same rendering every other card gets.
func TestAnAppendedCardIsNeverShortenedToFit(t *testing.T) {
	base := windowHarness(t, WindowPolicyFixed, nil)
	mixedFixture(base, 2, 60)
	baseTool, _ := base.build(t)
	baseline := call(t, baseTool, `{"query":"what did I decide about pooling"}`)

	h := windowHarness(t, WindowPolicyTokenBounded, nil)
	mixedFixture(h, 2, 60)
	invokable, _ := h.build(t)
	output := call(t, invokable, `{"query":"what did I decide about pooling"}`)

	if len(output.Results) <= len(baseline.Results) {
		t.Skip("this fixture appended nothing, so the rule is not exercised")
	}
	for _, card := range output.Results[len(baseline.Results):] {
		cost := rag.EstimateTokens(card.Content)
		if cost > 320 {
			t.Fatalf("appended card %s costs %d, over the per-card cap", card.Ref, cost)
		}
		// A body shortened by the observation budget would land strictly
		// between nothing and the per-card cap while its source was longer.
		// The only legitimate reason for Truncated here is the per-card cap.
		if card.Truncated && cost < 320 {
			t.Fatalf("appended card %s was shortened to %d tokens to fit", card.Ref, cost)
		}
	}
	if limit := int(float64(baseline.ObservationTokens) * 1.35); output.ObservationTokens > limit {
		t.Fatalf("observation = %d, budget = %d", output.ObservationTokens, limit)
	}
}

// Appended cards never carry source quotes, even when the model asked for them.
// Quotes are the only path by which a learner's raw text leaves the database
// verbatim, and a bonus slot does not justify widening that surface.
func TestAppendedCardsCarryNoSourceQuotes(t *testing.T) {
	h := windowHarness(t, WindowPolicyTokenBounded, func(h *toolHarness) {
		h.messages.rows[1] = model.Message{
			ID: 1, IsUser: true, Content: "I said this out loud",
			CreatedAt: toolClock.Add(-72 * time.Hour),
		}
	})
	mixedFixture(h, 4, 4)
	invokable, _ := h.build(t)
	base := 5
	output := call(t, invokable,
		`{"query":"what did I say about pooling","include_source_quotes":true}`)
	if len(output.Results) <= base {
		t.Skip("this fixture appended nothing, so the rule is not exercised")
	}
	for _, card := range output.Results[base:] {
		if len(card.SourceQuotes) != 0 {
			t.Fatalf("appended card %s carries %d quotes", card.Ref, len(card.SourceQuotes))
		}
	}
}

func TestUnknownWindowPolicyIsRefused(t *testing.T) {
	config := DefaultMemorySearchConfig()
	config.WindowPolicy = "wider_please"
	if err := config.Validate(); err == nil {
		t.Fatal("an unknown window policy was accepted")
	}
	for _, policy := range []memoryrecall.WindowPolicy{"", WindowPolicyFixed, WindowPolicyTokenBounded} {
		config.WindowPolicy = policy
		if err := config.Validate(); err != nil {
			t.Fatalf("policy %q was refused: %v", policy, err)
		}
	}
}
