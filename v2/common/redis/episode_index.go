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
	if client == nil {
		return fmt.Errorf("episode index: redis client is nil")
	}
	if dimension != EpisodeVectorDimension {
		return fmt.Errorf("episode index: dimension is %d, schema v1 requires %d", dimension, EpisodeVectorDimension)
	}
	schemas := []*redisCli.FieldSchema{
		{FieldName: EpisodeFieldEpisodeID, FieldType: redisCli.SearchFieldTypeTag},
		{FieldName: EpisodeFieldUserID, FieldType: redisCli.SearchFieldTypeTag},
		{FieldName: EpisodeFieldSessionID, FieldType: redisCli.SearchFieldTypeTag},
		{FieldName: EpisodeFieldStatus, FieldType: redisCli.SearchFieldTypeTag},
		{FieldName: EpisodeFieldLastMessageAt, FieldType: redisCli.SearchFieldTypeNumeric, Sortable: true},
		{FieldName: EpisodeFieldTitle, FieldType: redisCli.SearchFieldTypeText, Weight: 3, NoStem: true},
		{FieldName: EpisodeFieldTopicKeys, FieldType: redisCli.SearchFieldTypeTag},
		{FieldName: EpisodeFieldSearchText, FieldType: redisCli.SearchFieldTypeText, Weight: 1, NoStem: true},
		// TAG, not TEXT: the index tokenizes Chinese with a dictionary that
		// would split these pairs straight back into single characters, which
		// is the failure they exist to work around. A tag value is stored
		// verbatim, and tag matches still contribute to BM25 scoring, so this
		// stays one ranked query rather than a second retrieval channel.
		{FieldName: EpisodeFieldSearchBigrams, FieldType: redisCli.SearchFieldTypeTag, Separator: ","},
		{FieldName: EpisodeFieldEmbeddingVersion, FieldType: redisCli.SearchFieldTypeNumeric},
		{FieldName: EpisodeFieldVector, FieldType: redisCli.SearchFieldTypeVector, VectorArgs: &redisCli.FTVectorArgs{FlatOptions: &redisCli.FTFlatOptions{
			Type: "FLOAT32", Dim: dimension, DistanceMetric: "COSINE",
		}}},
	}
	options := &redisCli.FTCreateOptions{OnHash: true, Prefix: []interface{}{keyPrefix}, DefaultLanguage: "chinese"}
	if _, err := client.FTInfo(ctx, indexName).Result(); err == nil {
		// The index exists: verify its schema field by field. Tolerating a
		// drifted schema would let a search run against fields the rows never
		// carried. The verification is complete, not name-only: type,
		// sortable, weight, NOSTEM, TAG separator, VECTOR FLAT/FLOAT32/
		// dimension/distance, the index's own prefix and its default
		// language must all match the schema this function would create.
		raw, rawErr := client.Do(ctx, "FT.INFO", indexName).Result()
		if rawErr != nil {
			return fmt.Errorf("inspect episode index raw: %w", rawErr)
		}
		flat, ok := raw.([]any)
		if !ok || len(flat)%2 != 0 {
			return fmt.Errorf("unreadable FT.INFO for %s", indexName)
		}
		if err := verifyEpisodeSchemaRaw(indexName, keyPrefix, dimension,
			ftInfoMap(flat)); err != nil {
			return err
		}
		return nil
	} else if !isUnknownIndexError(err) {
		return fmt.Errorf("inspect episode index: %w", err)
	}
	if err := client.FTCreate(ctx, indexName, options, schemas...).Err(); err != nil {
		if isIndexAlreadyExistsError(err) {
			return nil
		}
		return fmt.Errorf("create episode index: %w", err)
	}
	return nil
}

// verifyEpisodeSchemaRaw compares an existing index against the schema this
// package would create, using the raw FT.INFO map: the structured client
// result does not carry TAG separators, VECTOR arguments or the default
// language. Any drift refuses: a search run against a drifted schema reads
// fields the rows never carried.
func verifyEpisodeSchemaRaw(indexName, keyPrefix string, dimension int,
	info map[string]any) error {
	attributes, _ := info["attributes"].([]any)
	expected := expectedEpisodeAttributes(keyPrefix, dimension)
	byName := map[string]map[string]any{}
	for _, want := range expected {
		byName[want["attribute"].(string)] = want
	}
	seen := map[string]bool{}
	for _, raw := range attributes {
		// Each attribute arrives as a flat key/value slice; fold it here so
		// nested lists-of-maps do not need a deeper heuristic.
		entries, isSlice := raw.([]any)
		var attribute map[string]any
		if isSlice {
			attribute = ftInfoMap(entries)
		} else {
			attribute, _ = raw.(map[string]any)
		}
		name, _ := attribute["attribute"].(string)
		if name == "" {
			name, _ = attribute["identifier"].(string)
		}
		want, known := byName[name]
		if !known {
			return fmt.Errorf("episode index %s carries unexpected field %s: "+
				"refusing to run against a drifted schema", indexName, name)
		}
		seen[name] = true
		if got, _ := attribute["type"].(string); got != want["type"] {
			return fmt.Errorf("episode index %s field %s is type %q, want %q",
				indexName, name, got, want["type"])
		}
		if wantSortable, _ := want["sortable"].(bool); wantSortable {
			if got, _ := attribute["SORTABLE"].(string); got != "WITHSCORE" && got != "" {
				// FT.INFO reports SORTABLE with a qualifier; any presence is
				// accepted, absence is the drift.
			}
			if _, present := attribute["SORTABLE"]; !present {
				return fmt.Errorf("episode index %s field %s is not sortable",
					indexName, name)
			}
		}
		if wantNoStem, _ := want["nostem"].(bool); wantNoStem {
			if _, present := attribute["NOSTEM"]; !present {
				return fmt.Errorf("episode index %s field %s is not NOSTEM",
					indexName, name)
			}
		}
		if wantWeight, ok := want["weight"].(float64); ok && wantWeight > 1 {
			// WEIGHT arrives as a string or a float depending on the reply
			// mode; compare numerically.
			gotWeight, _ := strconv.ParseFloat(fmt.Sprintf("%v",
				attribute["WEIGHT"]), 64)
			if attribute["WEIGHT"] != nil && gotWeight != wantWeight {
				return fmt.Errorf("episode index %s field %s weight = %v, want %v",
					indexName, name, attribute["WEIGHT"], wantWeight)
			}
		}
		if wantSeparator, _ := want["separator"].(string); wantSeparator != "" {
			got, _ := attribute["SEPARATOR"].(string)
			if got != wantSeparator {
				return fmt.Errorf("episode index %s field %s separator = %q, want %q",
					indexName, name, got, wantSeparator)
			}
		}
		if vectorArgs, ok := want["vector"].([]string); ok {
			// FT.INFO reports the vector field as algorithm/data_type/dim/
			// distance_metric rather than one vectorArgs list.
			if got, _ := attribute["algorithm"].(string); got != "FLAT" {
				return fmt.Errorf("episode index %s vector algorithm = %q, want FLAT",
					indexName, got)
			}
			if got, _ := attribute["data_type"].(string); got != vectorArgs[1] {
				return fmt.Errorf("episode index %s vector data_type = %q, want %q",
					indexName, got, vectorArgs[1])
			}
			gotDim, _ := attribute["dim"].(int64)
			if strconv.FormatInt(gotDim, 10) != vectorArgs[2] {
				return fmt.Errorf("episode index %s vector dim = %d, want %s",
					indexName, gotDim, vectorArgs[2])
			}
			if got, _ := attribute["distance_metric"].(string); got != vectorArgs[3] {
				return fmt.Errorf("episode index %s vector distance = %q, want %q",
					indexName, got, vectorArgs[3])
			}
		}
	}
	for _, want := range expected {
		name := want["attribute"].(string)
		if !seen[name] {
			return fmt.Errorf("episode index %s is missing field %s: refusing to "+
				"run against a drifted schema", indexName, name)
		}
	}
	// The index's own prefix must be this namespace, exactly one prefix.
	rawPrefixes, _ := info["index_definition"].(map[string]any)
	prefixes, _ := rawPrefixes["prefixes"].([]any)
	if len(prefixes) != 1 {
		return fmt.Errorf("episode index %s covers %d prefixes, want exactly 1",
			indexName, len(prefixes))
	}
	prefix, _ := prefixes[0].(string)
	if prefix != keyPrefix {
		return fmt.Errorf("episode index %s covers prefix %q, want %q",
			indexName, prefix, keyPrefix)
	}
	// The default language is part of the schema: the Chinese text search
	// depends on it.
	if lang, _ := rawPrefixes["default_language"].(string); lang != "chinese" {
		return fmt.Errorf("episode index %s default language = %q, want chinese",
			indexName, lang)
	}
	return nil
}

// expectedEpisodeAttributes is the schema this package creates, expressed
// with the same keys raw FT.INFO reports.
func expectedEpisodeAttributes(keyPrefix string, dimension int) []map[string]any {
	return []map[string]any{
		{"attribute": EpisodeFieldEpisodeID, "type": "TAG"},
		{"attribute": EpisodeFieldUserID, "type": "TAG"},
		{"attribute": EpisodeFieldSessionID, "type": "TAG"},
		{"attribute": EpisodeFieldStatus, "type": "TAG"},
		{"attribute": EpisodeFieldLastMessageAt, "type": "NUMERIC", "sortable": true},
		{"attribute": EpisodeFieldTitle, "type": "TEXT", "weight": 3.0, "nostem": true},
		{"attribute": EpisodeFieldTopicKeys, "type": "TAG"},
		{"attribute": EpisodeFieldSearchText, "type": "TEXT", "nostem": true},
		{"attribute": EpisodeFieldSearchBigrams, "type": "TAG", "separator": ","},
		{"attribute": EpisodeFieldEmbeddingVersion, "type": "NUMERIC"},
		{"attribute": EpisodeFieldVector, "type": "VECTOR",
			"vector": []string{"FLAT", "FLOAT32", strconv.Itoa(dimension), "COSINE"}},
	}
}

// ftInfoMap folds the flat key/value slice a generic FT.INFO returns into
// the nested map shape the verifier walks. A trailing unpaired key is a
// flag Redis emits with no value (NOSTEM, SORTABLE); it is stored as a
// present-with-nil entry so presence checks still work.
func ftInfoMap(flat []any) map[string]any {
	out := map[string]any{}
	for i := 0; i+1 < len(flat); i += 2 {
		key, _ := flat[i].(string)
		switch value := flat[i+1].(type) {
		case []any:
			out[key] = ftInfoMapSlice(value)
		default:
			out[key] = value
		}
	}
	if len(flat)%2 == 1 {
		if key, ok := flat[len(flat)-1].(string); ok {
			out[key] = nil
		}
	}
	return out
}

// ftInfoMapSlice maps a flat slice of alternating pairs; a slice whose
// entries alternate string/anything becomes a map, anything else stays a
// slice (FT.INFO's attributes and prefixes are maps and plain lists
// respectively).
func ftInfoMapSlice(entries []any) any {
	if len(entries)%2 != 0 {
		return entries
	}
	guess := map[string]any{}
	keys := true
	for i := 0; i < len(entries); i += 2 {
		if _, ok := entries[i].(string); !ok {
			keys = false
			break
		}
	}
	if !keys {
		return entries
	}
	for i := 0; i+1 < len(entries); i += 2 {
		key, _ := entries[i].(string)
		switch value := entries[i+1].(type) {
		case []any:
			guess[key] = ftInfoMapSlice(value)
		default:
			guess[key] = value
		}
	}
	return guess
}

// FTInfoMapForTest exposes the flat FT.INFO parser for tests.
func FTInfoMapForTest(flat []any) map[string]any { return ftInfoMap(flat) }

// FoldAttributeForTest exposes the per-attribute fold for tests.
func FoldAttributeForTest(raw any) map[string]any {
	if entries, ok := raw.([]any); ok {
		return ftInfoMap(entries)
	}
	out, _ := raw.(map[string]any)
	return out
}

// VerifyEpisodeSchema exposes the schema verifier for the drift drills.
func VerifyEpisodeSchema(indexName, keyPrefix string, dimension int,
	info map[string]any) error {
	return verifyEpisodeSchemaRaw(indexName, keyPrefix, dimension, info)
}
