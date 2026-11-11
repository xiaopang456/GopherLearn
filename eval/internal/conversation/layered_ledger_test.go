package conversation

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The ledger of a read-side run has to reach the disk.
//
// A ledger that only exists inside the result file cannot be used to ask
// which model calls a run made, or which calls no run claims: the reader
// would have to parse the whole result to find them. It is written as its own
// file next to the result, and it is written even when it is empty, so that a
// reader can tell a run that made no calls from a run whose ledger was never
// written.
func TestTheReadSideRunWritesItsLedgerNextToItsResult(t *testing.T) {
	dir := t.TempDir()
	read := LayeredRead{
		Scenario: "synthetic",
		Question: "现在用哪个编辑器？",
		Mode:     "on",
		Verdict:  "pass",
		Calls: []ModelCall{
			{ID: 1, Role: "summary", Purpose: "summary", Method: "http:/v1/chat/completions", Model: "synthetic"},
			{ID: 2, Role: "answer", Purpose: "answer", Method: "http:/v1/chat/completions", Model: "synthetic"},
		},
	}
	if err := WriteLayeredReadFiles(dir, read); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "read.json"))
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err = json.Unmarshal(body, &result); err != nil {
		t.Fatal(err)
	}
	if result["verdict"] != "pass" {
		t.Fatalf("the result was not written: %v", result)
	}
	calls, err := os.ReadFile(filepath.Join(dir, "model-calls.json"))
	if err != nil {
		t.Fatal(err)
	}
	var written []ModelCall
	if err = json.Unmarshal(calls, &written); err != nil {
		t.Fatal(err)
	}
	if len(written) != 2 {
		t.Fatalf("the ledger holds %d calls, want 2", len(written))
	}
	// The role and the purpose are what make a call attributable, so they
	// have to survive the round trip.
	if written[0].Role != "summary" || written[0].Purpose != "summary" {
		t.Fatalf("the first call lost its role or its purpose: %+v", written[0])
	}
	if written[1].Role != "answer" || written[1].Purpose != "answer" {
		t.Fatalf("the second call lost its role or its purpose: %+v", written[1])
	}
}

// A run that made no calls still writes its ledger, so that "no calls" and
// "no ledger" are two different facts.
func TestTheReadSideRunWritesAnEmptyLedgerRatherThanNone(t *testing.T) {
	dir := t.TempDir()
	if err := WriteLayeredReadFiles(dir, LayeredRead{Scenario: "synthetic"}); err != nil {
		t.Fatal(err)
	}
	calls, err := os.ReadFile(filepath.Join(dir, "model-calls.json"))
	if err != nil {
		t.Fatal(err)
	}
	var written []ModelCall
	if err = json.Unmarshal(calls, &written); err != nil {
		t.Fatal(err)
	}
	if len(written) != 0 {
		t.Fatalf("the ledger holds %d calls, want none", len(written))
	}
}
