package agenttool

import (
	"strings"
	"testing"

	"GopherAI/model"
	serviceagent "GopherAI/service/agent"
)

// v3.2: a message this tool reads back is the only thing that makes a citation to it
// legal. These pin the boundary from both sides - nothing registers before a real read,
// and a read registers exactly what it proved, carrying the provenance the ledger reads
// back. Every case runs against the fake reader; none reaches a provider, a database or
// Docker.

func evidenceRun(grants ...serviceagent.HistoryMessageGrant) (serviceagent.RunContext, *serviceagent.MemoryEvidenceRegistry) {
	registry := serviceagent.NewMemoryEvidenceRegistry()
	run := historyRun(grants...)
	run.MemoryEvidence = registry
	return run, registry
}

func soleEntry(t *testing.T, registry *serviceagent.MemoryEvidenceRegistry) serviceagent.MemoryEvidenceEntry {
	t.Helper()
	entries := registry.Entries()
	if len(entries) != 1 {
		t.Fatalf("the registry holds %d entries, want exactly 1: %+v", len(entries), entries)
	}
	return entries[0]
}

// TestReadBackRegistersNothingUntilAMessageIsRead is the "did not call the tool" half of
// requirement 4: an id the window showed is worth nothing until it is actually read.
func TestReadBackRegistersNothingUntilAMessageIsRead(t *testing.T) {
	reader := &fakeHistoryReader{rows: map[uint]model.Message{
		11: userMessage(11, historyRunSession, "那就用 Protobuf"),
	}}
	run, registry := evidenceRun(summaryGrant(11, historyRunSession))
	if tools := newHistoryTools(t, reader, run); len(tools) != 1 {
		t.Fatalf("the arm was offered %d tools, want 1", len(tools))
	}
	if len(registry.Entries()) != 0 {
		t.Fatalf("building the tool registered evidence before any read: %+v", registry.Entries())
	}
	outcome := serviceagent.CheckMemoryCitations(registry, "我当时说要用 Protobuf。[E1]")
	if len(outcome.Invalid) != 1 || outcome.Invalid[0] != "E1" {
		t.Fatalf("an invented reference on an empty registry was not Invalid: %+v", outcome)
	}
}

// TestSummaryReadBackIssuesSummaryProvenanceEvidence covers requirements 1 and 5: a
// summary read is citable, is episodic (the only kind that supports a history claim), and
// says in its provenance that it came from the summary.
func TestSummaryReadBackIssuesSummaryProvenanceEvidence(t *testing.T) {
	reader := &fakeHistoryReader{rows: map[uint]model.Message{
		11: userMessage(11, historyRunSession, "那就用 Protobuf"),
	}}
	run, registry := evidenceRun(summaryGrant(11, historyRunSession))
	tools := newHistoryTools(t, reader, run)

	out := invokeRead(t, tools, 11)
	if len(out.Messages) != 1 || out.Messages[0].Reference == "" {
		t.Fatalf("the read back a message without a citation label: %+v", out.Messages)
	}
	entry := soleEntry(t, registry)
	if entry.Ref != out.Messages[0].Reference {
		t.Errorf("the tool showed %q but the registry holds %q", out.Messages[0].Reference, entry.Ref)
	}
	if entry.Kind != serviceagent.MemoryEvidenceEpisodic {
		t.Errorf("a history read registered kind %q, want episodic", entry.Kind)
	}
	if entry.Source != serviceagent.HistorySourceSummary {
		t.Errorf("summary provenance = %q, want %q", entry.Source, serviceagent.HistorySourceSummary)
	}
	if entry.MessageID != 11 || entry.Owner != 7 || entry.Version != 3 {
		t.Errorf("provenance fields wrong: %+v", entry)
	}
	outcome := serviceagent.CheckMemoryCitations(registry, "我说要改用 Protobuf。["+entry.Ref+"]")
	if len(outcome.Invalid) != 0 || len(outcome.KindMismatch) != 0 || outcome.NoEvidenceButCited {
		t.Fatalf("a citation to a registered summary read was refused: %+v", outcome)
	}
}

// TestEpisodeReadBackIssuesEpisodeProvenanceEvidence is requirement 2: the same shape for
// an episode read, and the one place it differs is the provenance string.
func TestEpisodeReadBackIssuesEpisodeProvenanceEvidence(t *testing.T) {
	reader := &fakeHistoryReader{rows: map[uint]model.Message{
		21: userMessage(21, historyEpisodeSession, "我在上一个会话里说过的原话"),
	}}
	run, registry := evidenceRun(episodeGrant(21, historyEpisodeSession))
	tools := newHistoryTools(t, reader, run)

	out := invokeRead(t, tools, 21)
	if len(out.Messages) != 1 || out.Messages[0].Reference == "" {
		t.Fatalf("the episode read back a message without a citation label: %+v", out.Messages)
	}
	entry := soleEntry(t, registry)
	if entry.Source != serviceagent.HistorySourceEpisode {
		t.Errorf("episode provenance = %q, want %q", entry.Source, serviceagent.HistorySourceEpisode)
	}
	if entry.Version != uint64(historyEpisodeVersion) {
		t.Errorf("episode read carried version %d, want %d", entry.Version, historyEpisodeVersion)
	}
}

// TestMixedReadBackCarriesBothProvenances is requirement 6 and 5 together: one message an
// id was drawn from both a summary and an episode yields one reference whose provenance
// names both, so a mixed answer can cite it and the ledger can still tell the sources.
func TestMixedReadBackCarriesBothProvenances(t *testing.T) {
	shared := serviceagent.HistoryMessageGrant{
		MessageID: 31, SourceKind: serviceagent.HistorySourceEpisode, SourceSessionID: historyRunSession,
		EpisodeID: "episode-1", EpisodeVersion: historyEpisodeVersion, EpisodeWatermark: historyEpisodeWatermark,
	}
	reader := &fakeHistoryReader{rows: map[uint]model.Message{
		31: userMessage(31, historyRunSession, "那周我们把方案定下来了"),
	}}
	run, registry := evidenceRun(summaryGrant(31, historyRunSession), shared)
	tools := newHistoryTools(t, reader, run)

	out := invokeRead(t, tools, 31)
	if len(out.Messages) != 1 {
		t.Fatalf("a doubly-granted id read back %d messages, want 1: %+v", len(out.Messages), out.Messages)
	}
	entry := soleEntry(t, registry)
	if !strings.Contains(entry.Source, serviceagent.HistorySourceSummary) ||
		!strings.Contains(entry.Source, serviceagent.HistorySourceEpisode) {
		t.Errorf("a mixed read must name both provenances, got %q", entry.Source)
	}
	if out.Messages[0].Reference != entry.Ref {
		t.Errorf("the model was shown %q but the registry holds %q", out.Messages[0].Reference, entry.Ref)
	}
	outcome := serviceagent.CheckMemoryCitations(registry, "那周我们把方案定下来了。["+entry.Ref+"]")
	if len(outcome.Invalid) != 0 || len(outcome.KindMismatch) != 0 || outcome.NoEvidenceButCited {
		t.Fatalf("a citation to a doubly-granted read was refused: %+v", outcome)
	}
}

// TestRefusedAndUngrantedIDsNeverRegister is requirements 4 and 5: an id nobody granted,
// and a granted id the store will not hand back, produce no reference.
func TestRefusedAndUngrantedIDsNeverRegister(t *testing.T) {
	reader := &fakeHistoryReader{rows: map[uint]model.Message{
		11: userMessage(11, historyEpisodeSession, "别的会话里同号的一行"),
	}}
	// The grant says session-current, the row lives in session-episode: the reader's own
	// session scoping must refuse it, so no evidence exists to register.
	run, registry := evidenceRun(summaryGrant(11, historyRunSession))
	tools := newHistoryTools(t, reader, run)

	out := invokeRead(t, tools, 11, 42)
	if len(out.Messages) != 0 {
		t.Fatalf("a cross-session row and an ungranted id were both delivered: %+v", out.Messages)
	}
	if len(registry.Entries()) != 0 {
		t.Fatalf("refused reads registered %d references: %+v", len(registry.Entries()), registry.Entries())
	}
	if len(out.Refused) == 0 {
		t.Error("a refused read must still be reported as refused")
	}
}

// TestCitationRepairOnlySeesThisRunsRegisteredRefs is requirement 7: the list handed to a
// repair is exactly what this run issued, so a repair cannot be shown a reference it is
// then punished for not knowing.
func TestCitationRepairOnlySeesThisRunsRegisteredRefs(t *testing.T) {
	reader := &fakeHistoryReader{rows: map[uint]model.Message{
		11: userMessage(11, historyRunSession, "那就用 Protobuf"),
	}}
	run, registry := evidenceRun(summaryGrant(11, historyRunSession))
	tools := newHistoryTools(t, reader, run)
	invokeRead(t, tools, 11)

	summary := registry.LegalRefSummary()
	entry := soleEntry(t, registry)
	if !strings.Contains(summary, "["+entry.Ref+"]") {
		t.Errorf("the repair list omitted the reference this run issued: %q", summary)
	}
	if strings.Contains(summary, "E99") {
		t.Errorf("the repair list carried a reference nobody issued: %q", summary)
	}
}
