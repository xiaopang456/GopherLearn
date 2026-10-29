package knowledge

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/repository"
)

// ErrDocumentBusy reports a lifecycle operation that arrived while the document's
// staged version was still being worked on. It is a conflict in HTTP terms, exactly
// as in v2 (controller/knowledge/document.go:205-206).
var ErrDocumentBusy = fmt.Errorf("%w: knowledge: document is busy", repository.ErrConflict)

// ErrDocumentChanged reports a staged replacement built on a version the document no
// longer has - v2's ErrDocumentChanged (dao/knowledge/document.go:56-58). It differs from
// ErrDocumentBusy in cause, not in consequence: busy says "the current version is not
// finished", changed says "the version you read is not the version in the row". Both are
// conflicts to a client, and both leave the row exactly as it was; keeping them apart is
// what lets a caller tell "wait and retry" from "re-read and re-decide".
var ErrDocumentChanged = fmt.Errorf("%w: knowledge: document version changed", repository.ErrConflict)

// StageOntoCurrentVersion is the only way to say "I did not read the row first, so I have
// no version to stand on" to StageReplacement. A named constant rather than an absent or
// zero argument on purpose: real versions start at 1, so a caller that forgets to pass an
// expectation is refused, and a composition that genuinely has no pre-read cannot be
// mistaken for one.
const StageOntoCurrentVersion = -1

// ErrDocumentNotRetryable reports a retry asked for a document that is not in the
// failed state, so there is nothing for a retry to do. v2 refuses it the same way
// (dao/knowledge/document.go:333-351).
var ErrDocumentNotRetryable = fmt.Errorf("%w: knowledge: document is not failed", repository.ErrConflict)

// ErrIllegalTransition reports a lifecycle call that the current state does not
// allow - completing a row nobody claimed, cleaning up a row that is not waiting to
// be cleaned. v2 calls this ErrIndexTransition.
var ErrIllegalTransition = errors.New("knowledge: index status transition rejected")

// IndexOutcome is what an indexing pass reports for one staged version. Success
// publishes it as the active version; failure leaves the previous active version
// answering, which is the property a rewrite must never lose.
type IndexOutcome struct {
	Status       domain.DocumentIndexStatus
	ChunkCount   int
	ErrorMessage string
}

// QueuedDocument is one item of a cross-user queue read.
//
// The owner travels with the document because every transition on Lifecycle is scoped
// by it: a queue that hands back a document without its user cannot be worked, since
// claiming, completing and cleaning up all refuse a caller who cannot name the owner.
// The owner deliberately is not a field of domain.Document - that struct reaches the
// read paths and, through them, anything a client may see, and whose collection a
// document belongs to is not information a reader of the document should receive.
// v2 carries it on the row (model.KnowledgeDocument.UserID); the target keeps
// ownership in the collection, so a queue query has to join to find it.
type QueuedDocument struct {
	UserID   string
	Document domain.Document
}

// Lifecycle is v2's knowledge indexing and cleanup state machine, published apart
// from Store because almost every reader only ever reads: retrieval, citations and
// chat need "what is searchable now", not the machinery that got it there. Only
// whatever drives indexing needs this surface.
//
// The states are the ones the source names (model/knowledge_document.go:9-30):
//
//	pending        -> indexing -> ready            (publishes ActiveVersion = Version)
//	                                    -> cleanup_pending (supersedes an older ActiveVersion)
//	indexing       -> failed                      (ActiveVersion stays where it was)
//	failed         -> pending                     (RetryFailed, and only from failed)
//	indexing       -> pending                     (RequeueIndexing, on cancellation)
//	any non-indexing live -> deleting -> removed  (MarkDeleting, then cleanup)
//
// The transitions are compare-and-swap on (document, user, version, expected status):
// a claim that does not match exactly that tuple changes nothing and reports false,
// which is what lets several drivers look at the same queue without double-working a
// document. That is v2's ClaimForIndexing/ClaimCleanup shape, not a new design.
type Lifecycle interface {
	// StageDocument files the first version of a document as pending, with no
	// active version: nothing of it answers queries until an indexing pass succeeds.
	StageDocument(ctx context.Context, userID string, document *domain.Document, chunks []domain.DocumentChunk) (*domain.Document, error)
	// StageReplacement advances Version, keeps the previous version's chunks in
	// place, and leaves ActiveVersion where it was - so a rewrite that never indexes
	// cannot take down the text that was working.
	//
	// It refuses with ErrDocumentUnchanged only when the document is healthy
	// (status ready) and the incoming hash equals the active version's hash. On any
	// other status the same bytes still have work to do: a failed rewrite of
	// identical content must be able to try again, and short-circuiting there would
	// permanently strand a document whose index rows were never written.
	//
	// expectedVersion is v2's guard (dao StageUpdate's ErrDocumentChanged): the caller
	// read the document, chose the version its published source file is named after, and
	// is asking for that one. A row that has moved since then refuses - not because two
	// requests cannot be serialized by the lock, but because the file on disk is named
	// for a version that is no longer the next one, and silently renumbering the request
	// would pair it with bytes another request already committed. Every check below,
	// including this one, runs after the row is locked.
	StageReplacement(ctx context.Context, userID, documentID string, expectedVersion int,
		replacement DocumentReplacement) (*domain.Document, error)

	// ClaimForIndexing moves pending -> indexing for exactly that version and
	// reports false when the row is not in that state.
	ClaimForIndexing(ctx context.Context, userID, documentID string, version int) (bool, error)
	// CompleteIndexing closes a claimed pass. Success publishes the version;
	// failure records why and leaves the older active version in place. A success
	// that supersedes a different active version queues that version for cleanup
	// instead of dropping its rows inline, so a failed removal stays retryable.
	CompleteIndexing(ctx context.Context, userID, documentID string, version int, outcome IndexOutcome) error
	// RequeueIndexing returns a claimed pass to pending after a cancellation.
	RequeueIndexing(ctx context.Context, userID, documentID string, version int) error
	// RecoverInterruptedIndexing is the startup pass: no work from a previous
	// process can still be running, so every row stuck in indexing goes back to
	// pending. It reports how many rows it moved and is safe to run repeatedly - the
	// second run moves nothing.
	RecoverInterruptedIndexing(ctx context.Context) (int, error)
	// RetryFailed moves failed -> pending. Anything else reports
	// ErrDocumentNotRetryable, and a document of another user reports missing.
	RetryFailed(ctx context.Context, userID, documentID string) error

	// StagedChunks returns the chunk rows one version stored, whether or not that
	// version is the one answering queries. It is on Lifecycle rather than Store
	// because only a driver needs it: retrieval must never see a version that has not
	// published, while an indexing pass has to read exactly the version it claimed.
	StagedChunks(ctx context.Context, userID, documentID string, version int) ([]domain.DocumentChunk, error)

	// ListPending returns staged work oldest-first, bounded by limit. It is the one
	// read on this surface that is not scoped by a user, so each item carries its
	// owner - see QueuedDocument.
	ListPending(ctx context.Context, limit int) ([]QueuedDocument, error)
	// ClaimCleanup takes one lease on recorded cleanup work until leaseUntil.
	ClaimCleanup(ctx context.Context, userID, documentID string, now, leaseUntil time.Time) (bool, error)
	// ListDueCleanup returns recorded cleanup whose lease has expired or run out.
	ListDueCleanup(ctx context.Context, now time.Time, limit int) ([]QueuedDocument, error)
	// CompleteCleanup finishes recorded work: a superseded version returns the
	// document to ready with only the live version's rows left, and a deletion
	// removes the document.
	CompleteCleanup(ctx context.Context, userID, documentID string) error
	// FailCleanup records a failed cleanup attempt with its next attempt time, so
	// the work is not lost when the removal itself fails.
	FailCleanup(ctx context.Context, userID, documentID string, message string, retryAt time.Time) error
	// MarkDeleting makes the document invisible to every read and releases its name
	// and content hash for reuse, then records the removal of its rows. It refuses a
	// document whose version is mid-index rather than racing the pass.
	MarkDeleting(ctx context.Context, userID, documentID string) (*domain.Document, error)
}
