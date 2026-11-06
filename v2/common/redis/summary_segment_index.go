package redis

import (
	"context"
	"strconv"

	redisCli "github.com/redis/go-redis/v9"
)

// The summary segments' index.
//
// A segment is a stretch of one session's turns. It is indexed for the same
// reasons an episode card is - a question has to be able to find the stretch
// of the conversation it is about - and with the same component, because the
// two are the same kind of instrument.
//
// The fields are named for what they hold rather than for the episode they
// were written for: a segment's topic is its title, its points are its search
// text, and the names and numbers it recorded are its tags.

// The summary segments' index namespace.
const (
	SummarySegmentIndexName       = "gopherai_summary_segment_v1_idx"
	SummarySegmentKeyPrefix       = "gopherai:summary_segment:vec:"
	SummarySegmentVectorDimension = KnowledgeChunkVectorDimension
)

// The summary segments' index fields.
const (
	SummarySegmentFieldSegmentID        = "segment_id"
	SummarySegmentFieldUserID           = "user_id"
	SummarySegmentFieldSessionID        = "session_id"
	SummarySegmentFieldSeq              = "seq"
	SummarySegmentFieldEndMessageID     = "end_message_id"
	SummarySegmentFieldEndAt            = "end_at"
	SummarySegmentFieldTopic            = "topic"
	SummarySegmentFieldNamesAndNumbers  = "names_and_numbers"
	SummarySegmentFieldSearchText       = "search_text"
	SummarySegmentFieldSearchBigrams    = "search_bigrams"
	SummarySegmentFieldEmbeddingVersion = "embedding_version"
	SummarySegmentFieldVector           = "vector"
)

// SummarySegmentIndexSpec is the segment instrument's schema.
func SummarySegmentIndexSpec(indexName, keyPrefix string, dimension int) VectorIndexSpec {
	return VectorIndexSpec{
		Label:           "summary segment",
		Name:            indexName,
		KeyPrefix:       keyPrefix,
		Dimension:       dimension,
		DefaultLanguage: "chinese",
		Fields: []VectorIndexField{
			{Name: SummarySegmentFieldSegmentID, Type: "TAG"},
			{Name: SummarySegmentFieldUserID, Type: "TAG"},
			{Name: SummarySegmentFieldSessionID, Type: "TAG"},
			{Name: SummarySegmentFieldSeq, Type: "NUMERIC", Sortable: true},
			{Name: SummarySegmentFieldEndMessageID, Type: "NUMERIC"},
			{Name: SummarySegmentFieldEndAt, Type: "NUMERIC", Sortable: true},
			{Name: SummarySegmentFieldTopic, Type: "TEXT", Weight: 3, NoStem: true},
			{Name: SummarySegmentFieldNamesAndNumbers, Type: "TAG"},
			{Name: SummarySegmentFieldSearchText, Type: "TEXT", NoStem: true},
			// The bigrams are a tag field for the reason the episode index's
			// are: the Chinese tokenizer would split them back into single
			// characters.
			{Name: SummarySegmentFieldSearchBigrams, Type: "TAG", Separator: ","},
			{Name: SummarySegmentFieldEmbeddingVersion, Type: "NUMERIC"},
			{Name: SummarySegmentFieldVector, Type: "VECTOR", Dimension: dimension},
		},
	}
}

// EnsureSummarySegmentIndex creates (or verifies) the segment index.
func EnsureSummarySegmentIndex(ctx context.Context, client *redisCli.Client, dimension int) error {
	return EnsureSummarySegmentIndexWithNamespace(ctx, client, dimension,
		SummarySegmentIndexName, SummarySegmentKeyPrefix)
}

// EnsureSummarySegmentIndexWithNamespace creates (or verifies) one segment
// index bound to an explicit index name and key prefix. It is the isolated
// namespace an evaluation harness binds, not a different instrument.
func EnsureSummarySegmentIndexWithNamespace(ctx context.Context, client *redisCli.Client,
	dimension int, indexName, keyPrefix string) error {
	return EnsureVectorIndex(ctx, client, SummarySegmentIndexSpec(indexName, keyPrefix, dimension))
}

// GenerateSummarySegmentVectorKey is the key one segment's row lives under.
//
// The key carries the user and the session so that one user's rows can be
// found without scanning the whole index, and the sequence number because a
// session's segments are numbered inside it.
func GenerateSummarySegmentVectorKey(userID int64, sessionID string, seq int) string {
	return SummarySegmentKeyPrefix + strconv.FormatInt(userID, 10) + ":" + sessionID + ":" +
		strconv.Itoa(seq)
}

// GenerateSummarySegmentUserPrefix is the prefix of one user's segment rows.
func GenerateSummarySegmentUserPrefix(userID int64) string {
	return SummarySegmentKeyPrefix + strconv.FormatInt(userID, 10) + ":"
}
