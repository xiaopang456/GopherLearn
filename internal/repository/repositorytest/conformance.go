// Package repositorytest holds the behavioural contract every
// repository.Repository implementation must satisfy for one conversation turn:
// append order, session ownership, and the citations an assistant message was
// built from. It lives in a normal package rather than a _test file so the
// in-memory store and the GORM store are held to the same rules instead of each
// growing its own interpretation of what "stored" means.
package repositorytest

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/repository"
)

// Factory builds a fresh, empty repository for one subtest.
type Factory func(t *testing.T) repository.Repository

// Suite runs the shared repository contract for messages and citations.
func Suite(t *testing.T, newStore Factory) {
	t.Helper()
	ctx := context.Background()

	t.Run("turn round trips in append order", func(t *testing.T) {
		store := newStore(t)
		userID := mustUser(t, store, "owner")
		sessionID := mustSession(t, store, userID)

		question, err := store.AppendMessage(ctx, userID, sessionID, &domain.Message{Role: "user", Content: "question"})
		if err != nil {
			t.Fatal(err)
		}
		if question.ID == "" || question.CreatedAt.IsZero() {
			t.Fatalf("generated identity missing: %#v", question)
		}
		if question.SessionID != sessionID {
			t.Fatalf("message bound to %q, want the requested session %q", question.SessionID, sessionID)
		}
		if _, err := store.AppendMessage(ctx, userID, sessionID, &domain.Message{Role: "assistant", Content: "answer"}); err != nil {
			t.Fatal(err)
		}
		messages, err := store.ListMessages(ctx, userID, sessionID)
		if err != nil {
			t.Fatal(err)
		}
		if len(messages) != 2 || messages[0].Content != "question" || messages[1].Content != "answer" {
			t.Fatalf("messages = %#v, want the pair in append order", messages)
		}
		for _, message := range messages {
			if len(message.Sources) != 0 {
				t.Fatalf("an ungrounded message grew citations: %#v", message)
			}
		}
	})

	t.Run("citations survive a re-read in answer order", func(t *testing.T) {
		store := newStore(t)
		userID := mustUser(t, store, "owner")
		sessionID := mustSession(t, store, userID)
		cited := []domain.Source{
			{SourceID: "src-2", DocumentID: "doc-1", CollectionID: "col-1", ChunkID: "chunk-2", Title: "Redis 持久化", Section: "AOF", Snippet: "第二条"},
			{SourceID: "src-1", DocumentID: "doc-1", CollectionID: "col-1", ChunkID: "chunk-1", Title: "Redis 持久化", Section: "RDB", Snippet: "第一条"},
		}
		stored, err := store.AppendMessage(ctx, userID, sessionID, &domain.Message{Role: "assistant", Content: "answer", Sources: cited})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(stored.Sources, cited) {
			t.Fatalf("append returned %#v, want the citations it was given, in order", stored.Sources)
		}
		messages, err := store.ListMessages(ctx, userID, sessionID)
		if err != nil {
			t.Fatal(err)
		}
		if len(messages) != 1 {
			t.Fatalf("messages = %d, want 1", len(messages))
		}
		// Order is the answer's own citation order, not sorted by identifier:
		// the two sources above are deliberately listed so that a store that
		// ordered by source_id or chunk_id would return them the other way round.
		if !reflect.DeepEqual(messages[0].Sources, cited) {
			t.Fatalf("stored citations = %#v, want %#v", messages[0].Sources, cited)
		}
		if _, err := store.AppendMessage(ctx, userID, sessionID, &domain.Message{Role: "user", Content: "question"}); err != nil {
			t.Fatal(err)
		}
		again, err := store.ListMessages(ctx, userID, sessionID)
		if err != nil {
			t.Fatal(err)
		}
		if len(again) != 2 || !reflect.DeepEqual(again[0].Sources, cited) {
			t.Fatalf("a later append disturbed the earlier citation set: %#v", again)
		}
		if len(again[1].Sources) != 0 {
			t.Fatalf("the user message grew citations: %#v", again[1].Sources)
		}
	})

	t.Run("returned citations are detached copies", func(t *testing.T) {
		store := newStore(t)
		userID := mustUser(t, store, "owner")
		sessionID := mustSession(t, store, userID)
		given := []domain.Source{{SourceID: "src-1", ChunkID: "chunk-1", Title: "原始标题"}}
		stored, err := store.AppendMessage(ctx, userID, sessionID, &domain.Message{Role: "assistant", Content: "answer", Sources: given})
		if err != nil {
			t.Fatal(err)
		}
		given[0].Title = "调用方改了自己的切片"
		stored.Sources[0].Title = "调用方改了返回值"
		messages, err := store.ListMessages(ctx, userID, sessionID)
		if err != nil {
			t.Fatal(err)
		}
		if len(messages) != 1 || messages[0].Sources[0].Title != "原始标题" {
			t.Fatalf("stored citations = %#v, want what was handed to AppendMessage", messages[0].Sources)
		}
		messages[0].Sources[0].Title = "读取方改了返回值"
		reread, err := store.ListMessages(ctx, userID, sessionID)
		if err != nil {
			t.Fatal(err)
		}
		if reread[0].Sources[0].Title != "原始标题" {
			t.Fatalf("a caller mutated stored state: %#v", reread[0].Sources)
		}
	})

	t.Run("citations are invisible to another user", func(t *testing.T) {
		store := newStore(t)
		owner := mustUser(t, store, "owner")
		sessionID := mustSession(t, store, owner)
		intruder := mustUser(t, store, "intruder")
		if _, err := store.AppendMessage(ctx, owner, sessionID, &domain.Message{
			Role: "assistant", Content: "answer", Sources: []domain.Source{{SourceID: "src-1", ChunkID: "chunk-1"}},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.ListMessages(ctx, intruder, sessionID); !reflect.DeepEqual(err, repository.ErrNotFound) {
			t.Fatalf("error = %v, want ErrNotFound for another user's session", err)
		}
		if _, err := store.AppendMessage(ctx, intruder, sessionID, &domain.Message{Role: "user", Content: "question"}); err != repository.ErrNotFound {
			t.Fatalf("error = %v, want ErrNotFound", err)
		}
		messages, err := store.ListMessages(ctx, owner, sessionID)
		if err != nil {
			t.Fatal(err)
		}
		if len(messages) != 1 || len(messages[0].Sources) != 1 {
			t.Fatalf("owner's history changed: %#v", messages)
		}
	})

	t.Run("deleting a session removes its citations", func(t *testing.T) {
		store := newStore(t)
		userID := mustUser(t, store, "owner")
		first := mustSession(t, store, userID)
		second := mustSession(t, store, userID)
		cited := []domain.Source{{SourceID: "src-1", ChunkID: "chunk-1", Title: "Redis"}}
		if _, err := store.AppendMessage(ctx, userID, first, &domain.Message{Role: "assistant", Content: "answer", Sources: cited}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.AppendMessage(ctx, userID, second, &domain.Message{Role: "assistant", Content: "other answer", Sources: cited}); err != nil {
			t.Fatal(err)
		}
		if err := store.DeleteSession(ctx, userID, first); err != nil {
			t.Fatal(err)
		}
		if _, err := store.ListMessages(ctx, userID, first); err != repository.ErrNotFound {
			t.Fatalf("error = %v, want the deleted session to be gone", err)
		}
		// The surviving session must keep exactly what it had: a cascade keyed on
		// the wrong identifier would delete or orphan the other session's rows.
		messages, err := store.ListMessages(ctx, userID, second)
		if err != nil {
			t.Fatal(err)
		}
		if len(messages) != 1 || !reflect.DeepEqual(messages[0].Sources, cited) {
			t.Fatalf("the other session's citations = %#v, want them untouched", messages[0].Sources)
		}
	})

	t.Run("invalid turns are refused without writing", func(t *testing.T) {
		store := newStore(t)
		userID := mustUser(t, store, "owner")
		sessionID := mustSession(t, store, userID)
		if _, err := store.AppendMessage(ctx, userID, sessionID, nil); err != repository.ErrInvalidArgument {
			t.Fatalf("error = %v, want ErrInvalidArgument", err)
		}
		if _, err := store.AppendMessage(ctx, userID, "session-unknown", &domain.Message{Role: "user", Content: "question"}); err != repository.ErrNotFound {
			t.Fatalf("error = %v, want ErrNotFound for an unknown session", err)
		}
		if _, err := store.AppendMessage(nil, userID, sessionID, &domain.Message{Role: "user", Content: "question"}); err == nil {
			t.Fatal("a nil context was accepted")
		}
		messages, err := store.ListMessages(ctx, userID, sessionID)
		if err != nil {
			t.Fatal(err)
		}
		if len(messages) != 0 {
			t.Fatalf("refused writes left %d messages behind: %#v", len(messages), messages)
		}
	})

	// Session summaries: the same rules for both stores, because a rolling summary
	// that behaves differently in memory and in SQL would mean the version check and
	// the cascade are only tested where they are not enforced.
	t.Run("a session starts without a summary", func(t *testing.T) {
		store := newStore(t)
		userID := mustUser(t, store, "owner")
		sessionID := mustSession(t, store, userID)
		if _, err := store.GetSessionSummary(ctx, userID, sessionID); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("error = %v, want ErrNotFound", err)
		}
		// A session that does not exists is reported exactly like one with no summary.
		if _, err := store.GetSessionSummary(ctx, userID, "session-never-created"); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("unknown session error = %v, want the same ErrNotFound", err)
		}
	})

	t.Run("the first commit builds version one from no summary", func(t *testing.T) {
		store := newStore(t)
		userID := mustUser(t, store, "owner")
		sessionID := mustSession(t, store, userID)
		first, err := store.CommitSessionSummary(ctx, &domain.SessionSummary{
			SessionID: sessionID, UserID: userID, Content: "用户目标：复习 Redis 持久化", CoveredMessageID: "message-2",
		}, 0)
		if err != nil {
			t.Fatal(err)
		}
		if first.Version != 1 || first.UpdatedAt.IsZero() {
			t.Fatalf("committed = %+v, want version 1 with an update time", first)
		}
		stored, err := store.GetSessionSummary(ctx, userID, sessionID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.Content != first.Content || stored.CoveredMessageID != "message-2" ||
			stored.SessionID != sessionID || stored.UserID != userID {
			t.Fatalf("read-back = %+v, want the committed summary", stored)
		}
	})

	t.Run("each commit advances the version and the boundary", func(t *testing.T) {
		store := newStore(t)
		userID := mustUser(t, store, "owner")
		sessionID := mustSession(t, store, userID)
		if _, err := store.CommitSessionSummary(ctx, &domain.SessionSummary{
			SessionID: sessionID, UserID: userID, Content: "第一版", CoveredMessageID: "message-2",
		}, 0); err != nil {
			t.Fatal(err)
		}
		second, err := store.CommitSessionSummary(ctx, &domain.SessionSummary{
			SessionID: sessionID, UserID: userID, Content: "第二版", CoveredMessageID: "message-6",
		}, 1)
		if err != nil {
			t.Fatal(err)
		}
		if second.Version != 2 {
			t.Fatalf("version = %d, want 2", second.Version)
		}
		stored, err := store.GetSessionSummary(ctx, userID, sessionID)
		if err != nil || stored.Content != "第二版" || stored.CoveredMessageID != "message-6" {
			t.Fatalf("stored = %+v err %v, want the second version", stored, err)
		}
	})

	t.Run("a stale commit is refused and changes nothing", func(t *testing.T) {
		store := newStore(t)
		userID := mustUser(t, store, "owner")
		sessionID := mustSession(t, store, userID)
		if _, err := store.CommitSessionSummary(ctx, &domain.SessionSummary{
			SessionID: sessionID, UserID: userID, Content: "current", CoveredMessageID: "message-4",
		}, 0); err != nil {
			t.Fatal(err)
		}
		// Someone else moved on to version 2 while this writer was away.
		if _, err := store.CommitSessionSummary(ctx, &domain.SessionSummary{
			SessionID: sessionID, UserID: userID, Content: "newer", CoveredMessageID: "message-8",
		}, 1); err != nil {
			t.Fatal(err)
		}
		if _, err := store.CommitSessionSummary(ctx, &domain.SessionSummary{
			SessionID: sessionID, UserID: userID, Content: "too late", CoveredMessageID: "message-12",
		}, 1); !errors.Is(err, repository.ErrConflict) {
			t.Fatalf("stale commit error = %v, want ErrConflict", err)
		}
		stored, err := store.GetSessionSummary(ctx, userID, sessionID)
		if err != nil || stored.Content != "newer" || stored.Version != 2 || stored.CoveredMessageID != "message-8" {
			t.Fatalf("a refused commit rewrote the row: %+v err %v", stored, err)
		}
	})

	t.Run("summaries are invisible to another user", func(t *testing.T) {
		store := newStore(t)
		owner := mustUser(t, store, "owner")
		intruder := mustUser(t, store, "intruder")
		sessionID := mustSession(t, store, owner)
		if _, err := store.CommitSessionSummary(ctx, &domain.SessionSummary{
			SessionID: sessionID, UserID: owner, Content: "secret", CoveredMessageID: "message-2",
		}, 0); err != nil {
			t.Fatal(err)
		}
		if _, err := store.GetSessionSummary(ctx, intruder, sessionID); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("cross-user read error = %v, want ErrNotFound", err)
		}
		// Writing to another user's session is the same answer, so this cannot be used
		// to probe whether a session exists at all.
		if _, err := store.CommitSessionSummary(ctx, &domain.SessionSummary{
			SessionID: sessionID, UserID: intruder, Content: "overwrite", CoveredMessageID: "message-2",
		}, 0); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("cross-user commit error = %v, want ErrNotFound", err)
		}
		stored, err := store.GetSessionSummary(ctx, owner, sessionID)
		if err != nil || stored.Content != "secret" {
			t.Fatalf("the owner's summary changed: %+v err %v", stored, err)
		}
	})

	t.Run("deleting a session removes its summary", func(t *testing.T) {
		store := newStore(t)
		userID := mustUser(t, store, "owner")
		sessionID := mustSession(t, store, userID)
		if _, err := store.CommitSessionSummary(ctx, &domain.SessionSummary{
			SessionID: sessionID, UserID: userID, Content: "要被级联删掉的摘要", CoveredMessageID: "message-2",
		}, 0); err != nil {
			t.Fatal(err)
		}
		if err := store.DeleteSession(ctx, userID, sessionID); err != nil {
			t.Fatal(err)
		}
		if _, err := store.GetSessionSummary(ctx, userID, sessionID); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("after the cascade error = %v, want ErrNotFound", err)
		}
		// A new session for the same user must not inherit the deleted one's boundary.
		replacement := mustSession(t, store, userID)
		if _, err := store.GetSessionSummary(ctx, userID, replacement); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("a fresh session reported a summary: %v", err)
		}
	})

	t.Run("an invalid summary is refused without writing", func(t *testing.T) {
		store := newStore(t)
		userID := mustUser(t, store, "owner")
		sessionID := mustSession(t, store, userID)
		cases := []struct {
			name    string
			value   *domain.SessionSummary
			version int64
		}{
			{"nil", nil, 0},
			{"no content", &domain.SessionSummary{SessionID: sessionID, UserID: userID, CoveredMessageID: "message-2"}, 0},
			{"no boundary", &domain.SessionSummary{SessionID: sessionID, UserID: userID, Content: "text"}, 0},
			{"negative version", &domain.SessionSummary{
				SessionID: sessionID, UserID: userID, Content: "text", CoveredMessageID: "message-2",
			}, -1},
		}
		for _, testCase := range cases {
			if _, err := store.CommitSessionSummary(ctx, testCase.value, testCase.version); !errors.Is(err, repository.ErrInvalidArgument) {
				t.Fatalf("%s: error = %v, want ErrInvalidArgument", testCase.name, err)
			}
		}
		if _, err := store.GetSessionSummary(ctx, userID, sessionID); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("refused commits left a row behind: %v", err)
		}
	})
}

func mustUser(t *testing.T, store repository.Repository, label string) string {
	t.Helper()
	user, err := store.CreateUser(context.Background(), &domain.User{Email: label + "@example.com", DisplayName: label})
	if err != nil {
		t.Fatal(err)
	}
	return user.ID
}

func mustSession(t *testing.T, store repository.Repository, userID string) string {
	t.Helper()
	session, err := store.CreateSession(context.Background(), &domain.Session{UserID: userID, Title: "会话", Mode: domain.SessionModeChat})
	if err != nil {
		t.Fatal(err)
	}
	return session.ID
}
