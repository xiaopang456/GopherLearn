// Package config reads the evaluation's own configuration file.
//
// The file names where the corpus, the questions and the results live, which
// product configuration supplies the models and Redis address, which retrieval
// methods and TopK values to run, and how many paid calls a run may make.
// Nothing here is a secret: the keys stay in the product configuration, which is
// read by LoadProduct only when a run needs it.
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/xiaopang456/GopherLearn/eval/internal/pathguard"
)

// Retrieval methods of the matrix.
const (
	MethodVector       = "vector"
	MethodKeyword      = "keyword"
	MethodHybrid       = "hybrid"
	MethodHybridRerank = "hybrid+rerank"
)

// maxTopK bounds a TopK cell. A forced rerank is further limited to the
// reranker's pool of 24 (see rag.RerankCandidateK).
const (
	maxTopK       = 100
	maxRerankTopK = 24
)

// Config is the evaluation configuration.
type Config struct {
	Paths    Paths    `toml:"paths"`
	Services Services `toml:"services"`
	Matrix   Matrix   `toml:"matrix"`
	Metrics  Metrics  `toml:"metrics"`
	Limits   Limits   `toml:"limits"`
}

// Paths are absolute locations outside the repository.
type Paths struct {
	// CorpusManifest is the corpus manifest JSON (see dataset.LoadCorpus).
	CorpusManifest string `toml:"corpusManifest"`
	// DatasetFile is the question JSONL (see dataset.LoadDataset).
	DatasetFile string `toml:"datasetFile"`
	// ResultsDir receives one new directory per run.
	ResultsDir string `toml:"resultsDir"`
}

// Services point at the product configuration and the tenant the run uses.
type Services struct {
	// ProductConfig is a product configuration file in the format the isolated
	// entry reads. It supplies the embedding model and key, the dimension, the
	// reranker and the Redis address. It is a secret-bearing file.
	ProductConfig string `toml:"productConfig"`
	// EvalUserID is the user id every evaluation chunk is stored under.
	EvalUserID int64 `toml:"evalUserID"`
}

// Matrix selects the cells to run: every method at every TopK.
type Matrix struct {
	Methods []string `toml:"methods"`
	TopK    []int    `toml:"topK"`
}

// Metrics holds the scoring presets that must be fixed before results are seen.
type Metrics struct {
	// HitThreshold is the coverage at which a question counts as a hit at K.
	HitThreshold float64 `toml:"hitThreshold"`
	// BootstrapSeed seeds the cluster bootstrap, so a report can be regenerated
	// bit for bit.
	BootstrapSeed int64 `toml:"bootstrapSeed"`
	// BootstrapResamples is the number of bootstrap resamples per comparison.
	BootstrapResamples int `toml:"bootstrapResamples"`
}

// Limits cap the paid calls of a run. Zero means none: a run that would need a
// call the limit does not allow refuses to start, so forgetting to set a limit
// costs nothing.
type Limits struct {
	MaxEmbeddingCalls int `toml:"maxEmbeddingCalls"`
	MaxRerankCalls    int `toml:"maxRerankCalls"`
}

// Default returns the configuration a file starts from.
func Default() *Config {
	return &Config{
		Matrix: Matrix{
			Methods: []string{MethodVector, MethodKeyword, MethodHybrid, MethodHybridRerank},
			TopK:    []int{3, 5, 8, 12, 20},
		},
		Metrics: Metrics{HitThreshold: 0.5, BootstrapSeed: 20261030, BootstrapResamples: 2000},
	}
}

// Load reads and validates a configuration file.
//
// Unknown keys are an error. A misspelled limit would otherwise fall back to
// its default silently, which is the one place a typo must not be forgiven.
func Load(path string) (*Config, error) {
	cfg := Default()
	meta, err := toml.DecodeFile(path, cfg)
	if err != nil {
		return nil, fmt.Errorf("load evaluation configuration: %w", err)
	}
	if undecoded := meta.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, key := range undecoded {
			keys[i] = key.String()
		}
		sort.Strings(keys)
		return nil, fmt.Errorf("evaluation configuration %s has unknown key(s): %s", path, strings.Join(keys, ", "))
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("evaluation configuration %s: %w", path, err)
	}
	return cfg, nil
}

// Validate checks the values without touching the filesystem.
func (c *Config) Validate() error {
	for name, path := range c.namedPaths() {
		if strings.TrimSpace(path) == "" {
			return fmt.Errorf("%s is required", name)
		}
		if !filepath.IsAbs(path) {
			return fmt.Errorf("%s must be an absolute path, got %q", name, path)
		}
	}
	if c.Services.EvalUserID <= 0 {
		return fmt.Errorf("services.evalUserID must be positive, got %d", c.Services.EvalUserID)
	}
	if err := c.Matrix.validate(); err != nil {
		return err
	}
	if h := c.Metrics.HitThreshold; !(h > 0 && h <= 1) {
		return fmt.Errorf("metrics.hitThreshold must be in (0, 1], got %v", h)
	}
	if n := c.Metrics.BootstrapResamples; n < 100 || n > 100000 {
		return fmt.Errorf("metrics.bootstrapResamples must be in 100..100000, got %d", n)
	}
	if c.Limits.MaxEmbeddingCalls < 0 || c.Limits.MaxRerankCalls < 0 {
		return fmt.Errorf("limits must not be negative (embedding %d, rerank %d)", c.Limits.MaxEmbeddingCalls, c.Limits.MaxRerankCalls)
	}
	return nil
}

func (m Matrix) validate() error {
	if len(m.Methods) == 0 {
		return fmt.Errorf("matrix.methods is empty")
	}
	seen := map[string]bool{}
	for _, method := range m.Methods {
		switch method {
		case MethodVector, MethodKeyword, MethodHybrid, MethodHybridRerank:
		default:
			return fmt.Errorf("matrix.methods: unknown method %q (want %s, %s, %s or %s)", method, MethodVector, MethodKeyword, MethodHybrid, MethodHybridRerank)
		}
		if seen[method] {
			return fmt.Errorf("matrix.methods: %q is listed twice", method)
		}
		seen[method] = true
	}
	if len(m.TopK) == 0 {
		return fmt.Errorf("matrix.topK is empty")
	}
	topSeen := map[int]bool{}
	for _, k := range m.TopK {
		if k < 1 || k > maxTopK {
			return fmt.Errorf("matrix.topK: %d is outside 1..%d", k, maxTopK)
		}
		if topSeen[k] {
			return fmt.Errorf("matrix.topK: %d is listed twice", k)
		}
		topSeen[k] = true
		if seen[MethodHybridRerank] && k > maxRerankTopK {
			return fmt.Errorf("matrix.topK: %d is above %d, the reranker's candidate pool, so %q cannot run at it; drop the method or the value", k, maxRerankTopK, MethodHybridRerank)
		}
	}
	return nil
}

// HasMethod reports whether the matrix includes a method.
func (m Matrix) HasMethod(method string) bool {
	for _, have := range m.Methods {
		if have == method {
			return true
		}
	}
	return false
}

func (c *Config) namedPaths() map[string]string {
	return map[string]string{
		"paths.corpusManifest":   c.Paths.CorpusManifest,
		"paths.datasetFile":      c.Paths.DatasetFile,
		"paths.resultsDir":       c.Paths.ResultsDir,
		"services.productConfig": c.Services.ProductConfig,
	}
}

// CheckPaths refuses a configuration whose data, results or product
// configuration sit inside a git working tree.
func (c *Config) CheckPaths() error {
	return pathguard.CheckAll(c.namedPaths())
}

// Digest identifies the configuration for a run's manifest. The configuration
// holds no secret, so the digest is of its full canonical form.
func (c *Config) Digest() string {
	canonical, err := json.Marshal(c)
	if err != nil {
		// A Config is plain data; Marshal cannot fail on it.
		panic(err)
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}
