package summarysegment

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"GopherAI/common/rag"
	redisCommon "GopherAI/common/redis"

	redisCli "github.com/redis/go-redis/v9"
)

// The cross-session segment search.
//
// The index's own search is bound to one session: a segment belongs to one
// conversation, and a question about THIS session must not be answered with
// another session's stretch of turns. That rule is right for the browse tool
// and wrong for the tool that looks for a conversation in the first place:
// the whole point of that tool is to find a stretch of an EARLIER
// conversation, so it has to search across them.
//
// The difference is the filter and nothing else. The same index, the same
// fields, the same scoring - only the session filter is replaced by the user
// filter, and the session travels back with every hit because the caller has
// to know WHICH conversation it found before it may open it.

// UserSegmentHit is one segment the cross-session search found.
type UserSegmentHit struct {
	// SessionID is the conversation the segment belongs to.
	//
	// It travels with the hit because the caller's next question is "may I
	// open that conversation", and the answer is a property of the session,
	// not of the segment.
	SessionID string
	// Seq is the segment's sequence number inside its session.
	Seq int
	// Topic is the segment's own title.
	Topic string
	// Content is the segment's stored body.
	Content string
	// EndMessageID is the segment's high-water mark: the turns at or below it
	// are covered by this segment.
	EndMessageID uint
	// EndAt is when the segment was written.
	EndAt time.Time
	// Score is the text search's score.
	Score float64
}

// SearchUserSegmentsByText finds the user's segments across their sessions.
//
// The filter is the user's own: a segment of another account is not a
// candidate at all, and the index says so before anything is read.
//
// The words are matched against the segment's topic and its stored body, and
// the Chinese bigrams go to the untokenized tag field beside them: the Chinese
// tokenizer cannot match a term its dictionary does not hold, and a segment
// whose topic is the learner's own words has to be findable by them.
func (r *RedisSummarySegmentIndex) SearchUserSegmentsByText(ctx context.Context, userID int64,
	terms []string, topK int) ([]UserSegmentHit, error) {
	if r == nil || r.client == nil {
		return nil, fmt.Errorf("summary segment index is not initialized")
	}
	if userID <= 0 || topK <= 0 {
		return nil, fmt.Errorf("invalid summary segment text query")
	}
	clauses := make([]string, 0, len(terms)+1)
	fields := redisCommon.SummarySegmentFieldTopic + "|" + redisCommon.SummarySegmentFieldSearchText
	for _, term := range terms {
		if strings.TrimSpace(term) != "" {
			clauses = append(clauses, fmt.Sprintf("@%s:(%s)", fields, redisCommon.EscapeTextValue(term)))
		}
	}
	if bigrams := rag.HanBigramQueryTerms(terms); len(bigrams) > 0 {
		values := make([]string, 0, len(bigrams))
		for _, bigram := range bigrams {
			values = append(values, redisCommon.EscapeTagValue(bigram))
		}
		clauses = append(clauses, fmt.Sprintf("@%s:{%s}", redisCommon.SummarySegmentFieldSearchBigrams,
			strings.Join(values, "|")))
	}
	if len(clauses) == 0 {
		return []UserSegmentHit{}, nil
	}
	query := fmt.Sprintf("(%s) (%s)", r.userFilter(userID), strings.Join(clauses, "|"))
	result, err := r.client.FTSearchWithArgs(ctx, r.indexName, query, &redisCli.FTSearchOptions{
		Return: []redisCli.FTSearchReturn{
			{FieldName: redisCommon.SummarySegmentFieldSessionID},
			{FieldName: redisCommon.SummarySegmentFieldSeq},
			{FieldName: redisCommon.SummarySegmentFieldTopic},
			{FieldName: redisCommon.SummarySegmentFieldSearchText},
			{FieldName: redisCommon.SummarySegmentFieldEndMessageID},
			{FieldName: redisCommon.SummarySegmentFieldEndAt},
		},
		WithScores: true, Limit: topK, DialectVersion: 2, Language: "chinese", Scorer: "BM25STD",
	}).Result()
	if err != nil {
		return nil, fmt.Errorf("search user summary segments: %w", err)
	}
	return parseUserSegmentHits(result.Docs), nil
}

// SearchUserSegmentsByVector finds the user's segments across their sessions
// by meaning rather than by words.
//
// It reuses the segments' own vectors, so the semantic recall does not pay for
// an embedding of the segments: they were embedded once, when they were
// written. The one embedding this path does make is the query's own, and that
// one is a retrieval cost rather than a card cost - the thin card itself is
// still written without asking a model anything.
//
// The embedding version is part of the filter here, unlike the text search:
// a vector is only comparable to a vector of the same version, and a row
// written by an older embedding is not a candidate for a vector query.
func (r *RedisSummarySegmentIndex) SearchUserSegmentsByVector(ctx context.Context, userID int64,
	vector []float32, topK int) ([]UserSegmentHit, error) {
	if r == nil || r.client == nil {
		return nil, fmt.Errorf("summary segment index is not initialized")
	}
	if userID <= 0 || topK <= 0 {
		return nil, fmt.Errorf("invalid summary segment vector query")
	}
	if len(vector) != r.dimension {
		return nil, fmt.Errorf("invalid summary segment vector query")
	}
	filter := fmt.Sprintf("%s @%s:[%d %d]",
		r.userFilter(userID),
		redisCommon.SummarySegmentFieldEmbeddingVersion,
		CurrentSummarySegmentEmbeddingVersion, CurrentSummarySegmentEmbeddingVersion)
	query := fmt.Sprintf("(%s)=>[KNN %d @%s $query_vector AS distance]", filter, topK,
		redisCommon.SummarySegmentFieldVector)
	result, err := r.client.FTSearchWithArgs(ctx, r.indexName, query, &redisCli.FTSearchOptions{
		Return: []redisCli.FTSearchReturn{
			{FieldName: redisCommon.SummarySegmentFieldSessionID},
			{FieldName: redisCommon.SummarySegmentFieldSeq},
			{FieldName: redisCommon.SummarySegmentFieldTopic},
			{FieldName: redisCommon.SummarySegmentFieldSearchText},
			{FieldName: redisCommon.SummarySegmentFieldEndMessageID},
			{FieldName: redisCommon.SummarySegmentFieldEndAt},
			{FieldName: "distance"},
		},
		WithScores: true, SortBy: []redisCli.FTSearchSortBy{{FieldName: "distance", Asc: true}},
		Limit: topK, DialectVersion: 2,
		Params: map[string]interface{}{"query_vector": redisCommon.Float32VectorBytes(vector)},
	}).Result()
	if err != nil {
		return nil, fmt.Errorf("search user summary segment vectors: %w", err)
	}
	hits := parseUserSegmentHits(result.Docs)
	return hits, nil
}

// userFilter is the index's own scope for the cross-session search: this
// user's segments, and nothing else.
//
// The embedding version is not part of it. The version says which embedding
// wrote a row, and a text query does not read the embedding - a row written by
// an older embedding is still perfectly searchable by its words, and hiding it
// would make the tool depend on a reindex that has nothing to do with it.
func (r *RedisSummarySegmentIndex) userFilter(userID int64) string {
	return fmt.Sprintf("@%s:{%d}", redisCommon.SummarySegmentFieldUserID, userID)
}

// parseUserSegmentHits reads the index's answer.
func parseUserSegmentHits(docs []redisCli.Document) []UserSegmentHit {
	out := make([]UserSegmentHit, 0, len(docs))
	for _, doc := range docs {
		sessionID := strings.TrimSpace(doc.Fields[redisCommon.SummarySegmentFieldSessionID])
		seq, convErr := strconv.Atoi(doc.Fields[redisCommon.SummarySegmentFieldSeq])
		if sessionID == "" || convErr != nil || seq <= 0 {
			continue
		}
		hit := UserSegmentHit{
			SessionID: sessionID,
			Seq:       seq,
			Topic:     doc.Fields[redisCommon.SummarySegmentFieldTopic],
			Content:   doc.Fields[redisCommon.SummarySegmentFieldSearchText],
		}
		if end, err := strconv.ParseUint(doc.Fields[redisCommon.SummarySegmentFieldEndMessageID], 10, 64); err == nil {
			hit.EndMessageID = uint(end)
		}
		if endAt, err := strconv.ParseInt(doc.Fields[redisCommon.SummarySegmentFieldEndAt], 10, 64); err == nil {
			hit.EndAt = time.Unix(endAt, 0).UTC()
		}
		if doc.Score != nil {
			hit.Score = *doc.Score
		}
		// The vector search returns a distance rather than a score. It is
		// turned into the same scale the text search reports, so the caller
		// can rank the two channels against each other.
		if distance, convErr := strconv.ParseFloat(doc.Fields["distance"], 64); convErr == nil {
			hit.Score = 1 - distance
		}
		out = append(out, hit)
	}
	return out
}
