package agent

import (
	"strings"
	"testing"
)

// The lock's reach.
//
// The learner said which conversation they meant, the server checked that it
// is theirs, and it read that conversation's record. The turn may read THAT
// conversation and no other - not the conversations the window's cards
// opened, and not the ones a tool finds.
//
// It is a restriction and not a permission, and the difference is visible
// here: without the lock the same run may open three conversations, and with
// it exactly one.

// A run with a lock opens exactly one earlier conversation.
//
// The other two are the learner's own conversations and one of them has a
// card, so nothing but the lock stops the run from reading them.
func TestTheLockOpensExactlyOneEarlierConversation(t *testing.T) {
	run := RunContext{UserID: 7, SessionID: "current", LockedSessionID: "locked-session"}
	run.CardScopes = []CardScope{
		{SessionID: "locked-session", CardID: "card-locked", CardVersion: 2, CardWatermark: 144},
		{SessionID: "other-session", CardID: "card-other", CardVersion: 3, CardWatermark: 99},
	}
	grants := NewHistoryGrantSet(run, nil)

	if !grants.AuthorisesSession(run, "locked-session") {
		t.Fatal("the locked conversation is not readable")
	}
	if grants.AuthorisesSession(run, "other-session") {
		t.Fatal("a conversation the learner also owns is readable beside the lock")
	}
	if grants.AuthorisesSession(run, "some-third-session") {
		t.Fatal("a conversation nobody opened is readable")
	}
	// The run's own conversation is still readable: the lock is about the
	// earlier conversations, not about the present.
	if !grants.AuthorisesSession(run, "current") {
		t.Fatal("the run's own conversation is not readable")
	}
	// The card that opened the other conversation is not a way around the
	// lock.
	cardID, _, _, ok := grants.CardAuthorisation("other-session")
	if ok || cardID != "" {
		t.Fatalf("the other conversation carries an authorisation: %q %v", cardID, ok)
	}
}

// A tool that finds another conversation does not widen the lock.
//
// The card search is the case: it returns candidates, and a candidate is
// opened for the rest of the turn. With a lock in effect, the candidate is
// returned but NOT opened - the learner has already said which conversation
// they meant.
func TestAToolThatFindsAnotherConversationDoesNotWidenTheLock(t *testing.T) {
	run := RunContext{UserID: 7, SessionID: "current", LockedSessionID: "locked-session"}
	grants := NewHistoryGrantSet(run, nil)

	grants.ExtendCardScope(run, CardScope{
		SessionID: "other-session", CardID: "card-other", CardVersion: 3, CardWatermark: 99,
	})
	if grants.AuthorisesSession(run, "other-session") {
		t.Fatal("a tool opened a second conversation beside the lock")
	}
	// The locked conversation may still be opened - by its own scope.
	grants.ExtendCardScope(run, CardScope{
		SessionID: "locked-session", CardID: "card-locked", CardVersion: 2, CardWatermark: 144,
	})
	if !grants.AuthorisesSession(run, "locked-session") {
		t.Fatal("the locked conversation could not be opened by its own scope")
	}
}

// A grant out of another conversation is refused while the lock is in effect.
//
// The grant set is the authority for the WHOLE turn, not only for the
// conversations it opened: a tool cannot widen the turn's reach by handing
// the set an id it found somewhere else.
func TestAGrantOutOfAnotherConversationIsRefusedWhileTheLockIsInEffect(t *testing.T) {
	run := RunContext{UserID: 7, SessionID: "current", LockedSessionID: "locked-session"}
	grants := NewHistoryGrantSet(run, nil)

	grants.Extend(run, []HistoryMessageGrant{
		{MessageID: 11, SourceKind: HistorySourceSessionSearch, SourceSessionID: "other-session"},
	})
	if len(grants.Grants(11)) != 0 {
		t.Fatal("an id from another conversation was granted beside the lock")
	}
	// The locked conversation's own ids are granted: the server read that
	// conversation after checking that it is the learner's.
	grants.Extend(run, []HistoryMessageGrant{
		{MessageID: 21, SourceKind: HistorySourceSessionSearch, SourceSessionID: "locked-session"},
	})
	if len(grants.Grants(21)) != 1 {
		t.Fatal("an id from the locked conversation was not granted")
	}
}

// The lock survives a run without any card.
//
// The locked conversation may have no card at all: the learner may have picked
// it from their OWN list, which no search had offered. A grant set that
// required a card would refuse that conversation, and the evidence the server
// read would be unreadable.
func TestTheLockedConversationNeedsNoCardToBeReadable(t *testing.T) {
	run := RunContext{UserID: 7, SessionID: "current", LockedSessionID: "locked-session"}
	grants := NewHistoryGrantSet(run, nil)

	if !grants.AuthorisesSession(run, "locked-session") {
		t.Fatal("the locked conversation is not readable")
	}
	cardID, version, watermark, ok := grants.CardAuthorisation("locked-session")
	if !ok {
		t.Fatal("the locked conversation carries no authorisation")
	}
	if cardID != "" || version != 0 || watermark != 0 {
		t.Fatalf("the locked conversation carries a card's identity: %q %d %d", cardID, version, watermark)
	}
	// A read taken in it is bounded by the ownership check and not by a card
	// that never described it.
	grant := HistoryMessageGrant{
		MessageID: 31, SourceKind: HistorySourceSessionSearch, SourceSessionID: "locked-session",
	}
	if !grant.UsableIn(run) {
		t.Fatal("an id of the locked conversation is not readable")
	}
}

// Without a lock nothing changes.
//
// Every rule above is about the locked turn, and a turn with no lock is the
// turn it was before.
func TestWithoutALockNothingChanges(t *testing.T) {
	run := RunContext{UserID: 7, SessionID: "current"}
	run.CardScopes = []CardScope{
		{SessionID: "source-session", CardID: "card-1", CardVersion: 2, CardWatermark: 144},
	}
	grants := NewHistoryGrantSet(run, nil)

	if !grants.AuthorisesSession(run, "source-session") {
		t.Fatal("the conversation a card opened is not readable")
	}
	grants.ExtendCardScope(run, CardScope{
		SessionID: "found-session", CardID: "card-2", CardVersion: 3, CardWatermark: 99,
	})
	if !grants.AuthorisesSession(run, "found-session") {
		t.Fatal("a conversation a tool found is not readable")
	}
	if grants.LockedSession() != "" {
		t.Fatalf("a turn with no lock reports one: %q", grants.LockedSession())
	}
}

// The lock is reported, so that a reader can say what the turn was about.
//
// It is one line of the run's own record: which conversation the learner's
// answer pointed at.
func TestTheLockIsReported(t *testing.T) {
	run := RunContext{UserID: 7, SessionID: "current", LockedSessionID: "locked-session"}
	grants := NewHistoryGrantSet(run, nil)
	if grants.LockedSession() != "locked-session" {
		t.Fatalf("the lock is %q", grants.LockedSession())
	}
}

// The run's own conversation is not an earlier one.
//
// A lock on the run's own conversation would restrict the run to nothing at
// all, so it is dropped rather than obeyed.
func TestALockOnTheRunsOwnConversationIsDropped(t *testing.T) {
	run := RunContext{UserID: 7, SessionID: "current", LockedSessionID: "current"}
	grants := NewHistoryGrantSet(run, nil)
	if grants.LockedSession() != "" {
		t.Fatalf("the lock is %q", grants.LockedSession())
	}
}

// The evidence block is the locked conversation's, and the window says so.
//
// It is what the model reads instead of opening candidates one by one.
func TestTheLockedEvidenceBlockNamesTheConversation(t *testing.T) {
	locked := &LockedSession{SessionID: "locked-session", Title: "洄漪",
		Evidence: "第二道练的是读写拆分"}
	block := renderLockedSessionBlock(locked, "上次那三道题里，第二道练的是什么？")
	if !strings.Contains(block, "locked-session") {
		t.Fatalf("the block does not name the conversation: %s", block)
	}
	if !strings.Contains(block, "读写拆分") {
		t.Fatalf("the block does not carry the evidence: %s", block)
	}
}
