package redis

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode"

	redisCli "github.com/redis/go-redis/v9"
)

// AGENT-020 gives long-term memory the retrieval path the knowledge base has
// had since RAG-010. The schema mirrors the chunk index deliberately: one
// versioned index shared by every user, isolation enforced by a TAG filter at
// query time, and the same COSINE metric, so the two sides fail and are
// operated the same way.
const (
	MemoryIndexName       = "gopherai_memory_v2_idx"
	MemoryKeyPrefix       = "gopherai:memory:vec:"
	MemoryVectorDimension = KnowledgeChunkVectorDimension
)

const (
	MemoryFieldUserID        = "user_id"
	MemoryFieldMemoryID      = "memory_id"
	MemoryFieldMemoryScope   = "memory_scope"
	MemoryFieldTopicKey      = "topic_key"
	MemoryFieldTags          = "tags"
	MemoryFieldStatus        = "status"
	MemoryFieldConfidence    = "confidence"
	MemoryFieldUserConfirmed = "user_confirmed"
	MemoryFieldSubject       = "subject"
	MemoryFieldContent       = "content"
	MemoryFieldContentHash   = "content_hash"
	MemoryFieldVector        = "vector"
)

// GenerateMemoryVectorKey groups keys by user so that a user's whole index can
// be scanned or dropped without touching anyone else's.
func GenerateMemoryVectorKey(userID int64, memoryID string) string {
	return GenerateMemoryUserPrefix(userID) + memoryID
}

func GenerateMemoryUserPrefix(userID int64) string {
	return MemoryKeyPrefix + strconv.FormatInt(userID, 10) + ":"
}

func EnsureMemoryIndex(ctx context.Context, client *redisCli.Client, dimension int) error {
	if client == nil {
		return fmt.Errorf("memory index: redis client is nil")
	}
	if dimension != MemoryVectorDimension {
		return fmt.Errorf("memory index: dimension is %d, schema v1 requires %d", dimension, MemoryVectorDimension)
	}

	if _, err := client.FTInfo(ctx, MemoryIndexName).Result(); err == nil {
		return nil
	} else if !isUnknownIndexError(err) {
		return fmt.Errorf("inspect memory index: %w", err)
	}

	schemas := []*redisCli.FieldSchema{
		{FieldName: MemoryFieldUserID, FieldType: redisCli.SearchFieldTypeTag},
		{FieldName: MemoryFieldMemoryID, FieldType: redisCli.SearchFieldTypeTag},
		{FieldName: MemoryFieldMemoryScope, FieldType: redisCli.SearchFieldTypeTag},
		{FieldName: MemoryFieldTopicKey, FieldType: redisCli.SearchFieldTypeTag},
		{FieldName: MemoryFieldTags, FieldType: redisCli.SearchFieldTypeTag},
		{FieldName: MemoryFieldStatus, FieldType: redisCli.SearchFieldTypeTag},
		{FieldName: MemoryFieldConfidence, FieldType: redisCli.SearchFieldTypeNumeric},
		{FieldName: MemoryFieldUserConfirmed, FieldType: redisCli.SearchFieldTypeNumeric},
		// EXP-002B B1 measured that a hit in the subject is worth far more than a
		// hit somewhere in the body, which is why the two are separate fields
		// with different weights rather than one concatenated blob.
		{FieldName: MemoryFieldSubject, FieldType: redisCli.SearchFieldTypeText, Weight: 3, NoStem: true},
		{FieldName: MemoryFieldContent, FieldType: redisCli.SearchFieldTypeText, Weight: 1, NoStem: true},
		{FieldName: MemoryFieldContentHash, FieldType: redisCli.SearchFieldTypeTag},
		{
			FieldName: MemoryFieldVector,
			FieldType: redisCli.SearchFieldTypeVector,
			VectorArgs: &redisCli.FTVectorArgs{FlatOptions: &redisCli.FTFlatOptions{
				Type: "FLOAT32", Dim: dimension, DistanceMetric: "COSINE",
			}},
		},
	}
	options := &redisCli.FTCreateOptions{
		OnHash: true, Prefix: []interface{}{MemoryKeyPrefix},
		DefaultLanguage: "chinese",
	}
	if err := client.FTCreate(ctx, MemoryIndexName, options, schemas...).Err(); err != nil {
		if isIndexAlreadyExistsError(err) {
			return nil
		}
		return fmt.Errorf("create memory index: %w", err)
	}
	return nil
}

// Float32VectorBytes encodes a vector the way RediSearch expects it as a KNN
// parameter. Exported here rather than duplicated because both the knowledge
// and memory sides need the identical encoding.
func Float32VectorBytes(vector []float32) []byte {
	result := make([]byte, len(vector)*4)
	for index, value := range vector {
		binary.LittleEndian.PutUint32(result[index*4:], math.Float32bits(value))
	}
	return result
}

func EscapeTagValue(value string) string {
	var escaped strings.Builder
	for _, char := range value {
		if unicode.IsLetter(char) || unicode.IsNumber(char) || char == '_' {
			escaped.WriteRune(char)
			continue
		}
		escaped.WriteByte('\\')
		escaped.WriteRune(char)
	}
	return escaped.String()
}

func EscapeTextValue(value string) string {
	const special = `,.<>{}[]"':;!@#$%^&*()-+=~|/?\`
	var escaped strings.Builder
	for _, char := range value {
		if unicode.IsSpace(char) || strings.ContainsRune(special, char) {
			escaped.WriteByte('\\')
		}
		escaped.WriteRune(char)
	}
	return escaped.String()
}
