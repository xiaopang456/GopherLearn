package agent

// A card that does not fit the window is shown as a way in.
//
// The card that is stored carries every entry of the session; the window has a
// thousand tokens. Leaving the card out entirely means the retrieval found a
// memory and the model never saw it - and the arm then reports a path that
// found nothing while the memory sat in the index.
//
// So a card that does not fit whole is shown as a NAVIGATION display: the
// conversation it came from, its subject, and as many of its entries as fit -
// each one WHOLE. The entries that did not fit are still reachable, because
// the display carries the session and the card's scope opens it.
//
// The materials here are ANONYMOUS AND SYNTHETIC. The real model outputs stay
// in the private evaluation directory.

import (
	"strings"
	"testing"

	"GopherAI/common/rag"
	"GopherAI/model"
)

// aBigCard is a card whose stored document is larger than the window.
//
// Twelve entries in each of three groups is what the server accepts, and each
// entry is as long as an entry may be. That is the shape the real outputs
// have, and it is why the card does not fit.
func aBigCard() model.EpisodeMemory {
	arrangements := make([]model.SessionCardEntry, 0, 12)
	products := make([]model.SessionCardEntry, 0, 12)
	openItems := make([]model.SessionCardEntry, 0, 12)
	for index := 0; index < 12; index++ {
		text := strings.Repeat("每一轮都做了九道题并且复盘了错题。", 3)
		arrangements = append(arrangements, model.SessionCardEntry{
			Text: text, Status: model.SessionCardStatusStated, MessageIDs: []uint{uint(index + 1)},
		})
		products = append(products, model.SessionCardEntry{
			Text: text, Status: model.SessionCardStatusAssistantProposed, MessageIDs: []uint{uint(index + 100)},
		})
		openItems = append(openItems, model.SessionCardEntry{
			Text: text, Status: model.SessionCardStatusStated, MessageIDs: []uint{uint(index + 200)},
		})
	}
	card := cardRow("card-big", "source-session", arrangements, products)
	card.Card.OpenItems = openItems
	return card
}

// The card does not fit the window as a whole, and it is shown anyway.
func TestACardThatDoesNotFitIsShownAsAWayIn(t *testing.T) {
	card := aBigCard()
	whole, _ := renderSessionCard(card)
	if rag.EstimateTokens(whole) <= 1000 {
		t.Fatalf("the fixture is %d tokens, which fits; the test proves nothing", rag.EstimateTokens(whole))
	}
	text, ids, _, scopes, err := renderSessionCardsWithinBudget([]model.EpisodeMemory{card}, 1000, true)
	if err != nil {
		t.Fatal(err)
	}
	if text == "" || len(ids) != 1 || ids[0] != "card-big" {
		t.Fatalf("the card was left out of the window: %q %v", text, ids)
	}
	// The display carries the way in: the conversation it came from.
	if !cardContains(text, "source_session: source-session") {
		t.Fatalf("the display does not name the conversation:\n%s", text)
	}
	// And it fits the budget it was given.
	if cost := rag.EstimateTokens(text); cost > 1000 {
		t.Fatalf("the display costs %d tokens, over the 1000 it was given:\n%s", cost, text)
	}
	// The scope is still issued: the session is open, so the rest of the card
	// is reachable.
	if len(scopes) != 1 || scopes[0].SessionID != "source-session" {
		t.Fatalf("the scope is %+v", scopes)
	}
}

// The entries that do not fit are not silently gone: the display says so, and
// it says how to reach them.
func TestTheDisplaySaysWhatWasLeftOutAndHowToReachIt(t *testing.T) {
	text, _, _, _, err := renderSessionCardsWithinBudget([]model.EpisodeMemory{aBigCard()}, 1000, true)
	if err != nil {
		t.Fatal(err)
	}
	if !cardContains(text, "more entries") {
		t.Fatalf("the display does not say what was left out:\n%s", text)
	}
	if !cardContains(text, "browse") || !cardContains(text, "search") {
		t.Fatalf("the display does not say how to reach the rest:\n%s", text)
	}
}

// An entry's text is never shortened.
//
// A cut sentence can lose the negation or the condition that made it true, so
// the navigation display leaves out entries rather than cutting them.
func TestTheNavigationDisplayNeverShortensAnEntry(t *testing.T) {
	card := aBigCard()
	text, _, _, _, err := renderSessionCardsWithinBudget([]model.EpisodeMemory{card}, 1000, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range card.Card.Arrangements {
		if !cardContains(text, entry.Text) {
			t.Fatalf("an entry was shown in a shortened form:\n%s", text)
			break
		}
	}
	// Every entry line in the display is a whole entry.
	for _, line := range strings.Split(text, "\n") {
		if !strings.Contains(line, "arrangement: ") {
			continue
		}
		body := strings.TrimPrefix(line, "  arrangement: ")
		if idx := strings.Index(body, " (status="); idx >= 0 {
			body = body[:idx]
		}
		whole := false
		for _, entry := range card.Card.Arrangements {
			if entry.Text == body {
				whole = true
				break
			}
		}
		if !whole {
			t.Fatalf("the display carries a cut entry: %q", body)
		}
	}
}

// The authorisation follows what the display actually shows.
//
// An entry the budget left out is an entry the model never saw, so the id it
// cites is not one it may ask for.
func TestTheGrantsFollowTheEntriesTheDisplayShows(t *testing.T) {
	card := aBigCard()
	_, _, grants, _, err := renderSessionCardsWithinBudget([]model.EpisodeMemory{card}, 1000, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) == 0 {
		t.Fatal("the display granted nothing, so the reader cannot read anything back")
	}
	// Every granted id is one the display shows, and no id beyond the card's
	// own watermark.
	for _, grant := range grants {
		if grant.CardID != "card-big" || grant.CardWatermark != card.SourceLastMessageID {
			t.Fatalf("the grant does not name the card: %+v", grant)
		}
		if grant.MessageID > card.SourceLastMessageID {
			t.Fatalf("the grant is past the card's watermark: %+v", grant)
		}
	}
	// The grants are fewer than the citations: the entries that did not fit
	// are not granted.
	cited := len(card.Card.CitedMessageIDs())
	if len(grants) >= cited {
		t.Fatalf("the display granted %d of %d citations, so it granted what it did not show", len(grants), cited)
	}
}

// A card that fits whole is still shown whole.
//
// The navigation display is a fallback, not a replacement.
func TestACardThatFitsIsStillShownWhole(t *testing.T) {
	card := cardRow("card-1", "source-session", []model.SessionCardEntry{
		{Text: "每轮9道题", Status: model.SessionCardStatusStated, MessageIDs: []uint{12}},
	}, nil)
	text, ids, grants, _, err := renderSessionCardsWithinBudget([]model.EpisodeMemory{card}, 1000, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || !cardContains(text, "每轮9道题") {
		t.Fatalf("the card was not shown whole: %q %v", text, ids)
	}
	if cardContains(text, "more entries") {
		t.Fatalf("a card that fits was shown as a navigation display:\n%s", text)
	}
	if len(grants) != 1 {
		t.Fatalf("the card granted %d ids, want the one it cited", len(grants))
	}
}

// A budget too small for anything is left empty.
//
// The navigation display needs room for the conversation, the subject and at
// least one entry. A budget that cannot hold those shows nothing, which is
// what the caller expects.
func TestABudgetTooSmallForAnythingShowsNothing(t *testing.T) {
	text, ids, grants, scopes, err := renderSessionCardsWithinBudget([]model.EpisodeMemory{aBigCard()}, 4, true)
	if err != nil {
		t.Fatal(err)
	}
	if text != "" || len(ids) != 0 || len(grants) != 0 || len(scopes) != 0 {
		t.Fatalf("a budget of four tokens showed something: %q %v %v %v", text, ids, grants, scopes)
	}
}
