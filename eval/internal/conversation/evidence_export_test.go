package conversation

// The evidence a run leaves behind when the sandbox that produced it is gone.
//
// The counters say how many batches failed and how many candidates the board
// refused. They cannot say which entry was refused, on what grounds, or what
// the model actually wrote, and a reader who has only the counters has to
// guess. The evidence is what the run keeps so that the guess is not needed:
// the model's own answer, the segment body the program made of it, and the
// candidates that were refused.
//
// It is the learner's own text, so it is kept only when the deployment's
// debug dump is on, and it never reaches the result file.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"GopherAI/config"
	"GopherAI/model"
	agent "GopherAI/service/agent"
)

// evidenceLearnerText is what the learner said in the batch below.
//
// It is written here rather than read from a corpus: the evidence is the
// learner's own words, and a test that carried a real conversation would put
// a real conversation into a repository.
const evidenceLearnerText = "阅读计划以后每次读 41 页"

// evidenceProposal is an answer whose learner claim does not hold.
//
// The model quotes words the learner did not say, so the board refuses the
// entry. The refusal is what the evidence has to show: which entry, which
// check, and on which message.
func evidenceProposal() string {
	return `{"schema_version":1,"segment":{"topic":"阅读计划","points":[` +
		`{"text":"学习者把阅读计划改成了每次读 41 页","evidence_message_ids":[95]}]},` +
		`"whiteboard":{"schema_version":2,"constraints":[` +
		`{"key":"pages","text":"阅读计划每次读 41 页","value":"41 页/次","source_message_ids":[95],` +
		`"learner_claim":{"message_id":95,"quote":"阅读计划以后每次读55页","subject":"阅读计划",` +
		`"attribute":"页","value":"41页","stance":"decided","update":"yes"}}]}}`
}

// evidenceBatch is the batch the answer above was written for.
func evidenceBatch() []model.Message {
	return []model.Message{
		{ID: 95, IsUser: true, Content: evidenceLearnerText},
		{ID: 96, IsUser: false, Content: "往后按新的页数走。"},
	}
}

// evidenceGenerator builds the product's own generator around a stand-in
// model.
//
// The generator is the product's: a driver that called its own copy would be
// testing its own copy, and the refusal the evidence has to show is decided
// inside the board engine. The recorder is the model's wrapper, so the
// generator sees the recorder and the recorder sees the stand-in - the same
// order the read side uses.
func evidenceGenerator(t *testing.T, recorder *recordingChatModel) *agent.ModelSegmentGenerator {
	t.Helper()
	generator, err := agent.NewModelSegmentGenerator(recorder, 8000, 2000)
	if err != nil {
		t.Fatalf("build the segment generator: %v", err)
	}
	// The board is written in the version that carries the claim contract,
	// which is the version the mode that records the assistant's products
	// writes.
	generator.UseAssistantContent(true)
	return generator
}

// evidenceInput is what one batch is summarized from.
func evidenceInput() agent.SegmentGenerationInput {
	return agent.SegmentGenerationInput{
		Messages:       evidenceBatch(),
		FirstMessageID: 95,
		LastMessageID:  96,
	}
}

// The evidence of an attempt that produced a segment carries what the model
// wrote, what the program made of it, and which candidates were refused.
//
// Without the answer and the body, a refusal is a number: a reader cannot
// tell which entry was refused or what the model was trying to say.
func TestTheEvidenceOfAnAttemptCarriesTheAnswerTheBodyAndTheRefusedCandidates(t *testing.T) {
	answer := evidenceProposal()
	recorder := &recordingChatModel{inner: &answerStub{content: answer}}
	generator := evidenceGenerator(t, recorder)
	outcome := batchOutcome{}
	evidence := newBatchEvidenceCollector(config.AgentConfig{SummarySegmentDebugDump: true})

	result, err := runSegmentBatchAttempts(context.Background(), generator, recorder,
		evidenceInput(), 1, 1, 0, &outcome, evidence)
	if err != nil {
		t.Fatalf("run the batch: %v", err)
	}
	records := evidence.set().Records
	if len(records) != 1 {
		t.Fatalf("the run kept %d records, want 1", len(records))
	}
	record := records[0]
	if record.Outcome != EvidenceOutcomeGenerated {
		t.Fatalf("outcome = %q, want %q", record.Outcome, EvidenceOutcomeGenerated)
	}
	if record.Batch != 1 || record.Attempt != 1 {
		t.Fatalf("the record is not the first attempt at the first batch: %+v", record)
	}
	if record.StartMessageID != 95 || record.EndMessageID != 96 {
		t.Fatalf("the record does not say which stretch it covers: %+v", record)
	}
	if record.RawOutput != answer {
		t.Fatalf("the raw output is not the model's own answer:\n%q", record.RawOutput)
	}
	if record.SegmentBody != result.SegmentContent {
		t.Fatalf("the body is not the segment the program made:\n%q", record.SegmentBody)
	}
	if record.SegmentBody == "" {
		t.Fatal("the record carries no segment body")
	}
	if len(record.Claims) != 1 {
		t.Fatalf("the refused candidates = %d, want 1: %+v", len(record.Claims), record.Claims)
	}
	claim := record.Claims[0]
	if claim.Key != "pages" {
		t.Fatalf("the refused candidate is not the entry the model named: %+v", claim)
	}
	if claim.MessageID != 95 {
		t.Fatalf("the refused candidate does not say which message it rested on: %+v", claim)
	}
	if claim.Verdict != "rejected" {
		t.Fatalf("verdict = %q, want the refused one", claim.Verdict)
	}
	// The reason is one of the product's fixed reasons, so a candidate never
	// carries free text into a counter.
	if !model.ValidSessionWhiteboardAnchorReason(claim.Reason) {
		t.Fatalf("reason = %q, want one of the product's own reasons", claim.Reason)
	}
}

// A failed attempt is filed with the whole answer, not with the head of it.
//
// The head cannot tell a truncated answer from a wrongly shaped one, and the
// difference decides the fix. The excerpt the report keeps is still the
// excerpt: the two are not the same thing.
func TestTheEvidenceOfAFailedAttemptCarriesTheWholeAnswer(t *testing.T) {
	answer := strings.Repeat("答", excerptForReport+50)
	recorder := &recordingChatModel{inner: &answerStub{content: answer}}
	generator := evidenceGenerator(t, recorder)
	outcome := batchOutcome{}
	evidence := newBatchEvidenceCollector(config.AgentConfig{SummarySegmentDebugDump: true})

	if _, err := runSegmentBatchAttempts(context.Background(), generator, recorder,
		evidenceInput(), 1, 1, 0, &outcome, evidence); err == nil {
		t.Fatal("an answer that is not a document did not fail the batch")
	}
	records := evidence.set().Records
	if len(records) != 1 {
		t.Fatalf("the run kept %d records, want 1", len(records))
	}
	record := records[0]
	if record.Outcome != EvidenceOutcomeFailed {
		t.Fatalf("outcome = %q, want %q", record.Outcome, EvidenceOutcomeFailed)
	}
	if record.Reason == "" {
		t.Fatal("the failed attempt says nothing about why it failed")
	}
	if record.RawOutput != answer {
		t.Fatalf("the evidence holds %d runes, want the whole answer (%d)",
			len([]rune(record.RawOutput)), len([]rune(answer)))
	}
	// The excerpt the report keeps is still bounded: the two are not the
	// same thing.
	if got := recorder.excerpt(); len([]rune(got)) > excerptForReport+4 {
		t.Fatalf("the excerpt grew to %d runes, want it bounded", len([]rune(got)))
	}
	// A failed attempt has no segment body and no candidates: the program
	// never got as far as building them.
	if record.SegmentBody != "" {
		t.Fatalf("a failed attempt carries a segment body: %q", record.SegmentBody)
	}
	if len(record.Claims) != 0 {
		t.Fatalf("a failed attempt carries candidates: %+v", record.Claims)
	}
}

// The evidence of a run that may not keep it is not kept at all.
//
// The switch is the deployment's summary segment debug dump, and it is off by
// default: the evidence is the learner's own words, and a run nobody is going
// to read has no business keeping them.
func TestTheEvidenceIsNotKeptWhenTheDebugDumpIsOff(t *testing.T) {
	collector := newBatchEvidenceCollector(config.AgentConfig{SummarySegmentDebugDump: false})
	collector.add(BatchEvidence{Batch: 1, Attempt: 1, Outcome: EvidenceOutcomeGenerated})
	if set := collector.set(); set != nil {
		t.Fatalf("the run kept an evidence set although the dump was off: %+v", set)
	}
}

// The evidence is written next to the run's own files, one file per attempt,
// and the result file does not carry it.
func TestTheEvidenceIsWrittenNextToTheRunAndNotIntoTheResult(t *testing.T) {
	dir := t.TempDir()
	read := LayeredRead{
		Scenario: "evidence",
		BatchEvidence: &BatchEvidenceSet{
			Enabled: true,
			Records: []BatchEvidence{{
				Batch: 1, Attempt: 1, Outcome: EvidenceOutcomeGenerated,
				StartMessageID: 95, EndMessageID: 96,
				RawOutput: "the model's own answer", SegmentBody: "the segment body",
				Claims: []agent.WhiteboardClaimCandidate{{
					Key: "pages", Reason: model.SessionWhiteboardAnchorQuoteNotInMessage,
					MessageID: 95, Verdict: "rejected",
				}},
			}},
		},
	}
	if err := WriteLayeredReadFiles(dir, read); err != nil {
		t.Fatalf("write the run's files: %v", err)
	}

	path := filepath.Join(dir, EvidenceDirName, "batch-01-attempt-01.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the evidence file: %v", err)
	}
	var written map[string]any
	if err = json.Unmarshal(raw, &written); err != nil {
		t.Fatalf("the evidence file is not a JSON object: %v", err)
	}
	for _, field := range []string{"batch", "attempt", "outcome", "reason", "start_message_id",
		"end_message_id", "raw_output", "segment_body", "claims"} {
		if _, ok := written[field]; !ok {
			t.Fatalf("the evidence file has no %q: %v", field, written)
		}
	}
	if written["raw_output"] != "the model's own answer" {
		t.Fatalf("raw_output = %v, want the model's own answer", written["raw_output"])
	}

	// A candidate carries the four fields the product gives it and nothing
	// else: a candidate that carried the learner's own words would put them
	// into a counter.
	claims, ok := written["claims"].([]any)
	if !ok || len(claims) != 1 {
		t.Fatalf("claims = %v, want one candidate", written["claims"])
	}
	candidate, ok := claims[0].(map[string]any)
	if !ok {
		t.Fatalf("the candidate is not an object: %v", claims[0])
	}
	if len(candidate) != 4 {
		t.Fatalf("the candidate carries %d fields, want the four of the claim: %v", len(candidate), candidate)
	}
	for _, field := range []string{"key", "reason", "message_id", "verdict"} {
		if _, ok = candidate[field]; !ok {
			t.Fatalf("the candidate has no %q: %v", field, candidate)
		}
	}

	// The result file is read by people who do not need the learner's words,
	// so it does not carry them.
	result, err := os.ReadFile(filepath.Join(dir, "read.json"))
	if err != nil {
		t.Fatalf("read the result file: %v", err)
	}
	if strings.Contains(string(result), "the model's own answer") {
		t.Fatal("the result file carries the model's raw answer")
	}
}

// A run whose switch was off writes no evidence at all.
func TestNoEvidenceIsWrittenWhenTheDebugDumpIsOff(t *testing.T) {
	dir := t.TempDir()
	read := LayeredRead{Scenario: "evidence-off"}
	if err := WriteLayeredReadFiles(dir, read); err != nil {
		t.Fatalf("write the run's files: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, EvidenceDirName)); !os.IsNotExist(err) {
		t.Fatalf("the evidence directory exists although the dump was off: %v", err)
	}
}

// The evidence of a run that was allowed to keep it is written even when the
// run kept nothing: the directory itself says the switch was on.
func TestTheEvidenceDirectoryIsWrittenWhenTheDumpWasOn(t *testing.T) {
	dir := t.TempDir()
	read := LayeredRead{Scenario: "evidence-on", BatchEvidence: &BatchEvidenceSet{Enabled: true}}
	if err := WriteLayeredReadFiles(dir, read); err != nil {
		t.Fatalf("write the run's files: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, EvidenceDirName))
	if err != nil {
		t.Fatalf("the evidence directory is not there although the dump was on: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("the evidence path is not a directory")
	}
}

// A write that fails is reported rather than swallowed: evidence that was not
// written is a run that cannot be read.
func TestAFailedEvidenceWriteIsReported(t *testing.T) {
	dir := t.TempDir()
	// A file where the evidence directory has to be: it cannot be created.
	if err := os.WriteFile(filepath.Join(dir, EvidenceDirName), []byte("in the way"), 0o600); err != nil {
		t.Fatalf("write the blocking file: %v", err)
	}
	err := WriteBatchEvidence(dir, []BatchEvidence{{Batch: 1, Attempt: 1, Outcome: EvidenceOutcomeGenerated}})
	if err == nil {
		t.Fatal("the write that could not happen was reported as a success")
	}
}
