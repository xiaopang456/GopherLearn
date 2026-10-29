package repositorytest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/repository"
)

// TopicMemorySuite is the behavioural contract for one conversation's topic-memory consent. It
// runs against every store for the same reason the conversation and memory suites do: the read
// side decides whether a learner's memories reach a model, so "no row means off" and "committing
// the same answer twice changes nothing" cannot be one store's private interpretation.
//
// What is deliberately absent from this suite is any mode rule. Whether a chat conversation may
// hold this permission is decided by the service and checked again by the recall grant, exactly as
// the quiz workflow splits ownership (store) from mode (service); a store that refused modes would
// grow a second, possibly different, answer to that question.
func TopicMemorySuite(t *testing.T, newStore Factory) {
	t.Helper()
	ctx := context.Background()

	t.Run("a conversation nobody asked about reads as off", func(t *testing.T) {
		store := newStore(t)
		userID := mustUser(t, store, "consent-owner")
		sessionID := mustSession(t, store, userID)

		preference, err := store.GetTopicMemoryPreference(ctx, userID, sessionID)
		if err != nil {
			t.Fatalf("an unanswered consent must read as off, not as a failure: %v", err)
		}
		if preference.Enabled {
			t.Fatal("a conversation with no stored decision reads as enabled")
		}
		if preference.UserID != userID || preference.SessionID != sessionID {
			t.Fatalf("the off answer must still name whose conversation it is: %+v", preference)
		}
		if !preference.CreatedAt.IsZero() || !preference.UpdatedAt.IsZero() {
			t.Fatalf("a conversation that was never asked carries a decision time: %+v", preference)
		}
	})

	t.Run("yes is stored and reads back", func(t *testing.T) {
		store := newStore(t)
		userID := mustUser(t, store, "consent-yes")
		sessionID := mustSession(t, store, userID)

		committed, err := store.CommitTopicMemoryPreference(ctx, userID, sessionID, true)
		if err != nil {
			t.Fatal(err)
		}
		if committed == nil || !committed.Enabled || committed.UserID != userID || committed.SessionID != sessionID {
			t.Fatalf("commit returned %+v, want the stored yes", committed)
		}
		if committed.CreatedAt.IsZero() || committed.UpdatedAt.IsZero() {
			t.Fatalf("a stored decision with no times cannot answer when it was made: %+v", committed)
		}
		read, err := store.GetTopicMemoryPreference(ctx, userID, sessionID)
		if err != nil {
			t.Fatal(err)
		}
		if !read.Enabled || !read.UpdatedAt.Equal(committed.UpdatedAt) {
			t.Fatalf("read back %+v, want the committed yes at %v", read, committed.UpdatedAt)
		}
	})

	t.Run("committing the same answer twice changes nothing", func(t *testing.T) {
		store := newStore(t)
		userID := mustUser(t, store, "consent-idempotent")
		sessionID := mustSession(t, store, userID)

		first, err := store.CommitTopicMemoryPreference(ctx, userID, sessionID, true)
		if err != nil {
			t.Fatal(err)
		}
		// Wait until the clock has genuinely moved past the stored decision time before asking
		// for the same answer again. Without this, "the timestamp did not move" and "the clock
		// never advanced far enough to move" are the same observation, and an engine whose ticks
		// are coarse (Windows reports ~16ms) would let a store that rewrites on every commit pass
		// for the one that does not. This is a wait on the clock, not a sleep: it ends as soon as
		// the assertion below could tell the two apart.
		deadline := time.Now().Add(2 * time.Second)
		for !time.Now().UTC().After(first.UpdatedAt) {
			if time.Now().After(deadline) {
				t.Fatalf("the clock never advanced past %v, so the idempotence claim below cannot be "+
					"decided on this machine", first.UpdatedAt)
			}
		}
		repeated, err := store.CommitTopicMemoryPreference(ctx, userID, sessionID, true)
		if err != nil {
			t.Fatal(err)
		}
		if repeated.Enabled != first.Enabled || !repeated.CreatedAt.Equal(first.CreatedAt) ||
			!repeated.UpdatedAt.Equal(first.UpdatedAt) {
			t.Fatalf("a repeat of the same answer rewrote the record: %+v then %+v", first, repeated)
		}
		// The record a conversation ends up with after being asked the same thing twice is the
		// record from the first time: no write happened, so no timestamp moved. The wait above is
		// what makes that a decision rather than a coincidence - without it, a store that rewrote
		// the row on every commit would look identical to one that does not whenever the clock had
		// not ticked in between.
		if !repeated.CreatedAt.Equal(repeated.UpdatedAt) {
			t.Fatalf("a decision that changed nothing was recorded as if it had: %+v", repeated)
		}
		// Off is the same kind of answer, and it must converge too. The value, not the clock, is
		// what the recall path reads, so that is what a store must agree on.
		off, err := store.CommitTopicMemoryPreference(ctx, userID, sessionID, false)
		if err != nil {
			t.Fatal(err)
		}
		// Same rule for the other direction, because "off" is the answer a learner is most likely
		// to click twice - and it is the one a struct-form update would silently not write at all.
		offDeadline := time.Now().Add(2 * time.Second)
		for !time.Now().UTC().After(off.UpdatedAt) {
			if time.Now().After(offDeadline) {
				t.Fatalf("the clock never advanced past %v, so the repeated no cannot be judged", off.UpdatedAt)
			}
		}
		offAgain, err := store.CommitTopicMemoryPreference(ctx, userID, sessionID, false)
		if err != nil {
			t.Fatal(err)
		}
		if off.Enabled || offAgain.Enabled || !offAgain.UpdatedAt.Equal(off.UpdatedAt) {
			t.Fatalf("repeated no did not converge: %+v then %+v", off, offAgain)
		}
		// Turning it back on keeps the original decision time: "when did you first let this
		// conversation read topic memory" must survive the learner changing their mind, and
		// CreatedAt is what makes that question answerable later.
		reopened, err := store.CommitTopicMemoryPreference(ctx, userID, sessionID, true)
		if err != nil {
			t.Fatal(err)
		}
		if !reopened.Enabled || !reopened.CreatedAt.Equal(first.CreatedAt) {
			t.Fatalf("reopen rewrote the history it was supposed to keep: first %v, now %+v", first.CreatedAt, reopened)
		}
	})

	t.Run("another account's conversation reads and writes the same as a missing one", func(t *testing.T) {
		store := newStore(t)
		owner := mustUser(t, store, "consent-real")
		intruder := mustUser(t, store, "consent-intruder")
		sessionID := mustSession(t, store, owner)

		if _, err := store.CommitTopicMemoryPreference(ctx, intruder, sessionID, true); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("writing consent into a foreign conversation returned %v, want ErrNotFound", err)
		}
		if _, err := store.GetTopicMemoryPreference(ctx, intruder, sessionID); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("reading a foreign conversation's consent returned %v, want ErrNotFound", err)
		}
		// A conversation that never existed must not be distinguishable, or this becomes a way
		// to enumerate which sessions another account has.
		if _, err := store.GetTopicMemoryPreference(ctx, intruder, "session-never-created"); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("a missing conversation returned %v, want the same ErrNotFound", err)
		}
		// And the owner's answer is untouched by either attempt.
		stored, err := store.GetTopicMemoryPreference(ctx, owner, sessionID)
		if err != nil || stored.Enabled {
			t.Fatalf("a refused write still changed the owner's consent: %+v (%v)", stored, err)
		}
	})

	t.Run("the store does not decide which modes may hold a consent", func(t *testing.T) {
		store := newStore(t)
		userID := mustUser(t, store, "consent-chat")
		chatID := mustSession(t, store, userID)
		agentID := mustTopicMemorySession(t, store, userID, domain.SessionModeLearningAgent)

		for _, sessionID := range []string{chatID, agentID} {
			if _, err := store.CommitTopicMemoryPreference(ctx, userID, sessionID, true); err != nil {
				t.Fatalf("the store refused a consent on %s: %v", sessionID, err)
			}
		}
		// Two conversations, two independent decisions: flipping the switch in one must not be
		// readable from the other, which is what makes this a per-conversation consent.
		other := mustTopicMemorySession(t, store, userID, domain.SessionModeLearningAgent)
		read, err := store.GetTopicMemoryPreference(ctx, userID, other)
		if err != nil {
			t.Fatal(err)
		}
		if read.Enabled {
			t.Fatal("a consent set on one conversation leaked to another")
		}
	})

	t.Run("deleting the conversation deletes the decision", func(t *testing.T) {
		store := newStore(t)
		userID := mustUser(t, store, "consent-delete")
		sessionID := mustTopicMemorySession(t, store, userID, domain.SessionModeLearningAgent)
		if _, err := store.CommitTopicMemoryPreference(ctx, userID, sessionID, true); err != nil {
			t.Fatal(err)
		}
		if err := store.DeleteSession(ctx, userID, sessionID); err != nil {
			t.Fatal(err)
		}
		if _, err := store.GetTopicMemoryPreference(ctx, userID, sessionID); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("after the conversation is gone the consent reads %v, want ErrNotFound", err)
		}
	})

	t.Run("an empty identity is refused before anything is read", func(t *testing.T) {
		store := newStore(t)
		userID := mustUser(t, store, "consent-blank")
		sessionID := mustSession(t, store, userID)
		for _, pair := range [][2]string{{"", sessionID}, {userID, ""}, {"   ", sessionID}, {userID, "   "}} {
			if _, err := store.GetTopicMemoryPreference(ctx, pair[0], pair[1]); !errors.Is(err, repository.ErrInvalidArgument) {
				t.Fatalf("get(%q,%q) = %v, want ErrInvalidArgument", pair[0], pair[1], err)
			}
			if _, err := store.CommitTopicMemoryPreference(ctx, pair[0], pair[1], true); !errors.Is(err, repository.ErrInvalidArgument) {
				t.Fatalf("commit(%q,%q) = %v, want ErrInvalidArgument", pair[0], pair[1], err)
			}
		}
	})
}

// mustTopicMemorySession creates one conversation of a given mode, because a consent is scoped by
// conversation and the suite has to talk about both modes.
func mustTopicMemorySession(t *testing.T, store repository.Repository, userID string, mode domain.SessionMode) string {
	t.Helper()
	session, err := store.CreateSession(context.Background(), &domain.Session{UserID: userID, Title: "会话", Mode: mode})
	if err != nil {
		t.Fatal(err)
	}
	return session.ID
}
