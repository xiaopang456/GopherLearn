// Package dataset reads the two inputs of an evaluation: the corpus manifest and
// the question set. Both live outside the repository (see internal/pathguard);
// this package only checks that they are well formed and agree with each other.
package dataset

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Document is one corpus file as the evaluation indexes it.
type Document struct {
	// File is the manifest's name for the document: a slash-separated relative
	// path, and the name evidence spans refer to.
	File string
	// Path is where the file was read from.
	Path string
	// SHA256 is the hash of the file's bytes, lowercase hex.
	SHA256 string
	// Size is the file's length in bytes.
	Size int64
	// Lines is the number of lines (see CountLines).
	Lines int
}

// Corpus is the set of documents a run indexes.
type Corpus struct {
	Name      string
	Root      string
	Documents []Document

	byFile map[string]int
}

// manifestFile is the JSON the manifest is stored as:
//
//	{
//	  "name": "my-notes-2026-10",
//	  "root": "files",                       (optional, relative to the manifest)
//	  "documents": [
//	    {"file": "go/memory-model.md", "sha256": "ab12…"}   (sha256 is optional)
//	  ]
//	}
type manifestFile struct {
	Name      string `json:"name"`
	Root      string `json:"root"`
	Documents []struct {
		File   string `json:"file"`
		SHA256 string `json:"sha256"`
	} `json:"documents"`
}

// LoadCorpus reads a manifest and every document it lists.
//
// It verifies what the manifest claims: names are clean relative paths and
// unique, every file exists and is not empty, and a declared hash matches the
// file. A corpus that drifted from its manifest therefore fails loudly instead of
// quietly changing the results.
func LoadCorpus(manifestPath string) (*Corpus, error) {
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("read corpus manifest: %w", err)
	}
	var manifest manifestFile
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return nil, fmt.Errorf("parse corpus manifest %s: %w", manifestPath, err)
	}
	if strings.TrimSpace(manifest.Name) == "" {
		return nil, fmt.Errorf("corpus manifest %s: name is required", manifestPath)
	}
	if len(manifest.Documents) == 0 {
		return nil, fmt.Errorf("corpus manifest %s: documents is empty", manifestPath)
	}
	root := filepath.Dir(manifestPath)
	if manifest.Root != "" {
		if !validName(manifest.Root) {
			return nil, fmt.Errorf("corpus manifest %s: root %q must be a relative path without \"..\"", manifestPath, manifest.Root)
		}
		root = filepath.Join(root, filepath.FromSlash(manifest.Root))
	}

	corpus := &Corpus{Name: manifest.Name, Root: root, byFile: map[string]int{}}
	for _, entry := range manifest.Documents {
		if !validName(entry.File) || entry.File == "." {
			return nil, fmt.Errorf("corpus manifest %s: document name %q must be a relative path without \"..\"", manifestPath, entry.File)
		}
		if _, dup := corpus.byFile[entry.File]; dup {
			return nil, fmt.Errorf("corpus manifest %s: document %q is listed twice", manifestPath, entry.File)
		}
		path := filepath.Join(root, filepath.FromSlash(entry.File))
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("corpus document %q: %w", entry.File, err)
		}
		if len(data) == 0 {
			return nil, fmt.Errorf("corpus document %q is empty", entry.File)
		}
		sum := sha256.Sum256(data)
		got := hex.EncodeToString(sum[:])
		if want := strings.ToLower(strings.TrimSpace(entry.SHA256)); want != "" && want != got {
			return nil, fmt.Errorf("corpus document %q changed: manifest sha256 %s, file sha256 %s", entry.File, want, got)
		}
		corpus.byFile[entry.File] = len(corpus.Documents)
		corpus.Documents = append(corpus.Documents, Document{
			File:   entry.File,
			Path:   path,
			SHA256: got,
			Size:   int64(len(data)),
			Lines:  CountLines(data),
		})
	}
	return corpus, nil
}

// Lookup returns the document the manifest calls file.
func (c *Corpus) Lookup(file string) (Document, bool) {
	i, ok := c.byFile[file]
	if !ok {
		return Document{}, false
	}
	return c.Documents[i], true
}

// Hash identifies the corpus content: the sorted (file, sha256) pairs. Renaming
// the manifest, reordering it or changing its formatting does not change it;
// changing a document, adding one or renaming one does. The index fingerprint
// and each run's manifest record it.
func (c *Corpus) Hash() string {
	pairs := make([]string, 0, len(c.Documents))
	for _, doc := range c.Documents {
		pairs = append(pairs, doc.File+"\x00"+doc.SHA256)
	}
	sort.Strings(pairs)
	sum := sha256.Sum256([]byte(strings.Join(pairs, "\n")))
	return hex.EncodeToString(sum[:])
}

// CountLines counts the lines of a document the way evidence spans number them:
// 1-based, "\n" ends a line, "\r\n" counts as one terminator, and a final line
// without a terminator still counts. An empty document has zero lines, and a
// trailing terminator does not start another line.
func CountLines(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	n := bytes.Count(data, []byte{'\n'})
	if data[len(data)-1] != '\n' {
		n++
	}
	return n
}

// validName accepts a slash-separated relative path that stays inside its root on
// every platform: no "..", no leading slash, and no backslash or colon, which
// Windows reads as a separator or a drive.
func validName(name string) bool {
	return fs.ValidPath(name) && !strings.ContainsAny(name, `\:`)
}
