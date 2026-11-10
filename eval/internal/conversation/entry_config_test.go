package conversation

import (
	"path/filepath"
	"testing"

	"GopherAI/entry"
	"github.com/BurntSushi/toml"
)

// The released product refuses a configuration file that carries the retired
// [ordinaryChat] section, and it refuses it by name: the integrated entry
// decodes the section and treats its presence - even an empty one - as the
// fact. The evaluation used to write that section, so every L4 v1 attempt
// failed before the product was even started, for that reason alone.
//
// The written configuration must carry no such section. And because the
// ordinary path and the learning Agent now share one client, a section that
// is gone must not become a request that leaves the machine: the chat model
// the product finds has to be the one the evaluation aims at its own loopback
// wire.
func TestEntryConfigCarriesNoRetiredOrdinaryChatSection(t *testing.T) {
	repository := filepath.Join("..", "..", "..")
	settings, err := offlineSettings(repository, filepath.Join(t.TempDir(), "knowledge"))
	if err != nil {
		t.Fatal(err)
	}
	settings.Entry.ChatBaseURL = "http://127.0.0.1:1/v1"
	// The rerank credential is read from the process environment, never from
	// the configuration file - the product reads it that way and so does the
	// run that writes this file.
	t.Setenv("RAG_RERANK_AUTH_TOKEN", settings.Config.RagRerankToken)
	path := filepath.Join(t.TempDir(), "product.toml")
	if err = writeEntryConfig(path, settings); err != nil {
		t.Fatal(err)
	}
	// The product's own rule, mirrored here: the section's presence is the
	// fact, so an empty-but-written section counts as written.
	var sections struct {
		Ordinary *map[string]string `toml:"ordinaryChat"`
	}
	if _, err = toml.DecodeFile(path, &sections); err != nil {
		t.Fatal(err)
	}
	if sections.Ordinary != nil {
		t.Fatal("the configuration still carries the retired [ordinaryChat] section")
	}
	// The product's own loader accepts the file the evaluation wrote.
	loaded, err := entry.Load(path)
	if err != nil {
		t.Fatalf("the product refused the configuration the evaluation wrote: %v", err)
	}
	if loaded.Entry.ChatBaseURL != "http://127.0.0.1:1/v1" {
		t.Fatalf("the chat model points somewhere else: %q", loaded.Entry.ChatBaseURL)
	}
}
