package agenttool

import (
	"GopherAI/service/memorysearch"
	"strings"
	"testing"
)

func episodicOnlyConfig(t *testing.T) MemorySearchConfig {
	t.Helper()
	config := DefaultMemorySearchConfig()
	if err := config.Validate(); err != nil {
		t.Fatalf("default config invalid: %v", err)
	}
	return config
}

// The confirmed mode's central claim is that topic personal state reaches an
// answer only through an authoritative learning session or an explicit user
// confirmation. A memory_search that still served kinds=["semantic"] would be a
// second, unconfirmed door into the same data.
func TestEpisodicOnlyRefusesAnExplicitSemanticRequest(t *testing.T) {
	config := episodicOnlyConfig(t)
	_, reason, err := normalizeMemorySearchInput(
		&MemorySearchInput{Query: "我对 JWT 的理解", Kinds: []string{"semantic"}}, config, true)
	if err == nil {
		t.Fatal("an explicit semantic request must be refused under the confirmed mode")
	}
	if reason != reasonSemanticNotPermitted {
		t.Fatalf("reason = %q, want %q", reason, reasonSemanticNotPermitted)
	}
	// The refusal explains itself, so a model does not report that it checked
	// the learner's notes when it did not.
	if !strings.Contains(err.Error(), "confirmed") {
		t.Fatalf("refusal must name the reason: %v", err)
	}
}

func TestEpisodicOnlyRefusesSemanticEvenWhenPairedWithEpisodic(t *testing.T) {
	config := episodicOnlyConfig(t)
	if _, _, err := normalizeMemorySearchInput(
		&MemorySearchInput{Query: "上次聊到哪", Kinds: []string{"episodic", "semantic"}}, config, true); err == nil {
		t.Fatal("a mixed request must not smuggle semantic through")
	}
}

// An omitted kinds list means "search what you have". Under this mode that is
// episodes, so the default narrows rather than failing.
func TestEpisodicOnlyNarrowsTheDefaultInsteadOfFailing(t *testing.T) {
	config := episodicOnlyConfig(t)
	normalized, _, err := normalizeMemorySearchInput(&MemorySearchInput{Query: "上次聊到哪"}, config, true)
	if err != nil {
		t.Fatalf("an unqualified search must still work: %v", err)
	}
	if len(normalized.kinds) != 1 || normalized.kinds[0] != memorysearch.KindEpisodic {
		t.Fatalf("kinds = %v, want episodic only", normalized.kinds)
	}
}

func TestEpisodicSearchStillWorksUnderTheConfirmedMode(t *testing.T) {
	config := episodicOnlyConfig(t)
	normalized, _, err := normalizeMemorySearchInput(
		&MemorySearchInput{Query: "上次的决定", Kinds: []string{"episodic"}}, config, true)
	if err != nil {
		t.Fatalf("episodic history must stay reachable: %v", err)
	}
	if len(normalized.kinds) != 1 || normalized.kinds[0] != memorysearch.KindEpisodic {
		t.Fatalf("kinds = %v", normalized.kinds)
	}
}

// Every other mode is unaffected: the restriction is a per-run setter, not a
// change to the frozen config the published measurements were taken under.
func TestOtherModesKeepBothKinds(t *testing.T) {
	config := episodicOnlyConfig(t)
	normalized, _, err := normalizeMemorySearchInput(
		&MemorySearchInput{Query: "我对 JWT 的理解", Kinds: []string{"semantic"}}, config, false)
	if err != nil {
		t.Fatalf("semantic search must still work elsewhere: %v", err)
	}
	if len(normalized.kinds) != 1 || normalized.kinds[0] != memorysearch.KindSemantic {
		t.Fatalf("kinds = %v", normalized.kinds)
	}
	both, _, err := normalizeMemorySearchInput(&MemorySearchInput{Query: "任何东西"}, config, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(both.kinds) != 2 {
		t.Fatalf("the unrestricted default must still be both kinds, got %v", both.kinds)
	}
}
