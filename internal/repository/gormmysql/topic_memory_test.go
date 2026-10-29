package gormmysql

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/repository"
	"github.com/xiaopang456/GopherLearn/internal/repository/repositorytest"
)

// TestTopicMemoryContract runs the shared consent suite against the SQL store, so the engine a
// deployment actually runs on is held to exactly the rules the development store follows.
func TestTopicMemoryContract(t *testing.T) {
	repositorytest.TopicMemorySuite(t, func(t *testing.T) repository.Repository { return NewRepository(newTestDB(t)) })
}

// TestTopicMemoryTableIsCreatedByMigrate is the schema guard. AutoMigrate is additive, so a record
// type that is declared but never registered would leave every test in this file passing against a
// table it created itself, while a real deployment met "no such table" the first time a learner
// touched the switch.
func TestTopicMemoryTableIsCreatedByMigrate(t *testing.T) {
	db := newTestDB(t)
	if !db.Migrator().HasTable("session_topic_memory") {
		t.Fatal("Migrate created no session_topic_memory table")
	}
	// The key and the value, checked as columns rather than inferred from behaviour:
	// session_id being the primary key is the whole one-decision-per-conversation rule.
	for _, column := range []string{"session_id", "user_id", "enabled", "created_at", "updated_at"} {
		if !db.Migrator().HasColumn("session_topic_memory", column) {
			t.Fatalf("session_topic_memory has no %s column", column)
		}
	}
}

// TestOneConversationHoldsOneDecision checks the uniqueness rule where it is actually enforced: a
// conversation that is asked the same thing twice, and one that changes its mind, must both end
// with exactly one row. Two rows would mean the reader has to guess which decision applies, and
// the primary key on session_id is what makes guessing impossible rather than merely unlikely.
func TestOneConversationHoldsOneDecision(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	store := NewRepository(db)
	user := mustCreateUser(t, store, "topic-memory-one-row@example.com")
	sessionID := mustTopicMemorySession(t, store, user.ID, domain.SessionModeLearningAgent)

	for _, enabled := range []bool{true, true, false, true, true} {
		if _, err := store.CommitTopicMemoryPreference(ctx, user.ID, sessionID, enabled); err != nil {
			t.Fatal(err)
		}
	}
	var rows int64
	if err := db.Model(&TopicMemoryRecord{}).Where("session_id = ?", sessionID).Count(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("five commits on one conversation left %d rows, want one", rows)
	}
	stored, err := store.GetTopicMemoryPreference(ctx, user.ID, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if !stored.Enabled {
		t.Fatal("the last answer committed is not the answer stored")
	}
}

// TestTopicMemorySurvivesReopeningTheDatabase is the reason this switch is a row and not a process
// map: a learner who said yes and came back tomorrow must meet the same yes, with the same
// created-at, from a new process - and a learner who said no must not find the default quietly
// flipped by a restart.
func TestTopicMemorySurvivesReopeningTheDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "topic-memory-reopen.db")
	before := NewRepository(openTestDB(t, path))
	user := mustCreateUser(t, before, "topic-memory-reopen@example.com")
	sessionID := mustTopicMemorySession(t, before, user.ID, domain.SessionModeLearningAgent)

	on, err := before.CommitTopicMemoryPreference(ctx, user.ID, sessionID, true)
	if err != nil {
		t.Fatal(err)
	}

	after := NewRepository(openTestDB(t, path))
	read, err := after.GetTopicMemoryPreference(ctx, user.ID, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if !read.Enabled || !read.CreatedAt.Equal(on.CreatedAt) || !read.UpdatedAt.Equal(on.UpdatedAt) {
		t.Fatalf("a reopened process reads %+v, want the stored yes from %v", read, on.CreatedAt)
	}

	// The off direction matters most after a restart, because "it came back on" is the failure a
	// learner would have no way to notice.
	if _, err := after.CommitTopicMemoryPreference(ctx, user.ID, sessionID, false); err != nil {
		t.Fatal(err)
	}
	reopened := NewRepository(openTestDB(t, path))
	off, err := reopened.GetTopicMemoryPreference(ctx, user.ID, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if off.Enabled {
		t.Fatal("an explicit no came back as yes after a restart")
	}
}

// TestTopicMemoryRowLeavesWithTheConversation checks the cleanup in SQL terms the shared suite
// cannot reach: the row is gone from the table, not merely unreadable through a missing
// conversation. An orphaned "yes" is exactly the kind of state a later phase would re-adopt.
func TestTopicMemoryRowLeavesWithTheConversation(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	store := NewRepository(db)
	user := mustCreateUser(t, store, "topic-memory-delete@example.com")
	sessionID := mustTopicMemorySession(t, store, user.ID, domain.SessionModeLearningAgent)

	if _, err := store.CommitTopicMemoryPreference(ctx, user.ID, sessionID, true); err != nil {
		t.Fatal(err)
	}
	var rows int64
	if err := db.Model(&TopicMemoryRecord{}).Where("session_id = ?", sessionID).Count(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("one conversation owns %d consent rows, want 1", rows)
	}
	if err := store.DeleteSession(ctx, user.ID, sessionID); err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&TopicMemoryRecord{}).Count(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("deleting the conversation orphaned %d consent rows", rows)
	}
}

// mustTopicMemorySession creates one conversation of the given mode, because a consent is scoped by
// conversation and these tests need both modes without depending on the suite's helpers.
func mustTopicMemorySession(t *testing.T, store repository.Repository, userID string, mode domain.SessionMode) string {
	t.Helper()
	session, err := store.CreateSession(context.Background(), &domain.Session{UserID: userID, Title: "会话", Mode: mode})
	if err != nil {
		t.Fatal(err)
	}
	return session.ID
}
