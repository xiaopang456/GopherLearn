package index

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"GopherAI/common/rag"

	"github.com/xiaopang456/GopherLearn/eval/internal/dataset"
	"github.com/xiaopang456/GopherLearn/eval/internal/fake"
)

const (
	testUser  = int64(900001)
	testDim   = 64
	testModel = "synthetic-embedding"
	maxSource = int64(1 << 20)
)

func fixtureCorpus(t *testing.T) *dataset.Corpus {
	t.Helper()
	corpus, err := dataset.LoadCorpus(filepath.Join("..", "..", "testdata", "corpus.json"))
	if err != nil {
		t.Fatal(err)
	}
	return corpus
}

// smallChunks forces several chunks out of the short fixture documents, so line
// numbers are exercised. At 90 tokens a chunk is one or two whole lines.
func smallChunks() rag.ChunkerConfig {
	return rag.ChunkerConfig{MaxTokens: 90, OverlapTokens: 8, MaxChunks: 2000}
}

// tinyChunks makes chunks small enough that one fixture document needs more than
// one embedding batch.
func tinyChunks() rag.ChunkerConfig {
	return rag.ChunkerConfig{MaxTokens: 70, OverlapTokens: 8, MaxChunks: 2000}
}

func buildFixture(t *testing.T, cfg rag.ChunkerConfig) (*Index, *fake.Backend, *fake.Embedder, *dataset.Corpus) {
	t.Helper()
	corpus := fixtureCorpus(t)
	embedder := fake.NewEmbedder(testDim)
	backend := fake.NewBackend(testDim)
	ix, err := Build(context.Background(), Params{
		UserID: testUser, Corpus: corpus, Embedder: embedder, Repository: backend,
		Dimension: testDim, EmbeddingModel: testModel, Chunker: cfg, MaxSourceBytes: maxSource,
	})
	if err != nil {
		t.Fatal(err)
	}
	return ix, backend, embedder, corpus
}

// TestIndexedChunksEqualDirectChunking proves the evaluation indexes the chunks
// the product's own chunker produces, line numbers included.
func TestIndexedChunksEqualDirectChunking(t *testing.T) {
	for name, cfg := range map[string]rag.ChunkerConfig{"default config": rag.DefaultChunkerConfig(), "small chunks": smallChunks(), "tiny chunks": tinyChunks()} {
		t.Run(name, func(t *testing.T) {
			_, backend, _, corpus := buildFixture(t, cfg)
			chunker, err := rag.NewChunker(cfg)
			if err != nil {
				t.Fatal(err)
			}
			stored := backend.All()
			total := 0
			for _, doc := range corpus.Documents {
				content, err := os.ReadFile(doc.Path)
				if err != nil {
					t.Fatal(err)
				}
				want, err := chunker.Chunk(rag.ChunkInput{
					DocID: DocID(doc.File), DocVersion: 1, SourceFile: doc.File,
					Blocks: rag.ParseMarkdown(string(content)),
				})
				if err != nil {
					t.Fatal(err)
				}
				var got []rag.EmbeddedChunk
				for _, s := range stored {
					if s.Chunk.DocID == DocID(doc.File) {
						got = append(got, s)
					}
				}
				if len(got) != len(want) {
					t.Fatalf("%s: stored %d chunks, direct chunking gives %d", doc.File, len(got), len(want))
				}
				byID := map[string]rag.Chunk{}
				for _, g := range got {
					byID[g.Chunk.ID] = g.Chunk
				}
				for _, w := range want {
					g, ok := byID[w.ID]
					if !ok {
						t.Fatalf("%s: chunk %s was not stored", doc.File, w.ID)
					}
					if g.StartLine != w.StartLine || g.EndLine != w.EndLine || g.RawContent != w.RawContent || g.SourceFile != doc.File {
						t.Errorf("%s chunk %s: stored lines %d-%d source %q, direct lines %d-%d", doc.File, w.ID, g.StartLine, g.EndLine, g.SourceFile, w.StartLine, w.EndLine)
					}
					if g.StartLine < 1 || g.EndLine < g.StartLine || g.EndLine > doc.Lines {
						t.Errorf("%s chunk %s: lines %d-%d fall outside 1..%d", doc.File, w.ID, g.StartLine, g.EndLine, doc.Lines)
					}
				}
				total += len(want)
			}
			if backend.Len() != total {
				t.Errorf("backend holds %d chunks, direct chunking gives %d", backend.Len(), total)
			}
		})
	}
}

// TestChunkLinesUseTheSameNumberingAsEvidenceSpans pins the convention the
// metrics rely on: the chunk that contains a sentence reports a line interval
// that contains the sentence's true 1-based line in the file.
func TestChunkLinesUseTheSameNumberingAsEvidenceSpans(t *testing.T) {
	_, backend, _, corpus := buildFixture(t, smallChunks())
	phrases := map[string]string{
		"tide-tables.md":            "The sandbar at Grey Point dries",
		"lantern-maintenance.md":    "Wash the chimney in warm water",
		"notes/orchard-grafting.md": "Make a long sloping cut",
	}
	for file, phrase := range phrases {
		doc, _ := corpus.Lookup(file)
		content, err := os.ReadFile(doc.Path)
		if err != nil {
			t.Fatal(err)
		}
		line := 0
		for i, text := range strings.Split(string(content), "\n") {
			if strings.Contains(text, phrase) {
				line = i + 1
			}
		}
		if line == 0 {
			t.Fatalf("%s: phrase %q not found", file, phrase)
		}
		found := false
		for _, s := range backend.All() {
			c := s.Chunk
			if c.DocID != DocID(file) || !strings.Contains(c.RawContent, phrase) {
				continue
			}
			found = true
			if line < c.StartLine || line > c.EndLine {
				t.Errorf("%s: %q is on line %d but its chunk reports %d-%d", file, phrase, line, c.StartLine, c.EndLine)
			}
			if c.EndLine-c.StartLine > 12 {
				t.Errorf("%s: chunk %d-%d is too wide for a 90-token chunk; the check proves nothing", file, c.StartLine, c.EndLine)
			}
		}
		if !found {
			t.Errorf("%s: no chunk contains %q", file, phrase)
		}
	}
}

func TestEmbeddingCallsFollowTheProductBatching(t *testing.T) {
	corpus := fixtureCorpus(t)
	plan, err := Plan(context.Background(), testUser, corpus, tinyChunks(), 0, maxSource)
	if err != nil {
		t.Fatal(err)
	}
	biggest := 0
	for _, d := range plan.Chunks {
		biggest = max(biggest, d.Chunks)
	}
	if biggest <= DefaultBatchSize || plan.EmbeddingCalls <= len(corpus.Documents) {
		t.Fatalf("the largest document has %d chunks and the plan %d calls; the check needs a document that spans two batches", biggest, plan.EmbeddingCalls)
	}
	_, backend, embedder, _ := buildFixture(t, tinyChunks())
	if embedder.Calls() != plan.EmbeddingCalls || embedder.Texts() != plan.Total || backend.Len() != plan.Total {
		t.Errorf("plan: %d chunks, %d calls; actual: %d chunks stored, %d calls, %d texts",
			plan.Total, plan.EmbeddingCalls, backend.Len(), embedder.Calls(), embedder.Texts())
	}
	want := 0
	for _, d := range plan.Chunks {
		want += (d.Chunks + DefaultBatchSize - 1) / DefaultBatchSize
	}
	if plan.EmbeddingCalls != want {
		t.Errorf("EmbeddingCalls = %d, want the per-document ceil(chunks/%d) sum %d", plan.EmbeddingCalls, DefaultBatchSize, want)
	}
}

func TestPlanRunsOfflineAndHonoursTheBatchSize(t *testing.T) {
	corpus := fixtureCorpus(t)
	a, err := Plan(context.Background(), testUser, corpus, smallChunks(), 10, maxSource)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Plan(context.Background(), testUser, corpus, smallChunks(), 1, maxSource)
	if err != nil {
		t.Fatal(err)
	}
	if b.EmbeddingCalls != b.Total || a.Total != b.Total || a.EmbeddingCalls >= b.EmbeddingCalls {
		t.Errorf("batch 10: %d calls; batch 1: %d calls for %d chunks", a.EmbeddingCalls, b.EmbeddingCalls, b.Total)
	}
	if len(a.Chunks) != len(corpus.Documents) || a.Chunks[0].File != corpus.Documents[0].File {
		t.Errorf("per-document counts out of order: %+v", a.Chunks)
	}
}

func TestLoaderAndFileMapping(t *testing.T) {
	ix, _, _, corpus := buildFixture(t, rag.DefaultChunkerConfig())
	active, err := ix.Loader()(context.Background(), testUser)
	if err != nil || len(active) != len(corpus.Documents) {
		t.Fatalf("active = %+v, %v", active, err)
	}
	for i, a := range active {
		if a.Version != 1 || a.OriginalName != corpus.Documents[i].File || a.DocID != DocID(corpus.Documents[i].File) {
			t.Errorf("active[%d] = %+v", i, a)
		}
		if file, ok := ix.FileOf(a.DocID); !ok || file != a.OriginalName {
			t.Errorf("FileOf(%s) = %q, %v", a.DocID, file, ok)
		}
	}
	if other, _ := ix.Loader()(context.Background(), testUser+1); len(other) != 0 {
		t.Error("another tenant must see no documents")
	}
	if _, ok := ix.FileOf("nope"); ok {
		t.Error("FileOf invented a document")
	}
	if ix.Chunks() < len(corpus.Documents) {
		t.Errorf("Chunks() = %d", ix.Chunks())
	}
	if DocID("a.md") == DocID("b.md") || DocID("a.md") != DocID("a.md") || !strings.HasPrefix(DocID("a.md"), "eval-") {
		t.Error("DocID must be a deterministic, per-file identifier")
	}
}

// TestRetrievalOverTheBuiltIndex drives the real RAGService over the built
// index, so the loader, the stored metadata and the document names are proven
// to work together. It checks wiring, not ranking quality.
func TestRetrievalOverTheBuiltIndex(t *testing.T) {
	ix, backend, embedder, _ := buildFixture(t, smallChunks())
	service, err := rag.NewRAGService(context.Background(), embedder, backend, ix.Loader(), testDim)
	if err != nil {
		t.Fatal(err)
	}
	results, err := service.Retrieve(context.Background(), testUser, "How long before high water must a keel boat cross the Grey Point bar?", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("no results")
	}
	for _, r := range results {
		file, ok := ix.FileOf(r.DocID)
		if !ok || file != r.DocumentName {
			t.Errorf("result document %q (%s) does not map back to the manifest", r.DocumentName, r.DocID)
		}
		if r.StartLine < 1 || r.EndLine < r.StartLine {
			t.Errorf("result lines %d-%d", r.StartLine, r.EndLine)
		}
	}
	// A sanity check on the fake embedder, not a quality claim: a vector-only
	// query that repeats a passage's own words finds that passage's document.
	outcome, err := service.RetrieveWithOptions(context.Background(), testUser, "The sandbar at Grey Point dries at 1.8 metres",
		rag.RetrieveOptions{TopK: 3, Mode: rag.RetrievalModeVector})
	if err != nil || len(outcome.Results) == 0 || outcome.Results[0].DocumentName != "tide-tables.md" {
		t.Errorf("vector retrieval of a verbatim passage: %+v, %v", outcome.Results, err)
	}
	if other, err := service.Retrieve(context.Background(), testUser+1, "tide", 5); err != nil || len(other) != 0 {
		t.Errorf("another tenant got %d results, err %v", len(other), err)
	}
}

func writeCorpus(t *testing.T, files map[string]string) *dataset.Corpus {
	t.Helper()
	dir := t.TempDir()
	var entries []string
	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, fmt.Sprintf(`{"file":%q}`, name))
	}
	manifest := filepath.Join(dir, "corpus.json")
	if err := os.WriteFile(manifest, []byte(`{"name":"temp","documents":[`+strings.Join(entries, ",")+`]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	corpus, err := dataset.LoadCorpus(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return corpus
}

func TestBuildRefusesWhatTheProductWouldRefuse(t *testing.T) {
	base := func(corpus *dataset.Corpus) Params {
		return Params{
			UserID: testUser, Corpus: corpus, Embedder: fake.NewEmbedder(testDim), Repository: fake.NewBackend(testDim),
			Dimension: testDim, EmbeddingModel: testModel, Chunker: rag.DefaultChunkerConfig(), MaxSourceBytes: maxSource,
		}
	}
	t.Run("an unsupported file type", func(t *testing.T) {
		corpus := writeCorpus(t, map[string]string{"report.pdf": "%PDF-1.4 not really"})
		_, err := Build(context.Background(), base(corpus))
		if err == nil || !strings.Contains(err.Error(), "report.pdf") {
			t.Fatalf("err = %v, want it to name report.pdf", err)
		}
		if _, err := Plan(context.Background(), testUser, corpus, rag.DefaultChunkerConfig(), 0, maxSource); err == nil {
			t.Fatal("Plan must reject it too, so a bad corpus is found before any spend")
		}
	})
	t.Run("a document over the size limit", func(t *testing.T) {
		corpus := writeCorpus(t, map[string]string{"big.md": strings.Repeat("word ", 200)})
		p := base(corpus)
		p.MaxSourceBytes = 100
		if _, err := Build(context.Background(), p); err == nil || !strings.Contains(err.Error(), "big.md") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("a missing size limit", func(t *testing.T) {
		p := base(fixtureCorpus(t))
		p.MaxSourceBytes = 0
		if _, err := Build(context.Background(), p); err == nil {
			t.Fatal("MaxSourceBytes 0 must be refused rather than read as unlimited")
		}
	})
	t.Run("missing dependencies", func(t *testing.T) {
		for name, mutate := range map[string]func(*Params){
			"user":       func(p *Params) { p.UserID = 0 },
			"corpus":     func(p *Params) { p.Corpus = nil },
			"embedder":   func(p *Params) { p.Embedder = nil },
			"repository": func(p *Params) { p.Repository = nil },
		} {
			p := base(fixtureCorpus(t))
			mutate(&p)
			if _, err := Build(context.Background(), p); err == nil {
				t.Errorf("a missing %s was accepted", name)
			}
		}
	})
}

func TestBuildFailuresStopTheRunAndNameTheDocument(t *testing.T) {
	corpus := fixtureCorpus(t)
	boom := errors.New("embedding provider unavailable")
	embedder := fake.NewEmbedder(testDim)
	embedder.Fail(boom)
	backend := fake.NewBackend(testDim)
	_, err := Build(context.Background(), Params{
		UserID: testUser, Corpus: corpus, Embedder: embedder, Repository: backend,
		Dimension: testDim, EmbeddingModel: testModel, Chunker: rag.DefaultChunkerConfig(), MaxSourceBytes: maxSource,
	})
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), corpus.Documents[0].File) {
		t.Fatalf("err = %v, want the provider error naming the first document", err)
	}
	if embedder.Calls() != 1 || backend.Len() != 0 {
		t.Errorf("after the first failure: %d embedding calls, %d chunks stored; the run must stop and store nothing", embedder.Calls(), backend.Len())
	}

	// A dimension the embedder does not produce is caught before anything is stored.
	wrong := fake.NewBackend(16)
	_, err = Build(context.Background(), Params{
		UserID: testUser, Corpus: corpus, Embedder: fake.NewEmbedder(16), Repository: wrong,
		Dimension: testDim, EmbeddingModel: testModel, Chunker: rag.DefaultChunkerConfig(), MaxSourceBytes: maxSource,
	})
	if err == nil || wrong.Len() != 0 {
		t.Fatalf("err = %v, %d chunks stored", err, wrong.Len())
	}
}
