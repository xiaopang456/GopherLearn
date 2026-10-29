package service

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xiaopang456/GopherLearn/internal/document"
	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/knowledge"
)

// KnowledgeService owns document ingestion. It is the only place that decides how
// content becomes chunks, so the HTTP layer never assembles domain structures
// and the store never guesses at chunking policy.
type KnowledgeService struct {
	store        knowledge.Store
	chunker      func(text string, options document.Options) ([]domain.DocumentChunk, error)
	options      document.Options
	sources      knowledge.SourceStorage
	sourceReader knowledge.SourceReader
}

func NewKnowledgeService(store knowledge.Store) *KnowledgeService {
	return &KnowledgeService{store: store, chunker: document.Chunk, options: document.DefaultOptions()}
}

// WithChunkOptions overrides the chunker bounds, which tests use to force a
// split they cannot reach with production sizes.
func (s *KnowledgeService) WithChunkOptions(options document.Options) *KnowledgeService {
	s.options = options
	return s
}

// WithSourceStorage turns on immutable per-version source storage, v2's
// knowledgeConfig.storageDir equivalent. Without it the service behaves exactly as it
// did: chunks are stored, citations work, and read-back reports that no source exists.
// With it, every ingest publishes the bytes before the version becomes pending, and a
// stage that then refuses - unchanged, a taken name, a spent quota - removes what it
// published.
func (s *KnowledgeService) WithSourceStorage(storage knowledge.SourceStorage) *KnowledgeService {
	s.sources = storage
	if storage != nil {
		reader, err := knowledge.NewSourceReader(s.store, storage)
		if err != nil {
			// A store is already installed at this point, so the only way to fail here is
			// an assembly mistake; read-back then says "no reader", never a half-wired one.
			s.sourceReader = nil
			return s
		}
		s.sourceReader = reader
	}
	return s
}

// DocumentSource is one version's stored source, returned to callers that quote it.
type DocumentSource struct {
	DocumentID string
	Version    int
	Title      string
	Format     domain.DocumentFormat
	Content    string
	Bytes      int64
}

// Three answers a caller has to be able to tell apart, because they lead to three
// different actions: nothing was ever stored (a document predating the source, which no
// retry fixes), the version asked for is not the one answering (a caller working from
// stale state), and the bytes disagree with the row (corruption, which is an operator
// problem, not a user one). Collapsing them into one "unreadable" is how a corrupt file
// gets reported as "this document has no source" forever.
var (
	ErrDocumentSourceMissing = errors.New("service: document source was never stored")
	ErrDocumentVersionStale  = errors.New("service: document version is not the searchable one")
	ErrDocumentSourceInvalid = errors.New("service: document source is invalid")
)

// DocumentInput is one ingestion request. Format wins over Filename when both
// are supplied.
type DocumentInput struct {
	Title    string
	Format   string
	Filename string
	Content  string
}

// preparedDocument is what the service needs before it can ask the store to file
// anything: the metadata describing the bytes as sent, plus the chunks cut from
// them. ContentHash and FileSize deliberately disagree - one identifies what the
// text says after normalization, the other is the raw byte count the storage
// quota spends - so they are computed here, once, and never re-derived.
type preparedDocument struct {
	title       string
	format      domain.DocumentFormat
	chunks      []domain.DocumentChunk
	contentHash string
	fileSize    int64
	// raw is the byte sequence the two numbers above describe and the sequence the
	// source file holds. It travels so the ingest stores exactly the bytes that were
	// measured, rather than re-deriving them and risking a hash of something else.
	raw []byte
}

// EnsureReadable implements service.CollectionAccess. A grounded turn cannot run
// without this check passing.
func (s *KnowledgeService) EnsureReadable(ctx context.Context, userID string, collectionIDs []string) error {
	if s == nil || s.store == nil {
		return ErrInternal
	}
	if len(collectionIDs) == 0 {
		return nil
	}
	if err := s.store.EnsureReadable(ctx, userID, collectionIDs); err != nil {
		return mapRepositoryError(err)
	}
	return nil
}

func (s *KnowledgeService) CreateCollection(ctx context.Context, userID, title string) (*domain.Collection, error) {
	if s == nil || s.store == nil {
		return nil, ErrInternal
	}
	if strings.TrimSpace(userID) == "" {
		return nil, ErrUnauthorized
	}
	title = strings.TrimSpace(title)
	if title == "" || utf8.RuneCountInString(title) > 200 {
		return nil, ErrInvalidRequest
	}
	collection, err := s.store.CreateCollection(ctx, &domain.Collection{UserID: userID, Title: title})
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	return collection, nil
}

func (s *KnowledgeService) ListCollections(ctx context.Context, userID string) ([]domain.Collection, error) {
	if s == nil || s.store == nil {
		return nil, ErrInternal
	}
	if strings.TrimSpace(userID) == "" {
		return nil, ErrUnauthorized
	}
	collections, err := s.store.ListCollections(ctx, userID)
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	return collections, nil
}

func (s *KnowledgeService) AddDocument(ctx context.Context, userID, collectionID string, input DocumentInput) (*domain.Document, error) {
	if s == nil || s.store == nil {
		return nil, ErrInternal
	}
	if strings.TrimSpace(userID) == "" {
		return nil, ErrUnauthorized
	}
	prepared, err := s.prepare(input, true)
	if err != nil {
		return nil, err
	}
	if s.sources != nil {
		return s.addDocumentWithSource(ctx, userID, strings.TrimSpace(collectionID), prepared)
	}
	documentRecord, err := s.store.CreateDocument(ctx, userID, &domain.Document{
		CollectionID: strings.TrimSpace(collectionID),
		Title:        prepared.title,
		Format:       prepared.format,
		ContentHash:  prepared.contentHash,
		FileSize:     prepared.fileSize,
	}, prepared.chunks)
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	return documentRecord, nil
}

// addDocumentWithSource is v2's ingest for a deployment that keeps original bytes:
// SaveTemp, Publish, StageDocument, and the row comes back pending.
//
// The synchronous publish is deliberately not used here. Waiting for embedding inside the
// upload would mean the caller's request duration is set by a model call, and v2's answer
// is a queue: the upload's job ends at "this version exists and its source is readable",
// and the index driver's job starts there. A deployment that stores sources therefore has
// to run the index pool, or its documents stay pending forever - which is the same
// trade v2 makes, and the reason it ships a worker.
func (s *KnowledgeService) addDocumentWithSource(ctx context.Context, userID, collectionID string,
	prepared preparedDocument) (*domain.Document, error) {
	lifecycle, err := s.lifecycleFor(userID)
	if err != nil {
		return nil, err
	}
	// The id is minted before the row exists because the source file is named after it,
	// and the bytes have to be reachable before anything can call this version pending.
	minted, err := knowledge.NewDocumentID()
	if err != nil {
		return nil, ErrInternal
	}
	discard, err := s.publishSource(ctx, userID, minted, 1, prepared)
	if err != nil {
		return nil, err
	}
	staged, err := lifecycle.StageDocument(ctx, userID, &domain.Document{
		ID: minted, CollectionID: collectionID, Title: prepared.title, Format: prepared.format,
		ContentHash: prepared.contentHash, FileSize: prepared.fileSize,
	}, prepared.chunks)
	if err != nil {
		return nil, errors.Join(mapRepositoryError(err), discard())
	}
	return staged, nil
}

// publishSource runs v2's pre-stage half of an ingest - SaveTemp then Publish - and hands
// back the removal that undoes exactly this attempt's bytes. It is a no-op for a
// deployment with no source storage, so callers do not branch on it twice.
//
// Nothing here stages a row: a version becomes pending only once its bytes are readable,
// because pending is something a driver claims and indexes, and an index built over bytes
// that were never stored is a document nobody can cite and no retry can repair.
func (s *KnowledgeService) publishSource(ctx context.Context, userID, documentID string,
	version int, prepared preparedDocument) (func() error, error) {
	if s.sources == nil {
		return func() error { return nil }, nil
	}
	if documentID == "" {
		return nil, ErrInternal
	}
	stored, err := s.sources.SaveTemp(ctx, userID, documentID, prepared.raw)
	if err != nil {
		return nil, mapSourceError(err)
	}
	if stored.ContentHash != prepared.contentHash || stored.FileSize != prepared.fileSize {
		// The service already hashed these bytes to build the row's metadata. If the
		// storage reports different numbers for what it wrote, one of the two is looking at
		// something else, and publishing that under this version would put a file behind a
		// hash it does not satisfy.
		return nil, errors.Join(ErrDocumentSourceInvalid, s.discardTemp(ctx, userID, documentID, stored.TempPath))
	}
	published, err := s.sources.Publish(ctx, userID, documentID, version, prepared.format, stored.TempPath)
	if err != nil {
		return nil, errors.Join(mapSourceError(err), s.discardTemp(ctx, userID, documentID, stored.TempPath))
	}
	return func() error {
		// The compensation outlives the caller: an upload whose client hung up still has to
		// give back the bytes it published for a version that was never staged, and asking
		// the storage on the context that just died gets the removal refused. It removes
		// the file this call published, by the name Publish returned - not "the file for
		// this version", which is ambiguous the moment a second request names the same
		// version with another format, and would then either fail to clean up or destroy
		// the file the winning request committed.
		bounded, cancel := knowledge.CompensationContext(ctx)
		defer cancel()
		return mapSourceError(s.sources.Delete(bounded, userID, documentID, published))
	}, nil
}

// discardTemp removes a temporary upload, on a context that cannot be vetoed by a caller
// who has already left. The temp file is scratch by definition, so a failure here is
// reported but never replaces the error that caused it.
func (s *KnowledgeService) discardTemp(ctx context.Context, userID, documentID, tempPath string) error {
	bounded, cancel := knowledge.CompensationContext(ctx)
	defer cancel()
	return mapSourceError(s.sources.DiscardTemp(bounded, userID, documentID, tempPath))
}

// ReadDocumentSource returns the stored source of the version that is currently
// answering, normalized the way chunks and hashes are. version is checked against the
// searchable version rather than the newest one: during a staged - or a failed - update
// those differ, and the older one is the text retrieval is quoting.
//
// A document whose bytes were never stored reports ErrDocumentSourceMissing. Chunk text
// is never assembled into an answer for it: the chunks are excerpts of a normalized form
// that no longer has a whole-file counterpart, and calling that the original is the kind
// of claim the citation layer exists to prevent.
func (s *KnowledgeService) ReadDocumentSource(ctx context.Context, userID, documentID string,
	version int) (*DocumentSource, error) {
	if s == nil || s.store == nil {
		return nil, ErrInternal
	}
	if strings.TrimSpace(userID) == "" {
		return nil, ErrUnauthorized
	}
	if s.sourceReader == nil {
		return nil, ErrDocumentSourceMissing
	}
	source, err := s.sourceReader.ReadActiveSource(ctx, userID, strings.TrimSpace(documentID), version)
	if err != nil {
		return nil, mapSourceError(err)
	}
	return &DocumentSource{
		DocumentID: source.DocumentID, Version: source.Version, Title: source.Title,
		Format: source.Format, Content: source.Content, Bytes: source.Bytes,
	}, nil
}

// ReplaceDocumentContent re-chunks the new text and advances the document
// version, which retires the previous chunks from retrieval. Text that hashes to
// what the document already holds advances nothing: the store reports it as
// unchanged and the live version stays the answer.
func (s *KnowledgeService) ReplaceDocumentContent(ctx context.Context, userID, documentID string, input DocumentInput) (*domain.Document, error) {
	if s == nil || s.store == nil {
		return nil, ErrInternal
	}
	if strings.TrimSpace(userID) == "" {
		return nil, ErrUnauthorized
	}
	target := strings.TrimSpace(documentID)
	if s.sources == nil {
		return s.replaceWithoutSource(ctx, userID, target, input)
	}
	lifecycle, err := s.lifecycleFor(userID)
	if err != nil {
		return nil, err
	}
	current, err := s.store.GetDocument(ctx, userID, target)
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	// The two checks v2 makes before it writes a single byte: a document whose staged
	// version is still being worked, or whose delete is still being carried out, takes no
	// new version - and bytes identical to what is already stored advance nothing. Running
	// them here keeps a refused rewrite from publishing a file it would then have to take
	// back; the store decides both again inside its own transaction, under the user lock.
	if !current.Rewritable() {
		return nil, ErrConflict
	}
	prepared, err := s.prepare(input, false)
	if err != nil {
		return nil, err
	}
	if prepared.contentHash != "" && prepared.contentHash == current.ContentHash {
		return current, nil
	}
	// The version the new bytes land in is decided before the row is touched, so the file
	// can exist first - and it is handed back to the store as an expectation. If another
	// rewrite committed in between, this one is refused rather than renumbered onto the
	// next version, which would pair a version with bytes the other request published for
	// it. Two rewrites racing on the same version can still both publish, because the file
	// name carries the format; expectedVersion is what makes exactly one of them stage.
	next := current.Version + 1
	discard, err := s.publishSource(ctx, userID, target, next, prepared)
	if err != nil {
		return nil, err
	}
	staged, err := lifecycle.StageReplacement(ctx, userID, target, current.Version, knowledge.DocumentReplacement{
		// The title travels: v2's update always carries the new original name and takes
		// the rename when the content also changed. When only the name differs, v2's
		// unchanged rule wins - it compares the content hash and nothing else - so the
		// rename is refused rather than half-applied.
		Title:       prepared.title,
		Format:      prepared.format,
		ContentHash: prepared.contentHash,
		FileSize:    prepared.fileSize,
		Chunks:      prepared.chunks,
	})
	if err != nil {
		// Compensation covers this attempt's bytes and nothing else: the version still
		// answering keeps its source, and a retired version's bytes are the cleanup
		// queue's to remove - not something an ingest deletes early.
		compensation := discard()
		if errors.Is(err, knowledge.ErrDocumentUnchanged) {
			// The store decided unchanged under the lock, so the file published for a
			// version that will not exist goes back and the row answers as it did before
			// the call.
			return current, compensation
		}
		return nil, errors.Join(mapRepositoryError(err), compensation)
	}
	return staged, nil
}

// replaceWithoutSource is the path for a deployment that keeps no original bytes. It goes
// through the store's own composition, which publishes the version inline: no queue, no
// driver, and no read-back to quote.
func (s *KnowledgeService) replaceWithoutSource(ctx context.Context, userID, documentID string,
	input DocumentInput) (*domain.Document, error) {
	prepared, err := s.prepare(input, false)
	if err != nil {
		return nil, err
	}
	replaced, err := s.store.ReplaceDocumentContent(ctx, userID, documentID, knowledge.DocumentReplacement{
		Title:       prepared.title,
		Format:      prepared.format,
		ContentHash: prepared.contentHash,
		FileSize:    prepared.fileSize,
		Chunks:      prepared.chunks,
	})
	if errors.Is(err, knowledge.ErrDocumentUnchanged) {
		return replaced, nil
	}
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	return replaced, nil
}

// ListDocuments returns the documents of one collection the caller owns. The
// listing is scoped by the collection rather than by the caller so a client can
// show what a knowledge base contains before choosing it for a turn.
func (s *KnowledgeService) ListDocuments(ctx context.Context, userID, collectionID string) ([]domain.Document, error) {
	if s == nil || s.store == nil {
		return nil, ErrInternal
	}
	if strings.TrimSpace(userID) == "" {
		return nil, ErrUnauthorized
	}
	documents, err := s.store.ListDocuments(ctx, userID, strings.TrimSpace(collectionID))
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	return documents, nil
}

// DeleteDocument removes a document and stops its chunks from being retrievable.
func (s *KnowledgeService) DeleteDocument(ctx context.Context, userID, documentID string) error {
	if s == nil || s.store == nil {
		return ErrInternal
	}
	if strings.TrimSpace(userID) == "" {
		return ErrUnauthorized
	}
	target := strings.TrimSpace(documentID)
	if s.sources == nil {
		if err := s.store.DeleteDocument(ctx, userID, target); err != nil {
			return mapRepositoryError(err)
		}
		return nil
	}
	// With sources, a delete is v2's: mark the document deleting and return. The row is now
	// the record of outstanding work, and the cleanup pool removes the derived rows and then
	// the bytes, retrying with the source's own schedule if either fails. Removing them here
	// would throw away the only handle on a removal that can still fail - and hard-deleting
	// the row instead would leave the bytes with nothing left to retry from.
	lifecycle, err := s.lifecycleFor(userID)
	if err != nil {
		return err
	}
	if _, err := lifecycle.MarkDeleting(ctx, userID, target); err != nil {
		return mapRepositoryError(err)
	}
	return nil
}

func (s *KnowledgeService) GetDocument(ctx context.Context, userID, documentID string) (*domain.Document, error) {
	if s == nil || s.store == nil {
		return nil, ErrInternal
	}
	if strings.TrimSpace(userID) == "" {
		return nil, ErrUnauthorized
	}
	record, err := s.store.GetDocument(ctx, userID, documentID)
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	return record, nil
}

// DocumentWorkState is the lifecycle answer v2's status query gives: which version is
// staged, which one is answering, where the staged one got to, why it stopped, and
// whether anything is still owed to it. It is a separate shape rather than a
// domain.Document so a caller that only wants the queue state cannot mistake a stored
// version for a searchable one - the pair that carries that difference is Version and
// ActiveVersion, and Retrievable is the only reading of them worth re-implementing.
type DocumentWorkState struct {
	DocumentID      string
	Version         int
	ActiveVersion   int
	IndexStatus     domain.DocumentIndexStatus
	ErrorMessage    string
	ChunkCount      int
	CleanupVersion  int
	CleanupAttempts int
	CleanupAfter    time.Time
	Retrievable     bool
}

// RetryDocument puts a failed version back in the queue (v2's RetryDocument,
// service/knowledge/document.go:125-137). Only a failed version is retryable; anything
// else the document may be doing reports a conflict, and a document of another user
// reports missing.
func (s *KnowledgeService) RetryDocument(ctx context.Context, userID, documentID string) (*DocumentWorkState, error) {
	lifecycle, err := s.lifecycleFor(userID)
	if err != nil {
		return nil, err
	}
	documentID = strings.TrimSpace(documentID)
	if documentID == "" {
		return nil, ErrInvalidRequest
	}
	if err := lifecycle.RetryFailed(ctx, userID, documentID); err != nil {
		return nil, mapRepositoryError(err)
	}
	return s.workStateOf(ctx, lifecycle, userID, documentID)
}

// DocumentWorkState reads the lifecycle state of one document the caller owns.
func (s *KnowledgeService) DocumentWorkState(ctx context.Context, userID, documentID string) (*DocumentWorkState, error) {
	lifecycle, err := s.lifecycleFor(userID)
	if err != nil {
		return nil, err
	}
	documentID = strings.TrimSpace(documentID)
	if documentID == "" {
		return nil, ErrInvalidRequest
	}
	return s.workStateOf(ctx, lifecycle, userID, documentID)
}

func (s *KnowledgeService) workStateOf(ctx context.Context, lifecycle knowledge.Lifecycle, userID, documentID string) (*DocumentWorkState, error) {
	store, ok := lifecycle.(knowledge.Store)
	if !ok {
		// Every implementation of Lifecycle in this codebase is also a Store; the check
		// is here so a future one cannot silently answer a status query with zeros.
		return nil, ErrInternal
	}
	record, err := store.GetDocument(ctx, userID, documentID)
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	return &DocumentWorkState{
		DocumentID: record.ID, Version: record.Version, ActiveVersion: record.ActiveVersion,
		IndexStatus: record.IndexStatus, ErrorMessage: record.ErrorMessage, ChunkCount: record.ChunkCount,
		CleanupVersion: record.CleanupVersion, CleanupAttempts: record.CleanupAttempts,
		CleanupAfter: record.CleanupAfter, Retrievable: record.Retrievable(),
	}, nil
}

// lifecycleFor answers the two preconditions every lifecycle call needs. A store that
// cannot record index state is an assembly mistake, not a client error - and it must not
// be reported as one, because the caller would read "bad request" from a deployment that
// simply forgot to wire the index driver.
func (s *KnowledgeService) lifecycleFor(userID string) (knowledge.Lifecycle, error) {
	if s == nil || s.store == nil {
		return nil, ErrInternal
	}
	if strings.TrimSpace(userID) == "" {
		return nil, ErrUnauthorized
	}
	lifecycle, ok := s.store.(knowledge.Lifecycle)
	if !ok {
		return nil, ErrInternal
	}
	return lifecycle, nil
}

func (s *KnowledgeService) DocumentTitles(ctx context.Context, userID string, documentIDs []string) (map[string]string, error) {
	if s == nil || s.store == nil {
		return nil, ErrInternal
	}
	titles, err := s.store.DocumentTitles(ctx, userID, documentIDs)
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	return titles, nil
}

func (s *KnowledgeService) prepare(input DocumentInput, requireTitle bool) (preparedDocument, error) {
	title := strings.TrimSpace(input.Title)
	if title == "" {
		// A filename counts only when there is one. path.Base("") is ".", and a title
		// resolved to "." would be a document named after nothing - accepted silently
		// on create, and on a rewrite it would overwrite the name the caller left out.
		if filename := strings.TrimSpace(input.Filename); filename != "" {
			if base := path.Base(filename); base != "." && base != "/" && base != ".." {
				title = base
			}
		}
	}
	if title == "" && requireTitle {
		return preparedDocument{}, ErrInvalidRequest
	}
	if utf8.RuneCountInString(title) > 200 {
		return preparedDocument{}, ErrInvalidRequest
	}
	format, err := resolveFormat(input)
	if err != nil {
		return preparedDocument{}, err
	}
	content := input.Content
	if strings.TrimSpace(content) == "" {
		return preparedDocument{}, ErrInvalidRequest
	}
	raw := []byte(content)
	if len(raw) > document.MaximumContentBytes {
		return preparedDocument{}, ErrInvalidRequest
	}
	if s.chunker == nil {
		return preparedDocument{}, errors.New("service: no chunker configured")
	}
	chunks, chunkErr := s.chunker(content, s.options)
	if chunkErr != nil {
		if errors.Is(chunkErr, document.ErrEmptyDocument) || errors.Is(chunkErr, document.ErrContentTooLarge) {
			return preparedDocument{}, ErrInvalidRequest
		}
		return preparedDocument{}, chunkErr
	}
	return preparedDocument{
		title: title, format: format, chunks: chunks, raw: raw,
		contentHash: document.ContentHash(raw), fileSize: int64(len(raw)),
	}, nil
}

// mapSourceError turns a storage answer into a service answer without losing the first.
// Each result wraps the original, so a caller can ask either "is this the document having
// no source" or "is this the specific storage failure" - and an operator debugging a
// corrupt file can still tell it apart from a legacy document that never had one.
func mapSourceError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, knowledge.ErrSourceNotStored):
		return fmt.Errorf("%w: %w", ErrDocumentSourceMissing, err)
	case errors.Is(err, knowledge.ErrSourceVersionNotRead):
		return fmt.Errorf("%w: %w", ErrDocumentVersionStale, err)
	case errors.Is(err, knowledge.ErrSourceInvalid),
		errors.Is(err, knowledge.ErrSourceTooLarge),
		errors.Is(err, knowledge.ErrUnsupportedSource):
		return fmt.Errorf("%w: %w", ErrDocumentSourceInvalid, err)
	case errors.Is(err, knowledge.ErrSourceVersionTaken):
		return fmt.Errorf("%w: %w", ErrConflict, err)
	case errors.Is(err, knowledge.ErrUnsafeSourcePath):
		// Somebody handed the storage an identifier it refuses to make a path out of.
		// That is a caller of *this* service being wrong, not the end user's request.
		return fmt.Errorf("%w: %w", ErrInternal, err)
	default:
		return mapRepositoryError(err)
	}
}

func resolveFormat(input DocumentInput) (domain.DocumentFormat, error) {
	if value := strings.TrimSpace(input.Format); value != "" {
		format, ok := domain.NormalizeDocumentFormat(value)
		if !ok {
			return "", ErrInvalidRequest
		}
		return format, nil
	}
	if extension := strings.ToLower(strings.TrimPrefix(path.Ext(input.Filename), ".")); extension != "" {
		format, ok := domain.NormalizeDocumentFormat(extension)
		if !ok {
			return "", ErrInvalidRequest
		}
		return format, nil
	}
	// With no format and no usable filename, plain text is the only safe
	// assumption: nothing in it can be misparsed as Markdown structure.
	return domain.DocumentFormatText, nil
}
