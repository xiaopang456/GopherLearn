package redisindex

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"strings"
	"testing"

	redis "github.com/redis/go-redis/v9"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/knowledge"
	"github.com/xiaopang456/GopherLearn/internal/retrieval"
)

func TestNewValidatesTheAddressItIsGiven(t *testing.T) {
	accepted := []string{
		"127.0.0.1:6379", "redis:6379", ":6380", "redis://127.0.0.1:6379",
		"rediss://cache.internal:6380",
	}
	for _, address := range accepted {
		index, err := New(Options{Address: address, Dimensions: 4})
		if err != nil {
			t.Errorf("%q rejected: %v", address, err)
			continue
		}
		if index.Dimensions() != 4 {
			t.Errorf("%q: dimensions = %d, want 4", address, index.Dimensions())
		}
		_ = index.Close()
	}
	rejected := []string{
		"", "   ", "localhost", "http://127.0.0.1:6379", "redis://user:pw@127.0.0.1:6379",
		"redis://127.0.0.1:6379/3", "127.0.0.1:0", "127.0.0.1:99999", "127.0.0.1:notaport",
	}
	for _, address := range rejected {
		if _, err := New(Options{Address: address, Dimensions: 4}); !errors.Is(err, ErrConfig) {
			t.Errorf("%q accepted: want ErrConfig, got %v", address, err)
		}
	}
	if _, err := New(Options{Address: "127.0.0.1:6379"}); !errors.Is(err, ErrConfig) {
		t.Errorf("zero dimensions accepted: %v", err)
	}
	// A pasted ":port" means localhost to an operator and must not become a stray
	// empty host that resolves unpredictably.
	index, err := New(Options{Address: "127.0.0.1:6390", Password: "pw", Dimensions: 4})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = index.Close() }()
	if index.indexName != DefaultIndexName || index.keyPrefix != DefaultKeyPrefix {
		t.Fatalf("defaults: index %q prefix %q", index.indexName, index.keyPrefix)
	}
	// A non-zero database is not a weaker isolation level you can trade down to: FT.CREATE
	// answers "Cannot create index on db != 0" on a real server, so an index in database 7
	// is an index that does not exist. Refusing at construction is what turns that into a
	// startup message instead of a learner's failed upload.
	for _, database := range []int{1, 7, 15} {
		_, err := New(Options{Address: "127.0.0.1:6379", Dimensions: 4, Database: database})
		if !errors.Is(err, ErrConfig) {
			t.Errorf("database %d accepted: want ErrConfig, got %v", database, err)
			continue
		}
		if !strings.Contains(err.Error(), "database 0") {
			t.Errorf("database %d rejected without naming the reason: %v", database, err)
		}
	}
}

func TestVectorBytesIsLittleEndianFloat32(t *testing.T) {
	// RediSearch reads FLOAT32 vectors as little-endian. Getting this wrong is not a
	// failure: it stores noise and returns confident, meaningless neighbours.
	vector := []float32{1, -2.5, 0}
	encoded := vectorBytes(vector)
	if len(encoded) != 12 {
		t.Fatalf("encoded %d bytes, want 3 float32", len(encoded))
	}
	if math.Float32frombits(binary.LittleEndian.Uint32(encoded[0:4])) != 1 {
		t.Fatal("first component did not survive the round trip")
	}
	if math.Float32frombits(binary.LittleEndian.Uint32(encoded[4:8])) != -2.5 {
		t.Fatal("second component did not survive the round trip")
	}
	if encoded[8] != 0 || encoded[11] != 0 {
		t.Fatal("a zero float must encode as four zero bytes")
	}
}

func TestEscapeTagValueNeutralisesSeparators(t *testing.T) {
	const syntax = `,.;:!?|/\{}~*+()[]<>=&%#@$^-`
	escaped := escapeTagValue(`a,b|c}d{e:f-g_h`)
	for _, separator := range syntax {
		if strings.Count(escaped, `\`+string(separator)) != strings.Count(`a,b|c}d{e:f-g_h`, string(separator)) {
			t.Fatalf("escapeTagValue produced %q, want every %q escaped", escaped, string(separator))
		}
	}
	// The dash is in this list because a real server said so: FT.SEARCH answers
	// "Syntax error at offset 14 near user" for @user_id:{user-index-1200} and parses
	// @user_id:{user\-index\-1200}. The gated suite is what proves the escaped form still
	// matches the stored value, which is the risk of escaping too much.
	if escapeTagValue("user-index-1200") != `user\-index\-1200` {
		t.Errorf("dash not escaped as measured: %q", escapeTagValue("user-index-1200"))
	}
	// '_' carries no meaning in the tag grammar and must stay as it is.
	if escapeTagValue("doc_9_v1") != "doc_9_v1" {
		t.Errorf("underscore escaped: %q", escapeTagValue("doc_9_v1"))
	}
}

func TestScopeValuesThatWouldChangeWhatTheIndexMatches(t *testing.T) {
	// A comma is the separator a TAG field splits its values on, and '*', '?', '[' and '\'
	// are the characters SCAN reads in a match pattern. Both sets are refused rather than
	// escaped away, because the damage is not this document's search - it is some other
	// owner's query or sweep matching rows it should never have seen.
	refused := map[string][]string{
		"owner":         {"a,b", "a*b", "a?b", "a[b", "a\\b", " pad", "pad ", ""},
		"document id":   {"doc:1", "d,e"},
		"chunk id":      {"chunk-1:2"},
		"collection id": {"col,1"},
	}
	for kind, values := range refused {
		for _, value := range values {
			check := checkScopeValue
			if kind == "document id" || kind == "chunk id" || kind == "owner" {
				check = checkKeyValue
			}
			if err := check(kind, value); !errors.Is(err, ErrConfig) {
				t.Errorf("%s %q accepted: want ErrConfig, got %v", kind, value, err)
			}
		}
	}
	// What this project actually generates must pass untouched: the identifiers come from
	// domain and from a hex digest, and refusing them would silently stop indexing.
	for _, value := range []string{
		"user-4", "collection-18d9-x", "doc_9_v1", "chunk-7f3a", "0f4c9ab1d2e3",
		"learner@example.com", "deterministic-feature-hash-v1:256",
	} {
		if err := checkScopeValue("id", value); err != nil {
			t.Errorf("%q refused: %v", value, err)
		}
	}
	for _, value := range []string{"user-4", "doc_9_v1", "chunk-7f3a", "learner@example.com"} {
		if err := checkKeyValue("id", value); err != nil {
			t.Errorf("%q refused as a key segment: %v", value, err)
		}
	}
	// A colon is legal in a TAG value and illegal in a key segment; the split between the
	// two checks is exactly that difference.
	if err := checkScopeValue("vector identity", "model:256"); err != nil {
		t.Errorf("colon in a tag value refused: %v", err)
	}
}

func TestKeyCarriesOwnerDocumentVersionAndChunk(t *testing.T) {
	index, err := New(Options{Address: "127.0.0.1:6379", Dimensions: 4})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = index.Close() }()
	got := index.Key("user-1", "doc-9", 3, "doc-9-v3-c2")
	if got != DefaultKeyPrefix+"user-1:doc-9:3:doc-9-v3-c2" {
		t.Fatalf("key = %q", got)
	}
	if versionOfKey(got) != 3 {
		t.Fatalf("versionOfKey = %d, want 3", versionOfKey(got))
	}
	if versionOfKey("gopherlearn:knowledge:chunk:user:doc:bad:c") != 0 {
		t.Fatal("a malformed key must read as version 0 so a sweep deletes it")
	}
}

func TestScopeFilterRefusesAnythingWithoutScope(t *testing.T) {
	if _, err := scopeFilter(retrieval.VectorSearch{CollectionIDs: []string{"c1"}}); !errors.Is(err, ErrConfig) {
		t.Errorf("no owner: %v", err)
	}
	if _, err := scopeFilter(retrieval.VectorSearch{UserID: "u1"}); !errors.Is(err, ErrConfig) {
		t.Errorf("no collections: %v", err)
	}
	if _, err := scopeFilter(retrieval.VectorSearch{UserID: "u1", CollectionIDs: []string{"  "}}); !errors.Is(err, ErrConfig) {
		t.Errorf("blank collections: %v", err)
	}
	filter, err := scopeFilter(retrieval.VectorSearch{
		UserID: "u1", CollectionIDs: []string{"col-b", "col-a", "col-b"}, Identity: "id1",
	})
	if err != nil {
		t.Fatalf("scopeFilter: %v", err)
	}
	// Sorted and de-duplicated so the same scope always produces the same query text. The
	// '|' between the two collections is the tag OR and is not escaped; the dashes inside
	// each value are, because the server rejects a bare one.
	if filter != `@user_id:{u1} @collection_id:{col\-a|col\-b} @identity:{id1}` {
		t.Fatalf("filter = %q", filter)
	}
	if !strings.Contains(orUnknown(""), unknownIdentity) {
		t.Fatal("a missing identity must be written as a value, never as an empty TAG")
	}
}

func TestParseHitsRefusesAnUnscoredOrUnparseableHit(t *testing.T) {
	hits, err := parseHits([]redis.Document{{
		Fields: map[string]string{FieldChunkID: "chunk-a", fieldDistance: "0.2"},
	}})
	if err != nil {
		t.Fatalf("parseHits: %v", err)
	}
	if len(hits) != 1 || hits[0].ChunkID != "chunk-a" || hits[0].Similarity != 0.8 {
		t.Fatalf("hits = %+v, want distance 0.2 to read as similarity 0.8", hits)
	}
	broken := [][]redis.Document{
		{{Fields: map[string]string{fieldDistance: "0.1"}}},
		{{Fields: map[string]string{FieldChunkID: "  ", fieldDistance: "0.1"}}},
		{{Fields: map[string]string{FieldChunkID: "chunk-a"}}},
		{{Fields: map[string]string{FieldChunkID: "chunk-a", fieldDistance: "far"}}},
		{{Fields: map[string]string{FieldChunkID: "chunk-a", fieldDistance: "NaN"}}},
		{{Fields: map[string]string{FieldChunkID: "chunk-a", fieldDistance: "+Inf"}}},
	}
	for index, documents := range broken {
		if _, err := parseHits(documents); !errors.Is(err, ErrSchema) {
			t.Errorf("case %d: error = %v, want ErrSchema", index, err)
		}
	}
}

// fakeWriter records what the synchronizer asked an index to do, and can be made to
// fail at each step so the compensation paths are reachable without a server.
type fakeWriter struct {
	dimensions int
	upserts    [][]retrieval.VectorEntry
	deletes    []string
	kept       []int
	upsertErr  error
	deleteErr  error
	versionErr error
	ensureErr  error
}

func (f *fakeWriter) EnsureIndex(context.Context) error { return f.ensureErr }

func (f *fakeWriter) Dimensions() int { return f.dimensions }

func (f *fakeWriter) Upsert(_ context.Context, _ string, entries []retrieval.VectorEntry) error {
	if f.upsertErr != nil {
		return f.upsertErr
	}
	f.upserts = append(f.upserts, entries)
	return nil
}

func (f *fakeWriter) DeleteDocument(_ context.Context, userID, documentID string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deletes = append(f.deletes, userID+"/"+documentID)
	return nil
}

func (f *fakeWriter) DeleteVersions(_ context.Context, userID, documentID string, keep int) error {
	if f.versionErr != nil {
		return f.versionErr
	}
	f.deletes = append(f.deletes, "versions:"+userID+"/"+documentID)
	f.kept = append(f.kept, keep)
	return nil
}

// stubEmbedder returns one fixed vector per text, so identity and dimension checks are
// exercised without a model.
type stubEmbedder struct {
	dimensions int
	model      string
	calls      int
	lastTexts  []string
	err        error
}

func (s *stubEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	s.calls++
	s.lastTexts = append([]string(nil), texts...)
	if s.err != nil {
		return nil, s.err
	}
	vectors := make([][]float32, 0, len(texts))
	for range texts {
		vector := make([]float32, s.dimensions)
		vector[0] = 1
		vectors = append(vectors, vector)
	}
	return vectors, nil
}

func (s *stubEmbedder) Dimensions() int { return s.dimensions }
func (s *stubEmbedder) Model() string {
	if s.model == "" {
		return "stub-embedder"
	}
	return s.model
}

func newTestStores(t *testing.T) (knowledge.Store, string) {
	t.Helper()
	store := knowledge.NewMemoryStore()
	collection, err := store.CreateCollection(context.Background(), &domain.Collection{UserID: "user-1", Title: "notes"})
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	return store, collection.ID
}

func documentChunks(count int) []domain.DocumentChunk {
	chunks := make([]domain.DocumentChunk, 0, count)
	for index := 0; index < count; index++ {
		chunks = append(chunks, domain.DocumentChunk{
			Order: index + 1, Section: "章节", StartLine: index*4 + 1, EndLine: index*4 + 4,
			Text: strings.Repeat("内容", 40) + strings.Repeat("x", index),
		})
	}
	return chunks
}

func TestNewSynchronizerChecksItsCollaboratorsAgree(t *testing.T) {
	store, _ := newTestStores(t)
	embedder := &stubEmbedder{dimensions: 4}
	writer := &fakeWriter{dimensions: 4}
	if _, err := NewSynchronizer(nil, writer, embedder, "v1"); err == nil {
		t.Error("missing store accepted")
	}
	if _, err := NewSynchronizer(store, nil, embedder, "v1"); err == nil {
		t.Error("missing index accepted")
	}
	if _, err := NewSynchronizer(store, writer, nil, "v1"); err == nil {
		t.Error("missing embedder accepted")
	}
	if _, err := NewSynchronizer(store, writer, embedder, "  "); !errors.Is(err, ErrConfig) {
		t.Errorf("blank content version accepted: %v", err)
	}
	if _, err := NewSynchronizer(store, &fakeWriter{dimensions: 8}, embedder, "v1"); !errors.Is(err, ErrConfig) {
		t.Errorf("dimension disagreement accepted: %v", err)
	}
	created, err := NewSynchronizer(store, writer, embedder, "chunks-v1")
	if err != nil {
		t.Fatalf("NewSynchronizer: %v", err)
	}
	if created.Identity() == "" || created.Identity() != retrieval.VectorIdentity(embedder, "chunks-v1") {
		t.Fatalf("identity = %q, want the embedder fingerprint for this content version", created.Identity())
	}
	if again, err := NewSynchronizer(store, writer, &stubEmbedder{dimensions: 4, model: "different"}, "chunks-v1"); err != nil {
		t.Fatalf("NewSynchronizer: %v", err)
	} else if again.Identity() == created.Identity() {
		t.Fatal("a different model must produce a different identity: an index would mix the two")
	}
}

func TestSynchronizerWritesVectorsForTheChunksTheStorePublished(t *testing.T) {
	store, collectionID := newTestStores(t)
	writer := &fakeWriter{dimensions: 4}
	embedder := &stubEmbedder{dimensions: 4}
	synchronizer, err := NewSynchronizer(store, writer, embedder, "chunks-v1")
	if err != nil {
		t.Fatalf("NewSynchronizer: %v", err)
	}
	created, err := synchronizer.CreateDocument(context.Background(), "user-1",
		&domain.Document{CollectionID: collectionID, Title: "concurrency", Format: domain.DocumentFormatMarkdown},
		documentChunks(3))
	if err != nil {
		t.Fatalf("CreateDocument: %v", err)
	}
	if len(writer.upserts) != 1 {
		t.Fatalf("index saw %d upsert calls, want 1", len(writer.upserts))
	}
	entries := writer.upserts[0]
	if len(entries) != 3 {
		t.Fatalf("index received %d entries, want the three stored chunks", len(entries))
	}
	stored, err := store.ChunksFor(context.Background(), "user-1", []string{collectionID})
	if err != nil {
		t.Fatalf("ChunksFor: %v", err)
	}
	for index, entry := range entries {
		// The ids must be the store's, not the caller's: the caller sent chunks with no
		// id at all, and a citation built on an invented id points at nothing.
		if entry.Chunk.ID != stored[index].ID || entry.Chunk.Version != created.Version {
			t.Fatalf("entry %d = %s/v%d, want %s/v%d", index, entry.Chunk.ID, entry.Chunk.Version, stored[index].ID, created.Version)
		}
		if entry.Identity != synchronizer.Identity() {
			t.Fatalf("entry %d carries identity %q, want %q", index, entry.Identity, synchronizer.Identity())
		}
		if len(entry.Vector) != 4 {
			t.Fatalf("entry %d has %d dimensions", index, len(entry.Vector))
		}
	}
	// One embed call for three chunks, and each text is the shared EmbeddingInput.
	if embedder.calls != 1 || len(embedder.lastTexts) != 3 {
		t.Fatalf("embedder saw %d calls and %d texts", embedder.calls, len(embedder.lastTexts))
	}
	if embedder.lastTexts[0] != retrieval.EmbeddingInput(stored[0]) {
		t.Fatal("indexing embedded a different text than the chunk carries")
	}
}

// TestSynchronizerRecordsAFailedIndexAndKeepsTheDocumentRetryable is the v2 failure
// policy, and it is the opposite of a rollback: an index that refused the vectors leaves
// the document staged and marked failed, with the reason recorded, so the user can
// retry. Deleting the row instead would answer "your upload failed" with "and your
// document is gone", which is what the earlier compensating-delete design did.
func TestSynchronizerRecordsAFailedIndexAndKeepsTheDocumentRetryable(t *testing.T) {
	ctx := context.Background()
	store, collectionID := newTestStores(t)
	writer := &fakeWriter{dimensions: 4, upsertErr: errors.New("index down")}
	synchronizer, err := NewSynchronizer(store, writer, &stubEmbedder{dimensions: 4}, "chunks-v1")
	if err != nil {
		t.Fatalf("NewSynchronizer: %v", err)
	}
	if _, err := synchronizer.CreateDocument(ctx, "user-1",
		&domain.Document{CollectionID: collectionID, Title: "concurrency", Format: domain.DocumentFormatMarkdown},
		documentChunks(2)); err == nil {
		t.Fatal("an index that refused the vectors reported success")
	}
	stored, err := store.ListDocuments(ctx, "user-1", collectionID)
	if err != nil {
		t.Fatalf("ListDocuments: %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("documents after a failed index = %d, want the document kept", len(stored))
	}
	failed := stored[0]
	if failed.IndexStatus != domain.DocumentStatusFailed {
		t.Errorf("index status = %q, want failed", failed.IndexStatus)
	}
	if failed.ActiveVersion != 0 || failed.Retrievable() {
		t.Errorf("a document whose index write failed is retrievable: %#v", failed)
	}
	if chunks, err := synchronizer.ChunksFor(ctx, "user-1", []string{collectionID}); err != nil || len(chunks) != 0 {
		t.Errorf("chunks of a failed version = %#v (%v), want nothing searchable", chunks, err)
	}
	// The recorded reason is bounded and carries none of the index's own wording, so
	// a client reading the document back cannot be handed a downstream error.
	if failed.ErrorMessage == "" || strings.Contains(failed.ErrorMessage, "index down") {
		t.Errorf("error message = %q, want a recorded reason without the upstream text", failed.ErrorMessage)
	}
	if err := synchronizer.RetryFailed(ctx, "user-1", failed.ID); err != nil {
		t.Fatalf("retry of a failed document: %v", err)
	}
	// Only a failed document is retryable, which is what stops a retry from racing a
	// pass that is still running.
	if err := synchronizer.RetryFailed(ctx, "user-1", failed.ID); !errors.Is(err, knowledge.ErrDocumentNotRetryable) {
		t.Errorf("second retry from pending = %v, want only a failed document retryable", err)
	}
	// Re-submitting the same content after a failed index must be worked on, never
	// reported as a no-op: short-circuiting there is what would strand a document with
	// no vectors forever. The driver's path is claim then complete, and the decorator
	// is what does the indexing.
	writer.upsertErr = nil
	claimed, err := synchronizer.ClaimForIndexing(ctx, "user-1", failed.ID, failed.Version)
	if err != nil || !claimed {
		t.Fatalf("claim after retry = (%v, %v), want the retried version claimable", claimed, err)
	}
	if err := synchronizer.CompleteIndexing(ctx, "user-1", failed.ID, failed.Version, knowledge.IndexOutcome{
		Status: domain.DocumentStatusReady, ChunkCount: 2,
	}); err != nil {
		t.Fatalf("re-driving the failed version: %v", err)
	}
	published, err := store.GetDocument(ctx, "user-1", failed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if published.IndexStatus != domain.DocumentStatusReady || published.ActiveVersion != 1 {
		t.Fatalf("document after the retry = %#v, want it published", published)
	}
	if chunks, err := synchronizer.ChunksFor(ctx, "user-1", []string{collectionID}); err != nil || len(chunks) != 2 {
		t.Errorf("chunks after the retry = %#v (%v), want the version searchable", chunks, err)
	}
}

// TestSynchronizerReportsAnEmbedderThatRefusesToAnswer keeps the two properties that
// were already right - the embedder's error surfaces as itself, and no vectors are
// written from a failed embedding - and replaces the vanished-document expectation with
// the recorded-failure state above.
func TestSynchronizerReportsAnEmbedderThatRefusesToAnswer(t *testing.T) {
	ctx := context.Background()
	store, collectionID := newTestStores(t)
	writer := &fakeWriter{dimensions: 4}
	synchronizer, err := NewSynchronizer(store, writer, &stubEmbedder{dimensions: 4, err: retrieval.ErrEmptyQuery}, "chunks-v1")
	if err != nil {
		t.Fatalf("NewSynchronizer: %v", err)
	}
	if _, err := synchronizer.CreateDocument(ctx, "user-1",
		&domain.Document{CollectionID: collectionID, Title: "x", Format: domain.DocumentFormatMarkdown},
		documentChunks(1)); !errors.Is(err, retrieval.ErrEmptyQuery) {
		t.Fatalf("error = %v, want the embedder failure to surface unchanged", err)
	}
	if len(writer.upserts) != 0 {
		t.Fatal("vectors were written from a failed embedding")
	}
	documents, _ := store.ListDocuments(ctx, "user-1", collectionID)
	if len(documents) != 1 || documents[0].IndexStatus != domain.DocumentStatusFailed {
		t.Fatalf("documents after a failed embed = %#v, want one recorded failure", documents)
	}
}

func TestSynchronizerAdvancesVersionThenRetiresTheOldOne(t *testing.T) {
	store, collectionID := newTestStores(t)
	writer := &fakeWriter{dimensions: 4}
	synchronizer, err := NewSynchronizer(store, writer, &stubEmbedder{dimensions: 4}, "chunks-v1")
	if err != nil {
		t.Fatalf("NewSynchronizer: %v", err)
	}
	created, err := synchronizer.CreateDocument(context.Background(), "user-1",
		&domain.Document{CollectionID: collectionID, Title: "x", Format: domain.DocumentFormatMarkdown},
		documentChunks(2))
	if err != nil {
		t.Fatalf("CreateDocument: %v", err)
	}
	replaced, err := synchronizer.ReplaceDocumentContent(context.Background(), "user-1", created.ID,
		knowledge.DocumentReplacement{Format: domain.DocumentFormatMarkdown, Chunks: documentChunks(3)})
	if err != nil {
		t.Fatalf("ReplaceDocumentContent: %v", err)
	}
	if replaced.Version != created.Version+1 {
		t.Fatalf("version went %d to %d, want a single advance", created.Version, replaced.Version)
	}
	if len(writer.upserts) != 2 || len(writer.upserts[1]) != 3 {
		t.Fatalf("index saw %d upserts with %d new entries, want 2 calls and 3 new chunks",
			len(writer.upserts), len(writer.upserts[1]))
	}
	if len(writer.kept) != 1 || writer.kept[0] != replaced.Version {
		t.Fatalf("sweep kept %v, want only the new version %d", writer.kept, replaced.Version)
	}

	// A sweep that fails must be reported: the stale version is still searchable.
	failing := &fakeWriter{dimensions: 4, versionErr: errors.New("redis down")}
	failingStore, failingCollection := newTestStores(t)
	failingSync, err := NewSynchronizer(failingStore, failing, &stubEmbedder{dimensions: 4}, "chunks-v1")
	if err != nil {
		t.Fatalf("NewSynchronizer: %v", err)
	}
	first, err := failingSync.CreateDocument(context.Background(), "user-1",
		&domain.Document{CollectionID: failingCollection, Title: "x", Format: domain.DocumentFormatMarkdown},
		documentChunks(1))
	if err != nil {
		t.Fatalf("CreateDocument: %v", err)
	}
	if _, err := failingSync.ReplaceDocumentContent(context.Background(), "user-1", first.ID,
		knowledge.DocumentReplacement{Format: domain.DocumentFormatMarkdown, Chunks: documentChunks(2)}); err == nil || !strings.Contains(err.Error(), "left behind") {
		t.Fatalf("error = %v, want the abandoned old version to be named", err)
	}
}

func TestSynchronizerDeleteAndReindex(t *testing.T) {
	store, collectionID := newTestStores(t)
	writer := &fakeWriter{dimensions: 4}
	synchronizer, err := NewSynchronizer(store, writer, &stubEmbedder{dimensions: 4}, "chunks-v1")
	if err != nil {
		t.Fatalf("NewSynchronizer: %v", err)
	}
	created, err := synchronizer.CreateDocument(context.Background(), "user-1",
		&domain.Document{CollectionID: collectionID, Title: "x", Format: domain.DocumentFormatMarkdown},
		documentChunks(2))
	if err != nil {
		t.Fatalf("CreateDocument: %v", err)
	}
	if err := synchronizer.DeleteDocument(context.Background(), "user-1", created.ID); err != nil {
		t.Fatalf("DeleteDocument: %v", err)
	}
	if len(writer.deletes) != 1 || writer.deletes[0] != "user-1/"+created.ID {
		t.Fatalf("deletes = %v, want the document's vectors removed", writer.deletes)
	}
	// A document nobody owns must not touch the index at all.
	if err := synchronizer.DeleteDocument(context.Background(), "user-1", "missing-doc"); err == nil {
		t.Fatal("deleting a document that does not exist reported success")
	}
	if len(writer.deletes) != 1 {
		t.Fatalf("deletes = %v, want no extra index work for a failed delete", writer.deletes)
	}

	reindexSync, err := NewSynchronizer(store, writer, &stubEmbedder{dimensions: 4}, "chunks-v1")
	if err != nil {
		t.Fatalf("NewSynchronizer: %v", err)
	}
	for _, title := range []string{"one", "two"} {
		if _, err := reindexSync.CreateDocument(context.Background(), "user-1",
			&domain.Document{CollectionID: collectionID, Title: title, Format: domain.DocumentFormatMarkdown},
			documentChunks(1)); err != nil {
			t.Fatalf("CreateDocument: %v", err)
		}
	}
	indexed, err := reindexSync.Reindex(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("Reindex: %v", err)
	}
	if indexed != 2 {
		t.Fatalf("Reindex covered %d documents, want the two that exist", indexed)
	}
	// Reindex sweeps once per document, keeping that document's current version.
	if len(writer.kept) != 2 {
		t.Fatalf("sweeps = %d, want one per reindexed document", len(writer.kept))
	}
}

func TestDescribeClassifiesTransportAndHonoursTheDeadline(t *testing.T) {
	ctx := context.Background()
	if describe(ctx, nil) != nil {
		t.Fatal("a nil error must stay nil")
	}
	if !errors.Is(describe(ctx, errors.New("dial tcp 127.0.0.1:6379: connection refused")), ErrUnavailable) {
		t.Error("a refused connection must read as unavailable")
	}
	if errors.Is(describe(ctx, errors.New("ERR wrong number of arguments for FT.CREATE")), ErrUnavailable) {
		t.Error("a refused command is a bug, not an outage")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if !errors.Is(describe(cancelled, errors.New("context canceled")), context.Canceled) {
		t.Error("the caller's cancellation must win over the transport classification")
	}
}
