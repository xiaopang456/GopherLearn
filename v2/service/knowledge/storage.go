package knowledge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var (
	ErrUnsafePath      = errors.New("unsafe knowledge storage path")
	ErrUnsupportedType = errors.New("unsupported knowledge file type")
	safeDocumentID     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,63}$`)
)

type StoredFile struct {
	TempPath    string
	ContentHash string
	FileSize    int64
}

type FileStorage interface {
	SaveTemp(context.Context, int64, string, io.Reader) (StoredFile, error)
	DiscardTemp(string, int64, string) error
	Publish(string, int64, string, int, string) (string, error)
	Open(int64, string, string) (io.ReadCloser, error)
	Delete(int64, string, string) error
	DeleteDocument(int64, string) error
}

func (s *LocalFileStorage) DiscardTemp(tempPath string, userID int64, documentID string) error {
	directory, err := s.documentDir(userID, documentID)
	if err != nil {
		return err
	}
	if !pathWithin(directory, tempPath) {
		return ErrUnsafePath
	}
	if err := os.Remove(tempPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

type LocalFileStorage struct {
	root string
}

func NewLocalFileStorage(root string) (*LocalFileStorage, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("knowledge storage root is empty")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve knowledge storage root: %w", err)
	}
	return &LocalFileStorage{root: filepath.Clean(absolute)}, nil
}

func NormalizeContent(content []byte) []byte {
	content = bytes.TrimPrefix(content, []byte{0xEF, 0xBB, 0xBF})
	content = bytes.ReplaceAll(content, []byte("\r\n"), []byte("\n"))
	return bytes.ReplaceAll(content, []byte("\r"), []byte("\n"))
}

func ContentHash(content []byte) string {
	sum := sha256.Sum256(NormalizeContent(content))
	return hex.EncodeToString(sum[:])
}

func (s *LocalFileStorage) SaveTemp(ctx context.Context, userID int64, documentID string, source io.Reader) (StoredFile, error) {
	directory, err := s.documentDir(userID, documentID)
	if err != nil {
		return StoredFile{}, err
	}
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return StoredFile{}, fmt.Errorf("create document directory: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return StoredFile{}, err
	}
	content, err := io.ReadAll(source)
	if err != nil {
		return StoredFile{}, fmt.Errorf("read knowledge source: %w", err)
	}
	temp, err := os.CreateTemp(directory, ".upload-*")
	if err != nil {
		return StoredFile{}, fmt.Errorf("create temporary knowledge file: %w", err)
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
		return StoredFile{}, fmt.Errorf("write temporary knowledge file: %w", err)
	}
	if err := temp.Sync(); err != nil {
		return StoredFile{}, fmt.Errorf("sync temporary knowledge file: %w", err)
	}
	if err := temp.Close(); err != nil {
		return StoredFile{}, fmt.Errorf("close temporary knowledge file: %w", err)
	}
	committed = true
	return StoredFile{TempPath: tempPath, ContentHash: ContentHash(content), FileSize: int64(len(content))}, nil
}

func (s *LocalFileStorage) Publish(tempPath string, userID int64, documentID string, version int, fileType string) (string, error) {
	if version <= 0 {
		return "", errors.New("document version must be greater than zero")
	}
	extension, err := fileExtension(fileType)
	if err != nil {
		return "", err
	}
	directory, err := s.documentDir(userID, documentID)
	if err != nil {
		return "", err
	}
	if !pathWithin(directory, tempPath) {
		return "", ErrUnsafePath
	}
	target := filepath.Join(directory, fmt.Sprintf("source-v%d%s", version, extension))
	// Link is atomic and refuses an existing target. A separate Stat followed
	// by Rename has a TOCTOU window where concurrent updates can overwrite the
	// same immutable version file (Rename replaces targets on Unix).
	if err := os.Link(tempPath, target); err != nil {
		if errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("published knowledge file already exists: version %d", version)
		}
		return "", fmt.Errorf("publish knowledge file: %w", err)
	}
	// The target is already a complete, fsynced inode. Failure to remove the
	// temporary hard link must not turn a successful publish into an orphaned
	// version; stale .upload files are harmless and never referenced by MySQL.
	_ = os.Remove(tempPath)
	relative, err := filepath.Rel(s.root, target)
	if err != nil || strings.HasPrefix(relative, "..") {
		return "", ErrUnsafePath
	}
	return filepath.ToSlash(relative), nil
}

func (s *LocalFileStorage) Open(userID int64, documentID, relativePath string) (io.ReadCloser, error) {
	path, err := s.resolveDocumentRelative(userID, documentID, relativePath)
	if err != nil {
		return nil, err
	}
	return os.Open(path)
}

func (s *LocalFileStorage) Delete(userID int64, documentID, relativePath string) error {
	path, err := s.resolveDocumentRelative(userID, documentID, relativePath)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (s *LocalFileStorage) DeleteDocument(userID int64, documentID string) error {
	directory, err := s.documentDir(userID, documentID)
	if err != nil {
		return err
	}
	return os.RemoveAll(directory)
}

// DeleteSupersededSources removes old files in one document's directory while preserving its
// current storage path. The caller supplies a cutoff so a just-published upload cannot be removed
// before StageUpdate has recorded it in MySQL.
func (s *LocalFileStorage) DeleteSupersededSources(ctx context.Context, userID int64, documentID, currentPath string, before time.Time) (removed int, deferred bool, err error) {
	directory, err := s.documentDir(userID, documentID)
	if err != nil {
		return 0, false, err
	}
	currentFile, err := s.resolveDocumentRelative(userID, documentID, currentPath)
	if err != nil {
		return 0, false, err
	}
	currentName := filepath.Base(currentFile)
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read document source directory: %w", err)
	}
	for _, entry := range entries {
		if err := contextError(ctx); err != nil {
			return removed, deferred, err
		}
		if entry.Name() == currentName || entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return removed, deferred, fmt.Errorf("inspect document source file: %w", err)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		if !info.ModTime().Before(before) {
			deferred = true
			continue
		}
		if err := os.Remove(filepath.Join(directory, entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			return removed, deferred, fmt.Errorf("remove superseded document source: %w", err)
		} else if err == nil {
			removed++
		}
	}
	return removed, deferred, nil
}

func (s *LocalFileStorage) documentDir(userID int64, documentID string) (string, error) {
	if userID <= 0 || !safeDocumentID.MatchString(documentID) {
		return "", ErrUnsafePath
	}
	directory := filepath.Join(s.root, fmt.Sprintf("%d", userID), documentID)
	if !pathWithin(s.root, directory) {
		return "", ErrUnsafePath
	}
	return directory, nil
}

func (s *LocalFileStorage) resolveDocumentRelative(userID int64, documentID, relativePath string) (string, error) {
	if relativePath == "" || filepath.IsAbs(relativePath) {
		return "", ErrUnsafePath
	}
	directory, err := s.documentDir(userID, documentID)
	if err != nil {
		return "", err
	}
	path := filepath.Join(s.root, filepath.FromSlash(relativePath))
	if !pathWithin(directory, path) {
		return "", ErrUnsafePath
	}
	return path, nil
}

func pathWithin(root, candidate string) bool {
	root, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	candidate, err = filepath.Abs(candidate)
	if err != nil {
		return false
	}
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func fileExtension(fileType string) (string, error) {
	switch strings.ToLower(strings.TrimPrefix(fileType, ".")) {
	case "md":
		return ".md", nil
	case "txt":
		return ".txt", nil
	default:
		return "", ErrUnsupportedType
	}
}
