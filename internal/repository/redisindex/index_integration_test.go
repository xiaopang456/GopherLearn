package redisindex

import (
	"context"
	"errors"
	"math"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/knowledge"
	"github.com/xiaopang456/GopherLearn/internal/retrieval"
)

// The real-server suite is gated the same way the MySQL suite is: an ordinary
// `go test ./...` runs every other file in this package against fakes and never dials
// anything. Set GOPHERLEARN_REDIS_INDEX_ADDRESS to a throwaway Redis Stack with
// RediSearch, and the same assertions run against the real FT.CREATE / HSET / FT.SEARCH
// path.
//
// "Throwaway" is not a suggestion here. The search module creates its index in database 0
// and nowhere else, which is measured below rather than assumed, so this suite cannot keep
// its writes off whatever a shared server is already holding. Isolation inside one server
// comes from the key prefix and the index name; isolation from other people's data comes
// from a container this run owns and deletes.
//
// The suite writes under gopherlearn:knowledge:chunk: and deletes by that pattern for its
// own user ids only.
func TestIndexAgainstRealRedis(t *testing.T) {
	address := strings.TrimSpace(os.Getenv("GOPHERLEARN_REDIS_INDEX_ADDRESS"))
	if address == "" {
		t.Skip("set GOPHERLEARN_REDIS_INDEX_ADDRESS to run the RediSearch suite")
	}
	const dimensions = 8
	index, err := New(Options{Address: address, Dimensions: dimensions, Database: redisDatabaseForSuite(t)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = index.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if err := index.EnsureIndex(ctx); err != nil {
		t.Fatalf("EnsureIndex: %v", err)
	}
	if err := index.EnsureIndex(ctx); err != nil {
		t.Fatalf("EnsureIndex twice (a second start must tolerate an existing index): %v", err)
	}

	// Two owners, each with two chunks whose vectors are deliberately chosen so the
	// nearest neighbour of the query is known before the server answers.
	identity := "test-identity-a"
	otherIdentity := "test-identity-b"
	owner := "user-index-" + time.Now().UTC().Format("150405")
	neighbour := "user-other-" + time.Now().UTC().Format("150405")
	queries := []retrieval.VectorEntry{
		{
			Identity: identity,
			Chunk:    domain.DocumentChunk{ID: "chunk-near", DocumentID: "doc-1", CollectionID: "col-1", Version: 1, Order: 1},
			Vector:   axis(0, dimensions),
		},
		{
			Identity: identity,
			Chunk:    domain.DocumentChunk{ID: "chunk-far", DocumentID: "doc-1", CollectionID: "col-1", Version: 1, Order: 2},
			Vector:   axis(1, dimensions),
		},
		{
			Identity: otherIdentity,
			Chunk:    domain.DocumentChunk{ID: "chunk-other-user", DocumentID: "doc-2", CollectionID: "col-2", Version: 1, Order: 1},
			Vector:   axis(0, dimensions),
		},
	}
	if err := index.Upsert(ctx, owner, queries[:2]); err != nil {
		t.Fatalf("Upsert owner: %v", err)
	}
	if err := index.Upsert(ctx, neighbour, queries[2:]); err != nil {
		t.Fatalf("Upsert neighbour: %v", err)
	}

	search := func(userID, wantIdentity string, limit int) []retrieval.VectorHit {
		t.Helper()
		hits, err := index.Search(ctx, retrieval.VectorSearch{
			UserID: userID, CollectionIDs: []string{"col-1", "col-2"},
			Vector: axis(0, dimensions), Identity: wantIdentity, Limit: limit,
		})
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		return hits
	}

	hits := search(owner, identity, 5)
	if len(hits) != 2 {
		t.Fatalf("owner saw %d hits (%+v), want both of their own chunks", len(hits), hits)
	}
	if hits[0].ChunkID != "chunk-near" {
		t.Fatalf("first hit = %q, want the chunk on the query's own axis", hits[0].ChunkID)
	}
	if !(hits[0].Similarity > hits[1].Similarity) {
		t.Fatalf("ordering is not best-first: %+v", hits)
	}
	// A COSINE distance of 0 for the identical axis direction is the check that the byte
	// packing and the distance-to-similarity conversion agree with the server.
	if math.Abs(hits[0].Similarity-1) > 1e-6 {
		t.Fatalf("similarity of the aligned vector = %v, want 1", hits[0].Similarity)
	}
	if math.Abs(hits[1].Similarity-0) > 1e-6 {
		t.Fatalf("similarity of the orthogonal vector = %v, want 0", hits[1].Similarity)
	}
	for _, hit := range search(neighbour, otherIdentity, 5) {
		if hit.ChunkID == "chunk-near" || hit.ChunkID == "chunk-far" {
			t.Fatalf("owner isolation failed: %q was recalled by another user (%+v)", hit.ChunkID, hits)
		}
	}
	if wrong := search(owner, otherIdentity, 5); len(wrong) != 0 {
		t.Fatalf("identity isolation failed: an index built by another embedder answered with %+v", wrong)
	}

	// A replaced document retires its old version and keeps the new one.
	replaced := []retrieval.VectorEntry{
		{
			Identity: identity,
			Chunk:    domain.DocumentChunk{ID: "chunk-near", DocumentID: "doc-1", CollectionID: "col-1", Version: 2, Order: 1},
			Vector:   axis(2, dimensions),
		},
	}
	if err := index.Upsert(ctx, owner, replaced); err != nil {
		t.Fatalf("Upsert new version: %v", err)
	}
	if err := index.DeleteVersions(ctx, owner, "doc-1", 2); err != nil {
		t.Fatalf("DeleteVersions: %v", err)
	}
	after := search(owner, identity, 5)
	for _, hit := range after {
		if hit.ChunkID == "chunk-far" {
			t.Fatalf("the retired version is still recallable: %+v", after)
		}
	}

	if err := index.DeleteDocument(ctx, owner, "doc-1"); err != nil {
		t.Fatalf("DeleteDocument: %v", err)
	}
	if remaining := search(owner, identity, 5); len(remaining) != 0 {
		t.Fatalf("a deleted document still answers: %+v", remaining)
	}
}

// TestSynchronizerAgainstRealRedis drives the whole write path through a
// knowledge.Store so the decorator, the byte layout and the KNN query are proved
// together - and so "no chunks published" is caught as an error rather than as an
// empty index.
func TestSynchronizerAgainstRealRedis(t *testing.T) {
	address := strings.TrimSpace(os.Getenv("GOPHERLEARN_REDIS_INDEX_ADDRESS"))
	if address == "" {
		t.Skip("set GOPHERLEARN_REDIS_INDEX_ADDRESS to run the RediSearch suite")
	}
	const dimensions = 8
	index, err := New(Options{Address: address, Dimensions: dimensions, Database: redisDatabaseForSuite(t)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = index.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := index.EnsureIndex(ctx); err != nil {
		t.Fatalf("EnsureIndex: %v", err)
	}

	store := knowledge.NewMemoryStore()
	collection, err := store.CreateCollection(ctx, &domain.Collection{UserID: "user-sync", Title: "sync"})
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	embedder := &unitEmbedder{dimensions: dimensions}
	synchronizer, err := NewSynchronizer(store, index, embedder, "chunks-test-v1")
	if err != nil {
		t.Fatalf("NewSynchronizer: %v", err)
	}
	created, err := synchronizer.CreateDocument(ctx, "user-sync",
		&domain.Document{CollectionID: collection.ID, Title: "notes", Format: domain.DocumentFormatMarkdown},
		[]domain.DocumentChunk{
			{Order: 1, StartLine: 1, EndLine: 4, Text: "第一条内容", Section: "一"},
			{Order: 2, StartLine: 5, EndLine: 8, Text: "第二条内容", Section: "二"},
		})
	if err != nil {
		t.Fatalf("CreateDocument: %v", err)
	}
	if created.Version != 1 {
		t.Fatalf("version = %d, want 1", created.Version)
	}
	// One vector is axis 0 (aligned with every stored text's first component), so the
	// nearest neighbour must be the chunk whose text the embedder mapped there.
	hits, err := index.Search(ctx, retrieval.VectorSearch{
		UserID: "user-sync", CollectionIDs: []string{collection.ID},
		Vector: axis(0, dimensions), Identity: synchronizer.Identity(), Limit: 4,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("index returned %d hits, want both stored chunks", len(hits))
	}
	stored, err := store.ChunksFor(ctx, "user-sync", []string{collection.ID})
	if err != nil {
		t.Fatalf("ChunksFor: %v", err)
	}
	if hits[0].ChunkID != stored[0].ID || hits[1].ChunkID != stored[1].ID {
		t.Fatalf("index ids are not the store's: %q,%q vs %q,%q", hits[0].ChunkID, hits[1].ChunkID, stored[0].ID, stored[1].ID)
	}

	if _, err := synchronizer.Reindex(ctx, "user-sync"); err != nil {
		t.Fatalf("Reindex: %v", err)
	}
	if err := synchronizer.DeleteDocument(ctx, "user-sync", created.ID); err != nil {
		t.Fatalf("DeleteDocument: %v", err)
	}
	after, err := index.Search(ctx, retrieval.VectorSearch{
		UserID: "user-sync", CollectionIDs: []string{collection.ID},
		Vector: axis(0, dimensions), Identity: synchronizer.Identity(), Limit: 4,
	})
	if err != nil {
		t.Fatalf("Search after delete: %v", err)
	}
	if len(after) != 0 {
		t.Fatalf("deleted document still answers from the index: %+v", after)
	}
}

// redisDatabaseForSuite reads the database the gated suite should use. There is nothing to
// choose: measured against a real server, FT.CREATE answers "Cannot create index on db != 0",
// so the module only ever indexes database 0 and isolation comes from the key prefix, the
// index name and a throwaway server. The variable exists to catch a leftover setting - an
// operator who exports a non-zero number is told the reason instead of watching the suite
// search an index that was never created.
func redisDatabaseForSuite(t *testing.T) int {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv("GOPHERLEARN_REDIS_INDEX_DATABASE"))
	if raw == "" || raw == "0" {
		return 0
	}
	t.Fatalf("GOPHERLEARN_REDIS_INDEX_DATABASE=%q is unusable: RediSearch creates its index in database 0, so run this suite against a throwaway server", raw)
	return 0
}

func TestUpsertAndSearchRefuseTheWrongWidth(t *testing.T) {
	index, err := New(Options{Address: "127.0.0.1:6379", Dimensions: 4})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = index.Close() }()
	ctx := context.Background()
	err = index.Upsert(ctx, "user-1", []retrieval.VectorEntry{{
		Chunk: domain.DocumentChunk{ID: "chunk-a"}, Vector: []float32{1, 2, 3, 4, 5},
	}})
	if !errors.Is(err, ErrConfig) {
		t.Fatalf("upsert of a wrong-width vector: error = %v, want ErrConfig", err)
	}
	if _, err := index.Search(ctx, retrieval.VectorSearch{
		UserID: "user-1", CollectionIDs: []string{"col-1"}, Vector: []float32{1, 2}, Identity: "i",
	}); !errors.Is(err, ErrConfig) {
		t.Fatalf("search of a wrong-width vector: error = %v, want ErrConfig", err)
	}
}

// axis returns a unit vector with a single 1 at the given position. Orthogonal axes are
// cosine distance 1 apart, which makes the expected ordering exact rather than approximate.
func axis(position, dimensions int) []float32 {
	vector := make([]float32, dimensions)
	if position < dimensions {
		vector[position] = 1
	}
	return vector
}

type unitEmbedder struct {
	dimensions int
	calls      int
}

func (e *unitEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	e.calls++
	vectors := make([][]float32, 0, len(texts))
	for index := range texts {
		vectors = append(vectors, axis(index%e.dimensions, e.dimensions))
	}
	return vectors, nil
}

func (e *unitEmbedder) Dimensions() int { return e.dimensions }
func (e *unitEmbedder) Model() string   { return "unit-axis-test-embedder" }

// TestEscapedTagValuesRoundTripThroughTheServer is the reason the escape list is what it
// is. An unescaped dash inside a tag literal makes the query parser fail outright
// ("Syntax error at offset 14 near user"), which is loud; the opposite mistake - escaping
// something the server then stores differently - fails as "no results", which is silent and
// looks like a retrieval-quality problem. Only a real server can tell those apart, so a
// real server says so here: every identifier below is written, then recalled by a query
// that must find it, and a neighbour's identical-shaped identifier must not be.
func TestEscapedTagValuesRoundTripThroughTheServer(t *testing.T) {
	address := strings.TrimSpace(os.Getenv("GOPHERLEARN_REDIS_INDEX_ADDRESS"))
	if address == "" {
		t.Skip("set GOPHERLEARN_REDIS_INDEX_ADDRESS to run the RediSearch suite")
	}
	const dimensions = 4
	index, err := New(Options{
		Address: address, Dimensions: dimensions, Database: redisDatabaseForSuite(t),
		// Its own index and its own key prefix, so this test does not depend on which of
		// the other gated tests ran first, and a stale 8-dimensional index cannot make it
		// pass or fail by accident.
		IndexName: "gopherlearn_11a_probe_idx", KeyPrefix: "gopherlearn:knowledge:probe:",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = index.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := index.EnsureIndex(ctx); err != nil {
		t.Fatalf("EnsureIndex: %v", err)
	}

	// The shapes this project actually produces: dashes everywhere, dots and '@' in an
	// email-shaped owner, underscores in a slug, and a hex vector identity.
	const identity = "1f6c1e0a4b7d2e33"
	owners := []string{"probe-user-index-1", "probe.user_2@mail.example", "probe-user-3"}
	for position, owner := range owners {
		entry := []retrieval.VectorEntry{{
			Identity: identity,
			Chunk: domain.DocumentChunk{
				ID: "probe-chunk-" + strconv.Itoa(position), DocumentID: "probe-doc-1",
				CollectionID: "probe_col.1", Version: 1, Order: position,
			},
			Vector: axis(position%dimensions, dimensions),
		}}
		if err := index.Upsert(ctx, owner, entry); err != nil {
			t.Fatalf("Upsert for %q: %v", owner, err)
		}
	}
	for position, owner := range owners {
		hits, err := index.Search(ctx, retrieval.VectorSearch{
			UserID: owner, CollectionIDs: []string{"probe_col.1"},
			Vector: axis(position%dimensions, dimensions), Identity: identity, Limit: 5,
		})
		if err != nil {
			t.Fatalf("Search for %q: %v", owner, err)
		}
		if len(hits) != 1 || hits[0].ChunkID != "probe-chunk-"+strconv.Itoa(position) {
			t.Fatalf("owner %q saw %+v, want exactly its own chunk", owner, hits)
		}
		if math.Abs(hits[0].Similarity-1) > 1e-6 {
			t.Fatalf("owner %q: similarity = %v, want its own axis to score 1", owner, hits[0].Similarity)
		}
	}

	// The write path refuses the two values that cannot be represented safely, instead of
	// storing something a later query would match for the wrong reason. A batch has to carry
	// a chunk for the guard to be reached at all: an empty one writes nothing.
	probe := []retrieval.VectorEntry{{
		Identity: identity,
		Chunk:    domain.DocumentChunk{ID: "probe-chunk-x", DocumentID: "probe-doc-1", CollectionID: "probe_col.1", Version: 1},
		Vector:   axis(0, dimensions),
	}}
	if err := index.Upsert(ctx, "probe,user", probe); !errors.Is(err, ErrConfig) {
		t.Errorf("an owner containing the tag separator was accepted: %v", err)
	}
	if err := index.Upsert(ctx, "probe*user", probe); !errors.Is(err, ErrConfig) {
		t.Errorf("an owner containing a SCAN pattern character was accepted: %v", err)
	}
}

// TestEnsureIndexReportsAnIndexBuiltForAnotherWidth is the negative control for the probe
// EnsureIndex runs. Without it, a deployment that changed GOPHERLEARN_EMBEDDING_DIMENSIONS
// - or that points at an index another build created - starts healthy and then fails inside
// a learner's question, which is the worst possible place to discover a schema disagreement.
func TestEnsureIndexReportsAnIndexBuiltForAnotherWidth(t *testing.T) {
	address := strings.TrimSpace(os.Getenv("GOPHERLEARN_REDIS_INDEX_ADDRESS"))
	if address == "" {
		t.Skip("set GOPHERLEARN_REDIS_INDEX_ADDRESS to run the RediSearch suite")
	}
	// The probe suite above uses this index name at four dimensions. Create it first when
	// that suite is filtered out, so this test measures the mismatch rather than whichever
	// order the file happened to run in.
	const probeIndex = "gopherlearn_11a_probe_idx"
	const probePrefix = "gopherlearn:knowledge:probe:"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	right, err := New(Options{
		Address: address, Dimensions: 4, Database: redisDatabaseForSuite(t),
		IndexName: probeIndex, KeyPrefix: probePrefix,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = right.Close() }()
	if err := right.EnsureIndex(ctx); err != nil {
		t.Fatalf("EnsureIndex at the matching width: %v", err)
	}

	wrong, err := New(Options{
		Address: address, Dimensions: 8, Database: redisDatabaseForSuite(t),
		IndexName: probeIndex, KeyPrefix: probePrefix,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = wrong.Close() }()
	err = wrong.EnsureIndex(ctx)
	if !errors.Is(err, ErrConfig) {
		t.Fatalf("EnsureIndex accepted a foreign width: %v", err)
	}
	if !strings.Contains(err.Error(), "8-dimensional") {
		t.Errorf("error %q does not name the width this build asked for", err)
	}
	// And the matching width still works afterwards: the refusal above must not be the
	// probe failing for some unrelated reason that would also break a real start.
	if err := right.EnsureIndex(ctx); err != nil {
		t.Fatalf("EnsureIndex after the refusal: %v", err)
	}
}
