package redisindex

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/knowledge"
)

// waitingEmbedder blocks until the context it was handed ends and returns that
// context's error, which is what a real HTTP embedder does when its caller goes away
// or its deadline runs out. Nothing here races a wall clock: the fake cannot return
// before the cancellation or the timeout it is waiting for, so the test drives the
// distinction instead of guessing at timings.
//
// entered is closed once the call is in flight, which is the point at which the test
// cancels - the alternative is a sleep, and a sleep passes for the wrong reason.
type waitingEmbedder struct {
	dimensions int
	entered    chan struct{}
	once       sync.Once
}

func (w *waitingEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	w.once.Do(func() { close(w.entered) })
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(5 * time.Second):
		return nil, errors.New("the indexing context never ended")
	}
}

func (w *waitingEmbedder) Model() string   { return "waiting" }
func (w *waitingEmbedder) Dimensions() int { return w.dimensions }

// mustListOne reads back the single document in a collection, so each test names the
// document it is asserting on instead of trusting an id it assembled itself.
func mustListOne(t *testing.T, store knowledge.Store, collectionID string) string {
	t.Helper()
	documents, err := store.ListDocuments(context.Background(), "user-1", collectionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(documents) != 1 {
		t.Fatalf("listing = %#v, want exactly one document", documents)
	}
	return documents[0].ID
}

// TestCallerCancellationRequeuesInsteadOfFailing covers the distinction v2 draws in
// its worker (worker.go:203-210): a caller that walks away is not a failed document.
// It drives the real path - CreateDocument through the decorator - and asserts the row
// comes back to pending, claimable, and never enters the failed state.
//
// The requeue write has to survive a dead caller context. Had the drive completed the
// transition with the context that just died, the row would stay in indexing with no
// claimant, which is what this test would then catch.
func TestCallerCancellationRequeuesInsteadOfFailing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store, collectionID := newTestStores(t)
	embedder := &waitingEmbedder{dimensions: 4, entered: make(chan struct{})}
	synchronizer, err := NewSynchronizer(store, &fakeWriter{dimensions: 4}, embedder, "chunks-v1")
	if err != nil {
		t.Fatal(err)
	}

	failed := make(chan error, 1)
	go func() {
		_, err := synchronizer.CreateDocument(ctx, "user-1", &domain.Document{
			CollectionID: collectionID, Title: "walk-away.md", Format: domain.DocumentFormatMarkdown,
		}, documentChunks(2))
		failed <- err
	}()

	<-embedder.entered
	cancel()
	err = <-failed
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("drive error = %v, want the caller's cancellation to surface", err)
	}

	stored, err := synchronizer.GetDocument(context.Background(), "user-1", mustListOne(t, synchronizer, collectionID))
	if err != nil {
		t.Fatal(err)
	}
	if stored.IndexStatus != domain.DocumentStatusPending {
		t.Fatalf("index status after a cancellation = %q (%q), want it back in the queue", stored.IndexStatus, stored.ErrorMessage)
	}
	if stored.ErrorMessage != "" {
		t.Errorf("a cancelled pass recorded %q, want no failure reason", stored.ErrorMessage)
	}
	if !stored.Retrievable() && stored.Version != 1 {
		t.Fatalf("document = %#v", stored)
	}
	if claimed, err := synchronizer.ClaimForIndexing(context.Background(), "user-1", stored.ID, stored.Version); err != nil || !claimed {
		t.Fatalf("re-claim after a cancellation = (%v, %v), want the version workable again", claimed, err)
	}
}

// TestIndexTimeoutIsAFailureTheCallerStillSees is the other side of the same split:
// the caller is waiting, the attempt ran past its own budget, and that is a failure
// with a bounded reason - not a silent return to the queue.
func TestIndexTimeoutIsAFailureTheCallerStillSees(t *testing.T) {
	store, collectionID := newTestStores(t)
	embedder := &waitingEmbedder{dimensions: 4, entered: make(chan struct{})}
	synchronizer, err := NewSynchronizer(store, &fakeWriter{dimensions: 4}, embedder, "chunks-v1")
	if err != nil {
		t.Fatal(err)
	}
	synchronizer = synchronizer.WithIndexTimeout(20 * time.Millisecond)
	ctx := context.Background()

	if _, err := synchronizer.CreateDocument(ctx, "user-1", &domain.Document{
		CollectionID: collectionID, Title: "slow.md", Format: domain.DocumentFormatMarkdown,
	}, documentChunks(1)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("drive error = %v, want the budget to surface as a deadline", err)
	}
	documentID := mustListOne(t, synchronizer, collectionID)
	stored, err := synchronizer.GetDocument(ctx, "user-1", documentID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.IndexStatus != domain.DocumentStatusFailed {
		t.Fatalf("index status = %q, want a recorded failure", stored.IndexStatus)
	}
	if stored.ErrorMessage != "indexing timed out" {
		t.Errorf("error message = %q, want the bounded reason", stored.ErrorMessage)
	}
	// The reason is a fixed phrase: a deadline's own text can name an internal
	// deadline or a URL, and none of that belongs in a row a client reads back.
	if claimed, err := synchronizer.ClaimForIndexing(ctx, "user-1", documentID, stored.Version); err != nil || claimed {
		t.Errorf("a timed-out version claims as %v (%v), want only a retry to release it", claimed, err)
	}
	if err := synchronizer.RetryFailed(ctx, "user-1", documentID); err != nil {
		t.Errorf("retry: %v", err)
	}
}

// TestReindexRebuildsTheServingVersionNotTheStagedOne is the third closure: after an
// update failed, the staged version must not be published by a rebuild, and the version
// that is still answering must not have its vectors dropped as if it were superseded.
func TestReindexRebuildsTheServingVersionNotTheStagedOne(t *testing.T) {
	ctx := context.Background()
	store, collectionID := newTestStores(t)
	writer := &fakeWriter{dimensions: 4}
	synchronizer, err := NewSynchronizer(store, writer, &stubEmbedder{dimensions: 4}, "chunks-v1")
	if err != nil {
		t.Fatal(err)
	}
	created, err := synchronizer.CreateDocument(ctx, "user-1", &domain.Document{
		CollectionID: collectionID, Title: "rebuild.md", Format: domain.DocumentFormatMarkdown,
	}, documentChunks(2))
	if err != nil {
		t.Fatal(err)
	}
	// Fail the update the way an index outage does: the store has the new version, the
	// index never got it.
	writer.upsertErr = errors.New("index down")
	if _, err := synchronizer.ReplaceDocumentContent(ctx, "user-1", created.ID, knowledge.DocumentReplacement{
		Format: domain.DocumentFormatMarkdown, Chunks: documentChunks(3),
	}); err == nil {
		t.Fatal("the failing update reported success")
	}
	writer.upsertErr = nil
	failed, err := synchronizer.GetDocument(ctx, "user-1", created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if failed.Version != 2 || failed.ActiveVersion != 1 || failed.IndexStatus != domain.DocumentStatusFailed {
		t.Fatalf("document after the failed update = %#v, want v2 staged and failed with v1 serving", failed)
	}

	writer.upserts, writer.kept = nil, nil
	rebuilt, err := synchronizer.Reindex(ctx, "user-1")
	if err != nil {
		t.Fatalf("Reindex: %v", err)
	}
	if rebuilt != 1 {
		t.Fatalf("Reindex rebuilt %d documents, want the one that is serving", rebuilt)
	}
	if len(writer.upserts) != 1 {
		t.Fatalf("upserts = %d, want exactly the serving version written once", len(writer.upserts))
	}
	for _, entry := range writer.upserts[0] {
		if entry.Chunk.Version != 1 {
			t.Fatalf("Reindex wrote version %d, want only the serving version 1 - the staged one is text nobody is answering from", entry.Chunk.Version)
		}
	}
	if len(writer.kept) != 1 || writer.kept[0] != 1 {
		t.Fatalf("kept versions = %v, want the serving version kept and every other dropped", writer.kept)
	}
	after, err := synchronizer.GetDocument(ctx, "user-1", created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.IndexStatus != domain.DocumentStatusFailed || after.ActiveVersion != 1 {
		t.Fatalf("Reindex changed the document state: %#v", after)
	}
	if err := synchronizer.RetryFailed(ctx, "user-1", created.ID); err != nil {
		t.Errorf("the failed update must still be retryable after a rebuild: %v", err)
	}
}

// TestReindexSkipsDocumentsWithNothingServing keeps the rebuild honest in the other
// direction: a document that has never published has no vectors to protect and no
// version to serve, so writing its staged rows would be exactly the publication
// Reindex must never perform.
func TestReindexSkipsDocumentsWithNothingServing(t *testing.T) {
	ctx := context.Background()
	store, collectionID := newTestStores(t)
	writer := &fakeWriter{dimensions: 4}
	synchronizer, err := NewSynchronizer(store, writer, &stubEmbedder{dimensions: 4}, "chunks-v1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := synchronizer.StageDocument(ctx, "user-1", &domain.Document{
		CollectionID: collectionID, Title: "never-indexed.md", Format: domain.DocumentFormatMarkdown,
	}, documentChunks(1)); err != nil {
		t.Fatal(err)
	}
	rebuilt, err := synchronizer.Reindex(ctx, "user-1")
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt != 0 || len(writer.upserts) != 0 {
		t.Fatalf("rebuilt %d documents with %d writes, want nothing published for a document with no serving version", rebuilt, len(writer.upserts))
	}
}

// TestUnchangedRewriteNeverReachesTheIndex pins the case the fidelity checklist had
// registered as read-code-only: the store decides "unchanged", and the decorator has to
// pass that decision through without embedding, writing or sweeping anything. The error
// identity is part of the assertion, not a detail - a caller that cannot see "unchanged"
// cannot tell it apart from a rewrite that published, and a document whose index rows
// were never written has to be re-driven by its caller rather than short-circuited.
// The unhealthy half of the same rule lives in
// TestSynchronizerRecordsAFailedIndexAndKeepsTheDocumentRetryable.
func TestUnchangedRewriteNeverReachesTheIndex(t *testing.T) {
	ctx := context.Background()
	store, collectionID := newTestStores(t)
	writer := &fakeWriter{dimensions: 4}
	synchronizer, err := NewSynchronizer(store, writer, &stubEmbedder{dimensions: 4}, "chunks-v1")
	if err != nil {
		t.Fatal(err)
	}
	hash := strings.Repeat("ab", 32)
	created, err := synchronizer.CreateDocument(ctx, "user-1", &domain.Document{
		CollectionID: collectionID, Title: "same-bytes.md", Format: domain.DocumentFormatMarkdown,
		ContentHash: hash, FileSize: 128,
	}, documentChunks(2))
	if err != nil {
		t.Fatal(err)
	}
	if len(writer.upserts) != 1 || len(writer.kept) != 0 {
		t.Fatalf("create left %d upserts and %d sweeps, want one write and nothing superseded", len(writer.upserts), len(writer.kept))
	}

	// The same bytes with the same hash, submitted as a rewrite.
	unchanged, err := synchronizer.ReplaceDocumentContent(ctx, "user-1", created.ID, knowledge.DocumentReplacement{
		Format: domain.DocumentFormatMarkdown, ContentHash: hash, FileSize: 128, Chunks: documentChunks(2),
	})
	if !errors.Is(err, knowledge.ErrDocumentUnchanged) {
		t.Fatalf("unchanged rewrite = (%#v, %v), want the decision to travel to the caller", unchanged, err)
	}
	if len(writer.upserts) != 1 || len(writer.kept) != 0 {
		t.Fatalf("an unchanged rewrite touched the index: %d upserts, %d sweeps", len(writer.upserts), len(writer.kept))
	}
	stored, err := synchronizer.GetDocument(ctx, "user-1", created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Version != 1 || stored.ActiveVersion != 1 || stored.IndexStatus != domain.DocumentStatusReady {
		t.Fatalf("document after an unchanged rewrite = %#v, want nothing advanced", stored)
	}

	// Different bytes on the same document do advance, so the assertion above is not
	// simply "rewrites never reach the index".
	if _, err := synchronizer.ReplaceDocumentContent(ctx, "user-1", created.ID, knowledge.DocumentReplacement{
		Format: domain.DocumentFormatMarkdown, ContentHash: strings.Repeat("cd", 32), FileSize: 128,
		Chunks: documentChunks(3),
	}); err != nil {
		t.Fatalf("a genuinely new rewrite: %v", err)
	}
	advanced, err := synchronizer.GetDocument(ctx, "user-1", created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if advanced.Version != 2 || advanced.ActiveVersion != 2 || len(writer.upserts) != 2 || len(writer.kept) != 1 {
		t.Fatalf("document = %#v with %d upserts and %d sweeps, want the new version published and the old one swept",
			advanced, len(writer.upserts), len(writer.kept))
	}
}
