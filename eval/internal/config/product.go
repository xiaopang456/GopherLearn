package config

import (
	"fmt"
	"strings"

	"github.com/BurntSushi/toml"

	appconfig "GopherAI/config"
)

// Product is the product configuration a run needs: the application's own parsed
// configuration and the embedding key, which lives beside it in the isolated
// entry's section.
type Product struct {
	App          *appconfig.Config
	EmbeddingKey string
}

// LoadProduct reads the product configuration file with the loader the product
// itself uses (config.LoadExplicit), so the evaluation cannot drift from how the
// product reads its models, dimension and reranker settings.
//
// LoadExplicit reads one environment variable, RAG_RERANK_AUTH_TOKEN, and only
// when the file enables the reranker. The embedding key is read from
// [isolatedEntry].embeddingKey. Neither value is ever logged by this package.
func LoadProduct(path string) (*Product, error) {
	app, err := appconfig.LoadExplicit(path)
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Entry struct {
			EmbeddingKey string `toml:"embeddingKey"`
		} `toml:"isolatedEntry"`
	}
	if _, err := toml.DecodeFile(path, &envelope); err != nil {
		return nil, fmt.Errorf("read isolatedEntry from %s: %w", path, err)
	}
	if strings.TrimSpace(envelope.Entry.EmbeddingKey) == "" {
		return nil, fmt.Errorf("product configuration %s: isolatedEntry.embeddingKey is required", path)
	}
	return &Product{App: app, EmbeddingKey: envelope.Entry.EmbeddingKey}, nil
}
