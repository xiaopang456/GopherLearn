package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/knowledge"
)

// knowledgeFixture is a memory store with one collection and one published document,
// returned at every altitude the service methods work at.
func knowledgeFixture(t *testing.T) (*knowledge.MemoryStore, *KnowledgeService, string) {
	t.Helper()
	ctx := context.Background()
	store := knowledge.NewMemoryStore()
	collection, err := store.CreateCollection(ctx, &domain.Collection{UserID: "user-1", Title: "手册"})
	if err != nil {
		t.Fatal(err)
	}
	return store, NewKnowledgeService(store), collection.ID
}

func (s *KnowledgeService) addDocument(t *testing.T, ctx context.Context, userID, collectionID, title, content string) *domain.Document {
	t.Helper()
	created, err := s.AddDocument(ctx, userID, collectionID, DocumentInput{Title: title, Format: "markdown", Content: content})
	if err != nil {
		t.Fatalf("AddDocument(%s): %v", title, err)
	}
	return created
}

// TestRenameTravelsWithTheContent is the passthrough the previous round documented as
// missing: the store accepts a title on a rewrite, but the service dropped it, so a PUT
// could never rename anything.
func TestRenameTravelsWithTheContent(t *testing.T) {
	ctx := context.Background()
	store, service, collectionID := knowledgeFixture(t)
	created := service.addDocument(t, ctx, "user-1", collectionID, "旧名.md", "# 标题\n\n内容一")

	replaced, err := service.ReplaceDocumentContent(ctx, "user-1", created.ID, DocumentInput{
		Title: "新名.md", Format: "markdown", Content: "# 标题\n\n内容二",
	})
	if err != nil {
		t.Fatalf("ReplaceDocumentContent: %v", err)
	}
	if replaced.Title != "新名.md" {
		t.Fatalf("title = %q, want the rename to have been applied", replaced.Title)
	}
	if replaced.Version != 2 || replaced.ActiveVersion != 2 {
		t.Fatalf("document = %#v, want version 2 published", replaced)
	}
	stored, err := store.GetDocument(ctx, "user-1", created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Title != "新名.md" {
		t.Fatalf("stored title = %q", stored.Title)
	}

	// A rename that would collide with another live document of the same user is a
	// conflict, and the whole rewrite is refused: v2's StageUpdate decides the name
	// inside the same transaction that writes the new version, so a refused name cannot
	// leave a staged version, a new title or replaced text behind.
	service.addDocument(t, ctx, "user-1", collectionID, "占名.md", "# 别的\n\n内容三")
	_, err = service.ReplaceDocumentContent(ctx, "user-1", created.ID, DocumentInput{
		Title: "占名.md", Format: "markdown", Content: "# 标题\n\n内容四",
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("rename onto an existing name = %v, want a conflict", err)
	}
	afterConflict, err := store.GetDocument(ctx, "user-1", created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if afterConflict.Title != "新名.md" || afterConflict.Version != 2 || afterConflict.ActiveVersion != 2 {
		t.Fatalf("the refused rewrite left a trace: %#v", afterConflict)
	}
	if answering := chunksOf(mustRetrieve(t, store, collectionID), created.ID); len(answering) != 1 ||
		answering[0].Text != "# 标题\n内容二" {
		// The chunk text is the normalized form of the version that was already
		// published - not the text of the rewrite that was refused.
		t.Fatalf("the version still answering after the conflict = %#v", answering)
	}
}

// TestRenameWithUnchangedContentIsRefusedTheWayTheSourceDoesIt is the surprising half of
// v2's rule: UpdateDocument compares the content hash and nothing else
// (service/knowledge/document.go:168-171, dao StageUpdate line 62), so a PUT that only
// renames an identical document is reported as unchanged and the new name is dropped.
// The behaviour is migrated as the source has it rather than quietly improved, but it is
// pinned here so a future change is a decision and not an accident - and A must not build
// a rename UI on top of this endpoint without knowing.
func TestRenameWithUnchangedContentIsRefusedTheWayTheSourceDoesIt(t *testing.T) {
	ctx := context.Background()
	store, service, collectionID := knowledgeFixture(t)
	created := service.addDocument(t, ctx, "user-1", collectionID, "原名.md", "# 标题\n\n同一段内容")

	replaced, err := service.ReplaceDocumentContent(ctx, "user-1", created.ID, DocumentInput{
		Title: "想改的名.md", Format: "markdown", Content: "# 标题\n\n同一段内容",
	})
	if err != nil {
		t.Fatalf("an identical-content rewrite must succeed as unchanged: %v", err)
	}
	if replaced.Title != "原名.md" {
		t.Fatalf("title = %q, want the name to stay because the content did not change", replaced.Title)
	}
	if replaced.Version != 1 || replaced.ActiveVersion != 1 {
		t.Fatalf("document = %#v, want nothing advanced", replaced)
	}
	if state, err := service.DocumentWorkState(ctx, "user-1", created.ID); err != nil ||
		state.IndexStatus != domain.DocumentStatusReady || !state.Retrievable {
		t.Fatalf("work state = %+v (%v)", state, err)
	}
	if answering := chunksOf(mustRetrieve(t, store, collectionID), created.ID); len(answering) != 1 ||
		answering[0].Text != "# 标题\n同一段内容" || answering[0].Version != 1 {
		t.Fatalf("the document answers from a version the rewrite replaced: %#v", answering)
	}
}

// TestRewriteWithoutANameKeepsTheTitle is the half of the passthrough that must not
// happen: the caller who sends only new content must not have the document renamed.
// Before this, an empty name fell through to path.Base(""), which is ".", so the rewrite
// renamed the document to a dot - and the create path would have accepted the same dot
// as a title. v2 requires a name on update; the target's contract does not, so "no name
// sent" means "no name changed" here, and a name that resolves to nothing is no name.
func TestRewriteWithoutANameKeepsTheTitle(t *testing.T) {
	ctx := context.Background()
	store, service, collectionID := knowledgeFixture(t)
	created := service.addDocument(t, ctx, "user-1", collectionID, "redis-persistence.md", "# Redis\n\nAOF everysec")

	omitted, err := service.ReplaceDocumentContent(ctx, "user-1", created.ID, DocumentInput{
		Format: "markdown", Content: "# Redis\n\nAOF always",
	})
	if err != nil {
		t.Fatalf("a rewrite with no name: %v", err)
	}
	if omitted.Title != "redis-persistence.md" {
		t.Fatalf("title = %q, want the name to stay", omitted.Title)
	}
	if omitted.Version != 2 {
		t.Fatalf("version = %d, want the content rewrite to have advanced it", omitted.Version)
	}

	blankName, err := service.ReplaceDocumentContent(ctx, "user-1", created.ID, DocumentInput{
		Title: "   ", Filename: "  ", Format: "markdown", Content: "# Redis\n\nAOF every 100s",
	})
	if err != nil {
		t.Fatalf("a rewrite with a blank name: %v", err)
	}
	if blankName.Title != "redis-persistence.md" {
		t.Fatalf("title = %q, want a blank name treated as no name", blankName.Title)
	}

	// Creation still insists on something to call the document by.
	_, err = service.AddDocument(ctx, "user-1", collectionID, DocumentInput{Format: "markdown", Content: "# 无名\n\n内容"})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("create with no name = %v, want it refused rather than titled \".\"", err)
	}
	if state, err := service.DocumentWorkState(ctx, "user-1", created.ID); err != nil ||
		state.IndexStatus != domain.DocumentStatusReady || !state.Retrievable {
		t.Fatalf("work state = %+v (%v)", state, err)
	}
	// The store, not just the returned copy, kept the name - and it is the last rewrite
	// that answers, so the version did advance while the name stayed put.
	stored, err := store.GetDocument(ctx, "user-1", created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Title != "redis-persistence.md" || stored.Version != 3 {
		t.Fatalf("stored document = %#v, want the name intact at version 3", stored)
	}
	if answering := chunksOf(mustRetrieve(t, store, collectionID), created.ID); len(answering) != 1 ||
		answering[0].Text != "# Redis\nAOF every 100s" {
		t.Fatalf("the collection answers from %#v", answering)
	}
}

func TestRetryDocumentFollowsTheSourceStates(t *testing.T) {
	ctx := context.Background()
	store, service, collectionID := knowledgeFixture(t)
	created := service.addDocument(t, ctx, "user-1", collectionID, "retry.md", "# 标题\n\n内容一")

	// A ready document has nothing to retry.
	if _, err := service.RetryDocument(ctx, "user-1", created.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("retry of a ready document = %v, want a conflict", err)
	}

	// Stage a rewrite and fail it the way an index outage does, then retry it.
	staged, err := store.StageReplacement(ctx, "user-1", created.ID, created.Version, knowledge.DocumentReplacement{
		Format: domain.DocumentFormatMarkdown, Chunks: []domain.DocumentChunk{{Text: "内容二"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := store.ClaimForIndexing(ctx, "user-1", staged.ID, staged.Version); err != nil || !claimed {
		t.Fatalf("claim = (%v, %v)", claimed, err)
	}
	if err := store.CompleteIndexing(ctx, "user-1", staged.ID, staged.Version, knowledge.IndexOutcome{
		Status: domain.DocumentStatusFailed, ErrorMessage: "indexing operation failed",
	}); err != nil {
		t.Fatal(err)
	}

	failedState, err := service.DocumentWorkState(ctx, "user-1", created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if failedState.IndexStatus != domain.DocumentStatusFailed || failedState.Version != 2 ||
		failedState.ActiveVersion != 1 || !failedState.Retrievable {
		t.Fatalf("state of a failed rewrite = %+v, want v1 still answering", failedState)
	}
	if failedState.ErrorMessage == "" {
		t.Fatal("the recorded reason did not reach the status query")
	}
	// The answering version is what retrieval still serves.
	if answering := chunksOf(mustRetrieve(t, store, collectionID), created.ID); len(answering) != 1 ||
		answering[0].Text != "# 标题\n内容一" || answering[0].Version != 1 {
		t.Fatalf("retrieval while v2 failed = %#v", answering)
	}

	retried, err := service.RetryDocument(ctx, "user-1", created.ID)
	if err != nil {
		t.Fatalf("RetryDocument: %v", err)
	}
	if retried.IndexStatus != domain.DocumentStatusPending || retried.ErrorMessage != "" {
		t.Fatalf("after retry = %+v, want the reason cleared and the work queued", retried)
	}
	// A second retry has nothing to do: only a failed version is retryable.
	if _, err := service.RetryDocument(ctx, "user-1", created.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("second retry = %v, want a conflict", err)
	}
	// Somebody else's document is indistinguishable from a missing one.
	if _, err := service.RetryDocument(ctx, "user-2", created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-user retry = %v, want not found", err)
	}
	if _, err := service.RetryDocument(ctx, "user-1", "  "); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("empty id = %v, want an invalid request", err)
	}
	if _, err := service.RetryDocument(ctx, "", created.ID); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("no user = %v, want unauthorized", err)
	}
}

func TestDocumentWorkStateNamesTheQueuedCleanup(t *testing.T) {
	ctx := context.Background()
	store, service, collectionID := knowledgeFixture(t)
	created := service.addDocument(t, ctx, "user-1", collectionID, "state.md", "# 标题\n\n内容一")

	// A published rewrite leaves the record asking for its superseded version to be
	// dropped; the status query has to show that as outstanding work, not as ready.
	rewrite, err := store.StageReplacement(ctx, "user-1", created.ID, created.Version, knowledge.DocumentReplacement{
		Format: domain.DocumentFormatMarkdown, Chunks: []domain.DocumentChunk{{Text: "内容二"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := store.ClaimForIndexing(ctx, "user-1", rewrite.ID, rewrite.Version); err != nil || !claimed {
		t.Fatalf("claim = (%v, %v)", claimed, err)
	}
	if err := store.CompleteIndexing(ctx, "user-1", rewrite.ID, rewrite.Version, knowledge.IndexOutcome{
		Status: domain.DocumentStatusReady, ChunkCount: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.FailCleanup(ctx, "user-1", rewrite.ID, knowledge.CleanupFailureMessage("vector"),
		time.Now().UTC().Add(knowledge.CleanupBackoff(2))); err != nil {
		t.Fatal(err)
	}
	state, err := service.DocumentWorkState(ctx, "user-1", rewrite.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.CleanupVersion != 1 || state.CleanupAttempts != 1 || state.CleanupAfter.IsZero() {
		t.Fatalf("cleanup bookkeeping missing from the status query: %+v", state)
	}
	if state.ErrorMessage != "cleanup: vector delete failed" {
		t.Fatalf("error = %q, want the failing stage named", state.ErrorMessage)
	}
	if !state.Retrievable || state.ActiveVersion != 2 {
		t.Fatalf("a document with outstanding cleanup is not answering: %+v", state)
	}
}

// storeOnly hides the Lifecycle methods behind a Store, which is how a deployment that
// wired knowledge without an index driver would present itself.
type storeOnly struct {
	knowledge.Store
}

func TestLifecycleCallsOnAStoreThatCannotRecordStateAreNotClientErrors(t *testing.T) {
	ctx := context.Background()
	store := knowledge.NewMemoryStore()
	collection, err := store.CreateCollection(ctx, &domain.Collection{UserID: "user-1", Title: "手册"})
	if err != nil {
		t.Fatal(err)
	}
	service := NewKnowledgeService(storeOnly{store})
	if _, err := service.RetryDocument(ctx, "user-1", "anything"); !errors.Is(err, ErrInternal) {
		t.Fatalf("retry over a store without a lifecycle = %v, want an internal failure", err)
	}
	if _, err := service.DocumentWorkState(ctx, "user-1", "anything"); !errors.Is(err, ErrInternal) {
		t.Fatalf("status over a store without a lifecycle = %v, want an internal failure", err)
	}
	// Reads that do not need the lifecycle still work over the same wrapper.
	if _, err := service.ListDocuments(ctx, "user-1", collection.ID); err != nil {
		t.Fatalf("ListDocuments: %v", err)
	}
}

func mustRetrieve(t *testing.T, store *knowledge.MemoryStore, collectionID string) []domain.DocumentChunk {
	t.Helper()
	chunks, err := store.ChunksFor(context.Background(), "user-1", []string{collectionID})
	if err != nil {
		t.Fatal(err)
	}
	return chunks
}

// chunksOf picks one document's rows out of a collection's retrieval set. A collection
// holds several documents in these tests on purpose - a rewrite must not disturb the
// others, and an assertion that ignores them cannot tell a leak from a pass.
func chunksOf(chunks []domain.DocumentChunk, documentID string) []domain.DocumentChunk {
	found := make([]domain.DocumentChunk, 0, len(chunks))
	for _, chunk := range chunks {
		if chunk.DocumentID == documentID {
			found = append(found, chunk)
		}
	}
	return found
}
