package redis

import (
	"context"
	"errors"
	"fmt"
	"strings"

	redisCli "github.com/redis/go-redis/v9"
)

const (
	KnowledgeFieldUserID           = "user_id"
	KnowledgeFieldDocID            = "doc_id"
	KnowledgeFieldDocVersion       = "doc_version"
	KnowledgeFieldChunkID          = "chunk_id"
	KnowledgeFieldChunkIndex       = "chunk_index"
	KnowledgeFieldChunkType        = "chunk_type"
	KnowledgeFieldSourceFile       = "source_file"
	KnowledgeFieldTitlePath        = "title_path"
	KnowledgeFieldStartLine        = "start_line"
	KnowledgeFieldEndLine          = "end_line"
	KnowledgeFieldCodeLanguage     = "code_language"
	KnowledgeFieldSymbols          = "symbols"
	KnowledgeFieldRawContent       = "raw_content"
	KnowledgeFieldSearchBigrams    = "search_bigrams"
	KnowledgeFieldEmbeddingContent = "embedding_content"
	KnowledgeFieldContentHash      = "content_hash"
	KnowledgeFieldTokenEstimate    = "token_estimate"
	KnowledgeFieldPartIndex        = "part_index"
	KnowledgeFieldPartTotal        = "part_total"
	KnowledgeFieldVector           = "vector"

	// Vector-reuse metadata. Both fields stay out of the FT schema below: they
	// are read back by exact key, never queried, so adding them does not
	// invalidate the index version or require a rebuild.
	KnowledgeFieldEmbeddingInputHash   = "embedding_input_hash"
	KnowledgeFieldEmbeddingFingerprint = "embedding_fingerprint"
)

// EnsureKnowledgeChunkIndex creates one versioned RediSearch index shared by
// all users. User isolation is enforced by TAG filters at query time.
func EnsureKnowledgeChunkIndex(ctx context.Context, client *redisCli.Client, dimension int) error {
	if client == nil {
		return fmt.Errorf("knowledge chunk index: redis client is nil")
	}
	if dimension != KnowledgeChunkVectorDimension {
		return fmt.Errorf("knowledge chunk index: dimension is %d, schema v1 requires %d", dimension, KnowledgeChunkVectorDimension)
	}

	if _, err := client.FTInfo(ctx, KnowledgeChunkIndexName).Result(); err == nil {
		return nil
	} else if !isUnknownIndexError(err) {
		return fmt.Errorf("inspect knowledge chunk index: %w", err)
	}

	schemas := KnowledgeChunkSchema(dimension)
	options := &redisCli.FTCreateOptions{
		OnHash: true, Prefix: []interface{}{KnowledgeChunkKeyPrefix},
		DefaultLanguage: "chinese",
	}
	if err := client.FTCreate(ctx, KnowledgeChunkIndexName, options, schemas...).Err(); err != nil {
		// Multiple service instances may race to create the same versioned index.
		if isIndexAlreadyExistsError(err) {
			return nil
		}
		return fmt.Errorf("create knowledge chunk index: %w", err)
	}
	return nil
}

func isUnknownIndexError(err error) bool {
	if err == nil || errors.Is(err, redisCli.Nil) {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "unknown index") || strings.Contains(message, "no such index")
}

func isIndexAlreadyExistsError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "index already exists") || strings.Contains(message, "index name is already in use")
}

// KnowledgeChunkSchema shares field semantics with opt-in execution indexes.
func KnowledgeChunkSchema(dimension int) []*redisCli.FieldSchema {
	return []*redisCli.FieldSchema{
		{FieldName: KnowledgeFieldUserID, FieldType: redisCli.SearchFieldTypeTag},
		{FieldName: KnowledgeFieldDocID, FieldType: redisCli.SearchFieldTypeTag},
		{FieldName: KnowledgeFieldDocVersion, FieldType: redisCli.SearchFieldTypeNumeric},
		{FieldName: KnowledgeFieldChunkID, FieldType: redisCli.SearchFieldTypeTag},
		{FieldName: KnowledgeFieldChunkIndex, FieldType: redisCli.SearchFieldTypeNumeric, Sortable: true},
		{FieldName: KnowledgeFieldChunkType, FieldType: redisCli.SearchFieldTypeTag},
		{FieldName: KnowledgeFieldSourceFile, FieldType: redisCli.SearchFieldTypeText, Weight: 1.5, NoStem: true},
		{FieldName: KnowledgeFieldTitlePath, FieldType: redisCli.SearchFieldTypeText, Weight: 3, NoStem: true},
		{FieldName: KnowledgeFieldStartLine, FieldType: redisCli.SearchFieldTypeNumeric},
		{FieldName: KnowledgeFieldEndLine, FieldType: redisCli.SearchFieldTypeNumeric},
		{FieldName: KnowledgeFieldCodeLanguage, FieldType: redisCli.SearchFieldTypeTag},
		{FieldName: KnowledgeFieldSymbols, FieldType: redisCli.SearchFieldTypeText, Weight: 4, NoStem: true},
		{FieldName: KnowledgeFieldRawContent, FieldType: redisCli.SearchFieldTypeText, Weight: 1, NoStem: true},
		// Han bigrams, as a TAG so the Chinese analyzer cannot re-split them.
		// Measured on this index: 粘包 matched nothing while 粘 matched 32
		// chunks, because the analyzer's dictionary holds no such word and
		// stored the two characters separately. 半包, 拆包 and 令牌 fail the
		// same way, which is most of the vocabulary these notes are about.
		{FieldName: KnowledgeFieldSearchBigrams, FieldType: redisCli.SearchFieldTypeTag, Separator: ","},
		{FieldName: KnowledgeFieldEmbeddingContent, FieldType: redisCli.SearchFieldTypeText, NoIndex: true},
		{FieldName: KnowledgeFieldContentHash, FieldType: redisCli.SearchFieldTypeTag},
		{FieldName: KnowledgeFieldTokenEstimate, FieldType: redisCli.SearchFieldTypeNumeric},
		{FieldName: KnowledgeFieldPartIndex, FieldType: redisCli.SearchFieldTypeNumeric},
		{FieldName: KnowledgeFieldPartTotal, FieldType: redisCli.SearchFieldTypeNumeric},
		{
			FieldName: KnowledgeFieldVector,
			FieldType: redisCli.SearchFieldTypeVector,
			VectorArgs: &redisCli.FTVectorArgs{FlatOptions: &redisCli.FTFlatOptions{
				Type: "FLOAT32", Dim: dimension, DistanceMetric: "COSINE",
			}},
		},
	}
}
