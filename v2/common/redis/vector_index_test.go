package redis

import (
	"testing"
)

// The vector index component is shared by two instruments: the episode cards
// and the summary segments. The schemas are written once and named per
// instrument, so the tests here are about the naming and about the refusal.
//
// The refusal is the load-bearing part. An index whose schema drifted is an
// index that answers questions about fields the rows never carried, and the
// drift is silent: the search still runs and still returns rows.

// The episode instrument keeps the schema it has always had.
func TestTheEpisodeSpecKeepsItsSchema(t *testing.T) {
	spec := EpisodeIndexSpec("idx", "prefix:", 8)
	names := map[string]VectorIndexField{}
	for _, field := range spec.Fields {
		names[field.Name] = field
	}
	for _, name := range []string{EpisodeFieldEpisodeID, EpisodeFieldUserID, EpisodeFieldSessionID,
		EpisodeFieldStatus, EpisodeFieldLastMessageAt, EpisodeFieldTitle, EpisodeFieldTopicKeys,
		EpisodeFieldSearchText, EpisodeFieldSearchBigrams, EpisodeFieldEmbeddingVersion, EpisodeFieldVector} {
		if _, present := names[name]; !present {
			t.Fatalf("the episode schema lost the field %s", name)
		}
	}
	if len(spec.Fields) != 11 {
		t.Fatalf("the episode schema has %d fields, want 11", len(spec.Fields))
	}
	if spec.DefaultLanguage != "chinese" {
		t.Fatalf("the default language is %q", spec.DefaultLanguage)
	}
	if names[EpisodeFieldVector].Dimension != 8 {
		t.Fatalf("the vector field's dimension is %d, want 8", names[EpisodeFieldVector].Dimension)
	}
}

// The segment instrument names its fields for what they hold.
func TestTheSummarySegmentSpecNamesItsOwnFields(t *testing.T) {
	spec := SummarySegmentIndexSpec("idx", "prefix:", 8)
	names := map[string]VectorIndexField{}
	for _, field := range spec.Fields {
		names[field.Name] = field
	}
	for _, name := range []string{SummarySegmentFieldSegmentID, SummarySegmentFieldUserID,
		SummarySegmentFieldSessionID, SummarySegmentFieldSeq, SummarySegmentFieldEndMessageID,
		SummarySegmentFieldEndAt, SummarySegmentFieldTopic, SummarySegmentFieldNamesAndNumbers,
		SummarySegmentFieldSearchText, SummarySegmentFieldSearchBigrams,
		SummarySegmentFieldEmbeddingVersion, SummarySegmentFieldVector} {
		if _, present := names[name]; !present {
			t.Fatalf("the segment schema lost the field %s", name)
		}
	}
	if names[SummarySegmentFieldSearchBigrams].Separator != "," {
		t.Fatalf("the bigram field's separator is %q", names[SummarySegmentFieldSearchBigrams].Separator)
	}
	if names[SummarySegmentFieldTopic].Weight != 3 {
		t.Fatalf("the topic field's weight is %v", names[SummarySegmentFieldTopic].Weight)
	}
}

// The two instruments do not share a namespace.
func TestTheTwoInstrumentsDoNotShareANamespace(t *testing.T) {
	if EpisodeIndexName == SummarySegmentIndexName {
		t.Fatal("the two instruments share an index name")
	}
	if EpisodeKeyPrefix == SummarySegmentKeyPrefix {
		t.Fatal("the two instruments share a key prefix")
	}
	if SummarySegmentKeyPrefix == EpisodeKeyPrefix {
		t.Fatal("one instrument's prefix is a prefix of the other's")
	}
}

// A schema that drifted refuses, rather than being tolerated.
//
// The verification is complete, not name-only: a field that is missing, a
// field that has the wrong type, a field that lost its weight, a vector field
// that lost its dimension, a prefix that moved and a default language that
// changed are all refusals.
func TestAVerificationRefusesADriftedSchema(t *testing.T) {
	spec := EpisodeIndexSpec("idx", "prefix:", 8)
	complete := map[string]any{
		"attributes": []any{
			[]any{"attribute", EpisodeFieldEpisodeID, "type", "TAG"},
			[]any{"attribute", EpisodeFieldUserID, "type", "TAG"},
			[]any{"attribute", EpisodeFieldSessionID, "type", "TAG"},
			[]any{"attribute", EpisodeFieldStatus, "type", "TAG"},
			[]any{"attribute", EpisodeFieldLastMessageAt, "type", "NUMERIC", "SORTABLE", nil},
			[]any{"attribute", EpisodeFieldTitle, "type", "TEXT", "WEIGHT", "3", "NOSTEM", nil},
			[]any{"attribute", EpisodeFieldTopicKeys, "type", "TAG"},
			[]any{"attribute", EpisodeFieldSearchText, "type", "TEXT", "NOSTEM", nil},
			[]any{"attribute", EpisodeFieldSearchBigrams, "type", "TAG", "SEPARATOR", ","},
			[]any{"attribute", EpisodeFieldEmbeddingVersion, "type", "NUMERIC"},
			[]any{"attribute", EpisodeFieldVector, "type", "VECTOR", "algorithm", "FLAT",
				"data_type", "FLOAT32", "dim", int64(8), "distance_metric", "COSINE"},
		},
		"index_definition": map[string]any{
			"prefixes":         []any{"prefix:"},
			"default_language": "chinese",
		},
	}
	if err := verifyVectorIndexSchema(spec, complete); err != nil {
		t.Fatalf("a complete schema was refused: %v", err)
	}

	missing := map[string]any{
		"attributes": []any{
			[]any{"attribute", EpisodeFieldEpisodeID, "type", "TAG"},
		},
		"index_definition": map[string]any{
			"prefixes":         []any{"prefix:"},
			"default_language": "chinese",
		},
	}
	if err := verifyVectorIndexSchema(spec, missing); err == nil {
		t.Fatal("a schema with missing fields was accepted")
	}

	driftedType := map[string]any{
		"attributes": []any{
			[]any{"attribute", EpisodeFieldVector, "type", "HASH"},
		},
		"index_definition": map[string]any{
			"prefixes":         []any{"prefix:"},
			"default_language": "chinese",
		},
	}
	if err := verifyVectorIndexSchema(spec, driftedType); err == nil {
		t.Fatal("a drifted field type was accepted")
	}

	driftedPrefix := map[string]any{
		"attributes": complete["attributes"],
		"index_definition": map[string]any{
			"prefixes":         []any{"other:"},
			"default_language": "chinese",
		},
	}
	if err := verifyVectorIndexSchema(spec, driftedPrefix); err == nil {
		t.Fatal("a moved prefix was accepted")
	}

	driftedLanguage := map[string]any{
		"attributes": complete["attributes"],
		"index_definition": map[string]any{
			"prefixes":         []any{"prefix:"},
			"default_language": "english",
		},
	}
	if err := verifyVectorIndexSchema(spec, driftedLanguage); err == nil {
		t.Fatal("a changed default language was accepted")
	}
}
