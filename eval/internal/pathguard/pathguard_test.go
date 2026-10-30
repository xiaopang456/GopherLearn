package pathguard

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// outsideRoot returns a temporary directory, or skips the test when the machine's
// temp directory itself sits inside a git working tree (the guard would then
// correctly refuse it, which these tests do not mean to examine).
func outsideRoot(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := Check("probe", dir); err != nil {
		t.Skipf("temp directory is inside a git working tree: %v", err)
	}
	return dir
}

func TestCheckAcceptsPathsOutsideEveryWorktree(t *testing.T) {
	root := outsideRoot(t)
	file := filepath.Join(root, "dataset.jsonl")
	if err := os.WriteFile(file, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{
		"existing directory":  root,
		"existing file":       file,
		"missing results dir": filepath.Join(root, "results", "run-1"),
	} {
		if err := Check(name, path); err != nil {
			t.Errorf("%s: unexpected refusal: %v", name, err)
		}
	}
}

func TestCheckRefusesPathsInsideAWorktree(t *testing.T) {
	root := outsideRoot(t)
	repo := filepath.Join(root, "repo")
	nested := filepath.Join(repo, "eval", "data")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(nested, "dataset.jsonl")
	if err := os.WriteFile(file, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{
		"the repository root":              repo,
		"a nested directory":               nested,
		"a file":                           file,
		"a directory that does not exist":  filepath.Join(nested, "results", "run-1"),
		"a deep missing path":              filepath.Join(repo, "a", "b", "c", "d"),
		"a path written with a dot-dot":    filepath.Join(root, "elsewhere", "..", "repo", "eval"),
		"an unclean path with a duplicate": nested + string(filepath.Separator) + string(filepath.Separator) + "x",
	} {
		err := Check("paths.x", path)
		if !errors.Is(err, ErrInsideWorktree) {
			t.Errorf("%s: err = %v, want ErrInsideWorktree", name, err)
		}
	}
}

// A linked worktree has a .git *file*, not a directory.
func TestCheckRefusesALinkedWorktreeMarkerFile(t *testing.T) {
	root := outsideRoot(t)
	tree := filepath.Join(root, "tree")
	if err := os.MkdirAll(tree, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, ".git"), []byte("gitdir: /elsewhere\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Check("paths.x", filepath.Join(tree, "data")); !errors.Is(err, ErrInsideWorktree) {
		t.Fatalf("err = %v, want ErrInsideWorktree", err)
	}
}

func TestCheckFollowsSymbolicLinks(t *testing.T) {
	root := outsideRoot(t)
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "innocent")
	if err := os.Symlink(repo, link); err != nil {
		t.Skipf("symbolic links are not available: %v", err)
	}
	if err := Check("paths.x", filepath.Join(link, "results")); !errors.Is(err, ErrInsideWorktree) {
		t.Fatalf("a link into a repository was accepted: %v", err)
	}
}

func TestCheckNamesThePathAndRejectsEmpty(t *testing.T) {
	root := outsideRoot(t)
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	err := Check("paths.resultsDir", repo)
	if err == nil || !strings.Contains(err.Error(), "paths.resultsDir") {
		t.Fatalf("the error must name the setting: %v", err)
	}
	if err := Check("paths.x", ""); err == nil {
		t.Fatal("an empty path must be refused")
	}
}

func TestCheckAllReportsEveryNamedPathDeterministically(t *testing.T) {
	root := outsideRoot(t)
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	err := CheckAll(map[string]string{
		"paths.b": repo,
		"paths.a": repo,
		"paths.c": root,
	})
	if !errors.Is(err, ErrInsideWorktree) || !strings.Contains(err.Error(), "paths.a") {
		t.Fatalf("the alphabetically first offender must be reported: %v", err)
	}
	if err := CheckAll(map[string]string{"paths.c": root}); err != nil {
		t.Fatal(err)
	}
}
