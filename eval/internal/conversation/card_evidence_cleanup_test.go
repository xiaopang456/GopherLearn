package conversation

// The evidence has to be on disk BEFORE the sandbox is torn down, and a
// failed write must keep the sandbox.
//
// The sandbox holds the run's own rows: the conversations, the messages, the
// projection jobs and the memory rows. A teardown that ran anyway would take
// the very thing a reader needs to locate the failure with it - and the
// failure would be left with a name and nothing to look at.
//
// So the write and the teardown are one decision, and the decision is
// exercised here WITHOUT a product, a database and a model: the write and
// the close are parameters, so a failing write is a one-line fake.
//
// Three things have to hold at the same time:
//
//   - a failed write fails the preparation;
//   - the sandbox is NOT closed;
//   - the failure says where the file should have been, which container was
//     kept, and what the run still holds.
//
// The third one is what makes the first two useful: a reader who is told
// that the evidence is missing has to be told what is NOT missing.

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// The sandbox stays when the evidence could not be written.
func TestTheSandboxIsKeptWhenTheEvidenceCannotBeWritten(t *testing.T) {
	closed := false
	write := func(string, []CardExchange) error {
		return errors.New("the disk is full")
	}
	close := func() error {
		closed = true
		return nil
	}
	err := writeCardEvidenceThenClose("/runs/attempt-1",
		[]CardExchange{{SessionID: "s-1", TargetMessageID: 42}}, "gl-eval-0123456789abcdef",
		cardEvidenceSaved{Calls: 31, Refused: 1}, write, close)
	if err == nil {
		t.Fatal("a failed evidence write must fail the preparation")
	}
	if closed {
		t.Fatal("the sandbox must not be torn down when the evidence was not written")
	}
	// The failure has to say where the file should have been, which container
	// was kept, and what the run still holds. A reader without those three
	// facts has nowhere to look.
	message := err.Error()
	for _, want := range []string{
		CardEvidenceFileName,
		filepath.Join("runs", "attempt-1"),
		"gl-eval-0123456789abcdef",
		"was kept",
		"31 model calls",
		"1 refused conversation",
		"NOT saved",
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("the failure must say %q: %s", want, message)
		}
	}
	// The cause of the write failure is still in the message: a reader has to
	// be able to tell a full disk from a path inside a repository.
	if !strings.Contains(message, "the disk is full") {
		t.Fatalf("the failure must carry its cause: %s", message)
	}
}

// The sandbox is closed once the evidence is on disk.
func TestTheSandboxIsClosedAfterTheEvidenceIsWritten(t *testing.T) {
	closed := false
	write := func(dir string, bodies []CardExchange) error {
		// The evidence has to be on disk BEFORE the teardown: the close is
		// the last thing that happens, not the first.
		if closed {
			t.Fatal("the sandbox was closed before the evidence was written")
		}
		if dir != "/runs/attempt-1" {
			t.Fatalf("the evidence was written to the wrong place: %q", dir)
		}
		if len(bodies) != 1 {
			t.Fatalf("the evidence was written without the bodies: %d", len(bodies))
		}
		return nil
	}
	close := func() error {
		closed = true
		return nil
	}
	if err := writeCardEvidenceThenClose("/runs/attempt-1", []CardExchange{{SessionID: "s-1"}},
		"gl-eval-0123456789abcdef", cardEvidenceSaved{Calls: 3}, write, close); err != nil {
		t.Fatalf("a successful write must not fail: %v", err)
	}
	if !closed {
		t.Fatal("the sandbox must be torn down once the evidence is on disk")
	}
}

// A failed teardown is still a failure: the evidence is on disk, but the
// container is left behind and the reader has to know.
func TestAFailedTeardownIsStillReported(t *testing.T) {
	write := func(string, []CardExchange) error { return nil }
	close := func() error { return errors.New("the container could not be removed") }
	err := writeCardEvidenceThenClose("/runs/attempt-1", nil, "gl-eval-0123456789abcdef",
		cardEvidenceSaved{}, write, close)
	if err == nil || !strings.Contains(err.Error(), "the container could not be removed") {
		t.Fatalf("a failed teardown must be reported: %v", err)
	}
}

// A preparation that captured no card body says so, instead of talking about
// bodies it does not have.
func TestTheFailureSaysWhenNoCardBodyWasCaptured(t *testing.T) {
	write := func(string, []CardExchange) error { return errors.New("no") }
	close := func() error {
		t.Fatal("the sandbox must not be torn down when the evidence was not written")
		return nil
	}
	err := writeCardEvidenceThenClose("/runs/attempt-1", nil, "gl-eval-0123456789abcdef",
		cardEvidenceSaved{Calls: 2, Refused: 0}, write, close)
	if err == nil {
		t.Fatal("a failed evidence write must fail the preparation")
	}
	if !strings.Contains(err.Error(), "no card body was captured") {
		t.Fatalf("the failure must say that nothing of that kind was lost: %s", err)
	}
}
