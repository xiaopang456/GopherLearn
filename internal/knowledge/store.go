// Package knowledge stores user-owned documents and the chunks retrieved to
// ground a Chat turn. Everything here is reached through an interface so the
// service and responder layers stay free of storage and embedding-SDK types.
package knowledge

import (
	"context"
	"errors"
	"fmt"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/repository"
)

// Per-user ceilings, taken from the v2 deployment defaults (config.toml
// [knowledgeConfig]: maxDocumentsPerUser=100, maxTotalSizeBytes=52428800). They
// are product limits, not tuning knobs, so they are constants here and the
// per-store override exists only for tests that need to reach a boundary without
// writing fifty megabytes.
const (
	MaxDocumentsPerUser  = 100
	MaxTotalBytesPerUser = int64(50 << 20)
)

// Limits bounds one user's knowledge base.
type Limits struct {
	MaxDocuments  int
	MaxTotalBytes int64
}

// DefaultLimits is what a store enforces when nothing was configured.
func DefaultLimits() Limits {
	return Limits{MaxDocuments: MaxDocumentsPerUser, MaxTotalBytes: MaxTotalBytesPerUser}
}

// Usage is what one user currently occupies. Unmeasured counts the documents
// whose byte size was never recorded, so a quota that was computed over a
// partially measured table says so instead of looking exact.
type Usage struct {
	DocumentCount int64
	TotalBytes    int64
	Unmeasured    int64
}

var (
	// ErrDocumentNameTaken reports another live document of the same user already
	// carrying this title. It wraps ErrConflict so the HTTP contract stays the 409
	// it already is; the distinct sentinel is what lets tests and logs tell the
	// four refusals apart until the public code split is decided.
	ErrDocumentNameTaken = fmt.Errorf("%w: knowledge: a document with this title already exists", repository.ErrConflict)
	// ErrDocumentContentDuplicate reports the same normalized content already
	// stored live for this user, under any title.
	ErrDocumentContentDuplicate = fmt.Errorf("%w: knowledge: this content is already stored", repository.ErrConflict)
	// ErrDocumentCountLimit and ErrStorageLimit are the two quota ceilings: number
	// of documents, and summed raw bytes.
	ErrDocumentCountLimit = fmt.Errorf("%w: knowledge: document count limit reached", repository.ErrConflict)
	ErrStorageLimit       = fmt.Errorf("%w: knowledge: storage limit reached", repository.ErrConflict)
	// ErrDocumentUnchanged reports that a replacement carried the same content the
	// document already holds. It is not a failure: the store leaves the version
	// alone and hands back the untouched document, because rewriting identical text
	// would retire live chunks and re-embed them for no change in content.
	ErrDocumentUnchanged = errors.New("knowledge: document content is unchanged")
	// ErrOwnerUnresolved reports that the engine that is supposed to serialise one
	// user's knowledge writes found no row for that user to lock. It deliberately
	// wraps no repository sentinel: this is not the client's mistake and not a
	// missing resource, it is a deployment whose principal names a user the
	// database does not have, and reporting it as 404 or 409 would send a reader to
	// the wrong place.
	ErrOwnerUnresolved = errors.New("knowledge: the writing user has no row to lock")
)

// DocumentReplacement is one rewrite request: the new title, the new format, the new
// chunks, and the metadata describing the bytes they came from. It is a struct so a
// rewrite carries its own size and hash - a replacement that only restyles the
// document still has to be measured against the storage quota, and one that
// reproduced the same bytes must be recognised before the version advances.
//
// Title is part of the request because v2's update renames the document and the new
// name goes through the same per-user uniqueness rule (dao/knowledge/document.go:73-81).
// An empty Title keeps the current name.
type DocumentReplacement struct {
	Title       string
	Format      domain.DocumentFormat
	ContentHash string
	FileSize    int64
	Chunks      []domain.DocumentChunk
}

// Store persists collections, documents and their chunks.
//
// Every method takes the calling userID for anything that can be owned, and an
// implementation must report a resource owned by somebody else exactly the way
// it reports a missing one, so this package cannot be used to probe whether
// another user's collection exists.
//
// The two quota and uniqueness refusals belong to the store, not to the caller:
// they have to be decided against the same snapshot the row is written into, or
// two concurrent uploads could each pass a check made before the other existed.
// On an engine that can lock a row the ordering is fixed by that requirement:
// lock the owning user, then read, then decide, then write - never read, decide,
// and then find a lock.
type Store interface {
	CreateCollection(ctx context.Context, collection *domain.Collection) (*domain.Collection, error)
	ListCollections(ctx context.Context, userID string) ([]domain.Collection, error)
	GetCollection(ctx context.Context, userID, collectionID string) (*domain.Collection, error)

	// CreateDocument stores a version 1 document together with its chunks. The
	// store assigns the version number and chunk count, and resolves ownership
	// through the collection the document is filed under. It refuses a title or
	// content hash that another live document of this user already holds, and a
	// document that would pass the caller's limits.
	CreateDocument(ctx context.Context, userID string, document *domain.Document, chunks []domain.DocumentChunk) (*domain.Document, error)
	// UsageByUser reports what one user's whole knowledge base occupies, across
	// every collection they own.
	UsageByUser(ctx context.Context, userID string) (Usage, error)
	GetDocument(ctx context.Context, userID, documentID string) (*domain.Document, error)
	// ListDocuments returns the documents of one owned collection in creation
	// order. A collection the caller does not own is reported as missing, the way
	// every other read here does.
	ListDocuments(ctx context.Context, userID, collectionID string) ([]domain.Document, error)
	// ReplaceDocumentContent installs a new chunk set and advances the version,
	// which retires the previous chunks from retrieval in the same step. When the
	// replacement carries the document's own hash the store advances nothing and
	// returns the untouched document with ErrDocumentUnchanged.
	ReplaceDocumentContent(ctx context.Context, userID, documentID string, replacement DocumentReplacement) (*domain.Document, error)
	// DeleteDocument removes a document together with the chunks of every version
	// it ever had, so nothing it contained can be retrieved afterwards. It is the
	// only removal the store offers: a collection is deleted by nobody yet, which
	// is why documents have to be removable on their own.
	DeleteDocument(ctx context.Context, userID, documentID string) error

	// ChunksFor returns the currently retrievable chunks of the given
	// collections, ordered by collection, document and chunk position.
	ChunksFor(ctx context.Context, userID string, collectionIDs []string) ([]domain.DocumentChunk, error)
	// DocumentTitles maps document ids to their titles for citation building.
	DocumentTitles(ctx context.Context, userID string, documentIDs []string) (map[string]string, error)

	// EnsureReadable reports whether every collection id is known and owned by
	// userID. It is the authorization gate a grounded turn must pass before any
	// retrieval runs.
	EnsureReadable(ctx context.Context, userID string, collectionIDs []string) error
}
