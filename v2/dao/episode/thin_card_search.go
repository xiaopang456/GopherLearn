package episode

import (
	"GopherAI/model"
	"context"
	"fmt"
	"strconv"
	"strings"

	"GopherAI/common/rag"
	redisCommon "GopherAI/common/redis"
	redisCli "github.com/redis/go-redis/v9"
)

// The thin card's index.
//
// It is the episode index's shape without the vector, and the missing vector
// is the point: a vector needs an embedding, and an embedding is a model
// call. The thin card's claim is that keeping a card current costs none, so
// the card is found by its words.
//
// Everything else is the episode index's own: the same field names, the same
// filter, the same parsing. The two instruments read alike on purpose.

// RedisThinCardIndex is the thin cards' text index.
//
// It is a separate type and not a flag on the episode index, because the two
// indexes differ in a way a flag cannot express: one has a vector field and
// the other does not, and a query that names a vector field an index does not
// have fails rather than returning nothing.
type RedisThinCardIndex struct {
	client    *redisCli.Client
	indexName string
	keyPrefix string
}

// NewRedisThinCardIndex builds the index.
func NewRedisThinCardIndex(client *redisCli.Client, indexName, keyPrefix string) (*RedisThinCardIndex, error) {
	if client == nil {
		return nil, fmt.Errorf("thin card index needs a redis client")
	}
	if strings.TrimSpace(indexName) == "" || strings.TrimSpace(keyPrefix) == "" {
		return nil, fmt.Errorf("a thin card index namespace must name the index and the key prefix")
	}
	return &RedisThinCardIndex{client: client, indexName: indexName, keyPrefix: keyPrefix}, nil
}

// EnsureIndex creates the index, or verifies an existing one.
func (r *RedisThinCardIndex) EnsureIndex(ctx context.Context) error {
	return redisCommon.EnsureVectorIndex(ctx, r.client,
		redisCommon.ThinCardIndexSpec(r.indexName, r.keyPrefix))
}

// Store writes one card into the index.
//
// There is no vector field in it, and there is no embedding behind it: the
// card's words ARE the index entry, and keeping it current costs no model
// call.
func (r *RedisThinCardIndex) Store(ctx context.Context, card model.EpisodeMemory) error {
	if card.ID == "" || card.UserID <= 0 {
		return fmt.Errorf("invalid thin card index entry")
	}
	fields := map[string]interface{}{
		redisCommon.EpisodeFieldEpisodeID:     card.ID,
		redisCommon.EpisodeFieldUserID:        strconv.FormatInt(card.UserID, 10),
		redisCommon.EpisodeFieldSessionID:     card.SessionID,
		redisCommon.EpisodeFieldStatus:        string(card.Status),
		redisCommon.EpisodeFieldLastMessageAt: card.LastMessageAt.Unix(),
		redisCommon.EpisodeFieldTitle:         card.Title,
		redisCommon.EpisodeFieldTopicKeys:     strings.Join(card.TopicKeys, ","),
		redisCommon.EpisodeFieldSearchText:    card.SearchText,
		redisCommon.EpisodeFieldSearchBigrams: rag.HanBigramTagValue(card.Title + " " + card.SearchText),
	}
	key := r.keyPrefix + strconv.FormatInt(card.UserID, 10) + ":" + card.ID
	if err := r.client.HSet(ctx, key, fields).Err(); err != nil {
		return fmt.Errorf("store thin card index entry: %w", err)
	}
	return nil
}

// Delete removes cards from the index.
func (r *RedisThinCardIndex) Delete(ctx context.Context, userID int64, cardIDs []string) error {
	keys := make([]string, 0, len(cardIDs))
	for _, id := range cardIDs {
		if id != "" {
			keys = append(keys, r.keyPrefix+strconv.FormatInt(userID, 10)+":"+id)
		}
	}
	if len(keys) == 0 {
		return nil
	}
	return r.client.Del(ctx, keys...).Err()
}

// SearchByText finds cards by the words in them.
//
// The words are matched against the title and the search text, and the
// Chinese bigrams go to the untokenized tag field beside them: the Chinese
// tokenizer cannot match a term its dictionary does not hold, and a card
// whose title is the learner's own words has to be findable by them.
//
// The current session is excluded. A card about the conversation being
// answered is not a door to the past.
func (r *RedisThinCardIndex) SearchByText(ctx context.Context, userID int64, currentSession string,
	terms []string, topK int) ([]ThinCardSearchResult, error) {
	clauses := make([]string, 0, len(terms)+1)
	fields := redisCommon.EpisodeFieldTitle + "|" + redisCommon.EpisodeFieldSearchText
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
		clauses = append(clauses, fmt.Sprintf("@%s:{%s}", redisCommon.EpisodeFieldSearchBigrams, strings.Join(values, "|")))
	}
	if len(clauses) == 0 {
		return []ThinCardSearchResult{}, nil
	}
	query := fmt.Sprintf("(%s) (%s)", r.filter(userID, currentSession), strings.Join(clauses, "|"))
	result, err := r.client.FTSearchWithArgs(ctx, r.indexName, query, &redisCli.FTSearchOptions{
		Return:     []redisCli.FTSearchReturn{{FieldName: redisCommon.EpisodeFieldEpisodeID}, {FieldName: redisCommon.EpisodeFieldLastMessageAt}},
		WithScores: true, Limit: topK, DialectVersion: 2, Language: "chinese", Scorer: "BM25STD",
	}).Result()
	if err != nil {
		return nil, fmt.Errorf("search thin card text index: %w", err)
	}
	return parseThinCardResults(result.Docs), nil
}

// SearchExactTopics finds cards filed under the given topic keys.
func (r *RedisThinCardIndex) SearchExactTopics(ctx context.Context, userID int64, currentSession string,
	topics []string, topK int) ([]ThinCardSearchResult, error) {
	values := make([]string, 0, len(topics))
	for _, topic := range topics {
		if strings.TrimSpace(topic) != "" {
			values = append(values, redisCommon.EscapeTagValue(topic))
		}
	}
	if len(values) == 0 {
		return []ThinCardSearchResult{}, nil
	}
	query := fmt.Sprintf("%s @%s:{%s}", r.filter(userID, currentSession),
		redisCommon.EpisodeFieldTopicKeys, strings.Join(values, "|"))
	result, err := r.client.FTSearchWithArgs(ctx, r.indexName, query, &redisCli.FTSearchOptions{
		Return: []redisCli.FTSearchReturn{{FieldName: redisCommon.EpisodeFieldEpisodeID}, {FieldName: redisCommon.EpisodeFieldLastMessageAt}},
		SortBy: []redisCli.FTSearchSortBy{{FieldName: redisCommon.EpisodeFieldLastMessageAt, Asc: false}},
		Limit:  topK, DialectVersion: 2,
	}).Result()
	if err != nil {
		return nil, fmt.Errorf("search thin card topics: %w", err)
	}
	return parseThinCardResults(result.Docs), nil
}

// filter is the index's own scope: this user's active cards, minus the
// conversation being answered.
func (r *RedisThinCardIndex) filter(userID int64, currentSession string) string {
	filter := fmt.Sprintf("@%s:{%d} @%s:{%s}", redisCommon.EpisodeFieldUserID, userID,
		redisCommon.EpisodeFieldStatus, model.EpisodeStatusActive)
	if currentSession != "" {
		filter += fmt.Sprintf(" -@%s:{%s}", redisCommon.EpisodeFieldSessionID, redisCommon.EscapeTagValue(currentSession))
	}
	return filter
}

// ThinCardSearchResult is one card the index found.
type ThinCardSearchResult struct {
	// CardID is the row's id.
	CardID string
	// LastMessageUnix is when the card's session was last talked in.
	LastMessageUnix int64
	// Score is the text search's score. Zero for a topic lookup.
	Score float64
}

// parseThinCardResults reads the index's answer.
func parseThinCardResults(docs []redisCli.Document) []ThinCardSearchResult {
	out := make([]ThinCardSearchResult, 0, len(docs))
	for _, doc := range docs {
		id := doc.Fields[redisCommon.EpisodeFieldEpisodeID]
		if id == "" {
			continue
		}
		item := ThinCardSearchResult{CardID: id}
		item.LastMessageUnix, _ = strconv.ParseInt(doc.Fields[redisCommon.EpisodeFieldLastMessageAt], 10, 64)
		if doc.Score != nil {
			item.Score = *doc.Score
		}
		out = append(out, item)
	}
	return out
}
