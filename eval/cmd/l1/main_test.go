package main

import (
	"bytes"
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
	code := run(args, &stdout, &stderr)
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
		"4 methods x 5 topK values = 20 cells",
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
