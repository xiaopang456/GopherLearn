package contextwindow

import (
	"context"
	"sync"
	"testing"
)

func TestEvidenceKeySeparatesEveryAxisThatChangesTheText(t *testing.T) {
	base := EvidenceID{Kind: "document", Namespace: "c/d", RecordID: "chunk-1",
		Version: 3, DisplayStart: 0, DisplayLimit: 600}
	cases := []struct {
		name  string
		other EvidenceID
	}{
		{"identical", base},
		{"another kind", EvidenceID{Kind: "memory", Namespace: "c/d", RecordID: "chunk-1",
			Version: 3, DisplayStart: 0, DisplayLimit: 600}},
		{"another revision", EvidenceID{Kind: "document", Namespace: "c/d", RecordID: "chunk-1",
			Version: 4, DisplayStart: 0, DisplayLimit: 600}},
		{"a longer display", EvidenceID{Kind: "document", Namespace: "c/d", RecordID: "chunk-1",
			Version: 3, DisplayStart: 0, DisplayLimit: 900}},
		{"an offset display", EvidenceID{Kind: "document", Namespace: "c/d", RecordID: "chunk-1",
			Version: 3, DisplayStart: 100, DisplayLimit: 600}},
	}
	for _, testCase := range cases {
		if got, want := testCase.other.Key() == base.Key(), testCase.name == "identical"; got != want {
			t.Fatalf("%s: key equality = %v, want %v\n%s\n%s", testCase.name, got, want, base.Key(), testCase.other.Key())
		}
	}
	// A separator cannot be forged from inside an identifier, or one namespace could
	// impersonate another and cancel its text.
	forge := EvidenceID{Kind: "document", Namespace: "c", RecordID: "d|3|0:600",
		Version: 3, DisplayStart: 0, DisplayLimit: 600}
	if forge.Key() == base.Key() {
		t.Fatalf("an identifier holding the separators collides: %s", forge.Key())
	}
}

func TestEvidenceChargesWholeUnitsAndRefusesWithoutSpending(t *testing.T) {
	evidence := NewEvidence(100)
	if evidence.Remaining() != 100 {
		t.Fatalf("a fresh allowance reports %d remaining", evidence.Remaining())
	}
	if !evidence.TryCharge(60) {
		t.Fatal("the first unit was refused")
	}
	if evidence.TryCharge(50) {
		t.Fatal("a unit larger than what is left was admitted")
	}
	if evidence.Used() != 60 {
		t.Fatalf("a refused charge spent %d", evidence.Used())
	}
	if !evidence.TryCharge(30) {
		t.Fatal("a refusal earlier in the list must not block a smaller unit after it")
	}
	if evidence.TryCharge(-1) {
		t.Fatal("a negative charge was accepted, which would hand back budget")
	}
}

func TestEvidenceTracksBodiesPerKey(t *testing.T) {
	evidence := NewEvidence(1000)
	key := EvidenceID{Kind: "document", RecordID: "a", Version: Unversioned}.Key()
	if evidence.BodySent(key) {
		t.Fatal("a fresh allowance claims a body was already sent")
	}
	evidence.MarkBodySent(key)
	if !evidence.BodySent(key) {
		t.Fatal("a marked body is not recognised")
	}
	other := EvidenceID{Kind: "document", RecordID: "b", Version: Unversioned}.Key()
	if evidence.BodySent(other) {
		t.Fatal("one record's body silenced another's")
	}
}

// Two allowances are two turns. Nothing in this type is process-wide: a shared map
// would let one learner's long search empty the next one's budget, and a restart would
// not be able to tell the difference between a cold turn and a stolen one.
func TestEvidenceAllowancesDoNotMeet(t *testing.T) {
	first, second := NewEvidence(50), NewEvidence(50)
	if !first.TryCharge(50) {
		t.Fatal("the first allowance refused its own budget")
	}
	if !second.TryCharge(50) {
		t.Fatal("a second turn inherited the first one's spending")
	}
	if second.BodySent("k") {
		t.Fatal("a second turn inherited the first one's deduplication set")
	}
}

func TestEvidenceConcurrentChargesNeverOverdraw(t *testing.T) {
	evidence := NewEvidence(1000)
	var wg sync.WaitGroup
	for round := 0; round < 64; round++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			evidence.TryCharge(40)
			evidence.MarkBodySent("k")
			_ = evidence.Remaining()
		}()
	}
	wg.Wait()
	if used := evidence.Used(); used != 1000 {
		t.Fatalf("64 charges of 40 against an allowance of 1000 spent %d", used)
	}
}

func TestNilAllowanceLeavesAToolToItsOwnCap(t *testing.T) {
	var evidence *Evidence
	if !evidence.TryCharge(1_000_000) {
		t.Fatal("a missing allowance refused work it has no opinion about")
	}
	if evidence.BodySent("k") {
		t.Fatal("a missing allowance claims to remember a body")
	}
	// A nil allowance must not be able to spend, either: Used on nil is zero rather
	// than a panic, so a caller outside an agent turn reads the same shape.
	if evidence.Used() != 0 || evidence.Remaining() != 0 {
		t.Fatalf("a nil allowance reports usage %d and remaining %d", evidence.Used(), evidence.Remaining())
	}
}

func TestEvidenceFromContextRoundTrips(t *testing.T) {
	evidence := NewEvidence(10)
	ctx := WithEvidence(context.Background(), evidence)
	if got := EvidenceFrom(ctx); got != evidence {
		t.Fatal("the allowance did not survive the context")
	}
	if got := EvidenceFrom(context.Background()); got != nil {
		t.Fatal("a context outside an agent turn reports an allowance")
	}
	if got := EvidenceFrom(nil); got != nil {
		t.Fatal("a nil context reported an allowance")
	}
}

func TestEvidenceReservationsShareOneCap(t *testing.T) {
	e := NewEvidence(100)
	r, ok := e.Reserve(80)
	if !ok {
		t.Fatal("reserve failed")
	}
	if e.TryCharge(30) {
		t.Fatal("direct charge ignored reserved tokens")
	}
	if got := e.Remaining(); got != 20 {
		t.Fatalf("remaining=%d want 20", got)
	}
	r.Release()
	if !e.TryCharge(30) {
		t.Fatal("release did not restore capacity")
	}
	r2, ok := e.Reserve(70)
	if !ok {
		t.Fatal("second reserve failed")
	}
	r2.Commit()
	if e.TryCharge(1) {
		t.Fatal("commit allowed overdraw")
	}
}

func TestReservationCommitReleaseConcurrentAndIdempotent(t *testing.T) {
	e := NewEvidence(100)
	r, ok := e.Reserve(80)
	if !ok {
		t.Fatal("reserve failed")
	}
	start := make(chan struct{})
	done := make(chan struct{}, 20)
	for i := 0; i < 10; i++ {
		go func() { <-start; r.Commit(); r.Release(); done <- struct{}{} }()
	}
	close(start)
	for i := 0; i < 10; i++ {
		<-done
	}
	if got := e.Used(); got != 80 {
		t.Fatalf("used=%d want 80", got)
	}
	if e.TryCharge(21) {
		t.Fatal("concurrent reservation overcharged")
	}
}

func TestEvidenceMixedReservationsAndChargesConserveBudget(t *testing.T) {
	e := NewEvidence(100)
	r1, ok := e.Reserve(40)
	if !ok {
		t.Fatal("r1")
	}
	r2, ok := e.Reserve(30)
	if !ok {
		t.Fatal("r2")
	}
	if e.TryCharge(31) {
		t.Fatal("charge exceeded reserved cap")
	}
	if got := e.Remaining(); got != 30 {
		t.Fatalf("remaining=%d", got)
	}
	r1.Commit()
	r1.Commit()
	r2.Release()
	r2.Release()
	if got := e.Used(); got != 40 {
		t.Fatalf("used=%d", got)
	}
	if got := e.Remaining(); got != 60 {
		t.Fatalf("remaining=%d", got)
	}
	if !e.TryCharge(60) || e.TryCharge(1) {
		t.Fatal("final budget conservation failed")
	}
}

func TestEvidenceMixedReservationsAndChargesConcurrent(t *testing.T) {
	e := NewEvidence(100)
	start := make(chan struct{})
	done := make(chan struct{}, 30)
	for i := 0; i < 10; i++ {
		go func() {
			<-start
			r, ok := e.Reserve(5)
			if ok {
				r.Commit()
			}
			done <- struct{}{}
		}()
	}
	for i := 0; i < 10; i++ {
		go func() { <-start; e.TryCharge(3); done <- struct{}{} }()
	}
	for i := 0; i < 10; i++ {
		go func() { <-start; _ = e.Remaining(); _ = e.Used(); done <- struct{}{} }()
	}
	close(start)
	for i := 0; i < 30; i++ {
		<-done
	}
	if e.Used() > 100 || e.Remaining() < 0 {
		t.Fatalf("budget invariant broken: used=%d remaining=%d", e.Used(), e.Remaining())
	}
}
