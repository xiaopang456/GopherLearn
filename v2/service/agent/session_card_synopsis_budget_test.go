package agent

// The synopsis never crowds out the facts.
//
// The synopsis is the card's own overview of the session; the entries are the
// facts. A display that carries the overview and no entry tells the reader
// what the session was about and nothing else, while a display that carries
// one entry and no overview tells them one thing that happened - and says
// the overview is missing.
//
// So the synopsis is kept only when it fits AND leaves room for a fact and
// for the marker. When it does not, the facts win, and the marker says what
// was left out.
//
// The materials here are ANONYMOUS AND SYNTHETIC.

import (
	"strings"
	"testing"

	"GopherAI/common/rag"
	"GopherAI/model"
)

// aCardWithALongSynopsis is a card whose overview is long and whose entries
// are short.
func aCardWithALongSynopsis(length int) model.EpisodeMemory {
	card := cardRow("card-1", "source-session", []model.SessionCardEntry{
		{Text: "每轮9道题", Status: model.SessionCardStatusStated, MessageIDs: []uint{12}},
	}, nil)
	card.Card.Synopsis = strings.Repeat("题", length)
	return card
}

// Every synopsis length ends in one of two states, and both are stated.
//
//   - the synopsis is shown, and so is at least one entry;
//   - the synopsis is not shown, the marker says so, and at least one entry is
//     shown.
//
// A card that shows neither is left out - that is the honest answer for a
// budget that cannot hold anything.
func TestEverySynopsisLengthEndsInAStatedState(t *testing.T) {
	for length := 100; length <= 1100; length += 10 {
		card := aCardWithALongSynopsis(length)
		text, ids, _, _, err := renderSessionCardsWithinBudget([]model.EpisodeMemory{card}, 1000, true)
		if err != nil {
			t.Fatal(err)
		}
		if text == "" {
			// The card was left out. That is allowed only when not even one
			// entry fits beside the marker - which cannot happen here,
			// because the entries are short.
			t.Fatalf("a synopsis of %d characters left the card out entirely", length)
		}
		if len(ids) != 1 {
			t.Fatalf("a synopsis of %d characters showed %d cards", length, len(ids))
		}
		hasSynopsis := strings.Contains(text, "synopsis:")
		hasEntry := strings.Contains(text, "arrangement: 每轮9道题")
		switch {
		case hasSynopsis && hasEntry:
			// The ideal state.
		case !hasSynopsis && hasEntry:
			// The facts won; the marker has to say the overview is missing.
			if !strings.Contains(text, "synopsis is not shown") &&
				!strings.Contains(text, "synopsis are not shown") {
				t.Fatalf("a synopsis of %d characters was dropped without saying so:\n%s", length, text)
			}
		default:
			t.Fatalf("a synopsis of %d characters left the display with neither an overview "+
				"nor a fact:\n%s", length, text)
		}
	}
}

// The card is NOT dropped where the old code dropped it.
//
// The old code threw the overview away and kept the facts; a fix that kept the
// overview could crowd the facts out and leave the display with nothing, and
// then the card was dropped whole. A card that the old code showed has to be
// shown now, and with at least one fact in it.
func TestACardTheOldCodeShowedIsStillShownWithAFactInIt(t *testing.T) {
	for length := 700; length <= 950; length += 5 {
		card := aCardWithALongSynopsis(length)
		text, ids, _, _, err := renderSessionCardsWithinBudget([]model.EpisodeMemory{card}, 1000, true)
		if err != nil {
			t.Fatal(err)
		}
		if len(ids) != 1 {
			t.Fatalf("a synopsis of %d characters dropped the card the old code showed", length)
		}
		if !strings.Contains(text, "arrangement: 每轮9道题") {
			t.Fatalf("a synopsis of %d characters left the display with no fact:\n%s", length, text)
		}
	}
}

// The display never exceeds the budget, whatever it holds.
func TestTheDisplayIsWithinItsBudgetForEverySynopsisLength(t *testing.T) {
	for length := 100; length <= 1100; length += 50 {
		card := aCardWithALongSynopsis(length)
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
