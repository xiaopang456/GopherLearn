package agent

import (
	"strings"
	"testing"
)

// A summary grant covers only the session being summarised and only a decoded
// version; an episode grant names its card. Anything else authorises nothing.
func TestHistoryGrantUsability(t *testing.T) {
	summary := HistoryMessageGrant{MessageID: "m-1", SourceKind: HistorySourceSummary,
		SourceSessionID: "s-1", SummaryVersion: 3, SummaryWatermark: "m-9"}
	if !summary.UsableIn("u-1", "s-1") {
		t.Fatal("a decoded summary grant for this session was refused")
	}
	if summary.UsableIn("u-1", "s-other") {
		t.Fatal("a summary grant reached a different session")
	}
	noVersion := summary
	noVersion.SummaryVersion = 0
	if noVersion.UsableIn("u-1", "s-1") {
		t.Fatal("a summary grant without a version is usable")
	}
	episode := HistoryMessageGrant{MessageID: "m-2", SourceKind: HistorySourceEpisode,
		SourceSessionID: "s-2", EpisodeID: "ep-1", EpisodeVersion: 5, EpisodeWatermark: "m-9"}
	if !episode.UsableIn("u-1", "s-1") {
		t.Fatal("an episode grant may cite an earlier session")
	}
	badEpisode := episode
	badEpisode.EpisodeVersion = 0
	if badEpisode.UsableIn("u-1", "s-1") {
		t.Fatal("an episode grant without a version is usable")
	}
	unknown := HistoryMessageGrant{MessageID: "m-3", SourceKind: "elsewhere",
		SourceSessionID: "s-1"}
	if unknown.UsableIn("u-1", "s-1") {
		t.Fatal("an unknown source kind is usable")
	}
}

// The merged set deduplicates whole grants but keeps two provenances for one
// id, because "may this id be read" and "why" have different answers.
func TestMergeHistoryGrantsDeduplicatesExactRepeats(t *testing.T) {
	summaryA := HistoryMessageGrant{MessageID: "m-1", SourceKind: HistorySourceSummary,
		SourceSessionID: "s-1", SummaryVersion: 3, SummaryWatermark: "m-9"}
	summaryAgain := summaryA
	episodeSameID := HistoryMessageGrant{MessageID: "m-1", SourceKind: HistorySourceEpisode,
		SourceSessionID: "s-2", EpisodeID: "ep-1", EpisodeVersion: 5}
	merged := MergeHistoryMessageGrants([]HistoryMessageGrant{summaryA, summaryAgain}, []HistoryMessageGrant{episodeSameID})
	if len(merged) != 2 {
		t.Fatalf("merged %d grants, want the summary repeat dropped and both provenances kept", len(merged))
	}
	set := NewHistoryGrantSet("u-1", "s-1", merged)
	if set.Len() != 1 {
		t.Fatalf("the set holds %d distinct ids, want 1", set.Len())
	}
	provenances := set.Grants("m-1")
	if len(provenances) != 2 {
		t.Fatalf("one id carried %d provenances, want 2", len(provenances))
	}
	if !set.Covers("m-1", "s-1") || !set.Covers("m-1", "s-2") {
		t.Fatal("a covered session read as uncovered")
	}
	if set.Covers("m-1", "s-3") {
		t.Fatal("an uncovered session read as covered")
	}
}

// Unusable grants are dropped silently: they authorise nothing, and reporting
// them would suggest the model had a boundary to learn that it never had.
func TestGrantSetDropsUnusableGrants(t *testing.T) {
	grants := []HistoryMessageGrant{
		{MessageID: "m-1", SourceKind: HistorySourceSummary, SourceSessionID: "s-1", SummaryVersion: 0},
		{MessageID: "", SourceKind: HistorySourceSummary, SourceSessionID: "s-1", SummaryVersion: 2},
		{MessageID: "m-2", SourceKind: HistorySourceSummary, SourceSessionID: "s-other", SummaryVersion: 2},
		{MessageID: "m-3", SourceKind: HistorySourceSummary, SourceSessionID: "s-1", SummaryVersion: 2, SummaryWatermark: "m-9"},
	}
	set := NewHistoryGrantSet("u-1", "s-1", grants)
	if set.Len() != 1 {
		t.Fatalf("the set holds %d usable ids, want 1", set.Len())
	}
	if set.Grants("m-3") == nil {
		t.Fatal("the usable grant was dropped")
	}
}

func renderSummaryItem(id string, text string) string {
	return `{"kind":"decision","text":"` + text + `","evidence_message_ids":["` + id + `"]}`
}

// The scanner keeps the cited ids whose token is complete in the rendered
// block, and only inside evidence_message_ids arrays: a number in prose
// authorises nothing.
func TestVisibleSummaryEvidenceIDs(t *testing.T) {
	cited := []string{"m-1", "m-2", "m-9"}
	rendered := "Session summary\n" + renderSummaryItem("m-1", "决定用 JSON") + "\n" +
		renderSummaryItem("m-2", "决定用 Protobuf") + "\n普通正文提到 m-9 但不在任何数组里"
	visible := VisibleSummaryEvidenceIDs(rendered, cited)
	if len(visible) != 2 || visible[0] != "m-1" || visible[1] != "m-2" {
		t.Fatalf("visible %v, want m-1 and m-2", visible)
	}
}

// A cut anywhere yields fewer ids and never a wrong one: before the key, inside
// the array, mid-token.
func TestVisibleSummaryEvidenceIDsTruncation(t *testing.T) {
	cited := []string{"m-1", "m-2"}
	cases := []struct {
		name     string
		rendered string
		want     []string
	}{
		{"cut before the key finishes", `items: [{"evidence_message_ids":[`, nil},
		{"cut after the key before the array", `items: [{"evidence_message_ids"`, nil},
		{"cut inside the first id", `{"evidence_message_ids":["m-1`, nil},
		{"first id complete, second cut", `{"evidence_message_ids":["m-1", "m-2`, []string{"m-1"}},
		{"array closed, more text", `{"evidence_message_ids":["m-1","m-2"]} 后文`, []string{"m-1", "m-2"}},
		{"prose only", `他说过 evidence_message_ids 但没有数组`, nil},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			visible := VisibleSummaryEvidenceIDs(testCase.rendered, cited)
			if strings.Join(visible, ",") != strings.Join(testCase.want, ",") {
				t.Fatalf("visible %v, want %v", visible, testCase.want)
			}
		})
	}
}

// An id inside a longer id is not a visible id, and an escaped quote does not
// end the token early.
func TestVisibleSummaryEvidenceIDSTokenBoundaries(t *testing.T) {
	cited := []string{"m-1", "m-11", `m-"x"`}
	rendered := `{"evidence_message_ids":["m-11","m-\"x\""]}`
	visible := VisibleSummaryEvidenceIDs(rendered, cited)
	if len(visible) != 2 || visible[0] != "m-11" || visible[1] != `m-"x"` {
		t.Fatalf("visible %v, want m-11 and the escaped id", visible)
	}
}

// The grant builder licenses only what the rendered block shows, and only
// under a decoded version and a session.
func TestHistoryGrantsFromSummary(t *testing.T) {
	rendered := "前文\n" + renderSummaryItem("m-1", "决定") + "\n" + renderSummaryItem("m-2", "纠正")
	cited := []string{"m-1", "m-2", "m-3"}
	grants := HistoryGrantsFromSummary(rendered, "s-1", 7, "m-9", cited)
	if len(grants) != 2 {
		t.Fatalf("grants %v, want m-1 and m-2", grants)
	}
	for _, grant := range grants {
		if grant.SourceKind != HistorySourceSummary || grant.SourceSessionID != "s-1" || grant.SummaryVersion != 7 {
			t.Fatalf("grant carries the wrong provenance: %+v", grant)
		}
	}
	if HistoryGrantsFromSummary("", "s-1", 7, "m-9", cited) != nil {
		t.Fatal("no rendered block, no grants")
	}
	if HistoryGrantsFromSummary(rendered, "s-1", 0, "m-9", cited) != nil {
		t.Fatal("no version, no grants")
	}
	if HistoryGrantsFromSummary(rendered, "", 7, "m-9", cited) != nil {
		t.Fatal("no session, no grants")
	}
	if HistoryGrantsFromSummary(rendered, "s-1", 7, "m-9", nil) != nil {
		t.Fatal("nothing cited, no grants")
	}
}
