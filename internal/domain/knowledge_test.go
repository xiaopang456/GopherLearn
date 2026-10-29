package domain

import "testing"

// TestServingVersionIsKeyedOnRecordedAbsence pins the one rule that decides which
// version's chunks answer queries. Both halves matter on their own: a legacy row has
// to keep serving its own version after the lifecycle columns are added, and a row the
// lifecycle staged has to serve nothing until an indexing pass succeeds. A
// compatibility reading based on "ActiveVersion is 0" satisfies the first and breaks
// the second, which is why each row is asserted separately.
func TestServingVersionIsKeyedOnRecordedAbsence(t *testing.T) {
	cases := []struct {
		name        string
		document    Document
		wantServing int
		wantLive    bool
	}{
		{
			name:        "a row from before the lifecycle columns serves its own version",
			document:    Document{Version: 3, ActiveVersion: 0},
			wantServing: 3,
			wantLive:    true,
		},
		{
			name:        "a staged version answers nothing until indexing succeeds",
			document:    Document{Version: 2, ActiveVersion: 0, IndexStatus: DocumentStatusPending},
			wantServing: 0,
			wantLive:    false,
		},
		{
			name:        "a claimed pass is still not a version anyone can read",
			document:    Document{Version: 2, ActiveVersion: 0, IndexStatus: DocumentStatusIndexing},
			wantServing: 0,
			wantLive:    false,
		},
		{
			name:        "a failed update leaves the previous version answering",
			document:    Document{Version: 2, ActiveVersion: 1, IndexStatus: DocumentStatusFailed},
			wantServing: 1,
			wantLive:    true,
		},
		{
			name:        "a ready document serves what it published",
			document:    Document{Version: 4, ActiveVersion: 4, IndexStatus: DocumentStatusReady},
			wantServing: 4,
			wantLive:    true,
		},
		{
			name:        "a superseded version awaiting cleanup still serves the new one",
			document:    Document{Version: 5, ActiveVersion: 5, IndexStatus: DocumentStatusCleanupPending},
			wantServing: 5,
			wantLive:    true,
		},
		{
			name:        "a delete hides a version that is still stored",
			document:    Document{Version: 5, ActiveVersion: 5, IndexStatus: DocumentStatusDeleting},
			wantServing: 5,
			wantLive:    false,
		},
		{
			name:        "a finished delete is not retrievable either",
			document:    Document{Version: 5, ActiveVersion: 5, IndexStatus: DocumentStatusDeleted},
			wantServing: 5,
			wantLive:    false,
		},
		{
			name:        "a legacy version number of zero was never published",
			document:    Document{Version: 0},
			wantServing: 0,
			wantLive:    false,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := testCase.document.ServingVersion(); got != testCase.wantServing {
				t.Fatalf("ServingVersion() = %d, want %d for %#v", got, testCase.wantServing, testCase.document)
			}
			if got := testCase.document.Retrievable(); got != testCase.wantLive {
				t.Fatalf("Retrievable() = %v, want %v for %#v", got, testCase.wantLive, testCase.document)
			}
		})
	}
}

// TestRewritableIsTheIngestGate pins the check an ingest makes before it stores bytes.
// Both directions matter on their own: a document mid-pass or mid-delete must not take a
// second racing version, and an untracked legacy row must still accept a rewrite - the
// store's own gate reads its absence as published, and a service that disagreed would
// refuse every migrated document.
func TestRewritableIsTheIngestGate(t *testing.T) {
	cases := []struct {
		name string
		row  Document
		want bool
	}{
		{"a published document takes a rewrite", Document{Version: 2, ActiveVersion: 2, IndexStatus: DocumentStatusReady}, true},
		{"an untracked legacy row takes a rewrite", Document{Version: 3}, true},
		{"a staged pass is still working", Document{Version: 2, ActiveVersion: 0, IndexStatus: DocumentStatusPending}, false},
		{"a claimed pass holds the document", Document{Version: 2, ActiveVersion: 1, IndexStatus: DocumentStatusIndexing}, false},
		{"a failed pass is retried, not rewritten over", Document{Version: 2, ActiveVersion: 1, IndexStatus: DocumentStatusFailed}, false},
		{"outstanding cleanup still counts as healthy", Document{Version: 2, ActiveVersion: 2, IndexStatus: DocumentStatusCleanupPending}, false},
		{"a delete in progress takes no new version", Document{Version: 2, IndexStatus: DocumentStatusDeleting}, false},
		{"a finished document takes no new version", Document{Version: 2, IndexStatus: DocumentStatusDeleted}, false},
		{"an untracked row that never published takes nothing", Document{Version: 0}, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := testCase.row.Rewritable(); got != testCase.want {
				t.Fatalf("Rewritable() = %v, want %v for %#v", got, testCase.want, testCase.row)
			}
		})
	}
}

// TestFailedUpdateKeepsAVersionToRebuild states the consequence the index rebuild
// depends on: after an update fails, the document still has a serving version, so a
// rebuild has something to restore and the staged version is not it.
func TestFailedUpdateKeepsAVersionToRebuild(t *testing.T) {
	staged := Document{Version: 2, ActiveVersion: 1, IndexStatus: DocumentStatusFailed}
	if !staged.Retrievable() {
		t.Fatal("a failed update left nothing serving; the reader would lose the document outright")
	}
	if staged.ServingVersion() == staged.Version {
		t.Fatalf("serving version %d is the staged one; rebuilding it would publish the failed pass", staged.Version)
	}
	pending := Document{Version: 2, IndexStatus: DocumentStatusPending}
	if pending.Retrievable() {
		t.Fatal("a document that has never published reports itself retrievable")
	}
}
