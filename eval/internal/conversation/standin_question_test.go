package conversation

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	episodeService "GopherAI/service/episode"
)

// The stand-in's question is chosen from the product's own read plan, and this
// test is what pins that choice.
//
// The local stand-in asks one question and both arms have to hit a memory
// with it - a stand-in that misses would measure an empty window and call it
// a result. The question lives with the stand-in's fixtures, and this test
// says which plan it has to reach for the hit to be deterministic:
//
//   - the old arm's `legacy_v2` router reads "上次" as the last session and
//     takes it from MySQL, so it needs the question to name a past session;
//   - the card arm's router reads the question as a decision question, and
//     the retrieval only admits a card that the lexical channel and the
//     vector channel agree on - so the question has to carry the topic terms
//     the card was written from.
//
// If the router changes, this test fails and the stand-in's question has to be
// chosen again, rather than the stand-in quietly measuring nothing.
//
// The question is read out of the fixture rather than written here, so the
// test and the stand-in cannot drift apart.
func TestTheStandInQuestionReachesTheDecisionPlan(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "standin-question.txt"))
	if err != nil {
		t.Fatalf("read the stand-in's question: %v", err)
	}
	question := strings.TrimSpace(string(raw))
	if question == "" {
		t.Fatal("the stand-in's question is empty")
	}
	plan := episodeService.NewQueryRouter().Plan(episodeService.RouteRequest{Question: question})
	if !plan.NeedEpisode {
		t.Fatalf("the question asks for no recall: %+v", plan)
	}
	if plan.EvidenceMode != episodeService.EvidenceDecision {
		t.Fatalf("the question is not a decision question: %+v", plan)
	}
	if plan.TemporalMode != episodeService.TemporalLatestMatching {
		t.Fatalf("the question is not a recency question: %+v", plan)
	}
	for _, want := range []string{"Redis", "编码"} {
		found := false
		for _, term := range plan.TopicTerms {
			if strings.EqualFold(term, want) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("the question's topic terms lost %q: %v", want, plan.TopicTerms)
		}
	}
}
