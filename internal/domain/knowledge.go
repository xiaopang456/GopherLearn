package domain

import (
	"strings"
	"time"
)

// DocumentFormat is the closed set of text formats phase 3A can chunk.
type DocumentFormat string

const (
	DocumentFormatMarkdown DocumentFormat = "markdown"
	DocumentFormatText     DocumentFormat = "text"
)

func (f DocumentFormat) IsValid() bool {
	switch f {
	case DocumentFormatMarkdown, DocumentFormatText:
		return true
	default:
		return false
	}
}

// NormalizeDocumentFormat maps a file extension or format name onto a known
// format. The empty value and unknown spellings return false so a caller cannot
// silently store something the chunker will not understand.
func NormalizeDocumentFormat(value string) (DocumentFormat, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "markdown", "md", "mkd":
		return DocumentFormatMarkdown, true
	case "text", "txt", "plain", "plain text":
		return DocumentFormatText, true
	default:
		return "", false
	}
}

// Collection is a user-owned group of documents that a Chat turn may ground an
// answer against. It is the unit of authorization for knowledge.enabled=true.
type Collection struct {
	ID        string
	UserID    string
	Title     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// DocumentIndexStatus is the lifecycle state of one document's index, taken from
// v2's model/knowledge_document.go:21-30. It is a closed set on purpose: a state
// the store cannot name is a state no recovery pass can find again.
type DocumentIndexStatus string

const (
	// DocumentStatusPending is staged and waiting for an indexing pass. Nothing of
	// this version answers queries yet.
	DocumentStatusPending DocumentIndexStatus = "pending"
	// DocumentStatusIndexing is claimed by exactly one pass. A row left in this
	// state means the process that claimed it died, which is what the startup
	// recovery pass is for.
	DocumentStatusIndexing DocumentIndexStatus = "indexing"
	// DocumentStatusReady means ActiveVersion answers queries.
	DocumentStatusReady DocumentIndexStatus = "ready"
	// DocumentStatusFailed means the staged version could not be indexed. The
	// previous ActiveVersion keeps answering, and the document is retryable.
	DocumentStatusFailed DocumentIndexStatus = "failed"
	// DocumentStatusCleanupPending means a newer version is live and the superseded
	// one's stored rows still have to be dropped.
	DocumentStatusCleanupPending DocumentIndexStatus = "cleanup_pending"
	// DocumentStatusDeleting means the document is already invisible to every read
	// and its name and content hash are released; the removal of its rows is
	// recorded work.
	DocumentStatusDeleting DocumentIndexStatus = "deleting"
	// DocumentStatusDeleted is v2's terminal state. The target removes the row
	// outright, so a reader cannot tell a deleted document from a missing one -
	// which is what the ownership rules in knowledge.Store require.
	DocumentStatusDeleted DocumentIndexStatus = "deleted"
)

func (s DocumentIndexStatus) Valid() bool {
	switch s {
	case DocumentStatusPending, DocumentStatusIndexing, DocumentStatusReady, DocumentStatusFailed,
		DocumentStatusCleanupPending, DocumentStatusDeleting, DocumentStatusDeleted:
		return true
	default:
		return false
	}
}

// Document is one ingested source inside a Collection.
//
// ContentHash and FileSize describe the bytes the client sent, not the chunks
// that were stored. An empty ContentHash or a zero FileSize means "not measured"
// - rows written before ingestion recorded metadata, or rows a test planted
// directly - and a store must treat that as unknown rather than as a size of
// zero or a hash that matches every other unknown. See knowledge.Usage.
type Document struct {
	ID           string
	CollectionID string
	Title        string
	Format       DocumentFormat
	// Version is the staged version: the one being built, or the last one built,
	// whether or not it ever became searchable.
	Version int
	// ActiveVersion is the version answering queries. It lags Version while a
	// rewrite is pending and stays behind it when a rewrite failed, which is the
	// whole point of the split: a failed update must not silence the text that was
	// working before it.
	ActiveVersion int
	ChunkCount    int
	// IndexStatus names where the staged version is. An empty value means "not
	// tracked" and only rows written before the lifecycle columns existed can have
	// it; the store reads that as ready, because those documents were published by
	// the synchronous path.
	IndexStatus  DocumentIndexStatus
	ErrorMessage string
	// CleanupVersion is the superseded version whose stored rows still have to be
	// dropped; zero together with a deleting status means "every version this
	// document ever had". CleanupAttempts and CleanupAfter are what make the removal
	// retryable instead of a fire-and-forget call.
	CleanupVersion  int
	CleanupAttempts int
	CleanupAfter    time.Time
	// ContentHash is the hex SHA-256 of the document's normalized text: BOM
	// stripped and CRLF/CR folded to LF. Hashing the normalized form is what makes
	// the same notes uploaded from a Windows editor and a Linux editor one
	// document, while the raw bytes differ in length.
	ContentHash string
	// FileSize is the raw byte count as received, and it is what the per-user
	// storage quota adds up. It is deliberately not the length of the normalized
	// text: the quota bounds what a client had to send, and the hash bounds what
	// the content means.
	FileSize  int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ServingVersion is the version whose chunks answer queries, and it is the only
// place the legacy exception lives. A row written before the lifecycle columns
// existed was published at its own version by the synchronous writer, so it serves
// Version; a row the lifecycle wrote serves ActiveVersion, which is 0 until an
// indexing pass succeeds. Reading the exception in the mapping instead would leave
// retrieval - which compares against a column - unable to find any pre-existing
// document at all.
//
// The exception keys on the status being absent, never on ActiveVersion being 0:
// treating "no active version" as "serve the staged version" would make a document
// that is still pending indexable answer queries from chunks no embedder has ever
// seen. A migration that has to fall back has to fall back on recorded absence.
func (d Document) ServingVersion() int {
	if d.IndexStatus == "" {
		return d.Version
	}
	return d.ActiveVersion
}

// Retrievable reports whether this document's serving version may answer queries. A
// staged-but-unindexed version, a failed one and one being deleted all read as
// absent, which is why retrieval filters on this rather than on "a row exists".
func (d Document) Retrievable() bool {
	return d.ServingVersion() > 0 &&
		d.IndexStatus != DocumentStatusDeleting && d.IndexStatus != DocumentStatusDeleted
}

// Rewritable reports whether a new version may be staged onto this document.
//
// v2 checks this before it writes a single byte (service/knowledge/document.go:148-151):
// a rewrite onto a pass that is still running, or onto a delete that is being carried
// out, would produce two versions racing for one document row. Untracked rows count as
// rewritable because the synchronous writer had published them - which is the same
// reading ServingVersion makes, so the two rules cannot disagree about a legacy row.
//
// A store still has to decide this again inside its own transaction; this is the check
// that keeps an ingest from storing bytes for a rewrite the store would refuse.
func (d Document) Rewritable() bool {
	if d.IndexStatus == "" {
		return d.Version > 0
	}
	return d.IndexStatus == DocumentStatusReady && d.ActiveVersion == d.Version && d.Version > 0
}

// DocumentChunk is a retrievable unit of a Document version. It keeps the
// section it came from and the line span it covered so an answer can point at
// real provenance instead of a bare snippet.
type DocumentChunk struct {
	ID           string
	DocumentID   string
	CollectionID string
	Version      int
	Order        int
	Section      string
	Text         string
	StartLine    int
	EndLine      int
	CreatedAt    time.Time
}

// Source is the citation record a grounded turn produces. Scores are
// deliberately absent: retrieval ranking is an internal implementation detail
// and publishing it would make the public contract depend on it.
type Source struct {
	SourceID     string `json:"source_id"`
	DocumentID   string `json:"document_id"`
	CollectionID string `json:"collection_id"`
	ChunkID      string `json:"chunk_id"`
	Title        string `json:"title"`
	Section      string `json:"section,omitempty"`
	Snippet      string `json:"snippet"`
}

// BuildSource derives the citation for one chunk. The source id is stable for a
// given chunk so a client can deduplicate repeated references across turns.
func BuildSource(documentTitle string, chunk DocumentChunk) Source {
	return Source{
		SourceID:     "src-" + chunk.ID,
		DocumentID:   chunk.DocumentID,
		CollectionID: chunk.CollectionID,
		ChunkID:      chunk.ID,
		Title:        documentTitle,
		Section:      chunk.Section,
		Snippet:      Snippet(chunk.Text, 280),
	}
}

// CloneSources returns a detached copy of a citation list. Repositories hand
// copies in and get copies back, so a caller that sorts or truncates what it was
// given cannot rewrite what is stored. An empty list clones to nil, which is the
// one representation of "this message cites nothing".
func CloneSources(sources []Source) []Source {
	if len(sources) == 0 {
		return nil
	}
	return append([]Source(nil), sources...)
}

// Snippet returns the first limit runes of text on a rune boundary.
func Snippet(text string, limit int) string {
	text = strings.TrimSpace(text)
	runes := []rune(text)
	if limit <= 0 || len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "…"
}
