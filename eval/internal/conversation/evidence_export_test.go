package conversation

// The evidence a run leaves behind when the sandbox that produced it is gone.
//
// The counters say how many batches failed and how many candidates the board
// refused. They cannot say which entry was refused, on what grounds, or what
// the model actually wrote, and a reader who has only the counters has to
// guess. The evidence is what the run keeps so that the guess is not needed:
// the model's own answers, the segment body the program made of them, and the
// candidates that were refused.
//
// Two things about it are decided here.
//
// The first is that the evidence is per **model call**, not per attempt. A
// batch is one call to write the summary and up to two more to write it again
// when the first does not fit, and an attempt that failed is retried
// outside: filing the attempt under one answer would lose every answer but
// the last one.
//
// The second is that the evidence is written **before the sandbox goes**. The
// sandbox holds the model's own answers, and they are the only copy of what
// the run produced; a run that tore the sandbox down first has thrown them
// away. A write that fails therefore means the sandbox is kept.
//
// It is the learner's own text, so it is kept only when the deployment's
// debug dump is on, and it never reaches the result file.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"

	"GopherAI/config"
	"GopherAI/model"
	agent "GopherAI/service/agent"
	modelcomponent "github.com/cloudwego/eino/components/model"
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

// evidenceShortProposal is an answer that fits the window.
func evidenceShortProposal() string {
	return `{"schema_version":1,"segment":{"topic":"阅读计划","points":[` +
		`{"text":"学习者把阅读计划改成了每次读 41 页","evidence_message_ids":[95]}]}}`
}

// evidenceLongProposal is an answer whose summary does not fit the window.
//
// The points are long enough that the segment cannot be carried, so the
// product asks for it again.
func evidenceLongProposal() string {
	points := make([]string, 0, 10)
	for index := 0; index < 10; index++ {
		points = append(points, `{"text":"`+strings.Repeat("学习者把这一条说清楚了", 4)+
			`","evidence_message_ids":[95]}`)
	}
	return `{"schema_version":1,"segment":{"topic":"阅读计划","points":[` +
		strings.Join(points, ",") + `]},` +
		`"whiteboard":{"schema_version":2,"constraints":[` +
		`{"key":"pages","text":"阅读计划每次读 41 页","value":"41 页/次","source_message_ids":[95],` +
		`"learner_claim":{"message_id":95,"quote":"阅读计划以后每次读55页","subject":"阅读计划",` +
		`"attribute":"页","value":"41页","stance":"decided","update":"yes"}}]}}`
}

// evidenceBatch is the batch the answers above were written for.
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

// scriptedModel answers with the given bodies, one per call.
//
// The last body is repeated when the model is asked more times than there are
// bodies, so a test can say "the first answer is this and every answer after
// it is that".
type scriptedModel struct {
	bodies []string
	calls  int
}

func (m *scriptedModel) WithTools([]*schema.ToolInfo) (modelcomponent.ToolCallingChatModel, error) {
	return m, nil
}

func (m *scriptedModel) Generate(context.Context, []*schema.Message,
	...modelcomponent.Option) (*schema.Message, error) {
	index := m.calls
	m.calls++
	if index >= len(m.bodies) {
		index = len(m.bodies) - 1
	}
	if index < 0 {
		return nil, errors.New("no answer to give")
	}
	return &schema.Message{Role: schema.Assistant, Content: m.bodies[index]}, nil
}

func (m *scriptedModel) Stream(context.Context, []*schema.Message,
	...modelcomponent.Option) (*schema.StreamReader[*schema.Message], error) {
	return nil, nil
}

// readEvidenceAttempt reads one attempt file.
func readEvidenceAttempt(t *testing.T, dir string, batch, attempt int) map[string]any {
	t.Helper()
	path := filepath.Join(dir, EvidenceDirName,
		"batch-"+twoDigits(batch)+"-attempt-"+twoDigits(attempt)+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the evidence of batch %d attempt %d: %v", batch, attempt, err)
	}
	var written map[string]any
	if err = json.Unmarshal(raw, &written); err != nil {
		t.Fatalf("the evidence file is not a JSON object: %v", err)
	}
	return written
}

// twoDigits is the number a file name is built from.
func twoDigits(value int) string {
	if value < 10 {
		return "0" + string(rune('0'+value))
	}
	return string(rune('0'+value/10)) + string(rune('0'+value%10))
}

// The evidence of an attempt carries what the model wrote, what the program
// made of it, and which candidates were refused.
//
// Without the answer and the body, a refusal is a number: a reader cannot
// tell which entry was refused or what the model was trying to say.
func TestTheEvidenceOfAnAttemptCarriesTheCallsTheBodyAndTheRefusedCandidates(t *testing.T) {
	answer := evidenceProposal()
	recorder := &recordingChatModel{inner: &scriptedModel{bodies: []string{answer}}}
	generator := evidenceGenerator(t, recorder)
	outcome := batchOutcome{}
	evidence := newBatchEvidenceCollector(config.AgentConfig{SummarySegmentDebugDump: true}, "run-1")

	result, err := runSegmentBatchAttempts(context.Background(), generator, recorder,
		evidenceInput(), 1, 1, 0, &outcome, evidence, model.SessionWhiteboardDocument{})
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
	if record.SegmentBody != result.SegmentContent {
		t.Fatalf("the body is not the segment the program made:\n%q", record.SegmentBody)
	}
	if record.SegmentBody == "" {
		t.Fatal("the record carries no segment body")
	}
	if len(record.Calls) != 1 {
		t.Fatalf("the attempt made %d calls, want 1", len(record.Calls))
	}
	call := record.Calls[0]
	if call.Kind != EvidenceCallSummary {
		t.Fatalf("the call's kind = %q, want the one that writes the summary", call.Kind)
	}
	if call.Outcome != EvidenceOutcomeOK {
		t.Fatalf("the call's outcome = %q, want the one that succeeded", call.Outcome)
	}
	if call.RawOutput != answer {
		t.Fatalf("the call does not carry the model's own answer:\n%q", call.RawOutput)
	}
	if len(record.Entries) != 1 {
		t.Fatalf("the entries = %d, want 1: %+v", len(record.Entries), record.Entries)
	}
	entry := record.Entries[0]
	if entry.Key != "pages" {
		t.Fatalf("the entry is not the one the model named: %+v", entry)
	}
	if entry.ClaimMessageID != 95 {
		t.Fatalf("the entry does not say which message the claim rested on: %+v", entry)
	}
	if entry.ClaimVerdict != "rejected" {
		t.Fatalf("verdict = %q, want the refused one", entry.ClaimVerdict)
	}
	// The reason is one of the product's fixed reasons, so a record never
	// carries free text into a counter.
	if !model.ValidSessionWhiteboardAnchorReason(entry.ClaimReason) {
		t.Fatalf("reason = %q, want one of the product's own reasons", entry.ClaimReason)
	}
	// The claim and the entry are two things, and a reader has to be able to
	// see both: a claim that holds is not an entry on the board, and an
	// entry that was refused may still have left the words findable.
	if entry.Outcome == "" {
		t.Fatalf("the entry does not say what became of it: %+v", entry)
	}
	if entry.OutcomeReason == "" {
		t.Fatalf("the entry does not say why it ended that way: %+v", entry)
	}
}

// The first attempt fails and the rewrite after it succeeds: two calls, two
// records, two bodies.
//
// The point is that the two answers are not the same answer, and an attempt
// filed under one of them loses the other.
func TestTheFirstFailureAndTheRewriteAfterItAreTwoCalls(t *testing.T) {
	recorder := &recordingChatModel{inner: &scriptedModel{bodies: []string{
		evidenceLongProposal(), evidenceShortProposal(),
	}}}
	generator := evidenceGenerator(t, recorder)
	outcome := batchOutcome{}
	evidence := newBatchEvidenceCollector(config.AgentConfig{SummarySegmentDebugDump: true}, "run-1")

	if _, err := runSegmentBatchAttempts(context.Background(), generator, recorder,
		evidenceInput(), 1, 1, 0, &outcome, evidence, model.SessionWhiteboardDocument{}); err != nil {
		t.Fatalf("run the batch: %v", err)
	}
	records := evidence.set().Records
	if len(records) != 1 {
		t.Fatalf("the run kept %d records, want 1", len(records))
	}
	calls := records[0].Calls
	if len(calls) < 2 {
		t.Fatalf("the attempt made %d calls, want at least 2", len(calls))
	}
	if calls[0].Kind != EvidenceCallSummary {
		t.Fatalf("the first call's kind = %q, want the one that writes the summary", calls[0].Kind)
	}
	if calls[0].RawOutput != evidenceLongProposal() {
		t.Fatal("the first call does not carry the first answer")
	}
	if calls[1].Kind != EvidenceCallRewrite {
		t.Fatalf("the second call's kind = %q, want the rewrite", calls[1].Kind)
	}
	if calls[1].RawOutput != evidenceShortProposal() {
		t.Fatal("the second call does not carry the second answer")
	}
	for index, call := range calls {
		if call.Index != index+1 {
			t.Fatalf("call %d is numbered %d", index, call.Index)
		}
	}
}

// Every answer that did not fit is kept, and the attempt is marked failed.
func TestTheConsecutiveRewritesThatFailedAreAllKept(t *testing.T) {
	recorder := &recordingChatModel{inner: &scriptedModel{bodies: []string{evidenceLongProposal()}}}
	generator := evidenceGenerator(t, recorder)
	outcome := batchOutcome{}
	evidence := newBatchEvidenceCollector(config.AgentConfig{SummarySegmentDebugDump: true}, "run-1")

	if _, err := runSegmentBatchAttempts(context.Background(), generator, recorder,
		evidenceInput(), 1, 1, 0, &outcome, evidence, model.SessionWhiteboardDocument{}); err == nil {
		t.Fatal("a summary that did not fit was stored")
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
	// The first answer and every rewrite of it are kept, one call each.
	if len(record.Calls) < 2 {
		t.Fatalf("the attempt made %d calls, want the first one and its rewrites", len(record.Calls))
	}
	for index, call := range record.Calls {
		if call.Outcome != EvidenceOutcomeOK {
			t.Fatalf("call %d ended as %q, want the one that came back", index, call.Outcome)
		}
		if call.RawOutput == "" {
			t.Fatalf("call %d carries no answer", index)
		}
	}
}

// The outer retry is another attempt, and its calls are its own.
func TestTheOuterRetryIsAnotherAttemptWithItsOwnCalls(t *testing.T) {
	recorder := &recordingChatModel{inner: &scriptedModel{bodies: []string{"not a document"}}}
	generator := evidenceGenerator(t, recorder)
	outcome := batchOutcome{}
	evidence := newBatchEvidenceCollector(config.AgentConfig{SummarySegmentDebugDump: true}, "run-1")

	if _, err := runSegmentBatchAttempts(context.Background(), generator, recorder,
		evidenceInput(), 1, 2, 0, &outcome, evidence, model.SessionWhiteboardDocument{}); err == nil {
		t.Fatal("an answer that is not a document did not fail the batch")
	}
	records := evidence.set().Records
	if len(records) != 2 {
		t.Fatalf("the run kept %d records, want 2", len(records))
	}
	for index, record := range records {
		if record.Attempt != index+1 {
			t.Fatalf("record %d is attempt %d", index, record.Attempt)
		}
		if len(record.Calls) == 0 {
			t.Fatalf("attempt %d carries no calls", record.Attempt)
		}
		for _, call := range record.Calls {
			if call.Index != 1 {
				t.Fatalf("attempt %d numbers its calls from %d", record.Attempt, call.Index)
			}
		}
	}
}

// An answer that is too long is cut, and the cut is marked.
//
// The file says that it was cut and how long the original was: a reader has
// to be able to tell an answer that was cut from an answer that was short.
func TestAnAnswerThatIsTooLongIsMarkedAsTruncated(t *testing.T) {
	long := strings.Repeat("答", evidenceAnswerMaxBytes)
	recorder := &recordingChatModel{inner: &scriptedModel{bodies: []string{long}}}
	generator := evidenceGenerator(t, recorder)
	outcome := batchOutcome{}
	evidence := newBatchEvidenceCollector(config.AgentConfig{SummarySegmentDebugDump: true}, "run-1")

	if _, err := runSegmentBatchAttempts(context.Background(), generator, recorder,
		evidenceInput(), 1, 1, 0, &outcome, evidence, model.SessionWhiteboardDocument{}); err == nil {
		t.Fatal("an answer that is not a document did not fail the batch")
	}
	records := evidence.set().Records
	if len(records) != 1 || len(records[0].Calls) == 0 {
		t.Fatalf("the run kept %+v, want one attempt with a call", records)
	}
	call := records[0].Calls[0]
	if !call.Truncated {
		t.Fatal("the call does not say that its answer was cut")
	}
	if call.OriginalBytes != len(long) {
		t.Fatalf("the call says the answer was %d bytes, want %d", call.OriginalBytes, len(long))
	}
	if len(call.RawOutput) > evidenceAnswerMaxBytes+32 {
		t.Fatalf("the call kept %d bytes, want it cut", len(call.RawOutput))
	}
}

// The evidence of a run that may not keep it is not kept at all.
//
// The switch is the deployment's summary segment debug dump, and it is off by
// default: the evidence is the learner's own words, and a run nobody is going
// to read has no business keeping them.
func TestTheEvidenceIsNotKeptWhenTheDebugDumpIsOff(t *testing.T) {
	collector := newBatchEvidenceCollector(config.AgentConfig{SummarySegmentDebugDump: false}, "run-1")
	collector.add(BatchEvidence{Batch: 1, Attempt: 1, Outcome: EvidenceOutcomeGenerated})
	if set := collector.set(); set != nil {
		t.Fatalf("the run kept an evidence set although the dump was off: %+v", set)
	}
}

// The evidence is written next to the run's own files, one file per attempt
// and one per model call, and the result file does not carry it.
func TestTheEvidenceIsWrittenNextToTheRunAndNotIntoTheResult(t *testing.T) {
	dir := t.TempDir()
	records := []BatchEvidence{{
		Batch: 1, Attempt: 1, Outcome: EvidenceOutcomeGenerated,
		StartMessageID: 95, EndMessageID: 96,
		SegmentBody: "the segment body",
		Calls: []CallEvidence{{
			Index: 1, Kind: EvidenceCallSummary, Outcome: EvidenceOutcomeOK,
			RawOutput: "the model's own answer", OriginalBytes: len("the model's own answer"),
		}},
		Entries: []agent.WhiteboardEntryOutcome{{
			Key: "pages", ClaimReason: model.SessionWhiteboardAnchorQuoteNotInMessage,
			ClaimMessageID: 95, ClaimVerdict: "rejected",
			Outcome: "refused", OutcomeReason: model.SessionWhiteboardAnchorQuoteNotInMessage,
			AnchorMessageID: 95, AnchorReason: model.SessionWhiteboardAnchorQuoteNotInMessage,
		}},
	}}
	if err := WriteBatchEvidence(dir, records); err != nil {
		t.Fatalf("write the evidence: %v", err)
	}

	written := readEvidenceAttempt(t, dir, 1, 1)
	for _, field := range []string{"batch", "attempt", "outcome", "reason", "start_message_id",
		"end_message_id", "segment_body", "calls", "entries"} {
		if _, ok := written[field]; !ok {
			t.Fatalf("the evidence file has no %q: %v", field, written)
		}
	}
	calls, ok := written["calls"].([]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("calls = %v, want the one call", written["calls"])
	}
	call, ok := calls[0].(map[string]any)
	if !ok {
		t.Fatalf("the call is not an object: %v", calls[0])
	}
	for _, field := range []string{"index", "kind", "outcome", "file"} {
		if _, ok = call[field]; !ok {
			t.Fatalf("the call has no %q: %v", field, call)
		}
	}
	callFile, ok := call["file"].(string)
	if !ok {
		t.Fatalf("the call does not name its own file: %v", call)
	}
	raw, err := os.ReadFile(filepath.Join(dir, EvidenceDirName, callFile))
	if err != nil {
		t.Fatalf("read the call file: %v", err)
	}
	var callWritten map[string]any
	if err = json.Unmarshal(raw, &callWritten); err != nil {
		t.Fatalf("the call file is not a JSON object: %v", err)
	}
	if callWritten["raw_output"] != "the model's own answer" {
		t.Fatalf("raw_output = %v, want the model's own answer", callWritten["raw_output"])
	}
	for _, field := range []string{"run_id", "batch", "attempt", "call_index", "call_type",
		"outcome", "raw_output"} {
		if _, ok = callWritten[field]; !ok {
			t.Fatalf("the call file has no %q: %v", field, callWritten)
		}
	}

	// An entry carries the three layers and nothing else: an entry that
	// carried the learner's own words would put them into a counter.
	entries, ok := written["entries"].([]any)
	if !ok || len(entries) != 1 {
		t.Fatalf("entries = %v, want one entry", written["entries"])
	}
	entry, ok := entries[0].(map[string]any)
	if !ok {
		t.Fatalf("the entry is not an object: %v", entries[0])
	}
	if len(entry) != 8 {
		t.Fatalf("the entry carries %d fields, want the three layers of it: %v", len(entry), entry)
	}
	for _, field := range []string{"key", "claim_verdict", "claim_reason", "claim_message_id",
		"outcome", "outcome_reason", "anchor_message_id", "anchor_reason"} {
		if _, ok = entry[field]; !ok {
			t.Fatalf("the entry has no %q: %v", field, entry)
		}
	}

	// The result file is read by people who do not need the learner's words,
	// so it does not carry them.
	if err = WriteLayeredReadFiles(dir, LayeredRead{Scenario: "evidence"}); err != nil {
		t.Fatalf("write the run's files: %v", err)
	}
	result, err := os.ReadFile(filepath.Join(dir, "read.json"))
	if err != nil {
		t.Fatalf("read the result file: %v", err)
	}
	if strings.Contains(string(result), "the model's own answer") {
		t.Fatal("the result file carries the model's raw answer")
	}
}

// The evidence carries no credential.
//
// The identity of a call is local to the driver: nothing about the request
// is sent on, and nothing about how it was asked is kept.
func TestTheEvidenceCarriesNoCredential(t *testing.T) {
	dir := t.TempDir()
	records := []BatchEvidence{{
		Batch: 1, Attempt: 1, Outcome: EvidenceOutcomeFailed, Reason: "model_error",
		Calls: []CallEvidence{{
			Index: 1, Kind: EvidenceCallSummary, Outcome: EvidenceOutcomeFailed,
			Reason: "model_error", RawOutput: "the model's own answer",
		}},
	}}
	if err := WriteBatchEvidence(dir, records); err != nil {
		t.Fatalf("write the evidence: %v", err)
	}
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		body := string(raw)
		for _, forbidden := range []string{"Authorization", "api_key", "apiKey", "Bearer", "password"} {
			if strings.Contains(body, forbidden) {
				t.Fatalf("the file %s carries %q", filepath.Base(path), forbidden)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the evidence: %v", err)
	}
}

// The evidence is written before the sandbox goes.
//
// The order is the point: the sandbox holds the model's own answers, and a
// run that tore it down first has thrown them away. A write that fails
// therefore means the sandbox is kept, and the caller is told.
func TestTheEvidenceIsWrittenBeforeTheSandboxGoes(t *testing.T) {
	dir := t.TempDir()
	order := make([]string, 0, 2)
	teardown := func() error {
		order = append(order, "teardown")
		return nil
	}
	records := []BatchEvidence{{
		Batch: 1, Attempt: 1, Outcome: EvidenceOutcomeGenerated,
		Calls: []CallEvidence{{Index: 1, Kind: EvidenceCallSummary, Outcome: EvidenceOutcomeOK,
			RawOutput: "the model's own answer"}},
	}}
	if err := finishRunWithEvidence(dir, records, true, teardown); err != nil {
		t.Fatalf("finish the run: %v", err)
	}
	if len(order) != 1 || order[0] != "teardown" {
		t.Fatalf("the teardown ran %v, want it after the evidence was written", order)
	}
	if _, err := os.Stat(filepath.Join(dir, EvidenceDirName)); err != nil {
		t.Fatalf("the evidence was not written before the teardown: %v", err)
	}
}

// A write that fails keeps the sandbox.
//
// The sandbox holds the only copy of the model's own answers, so a run that
// could not write them down must not throw them away: the error says so and
// the teardown does not happen.
func TestAFailedExportKeepsTheSandboxAndIsReported(t *testing.T) {
	dir := t.TempDir()
	// A file where the evidence directory has to be: it cannot be created.
	if err := os.WriteFile(filepath.Join(dir, EvidenceDirName), []byte("in the way"), 0o600); err != nil {
		t.Fatalf("write the blocking file: %v", err)
	}
	torn := false
	err := finishRunWithEvidence(dir, []BatchEvidence{{Batch: 1, Attempt: 1}}, true,
		func() error {
			torn = true
			return nil
		})
	if err == nil {
		t.Fatal("the write that could not happen was reported as a success")
	}
	if !errors.Is(err, errEvidenceNotExported) {
		t.Fatalf("the error = %v, want the one that says the evidence was not exported", err)
	}
	if torn {
		t.Fatal("the sandbox was torn down although the evidence was not written")
	}
}

// A run whose switch was off writes no evidence at all.
func TestNoEvidenceIsWrittenWhenTheDebugDumpIsOff(t *testing.T) {
	dir := t.TempDir()
	torn := false
	err := finishRunWithEvidence(dir, nil, false, func() error {
		torn = true
		return nil
	})
	if err != nil {
		t.Fatalf("finish the run: %v", err)
	}
	if !torn {
		t.Fatal("the sandbox was not torn down")
	}
	if _, statErr := os.Stat(filepath.Join(dir, EvidenceDirName)); !os.IsNotExist(statErr) {
		t.Fatalf("the evidence directory exists although the dump was off: %v", statErr)
	}
}

func TestGeneratedRetentionIsWrittenSeparatelyFromSnapshots(t *testing.T) {
	dir := t.TempDir()
	before := whiteboardSnapshotOf(model.SessionWhiteboardDocument{SchemaVersion: model.SessionWhiteboardSchemaV3})
	after := whiteboardSnapshotOf(model.SessionWhiteboardDocument{SchemaVersion: model.SessionWhiteboardSchemaV3})
	records := []BatchEvidence{{Batch: 1, Attempt: 1, Outcome: EvidenceOutcomeGenerated, WhiteboardBefore: before, WhiteboardAfter: after, Entries: []agent.WhiteboardEntryOutcome{{Key: "key20", Outcome: "applied", EntryRetention: "removed", EntryRetentionReason: "item_capacity", AnchorMessageID: 12, AnchorRetention: "removed", AnchorRetentionReason: "render_budget"}}}}
	if err := WriteBatchEvidence(dir, records); err != nil {
		t.Fatal(err)
	}
	file := readEvidenceAttempt(t, dir, 1, 1)
	entries := file["entries"].([]any)
	e := entries[0].(map[string]any)
	for key, want := range map[string]string{"outcome": "applied", "generated_entry_retention": "removed", "generated_entry_retention_reason": "item_capacity", "generated_anchor_retention": "removed", "generated_anchor_retention_reason": "render_budget"} {
		if e[key] != want {
			t.Fatalf("%s=%v want %s", key, e[key], want)
		}
	}
}

type splitExportModel struct{ calls int }

func (m *splitExportModel) WithTools([]*schema.ToolInfo) (modelcomponent.ToolCallingChatModel, error) {
	return m, nil
}
func (m *splitExportModel) Stream(context.Context, []*schema.Message, ...modelcomponent.Option) (*schema.StreamReader[*schema.Message], error) {
	return nil, errors.New("unused")
}
func (m *splitExportModel) Generate(_ context.Context, input []*schema.Message, _ ...modelcomponent.Option) (*schema.Message, error) {
	m.calls++
	re := regexp.MustCompile(`\[message_id=(\d+) role=user\]`)
	match := re.FindStringSubmatch(input[1].Content)
	if len(match) < 2 {
		return nil, errors.New("missing learner message")
	}
	id, _ := strconv.Atoi(match[1])
	entries := make([]map[string]any, 5)
	for i := range entries {
		entries[i] = map[string]any{"key": fmt.Sprintf("call%d_entry%d", m.calls, i), "text": fmt.Sprintf("entry %d %d", m.calls, i), "value": fmt.Sprintf("%d-%d", m.calls, i), "source_message_ids": []int{id}, "updated_at_message_id": id}
	}
	obj := map[string]any{"schema_version": 1, "segment": map[string]any{"topic": "test", "points": []any{map[string]any{"text": "point", "evidence_message_ids": []int{id}, "source": "user"}}}, "whiteboard": map[string]any{"schema_version": 1, "constraints": entries}}
	raw, _ := json.Marshal(obj)
	return schema.AssistantMessage(string(raw), nil), nil
}
func TestSplitGeneratorFinalRetentionReachesAttemptFile(t *testing.T) {
	modelStub := &splitExportModel{}
	recorder := &recordingChatModel{inner: modelStub}
	generator, err := agent.NewModelSegmentGenerator(recorder, 1200, 2000)
	if err != nil {
		t.Fatal(err)
	}
	generator.UseAssistantContent(true)
	messages := make([]model.Message, 0, 130)
	for i := 1; i <= 65; i++ {
		messages = append(messages, model.Message{ID: uint(2*i - 1), IsUser: true, Content: fmt.Sprintf("turn %d", i)}, model.Message{ID: uint(2 * i), Content: strings.Repeat("assistant explanation ", 100)})
	}
	collector := newBatchEvidenceCollector(config.AgentConfig{SummarySegmentDebugDump: true}, "run")
	result, err := runSegmentBatchAttempts(context.Background(), generator, recorder, agent.SegmentGenerationInput{Messages: messages, FirstMessageID: 1, LastMessageID: 130}, 1, 1, 0, &batchOutcome{}, collector, model.SessionWhiteboardDocument{})
	if err != nil {
		t.Fatal(err)
	}
	if modelStub.calls < 5 {
		t.Fatalf("only %d calls", modelStub.calls)
	}
	dir := t.TempDir()
	if err = WriteBatchEvidence(dir, collector.set().Records); err != nil {
		t.Fatal(err)
	}
	file := readEvidenceAttempt(t, dir, 1, 1)
	entries := file["entries"].([]any)
	found := false
	for _, raw := range entries {
		row := raw.(map[string]any)
		if row["key"] == "call1_entry0" {
			found = true
			if row["call_index"] != float64(1) || row["outcome"] != "applied" || row["generated_entry_retention"] != "removed" || row["generated_entry_retention_reason"] != "item_capacity" {
				t.Fatalf("row=%v", row)
			}
		}
	}
	if !found {
		t.Fatal("first call not exported")
	}
	for _, entry := range result.Whiteboard.Constraints {
		if entry.Key == "call1_entry0" {
			t.Fatal("evicted entry persisted")
		}
	}
}

type splitAnchorExportModel struct{ calls int }

func (m *splitAnchorExportModel) WithTools([]*schema.ToolInfo) (modelcomponent.ToolCallingChatModel, error) {
	return m, nil
}
func (m *splitAnchorExportModel) Stream(context.Context, []*schema.Message, ...modelcomponent.Option) (*schema.StreamReader[*schema.Message], error) {
	return nil, errors.New("unused")
}
func (m *splitAnchorExportModel) Generate(_ context.Context, input []*schema.Message, _ ...modelcomponent.Option) (*schema.Message, error) {
	m.calls++
	match := regexp.MustCompile(`\[message_id=(\d+) role=user\]`).FindStringSubmatch(input[1].Content)
	if len(match) < 2 {
		return nil, errors.New("missing message")
	}
	id, _ := strconv.Atoi(match[1])
	entry := func(key, value string, ids []int) map[string]any {
		return map[string]any{"key": key, "text": "每天学习 " + value, "value": value, "source_message_ids": ids, "updated_at_message_id": id}
	}
	items := []map[string]any{}
	if m.calls == 1 {
		items = append(items, entry("resolved", "30页", []int{id, 99999}), entry("expired", "31页", []int{id, 99999}))
	} else {
		if m.calls == 2 {
			items = append(items, entry("resolved", "30页", []int{id}))
		}
		items = append(items, entry(fmt.Sprintf("a%d", m.calls), "31页", []int{id, 99999}))
	}
	obj := map[string]any{"schema_version": 1, "segment": map[string]any{"topic": "test", "points": []any{map[string]any{"text": "point", "evidence_message_ids": []int{id}, "source": "user"}}}, "whiteboard": map[string]any{"schema_version": 1, "constraints": items}}
	b, _ := json.Marshal(obj)
	return schema.AssistantMessage(string(b), nil), nil
}
func TestSplitGeneratorAnchorRetentionReachesAttemptFile(t *testing.T) {
	fake := &splitAnchorExportModel{}
	recorder := &recordingChatModel{inner: fake}
	generator, err := agent.NewModelSegmentGenerator(recorder, 1200, 2000)
	if err != nil {
		t.Fatal(err)
	}
	generator.UseAssistantContent(true)
	messages := make([]model.Message, 0, 280)
	for i := 1; i <= 140; i++ {
		messages = append(messages, model.Message{ID: uint(2*i - 1), IsUser: true, Content: fmt.Sprintf("turn %d 每天学习30页、31页。", i)}, model.Message{ID: uint(2 * i), Content: strings.Repeat("assistant explanation ", 100)})
	}
	collector := newBatchEvidenceCollector(config.AgentConfig{SummarySegmentDebugDump: true}, "run")
	_, err = runSegmentBatchAttempts(context.Background(), generator, recorder, agent.SegmentGenerationInput{Messages: messages, FirstMessageID: 1, LastMessageID: 280}, 1, 1, 0, &batchOutcome{}, collector, model.SessionWhiteboardDocument{})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err = WriteBatchEvidence(dir, collector.set().Records); err != nil {
		t.Fatal(err)
	}
	entries := readEvidenceAttempt(t, dir, 1, 1)["entries"].([]any)
	seen := map[string]bool{}
	for _, raw := range entries {
		row := raw.(map[string]any)
		key, _ := row["key"].(string)
		if row["call_index"] != float64(1) {
			continue
		}
		if key == "resolved" || key == "expired" {
			want := map[string]string{"resolved": "anchor_resolved", "expired": "anchor_capacity"}[key]
			if row["generated_anchor_retention"] != "removed" || row["generated_anchor_retention_reason"] != want {
				t.Fatalf("%s: %v", key, row)
			}
			seen[key] = true
		}
	}
	if !seen["resolved"] || !seen["expired"] {
		t.Fatalf("missing anchors: %v", seen)
	}
}
