package gormmysql

import (
	"context"
	"errors"
	"testing"

	"gorm.io/gorm"

	"github.com/xiaopang456/GopherLearn/internal/domain"
)

// Three claims about the hand-written memory door that only the SQL shape can answer: what
// rows a write leaves behind, whether the two statements in one request commit together, and
// how a row stored before the origin column existed is classified afterwards. The shared
// behavioural suite covers what a store *does*; these cover what the database ends up
// holding, which is what a later migration or a restart has to survive.

func storedOrigin(t *testing.T, db *gorm.DB, memoryID string) string {
	t.Helper()
	var origin string
	if err := db.Raw(`SELECT origin FROM memories WHERE memory_id = ?`, memoryID).Scan(&origin).Error; err != nil {
		t.Fatal(err)
	}
	return origin
}

// failMemoryInsert installs a create failure on the memories table and returns the way to
// take it off. The package's shared helper only removes its hook when the test ends, and
// this case has to fail once and then succeed, so the control has to be in the test's hands.
func failMemoryInsert(t *testing.T, db *gorm.DB) func() {
	t.Helper()
	const name = "test:fail-insert:memories"
	if err := db.Callback().Create().Before("gorm:create").Register(name, func(tx *gorm.DB) {
		if tx.Statement.Table == "memories" {
			tx.AddError(errors.New("injected create failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Callback().Create().Remove(name) })
	return func() {
		if err := db.Callback().Create().Remove(name); err != nil {
			t.Fatal(err)
		}
	}
}

func TestStoreUserAuthoredMemoryLeavesNoSourceRows(t *testing.T) {
	ctx := context.Background()
	store, db := newTestRepository(t)
	user := mustCreateUser(t, store, "authored-no-source@example.com")
	session := mustCreateSession(t, store, user.ID, domain.SessionModeChat)
	message, err := store.AppendMessage(ctx, user.ID, session.ID, &domain.Message{Role: "user", Content: "我平时用 Go 写服务"})
	if err != nil {
		t.Fatal(err)
	}

	authored, err := store.StoreUserAuthoredMemory(ctx, &domain.UserAuthoredMemory{
		UserID: user.ID, Scope: domain.MemoryScopeGlobal, Content: "我把值班交给了 SRE",
	})
	if err != nil {
		t.Fatal(err)
	}
	// A memory with no provenance has no child rows, and that is a fact about the database,
	// not just about the struct the call returned: an empty placeholder row would read back as
	// "the evidence existed and was lost", which is a different claim from "there was none".
	if got := countRows(t, db, `SELECT COUNT(*) FROM memory_sources WHERE memory_id = ?`, authored.MemoryID); got != 0 {
		t.Fatalf("a hand-written memory left %d source row(s), want none", got)
	}
	if got := storedOrigin(t, db, authored.MemoryID); got != string(domain.MemoryOriginUserAuthored) {
		t.Fatalf("stored origin = %q, want %q", got, domain.MemoryOriginUserAuthored)
	}

	// The same store, the other door: a proposal keeps its ordered evidence.
	draft := mustStoredDraft(t, store, user.ID, session.ID, message.ID, "代理读到的一条")
	confirmed, err := store.ConfirmMemoryDraft(ctx, user.ID, draft.MemoryID)
	if err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, db, `SELECT COUNT(*) FROM memory_sources WHERE memory_id = ?`, confirmed.MemoryID); got != 1 {
		t.Fatalf("a confirmed proposal kept %d source row(s), want the one it cited", got)
	}
	if got := storedOrigin(t, db, confirmed.MemoryID); got != string(domain.MemoryOriginAgentProposed) {
		t.Fatalf("stored origin for a proposal = %q, want %q", got, domain.MemoryOriginAgentProposed)
	}
}

// TestStoreUserAuthoredMemoryRollsBackTheRetirementWithTheInsert is the atomic half of the
// contract. The write retires the previous active answer and inserts the new one; if only
// the first half committed, the subject would be left with no active answer at all - and
// after a retry, with two. Injecting a failure into the insert is the only way to reach that
// point, because it never happens on its own.
func TestStoreUserAuthoredMemoryRollsBackTheRetirementWithTheInsert(t *testing.T) {
	ctx := context.Background()
	store, db := newTestRepository(t)
	user := mustCreateUser(t, store, "authored-rollback@example.com")

	predecessor, err := store.StoreUserAuthoredMemory(ctx, &domain.UserAuthoredMemory{
		UserID: user.ID, Scope: domain.MemoryScopeGlobal, Content: "第一版",
	})
	if err != nil {
		t.Fatal(err)
	}

	stopFailure := failMemoryInsert(t, db)
	if _, err := store.StoreUserAuthoredMemory(ctx, &domain.UserAuthoredMemory{
		UserID: user.ID, Scope: domain.MemoryScopeGlobal, Content: "第二版",
	}); err == nil {
		stopFailure()
		t.Fatal("the write succeeded despite the injected insert failure")
	}
	stopFailure()

	// The old answer is still the answer: a failed replacement must not have retired it.
	kept, err := store.GetMemory(ctx, user.ID, predecessor.MemoryID)
	if err != nil {
		t.Fatal(err)
	}
	if kept.Status != domain.MemoryStatusActive {
		t.Fatalf("the predecessor came out %q after a rolled-back replacement, want it still active", kept.Status)
	}
	if !kept.SupersededAt.IsZero() {
		t.Fatalf("a rolled-back write still stamped a superseded time: %+v", kept)
	}
	if got := countRows(t, db, `SELECT COUNT(*) FROM memories WHERE user_id = ?`, user.ID); got != 1 {
		t.Fatalf("memories rows = %d, want the one pre-existing row: the insert and the retirement must commit together", got)
	}
	if got := countRows(t, db, `SELECT COUNT(*) FROM memories WHERE content = ?`, "第二版"); got != 0 {
		t.Fatalf("the rejected sentence is stored anyway (%d row(s))", got)
	}

	// Same request again with no injected failure: one subject, one active answer, and the
	// predecessor retired by the write that replaced it.
	replacement, err := store.StoreUserAuthoredMemory(ctx, &domain.UserAuthoredMemory{
		UserID: user.ID, Scope: domain.MemoryScopeGlobal, Content: "第二版",
	})
	if err != nil {
		t.Fatal(err)
	}
	if replacement.MemoryID == predecessor.MemoryID {
		t.Fatal("the replacement reused the predecessor's id")
	}
	retired, err := store.GetMemory(ctx, user.ID, predecessor.MemoryID)
	if err != nil {
		t.Fatal(err)
	}
	if retired.Status != domain.MemoryStatusSuperseded || retired.SupersededAt.IsZero() {
		t.Fatalf("predecessor after the retry = %+v, want superseded with a time", retired)
	}
	active, err := store.ListActiveMemories(ctx, user.ID, domain.MemoryScopeGlobal, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].MemoryID != replacement.MemoryID {
		t.Fatalf("active listing = %+v, want exactly the retry's row", active)
	}
}

// TestAMemoryStoredBeforeTheOriginColumnIsStillAProposal is the migration rule the domain
// documents: an empty origin is read as agent-proposed, never as hand-written. Guessing the
// other way would promote every pre-existing row into a decision its author never claimed to
// have made by hand - and the row that proves it is a confirmed proposal with its origin
// cleared, which is exactly what a pre-column record looks like.
func TestAMemoryStoredBeforeTheOriginColumnIsStillAProposal(t *testing.T) {
	ctx := context.Background()
	store, db := newTestRepository(t)
	user := mustCreateUser(t, store, "authored-legacy@example.com")
	session := mustCreateSession(t, store, user.ID, domain.SessionModeChat)
	message, err := store.AppendMessage(ctx, user.ID, session.ID, &domain.Message{Role: "user", Content: "第一条"})
	if err != nil {
		t.Fatal(err)
	}
	draft := mustStoredDraft(t, store, user.ID, session.ID, message.ID, "升级前存下的一条")
	confirmed, err := store.ConfirmMemoryDraft(ctx, user.ID, draft.MemoryID)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&MemoryRecord{}).Where("memory_id = ?", confirmed.MemoryID).
		Update("origin", "").Error; err != nil {
		t.Fatal(err)
	}

	read, err := store.GetMemory(ctx, user.ID, confirmed.MemoryID)
	if err != nil {
		t.Fatal(err)
	}
	if read.Origin != "" {
		t.Fatalf("origin read back as %q, want the empty value the column started with", read.Origin)
	}
	if read.AuthoredBy() != domain.MemoryOriginAgentProposed {
		t.Fatalf("a pre-column row classified itself as %q, want agent_proposed", read.AuthoredBy())
	}
	// It is still the same record: the classification is a reading rule, not a rewrite.
	if read.Status != domain.MemoryStatusActive || read.SourceSessionID != session.ID ||
		len(read.SourceMessageIDs) != 1 || read.SourceMessageIDs[0] != message.ID {
		t.Fatalf("the row changed under the read: %+v", read)
	}
}
