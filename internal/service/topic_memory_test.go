package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/repository"
)

// TestCommitTopicMemoryRefusesEveryConversationItShould is the write side of the switch. Two
// properties are checked together because they are the two ways a consent surface can leak: the
// mode rule must refuse an ordinary chat conversation, and the ownership rule must refuse another
// account's conversation in exactly the shape it refuses one that never existed - so being told
// "no" here never tells anybody anything about what is on the other side of the boundary.
func TestCommitTopicMemoryRefusesEveryConversationItShould(t *testing.T) {
	ctx := context.Background()
	fixture := newTopicFixture(t)
	sessions := NewSessionService(fixture.store)

	// (a) A chat conversation cannot hold the switch, whatever else it has.
	quiz := fixture.openQuiz(t, fixture.userID, fixture.sessionID, "redis", time.Now().Add(time.Hour))
	if _, err := sessions.CommitTopicMemory(ctx, fixture.userID, fixture.sessionID, true); !errors.Is(err, ErrTopicMemoryNeedsLearningAgent) {
		t.Fatalf("a chat conversation was switched on (quiz %s open on it): %v", quiz.ID, err)
	}
	// A live quiz is not a reason either: the two conditions are independent, and neither one
	// alone opens the door.
	if _, err := sessions.CommitTopicMemory(ctx, fixture.userID, fixture.secondSess, true); !errors.Is(err, ErrTopicMemoryNeedsLearningAgent) {
		t.Fatalf("a second chat conversation was switched on: %v", err)
	}

	// (b) Foreign and missing conversations are the same answer, for the write verb.
	foreignWrite, foreignErr := sessions.CommitTopicMemory(ctx, fixture.userID, fixture.intruderAgent, true)
	missingWrite, missingErr := sessions.CommitTopicMemory(ctx, fixture.userID, "session-never-created", true)
	if foreignWrite != nil || missingWrite != nil {
		t.Fatalf("a consent write succeeded on a conversation the caller does not own: %+v / %+v",
			foreignWrite, missingWrite)
	}
	if !errors.Is(foreignErr, ErrNotFound) || !errors.Is(missingErr, ErrNotFound) {
		t.Fatalf("foreign=%v, missing=%v, want both to be the same not-found refusal", foreignErr, missingErr)
	}
	if foreignErr.Error() != missingErr.Error() {
		t.Fatalf("the two refusals differ, which is a way to enumerate conversations: %q vs %q", foreignErr, missingErr)
	}
	// And a read of the same pair refuses to distinguish them too, so the switch cannot be
	// probed from either side.
	if _, err := fixture.store.GetTopicMemoryPreference(ctx, fixture.userID, fixture.intruderAgent); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("reading a foreign conversation's consent returned %v", err)
	}
	if _, err := fixture.store.GetTopicMemoryPreference(ctx, fixture.userID, "session-never-created"); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("reading a missing conversation's consent returned %v", err)
	}

	// (c) The owner's own learning conversation is accepted, and nothing was written to the
	// conversations refused above.
	if _, err := sessions.CommitTopicMemory(ctx, fixture.userID, fixture.learnerAgent, true); err != nil {
		t.Fatal(err)
	}
	for _, sessionID := range []string{fixture.sessionID, fixture.secondSess, fixture.intruderAgent} {
		stored, err := fixture.store.GetTopicMemoryPreference(ctx, fixture.userID, sessionID)
		if err != nil {
			if errors.Is(err, repository.ErrNotFound) {
				continue
			}
			t.Fatal(err)
		}
		if stored.Enabled {
			t.Fatalf("a refused conversation ended up switched on: %s", sessionID)
		}
	}
}

// TestGrantDoesNotMoveWhenTheSameAnswerIsCommittedAgain is the recall-level half of idempotence.
// The store already refuses to rewrite a record that says the same thing; what a learner has to be
// able to trust is that clicking twice cannot change what this conversation is allowed to read -
// or how the read describes itself.
func TestGrantDoesNotMoveWhenTheSameAnswerIsCommittedAgain(t *testing.T) {
	fixture := newTopicFixture(t)
	seeded := fixture.seedTopicMemories(t)
	fixture.openQuiz(t, fixture.userID, fixture.learnerAgent, "redis", time.Now().Add(time.Hour))
	access := fixture.topicAccess(t, wideOptions())

	// Nothing committed yet: the baseline the two later reads must return to.
	before, beforeNote := recall(t, access, fixture.userID, fixture.learnerAgent, "fsync")
	fixture.switchTopicMemory(t, fixture.userID, fixture.learnerAgent, true)
	first, firstNote := recall(t, access, fixture.userID, fixture.learnerAgent, "fsync")
	if !recalled(first, seeded.redisTopic.MemoryID) {
		t.Fatalf("the first yes changed nothing: %s", memoryIDs(first))
	}

	for _, repeat := range []string{"second", "third"} {
		fixture.switchTopicMemory(t, fixture.userID, fixture.learnerAgent, true)
		hits, note := recall(t, access, fixture.userID, fixture.learnerAgent, "fsync")
		if memoryIDs(hits) != memoryIDs(first) || note != firstNote {
			t.Fatalf("the %s repeat of the same yes changed the read: %s/%q vs %s/%q",
				repeat, memoryIDs(hits), note, memoryIDs(first), firstNote)
		}
	}

	// Turning it off, then off again, returns to the baseline exactly - which is the property a
	// learner checking "did that work" would look at.
	fixture.switchTopicMemory(t, fixture.userID, fixture.learnerAgent, false)
	off, offNote := recall(t, access, fixture.userID, fixture.learnerAgent, "fsync")
	fixture.switchTopicMemory(t, fixture.userID, fixture.learnerAgent, false)
	again, againNote := recall(t, access, fixture.userID, fixture.learnerAgent, "fsync")
	if memoryIDs(off) != memoryIDs(before) || offNote != beforeNote {
		t.Fatalf("switching off did not restore the default read:\n off  %s / %q\n want %s / %q",
			memoryIDs(off), offNote, memoryIDs(before), beforeNote)
	}
	if memoryIDs(again) != memoryIDs(off) || againNote != offNote {
		t.Fatalf("a repeat of no changed the read: %s/%q vs %s/%q", memoryIDs(again), againNote, memoryIDs(off), offNote)
	}
	// The base layers are present in all of it: switching off removes a subject, not the
	// learner's own global facts and this conversation's episodes.
	if !recalled(off, seeded.global.MemoryID) || !recalled(off, seeded.episodeHere.MemoryID) {
		t.Fatalf("the default read lost a layer it had before: %s", memoryIDs(off))
	}
	if strings.Contains(offNote, "已授权主题") {
		t.Fatalf("the scope note still claims a subject after the switch went off: %q", offNote)
	}
}

// TestConsentReadFailureEndsTheReadInsteadOfWideningIt puts the failure in the middle gate. The
// two directions a broken consent read could go are both wrong: treating it as "off" would make a
// database fault look like a learner who never asked, and treating it as "on" would read topic
// memory on the strength of an error. Neither happens; the read ends.
func TestConsentReadFailureEndsTheReadInsteadOfWideningIt(t *testing.T) {
	fixture := newTopicFixture(t)
	fixture.seedTopicMemories(t)
	fixture.studyOnOneSubject(t, "redis")
	spy := &failingTopicStore{Repository: fixture.store, consentErr: "database is down"}
	grant, err := NewLearningTopicGrant(spy)
	if err != nil {
		t.Fatal(err)
	}
	hits, note, err := fixture.accessWith(t, grant, wideOptions()).Recall(context.Background(), MemoryQuery{
		UserID: fixture.userID, SessionID: fixture.learnerAgent, Query: "fsync",
	})
	if err == nil {
		t.Fatalf("a failed consent read produced %s / %q, want an error", memoryIDs(hits), note)
	}
	if len(hits) != 0 || note != "" {
		t.Fatalf("a failed consent read still returned %d hits and a note", len(hits))
	}
	if spy.reads != 0 {
		t.Fatalf("a failed consent read went on to list memories %d times", spy.reads)
	}
}

// TestABlankAuthorizedKeyQueriesNothing is the executable version of a claim the mutation report
// otherwise has to make in prose. One guard in the grant - "a quiz with no topic key grants
// nothing" - is redundant, because the recall layer drops blank keys through normalizeKeys before
// it decides whether a grant is empty and before it issues any query. So removing that guard changes
// no output, no scope note and, as this test counts, no store reads.
//
// It is not a test with teeth against that one mutation - nothing can be, for a change that is
// provably inert - but it is the reason the survivor in the mutation report is an equivalence
// rather than a gap: the property is checked where it is actually decided.
func TestABlankAuthorizedKeyQueriesNothing(t *testing.T) {
	fixture := newTopicFixture(t)
	fixture.seedTopicMemories(t)
	spy := &failingTopicStore{Repository: fixture.store}

	for name, grant := range map[string]MemoryGrant{
		"no keys at all":  {},
		"one blank key":   {TopicKeys: []string{"   "}},
		"blank and empty": {TopicKeys: []string{"", "\t", " "}},
	} {
		access := fixture.accessWith(t, fixedGrant{grant: grant}, wideOptions())
		hits, note, err := access.Recall(context.Background(), MemoryQuery{
			UserID: fixture.userID, SessionID: fixture.learnerAgent, Query: "fsync",
		})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(hits) != 0 {
			t.Fatalf("%s: a blank subject authorized %d memories", name, len(hits))
		}
		if strings.Contains(note, "已授权主题") {
			t.Fatalf("%s: the note claims a subject nobody named: %q", name, note)
		}
	}
	if spy.reads != 0 {
		t.Fatalf("blank subject keys cost %d store reads, want none", spy.reads)
	}
}
