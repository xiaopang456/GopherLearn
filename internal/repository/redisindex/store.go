package redisindex

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/knowledge"
	"github.com/xiaopang456/GopherLearn/internal/retrieval"
)

// vectorWriter is the part of an index the synchronizer touches. Naming it as an
// interface is what lets the compensation policy be tested with no Redis at all: the
// byte layout and the KNN query are asserted against a real server in the gated test,
// and neither needs to be re-proved here.
type vectorWriter interface {
	EnsureIndex(ctx context.Context) error
	Dimensions() int
	Upsert(ctx context.Context, userID string, entries []retrieval.VectorEntry) error
	DeleteDocument(ctx context.Context, userID string, documentID string) error
	DeleteVersions(ctx context.Context, userID string, documentID string, keepVersion int) error
}

// Synchronizer is a knowledge.Store that keeps a vector index in step with the
// documents it writes. It decorates rather than modifies the store: both backends keep
// passing their own conformance suite, and the retrieval side cannot reach a chunk the
// store did not publish.
//
// Its failure policy is v2's, and it is deliberately not "roll the write back". An
// index step that cannot be done leaves the document staged and marked failed, with the
// version that was answering before still answering: a user's notes do not disappear
// because an embedder was unreachable. What has to be true for that to be recoverable
// is that the failure is recorded and retryable, which is what CompleteIndexing and
// RetryFailed on the wrapped store provide.
type Synchronizer struct {
	knowledge.Store
	lifecycle    knowledge.Lifecycle
	index        vectorWriter
	embedder     retrieval.Embedder
	identity     string
	indexTimeout time.Duration
}

var (
	_ knowledge.Store     = (*Synchronizer)(nil)
	_ knowledge.Lifecycle = (*Synchronizer)(nil)
	// The decorator is also what a queue driver would call: it owns the embedder and
	// the index, so it is the only object that can turn a claimed version into vectors.
	_ knowledge.IndexRunner = (*Driver)(nil)
	_ knowledge.RowCleaner  = (*Driver)(nil)
)

// errNoLifecycle is reported at construction, not at first use: an index decorator on
// a store that cannot record index state would silently turn every failed embed into a
// document that exists but answers nothing.
var errNoLifecycle = errors.New("redisindex: the wrapped store does not expose the index lifecycle")

// NewSynchronizer wires a store to an index. contentVersion names how chunk text is
// produced today; changing it invalidates every stored vector on purpose, so a
// re-index becomes an explicit operation instead of a silent comparison of two
// incompatible vector spaces.
func NewSynchronizer(store knowledge.Store, index vectorWriter, embedder retrieval.Embedder, contentVersion string) (*Synchronizer, error) {
	if store == nil {
		return nil, errors.New("redisindex: a store is required")
	}
	if index == nil {
		return nil, errors.New("redisindex: an index is required")
	}
	if embedder == nil {
		return nil, errors.New("redisindex: an embedder is required")
	}
	if len(strings.TrimSpace(contentVersion)) == 0 {
		return nil, fmt.Errorf("%w: content version is required", ErrConfig)
	}
	if embedder.Dimensions() != index.Dimensions() {
		return nil, fmt.Errorf("%w: embedder produces %d dimensions, index holds %d",
			ErrConfig, embedder.Dimensions(), index.Dimensions())
	}
	lifecycle, ok := store.(knowledge.Lifecycle)
	if !ok {
		return nil, errNoLifecycle
	}
	return &Synchronizer{
		Store: store, lifecycle: lifecycle, index: index, embedder: embedder,
		identity: retrieval.VectorIdentity(embedder, contentVersion),
	}, nil
}

// WithIndexTimeout bounds one indexing attempt, separately from the caller's deadline.
// The distinction is v2's: a caller that walks away re-queues the work, while a task
// that outlived its own budget is a failure the user can see and retry. It is a setter
// rather than a constructor argument because the assembly that builds this decorator
// belongs to window A; the configuration need is published with the contract, and the
// default inherits the caller's context, which is today's behaviour.
func (s *Synchronizer) WithIndexTimeout(timeout time.Duration) *Synchronizer {
	s.indexTimeout = timeout
	return s
}

// Identity reports the embedder fingerprint this synchronizer writes and filters on.
func (s *Synchronizer) Identity() string { return s.identity }

// EnsureIndex prepares the index before the first write. It is exposed on the
// decorator because startup has the store, not the index.
func (s *Synchronizer) EnsureIndex(ctx context.Context) error { return s.index.EnsureIndex(ctx) }

// UsageByUser, GetDocument, ListDocuments, ChunksFor, titles and readability come from
// the wrapped store through embedding: this decorator changes what happens when a
// version is written, never what is readable.

// CreateDocument stages the first version, publishes it through the index, and records
// the outcome. A failed index step leaves the document pending-or-failed with the
// reason, exactly as v2's worker does; nothing is rolled back, because a document the
// user can see and retry is worth more than a clean table.
func (s *Synchronizer) CreateDocument(ctx context.Context, userID string, document *domain.Document,
	chunks []domain.DocumentChunk) (*domain.Document, error) {
	staged, err := s.lifecycle.StageDocument(ctx, userID, document, chunks)
	if err != nil {
		return nil, err
	}
	return s.publish(ctx, userID, staged)
}

// ReplaceDocumentContent stages the new version, publishes it through the index, then
// drops the superseded version's vectors - so the retired text stops being recallable at
// the moment the new text starts being recallable, never after.
//
// An unchanged rewrite is passed straight through only because the store decided it is
// unchanged, and the store only decides that for a healthy document whose active version
// is the one it recorded. On any other state the same bytes are re-published, since
// short-circuiting there would strand a document whose index rows were never written.
func (s *Synchronizer) ReplaceDocumentContent(ctx context.Context, userID, documentID string,
	replacement knowledge.DocumentReplacement) (*domain.Document, error) {
	// No expectation: this is the Store composition, which stages and publishes without a
	// prior read of its own. A caller that reads first - the source path, which names a
	// file for the version it chose - goes through the forwarded StageReplacement instead.
	staged, err := s.lifecycle.StageReplacement(ctx, userID, documentID, knowledge.StageOntoCurrentVersion, replacement)
	if err != nil {
		return staged, err
	}
	return s.publish(ctx, userID, staged)
}

// publish drives one staged version through claim, index and completion - the
// transitions v2's worker runs, executed here because this process has no queue yet.
func (s *Synchronizer) publish(ctx context.Context, userID string, staged *domain.Document) (*domain.Document, error) {
	claimed, err := s.lifecycle.ClaimForIndexing(ctx, userID, staged.ID, staged.Version)
	if err != nil {
		return nil, err
	}
	if !claimed {
		return nil, knowledge.ErrIllegalTransition
	}
	// The index attempt gets its own budget when one is configured, so the two ways an
	// attempt can end apart stay tellable: the caller walking away, versus a task that
	// outlived its budget while the caller was still waiting.
	taskCtx, cancelTask := ctx, func() {}
	if s.indexTimeout > 0 {
		taskCtx, cancelTask = context.WithTimeout(ctx, s.indexTimeout)
	}
	count, indexErr := s.indexDocument(taskCtx, userID, staged, staged.Version)
	cancelTask()
	if indexErr != nil {
		return nil, s.abandonPublish(ctx, userID, staged, indexErr)
	}
	if err := s.lifecycle.CompleteIndexing(ctx, userID, staged.ID, staged.Version, knowledge.IndexOutcome{
		Status: domain.DocumentStatusReady, ChunkCount: count,
	}); err != nil {
		return nil, err
	}
	published, err := s.Store.GetDocument(ctx, userID, staged.ID)
	if err != nil {
		return nil, err
	}
	// A success that superseded a different version leaves that version queued for
	// cleanup, so its vectors and its recorded work are finished in one place.
	if err := s.driveCleanup(ctx, userID, published); err != nil {
		return nil, err
	}
	if published.IndexStatus == domain.DocumentStatusCleanupPending {
		// Cleanup finished the record, which returns the document to ready.
		return s.Store.GetDocument(ctx, userID, staged.ID)
	}
	return published, nil
}

// abandonPublish is the shared decision (knowledge.ResolveAbandonedIndexing) applied to
// the inline path. It lives here only so the publish function reads in one voice; the
// queued path reaches the same rules through the same function, which is the point of
// putting the state semantics in one place instead of one per driver.
func (s *Synchronizer) abandonPublish(ctx context.Context, userID string, staged *domain.Document, indexErr error) error {
	return knowledge.ResolveAbandonedIndexing(ctx, s.lifecycle, userID, staged.ID, staged.Version, indexErr)
}

// driveCleanup carries out recorded cleanup for one document. A failure is reported
// and left recorded with its next attempt, never presented as finished.
func (s *Synchronizer) driveCleanup(ctx context.Context, userID string, current *domain.Document) error {
	if current.IndexStatus != domain.DocumentStatusCleanupPending {
		return nil
	}
	now := time.Now().UTC()
	claimed, err := s.lifecycle.ClaimCleanup(ctx, userID, current.ID, now, now.Add(time.Minute))
	if err != nil {
		return err
	}
	if !claimed {
		return nil
	}
	if err := s.index.DeleteVersions(ctx, userID, current.ID, current.ActiveVersion); err != nil {
		message := knowledge.CleanupFailureMessage("vector")
		if failErr := s.lifecycle.FailCleanup(ctx, userID, current.ID, message, now.Add(knowledge.CleanupBackoff(current.CleanupAttempts+1))); failErr != nil {
			return errors.Join(err, failErr)
		}
		// The new version is searchable and the old one is still in the index. Saying
		// so, with the retry recorded, is better than a half-done cleanup that looks
		// complete.
		return fmt.Errorf("redisindex: superseded vectors of %s were left behind: %w", current.ID, err)
	}
	return s.lifecycle.CompleteCleanup(ctx, userID, current.ID)
}

// DeleteDocument makes the document invisible first, then removes its vectors, then
// finishes the removal. v2's ordering is the point: the name and content hash are
// released as soon as the document stops being somebody's knowledge, and the leftover
// index rows are recorded work rather than a fire-and-forget call.
func (s *Synchronizer) DeleteDocument(ctx context.Context, userID, documentID string) error {
	if _, err := s.lifecycle.MarkDeleting(ctx, userID, documentID); err != nil {
		return err
	}
	if err := s.index.DeleteDocument(ctx, userID, documentID); err != nil {
		now := time.Now().UTC()
		message := knowledge.CleanupFailureMessage("vector")
		if failErr := s.lifecycle.FailCleanup(ctx, userID, documentID, message, now.Add(knowledge.CleanupBackoff(1))); failErr != nil {
			return errors.Join(err, failErr)
		}
		return err
	}
	return s.lifecycle.CompleteCleanup(ctx, userID, documentID)
}

// The lifecycle methods with no index work of their own are forwarded: they exist so a
// driver can work the queue, and the decorator must not be the place that loses them.

func (s *Synchronizer) StageDocument(ctx context.Context, userID string, document *domain.Document, chunks []domain.DocumentChunk) (*domain.Document, error) {
	return s.lifecycle.StageDocument(ctx, userID, document, chunks)
}

func (s *Synchronizer) StageReplacement(ctx context.Context, userID, documentID string, expectedVersion int,
	replacement knowledge.DocumentReplacement) (*domain.Document, error) {
	return s.lifecycle.StageReplacement(ctx, userID, documentID, expectedVersion, replacement)
}

func (s *Synchronizer) ClaimForIndexing(ctx context.Context, userID, documentID string, version int) (bool, error) {
	return s.lifecycle.ClaimForIndexing(ctx, userID, documentID, version)
}

// CompleteIndexing refuses to publish a version the index never received. A success
// recorded without the vectors behind it would make the document answer queries from an
// empty premise, which is worse than the failed state it came from.
func (s *Synchronizer) CompleteIndexing(ctx context.Context, userID, documentID string, version int, outcome knowledge.IndexOutcome) error {
	if outcome.Status == domain.DocumentStatusReady {
		current, err := s.Store.GetDocument(ctx, userID, documentID)
		if err != nil {
			return err
		}
		count, err := s.indexDocument(ctx, userID, current, version)
		if err != nil {
			return err
		}
		outcome.ChunkCount = count
	}
	return s.lifecycle.CompleteIndexing(ctx, userID, documentID, version, outcome)
}

func (s *Synchronizer) RequeueIndexing(ctx context.Context, userID, documentID string, version int) error {
	return s.lifecycle.RequeueIndexing(ctx, userID, documentID, version)
}

func (s *Synchronizer) RecoverInterruptedIndexing(ctx context.Context) (int, error) {
	return s.lifecycle.RecoverInterruptedIndexing(ctx)
}

func (s *Synchronizer) RetryFailed(ctx context.Context, userID, documentID string) error {
	return s.lifecycle.RetryFailed(ctx, userID, documentID)
}

func (s *Synchronizer) ListPending(ctx context.Context, limit int) ([]knowledge.QueuedDocument, error) {
	return s.lifecycle.ListPending(ctx, limit)
}

func (s *Synchronizer) ListDueCleanup(ctx context.Context, now time.Time, limit int) ([]knowledge.QueuedDocument, error) {
	return s.lifecycle.ListDueCleanup(ctx, now, limit)
}

func (s *Synchronizer) ClaimCleanup(ctx context.Context, userID, documentID string, now, leaseUntil time.Time) (bool, error) {
	return s.lifecycle.ClaimCleanup(ctx, userID, documentID, now, leaseUntil)
}

func (s *Synchronizer) CompleteCleanup(ctx context.Context, userID, documentID string) error {
	return s.lifecycle.CompleteCleanup(ctx, userID, documentID)
}

func (s *Synchronizer) FailCleanup(ctx context.Context, userID, documentID string, message string, retryAt time.Time) error {
	return s.lifecycle.FailCleanup(ctx, userID, documentID, message, retryAt)
}

func (s *Synchronizer) MarkDeleting(ctx context.Context, userID, documentID string) (*domain.Document, error) {
	return s.lifecycle.MarkDeleting(ctx, userID, documentID)
}

// StagedChunks is a read the index never touches, forwarded so a driver holding the
// decorator can see the whole queue interface.
func (s *Synchronizer) StagedChunks(ctx context.Context, userID, documentID string, version int) ([]domain.DocumentChunk, error) {
	return s.lifecycle.StagedChunks(ctx, userID, documentID, version)
}

// indexDocument embeds the chunks the store actually published for one document
// version and writes them, returning how many it stored. It reads back rather than
// trusting the caller's slice because chunk ids are assigned by the store, and an id
// that differs by one character is a citation that names nothing.
func (s *Synchronizer) indexDocument(ctx context.Context, userID string, document *domain.Document, version int) (int, error) {
	if document == nil {
		return 0, errors.New("redisindex: a document is required")
	}
	chunks, err := s.lifecycle.StagedChunks(ctx, userID, document.ID, version)
	if err != nil {
		return 0, err
	}
	pending := chunks
	if len(pending) == 0 {
		// Nothing of this version is retrievable. That is a store question, not an
		// index one, and staying silent here would let a document look healthy forever.
		return 0, fmt.Errorf("redisindex: document %s version %d published no chunks", document.ID, version)
	}
	vectors, err := s.embed(ctx, pending)
	if err != nil {
		return 0, err
	}
	entries := make([]retrieval.VectorEntry, 0, len(pending))
	for index, chunk := range pending {
		entries = append(entries, retrieval.VectorEntry{Chunk: chunk, Vector: vectors[index], Identity: s.identity})
	}
	if err := s.index.Upsert(ctx, userID, entries); err != nil {
		return 0, err
	}
	return len(pending), nil
}

func (s *Synchronizer) embed(ctx context.Context, chunks []domain.DocumentChunk) ([][]float32, error) {
	texts := make([]string, 0, len(chunks))
	for _, chunk := range chunks {
		texts = append(texts, retrieval.EmbeddingInput(chunk))
	}
	vectors, err := s.embedder.Embed(ctx, texts)
	if err != nil {
		return nil, err
	}
	if len(vectors) != len(chunks) {
		return nil, fmt.Errorf("redisindex: embedder returned %d vectors for %d chunks", len(vectors), len(chunks))
	}
	return vectors, nil
}

// indexFailureReason and the cleanup schedule used to live here. They are the
// contract's now (knowledge.IndexFailureReason, knowledge.CleanupBackoff) because the
// inline publish and a queued driver must record the same words and wait the same long.

// Reindex rebuilds the vectors of one user's whole knowledge base. It exists because a
// model change makes every stored identity stale, and an operator needs one operation
// that ends with an index that answers - not a half-migrated mix of two models.
//
// It is reachable through the Lifecycle surface only: no HTTP route drives it, and it
// is not a substitute for the failure, retry and cleanup states above.
func (s *Synchronizer) Reindex(ctx context.Context, userID string) (int, error) {
	collections, err := s.Store.ListCollections(ctx, userID)
	if err != nil {
		return 0, err
	}
	indexed := 0
	for _, collection := range collections {
		documents, err := s.Store.ListDocuments(ctx, userID, collection.ID)
		if err != nil {
			return indexed, err
		}
		for _, document := range documents {
			// The version answering queries is what gets rebuilt. document.Version is
			// the staged one, which after a failed update points at text nobody is
			// serving: indexing it would publish the failure, and keeping it is what
			// would delete the vectors that still answer. With no serving version there
			// is nothing to rebuild and nothing to drop, so the document is skipped.
			serving := document.ServingVersion()
			if serving <= 0 {
				continue
			}
			if _, err := s.indexDocument(ctx, userID, &document, serving); err != nil {
				return indexed, err
			}
			indexed++
			if err := s.index.DeleteVersions(ctx, userID, document.ID, serving); err != nil {
				return indexed, err
			}
		}
	}
	return indexed, nil
}
