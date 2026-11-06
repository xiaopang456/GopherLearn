package redis

import (
	"context"
	"fmt"
	"strconv"

	redisCli "github.com/redis/go-redis/v9"
)

// The vector index component.
//
// Two things in this system are indexed as vectors over text: the episode
// cards, and the summary segments. They are different instruments - one is a
// card about another session, the other a stretch of this session's own
// conversation - but they are the same kind of instrument: a flat vector
// field, a text field, a bigram tag field and the tags a filter needs.
//
// So the schema is written once and named per instrument. An instrument is a
// name, a key prefix and a list of fields; nothing else changes between them.
//
// The verification is the load-bearing part. An index whose schema drifted is
// an index that answers questions about fields the rows never carried, and
// the drift is silent: the search still runs and still returns rows. So an
// existing index is verified field by field, and any difference refuses.

// VectorIndexField is one field of a vector index's schema.
type VectorIndexField struct {
	// Name is the field's name in the index.
	Name string
	// Type is one of TAG, TEXT, NUMERIC or VECTOR.
	Type string
	// Weight is the field's weight in the BM25 score. It only applies to a
	// text field.
	Weight float64
	// NoStem keeps the tokenizer from stemming the field's words. It is what
	// makes a Chinese term match as the learner wrote it.
	NoStem bool
	// Sortable allows the field to be sorted on.
	Sortable bool
	// Separator is the delimiter of a tag field's values.
	Separator string
	// Dimension is the vector's length. It only applies to a vector field.
	Dimension int
}

// VectorIndexSpec is one instrument's schema.
type VectorIndexSpec struct {
	// Label names the instrument in an error message: "episode", "summary
	// segment". It is what a person reads when the schema refuses.
	Label string
	// Name is the index's name in Redis.
	Name string
	// KeyPrefix is the prefix of the keys the index covers. It has to be
	// exactly one prefix: the verification checks it.
	KeyPrefix string
	// Dimension is the vector length the index's vector field carries.
	Dimension int
	// Fields are the index's fields, in the order they are created.
	Fields []VectorIndexField
	// DefaultLanguage is the tokenizer's language. The Chinese text search
	// depends on it, so it is part of the schema rather than a preference.
	DefaultLanguage string
}

// EnsureVectorIndex creates the index, or verifies an existing one.
//
// An existing index is verified field by field: type, sortable, weight,
// NOSTEM, the tag separator, the vector's algorithm, data type, dimension and
// distance, the index's own prefix and its default language must all match
// the schema this function would create. Any drift refuses, because a search
// run against a drifted schema reads fields the rows never carried.
func EnsureVectorIndex(ctx context.Context, client *redisCli.Client, spec VectorIndexSpec) error {
	if client == nil {
		return fmt.Errorf("%s index: redis client is nil", spec.Label)
	}
	if spec.Name == "" || spec.KeyPrefix == "" {
		return fmt.Errorf("%s index: the index needs a name and a key prefix", spec.Label)
	}
	if spec.Dimension <= 0 {
		return fmt.Errorf("%s index: dimension is %d", spec.Label, spec.Dimension)
	}
	if _, err := client.FTInfo(ctx, spec.Name).Result(); err == nil {
		raw, rawErr := client.Do(ctx, "FT.INFO", spec.Name).Result()
		if rawErr != nil {
			return fmt.Errorf("inspect %s index raw: %w", spec.Label, rawErr)
		}
		flat, ok := raw.([]any)
		if !ok || len(flat)%2 != 0 {
			return fmt.Errorf("unreadable FT.INFO for %s", spec.Name)
		}
		return verifyVectorIndexSchema(spec, ftInfoMap(flat))
	} else if !isUnknownIndexError(err) {
		return fmt.Errorf("inspect %s index: %w", spec.Label, err)
	}
	options := &redisCli.FTCreateOptions{OnHash: true, Prefix: []interface{}{spec.KeyPrefix},
		DefaultLanguage: spec.DefaultLanguage}
	if err := client.FTCreate(ctx, spec.Name, options, vectorIndexSchemas(spec)...).Err(); err != nil {
		if isIndexAlreadyExistsError(err) {
			return nil
		}
		return fmt.Errorf("create %s index: %w", spec.Label, err)
	}
	return nil
}

// vectorIndexSchemas is the schema this package creates.
func vectorIndexSchemas(spec VectorIndexSpec) []*redisCli.FieldSchema {
	schemas := make([]*redisCli.FieldSchema, 0, len(spec.Fields))
	for _, field := range spec.Fields {
		schema := &redisCli.FieldSchema{FieldName: field.Name}
		switch field.Type {
		case "TAG":
			schema.FieldType = redisCli.SearchFieldTypeTag
			schema.Separator = field.Separator
		case "TEXT":
			schema.FieldType = redisCli.SearchFieldTypeText
			schema.Weight = field.Weight
			schema.NoStem = field.NoStem
		case "NUMERIC":
			schema.FieldType = redisCli.SearchFieldTypeNumeric
			schema.Sortable = field.Sortable
		case "VECTOR":
			schema.FieldType = redisCli.SearchFieldTypeVector
			schema.VectorArgs = &redisCli.FTVectorArgs{FlatOptions: &redisCli.FTFlatOptions{
				Type: "FLOAT32", Dim: field.Dimension, DistanceMetric: "COSINE",
			}}
		default:
			// An unknown type is a programming error rather than a runtime
			// condition, and the index would be created without the field.
			continue
		}
		schemas = append(schemas, schema)
	}
	return schemas
}

// verifyVectorIndexSchema compares an existing index against the schema this
// package would create, using the raw FT.INFO map: the structured client
// result does not carry TAG separators, VECTOR arguments or the default
// language.
func verifyVectorIndexSchema(spec VectorIndexSpec, info map[string]any) error {
	attributes, _ := info["attributes"].([]any)
	expected := expectedVectorIndexAttributes(spec)
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
			return fmt.Errorf("%s index %s carries unexpected field %s: "+
				"refusing to run against a drifted schema", spec.Label, spec.Name, name)
		}
		seen[name] = true
		if got, _ := attribute["type"].(string); got != want["type"] {
			return fmt.Errorf("%s index %s field %s is type %q, want %q",
				spec.Label, spec.Name, name, got, want["type"])
		}
		if wantSortable, _ := want["sortable"].(bool); wantSortable {
			if _, present := attribute["SORTABLE"]; !present {
				return fmt.Errorf("%s index %s field %s is not sortable",
					spec.Label, spec.Name, name)
			}
		}
		if wantNoStem, _ := want["nostem"].(bool); wantNoStem {
			if _, present := attribute["NOSTEM"]; !present {
				return fmt.Errorf("%s index %s field %s is not NOSTEM",
					spec.Label, spec.Name, name)
			}
		}
		if wantWeight, ok := want["weight"].(float64); ok && wantWeight > 1 {
			// WEIGHT arrives as a string or a float depending on the reply
			// mode; compare numerically.
			gotWeight, _ := strconv.ParseFloat(fmt.Sprintf("%v",
				attribute["WEIGHT"]), 64)
			if attribute["WEIGHT"] != nil && gotWeight != wantWeight {
				return fmt.Errorf("%s index %s field %s weight = %v, want %v",
					spec.Label, spec.Name, name, attribute["WEIGHT"], wantWeight)
			}
		}
		if wantSeparator, _ := want["separator"].(string); wantSeparator != "" {
			got, _ := attribute["SEPARATOR"].(string)
			if got != wantSeparator {
				return fmt.Errorf("%s index %s field %s separator = %q, want %q",
					spec.Label, spec.Name, name, got, wantSeparator)
			}
		}
		if vectorArgs, ok := want["vector"].([]string); ok {
			// FT.INFO reports the vector field as algorithm/data_type/dim/
			// distance_metric rather than one vectorArgs list.
			if got, _ := attribute["algorithm"].(string); got != "FLAT" {
				return fmt.Errorf("%s index %s vector algorithm = %q, want FLAT",
					spec.Label, spec.Name, got)
			}
			if got, _ := attribute["data_type"].(string); got != vectorArgs[1] {
				return fmt.Errorf("%s index %s vector data_type = %q, want %q",
					spec.Label, spec.Name, got, vectorArgs[1])
			}
			gotDim, _ := attribute["dim"].(int64)
			if strconv.FormatInt(gotDim, 10) != vectorArgs[2] {
				return fmt.Errorf("%s index %s vector dim = %d, want %s",
					spec.Label, spec.Name, gotDim, vectorArgs[2])
			}
			if got, _ := attribute["distance_metric"].(string); got != vectorArgs[3] {
				return fmt.Errorf("%s index %s vector distance = %q, want %q",
					spec.Label, spec.Name, got, vectorArgs[3])
			}
		}
	}
	for _, want := range expected {
		name := want["attribute"].(string)
		if !seen[name] {
			return fmt.Errorf("%s index %s is missing field %s: refusing to "+
				"run against a drifted schema", spec.Label, spec.Name, name)
		}
	}
	// The index's own prefix must be this namespace, exactly one prefix.
	rawPrefixes, _ := info["index_definition"].(map[string]any)
	prefixes, _ := rawPrefixes["prefixes"].([]any)
	if len(prefixes) != 1 {
		return fmt.Errorf("%s index %s covers %d prefixes, want exactly 1",
			spec.Label, spec.Name, len(prefixes))
	}
	prefix, _ := prefixes[0].(string)
	if prefix != spec.KeyPrefix {
		return fmt.Errorf("%s index %s covers prefix %q, want %q",
			spec.Label, spec.Name, prefix, spec.KeyPrefix)
	}
	// The default language is part of the schema: the Chinese text search
	// depends on it.
	if lang, _ := rawPrefixes["default_language"].(string); lang != spec.DefaultLanguage {
		return fmt.Errorf("%s index %s default language = %q, want %q",
			spec.Label, spec.Name, lang, spec.DefaultLanguage)
	}
	return nil
}

// expectedVectorIndexAttributes is the schema this package creates, expressed
// with the same keys raw FT.INFO reports.
func expectedVectorIndexAttributes(spec VectorIndexSpec) []map[string]any {
	out := make([]map[string]any, 0, len(spec.Fields))
	for _, field := range spec.Fields {
		attribute := map[string]any{"attribute": field.Name, "type": field.Type}
		switch field.Type {
		case "TAG":
			if field.Separator != "" {
				attribute["separator"] = field.Separator
			}
		case "TEXT":
			if field.Weight > 1 {
				attribute["weight"] = field.Weight
			}
			if field.NoStem {
				attribute["nostem"] = true
			}
		case "NUMERIC":
			if field.Sortable {
				attribute["sortable"] = true
			}
		case "VECTOR":
			attribute["vector"] = []string{"FLAT", "FLOAT32", strconv.Itoa(field.Dimension), "COSINE"}
		}
		out = append(out, attribute)
	}
	return out
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
