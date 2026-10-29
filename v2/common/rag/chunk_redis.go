package rag

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	redisCommon "GopherAI/common/redis"
	redisCli "github.com/redis/go-redis/v9"
)

type RedisChunkRepository struct {
	client    *redisCli.Client
	dimension int
}

func NewRedisChunkRepository(client *redisCli.Client, dimension int) (*RedisChunkRepository, error) {
	if client == nil {
		return nil, fmt.Errorf("redis chunk repository: client must not be nil")
	}
	if dimension != redisCommon.KnowledgeChunkVectorDimension {
		return nil, fmt.Errorf("redis chunk repository: dimension is %d, schema requires %d", dimension, redisCommon.KnowledgeChunkVectorDimension)
	}
	return &RedisChunkRepository{client: client, dimension: dimension}, nil
}

func (r *RedisChunkRepository) EnsureIndex(ctx context.Context) error {
	return redisCommon.EnsureKnowledgeChunkIndex(ctx, r.client, r.dimension)
}

func (r *RedisChunkRepository) Store(ctx context.Context, chunks []EmbeddedChunk) error {
	if len(chunks) == 0 {
		return nil
	}
	if err := validateEmbeddedChunks(chunks, r.dimension); err != nil {
		return err
	}

	keys := make([]string, len(chunks))
	existsPipe := r.client.Pipeline()
	existsCommands := make([]*redisCli.IntCmd, len(chunks))
	for index, embedded := range chunks {
		keys[index] = redisCommon.GenerateKnowledgeChunkKey(embedded.UserID, embedded.Chunk.DocID, embedded.Chunk.DocVersion, embedded.Chunk.ID)
		existsCommands[index] = existsPipe.Exists(ctx, keys[index])
	}
	if _, err := existsPipe.Exec(ctx); err != nil {
		return fmt.Errorf("inspect existing chunk keys: %w", err)
	}
	newKeys := make([]string, 0, len(keys))
	for index, command := range existsCommands {
		if command.Val() == 0 {
			newKeys = append(newKeys, keys[index])
		}
	}

	transaction := r.client.TxPipeline()
	for index, embedded := range chunks {
		transaction.HSet(ctx, keys[index], embeddedChunkFields(embedded))
	}
	if _, err := transaction.Exec(ctx); err != nil {
		cleanupErr := deleteKeys(ctx, r.client, newKeys)
		if cleanupErr != nil {
			return errors.Join(fmt.Errorf("write chunk transaction: %w", err), fmt.Errorf("cleanup new chunk keys: %w", cleanupErr))
		}
		return fmt.Errorf("write chunk transaction: %w", err)
	}

	desired := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		desired[key] = struct{}{}
	}
	prefix := redisCommon.GenerateKnowledgeDocumentChunkPrefix(chunks[0].UserID, chunks[0].Chunk.DocID, chunks[0].Chunk.DocVersion)
	existing, err := scanKeys(ctx, r.client, prefix+"*")
	if err != nil {
		return fmt.Errorf("scan stale document chunks: %w", err)
	}
	stale := make([]string, 0)
	for _, key := range existing {
		if _, keep := desired[key]; !keep {
			stale = append(stale, key)
		}
	}
	if err := deleteKeys(ctx, r.client, stale); err != nil {
		return fmt.Errorf("delete stale document chunks: %w", err)
	}
	return nil
}

func (r *RedisChunkRepository) CountDocumentVersion(ctx context.Context, userID int64, docID string, version int) (int, error) {
	keys, err := scanKeys(ctx, r.client, redisCommon.GenerateKnowledgeDocumentChunkPrefix(userID, docID, version)+"*")
	if err != nil {
		return 0, err
	}
	return len(keys), nil
}

func (r *RedisChunkRepository) DeleteDocumentVersion(ctx context.Context, userID int64, docID string, version int) error {
	keys, err := scanKeys(ctx, r.client, redisCommon.GenerateKnowledgeDocumentChunkPrefix(userID, docID, version)+"*")
	if err != nil {
		return err
	}
	return deleteKeys(ctx, r.client, keys)
}

type RedisChunkSearchResult struct {
	ExecutionID      string `json:"-"`
	RedisKey         string
	ChunkID          string
	DocID            string
	DocVersion       int
	ChunkIndex       int
	ChunkType        string
	SourceFile       string
	TitlePath        string
	StartLine        int
	EndLine          int
	RawContent       string
	EmbeddingContent string
	ContentHash      string
	CodeLanguage     string
	Symbols          string
	TokenEstimate    int
	PartIndex        int
	PartTotal        int
	Distance         float64
	LexicalScore     float64
	FusionScore      float64
	VectorRank       int
	LexicalRank      int
	MatchKind        string
}

// SearchByUserVector is a storage-level proof of TAG isolation. The chat-level
// retrieval service with active-version filtering is implemented in RAG-010.
func (r *RedisChunkRepository) SearchByUserVector(ctx context.Context, userID int64, vector []float32, topK int) ([]RedisChunkSearchResult, error) {
	if userID <= 0 || len(vector) != r.dimension || topK <= 0 {
		return nil, fmt.Errorf("search chunk vectors: invalid user, vector dimension, or topK")
	}
	filter := fmt.Sprintf("@%s:{%d}", redisCommon.KnowledgeFieldUserID, userID)
	return r.searchByFilter(ctx, filter, vector, topK)
}

// SearchActiveByUserVector pushes the MySQL active-version allow-list into
// RediSearch before KNN ranking. Stale chunks can therefore never consume the
// requested TopK, even when physical cleanup is delayed.
func (r *RedisChunkRepository) SearchActiveByUserVector(ctx context.Context, userID int64, vector []float32, topK int, activeVersions map[string]int) ([]RedisChunkSearchResult, error) {
	if userID <= 0 || len(vector) != r.dimension || topK <= 0 {
		return nil, fmt.Errorf("search active chunk vectors: invalid user, vector dimension, or topK")
	}
	if len(activeVersions) == 0 {
		return []RedisChunkSearchResult{}, nil
	}
	filter, err := activeDocumentFilter(userID, activeVersions)
	if err != nil {
		return nil, fmt.Errorf("search active chunk vectors: %w", err)
	}
	return r.searchByFilter(ctx, filter, vector, topK)
}

// SearchActiveByUserText searches only the same active user/document versions
// as vector retrieval. Query terms are extracted and escaped by the service;
// RediSearch applies the index's Chinese analyzer and field weights.
func (r *RedisChunkRepository) SearchActiveByUserText(ctx context.Context, userID int64, query string, topK int, activeVersions map[string]int) ([]RedisChunkSearchResult, error) {
	if userID <= 0 || topK <= 0 {
		return nil, fmt.Errorf("search active chunk text: invalid user or topK")
	}
	if len(activeVersions) == 0 {
		return []RedisChunkSearchResult{}, nil
	}
	if len(ExtractLexicalTerms(query)) == 0 {
		return []RedisChunkSearchResult{}, nil
	}
	filter, err := activeDocumentFilter(userID, activeVersions)
	if err != nil {
		return nil, fmt.Errorf("search active chunk text: %w", err)
	}
	textQuery := chunkTextQuery(filter, query)
	if textQuery == "" {
		return []RedisChunkSearchResult{}, nil
	}
	return r.searchText(ctx, textQuery, topK)
}

// Shared lexical analysis for legacy and execution-scoped indexes.
func chunkTextQuery(filter, query string) string {
	terms := ExtractLexicalTerms(query)
	if len(terms) == 0 {
		return ""
	}
	fields := strings.Join([]string{
		redisCommon.KnowledgeFieldSourceFile,
		redisCommon.KnowledgeFieldTitlePath,
		redisCommon.KnowledgeFieldSymbols,
		redisCommon.KnowledgeFieldRawContent,
	}, "|")
	clauses := make([]string, 0, len(terms)+1)
	for _, term := range terms {
		clauses = append(clauses, fmt.Sprintf("@%s:(%s)", fields, redisCommon.EscapeTextValue(term)))
	}
	// The analyzer cannot produce a term its dictionary lacks, so Han bigrams are
	// matched against the untokenized tag field instead. Both halves ride in one
	// query: tag hits are scored with the text hits, so a chunk matching several
	// bigrams still outranks one matching a single bigram.
	if bigrams := HanBigramQueryTerms(terms); len(bigrams) > 0 {
		values := make([]string, 0, len(bigrams))
		for _, bigram := range bigrams {
			values = append(values, redisCommon.EscapeTagValue(bigram))
		}
		clauses = append(clauses, fmt.Sprintf("@%s:{%s}", redisCommon.KnowledgeFieldSearchBigrams, strings.Join(values, "|")))
	}
	textQuery := fmt.Sprintf("(%s) (%s)", filter, strings.Join(clauses, "|"))
	return textQuery
}

func activeDocumentFilter(userID int64, activeVersions map[string]int) (string, error) {
	documentIDs := make([]string, 0, len(activeVersions))
	for documentID, version := range activeVersions {
		if documentID == "" || version <= 0 {
			return "", fmt.Errorf("invalid active document version")
		}
		documentIDs = append(documentIDs, documentID)
	}
	sort.Strings(documentIDs)
	clauses := make([]string, 0, len(documentIDs))
	for _, documentID := range documentIDs {
		version := activeVersions[documentID]
		clauses = append(clauses, fmt.Sprintf("(@%s:{%s} @%s:[%d %d])",
			redisCommon.KnowledgeFieldDocID, redisCommon.EscapeTagValue(documentID),
			redisCommon.KnowledgeFieldDocVersion, version, version))
	}
	return fmt.Sprintf("@%s:{%d} (%s)", redisCommon.KnowledgeFieldUserID, userID, strings.Join(clauses, "|")), nil
}

func (r *RedisChunkRepository) searchByFilter(ctx context.Context, filter string, vector []float32, topK int) ([]RedisChunkSearchResult, error) {
	query := fmt.Sprintf("(%s)=>[KNN %d @%s $query_vector AS distance]", filter, topK, redisCommon.KnowledgeFieldVector)
	returnFields := chunkSearchReturnFields(true)
	returns := make([]redisCli.FTSearchReturn, len(returnFields))
	for index, field := range returnFields {
		returns[index] = redisCli.FTSearchReturn{FieldName: field}
	}
	result, err := r.client.FTSearchWithArgs(ctx, redisCommon.KnowledgeChunkIndexName, query, &redisCli.FTSearchOptions{
		Return: returns, SortBy: []redisCli.FTSearchSortBy{{FieldName: "distance", Asc: true}},
		Limit: topK, DialectVersion: 2, Params: map[string]interface{}{"query_vector": float32VectorBytes(vector)},
	}).Result()
	if err != nil {
		return nil, fmt.Errorf("search shared chunk index: %w", err)
	}
	return parseRedisChunkSearchResults(result.Docs)
}

func (r *RedisChunkRepository) searchText(ctx context.Context, query string, topK int) ([]RedisChunkSearchResult, error) {
	returnFields := chunkSearchReturnFields(false)
	returns := make([]redisCli.FTSearchReturn, len(returnFields))
	for index, field := range returnFields {
		returns[index] = redisCli.FTSearchReturn{FieldName: field}
	}
	result, err := r.client.FTSearchWithArgs(ctx, redisCommon.KnowledgeChunkIndexName, query, &redisCli.FTSearchOptions{
		Return: returns, WithScores: true, Limit: topK, DialectVersion: 2,
		Language: "chinese", Scorer: "BM25STD",
	}).Result()
	if err != nil {
		return nil, fmt.Errorf("search shared chunk text index: %w", err)
	}
	return parseRedisChunkSearchResults(result.Docs)
}

func chunkSearchReturnFields(includeDistance bool) []string {
	fields := []string{
		redisCommon.KnowledgeFieldChunkID, redisCommon.KnowledgeFieldDocID,
		redisCommon.KnowledgeFieldDocVersion, redisCommon.KnowledgeFieldChunkIndex,
		redisCommon.KnowledgeFieldChunkType,
		redisCommon.KnowledgeFieldSourceFile, redisCommon.KnowledgeFieldTitlePath,
		redisCommon.KnowledgeFieldStartLine, redisCommon.KnowledgeFieldEndLine,
		redisCommon.KnowledgeFieldRawContent, redisCommon.KnowledgeFieldContentHash,
		redisCommon.KnowledgeFieldEmbeddingContent, redisCommon.KnowledgeFieldCodeLanguage,
		redisCommon.KnowledgeFieldSymbols, redisCommon.KnowledgeFieldTokenEstimate,
		redisCommon.KnowledgeFieldPartIndex, redisCommon.KnowledgeFieldPartTotal,
	}
	if includeDistance {
		fields = append(fields, "distance")
	}
	return fields
}

func parseRedisChunkSearchResults(documents []redisCli.Document) ([]RedisChunkSearchResult, error) {
	results := make([]RedisChunkSearchResult, 0, len(documents))
	for _, document := range documents {
		parsed, err := parseRedisChunkSearchResult(document)
		if err != nil {
			return nil, err
		}
		results = append(results, parsed)
	}
	return results, nil
}

func validateEmbeddedChunks(chunks []EmbeddedChunk, dimension int) error {
	first := chunks[0]
	if first.UserID <= 0 || first.Chunk.DocID == "" || first.Chunk.DocVersion <= 0 {
		return fmt.Errorf("store chunks: invalid document identity")
	}
	seenKeys := make(map[string]struct{}, len(chunks))
	for _, embedded := range chunks {
		if embedded.UserID != first.UserID || embedded.Chunk.DocID != first.Chunk.DocID || embedded.Chunk.DocVersion != first.Chunk.DocVersion {
			return fmt.Errorf("store chunks: batch must contain one user document version")
		}
		if embedded.Chunk.ID == "" || len(embedded.Vector) != dimension {
			return fmt.Errorf("store chunks: invalid chunk ID or vector dimension")
		}
		key := redisCommon.GenerateKnowledgeChunkKey(embedded.UserID, embedded.Chunk.DocID, embedded.Chunk.DocVersion, embedded.Chunk.ID)
		if _, exists := seenKeys[key]; exists {
			return fmt.Errorf("store chunks: duplicate key %q", key)
		}
		seenKeys[key] = struct{}{}
	}
	return nil
}

func embeddedChunkFields(embedded EmbeddedChunk) map[string]interface{} {
	chunk := embedded.Chunk
	return map[string]interface{}{
		redisCommon.KnowledgeFieldUserID:           strconv.FormatInt(embedded.UserID, 10),
		redisCommon.KnowledgeFieldDocID:            chunk.DocID,
		redisCommon.KnowledgeFieldDocVersion:       chunk.DocVersion,
		redisCommon.KnowledgeFieldChunkID:          chunk.ID,
		redisCommon.KnowledgeFieldChunkIndex:       chunk.Index,
		redisCommon.KnowledgeFieldChunkType:        string(chunk.Type),
		redisCommon.KnowledgeFieldSourceFile:       chunk.SourceFile,
		redisCommon.KnowledgeFieldTitlePath:        strings.Join(chunk.TitlePath, " > "),
		redisCommon.KnowledgeFieldStartLine:        chunk.StartLine,
		redisCommon.KnowledgeFieldEndLine:          chunk.EndLine,
		redisCommon.KnowledgeFieldCodeLanguage:     chunk.CodeLanguage,
		redisCommon.KnowledgeFieldSymbols:          strings.Join(chunk.Symbols, " "),
		redisCommon.KnowledgeFieldRawContent:       chunk.RawContent,
		redisCommon.KnowledgeFieldSearchBigrams:    HanBigramTagValue(strings.Join(chunk.TitlePath, " ") + " " + chunk.RawContent),
		redisCommon.KnowledgeFieldEmbeddingContent: chunk.EmbeddingContent,
		redisCommon.KnowledgeFieldContentHash:      chunk.ContentHash,
		redisCommon.KnowledgeFieldTokenEstimate:    chunk.TokenEstimate,
		redisCommon.KnowledgeFieldPartIndex:        chunk.PartIndex,
		redisCommon.KnowledgeFieldPartTotal:        chunk.PartTotal,
		redisCommon.KnowledgeFieldVector:           float32VectorBytes(embedded.Vector),
		// Written even when empty so a chunk stored by a caller that does not
		// track embedding identity reads back as "not reusable" rather than as
		// a chunk whose identity is unknown.
		redisCommon.KnowledgeFieldEmbeddingInputHash:   embedded.EmbeddingInputHash,
		redisCommon.KnowledgeFieldEmbeddingFingerprint: embedded.EmbeddingFingerprint,
	}
}

// StoredChunkEmbedding is one previously written chunk that is a candidate for
// vector reuse. Only records that survived identity, dimension and finite-value
// checks are returned; everything else is dropped so a caller cannot
// accidentally treat corrupt data as a cache hit.
type StoredChunkEmbedding struct {
	UserID               int64
	DocID                string
	DocVersion           int
	ChunkID              string
	EmbeddingContent     string
	EmbeddingInputHash   string
	EmbeddingFingerprint string
	Vector               []float32
}

var loadedChunkEmbeddingFields = []string{
	redisCommon.KnowledgeFieldUserID, redisCommon.KnowledgeFieldDocID,
	redisCommon.KnowledgeFieldDocVersion, redisCommon.KnowledgeFieldChunkID,
	redisCommon.KnowledgeFieldEmbeddingContent,
	redisCommon.KnowledgeFieldEmbeddingInputHash, redisCommon.KnowledgeFieldEmbeddingFingerprint,
	redisCommon.KnowledgeFieldVector,
}

// LoadDocumentVersionEmbeddings reads one user's one document version as a
// read-only reuse source. The key prefix already scopes the scan, but every
// record is re-checked against the HASH's own user/doc/version fields, so a
// forged or misplaced key can never leak a vector across tenants or documents.
func (r *RedisChunkRepository) LoadDocumentVersionEmbeddings(ctx context.Context, userID int64, docID string, version int) ([]StoredChunkEmbedding, error) {
	if userID <= 0 || docID == "" || version <= 0 {
		return nil, fmt.Errorf("load document version embeddings: invalid user, document, or version")
	}
	keys, err := scanKeys(ctx, r.client, redisCommon.GenerateKnowledgeDocumentChunkPrefix(userID, docID, version)+"*")
	if err != nil {
		return nil, fmt.Errorf("scan document version chunks: %w", err)
	}
	if len(keys) == 0 {
		return nil, nil
	}
	// Sorting before the pipeline makes the returned order independent of the
	// SCAN cursor, which keeps reuse decisions reproducible run to run.
	sort.Strings(keys)

	loaded := make([]StoredChunkEmbedding, 0, len(keys))
	for start := 0; start < len(keys); start += 200 {
		end := start + 200
		if end > len(keys) {
			end = len(keys)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		pipe := r.client.Pipeline()
		commands := make([]*redisCli.SliceCmd, end-start)
		for offset, key := range keys[start:end] {
			commands[offset] = pipe.HMGet(ctx, key, loadedChunkEmbeddingFields...)
		}
		if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redisCli.Nil) {
			return nil, fmt.Errorf("read document version chunks: %w", err)
		}
		for _, command := range commands {
			values, err := command.Result()
			if err != nil {
				if errors.Is(err, redisCli.Nil) {
					continue
				}
				return nil, fmt.Errorf("read document version chunks: %w", err)
			}
			record, usable := decodeStoredChunkEmbedding(values, userID, docID, version, r.dimension)
			if usable {
				loaded = append(loaded, record)
			}
		}
	}
	return loaded, nil
}

// decodeStoredChunkEmbedding reports usable=false for anything it cannot fully
// trust: a legacy chunk without reuse metadata, a mismatched owner, or a
// damaged vector. That is not a data-corruption error — the caller simply
// recomputes those chunks.
func decodeStoredChunkEmbedding(values []interface{}, userID int64, docID string, version, dimension int) (StoredChunkEmbedding, bool) {
	if len(values) != len(loadedChunkEmbeddingFields) {
		return StoredChunkEmbedding{}, false
	}
	storedUserID, err := strconv.ParseInt(redisStringValue(values[0]), 10, 64)
	if err != nil || storedUserID != userID {
		return StoredChunkEmbedding{}, false
	}
	if redisStringValue(values[1]) != docID {
		return StoredChunkEmbedding{}, false
	}
	storedVersion, err := strconv.Atoi(redisStringValue(values[2]))
	if err != nil || storedVersion != version {
		return StoredChunkEmbedding{}, false
	}
	chunkID := redisStringValue(values[3])
	embeddingContent := redisStringValue(values[4])
	inputHash := redisStringValue(values[5])
	fingerprint := redisStringValue(values[6])
	if chunkID == "" || embeddingContent == "" || inputHash == "" || fingerprint == "" {
		return StoredChunkEmbedding{}, false
	}
	vector, ok := decodeFloat32Vector(redisStringValue(values[7]), dimension)
	if !ok {
		return StoredChunkEmbedding{}, false
	}
	return StoredChunkEmbedding{
		UserID: userID, DocID: docID, DocVersion: version, ChunkID: chunkID,
		EmbeddingContent: embeddingContent, EmbeddingInputHash: inputHash,
		EmbeddingFingerprint: fingerprint, Vector: vector,
	}, true
}

func redisStringValue(value interface{}) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []byte:
		return string(typed)
	default:
		return ""
	}
}

// decodeFloat32Vector mirrors float32VectorBytes. A length that is not exactly
// dimension*4, or any non-finite component, makes the record unusable.
func decodeFloat32Vector(encoded string, dimension int) ([]float32, bool) {
	if dimension <= 0 || len(encoded) != dimension*4 {
		return nil, false
	}
	raw := []byte(encoded)
	vector := make([]float32, dimension)
	for index := 0; index < dimension; index++ {
		value := math.Float32frombits(binary.LittleEndian.Uint32(raw[index*4:]))
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return nil, false
		}
		vector[index] = value
	}
	return vector, true
}

func float32VectorBytes(vector []float32) []byte {
	result := make([]byte, len(vector)*4)
	for index, value := range vector {
		binary.LittleEndian.PutUint32(result[index*4:], math.Float32bits(value))
	}
	return result
}

func scanKeys(ctx context.Context, client *redisCli.Client, pattern string) ([]string, error) {
	keys := make([]string, 0)
	var cursor uint64
	for {
		batch, next, err := client.Scan(ctx, cursor, pattern, 200).Result()
		if err != nil {
			return nil, err
		}
		keys = append(keys, batch...)
		cursor = next
		if cursor == 0 {
			return keys, nil
		}
	}
}

func deleteKeys(ctx context.Context, client *redisCli.Client, keys []string) error {
	for start := 0; start < len(keys); start += 200 {
		end := start + 200
		if end > len(keys) {
			end = len(keys)
		}
		if err := client.Del(ctx, keys[start:end]...).Err(); err != nil {
			return err
		}
	}
	return nil
}

func parseRedisChunkSearchResult(document redisCli.Document) (RedisChunkSearchResult, error) {
	version, err := strconv.Atoi(document.Fields[redisCommon.KnowledgeFieldDocVersion])
	if err != nil {
		return RedisChunkSearchResult{}, fmt.Errorf("parse doc_version for redis result %q: %w", document.ID, err)
	}
	index, err := strconv.Atoi(document.Fields[redisCommon.KnowledgeFieldChunkIndex])
	if err != nil {
		return RedisChunkSearchResult{}, fmt.Errorf("parse chunk_index for redis result %q: %w", document.ID, err)
	}
	distance := 1.0
	if value := document.Fields["distance"]; value != "" {
		distance, err = strconv.ParseFloat(value, 64)
		if err != nil {
			return RedisChunkSearchResult{}, fmt.Errorf("parse distance for redis result %q: %w", document.ID, err)
		}
	}
	lexicalScore := 0.0
	if document.Score != nil {
		lexicalScore = *document.Score
	}
	startLine, err := parseOptionalIntField(document, redisCommon.KnowledgeFieldStartLine)
	if err != nil {
		return RedisChunkSearchResult{}, err
	}
	endLine, err := parseOptionalIntField(document, redisCommon.KnowledgeFieldEndLine)
	if err != nil {
		return RedisChunkSearchResult{}, err
	}
	tokenEstimate, err := parseOptionalIntField(document, redisCommon.KnowledgeFieldTokenEstimate)
	if err != nil {
		return RedisChunkSearchResult{}, err
	}
	partIndex, err := parseOptionalIntField(document, redisCommon.KnowledgeFieldPartIndex)
	if err != nil {
		return RedisChunkSearchResult{}, err
	}
	partTotal, err := parseOptionalIntField(document, redisCommon.KnowledgeFieldPartTotal)
	if err != nil {
		return RedisChunkSearchResult{}, err
	}
	return RedisChunkSearchResult{
		RedisKey: document.ID, ChunkID: document.Fields[redisCommon.KnowledgeFieldChunkID],
		DocID: document.Fields[redisCommon.KnowledgeFieldDocID], DocVersion: version,
		ChunkIndex: index, ChunkType: document.Fields[redisCommon.KnowledgeFieldChunkType],
		SourceFile: document.Fields[redisCommon.KnowledgeFieldSourceFile], TitlePath: document.Fields[redisCommon.KnowledgeFieldTitlePath],
		StartLine: startLine, EndLine: endLine,
		RawContent: document.Fields[redisCommon.KnowledgeFieldRawContent], EmbeddingContent: document.Fields[redisCommon.KnowledgeFieldEmbeddingContent],
		ContentHash: document.Fields[redisCommon.KnowledgeFieldContentHash], CodeLanguage: document.Fields[redisCommon.KnowledgeFieldCodeLanguage],
		Symbols: document.Fields[redisCommon.KnowledgeFieldSymbols], TokenEstimate: tokenEstimate,
		PartIndex: partIndex, PartTotal: partTotal, Distance: distance, LexicalScore: lexicalScore,
	}, nil
}

func parseOptionalIntField(document redisCli.Document, field string) (int, error) {
	value := document.Fields[field]
	if value == "" {
		return 0, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("parse %s for redis result %q: %w", field, document.ID, err)
	}
	return parsed, nil
}
