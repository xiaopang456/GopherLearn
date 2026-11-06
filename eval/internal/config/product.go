package config

import (
	"fmt"
	"strings"

	appconfig "GopherAI/config"
)

// Product is the product configuration a run needs: the application's own parsed
// configuration and the embedding key it will spend, which sits in the RAG section
// beside the embedding model name and address it authenticates to.
type Product struct {
	App          *appconfig.Config
	EmbeddingKey string
}

// LoadProduct reads the product configuration file with the loader the product
// itself uses (config.LoadExplicit), so the evaluation cannot drift from how the
// product reads its models, dimension and reranker settings.
//
// LoadExplicit reads one environment variable, RAG_RERANK_AUTH_TOKEN, and only
// when the file enables the reranker. The embedding key comes from
// [ragModelConfig].embeddingApiKey - the section holding the model it belongs to -
// and is taken from that same parse rather than a second decode of the file.
// Neither value is ever logged by this package.
func LoadProduct(path string) (*Product, error) {
	app, err := appconfig.LoadExplicit(path)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(app.RagModelConfig.RagEmbeddingAPIKey) == "" {
		return nil, fmt.Errorf("product configuration %s: ragModelConfig.embeddingApiKey is required", path)
	}
	return &Product{App: app, EmbeddingKey: app.RagModelConfig.RagEmbeddingAPIKey}, nil
}

// LoadProductOffline reads the product configuration for planning: the
// reranker section is neither read nor validated, so no rerank token is needed,
// and the embedding key is not required. Planning calls no service.
func LoadProductOffline(path string) (*appconfig.Config, error) {
	return appconfig.LoadExplicitForSchema(path)
}
