package episode

import (
	"GopherAI/common/rag"
	redisCommon "GopherAI/common/redis"
	"GopherAI/model"
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	redisCli "github.com/redis/go-redis/v9"
)

const CurrentEpisodeEmbeddingVersion = 1

type IndexedEpisode struct {
	Episode model.EpisodeMemory
	Vector  []float32
}

type EpisodeSearchResult struct {
	EpisodeID        string
	EmbeddingVersion int
	LastMessageUnix  int64
	Score            float64
	Distance         float64
	Rank             int
}

// RedisEpisodeIndex is the production RediSearch episode index. The zero
// namespace uses the production constants; an explicit config (see
// NewRedisEpisodeIndexWithConfig) lets an isolated test or a separately
// authorised build bind its own index name and key prefix without touching
// the production namespace.
type RedisEpisodeIndex struct {
	client    *redisCli.Client
	dimension int
	indexName string
	keyPrefix string
}

// EpisodeIndexConfig carries the namespace an index instance binds.
type EpisodeIndexConfig struct {
	IndexName string
	KeyPrefix string
}

func NewRedisEpisodeIndex(client *redisCli.Client, dimension int) (*RedisEpisodeIndex, error) {
	if client == nil || dimension != redisCommon.EpisodeVectorDimension {
		return nil, fmt.Errorf("invalid redis episode index configuration")
	}
	return &RedisEpisodeIndex{client: client, dimension: dimension,
		indexName: redisCommon.EpisodeIndexName,
		keyPrefix: redisCommon.EpisodeKeyPrefix}, nil
}

// NewRedisEpisodeIndexWithConfig is the configured constructor. It validates
// its namespace explicitly and keeps the production constructor's defaults
// untouched.
func NewRedisEpisodeIndexWithConfig(client *redisCli.Client,
	dimension int, config EpisodeIndexConfig) (*RedisEpisodeIndex, error) {
	if client == nil || dimension != redisCommon.EpisodeVectorDimension {
		return nil, fmt.Errorf("invalid redis episode index configuration")
	}
	if strings.TrimSpace(config.IndexName) == "" || strings.TrimSpace(config.KeyPrefix) == "" {
		return nil, fmt.Errorf("an index namespace must name the index and the key prefix")
	}
	return &RedisEpisodeIndex{client: client, dimension: dimension,
		indexName: config.IndexName, keyPrefix: config.KeyPrefix}, nil
}

func (r *RedisEpisodeIndex) EnsureIndex(ctx context.Context) error {
	return redisCommon.EnsureEpisodeIndexWithNamespace(ctx, r.client, r.dimension,
		r.indexName, r.keyPrefix)
}

func (r *RedisEpisodeIndex) Store(ctx context.Context, item IndexedEpisode) error {
	if item.Episode.ID == "" || item.Episode.UserID <= 0 || len(item.Vector) != r.dimension {
		return fmt.Errorf("invalid indexed episode")
	}
	fields := map[string]interface{}{
		redisCommon.EpisodeFieldEpisodeID:        item.Episode.ID,
		redisCommon.EpisodeFieldUserID:           strconv.FormatInt(item.Episode.UserID, 10),
		redisCommon.EpisodeFieldSessionID:        item.Episode.SessionID,
		redisCommon.EpisodeFieldStatus:           string(item.Episode.Status),
		redisCommon.EpisodeFieldLastMessageAt:    item.Episode.LastMessageAt.Unix(),
		redisCommon.EpisodeFieldTitle:            item.Episode.Title,
		redisCommon.EpisodeFieldTopicKeys:        strings.Join(item.Episode.TopicKeys, ","),
		redisCommon.EpisodeFieldSearchText:       item.Episode.SearchText,
		redisCommon.EpisodeFieldSearchBigrams:    rag.HanBigramTagValue(item.Episode.Title + " " + item.Episode.SearchText),
		redisCommon.EpisodeFieldEmbeddingVersion: CurrentEpisodeEmbeddingVersion,
		redisCommon.EpisodeFieldVector:           redisCommon.Float32VectorBytes(item.Vector),
	}
	if err := r.client.HSet(ctx, r.keyPrefix+strconv.FormatInt(item.Episode.UserID, 10)+":"+item.Episode.ID, fields).Err(); err != nil {
		return fmt.Errorf("store episode index: %w", err)
	}
	return nil
}

func (r *RedisEpisodeIndex) Delete(ctx context.Context, userID int64, episodeIDs []string) error {
	keys := make([]string, 0, len(episodeIDs))
	for _, id := range episodeIDs {
		if id != "" {
			keys = append(keys, r.keyPrefix+strconv.FormatInt(userID, 10)+":"+id)
		}
	}
	if len(keys) == 0 {
		return nil
	}
	return r.client.Del(ctx, keys...).Err()
}

func (r *RedisEpisodeIndex) CountUser(ctx context.Context, userID int64) (int, error) {
	var cursor uint64
	total := 0
	for {
		keys, next, err := r.client.Scan(ctx, cursor, r.keyPrefix+strconv.FormatInt(userID, 10)+":*", 500).Result()
		if err != nil {
			return 0, err
		}
		total += len(keys)
		cursor = next
		if cursor == 0 {
			return total, nil
		}
	}
}

// ResetUser removes the complete derived view, including keys whose MySQL row
// was archived or deleted while Redis was unavailable.
func (r *RedisEpisodeIndex) ResetUser(ctx context.Context, userID int64) error {
	var cursor uint64
	allKeys := make([]string, 0, 512)
	for {
		keys, next, err := r.client.Scan(ctx, cursor, r.keyPrefix+strconv.FormatInt(userID, 10)+":*", 500).Result()
		if err != nil {
			return fmt.Errorf("scan episode index: %w", err)
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
		return fmt.Errorf("reset episode index: %w", err)
	}
	return nil
}

func (r *RedisEpisodeIndex) SearchByVector(ctx context.Context, userID int64, currentSession string, vector []float32, topK int) ([]EpisodeSearchResult, error) {
	if len(vector) != r.dimension || topK <= 0 {
		return nil, fmt.Errorf("invalid episode vector query")
	}
	filter := episodeFilter(userID, currentSession)
	query := fmt.Sprintf("(%s)=>[KNN %d @%s $query_vector AS distance]", filter, topK, redisCommon.EpisodeFieldVector)
	result, err := r.client.FTSearchWithArgs(ctx, r.indexName, query, &redisCli.FTSearchOptions{
		Return: []redisCli.FTSearchReturn{{FieldName: redisCommon.EpisodeFieldEpisodeID}, {FieldName: redisCommon.EpisodeFieldEmbeddingVersion}, {FieldName: redisCommon.EpisodeFieldLastMessageAt}, {FieldName: "distance"}},
		SortBy: []redisCli.FTSearchSortBy{{FieldName: "distance", Asc: true}}, Limit: topK, DialectVersion: 2,
		Params: map[string]interface{}{"query_vector": redisCommon.Float32VectorBytes(vector)},
	}).Result()
	if err != nil {
		return nil, fmt.Errorf("search episode vector index: %w", err)
	}
	return parseEpisodeResults(result.Docs, true, false), nil
}

func (r *RedisEpisodeIndex) SearchByText(ctx context.Context, userID int64, currentSession string, terms []string, topK int) ([]EpisodeSearchResult, error) {
	clauses := make([]string, 0, len(terms)+1)
	fields := redisCommon.EpisodeFieldTitle + "|" + redisCommon.EpisodeFieldSearchText
	for _, term := range terms {
		if strings.TrimSpace(term) != "" {
			clauses = append(clauses, fmt.Sprintf("@%s:(%s)", fields, redisCommon.EscapeTextValue(term)))
		}
	}
	// The Chinese tokenizer cannot match a term its dictionary does not hold, so
	// the Han bigrams go to the untokenized tag field instead. Both halves are
	// one query: tag matches are scored alongside text matches, so a document
	// hitting several bigrams still outranks one hitting a single bigram.
	if bigrams := rag.HanBigramQueryTerms(terms); len(bigrams) > 0 {
		values := make([]string, 0, len(bigrams))
		for _, bigram := range bigrams {
			values = append(values, redisCommon.EscapeTagValue(bigram))
		}
		clauses = append(clauses, fmt.Sprintf("@%s:{%s}", redisCommon.EpisodeFieldSearchBigrams, strings.Join(values, "|")))
	}
	if len(clauses) == 0 {
		return []EpisodeSearchResult{}, nil
	}
	query := fmt.Sprintf("(%s) (%s)", episodeFilter(userID, currentSession), strings.Join(clauses, "|"))
	result, err := r.client.FTSearchWithArgs(ctx, r.indexName, query, &redisCli.FTSearchOptions{
		Return:     []redisCli.FTSearchReturn{{FieldName: redisCommon.EpisodeFieldEpisodeID}, {FieldName: redisCommon.EpisodeFieldEmbeddingVersion}, {FieldName: redisCommon.EpisodeFieldLastMessageAt}},
		WithScores: true, Limit: topK, DialectVersion: 2, Language: "chinese", Scorer: "BM25STD",
	}).Result()
	if err != nil {
		return nil, fmt.Errorf("search episode text index: %w", err)
	}
	return parseEpisodeResults(result.Docs, false, false), nil
}

func (r *RedisEpisodeIndex) SearchExactTopics(ctx context.Context, userID int64, currentSession string, topics []string, topK int) ([]EpisodeSearchResult, error) {
	values := make([]string, 0, len(topics))
	for _, topic := range topics {
		if strings.TrimSpace(topic) != "" {
			values = append(values, redisCommon.EscapeTagValue(topic))
		}
	}
	if len(values) == 0 {
		return []EpisodeSearchResult{}, nil
	}
	query := fmt.Sprintf("%s @%s:{%s}", episodeFilter(userID, currentSession), redisCommon.EpisodeFieldTopicKeys, strings.Join(values, "|"))
	result, err := r.client.FTSearchWithArgs(ctx, r.indexName, query, &redisCli.FTSearchOptions{
		Return: []redisCli.FTSearchReturn{{FieldName: redisCommon.EpisodeFieldEpisodeID}, {FieldName: redisCommon.EpisodeFieldEmbeddingVersion}, {FieldName: redisCommon.EpisodeFieldLastMessageAt}},
		SortBy: []redisCli.FTSearchSortBy{{FieldName: redisCommon.EpisodeFieldLastMessageAt, Asc: false}}, Limit: topK, DialectVersion: 2,
	}).Result()
	if err != nil {
		return nil, fmt.Errorf("search exact episode topics: %w", err)
	}
	return parseEpisodeResults(result.Docs, false, true), nil
}

func episodeFilter(userID int64, currentSession string) string {
	filter := fmt.Sprintf("@%s:{%d} @%s:{%s} @%s:[%d %d]", redisCommon.EpisodeFieldUserID, userID,
		redisCommon.EpisodeFieldStatus, model.EpisodeStatusActive, redisCommon.EpisodeFieldEmbeddingVersion, CurrentEpisodeEmbeddingVersion, CurrentEpisodeEmbeddingVersion)
	if currentSession != "" {
		filter += fmt.Sprintf(" -@%s:{%s}", redisCommon.EpisodeFieldSessionID, redisCommon.EscapeTagValue(currentSession))
	}
	return filter
}

func parseEpisodeResults(docs []redisCli.Document, distance, preserveOrder bool) []EpisodeSearchResult {
	results := make([]EpisodeSearchResult, 0, len(docs))
	for _, doc := range docs {
		id := doc.Fields[redisCommon.EpisodeFieldEpisodeID]
		if id == "" {
			continue
		}
		item := EpisodeSearchResult{EpisodeID: id}
		item.EmbeddingVersion, _ = strconv.Atoi(doc.Fields[redisCommon.EpisodeFieldEmbeddingVersion])
		item.LastMessageUnix, _ = strconv.ParseInt(doc.Fields[redisCommon.EpisodeFieldLastMessageAt], 10, 64)
		if doc.Score != nil {
			item.Score = *doc.Score
		}
		if distance {
			item.Distance, _ = strconv.ParseFloat(doc.Fields["distance"], 64)
		}
		results = append(results, item)
	}
	if !preserveOrder {
		sort.SliceStable(results, func(i, j int) bool {
			if distance && results[i].Distance != results[j].Distance {
				return results[i].Distance < results[j].Distance
			}
			if !distance && results[i].Score != results[j].Score {
				return results[i].Score > results[j].Score
			}
			if results[i].LastMessageUnix != results[j].LastMessageUnix {
				return results[i].LastMessageUnix > results[j].LastMessageUnix
			}
			return results[i].EpisodeID < results[j].EpisodeID
		})
	}
	for i := range results {
		results[i].Rank = i + 1
	}
	return results
}

func (r *Repository) UpdateIndexStatus(ctx context.Context, userID int64, episodeID string, version uint64, status model.EpisodeIndexStatus) error {
	result := r.db.WithContext(ctx).Model(&model.EpisodeMemory{}).Where("id = ? AND user_id = ? AND version = ?", episodeID, userID, version).Update("index_status", status)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrEpisodeConflict
	}
	return nil
}

func (r *Repository) ListActiveForReindex(ctx context.Context, userID int64, afterID string, limit int) ([]model.EpisodeMemory, error) {
	var rows []model.EpisodeMemory
	query := activeEpisodeQuery(r.db.WithContext(ctx)).Where("episode_memories.user_id = ? AND episode_memories.status = ? AND episode_memories.id > ?", userID, model.EpisodeStatusActive, afterID).Order("episode_memories.id ASC").Limit(limit)
	err := query.Find(&rows).Error
	return rows, err
}

func (r *Repository) LoadCandidates(ctx context.Context, userID int64, currentSession string, ids []string) ([]model.EpisodeMemory, error) {
	if len(ids) == 0 {
		return []model.EpisodeMemory{}, nil
	}
	var rows []model.EpisodeMemory
	err := activeEpisodeQuery(r.db.WithContext(ctx)).Where("episode_memories.user_id = ? AND episode_memories.status = ? AND episode_memories.index_status = ? AND episode_memories.session_id <> ? AND episode_memories.id IN ?", userID, model.EpisodeStatusActive, model.EpisodeIndexIndexed, currentSession, ids).Find(&rows).Error
	if err != nil {
		return nil, err
	}
	byID := make(map[string]model.EpisodeMemory, len(rows))
	for _, row := range rows {
		byID[row.ID] = row
	}
	ordered := make([]model.EpisodeMemory, 0, len(rows))
	for _, id := range ids {
		if row, ok := byID[id]; ok {
			ordered = append(ordered, row)
		}
	}
	return r.filterValidProvenance(ctx, userID, ordered)
}

func (r *Repository) ListByTime(ctx context.Context, userID int64, currentSession string, from, to time.Time, limit int) ([]model.EpisodeMemory, error) {
	var rows []model.EpisodeMemory
	query := activeEpisodeQuery(r.db.WithContext(ctx)).Where("episode_memories.user_id = ? AND episode_memories.status = ? AND episode_memories.session_id <> ?", userID, model.EpisodeStatusActive, currentSession)
	if !from.IsZero() {
		query = query.Where("episode_memories.last_message_at >= ?", from)
	}
	if !to.IsZero() {
		query = query.Where("episode_memories.last_message_at < ?", to)
	}
	// A newer derived Episode may be invalid because one of its sources was
	// deleted or updated. Fetch the user's bounded Session population before
	// applying the requested limit so "last session" can still return the next
	// valid Episode instead of failing open or producing a false empty result.
	fetchLimit := model.MaxSessionsPerUser
	if limit > fetchLimit {
		fetchLimit = limit
	}
	if err := query.Order("episode_memories.last_message_at DESC, episode_memories.id ASC").Limit(fetchLimit).Find(&rows).Error; err != nil {
		return nil, err
	}
	rows, err := r.filterValidProvenance(ctx, userID, rows)
	if err != nil {
		return nil, err
	}
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, nil
}

// filterValidProvenance is the final MySQL source boundary after Redis (or a
// time query) has proposed candidates. A derived Episode is eligible only if
// every exact source version still belongs to the same user and remains backed
// by an active Session and an active, indexed Episode row.
func (r *Repository) filterValidProvenance(ctx context.Context, userID int64, rows []model.EpisodeMemory) ([]model.EpisodeMemory, error) {
	if len(rows) == 0 {
		return rows, nil
	}
	ids := make([]string, 0)
	seen := make(map[string]struct{})
	for _, row := range rows {
		for _, ref := range row.SourceEpisodes {
			if _, exists := seen[ref.EpisodeID]; exists {
				continue
			}
			seen[ref.EpisodeID] = struct{}{}
			ids = append(ids, ref.EpisodeID)
		}
	}
	if len(ids) == 0 {
		return rows, nil
	}
	var sources []model.EpisodeMemory
	if err := activeEpisodeQuery(r.db.WithContext(ctx)).
		Where("episode_memories.user_id = ? AND episode_memories.status = ? AND episode_memories.index_status = ? AND episode_memories.id IN ?", userID, model.EpisodeStatusActive, model.EpisodeIndexIndexed, ids).
		Find(&sources).Error; err != nil {
		return nil, err
	}
	versions := make(map[string]uint64, len(sources))
	for _, source := range sources {
		versions[source.ID] = source.Version
	}
	eligible := make([]model.EpisodeMemory, 0, len(rows))
	for _, row := range rows {
		valid := true
		for _, ref := range row.SourceEpisodes {
			if ref.EpisodeID == row.ID || ref.Version == 0 || versions[ref.EpisodeID] != ref.Version {
				valid = false
				break
			}
		}
		if valid {
			eligible = append(eligible, row)
		}
	}
	return eligible, nil
}

func (r *Repository) HasSessionHistory(ctx context.Context, userID int64, sessionID string) (bool, error) {
	var count int64
	err := r.db.WithContext(ctx).Model(&model.Message{}).Joins("JOIN sessions ON sessions.id = messages.session_id AND sessions.deleted_at IS NULL").Where("sessions.user_id = ? AND messages.session_id = ?", userID, sessionID).Limit(1).Count(&count).Error
	return count > 0, err
}
