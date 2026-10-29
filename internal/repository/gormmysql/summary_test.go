package gormmysql

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/repository"
)

// TestSummarySurvivesReopeningTheDatabase is the restart case: a summary is only
// worth the model call that produced it if a process restart reads it back with the
// same text, the same covered boundary and the same version. A boundary that came
// back shifted would make the next turn either re-summarize messages it already
// folded in or drop messages it never covered.
func TestSummarySurvivesReopeningTheDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "summary-reopen.db")
	db := openPlainTestDB(t, path)
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	repo := NewRepository(db)
	user := mustCreateUser(t, repo, "summary-reopen@example.com")
	session, err := repo.CreateSession(ctx, &domain.Session{UserID: user.ID, Title: "会话", Mode: domain.SessionModeChat})
	if err != nil {
		t.Fatal(err)
	}
	first, err := repo.CommitSessionSummary(ctx, &domain.SessionSummary{
		SessionID: session.ID, UserID: user.ID,
		Content: "用户在复习 Redis 持久化，尚未决定选型", CoveredMessageID: "message-12",
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	second, err := repo.CommitSessionSummary(ctx, &domain.SessionSummary{
		SessionID: session.ID, UserID: user.ID,
		Content: "用户已选定 AOF everysec，仍在比较 RDB", CoveredMessageID: "message-24",
	}, first.Version)
	if err != nil {
		t.Fatal(err)
	}

	reopened := openPlainTestDB(t, path)
	if err := Migrate(ctx, reopened); err != nil {
		t.Fatalf("migrating a database that already has summaries: %v", err)
	}
	stored, err := NewRepository(reopened).GetSessionSummary(ctx, user.ID, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Content != second.Content || stored.CoveredMessageID != "message-24" || stored.Version != 2 {
		t.Fatalf("after reopen = %+v, want the second version intact", stored)
	}
	if stored.UserID != user.ID || stored.SessionID != session.ID {
		t.Fatalf("identity fields were not stored: %+v", stored)
	}
	// The version is the CAS token, so it has to keep working across a restart:
	// building on the pre-restart version is the same stale write as any other.
	if _, err := NewRepository(reopened).CommitSessionSummary(ctx, &domain.SessionSummary{
		SessionID: session.ID, UserID: user.ID, Content: "stale", CoveredMessageID: "message-30",
	}, first.Version); !errors.Is(err, repository.ErrConflict) {
		t.Fatalf("stale commit after reopen error = %v, want ErrConflict", err)
	}
}

// TestSessionIDServesOneSummaryRow pins the uniqueness the version check depends on.
// Two turns that both read "no summary yet" must not both create one: the loser gets
// ErrConflict, which is the same answer a stale version gets, so the caller has one
// re-read path rather than two.
func TestSessionIDServesOneSummaryRow(t *testing.T) {
	ctx := context.Background()
	repo := NewRepository(newTestDB(t))
	user := mustCreateUser(t, repo, "summary-race@example.com")
	session, err := repo.CreateSession(ctx, &domain.Session{UserID: user.ID, Title: "会话", Mode: domain.SessionModeChat})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CommitSessionSummary(ctx, &domain.SessionSummary{
		SessionID: session.ID, UserID: user.ID, Content: "先到", CoveredMessageID: "message-2",
	}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CommitSessionSummary(ctx, &domain.SessionSummary{
		SessionID: session.ID, UserID: user.ID, Content: "后到", CoveredMessageID: "message-2",
	}, 0); !errors.Is(err, repository.ErrConflict) {
		t.Fatalf("second first-commit error = %v, want ErrConflict", err)
	}
	stored, err := repo.GetSessionSummary(ctx, user.ID, session.ID)
	if err != nil || stored.Content != "先到" || stored.Version != 1 {
		t.Fatalf("the loser rewrote the winner: %+v err %v", stored, err)
	}
}

// TestDeleteSessionCascadesTheSummary is the SQL side of a rule the in-memory store
// gets for free: a session that is gone must not leave behind a boundary id pointing
// into rows that no longer exist.
func TestDeleteSessionCascadesTheSummary(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	repo := NewRepository(db)
	user := mustCreateUser(t, repo, "summary-cascade@example.com")
	session, err := repo.CreateSession(ctx, &domain.Session{UserID: user.ID, Title: "会话", Mode: domain.SessionModeChat})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CommitSessionSummary(ctx, &domain.SessionSummary{
		SessionID: session.ID, UserID: user.ID, Content: "级联前", CoveredMessageID: "message-2",
	}, 0); err != nil {
		t.Fatal(err)
	}
	// Counted, not just read back: every per-session read also refuses on a missing
	// session, so an error alone cannot tell "the row was deleted" from "the row is
	// still there but unreachable", and an unreachable row is a leak.
	if got := countRows(t, db, "SELECT COUNT(*) FROM session_summaries"); got != 1 {
		t.Fatalf("summary rows before the delete = %d, want 1", got)
	}
	if err := repo.DeleteSession(ctx, user.ID, session.ID); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, db, "SELECT COUNT(*) FROM session_summaries"); got != 0 {
		t.Fatalf("summary rows after deleting the session = %d, want the cascade to remove them", got)
	}
	if _, err := repo.GetSessionSummary(ctx, user.ID, session.ID); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("after the delete error = %v, want ErrNotFound", err)
	}
	// A second session belonging to the same user is a fresh boundary, not an
	// inherited one.
	replacement, err := repo.CreateSession(ctx, &domain.Session{UserID: user.ID, Title: "另一会话", Mode: domain.SessionModeChat})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.GetSessionSummary(ctx, user.ID, replacement.ID); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("a new session reported a summary: %v", err)
	}
}
