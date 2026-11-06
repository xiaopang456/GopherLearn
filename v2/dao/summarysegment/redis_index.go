package summarysegment

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"GopherAI/common/rag"
	redisCommon "GopherAI/common/redis"
	"GopherAI/model"

	redisCli "github.com/redis/go-redis/v9"
)

// The session's summary segments, as the server sees them.
//
// The index is a derived view: MySQL holds the segment, and this holds the
// vector that finds it. Every write here can be lost - Redis is a cache - and
// every loss is repaired from MySQL, so a reader never has to decide whether
// the index is complete.
//
// The recent-use record lives here too. It is the answer to "which segments
// did the last few turns reach for", and it is what keeps a segment the
// learner has been asking about in front of them even after the newest
// segments have moved past it.

const (
	// CurrentSummarySegmentEmbeddingVersion is the embedding the index holds.
	// It is part of every filter: a row written by an older embedding is not
	// searched against a newer query.
	CurrentSummarySegmentEmbeddingVersion = 1
	// recentUseLimit is how many segments the recent-use record keeps.
	//
	// It is larger than the resident set because the record is a memory of
	// what was asked for, and the resident set is a budget: a segment that was
	// reached for twice is worth keeping even when the newest ones have moved
	// on.
	recentUseLimit = 16
)

// The recent-use record's key prefix.
const summarySegmentRecentUsePrefix = "gopherai:summary_segment:recent:"

// SummarySegmentIndexConfig carries the namespace an index instance binds.
type SummarySegmentIndexConfig struct {
	IndexName string
	KeyPrefix string
}

// SummarySegmentVectorHit is one segment the vector channel found.
type SummarySegmentVectorHit struct {
	// Seq is the segment's sequence number inside its session.
	Seq int
	// Score is the channel's own similarity score.
	Score float64
	// Distance is the raw cosine distance the index reported.
	Distance float64
}

// RedisSummarySegmentIndex is the production RediSearch index of the session's
// summary segments.
type RedisSummarySegmentIndex struct {
	client    *redisCli.Client
	dimension int
	indexName string
	keyPrefix string
}

// NewRedisSummarySegmentIndex builds the index over the production namespace.
func NewRedisSummarySegmentIndex(client *redisCli.Client, dimension int) (*RedisSummarySegmentIndex, error) {
	if client == nil || dimension != redisCommon.SummarySegmentVectorDimension {
		return nil, fmt.Errorf("invalid redis summary segment index configuration")
	}
	return &RedisSummarySegmentIndex{client: client, dimension: dimension,
		indexName: redisCommon.SummarySegmentIndexName,
		keyPrefix: redisCommon.SummarySegmentKeyPrefix}, nil
}

// NewRedisSummarySegmentIndexWithConfig binds an explicit namespace. It is the
// isolated namespace an evaluation harness uses, not a different instrument.
func NewRedisSummarySegmentIndexWithConfig(client *redisCli.Client, dimension int,
	config SummarySegmentIndexConfig) (*RedisSummarySegmentIndex, error) {
	if client == nil || dimension != redisCommon.SummarySegmentVectorDimension {
		return nil, fmt.Errorf("invalid redis summary segment index configuration")
	}
	if strings.TrimSpace(config.IndexName) == "" || strings.TrimSpace(config.KeyPrefix) == "" {
		return nil, fmt.Errorf("an index namespace must name the index and the key prefix")
	}
	return &RedisSummarySegmentIndex{client: client, dimension: dimension,
		indexName: config.IndexName, keyPrefix: config.KeyPrefix}, nil
}

// EnsureIndex creates the index, or verifies an existing one.
func (r *RedisSummarySegmentIndex) EnsureIndex(ctx context.Context) error {
	return redisCommon.EnsureSummarySegmentIndexWithNamespace(ctx, r.client, r.dimension,
		r.indexName, r.keyPrefix)
}

// Store writes one segment's row.
//
// The row carries the segment's own words - the topic, the points and the
// names and numbers it recorded - so that a query that matches on the words
// finds it without reading MySQL first.
func (r *RedisSummarySegmentIndex) Store(ctx context.Context, segment model.SessionSummarySegment,
	vector []float32) error {
	if segment.UserID <= 0 || strings.TrimSpace(segment.SessionID) == "" || segment.Seq <= 0 {
		return fmt.Errorf("invalid indexed summary segment")
	}
	if len(vector) != r.dimension {
		return fmt.Errorf("invalid summary segment vector")
	}
	fields := map[string]interface{}{
		redisCommon.SummarySegmentFieldSegmentID:        r.segmentID(segment),
		redisCommon.SummarySegmentFieldUserID:           strconv.FormatInt(segment.UserID, 10),
		redisCommon.SummarySegmentFieldSessionID:        segment.SessionID,
		redisCommon.SummarySegmentFieldSeq:              segment.Seq,
		redisCommon.SummarySegmentFieldEndMessageID:     segment.EndMessageID,
		redisCommon.SummarySegmentFieldEndAt:            segment.EndAt.Unix(),
		redisCommon.SummarySegmentFieldTopic:            segment.Topic,
		redisCommon.SummarySegmentFieldNamesAndNumbers:  segment.NamesAndNumbers,
		redisCommon.SummarySegmentFieldSearchText:       segment.Content,
		redisCommon.SummarySegmentFieldSearchBigrams:    rag.HanBigramTagValue(segment.Topic + " " + segment.Content),
		redisCommon.SummarySegmentFieldEmbeddingVersion: CurrentSummarySegmentEmbeddingVersion,
		redisCommon.SummarySegmentFieldVector:           redisCommon.Float32VectorBytes(vector),
	}
	key := r.keyPrefix + strconv.FormatInt(segment.UserID, 10) + ":" + segment.SessionID + ":" +
		strconv.Itoa(segment.Seq)
	if err := r.client.HSet(ctx, key, fields).Err(); err != nil {
		return fmt.Errorf("store summary segment index: %w", err)
	}
	return nil
}

// Delete removes one session's segments from the index.
func (r *RedisSummarySegmentIndex) Delete(ctx context.Context, userID int64, sessionID string,
	seqs []int) error {
	keys := make([]string, 0, len(seqs))
	for _, seq := range seqs {
		if seq > 0 {
			keys = append(keys, r.keyPrefix+strconv.FormatInt(userID, 10)+":"+sessionID+":"+strconv.Itoa(seq))
		}
	}
	if len(keys) == 0 {
		return nil
	}
	if err := r.client.Del(ctx, keys...).Err(); err != nil {
		return fmt.Errorf("delete summary segment index: %w", err)
	}
	return nil
}

// CountUser counts one user's indexed segments.
func (r *RedisSummarySegmentIndex) CountUser(ctx context.Context, userID int64) (int, error) {
	var cursor uint64
	total := 0
	for {
		keys, next, err := r.client.Scan(ctx, cursor,
			r.keyPrefix+strconv.FormatInt(userID, 10)+":*", 500).Result()
		if err != nil {
			return 0, err
		}
		total += len(keys)
		cursor = next
		if cursor == 0 {
			break
		}
	}
	return total, nil
}

// CountAll counts every indexed segment.
//
// It is the cheap half of the repair's check: the whole index against the
// whole table. The two agree in the normal case, and the per-user walk only
// runs when they do not.
func (r *RedisSummarySegmentIndex) CountAll(ctx context.Context) (int, error) {
	var cursor uint64
	total := 0
	for {
		keys, next, err := r.client.Scan(ctx, cursor, r.keyPrefix+"*", 500).Result()
		if err != nil {
			return 0, err
		}
		total += len(keys)
		cursor = next
		if cursor == 0 {
			break
		}
	}
	return total, nil
}

// ResetUser removes one user's complete derived view.
func (r *RedisSummarySegmentIndex) ResetUser(ctx context.Context, userID int64) error {
	var cursor uint64
	allKeys := make([]string, 0, 512)
	for {
		keys, next, err := r.client.Scan(ctx, cursor,
			r.keyPrefix+strconv.FormatInt(userID, 10)+":*", 500).Result()
		if err != nil {
			return fmt.Errorf("scan summary segment index: %w", err)
		}
		allKeys = append(allKeys, keys...)
		cursor = next
		if cursor == 0 {
			break
		}
	}
	if len(allKeys) == 0 {
		return nil
	}
	if err := r.client.Del(ctx, allKeys...).Err(); err != nil {
		return fmt.Errorf("reset summary segment index: %w", err)
	}
	return nil
}

// SearchByVector returns the session's segments closest to the query vector.
//
// The filter is the session's own: a segment belongs to one conversation, and
// a question about this session must not be answered with another session's
// stretch of turns.
func (r *RedisSummarySegmentIndex) SearchByVector(ctx context.Context, userID int64, sessionID string,
	vector []float32, topK int) ([]SummarySegmentVectorHit, error) {
	if len(vector) != r.dimension || topK <= 0 {
		return nil, fmt.Errorf("invalid summary segment vector query")
	}
	filter := fmt.Sprintf("@%s:{%d} @%s:{%s} @%s:[%d %d]",
		redisCommon.SummarySegmentFieldUserID, userID,
		redisCommon.SummarySegmentFieldSessionID, redisCommon.EscapeTagValue(sessionID),
		redisCommon.SummarySegmentFieldEmbeddingVersion,
		CurrentSummarySegmentEmbeddingVersion, CurrentSummarySegmentEmbeddingVersion)
	query := fmt.Sprintf("(%s)=>[KNN %d @%s $query_vector AS distance]", filter, topK,
		redisCommon.SummarySegmentFieldVector)
	result, err := r.client.FTSearchWithArgs(ctx, r.indexName, query, &redisCli.FTSearchOptions{
		Return: []redisCli.FTSearchReturn{
			{FieldName: redisCommon.SummarySegmentFieldSeq},
			{FieldName: redisCommon.SummarySegmentFieldEmbeddingVersion},
			{FieldName: "distance"},
		},
		SortBy:         []redisCli.FTSearchSortBy{{FieldName: "distance", Asc: true}},
		Limit:          topK,
		DialectVersion: 2,
		Params:         map[string]interface{}{"query_vector": redisCommon.Float32VectorBytes(vector)},
	}).Result()
	if err != nil {
		return nil, fmt.Errorf("search summary segment vector index: %w", err)
	}
	hits := make([]SummarySegmentVectorHit, 0, len(result.Docs))
	for _, doc := range result.Docs {
		seq, convErr := strconv.Atoi(doc.Fields[redisCommon.SummarySegmentFieldSeq])
		if convErr != nil || seq <= 0 {
			continue
		}
		hit := SummarySegmentVectorHit{Seq: seq}
		hit.Distance, _ = strconv.ParseFloat(doc.Fields["distance"], 64)
		hit.Score = 1 - hit.Distance
		hits = append(hits, hit)
	}
	return hits, nil
}

// RecordRecentUse remembers that a segment was reached for.
//
// The record is a sorted set of the sequence numbers, scored by when they
// were used. It is a memory of what the learner asked about, and it is what
// keeps a segment they keep asking about in front of them.
func (r *RedisSummarySegmentIndex) RecordRecentUse(ctx context.Context, userID int64, sessionID string,
	seq int) error {
	if userID <= 0 || strings.TrimSpace(sessionID) == "" || seq <= 0 {
		return fmt.Errorf("invalid summary segment recent use")
	}
	key := r.recentUseKey(userID, sessionID)
	member := strconv.Itoa(seq)
	pipe := r.client.TxPipeline()
	pipe.ZAdd(ctx, key, redisCli.Z{Score: float64(time.Now().UnixNano()), Member: member})
	pipe.ZRemRangeByRank(ctx, key, 0, -(recentUseLimit + 1))
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("record summary segment recent use: %w", err)
	}
	return nil
}

// ListRecentUse returns the segments that were reached for, newest first.
func (r *RedisSummarySegmentIndex) ListRecentUse(ctx context.Context, userID int64, sessionID string,
	limit int) ([]int, error) {
	if limit <= 0 {
		limit = recentUseLimit
	}
	members, err := r.client.ZRevRange(ctx, r.recentUseKey(userID, sessionID), 0, int64(limit-1)).Result()
	if err != nil {
		return nil, fmt.Errorf("read summary segment recent use: %w", err)
	}
	out := make([]int, 0, len(members))
	for _, member := range members {
		seq, convErr := strconv.Atoi(member)
		if convErr != nil || seq <= 0 {
			continue
		}
		out = append(out, seq)
	}
	return out, nil
}

// recentUseKey is the key one session's recent-use record lives under.
func (r *RedisSummarySegmentIndex) recentUseKey(userID int64, sessionID string) string {
	return summarySegmentRecentUsePrefix + strconv.FormatInt(userID, 10) + ":" + sessionID
}

// segmentID is the row's own id: the session and the sequence number.
func (r *RedisSummarySegmentIndex) segmentID(segment model.SessionSummarySegment) string {
	return segment.SessionID + ":" + strconv.Itoa(segment.Seq)
}
