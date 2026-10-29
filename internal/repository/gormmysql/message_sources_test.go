package gormmysql

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gorm.io/gorm"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/repository"
	"github.com/xiaopang456/GopherLearn/internal/repository/repositorytest"
)

// TestRepositoryContract runs the shared message and citation suite against the
// GORM store on the SQLite test engine, so the two implementations cannot drift
// on what "stored with its citations" means.
func TestRepositoryContract(t *testing.T) {
	repositorytest.Suite(t, func(*testing.T) repository.Repository { return NewRepository(newTestDB(t)) })
}

func citationFixture(t *testing.T) (*Repository, *gorm.DB, string, string) {
	t.Helper()
	repo, db := newTestRepository(t)
	user := mustCreateUser(t, repo, "cites@example.com")
	session := mustCreateSession(t, repo, user.ID, domain.SessionModeChat)
	return repo, db, user.ID, session.ID
}

// TestCitationsSurviveReopeningTheDatabase is the restart case the in-memory
// suite cannot reach: a process that comes back must serve the same evidence for
// an answer it gave before, because that answer is already on the user's screen.
func TestCitationsSurviveReopeningTheDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "reopen.db")
	db := openTestDB(t, path)
	repo := NewRepository(db)
	user := mustCreateUser(t, repo, "reopen@example.com")
	session := mustCreateSession(t, repo, user.ID, domain.SessionModeChat)
	cited := []domain.Source{
		{SourceID: "src-b", DocumentID: "doc-1", CollectionID: "col-1", ChunkID: "chunk-b", Title: "Redis 持久化", Section: "AOF", Snippet: "追加写"},
		{SourceID: "src-a", DocumentID: "doc-1", CollectionID: "col-1", ChunkID: "chunk-a", Title: "Redis 持久化", Section: "RDB", Snippet: "快照"},
	}
	if _, err := repo.AppendMessage(ctx, user.ID, session.ID, &domain.Message{Role: "user", Content: "question"}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AppendMessage(ctx, user.ID, session.ID, &domain.Message{Role: "assistant", Content: "answer", Sources: cited}); err != nil {
		t.Fatal(err)
	}
	closeDB(db)

	// A brand new handle on the same file, migrated again the way a restart does.
	reopened := openPlainTestDB(t, path)
	if err := Migrate(ctx, reopened); err != nil {
		t.Fatalf("migrate on restart: %v", err)
	}
	messages, err := NewRepository(reopened).ListMessages(ctx, user.ID, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 {
		t.Fatalf("messages after reopen = %d, want the pair", len(messages))
	}
	if len(messages[0].Sources) != 0 {
		t.Fatalf("the user message grew citations: %#v", messages[0].Sources)
	}
	if !reflect.DeepEqual(messages[1].Sources, cited) {
		t.Fatalf("citations after reopen = %#v, want %#v in answer order", messages[1].Sources, cited)
	}
}

// TestAppendMessageRollsBackTheWholeTurnWhenACitationInsertFails proves the rule
// the requirement is about: there is no state where the answer is saved and its
// sources are missing. A half-grounded answer is a claim with nothing behind it.
func TestAppendMessageRollsBackTheWholeTurnWhenACitationInsertFails(t *testing.T) {
	ctx := context.Background()
	repo, db, userID, sessionID := citationFixture(t)
	failCreateOn(t, db, "message_sources")

	_, err := repo.AppendMessage(ctx, userID, sessionID, &domain.Message{
		Role:    "assistant",
		Content: "answer",
		Sources: []domain.Source{{SourceID: "src-1", ChunkID: "chunk-1"}, {SourceID: "src-2", ChunkID: "chunk-2"}},
	})
	if err == nil {
		t.Fatal("the turn succeeded although a citation row could not be written")
	}
	if messages, err := repo.ListMessages(ctx, userID, sessionID); err != nil || len(messages) != 0 {
		t.Fatalf("messages = %#v (err %v), want the turn rolled back with the citation", messages, err)
	}
	if got := countRows(t, db, "SELECT COUNT(*) FROM message_sources"); got != 0 {
		t.Fatalf("citation rows = %d, want none left behind", got)
	}
	if got := countRows(t, db, "SELECT COUNT(*) FROM messages"); got != 0 {
		t.Fatalf("message rows = %d, want the answer rolled back too", got)
	}
}

// TestAppendMessageRollsBackCitationsWhenTheMessageTurnFails checks the other
// direction, because an orphaned citation row is just as wrong: it names an
// answer that was never stored.
func TestAppendMessageRollsBackCitationsWhenTheMessageTurnFails(t *testing.T) {
	ctx := context.Background()
	repo, db, userID, sessionID := citationFixture(t)
	failUpdateOn(t, db, "sessions")

	_, err := repo.AppendMessage(ctx, userID, sessionID, &domain.Message{
		Role:    "assistant",
		Content: "answer",
		Sources: []domain.Source{{SourceID: "src-1", ChunkID: "chunk-1"}},
	})
	if err == nil {
		t.Fatal("the turn succeeded although the session could not be updated")
	}
	if got := countRows(t, db, "SELECT COUNT(*) FROM message_sources"); got != 0 {
		t.Fatalf("citation rows = %d, want them rolled back with the failed turn", got)
	}
	if got := countRows(t, db, "SELECT COUNT(*) FROM messages"); got != 0 {
		t.Fatalf("message rows = %d, want the failed turn gone", got)
	}
}

// TestDeleteSessionRemovesCitationRowsOnlyForThatSession pins the cascade
// through the messages table. Keying it on anything looser would either leave
// citation rows behind or delete another session's evidence.
func TestDeleteSessionRemovesCitationRowsOnlyForThatSession(t *testing.T) {
	ctx := context.Background()
	repo, db, userID, first := citationFixture(t)
	second := mustCreateSession(t, repo, userID, domain.SessionModeChat).ID
	cited := []domain.Source{{SourceID: "src-1", ChunkID: "chunk-1"}, {SourceID: "src-2", ChunkID: "chunk-2"}}

	for _, sessionID := range []string{first, second} {
		if _, err := repo.AppendMessage(ctx, userID, sessionID, &domain.Message{Role: "user", Content: "question"}); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.AppendMessage(ctx, userID, sessionID, &domain.Message{Role: "assistant", Content: "answer", Sources: cited}); err != nil {
			t.Fatal(err)
		}
	}
	if got := countRows(t, db, "SELECT COUNT(*) FROM message_sources"); got != 4 {
		t.Fatalf("citation rows before delete = %d, want two per grounded answer", got)
	}

	if err := repo.DeleteSession(ctx, userID, first); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, db, "SELECT COUNT(*) FROM message_sources"); got != 2 {
		t.Fatalf("citation rows after delete = %d, want only the deleted session's gone", got)
	}
	if got := countRows(t, db, "SELECT COUNT(*) FROM messages WHERE session_id = ?", first); got != 0 {
		t.Fatalf("messages of the deleted session = %d, want none", got)
	}
	messages, err := repo.ListMessages(ctx, userID, second)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || !reflect.DeepEqual(messages[1].Sources, cited) {
		t.Fatalf("surviving session = %#v, want its citations untouched", messages)
	}

	// Deleting a session whose messages are gone must not strand rows either:
	// nothing in the schema lets a citation outlive its message.
	if err := repo.DeleteSession(ctx, userID, second); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, db, "SELECT COUNT(*) FROM message_sources"); got != 0 {
		t.Fatalf("citation rows left = %d, want zero", got)
	}
}

// TestCitationsAreReadBackByPositionNotRowOrder proves `position` is the order
// contract, not the order rows happen to come back in. The positions are swapped
// underneath the repository, so any read that relied on physical row order would
// still return the old sequence.
func TestCitationsAreReadBackByPositionNotRowOrder(t *testing.T) {
	ctx := context.Background()
	repo, db, userID, sessionID := citationFixture(t)
	if _, err := repo.AppendMessage(ctx, userID, sessionID, &domain.Message{
		Role:    "assistant",
		Content: "answer",
		Sources: []domain.Source{{SourceID: "src-first", ChunkID: "chunk-1"}, {SourceID: "src-second", ChunkID: "chunk-2"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("UPDATE message_sources SET position = 5 - position").Error; err != nil {
		t.Fatal(err)
	}
	messages, err := repo.ListMessages(ctx, userID, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || len(messages[0].Sources) != 2 {
		t.Fatalf("messages = %#v", messages)
	}
	if messages[0].Sources[0].SourceID != "src-second" || messages[0].Sources[1].SourceID != "src-first" {
		t.Fatalf("read order = %#v, want the stored positions to decide", messages[0].Sources)
	}
}

// TestDeleteSessionRollsBackCitationCleanup is the rollback counterpart: if the
// citation delete fails, the session and its messages must all still be there.
func TestDeleteSessionRollsBackCitationCleanup(t *testing.T) {
	ctx := context.Background()
	repo, db, userID, sessionID := citationFixture(t)
	if _, err := repo.AppendMessage(ctx, userID, sessionID, &domain.Message{
		Role: "assistant", Content: "answer", Sources: []domain.Source{{SourceID: "src-1", ChunkID: "chunk-1"}},
	}); err != nil {
		t.Fatal(err)
	}
	failDeleteOn(t, db, "message_sources")

	if err := repo.DeleteSession(ctx, userID, sessionID); err == nil {
		t.Fatal("the session was deleted although its citations could not be removed")
	}
	if got := countRows(t, db, "SELECT COUNT(*) FROM sessions"); got != 1 {
		t.Fatalf("session rows = %d, want the session kept", got)
	}
	if got := countRows(t, db, "SELECT COUNT(*) FROM messages"); got != 1 {
		t.Fatalf("message rows = %d, want the messages kept", got)
	}
	if got := countRows(t, db, "SELECT COUNT(*) FROM message_sources"); got != 1 {
		t.Fatalf("citation rows = %d, want them kept", got)
	}
}

// TestCitationColumnsRoundTripEveryField keeps the narrow columns honest: a long
// CJK snippet or a title with punctuation must come back unchanged, because the
// citation is what a user checks an answer against.
func TestCitationColumnsRoundTripEveryField(t *testing.T) {
	ctx := context.Background()
	repo, _, userID, sessionID := citationFixture(t)
	source := domain.Source{
		SourceID:     "src-document-18d8765baa51c990-0002-v1-c3",
		DocumentID:   "document-18d8765baa51c990-0002",
		CollectionID: "collection-18d8765baa51c990-0001",
		ChunkID:      "document-18d8765baa51c990-0002-v1-c3",
		Title:        "redis-persistence.md（第二版）",
		Section:      "Redis 持久化 / AOF",
		Snippet:      strings.Repeat("AOF 以追加写记录每条写命令", 14),
	}
	if _, err := repo.AppendMessage(ctx, userID, sessionID, &domain.Message{Role: "assistant", Content: "answer", Sources: []domain.Source{source}}); err != nil {
		t.Fatal(err)
	}
	messages, err := repo.ListMessages(ctx, userID, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || len(messages[0].Sources) != 1 {
		t.Fatalf("messages = %#v, want one answer with one citation", messages)
	}
	if !reflect.DeepEqual(messages[0].Sources[0], source) {
		t.Fatalf("citation = %#v, want every field preserved (snippet is %d runes)", messages[0].Sources[0], len([]rune(source.Snippet)))
	}
}
