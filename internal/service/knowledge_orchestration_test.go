package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/knowledge"
)

// These tests drive the service through the assembly a deployment with sources actually
// has: the service stages versions, the index pool indexes them, the cleanup pool removes
// what a row says is retired. Nothing here hand-builds a state the entry points cannot
// produce. An earlier version of these tests asserted against the inline publish the store
// does for itself, and that is not the path a source deployment takes - it is also the path
// that would have hidden the fact that the ingest was waiting on a model call.

// ingestRunner is the index side of the assembly. It records the passes it was asked to
// make, can fail, and can hold a pass open so a test can observe the ingest while indexing
// is demonstrably still running.
type ingestRunner struct {
	mu       sync.Mutex
	calls    int
	err      error
	holdFrom int
	entered  chan struct{}
	release  chan struct{}
	once     sync.Once
}

// holdFrom parks every pass from the n-th one onwards, so a test can look at the state
// between "staged" and "indexed" without racing the worker.
func (r *ingestRunner) holdPassesFrom(n int) {
	r.mu.Lock()
	r.holdFrom = n
	r.mu.Unlock()
}

func (r *ingestRunner) IndexVersion(ctx context.Context, _, _ string, _ int) (int, error) {
	r.mu.Lock()
	r.calls++
	failure := r.err
	parked := r.holdFrom > 0 && r.calls >= r.holdFrom
	r.mu.Unlock()
	if r.entered != nil {
		r.once.Do(func() { close(r.entered) })
	}
	if parked && r.release != nil {
		select {
		case <-r.release:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	if failure != nil {
		return 0, failure
	}
	return 1, nil
}

// setErr changes what the next passes return. The pool reads the runner from its own
// goroutine, so every field it touches is behind the same mutex - a test that wrote r.err
// directly would be a data race, and -race would catch it as flakiness in the wrong place.
func (r *ingestRunner) setErr(err error) {
	r.mu.Lock()
	r.err = err
	r.mu.Unlock()
}

func (r *ingestRunner) snapshot() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// rowCleaner is the derived-index half of cleanup. There is no vector index in these
// tests, so its job is to record that the queue reached it, and to be able to refuse.
type rowCleaner struct {
	mu      sync.Mutex
	sweeps  []int
	dropped int
	refused bool
}

func (c *rowCleaner) refuse(value bool) {
	c.mu.Lock()
	c.refused = value
	c.mu.Unlock()
}

func (c *rowCleaner) SweepExcept(_ context.Context, _, _ string, keep int) error {
	c.mu.Lock()
	refused := c.refused
	if !refused {
		c.sweeps = append(c.sweeps, keep)
	}
	c.mu.Unlock()
	if refused {
		return errors.New("index refused the sweep")
	}
	return nil
}

func (c *rowCleaner) DropDocument(_ context.Context, _, _ string) error {
	c.mu.Lock()
	refused := c.refused
	if !refused {
		c.dropped++
	}
	c.mu.Unlock()
	if refused {
		return errors.New("index refused the removal")
	}
	return nil
}

func (c *rowCleaner) counts() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.sweeps), c.dropped
}

// faultyStorage fails the write or the removal half on demand. A real "disk full" cannot be
// produced portably, and the property under test is what the orchestration does when the
// storage refuses - not the storage's own error handling, which has its own tests.
type faultyStorage struct {
	knowledge.SourceStorage
	mu        sync.Mutex
	failSave  bool
	failDrop  bool
	publishes int
	attempts  []publishAttempt
}

// publishAttempt records the call as the storage saw it. Which refusal a rewrite came back
// with cannot be read off the error alone - "refused before writing", "refused at the
// publish" and "refused at the stage" all surface as a conflict - so the record of the call
// and its result is the only thing that proves a test reached the branch it claims to.
type publishAttempt struct {
	version int
	format  domain.DocumentFormat
	target  string
	err     error
}

func (f *faultyStorage) setFailSave(value bool) {
	f.mu.Lock()
	f.failSave = value
	f.mu.Unlock()
}

func (f *faultyStorage) setFailDrop(value bool) {
	f.mu.Lock()
	f.failDrop = value
	f.mu.Unlock()
}

// publishCount counts how many times the orchestration reached the storage with a file to
// keep. A refusal that happens before any byte is written is the behaviour v2 has, and the
// compensating removal afterwards would produce the same file count by a worse route - so
// this is the only assertion that tells them apart.
func (f *faultyStorage) publishCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.publishes
}

func (f *faultyStorage) publishAttempts() []publishAttempt {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]publishAttempt(nil), f.attempts...)
}

func (f *faultyStorage) Publish(ctx context.Context, userID, documentID string, version int,
	format domain.DocumentFormat, tempPath string) (string, error) {
	target, err := f.SourceStorage.Publish(ctx, userID, documentID, version, format, tempPath)
	f.mu.Lock()
	f.publishes++
	f.attempts = append(f.attempts, publishAttempt{version: version, format: format, target: target, err: err})
	f.mu.Unlock()
	return target, err
}

func (f *faultyStorage) SaveTemp(ctx context.Context, userID, documentID string, content []byte) (knowledge.StoredSource, error) {
	f.mu.Lock()
	refuse := f.failSave
	f.mu.Unlock()
	if refuse {
		return knowledge.StoredSource{}, errors.New("disk full")
	}
	return f.SourceStorage.SaveTemp(ctx, userID, documentID, content)
}

func (f *faultyStorage) DropVersion(ctx context.Context, userID, documentID string, version int) error {
	f.mu.Lock()
	refuse := f.failDrop
	f.mu.Unlock()
	if refuse {
		return errors.New("removal refused")
	}
	return f.SourceStorage.DropVersion(ctx, userID, documentID, version)
}

type orchestration struct {
	store      *knowledge.MemoryStore
	service    *KnowledgeService
	storage    *faultyStorage
	root       string
	index      *ingestRunner
	rows       *rowCleaner
	collection string
	indexPool  *knowledge.IndexPool
	cleanPool  *knowledge.CleanupPool
	runCtx     context.Context
	runCancel  context.CancelFunc
}

type orchestrationOptions struct {
	maxBytes int64
	limits   knowledge.Limits
	// blockedIndex parks the index worker inside the first pass it takes.
	blockedIndex bool
	// noLifecycle keeps only the Store half, which is an assembly mistake rather than a
	// supported mode; the tests that use it assert what such a service refuses to do.
	storeOnly bool
}

func orchestrationFixture(t *testing.T, options orchestrationOptions) *orchestration {
	t.Helper()
	ctx := context.Background()
	maxBytes := options.maxBytes
	if maxBytes == 0 {
		maxBytes = 1 << 20
	}
	root := filepath.Join(t.TempDir(), "sources")
	local, err := knowledge.NewLocalSourceStorage(root, maxBytes)
	if err != nil {
		t.Fatal(err)
	}
	storage := &faultyStorage{SourceStorage: local}
	store := knowledge.NewMemoryStore()
	if options.limits.MaxDocuments != 0 || options.limits.MaxTotalBytes != 0 {
		store = store.WithLimits(options.limits)
	}
	collection, err := store.CreateCollection(ctx, &domain.Collection{UserID: "user-1", Title: "手册"})
	if err != nil {
		t.Fatal(err)
	}
	runner := &ingestRunner{entered: make(chan struct{}), release: make(chan struct{})}
	if options.blockedIndex {
		runner.holdPassesFrom(1)
	}
	rows := &rowCleaner{}
	var serviceStore knowledge.Store = store
	if options.storeOnly {
		serviceStore = storeOnly{store}
	}
	service := NewKnowledgeService(serviceStore).WithSourceStorage(storage)

	pool, err := knowledge.NewIndexPool(store, runner, knowledge.WorkerConfig{
		Workers: 1, QueueSize: 4, ScanInterval: 2 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	pool.WithLogger(func(string, ...any) {})
	cleanup, err := knowledge.NewCleanupPool(store, rows, storage, knowledge.WorkerConfig{
		Workers: 1, QueueSize: 4, ScanInterval: 2 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	cleanup.WithLogger(func(string, ...any) {})

	// The pools are built but not started. A worker that is running while a test counts
	// files is an observer race, not a subject: on Windows it shows up as a sharing
	// violation on a file the test happened to have open. Tests start exactly the worker
	// whose behaviour they are asserting on, and stop it before a count that has to be
	// exact.
	holder := &orchestration{store: store, service: service, storage: storage, root: root,
		index: runner, rows: rows, collection: collection.ID, indexPool: pool, cleanPool: cleanup}
	runCtx, cancel := context.WithCancel(context.Background())
	holder.runCtx, holder.runCancel = runCtx, cancel
	t.Cleanup(func() {
		holder.stop(t)
		cancel()
	})
	return holder
}

// startIndex runs the index worker: staged versions get claimed, "indexed" and published.
func (o *orchestration) startIndex(t *testing.T) {
	t.Helper()
	if err := o.indexPool.Start(o.runCtx); err != nil {
		t.Fatalf("index pool Start: %v", err)
	}
}

// startCleanup runs the cleanup worker: retired rows, then retired bytes, then the record.
func (o *orchestration) startCleanup(t *testing.T) {
	t.Helper()
	if err := o.cleanPool.Start(o.runCtx); err != nil {
		t.Fatalf("cleanup pool Start: %v", err)
	}
}

// stop drains both pools so a following filesystem count sees a settled tree. Stopping an
// unstarted pool is a no-op, so tests can call it without tracking what they started.
func (o *orchestration) stop(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := o.indexPool.Stop(ctx); err != nil {
		t.Errorf("index pool Stop: %v", err)
	}
	if err := o.cleanPool.Stop(ctx); err != nil {
		t.Errorf("cleanup pool Stop: %v", err)
	}
}

// waitUntil polls a condition the pools write, with a deadline: a worker that never runs
// fails the test instead of hanging it, and a slow machine cannot make it pass.
func waitUntil(t *testing.T, timeout time.Duration, description string, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

func (o *orchestration) row(t *testing.T, documentID string) domain.Document {
	t.Helper()
	record, err := o.store.GetDocument(context.Background(), "user-1", documentID)
	if err != nil {
		t.Fatalf("GetDocument(%s): %v", documentID, err)
	}
	return *record
}

func (o *orchestration) chunks(t *testing.T) []domain.DocumentChunk {
	t.Helper()
	found, err := o.store.ChunksFor(context.Background(), "user-1", []string{o.collection})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// fileCount counts the bytes under the source root, which is how these tests tell
// "cleaned up" from "cleaned up except for an empty directory". A root that was never
// created counts as zero files - that is the strongest form of the assertion.
func (o *orchestration) fileCount(t *testing.T) int {
	t.Helper()
	return countFiles(t, o.root)
}

func countFiles(t *testing.T, root string) int {
	t.Helper()
	var files int
	err := filepath.WalkDir(root, func(_ string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() {
			files++
		}
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// hasFile names one exact file. hasVersion cannot carry the race and compensation
// assertions: two requests for the same version may each have published a file, and "the
// version's file" is then not a thing that exists - which is exactly the confusion the
// compensation must avoid.
func (o *orchestration) hasFile(t *testing.T, documentID, name string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(o.root, "user-1", documentID, name))
	if err == nil {
		return true
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat %s/%s: %v", documentID, name, err)
	}
	return false
}

// hasVersion asks whether one version's bytes are on disk, by reading it. Any error that
// is not "never stored" or the deliberate oversized case fails the test: a file a worker
// happens to be deleting is not something to read through, and the tests are arranged so
// this only runs when the workers that delete are stopped.
func (o *orchestration) hasVersion(t *testing.T, documentID string, version int) bool {
	t.Helper()
	_, err := o.storage.ReadVersion(context.Background(), "user-1", documentID, version)
	if errors.Is(err, knowledge.ErrSourceNotStored) {
		return false
	}
	if err != nil && !errors.Is(err, knowledge.ErrSourceTooLarge) {
		t.Fatalf("ReadVersion(%s, v%d) = %v", documentID, version, err)
	}
	return true
}

// waitRow waits on the row the pools write, never on the filesystem: the state is what
// proves a cleanup is over, while opening a file during its removal is a race with the
// worker instead of an observation of it.
func (o *orchestration) waitRow(t *testing.T, documentID, description string, ready func(domain.Document) bool) domain.Document {
	t.Helper()
	var record domain.Document
	waitUntil(t, 20*time.Second, description, func() bool {
		record = o.row(t, documentID)
		return ready(record)
	})
	return record
}

// waitRowGone is the same idea for a document whose deletion has finished: the row leaves
// the visible set and the cleanup queue, and both are states, not files.
func (o *orchestration) waitRowGone(t *testing.T, documentID string) {
	t.Helper()
	waitUntil(t, 20*time.Second, "the document to disappear from reads and from the queue", func() bool {
		if _, err := o.store.GetDocument(context.Background(), "user-1", documentID); err == nil {
			return false
		}
		due, err := o.store.ListDueCleanup(context.Background(), time.Now().UTC().Add(time.Hour), 50)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range due {
			if item.Document.ID == documentID {
				return false
			}
		}
		return true
	})
}

// TestUploadReturnsPendingWhileIndexingIsHeldOpen is the reason the source path is not the
// synchronous one: an upload's duration must not be set by a model call. The worker is
// parked inside a pass, and the ingest still comes back with a pending version whose
// source is already readable.
func TestUploadReturnsPendingWhileIndexingIsHeldOpen(t *testing.T) {
	fixture := orchestrationFixture(t, orchestrationOptions{blockedIndex: true})
	fixture.startIndex(t)
	ctx := context.Background()

	created := fixture.service.addDocument(t, ctx, "user-1", fixture.collection, "fresh.md",
		"# 新上传\r\n\r\n带 BOM 与 CRLF 的原文")
	if created.IndexStatus != domain.DocumentStatusPending || created.ActiveVersion != 0 {
		t.Fatalf("upload returned %#v, want a pending version that has not been indexed", created)
	}
	if !fixture.hasVersion(t, created.ID, 1) {
		t.Fatal("the upload returned before its source was published")
	}
	// Nothing answers from a version nobody has indexed, and read-back refuses it for the
	// same reason retrieval does - there is no serving version to ask for.
	if _, err := fixture.service.ReadDocumentSource(ctx, "user-1", created.ID, 0); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("read of an unindexed document = %v, want the version guard to answer", err)
	}
	if got := fixture.chunks(t); len(got) != 0 {
		t.Fatalf("retrieval holds %#v, want nothing from an unindexed version", got)
	}

	close(fixture.index.release)
	waitUntil(t, 10*time.Second, "the queue to publish the document", func() bool {
		return fixture.row(t, created.ID).Retrievable()
	})
	source, err := fixture.service.ReadDocumentSource(ctx, "user-1", created.ID, 1)
	if err != nil {
		t.Fatalf("read-back after indexing: %v", err)
	}
	if source.Content != "# 新上传\n\n带 BOM 与 CRLF 的原文" {
		t.Fatalf("read-back = %q, want the normalized text", source.Content)
	}
	if source.Format != domain.DocumentFormatMarkdown || source.Bytes <= 0 {
		t.Fatalf("read-back = %+v, want the file's own type and the raw length", source)
	}
	if got := fixture.chunks(t); len(got) != 1 {
		t.Fatalf("retrieval after indexing = %#v", got)
	}
}

// TestIngestFaultsRefuseBeforeBytesOrRows is the ordering rule counted rather than trusted:
// a storage that cannot write produces no row, no queue item and no quota; a rewrite the
// source must refuse produces no bytes.
func TestIngestFaultsRefuseBeforeBytesOrRows(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "sources")
	storage, err := knowledge.NewLocalSourceStorage(root, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	store := knowledge.NewMemoryStore()
	collection, err := store.CreateCollection(ctx, &domain.Collection{UserID: "user-1", Title: "手册"})
	if err != nil {
		t.Fatal(err)
	}
	unwritable := NewKnowledgeService(store).WithSourceStorage(&faultyStorage{SourceStorage: storage, failSave: true})
	if _, err := unwritable.AddDocument(ctx, "user-1", collection.ID, DocumentInput{
		Title: "never.md", Format: "markdown", Content: "# 内容\n\n写不进去的原文",
	}); err == nil {
		t.Fatal("an ingest whose source could not be stored reported success")
	}
	documents, err := store.ListDocuments(ctx, "user-1", collection.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(documents) != 0 {
		t.Fatalf("%d documents after a failed source write: %#v", len(documents), documents)
	}
	pending, err := store.ListPending(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("a version with no stored source is queued for indexing: %+v", pending)
	}
	usage, err := store.UsageByUser(ctx, "user-1")
	if err != nil {
		t.Fatal(err)
	}
	if usage.DocumentCount != 0 || usage.TotalBytes != 0 {
		t.Fatalf("quota spent by an ingest that produced nothing: %+v", usage)
	}
	if got := countFiles(t, root); got != 0 {
		t.Fatalf("%d stored files after a write that was refused", got)
	}

	// A document whose first pass is still pending takes no rewrite - and the refusal
	// happens before any byte is published for a version that will not exist.
	fixture := orchestrationFixture(t, orchestrationOptions{blockedIndex: true})
	fixture.startIndex(t)
	created := fixture.service.addDocument(t, ctx, "user-1", fixture.collection, "busy.md", "# 忙\n\n第一版")
	before := fixture.fileCount(t)
	published := fixture.storage.publishCount()
	if _, err := fixture.service.ReplaceDocumentContent(ctx, "user-1", created.ID, DocumentInput{
		Title: "busy.md", Format: "markdown", Content: "# 忙\n\n第二版",
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("rewrite onto a pending version = %v, want the busy refusal", err)
	}
	if fixture.storage.publishCount() != published {
		t.Fatalf("%d publishes after a busy refusal, want the state check to have happened first",
			fixture.storage.publishCount())
	}
	if fixture.fileCount(t) != before {
		t.Fatalf("the refused rewrite published bytes anyway: %d files, was %d", fixture.fileCount(t), before)
	}
	close(fixture.index.release)
}

// waitServing blocks until the given version is what answers queries, and returns the row.
func (o *orchestration) waitServing(t *testing.T, documentID string, version int) domain.Document {
	t.Helper()
	waitUntil(t, 10*time.Second, fmt.Sprintf("version %d to be serving", version), func() bool {
		record := o.row(t, documentID)
		return record.ActiveVersion == version && record.Retrievable()
	})
	return o.row(t, documentID)
}

// TestUpdateKeepsTheOldSourceServingUntilTheNewOneIndexes is the version split at the
// service entry point. The rewrite returns a pending version, the old one keeps answering,
// and the old version's bytes stay readable while the row already carries the NEW version's
// hash and size - the two contents are deliberately different lengths, so an implementation
// that measured the old file against the new metadata would fail this test outright.
func TestUpdateKeepsTheOldSourceServingUntilTheNewOneIndexes(t *testing.T) {
	fixture := orchestrationFixture(t, orchestrationOptions{})
	fixture.startIndex(t)
	ctx := context.Background()

	first := fixture.service.addDocument(t, ctx, "user-1", fixture.collection, "v.md", "# 版本\r\n\r\n第一版内容")
	fixture.waitServing(t, first.ID, 1)
	fixture.index.holdPassesFrom(2)

	second, err := fixture.service.ReplaceDocumentContent(ctx, "user-1", first.ID, DocumentInput{
		Title: "v.md", Format: "markdown", Content: "# 版本\n\n改写之后长得很不一样的第二版内容",
	})
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if second.Version != 2 || second.ActiveVersion != 1 || second.IndexStatus != domain.DocumentStatusPending {
		t.Fatalf("rewrite returned %#v, want v2 pending with v1 still serving", second)
	}
	if !fixture.hasFile(t, first.ID, "source-v2.md") || !fixture.hasFile(t, first.ID, "source-v1.md") {
		t.Fatal("both versions' bytes must exist while v2 is still being worked")
	}
	if chunks := fixture.chunks(t); len(chunks) != 1 || !strings.Contains(chunks[0].Text, "第一版内容") {
		t.Fatalf("retrieval during the pending rewrite = %#v, want the old version answering", chunks)
	}
	source, err := fixture.service.ReadDocumentSource(ctx, "user-1", first.ID, 1)
	if err != nil {
		t.Fatalf("the old source stopped being readable during a pending update: %v", err)
	}
	if source.Content != "# 版本\n\n第一版内容" {
		t.Fatalf("read-back = %q, want the old version's text", source.Content)
	}
	if _, err := fixture.service.ReadDocumentSource(ctx, "user-1", first.ID, 2); !errors.Is(err, ErrDocumentVersionStale) {
		t.Fatalf("read of the pending version = %v, want it refused as not serving", err)
	}

	close(fixture.index.release)
	fixture.waitServing(t, first.ID, 2)
	if _, err := fixture.service.ReadDocumentSource(ctx, "user-1", first.ID, 2); err != nil {
		t.Fatalf("the new version's source is not readable once it serves: %v", err)
	}
	if _, err := fixture.service.ReadDocumentSource(ctx, "user-1", first.ID, 1); !errors.Is(err, ErrDocumentVersionStale) {
		t.Fatalf("read of the retired version = %v, want it refused", err)
	}
	// The retired version's bytes are the cleanup queue's work, not the ingest's: the pool
	// takes them, and only after the rows for that version are gone. Until it does, the file
	// is still there - which is what the ingest must never have decided on its own.
	if !fixture.hasFile(t, first.ID, "source-v1.md") {
		t.Fatal("the ingest removed the retired version's bytes before any cleanup ran")
	}
	fixture.startCleanup(t)
	// The wait is on the row the queue writes, not on the file it is deleting. Polling the
	// filesystem during a removal races the worker - on Windows the read of a file being
	// deleted fails with a sharing violation - and the row leaving cleanup state is the fact
	// the test actually needs anyway.
	fixture.waitRow(t, first.ID, "the queue to finish the retired version's cleanup", func(record domain.Document) bool {
		return record.IndexStatus == domain.DocumentStatusReady && record.CleanupVersion == 0 &&
			record.CleanupAttempts == 0 && record.CleanupAfter.IsZero()
	})
	// Only once the worker is drained does a count measure a settled tree.
	fixture.stop(t)
	if !fixture.hasFile(t, first.ID, "source-v2.md") {
		t.Fatal("the serving version's bytes were removed as collateral")
	}
	if fixture.hasFile(t, first.ID, "source-v1.md") {
		t.Fatal("the retired version's bytes outlived the cleanup that recorded itself as finished")
	}
	if got := fixture.fileCount(t); got != 1 {
		t.Fatalf("%d stored files after the retired version's cleanup, want the serving version only", got)
	}
	if source, err := fixture.service.ReadDocumentSource(ctx, "user-1", first.ID, 2); err != nil ||
		source.Content != "# 版本\n\n改写之后长得很不一样的第二版内容" {
		t.Fatalf("read-back of the serving version after the cleanup = %+v (%v)", source, err)
	}
}

// TestBackgroundIndexFailureKeepsTheNewSourceAndRestoresRetry is the failure half: a pass
// that dies in the background must leave both the metadata and the new bytes in place, keep
// the old version answering, and stay retryable - because the retry re-indexes the same
// stored source rather than asking the client to upload again.
func TestBackgroundIndexFailureKeepsTheNewSourceAndRestoresRetry(t *testing.T) {
	fixture := orchestrationFixture(t, orchestrationOptions{})
	fixture.startIndex(t)
	ctx := context.Background()

	first := fixture.service.addDocument(t, ctx, "user-1", fixture.collection, "fail.md", "# 后台\n\n第一版内容")
	fixture.waitServing(t, first.ID, 1)
	fixture.index.setErr(errors.New("embedder unreachable"))

	staged, err := fixture.service.ReplaceDocumentContent(ctx, "user-1", first.ID, DocumentInput{
		Title: "fail.md", Format: "markdown", Content: "# 后台\n\n第二版内容更长",
	})
	if err != nil {
		t.Fatal(err)
	}
	if staged.IndexStatus != domain.DocumentStatusPending {
		t.Fatalf("rewrite returned %#v, want pending before the background pass runs", staged)
	}

	waitUntil(t, 10*time.Second, "the background pass to record a failure", func() bool {
		return fixture.row(t, first.ID).IndexStatus == domain.DocumentStatusFailed
	})
	failed := fixture.row(t, first.ID)
	if failed.Version != 2 || failed.ActiveVersion != 1 || failed.ErrorMessage != knowledge.IndexFailureReason(errors.New("anything")) {
		t.Fatalf("row after the failed pass = %#v", failed)
	}
	if !fixture.hasVersion(t, first.ID, 2) {
		t.Fatal("a background index failure deleted the new source: the retry would have nothing to index")
	}
	if chunks := fixture.chunks(t); len(chunks) != 1 || !strings.Contains(chunks[0].Text, "第一版内容") {
		t.Fatalf("retrieval after the failed pass = %#v, want the old version answering", chunks)
	}
	if source, err := fixture.service.ReadDocumentSource(ctx, "user-1", first.ID, 1); err != nil ||
		source.Content != "# 后台\n\n第一版内容" {
		t.Fatalf("read-back of the serving version after the failure = %+v (%v)", source, err)
	}
	state, err := fixture.service.DocumentWorkState(ctx, "user-1", first.ID)
	if err != nil || state.IndexStatus != domain.DocumentStatusFailed || state.Retrievable != true ||
		state.ActiveVersion != 1 {
		t.Fatalf("work state = %+v (%v)", state, err)
	}

	fixture.index.setErr(nil)
	retried, err := fixture.service.RetryDocument(ctx, "user-1", first.ID)
	if err != nil {
		t.Fatalf("RetryDocument: %v", err)
	}
	if retried.IndexStatus != domain.DocumentStatusPending {
		t.Fatalf("after retry = %+v, want the version queued again", retried)
	}
	fixture.waitServing(t, first.ID, 2)
	if source, err := fixture.service.ReadDocumentSource(ctx, "user-1", first.ID, 2); err != nil ||
		source.Content != "# 后台\n\n第二版内容更长" {
		t.Fatalf("read-back after the successful retry = %+v (%v)", source, err)
	}
}

// TestDeletedDocumentKeepsTheCleanupTaskUntilItFinishes goes through the service entry
// point for a delete: the document disappears from every read path, and the removal of the
// rows and then the bytes is recorded work that survives a failure. A delete that removed
// the bytes inline would report success while the queue still owed the rows, and a failed
// removal that dropped the record would lose the bytes forever.
func TestDeletedDocumentKeepsTheCleanupTaskUntilItFinishes(t *testing.T) {
	fixture := orchestrationFixture(t, orchestrationOptions{})
	fixture.startIndex(t)
	fixture.startCleanup(t)
	ctx := context.Background()

	created := fixture.service.addDocument(t, ctx, "user-1", fixture.collection, "gone.md", "# 删除\n\n要被删掉的原文")
	fixture.waitServing(t, created.ID, 1)
	if !fixture.hasFile(t, created.ID, "source-v1.md") {
		t.Fatal("no source to delete")
	}
	fixture.rows.refuse(true)

	if err := fixture.service.DeleteDocument(ctx, "user-1", created.ID); err != nil {
		t.Fatalf("DeleteDocument: %v", err)
	}
	// The delete is visible immediately even though nothing has been removed yet.
	if _, err := fixture.service.GetDocument(ctx, "user-1", created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("read after the delete = %v, want the document already invisible", err)
	}
	if chunks := fixture.chunks(t); len(chunks) != 0 {
		t.Fatalf("a deleted document still answers: %#v", chunks)
	}
	if !fixture.hasFile(t, created.ID, "source-v1.md") {
		t.Fatal("the delete removed the bytes before the rows were cleaned")
	}

	// A deleting row is invisible to every document read by design, so the queue is where
	// its state has to be observed - which is also the point: the work survives as recorded
	// work, not as something a caller can see only while the row is still readable.
	queuedCleanup := func() *domain.Document {
		due, err := fixture.store.ListDueCleanup(context.Background(), time.Now().UTC().Add(time.Hour), 50)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range due {
			if item.Document.ID == created.ID {
				found := item.Document
				return &found
			}
		}
		return nil
	}
	waitUntil(t, 10*time.Second, "the failed cleanup to be recorded", func() bool {
		record := queuedCleanup()
		return record != nil && record.CleanupAttempts >= 1 &&
			record.ErrorMessage == knowledge.CleanupFailureMessage("vector")
	})
	// The record says the bytes are still owed, and they are. A deleting row carries
	// CleanupVersion 0, which means "every version this document ever had" - the status is
	// what identifies the work, not a version number.
	owed := queuedCleanup()
	if owed == nil || owed.IndexStatus != domain.DocumentStatusDeleting {
		t.Fatalf("the cleanup record after a refused pass = %+v, want the deleting document still owed", owed)
	}
	if !fixture.hasFile(t, created.ID, "source-v1.md") {
		t.Fatal("the refused cleanup removed the bytes anyway")
	}

	fixture.rows.refuse(false)
	// Again the queue's own state is the finish line: the row is gone from reads and from the
	// list of due cleanup. Waiting on that, then draining the workers, is what makes the file
	// assertions below observations of a settled tree rather than of a deletion in progress.
	fixture.waitRowGone(t, created.ID)
	fixture.stop(t)
	if _, err := fixture.store.GetDocument(context.Background(), "user-1", created.ID); err == nil {
		t.Fatal("the document row survived its finished cleanup")
	}
	if fixture.hasFile(t, created.ID, "source-v1.md") {
		t.Fatal("the cleanup recorded itself as finished while the source was still on disk")
	}
	if got := fixture.fileCount(t); got != 0 {
		t.Fatalf("%d stored files after a finished delete, want none", got)
	}
	if _, dropped := fixture.rows.counts(); dropped != 1 {
		t.Fatalf("document row removals = %d, want exactly one", dropped)
	}
}

// TestUnchangedRewritePublishesNothing pins the refusal that costs nothing: identical bytes
// under a new name are unchanged by v2's rule, and the service decides it before writing a
// file - so no version is published, no bytes are staged for the storage to take back, and
// the row is untouched. The publish count is the assertion that distinguishes "refused
// first" from "published then compensated", which leaves the same file count by a worse route.
func TestUnchangedRewritePublishesNothing(t *testing.T) {
	fixture := orchestrationFixture(t, orchestrationOptions{})
	fixture.startIndex(t)
	ctx := context.Background()

	content := "# 同一段\n\n一个字都没改的原文"
	created := fixture.service.addDocument(t, ctx, "user-1", fixture.collection, "same.md", content)
	fixture.waitServing(t, created.ID, 1)
	before := fixture.fileCount(t)

	replaced, err := fixture.service.ReplaceDocumentContent(ctx, "user-1", created.ID, DocumentInput{
		Title: "改了名字.md", Format: "markdown", Content: content,
	})
	if err != nil {
		t.Fatalf("an identical rewrite must succeed as unchanged: %v", err)
	}
	if got := fixture.storage.publishCount(); got != 1 {
		t.Fatalf("%d publishes after an unchanged rewrite, want the ingest's one and nothing else", got)
	}
	if replaced.Version != 1 || replaced.Title != "same.md" {
		t.Fatalf("unchanged rewrite = %#v, want the row untouched", replaced)
	}
	if fixture.fileCount(t) != before {
		t.Fatalf("%d stored files after an unchanged rewrite, want none added", fixture.fileCount(t))
	}
	if source, err := fixture.service.ReadDocumentSource(ctx, "user-1", created.ID, 1); err != nil ||
		source.Content != content {
		t.Fatalf("read-back = %+v (%v)", source, err)
	}
	// The queue took nothing from this attempt either.
	pending, err := fixture.store.ListPending(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("an unchanged rewrite queued work: %+v", pending)
	}
}

// slowStageStore refuses to return from StageReplacement until its caller has gone away,
// which is how the test reaches the state "the bytes are published, the row was never
// staged, and the caller is already gone".
type slowStageStore struct {
	knowledge.Store
	knowledge.Lifecycle
}

func (s slowStageStore) StageReplacement(ctx context.Context, _, _ string, _ int,
	_ knowledge.DocumentReplacement) (*domain.Document, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestCompensationSurvivesACallerThatAlreadyLeft is the second half of the compensation
// rule. The bytes published for a version that never got staged have to go back even when
// the request that produced them is already cancelled - asking the storage to remove them
// on the dead context gets the removal refused, and the leftover would be a file no row
// points at and no cleanup will ever find.
func TestCompensationSurvivesACallerThatAlreadyLeft(t *testing.T) {
	fixture := orchestrationFixture(t, orchestrationOptions{})
	fixture.startIndex(t)
	ctx := context.Background()

	created := fixture.service.addDocument(t, ctx, "user-1", fixture.collection, "leave.md", "# 离开\n\n第一版原文")
	fixture.waitServing(t, created.ID, 1)
	if got := fixture.fileCount(t); got != 1 {
		t.Fatalf("%d stored files before the cancellation test", got)
	}

	half := NewKnowledgeService(slowStageStore{Store: fixture.store, Lifecycle: fixture.store}).
		WithSourceStorage(fixture.storage)
	callCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := half.ReplaceDocumentContent(callCtx, "user-1", created.ID, DocumentInput{
			Title: "leave.md", Format: "markdown", Content: "# 离开\n\n第二版原文更长一些",
		})
		done <- err
	}()
	// By name, not "whatever version 2's file is": the file this attempt published is the one
	// the compensation has to remove, and naming it is what makes the next assertion mean
	// something.
	waitUntil(t, 10*time.Second, "the stage to be reached with the bytes published", func() bool {
		return fixture.hasFile(t, created.ID, "source-v2.md")
	})
	cancel()

	var err error
	select {
	case err = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the rewrite never returned after its context was cancelled")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled rewrite = %v, want the caller's cancellation to surface", err)
	}
	if fixture.hasFile(t, created.ID, "source-v2.md") {
		t.Fatal("the compensation was rejected with the caller's dead context and left an orphan file")
	}
	if got := fixture.fileCount(t); got != 1 {
		t.Fatalf("%d stored files after the compensation, want the version-1 source and nothing else", got)
	}
	if !fixture.hasFile(t, created.ID, "source-v1.md") {
		t.Fatal("the compensation removed the serving version's file instead of its own")
	}
	if _, err := fixture.service.ReadDocumentSource(ctx, "user-1", created.ID, 1); err != nil {
		t.Fatalf("the compensation disturbed the serving version's source: %v", err)
	}
	row := fixture.row(t, created.ID)
	if row.Version != 1 || row.ActiveVersion != 1 || row.IndexStatus != domain.DocumentStatusReady {
		t.Fatalf("row after the cancelled rewrite = %#v, want it untouched", row)
	}
}

// TestRefusedStagesTakeTheirBytesBackIsNotADuplicateOfTheFaultyWrite covers the other
// refusal that must cost nothing: the storage wrote the bytes happily, and the store said
// no. A spent quota, a name taken by another live document, and a second concurrent
// rewrite of the same version each publish a file the row will never own.
func TestRefusedStagesTakeTheirBytesBack(t *testing.T) {
	ctx := context.Background()
	fixture := orchestrationFixture(t, orchestrationOptions{
		limits: knowledge.Limits{MaxDocuments: 2, MaxTotalBytes: 1 << 20},
	})
	fixture.startIndex(t)
	first := fixture.service.addDocument(t, ctx, "user-1", fixture.collection, "第一篇.md", "# 一\n\n第一篇原文")
	fixture.waitServing(t, first.ID, 1)
	second := fixture.service.addDocument(t, ctx, "user-1", fixture.collection, "占名.md", "# 占\n\n另一篇的原文")
	fixture.waitServing(t, second.ID, 1)
	if got := fixture.fileCount(t); got != 2 {
		t.Fatalf("%d stored files after two accepted ingests", got)
	}

	// A rewrite whose name is already taken by another live document is refused whole: no
	// new version, no rename, no bytes left behind.
	if _, err := fixture.service.ReplaceDocumentContent(ctx, "user-1", first.ID, DocumentInput{
		Title: "占名.md", Format: "markdown", Content: "# 一\n\n改名撞上别人的内容",
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("rename onto a taken name = %v, want a conflict", err)
	}
	untouched := fixture.row(t, first.ID)
	if untouched.Title != "第一篇.md" || untouched.Version != 1 {
		t.Fatalf("the refused rewrite disturbed the row: %#v", untouched)
	}
	if got := fixture.fileCount(t); got != 2 {
		t.Fatalf("%d stored files after a name conflict, want no orphan from the refused rewrite", got)
	}
	// Uniqueness is decided inside the store's transaction, not by a pre-check, so this
	// refusal is the case where bytes were published and then had to be taken back. The
	// state checks above are the ones that refuse before writing anything.
	if got := fixture.storage.publishCount(); got != 3 {
		t.Fatalf("%d publishes, want the two ingests plus the rewritten version that was then refused", got)
	}

	// A spent ceiling is the same story through a different door.
	if _, err := fixture.service.AddDocument(ctx, "user-1", fixture.collection, DocumentInput{
		Title: "第三篇.md", Format: "markdown", Content: "# 三\n\n超配额的第二篇",
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("third document over a two-document ceiling = %v, want a conflict", err)
	}
	if got := fixture.fileCount(t); got != 2 {
		t.Fatalf("%d stored files after a quota refusal, want the two accepted documents only", got)
	}
	documents, err := fixture.store.ListDocuments(ctx, "user-1", fixture.collection)
	if err != nil {
		t.Fatal(err)
	}
	if len(documents) != 2 {
		t.Fatalf("collection holds %#v", documents)
	}
}

// TestSourceReadRefusalsAtTheServiceEdge keeps the three answers a caller has to tell
// apart visible at the entry point the HTTP layer uses, not only at the storage.
func TestSourceReadRefusalsAtTheServiceEdge(t *testing.T) {
	ctx := context.Background()
	fixture := orchestrationFixture(t, orchestrationOptions{maxBytes: 32})
	fixture.startIndex(t)
	created := fixture.service.addDocument(t, ctx, "user-1", fixture.collection, "edge.md", "# 边界\n\n短原文")
	fixture.waitServing(t, created.ID, 1)

	// Somebody else's document is indistinguishable from a missing one.
	if _, err := fixture.service.ReadDocumentSource(ctx, "user-2", created.ID, 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-user read = %v, want the missing-document answer", err)
	}
	// A deployment with no source storage says nothing was stored, rather than returning an
	// empty document.
	plain := NewKnowledgeService(fixture.store)
	if _, err := plain.ReadDocumentSource(ctx, "user-1", created.ID, 1); !errors.Is(err, ErrDocumentSourceMissing) {
		t.Fatalf("read with no storage configured = %v", err)
	}
	// An oversized file is refused before it is buffered, and the refusal reaches the caller
	// as "invalid" rather than as a truncated source it would cite as whole. The file is
	// placed by hand: a write path that enforces the bound cannot produce it - it comes from
	// an older build, a bigger limit elsewhere, or a directory moved in from outside.
	directory := filepath.Join(fixture.root, "user-1", created.ID)
	if err := os.WriteFile(filepath.Join(directory, "source-v1.md"),
		[]byte(strings.Repeat("a", 64)), 0o600); err != nil {
		t.Fatal(err)
	}
	reader := NewKnowledgeService(fixture.store).WithSourceStorage(fixture.storage)
	if _, err := reader.ReadDocumentSource(ctx, "user-1", created.ID, 1); !errors.Is(err, ErrDocumentSourceInvalid) {
		t.Fatalf("read of an oversized serving source = %v, want it refused as invalid", err)
	}
	// The refusal removed nothing: the file is still there for an operator to look at. The
	// check is a stat rather than a read, since reading is exactly what was refused.
	if _, err := os.Stat(filepath.Join(directory, "source-v1.md")); err != nil {
		t.Fatalf("a refused read deleted the source it could not serve: %v", err)
	}
}

// gatedStage announces the version expectation a replacement carries and waits for the
// test to act, so the interleaving is caused by a signal instead of a sleep. It then
// forwards the call unchanged, which means the store's own expected-version check is what
// decides - the wrapper does not refuse anything on its own.
type gatedStage struct {
	knowledge.Store
	knowledge.Lifecycle
	expectations chan int
	release      chan struct{}
}

func (g gatedStage) StageReplacement(ctx context.Context, userID, documentID string, expectedVersion int,
	replacement knowledge.DocumentReplacement) (*domain.Document, error) {
	select {
	case g.expectations <- expectedVersion:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case <-g.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return g.Lifecycle.StageReplacement(ctx, userID, documentID, expectedVersion, replacement)
}

// TestConcurrentRewriteLosesOnVersionAndCompensatesExactlyItsOwnFile is the race the
// pre-read creates: two requests read version 1, each names its source file for version 2,
// and because the name carries the format both can publish. expectedVersion is what makes
// exactly one of them stage, and the loser's compensation has to remove the file it
// published without touching the one the winner committed - which rules out any
// "delete the version's file" cleanup once two files share a version name.
func TestConcurrentRewriteLosesOnVersionAndCompensatesExactlyItsOwnFile(t *testing.T) {
	fixture := orchestrationFixture(t, orchestrationOptions{})
	fixture.startIndex(t)
	fixture.startCleanup(t)
	ctx := context.Background()

	created := fixture.service.addDocument(t, ctx, "user-1", fixture.collection, "contend.md", "# 竞争\n\n第一版原文")
	fixture.waitServing(t, created.ID, 1)

	gate := gatedStage{Store: fixture.store, Lifecycle: fixture.store,
		expectations: make(chan int, 1), release: make(chan struct{})}
	loser := NewKnowledgeService(gate).WithSourceStorage(fixture.storage)
	loserFailure := make(chan error, 1)
	go func() {
		_, err := loser.ReplaceDocumentContent(ctx, "user-1", created.ID, DocumentInput{
			Title: "contend.md", Format: "text", Content: "竞争失败的纯文本内容",
		})
		loserFailure <- err
	}()

	var expected int
	select {
	case expected = <-gate.expectations:
	case <-time.After(10 * time.Second):
		t.Fatal("the losing rewrite never reached the stage")
	}
	if expected != 1 {
		t.Fatalf("the losing rewrite carried expectation %d, want the version it pre-read", expected)
	}
	if !fixture.hasFile(t, created.ID, "source-v2.txt") {
		t.Fatal("the losing rewrite had not published its file yet")
	}

	// The winner commits while the loser is held at the stage, so the row is on version 2
	// by the time the loser asks for it.
	winnerText := "# 竞争\n\n获胜版本的 Markdown 原文"
	if _, err := fixture.service.ReplaceDocumentContent(ctx, "user-1", created.ID, DocumentInput{
		Title: "contend.md", Format: "markdown", Content: winnerText,
	}); err != nil {
		t.Fatalf("the winning rewrite: %v", err)
	}
	close(gate.release)

	var err error
	select {
	case err = <-loserFailure:
	case <-time.After(10 * time.Second):
		t.Fatal("the losing rewrite never returned")
	}
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("losing rewrite = %v, want a conflict from the version it was built on", err)
	}
	row := fixture.row(t, created.ID)
	if row.Version != 2 || row.Format != domain.DocumentFormatMarkdown {
		t.Fatalf("row after the race = %#v, want the winner's version 2 markdown", row)
	}
	if row.ContentHash == "" {
		t.Fatal("the winner's metadata is gone")
	}

	// The loser's file is gone and the winner's is intact, readable, and answerable - the
	// loser could only have achieved that by naming the file it published itself.
	fixture.waitServing(t, created.ID, 2)
	if fixture.hasFile(t, created.ID, "source-v2.txt") {
		t.Fatal("the loser's source file survived its own compensation")
	}
	if !fixture.hasFile(t, created.ID, "source-v2.md") {
		t.Fatal("the loser's compensation removed the winner's file instead of its own")
	}
	source, err := fixture.service.ReadDocumentSource(ctx, "user-1", created.ID, 2)
	if err != nil {
		t.Fatalf("the winner's source must still read back: %v", err)
	}
	if source.Content != winnerText || source.Format != domain.DocumentFormatMarkdown {
		t.Fatalf("the winner's source came back as %+v, want its own bytes and type", source)
	}

	// And the winner's version is still fully live: once its predecessor's cleanup closes
	// (a document with cleanup owed takes no new version - v2's status rule), the next
	// rewrite lands, and a background failure of it is retryable.
	waitUntil(t, 20*time.Second, "version 1's cleanup to close", func() bool {
		return fixture.row(t, created.ID).IndexStatus == domain.DocumentStatusReady
	})
	fixture.index.setErr(errors.New("embedder unavailable"))
	third := "# 竞争\n\n第三版内容，先失败一次"
	if _, err := fixture.service.ReplaceDocumentContent(ctx, "user-1", created.ID, DocumentInput{
		Title: "contend.md", Format: "markdown", Content: third,
	}); err != nil {
		t.Fatalf("the follow-up rewrite: %v", err)
	}
	waitUntil(t, 10*time.Second, "the follow-up pass to fail", func() bool {
		return fixture.row(t, created.ID).IndexStatus == domain.DocumentStatusFailed
	})
	fixture.index.setErr(nil)
	if _, err := fixture.service.RetryDocument(ctx, "user-1", created.ID); err != nil {
		t.Fatalf("RetryDocument after the race: %v", err)
	}
	fixture.waitServing(t, created.ID, 3)
	retried, err := fixture.service.ReadDocumentSource(ctx, "user-1", created.ID, 3)
	if err != nil || retried.Content != third {
		t.Fatalf("read-back after the retry = %+v (%v)", retried, err)
	}
	// Version 2's retirement is the queue's business, so the queue's own row is what the test
	// waits on. Reading version 2's file to find out whether it is still there races the
	// worker that is deleting it, and the answer the race produces depends on the machine.
	fixture.waitRow(t, created.ID, "the queue to finish version 2's cleanup", func(record domain.Document) bool {
		return record.IndexStatus == domain.DocumentStatusReady && record.CleanupVersion == 0 &&
			record.CleanupAttempts == 0 && record.CleanupAfter.IsZero()
	})
	fixture.stop(t)
	if fixture.hasFile(t, created.ID, "source-v2.md") {
		t.Fatal("the retired version's source outlived the cleanup that recorded itself as finished")
	}
	if !fixture.hasFile(t, created.ID, "source-v3.md") {
		t.Fatal("the serving version's source was removed with the retired one")
	}
	if got := fixture.fileCount(t); got != 1 {
		t.Fatalf("%d stored files after two finished retirements, want the serving version only", got)
	}
	if after, err := fixture.service.ReadDocumentSource(ctx, "user-1", created.ID, 3); err != nil ||
		after.Content != third {
		t.Fatalf("read-back after the retired version's cleanup = %+v (%v)", after, err)
	}
}

// gatedPreRead announces that a caller has read the row - the moment its target version is
// chosen and its file name is decided - and holds it there, so another request can commit
// in between. Signalled rather than slept: the ordering the test needs is the ordering it
// creates.
type gatedPreRead struct {
	knowledge.Store
	knowledge.Lifecycle
	observed chan string
	proceed  chan struct{}
	seen     bool
	mu       sync.Mutex
}

// GetDocument gates on the *first* read, which is where the caller's target version and
// file name get decided. The pointer receiver keeps the mutex from being copied into the
// service.
func (g *gatedPreRead) GetDocument(ctx context.Context, userID, documentID string) (*domain.Document, error) {
	record, err := g.Store.GetDocument(ctx, userID, documentID)
	if err != nil {
		return record, err
	}
	g.mu.Lock()
	first := !g.seen
	g.seen = true
	g.mu.Unlock()
	if !first {
		return record, nil
	}
	select {
	case g.observed <- documentID:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case <-g.proceed:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return record, nil
}

// TestSameFormatRewriteIsRefusedAtPublishWithoutDisturbingTheWinner is the other collision:
// two requests for the same version with the same format cannot both have a file, so the
// second is refused before it can stage anything. The refusal must leave the winner's file
// exactly as it was - which rules out a compensation that deletes "the version's file": at
// that moment the only file for that version is the winner's.
func TestSameFormatRewriteIsRefusedAtPublishWithoutDisturbingTheWinner(t *testing.T) {
	fixture := orchestrationFixture(t, orchestrationOptions{})
	fixture.startIndex(t)
	ctx := context.Background()

	created := fixture.service.addDocument(t, ctx, "user-1", fixture.collection, "clash.md", "# 撞车\n\n第一版原文")
	fixture.waitServing(t, created.ID, 1)

	// The loser goes first. It reads the row at ready@version 1 and is held there BEFORE that
	// snapshot is handed back, which is the moment it fixed version 2 as its own target and
	// markdown as its own file name. Only then does the winner run, so the exact file the
	// loser is about to write is written by somebody else while the loser is still parked.
	// Parking it after the pre-read instead would give it the winner's version 2 and a target
	// of version 3: a different file name, no collision, and a test that asserts nothing.
	observed, proceed := make(chan string, 1), make(chan struct{})
	gated := &gatedPreRead{Store: fixture.store, Lifecycle: fixture.store, observed: observed, proceed: proceed}
	loser := NewKnowledgeService(gated).WithSourceStorage(fixture.storage)
	loserText := "# 撞车\n\n迟到的第二版内容"
	loserResult := make(chan error, 1)
	go func() {
		_, err := loser.ReplaceDocumentContent(ctx, "user-1", created.ID, DocumentInput{
			Title: "clash.md", Format: "markdown", Content: loserText,
		})
		loserResult <- err
	}()
	select {
	case <-observed:
	case <-time.After(10 * time.Second):
		t.Fatal("the losing rewrite never read the row")
	}

	winnerText := "# 撞车\n\n获胜的第二版内容"
	won, err := fixture.service.ReplaceDocumentContent(ctx, "user-1", created.ID, DocumentInput{
		Title: "clash.md", Format: "markdown", Content: winnerText,
	})
	if err != nil {
		t.Fatalf("the winning rewrite: %v", err)
	}
	fixture.waitServing(t, created.ID, 2)
	// The winner's own record of what it published, taken before the loser is released, so
	// the only call that can be added afterwards is the loser's.
	winnerAttempts := fixture.storage.publishAttempts()
	winnerCall := winnerAttempts[len(winnerAttempts)-1]
	// Publish hands back the path it committed, relative to the storage root, and that path is
	// what a later compensation removes - so the name has to be the winner's own.
	if winnerCall.err != nil || winnerCall.version != 2 ||
		filepath.Base(winnerCall.target) != "source-v2.md" {
		t.Fatalf("the winning rewrite published %#v, want version 2 committed as source-v2.md", winnerCall)
	}

	close(proceed)

	var lostErr error
	select {
	case lostErr = <-loserResult:
	case <-time.After(10 * time.Second):
		t.Fatal("the losing rewrite never returned")
	}
	if !errors.Is(lostErr, ErrConflict) {
		t.Fatalf("losing rewrite = %v, want the taken version refused as a conflict", lostErr)
	}
	// The conflict has to be the publish saying no. Refusing on the state checks before any
	// byte was written, or on the stage after it, returns the same ErrConflict and leaves the
	// same files - so the record of the call is the assertion here, not the error the caller
	// happened to get.
	attempts := fixture.storage.publishAttempts()
	if len(attempts) != len(winnerAttempts)+1 {
		t.Fatalf("%d publish calls after the loser, want exactly one more than the %d before it",
			len(attempts), len(winnerAttempts))
	}
	loserCall := attempts[len(attempts)-1]
	if !errors.Is(loserCall.err, knowledge.ErrSourceVersionTaken) {
		t.Fatalf("the losing rewrite's publish = %#v, want it refused because version 2's file exists",
			loserCall)
	}
	if loserCall.version != winnerCall.version || loserCall.format != winnerCall.format ||
		loserCall.target != "" {
		t.Fatalf("the loser published %#v, want the same version and format as the winner and no path",
			loserCall)
	}
	if !errors.Is(lostErr, knowledge.ErrSourceVersionTaken) {
		t.Fatalf("losing rewrite = %v, want it to carry the publish's own refusal", lostErr)
	}
	var refusals int
	for _, attempt := range attempts {
		if attempt.err != nil {
			refusals++
		}
	}
	if refusals != 1 {
		t.Fatalf("%d refused publishes, want only the loser's", refusals)
	}

	// The winner's committed bytes are the point of the whole test: at this moment the only
	// file for version 2 IS the winner's, so a compensation written as "remove this version's
	// file" would have deleted the row's own source.
	if !fixture.hasFile(t, created.ID, "source-v2.md") {
		t.Fatal("the refused rewrite removed the winner's source for version 2")
	}
	source, readErr := fixture.service.ReadDocumentSource(ctx, "user-1", created.ID, 2)
	if readErr != nil || source.Content != winnerText || source.Format != domain.DocumentFormatMarkdown {
		t.Fatalf("the winner's source changed: %+v (%v)", source, readErr)
	}
	untouched := fixture.row(t, created.ID)
	if untouched.Version != won.Version || untouched.Title != won.Title ||
		untouched.Format != won.Format || untouched.ContentHash != won.ContentHash ||
		untouched.FileSize != won.FileSize {
		t.Fatalf("row after the loser's refusal = %#v, want the winner's %#v", untouched, *won)
	}
	// The winner is still the one answering. Its row may be in cleanup_pending by now - v1's
	// bytes are owed to the queue and this test does not run it - which is why the assertion
	// is "still serving version 2", not "idle".
	if untouched.ActiveVersion != 2 || !untouched.Retrievable() {
		t.Fatalf("row after the loser's refusal = %#v, want the winner still serving version 2", untouched)
	}
	// And nothing of the loser's is left behind: no third file, no temp file in the document's
	// directory.
	if got := fixture.fileCount(t); got != 2 {
		t.Fatalf("%d stored files, want the two versions and no leftover from the loser", got)
	}
}

// TestRewriteChangingFormatReportsTheServingFilesFormat is the third correction: the row's
// Format belongs to the newest staged version, and the document quoting text the user can
// still read must report the type of THAT file. A markdown document rewritten to plain text
// whose pass then failed would otherwise be answered with "text" for markdown bytes.
func TestRewriteChangingFormatReportsTheServingFilesFormat(t *testing.T) {
	fixture := orchestrationFixture(t, orchestrationOptions{})
	fixture.startIndex(t)
	ctx := context.Background()

	created := fixture.service.addDocument(t, ctx, "user-1", fixture.collection, "note.md", "# 标题\n\nMarkdown 原文")
	fixture.waitServing(t, created.ID, 1)
	fixture.index.setErr(errors.New("embedder unavailable"))

	staged, err := fixture.service.ReplaceDocumentContent(ctx, "user-1", created.ID, DocumentInput{
		Title: "note.txt", Format: "text", Content: "标题\n\n纯文本的新版本内容",
	})
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 10*time.Second, "the format-changing pass to fail", func() bool {
		return fixture.row(t, created.ID).IndexStatus == domain.DocumentStatusFailed
	})
	row := fixture.row(t, created.ID)
	if row.Format != domain.DocumentFormatText {
		t.Fatalf("row format = %q, want the staged version to have changed it", row.Format)
	}
	source, err := fixture.service.ReadDocumentSource(ctx, "user-1", created.ID, staged.ActiveVersion)
	if err != nil {
		t.Fatalf("read-back of the serving version: %v", err)
	}
	if source.Format != domain.DocumentFormatMarkdown {
		t.Fatalf("read-back format = %q, want the serving file's own type", source.Format)
	}
	if source.Content != "# 标题\n\nMarkdown 原文" {
		t.Fatalf("read-back = %q, want the markdown bytes that are still answering", source.Content)
	}
	fixture.index.setErr(nil)
}

// TestSourceIngestWithoutLifecycleIsAnAssemblyError refuses the fallback that would be
// quietly destructive: a deployment that wires sources but no driver cannot finish a delete
// or advance a version, and it must not be allowed to hard-delete rows or drop bytes to
// look like it did.
func TestSourceIngestWithoutLifecycleIsAnAssemblyError(t *testing.T) {
	fixture := orchestrationFixture(t, orchestrationOptions{storeOnly: true})
	fixture.startIndex(t)
	ctx := context.Background()

	if _, err := fixture.service.AddDocument(ctx, "user-1", fixture.collection, DocumentInput{
		Title: "nowhere.md", Format: "markdown", Content: "# 装配\n\n缺驱动的配置",
	}); !errors.Is(err, ErrInternal) {
		t.Fatalf("ingest without a lifecycle = %v, want an internal failure", err)
	}
	if fixture.fileCount(t) != 0 {
		t.Fatalf("%d stored files after an ingest that was refused before writing", fixture.fileCount(t))
	}
	documents, err := fixture.store.ListDocuments(ctx, "user-1", fixture.collection)
	if err != nil {
		t.Fatal(err)
	}
	if len(documents) != 0 {
		t.Fatalf("%d documents after a refused ingest: %#v", len(documents), documents)
	}

	// A document that does exist, deleted through the same half-wired service, stays exactly
	// where it was: no hard delete, no early byte removal, one internal error.
	complete := NewKnowledgeService(fixture.store).WithSourceStorage(fixture.storage)
	created := complete.addDocument(t, ctx, "user-1", fixture.collection, "kept.md", "# 保留\n\n不该被硬删的原文")
	fixture.waitServing(t, created.ID, 1)

	if err := fixture.service.DeleteDocument(ctx, "user-1", created.ID); !errors.Is(err, ErrInternal) {
		t.Fatalf("delete without a lifecycle = %v, want an internal failure", err)
	}
	if _, err := fixture.store.GetDocument(ctx, "user-1", created.ID); err != nil {
		t.Fatalf("the refused delete disturbed the document: %v", err)
	}
	if !fixture.hasVersion(t, created.ID, 1) {
		t.Fatal("the refused delete removed the source bytes")
	}
	// Reads are the one thing a half-wired assembly can still do: the reader needs the
	// Store, not the driver, and pretending otherwise would deny a client a document that
	// is intact.
	if source, err := fixture.service.ReadDocumentSource(ctx, "user-1", created.ID, 1); err != nil ||
		source.Content != "# 保留\n\n不该被硬删的原文" {
		t.Fatalf("reads must keep working on a half-wired assembly: %+v (%v)", source, err)
	}
	if chunks := fixture.chunks(t); len(chunks) != 1 {
		t.Fatalf("a half-wired assembly disturbed retrieval: %#v", chunks)
	}
}
