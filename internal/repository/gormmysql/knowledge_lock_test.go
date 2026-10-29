package gormmysql

import (
	"errors"
	"testing"

	"github.com/xiaopang456/GopherLearn/internal/knowledge"
	"github.com/xiaopang456/GopherLearn/internal/repository"
)

// TestOwnerLockOutcomeIsRefusedWhereTheEngineCouldLock is the offline half of the
// user-lock rule: the decision itself, separated from the connection that would
// have to wait for it.
//
// The asymmetry is the point. On an engine that honours FOR UPDATE, no row means no
// serialisation, and continuing would let the store report a per-user ceiling that
// nothing held - so it has to fail. On SQLite the clause is ignored whether or not a
// row exists, so there is no observation to report and the store must not invent a
// failure the engine never showed it.
func TestOwnerLockOutcomeIsRefusedWhereTheEngineCouldLock(t *testing.T) {
	cases := []struct {
		dialect string
		found   bool
		want    error
	}{
		{dialect: "mysql", found: true, want: nil},
		{dialect: "mysql", found: false, want: knowledge.ErrOwnerUnresolved},
		{dialect: "postgres", found: false, want: knowledge.ErrOwnerUnresolved},
		{dialect: "sqlite", found: false, want: nil},
		{dialect: "sqlite", found: true, want: nil},
	}
	for _, item := range cases {
		got := ownerLockOutcome(item.dialect, item.found)
		if !errors.Is(got, item.want) && !(item.want == nil && got == nil) {
			t.Errorf("ownerLockOutcome(%q, %v) = %v, want %v", item.dialect, item.found, got, item.want)
		}
		if item.want == nil && got != nil {
			t.Errorf("ownerLockOutcome(%q, %v) = %v, want no error", item.dialect, item.found, got)
		}
	}
}

// TestUnresolvedOwnerIsNotAClientError keeps the refusal on the side it belongs to.
// Wrapping repository.ErrNotFound would answer 404 and send a reader to look for a
// missing document; ErrConflict would answer 409 and imply the user already owns
// something; ErrInvalidArgument would answer 400 and tell the client to fix a request
// they did not write wrong. None of those is true: the principal in a valid token has
// no row, which is a deployment fault.
func TestUnresolvedOwnerIsNotAClientError(t *testing.T) {
	for _, sentinel := range []error{
		repository.ErrNotFound, repository.ErrConflict, repository.ErrInvalidArgument,
		knowledge.ErrDocumentNameTaken, knowledge.ErrDocumentContentDuplicate,
		knowledge.ErrDocumentCountLimit, knowledge.ErrStorageLimit, knowledge.ErrDocumentUnchanged,
	} {
		if errors.Is(knowledge.ErrOwnerUnresolved, sentinel) {
			t.Errorf("ErrOwnerUnresolved is also %v, so the HTTP layer would classify it as that", sentinel)
		}
	}
	if supportsRowLock("sqlite") {
		t.Error("SQLite must not be listed as a row-locking engine; it ignores FOR UPDATE")
	}
	if !supportsRowLock("mysql") {
		t.Error("MySQL must be listed as a row-locking engine, or the ceilings have no mechanism")
	}
}
