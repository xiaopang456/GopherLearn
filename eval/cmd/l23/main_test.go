package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiaopang456/GopherLearn/eval/internal/pathguard"
)

// stage copies the golden outcomes into a temp directory under the names score.mjs writes.
func stage(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := pathguard.Check("temp directory", dir); err != nil {
		t.Skipf("temp directory is inside a git working tree: %v", err)
	}
	fixtures := filepath.Join("..", "..", "l2l3", "testdata")
	for from, to := range map[string]string{"outcomes.fixture.jsonl": "outcomes.jsonl", "outcomes.fixture.meta.json": "outcomes.meta.json"} {
		data, err := os.ReadFile(filepath.Join(fixtures, from))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, to), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func cli(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestReportWritesMarkdownAndJSONAndIsRepeatable(t *testing.T) {
	dir := stage(t)
	code, stdout, stderr := cli("report", "-outcomes", dir, "-baseline", "good")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "reported 48 outcomes from 2 arm(s)") {
		t.Errorf("stdout: %s", stdout)
	}
	first, err := os.ReadFile(filepath.Join(dir, "report.md"))
	if err != nil || !strings.Contains(string(first), "离线替身环境（fake）") {
		t.Fatalf("report.md: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "report.json")); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := cli("report", "-outcomes", dir, "-baseline", "good"); code != 0 {
		t.Fatalf("second run: %d %s", code, stderr)
	}
	second, _ := os.ReadFile(filepath.Join(dir, "report.md"))
	if !bytes.Equal(first, second) {
		t.Fatal("reporting twice gave different bytes")
	}
}

func TestReportRefusesWhatItCannotCompareOrTrust(t *testing.T) {
	dir := stage(t)
	if code, _, stderr := cli("report", "-outcomes", dir); code != 1 || !strings.Contains(stderr, "which one is the baseline") {
		t.Errorf("two arms, no baseline: %d %q", code, stderr)
	}
	if code, _, stderr := cli("report", "-outcomes", dir, "-baseline", "nobody"); code != 1 || !strings.Contains(stderr, "no outcomes") {
		t.Errorf("unknown baseline: %d %q", code, stderr)
	}
	if code, _, stderr := cli("report"); code != 2 || !strings.Contains(stderr, "-outcomes is required") {
		t.Errorf("no -outcomes: %d %q", code, stderr)
	}
	if code, _, stderr := cli("report", "-outcomes", dir, "-baseline", "good", "-resamples", "5"); code != 2 || !strings.Contains(stderr, "-resamples") {
		t.Errorf("too few resamples: %d %q", code, stderr)
	}
	if code, _, _ := cli("frobnicate"); code != 2 {
		t.Errorf("an unknown mode must be a usage error, exit %d", code)
	}
	if code, _, _ := cli("report", "-outcomes", filepath.Join(dir, "absent"), "-baseline", "good"); code != 1 {
		t.Errorf("a missing directory must fail, exit %d", code)
	}

	// A meta file that disagrees with the outcomes is a corrupted pair.
	meta := filepath.Join(dir, "outcomes.meta.json")
	data, _ := os.ReadFile(meta)
	if err := os.WriteFile(meta, []byte(strings.Replace(string(data), `"outcomes": 48`, `"outcomes": 47`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := cli("report", "-outcomes", dir, "-baseline", "good"); code != 1 || !strings.Contains(stderr, "says 47 outcomes") {
		t.Errorf("disagreeing meta: %d %q", code, stderr)
	}
}

func TestReportKeepsEvaluationDataOutOfWorktrees(t *testing.T) {
	dir := stage(t)
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := cli("report", "-outcomes", dir, "-baseline", "good", "-out", filepath.Join(repo, "out")); code != 1 || !strings.Contains(stderr, "inside a git working tree") {
		t.Errorf("-out inside a worktree: %d %q", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(repo, "out")); !os.IsNotExist(err) {
		t.Error("the output directory was created inside a worktree")
	}
}

// stageLedgers writes a run ledger and a call ledger: one run of the "good"
// arm, one call before its answer and one after it.
func stageLedgers(t *testing.T, dir string) (string, string) {
	t.Helper()
	ledger := filepath.Join(dir, "runs.jsonl")
	lines := []string{
		`{"at":"2026-11-02T11:00:00Z","phase":"run","arm":"good","qid":"q-bar","rep":0,"sessionId":"s1"}`,
		`{"at":"2026-11-02T11:00:10Z","phase":"result","arm":"good","qid":"q-bar","rep":0,"http":200,"statusCode":1000,"modelCalls":1,"toolCalls":0,"runId":"run-1","answeredAt":"2026-11-02T11:00:10Z"}`,
	}
	if err := os.WriteFile(ledger, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	calls := filepath.Join(dir, "calls.jsonl")
	entries := []string{
		`{"schema":"call.v1","at":"2026-11-02T11:00:05Z","lane":"chat","model":"m","stream":true,"run":"run-1","session":"s1","phase":"answer","usage":{"prompt":120,"completion":30,"total":150}}`,
		`{"schema":"call.v1","at":"2026-11-02T11:00:12Z","lane":"chat","model":"m","stream":false,"run":"run-1","session":"s1","phase":"after_answer","usage":{"prompt":800,"completion":200,"total":1000}}`,
	}
	if err := os.WriteFile(calls, []byte(strings.Join(entries, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return ledger, calls
}

func TestTheReportSplitsTheCostWhenACallLedgerIsGiven(t *testing.T) {
	dir := stage(t)
	ledger, calls := stageLedgers(t, dir)
	code, stdout, stderr := cli("report", "-outcomes", dir, "-baseline", "good", "-ledger", ledger, "-calls", calls)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	_ = stdout
	report, err := os.ReadFile(filepath.Join(dir, "report.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"## 表 6", "| good | 回答本身 |", "| good | 回答之后的后台 |", "不用墙钟时间窗"} {
		if !strings.Contains(string(report), want) {
			t.Errorf("report lacks %q", want)
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, "report.json"))
	if err != nil || !strings.Contains(string(data), "mean_after_answer_calls") {
		t.Fatalf("%v the json does not carry the split", err)
	}
}

func TestTheReportRefusesHalfALedger(t *testing.T) {
	dir := stage(t)
	ledger, calls := stageLedgers(t, dir)
	if code, _, stderr := cli("report", "-outcomes", dir, "-baseline", "good", "-ledger", ledger); code != 2 || !strings.Contains(stderr, "go together") {
		t.Errorf("only a run ledger: %d %q", code, stderr)
	}
	if code, _, stderr := cli("report", "-outcomes", dir, "-baseline", "good", "-calls", calls); code != 2 || !strings.Contains(stderr, "go together") {
		t.Errorf("only a call ledger: %d %q", code, stderr)
	}
}
