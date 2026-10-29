package knowledgetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/knowledge"
	"github.com/xiaopang456/GopherLearn/internal/repository"
)

// LifecycleSuite is the shared contract for the indexing and cleanup state machine, so
// both backends are held to the same transitions instead of each growing its own. It is
// separate from Suite because Store and Lifecycle are separate surfaces: almost every
// reader needs the first and none of them needs the second.
//
// The suite asserts states and observable outcomes, not implementation: what a document
// reads as, what retrieval serves, and what a queue pass would find.
func LifecycleSuite(t *testing.T, newStore Factory) {
	t.Helper()
	ctx := context.Background()

	t.Run("a staged document answers nothing until it is published", func(t *testing.T) {
		store, lifecycle, collection := lifecycleFixture(t, newStore)
		staged, err := lifecycle.StageDocument(ctx, "user-1", measured("staged.md", domain.DocumentFormatMarkdown, collection.ID, "第一版内容"), []domain.DocumentChunk{{Text: "第一版内容"}})
		if err != nil {
			t.Fatal(err)
		}
		if staged.IndexStatus != domain.DocumentStatusPending || staged.ActiveVersion != 0 {
			t.Fatalf("staged document = %#v, want pending with no active version", staged)
		}
		if chunks, err := store.ChunksFor(ctx, "user-1", []string{collection.ID}); err != nil || len(chunks) != 0 {
			t.Fatalf("a pending version is searchable: %#v (%v)", chunks, err)
		}
		pending, err := lifecycle.ListPending(ctx, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(pending) != 1 || pending[0].Document.ID != staged.ID {
			t.Fatalf("queue = %#v, want the staged version", pending)
		}
		// The queue item has to carry its owner: every transition a driver can make is
		// scoped by the user, so an item without one is work nobody can claim.
		if pending[0].UserID != "user-1" {
			t.Fatalf("queue owner = %q, want the document's user", pending[0].UserID)
		}
	})

	t.Run("a claim is one shot and completion publishes", func(t *testing.T) {
		_, lifecycle, collection := lifecycleFixture(t, newStore)
		staged := mustStage(t, lifecycle, collection.ID, "claim.md", "内容一")
		claimed, err := lifecycle.ClaimForIndexing(ctx, "user-1", staged.ID, staged.Version)
		if err != nil || !claimed {
			t.Fatalf("first claim = (%v, %v), want it", claimed, err)
		}
		if again, err := lifecycle.ClaimForIndexing(ctx, "user-1", staged.ID, staged.Version); err != nil || again {
			t.Fatalf("second claim = (%v, %v), want exactly one claim per version", again, err)
		}
		if _, err := lifecycle.ClaimForIndexing(ctx, "user-1", staged.ID, staged.Version+1); err != nil {
			t.Fatal(err)
		}
		if err := lifecycle.CompleteIndexing(ctx, "user-1", staged.ID, staged.Version+1,
			knowledge.IndexOutcome{Status: domain.DocumentStatusReady, ChunkCount: 1}); !errors.Is(err, knowledge.ErrIllegalTransition) {
			t.Errorf("completion for another version = %v, want the transition refused", err)
		}
		if err := lifecycle.CompleteIndexing(ctx, "user-1", staged.ID, staged.Version,
			knowledge.IndexOutcome{Status: domain.DocumentStatusReady, ChunkCount: 1}); err != nil {
			t.Fatalf("complete: %v", err)
		}
		published := mustGet(t, lifecycle, staged.ID)
		if published.ActiveVersion != published.Version || published.IndexStatus != domain.DocumentStatusReady {
			t.Fatalf("published document = %#v", published)
		}
	})

	t.Run("a failed rewrite leaves the previous version answering", func(t *testing.T) {
		store, lifecycle, collection := lifecycleFixture(t, newStore)
		first := mustStagePublish(t, lifecycle, store, collection.ID, "notes.md", "第一版说明")
		second, err := lifecycle.StageReplacement(ctx, "user-1", first.ID, first.Version, measuredReplacement("第二版说明", 15))
		if err != nil {
			t.Fatal(err)
		}
		if second.Version != first.Version+1 || second.ActiveVersion != first.ActiveVersion {
			t.Fatalf("staged rewrite = %#v, want the next version and the old active one", second)
		}
		if _, err := lifecycle.ClaimForIndexing(ctx, "user-1", first.ID, second.Version); err != nil {
			t.Fatal(err)
		}
		if err := lifecycle.CompleteIndexing(ctx, "user-1", first.ID, second.Version,
			knowledge.IndexOutcome{Status: domain.DocumentStatusFailed, ErrorMessage: "indexing failed"}); err != nil {
			t.Fatal(err)
		}
		failed := mustGet(t, lifecycle, first.ID)
		if failed.IndexStatus != domain.DocumentStatusFailed || failed.ActiveVersion != first.ActiveVersion {
			t.Fatalf("after a failed rewrite = %#v, want the old active version kept", failed)
		}
		chunks, err := store.ChunksFor(ctx, "user-1", []string{collection.ID})
		if err != nil {
			t.Fatal(err)
		}
		if len(chunks) != 1 || chunks[0].Text != "第一版说明" {
			t.Fatalf("retrieval after a failed rewrite = %#v, want the previous version's text", chunks)
		}
		// The same bytes are still owed work: unchanged is a decision about a healthy
		// document, not about a hash match alone.
		if _, err := lifecycle.StageReplacement(ctx, "user-1", first.ID, mustGet(t, lifecycle, first.ID).Version,
			measuredReplacement("第二版说明", 15)); !errors.Is(err, knowledge.ErrDocumentBusy) {
			t.Errorf("staging on a failed version = %v, want the document reported busy", err)
		}
		if err := lifecycle.RetryFailed(ctx, "user-1", first.ID); err != nil {
			t.Fatalf("retry: %v", err)
		}
		if _, err := lifecycle.ClaimForIndexing(ctx, "user-1", first.ID, failed.Version); err != nil {
			t.Fatal(err)
		}
		if err := lifecycle.CompleteIndexing(ctx, "user-1", first.ID, failed.Version,
			knowledge.IndexOutcome{Status: domain.DocumentStatusReady, ChunkCount: 1}); err != nil {
			t.Fatal(err)
		}
		published := mustGet(t, lifecycle, first.ID)
		if published.ActiveVersion != published.Version {
			t.Fatalf("after the retry = %#v, want the second version live", published)
		}
		if published.IndexStatus != domain.DocumentStatusCleanupPending || published.CleanupVersion != first.ActiveVersion {
			t.Fatalf("superseded version was not queued for cleanup: %#v", published)
		}
		// Both versions' rows are still stored while that work is outstanding, and
		// retrieval still serves only the active one.
		if staged, err := lifecycle.StagedChunks(ctx, "user-1", first.ID, first.ActiveVersion); err != nil || len(staged) == 0 {
			t.Fatalf("the superseded version's rows disappeared early: %#v (%v)", staged, err)
		}
		now := time.Now().UTC()
		claimed, err := lifecycle.ClaimCleanup(ctx, "user-1", first.ID, now, now.Add(time.Minute))
		if err != nil || !claimed {
			t.Fatalf("cleanup claim = (%v, %v)", claimed, err)
		}
		if err := lifecycle.CompleteCleanup(ctx, "user-1", first.ID); err != nil {
			t.Fatal(err)
		}
		cleaned := mustGet(t, lifecycle, first.ID)
		if cleaned.IndexStatus != domain.DocumentStatusReady || cleaned.CleanupVersion != 0 {
			t.Fatalf("after cleanup = %#v", cleaned)
		}
		if stale, err := lifecycle.StagedChunks(ctx, "user-1", first.ID, cleaned.CleanupVersion); err != nil || len(stale) != 0 {
			t.Errorf("superseded rows survived cleanup: %#v (%v)", stale, err)
		}
	})

	// The version a request was built on is part of the request. v2 compares it inside the
	// locked transaction (dao StageUpdate) before judging status, and that order is what
	// stops a rewrite from being renumbered onto a version another request already
	// committed - the case where the file on disk is named for a version that is no
	// longer next. A zero expectation is refused too: forgetting to pass one is not the
	// same as having none.
	t.Run("a replacement carries the version it was built on", func(t *testing.T) {
		store, lifecycle, collection := lifecycleFixture(t, newStore)
		first := mustStagePublish(t, lifecycle, store, collection.ID, "race.md", "第一版内容")
		stale := mustGet(t, lifecycle, first.ID).Version

		// Somebody else's rewrite lands and publishes, which is what makes the read
		// above stale rather than merely old.
		if _, err := lifecycle.StageReplacement(ctx, "user-1", first.ID, stale,
			measuredReplacement("第二版内容", 16)); err != nil {
			t.Fatal(err)
		}
		// Precedence is part of the contract: v2 compares the version before the status
		// (dao StageUpdate), so a request built on a superseded read is told "the row
		// moved" even while that row happens to be busy. A status-first implementation
		// answers busy, and the caller would then wait for a version that never comes
		// back instead of re-reading.
		if _, err := lifecycle.StageReplacement(ctx, "user-1", first.ID, stale,
			measuredReplacement("同一旧版本的第二次请求", 17)); !errors.Is(err, knowledge.ErrDocumentChanged) {
			t.Fatalf("stale request against a %s row = %v, want the version refusal first",
				mustGet(t, lifecycle, first.ID).IndexStatus, err)
		}
		if claimed, err := lifecycle.ClaimForIndexing(ctx, "user-1", first.ID, stale+1); err != nil || !claimed {
			t.Fatalf("claim v%d = (%v, %v)", stale+1, claimed, err)
		}
		if err := lifecycle.CompleteIndexing(ctx, "user-1", first.ID, stale+1,
			knowledge.IndexOutcome{Status: domain.DocumentStatusReady, ChunkCount: 1}); err != nil {
			t.Fatal(err)
		}
		// Publishing v2 leaves v1 as outstanding cleanup, and a document with cleanup
		// owed takes no new version - so the queue's work has to be finished here, the
		// way the cleanup pool finishes it in a running deployment.
		if pending := mustGet(t, lifecycle, first.ID); pending.IndexStatus == domain.DocumentStatusCleanupPending {
			if claimed, err := lifecycle.ClaimCleanup(ctx, "user-1", first.ID, time.Now().UTC(),
				time.Now().UTC().Add(time.Minute)); err != nil || !claimed {
				t.Fatalf("claim cleanup = (%v, %v)", claimed, err)
			}
			if err := lifecycle.CompleteCleanup(ctx, "user-1", first.ID); err != nil {
				t.Fatal(err)
			}
		}
		current := mustGet(t, lifecycle, first.ID)
		if current.Version != 2 || current.ActiveVersion != 2 {
			t.Fatalf("row = %#v, want version 2 published before the stale request", current)
		}
		before := *current

		if _, err := lifecycle.StageReplacement(ctx, "user-1", first.ID, stale,
			measuredReplacement("基于旧版本的内容", 20)); !errors.Is(err, knowledge.ErrDocumentChanged) {
			t.Fatalf("stale replacement = %v, want the version it was built on to be refused", err)
		}
		// Precedence was asserted above, against a row that was busy; the same request
		// against a healthy row is refused for the same reason.
		after := mustGet(t, lifecycle, first.ID)
		if after.Version != before.Version || after.ContentHash != before.ContentHash ||
			after.FileSize != before.FileSize || after.IndexStatus != before.IndexStatus ||
			after.ActiveVersion != before.ActiveVersion || after.Title != before.Title {
			t.Fatalf("the refused replacement changed the row: before %#v after %#v", before, after)
		}
		if _, err := lifecycle.StageReplacement(ctx, "user-1", first.ID, 0,
			measuredReplacement("没带版本的请求", 18)); !errors.Is(err, knowledge.ErrDocumentChanged) {
			t.Fatalf("an unset expectation = %v, want it refused rather than treated as any version", err)
		}
		// The version that is actually current still works, so the guard is not a refusal
		// of everything - and the composition inside Store, which has no earlier
		// observation to be stale about, keeps advancing too.
		if _, err := lifecycle.StageReplacement(ctx, "user-1", first.ID, before.Version,
			measuredReplacement("基于新版本的第三稿", 22)); err != nil {
			t.Fatalf("a replacement built on the current version: %v", err)
		}
		if got := mustGet(t, lifecycle, first.ID); got.Version != 3 {
			t.Fatalf("row = %#v, want version 3 after the expected-version match", got)
		}
	})

	t.Run("a cleanup failure stays recorded and retryable", func(t *testing.T) {
		_, lifecycle, collection := lifecycleFixture(t, newStore)
		// A document that never published: staged, claimed, then reported failed. Only
		// a failed document can be deleted out from under a retry, which is the state
		// the cleanup schedule below has to survive.
		failed := mustStage(t, lifecycle, collection.ID, "retry.md", "第一版")
		if _, err := lifecycle.ClaimForIndexing(ctx, "user-1", failed.ID, failed.Version); err != nil {
			t.Fatal(err)
		}
		if err := lifecycle.CompleteIndexing(ctx, "user-1", failed.ID, failed.Version, knowledge.IndexOutcome{
			Status: domain.DocumentStatusFailed, ErrorMessage: "indexing failed",
		}); err != nil {
			t.Fatal(err)
		}
		retryAt := time.Now().UTC().Add(time.Minute)
		if _, err := lifecycle.MarkDeleting(ctx, "user-1", failed.ID); err != nil {
			t.Fatal(err)
		}
		if err := lifecycle.FailCleanup(ctx, "user-1", failed.ID, "cleanup: vector removal failed", retryAt); err != nil {
			t.Fatal(err)
		}
		recorded := mustGet(t, lifecycle, failed.ID)
		if recorded.CleanupAttempts != 1 || recorded.ErrorMessage == "" {
			t.Fatalf("cleanup failure was not recorded: %#v", recorded)
		}
		// A lease that has not run out keeps the work off the queue - that is what a
		// retry schedule is for.
		due, err := lifecycle.ListDueCleanup(ctx, time.Now().UTC(), 10)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range due {
			if item.Document.ID == failed.ID {
				t.Fatal("a cleanup inside its lease came back as due")
			}
		}
		if due, err := lifecycle.ListDueCleanup(ctx, retryAt.Add(time.Second), 10); err != nil {
			t.Fatal(err)
		} else {
			found := false
			for _, item := range due {
				found = found || item.Document.ID == failed.ID
			}
			if !found {
				t.Fatal("the failed cleanup never became due again")
			}
		}
	})

	t.Run("deleting releases the name, the hash and the queue slot", func(t *testing.T) {
		store, lifecycle, collection := lifecycleFixture(t, newStore)
		first := mustStagePublish(t, lifecycle, store, collection.ID, "delete.md", "要被删除的内容")
		if _, err := lifecycle.MarkDeleting(ctx, "user-1", first.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := store.GetDocument(ctx, "user-1", first.ID); !errors.Is(err, repository.ErrNotFound) {
			t.Errorf("a document being deleted still reads as present: %v", err)
		}
		if listed, err := store.ListDocuments(ctx, "user-1", collection.ID); err != nil || len(listed) != 0 {
			t.Errorf("listing after delete = %#v (%v)", listed, err)
		}
		if chunks, err := store.ChunksFor(ctx, "user-1", []string{collection.ID}); err != nil || len(chunks) != 0 {
			t.Errorf("chunks after delete = %#v (%v)", chunks, err)
		}
		// Uniqueness is released on the mark, not on the removal: the user may file the
		// same notes again while the first copy is still being cleared out.
		again, err := lifecycle.StageDocument(ctx, "user-1", measured("delete.md", domain.DocumentFormatMarkdown, collection.ID, "要被删除的内容"), []domain.DocumentChunk{{Text: "要被删除的内容"}})
		if err != nil {
			t.Fatalf("re-upload after delete = %v, want the name and hash released", err)
		}
		if _, err := lifecycle.ClaimForIndexing(ctx, "user-1", again.ID, again.Version); err != nil {
			t.Fatal(err)
		}
		if err := lifecycle.CompleteIndexing(ctx, "user-1", again.ID, again.Version, knowledge.IndexOutcome{Status: domain.DocumentStatusReady, ChunkCount: 1}); err != nil {
			t.Fatal(err)
		}
		// The first copy's removal is queued work, and completing it must not touch the
		// copy that replaced it.
		due, err := lifecycle.ListDueCleanup(ctx, time.Now().UTC().Add(time.Hour), 10)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range due {
			if item.Document.ID == first.ID {
				if err := lifecycle.CompleteCleanup(ctx, item.UserID, item.Document.ID); err != nil {
					t.Fatalf("finishing the queued delete: %v", err)
				}
			}
		}
		live := mustGet(t, lifecycle, again.ID)
		if !live.Retrievable() {
			t.Fatalf("finishing an old delete took down the new copy: %#v", live)
		}
	})

	t.Run("startup recovery returns claimed work and is idempotent", func(t *testing.T) {
		_, lifecycle, collection := lifecycleFixture(t, newStore)
		staged := mustStage(t, lifecycle, collection.ID, "interrupted.md", "内容")
		if _, err := lifecycle.ListPending(ctx, 0); !errors.Is(err, repository.ErrInvalidArgument) {
			t.Errorf("ListPending(0) = %v, want an invalid argument rather than an unbounded scan", err)
		}
		if claimed, err := lifecycle.ClaimForIndexing(ctx, "user-1", staged.ID, staged.Version); err != nil || !claimed {
			t.Fatalf("claim = (%v, %v)", claimed, err)
		}
		moved, err := lifecycle.RecoverInterruptedIndexing(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if moved != 1 {
			t.Fatalf("recovery moved %d rows, want the one claimed row returned to the queue", moved)
		}
		if again, err := lifecycle.RecoverInterruptedIndexing(ctx); err != nil || again != 0 {
			t.Fatalf("second recovery moved %d rows (%v), want nothing - a pass that is not idempotent hides a stuck row", again, err)
		}
		if claimed, err := lifecycle.ClaimForIndexing(ctx, "user-1", staged.ID, staged.Version); err != nil || !claimed {
			t.Fatalf("re-claim after recovery = (%v, %v), want the recovered row workable again", claimed, err)
		}
	})
}

func lifecycleFixture(t *testing.T, newStore Factory) (knowledge.Store, knowledge.Lifecycle, *domain.Collection) {
	t.Helper()
	store := newStore(t, knowledge.DefaultLimits())
	lifecycle, ok := store.(knowledge.Lifecycle)
	if !ok {
		t.Fatalf("%T does not implement knowledge.Lifecycle: the state machine would be untested on this backend", store)
	}
	collection := mustCollection(t, store, "user-1")
	return store, lifecycle, collection
}

func mustStage(t *testing.T, lifecycle knowledge.Lifecycle, collectionID, title, content string) *domain.Document {
	t.Helper()
	staged, err := lifecycle.StageDocument(context.Background(), "user-1", measured(title, domain.DocumentFormatMarkdown, collectionID, content),
		[]domain.DocumentChunk{{Text: content}})
	if err != nil {
		t.Fatalf("stage %q: %v", title, err)
	}
	return staged
}

func mustStagePublish(t *testing.T, lifecycle knowledge.Lifecycle, store knowledge.Store, collectionID, title, content string) *domain.Document {
	t.Helper()
	ctx := context.Background()
	staged := mustStage(t, lifecycle, collectionID, title, content)
	if _, err := lifecycle.ClaimForIndexing(ctx, "user-1", staged.ID, staged.Version); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.CompleteIndexing(ctx, "user-1", staged.ID, staged.Version,
		knowledge.IndexOutcome{Status: domain.DocumentStatusReady, ChunkCount: 1}); err != nil {
		t.Fatal(err)
	}
	if store != nil {
		// Confirm the version is actually serving before the next step builds on it.
		if _, err := store.ChunksFor(ctx, "user-1", []string{collectionID}); err != nil {
			t.Fatal(err)
		}
	}
	return mustGet(t, lifecycle, staged.ID)
}

func mustGet(t *testing.T, lifecycle knowledge.Lifecycle, documentID string) *domain.Document {
	t.Helper()
	// Read through the queue side too: a driver has to see the state it is acting on,
	// and ListPending is the only queue view Lifecycle offers by version.
	store, ok := lifecycle.(knowledge.Store)
	if !ok {
		t.Fatal("the lifecycle under test is not also a Store, so this suite cannot read the result back")
	}
	document, err := store.GetDocument(context.Background(), "user-1", documentID)
	if err != nil {
		// A document whose deletion is recorded is invisible by design; the queue is
		// where it still exists.
		due, queueErr := lifecycle.ListDueCleanup(context.Background(), time.Now().UTC().Add(time.Hour), 50)
		if queueErr != nil {
			t.Fatal(queueErr)
		}
		for _, item := range due {
			if item.Document.ID == documentID {
				found := item.Document
				return &found
			}
		}
		t.Fatalf("read back %q: %v", documentID, err)
	}
	return document
}
