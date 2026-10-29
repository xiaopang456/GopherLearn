package knowledge

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/xiaopang456/GopherLearn/internal/document"
	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/repository"
)

// This file is the migration of v2's knowledge source storage and read-back
// (service/knowledge/storage.go, service/knowledge/source_reader.go). v2 can quote the
// searchable version of a document because it kept the uploaded bytes on disk, one
// immutable file per version. The target did not store them at all, so this is the
// piece that makes read-back exist rather than something it approximates.
//
// Two rules carried straight from the source decide everything else here:
//
//   - the bytes are published BEFORE a version becomes pending, so nothing can be
//     indexed and answered from a document whose source could not be stored;
//   - a published version is never overwritten. v2 uses os.Link rather than
//     Stat-then-Rename for exactly this reason (rename silently replaces a target, and a
//     version whose bytes changed under a reader breaks the hash the row carries).

// DefaultSourceMaxBytes is v2's knowledgeConfig.maxFileSizeBytes: the single-document
// ceiling the storage itself enforces on both sides of the read and write. It is not the
// chunker's bound (document.MaximumContentBytes) and not the per-user quota
// (Limits.MaxTotalBytesPerUser); three different numbers with three different owners, and
// folding them together is how one of them stops being enforceable.
const DefaultSourceMaxBytes int64 = 5 << 20

const (
	sourceFilePrefix = "source-v"
	sourceTempPrefix = ".upload-"
	// sourceDirMode and the file mode are v2's: 0o750 on directories, default on files.
	sourceDirMode os.FileMode = 0o750
)

// The names are path segments, so the characters that could make one mean something else
// - separators, dot-dot, glob - are refused at the door rather than escaped later.
var safePathSegment = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,63}$`)

// Source errors. Each names a different operator action: "never stored" is expected for
// documents written before the source existed and is reported, not repaired; "invalid"
// means the bytes on disk disagree with the row, which is corruption; "too large" means
// a file exceeded the read bound; "unsafe path" means somebody handed the storage an id it
// refuses to turn into a path.
var (
	ErrUnsafeSourcePath     = errors.New("knowledge: unsafe document source path")
	ErrUnsupportedSource    = errors.New("knowledge: unsupported document source type")
	ErrSourceVersionTaken   = errors.New("knowledge: document source version is already published")
	ErrSourceNotStored      = errors.New("knowledge: document source was never stored")
	ErrSourceInvalid        = errors.New("knowledge: document source does not match its metadata")
	ErrSourceTooLarge       = errors.New("knowledge: document source exceeds the read limit")
	ErrSourceVersionNotRead = errors.New("knowledge: document version is not the searchable one")
)

// StoredSource is what staging an upload costs the caller: where the temporary copy is,
// and the two numbers that describe it. ContentHash is over the NORMALIZED bytes and
// FileSize is the RAW length - deliberately not the same byte count, because the hash
// answers "what does this text say" (so a CRLF and an LF copy of one note are one
// document) while the size answers "what did the client have to send" (which is what the
// storage quota spends).
type StoredSource struct {
	TempPath    string
	ContentHash string
	FileSize    int64
}

// DocumentSource is one version's source, read back the way retrieval sees it: normalized
// text, the version it belongs to, and the document it came from. The filesystem path is
// not part of it - v2 keeps StoragePath out of model-visible output for the same reason.
type DocumentSource struct {
	DocumentID string
	Version    int
	Title      string
	Format     domain.DocumentFormat
	Content    string
	Bytes      int64
}

// SourceVersion is one version's stored bytes together with the format the file itself
// says it is. The format travels with the bytes because the document row cannot supply it
// honestly: during a staged or failed update the row's Format describes the newest upload,
// while the version still answering - and the file being read - is the older one. v2 reads
// the type off the resolved path for the same reason (source_reader.go:74-78).
type SourceVersion struct {
	Bytes  []byte
	Format domain.DocumentFormat
}

// SourceStorage keeps one immutable byte copy per document version. The owner travels with
// every call because ownership is decided before a path is ever built: a storage that
// took only a document id could read another user's file if ids were guessable.
type SourceStorage interface {
	SaveTemp(ctx context.Context, userID, documentID string, content []byte) (StoredSource, error)
	DiscardTemp(ctx context.Context, userID, documentID, tempPath string) error
	Publish(ctx context.Context, userID, documentID string, version int, format domain.DocumentFormat, tempPath string) (string, error)
	// Delete removes the one file at relativePath - the path Publish returned. Removal
	// that has to undo a specific publish cannot be done by version, because two requests
	// can name the same version with different formats: deleting "the file for version N"
	// is either blocked by the ambiguity or destroys the file the winning request committed.
	Delete(ctx context.Context, userID, documentID, relativePath string) error
	ReadVersion(ctx context.Context, userID, documentID string, version int) (SourceVersion, error)
	SourceCleaner
}

// SourceReader is the seam a caller uses to quote a document's source. It is separate from
// SourceStorage because it carries the two checks the storage cannot do on its own: whose
// document this is, and which version is allowed to answer.
type SourceReader interface {
	ReadActiveSource(ctx context.Context, userID, documentID string, version int) (DocumentSource, error)
}

// LocalSourceStorage stores sources as files under one root, laid out
// <root>/<userID>/<documentID>/source-v<version>.<extension> - v2's layout, with the
// version in the name so the version itself is the addressing key. v2 persisted two path
// columns (storage_path and cleanup_storage_path) to know which file held which version;
// deriving it from the version the row already carries removes the chance for a row to
// point at another version's bytes, and needs no schema change.
type LocalSourceStorage struct {
	root     string
	maxBytes int64
}

var (
	_ SourceStorage = (*LocalSourceStorage)(nil)
	_ SourceCleaner = (*LocalSourceStorage)(nil)
)

// NewLocalSourceStorage prepares a storage rooted at one directory. The root is resolved
// eagerly: a relative root means every later path check is being done against whatever
// the process's working directory happens to be, which is not a boundary at all.
func NewLocalSourceStorage(root string, maxBytes int64) (*LocalSourceStorage, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("knowledge: source storage root is empty")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("knowledge: resolve source storage root: %w", err)
	}
	if maxBytes <= 0 {
		maxBytes = DefaultSourceMaxBytes
	}
	return &LocalSourceStorage{root: filepath.Clean(absolute), maxBytes: maxBytes}, nil
}

// Root reports the resolved directory the storage will never write outside of.
func (s *LocalSourceStorage) Root() string { return s.root }

func (s *LocalSourceStorage) SaveTemp(ctx context.Context, userID, documentID string, content []byte) (StoredSource, error) {
	if err := checkContext(ctx); err != nil {
		return StoredSource{}, err
	}
	if int64(len(content)) > s.maxBytes {
		return StoredSource{}, ErrSourceTooLarge
	}
	if len(content) == 0 {
		return StoredSource{}, repository.ErrInvalidArgument
	}
	directory, err := s.documentDir(userID, documentID)
	if err != nil {
		return StoredSource{}, err
	}
	if err := os.MkdirAll(directory, sourceDirMode); err != nil {
		return StoredSource{}, fmt.Errorf("knowledge: create document source directory: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return StoredSource{}, err
	}
	temp, err := os.CreateTemp(directory, sourceTempPrefix+"*")
	if err != nil {
		return StoredSource{}, fmt.Errorf("knowledge: create temporary source: %w", err)
	}
	tempPath := temp.Name()
	committed := false
	defer func() {
		_ = temp.Close()
		if !committed {
			_ = os.Remove(tempPath)
		}
	}()
	if _, err := temp.Write(content); err != nil {
		return StoredSource{}, fmt.Errorf("knowledge: write temporary source: %w", err)
	}
	// Sync before the close: a temp file that only reached the OS cache is not something
	// a later Publish may rely on being readable after a crash.
	if err := temp.Sync(); err != nil {
		return StoredSource{}, fmt.Errorf("knowledge: sync temporary source: %w", err)
	}
	if err := temp.Close(); err != nil {
		return StoredSource{}, fmt.Errorf("knowledge: close temporary source: %w", err)
	}
	committed = true
	return StoredSource{
		TempPath:    tempPath,
		ContentHash: document.ContentHash(content),
		FileSize:    int64(len(content)),
	}, nil
}

func (s *LocalSourceStorage) DiscardTemp(ctx context.Context, userID, documentID, tempPath string) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	directory, err := s.documentDir(userID, documentID)
	if err != nil {
		return err
	}
	if !pathWithin(directory, tempPath) {
		return ErrUnsafeSourcePath
	}
	if err := os.Remove(tempPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Publish turns the temporary upload into the immutable file for one version.
func (s *LocalSourceStorage) Publish(ctx context.Context, userID, documentID string, version int,
	format domain.DocumentFormat, tempPath string) (string, error) {
	if err := checkContext(ctx); err != nil {
		return "", err
	}
	if version <= 0 {
		return "", errors.New("knowledge: document version must be greater than zero")
	}
	extension, err := sourceExtension(format)
	if err != nil {
		return "", err
	}
	directory, err := s.documentDir(userID, documentID)
	if err != nil {
		return "", err
	}
	// The temp path has to be inside this document's own directory. Every caller up to
	// here is internal, but a Publish that accepted any path would turn a stale or forged
	// temp name into a read of somebody else's upload.
	if !pathWithin(directory, tempPath) {
		return "", ErrUnsafeSourcePath
	}
	target := filepath.Join(directory, fmt.Sprintf("%s%d%s", sourceFilePrefix, version, extension))
	// Link is atomic and refuses an existing target. Stat-then-Rename has a window in
	// which two concurrent rewrites both see "no file" and the second replaces the
	// first - and then the row's hash describes bytes nobody uploaded.
	if err := os.Link(tempPath, target); err != nil {
		if errors.Is(err, os.ErrExist) || s.versionFileExists(directory, version) {
			return "", fmt.Errorf("%w: version %d of %s", ErrSourceVersionTaken, version, documentID)
		}
		return "", fmt.Errorf("knowledge: publish document source: %w", err)
	}
	// The temp link is dropped, not required to vanish: an unread leftover is harmless
	// and the version file is already a complete, synced inode. Refusing to publish
	// because cleanup of a scratch name failed would be a worse failure than the litter.
	_ = os.Remove(tempPath)
	relative, err := filepath.Rel(s.root, target)
	if err != nil || strings.HasPrefix(relative, "..") {
		return "", ErrUnsafeSourcePath
	}
	return filepath.ToSlash(relative), nil
}

// ReadVersion returns one version's raw bytes and the format its file carries. The bound
// is enforced here rather than left to callers: "read the source" without a limit is a
// request that can be answered with an unbounded allocation.
func (s *LocalSourceStorage) ReadVersion(ctx context.Context, userID, documentID string, version int) (SourceVersion, error) {
	if err := checkContext(ctx); err != nil {
		return SourceVersion{}, err
	}
	path, format, err := s.versionFile(userID, documentID, version)
	if err != nil {
		return SourceVersion{}, err
	}
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return SourceVersion{}, fmt.Errorf("%w: version %d of %s", ErrSourceNotStored, version, documentID)
		}
		return SourceVersion{}, fmt.Errorf("knowledge: open document source: %w", err)
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, s.maxBytes+1))
	if err != nil {
		return SourceVersion{}, fmt.Errorf("knowledge: read document source: %w", err)
	}
	if int64(len(content)) > s.maxBytes {
		return SourceVersion{}, ErrSourceTooLarge
	}
	if err := ctx.Err(); err != nil {
		return SourceVersion{}, err
	}
	return SourceVersion{Bytes: content, Format: format}, nil
}

// DropVersion removes one version's file. Something that is already gone counts as
// removed: the recorded work is to make sure no version's bytes outlive its row, and a
// retried cleanup must not fail because the first attempt finished.
func (s *LocalSourceStorage) DropVersion(ctx context.Context, userID, documentID string, version int) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	if version <= 0 {
		return nil
	}
	path, _, err := s.versionFile(userID, documentID, version)
	if err != nil {
		if errors.Is(err, ErrSourceNotStored) {
			return nil
		}
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// DropDocument removes the whole directory: every version the document ever had.
func (s *LocalSourceStorage) DropDocument(ctx context.Context, userID, documentID string) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	directory, err := s.documentDir(userID, documentID)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(directory); err != nil {
		return fmt.Errorf("knowledge: remove document sources: %w", err)
	}
	return nil
}

// Delete removes one published file, addressed by the relative path Publish returned.
// Something already gone counts as removed - an undo that is retried after it landed is
// not a failure.
func (s *LocalSourceStorage) Delete(ctx context.Context, userID, documentID, relativePath string) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	path, err := s.resolveRelative(userID, documentID, relativePath)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// resolveRelative is v2's resolveDocumentRelative: a stored path is relative, is inside
// this document's own directory, and names a file. The last part matters more than it
// looks - without it a caller could hand in the document directory itself and have a
// "remove one file" delete a directory.
func (s *LocalSourceStorage) resolveRelative(userID, documentID, relativePath string) (string, error) {
	if relativePath == "" || filepath.IsAbs(relativePath) {
		return "", ErrUnsafeSourcePath
	}
	directory, err := s.documentDir(userID, documentID)
	if err != nil {
		return "", err
	}
	path := filepath.Join(s.root, filepath.FromSlash(relativePath))
	if !pathWithin(directory, path) {
		return "", ErrUnsafeSourcePath
	}
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return path, nil
		}
		return "", err
	}
	if info.IsDir() {
		return "", ErrUnsafeSourcePath
	}
	return path, nil
}

func (s *LocalSourceStorage) documentDir(userID, documentID string) (string, error) {
	if !safePathSegment.MatchString(userID) || !safePathSegment.MatchString(documentID) {
		return "", ErrUnsafeSourcePath
	}
	directory := filepath.Join(s.root, userID, documentID)
	if !pathWithin(s.root, directory) {
		return "", ErrUnsafeSourcePath
	}
	return directory, nil
}

// versionFile resolves a version to its file by name prefix, and reports the format the
// file's own extension says it is. Two files for one version is not ambiguity to pick
// through - it means an immutable version was written twice, so whichever is read may not
// be the one the hash describes.
func (s *LocalSourceStorage) versionFile(userID, documentID string, version int) (string, domain.DocumentFormat, error) {
	directory, err := s.documentDir(userID, documentID)
	if err != nil {
		return "", "", err
	}
	prefix := fmt.Sprintf("%s%d.", sourceFilePrefix, version)
	var found []string
	var format domain.DocumentFormat
	entries, err := os.ReadDir(directory)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", "", fmt.Errorf("%w: version %d of %s", ErrSourceNotStored, version, documentID)
		}
		return "", "", fmt.Errorf("knowledge: list document sources: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		switch strings.ToLower(strings.TrimPrefix(name[len(prefix):], ".")) {
		case "md":
			format = domain.DocumentFormatMarkdown
		case "txt":
			format = domain.DocumentFormatText
		default:
			return "", "", fmt.Errorf("%w: %s", ErrUnsupportedSource, name)
		}
		found = append(found, filepath.Join(directory, name))
	}
	switch len(found) {
	case 0:
		return "", "", fmt.Errorf("%w: version %d of %s", ErrSourceNotStored, version, documentID)
	case 1:
		return found[0], format, nil
	default:
		return "", "", fmt.Errorf("%w: %d files for version %d of %s", ErrSourceInvalid, len(found), version, documentID)
	}
}

func (s *LocalSourceStorage) versionFileExists(directory string, version int) bool {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return false
	}
	prefix := fmt.Sprintf("%s%d.", sourceFilePrefix, version)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), prefix) {
			return true
		}
	}
	return false
}

func sourceExtension(format domain.DocumentFormat) (string, error) {
	switch format {
	case domain.DocumentFormatMarkdown:
		return ".md", nil
	case domain.DocumentFormatText:
		return ".txt", nil
	default:
		return "", ErrUnsupportedSource
	}
}

// NewDocumentID mints a document id for an ingest that stores its source.
//
// The id has to exist before the row does, because the file is addressed by it: v2's
// sequence is SaveTemp -> Publish -> Stage, and a stage that produced a pending version
// first would be a version something could index before its bytes were readable. The
// stores keep minting ids themselves for every caller that does not need that ordering,
// and both honour a caller-supplied id - refusing one that already exists rather than
// overwriting it, which is what makes a random id safe here rather than merely convenient.
func NewDocumentID() (string, error) {
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("knowledge: mint document id: %w", err)
	}
	// The shape matches the stores' own ids and satisfies safePathSegment, so an id is
	// usable as a path component without a second, storage-specific namespace.
	return fmt.Sprintf("document-%x", raw), nil
}

// pathWithin is v2's containment test, kept as a helper rather than inlined because three
// call sites need the same answer and only one of them can afford to be wrong.
func pathWithin(root, candidate string) bool {
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	absoluteCandidate, err := filepath.Abs(candidate)
	if err != nil {
		return false
	}
	relative, err := filepath.Rel(absoluteRoot, absoluteCandidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

// localSourceReader answers source reads over a Store and a SourceStorage. It is the
// migration of v2's ReadActiveSource, and the two checks it adds are the reason the
// storage alone is not enough: a document row is what says who owns the bytes and which
// version is allowed to answer.
type localSourceReader struct {
	store   Store
	storage SourceStorage
}

var _ SourceReader = (*localSourceReader)(nil)

// NewSourceReader wires read-back to an existing knowledge store. A deployment that
// stores no sources passes no reader at all: the absence is then reported at the door,
// not discovered as an empty result later.
func NewSourceReader(store Store, storage SourceStorage) (SourceReader, error) {
	if store == nil || storage == nil {
		return nil, errors.New("knowledge: a source reader needs a store and a source storage")
	}
	return &localSourceReader{store: store, storage: storage}, nil
}

// ReadActiveSource returns the source bytes of the version the caller asked for, and
// refuses anything else. version is checked against the version that answers queries
// rather than the version that was last staged: during a staged update - and during a
// failed one, which is the interesting case - those are different numbers, and the older
// one is the text retrieval is currently quoting.
//
// The metadata check runs only when the requested version is also the document's current
// one. The row's hash and size describe the newest upload; comparing them against an
// older version's file would report a healthy document as corrupt.
func (r *localSourceReader) ReadActiveSource(ctx context.Context, userID, documentID string, version int) (DocumentSource, error) {
	if err := checkContext(ctx); err != nil {
		return DocumentSource{}, err
	}
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(documentID) == "" || version <= 0 {
		return DocumentSource{}, repository.ErrInvalidArgument
	}
	record, err := r.store.GetDocument(ctx, userID, strings.TrimSpace(documentID))
	if err != nil {
		return DocumentSource{}, err
	}
	if !record.Retrievable() || record.ServingVersion() != version {
		return DocumentSource{}, fmt.Errorf("%w: asked %d, serving %d",
			ErrSourceVersionNotRead, version, record.ServingVersion())
	}
	stored, err := r.storage.ReadVersion(ctx, userID, record.ID, version)
	if err != nil {
		return DocumentSource{}, err
	}
	content := stored.Bytes
	if version == record.Version && record.FileSize > 0 && int64(len(content)) != record.FileSize {
		return DocumentSource{}, fmt.Errorf("%w: %s v%d is %d bytes, the row says %d",
			ErrSourceInvalid, record.ID, version, len(content), record.FileSize)
	}
	if version == record.Version && record.ContentHash != "" && document.ContentHash(content) != record.ContentHash {
		return DocumentSource{}, fmt.Errorf("%w: %s v%d does not hash to the recorded digest",
			ErrSourceInvalid, record.ID, version)
	}
	return DocumentSource{
		DocumentID: record.ID,
		Version:    version,
		Title:      record.Title,
		// The format comes from the file being read, not from the row: a rewrite that
		// changed markdown to text has already overwritten record.Format, while the
		// version still answering - and quoted - is the markdown one.
		Format:  stored.Format,
		Content: string(document.NormalizeContent(content)),
		Bytes:   int64(len(content)),
	}, nil
}
