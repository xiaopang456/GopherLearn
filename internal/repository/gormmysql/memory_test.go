package gormmysql

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"gorm.io/gorm"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/repository"
	"github.com/xiaopang456/GopherLearn/internal/repository/repositorytest"
)

// TestMemoryContract runs the shared long-term memory suite against the SQL store, so the
// production engine is held to exactly the rules the development store follows.
func TestMemoryContract(t *testing.T) {
	repositorytest.MemorySuite(t, func(t *testing.T) repository.Repository { return NewRepository(newTestDB(t)) })
}

// TestMemorySurvivesReopeningTheDatabase is the restart case: a confirmed memory is only
// worth the confirmation if a new process reads back the same state, the same superseded
// time, and the same provenance order.
func TestMemorySurvivesReopeningTheDatabase(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "memory-reopen.db")
	db := openTestDB(t, path)
	repo := NewRepository(db)
	user := mustCreateUser(t, repo, "memory-reopen@example.com")
	session, err := repo.CreateSession(ctx, &domain.Session{UserID: user.ID, Title: "会话", Mode: domain.SessionModeChat})
	if err != nil {
		t.Fatal(err)
	}
	first, err := repo.AppendMessage(ctx, user.ID, session.ID, &domain.Message{Role: "user", Content: "第一条"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := repo.AppendMessage(ctx, user.ID, session.ID, &domain.Message{Role: "assistant", Content: "第二条"})
	if err != nil {
		t.Fatal(err)
	}

	old := mustStoredDraft(t, repo, user.ID, session.ID, first.ID, "我用 Go 写后端")
	if _, err := repo.ConfirmMemoryDraft(ctx, user.ID, old.MemoryID); err != nil {
		t.Fatal(err)
	}
	replacement := mustStoredDraft(t, repo, user.ID, session.ID, second.ID, "我改用 Go 写基础服务")
	confirmed, err := repo.ConfirmMemoryDraft(ctx, user.ID, replacement.MemoryID)
	if err != nil {
		t.Fatal(err)
	}
	if confirmed.Status != domain.MemoryStatusActive || confirmed.MemoryID != replacement.MemoryID {
		t.Fatalf("confirm returned %+v, want the new memory active", confirmed)
	}
	rejected := mustStoredDraft(t, repo, user.ID, session.ID, first.ID, "被拒绝的候选")
	if _, err := repo.RejectMemoryDraft(ctx, user.ID, rejected.MemoryID); err != nil {
		t.Fatal(err)
	}
	supersededAt := mustReadMemory(t, repo, user.ID, old.MemoryID).SupersededAt

	reopened := NewRepository(openPlainTestDB(t, path))
	if err := Migrate(ctx, reopened.db); err != nil {
		t.Fatal(err)
	}
	if got := mustReadMemory(t, reopened, user.ID, old.MemoryID); got.Status != domain.MemoryStatusSuperseded ||
		!got.SupersededAt.Equal(supersededAt) {
		t.Fatalf("the superseded version came back as %+v", got)
	}
	if got := mustReadMemory(t, reopened, user.ID, replacement.MemoryID); got.Status != domain.MemoryStatusActive ||
		!reflect.DeepEqual(got.SourceMessageIDs, []string{second.ID}) {
		t.Fatalf("the active version came back as %+v", got)
	}
	if found, err := reopened.ListActiveMemories(ctx, user.ID, domain.MemoryScopeGlobal, ""); err != nil ||
		len(found) != 1 || found[0].MemoryID != replacement.MemoryID {
		t.Fatalf("listing after reopen = %+v err %v, want only the newer active memory", found, err)
	}
	// A rejected draft stays rejected across a restart: the process boundary must not be
	// able to hand the user a confirmation they already refused.
	if _, err := reopened.ConfirmMemoryDraft(ctx, user.ID, rejected.MemoryID); !errors.Is(err, repository.ErrConflict) {
		t.Fatalf("confirming a rejected draft after reopen = %v, want a conflict", err)
	}
}

// TestDraftRollsBackWhenTheProvenanceRowsFail checks that a memory cannot be stored with
// its content but without its evidence.
func TestDraftRollsBackWhenTheProvenanceRowsFail(t *testing.T) {
	ctx := t.Context()
	db := newTestDB(t)
	repo := NewRepository(db)
	user := mustCreateUser(t, repo, "memory-rollback@example.com")
	session, err := repo.CreateSession(ctx, &domain.Session{UserID: user.ID, Title: "会话", Mode: domain.SessionModeChat})
	if err != nil {
		t.Fatal(err)
	}
	message, err := repo.AppendMessage(ctx, user.ID, session.ID, &domain.Message{Role: "user", Content: "证据"})
	if err != nil {
		t.Fatal(err)
	}

	failCreateOn(t, db, "memory_sources")
	if _, err := repo.CreateMemoryDraft(ctx, &domain.MemoryDraft{
		UserID: user.ID, Scope: domain.MemoryScopeGlobal, Content: "半条记忆",
		SourceSessionID: session.ID, SourceMessageIDs: []string{message.ID},
	}); err == nil {
		t.Fatal("the draft was stored although its provenance rows failed")
	}
	if found := countRows(t, db, "SELECT COUNT(*) FROM memories"); found != 0 {
		t.Fatalf("memories rows = %d, want the whole insert rolled back", found)
	}
	if found := countRows(t, db, "SELECT COUNT(*) FROM memory_sources"); found != 0 {
		t.Fatalf("memory_sources rows = %d, want none", found)
	}

	// The idempotent write must fail the same way, not half-apply through its own copy of
	// the insert. Both paths share one transaction, and this is what proves it.
	failCreateOn(t, db, "memory_sources")
	if _, _, err := repo.CreateMemoryDraftOnce(ctx, &domain.MemoryDraft{
		UserID: user.ID, Scope: domain.MemoryScopeGlobal, Content: "半条幂等记忆", Confidence: 1,
		SourceSessionID: session.ID, SourceMessageIDs: []string{message.ID},
	}); err == nil {
		t.Fatal("the idempotent draft was stored although its provenance rows failed")
	}
	if found := countRows(t, db, "SELECT COUNT(*) FROM memories"); found != 0 {
		t.Fatalf("memories rows after the idempotent attempt = %d, want the whole insert rolled back", found)
	}
}

// TestConfirmationRollsBackWhenTheSupersedeFails is the interesting partial failure: the
// promotion has already been written when the retirement of the predecessor fails. If the
// transaction did not undo both, the store would hold two active memories for one subject
// - the exact state the supersede rule exists to make impossible.
func TestConfirmationRollsBackWhenTheSupersedeFails(t *testing.T) {
	ctx := t.Context()
	db := newTestDB(t)
	repo := NewRepository(db)
	user := mustCreateUser(t, repo, "memory-supersede@example.com")
	session, err := repo.CreateSession(ctx, &domain.Session{UserID: user.ID, Title: "会话", Mode: domain.SessionModeChat})
	if err != nil {
		t.Fatal(err)
	}
	message, err := repo.AppendMessage(ctx, user.ID, session.ID, &domain.Message{Role: "user", Content: "证据"})
	if err != nil {
		t.Fatal(err)
	}
	older := mustStoredDraft(t, repo, user.ID, session.ID, message.ID, "旧答案")
	if _, err := repo.ConfirmMemoryDraft(ctx, user.ID, older.MemoryID); err != nil {
		t.Fatal(err)
	}
	newer := mustStoredDraft(t, repo, user.ID, session.ID, message.ID, "新答案")

	failSecondUpdateOnMemories(t, db)
	if _, err := repo.ConfirmMemoryDraft(ctx, user.ID, newer.MemoryID); err == nil {
		t.Fatal("the confirmation succeeded although retiring the predecessor failed")
	}

	// Read back through a fresh query: the failed statement was inside a transaction that
	// has already been rolled back, so anything visible now is committed state.
	if got := mustReadMemory(t, repo, user.ID, older.MemoryID); got.Status != domain.MemoryStatusActive {
		t.Fatalf("the predecessor ended as %q, want it still active", got.Status)
	}
	if got := mustReadMemory(t, repo, user.ID, newer.MemoryID); got.Status != domain.MemoryStatusPending {
		t.Fatalf("the draft ended as %q, want it still pending", got.Status)
	}
	if got := mustReadMemory(t, repo, user.ID, newer.MemoryID); !got.SupersededAt.IsZero() {
		t.Fatalf("a pending draft carries a superseded time: %+v", got)
	}
	if found, err := repo.ListActiveMemories(ctx, user.ID, domain.MemoryScopeGlobal, ""); err != nil ||
		len(found) != 1 || found[0].MemoryID != older.MemoryID {
		t.Fatalf("active globals after the rollback = %+v err %v, want exactly the predecessor", found, err)
	}
	// The evidence is still attached to the draft that was not promoted, so the same
	// confirmation can succeed once the failing statement is gone.
	if _, err := repo.ConfirmMemoryDraft(ctx, user.ID, newer.MemoryID); err != nil {
		t.Fatalf("retrying the confirmation after the injected failure: %v", err)
	}
	if found, err := repo.ListActiveMemories(ctx, user.ID, domain.MemoryScopeGlobal, ""); err != nil ||
		len(found) != 1 || found[0].MemoryID != newer.MemoryID {
		t.Fatalf("active globals after the retry = %+v err %v, want only the newer memory", found, err)
	}
}

// TestMemoryProvenanceOrderComesFromPositionNotRowOrder is why provenance carries an
// explicit position column. Reading it back in whatever order the engine returns is not
// "the order it was cited", so the test rewrites the stored positions to run against the
// physical row order and requires the read to follow the data.
func TestMemoryProvenanceOrderComesFromPositionNotRowOrder(t *testing.T) {
	ctx := t.Context()
	db := newTestDB(t)
	repo := NewRepository(db)
	user := mustCreateUser(t, repo, "memory-order@example.com")
	session, err := repo.CreateSession(ctx, &domain.Session{UserID: user.ID, Title: "会话", Mode: domain.SessionModeChat})
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, 3)
	for index, content := range []string{"第一条", "第二条", "第三条"} {
		message, err := repo.AppendMessage(ctx, user.ID, session.ID, &domain.Message{Role: "user", Content: content})
		if err != nil {
			t.Fatal(err)
		}
		if index == 1 {
			// Skip the middle message as evidence: three rows, cited in a written order.
			continue
		}
		ids = append(ids, message.ID)
	}
	// The draft cites two messages, stored with positions 0 -> first and 1 -> third, which
	// is also the physical order. Swapping the positions keeps the rows where they are and
	// reverses the citation order - the only arrangement that can tell the two apart.
	stored, err := repo.CreateMemoryDraft(ctx, &domain.MemoryDraft{
		UserID: user.ID, Scope: domain.MemoryScopeGlobal, Content: "引用的顺序", Confidence: 0.5,
		SourceSessionID: session.ID, SourceMessageIDs: ids,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Swap the two positions through a spare value: (memory_id, position) is unique, so
	// exchanging them in one statement would collide with the row still sitting there.
	for step, statement := range []string{
		"UPDATE memory_sources SET position = 7 WHERE memory_id = ? AND position = 0",
		"UPDATE memory_sources SET position = 0 WHERE memory_id = ? AND position = 1",
		"UPDATE memory_sources SET position = 1 WHERE memory_id = ? AND position = 7",
	} {
		if err := db.Exec(statement, stored.MemoryID).Error; err != nil {
			t.Fatalf("swap step %d: %v", step, err)
		}
	}

	read, err := repo.GetMemory(ctx, user.ID, stored.MemoryID)
	if err != nil {
		t.Fatal(err)
	}
	if len(read.SourceMessageIDs) != 2 || read.SourceMessageIDs[0] != ids[1] || read.SourceMessageIDs[1] != ids[0] {
		t.Fatalf("provenance = %v, want the order the position column now declares (%v, %v)",
			read.SourceMessageIDs, ids[1], ids[0])
	}
	found, err := repo.ListActiveMemories(ctx, user.ID, domain.MemoryScopeEpisodic, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 0 {
		t.Fatalf("a pending draft appeared in an active listing: %+v", found)
	}
}

func mustStoredDraft(t *testing.T, repo repository.Repository, userID, sessionID, messageID, content string) *domain.Memory {
	t.Helper()
	stored, err := repo.CreateMemoryDraft(t.Context(), &domain.MemoryDraft{
		UserID: userID, Scope: domain.MemoryScopeGlobal, Content: content,
		SourceSessionID: sessionID, SourceMessageIDs: []string{messageID},
	})
	if err != nil {
		t.Fatal(err)
	}
	return stored
}

func mustReadMemory(t *testing.T, repo repository.Repository, userID, memoryID string) domain.Memory {
	t.Helper()
	memory, err := repo.GetMemory(context.Background(), userID, memoryID)
	if err != nil {
		t.Fatalf("read memory %s: %v", memoryID, err)
	}
	return *memory
}

// failSecondUpdateOnMemories injects a database error into the second update of the
// memories table inside one transaction, which is the supersede step of a confirmation.
func failSecondUpdateOnMemories(t *testing.T, db *gorm.DB) {
	t.Helper()
	name := "test:fail-second-update:memories"
	updates := 0
	if err := db.Callback().Update().Before("gorm:update").Register(name, func(tx *gorm.DB) {
		if tx.Statement.Table != "memories" {
			return
		}
		updates++
		if updates == 2 {
			tx.AddError(errors.New("injected supersede failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Callback().Update().Remove(name) })
}
