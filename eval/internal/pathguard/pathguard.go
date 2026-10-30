// Package pathguard keeps evaluation data out of the repository.
//
// The corpus, the question set and the raw results of a real run must never be
// committed: they hold the user's documents and the model's raw answers. The
// guard refuses any configured path that sits inside a git working tree, so a
// stray `git add .` cannot publish them.
package pathguard

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// ErrInsideWorktree is wrapped by every refusal.
var ErrInsideWorktree = errors.New("path is inside a git working tree")

// Check refuses every path that resolves inside a git working tree.
//
// A path need not exist yet (a results directory is created by the run): the
// nearest existing ancestor is resolved instead, symbolic links included, and
// every directory from there up to the filesystem root is searched for a `.git`
// entry. A `.git` file counts as well as a `.git` directory, because a linked
// worktree has a file.
//
// name labels the path in the error ("paths.resultsDir").
func Check(name, path string) error {
	if path == "" {
		return fmt.Errorf("%s is empty", name)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	dir, err := resolveExisting(abs)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	for {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return fmt.Errorf("%s = %s: %w (%s); keep evaluation data outside every repository", name, path, ErrInsideWorktree, dir)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%s: %w", name, err)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil
		}
		dir = parent
	}
}

// CheckAll checks several named paths and reports the first refusal.
func CheckAll(paths map[string]string) error {
	names := make([]string, 0, len(paths))
	for name := range paths {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := Check(name, paths[name]); err != nil {
			return err
		}
	}
	return nil
}

// resolveExisting returns the directory the path lives in once symbolic links are
// followed: the path itself when it is a directory, otherwise its parent, and for
// a path that does not exist yet, its nearest existing ancestor.
func resolveExisting(abs string) (string, error) {
	cur := abs
	for {
		real, err := filepath.EvalSymlinks(cur)
		if err == nil {
			info, statErr := os.Stat(real)
			if statErr != nil {
				return "", statErr
			}
			if !info.IsDir() {
				real = filepath.Dir(real)
			}
			return real, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", err
		}
		cur = parent
	}
}
