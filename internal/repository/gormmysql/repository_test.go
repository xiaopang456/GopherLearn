package gormmysql

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/repository"
)

// newTestDB opens a throwaway file database and migrates it. A file, not
// ":memory:", is used so a test can close and reopen the same database and
// prove data really survived the process boundary.
func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	return openTestDB(t, filepath.Join(t.TempDir(), "gopherlearn-test.db"))
}

func openTestDB(t *testing.T, path string) *gorm.DB {
	t.Helper()
	db := openPlainTestDB(t, path)
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}
	return db
}

// openPlainTestDB opens the engine without a schema, so a test can require the
// code under test to run its own migration.
func openPlainTestDB(t *testing.T, path string) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(path), testConfig())
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() { closeDB(db) })
	return db
}

func newPlainTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	return openPlainTestDB(t, filepath.Join(t.TempDir(), "gopherlearn-plain.db"))
}

func testConfig() *gorm.Config {
	config := gormConfig()
	config.Logger = gormlogger.Default.LogMode(gormlogger.Silent)
	return config
}

func newTestRepository(t *testing.T) (*Repository, *gorm.DB) {
	t.Helper()
	db := newTestDB(t)
	return NewRepository(db), db
}

func mustCreateUser(t *testing.T, store repository.Repository, email string) *domain.User {
	t.Helper()
	user, err := store.CreateUser(context.Background(), &domain.User{Email: email, DisplayName: "Owner", PasswordHash: "hash"})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if user.ID == "" {
		t.Fatal("created user has no id")
	}
	return user
}

func mustCreateSession(t *testing.T, store repository.Repository, userID string, mode domain.SessionMode) *domain.Session {
	t.Helper()
	session, err := store.CreateSession(context.Background(), &domain.Session{UserID: userID, Title: "Chat", Mode: mode})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return session
}

func TestSQLiteBackedRoundTrip(t *testing.T) {
	store, _ := newTestRepository(t)
	ctx := context.Background()

	user := mustCreateUser(t, store, "owner@example.com")
	loaded, err := store.GetUser(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Email != user.Email || loaded.DisplayName != "Owner" || loaded.PasswordHash != "hash" {
		t.Fatalf("user round trip mismatch: %#v", loaded)
	}
	if loaded.CreatedAt.Location() != time.UTC {
		t.Fatalf("created_at came back in location %v", loaded.CreatedAt.Location())
	}

	session := mustCreateSession(t, store, user.ID, domain.SessionModeChat)
	reloaded, err := store.GetSession(ctx, user.ID, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Mode != domain.SessionModeChat || reloaded.UserID != user.ID {
		t.Fatalf("session round trip mismatch: %#v", reloaded)
	}

	message, err := store.AppendMessage(ctx, user.ID, session.ID, &domain.Message{Role: "user", Content: "question"})
	if err != nil {
		t.Fatal(err)
	}
	messages, err := store.ListMessages(ctx, user.ID, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].ID != message.ID || messages[0].Content != "question" {
		t.Fatalf("message round trip mismatch: %#v", messages)
	}
}
