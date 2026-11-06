package redis

import (
	"context"
	"fmt"
	"strconv"

	redisCli "github.com/redis/go-redis/v9"
)

// Episode uses its own index because a session projection has different
// lifecycle and ranking fields from both knowledge chunks and semantic memory.
const (
	EpisodeIndexName       = "gopherai_episode_v2_idx"
	EpisodeKeyPrefix       = "gopherai:episode:vec:"
	EpisodeVectorDimension = KnowledgeChunkVectorDimension
)

const (
	EpisodeFieldEpisodeID        = "episode_id"
	EpisodeFieldUserID           = "user_id"
	EpisodeFieldSessionID        = "session_id"
	EpisodeFieldStatus           = "status"
	EpisodeFieldLastMessageAt    = "last_message_at"
	EpisodeFieldTitle            = "title"
	EpisodeFieldTopicKeys        = "topic_keys"
	EpisodeFieldSearchText       = "search_text"
	EpisodeFieldSearchBigrams    = "search_bigrams"
	EpisodeFieldVector           = "vector"
	EpisodeFieldEmbeddingVersion = "embedding_version"
)

func GenerateEpisodeVectorKey(userID int64, episodeID string) string {
	return GenerateEpisodeUserPrefix(userID) + episodeID
}

func GenerateEpisodeUserPrefix(userID int64) string {
	return EpisodeKeyPrefix + strconv.FormatInt(userID, 10) + ":"
}

// EpisodeIndexSpec is the episode instrument's schema.
//
// The schema is the component's: the summary segments index the same kinds of
// fields for the same reasons, and a second copy of the schema would be a
// second thing to drift.
func EpisodeIndexSpec(indexName, keyPrefix string, dimension int) VectorIndexSpec {
	return VectorIndexSpec{
		Label:           "episode",
		Name:            indexName,
		KeyPrefix:       keyPrefix,
		Dimension:       dimension,
		DefaultLanguage: "chinese",
		Fields: []VectorIndexField{
			{Name: EpisodeFieldEpisodeID, Type: "TAG"},
			{Name: EpisodeFieldUserID, Type: "TAG"},
			{Name: EpisodeFieldSessionID, Type: "TAG"},
			{Name: EpisodeFieldStatus, Type: "TAG"},
			{Name: EpisodeFieldLastMessageAt, Type: "NUMERIC", Sortable: true},
			{Name: EpisodeFieldTitle, Type: "TEXT", Weight: 3, NoStem: true},
			{Name: EpisodeFieldTopicKeys, Type: "TAG"},
			{Name: EpisodeFieldSearchText, Type: "TEXT", NoStem: true},
			// TAG, not TEXT: the index tokenizes Chinese with a dictionary that
			// would split these pairs straight back into single characters, which
			// is the failure they exist to work around. A tag value is stored
			// verbatim, and tag matches still contribute to BM25 scoring, so this
			// stays one ranked query rather than a second retrieval channel.
			{Name: EpisodeFieldSearchBigrams, Type: "TAG", Separator: ","},
			{Name: EpisodeFieldEmbeddingVersion, Type: "NUMERIC"},
			{Name: EpisodeFieldVector, Type: "VECTOR", Dimension: dimension},
		},
	}
}

func EnsureEpisodeIndex(ctx context.Context, client *redisCli.Client, dimension int) error {
	return EnsureEpisodeIndexWithNamespace(ctx, client, dimension, EpisodeIndexName, EpisodeKeyPrefix)
}

// EnsureEpisodeIndexWithNamespace creates (or verifies) one episode index
// bound to an explicit index name and key prefix. The schema is identical to
// the production index's: an isolated namespace is a different view of the
// same instrument, not a different instrument. An existing index is verified
// field by field; a drift refuses rather than being tolerated.
func EnsureEpisodeIndexWithNamespace(ctx context.Context, client *redisCli.Client,
	dimension int, indexName, keyPrefix string) error {
	if dimension != EpisodeVectorDimension {
		return fmt.Errorf("episode index: dimension is %d, schema v1 requires %d", dimension, EpisodeVectorDimension)
	}
	return EnsureVectorIndex(ctx, client, EpisodeIndexSpec(indexName, keyPrefix, dimension))
}

// VerifyEpisodeSchema exposes the schema verifier for the drift drills.
func VerifyEpisodeSchema(indexName, keyPrefix string, dimension int,
	info map[string]any) error {
	return verifyVectorIndexSchema(EpisodeIndexSpec(indexName, keyPrefix, dimension), info)
}
