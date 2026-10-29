package memory

import (
	redisCommon "GopherAI/common/redis"
	"GopherAI/model"
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	redisCli "github.com/redis/go-redis/v9"
)

// RedisMemoryIndex is the retrieval side of long-term memory. MySQL remains the
// source of truth: this index stores only what is needed to rank, and the
// caller reloads full rows by ID afterwards. That split is what keeps a stale
// or lost Redis instance a degradation rather than data loss.
type RedisMemoryIndex struct {
	client    *redisCli.Client
	dimension int
}

func NewRedisMemoryIndex(client *redisCli.Client, dimension int) (*RedisMemoryIndex, error) {
	if client == nil {
		return nil, fmt.Errorf("redis memory index: client must not be nil")
	}
	if dimension != redisCommon.MemoryVectorDimension {
		return nil, fmt.Errorf("redis memory index: dimension is %d, schema requires %d", dimension, redisCommon.MemoryVectorDimension)
	}
	return &RedisMemoryIndex{client: client, dimension: dimension}, nil
}

func (r *RedisMemoryIndex) EnsureIndex(ctx context.Context) error {
	return redisCommon.EnsureMemoryIndex(ctx, r.client, r.dimension)
}

// IndexedMemory pairs a memory with the vector computed from it. The embedding
// is passed in rather than computed here so that the DAO stays free of provider
// calls and can be tested without one.
type IndexedMemory struct {
	Memory model.LongTermMemory
	Vector []float32
}

func (r *RedisMemoryIndex) Store(ctx context.Context, items []IndexedMemory) error {
	if len(items) == 0 {
		return nil
	}
	transaction := r.client.TxPipeline()
	for _, item := range items {
		if err := validateIndexedMemory(item, r.dimension); err != nil {
			return err
		}
		key := redisCommon.GenerateMemoryVectorKey(item.Memory.UserID, item.Memory.ID)
		transaction.HSet(ctx, key, memoryIndexFields(item))
	}
	if _, err := transaction.Exec(ctx); err != nil {
		return fmt.Errorf("write memory index transaction: %w", err)
	}
	return nil
}

// Delete removes a memory from the index. Archived and tombstoned memories are
// deleted rather than re-indexed with a status flag: leaving them in place
// would let a retired memory occupy a KNN slot that a live one needed.
func (r *RedisMemoryIndex) Delete(ctx context.Context, userID int64, memoryIDs []string) error {
	if len(memoryIDs) == 0 {
		return nil
	}
	keys := make([]string, 0, len(memoryIDs))
	for _, id := range memoryIDs {
		if strings.TrimSpace(id) == "" {
			continue
		}
		keys = append(keys, redisCommon.GenerateMemoryVectorKey(userID, id))
	}
	if len(keys) == 0 {
		return nil
	}
	if err := r.client.Del(ctx, keys...).Err(); err != nil {
		return fmt.Errorf("delete memory index keys: %w", err)
	}
	return nil
}

// CountUser reports how many memories a user has indexed, which is what the
// reindex command compares against MySQL. Without this number an incomplete
// backfill is invisible: an unindexed memory produces no error, it simply can
// never be recalled again.
func (r *RedisMemoryIndex) CountUser(ctx context.Context, userID int64) (int, error) {
	var cursor uint64
	total := 0
	pattern := redisCommon.GenerateMemoryUserPrefix(userID) + "*"
	for {
		keys, next, err := r.client.Scan(ctx, cursor, pattern, 500).Result()
		if err != nil {
			return 0, fmt.Errorf("scan memory index keys: %w", err)
		}
		total += len(keys)
		cursor = next
		if cursor == 0 {
			return total, nil
		}
	}
}

// ResetUser removes the complete per-user derived view, including keys whose
// MySQL rows are no longer active and therefore cannot appear in a reindex
// source query.
func (r *RedisMemoryIndex) ResetUser(ctx context.Context, userID int64) error {
	if userID <= 0 {
		return fmt.Errorf("reset memory index: invalid user")
	}
	var cursor uint64
	keys := make([]string, 0, 256)
	for {
		batch, next, err := r.client.Scan(ctx, cursor, redisCommon.GenerateMemoryUserPrefix(userID)+"*", 500).Result()
		if err != nil {
			return fmt.Errorf("scan memory index keys: %w", err)
		}
		keys = append(keys, batch...)
		cursor = next
		if cursor == 0 {
			break
		}
	}
	for start := 0; start < len(keys); start += 200 {
		end := start + 200
		if end > len(keys) {
			end = len(keys)
		}
		if err := r.client.Unlink(ctx, keys[start:end]...).Err(); err != nil {
			return fmt.Errorf("reset memory index keys: %w", err)
		}
	}
	return nil
}

// MemorySearchResult carries only identity and score. Content deliberately does
// not come back from Redis: reading it from MySQL guarantees the caller sees
// the current row, including a correction applied after indexing.
type MemorySearchResult struct {
	MemoryID string
	Distance float64
	Score    float64
	Rank     int
}

// Similarity converts the COSINE distance RediSearch returns into the cosine
// similarity the abstention threshold is expressed in.
func (m MemorySearchResult) Similarity() float64 { return 1 - m.Distance }

func (r *RedisMemoryIndex) SearchByVector(ctx context.Context, userID int64, vector []float32, topK int) ([]MemorySearchResult, error) {
	if userID <= 0 || len(vector) != r.dimension || topK <= 0 {
		return nil, fmt.Errorf("search memory vectors: invalid user, vector dimension, or topK")
	}
	filter := activeMemoryFilter(userID)
	query := fmt.Sprintf("(%s)=>[KNN %d @%s $query_vector AS distance]", filter, topK, redisCommon.MemoryFieldVector)
	result, err := r.client.FTSearchWithArgs(ctx, redisCommon.MemoryIndexName, query, &redisCli.FTSearchOptions{
		Return: []redisCli.FTSearchReturn{
			{FieldName: redisCommon.MemoryFieldMemoryID}, {FieldName: "distance"},
		},
		SortBy:         []redisCli.FTSearchSortBy{{FieldName: "distance", Asc: true}},
		Limit:          topK,
		DialectVersion: 2,
		Params:         map[string]interface{}{"query_vector": redisCommon.Float32VectorBytes(vector)},
	}).Result()
	if err != nil {
		return nil, fmt.Errorf("search memory index: %w", err)
	}
	return parseMemorySearchResults(result.Docs, true), nil
}

func (r *RedisMemoryIndex) SearchByText(ctx context.Context, userID int64, terms []string, topK int) ([]MemorySearchResult, error) {
	if userID <= 0 || topK <= 0 {
		return nil, fmt.Errorf("search memory text: invalid user or topK")
	}
	clauses := make([]string, 0, len(terms))
	fields := redisCommon.MemoryFieldSubject + "|" + redisCommon.MemoryFieldContent
	for _, term := range terms {
		if strings.TrimSpace(term) == "" {
			continue
		}
		clauses = append(clauses, fmt.Sprintf("@%s:(%s)", fields, redisCommon.EscapeTextValue(term)))
	}
	if len(clauses) == 0 {
		return []MemorySearchResult{}, nil
	}
	query := fmt.Sprintf("(%s) (%s)", activeMemoryFilter(userID), strings.Join(clauses, "|"))
	result, err := r.client.FTSearchWithArgs(ctx, redisCommon.MemoryIndexName, query, &redisCli.FTSearchOptions{
		Return:         []redisCli.FTSearchReturn{{FieldName: redisCommon.MemoryFieldMemoryID}},
		WithScores:     true,
		Limit:          topK,
		DialectVersion: 2,
		Language:       "chinese",
		Scorer:         "BM25STD",
	}).Result()
	if err != nil {
		return nil, fmt.Errorf("search memory text index: %w", err)
	}
	return parseMemorySearchResults(result.Docs, false), nil
}

// activeMemoryFilter pushes the status and type constraints into RediSearch so
// a retired or wrong-typed memory cannot consume a TopK slot. The caller still
// re-checks ownership on the MySQL rows; this is the outer of two gates, not
// the only one.
func activeMemoryFilter(userID int64) string {
	return fmt.Sprintf("@%s:{%d} @%s:{%s} @%s:{%s}",
		redisCommon.MemoryFieldUserID, userID,
		redisCommon.MemoryFieldStatus, redisCommon.EscapeTagValue(string(model.LongTermMemoryActive)),
		redisCommon.MemoryFieldMemoryScope, redisCommon.EscapeTagValue(string(model.MemoryScopeTopic)))
}

// parseMemorySearchResults assigns ranks in a total order rather than in the
// order RediSearch happened to return.
//
// Ties are common here — several notes on one topic score identically — and
// RediSearch does not promise a stable order among them. Measured over repeated
// runs that showed up as MRR moving by about a point while hit@5 stayed put:
// the same five memories, in a different order. Breaking ties by id makes the
// rank a function of the data alone, which retrieval evaluation depends on and
// which also keeps a user's answers from reshuffling between identical asks.
func parseMemorySearchResults(documents []redisCli.Document, withDistance bool) []MemorySearchResult {
	results := make([]MemorySearchResult, 0, len(documents))
	for _, document := range documents {
		id := document.Fields[redisCommon.MemoryFieldMemoryID]
		if strings.TrimSpace(id) == "" {
			continue
		}
		entry := MemorySearchResult{MemoryID: id}
		if document.Score != nil {
			entry.Score = *document.Score
		}
		if withDistance {
			if parsed, err := strconv.ParseFloat(document.Fields["distance"], 64); err == nil {
				entry.Distance = parsed
			}
		}
		results = append(results, entry)
	}
	sort.SliceStable(results, func(i, j int) bool {
		if withDistance {
			if results[i].Distance != results[j].Distance {
				return results[i].Distance < results[j].Distance
			}
		} else if results[i].Score != results[j].Score {
			return results[i].Score > results[j].Score
		}
		return results[i].MemoryID < results[j].MemoryID
	})
	for index := range results {
		results[index].Rank = index
	}
	return results
}

func memoryIndexFields(item IndexedMemory) map[string]interface{} {
	memory := item.Memory
	tags := append([]string(nil), memory.Tags...)
	sort.Strings(tags)
	confirmed := 0
	if memory.UserConfirmed {
		confirmed = 1
	}
	return map[string]interface{}{
		redisCommon.MemoryFieldUserID:        strconv.FormatInt(memory.UserID, 10),
		redisCommon.MemoryFieldMemoryID:      memory.ID,
		redisCommon.MemoryFieldMemoryScope:   string(memory.Scope),
		redisCommon.MemoryFieldTopicKey:      memory.TopicKey,
		redisCommon.MemoryFieldTags:          strings.Join(tags, ","),
		redisCommon.MemoryFieldStatus:        string(memory.Status),
		redisCommon.MemoryFieldConfidence:    memory.Confidence,
		redisCommon.MemoryFieldUserConfirmed: confirmed,
		redisCommon.MemoryFieldSubject:       memory.Subject,
		redisCommon.MemoryFieldContent:       memory.Content,
		redisCommon.MemoryFieldContentHash:   memory.ContentHash,
		redisCommon.MemoryFieldVector:        redisCommon.Float32VectorBytes(item.Vector),
	}
}

func validateIndexedMemory(item IndexedMemory, dimension int) error {
	if strings.TrimSpace(item.Memory.ID) == "" || item.Memory.UserID <= 0 {
		return fmt.Errorf("index memory: identity is required")
	}
	if len(item.Vector) != dimension {
		return fmt.Errorf("index memory %s: vector has %d dimensions, schema requires %d", item.Memory.ID, len(item.Vector), dimension)
	}
	return nil
}
