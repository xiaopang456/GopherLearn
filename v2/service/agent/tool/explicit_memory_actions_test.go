package agenttool

import (
	"context"
	"strings"
	"testing"

	serviceagent "GopherAI/service/agent"
)

// STABILIZE-061, the contract layer.
//
// Two claims are checked here. The first is that the explicit surface really is
// the old implementation under new names - the parameter table it hands the
// model is the Episode contract's, with no kinds field and nothing added. The
// second is that the modes with published measurements did not move.

// A. The explicit read contract is the Episode contract, renamed.
//
// This matters more than it looks. If the rename had accidentally been applied
// to the general tool, the model would read "search_past_sessions" over a
// parameter table that still offers kinds=["semantic"] - a name promising the
// narrow contract over an implementation that does not keep it, which is worse
// than either contract alone.
func TestExplicitReadContractIsTheEpisodeContractRenamed(t *testing.T) {
	h := episodicHarness(t, func(h *toolHarness) {})
	h.registry.UseExplicitActionNames(true)
	invokable, _ := h.build(t)
	info, err := invokable.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != serviceagent.SearchPastSessionsToolName {
		t.Fatalf("tool name = %s", info.Name)
	}
	if serviceagent.CapabilityOf(info.Name) != serviceagent.CapabilityEpisodeSearch {
		t.Fatal("the renamed read tool lost its capability")
	}
	description, blob := toolParameters(t, invokable)
	if strings.Contains(blob, "kinds") {
		t.Fatalf("the explicit read contract offers kinds: %s", blob)
	}
	for _, field := range []string{"query", "temporal", "top_k", "include_source_quotes"} {
		if !strings.Contains(blob, field) {
			t.Fatalf("the explicit read contract lost %s: %s", field, blob)
		}
	}
	// The description has to carry both halves of the exclusion, because the
	// old one carried only the positive half and STABILIZE-060 still sent three
	// save requests into it.
	if !strings.Contains(description, "saves nothing") {
		t.Fatalf("the description does not say the tool cannot save: %s", description)
	}
	if !strings.Contains(description, serviceagent.SaveMemoryDraftToolName) {
		t.Fatalf("the description does not name the tool that can: %s", description)
	}
	for _, excluded := range []string{"topic notes", "learning state", "current session"} {
		if !strings.Contains(description, excluded) {
			t.Fatalf("the description does not exclude %s: %s", excluded, description)
		}
	}
}

// B. The explicit surface fails closed rather than serving the general contract
// under the narrow name.
func TestExplicitReadNamesRequireTheEpisodicContract(t *testing.T) {
	h := newToolHarness(t, nil)
	h.registry.UseEpisodicOnly(false)
	h.registry.UseExplicitActionNames(true)
	_, err := h.registry.Tools(context.Background(), serviceagent.RunContext{
		RunID: "run-1", UserID: 1, UserName: "learner", SessionID: "current", Question: "q",
		MemoryEvidence: serviceagent.NewMemoryEvidenceRegistry(),
	})
	if err == nil {
		t.Fatal("the general contract was served under the explicit read name")
	}
}

// C. The write contract states its provenance rule, and states that it does not
// search.
//
// The server enforces the first - a draft is built from the tool's own
// arguments and the run identity, and no retrieval result reaches a
// DraftRequest - but enforcement cannot stop a model from copying a card it is
// looking at into the content field, and a draft quoting something the learner
// never said is a false memory with their name on it.
func TestExplicitWriteContractStatesItsProvenanceRule(t *testing.T) {
	for _, required := range []string{
		"CURRENT user message", "searches nothing", "must confirm",
		"Never write retrieved memory cards, search results, your own inferences",
	} {
		if !strings.Contains(saveMemoryDraftDescription, required) {
			t.Fatalf("the draft description does not say %q", required)
		}
	}
	if strings.Contains(searchPastSessionsDescription, "remember this") {
		t.Fatal("the read description invites the call this round exists to stop")
	}
}

// D. The two explicit contracts are distinguishable from each other and from
// both frozen contracts, so a freeze cannot bind one while describing another.
func TestExplicitContractDigestsAreTheirOwn(t *testing.T) {
	read, err := SearchPastSessionsSchemaDigest()
	if err != nil {
		t.Fatal(err)
	}
	write, err := SaveMemoryDraftSchemaDigest()
	if err != nil {
		t.Fatal(err)
	}
	general, err := MemorySearchSchemaDigest()
	if err != nil {
		t.Fatal(err)
	}
	episode, err := EpisodeSearchSchemaDigest()
	if err != nil {
		t.Fatal(err)
	}
	digests := map[string]string{"read": read, "write": write, "general": general, "episode": episode}
	seen := map[string]string{}
	for name, digest := range digests {
		if len(digest) != 64 {
			t.Fatalf("%s digest is not a sha256: %q", name, digest)
		}
		if other, clash := seen[digest]; clash {
			t.Fatalf("%s and %s produced the same digest", name, other)
		}
		seen[digest] = name
	}
}

// E. The frozen surfaces did not move. The STABILIZE-059/060 freeze bound these
// two numbers, and a round that changed either of them would make its own
// results incomparable with the ones it is trying to improve on.
func TestFrozenContractsAreByteIdenticalAfterTheRename(t *testing.T) {
	const generalFrozenBySTABILIZE058v181 = "790f2ba6e0bf9b9f517d72437c01636f6324ad6c59b1652109fed93e1a8c6490"
	general, err := MemorySearchSchemaDigest()
	if err != nil {
		t.Fatal(err)
	}
	if general != generalFrozenBySTABILIZE058v181 {
		t.Fatalf("the general memory_search schema changed: %s", general)
	}
	// The default registry - every mode before this round - still builds the
	// old names.
	h := episodicHarness(t, func(h *toolHarness) {})
	invokable, _ := h.build(t)
	info, err := invokable.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != MemorySearchName {
		t.Fatalf("the episodic mode's tool is now called %s", info.Name)
	}
}
