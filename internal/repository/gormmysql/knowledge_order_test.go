package gormmysql

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestKnowledgeWritePathsLockBeforeTheyRead is a structural guard, and it is worth
// being precise about what it can and cannot show. The ordering itself - user row
// lock, then read the document, then decide - is only observable on an engine that
// honours FOR UPDATE, so the behavioural proof is
// TestRealMySQLKnowledgeWriteLocking, which is gated and skipped without a server.
//
// What this test does prove, every run and with no database, is that the guard is not
// vacuous: it locates the two write transactions in this file by their real call
// sequence and refuses as soon as a locking read stops being the first thing inside
// them. Reverting ReplaceDocumentContent to its previous read-then-lock shape fails
// here immediately, in the default suite, rather than only in a gated run nobody has
// been authorised to start.
//
// It says nothing about whether MySQL serialises, whether the read snapshot is
// established late enough, or whether the ceilings hold under concurrency. Those are
// the gated cases' job.
func TestKnowledgeWritePathsLockBeforeTheyRead(t *testing.T) {
	// The write paths are split across two files: the synchronous compositions and
	// the lock live in knowledge.go, the staged state machine in knowledge_lifecycle.go.
	// The guard reads both, so moving a transaction between them cannot quietly take a
	// locked path out of view.
	var source strings.Builder
	for _, name := range []string{"knowledge.go", "knowledge_lifecycle.go"} {
		contents, err := os.ReadFile(filepath.Join(name))
		if err != nil {
			t.Fatalf("reading the store source: %v", err)
		}
		// A checkout with core.autocrlf=true hands the guard CRLF bytes for the
		// same source. The guard matches structural spans like "\n\t})\n", so it
		// normalizes the line endings before matching: the shapes it pins are
		// code structure, not the checkout's newline convention.
		source.Write(bytes.ReplaceAll(contents, []byte("\r\n"), []byte("\n")))
	}

	for _, path := range []struct {
		name    string
		first   string
		decided []string
	}{
		{
			name:  "StageDocument",
			first: "lockUserRow(tx, userID)",
			// Ownership is decided after the lock, so a foreign collection still
			// answers not_found rather than being reached before the serialisation
			// point, and nothing may be measured before it either.
			decided: []string{"requireOwnedCollection(", "requireUniqueDocument(", "usageForUser("},
		},
		{
			name:  "StageReplacement",
			first: "lockUserRow(tx, userID)",
			// The live document is read, compared for unchanged content, measured for
			// uniqueness and for bytes - all after the lock.
			decided: []string{"Take(&current)", "ErrDocumentUnchanged", "requireUniqueDocument(", "usageForUser("},
		},
	} {
		body, ok := transactionBody(source.String(), path.name)
		if !ok {
			t.Fatalf("no transaction body found for %s; the guard has drifted from the code it reads", path.name)
		}
		lockAt := strings.Index(body, path.first)
		if lockAt < 0 {
			t.Errorf("%s takes no user row lock, so its ceilings have nothing behind them", path.name)
			continue
		}
		for _, decision := range path.decided {
			at := strings.Index(body, decision)
			if at < 0 {
				t.Errorf("%s never contains %q; update the guard deliberately, not by accident", path.name, decision)
				continue
			}
			if at < lockAt {
				t.Errorf("%s does %s before locking the user row - measure-then-write is back, and the ceilings are decided on a snapshot a competitor is about to replace", path.name, decision)
			}
		}
	}
}

// transactionBody returns the body of the named method's r.transaction(...) closure,
// or reports that the shape it expects is no longer there.
func transactionBody(source, method string) (string, bool) {
	marker := "func (r *KnowledgeRepository) " + method + "("
	at := strings.Index(source, marker)
	if at < 0 {
		return "", false
	}
	start := strings.Index(source[at:], "r.transaction(ctx, func(tx *gorm.DB) error {")
	if start < 0 {
		return "", false
	}
	body := source[at+start:]
	// The closure ends at the first line that closes it and returns the error.
	end := strings.Index(body, "\n\t})\n")
	if end < 0 {
		return "", false
	}
	return body[:end], true
}
