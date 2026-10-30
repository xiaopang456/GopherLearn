package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiaopang456/GopherLearn/eval/internal/pathguard"
)

// stageInputs copies the synthetic corpus and questions into a temp directory (the
// committed copies sit inside the repository, which the path guard refuses) and
// writes a configuration pointing at them. It returns the configuration path.
func stageInputs(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := pathguard.Check("temp directory", root); err != nil {
		t.Skipf("temp directory is inside a git working tree: %v", err)
	}
	copyTree(t, filepath.Join("..", "..", "testdata"), filepath.Join(root, "data"))
	path := filepath.Join(root, "eval.toml")
	body := "[paths]\n" +
		"corpusManifest = '" + filepath.Join(root, "data", "corpus.json") + "'\n" +
		"datasetFile = '" + filepath.Join(root, "data", "dataset.jsonl") + "'\n" +
		"resultsDir = '" + filepath.Join(root, "results") + "'\n" +
		"[services]\n" +
		"productConfig = '" + filepath.Join(root, "product.toml.local") + "'\n" +
		"evalUserID = 900001\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	product := `[jwtConfig]
key = "a-jwt-secret-of-at-least-thirty-two-bytes"

[ragModelConfig]
embeddingModel = "synthetic-embedding"
embeddingBaseUrl = "https://embedding.example.invalid/v1"
chatModelName = "synthetic-chat"
baseUrl = "https://chat.example.invalid/v1"
dimension = 1024
`
	if err := os.WriteFile(filepath.Join(root, "product.toml.local"), []byte(product), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func copyTree(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.Walk(from, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		target := filepath.Join(to, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func runCLI(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestCheckSummarisesTheSyntheticInputs(t *testing.T) {
	code, stdout, stderr := runCLI("check", "-config", stageInputs(t))
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	for _, want := range []string{
		"synthetic-harbour-v1: 3 documents, 65 lines",
		"6 (5 dev, 1 sealed); 5 answerable, 1 unanswerable",
		"4 methods x 5 topK values",
		"no service was contacted",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("output lacks %q:\n%s", want, stdout)
		}
	}
}

func TestCheckRefusesInputsInsideAWorktree(t *testing.T) {
	cfgPath := stageInputs(t)
	// Turn the staging directory into a "repository".
	if err := os.Mkdir(filepath.Join(filepath.Dir(cfgPath), ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := runCLI("check", "-config", cfgPath)
	if code == 0 || !strings.Contains(stderr, "inside a git working tree") {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
}

func TestCheckReportsBadInputs(t *testing.T) {
	cfgPath := stageInputs(t)
	data := filepath.Join(filepath.Dir(cfgPath), "data", "dataset.jsonl")
	if err := os.WriteFile(data, []byte(`{"id":"a","question":"q","answerable":true,"split":"dev"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := runCLI("check", "-config", cfgPath)
	if code == 0 || !strings.Contains(stderr, "at least one evidence set") {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
}

func TestUsageAndFlagErrors(t *testing.T) {
	if code, _, stderr := runCLI(); code != 2 || !strings.Contains(stderr, "usage:") {
		t.Errorf("no arguments: exit %d, %q", code, stderr)
	}
	if code, _, stderr := runCLI("frobnicate"); code != 2 || !strings.Contains(stderr, `unknown mode "frobnicate"`) {
		t.Errorf("unknown mode: exit %d, %q", code, stderr)
	}
	if code, _, stderr := runCLI("check"); code == 0 || !strings.Contains(stderr, "-config is required") {
		t.Errorf("missing -config: exit %d, %q", code, stderr)
	}
	if code, _, _ := runCLI("check", "-config", filepath.Join(t.TempDir(), "absent.toml")); code == 0 {
		t.Error("a missing configuration file must fail")
	}
	if code, stdout, _ := runCLI("help"); code != 0 || !strings.Contains(stdout, "modes:") {
		t.Errorf("help: exit %d", code)
	}
}

func TestPlanForecastsAgainstTheLimits(t *testing.T) {
	code, stdout, stderr := runCLI("plan", "-config", stageInputs(t))
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	for _, want := range []string{
		"5 (5 distinct texts) x 22 cells = 110 retrievals",
		"embedding calls",
		"rerank calls     6 = 5 queries + 1 startup check   (limit 0)",
		"a real run would be refused",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("output lacks %q:\n%s", want, stdout)
		}
	}
	if code, _, stderr := runCLI("plan", "-config", stageInputs(t), "-split", "nope"); code == 0 || !strings.Contains(stderr, "no questions") {
		t.Errorf("an empty split: exit %d, %q", code, stderr)
	}
}

func TestRunRequiresAnExplicitEnvironment(t *testing.T) {
	code, _, stderr := runCLI("run", "-config", stageInputs(t))
	if code != 2 || !strings.Contains(stderr, "-env must be") {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	if code, _, _ := runCLI("run", "-config", stageInputs(t), "-env", "prod"); code != 2 {
		t.Fatalf("an unknown -env must be refused, exit %d", code)
	}
}

func TestRunOnFakesWritesARunDirectoryAndSaysItIsOffline(t *testing.T) {
	cfgPath := stageInputs(t)
	code, stdout, stderr := runCLI("run", "-config", cfgPath, "-env", "fake")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	for _, want := range []string{"OFFLINE FAKE ENVIRONMENT", "110 (0 with errors); complete: true", "run directory"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("output lacks %q:\n%s", want, stdout)
		}
	}
	runs, err := os.ReadDir(filepath.Join(filepath.Dir(cfgPath), "results"))
	if err != nil || len(runs) != 1 {
		t.Fatalf("results: %v, %d runs", err, len(runs))
	}
	manifest, err := os.ReadFile(filepath.Join(filepath.Dir(cfgPath), "results", runs[0].Name(), "manifest.json"))
	if err != nil || !strings.Contains(string(manifest), `"environment": "fake"`) {
		t.Fatalf("the manifest must say the run was offline: %s %v", manifest, err)
	}
}

func TestRealRunNeedsAProductConfigWithAnEmbeddingKey(t *testing.T) {
	// The staged product file has no isolatedEntry.embeddingKey, so a real run
	// stops before it contacts anything.
	code, _, stderr := runCLI("run", "-config", stageInputs(t), "-env", "real")
	if code == 0 || !strings.Contains(stderr, "embeddingKey is required") {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
}
