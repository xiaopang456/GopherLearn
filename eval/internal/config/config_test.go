package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiaopang456/GopherLearn/eval/internal/pathguard"
)

// writeConfig writes an evaluation configuration and returns its path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "eval.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// tomlPath quotes a path as a TOML literal string, so backslashes survive.
func tomlPath(p string) string { return "'" + p + "'" }

func validBody(t *testing.T, extra string) string {
	t.Helper()
	root := t.TempDir()
	return `[paths]
corpusManifest = ` + tomlPath(filepath.Join(root, "corpus.json")) + `
datasetFile = ` + tomlPath(filepath.Join(root, "dataset.jsonl")) + `
resultsDir = ` + tomlPath(filepath.Join(root, "results")) + `

[services]
productConfig = ` + tomlPath(filepath.Join(root, "product.toml.local")) + `
evalUserID = 900001
` + extra
}

func TestLoadAppliesDefaultsAndFailsClosedOnLimits(t *testing.T) {
	cfg, err := Load(writeConfig(t, validBody(t, "")))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cfg.Matrix.Methods, ","); got != "vector,keyword,hybrid,hybrid+rerank" {
		t.Errorf("methods = %s", got)
	}
	if len(cfg.Matrix.TopK) != 5 || cfg.Matrix.TopK[0] != 3 || cfg.Matrix.TopK[4] != 20 {
		t.Errorf("topK = %v", cfg.Matrix.TopK)
	}
	if cfg.Metrics.HitThreshold != 0.5 || cfg.Metrics.BootstrapSeed != 20261030 || cfg.Metrics.BootstrapResamples != 2000 {
		t.Errorf("metrics defaults = %+v", cfg.Metrics)
	}
	// Forgetting [limits] must mean "no paid calls", never "unlimited".
	if cfg.Limits.MaxEmbeddingCalls != 0 || cfg.Limits.MaxRerankCalls != 0 {
		t.Errorf("limits default to %+v, want zero", cfg.Limits)
	}
}

func TestLoadReadsEveryField(t *testing.T) {
	cfg, err := Load(writeConfig(t, validBody(t, `
[matrix]
methods = ["hybrid", "hybrid+rerank"]
topK = [5, 24]

[metrics]
hitThreshold = 0.6

[limits]
maxEmbeddingCalls = 120
maxRerankCalls = 45
`)))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Services.EvalUserID != 900001 {
		t.Errorf("evalUserID = %d", cfg.Services.EvalUserID)
	}
	if !cfg.Matrix.HasMethod(MethodHybridRerank) || cfg.Matrix.HasMethod(MethodVector) {
		t.Errorf("methods = %v", cfg.Matrix.Methods)
	}
	if len(cfg.Matrix.TopK) != 2 || cfg.Matrix.TopK[1] != 24 {
		t.Errorf("topK = %v", cfg.Matrix.TopK)
	}
	if cfg.Metrics.HitThreshold != 0.6 || cfg.Limits.MaxEmbeddingCalls != 120 || cfg.Limits.MaxRerankCalls != 45 {
		t.Errorf("metrics %+v limits %+v", cfg.Metrics, cfg.Limits)
	}
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	for name, extra := range map[string]string{
		"a misspelled limit":  "\n[limits]\nmaxEmbeddingCall = 5\n",
		"an unknown section":  "\n[budget]\nmax = 5\n",
		"a misspelled matrix": "\n[matrix]\ntopKs = [3]\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(writeConfig(t, validBody(t, extra)))
			if err == nil || !strings.Contains(err.Error(), "unknown key") {
				t.Fatalf("err = %v, want an unknown-key error", err)
			}
		})
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	cases := map[string]struct{ extra, want string }{
		"unknown method":        {"\n[matrix]\nmethods = [\"vector\", \"bm25\"]\n", `unknown method "bm25"`},
		"duplicate method":      {"\n[matrix]\nmethods = [\"vector\", \"vector\"]\n", "listed twice"},
		"no methods":            {"\n[matrix]\nmethods = []\n", "methods is empty"},
		"no topK":               {"\n[matrix]\ntopK = []\n", "topK is empty"},
		"topK zero":             {"\n[matrix]\ntopK = [0]\n", "outside 1..100"},
		"topK too big":          {"\n[matrix]\nmethods = [\"vector\"]\ntopK = [101]\n", "outside 1..100"},
		"duplicate topK":        {"\n[matrix]\ntopK = [3, 3]\n", "listed twice"},
		"rerank above the pool": {"\n[matrix]\ntopK = [8, 30]\n", "candidate pool"},
		"threshold zero":        {"\n[metrics]\nhitThreshold = 0.0\n", "hitThreshold"},
		"threshold above one":   {"\n[metrics]\nhitThreshold = 1.01\n", "hitThreshold"},
		"too few resamples":     {"\n[metrics]\nbootstrapResamples = 10\n", "bootstrapResamples"},
		"negative limit":        {"\n[limits]\nmaxRerankCalls = -1\n", "must not be negative"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Load(writeConfig(t, validBody(t, tc.extra)))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}

	t.Run("vector-only may go past the rerank pool", func(t *testing.T) {
		if _, err := Load(writeConfig(t, validBody(t, "\n[matrix]\nmethods = [\"vector\"]\ntopK = [30, 100]\n"))); err != nil {
			t.Fatal(err)
		}
	})
}

func TestLoadRejectsMissingOrRelativePathsAndUser(t *testing.T) {
	root := t.TempDir()
	full := func(replace map[string]string) string {
		fields := map[string]string{
			"corpusManifest": tomlPath(filepath.Join(root, "corpus.json")),
			"datasetFile":    tomlPath(filepath.Join(root, "dataset.jsonl")),
			"resultsDir":     tomlPath(filepath.Join(root, "results")),
			"productConfig":  tomlPath(filepath.Join(root, "product.toml.local")),
			"evalUserID":     "7",
		}
		for k, v := range replace {
			fields[k] = v
		}
		var b strings.Builder
		b.WriteString("[paths]\n")
		for _, k := range []string{"corpusManifest", "datasetFile", "resultsDir"} {
			if fields[k] != "" {
				b.WriteString(k + " = " + fields[k] + "\n")
			}
		}
		b.WriteString("[services]\n")
		for _, k := range []string{"productConfig", "evalUserID"} {
			if fields[k] != "" {
				b.WriteString(k + " = " + fields[k] + "\n")
			}
		}
		return b.String()
	}
	cases := map[string]struct {
		replace map[string]string
		want    string
	}{
		"no corpus manifest": {map[string]string{"corpusManifest": ""}, "paths.corpusManifest is required"},
		"no results dir":     {map[string]string{"resultsDir": ""}, "paths.resultsDir is required"},
		"no product config":  {map[string]string{"productConfig": ""}, "services.productConfig is required"},
		"relative dataset":   {map[string]string{"datasetFile": "'data/dataset.jsonl'"}, "must be an absolute path"},
		"no user":            {map[string]string{"evalUserID": ""}, "evalUserID must be positive"},
		"negative user":      {map[string]string{"evalUserID": "-3"}, "evalUserID must be positive"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Load(writeConfig(t, full(tc.replace)))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
	if _, err := Load(filepath.Join(root, "absent.toml")); err == nil {
		t.Fatal("a missing configuration file must be an error")
	}
}

func TestCheckPathsRefusesEveryConfiguredPathInsideAWorktree(t *testing.T) {
	outside := t.TempDir()
	if err := pathguard.Check("probe", outside); err != nil {
		t.Skipf("temp directory is inside a git working tree: %v", err)
	}
	repo := filepath.Join(outside, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	good := func() *Config {
		return &Config{Paths: Paths{
			CorpusManifest: filepath.Join(outside, "corpus.json"),
			DatasetFile:    filepath.Join(outside, "dataset.jsonl"),
			ResultsDir:     filepath.Join(outside, "results"),
		}, Services: Services{ProductConfig: filepath.Join(outside, "product.toml.local")}}
	}
	if err := good().CheckPaths(); err != nil {
		t.Fatalf("a configuration outside every repository was refused: %v", err)
	}
	for name, mutate := range map[string]func(*Config){
		"paths.corpusManifest":   func(c *Config) { c.Paths.CorpusManifest = filepath.Join(repo, "corpus.json") },
		"paths.datasetFile":      func(c *Config) { c.Paths.DatasetFile = filepath.Join(repo, "eval", "dataset.jsonl") },
		"paths.resultsDir":       func(c *Config) { c.Paths.ResultsDir = filepath.Join(repo, "results") },
		"services.productConfig": func(c *Config) { c.Services.ProductConfig = filepath.Join(repo, "product.toml.local") },
	} {
		cfg := good()
		mutate(cfg)
		err := cfg.CheckPaths()
		if !errors.Is(err, pathguard.ErrInsideWorktree) || !strings.Contains(err.Error(), name) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestDigestIsStableAndTracksSettings(t *testing.T) {
	load := func(extra string) *Config {
		cfg, err := Load(writeConfig(t, validBody(t, extra)))
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	base := load("")
	if base.Digest() != base.Digest() || len(base.Digest()) != 64 {
		t.Fatal("digest must be stable")
	}
	changed := *base
	changed.Limits.MaxRerankCalls = 1
	if changed.Digest() == base.Digest() {
		t.Fatal("changing a limit did not change the digest")
	}
}

// ---- product configuration ------------------------------------------------

func writeProduct(t *testing.T, entry string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "product.toml")
	body := `[jwtConfig]
key = "a-jwt-secret-of-at-least-thirty-two-bytes"

[ragModelConfig]
embeddingModel = "synthetic-embedding"
embeddingBaseUrl = "https://embedding.example.invalid/v1"
chatModelName = "synthetic-chat"
baseUrl = "https://chat.example.invalid/v1"
dimension = 64
` + entry
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadProductReusesTheProductLoader(t *testing.T) {
	path := writeProduct(t, "\n[isolatedEntry]\nembeddingKey = \"synthetic-key-never-sent\"\n")
	product, err := LoadProduct(path)
	if err != nil {
		t.Fatal(err)
	}
	if product.EmbeddingKey != "synthetic-key-never-sent" {
		t.Errorf("embedding key was not read")
	}
	rag := product.App.RagModelConfig
	if rag.RagEmbeddingModel != "synthetic-embedding" || rag.RagDimension != 64 || rag.RagRerankEnabled {
		t.Errorf("rag settings = %+v", rag)
	}
}

func TestLoadProductRequiresTheEmbeddingKeyAndAValidProductFile(t *testing.T) {
	for name, entry := range map[string]string{
		"no isolatedEntry":  "",
		"a blank key":       "\n[isolatedEntry]\nembeddingKey = \"  \"\n",
		"the key is absent": "\n[isolatedEntry]\nisolated = true\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := LoadProduct(writeProduct(t, entry))
			if err == nil || !strings.Contains(err.Error(), "embeddingKey is required") {
				t.Fatalf("err = %v", err)
			}
		})
	}
	if _, err := LoadProduct(filepath.Join(t.TempDir(), "absent.toml")); err == nil {
		t.Fatal("a missing product file must be an error")
	}
	// The product loader's own validation still applies: a rerank-enabled file
	// without the token in the environment is refused, and the error names the
	// variable (never a value).
	t.Setenv("RAG_RERANK_AUTH_TOKEN", "")
	os.Unsetenv("RAG_RERANK_AUTH_TOKEN")
	path := writeProduct(t, "rerankEnabled = true\nrerankProvider = \"dashscope\"\nrerankEndpoint = \"https://rerank.example.invalid/v1/rerank\"\nrerankModel = \"synthetic-rerank\"\n\n[isolatedEntry]\nembeddingKey = \"k\"\n")
	// rerankEnabled sits in [ragModelConfig]; appending it above is only valid
	// before the next table header, which writeProduct's layout guarantees.
	if _, err := LoadProduct(path); err == nil || !strings.Contains(err.Error(), "RAG_RERANK_AUTH_TOKEN") {
		t.Fatalf("err = %v, want it to name RAG_RERANK_AUTH_TOKEN", err)
	}
}
