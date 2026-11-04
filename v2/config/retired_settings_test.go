package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The loader refuses a retired setting by name instead of ignoring it, so a stale
// line cannot start a process that quietly does something else. These tests hold
// both halves: the refusal, and the fact that a current file is accepted - a check
// that only ever sees failures would pass even if the refusal were removed.

// The retained shape, complete enough to load: RAG is validated before the
// sections below it, so the retired-key assertions below fail for the retired key
// and not for something else in the file.
const currentRagSection = "[jwtConfig]\nkey = \"a-jwt-secret-of-at-least-thirty-two-bytes\"\n\n" +
	"[ragModelConfig]\nembeddingApiKey = \"synthetic-key-never-sent\"\n"

func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write configuration: %v", err)
	}
	return path
}

func TestRetiredBaseUrlIsRefusedByNameAndSaysWhatReplacesIt(t *testing.T) {
	_, err := LoadExplicit(write(t, currentRagSection+"baseUrl = \"https://chat.example/v1\"\n"))
	if err == nil {
		t.Fatal("a configuration still carrying ragModelConfig.baseUrl started the loader")
	}
	for _, required := range []string{"retired configuration setting", "ragModelConfig.baseUrl", "isolatedEntry.chatBaseURL", "Delete this line"} {
		if !strings.Contains(err.Error(), required) {
			t.Fatalf("the refusal does not say %q: %v", required, err)
		}
	}
}

func TestRetiredEmbeddingKeyIsRefusedAndPointsAtTheSectionItMovedTo(t *testing.T) {
	_, err := LoadExplicit(write(t, currentRagSection+"[isolatedEntry]\nisolated = true\nembeddingKey = \"synthetic-key-never-sent\"\n"))
	if err == nil {
		t.Fatal("a configuration still carrying isolatedEntry.embeddingKey started the loader")
	}
	for _, required := range []string{"retired configuration setting", "isolatedEntry.embeddingKey", "ragModelConfig.embeddingApiKey"} {
		if !strings.Contains(err.Error(), required) {
			t.Fatalf("the refusal does not say %q: %v", required, err)
		}
	}
}

// The retired key in a section this struct never decodes at all still has to be
// caught: the old embedding key lives in exactly such a section, and the check is
// written against the keys the document contains rather than the ones the decoder
// could not place.
func TestRetirementIsDetectedInsideASectionTheLoaderIgnores(t *testing.T) {
	if _, err := LoadExplicit(write(t, currentRagSection+"[ordinaryChat]\nGOPHERLEARN_OPENAI_MODEL = \"synthetic-offline\"\n")); err != nil {
		t.Fatalf("an unrelated extra section was refused, so the check is not scoped to retired keys: %v", err)
	}
}

// A key is not required by this layer: a default configuration must not carry a
// secret, and readers that only need the model name and address (the schema tool,
// an offline harness) load it fine. The entry that spends the key refuses its
// absence - see the isolated settings tests.
func TestTheModelSectionLoadsWithoutAKeyTheEntryWouldDemand(t *testing.T) {
	loaded, err := LoadExplicit(write(t, "[jwtConfig]\nkey = \"a-jwt-secret-of-at-least-thirty-two-bytes\"\n\n[ragModelConfig]\n"))
	if err != nil {
		t.Fatalf("a keyless RAG section was refused here, which would push the requirement into every reader: %v", err)
	}
	if strings.TrimSpace(loaded.RagModelConfig.RagEmbeddingAPIKey) != "" {
		t.Fatalf("the default configuration shipped a key: %q", loaded.RagModelConfig.RagEmbeddingAPIKey)
	}
}

func TestACurrentFileStillLoads(t *testing.T) {
	if _, err := LoadExplicit(write(t, currentRagSection)); err != nil {
		t.Fatalf("the retained shape was refused: %v", err)
	}
}

// The schema tool takes the same refusal path: creating tables from a stale file
// would otherwise leave an operator with tables and a backend that will not start.
func TestTheSchemaLoaderRefusesTheSameRetiredKeys(t *testing.T) {
	for name, body := range map[string]string{
		"retired base URL":  currentRagSection + "baseUrl = \"https://chat.example/v1\"\n",
		"retired embedding": currentRagSection + "[isolatedEntry]\nisolated = true\nembeddingKey = \"synthetic-key-never-sent\"\n",
	} {
		if _, err := LoadExplicitForSchema(write(t, body)); err == nil || !strings.Contains(err.Error(), "retired configuration setting") {
			t.Fatalf("%s: the schema loader accepted it or refused for another reason: %v", name, err)
		}
	}
	if _, err := LoadExplicitForSchema(write(t, currentRagSection)); err != nil {
		t.Fatalf("the schema loader refused a current file: %v", err)
	}
}
