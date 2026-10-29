package knowledge

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLocalFileStorageSavePublishOpenDelete(t *testing.T) {
	storage, err := NewLocalFileStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stored, err := storage.SaveTemp(context.Background(), 73, "doc-123", strings.NewReader("# Gin\r\n\r\n内容\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if stored.FileSize != int64(len("# Gin\r\n\r\n内容\r\n")) {
		t.Fatalf("stored size = %d", stored.FileSize)
	}
	relativePath, err := storage.Publish(stored.TempPath, 73, "doc-123", 1, "md")
	if err != nil {
		t.Fatal(err)
	}
	if relativePath != "73/doc-123/source-v1.md" {
		t.Fatalf("relative path = %q", relativePath)
	}
	file, err := storage.Open(73, "doc-123", relativePath)
	if err != nil {
		t.Fatal(err)
	}
	content, err := io.ReadAll(file)
	_ = file.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "# Gin\r\n\r\n内容\r\n" {
		t.Fatalf("content = %q", content)
	}
	if _, err := storage.Open(74, "doc-123", relativePath); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("cross-user open error = %v", err)
	}
	if err := storage.Delete(73, "doc-123", relativePath); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.Open(73, "doc-123", relativePath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("open after delete error = %v", err)
	}
}

func TestLocalFileStorageDeleteSupersededSourcesHonorsCurrentPathAndGrace(t *testing.T) {
	storage, err := NewLocalFileStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	paths := make([]string, 0, 3)
	for version, content := range []string{"v1", "v2", "upload"} {
		stored, saveErr := storage.SaveTemp(context.Background(), 73, "doc-clean", strings.NewReader(content))
		if saveErr != nil {
			t.Fatal(saveErr)
		}
		if version < 2 {
			path, publishErr := storage.Publish(stored.TempPath, 73, "doc-clean", version+1, "md")
			if publishErr != nil {
				t.Fatal(publishErr)
			}
			paths = append(paths, path)
			continue
		}
		// Keep the young, not-yet-referenced upload in the same document directory.
		paths = append(paths, filepath.ToSlash(filepath.Join("73", "doc-clean", "upload-pending.md")))
		if err = os.Rename(stored.TempPath, filepath.Join(filepath.Dir(stored.TempPath), "upload-pending.md")); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-20 * time.Minute)
	for _, path := range paths[:1] {
		absolute := filepath.Join(storage.root, filepath.FromSlash(path))
		if err = os.Chtimes(absolute, old, old); err != nil {
			t.Fatal(err)
		}
	}
	removed, deferred, err := storage.DeleteSupersededSources(context.Background(), 73, "doc-clean", paths[1], time.Now().Add(-10*time.Minute))
	if err != nil || removed != 1 || !deferred {
		t.Fatalf("removed=%d deferred=%t err=%v, want the old source removed and upload deferred", removed, deferred, err)
	}
	if _, err = os.Stat(filepath.Join(storage.root, filepath.FromSlash(paths[1]))); err != nil {
		t.Fatalf("current source was removed: %v", err)
	}
	if _, err = os.Stat(filepath.Join(storage.root, filepath.FromSlash(paths[2]))); err != nil {
		t.Fatalf("young pending upload was removed: %v", err)
	}
}

func TestLocalFileStorageConcurrentPublishNeverOverwritesVersion(t *testing.T) {
	storage, err := NewLocalFileStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	temps := make([]StoredFile, 2)
	for index, content := range []string{"first", "second"} {
		temps[index], err = storage.SaveTemp(context.Background(), 73, "doc-race", strings.NewReader(content))
		if err != nil {
			t.Fatal(err)
		}
	}
	var wait sync.WaitGroup
	results := make(chan error, 2)
	for index := range temps {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, publishErr := storage.Publish(temps[index].TempPath, 73, "doc-race", 2, "md")
			results <- publishErr
		}()
	}
	wait.Wait()
	close(results)
	successes := 0
	for publishErr := range results {
		if publishErr == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("successful immutable publishes = %d, want 1", successes)
	}
	file, err := storage.Open(73, "doc-race", "73/doc-race/source-v2.md")
	if err != nil {
		t.Fatal(err)
	}
	content, _ := io.ReadAll(file)
	_ = file.Close()
	if string(content) != "first" && string(content) != "second" {
		t.Fatalf("published content was corrupted: %q", content)
	}
}

func TestContentHashNormalizesBOMAndLineEndings(t *testing.T) {
	windows := []byte{0xEF, 0xBB, 0xBF, 'a', '\r', '\n', 'b', '\r'}
	unix := []byte("a\nb\n")
	if ContentHash(windows) != ContentHash(unix) {
		t.Fatal("equivalent text must have the same normalized hash")
	}
}

func TestLocalFileStorageRejectsTraversalAndUnsafeIDs(t *testing.T) {
	storage, err := NewLocalFileStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.SaveTemp(context.Background(), 1, "../other", strings.NewReader("x")); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("unsafe document ID error = %v", err)
	}
	if _, err := storage.Open(1, "safe-id", "../secret.txt"); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("path traversal error = %v", err)
	}
	if _, err := storage.Publish("outside", 1, "safe-id", 1, "pdf"); !errors.Is(err, ErrUnsupportedType) {
		t.Fatalf("unsupported type error = %v", err)
	}
}

func TestLocalFileStorageCleansTemporaryFileOnReadFailure(t *testing.T) {
	root := t.TempDir()
	storage, err := NewLocalFileStorage(root)
	if err != nil {
		t.Fatal(err)
	}
	failing := io.MultiReader(strings.NewReader("partial"), errorReader{})
	if _, err := storage.SaveTemp(context.Background(), 1, "doc-1", failing); err == nil {
		t.Fatal("expected source read failure")
	}
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) {
	return 0, errors.New("read failed")
}
