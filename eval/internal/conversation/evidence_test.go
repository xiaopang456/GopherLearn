package conversation

import (
	"encoding/json"
	"github.com/xiaopang456/GopherLearn/eval/internal/pathguard"
	"os"
	"path/filepath"
	"testing"
)

// Optional local artifacts contain synthetic test data only and are always
// written outside Git worktrees, with exclusive creation just like CLI results.
func writeOfflineEvidence(t *testing.T, name string, value any) {
	t.Helper()
	root := os.Getenv("GL_CONVERSATION_EVIDENCE_DIR")
	if root == "" {
		return
	}
	if err := pathguard.Check("evidence", root); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(filepath.Join(root, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	err = encoder.Encode(value)
	closeErr := file.Close()
	if err != nil {
		t.Fatal(err)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
}
