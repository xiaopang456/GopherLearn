package gormmysql

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
	"gorm.io/gorm"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/repository"
)

func countRows(t *testing.T, db *gorm.DB, query string, args ...any) int64 {
	t.Helper()
	var count int64
	if err := db.Raw(query, args...).Scan(&count).Error; err != nil {
		t.Fatalf("count rows (%s): %v", query, err)
	}
	return count
}

// failCreateOn and failDeleteOn inject a database error at one exact statement
// inside a transaction, which is the only way a test can reach a rollback point
// that never occurs on its own.
func failCreateOn(t *testing.T, db *gorm.DB, table string) {
	t.Helper()
	name := "test:fail-create:" + table
	boom := errors.New("injected create failure")
	if err := db.Callback().Create().Before("gorm:create").Register(name, func(tx *gorm.DB) {
		if tx.Statement.Table == table {
			tx.AddError(boom)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Callback().Create().Remove(name) })
}

func failDeleteOn(t *testing.T, db *gorm.DB, table string) {
	t.Helper()
	name := "test:fail-delete:" + table
	boom := errors.New("injected delete failure")
	if err := db.Callback().Delete().Before("gorm:delete").Register(name, func(tx *gorm.DB) {
		if tx.Statement.Table == table {
			tx.AddError(boom)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Callback().Delete().Remove(name) })
}

func failUpdateOn(t *testing.T, db *gorm.DB, table string) {
	t.Helper()
	name := "test:fail-update:" + table
	boom := errors.New("injected update failure")
	if err := db.Callback().Update().Before("gorm:update").Register(name, func(tx *gorm.DB) {
		if tx.Statement.Table == table {
			tx.AddError(boom)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Callback().Update().Remove(name) })
}

func TestAppendMessageRollsBackTheMessageWhenTheSessionBumpFails(t *testing.T) {
	// The insert and the activity-timestamp update must commit together: a
	// half-applied turn would leave a message the session list never shows.
	store, db := newTestRepository(t)
	ctx := context.Background()
	user := mustCreateUser(t, store, "rollback-bump@example.com")
	session := mustCreateSession(t, store, user.ID, domain.SessionModeChat)
	before, err := store.GetSession(ctx, user.ID, session.ID)
	if err != nil {
		t.Fatal(err)
	}

	failUpdateOn(t, db, "sessions")
	if _, err := store.AppendMessage(ctx, user.ID, session.ID, &domain.Message{Role: "user", Content: "orphan?"}); err == nil {
		t.Fatal("AppendMessage succeeded despite the injected session update failure")
	}
	if got := countRows(t, db, "SELECT COUNT(*) FROM messages"); got != 0 {
		t.Fatalf("the message survived a failed session update: %d rows", got)
	}
	after, err := store.GetSession(ctx, user.ID, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !after.UpdatedAt.UTC().Equal(before.UpdatedAt.UTC()) {
		t.Fatalf("updated_at moved even though the turn was rolled back: %v -> %v", before.UpdatedAt.UTC(), after.UpdatedAt.UTC())
	}
}

func TestAppliedIndexShapes(t *testing.T) {
	// HasIndex only proves a name exists, so this reads back the DDL that the
	// migration actually applied: uniqueness on email and the column order of
	// the composite message index are the two things a later refactor could
	// silently lose. The query is engine-specific on purpose; it runs on the
	// test engine, and the same GORM tags produce the MySQL DDL.
	db := newTestDB(t)
	var definitions []string
	if err := db.Raw("SELECT sql FROM sqlite_master WHERE type = 'index' AND sql IS NOT NULL").Scan(&definitions).Error; err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(definitions, " | ")
	want := []string{
		"CREATE UNIQUE INDEX `idx_users_email` ON `users`(`email`)",
		"CREATE INDEX `idx_sessions_user` ON `sessions`(`user_id`)",
		"CREATE INDEX `idx_messages_session_created` ON `messages`(`session_id`,`created_at`)",
		"CREATE UNIQUE INDEX `idx_message_sources` ON `message_sources`(`message_id`,`position`)",
	}
	for _, statement := range want {
		if !strings.Contains(joined, statement) {
			t.Errorf("missing index DDL %q; applied: %s", statement, joined)
		}
	}
	if strings.Contains(joined, "`created_at`,`session_id`") {
		t.Error("message index columns are reversed: session-scoped reads need session_id first")
	}
}

func TestEmailIsStoredNormalizedAndUnique(t *testing.T) {
	store, db := newTestRepository(t)
	ctx := context.Background()

	created, err := store.CreateUser(ctx, &domain.User{Email: "  Mixed.Case@Example.COM  ", DisplayName: "Owner"})
	if err != nil {
		t.Fatal(err)
	}
	if created.Email != "mixed.case@example.com" {
		t.Fatalf("stored email = %q, want the normalized form", created.Email)
	}
	// Case and space differences must not create a second account.
	if _, err := store.CreateUser(ctx, &domain.User{Email: "mixed.case@example.com"}); !errors.Is(err, repository.ErrConflict) {
		t.Fatalf("duplicate email error = %v, want ErrConflict", err)
	}
	if _, err := store.CreateUser(ctx, &domain.User{Email: "  MIXED.CASE@EXAMPLE.COM "}); !errors.Is(err, repository.ErrConflict) {
		t.Fatalf("case-variant email error = %v, want ErrConflict", err)
	}
	if got := countRows(t, db, "SELECT COUNT(*) FROM users"); got != 1 {
		t.Fatalf("users after conflicting writes = %d, want 1", got)
	}
}

func TestUsersWithoutEmailDoNotCollide(t *testing.T) {
	store, _ := newTestRepository(t)
	ctx := context.Background()

	for index := 0; index < 2; index++ {
		if _, err := store.CreateUser(ctx, &domain.User{DisplayName: "No Email"}); err != nil {
			t.Fatalf("create user %d: %v", index, err)
		}
	}
	if _, err := store.CreateUser(ctx, nil); !errors.Is(err, repository.ErrInvalidArgument) {
		t.Fatalf("nil user error = %v", err)
	}
	if _, err := store.FindUserByEmail(ctx, "   "); !errors.Is(err, repository.ErrInvalidArgument) {
		t.Fatalf("blank lookup error = %v", err)
	}
	if _, err := store.FindUserByEmail(ctx, "nobody@example.com"); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("unknown email error = %v", err)
	}
	if _, err := store.GetUser(ctx, " "); !errors.Is(err, repository.ErrInvalidArgument) {
		t.Fatalf("blank user id error = %v", err)
	}
}

func TestSuppliedIdentityConflictsAreReported(t *testing.T) {
	store, _ := newTestRepository(t)
	ctx := context.Background()

	first, err := store.CreateUser(ctx, &domain.User{ID: "user-fixed", Email: "fixed@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != "user-fixed" {
		t.Fatalf("id = %q, want the supplied id", first.ID)
	}
	if _, err := store.CreateUser(ctx, &domain.User{ID: "user-fixed", Email: "other@example.com"}); !errors.Is(err, repository.ErrConflict) {
		t.Fatalf("duplicate id error = %v, want ErrConflict", err)
	}
}

func TestSessionModesPersistAndRetiredModesAreRejected(t *testing.T) {
	store, db := newTestRepository(t)
	ctx := context.Background()
	user := mustCreateUser(t, store, "modes@example.com")

	for _, mode := range []domain.SessionMode{domain.SessionModeChat, domain.SessionModeLearningAgent} {
		session := mustCreateSession(t, store, user.ID, mode)
		loaded, err := store.GetSession(ctx, user.ID, session.ID)
		if err != nil {
			t.Fatal(err)
		}
		if loaded.Mode != mode {
			t.Fatalf("stored mode = %q, want %q", loaded.Mode, mode)
		}
	}

	for _, mode := range []domain.SessionMode{"fast_chat", "knowledge_qa", "direct_chat", "agent", "", "unsupported"} {
		if _, err := store.CreateSession(ctx, &domain.Session{UserID: user.ID, Mode: mode}); !errors.Is(err, repository.ErrInvalidArgument) {
			t.Errorf("mode %q error = %v, want ErrInvalidArgument", mode, err)
		}
	}
	if _, err := store.CreateSession(ctx, &domain.Session{UserID: "missing-user", Mode: domain.SessionModeChat}); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("unknown owner error = %v, want ErrNotFound", err)
	}
	if _, err := store.CreateSession(ctx, &domain.Session{UserID: " ", Mode: domain.SessionModeChat}); !errors.Is(err, repository.ErrInvalidArgument) {
		t.Fatalf("blank owner error = %v, want ErrInvalidArgument", err)
	}
	if _, err := store.CreateSession(ctx, nil); !errors.Is(err, repository.ErrInvalidArgument) {
		t.Fatalf("nil session error = %v", err)
	}
	if got := countRows(t, db, "SELECT COUNT(*) FROM sessions"); got != 2 {
		t.Fatalf("sessions written = %d, want only the 2 valid ones", got)
	}
}

func TestStoredModeOutsideClosedSetIsReported(t *testing.T) {
	store, db := newTestRepository(t)
	ctx := context.Background()
	user := mustCreateUser(t, store, "corrupt@example.com")
	session := mustCreateSession(t, store, user.ID, domain.SessionModeChat)

	// Simulate a row written by an older or broken deployment.
	if err := db.Exec("UPDATE sessions SET mode = 'fast_chat' WHERE id = ?", session.ID).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetSession(ctx, user.ID, session.ID); !errors.Is(err, errCorruptStoredData) {
		t.Fatalf("GetSession error = %v, want the corrupt-data error", err)
	}
	if _, err := store.ListSessions(ctx, user.ID); !errors.Is(err, errCorruptStoredData) {
		t.Fatalf("ListSessions error = %v, want the corrupt-data error", err)
	}
}

func TestListSessionsRequiresExistingUserAndKeepsCreationOrder(t *testing.T) {
	store, _ := newTestRepository(t)
	ctx := context.Background()
	user := mustCreateUser(t, store, "order@example.com")
	other := mustCreateUser(t, store, "other@example.com")

	want := make([]string, 0, 3)
	for index := 0; index < 3; index++ {
		session := mustCreateSession(t, store, user.ID, domain.SessionModeChat)
		want = append(want, session.ID)
	}
	mustCreateSession(t, store, other.ID, domain.SessionModeLearningAgent)

	items, err := store.ListSessions(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != len(want) {
		t.Fatalf("listed %d sessions, want %d", len(items), len(want))
	}
	for index, session := range items {
		if session.ID != want[index] {
			t.Fatalf("session[%d] = %q, want %q (creation order)", index, session.ID, want[index])
		}
	}
	if _, err := store.ListSessions(ctx, "missing-user"); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("unknown user error = %v, want ErrNotFound", err)
	}
	if _, err := store.ListSessions(ctx, ""); !errors.Is(err, repository.ErrInvalidArgument) {
		t.Fatalf("blank user error = %v", err)
	}
}

func TestMessageOrderAndSessionActivityTimestamp(t *testing.T) {
	store, _ := newTestRepository(t)
	ctx := context.Background()
	user := mustCreateUser(t, store, "messages@example.com")
	session := mustCreateSession(t, store, user.ID, domain.SessionModeChat)

	var want []string
	for _, content := range []string{"first", "second", "third"} {
		for _, role := range []string{"user", "assistant"} {
			stored, err := store.AppendMessage(ctx, user.ID, session.ID, &domain.Message{Role: role, Content: content})
			if err != nil {
				t.Fatal(err)
			}
			want = append(want, role+"|"+content+"|"+stored.ID)
		}
	}
	messages, err := store.ListMessages(ctx, user.ID, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != len(want) {
		t.Fatalf("listed %d messages, want %d", len(messages), len(want))
	}
	got := make([]string, 0, len(messages))
	for _, message := range messages {
		got = append(got, message.Role+"|"+message.Content+"|"+message.ID)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("message order =\n%#v\nwant\n%#v", got, want)
	}

	updated, err := store.GetSession(ctx, user.ID, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	last := messages[len(messages)-1]
	if !updated.UpdatedAt.UTC().Equal(last.CreatedAt.UTC()) {
		t.Fatalf("session updated_at = %v, want the last message timestamp %v", updated.UpdatedAt.UTC(), last.CreatedAt.UTC())
	}
	if updated.CreatedAt.After(updated.UpdatedAt) {
		t.Fatalf("session updated_at %v is older than created_at %v", updated.UpdatedAt, updated.CreatedAt)
	}
	if _, err := store.AppendMessage(ctx, user.ID, session.ID, nil); !errors.Is(err, repository.ErrInvalidArgument) {
		t.Fatalf("nil message error = %v", err)
	}
}

func TestCrossUserAccessIsNotFoundOnEverySessionPath(t *testing.T) {
	store, db := newTestRepository(t)
	ctx := context.Background()
	owner := mustCreateUser(t, store, "owner@example.com")
	intruder := mustCreateUser(t, store, "intruder@example.com")
	session := mustCreateSession(t, store, owner.ID, domain.SessionModeChat)
	if _, err := store.AppendMessage(ctx, owner.ID, session.ID, &domain.Message{Role: "user", Content: "private"}); err != nil {
		t.Fatal(err)
	}

	if _, err := store.GetSession(ctx, intruder.ID, session.ID); !errors.Is(err, repository.ErrNotFound) {
		t.Errorf("GetSession error = %v, want ErrNotFound", err)
	}
	if _, err := store.ListMessages(ctx, intruder.ID, session.ID); !errors.Is(err, repository.ErrNotFound) {
		t.Errorf("ListMessages error = %v, want ErrNotFound", err)
	}
	if _, err := store.AppendMessage(ctx, intruder.ID, session.ID, &domain.Message{Role: "user", Content: "intrusion"}); !errors.Is(err, repository.ErrNotFound) {
		t.Errorf("AppendMessage error = %v, want ErrNotFound", err)
	}
	if err := store.DeleteSession(ctx, intruder.ID, session.ID); !errors.Is(err, repository.ErrNotFound) {
		t.Errorf("DeleteSession error = %v, want ErrNotFound", err)
	}
	// The refused paths wrote nothing and removed nothing.
	if got := countRows(t, db, "SELECT COUNT(*) FROM messages WHERE content = ?", "intrusion"); got != 0 {
		t.Fatalf("cross-user append wrote %d rows", got)
	}
	if got := countRows(t, db, "SELECT COUNT(*) FROM sessions WHERE id = ?", session.ID); got != 1 {
		t.Fatalf("cross-user delete removed the session, rows left = %d", got)
	}
	// An unknown session id is indistinguishable from another user's session.
	if _, err := store.GetSession(ctx, owner.ID, "session-does-not-exist"); !errors.Is(err, repository.ErrNotFound) {
		t.Errorf("unknown session error = %v, want ErrNotFound", err)
	}
}

func TestSessionLookupIsScopedToItsOwner(t *testing.T) {
	// Two rows can share nothing today, but reading by id alone must never
	// resolve for a user who does not own the row.
	store, db := newTestRepository(t)
	ctx := context.Background()
	owner := mustCreateUser(t, store, "scoped-owner@example.com")
	other := mustCreateUser(t, store, "scoped-other@example.com")
	session := mustCreateSession(t, store, owner.ID, domain.SessionModeChat)

	if _, err := store.GetSession(ctx, other.ID, session.ID); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("non-owner read error = %v, want ErrNotFound", err)
	}
	mine, err := store.GetSession(ctx, owner.ID, session.ID)
	if err != nil || mine.UserID != owner.ID {
		t.Fatalf("owner read error = %v session = %#v", err, mine)
	}
	if got := countRows(t, db, "SELECT COUNT(*) FROM sessions WHERE id = ? AND user_id = ?", session.ID, other.ID); got != 0 {
		t.Fatalf("owner scoping leaked a row to %q", other.ID)
	}
}

func TestDeleteSessionRemovesItsMessages(t *testing.T) {
	store, db := newTestRepository(t)
	ctx := context.Background()
	user := mustCreateUser(t, store, "delete@example.com")
	session := mustCreateSession(t, store, user.ID, domain.SessionModeChat)
	keep := mustCreateSession(t, store, user.ID, domain.SessionModeChat)

	for _, target := range []struct{ sessionID, content string }{{session.ID, "doomed"}, {keep.ID, "kept"}} {
		for index := 0; index < 2; index++ {
			if _, err := store.AppendMessage(ctx, user.ID, target.sessionID, &domain.Message{Role: "user", Content: target.content}); err != nil {
				t.Fatal(err)
			}
		}
	}

	if err := store.DeleteSession(ctx, user.ID, session.ID); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, db, "SELECT COUNT(*) FROM messages WHERE session_id = ?", session.ID); got != 0 {
		t.Fatalf("messages left after delete = %d, want 0", got)
	}
	if got := countRows(t, db, "SELECT COUNT(*) FROM sessions WHERE id = ?", session.ID); got != 0 {
		t.Fatalf("session left after delete = %d, want 0", got)
	}
	if got := countRows(t, db, "SELECT COUNT(*) FROM messages WHERE session_id = ?", keep.ID); got != 2 {
		t.Fatalf("another session lost rows: %d left, want 2", got)
	}
	if err := store.DeleteSession(ctx, user.ID, session.ID); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("second delete error = %v, want ErrNotFound", err)
	}
}

func TestAppendMessageRollsBackSessionTimestampWhenInsertFails(t *testing.T) {
	store, db := newTestRepository(t)
	ctx := context.Background()
	user := mustCreateUser(t, store, "rollback-append@example.com")
	session := mustCreateSession(t, store, user.ID, domain.SessionModeChat)
	before, err := store.GetSession(ctx, user.ID, session.ID)
	if err != nil {
		t.Fatal(err)
	}

	failCreateOn(t, db, "messages")
	if _, err := store.AppendMessage(ctx, user.ID, session.ID, &domain.Message{Role: "user", Content: "never stored"}); err == nil {
		t.Fatal("AppendMessage succeeded despite the injected insert failure")
	}
	if got := countRows(t, db, "SELECT COUNT(*) FROM messages"); got != 0 {
		t.Fatalf("rolled-back append left %d messages", got)
	}
	after, err := store.GetSession(ctx, user.ID, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !after.UpdatedAt.UTC().Equal(before.UpdatedAt.UTC()) {
		t.Fatalf("rolled-back append still moved updated_at: %v -> %v", before.UpdatedAt.UTC(), after.UpdatedAt.UTC())
	}
}

func TestDeleteSessionRollsBackMessageCleanupWhenSessionDeleteFails(t *testing.T) {
	store, db := newTestRepository(t)
	ctx := context.Background()
	user := mustCreateUser(t, store, "rollback-delete@example.com")
	session := mustCreateSession(t, store, user.ID, domain.SessionModeChat)
	if _, err := store.AppendMessage(ctx, user.ID, session.ID, &domain.Message{Role: "user", Content: "must survive"}); err != nil {
		t.Fatal(err)
	}

	failDeleteOn(t, db, "sessions")
	if err := store.DeleteSession(ctx, user.ID, session.ID); err == nil {
		t.Fatal("DeleteSession succeeded despite the injected delete failure")
	}
	// The message cleanup in the same transaction must be undone as well.
	if got := countRows(t, db, "SELECT COUNT(*) FROM messages WHERE session_id = ?", session.ID); got != 1 {
		t.Fatalf("rolled-back delete left %d messages, want 1", got)
	}
	if got := countRows(t, db, "SELECT COUNT(*) FROM sessions WHERE id = ?", session.ID); got != 1 {
		t.Fatal("rolled-back delete removed the session row")
	}
}

func TestCanceledContextPropagatesInsteadOfServing(t *testing.T) {
	store, db := newTestRepository(t)
	user := mustCreateUser(t, store, "cancel@example.com")
	session := mustCreateSession(t, store, user.ID, domain.SessionModeChat)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	assertCanceled := func(name string, err error) {
		t.Helper()
		if !errors.Is(err, context.Canceled) {
			t.Errorf("%s error = %v, want context.Canceled", name, err)
		}
	}
	_, err := store.CreateUser(ctx, &domain.User{Email: "never@example.com"})
	assertCanceled("CreateUser", err)
	_, err = store.GetUser(ctx, user.ID)
	assertCanceled("GetUser", err)
	_, err = store.FindUserByEmail(ctx, "cancel@example.com")
	assertCanceled("FindUserByEmail", err)
	_, err = store.CreateSession(ctx, &domain.Session{UserID: user.ID, Mode: domain.SessionModeChat})
	assertCanceled("CreateSession", err)
	_, err = store.ListSessions(ctx, user.ID)
	assertCanceled("ListSessions", err)
	_, err = store.GetSession(ctx, user.ID, session.ID)
	assertCanceled("GetSession", err)
	assertCanceled("DeleteSession", store.DeleteSession(ctx, user.ID, session.ID))
	_, err = store.AppendMessage(ctx, user.ID, session.ID, &domain.Message{Role: "user", Content: "never"})
	assertCanceled("AppendMessage", err)
	_, err = store.ListMessages(ctx, user.ID, session.ID)
	assertCanceled("ListMessages", err)

	if got := countRows(t, db, "SELECT COUNT(*) FROM users WHERE email = ?", "never@example.com"); got != 0 {
		t.Fatal("canceled write stored a user row")
	}
	if got := countRows(t, db, "SELECT COUNT(*) FROM messages"); got != 0 {
		t.Fatalf("canceled write stored %d messages", got)
	}
}

func TestMigrateIsIdempotentAndPreservesData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "repeat.db")
	db := openTestDB(t, path)
	repo := NewRepository(db)
	user := mustCreateUser(t, repo, "migrate@example.com")
	mustCreateSession(t, repo, user.ID, domain.SessionModeChat)

	for round := 0; round < 2; round++ {
		if err := Migrate(context.Background(), db); err != nil {
			t.Fatalf("repeat migrate round %d: %v", round, err)
		}
	}
	if got := countRows(t, db, "SELECT COUNT(*) FROM users"); got != 1 {
		t.Fatalf("migrate changed user rows: %d", got)
	}
	if got := countRows(t, db, "SELECT COUNT(*) FROM sessions"); got != 1 {
		t.Fatalf("migrate changed session rows: %d", got)
	}
	for _, table := range []string{"users", "sessions", "messages", "message_sources"} {
		if !db.Migrator().HasTable(table) {
			t.Fatalf("table %q missing after migrate", table)
		}
	}
	if !db.Migrator().HasIndex(&UserRecord{}, "idx_users_email") {
		t.Error("email unique index missing")
	}
	if !db.Migrator().HasIndex(&SessionRecord{}, "idx_sessions_user") {
		t.Error("session owner index missing")
	}
	if !db.Migrator().HasIndex(&MessageRecord{}, "idx_messages_session_created") {
		t.Error("message (session_id, created_at) index missing")
	}
	if !db.Migrator().HasIndex(&MessageSourceRecord{}, "idx_message_sources") {
		t.Error("citation (message_id, position) unique index missing")
	}
	if err := Migrate(context.Background(), nil); !errors.Is(err, repository.ErrInvalidArgument) {
		t.Fatalf("nil db error = %v", err)
	}
	if err := Migrate(nil, db); !errors.Is(err, repository.ErrInvalidArgument) {
		t.Fatalf("nil ctx error = %v", err)
	}
}

func TestMigrateReportsFailure(t *testing.T) {
	db := newTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Migrate(ctx, db); err == nil {
		t.Fatal("migrate reported no error against a canceled context")
	}
}

func TestGeneratedIDsSortInCreationOrder(t *testing.T) {
	first := newID("message")
	second := newID("message")
	if first >= second {
		t.Fatalf("generated ids are not increasing: %q then %q", first, second)
	}
	if !strings.HasPrefix(first, "message-") || len(first) > 96 {
		t.Fatalf("unexpected identifier shape: %q", first)
	}
}

func TestTranslateErrorMapsDriverErrors(t *testing.T) {
	runtimeBoom := errors.New("runtime boom")
	cases := []struct {
		name string
		in   error
		want error
	}{
		{name: "nil stays nil"},
		{name: "record not found", in: gorm.ErrRecordNotFound, want: repository.ErrNotFound},
		{name: "translated duplicate", in: gorm.ErrDuplicatedKey, want: repository.ErrConflict},
		{name: "mysql 1062", in: &mysql.MySQLError{Number: 1062, Message: "Duplicate entry for key idx_users_email"}, want: repository.ErrConflict},
		{name: "duplicate by text", in: errors.New("UNIQUE constraint failed: users.email"), want: repository.ErrConflict},
		{name: "canceled", in: context.Canceled, want: context.Canceled},
		{name: "deadline", in: context.DeadlineExceeded, want: context.DeadlineExceeded},
		{name: "repository not found passes through", in: repository.ErrNotFound, want: repository.ErrNotFound},
		{name: "unknown error passes through", in: runtimeBoom, want: runtimeBoom},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := translate(testCase.in)
			switch {
			case testCase.want == nil && got != nil:
				t.Fatalf("translate(nil) = %v, want nil", got)
			case testCase.want == nil:
				return
			}
			if got == nil || got.Error() != testCase.want.Error() {
				t.Fatalf("translate(%v) = %v, want %v", testCase.in, got, testCase.want)
			}
			if !errors.Is(got, testCase.want) {
				t.Fatalf("translate(%v) lost the sentinel: %v", testCase.in, got)
			}
		})
	}
}
