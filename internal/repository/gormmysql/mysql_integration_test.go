package gormmysql

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/knowledge"
	"github.com/xiaopang456/GopherLearn/internal/knowledge/knowledgetest"
	"github.com/xiaopang456/GopherLearn/internal/repository"
	"github.com/xiaopang456/GopherLearn/internal/repository/repositorytest"
)

// EnvRealMySQLDSN is the only way this file runs. Every other test in this
// package speaks SQL through an embedded engine, which proves the statements and
// the transactions but not the engine the deployment actually uses: real MySQL
// decides charset storage, unique-index behaviour with NULL columns, and
// duplicate-key errors on its own terms.
//
// The default suite therefore must not need a database. When the variable is
// unset the whole thing skips with a reason rather than failing.
const EnvRealMySQLDSN = "GOPHERLEARN_MYSQL_INTEGRATION_DSN"

// integrationTables is every table Migrate and MigrateKnowledge create. Listed
// here rather than discovered, so a newly registered record type that this list
// forgets shows up as leftover rows in the next suite run instead of silently
// sharing state between them.
var integrationTables = []string{
	"users", "sessions", "messages", "message_sources", "session_summaries",
	"session_topic_memory",
	"memories", "memory_sources",
	"learning_sessions", "learning_questions", "learning_attempts", "topic_progress",
	"episode_memories", "episode_projection_jobs",
	"knowledge_collections", "knowledge_documents", "knowledge_chunks",
}

// TestRealMySQLContract runs the shared behavioural suites against a real MySQL
// server, then asserts the three things only that engine can answer: that the
// open-quiz uniqueness rule is a database constraint rather than a service
// convention, that a summary commit refuses a stale version, and that four-byte
// characters survive the round trip unchanged.
func TestRealMySQLContract(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv(EnvRealMySQLDSN))
	if dsn == "" {
		t.Skipf("%s is unset; the default suite needs no Docker and no MySQL server", EnvRealMySQLDSN)
	}

	db := openIntegrationSchema(t, dsn)
	fresh := func(t *testing.T) repository.Repository {
		t.Helper()
		truncateAll(t, db)
		return NewRepository(db)
	}

	t.Run("repository contract", func(t *testing.T) { repositorytest.Suite(t, fresh) })
	t.Run("memory contract", func(t *testing.T) { repositorytest.MemorySuite(t, fresh) })
	t.Run("topic memory contract", func(t *testing.T) { repositorytest.TopicMemorySuite(t, fresh) })
	t.Run("learning contract", func(t *testing.T) { repositorytest.LearningSuite(t, fresh) })
	t.Run("episode contract", func(t *testing.T) { repositorytest.EpisodeSuite(t, fresh) })
	t.Run("knowledge contract", func(t *testing.T) {
		knowledgetest.Suite(t, func(t *testing.T, limits knowledge.Limits) knowledge.Store {
			t.Helper()
			truncateAll(t, db)
			// The suite writes as "user-1" and "user-2", and on real MySQL a knowledge
			// write locks that principal's users row before it decides anything - so
			// the contract only runs against the deployment's actual shape, the rows
			// get created here rather than being assumed.
			seedUserRow(t, db, "user-1")
			seedUserRow(t, db, "user-2")
			return NewKnowledgeRepository(db).WithLimits(limits)
		})
		t.Run("knowledge lifecycle contract", func(t *testing.T) {
			knowledgetest.LifecycleSuite(t, func(t *testing.T, limits knowledge.Limits) knowledge.Store {
				t.Helper()
				truncateAll(t, db)
				seedUserRow(t, db, "user-1")
				seedUserRow(t, db, "user-2")
				return NewKnowledgeRepository(db).WithLimits(limits)
			})
		})
	})

	t.Run("the topic-memory row belongs to the database, not to the process", func(t *testing.T) {
		truncateAll(t, db)
		ctx := t.Context()
		userID := integrationUser(t, db, ctx, "topic-memory")
		sessionID := integrationSession(t, db, ctx, userID, "学习会话")
		store := NewRepository(db)
		count := func(t *testing.T, where string, args ...any) int64 {
			t.Helper()
			var rows int64
			query := db.WithContext(ctx).Table("session_topic_memory")
			if where != "" {
				query = query.Where(where, args...)
			}
			if err := query.Count(&rows).Error; err != nil {
				t.Fatal(err)
			}
			return rows
		}

		// The table is created by the production migration path, and `enabled` is the first
		// boolean column this schema has ever carried - MySQL stores it as 0/1, and a claim
		// about a default being false has to be checked against the engine that stores it.
		if !db.Migrator().HasTable("session_topic_memory") {
			t.Fatal("Migrate created no session_topic_memory table on real MySQL")
		}
		if !db.Migrator().HasColumn("session_topic_memory", "enabled") {
			t.Fatal("session_topic_memory has no enabled column")
		}

		// Default off, and "off" costs no row: a learner who was never asked leaves nothing
		// behind, which is what keeps the switch's storage proportional to its use.
		stored, err := store.GetTopicMemoryPreference(ctx, userID, sessionID)
		if err != nil {
			t.Fatalf("a conversation with no consent row must read as off, not as a failure: %v", err)
		}
		if stored.Enabled || count(t, "") != 0 {
			t.Fatalf("the default state is %+v with %d rows, want off with none", stored, count(t, ""))
		}

		if _, err := store.CommitTopicMemoryPreference(ctx, userID, sessionID, true); err != nil {
			t.Fatal(err)
		}
		first, err := store.GetTopicMemoryPreference(ctx, userID, sessionID)
		if err != nil || !first.Enabled {
			t.Fatalf("the yes did not stick: %+v (%v)", first, err)
		}
		if rows := count(t, "session_id = ?", sessionID); rows != 1 {
			t.Fatalf("one conversation holds %d consent rows on MySQL, want 1", rows)
		}

		// Idempotence, stated in the column the reader cannot fake: a repeat of the same answer
		// writes nothing, so updated_at stands where it was. MySQL is where this is worth
		// asserting, because each of these statements costs real milliseconds.
		if _, err := store.CommitTopicMemoryPreference(ctx, userID, sessionID, true); err != nil {
			t.Fatal(err)
		}
		after, err := store.GetTopicMemoryPreference(ctx, userID, sessionID)
		if err != nil || !after.Enabled {
			t.Fatalf("a repeated yes changed the answer: %+v (%v)", after, err)
		}
		if !after.UpdatedAt.Equal(first.UpdatedAt) || !after.CreatedAt.Equal(first.CreatedAt) {
			t.Fatalf("a repeated yes rewrote the record: %v then %v", first.UpdatedAt, after.UpdatedAt)
		}
		if rows := count(t, "session_id = ?", sessionID); rows != 1 {
			t.Fatalf("repeating the answer added a row: %d", rows)
		}

		// And no is the same kind of answer: it must actually reach the column, which is the
		// exact place a struct-form GORM update would silently drop it as a zero value.
		if _, err := store.CommitTopicMemoryPreference(ctx, userID, sessionID, false); err != nil {
			t.Fatal(err)
		}
		off, err := store.GetTopicMemoryPreference(ctx, userID, sessionID)
		if err != nil || off.Enabled {
			t.Fatalf("the switch did not commit as off: %+v (%v)", off, err)
		}
		var storedFlag int
		if err := db.WithContext(ctx).Raw("SELECT enabled FROM session_topic_memory WHERE session_id = ?",
			sessionID).Scan(&storedFlag).Error; err != nil {
			t.Fatal(err)
		}
		if storedFlag != 0 {
			t.Fatalf("MySQL stores the off answer as %d, want 0", storedFlag)
		}
		if _, err := store.CommitTopicMemoryPreference(ctx, userID, sessionID, false); err != nil {
			t.Fatal(err)
		}
		offAgain, err := store.GetTopicMemoryPreference(ctx, userID, sessionID)
		if err != nil {
			t.Fatal(err)
		}
		if !offAgain.UpdatedAt.Equal(off.UpdatedAt) {
			t.Fatalf("a repeated no moved the record: %v then %v", off.UpdatedAt, offAgain.UpdatedAt)
		}

		// A second connection to the same schema is the part of "after a restart" this test can
		// honestly check: the decision has to be readable by a process that did not write it.
		// Restarting the engine itself is the claim mysql-runtime-v1/v2/v3 recorded, not this one.
		reopened := NewRepository(reopenIntegrationSchema(t, db))
		if _, err := store.CommitTopicMemoryPreference(ctx, userID, sessionID, true); err != nil {
			t.Fatal(err)
		}
		across, err := reopened.GetTopicMemoryPreference(ctx, userID, sessionID)
		if err != nil {
			t.Fatal(err)
		}
		if !across.Enabled || !across.CreatedAt.Equal(off.CreatedAt) {
			t.Fatalf("a second connection reads %+v, want the stored yes with the original created_at", across)
		}

		// Deleting the conversation releases the permission. An orphaned yes is state a later
		// phase could find and re-adopt, so the count is checked in the table rather than
		// inferred from a read that would be gated behind the missing conversation anyway.
		if err := store.DeleteSession(ctx, userID, sessionID); err != nil {
			t.Fatal(err)
		}
		if rows := count(t, "session_id = ?", sessionID); rows != 0 {
			t.Fatalf("deleting the conversation left %d orphan consent rows", rows)
		}
		if _, err := store.GetTopicMemoryPreference(ctx, userID, sessionID); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("a deleted conversation's consent reads %v, want ErrNotFound", err)
		}

		// A foreign or missing conversation must not be able to write, and must not be able to
		// learn anything by trying. The row count is the proof that the refusal was a refusal and
		// not a write that answered an error afterwards.
		other := integrationUser(t, db, ctx, "topic-memory-other")
		otherSession := integrationSession(t, db, ctx, other, "别人的会话")
		if _, err := store.CommitTopicMemoryPreference(ctx, other, sessionID, true); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("writing into a deleted/foreign conversation returned %v", err)
		}
		if _, err := store.CommitTopicMemoryPreference(ctx, userID, otherSession, true); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("the real owner's session was writable by the wrong user: %v", err)
		}
		if _, err := store.GetTopicMemoryPreference(ctx, userID, otherSession); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("reading a foreign conversation returned %v, want ErrNotFound", err)
		}
		if rows := count(t, "session_id = ?", otherSession); rows != 0 {
			t.Fatalf("a refused write left %d rows on the foreign conversation", rows)
		}
		if _, err := store.CommitTopicMemoryPreference(ctx, userID, "session-never-created", true); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("a missing conversation returned %v, want ErrNotFound", err)
		}
		if rows := count(t, "session_id = ?", "session-never-created"); rows != 0 {
			t.Fatalf("a missing conversation ended up with %d rows", rows)
		}
		// The other account's own conversation is still theirs alone.
		if _, err := store.CommitTopicMemoryPreference(ctx, other, otherSession, true); err != nil {
			t.Fatalf("the owner could not commit their own consent: %v", err)
		}
		if readable, err := store.GetTopicMemoryPreference(ctx, userID, otherSession); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("the first account can read another account's consent: %+v (%v)", readable, err)
		}
	})

	t.Run("the one-open-quiz rule is enforced by the index, not by the caller", func(t *testing.T) {
		truncateAll(t, db)
		ctx := t.Context()
		userID := integrationUser(t, db, ctx, "open-quiz")
		sessionID := integrationSession(t, db, ctx, userID, "会话一")
		key := userID + ":" + sessionID

		first := LearningSessionRecord{ID: "lsn-first", UserID: userID, SessionID: sessionID,
			TopicKey: "redis", Topic: "Redis 持久化", Status: string(domain.LearningSessionWaitingAnswer),
			ActiveKey: &key, Version: 1, TotalQuestions: 3, ExpiresAt: time.Now().UTC().Add(time.Hour)}
		if err := db.WithContext(ctx).Create(&first).Error; err != nil {
			t.Fatalf("first open quiz: %v", err)
		}
		second := LearningSessionRecord{ID: "lsn-second", UserID: userID, SessionID: sessionID,
			TopicKey: "redis", Topic: "Redis 持久化", Status: string(domain.LearningSessionWaitingAnswer),
			ActiveKey: &key, Version: 1, TotalQuestions: 3, ExpiresAt: time.Now().UTC().Add(time.Hour)}
		if err := db.WithContext(ctx).Create(&second).Error; err == nil {
			t.Fatal("a second open quiz on one conversation was stored; uniq_learning_open is not the database's rule")
		} else if !errors.Is(err, gorm.ErrDuplicatedKey) && !isDuplicateKey(err) {
			// The same pair of tests the repository itself uses: gorm is configured to
			// translate 1062 into ErrDuplicatedKey, so the driver error type alone would
			// miss the refusal this case exists to prove.
			t.Fatalf("second open quiz failed with %v, want a duplicate-key error", err)
		}

		// NULL must not collide: a finished quiz releases its conversation, and two
		// released quizzes on two conversations must coexist. MySQL treats NULLs as
		// distinct here, which is the whole reason the column is nullable rather
		// than an empty string.
		cleared := map[string]any{"active_key": nil, "status": string(domain.LearningSessionCompleted), "version": 2}
		if err := db.WithContext(ctx).Model(&LearningSessionRecord{}).Where("id = ?", "lsn-first").Updates(cleared).Error; err != nil {
			t.Fatal(err)
		}
		other := integrationSession(t, db, ctx, userID, "会话二")
		third := LearningSessionRecord{ID: "lsn-third", UserID: userID, SessionID: other,
			TopicKey: "go", Topic: "Go 调度", Status: string(domain.LearningSessionWaitingAnswer),
			ActiveKey: nil, Version: 1, TotalQuestions: 3, ExpiresAt: time.Now().UTC().Add(time.Hour)}
		if err := db.WithContext(ctx).Create(&third).Error; err != nil {
			t.Fatalf("a released quiz blocked a new row: %v", err)
		}
	})

	t.Run("a stale summary commit changes nothing", func(t *testing.T) {
		truncateAll(t, db)
		ctx := t.Context()
		store := NewRepository(db)
		userID := integrationUser(t, db, ctx, "summary")
		sessionID := integrationSession(t, db, ctx, userID, "对话")

		committed, err := store.CommitSessionSummary(ctx, &domain.SessionSummary{
			UserID: userID, SessionID: sessionID, Content: "学习者问到了持久化", CoveredMessageID: "msg-1",
		}, 0)
		if err != nil {
			t.Fatal(err)
		}
		if committed.Version != 1 {
			t.Fatalf("first summary stored version %d, want 1", committed.Version)
		}
		if _, err := store.CommitSessionSummary(ctx, &domain.SessionSummary{
			UserID: userID, SessionID: sessionID, Content: "过期的一版", CoveredMessageID: "msg-2",
		}, 0); !errors.Is(err, repository.ErrConflict) {
			t.Fatalf("stale commit = %v, want ErrConflict", err)
		}
		current, err := store.GetSessionSummary(ctx, userID, sessionID)
		if err != nil {
			t.Fatal(err)
		}
		if current.Content != "学习者问到了持久化" || current.CoveredMessageID != "msg-1" {
			t.Fatalf("a refused commit still rewrote the row: %+v", current)
		}
	})

	t.Run("four-byte characters and long text round trip through utf8mb4", func(t *testing.T) {
		truncateAll(t, db)
		ctx := t.Context()
		store := NewRepository(db)
		userID := integrationUser(t, db, ctx, "charset")
		sessionID := integrationSession(t, db, ctx, userID, "中文")

		content := "持久化 🧪 取舍😀" + strings.Repeat("长", 2000)
		sent, err := store.AppendMessage(ctx, userID, sessionID, &domain.Message{Role: "user", Content: content})
		if err != nil {
			t.Fatal(err)
		}
		stored, err := store.ListMessages(ctx, userID, sessionID)
		if err != nil {
			t.Fatal(err)
		}
		if len(stored) != 1 {
			t.Fatalf("%d messages stored, want the one appended", len(stored))
		}
		if stored[0].Content != content {
			t.Fatalf("content changed through the engine: got %d runes, want %d", len([]rune(stored[0].Content)), len([]rune(content)))
		}
		if stored[0].ID != sent.ID {
			t.Fatalf("id %q, want %q", stored[0].ID, sent.ID)
		}

		var bytes, characters int
		if err := db.WithContext(ctx).Raw(
			"SELECT LENGTH(content), CHAR_LENGTH(content) FROM messages WHERE id = ?", sent.ID,
		).Row().Scan(&bytes, &characters); err != nil {
			t.Fatal(err)
		}
		if characters != len([]rune(content)) {
			t.Fatalf("MySQL counts %d characters, want %d", characters, len([]rune(content)))
		}
		if bytes == characters {
			t.Fatal("the multi-byte text stored as one byte per character, so the column is not utf8mb4")
		}
	})
}

// TestRealMySQLIdleBoundRetiresAKilledConnection is the reason
// Options.ConnMaxIdleTime exists, and the only test here that proves the pool setter
// was actually applied.
//
// The failure it guards is measured, not theoretical: a connection the server closed
// (a restart, a KILL, a firewall reaping it) stays in the pool's free list, and the
// next request that draws it fails. The driver can rule that out before use only on
// platforms where it can probe the socket, which does not include Windows, so the idle
// bound is what limits the window here. The test therefore injects a short bound rather
// than waiting out the production 90s.
func TestRealMySQLIdleBoundRetiresAKilledConnection(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv(EnvRealMySQLDSN))
	if dsn == "" {
		t.Skipf("%s is unset; this needs a real MySQL server, not the embedded engine", EnvRealMySQLDSN)
	}
	ctx := t.Context()
	const idleBound = 300 * time.Millisecond

	config, err := driver.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("the %s value cannot be parsed", EnvRealMySQLDSN)
	}

	stores, err := Connect(ctx, dsn, Options{ConnectTimeout: 3 * time.Second, ConnMaxIdleTime: idleBound})
	if err != nil {
		t.Fatalf("the production dial path failed against the configured server: %v", err)
	}
	t.Cleanup(func() { closeDB(stores.db) })

	sqlDB, err := stores.db.DB()
	if err != nil {
		t.Fatal(err)
	}

	// A separate handle, so the KILL is not the pool killing its own connection.
	admin, err := gorm.Open(mysql.Open(dsn), gormConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeDB(admin) })

	// Warm: one query, and its connection goes back to the free list.
	var one int
	if err := sqlDB.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
		t.Fatal(err)
	}
	before := sleepConnections(t, ctx, admin, config.User)
	if len(before) == 0 {
		t.Fatal("the pool opened no visible connection, so this case would prove nothing")
	}

	victim := before[0]
	// KILL takes a literal id: the prepared-statement protocol rejects a placeholder.
	// The value is an int64 read back from the server's own processlist, so it cannot
	// carry anything but a number.
	if err := admin.WithContext(ctx).Exec("KILL " + strconv.FormatInt(victim, 10)).Error; err != nil {
		t.Fatalf("kill connection %d: %v", victim, err)
	}
	if stillPooled(t, ctx, admin, victim) {
		t.Fatalf("connection %d survived the KILL", victim)
	}

	// Past the bound: the pool must have dropped the dead socket instead of handing it
	// to the next query.
	time.Sleep(6 * idleBound)
	if err := sqlDB.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
		t.Fatalf("the query after the idle bound failed, so the dead connection was handed to it: %v", err)
	}
	if one != 1 {
		t.Fatalf("SELECT 1 returned %d", one)
	}
	for _, id := range sleepConnections(t, ctx, admin, config.User) {
		if id == victim {
			t.Fatalf("connection %d came back after being killed", victim)
		}
	}

	// No leak: serial queries need at most a couple of connections, and closing the
	// pool must return every one of them.
	if open := sqlDB.Stats().OpenConnections; open > 3 {
		t.Fatalf("%d open connections for a serial test, want at most 3", open)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}
	if open := sqlDB.Stats().OpenConnections; open != 0 {
		t.Fatalf("%d connections still open after closing the pool", open)
	}
}

// sleepConnections lists the server-side ids of this user's idle connections, minus the
// admin handle that is asking.
func sleepConnections(t *testing.T, ctx context.Context, admin *gorm.DB, user string) []int64 {
	t.Helper()
	rows, err := admin.WithContext(ctx).Raw(
		"SELECT ID FROM information_schema.PROCESSLIST WHERE USER = ? AND COMMAND = 'Sleep' AND ID <> CONNECTION_ID()", user,
	).Rows()
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return ids
}

func stillPooled(t *testing.T, ctx context.Context, admin *gorm.DB, id int64) bool {
	t.Helper()
	var count int64
	if err := admin.WithContext(ctx).Raw(
		"SELECT COUNT(*) FROM information_schema.PROCESSLIST WHERE ID = ?", id,
	).Scan(&count).Error; err != nil {
		t.Fatal(err)
	}
	return count > 0
}

// reopenIntegrationSchema opens a second, independent connection pool against the schema the
// given connection is already using. It exists for the one claim a shared pool cannot make: that a
// decision written by this process is readable by a process that never wrote it. What it does not
// claim is a server restart - that is the measured story mysql-runtime-v1/v2/v3 recorded, and this
// test says so rather than borrowing its conclusion.
func reopenIntegrationSchema(t *testing.T, db *gorm.DB) *gorm.DB {
	t.Helper()
	ctx := t.Context()

	var schema string
	if err := db.WithContext(ctx).Raw("SELECT DATABASE()").Row().Scan(&schema); err != nil {
		t.Fatalf("the integration connection cannot report its own schema: %v", err)
	}
	if schema == "" {
		t.Fatal("the integration connection reports no schema")
	}

	config, err := driver.ParseDSN(strings.TrimSpace(os.Getenv(EnvRealMySQLDSN)))
	if err != nil {
		t.Fatal(err)
	}
	normalized, err := NormalizeDSN(config.FormatDSN(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	reopen, err := driver.ParseDSN(normalized)
	if err != nil {
		t.Fatal(err)
	}
	reopen.DBName = schema
	pool, err := gorm.Open(mysql.Open(reopen.FormatDSN()), gormConfig())
	if err != nil {
		t.Fatalf("a second connection to %s could not be opened", schema)
	}
	t.Cleanup(func() { closeDB(pool) })
	return pool
}

// openIntegrationSchema creates a throwaway schema named after this run, migrates
// it with the production path, and drops it when the test ends. The connection
// string never reaches a log line: the identifiers come from the process's own
// random bytes, and every error is reported as a fixed message.
func openIntegrationSchema(t *testing.T, dsn string) *gorm.DB {
	t.Helper()
	ctx := t.Context()

	normalized, err := NormalizeDSN(dsn, 5*time.Second)
	if err != nil {
		t.Fatalf("the %s value cannot be used: %v", EnvRealMySQLDSN, err)
	}
	config, err := driver.ParseDSN(normalized)
	if err != nil {
		t.Fatalf("the %s value cannot be parsed", EnvRealMySQLDSN)
	}

	seed, err := gorm.Open(mysql.Open(config.FormatDSN()), gormConfig())
	if err != nil {
		t.Fatalf("no MySQL server answered on the configured DSN: %v", err)
	}
	t.Cleanup(func() { closeDB(seed) })

	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	schema := "gopherlearn_it_" + hex.EncodeToString(suffix)
	if err := seed.WithContext(ctx).Exec("CREATE DATABASE " + schema +
		" CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci").Error; err != nil {
		t.Fatalf("%s needs a user that may CREATE DATABASE: %v", EnvRealMySQLDSN, err)
	}
	t.Cleanup(func() {
		drop, err := gorm.Open(mysql.Open(config.FormatDSN()), gormConfig())
		if err != nil {
			return
		}
		_ = drop.Exec("DROP DATABASE IF EXISTS " + schema).Error
		closeDB(drop)
	})

	config.DBName = schema
	db, err := gorm.Open(mysql.Open(config.FormatDSN()), gormConfig())
	if err != nil {
		t.Fatalf("opened the seed schema but not %s: %v", schema, err)
	}
	t.Cleanup(func() { closeDB(db) })

	for _, step := range []func(context.Context, *gorm.DB) error{Migrate, MigrateKnowledge} {
		if err := step(ctx, db); err != nil {
			t.Fatalf("migration against real MySQL failed: %v", err)
		}
	}
	for _, table := range integrationTables {
		if !db.Migrator().HasTable(table) {
			t.Fatalf("real MySQL has no %q table after Migrate", table)
		}
	}
	return db
}

func truncateAll(t *testing.T, db *gorm.DB) {
	t.Helper()
	ctx := t.Context()
	if err := db.WithContext(ctx).Exec("SET FOREIGN_KEY_CHECKS = 0").Error; err != nil {
		t.Fatal(err)
	}
	for _, table := range integrationTables {
		if err := db.WithContext(ctx).Exec("TRUNCATE TABLE " + table).Error; err != nil {
			t.Fatalf("truncate %s: %v", table, err)
		}
	}
	if err := db.WithContext(ctx).Exec("SET FOREIGN_KEY_CHECKS = 1").Error; err != nil {
		t.Fatal(err)
	}
}

func integrationUser(t *testing.T, db *gorm.DB, ctx context.Context, label string) string {
	t.Helper()
	user, err := NewRepository(db).CreateUser(ctx, &domain.User{
		Email: label + "-" + time.Now().UTC().Format("150405.000000") + "@example.invalid", DisplayName: label,
	})
	if err != nil {
		t.Fatal(err)
	}
	return user.ID
}

func integrationSession(t *testing.T, db *gorm.DB, ctx context.Context, userID, title string) string {
	t.Helper()
	session, err := NewRepository(db).CreateSession(ctx, &domain.Session{
		UserID: userID, Title: title, Mode: domain.SessionModeChat,
	})
	if err != nil {
		t.Fatal(err)
	}
	return session.ID
}
