package episode

// The card's own text has two boundaries, and both of them used to be able to
// lose a whole card.
//
//   - The SYNOPSIS is prose. It is cut at a sentence end, and when there is no
//     sentence end it is cut where nothing is left open. When neither exists
//     inside the limit - a long quotation whose closing quote is past the
//     limit - there was no legal cut, and the text was cut by the character
//     count anyway. That left the quotation open, and a description that ends
//     in the middle of a quotation reads as a typo.
//
//   - The TITLE is a label. It is cut where the cut would not leave half a
//     word. A title made of one unbroken run of letters has no such place, so
//     the cut retreated to the beginning and left nothing - and a card with no
//     title is refused, which loses the whole session over a label.
//
// Both are about the card's own description, not about the facts: the entries
// are stored whole either way. So the fallback for a description that cannot
// be cut legally is a text that is already whole - a complete entry - and for
// the title a legal subject.
//
// The materials here are ANONYMOUS AND SYNTHETIC. The real model outputs stay
// in the private evaluation directory, and they are replayed by the private
// harness.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"GopherAI/model"
	"github.com/cloudwego/eino/schema"
)

// A synopsis with no legal cut inside the limit falls back to a complete
// entry, and never keeps an unclosed quotation.
func TestASynopsisWithNoLegalCutFallsBackToACompleteEntry(t *testing.T) {
	// A quotation that opens at the start and closes past the limit: there is
	// no sentence end inside the limit and no place where nothing is left
	// open.
	body := strings.Repeat("长", 1300)
	unclosable := `"` + body + `"`
	if utf8Len(unclosable) <= model.SessionCardSynopsisMaxRunes {
		t.Fatalf("the sample is %d characters, which is not over the limit", utf8Len(unclosable))
	}
	now := time.Now()
	evidence := cardEvidenceFor(
		model.Message{ID: 1, SessionID: "session-card", IsUser: true, Content: "题量", CreatedAt: now},
	)
	chat := &cardChat{responses: []*schema.Message{cardToolResponse(`{
		"version":1,
		"title":"Go 并发练习",
		"synopsis":` + quoteJSON(unclosable) + `,
		"topics":[],
		"arrangements":[{"text":"每轮9道题","status":"stated","message_ids":[1]}],
		"assistant_products":[],"open_items":[]}`)}}
	generator, err := NewModelCardGenerator(chat, 4096, 1200)
	if err != nil {
		t.Fatal(err)
	}
	document, err := generator.Generate(context.Background(), evidence)
	if err != nil {
		t.Fatalf("the card was refused over its synopsis: %v", err)
	}
	if !cardSynopsisIsBalanced(document.Synopsis) {
		t.Fatalf("the synopsis leaves something open: %q", document.Synopsis)
	}
	if strings.TrimSpace(document.Synopsis) == "" {
		t.Fatal("the synopsis is empty")
	}
	if utf8Len(document.Synopsis) > model.SessionCardSynopsisMaxRunes {
		t.Fatalf("the synopsis is %d characters, over the limit", utf8Len(document.Synopsis))
	}
}

// A title that the cut empties gets a legal subject, and the card is not
// refused over a label.
func TestATitleThatTheCutEmptiesGetsALegalSubject(t *testing.T) {
	// One unbroken run of ASCII letters: there is no place inside it where a
	// cut would not leave half a word.
	unbreakable := strings.Repeat("a", 161)
	now := time.Now()
	evidence := cardEvidenceFor(
		model.Message{ID: 1, SessionID: "session-card", IsUser: true, Content: "题量", CreatedAt: now},
	)
	chat := &cardChat{responses: []*schema.Message{cardToolResponse(`{
		"version":1,
		"title":` + quoteJSON(unbreakable) + `,
		"synopsis":"这一场练习定了每轮九道题。",
		"topics":[],
		"arrangements":[{"text":"每轮9道题","status":"stated","message_ids":[1]}],
		"assistant_products":[],"open_items":[]}`)}}
	generator, err := NewModelCardGenerator(chat, 4096, 1200)
	if err != nil {
		t.Fatal(err)
	}
	document, err := generator.Generate(context.Background(), evidence)
	if err != nil {
		t.Fatalf("the card was refused over its title: %v", err)
	}
	if strings.TrimSpace(document.Title) == "" {
		t.Fatal("the title is empty")
	}
	if utf8Len(document.Title) > model.SessionCardTitleMaxRunes {
		t.Fatalf("the title is %d characters, over the limit", utf8Len(document.Title))
	}
	// The fallback title says what the card is about without claiming
	// anything the model did not write: it is either the synopsis' first
	// sentence or the card's own subject.
	if document.Title != "这一场练习定了每轮九道题。" && document.Title != cardSubjectFallback {
		t.Fatalf("the title is %q, want a legal fallback", document.Title)
	}
}

// cardSynopsisIsBalanced says whether the text leaves nothing open.
//
// It is the promise the synopsis makes: a reader must not have to guess where
// the quotation ends.
func cardSynopsisIsBalanced(value string) bool {
	depth := 0
	for _, char := range value {
		switch char {
		case '"':
			if depth > 0 {
				depth--
			} else {
				depth++
			}
		case '“', '‘', '（', '(', '「', '《', '[', '【':
			depth++
		case '”', '’', '）', ')', '」', '》', ']', '】':
			if depth > 0 {
				depth--
			}
		}
	}
	return depth == 0
}

// utf8Len is the number of characters, which is what the server's limits
// count.
func utf8Len(value string) int { return len([]rune(value)) }

// quoteJSON writes a string as a JSON string.
func quoteJSON(value string) string {
	replaced := strings.ReplaceAll(value, `\`, `\\`)
	replaced = strings.ReplaceAll(replaced, `"`, `\"`)
	return `"` + replaced + `"`
}

// The synopsis fallback needs a card whose synopsis cannot be cut, and the
// fallback must be a complete entry - so the entry has to survive the same
// limit.
func TestTheSynopsisFallbackIsACompleteEntry(t *testing.T) {
	document := &model.SessionCardDocument{
		Version:  model.SessionCardContentVersion,
		Title:    "题量",
		Synopsis: `"` + strings.Repeat("长", 1300) + `"`,
		Arrangements: []model.SessionCardEntry{{
			Text: "每轮9道题", Status: model.SessionCardStatusStated, MessageIDs: []uint{1},
		}},
	}
	fitted, fit := fitCardSynopsis(document.Synopsis, document, model.SessionCardSynopsisMaxRunes)
	if strings.TrimSpace(fitted) == "" {
		t.Fatalf("the synopsis is empty: %+v", fit)
	}
	if !cardSynopsisIsBalanced(fitted) {
		t.Fatalf("the fallback leaves something open: %q", fitted)
	}
	if fitted != "每轮9道题" {
		t.Fatalf("the fallback is %q, want the first entry", fitted)
	}
	if !fit.Filled {
		t.Fatalf("the fallback is not recorded as filled in: %+v", fit)
	}
}

// A card with no legal synopsis and no entries has nothing to describe, and
// the caller refuses it - the refusal is the honest answer.
func TestACardWithNoLegalSynopsisAndNoEntriesIsRefused(t *testing.T) {
	document := &model.SessionCardDocument{
		Version:  model.SessionCardContentVersion,
		Title:    "题量",
		Synopsis: `"` + strings.Repeat("长", 1300) + `"`,
	}
	fitted, fit := fitCardSynopsis(document.Synopsis, document, model.SessionCardSynopsisMaxRunes)
	if strings.TrimSpace(fitted) != "" {
		t.Fatalf("the synopsis is %q, want none: %+v", fitted, fit)
	}
}

// The cut that cannot be made is reported as such, and not as a cut at the
// limit.
func TestTheSynopsisCutSaysWhenThereWasNoLegalCut(t *testing.T) {
	value := `"` + strings.Repeat("长", 1300) + `"`
	kept, fit := cutCardSynopsis(value, model.SessionCardSynopsisMaxRunes)
	if kept != "" {
		t.Fatalf("the text was kept anyway: %q", kept)
	}
	if fit.Reason != "unclosed" {
		t.Fatalf("the reason is %q, want %q", fit.Reason, "unclosed")
	}
}

// The title fallback is used only when the cut empties the title.
func TestTheTitleKeepsWhatItCanWhenItCan(t *testing.T) {
	title, fit := fitCardTitle("Go 并发练习", "这一场练习定了每轮九道题。", model.SessionCardTitleMaxRunes)
	if title != "Go 并发练习" {
		t.Fatalf("the title is %q, want the one the model wrote", title)
	}
	if fit.Filled {
		t.Fatalf("the title was replaced although it was fine: %+v", fit)
	}
}

// The card's own subject is the last fallback, and it is legal by
// construction.
func TestTheLastTitleFallbackIsTheCardsOwnSubject(t *testing.T) {
	title, fit := fitCardTitle(strings.Repeat("a", 161), "", model.SessionCardTitleMaxRunes)
	if title != cardSubjectFallback {
		t.Fatalf("the title is %q, want %q", title, cardSubjectFallback)
	}
	if !fit.Filled {
		t.Fatalf("the fallback is not recorded as filled in: %+v", fit)
	}
}

// The generation fails for reasons that have nothing to do with the text, and
// the error is the protocol one.
func TestTheCardIsStillRefusedWhenTheModelBreaksTheProtocol(t *testing.T) {
	now := time.Now()
	evidence := cardEvidenceFor(
		model.Message{ID: 1, SessionID: "session-card", IsUser: true, Content: "题量", CreatedAt: now},
	)
	chat := &cardChat{responses: []*schema.Message{cardToolResponse(`{
		"version":1,
		"title":"Go 并发练习",
		"synopsis":"这一场练习定了每轮九道题。",
		"topics":[],
		"arrangements":[{"text":"每轮9道题","status":"stated","message_ids":[99]}],
		"assistant_products":[],"open_items":[]}`)}}
	generator, err := NewModelCardGenerator(chat, 4096, 1200)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := generator.Generate(context.Background(), evidence); !errors.Is(err, ErrCardSourceBoundary) {
		t.Fatalf("the forged citation was accepted: %v", err)
	}
	_ = fmt.Sprint()
}
