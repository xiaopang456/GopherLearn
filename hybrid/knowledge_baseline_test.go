package hybrid

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"mime/multipart"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"GopherAI/common/rag"
	redisCommon "GopherAI/common/redis"
	"GopherAI/config"
	"GopherAI/entry"
	"GopherAI/model"
	agentService "GopherAI/service/agent"
	"GopherAI/utils"
	"github.com/cloudwego/eino/components/embedding"
	modelcomponent "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/glebarez/sqlite"
	redisclient "github.com/redis/go-redis/v9"
	"github.com/xiaopang456/GopherLearn/conversation"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// This file is the shared harness for the DOC-B01 baseline. It exists because the
// existing hybrid fixtures cannot answer the questions this round asks:
//
//   - testChunks.Store returns nil without saving anything, so nothing it stores can be
//     retrieved afterwards. A test that claimed "the uploaded chunk is retrievable"
//     through it would be asserting on an empty store.
//   - retrievalFakeEmbedder returns the same vector for every input, so the vector
//     channel cannot be told apart from noise, and nothing counts calls or bytes.
//   - neither fixture survives a re-assembly, so the restart case has nothing to
//     rebuild against.
//
// Everything below is test-only. It never touches production behaviour: the fake store
// and the fake embedder are injected through the same entry.Dependencies seam the
// existing offline tests already use, so the code under test is the real HTTP handler,
// the real DocumentService and DAO, the real IndexWorkerPool and CleanupWorkerPool, the
// real DocumentIngestor and chunker, the real KnowledgeChunkIndexer and the real
// RAGService. Only Redis and the embedding provider are replaced.

const baselineDimension = 1024

// ---------------------------------------------------------------- fake embedding

// baselineEmbedRecord is one EmbedStrings call. It records counts and byte totals only:
// no text, no vector and no document content may appear here, because these records are
// written to a JSONL file outside the repository.
type baselineEmbedRecord struct {
	Call   int
	Ingest bool
	Texts  int
	Bytes  int
}

// baselineEmbedder is a deterministic, offline stand-in for the embedding provider.
//
// Its output is a lexical hashing embedding: the same production tokenizer the retrieval
// side uses (rag.ExtractLexicalTerms) turns the input into terms, each term is hashed to
// one bucket and the result is L2-normalised. That is deliberately NOT a semantic model.
// It exists so vector search is deterministic and so a query that shares vocabulary with
// a chunk ranks that chunk above unrelated chunks; no recall or ranking quality measured
// through it may be reported as a property of the real provider.
//
// Byte counts are bytes of UTF-8 input. They are not tokens, they are not a price, and
// no log line here contains an input's text.
type baselineEmbedder struct {
	dimension int

	mu           sync.Mutex
	calls        int
	ingestCalls  int
	queryCalls   int
	texts        int
	bytes        int
	records      []baselineEmbedRecord
	failIngestAt int
	failErr      error
	delay        time.Duration
	barrier      *baselineBarrier
}

func (e *baselineEmbedder) armIngestFailure(at int, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.failIngestAt, e.failErr = at, err
}

func (e *baselineEmbedder) disarmFailure() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.failIngestAt, e.failErr = 0, nil
}

func (e *baselineEmbedder) setDelay(delay time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.delay = delay
}

func (e *baselineEmbedder) useBarrier(barrier *baselineBarrier) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.barrier = barrier
}

type baselineEmbedCounts struct {
	Calls       int
	IngestCalls int
	QueryCalls  int
	Texts       int
	Bytes       int
}

func (e *baselineEmbedder) counts() baselineEmbedCounts {
	e.mu.Lock()
	defer e.mu.Unlock()
	return baselineEmbedCounts{Calls: e.calls, IngestCalls: e.ingestCalls, QueryCalls: e.queryCalls, Texts: e.texts, Bytes: e.bytes}
}

func (e *baselineEmbedder) callRecords() []baselineEmbedRecord {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]baselineEmbedRecord(nil), e.records...)
}

func (e *baselineEmbedder) EmbedStrings(ctx context.Context, texts []string, _ ...embedding.Option) ([][]float64, error) {
	ingest := baselineIngestBatch(texts)
	e.mu.Lock()
	e.calls++
	call := e.calls
	if ingest {
		e.ingestCalls++
	} else {
		e.queryCalls++
	}
	size := 0
	for _, text := range texts {
		size += len(text)
	}
	e.texts += len(texts)
	e.bytes += size
	e.records = append(e.records, baselineEmbedRecord{Call: call, Ingest: ingest, Texts: len(texts), Bytes: size})
	failAt, failErr, delay, barrier := e.failIngestAt, e.failErr, e.delay, e.barrier
	e.mu.Unlock()

	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if ingest && barrier != nil {
		if err := barrier.wait(ctx); err != nil {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if ingest && failErr != nil && failAt > 0 {
		e.mu.Lock()
		ingestIndex := e.ingestCalls
		e.mu.Unlock()
		if ingestIndex == failAt {
			return nil, failErr
		}
	}
	vectors := make([][]float64, len(texts))
	for index, text := range texts {
		vectors[index] = baselineHashingVector(text, e.dimension)
	}
	return vectors, nil
}

// baselineIngestBatch tells an ingest batch from a query embedding without looking at any
// content: buildEmbeddingContent prefixes every chunk's embedder input with the file name
// header, and RAGService embeds one bare query string. The discriminator is a property of
// the production code, not of the test data.
func baselineIngestBatch(texts []string) bool {
	if len(texts) == 0 {
		return false
	}
	for _, text := range texts {
		if !strings.HasPrefix(text, "文件: ") {
			return false
		}
	}
	return true
}

// baselineHashingVector is the deterministic lexical hashing embedding described above.
func baselineHashingVector(text string, dimension int) []float64 {
	vector := make([]float64, dimension)
	for _, term := range rag.ExtractLexicalTerms(text) {
		sum := sha256.Sum256([]byte(strings.ToLower(term)))
		vector[binary.BigEndian.Uint32(sum[:4])%uint32(dimension)]++
	}
	norm := 0.0
	for _, value := range vector {
		norm += value * value
	}
	if norm == 0 {
		// A text with no extractable term still has to return a finite, correctly sized
		// vector: returning zeros would make cosine distance undefined.
		vector[0] = 1
		return vector
	}
	norm = math.Sqrt(norm)
	for index := range vector {
		vector[index] /= norm
	}
	return vector
}

// baselineBarrier stops an ingest embedding call at a chosen point and lets the test
// release it. It is deterministic: the caller waits on entered, never on a sleep.
type baselineBarrier struct {
	mu      sync.Mutex
	armed   bool
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBaselineBarrier() *baselineBarrier {
	return &baselineBarrier{entered: make(chan struct{}, 1), release: make(chan struct{})}
}

func (b *baselineBarrier) arm() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.armed = true
}

func (b *baselineBarrier) wait(ctx context.Context) error {
	b.mu.Lock()
	armed := b.armed
	b.mu.Unlock()
	if !armed {
		return nil
	}
	select {
	case b.entered <- struct{}{}:
	default:
	}
	select {
	case <-b.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *baselineBarrier) enteredOnce(t *testing.T, timeout time.Duration) {
	t.Helper()
	select {
	case <-b.entered:
	case <-time.After(timeout):
		t.Fatal("the ingest embedding call never reached the barrier")
	}
}

func (b *baselineBarrier) releaseAll() { b.once.Do(func() { close(b.release) }) }

// -------------------------------------------------------- scripted agent model

// baselineCitedAnswer cites citation [1], which is what the tool issues for its first
// result. The server-side citation gate then has to accept it, which is what makes a
// retrieval assertion about the delivered answer rather than about an intermediate value.
const baselineCitedAnswer = "依据上传的笔记 [1]。"

// baselineAgentChat is a chat model that answers one Agent turn the way a tool-calling
// model does: it asks for knowledge_search, and once a tool result is in the message
// history it answers and cites citation [1].
//
// It decides from the conversation rather than from an absolute call counter, because the
// baseline scenarios run several Agent turns against one application, and a counter-based
// script would hand the second turn's first call the first turn's answer. That is a defect
// of the harness rather than of the product, and it would have quietly turned every later
// retrieval into an ungrounded turn.
type baselineAgentChat struct {
	toolName string

	mu    sync.Mutex
	query string
	calls int
}

// setQuery chooses the query the next tool call will carry. Scenarios that retrieve more
// than once set it before each turn, so the query always belongs to the document under
// test rather than to whichever document was uploaded first.
func (m *baselineAgentChat) setQuery(query string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.query = query
}

func (m *baselineAgentChat) Generate(_ context.Context, input []*schema.Message, opts ...modelcomponent.Option) (*schema.Message, error) {
	return m.reply(input, opts...)
}

func (m *baselineAgentChat) Stream(_ context.Context, input []*schema.Message, opts ...modelcomponent.Option) (*schema.StreamReader[*schema.Message], error) {
	message, err := m.reply(input, opts...)
	if err != nil {
		return nil, err
	}
	if message.ResponseMeta == nil {
		message.ResponseMeta = &schema.ResponseMeta{FinishReason: "stop"}
		if len(message.ToolCalls) > 0 {
			message.ResponseMeta.FinishReason = "tool_calls"
		}
	}
	reader, writer := schema.Pipe[*schema.Message](1)
	writer.Send(message, nil)
	writer.Close()
	return reader, nil
}

func (m *baselineAgentChat) WithTools([]*schema.ToolInfo) (modelcomponent.ToolCallingChatModel, error) {
	return m, nil
}

func (m *baselineAgentChat) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func (m *baselineAgentChat) reply(input []*schema.Message, opts ...modelcomponent.Option) (*schema.Message, error) {
	m.mu.Lock()
	m.calls++
	m.mu.Unlock()
	options := modelcomponent.GetCommonOptions(nil, opts...)
	if options.ToolChoice != nil && *options.ToolChoice == schema.ToolChoiceForbidden {
		// The citation-repair call runs with tools forbidden. Answering it with the same
		// cited sentence keeps that path exercised without pretending a tool ran.
		return schema.AssistantMessage(baselineCitedAnswer, nil), nil
	}
	for _, message := range input {
		if message != nil && message.Role == schema.Tool {
			return schema.AssistantMessage(baselineCitedAnswer, nil), nil
		}
	}
	m.mu.Lock()
	query := m.query
	m.mu.Unlock()
	arguments, err := json.Marshal(map[string]any{"query": query, "top_k": 5})
	if err != nil {
		return nil, err
	}
	return schema.AssistantMessage("", []schema.ToolCall{{
		ID: "baseline-call", Type: "function",
		Function: schema.FunctionCall{Name: m.toolName, Arguments: string(arguments)},
	}}), nil
}

// ------------------------------------------------------------- fake chunk store

type baselineStoredChunk struct {
	UserID               int64
	DocID                string
	DocVersion           int
	Chunk                rag.Chunk
	Vector               []float32
	EmbeddingInputHash   string
	EmbeddingFingerprint string
}

// baselineChunkStore is an in-process stand-in for the Redis chunk repository.
//
// It saves what it is given, returns only the active versions it is asked for, honours
// the same one-user-document-version-per-batch contract validateEmbeddedChunks enforces
// in production, and answers both search channels with real ranking over the stored
// vectors and text. That is what lets a test prove a chunk is retrievable through the
// production RAGService instead of asserting against a store that discards everything.
//
// It is NOT a Redis substitute for measurement: its latency is a map lookup, its
// isolation is a map key rather than a RediSearch TAG filter, and its lexical ranking is
// term containment rather than the index's Chinese analyzer and BM25STD. No timing or
// ranking measured here describes Redis.
type baselineChunkStore struct {
	mu sync.Mutex

	chunks map[string]baselineStoredChunk

	storeCalls        int
	vectorSearchCalls int
	textSearchCalls   int
	deletedVersions   int
	loadCalls         int
	ensureCalls       int
}

func newBaselineChunkStore() *baselineChunkStore {
	return &baselineChunkStore{chunks: map[string]baselineStoredChunk{}}
}

func baselineChunkKey(userID int64, docID string, version int, chunkID string) string {
	return redisCommon.GenerateKnowledgeChunkKey(userID, docID, version, chunkID)
}

func (s *baselineChunkStore) EnsureIndex(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureCalls++
	return nil
}

func (s *baselineChunkStore) Store(_ context.Context, chunks []rag.EmbeddedChunk) error {
	if len(chunks) == 0 {
		return nil
	}
	first := chunks[0]
	if first.UserID <= 0 || first.Chunk.DocID == "" || first.Chunk.DocVersion <= 0 {
		return errors.New("baseline chunk store: invalid document identity")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.storeCalls++
	// Same batch contract the production repository enforces before it writes.
	desired := map[string]struct{}{}
	for _, embedded := range chunks {
		if embedded.UserID != first.UserID || embedded.Chunk.DocID != first.Chunk.DocID || embedded.Chunk.DocVersion != first.Chunk.DocVersion {
			return errors.New("baseline chunk store: batch must contain one user document version")
		}
		if embedded.Chunk.ID == "" || len(embedded.Vector) != baselineDimension {
			return errors.New("baseline chunk store: invalid chunk ID or vector dimension")
		}
		key := baselineChunkKey(embedded.UserID, embedded.Chunk.DocID, embedded.Chunk.DocVersion, embedded.Chunk.ID)
		if _, exists := desired[key]; exists {
			return fmt.Errorf("baseline chunk store: duplicate key %q", key)
		}
		desired[key] = struct{}{}
	}
	// A version is replaced wholesale, as the production Store does when it deletes the
	// stale keys under this document version's prefix.
	for key, stored := range s.chunks {
		if stored.UserID == first.UserID && stored.DocID == first.Chunk.DocID && stored.DocVersion == first.Chunk.DocVersion {
			if _, keep := desired[key]; !keep {
				delete(s.chunks, key)
			}
		}
	}
	for _, embedded := range chunks {
		key := baselineChunkKey(embedded.UserID, embedded.Chunk.DocID, embedded.Chunk.DocVersion, embedded.Chunk.ID)
		s.chunks[key] = baselineStoredChunk{
			UserID: embedded.UserID, DocID: embedded.Chunk.DocID, DocVersion: embedded.Chunk.DocVersion,
			Chunk: embedded.Chunk, Vector: append([]float32(nil), embedded.Vector...),
			EmbeddingInputHash: embedded.EmbeddingInputHash, EmbeddingFingerprint: embedded.EmbeddingFingerprint,
		}
	}
	return nil
}

func (s *baselineChunkStore) LoadDocumentVersionEmbeddings(_ context.Context, userID int64, docID string, version int) ([]rag.StoredChunkEmbedding, error) {
	if userID <= 0 || docID == "" || version <= 0 {
		return nil, errors.New("baseline chunk store: invalid user, document, or version")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadCalls++
	loaded := make([]rag.StoredChunkEmbedding, 0, len(s.chunks))
	for _, key := range s.sortedKeys() {
		stored := s.chunks[key]
		if stored.UserID != userID || stored.DocID != docID || stored.DocVersion != version {
			continue
		}
		loaded = append(loaded, rag.StoredChunkEmbedding{
			UserID: stored.UserID, DocID: stored.DocID, DocVersion: stored.DocVersion, ChunkID: stored.Chunk.ID,
			EmbeddingContent: stored.Chunk.EmbeddingContent, EmbeddingInputHash: stored.EmbeddingInputHash,
			EmbeddingFingerprint: stored.EmbeddingFingerprint, Vector: append([]float32(nil), stored.Vector...),
		})
	}
	return loaded, nil
}

func (s *baselineChunkStore) SearchActiveByUserVector(_ context.Context, userID int64, vector []float32, topK int, activeVersions map[string]int) ([]rag.RedisChunkSearchResult, error) {
	if userID <= 0 || len(vector) != baselineDimension || topK <= 0 {
		return nil, errors.New("baseline chunk store: invalid user, vector dimension, or topK")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.vectorSearchCalls++
	type scored struct {
		result rag.RedisChunkSearchResult
		score  float64
	}
	ranked := make([]scored, 0, len(s.chunks))
	for _, key := range s.sortedKeys() {
		stored := s.chunks[key]
		if stored.UserID != userID || !baselineActive(activeVersions, stored.DocID, stored.DocVersion) {
			continue
		}
		cosine := baselineCosine(vector, stored.Vector)
		result := baselineSearchResult(key, stored)
		result.Distance = 1 - cosine
		ranked = append(ranked, scored{result: result, score: cosine})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		return ranked[i].result.RedisKey < ranked[j].result.RedisKey
	})
	if topK > len(ranked) {
		topK = len(ranked)
	}
	results := make([]rag.RedisChunkSearchResult, topK)
	for index := 0; index < topK; index++ {
		results[index] = ranked[index].result
	}
	return results, nil
}

func (s *baselineChunkStore) SearchActiveByUserText(_ context.Context, userID int64, query string, topK int, activeVersions map[string]int) ([]rag.RedisChunkSearchResult, error) {
	if userID <= 0 || topK <= 0 {
		return nil, errors.New("baseline chunk store: invalid user or topK")
	}
	terms := rag.ExtractLexicalTerms(query)
	if len(terms) == 0 {
		return []rag.RedisChunkSearchResult{}, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.textSearchCalls++
	type scored struct {
		result rag.RedisChunkSearchResult
		score  float64
	}
	ranked := make([]scored, 0, len(s.chunks))
	for _, key := range s.sortedKeys() {
		stored := s.chunks[key]
		if stored.UserID != userID || !baselineActive(activeVersions, stored.DocID, stored.DocVersion) {
			continue
		}
		haystack := strings.ToLower(strings.Join(stored.Chunk.TitlePath, " ") + " " + stored.Chunk.SourceFile + " " +
			strings.Join(stored.Chunk.Symbols, " ") + " " + stored.Chunk.RawContent)
		score := 0.0
		for _, term := range terms {
			if strings.Contains(haystack, strings.ToLower(term)) {
				score++
			}
		}
		if score == 0 {
			continue
		}
		result := baselineSearchResult(key, stored)
		result.LexicalScore = score
		result.Distance = 1
		ranked = append(ranked, scored{result: result, score: score})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		return ranked[i].result.RedisKey < ranked[j].result.RedisKey
	})
	if topK > len(ranked) {
		topK = len(ranked)
	}
	results := make([]rag.RedisChunkSearchResult, topK)
	for index := 0; index < topK; index++ {
		results[index] = ranked[index].result
	}
	return results, nil
}

func (s *baselineChunkStore) DeleteDocumentVersion(_ context.Context, userID int64, docID string, version int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, stored := range s.chunks {
		if stored.UserID == userID && stored.DocID == docID && stored.DocVersion == version {
			delete(s.chunks, key)
		}
	}
	s.deletedVersions++
	return nil
}

// Reset discards every stored chunk. The restart case uses it to run the same scenario
// once with the index retained and once with it cleared, so the difference in re-embedding
// is measured instead of assumed.
func (s *baselineChunkStore) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.chunks = map[string]baselineStoredChunk{}
}

func (s *baselineChunkStore) counts() (store, vector, text, deleted, load, ensure int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.storeCalls, s.vectorSearchCalls, s.textSearchCalls, s.deletedVersions, s.loadCalls, s.ensureCalls
}

func (s *baselineChunkStore) versionChunks(userID int64, docID string, version int) []baselineStoredChunk {
	s.mu.Lock()
	defer s.mu.Unlock()
	found := make([]baselineStoredChunk, 0)
	for _, key := range s.sortedKeys() {
		stored := s.chunks[key]
		if stored.UserID == userID && stored.DocID == docID && stored.DocVersion == version {
			found = append(found, stored)
		}
	}
	sort.Slice(found, func(i, j int) bool { return found[i].Chunk.Index < found[j].Chunk.Index })
	return found
}

func (s *baselineChunkStore) countVersion(userID int64, docID string, version int) int {
	return len(s.versionChunks(userID, docID, version))
}

func (s *baselineChunkStore) totalChunks() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.chunks)
}

func (s *baselineChunkStore) sortedKeys() []string {
	keys := make([]string, 0, len(s.chunks))
	for key := range s.chunks {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func baselineActive(activeVersions map[string]int, docID string, version int) bool {
	active, exists := activeVersions[docID]
	return exists && active == version
}

func baselineSearchResult(key string, stored baselineStoredChunk) rag.RedisChunkSearchResult {
	chunk := stored.Chunk
	return rag.RedisChunkSearchResult{
		RedisKey: key, ChunkID: chunk.ID, DocID: chunk.DocID, DocVersion: chunk.DocVersion,
		ChunkIndex: chunk.Index, ChunkType: string(chunk.Type), SourceFile: chunk.SourceFile,
		TitlePath: strings.Join(chunk.TitlePath, " > "), StartLine: chunk.StartLine, EndLine: chunk.EndLine,
		RawContent: chunk.RawContent, EmbeddingContent: chunk.EmbeddingContent, ContentHash: chunk.ContentHash,
		CodeLanguage: chunk.CodeLanguage, Symbols: strings.Join(chunk.Symbols, " "), TokenEstimate: chunk.TokenEstimate,
		PartIndex: chunk.PartIndex, PartTotal: chunk.PartTotal,
	}
}

func baselineCosine(left []float32, right []float32) float64 {
	if len(left) != len(right) || len(left) == 0 {
		return 0
	}
	dot, leftNorm, rightNorm := 0.0, 0.0, 0.0
	for index := range left {
		dot += float64(left[index]) * float64(right[index])
		leftNorm += float64(left[index]) * float64(left[index])
		rightNorm += float64(right[index]) * float64(right[index])
	}
	if leftNorm == 0 || rightNorm == 0 {
		return 0
	}
	return dot / (math.Sqrt(leftNorm) * math.Sqrt(rightNorm))
}

// ---------------------------------------------------------------- fake world

type baselineOptions struct {
	databasePath string
	storageDir   string
	store        *baselineChunkStore
	embedder     *baselineEmbedder
	chat         modelcomponent.ToolCallingChatModel
	scanSeconds  int
}

// baselineWorld owns the state that has to outlive one application assembly: the SQLite
// file, the storage root, the fake chunk store and the fake embedder. Re-assembling is
// therefore a real restart over the same durable state, which is what the recovery case
// needs and what the existing per-test fixtures cannot express.
type baselineWorld struct {
	// Negative controls capture rejection from the actual retrieveChecked path.
	// Normal callers leave this nil and retain the fatal assertion.
	retrievalRejected func([]string)
	t                 *testing.T
	dbPath            string
	storageDir        string
	store             *baselineChunkStore
	embedder          *baselineEmbedder
	chat              modelcomponent.ToolCallingChatModel
	scanSecond        int

	db     *gorm.DB
	app    *Application
	users  bool
	closed bool
	// sessions is keyed by the token that created the session. One cached session for the
	// whole world would let a second identity query through the first identity's session,
	// which is the mistake a cross-user case exists to rule out: the test would then be
	// asserting on the wrong user's data and could pass while isolation was broken.
	sessions map[string]string
	// tokens maps a fixture user id to the token login issued for it. It exists so a check
	// can name the user a result must belong to without decoding JWT claims: the fixture
	// created these accounts, so it already knows which id each one is.
	tokens map[int64]string
	runs   int
}

func newBaselineWorld(t *testing.T, opts baselineOptions) *baselineWorld {
	t.Helper()
	w := &baselineWorld{
		t: t, dbPath: filepath.Join(t.TempDir(), "baseline.db"), storageDir: t.TempDir(),
		store: opts.store, embedder: opts.embedder, chat: opts.chat, scanSecond: opts.scanSeconds,
		sessions: map[string]string{}, tokens: map[int64]string{},
	}
	if opts.databasePath != "" {
		w.dbPath = opts.databasePath
	}
	if opts.storageDir != "" {
		w.storageDir = opts.storageDir
	}
	if w.store == nil {
		w.store = newBaselineChunkStore()
	}
	if w.embedder == nil {
		w.embedder = &baselineEmbedder{dimension: baselineDimension}
	}
	if w.chat == nil {
		w.chat = &baselineAgentChat{toolName: agentService.MustToolNameFor(agentService.CapabilityKnowledgeSearch, agentService.SurfaceLegacy)}
	}
	if w.scanSecond <= 0 {
		w.scanSecond = 1
	}
	t.Cleanup(func() {
		if err := w.Close(); err != nil {
			t.Error(err)
		}
	})
	w.Assemble()
	return w
}

func baselineMigrate(db *gorm.DB) error {
	return db.AutoMigrate(&model.User{}, &model.Session{}, &model.Message{}, &model.AgentRun{}, &model.AgentStep{}, &model.SessionSummary{}, &model.KnowledgeDocument{}, &model.LearningSession{}, &model.LearningQuestion{}, &model.LearningAttempt{}, &model.TopicProgress{}, &model.LongTermMemory{}, &model.MemoryDraft{}, &model.MemoryRevision{}, &model.MemoryCandidate{}, &model.MemoryExtractionJob{}, &model.MemoryDreamRun{}, &model.EpisodeMemory{}, &model.EpisodeProjectionJob{}, &model.EpisodeAssertionState{}, &model.EpisodeAssertionRelation{}, &model.EpisodeRelationProposal{})
}

// Assemble opens the durable state again and builds a fresh application over it. Calling
// it after Close is the graceful stop / rebuild the task asks for; it is not a crash and
// must not be described as one.
func (w *baselineWorld) Assemble() {
	w.t.Helper()
	db, err := gorm.Open(sqlite.Open(w.dbPath), &gorm.Config{Logger: logger.Default.LogMode(logger.Warn)})
	if err != nil {
		w.t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		w.t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	if !w.users {
		if err := baselineMigrate(db); err != nil {
			_ = sqlDB.Close()
			w.t.Fatal(err)
		}
		if err := db.Exec("ALTER TABLE users ADD COLUMN email_normalized TEXT GENERATED ALWAYS AS (NULLIF(LOWER(TRIM(email)), '')) STORED").Error; err != nil {
			_ = sqlDB.Close()
			w.t.Fatal(err)
		}
		hash, err := utils.HashPassword("synthetic-password")
		if err != nil {
			_ = sqlDB.Close()
			w.t.Fatal(err)
		}
		for _, user := range []model.User{
			{ID: 1, Username: "learner", Email: "learner@invalid.example", Password: hash},
			{ID: 2, Username: "other", Email: "other@invalid.example", Password: hash},
		} {
			if err := db.Create(&user).Error; err != nil {
				_ = sqlDB.Close()
				w.t.Fatal(err)
			}
		}
		w.users = true
	}
	w.db = db

	settings := testSettings(w.t)
	settings.Config.KnowledgeConfig.StorageDir = w.storageDir
	settings.Config.KnowledgeConfig.ScanIntervalSeconds = w.scanSecond
	chat := w.chat
	if chat == nil {
		chat = &scriptedChatModel{}
	}
	app, err := Assemble(context.Background(), settings, baselineDependencies(db, chat, w.store, w.embedder), &ordinaryModel{}, conversation.Options{})
	if err != nil {
		_ = sqlDB.Close()
		w.t.Fatal(err)
	}
	w.app = app
	w.closed = false
	w.sessions = map[string]string{}
}

func (w *baselineWorld) Close() error {
	if w.app == nil || w.closed {
		return nil
	}
	w.closed = true
	return w.app.Close()
}

func baselineDependencies(db *gorm.DB, chat modelcomponent.ToolCallingChatModel, store *baselineChunkStore, embedder embedding.Embedder) entry.Dependencies {
	return entry.Dependencies{
		Clock: time.Now,
		Infrastructure: entry.Infrastructure{
			OpenDatabase:       func(context.Context, *config.Config) (*gorm.DB, error) { return db, nil },
			InitializeDatabase: func(*gorm.DB) error { return nil },
			OpenRedis: func(*config.Config) (*redisclient.Client, error) {
				client := redisclient.NewClient(&redisclient.Options{Addr: "offline.invalid:6379", MaxRetries: -1})
				client.AddHook(redisTransport{})
				return client, nil
			},
		},
		ChatModel: func(context.Context, *entry.Settings) (modelcomponent.ToolCallingChatModel, error) { return chat, nil },
		Embedder:  func(context.Context, *config.Config) (embedding.Embedder, error) { return embedder, nil },
		Chunks:    func(*redisclient.Client, int) (entry.ChunkStore, error) { return store, nil },
	}
}

// ---------------------------------------------------------------- HTTP surface

func baselineMultipart(name string, content []byte) (*bytes.Buffer, string) {
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	file, err := writer.CreateFormFile("file", name)
	if err != nil {
		panic(err)
	}
	if _, err := file.Write(content); err != nil {
		panic(err)
	}
	_ = writer.Close()
	return body, writer.FormDataContentType()
}

func (w *baselineWorld) request(verb, path, token, name string, content []byte) (int, map[string]any) {
	w.t.Helper()
	body := &bytes.Buffer{}
	kind := ""
	if name != "" {
		body, kind = baselineMultipart(name, content)
	}
	req := httptest.NewRequest(verb, path, body)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if kind != "" {
		req.Header.Set("Content-Type", kind)
	}
	recorder := httptest.NewRecorder()
	w.app.Handler.ServeHTTP(recorder, req)
	var parsed map[string]any
	if len(recorder.Body.Bytes()) > 0 {
		if err := json.Unmarshal(recorder.Body.Bytes(), &parsed); err != nil {
			w.t.Fatalf("%s %s: body is not JSON: %q", verb, path, recorder.Body.String())
		}
	}
	return recorder.Code, parsed
}

// baselineFixtureUserID is the id the fixture gives each synthetic account, read from the
// email so a caller never has to repeat the mapping.
func baselineFixtureUserID(email string) int64 {
	if strings.HasPrefix(email, "other@") {
		return 2
	}
	return 1
}

func (w *baselineWorld) login(email string) string {
	w.t.Helper()
	token := loginHTTP(w.t, w.app.Handler, email)
	w.tokens[baselineFixtureUserID(email)] = token
	return token
}

func (w *baselineWorld) upload(token, name string, content []byte) (int, map[string]any) {
	w.t.Helper()
	return w.request("POST", "/api/v1/knowledge/documents", token, name, content)
}

func (w *baselineWorld) document(token, id string) map[string]any {
	w.t.Helper()
	status, body := w.request("GET", "/api/v1/knowledge/documents/"+id, token, "", nil)
	if status != 200 {
		w.t.Fatalf("document %s: %d %v", id, status, body)
	}
	return docView(w.t, body)
}

// baselineWaitOutcome is what the poller observed, including how long it took. The
// duration is a wall-clock observation of this machine while the other development
// window was building and testing; it is a smoke value, not a performance result.
type baselineWaitOutcome struct {
	View     map[string]any
	State    string
	Elapsed  time.Duration
	Deadline bool
}

func (w *baselineWorld) waitDocumentState(token, id, state string, timeout time.Duration) baselineWaitOutcome {
	w.t.Helper()
	started := time.Now()
	deadline := started.Add(timeout)
	last := ""
	for {
		view := w.document(token, id)
		last = fmt.Sprint(view["index_status"])
		if last == state {
			return baselineWaitOutcome{View: view, State: last, Elapsed: time.Since(started)}
		}
		if time.Now().After(deadline) {
			return baselineWaitOutcome{View: view, State: last, Elapsed: time.Since(started), Deadline: true}
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func (w *baselineWorld) requireDocumentState(token, id, state string, timeout time.Duration) baselineWaitOutcome {
	w.t.Helper()
	outcome := w.waitDocumentState(token, id, state, timeout)
	if outcome.Deadline {
		w.t.Fatalf("document %s did not reach %s within %s (last %s, %+v)", id, state, timeout, outcome.State, outcome.View)
	}
	return outcome
}

// agentSession returns the agent session belonging to this token, creating it on first use.
// Sessions are per identity: a single cached session shared by two users would let the
// second identity query through the first identity's session, so a cross-user case would
// assert on the wrong user's data and could pass while isolation was broken.
func (w *baselineWorld) agentSession(token string) string {
	w.t.Helper()
	if existing, ok := w.sessions[token]; ok {
		return existing
	}
	body, _ := json.Marshal(map[string]string{"mode": "learning_agent", "title": "baseline", "requestId": "baseline-session"})
	status, envelope, _ := requestHTTP(w.t, w.app.Handler, "POST", "/api/v1/sessions", string(body), token)
	if status != 200 || envelope["error"] != nil {
		w.t.Fatalf("agent session: %d %v", status, envelope)
	}
	created := envelope["data"].(map[string]any)["id"].(string)
	w.sessions[token] = created
	return created
}

// ------------------------------------------------------- retrieval identity checks

// baselineExpectedDocument is one document version a query is allowed to return.
type baselineExpectedDocument struct {
	DocID   string
	Version int
	Label   string
}

// baselineRetrievalExpectation is the complete rule one retrieval result is judged by.
// Every returned source must be an allowed document at its currently valid version; a
// multi-document query must include the target document; and an empty-owner stage can
// explicitly require no source at all.
type baselineRetrievalExpectation struct {
	Allowed         []baselineExpectedDocument
	MustInclude     []string
	ForbiddenDocIDs []string
	ExpectNoSources bool
}

// baselineSourceVerdict preserves both the failure reasons and the identity list seen in
// the response. Keeping the two together makes a pollution test able to prove the verifier
// rejected the injected identity rather than simply returning no evidence.
type baselineSourceVerdict struct {
	Problems []string
	Seen     []string
}

func (v baselineSourceVerdict) Failed() bool { return len(v.Problems) > 0 }

func baselineLabelOf(labels map[string]string, docID string) string {
	if label := labels[docID]; label != "" {
		return label
	}
	sum := sha256.Sum256([]byte(docID))
	return "unexpected document " + hex.EncodeToString(sum[:6])
}

// baselineVerifySources is pure response verification. It is deliberately independent of
// the HTTP/Worker path so the same verifier can be run against a deliberately polluted
// response as a negative control.
func baselineVerifySources(expectation baselineRetrievalExpectation, sources []map[string]any) baselineSourceVerdict {
	verdict := baselineSourceVerdict{}
	allowed := make(map[string]int, len(expectation.Allowed))
	labels := make(map[string]string, len(expectation.Allowed))
	for _, document := range expectation.Allowed {
		allowed[document.DocID] = document.Version
		labels[document.DocID] = document.Label
	}
	forbidden := make(map[string]struct{}, len(expectation.ForbiddenDocIDs))
	for _, docID := range expectation.ForbiddenDocIDs {
		forbidden[docID] = struct{}{}
	}
	counts := map[string]int{}
	for position, source := range sources {
		if source == nil {
			verdict.Problems = append(verdict.Problems, fmt.Sprintf("source %d is not an object", position))
			continue
		}
		docID, _ := source["doc_id"].(string)
		if docID == "" {
			verdict.Problems = append(verdict.Problems, fmt.Sprintf("source %d has missing or invalid doc_id", position))
		}
		if index, ok := baselineSourceInt(source["chunk_index"]); !ok || index < 0 {
			verdict.Problems = append(verdict.Problems, fmt.Sprintf("source %d has missing or invalid chunk_index", position))
		}
		for _, field := range []string{"start_line", "end_line"} {
			if line, ok := baselineSourceInt(source[field]); !ok || line <= 0 {
				verdict.Problems = append(verdict.Problems, fmt.Sprintf("source %d has missing or invalid %s", position, field))
			}
		}
		if !baselineValidTitlePath(source) {
			verdict.Problems = append(verdict.Problems, fmt.Sprintf("source %d has missing or invalid title_path", position))
		}
		verdict.Seen = append(verdict.Seen, docID)
		if _, denied := forbidden[docID]; denied {
			verdict.Problems = append(verdict.Problems, fmt.Sprintf("source %d is forbidden: %s", position, baselineLabelOf(labels, docID)))
			continue
		}
		version, ok := baselineSourceInt(source["doc_version"])
		if !ok || version <= 0 {
			verdict.Problems = append(verdict.Problems, fmt.Sprintf("source %d has no readable document version", position))
			continue
		}
		expected, known := allowed[docID]
		if !known {
			verdict.Problems = append(verdict.Problems, fmt.Sprintf("source %d is not an active document for this user: %s", position, baselineLabelOf(labels, docID)))
			continue
		}
		if version != expected {
			verdict.Problems = append(verdict.Problems, fmt.Sprintf("source %d is %s at version %d, want active version %d", position, baselineLabelOf(labels, docID), version, expected))
			continue
		}
		counts[docID]++
	}
	for _, required := range expectation.MustInclude {
		if counts[required] == 0 {
			verdict.Problems = append(verdict.Problems, fmt.Sprintf("no source came from the target document: %s", baselineLabelOf(labels, required)))
		}
	}
	if expectation.ExpectNoSources && len(sources) > 0 {
		verdict.Problems = append(verdict.Problems, fmt.Sprintf("no source may be returned here, saw %d", len(sources)))
	}
	if !expectation.ExpectNoSources && len(sources) == 0 {
		verdict.Problems = append(verdict.Problems, "the query returned no source")
	}
	return verdict
}

func baselineSourceInt(value any) (int, bool) {
	switch typed := value.(type) {
	case float64:
		// The upper bound is exclusive: float64(MaxInt) can round up to 2^63.
		limit := math.Ldexp(1, strconv.IntSize-1)
		if math.IsNaN(typed) || math.IsInf(typed, 0) || math.Trunc(typed) != typed || typed < -limit || typed >= limit {
			return 0, false
		}
		return int(typed), true
	case int:
		return typed, true
	case json.Number:
		parsed, err := typed.Int64()
		return int(parsed), err == nil && int64(int(parsed)) == parsed
	default:
		return 0, false
	}
}

func baselineSourceStrings(value any) []string {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		text, ok := item.(string)
		if !ok {
			return nil
		}
		out = append(out, text)
	}
	return out
}

// baselineLocationProblems cross-checks every source's citation location against the
// corresponding stored chunk. This catches a valid document id paired with a line/index
// from a different version or a fabricated title path.
func baselineLocationProblems(store *baselineChunkStore, userID int64, sources []map[string]any) []string {
	var problems []string
	for position, source := range sources {
		docID, _ := source["doc_id"].(string)
		version, versionOK := baselineSourceInt(source["doc_version"])
		index, indexOK := baselineSourceInt(source["chunk_index"])
		if !versionOK || version <= 0 {
			problems = append(problems, fmt.Sprintf("source %d has invalid doc_version", position))
		}
		if !indexOK || index < 0 {
			problems = append(problems, fmt.Sprintf("source %d has missing or invalid chunk_index", position))
		}
		if !versionOK || version <= 0 || !indexOK || index < 0 {
			continue
		}
		stored := store.versionChunks(userID, docID, version)
		var found *baselineStoredChunk
		for i := range stored {
			if stored[i].Chunk.Index == index {
				found = &stored[i]
				break
			}
		}
		if found == nil {
			problems = append(problems, fmt.Sprintf("source %d cites an un-stored document/version/chunk", position))
			continue
		}
		if start, ok := baselineSourceInt(source["start_line"]); !ok || start <= 0 || start != found.Chunk.StartLine {
			problems = append(problems, fmt.Sprintf("source %d start_line does not match stored chunk", position))
		}
		if end, ok := baselineSourceInt(source["end_line"]); !ok || end <= 0 || end != found.Chunk.EndLine {
			problems = append(problems, fmt.Sprintf("source %d end_line does not match stored chunk", position))
		}
		if !baselineValidTitlePath(source) || !baselineSameStrings(baselineSourceStrings(source["title_path"]), found.Chunk.TitlePath) {
			problems = append(problems, fmt.Sprintf("source %d title_path does not match stored chunk", position))
		}
	}
	return problems
}

func baselineSameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

// Successful responses must carry sources. The product serializes a nil source
// slice as null; null and [] are empty results, still rejected when hits are required.
func baselineSourcesOf(data map[string]any) ([]map[string]any, []string) {
	if value, exists := data["sources"]; exists && value == nil {
		return nil, nil
	}
	items, ok := data["sources"].([]any)
	if !ok {
		return nil, []string{"sources is missing or is not an array"}
	}
	out := make([]map[string]any, 0, len(items))
	var problems []string
	for position, item := range items {
		source, ok := item.(map[string]any)
		if !ok || source == nil {
			problems = append(problems, fmt.Sprintf("source %d is not an object", position))
		}
		// Retain the slot even when malformed; never filter a bad later source.
		out = append(out, source)
	}
	return out, problems
}

// baselineSourceObserver is nil on the normal path. The negative-control test temporarily
// sets it to corrupt the response before verification, proving that the verifier fails
// instead of filtering polluted output into a false pass. It returns the sources to verify,
// so a corruption can also add an entry rather than only rewrite one.
var baselineSourceObserver func(sources []any) []any

func baselineSourceDigestOf(source map[string]any) map[string]any {
	digest := map[string]any{
		"doc_id": source["doc_id"], "doc_version": source["doc_version"],
		"chunk_index": source["chunk_index"], "start_line": source["start_line"],
		"end_line": source["end_line"], "title_path_depth": len(baselineSourceStrings(source["title_path"])),
	}
	path := baselineSourceStrings(source["title_path"])
	if len(path) > 0 {
		sum := sha256.Sum256([]byte(strings.Join(path, "\x1f")))
		digest["title_path_sha256_16"] = hex.EncodeToString(sum[:8])
	}
	return digest
}

func baselineSourceDigestsOf(sources []map[string]any) []map[string]any {
	digests := make([]map[string]any, 0, len(sources))
	for _, source := range sources {
		digests = append(digests, baselineSourceDigestOf(source))
	}
	return digests
}

// baselineRetrieval is one Agent turn with the response, all sources and its observation
// time. The same shape is used by normal and negative-control paths.
type baselineRetrieval struct {
	Data     map[string]any
	Sources  []map[string]any
	Elapsed  time.Duration
	Problems []string
}

func (w *baselineWorld) retrieveRaw(token, query string) baselineRetrieval {
	w.t.Helper()
	if chat, ok := w.chat.(*baselineAgentChat); ok {
		chat.setQuery(query)
	}
	sid := w.agentSession(token)
	w.runs++
	payload, _ := json.Marshal(map[string]any{"content": query, "requestId": fmt.Sprintf("baseline-retrieval-%d", w.runs)})
	started := time.Now()
	status, envelope, body := requestHTTP(w.t, w.app.Handler, "POST", "/api/v1/sessions/"+sid+"/messages", string(payload), token)
	elapsed := time.Since(started)
	if status != 200 {
		w.t.Fatalf("agent retrieval failed for query %q: %d %s", query, status, body)
	}
	data, ok := envelope["data"].(map[string]any)
	if !ok {
		w.t.Fatalf("agent retrieval returned no data: %v", envelope)
	}
	if raw, ok := data["sources"].([]any); ok && baselineSourceObserver != nil {
		data["sources"] = baselineSourceObserver(raw)
	}
	sources, problems := baselineSourcesOf(data)
	return baselineRetrieval{Data: data, Sources: sources, Problems: problems, Elapsed: elapsed}
}

// baselineTolerantRetrieval retains the fixture's model failure as a failure observation.
// Isolation additionally requires the actual pre-generation knowledge tool output.
type baselineTolerantRetrieval struct {
	Status    int
	Data      map[string]any
	Sources   []map[string]any
	Refusal   string
	Elapsed   time.Duration
	Envelope  map[string]any
	Problems  []string
	ToolSteps []model.AgentStep
}

func (w *baselineWorld) retrieveTolerant(token, query string) baselineTolerantRetrieval {
	w.t.Helper()
	if chat, ok := w.chat.(*baselineAgentChat); ok {
		chat.setQuery(query)
	}
	sid := w.agentSession(token)
	w.runs++
	payload, _ := json.Marshal(map[string]any{"content": query, "requestId": fmt.Sprintf("baseline-tolerant-%d", w.runs)})
	started := time.Now()
	status, envelope, _ := requestHTTP(w.t, w.app.Handler, "POST", "/api/v1/sessions/"+sid+"/messages", string(payload), token)
	elapsed := time.Since(started)
	result := baselineTolerantRetrieval{Status: status, Envelope: envelope, Elapsed: elapsed}
	if data, ok := envelope["data"].(map[string]any); ok {
		result.Data = data
		result.Sources, result.Problems = baselineSourcesOf(data)
	}
	if status != 200 {
		if failure, ok := envelope["error"].(map[string]any); ok {
			result.Refusal = fmt.Sprint(failure["code"])
		}
	}
	// The runner persists the parsed tool result before taking the empty-evidence
	// boundary and attempting fallback generation. Scope to this user's exact run,
	// so an earlier successful retrieval cannot supply the evidence.
	var run model.AgentRun
	requestKey := fmt.Sprintf("baseline-tolerant-%d", w.runs)
	if err := w.db.Where("user_id = ? AND session_id = ? AND request_key = ?", w.userIDOfToken(token), sid, requestKey).First(&run).Error; err != nil {
		result.Problems = append(result.Problems, "empty-owner run not recorded")
	} else if err := w.db.Where("user_id = ? AND run_id = ? AND type = ? AND tool_name = ?", w.userIDOfToken(token), run.ID, model.AgentStepTool, baselineToolName(w.t)).Order("sequence").Find(&result.ToolSteps).Error; err != nil {
		result.Problems = append(result.Problems, "empty-owner tool observations unavailable")
	}
	return result
}

func (w *baselineWorld) retrieveChecked(token, query string, expectation baselineRetrievalExpectation) baselineRetrieval {
	w.t.Helper()
	attempt := w.retrieveRaw(token, query)
	verdict := baselineVerifySources(expectation, attempt.Sources)
	verdict.Problems = append(verdict.Problems, attempt.Problems...)
	verdict.Problems = append(verdict.Problems, baselineLocationProblems(w.store, w.userIDOfToken(token), attempt.Sources)...)
	attempt.Problems = verdict.Problems
	if len(attempt.Problems) > 0 {
		if w.retrievalRejected != nil {
			w.retrievalRejected(append([]string(nil), attempt.Problems...))
			return attempt
		}
		w.t.Fatalf("retrieval %q failed identity/location verification: %v", query, attempt.Problems)
	}
	return attempt
}

func (w *baselineWorld) retrieve(token, query string) (map[string]any, time.Duration) {
	attempt := w.retrieveRaw(token, query)
	return attempt.Data, attempt.Elapsed
}

// userIDOfToken maps the fixture's two login tokens without decoding JWT claims.
func (w *baselineWorld) userIDOfToken(token string) int64 {
	if w.tokens != nil && w.tokens[2] == token {
		return 2
	}
	return 1
}

// ---------------------------------------------------------------- digests
// ---------------------------------------------------------------- digests

// baselineChunkDigest is the frozen localisation summary of one chunk. It records where
// the chunk sits and what it hashes to, never the text: the frozen notes are private and
// their bodies must not reach a committed file.
type baselineChunkDigest struct {
	Index         int      `json:"index"`
	Type          string   `json:"type"`
	StartLine     int      `json:"start_line"`
	EndLine       int      `json:"end_line"`
	TitlePath     []string `json:"title_path"`
	TokenEstimate int      `json:"token_estimate"`
	RawHash       string   `json:"raw_sha256_16"`
	PartIndex     int      `json:"part_index"`
	PartTotal     int      `json:"part_total"`
}

func baselineDigestChunks(chunks []baselineStoredChunk) []baselineChunkDigest {
	digests := make([]baselineChunkDigest, 0, len(chunks))
	for _, stored := range chunks {
		sum := sha256.Sum256([]byte(stored.Chunk.RawContent))
		digests = append(digests, baselineChunkDigest{
			Index: stored.Chunk.Index, Type: string(stored.Chunk.Type),
			StartLine: stored.Chunk.StartLine, EndLine: stored.Chunk.EndLine,
			TitlePath: stored.Chunk.TitlePath, TokenEstimate: stored.Chunk.TokenEstimate,
			RawHash: hex.EncodeToString(sum[:8]), PartIndex: stored.Chunk.PartIndex, PartTotal: stored.Chunk.PartTotal,
		})
	}
	return digests
}

func baselineDigestEqual(left, right []baselineChunkDigest) bool {
	encodedLeft, _ := json.Marshal(left)
	encodedRight, _ := json.Marshal(right)
	return string(encodedLeft) == string(encodedRight)
}

// ---------------------------------------------------------------- event recorder

// baselineRunIDEnv names the run an event belongs to. Every record carries it, so two runs
// whose files were somehow merged can still be told apart; the runner refuses to reuse a
// directory, so a merge should not happen in the first place.
const baselineRunIDEnv = "GOPHERLEARN_DOC_B01_RUN_ID"

func baselineRunID() string {
	return strings.TrimSpace(os.Getenv(baselineRunIDEnv))
}

// baselineRecorder writes one JSONL event per observation to
// GOPHERLEARN_DOC_B01_OUT/<scenario>.jsonl when that variable names a directory. When it
// is unset the tests still run and still assert; only the raw event file is skipped.
// Records carry counts, digests and durations. They never carry document text, a vector,
// or a credential.
//
// A scenario file belongs to exactly one run. The first writer creates it exclusively. A
// later writer of the same run may append to it - two commands of one run can select the same
// test, and those records are still one run's evidence - but a file written by a different
// run, or by a run that never named itself, is refused outright. An earlier revision appended
// unconditionally, which meant a second run into the same directory produced one file holding
// both runs' events while the command summary beside it described only the last one.
type baselineRecorder struct {
	mu      sync.Mutex
	path    string
	runID   string
	records []map[string]any
}

func baselineOutDir() string {
	return strings.TrimSpace(os.Getenv("GOPHERLEARN_DOC_B01_OUT"))
}

func newBaselineRecorder(t *testing.T, scenario string) *baselineRecorder {
	t.Helper()
	recorder := &baselineRecorder{runID: baselineRunID()}
	if dir := baselineOutDir(); dir != "" {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		recorder.path = filepath.Join(dir, scenario+".jsonl")
	}
	return recorder
}

func (r *baselineRecorder) event(kind string, fields map[string]any) {
	if r == nil {
		return
	}
	record := map[string]any{
		"event": kind, "at": time.Now().UTC().Format(time.RFC3339Nano),
	}
	if r.runID != "" {
		record["run_id"] = r.runID
	}
	for key, value := range fields {
		record[key] = value
	}
	r.mu.Lock()
	r.records = append(r.records, record)
	r.mu.Unlock()
}

// flush writes this scenario's events to its own file.
//
// A file that already exists is opened only if every record in it carries this run's id. That
// keeps two facts true at once: one run's evidence may be assembled from more than one command
// (two commands of a round can select the same test), and no run may ever append its records to
// a different run's file. When this run never named itself, an existing file is refused rather
// than merged, because there would be nothing to check it against.
func (r *baselineRecorder) flush(t *testing.T) {
	t.Helper()
	if r == nil || r.path == "" {
		return
	}
	if err := baselineCheckEventFileOwnership(r.path, r.runID); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(r.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	encoder := json.NewEncoder(file)
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, record := range r.records {
		if err := encoder.Encode(record); err != nil {
			t.Fatal(err)
		}
	}
}

// baselineCheckEventFileOwnership decides whether a run may write to an event file. A missing
// or empty file belongs to nobody, so the first writer takes it. A file that holds records
// belongs to the run named inside it, and only that run may add to it.
//
// The rule is stated as a function rather than inlined so it can be exercised directly: a
// separation rule that is never asked to refuse anything is a comment, not a check.
func baselineCheckEventFileOwnership(path, runID string) error {
	content, err := os.ReadFile(path)
	if os.IsNotExist(err) || len(content) == 0 {
		return nil
	}
	if err != nil {
		return err
	}
	if runID == "" {
		return fmt.Errorf("%s already holds records and this run has no %s, so the two cannot be told apart", path, baselineRunIDEnv)
	}
	for index, line := range strings.Split(string(content), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var existing map[string]any
		if err := json.Unmarshal([]byte(line), &existing); err != nil {
			return fmt.Errorf("%s line %d: %w", path, index+1, err)
		}
		if existing["run_id"] != runID {
			return fmt.Errorf("%s already holds records from run %v, not %s: a run must never append to another run's events", path, existing["run_id"], runID)
		}
	}
	return nil
}

// ---------------------------------------------------------------- dataset access

// baselineDatasetRoot is the frozen dataset root. The real notes are read-only inputs, are
// not committed, and their bodies never appear in a record. Without this variable the
// real-note cases skip explicitly; the synthetic cases below always run.
const baselineDatasetEnv = "GOPHERLEARN_DOC_DATASET_ROOT"

func baselineDatasetRoot(t *testing.T) string {
	t.Helper()
	root := strings.TrimSpace(os.Getenv(baselineDatasetEnv))
	if root == "" {
		t.Skipf("%s is not set: the frozen notes are private inputs and are not committed, so the real-note case is skipped rather than guessed at", baselineDatasetEnv)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("%s=%q: %v", baselineDatasetEnv, root, err)
	}
	return root
}

// baselineDatasetManifest is the shared manifest the frozen dataset is pinned to. Only the
// fields this round needs are read; the manifest itself stays the authority.
type baselineDatasetManifest struct {
	DatasetID string `json:"dataset_id"`
	Entries   []struct {
		Path   string `json:"path"`
		Split  string `json:"split"`
		Kind   string `json:"kind"`
		Sha256 string `json:"sha256"`
		Bytes  int64  `json:"bytes"`
		Lines  int    `json:"lines"`
		CRLF   int    `json:"crlf"`
	} `json:"entries"`
}

// baselineLoadManifest reads the manifest and verifies every entry against the bytes on
// disk. The digest is over the raw bytes, so it is unaffected by the read boundary used
// for delivery text checks elsewhere.
func baselineLoadManifest(t *testing.T, manifestPath, root string) (baselineDatasetManifest, []map[string]any) {
	t.Helper()
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("dataset manifest: %v", err)
	}
	var manifest baselineDatasetManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("dataset manifest is not valid JSON: %v", err)
	}
	if len(manifest.Entries) == 0 {
		t.Fatal("dataset manifest declares no entries")
	}
	report := make([]map[string]any, 0, len(manifest.Entries))
	for _, entry := range manifest.Entries {
		path := filepath.Join(root, filepath.FromSlash(entry.Path))
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("dataset entry %s: %v", entry.Path, err)
		}
		sum := sha256.Sum256(content)
		actual := hex.EncodeToString(sum[:])
		lines := len(strings.Split(strings.ReplaceAll(strings.ReplaceAll(string(content), "\r\n", "\n"), "\r", "\n"), "\n"))
		if strings.HasSuffix(string(content), "\n") {
			lines--
		}
		record := map[string]any{
			"path": entry.Path, "split": entry.Split, "kind": entry.Kind,
			"sha256": actual, "bytes": len(content), "lines": lines,
			"crlf":           strings.Count(string(content), "\r\n"),
			"sha256_matches": actual == entry.Sha256, "bytes_match": int64(len(content)) == entry.Bytes,
			"lines_match": lines == entry.Lines,
		}
		report = append(report, record)
		if actual != entry.Sha256 {
			t.Errorf("dataset entry %s drifted: sha256 %s, manifest %s", entry.Path, actual, entry.Sha256)
		}
		if int64(len(content)) != entry.Bytes {
			t.Errorf("dataset entry %s drifted: %d bytes, manifest %d", entry.Path, len(content), entry.Bytes)
		}
	}
	return manifest, report
}

// baselineFirstHeading returns the document's first ATX heading, or its first non-empty
// line when it has none. It is used as the retrieval query so the query is derived from
// the frozen document itself rather than chosen by hand after seeing the results.
func baselineFirstHeading(content string) string {
	for _, line := range strings.Split(strings.ReplaceAll(string(content), "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		trimmed = strings.TrimLeft(trimmed, "#")
		trimmed = strings.TrimSpace(trimmed)
		if trimmed == "" {
			continue
		}
		runes := []rune(trimmed)
		if len(runes) > 120 {
			trimmed = string(runes[:120])
		}
		return trimmed
	}
	return ""
}

// baselineSourceDigest is the localisation summary of one retrievable chunk, used to state
// that a specific part of a specific version became visible without quoting it. The keys
// are the ones rag.Source marshals under, so a renamed field cannot leave this recording
// nulls while the assertions above still pass.
func baselineSourceDigest(source map[string]any) map[string]any {
	return map[string]any{
		"doc_id": source["doc_id"], "doc_version": source["doc_version"],
		"file_name": source["file_name"], "chunk_index": source["chunk_index"],
		"title_path": source["title_path"], "start_line": source["start_line"], "end_line": source["end_line"],
	}
}

// baselineRedactedTitlePath replaces a real note's heading path with a digest of it. The
// frozen notes are private: their headings are still their text, so a record may state that
// a chunk sits under some heading, and how many headings deep, without reproducing them.
func baselineRedactedTitlePath(path []string) map[string]any {
	joined := strings.Join(path, "\x1f")
	sum := sha256.Sum256([]byte(joined))
	return map[string]any{
		"depth": len(path), "sha256_16": hex.EncodeToString(sum[:8]),
	}
}

// baselineRedactedSourceDigest is baselineSourceDigest for a private document: identity,
// version and location are kept, the heading text is replaced by its digest.
func baselineRedactedSourceDigest(source map[string]any) map[string]any {
	redacted := baselineSourceDigest(source)
	if path, ok := source["title_path"].([]any); ok {
		parts := make([]string, 0, len(path))
		for _, item := range path {
			parts = append(parts, fmt.Sprint(item))
		}
		redacted["title_path"] = baselineRedactedTitlePath(parts)
	}
	return redacted
}

// baselineRedactedChunkDigests is baselineDigestChunks for a private document.
func baselineRedactedChunkDigests(chunks []baselineStoredChunk) []map[string]any {
	digests := baselineDigestChunks(chunks)
	redacted := make([]map[string]any, 0, len(digests))
	for _, digest := range digests {
		redacted = append(redacted, map[string]any{
			"index": digest.Index, "type": digest.Type,
			"start_line": digest.StartLine, "end_line": digest.EndLine,
			"token_estimate": digest.TokenEstimate, "raw_sha256_16": digest.RawHash,
			"part_index": digest.PartIndex, "part_total": digest.PartTotal,
			"title_path": baselineRedactedTitlePath(digest.TitlePath),
		})
	}
	return redacted
}
