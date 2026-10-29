package repository

import (
	"context"
	"testing"

	"github.com/xiaopang456/GopherLearn/internal/domain"
)

// TestDeletingAConversationReleasesItsConsentRecord is the same claim the shared suite makes
// behaviourally - "after the conversation is gone, nothing reads as authorized" - stated where it
// can actually be seen. Through the public API an orphaned row is invisible: every read is gated
// behind the conversation existing. So the leak this catches is not a wrong answer, it is a
// permission record that outlived the thing it was about, which is exactly the kind of row a later
// phase would find and re-adopt.
func TestDeletingAConversationReleasesItsConsentRecord(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryRepository()
	user, err := store.CreateUser(ctx, &domain.User{Email: "consent-release@example.com", DisplayName: "Owner"})
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.CreateSession(ctx, &domain.Session{UserID: user.ID, Title: "one", Mode: domain.SessionModeLearningAgent})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.CreateSession(ctx, &domain.Session{UserID: user.ID, Title: "two", Mode: domain.SessionModeLearningAgent})
	if err != nil {
		t.Fatal(err)
	}
	for _, sessionID := range []string{first.ID, second.ID} {
		if _, err := store.CommitTopicMemoryPreference(ctx, user.ID, sessionID, true); err != nil {
			t.Fatal(err)
		}
	}
	if len(store.topicMemory) != 2 {
		t.Fatalf("two conversations hold %d consent records", len(store.topicMemory))
	}

	if err := store.DeleteSession(ctx, user.ID, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, held := store.topicMemory[first.ID]; held {
		t.Fatalf("deleting %s left its consent record behind", first.ID)
	}
	// The other conversation's decision is untouched, which is the half that stops this cleanup
	// from being implemented as "clear the map".
	if _, held := store.topicMemory[second.ID]; !held {
		t.Fatal("deleting one conversation dropped another conversation's decision")
	}
}
