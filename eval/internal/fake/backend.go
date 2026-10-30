package fake

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"

	"GopherAI/common/rag"
	redisCommon "GopherAI/common/redis"
)

// Search is one recorded search call.
type Search struct {
	// Kind is "vector" or "text".
	Kind string
	TopK int
}

// Backend is an in-memory index. It implements both rag.ChunkVectorRepository
// (what indexing writes through) and rag.ActiveChunkSearcher (what retrieval
// reads through), so one value can stand where the production code uses the
// Redis repository.
//
// It reproduces the contract that matters to the service above it: results are
// scoped to one user and to the allowed (document, version) pairs, a vector
// search ranks by cosine distance (1 - cosine, ascending), a text search ranks by
// a BM25-style score over the same fields the Redis index searches, and storing a
// document version replaces whatever was stored for it. It does not reproduce
// RediSearch's analyzer, so keyword rankings differ from production.
type Backend struct {
	dim int

	mu           sync.Mutex
	records      map[string]rag.EmbeddedChunk
	ensureCalls  int
	storeCalls   int
	loadCalls    int
	searches     []Search
	vectorErr    error
	textErr      error
	storeErr     error
	ensureIndexE error
}

var (
	_ rag.ChunkVectorRepository = (*Backend)(nil)
	_ rag.ActiveChunkSearcher   = (*Backend)(nil)
)

// NewBackend returns an empty index for vectors of the given dimension.
func NewBackend(dimension int) *Backend {
	return &Backend{dim: dimension, records: map[string]rag.EmbeddedChunk{}}
}

// FailVectorSearch, FailTextSearch, FailStore and FailEnsureIndex make the named
// operation return err; nil restores it.
func (b *Backend) FailVectorSearch(err error) { b.set(&b.vectorErr, err) }
func (b *Backend) FailTextSearch(err error)   { b.set(&b.textErr, err) }
func (b *Backend) FailStore(err error)        { b.set(&b.storeErr, err) }
func (b *Backend) FailEnsureIndex(err error)  { b.set(&b.ensureIndexE, err) }

func (b *Backend) set(target *error, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	*target = err
}

// Searches returns the recorded search calls in order.
func (b *Backend) Searches() []Search {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]Search(nil), b.searches...)
}

// StoreCalls is the number of Store calls, failed ones included.
func (b *Backend) StoreCalls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.storeCalls
}

// Len is the number of stored chunks.
func (b *Backend) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.records)
}

// All returns every stored chunk ordered by Redis key.
func (b *Backend) All() []rag.EmbeddedChunk {
	b.mu.Lock()
	defer b.mu.Unlock()
	keys := make([]string, 0, len(b.records))
	for key := range b.records {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]rag.EmbeddedChunk, len(keys))
	for i, key := range keys {
		out[i] = b.records[key]
	}
	return out
}

// EnsureIndex implements both interfaces. Creating the index is a no-op.
func (b *Backend) EnsureIndex(context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ensureCalls++
	return b.ensureIndexE
}

// Store implements rag.ChunkVectorRepository. Like the Redis repository it
// leaves the stored version holding exactly the chunks passed in.
func (b *Backend) Store(ctx context.Context, chunks []rag.EmbeddedChunk) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.storeCalls++
	if b.storeErr != nil {
		return b.storeErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(chunks) == 0 {
		return nil
	}
	for _, embedded := range chunks {
		if embedded.UserID <= 0 || embedded.Chunk.DocID == "" || embedded.Chunk.DocVersion <= 0 || embedded.Chunk.ID == "" {
			return fmt.Errorf("fake backend: chunk without a user, document, version or id")
		}
		if len(embedded.Vector) != b.dim {
			return fmt.Errorf("fake backend: vector dimension is %d, want %d", len(embedded.Vector), b.dim)
		}
	}
	first := chunks[0]
	prefix := redisCommon.GenerateKnowledgeDocumentChunkPrefix(first.UserID, first.Chunk.DocID, first.Chunk.DocVersion)
	for key := range b.records {
		if strings.HasPrefix(key, prefix) {
			delete(b.records, key)
		}
	}
	for _, embedded := range chunks {
		key := redisCommon.GenerateKnowledgeChunkKey(embedded.UserID, embedded.Chunk.DocID, embedded.Chunk.DocVersion, embedded.Chunk.ID)
		embedded.Vector = append([]float32(nil), embedded.Vector...)
		b.records[key] = embedded
	}
	return nil
}

// LoadDocumentVersionEmbeddings implements rag.ChunkVectorRepository.
func (b *Backend) LoadDocumentVersionEmbeddings(_ context.Context, userID int64, docID string, version int) ([]rag.StoredChunkEmbedding, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.loadCalls++
	prefix := redisCommon.GenerateKnowledgeDocumentChunkPrefix(userID, docID, version)
	var out []rag.StoredChunkEmbedding
	for key, embedded := range b.records {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		out = append(out, rag.StoredChunkEmbedding{
			UserID: embedded.UserID, DocID: embedded.Chunk.DocID, DocVersion: embedded.Chunk.DocVersion,
			ChunkID: embedded.Chunk.ID, EmbeddingContent: embedded.Chunk.EmbeddingContent,
			EmbeddingInputHash: embedded.EmbeddingInputHash, EmbeddingFingerprint: embedded.EmbeddingFingerprint,
			Vector: append([]float32(nil), embedded.Vector...),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ChunkID < out[j].ChunkID })
	return out, nil
}

// SearchActiveByUserVector implements rag.ActiveChunkSearcher.
func (b *Backend) SearchActiveByUserVector(_ context.Context, userID int64, vector []float32, topK int, active map[string]int) ([]rag.RedisChunkSearchResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.searches = append(b.searches, Search{Kind: "vector", TopK: topK})
	if b.vectorErr != nil {
		return nil, b.vectorErr
	}
	if userID <= 0 || len(vector) != b.dim || topK <= 0 {
		return nil, errors.New("fake backend: invalid user, vector dimension, or topK")
	}
	type scored struct {
		key      string
		distance float64
	}
	var hits []scored
	for key, embedded := range b.records {
		if !eligible(embedded, userID, active) {
			continue
		}
		cos, err := cosine(embedded.Vector, vector)
		if err != nil {
			return nil, err
		}
		hits = append(hits, scored{key: key, distance: 1 - cos})
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].distance != hits[j].distance {
			return hits[i].distance < hits[j].distance
		}
		return hits[i].key < hits[j].key
	})
	if len(hits) > topK {
		hits = hits[:topK]
	}
	results := make([]rag.RedisChunkSearchResult, len(hits))
	for i, hit := range hits {
		results[i] = toResult(hit.key, b.records[hit.key])
		results[i].Distance = hit.distance
	}
	return results, nil
}

// SearchActiveByUserText implements rag.ActiveChunkSearcher.
func (b *Backend) SearchActiveByUserText(_ context.Context, userID int64, query string, topK int, active map[string]int) ([]rag.RedisChunkSearchResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.searches = append(b.searches, Search{Kind: "text", TopK: topK})
	if b.textErr != nil {
		return nil, b.textErr
	}
	if userID <= 0 || topK <= 0 {
		return nil, errors.New("fake backend: invalid user or topK")
	}
	terms := rag.ExtractLexicalTerms(query)
	if len(terms) == 0 {
		return []rag.RedisChunkSearchResult{}, nil
	}
	type candidate struct {
		key  string
		text string
	}
	var pool []candidate
	for key, embedded := range b.records {
		if !eligible(embedded, userID, active) {
			continue
		}
		chunk := embedded.Chunk
		text := strings.ToLower(strings.Join([]string{
			chunk.SourceFile, strings.Join(chunk.TitlePath, " "), strings.Join(chunk.Symbols, " "), chunk.RawContent,
		}, " "))
		pool = append(pool, candidate{key: key, text: text})
	}
	lowered := make([]string, len(terms))
	docFrequency := make([]int, len(terms))
	for i, term := range terms {
		lowered[i] = strings.ToLower(term)
		for _, c := range pool {
			if strings.Contains(c.text, lowered[i]) {
				docFrequency[i]++
			}
		}
	}
	type scored struct {
		key   string
		score float64
	}
	var hits []scored
	for _, c := range pool {
		var score float64
		for i, term := range lowered {
			tf := strings.Count(c.text, term)
			if tf == 0 {
				continue
			}
			n := float64(docFrequency[i])
			idf := math.Log(1 + (float64(len(pool))-n+0.5)/(n+0.5))
			score += idf * float64(tf) / (float64(tf) + 1.2)
		}
		if score > 0 {
			hits = append(hits, scored{key: c.key, score: score})
		}
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].key < hits[j].key
	})
	if len(hits) > topK {
		hits = hits[:topK]
	}
	results := make([]rag.RedisChunkSearchResult, len(hits))
	for i, hit := range hits {
		results[i] = toResult(hit.key, b.records[hit.key])
		results[i].LexicalScore = hit.score
	}
	return results, nil
}

func eligible(embedded rag.EmbeddedChunk, userID int64, active map[string]int) bool {
	if embedded.UserID != userID {
		return false
	}
	version, ok := active[embedded.Chunk.DocID]
	return ok && version == embedded.Chunk.DocVersion
}

func toResult(key string, embedded rag.EmbeddedChunk) rag.RedisChunkSearchResult {
	c := embedded.Chunk
	return rag.RedisChunkSearchResult{
		RedisKey: key, ChunkID: c.ID, DocID: c.DocID, DocVersion: c.DocVersion, ChunkIndex: c.Index,
		ChunkType: string(c.Type), SourceFile: c.SourceFile, TitlePath: strings.Join(c.TitlePath, " > "),
		StartLine: c.StartLine, EndLine: c.EndLine, RawContent: c.RawContent, EmbeddingContent: c.EmbeddingContent,
		ContentHash: c.ContentHash, CodeLanguage: c.CodeLanguage, Symbols: strings.Join(c.Symbols, " "),
		TokenEstimate: c.TokenEstimate, PartIndex: c.PartIndex, PartTotal: c.PartTotal,
	}
}
