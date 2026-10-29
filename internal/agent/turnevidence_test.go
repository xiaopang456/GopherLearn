package agent

import (
	"errors"
	"strings"
	"testing"

	"github.com/xiaopang456/GopherLearn/internal/contextwindow"
)

// The citation a search issued in one round keeps its number in a later round,
// so an answer that cites [2] cites the same passage it named when the number
// was shown. A per-call renumbering would make a multi-round answer cite
// whichever passage happened to sort second that call.
func TestTurnEvidenceCitationsAreStableAcrossCalls(t *testing.T) {
	evidence := NewTurnEvidence(6500)
	evidence.AttachBudget(contextwindow.NewEvidence(6500))
	first := evidence.CitationFor("col/doc:v1:chunk-a")
	again := evidence.CitationFor("col/doc:v1:chunk-a")
	second := evidence.CitationFor("col/doc:v1:chunk-b")
	if first != 1 || again != 1 || second != 2 {
		t.Fatalf("citations %d/%d/%d, want 1/1/2", first, again, second)
	}
	if !evidence.HasCitation("col/doc:v1:chunk-a") {
		t.Fatal("an issued citation was forgotten")
	}
}

// A repeated display unit keeps its citation and drops its body, and only a
// unit whose text actually went out may be marked as sent.
func TestTurnEvidenceBodyDedup(t *testing.T) {
	evidence := NewTurnEvidence(6500)
	evidence.AttachBudget(contextwindow.NewEvidence(6500))
	key := "document|\"col/doc\"|\"chunk-a\"|1|0:120"
	if evidence.BodySent(key) {
		t.Fatal("nothing was sent yet")
	}
	evidence.MarkBodySent(key)
	if !evidence.BodySent(key) {
		t.Fatal("a marked body was forgotten")
	}
	if evidence.BodySent(key + "-other") {
		t.Fatal("an unmarked body reads as sent")
	}
}

// Only versions search surfaced may be read back: the tool gates on this, so a
// model that invents a document id finds no eligibility behind it.
func TestTurnEvidenceDocumentAllowance(t *testing.T) {
	evidence := NewTurnEvidence(6500)
	if evidence.DocumentAllowed("doc-1", 3) {
		t.Fatal("an unshown document is eligible")
	}
	evidence.AllowDocument("doc-1", 3)
	if !evidence.DocumentAllowed("doc-1", 3) {
		t.Fatal("a surfaced document lost its eligibility")
	}
	if evidence.DocumentAllowed("doc-1", 4) {
		t.Fatal("a different version of a surfaced document is eligible")
	}
}

// The letter declares the kind, so a wrong letter is a kind mismatch rather
// than an unknown reference - the distinction that catches a preference being
// passed off as a past decision.
func TestTurnEvidenceMemoryRefsResolveAndMismatch(t *testing.T) {
	evidence := NewTurnEvidence(6500)
	ref := evidence.Register(MemoryEvidenceEntry{Kind: MemoryEvidenceGlobal,
		InternalID: "memory-1", Version: 2, Subject: "偏好"})
	if ref != "G1" {
		t.Fatalf("ref %q, want G1", ref)
	}
	entry, err := evidence.Lookup("[G1]")
	if err != nil || entry.InternalID != "memory-1" {
		t.Fatalf("lookup = %v, %v", entry, err)
	}
	if _, err := evidence.Lookup("[E1]"); !errors.Is(err, ErrMemoryKindMismatch) {
		t.Fatalf("an E citation of a G record = %v, want a kind mismatch", err)
	}
	if _, err := evidence.Lookup("[G2]"); !errors.Is(err, ErrUnknownMemoryRef) {
		t.Fatalf("an unissued number = %v, want unknown reference", err)
	}
	if _, err := evidence.Lookup("[G0]"); !errors.Is(err, ErrUnknownMemoryRef) {
		t.Fatalf("a zero number = %v, want unknown reference", err)
	}
}

// Only topic and episodic kinds count as long-term evidence, and a nil or
// empty registry answers no.
func TestTurnEvidenceLongTermPredicate(t *testing.T) {
	evidence := NewTurnEvidence(6500)
	if evidence.HasLongTermEvidence() {
		t.Fatal("an empty registry holds long-term evidence")
	}
	evidence.Register(MemoryEvidenceEntry{Kind: MemoryEvidenceGlobal, Subject: "偏好"})
	if evidence.HasLongTermEvidence() {
		t.Fatal("a global record is not long-term evidence")
	}
	evidence.Register(MemoryEvidenceEntry{Kind: MemoryEvidenceEpisodic, Subject: "上次会话"})
	if !evidence.HasLongTermEvidence() {
		t.Fatal("an episodic record is long-term evidence")
	}
	if !evidence.Attempted() == false {
		t.Fatal("nothing attempted yet")
	}
	evidence.RecordMemoryAttempt()
	if !evidence.Attempted() {
		t.Fatal("a recorded attempt was forgotten")
	}
}

// The legal-ref summary lists every issued reference with its kind and
// subject, and nothing else: it is what a repair call is shown.
func TestTurnEvidenceLegalRefSummary(t *testing.T) {
	evidence := NewTurnEvidence(6500)
	if summary := evidence.LegalRefSummary(); !strings.Contains(summary, "no memory evidence") {
		t.Fatalf("an empty summary = %q", summary)
	}
	evidence.Register(MemoryEvidenceEntry{Kind: MemoryEvidenceTopic, InternalID: "m-2", Subject: "TCP 理解"})
	summary := evidence.LegalRefSummary()
	if !strings.Contains(summary, "[S1]") || !strings.Contains(summary, "topic_semantic") ||
		!strings.Contains(summary, "TCP 理解") {
		t.Fatalf("the summary lost a field:\n%s", summary)
	}
	if strings.Contains(summary, "m-2") {
		t.Fatal("the summary leaked an internal id the model must not see")
	}
}

// The token charge on the document half mirrors the allowance the tools spend
// against: a failed charge spends nothing.
func TestTurnEvidenceTokenCharge(t *testing.T) {
	evidence := NewTurnEvidence(100)
	evidence.AttachBudget(contextwindow.NewEvidence(100))
	if !evidence.ChargeTokens(60) {
		t.Fatal("a charge under the ceiling was refused")
	}
	if evidence.ChargeTokens(41) {
		t.Fatal("a charge past the ceiling was accepted")
	}
	if got := evidence.RemainingTokens(); got != 40 {
		t.Fatalf("remaining %d, want 40 - the failed charge spent nothing", got)
	}
	if evidence.UsedTokens() != 60 {
		t.Fatalf("used %d, want 60", evidence.UsedTokens())
	}
}

// A nil registry answers without failing: outside an agent turn there is no
// registry, and a tool must not answer "not allowed" because nobody installed
// one.
func TestTurnEvidenceNilIsInert(t *testing.T) {
	var evidence *TurnEvidence
	if evidence.CitationFor("k") != 0 {
		t.Fatal("a nil registry issued a citation")
	}
	if evidence.DocumentAllowed("d", 1) {
		t.Fatal("a nil registry granted eligibility")
	}
	if evidence.BodySent("k") {
		t.Fatal("a nil registry remembers bodies")
	}
	if _, err := evidence.Lookup("[G1]"); !errors.Is(err, ErrUnknownMemoryRef) {
		t.Fatalf("a nil registry lookup = %v", err)
	}
}
