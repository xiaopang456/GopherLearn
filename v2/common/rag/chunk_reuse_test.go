package rag

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/embedding"
)

const reuseTestModel = "test-embedding-model"

// TestChunkReuseSingleChunkEditEmbedsOnce is the acceptance measurement for
// STABILIZE-028: with stable chunk boundaries, editing one paragraph must cost
// one embedding call worth of vectors instead of N.
func TestChunkReuseSingleChunkEditEmbedsOnce(t *testing.T) {
	t.Parallel()

	const sections = 6
	oldChunks := reuseDocumentChunks(t, "doc-single-edit", 1, reuseMarkdown(sections, -1, ""))
	newChunks := reuseDocumentChunks(t, "doc-single-edit", 2, reuseMarkdown(sections, 3, "第3节被改写为新的内容。"))
	if len(oldChunks) != sections || len(newChunks) != sections {
		t.Fatalf("prepared %d old and %d new chunks, want %d", len(oldChunks), len(newChunks), sections)
	}

	repository := &fakeChunkVectorRepository{loadResult: storedFromChunks(t, 7, oldChunks, reuseTestModel, 4)}
	embedder := &countingChunkEmbedder{dimension: 4}
	indexer := mustKnowledgeChunkIndexer(t, embedder, repository, testIndexConfig(4, 10))

	result, err := indexer.Index(context.Background(), ChunkIndexRequest{UserID: 7, Chunks: newChunks, ReuseFromVersion: 1})
	if err != nil {
		t.Fatalf("Index() error: %v", err)
	}
	if result.TotalChunks != sections || result.ReusedChunks != sections-1 || result.EmbeddedChunks != 1 {
		t.Fatalf("single edit result = total %d, reused %d, embedded %d; want %d/%d/1",
			result.TotalChunks, result.ReusedChunks, result.EmbeddedChunks, sections, sections-1)
	}
	if result.ReuseStatus != ChunkReuseApplied || result.FallbackReason != "" {
		t.Fatalf("reuse status/reason = %q/%q", result.ReuseStatus, result.FallbackReason)
	}
	if embedder.textCount != 1 {
		t.Fatalf("embedder received %d texts, want 1", embedder.textCount)
	}
	assertReuseAccounting(t, result)
	assertStoredCompleteVersion(t, repository, newChunks, 7)

	// The reuse source must be the old ActiveVersion of the same user document.
	want := []loadedVersionKey{{7, "doc-single-edit", 1}}
	if !reflect.DeepEqual(repository.loadedFrom, want) {
		t.Fatalf("reuse source loads = %+v, want %+v", repository.loadedFrom, want)
	}
}

// TestChunkReuseCarriesVectorBytesUnchanged proves reuse is not an
// approximation: a carried-over vector is byte-identical to what the embedder
// would return, so retrieval input does not change.
func TestChunkReuseCarriesVectorBytesUnchanged(t *testing.T) {
	t.Parallel()

	const sections = 4
	oldChunks := reuseDocumentChunks(t, "doc-bytes", 1, reuseMarkdown(sections, -1, ""))
	newChunks := reuseDocumentChunks(t, "doc-bytes", 2, reuseMarkdown(sections, 0, "第0节改写。"))

	repository := &fakeChunkVectorRepository{loadResult: storedFromChunks(t, 7, oldChunks, reuseTestModel, 4)}
	indexer := mustKnowledgeChunkIndexer(t, &countingChunkEmbedder{dimension: 4}, repository, testIndexConfig(4, 10))
	if _, err := indexer.Index(context.Background(), ChunkIndexRequest{UserID: 7, Chunks: newChunks, ReuseFromVersion: 1}); err != nil {
		t.Fatalf("Index() error: %v", err)
	}
	for _, stored := range repository.stored {
		want := deterministicVector(stored.Chunk.EmbeddingContent, 4)
		if !reflect.DeepEqual(stored.Vector, want) {
			t.Fatalf("chunk %d vector differs from a freshly embedded vector", stored.Chunk.Index)
		}
		if stored.EmbeddingInputHash != EmbeddingInputHash(stored.Chunk.EmbeddingContent) {
			t.Fatalf("chunk %d stored an input hash that does not describe its own content", stored.Chunk.Index)
		}
		if stored.EmbeddingFingerprint != EmbeddingFingerprint(reuseTestModel, 4, EmbeddingContentVersion) {
			t.Fatalf("chunk %d stored fingerprint %q", stored.Chunk.Index, stored.EmbeddingFingerprint)
		}
	}
}

// TestChunkReuseSurvivesChunkIDAndLineShift covers the reason ChunkID and
// ContentHash cannot be the reuse identity: inserting a section rewrites both
// while the embedder input of untouched sections stays byte-identical.
func TestChunkReuseSurvivesChunkIDAndLineShift(t *testing.T) {
	t.Parallel()

	const sections = 5
	oldChunks := reuseDocumentChunks(t, "doc-insert", 1, reuseMarkdown(sections, -1, ""))
	inserted := "## 新增章节\n插入的新内容。\n\n" + reuseMarkdown(sections, -1, "")
	newChunks := reuseDocumentChunks(t, "doc-insert", 2, inserted)
	if len(newChunks) != sections+1 {
		t.Fatalf("insertion produced %d chunks, want %d", len(newChunks), sections+1)
	}
	sharedIDs := 0
	for _, newChunk := range newChunks {
		for _, oldChunk := range oldChunks {
			if newChunk.ID == oldChunk.ID {
				sharedIDs++
			}
		}
	}
	if sharedIDs != 0 {
		t.Fatalf("%d chunk IDs survived the version bump; the test would not prove anything", sharedIDs)
	}

	repository := &fakeChunkVectorRepository{loadResult: storedFromChunks(t, 7, oldChunks, reuseTestModel, 4)}
	embedder := &countingChunkEmbedder{dimension: 4}
	indexer := mustKnowledgeChunkIndexer(t, embedder, repository, testIndexConfig(4, 10))
	result, err := indexer.Index(context.Background(), ChunkIndexRequest{UserID: 7, Chunks: newChunks, ReuseFromVersion: 1})
	if err != nil {
		t.Fatalf("Index() error: %v", err)
	}
	if result.ReusedChunks != sections || result.EmbeddedChunks != 1 {
		t.Fatalf("insertion result = reused %d, embedded %d; want %d/1", result.ReusedChunks, result.EmbeddedChunks, sections)
	}
	assertReuseAccounting(t, result)
}

// TestChunkReuseRejectsEmbeddingContextChanges guards the fields that are part
// of the embedder input but not part of RawContent: file name, title path and
// symbols. A ContentHash-based cache would wrongly reuse all of these.
func TestChunkReuseRejectsEmbeddingContextChanges(t *testing.T) {
	t.Parallel()

	body := "## 只有一节\n内容保持不变，但周围元数据变化。\n"
	cases := []struct {
		name       string
		sourceFile string
		markdown   string
	}{
		{"renamed file", "renamed.md", body},
		{"changed title path", "reuse.md", "# 新的一级标题\n\n" + body},
		{"changed symbols", "reuse.md", "## 只有一节\n内容保持不变，但周围元数据变化。\n\n```go\nfunc NewlyAddedSymbol() {}\n```\n"},
	}
	oldChunks := reuseChunksWithSource(t, "doc-context", 1, "reuse.md", body)
	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			newChunks := reuseChunksWithSource(t, "doc-context", 2, testCase.sourceFile, testCase.markdown)
			repository := &fakeChunkVectorRepository{loadResult: storedFromChunks(t, 7, oldChunks, reuseTestModel, 4)}
			indexer := mustKnowledgeChunkIndexer(t, &countingChunkEmbedder{dimension: 4}, repository, testIndexConfig(4, 10))
			result, err := indexer.Index(context.Background(), ChunkIndexRequest{UserID: 7, Chunks: newChunks, ReuseFromVersion: 1})
			if err != nil {
				t.Fatalf("Index() error: %v", err)
			}
			if result.ReusedChunks != 0 {
				t.Fatalf("%s reused %d chunks, want 0", testCase.name, result.ReusedChunks)
			}
			assertReuseAccounting(t, result)
		})
	}
}

func TestChunkReuseSkippedForFirstUploadAndExplicitReindex(t *testing.T) {
	t.Parallel()

	chunks := reuseDocumentChunks(t, "doc-skip", 1, reuseMarkdown(3, -1, ""))
	cases := []struct {
		name             string
		reuseFromVersion int
	}{
		{"first upload", 0},
		{"explicit reindex", 1},
	}
	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			// The reuse source deliberately holds a perfect match. Reindex must
			// still recompute, otherwise it could not repair damaged vectors.
			repository := &fakeChunkVectorRepository{loadResult: storedFromChunks(t, 7, chunks, reuseTestModel, 4)}
			embedder := &countingChunkEmbedder{dimension: 4}
			indexer := mustKnowledgeChunkIndexer(t, embedder, repository, testIndexConfig(4, 10))
			result, err := indexer.Index(context.Background(), ChunkIndexRequest{UserID: 7, Chunks: chunks, ReuseFromVersion: testCase.reuseFromVersion})
			if err != nil {
				t.Fatalf("Index() error: %v", err)
			}
			if result.ReusedChunks != 0 || result.EmbeddedChunks != len(chunks) || embedder.textCount != len(chunks) {
				t.Fatalf("%s reused %d, embedded %d, texts %d", testCase.name, result.ReusedChunks, result.EmbeddedChunks, embedder.textCount)
			}
			if result.ReuseStatus != ChunkReuseNotApplicable || repository.loadCalls != 0 {
				t.Fatalf("%s status %q with %d reuse source loads", testCase.name, result.ReuseStatus, repository.loadCalls)
			}
			assertReuseAccounting(t, result)
		})
	}
}

func TestChunkReuseFallsBackWhenSourceIsUnusable(t *testing.T) {
	t.Parallel()

	oldChunks := reuseDocumentChunks(t, "doc-fallback", 1, reuseMarkdown(4, -1, ""))
	newChunks := reuseDocumentChunks(t, "doc-fallback", 2, reuseMarkdown(4, 1, "第1节改写。"))
	legacy := storedFromChunks(t, 7, oldChunks, reuseTestModel, 4)
	for index := range legacy {
		// A chunk written before this feature existed: vector present, identity
		// metadata absent. It is not corruption, so it must not fail the update.
		legacy[index].EmbeddingInputHash = ""
		legacy[index].EmbeddingFingerprint = ""
	}

	cases := []struct {
		name       string
		repository *fakeChunkVectorRepository
		wantStatus string
		wantReason string
	}{
		{"redis read failure", &fakeChunkVectorRepository{loadErr: errors.New("redis unavailable")}, ChunkReuseFallback, ChunkReuseReasonSourceLoadFailed},
		{"legacy chunks without metadata", &fakeChunkVectorRepository{loadResult: legacy}, ChunkReuseLegacyMetadata, ChunkReuseReasonNoUsableMetadata},
		{"empty reuse source", &fakeChunkVectorRepository{}, ChunkReuseLegacyMetadata, ChunkReuseReasonNoUsableMetadata},
	}
	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			embedder := &countingChunkEmbedder{dimension: 4}
			indexer := mustKnowledgeChunkIndexer(t, embedder, testCase.repository, testIndexConfig(4, 10))
			result, err := indexer.Index(context.Background(), ChunkIndexRequest{UserID: 7, Chunks: newChunks, ReuseFromVersion: 1})
			if err != nil {
				t.Fatalf("%s must degrade, not fail: %v", testCase.name, err)
			}
			if result.ReusedChunks != 0 || result.EmbeddedChunks != len(newChunks) || embedder.textCount != len(newChunks) {
				t.Fatalf("%s reused %d, embedded %d", testCase.name, result.ReusedChunks, result.EmbeddedChunks)
			}
			if result.ReuseStatus != testCase.wantStatus || result.FallbackReason != testCase.wantReason {
				t.Fatalf("%s status/reason = %q/%q, want %q/%q", testCase.name, result.ReuseStatus, result.FallbackReason, testCase.wantStatus, testCase.wantReason)
			}
			// A fallback still writes the complete new version with fresh metadata,
			// so the next update can reuse it.
			assertStoredCompleteVersion(t, testCase.repository, newChunks, 7)
			assertReuseAccounting(t, result)
		})
	}
}

// TestChunkReuseRejectsChangedEmbeddingIdentity covers the three inputs of the
// fingerprint. Reusing across any of them would mix vector spaces inside one
// document version.
func TestChunkReuseRejectsChangedEmbeddingIdentity(t *testing.T) {
	t.Parallel()

	oldChunks := reuseDocumentChunks(t, "doc-identity", 1, reuseMarkdown(4, -1, ""))
	newChunks := reuseDocumentChunks(t, "doc-identity", 2, reuseMarkdown(4, 2, "第2节改写。"))
	cases := []struct {
		name          string
		storedModel   string
		storedDim     int
		configVersion string
	}{
		{"different model name", "other-embedding-model", 4, EmbeddingContentVersion},
		{"different dimension", reuseTestModel, 8, EmbeddingContentVersion},
		{"different embedding content version", reuseTestModel, 4, "knowledge-embedding-content-v0"},
	}
	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			stored := storedFromChunks(t, 7, oldChunks, testCase.storedModel, testCase.storedDim)
			for index := range stored {
				stored[index].EmbeddingFingerprint = EmbeddingFingerprint(testCase.storedModel, testCase.storedDim, testCase.configVersion)
				// Keep every vector at the indexer's dimension so the rejection
				// can only come from the fingerprint, not from a length check.
				stored[index].Vector = deterministicVector(stored[index].EmbeddingContent, 4)
			}
			repository := &fakeChunkVectorRepository{loadResult: stored}
			indexer := mustKnowledgeChunkIndexer(t, &countingChunkEmbedder{dimension: 4}, repository, testIndexConfig(4, 10))
			result, err := indexer.Index(context.Background(), ChunkIndexRequest{UserID: 7, Chunks: newChunks, ReuseFromVersion: 1})
			if err != nil {
				t.Fatalf("Index() error: %v", err)
			}
			if result.ReusedChunks != 0 || result.ReuseStatus != ChunkReuseLegacyMetadata {
				t.Fatalf("%s reused %d chunks with status %q", testCase.name, result.ReusedChunks, result.ReuseStatus)
			}
			assertReuseAccounting(t, result)
		})
	}
}

// TestChunkReuseRecomputesDamagedCandidatesOnly keeps a single bad record from
// escalating into a full recomputation, and keeps it from being reused.
func TestChunkReuseRecomputesDamagedCandidatesOnly(t *testing.T) {
	t.Parallel()

	const sections = 5
	chunks := reuseDocumentChunks(t, "doc-damaged", 1, reuseMarkdown(sections, -1, ""))
	newChunks := reuseDocumentChunks(t, "doc-damaged", 2, reuseMarkdown(sections, -1, ""))
	cases := []struct {
		name   string
		damage func(*StoredChunkEmbedding)
	}{
		{"short vector", func(record *StoredChunkEmbedding) { record.Vector = record.Vector[:2] }},
		{"not a number", func(record *StoredChunkEmbedding) { record.Vector[1] = float32(math.NaN()) }},
		{"infinite value", func(record *StoredChunkEmbedding) { record.Vector[0] = float32(math.Inf(1)) }},
		{"hash does not describe content", func(record *StoredChunkEmbedding) {
			record.EmbeddingContent = record.EmbeddingContent + "被篡改"
		}},
	}
	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			stored := storedFromChunks(t, 7, chunks, reuseTestModel, 4)
			testCase.damage(&stored[2])
			repository := &fakeChunkVectorRepository{loadResult: stored}
			embedder := &countingChunkEmbedder{dimension: 4}
			indexer := mustKnowledgeChunkIndexer(t, embedder, repository, testIndexConfig(4, 10))
			result, err := indexer.Index(context.Background(), ChunkIndexRequest{UserID: 7, Chunks: newChunks, ReuseFromVersion: 1})
			if err != nil {
				t.Fatalf("Index() error: %v", err)
			}
			if result.ReusedChunks != sections-1 || result.EmbeddedChunks != 1 || embedder.textCount != 1 {
				t.Fatalf("%s reused %d, embedded %d, texts %d", testCase.name, result.ReusedChunks, result.EmbeddedChunks, embedder.textCount)
			}
			// The damaged position must hold a freshly computed vector.
			damaged := repository.stored[2]
			if !reflect.DeepEqual(damaged.Vector, deterministicVector(damaged.Chunk.EmbeddingContent, 4)) {
				t.Fatalf("%s stored a damaged vector", testCase.name)
			}
			assertReuseAccounting(t, result)
		})
	}
}

// TestChunkReuseRejectsForeignCandidates is the tenant boundary. The Redis
// loader already re-reads user/doc/version from the HASH; this proves the
// indexer refuses foreign records even if a repository hands them over.
func TestChunkReuseRejectsForeignCandidates(t *testing.T) {
	t.Parallel()

	oldChunks := reuseDocumentChunks(t, "doc-tenant", 1, reuseMarkdown(4, -1, ""))
	newChunks := reuseDocumentChunks(t, "doc-tenant", 2, reuseMarkdown(4, -1, ""))
	cases := []struct {
		name    string
		corrupt func(*StoredChunkEmbedding)
	}{
		{"another user", func(record *StoredChunkEmbedding) { record.UserID = 99 }},
		{"another document", func(record *StoredChunkEmbedding) { record.DocID = "doc-other" }},
		{"another version", func(record *StoredChunkEmbedding) { record.DocVersion = 5 }},
	}
	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			stored := storedFromChunks(t, 7, oldChunks, reuseTestModel, 4)
			for index := range stored {
				testCase.corrupt(&stored[index])
			}
			repository := &fakeChunkVectorRepository{loadResult: stored}
			embedder := &countingChunkEmbedder{dimension: 4}
			indexer := mustKnowledgeChunkIndexer(t, embedder, repository, testIndexConfig(4, 10))
			result, err := indexer.Index(context.Background(), ChunkIndexRequest{UserID: 7, Chunks: newChunks, ReuseFromVersion: 1})
			if err != nil {
				t.Fatalf("Index() error: %v", err)
			}
			if result.ReusedChunks != 0 || embedder.textCount != len(newChunks) {
				t.Fatalf("%s reused %d chunks", testCase.name, result.ReusedChunks)
			}
			assertReuseAccounting(t, result)
		})
	}
}

// TestChunkReuseHandlesRepeatedEmbeddingInputs pins the behaviour for a
// document whose chunks carry the same embedder input more than once: they may
// share one old vector, and the outcome must not depend on map iteration order.
func TestChunkReuseHandlesRepeatedEmbeddingInputs(t *testing.T) {
	t.Parallel()

	repeated := "## 重复\n同一段内容。\n\n## 重复\n同一段内容。\n\n## 唯一\n另一段内容。\n"
	oldChunks := reuseDocumentChunks(t, "doc-repeat", 1, repeated)
	newChunks := reuseDocumentChunks(t, "doc-repeat", 2, repeated+"\n## 新增\n新增内容。\n")
	duplicates := 0
	for _, chunk := range newChunks {
		if chunk.EmbeddingContent == newChunks[0].EmbeddingContent {
			duplicates++
		}
	}
	if duplicates < 2 {
		t.Fatalf("fixture produced %d identical embedding inputs, want at least 2", duplicates)
	}

	var firstRun []EmbeddedChunk
	for attempt := 0; attempt < 5; attempt++ {
		repository := &fakeChunkVectorRepository{loadResult: storedFromChunks(t, 7, oldChunks, reuseTestModel, 4)}
		indexer := mustKnowledgeChunkIndexer(t, &countingChunkEmbedder{dimension: 4}, repository, testIndexConfig(4, 10))
		result, err := indexer.Index(context.Background(), ChunkIndexRequest{UserID: 7, Chunks: newChunks, ReuseFromVersion: 1})
		if err != nil {
			t.Fatalf("Index() error: %v", err)
		}
		if result.ReusedChunks != len(oldChunks) || result.EmbeddedChunks != len(newChunks)-len(oldChunks) {
			t.Fatalf("attempt %d reused %d, embedded %d", attempt, result.ReusedChunks, result.EmbeddedChunks)
		}
		assertReuseAccounting(t, result)
		if attempt == 0 {
			firstRun = repository.stored
			continue
		}
		if !reflect.DeepEqual(repository.stored, firstRun) {
			t.Fatalf("attempt %d produced a different assignment than the first run", attempt)
		}
	}
}

// TestChunkReuseEmbeddingFailureWritesNothing keeps the failure guarantee that
// existed before reuse: a partially embedded version never reaches Redis, so
// the old ActiveVersion keeps serving retrieval.
func TestChunkReuseEmbeddingFailureWritesNothing(t *testing.T) {
	t.Parallel()

	oldChunks := reuseDocumentChunks(t, "doc-partial", 1, reuseMarkdown(6, -1, ""))
	newChunks := reuseDocumentChunks(t, "doc-partial", 2, reuseMarkdown(6, 4, "第4节改写。"))
	repository := &fakeChunkVectorRepository{loadResult: storedFromChunks(t, 7, oldChunks, reuseTestModel, 4)}
	embedder := &countingChunkEmbedder{dimension: 4, failCall: 1}
	indexer := mustKnowledgeChunkIndexer(t, embedder, repository, testIndexConfig(4, 10))
	if _, err := indexer.Index(context.Background(), ChunkIndexRequest{UserID: 7, Chunks: newChunks, ReuseFromVersion: 1}); !errors.Is(err, ErrEmbeddingBatch) {
		t.Fatalf("Index() error = %v, want ErrEmbeddingBatch", err)
	}
	if repository.storeCalls != 0 || len(repository.stored) != 0 {
		t.Fatalf("failed update wrote %d chunks in %d store calls", len(repository.stored), repository.storeCalls)
	}
}

// TestChunkReuseStopsOnCanceledContext proves the fallback path does not
// swallow ctx.Err() to keep the update alive.
func TestChunkReuseStopsOnCanceledContext(t *testing.T) {
	t.Parallel()

	newChunks := reuseDocumentChunks(t, "doc-cancel", 2, reuseMarkdown(4, -1, ""))
	repository := &fakeChunkVectorRepository{loadErr: errors.New("redis unavailable")}
	embedder := &countingChunkEmbedder{dimension: 4}
	indexer := mustKnowledgeChunkIndexer(t, embedder, repository, testIndexConfig(4, 10))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := indexer.Index(ctx, ChunkIndexRequest{UserID: 7, Chunks: newChunks, ReuseFromVersion: 1}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Index() error = %v, want context.Canceled", err)
	}
	if embedder.textCount != 0 || repository.storeCalls != 0 {
		t.Fatalf("canceled update embedded %d texts and stored %d times", embedder.textCount, repository.storeCalls)
	}
}

func TestChunkIndexConfigRequiresEmbeddingIdentity(t *testing.T) {
	t.Parallel()

	if _, err := NewKnowledgeChunkIndexer(&countingChunkEmbedder{dimension: 4}, &fakeChunkVectorRepository{},
		ChunkIndexConfig{Dimension: 4, BatchSize: 2}); !errors.Is(err, ErrInvalidChunkIndexConfig) {
		t.Fatalf("missing model name error = %v", err)
	}
	// An omitted content version falls back to the current code constant rather
	// than to an empty string that would collide across future changes.
	indexer, err := NewKnowledgeChunkIndexer(&countingChunkEmbedder{dimension: 4}, &fakeChunkVectorRepository{},
		ChunkIndexConfig{Dimension: 4, BatchSize: 2, EmbeddingModel: reuseTestModel})
	if err != nil {
		t.Fatalf("default content version error: %v", err)
	}
	if indexer.fingerprint != EmbeddingFingerprint(reuseTestModel, 4, EmbeddingContentVersion) {
		t.Fatalf("default fingerprint does not use the current content version")
	}
}

func TestEmbeddingIdentityDistinguishesItsInputs(t *testing.T) {
	t.Parallel()

	base := EmbeddingFingerprint("model-a", 1024, EmbeddingContentVersion)
	for _, other := range []string{
		EmbeddingFingerprint("model-b", 1024, EmbeddingContentVersion),
		EmbeddingFingerprint("model-a", 512, EmbeddingContentVersion),
		EmbeddingFingerprint("model-a", 1024, "knowledge-embedding-content-v2"),
	} {
		if other == base {
			t.Fatalf("fingerprint collision with %q", other)
		}
	}
	if base != EmbeddingFingerprint("model-a", 1024, EmbeddingContentVersion) {
		t.Fatalf("fingerprint is not deterministic")
	}
	// The input hash must cover the embedder input, not RawContent.
	if EmbeddingInputHash("文件: a.md\n\n内容") == EmbeddingInputHash("文件: b.md\n\n内容") {
		t.Fatalf("input hash ignores the file name that the embedder actually sees")
	}
}

func assertReuseAccounting(t *testing.T, result ChunkIndexResult) {
	t.Helper()
	if result.TotalChunks != result.ReusedChunks+result.EmbeddedChunks {
		t.Fatalf("accounting broken: %+v", result)
	}
}

// assertStoredCompleteVersion checks the invariant that matters most: reuse
// changes which vectors are computed, never which chunks are written.
func assertStoredCompleteVersion(t *testing.T, repository *fakeChunkVectorRepository, chunks []Chunk, userID int64) {
	t.Helper()
	if repository.storeCalls != 1 {
		t.Fatalf("store called %d times, want exactly one complete version write", repository.storeCalls)
	}
	if len(repository.stored) != len(chunks) {
		t.Fatalf("stored %d chunks, want the complete version of %d", len(repository.stored), len(chunks))
	}
	fingerprint := EmbeddingFingerprint(reuseTestModel, 4, EmbeddingContentVersion)
	for index, stored := range repository.stored {
		if stored.UserID != userID || stored.Chunk.ID != chunks[index].ID || stored.Chunk.Index != index {
			t.Fatalf("stored[%d] identity = user %d chunk %q index %d", index, stored.UserID, stored.Chunk.ID, stored.Chunk.Index)
		}
		if stored.Chunk.DocVersion != chunks[index].DocVersion {
			t.Fatalf("stored[%d] carries version %d, want %d", index, stored.Chunk.DocVersion, chunks[index].DocVersion)
		}
		if len(stored.Vector) != 4 {
			t.Fatalf("stored[%d] vector length %d", index, len(stored.Vector))
		}
		if stored.EmbeddingFingerprint != fingerprint {
			t.Fatalf("stored[%d] fingerprint %q", index, stored.EmbeddingFingerprint)
		}
	}
}

// storedFromChunks builds a reuse source exactly as a previous successful index
// of those chunks would have written it.
func storedFromChunks(t *testing.T, userID int64, chunks []Chunk, model string, dimension int) []StoredChunkEmbedding {
	t.Helper()
	fingerprint := EmbeddingFingerprint(model, dimension, EmbeddingContentVersion)
	stored := make([]StoredChunkEmbedding, len(chunks))
	for index, chunk := range chunks {
		stored[index] = StoredChunkEmbedding{
			UserID: userID, DocID: chunk.DocID, DocVersion: chunk.DocVersion, ChunkID: chunk.ID,
			EmbeddingContent:   chunk.EmbeddingContent,
			EmbeddingInputHash: EmbeddingInputHash(chunk.EmbeddingContent),
			// dimension here is the identity claim; the bytes follow the
			// indexer's configured dimension so tests isolate one variable.
			EmbeddingFingerprint: fingerprint,
			Vector:               deterministicVector(chunk.EmbeddingContent, dimension),
		}
	}
	return stored
}

func reuseMarkdown(sections, editedSection int, replacement string) string {
	var builder strings.Builder
	for index := 0; index < sections; index++ {
		builder.WriteString(fmt.Sprintf("## 第%d节\n", index))
		if index == editedSection {
			builder.WriteString(replacement + "\n\n")
			continue
		}
		builder.WriteString(fmt.Sprintf("这是第%d节稳定不变的内容。\n\n", index))
	}
	return builder.String()
}

func reuseDocumentChunks(t *testing.T, docID string, version int, markdown string) []Chunk {
	t.Helper()
	return reuseChunksWithSource(t, docID, version, docID+".md", markdown)
}

func reuseChunksWithSource(t *testing.T, docID string, version int, sourceFile, markdown string) []Chunk {
	t.Helper()
	chunker := mustChunker(t, DefaultChunkerConfig())
	chunks, err := chunker.Chunk(ChunkInput{
		DocID: docID, DocVersion: version, SourceFile: sourceFile, Blocks: ParseMarkdown(markdown),
	})
	if err != nil {
		t.Fatalf("prepare chunks: %v", err)
	}
	if len(chunks) == 0 {
		t.Fatalf("fixture produced no chunks")
	}
	return chunks
}

// deterministicVector derives the vector from the embedder input, so a reused
// vector and a freshly computed one are comparable byte for byte.
func deterministicVector(text string, dimension int) []float32 {
	hash := EmbeddingInputHash(text)
	vector := make([]float32, dimension)
	for index := range vector {
		vector[index] = float32(int(hash[index%len(hash)])) / 255
	}
	return vector
}

type countingChunkEmbedder struct {
	dimension  int
	failCall   int
	calls      int
	textCount  int
	batchSizes []int
}

func (c *countingChunkEmbedder) EmbedStrings(_ context.Context, texts []string, _ ...embedding.Option) ([][]float64, error) {
	c.calls++
	c.textCount += len(texts)
	c.batchSizes = append(c.batchSizes, len(texts))
	if c.failCall == c.calls {
		return nil, errors.New("fake embedding failure")
	}
	vectors := make([][]float64, len(texts))
	for index, text := range texts {
		converted := deterministicVector(text, c.dimension)
		vectors[index] = make([]float64, c.dimension)
		for dimension, value := range converted {
			vectors[index][dimension] = float64(value)
		}
	}
	return vectors, nil
}
