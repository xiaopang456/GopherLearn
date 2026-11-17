package agent

import (
	"GopherAI/model"
	"testing"
	"time"
)

// cardRow builds one stored session card.
func cardRow(id, sessionID string, arrangements, products []model.SessionCardEntry) model.EpisodeMemory {
	return model.EpisodeMemory{
		ID: id, UserID: 7, SessionID: sessionID,
		ContentKind: model.EpisodeContentSessionCard,
		Title:       "Go 并发练习",
		Card: model.SessionCardDocument{
			Version:           model.SessionCardContentVersion,
			Title:             "Go 并发练习",
			Synopsis:          "讨论了每轮题量，助手给了三道练习。",
			Arrangements:      arrangements,
			AssistantProducts: products,
		},
		SearchText:          "Go 并发练习 讨论了每轮题量",
		SourceLastMessageID: 144, LastMessageAt: time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC),
		Version: 2, Status: model.EpisodeStatusActive,
		IndexStatus: model.EpisodeIndexIndexed,
	}
}

func TestSessionCardRendersTheSourceSessionAndTheStatuses(t *testing.T) {
	card := cardRow("card-1", "source-session",
		[]model.SessionCardEntry{{Text: "每轮9道题", Status: model.SessionCardStatusStated, MessageIDs: []uint{12}}},
		[]model.SessionCardEntry{{Text: "三道练习", Status: model.SessionCardStatusAssistantProposed, MessageIDs: []uint{13}, SegmentSeqs: []int{2}}},
	)
	text, ids, grants, scopes, err := renderSessionCardsWithinBudget([]model.EpisodeMemory{card}, 1000, true)
	if err != nil {
		t.Fatal(err)
	}
	if text == "" || len(ids) != 1 || ids[0] != "card-1" {
		t.Fatalf("the card was not rendered: %q %v", text, ids)
	}
	// The scope is what opens the source conversation, and it is issued for
	// every card the window shows - not only for the ones that cite a raw
	// message.
	if len(scopes) != 1 || scopes[0].SessionID != "source-session" ||
		scopes[0].CardID != "card-1" || scopes[0].CardVersion != 2 {
		t.Fatalf("the card scope is %+v", scopes)
	}
	for _, want := range []string{"source_session: source-session", "arrangement: 每轮9道题",
		"assistant_product: 三道练习", "status=assistant_proposed", "message_ids=13", "segments=2"} {
		if !cardContains(text, want) {
			t.Fatalf("the card block does not carry %q:\n%s", want, text)
		}
	}
	if len(grants) != 2 {
		t.Fatalf("the card granted %d ids, want the two it cited: %+v", len(grants), grants)
	}
	for _, grant := range grants {
		if grant.SourceKind != HistorySourceSessionCard {
			t.Fatalf("the grant says %q", grant.SourceKind)
		}
		if grant.SourceSessionID != "source-session" {
			t.Fatalf("the grant names the session %q", grant.SourceSessionID)
		}
		if grant.CardID != "card-1" || grant.CardVersion != 2 || grant.CardWatermark != 144 {
			t.Fatalf("the grant does not name the card: %+v", grant)
		}
	}
}

// The header has to say what a card is, because the difference between "what
// was said then" and "what is required now" is the whole point of the format.
func TestSessionCardHeaderSeparatesThePastFromThePresent(t *testing.T) {
	if !cardContains(sessionCardHeader, "not what is required now") {
		t.Fatalf("the header does not separate the past from the present: %q", sessionCardHeader)
	}
}

// A card that does not fit is dropped whole rather than cut in half, and the
// ids of a cut card are ids the model never saw.
func TestSessionCardBudgetDropsWholeCards(t *testing.T) {
	card := cardRow("card-1", "source-session", []model.SessionCardEntry{
		{Text: "每轮9道题", Status: model.SessionCardStatusStated, MessageIDs: []uint{12}},
	}, nil)
	text, ids, grants, scopes, err := renderSessionCardsWithinBudget([]model.EpisodeMemory{card}, 4, true)
	if err != nil {
		t.Fatal(err)
	}
	if text != "" || len(ids) != 0 || len(grants) != 0 || len(scopes) != 0 {
		t.Fatalf("a card that did not fit was shown: %q %v %v %v", text, ids, grants, scopes)
	}
}

// A card that opens the run's own session grants nothing extra.
//
// The exclusion of the current session's own card lives in the store, and
// this test states the rule the authorisation set implements: a grant set
// built from a card about the current session may not name that session as
// an earlier conversation, or the run would read its own present as if it
// were its past.
func TestSessionCardGrantAboutTheCurrentSessionOpensNoEarlierSession(t *testing.T) {
	card := cardRow("card-1", "current-session", []model.SessionCardEntry{
		{Text: "每轮9道题", Status: model.SessionCardStatusStated, MessageIDs: []uint{12}},
	}, nil)
	if card.SessionID != "current-session" {
		t.Fatal("the fixture is wrong")
	}
	run := RunContext{UserID: 7, SessionID: "current-session"}
	grants := NewHistoryGrantSet(run, []HistoryMessageGrant{{
		MessageID: 12, SourceKind: HistorySourceSessionCard,
		SourceSessionID: "current-session", CardID: "card-1", CardVersion: 2, CardWatermark: 144,
	}})
	if !grants.AuthorisesSession(run, "current-session") {
		t.Fatal("the run's own session is not readable")
	}
	if grants.AuthorisesSession(run, "other-session") {
		t.Fatal("a session no card pointed at is readable")
	}
	// The current session is not listed as an earlier conversation either:
	// the list is what says "this one is from before". The scope of a card
	// about the run's own conversation is refused for the same reason, and
	// the refusal is what this asserts - so another session's scope is set
	// as well, and the list is shown to be non-empty.
	grants.SetCardScope(CardScope{SessionID: "current-session", CardID: "card-1",
		CardVersion: 2, CardWatermark: 144})
	grants.SetCardScope(CardScope{SessionID: "other-session", CardID: "card-2",
		CardVersion: 3, CardWatermark: 220})
	sessions := grants.CardSessions()
	if len(sessions) != 1 || sessions[0] != "other-session" {
		t.Fatalf("the card sessions are %v", sessions)
	}
	for _, session := range sessions {
		if session == "current-session" {
			t.Fatalf("the run's own session is listed as an earlier one: %v", sessions)
		}
	}
}

// The rule is enforced on the production path as well.
//
// The scopes reach the run through the window, and the grant set reads them
// from there - not through the setter the other test uses. A scope naming the
// run's own conversation has to be refused on that path too, or the list of
// earlier conversations could contain the conversation being answered.
func TestTheWindowScopesCannotNameTheRunsOwnConversation(t *testing.T) {
	run := RunContext{UserID: 7, SessionID: "current-session"}
	run.CardScopes = []CardScope{
		{SessionID: "current-session", CardID: "card-1", CardVersion: 2, CardWatermark: 144},
		{SessionID: "source-session", CardID: "card-2", CardVersion: 3, CardWatermark: 220},
	}
	grants := NewHistoryGrantSet(run, nil)
	sessions := grants.CardSessions()
	if len(sessions) != 1 || sessions[0] != "source-session" {
		t.Fatalf("the card sessions are %v", sessions)
	}
}

// The card's own watermark bounds what it authorises.
func TestSessionCardGrantIsBoundedByTheCardWatermark(t *testing.T) {
	run := RunContext{UserID: 7, SessionID: "current-session"}
	grants := NewHistoryGrantSet(run, []HistoryMessageGrant{{
		MessageID: 200, SourceKind: HistorySourceSessionCard,
		SourceSessionID: "source-session", CardID: "card-1", CardVersion: 2, CardWatermark: 144,
	}})
	if len(grants.Grants(200)) != 0 {
		t.Fatal("a message above the card's watermark was granted")
	}
}

func TestSessionCardGrantSetNamesTheSourceSessions(t *testing.T) {
	run := RunContext{UserID: 7, SessionID: "current-session"}
	grants := NewHistoryGrantSet(run, []HistoryMessageGrant{
		{MessageID: 12, SourceKind: HistorySourceSessionCard, SourceSessionID: "source-session",
			CardID: "card-1", CardVersion: 2, CardWatermark: 144},
		{MessageID: 13, SourceKind: HistorySourceSessionCard, SourceSessionID: "source-session",
			CardID: "card-1", CardVersion: 2, CardWatermark: 144},
	})
	grants.SetCardScope(CardScope{SessionID: "source-session", CardID: "card-1",
		CardVersion: 2, CardWatermark: 144})
	sessions := grants.CardSessions()
	if len(sessions) != 1 || sessions[0] != "source-session" {
		t.Fatalf("the card sessions are %v", sessions)
	}
	cardID, version, watermark, ok := grants.CardAuthorisation("source-session")
	if !ok || cardID != "card-1" || version != 2 || watermark != 144 {
		t.Fatalf("the card authorisation is %q %d %d %v", cardID, version, watermark, ok)
	}
}

// A card that cites summary segments and no raw message still opens its
// conversation.
//
// This is the case a grant set built from the message grants would get
// wrong: such a card grants no message at all, so the conversation it points
// at would look unreachable and the whole chain would stop at the card.
func TestACardThatCitesNoRawMessageStillOpensItsConversation(t *testing.T) {
	run := RunContext{UserID: 7, SessionID: "current-session"}
	grants := NewHistoryGrantSet(run, nil)
	grants.cardScopes["source-session"] = CardScope{
		SessionID: "source-session", CardID: "card-1", CardVersion: 2, CardWatermark: 144,
	}
	if !grants.AuthorisesSession(run, "source-session") {
		t.Fatal("a conversation a card opened is not reachable")
	}
	sessions := grants.CardSessions()
	if len(sessions) != 1 || sessions[0] != "source-session" {
		t.Fatalf("the card sessions are %v", sessions)
	}
	if grants.AuthorisesSession(run, "other-session") {
		t.Fatal("a conversation no card opened is reachable")
	}
}

// The card and the Episode are different documents, and a row of the wrong
// kind must not be rendered as a card.
func TestSessionCardRefusesAnEpisodeRow(t *testing.T) {
	row := model.EpisodeMemory{ID: "episode-1", Title: "一个 Episode", ContentKind: model.EpisodeContentEpisode}
	if block, _ := renderSessionCard(row); block != "" {
		t.Fatal("an Episode row was rendered as a card")
	}
}

func cardContains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
