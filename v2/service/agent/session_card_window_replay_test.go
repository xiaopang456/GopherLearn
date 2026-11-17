package agent

// The whole chain, on the cards the real model wrote.
//
// The receive step is not the end of the story. A card can be received, stored
// whole, and still never reach the window: the window has a budget, and a
// card that does not fit used to be left out entirely. Then the retrieval
// finds a memory, the model never sees it, and the arm reports a path that
// found nothing while the memory sat in the index.
//
// So this test runs the chain the reader actually goes through:
//
//	receive -> persist -> render into the window -> source and authorisation
//
// and it checks the LAST step, not the first one. A card that is received and
// never shown is not a card the arm can measure.
//
// The fixtures are the real answers when PHASE2_CARD_REPLAY_DIR points at
// them, and the synthetic ones in this repository otherwise: the repository
// is public and a recorded answer is a real conversation.
//
// The materials here are ANONYMOUS AND SYNTHETIC. The real model outputs stay
// in the private evaluation directory.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"GopherAI/common/rag"
	"GopherAI/model"
	"GopherAI/service/episode"
)

// theWindowBudget is the budget the cross-session block gets.
//
// It is the budget of the card path: a card and an Episode are two ways of
// carrying the same amount of cross-session context, and a deployment that
// switches paths must not also double the window it spends on the past.
const theWindowBudget = 1000

// aRecordedCard is one recorded answer.
type aRecordedCard struct {
	Scenario   string   `json:"scenario"`
	Arguments  string   `json:"arguments"`
	MessageIDs []uint   `json:"message_ids"`
	Problems   []string `json:"problems"`
	Expect     string   `json:"expect"`
}

// aPersistedCard turns a received document into the row the database holds.
//
// It is the same shape the projector writes: the document, the title and the
// search text the card is found by, and the watermark the card covers.
func aPersistedCard(document model.SessionCardDocument, sessionID string, lastMessageID uint) model.EpisodeMemory {
	return model.EpisodeMemory{
		ID: "card-" + sessionID, UserID: 7, SessionID: sessionID,
		ContentKind:         model.EpisodeContentSessionCard,
		Title:               episode.CardTitle(document),
		Card:                document,
		SearchText:          episode.CardSearchText(document),
		SourceLastMessageID: lastMessageID,
		LastMessageAt:       time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC),
		Version:             1, Status: model.EpisodeStatusActive,
		IndexStatus: model.EpisodeIndexIndexed,
	}
}

// The cards the model wrote are shown in the window, and what is shown is
// what the reader may read back.
func TestTheRecordedCardsReachTheWindow(t *testing.T) {
	dir := strings.TrimSpace(os.Getenv("PHASE2_CARD_REPLAY_DIR"))
	if dir == "" {
		dir = filepath.Join("..", "episode", "testdata", "card-replay")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the recorded cards: %v", err)
	}
	played := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		var recorded aRecordedCard
		if err := json.Unmarshal(raw, &recorded); err != nil {
			t.Fatalf("decode %s: %v", entry.Name(), err)
		}
		played++

		// The evidence is the session's own: the messages the card may cite.
		messages := make([]model.Message, 0, len(recorded.MessageIDs))
		for _, id := range recorded.MessageIDs {
			messages = append(messages, model.Message{ID: id, SessionID: "session-card"})
		}
		evidence := episode.CardEvidence{
			UserID: 7, Session: model.Session{ID: "session-card", UserID: 7, Title: "练习"},
			TargetMessageID: lastID(messages),
			Messages:        messages,
		}
		document, err := episode.ReceiveCardDocument(recorded.Arguments, evidence)
		if err != nil {
			if recorded.Expect == "refused" {
				continue
			}
			t.Fatalf("%s: the card was refused: %v", recorded.Scenario, err)
		}
		if recorded.Expect == "refused" {
			t.Fatalf("%s: a card that cites what it was never shown was accepted", recorded.Scenario)
		}

		// The persisted form: the row the database holds.
		card := aPersistedCard(document, "source-session", evidence.TargetMessageID)

		// The window: the product's own renderer, with the budget the card
		// path gets.
		text, ids, grants, scopes, err := renderSessionCardsWithinBudget(
			[]model.EpisodeMemory{card}, theWindowBudget, true)
		if err != nil {
			t.Fatalf("%s: the window failed: %v", recorded.Scenario, err)
		}
		if text == "" || len(ids) != 1 {
			t.Fatalf("%s: the card was received and stored but never reached the window "+
				"(ids=%d, %d tokens)", recorded.Scenario, len(ids), rag.EstimateTokens(text))
		}
		if cost := rag.EstimateTokens(text); cost > theWindowBudget {
			t.Fatalf("%s: the window shows %d tokens, over the %d it was given",
				recorded.Scenario, cost, theWindowBudget)
		}
		// The source: the display names the conversation it came from, and the
		// scope opens it.
		if !strings.Contains(text, "source_session: source-session") {
			t.Fatalf("%s: the display does not name the conversation:\n%s", recorded.Scenario, text)
		}
		if len(scopes) != 1 || scopes[0].SessionID != "source-session" || scopes[0].CardID != card.ID {
			t.Fatalf("%s: the scope is %+v", recorded.Scenario, scopes)
		}
		// The authorisation: every granted id is one the display shows, and
		// none of them is past the card's own watermark.
		for _, grant := range grants {
			if grant.CardID != card.ID || grant.CardWatermark != card.SourceLastMessageID {
				t.Fatalf("%s: the grant does not name the card: %+v", recorded.Scenario, grant)
			}
			if grant.MessageID > card.SourceLastMessageID {
				t.Fatalf("%s: the grant is past the card's watermark: %+v", recorded.Scenario, grant)
			}
			if grant.SourceSessionID != "source-session" {
				t.Fatalf("%s: the grant names the session %q", recorded.Scenario, grant.SourceSessionID)
			}
		}
		t.Logf("%s: %d entries stored, shown in %d tokens, %d ids readable",
			recorded.Scenario, len(document.Arrangements)+len(document.AssistantProducts)+len(document.OpenItems),
			rag.EstimateTokens(text), len(grants))
	}
	if played == 0 {
		t.Fatal("no card was replayed")
	}
}

// lastID is the newest message id of the evidence.
func lastID(messages []model.Message) uint {
	if len(messages) == 0 {
		return 0
	}
	return messages[len(messages)-1].ID
}

// The display of a card that does not fit is a way in, and the reader is told
// so.
//
// This is the part the independent check found missing: the cards were
// received and stored, and the window showed none of them.
func TestTheDisplayOfACardThatDoesNotFitIsAWayIn(t *testing.T) {
	card := aBigCard()
	whole := renderSessionCard(card)
	if rag.EstimateTokens(whole) <= theWindowBudget {
		t.Fatalf("the fixture is %d tokens, which fits; the test proves nothing", rag.EstimateTokens(whole))
	}
	text, ids, grants, scopes, err := renderSessionCardsWithinBudget(
		[]model.EpisodeMemory{card}, theWindowBudget, true)
	if err != nil {
		t.Fatal(err)
	}
	if text == "" || len(ids) != 1 {
		t.Fatalf("a card that does not fit was left out of the window: %q %v", text, ids)
	}
	if cost := rag.EstimateTokens(text); cost > theWindowBudget {
		t.Fatalf("the display costs %d tokens, over the %d it was given", cost, theWindowBudget)
	}
	// The source and the authorisation: the conversation is named and opened,
	// and the ids the reader may read back are the ones the display shows.
	if !strings.Contains(text, "source_session: source-session") {
		t.Fatalf("the display does not name the conversation:\n%s", text)
	}
	if len(scopes) != 1 {
		t.Fatalf("the scope is %+v", scopes)
	}
	if len(grants) == 0 {
		t.Fatal("the display granted nothing, so the reader cannot read anything back")
	}
	for _, grant := range grants {
		if grant.MessageID > card.SourceLastMessageID {
			t.Fatalf("the grant is past the card's watermark: %+v", grant)
		}
	}
	// And the reader is told that there is more and how to reach it.
	if !strings.Contains(text, "more entries") {
		t.Fatalf("the display does not say what was left out:\n%s", text)
	}
}
