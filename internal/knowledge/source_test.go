package knowledge

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/document"
	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/repository"
)

// sourceFixture puts the storage under a test-scoped directory with a small read bound,
// so "bounded" is something a test can actually reach rather than a number printed in a
// comment.
func sourceFixture(t *testing.T, maxBytes int64) *LocalSourceStorage {
	t.Helper()
	storage, err := NewLocalSourceStorage(filepath.Join(t.TempDir(), "knowledge-sources"), maxBytes)
	if err != nil {
		t.Fatalf("NewLocalSourceStorage: %v", err)
	}
	return storage
}

func mustPublish(t *testing.T, storage *LocalSourceStorage, userID, documentID string, version int,
	format domain.DocumentFormat, content []byte) string {
	t.Helper()
	stored, err := storage.SaveTemp(context.Background(), userID, documentID, content)
	if err != nil {
		t.Fatalf("SaveTemp: %v", err)
	}
	path, err := storage.Publish(context.Background(), userID, documentID, version, format, stored.TempPath)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	return path
}

// TestSourceStoresRawBytesAndReportsNormalizedHash is the pair of numbers the whole design
// rests on. The file holds what the client sent; the hash describes what the text says
// after the BOM and the newline style are dropped; the size is the raw length the quota
// spends. Verifying raw bytes against a normalized hash is what would make every Windows
// upload look corrupt, so the two are asserted apart.
func TestSourceStoresRawBytesAndReportsNormalizedHash(t *testing.T) {
	storage := sourceFixture(t, 1<<20)
	ctx := context.Background()
	raw := []byte("\xEF\xBB\xBF# Redis\r\n\r\nAOF everysec 是默认策略。\r\n第二行\r")

	stored, err := storage.SaveTemp(ctx, "user-1", "document-source-1", raw)
	if err != nil {
		t.Fatalf("SaveTemp: %v", err)
	}
	if stored.FileSize != int64(len(raw)) {
		t.Fatalf("reported size %d, want the raw length %d", stored.FileSize, len(raw))
	}
	if stored.ContentHash != document.ContentHash(raw) {
		t.Fatalf("reported hash %q, want the normalized digest", stored.ContentHash)
	}
	// A CRLF copy and an LF copy of one note hash alike but differ in length: that is
	// exactly the difference the dedup rule and the quota each depend on.
	lf := []byte(document.NormalizeText(string(raw)))
	if document.ContentHash(lf) != stored.ContentHash {
		t.Fatal("the hash is not over the normalized form")
	}
	if int64(len(lf)) == stored.FileSize {
		t.Fatal("the size is not the raw byte count any more")
	}

	path, err := storage.Publish(ctx, "user-1", "document-source-1", 1, domain.DocumentFormatMarkdown, stored.TempPath)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if filepath.ToSlash(path) != "user-1/document-source-1/source-v1.md" {
		t.Fatalf("published path = %q, want the version-addressed layout", path)
	}
	read, err := storage.ReadVersion(ctx, "user-1", "document-source-1", 1)
	if err != nil {
		t.Fatal(err)
	}
	if read.Format != domain.DocumentFormatMarkdown {
		t.Fatalf("read format = %q, want the file's own extension to answer", read.Format)
	}
	if string(read.Bytes) != string(raw) {
		t.Fatalf("stored bytes were normalized on the way in: %q", read.Bytes)
	}
	if string(document.NormalizeContent(read.Bytes)) != "# Redis\n\nAOF everysec 是默认策略。\n第二行\n" {
		t.Fatalf("normalized form = %q", document.NormalizeContent(read.Bytes))
	}
}

// TestPublishRefusesTheSameVersionTwice is the immutability rule the hash check depends
// on. The first upload's bytes have to survive the second attempt, because the row that
// was already written describes them.
func TestPublishRefusesTheSameVersionTwice(t *testing.T) {
	storage := sourceFixture(t, 1<<20)
	ctx := context.Background()
	first := mustPublish(t, storage, "user-1", "document-twice", 1, domain.DocumentFormatMarkdown, []byte("第一份原文"))
	if filepath.ToSlash(first) != "user-1/document-twice/source-v1.md" {
		t.Fatalf("first publish returned %q, want the version file name", first)
	}

	stored, err := storage.SaveTemp(ctx, "user-1", "document-twice", []byte("第二份原文，不该覆盖任何东西"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.Publish(ctx, "user-1", "document-twice", 1, domain.DocumentFormatMarkdown, stored.TempPath); !errors.Is(err, ErrSourceVersionTaken) {
		t.Fatalf("second publish of version 1 = %v, want it refused", err)
	}
	if err := storage.DiscardTemp(ctx, "user-1", "document-twice", stored.TempPath); err != nil {
		t.Fatalf("DiscardTemp: %v", err)
	}
	read, err := storage.ReadVersion(ctx, "user-1", "document-twice", 1)
	if err != nil {
		t.Fatal(err)
	}
	if string(read.Bytes) != "第一份原文" {
		t.Fatalf("version 1 now holds %q, want the bytes that were published first", read.Bytes)
	}
	// The rejected attempt left no second file behind, so the version still resolves to
	// exactly one file rather than an ambiguous pair.
	entries, err := os.ReadDir(filepath.Join(storage.root, "user-1", "document-twice"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("directory holds %d entries, want the one version file", len(entries))
	}
	if _, err := storage.Publish(ctx, "user-1", "document-twice", 0, domain.DocumentFormatMarkdown, stored.TempPath); err == nil {
		t.Fatal("version 0 was published")
	}
}

func TestSourcePathsRefuseAnythingThatCouldLeaveTheRoot(t *testing.T) {
	storage := sourceFixture(t, 1<<20)
	ctx := context.Background()
	cases := []struct {
		name       string
		userID     string
		documentID string
	}{
		{"document escape", "user-1", "../other"},
		{"nested escape", "user-1", "../../outside"},
		{"absolute document", "user-1", "/etc/passwd"},
		{"user escape", "../attacker", "document-safe"},
		{"empty user", "", "document-safe"},
		{"separator in id", "user-1", "document/1"},
		{"dot only", "user-1", "."},
		{"leading dash", "user-1", "-document"},
		{"too long", "user-1", strings.Repeat("d", 70)},
		{"underscore", "user-1", "document_1"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := storage.SaveTemp(ctx, testCase.userID, testCase.documentID, []byte("内容")); !errors.Is(err, ErrUnsafeSourcePath) {
				t.Fatalf("SaveTemp(%q,%q) = %v, want the path refused", testCase.userID, testCase.documentID, err)
			}
			if _, err := storage.ReadVersion(ctx, testCase.userID, testCase.documentID, 1); !errors.Is(err, ErrUnsafeSourcePath) {
				t.Fatalf("ReadVersion(%q,%q) = %v, want the path refused", testCase.userID, testCase.documentID, err)
			}
			if err := storage.DropVersion(ctx, testCase.userID, testCase.documentID, 1); !errors.Is(err, ErrUnsafeSourcePath) {
				t.Fatalf("DropVersion(%q,%q) = %v, want the path refused", testCase.userID, testCase.documentID, err)
			}
		})
	}
	// A temp path from another document's directory is not this document's to publish.
	directory := filepath.Join(storage.root, "user-1", "document-a")
	if err := os.MkdirAll(directory, 0o750); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(storage.root, "user-2", "document-b", ".upload-x")
	if err := os.MkdirAll(filepath.Dir(foreign), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(foreign, []byte("别人的上传"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.Publish(ctx, "user-1", "document-a", 1, domain.DocumentFormatMarkdown, foreign); !errors.Is(err, ErrUnsafeSourcePath) {
		t.Fatalf("publish of a foreign temp = %v, want it refused", err)
	}
	if err := storage.DiscardTemp(ctx, "user-1", "document-a", foreign); !errors.Is(err, ErrUnsafeSourcePath) {
		t.Fatalf("discard of a foreign temp = %v, want it refused", err)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Fatal("the refused operation removed somebody else's file")
	}
}

// TestDeleteRemovesOneNamedFile is the precision the compensation needs. Two requests can
// name the same version with different formats, so "the file for version N" is not a thing
// that exists; removing the wrong one of them destroys a source another request committed.
func TestDeleteRemovesOneNamedFile(t *testing.T) {
	storage := sourceFixture(t, 1<<20)
	ctx := context.Background()
	md := mustPublish(t, storage, "user-1", "document-twin", 2, domain.DocumentFormatMarkdown, []byte("markdown 的那一份"))
	txt := mustPublish(t, storage, "user-1", "document-twin", 2, domain.DocumentFormatText, []byte("纯文本的那一份"))
	if md == txt {
		t.Fatal("the two formats published the same path")
	}
	// Both exist, and reading the version is now ambiguous by design.
	if _, err := storage.ReadVersion(ctx, "user-1", "document-twin", 2); !errors.Is(err, ErrSourceInvalid) {
		t.Fatalf("read of an ambiguous version = %v, want it refused rather than guessed", err)
	}
	if err := storage.Delete(ctx, "user-1", "document-twin", txt); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	remaining, err := storage.ReadVersion(ctx, "user-1", "document-twin", 2)
	if err != nil {
		t.Fatalf("the surviving file became unreadable: %v", err)
	}
	if string(remaining.Bytes) != "markdown 的那一份" || remaining.Format != domain.DocumentFormatMarkdown {
		t.Fatalf("surviving source = %q (%q), want the markdown one left alone", remaining.Bytes, remaining.Format)
	}
	// A retried removal of something already gone is success, and the paths that could
	// reach outside the document are refused without touching anything.
	if err := storage.Delete(ctx, "user-1", "document-twin", txt); err != nil {
		t.Fatalf("retried Delete = %v, want success", err)
	}
	for _, attempt := range []string{"", "user-1/document-twin/../escape.md", "../../elsewhere.md",
		"/absolute/path.md", "user-2/document-twin/source-v2.md", "user-1/document-twin"} {
		if err := storage.Delete(ctx, "user-1", "document-twin", attempt); !errors.Is(err, ErrUnsafeSourcePath) {
			t.Fatalf("Delete(%q) = %v, want the path refused", attempt, err)
		}
	}
	if _, err := storage.ReadVersion(ctx, "user-1", "document-twin", 2); err != nil {
		t.Fatalf("a refused Delete disturbed the stored file: %v", err)
	}
}

func TestSourceReadIsBoundedByTheStorageItself(t *testing.T) {
	const limit = 32
	storage := sourceFixture(t, limit)
	ctx := context.Background()
	big := []byte(strings.Repeat("a", limit+1))
	// The file is placed by hand: a write path that respects the limit could never
	// produce the oversized file the read bound exists for - a file written by an older
	// build, a bigger limit in another deployment, or a directory moved in from outside.
	directory := filepath.Join(storage.root, "user-1", "document-big")
	if err := os.MkdirAll(directory, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "source-v1.md"), big, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.ReadVersion(ctx, "user-1", "document-big", 1); !errors.Is(err, ErrSourceTooLarge) {
		t.Fatalf("read of an oversized file = %v, want it refused before it is buffered", err)
	}
	if _, err := storage.SaveTemp(ctx, "user-1", "document-big", big); !errors.Is(err, ErrSourceTooLarge) {
		t.Fatalf("write of an oversized body = %v, want the same bound on the way in", err)
	}
	if _, err := storage.ReadVersion(ctx, "user-1", "document-big", 7); !errors.Is(err, ErrSourceNotStored) {
		t.Fatalf("read of an absent version = %v, want 'never stored'", err)
	}
}

func TestSourceReadNamesAmbiguityAndWrongTypeAsRefusable(t *testing.T) {
	storage := sourceFixture(t, 1<<20)
	ctx := context.Background()
	mustPublish(t, storage, "user-1", "document-ambiguous", 1, domain.DocumentFormatMarkdown, []byte("第一份"))
	directory := filepath.Join(storage.root, "user-1", "document-ambiguous")
	if err := os.WriteFile(filepath.Join(directory, "source-v1.txt"), []byte("第二份，同名不同扩展名"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.ReadVersion(ctx, "user-1", "document-ambiguous", 1); !errors.Is(err, ErrSourceInvalid) {
		t.Fatalf("read with two files for one version = %v, want it called out, not picked", err)
	}

	other := filepath.Join(storage.root, "user-1", "document-bin")
	if err := os.MkdirAll(other, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "source-v1.bin"), []byte("binary"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.ReadVersion(ctx, "user-1", "document-bin", 1); !errors.Is(err, ErrUnsupportedSource) {
		t.Fatalf("read of an unknown type = %v, want it refused", err)
	}
	stored, err := storage.SaveTemp(ctx, "user-1", "document-format", []byte("内容"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.Publish(ctx, "user-1", "document-format", 1, domain.DocumentFormat("pdf"), stored.TempPath); !errors.Is(err, ErrUnsupportedSource) {
		t.Fatalf("publish of an unsupported format = %v, want it refused", err)
	}
}

func TestSourceDropsFollowTheVersionAndTheDocument(t *testing.T) {
	storage := sourceFixture(t, 1<<20)
	ctx := context.Background()
	for version := 1; version <= 3; version++ {
		mustPublish(t, storage, "user-1", "document-three", version, domain.DocumentFormatText,
			[]byte(strings.Repeat("内容", version)))
	}
	if err := storage.DropVersion(ctx, "user-1", "document-three", 2); err != nil {
		t.Fatalf("DropVersion: %v", err)
	}
	if _, err := storage.ReadVersion(ctx, "user-1", "document-three", 2); !errors.Is(err, ErrSourceNotStored) {
		t.Fatalf("version 2 after its drop = %v", err)
	}
	for _, version := range []int{1, 3} {
		if _, err := storage.ReadVersion(ctx, "user-1", "document-three", version); err != nil {
			t.Fatalf("version %d was collateral damage: %v", version, err)
		}
	}
	// A retried cleanup that finds nothing already did its job.
	if err := storage.DropVersion(ctx, "user-1", "document-three", 2); err != nil {
		t.Fatalf("retried drop of a gone version = %v, want success", err)
	}
	if err := storage.DropVersion(ctx, "user-1", "document-three", 0); err != nil {
		t.Fatalf("drop of version 0 = %v, want a no-op", err)
	}
	if err := storage.DropDocument(ctx, "user-1", "document-three"); err != nil {
		t.Fatalf("DropDocument: %v", err)
	}
	if _, err := os.Stat(filepath.Join(storage.root, "user-1", "document-three")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("document directory after DropDocument: %v", err)
	}
	if err := storage.DropDocument(ctx, "user-1", "document-never-stored"); err != nil {
		t.Fatalf("DropDocument of an unknown document = %v, want success", err)
	}
}

func TestSourceCallsObeyTheirContext(t *testing.T) {
	storage := sourceFixture(t, 1<<20)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := storage.SaveTemp(canceled, "user-1", "document-ctx", []byte("内容")); !errors.Is(err, context.Canceled) {
		t.Fatalf("SaveTemp = %v, want the cancellation", err)
	}
	if _, err := storage.ReadVersion(canceled, "user-1", "document-ctx", 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("ReadVersion = %v, want the cancellation", err)
	}
	if err := storage.DropDocument(canceled, "user-1", "document-ctx"); !errors.Is(err, context.Canceled) {
		t.Fatalf("DropDocument = %v, want the cancellation", err)
	}
	if _, err := NewLocalSourceStorage("  ", 10); err == nil {
		t.Fatal("an empty storage root was accepted")
	}
}

// sourceStoreFixture stages rows without touching the filesystem, so the reader's checks
// can be pointed at exactly the combinations a live deployment produces.
func sourceStoreFixture(t *testing.T) (*MemoryStore, *LocalSourceStorage, string) {
	t.Helper()
	store := NewMemoryStore()
	collection, err := store.CreateCollection(context.Background(), &domain.Collection{UserID: "user-1", Title: "手册"})
	if err != nil {
		t.Fatal(err)
	}
	return store, sourceFixture(t, 1<<20), collection.ID
}

// TestReaderVerifiesOnlyTheVersionTheRowDescribes is the correction this whole section came
// from. During a staged update the document row carries the NEW version's hash and size,
// while the version answering queries - and the file behind it - is still the old one.
// Verifying the old file against the new metadata reports a healthy document as corrupt,
// which is why the check is limited to the version the row describes.
func TestReaderVerifiesOnlyTheVersionTheRowDescribes(t *testing.T) {
	ctx := context.Background()
	store, storage, collectionID := sourceStoreFixture(t)
	old := []byte("# 旧版\r\n\r\n仍在回答的内容\r\n")
	staged, err := store.StageDocument(ctx, "user-1", &domain.Document{
		ID: "document-reader", CollectionID: collectionID, Title: "reader.md",
		Format:      domain.DocumentFormatMarkdown,
		ContentHash: document.ContentHash(old), FileSize: int64(len(old)),
	}, []domain.DocumentChunk{{Text: "仍在回答的内容"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimForIndexing(ctx, "user-1", staged.ID, 1); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteIndexing(ctx, "user-1", staged.ID, 1, IndexOutcome{Status: domain.DocumentStatusReady, ChunkCount: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := publishSourceVersion(t, storage, ctx, "user-1", "document-reader", 1, domain.DocumentFormatMarkdown, old); err != nil {
		t.Fatalf("publish v1: %v", err)
	}
	if first := mustReadRow(t, store, "user-1", "document-reader"); first.Version != 1 {
		t.Fatalf("version after the first publish = %d", first.Version)
	}

	reader, err := NewSourceReader(store, storage)
	if err != nil {
		t.Fatal(err)
	}
	source, err := reader.ReadActiveSource(ctx, "user-1", "document-reader", 1)
	if err != nil {
		t.Fatalf("read of the serving version: %v", err)
	}
	if source.Content != "# 旧版\n\n仍在回答的内容\n" {
		t.Fatalf("returned content = %q, want the normalized text", source.Content)
	}
	if source.Bytes != int64(len(old)) {
		t.Fatalf("reported bytes = %d, want the raw length %d", source.Bytes, len(old))
	}

	// Now stage a rewrite whose bytes differ, and leave it failed: the row carries v2's
	// metadata, version 1 is still what answers, and v1's file must still read.
	fresh := []byte("# 新版\r\n\r\n改写后的内容\r\n")
	next, err := store.StageReplacement(ctx, "user-1", "document-reader",
		mustReadRow(t, store, "user-1", "document-reader").Version, DocumentReplacement{
			Format: domain.DocumentFormatMarkdown, ContentHash: document.ContentHash(fresh),
			FileSize: int64(len(fresh)), Chunks: []domain.DocumentChunk{{Text: "改写后的内容"}},
		})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := publishSourceVersion(t, storage, ctx, "user-1", "document-reader", next.Version, next.Format, fresh); err != nil {
		t.Fatalf("publish v%d: %v", next.Version, err)
	}
	if _, err := store.ClaimForIndexing(ctx, "user-1", next.ID, next.Version); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteIndexing(ctx, "user-1", next.ID, next.Version, IndexOutcome{
		Status: domain.DocumentStatusFailed, ErrorMessage: "indexing operation failed",
	}); err != nil {
		t.Fatal(err)
	}
	row := mustReadRow(t, store, "user-1", "document-reader")
	if row.Version != 2 || row.ActiveVersion != 1 || row.IndexStatus != domain.DocumentStatusFailed {
		t.Fatalf("row after the failed update = %#v", row)
	}
	if row.ContentHash != document.ContentHash(fresh) {
		t.Fatal("the row no longer describes the newest upload, which is what this test assumes")
	}

	stale, err := reader.ReadActiveSource(ctx, "user-1", "document-reader", 1)
	if err != nil {
		t.Fatalf("reading the serving version during a failed update: %v", err)
	}
	if stale.Content != "# 旧版\n\n仍在回答的内容\n" {
		t.Fatalf("serving source = %q, want the version retrieval is quoting", stale.Content)
	}
	// The staged version is not answerable, so it is not readable either - even though
	// its file exists on disk.
	if _, err := reader.ReadActiveSource(ctx, "user-1", "document-reader", 2); !errors.Is(err, ErrSourceVersionNotRead) {
		t.Fatalf("read of the staged version = %v, want it refused as not serving", err)
	}
	if _, err := reader.ReadActiveSource(ctx, "user-1", "document-reader", 9); !errors.Is(err, ErrSourceVersionNotRead) {
		t.Fatalf("read of an invented version = %v, want it refused", err)
	}
}

// TestReaderReportsMismatchInsteadOfReadingOn is the other direction: when the row and the
// file for the same version disagree, that is corruption and saying so is the only
// honest answer. A document that quotes bytes it cannot vouch for is worse than one that
// admits it cannot read them.
func TestReaderReportsMismatchInsteadOfReadingOn(t *testing.T) {
	ctx := context.Background()
	store, storage, collectionID := sourceStoreFixture(t)
	staged, err := store.StageDocument(ctx, "user-1", &domain.Document{
		ID: "document-corrupt", CollectionID: collectionID, Title: "corrupt.md",
		Format: domain.DocumentFormatMarkdown,
		// The row claims a digest the file will not have.
		ContentHash: document.ContentHash([]byte("行里写的内容")), FileSize: 99,
	}, []domain.DocumentChunk{{Text: "内容"}})
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := store.ClaimForIndexing(ctx, "user-1", staged.ID, 1); err != nil || !claimed {
		t.Fatalf("claim = (%v, %v)", claimed, err)
	}
	if err := store.CompleteIndexing(ctx, "user-1", staged.ID, 1, IndexOutcome{Status: domain.DocumentStatusReady, ChunkCount: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := publishSourceVersion(t, storage, ctx, "user-1", "document-corrupt", 1, domain.DocumentFormatMarkdown, []byte("磁盘上真正的内容")); err != nil {
		t.Fatalf("publish v1: %v", err)
	}

	reader, err := NewSourceReader(store, storage)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.ReadActiveSource(ctx, "user-1", "document-corrupt", 1); !errors.Is(err, ErrSourceInvalid) {
		t.Fatalf("read over a mismatched row = %v, want the mismatch named", err)
	}
	// The size alone is not the check. A file with the right length and the wrong bytes
	// would answer every citation with text nobody uploaded, and only the digest catches
	// that - so this second document is sized to match and differs in content.
	sized, err := store.StageDocument(ctx, "user-1", &domain.Document{
		ID: "document-same-size", CollectionID: collectionID, Title: "sized.md",
		Format:      domain.DocumentFormatMarkdown,
		ContentHash: document.ContentHash([]byte("AAAAA")), FileSize: 5,
	}, []domain.DocumentChunk{{Text: "内容"}})
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := store.ClaimForIndexing(ctx, "user-1", sized.ID, 1); err != nil || !claimed {
		t.Fatalf("claim = (%v, %v)", claimed, err)
	}
	if err := store.CompleteIndexing(ctx, "user-1", sized.ID, 1, IndexOutcome{Status: domain.DocumentStatusReady, ChunkCount: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := publishSourceVersion(t, storage, ctx, "user-1", sized.ID, 1,
		domain.DocumentFormatMarkdown, []byte("BBBBB")); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.ReadActiveSource(ctx, "user-1", sized.ID, 1); !errors.Is(err, ErrSourceInvalid) {
		t.Fatalf("read of same-length different-content bytes = %v, want the digest to refuse it", err)
	}
	// A foreign user cannot reach the file at all: the row lookup is what gates it, and it
	// answers exactly like a missing document.
	if _, err := reader.ReadActiveSource(ctx, "user-2", "document-corrupt", 1); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("cross-user read = %v, want the missing-document answer", err)
	}
	if _, err := reader.ReadActiveSource(ctx, "", "document-corrupt", 1); !errors.Is(err, repository.ErrInvalidArgument) {
		t.Fatalf("no user = %v, want an invalid argument", err)
	}
	if _, err := reader.ReadActiveSource(ctx, "user-1", "document-corrupt", 0); !errors.Is(err, repository.ErrInvalidArgument) {
		t.Fatalf("version 0 = %v, want an invalid argument", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := reader.ReadActiveSource(canceled, "user-1", "document-corrupt", 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled read = %v, want the cancellation", err)
	}
}

func TestReaderRefusesItsOwnMissingHalf(t *testing.T) {
	store, storage, _ := sourceStoreFixture(t)
	if _, err := NewSourceReader(nil, storage); err == nil {
		t.Fatal("a reader with no store was accepted")
	}
	if _, err := NewSourceReader(store, nil); err == nil {
		t.Fatal("a reader with no storage was accepted")
	}
}

// TestLegacyDocumentReportsNoSourceKeepsItReadable states the boundary for rows written
// before sources existed: no bytes were ever stored, so read-back says so, the document
// stays searchable and updatable, and nothing is fabricated from chunks to fill the gap.
func TestLegacyDocumentReportsNoSourceKeepsItReadable(t *testing.T) {
	ctx := context.Background()
	store, storage, collectionID := sourceStoreFixture(t)
	legacy := stageDocument(t, store, "user-1", collectionID, "legacy.md")
	published := publish(t, store, "user-1", legacy)

	reader, err := NewSourceReader(store, storage)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.ReadActiveSource(ctx, "user-1", published.ID, published.ServingVersion()); !errors.Is(err, ErrSourceNotStored) {
		t.Fatalf("legacy read-back = %v, want 'never stored' rather than reconstructed text", err)
	}
	chunks, err := store.ChunksFor(ctx, "user-1", []string{collectionID})
	if err != nil || len(chunks) == 0 {
		t.Fatalf("legacy retrieval broke: %#v (%v)", chunks, err)
	}
	updated, err := store.ReplaceDocumentContent(ctx, "user-1", published.ID, DocumentReplacement{
		Format: domain.DocumentFormatMarkdown, Chunks: []domain.DocumentChunk{{Text: "改写后的内容"}},
	})
	if err != nil {
		t.Fatalf("a legacy document must stay updatable: %v", err)
	}
	if updated.Version != 2 || updated.ActiveVersion != 2 {
		t.Fatalf("legacy update = %#v, want version 2 published", updated)
	}
	if _, err := storage.ReadVersion(ctx, "user-1", published.ID, 1); !errors.Is(err, ErrSourceNotStored) {
		t.Fatalf("the legacy version grew a source from nowhere: %v", err)
	}
}

// publishSourceVersion writes one version's bytes the way an ingest would: temp first,
// then published under the version it belongs to.
func publishSourceVersion(t *testing.T, storage *LocalSourceStorage, ctx context.Context,
	userID, documentID string, version int, format domain.DocumentFormat, content []byte) (string, error) {
	t.Helper()
	stored, err := storage.SaveTemp(ctx, userID, documentID, content)
	if err != nil {
		return "", err
	}
	return storage.Publish(ctx, userID, documentID, version, format, stored.TempPath)
}

// failingDropStorage fails the first version removal and then delegates to the real
// storage. It exists because the fault this test is about - a removal that fails and has
// to be retried - cannot be produced portably by permissions: a read-only file is still
// deletable here, and inventing a permission failure that did not happen would make the
// retry claim mean less than it says. Everything the retry then does runs against the real
// filesystem, and the assertions are about bytes that exist or do not.
type failingDropStorage struct {
	*LocalSourceStorage
	failNext bool
}

func (f *failingDropStorage) DropVersion(ctx context.Context, userID, documentID string, version int) error {
	if f.failNext {
		f.failNext = false
		return fmt.Errorf("knowledge: simulated removal failure for version %d", version)
	}
	return f.LocalSourceStorage.DropVersion(ctx, userID, documentID, version)
}

// TestCleanupPoolRetriesARealFileRemoval is the cleanup path with a failure in the way:
// the pool has to leave the bytes in place, record the failure with the source's own
// phrasing and a backoff, and then finish the job once the fault clears - without ever
// dropping the version that is answering.
func TestCleanupPoolRetriesARealFileRemoval(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	collectionID := newCollection(t, store, "user-1")
	storage := sourceFixture(t, 1<<20)

	first := stageDocument(t, store, "user-1", collectionID, "retry-file.md")
	if _, err := publishSourceVersion(t, storage, ctx, "user-1", first.ID, 1,
		domain.DocumentFormatMarkdown, []byte("# 第一版")); err != nil {
		t.Fatal(err)
	}
	published := publish(t, store, "user-1", first)

	// A successful rewrite retires version 1 and records it as outstanding cleanup.
	next, err := store.StageReplacement(ctx, "user-1", published.ID, published.Version, DocumentReplacement{
		Format: domain.DocumentFormatMarkdown, Chunks: []domain.DocumentChunk{{Text: "第二版内容"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := publishSourceVersion(t, storage, ctx, "user-1", published.ID, next.Version,
		next.Format, []byte("# 第二版")); err != nil {
		t.Fatal(err)
	}
	retiring := publish(t, store, "user-1", *next)
	if retiring.IndexStatus != domain.DocumentStatusCleanupPending || retiring.CleanupVersion != 1 {
		t.Fatalf("row after the rewrite = %#v, want version 1 queued for cleanup", retiring)
	}
	retired := filepath.Join(storage.Root(), "user-1", published.ID, "source-v1.md")

	pool, err := NewCleanupPool(store, &stubCleaner{},
		&failingDropStorage{LocalSourceStorage: storage, failNext: true},
		WorkerConfig{Workers: 1, QueueSize: 2, ScanInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	pool.WithLogger(func(string, ...any) {})
	pool.process(ctx, QueuedDocument{UserID: "user-1", Document: retiring})

	after := mustReadRow(t, store, "user-1", published.ID)
	if after.IndexStatus != domain.DocumentStatusCleanupPending || after.CleanupAttempts != 1 {
		t.Fatalf("document after the failed removal = %#v, want the work still recorded", after)
	}
	if after.ErrorMessage != CleanupFailureMessage("source") {
		t.Fatalf("error = %q, want the source stage named", after.ErrorMessage)
	}
	if _, err := os.Stat(retired); err != nil {
		t.Fatalf("the failed removal lost the file anyway: %v", err)
	}
	// The retry follows the schedule rather than firing immediately: the item is not due.
	if cleanupIsDue(t, store, time.Now().UTC().Add(CleanupBackoff(1)/2), published.ID) {
		t.Fatal("the failed cleanup came back as due before its schedule")
	}
	if _, err := storage.ReadVersion(ctx, "user-1", published.ID, 2); err != nil {
		t.Fatalf("the serving version's bytes were disturbed by the failed cleanup: %v", err)
	}

	// The fault clears and the work is due again. Making it due uses the store's own
	// bookkeeping call rather than a sleep, so the retry is observed at a fixed point.
	if err := store.FailCleanup(ctx, "user-1", published.ID, "made due for the retry",
		time.Now().UTC().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	retry := mustReadRow(t, store, "user-1", published.ID)
	pool.process(ctx, QueuedDocument{UserID: "user-1", Document: retry})

	if _, err := os.Stat(retired); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the retried cleanup left version 1's bytes on disk: %v", err)
	}
	if _, err := storage.ReadVersion(ctx, "user-1", published.ID, 1); !errors.Is(err, ErrSourceNotStored) {
		t.Fatalf("the storage still answers for a file that is gone: %v", err)
	}
	if _, err := storage.ReadVersion(ctx, "user-1", published.ID, 2); err != nil {
		t.Fatalf("the retry dropped the serving version too: %v", err)
	}
	closed := mustReadRow(t, store, "user-1", published.ID)
	if closed.IndexStatus != domain.DocumentStatusReady || closed.CleanupVersion != 0 {
		t.Fatalf("document after the retried cleanup = %#v, want the record closed", closed)
	}
}

func mustReadRow(t *testing.T, store *MemoryStore, userID, documentID string) domain.Document {
	t.Helper()
	record, err := store.GetDocument(context.Background(), userID, documentID)
	if err != nil {
		t.Fatalf("GetDocument(%s): %v", documentID, err)
	}
	return *record
}
