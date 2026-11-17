package agent

// The synopsis's budget check counts the header once.
//
// It used to count it twice, and a synopsis that fitted beside the header was
// thrown away over a band of lengths - the display showed a card without its
// overview and said nothing about it.
//
// The materials here are ANONYMOUS AND SYNTHETIC.

import (
	"strings"
	"testing"

	"GopherAI/common/rag"
	"GopherAI/model"
)

// A synopsis that fits is kept.
//
// The band is the point: every length in it has to keep its synopsis.
func TestASynopsisThatFitsBesideTheHeaderIsKept(t *testing.T) {
	dropped := 0
	for length := 100; length <= 1100; length += 10 {
		card := cardRow("card-1", "source-session", []model.SessionCardEntry{
			{Text: "每轮9道题", Status: model.SessionCardStatusStated, MessageIDs: []uint{12}},
		}, nil)
		card.Card.Synopsis = strings.Repeat("题", length)
		// The entries are long enough that the whole card does not fit, so
		// the navigation display is the one that is measured.
		card.Card.Arrangements = make([]model.SessionCardEntry, 0, 12)
		for index := 0; index < 12; index++ {
			card.Card.Arrangements = append(card.Card.Arrangements, model.SessionCardEntry{
				Text:   strings.Repeat("每一轮都做了九道题并且复盘了错题。", 3),
				Status: model.SessionCardStatusStated, MessageIDs: []uint{uint(index + 1)},
			})
		}
		text, _, _, _, err := renderSessionCardsWithinBudget([]model.EpisodeMemory{card}, 1000, true)
		if err != nil {
			t.Fatal(err)
		}
		if text == "" {
			continue
		}
		// Does the synopsis fit? Three things share the budget: the header
		// every block carries, the card's own header, and the synopsis. The
		// test computes the same thing the renderer does rather than
		// assuming it.
		header := "- time: 2026-11-15T09:00:00Z\n  source_session: source-session\n  title: Go 并发练习"
		fits := messageTokenCost(sessionCardHeader)+rag.EstimateTokens(header+"\n  synopsis: "+card.Card.Synopsis)+1 <= 1000
		if fits && !strings.Contains(text, "synopsis:") {
			dropped++
		}
		if !fits && strings.Contains(text, "synopsis:") {
			t.Fatalf("a synopsis of %d characters was kept although it does not fit", length)
		}
	}
	if dropped != 0 {
		t.Fatalf("%d synopsis lengths were dropped although they fitted", dropped)
	}
}

// The display never exceeds the budget, whatever it holds.
func TestTheDisplayIsWithinItsBudgetForEverySynopsisLength(t *testing.T) {
	for length := 100; length <= 1100; length += 50 {
		card := cardRow("card-1", "source-session", []model.SessionCardEntry{
			{Text: strings.Repeat("每一轮都做了九道题并且复盘了错题。", 3),
				Status: model.SessionCardStatusStated, MessageIDs: []uint{12}},
		}, nil)
		card.Card.Synopsis = strings.Repeat("题", length)
		text, _, _, _, err := renderSessionCardsWithinBudget([]model.EpisodeMemory{card}, 1000, true)
		if err != nil {
			t.Fatal(err)
		}
		if text == "" {
			continue
		}
		if cost := rag.EstimateTokens(text); cost > 1000 {
			t.Fatalf("a synopsis of %d characters showed %d tokens", length, cost)
		}
	}
}
