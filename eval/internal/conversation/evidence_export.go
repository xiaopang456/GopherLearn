package conversation

// The evidence a run leaves behind when the sandbox that produced it is gone.
//
// The counters a run reports say how many batches failed and how many
// candidates the board refused. They cannot say which entry was refused, on
// what grounds, or what the model actually wrote: the program's own answer is
// dropped with the error, and a refused candidate is counted rather than
// kept. The three that can answer those questions - the model's own answers,
// the segment body the program made of them, and the refused candidates - are
// the learner's own words.
//
// So they are written into the private run directory and nowhere else: not
// into the result file, which is read by people who do not need them, not
// into a test's expectations, and not into anything that is committed. The
// switch is the deployment's summary segment debug dump: a run that nobody is
// going to read afterwards has no business keeping the learner's words.
//
// The evidence is per model call, not per attempt. A batch is one call to
// write the summary and up to two more to write it again when the first does
// not fit, and an attempt that failed is retried outside: filing the whole
// attempt under one answer would lose every answer but the last one. Each
// call therefore carries its own number, its own kind and its own body, and
// the attempt carries the list of them.
//
// An answer that is too long is cut, and the cut is marked: the file says
// that it was cut and how long the original was. It does not promise the
// whole text.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"unicode/utf8"

	"GopherAI/config"
	agent "GopherAI/service/agent"
)

// The outcomes one model call can have.
const (
	// EvidenceOutcomeOK says the call came back with an answer.
	EvidenceOutcomeOK = "ok"
	// EvidenceOutcomeGenerated says the attempt produced a segment.
	EvidenceOutcomeGenerated = "generated"
	// EvidenceOutcomeFailed says the call came back with nothing usable.
	EvidenceOutcomeFailed = "failed"
)

// The kinds of call the segment writer makes.
//
// The kind is what a reader needs to tell a first answer from the ones that
// were asked for again, and the answers are not interchangeable: the first
// one is what the model thought, the others are what it said when it was told
// to be shorter.
const (
	// EvidenceCallSummary is a call that writes the summary.
	EvidenceCallSummary = "summary"
	// EvidenceCallRewrite is a call that writes the summary again because
	// the one before it did not fit the window.
	EvidenceCallRewrite = "rewrite"
	// EvidenceCallChanges is a call that asks for the board's changes
	// instead of the whole board.
	EvidenceCallChanges = "changes"
)

// errEvidenceNotExported says the evidence of a run was not written.
//
// It is a separate error because it has a separate consequence: the sandbox
// is kept, because it holds the only copy of the model's own answers.
var errEvidenceNotExported = errors.New("the evidence of the run was not exported")

// EvidenceDirName is the directory the evidence is written into, under the
// run's own output directory.
const EvidenceDirName = "evidence"

// The bound on one answer that is kept.
//
// A model that ignored the format can answer with prose of any length, and
// one runaway answer must not fill the run's directory. The bound is on
// bytes, and the cut is put on a rune boundary: half a character at the end
// of an answer would make the file unreadable for everything that reads it.
const evidenceAnswerMaxBytes = 256 << 10

// CallEvidence is one model call of one attempt.
type CallEvidence struct {
	// Index is the call's number inside its attempt, counting from one.
	Index int
	// Kind says what the call was for.
	Kind string
	// Outcome says how it ended.
	Outcome string
	// Reason names why, for a call that did not succeed.
	Reason string
	// RawOutput is what the model answered, as far as it is kept.
	RawOutput string
	// Truncated says the answer was cut.
	Truncated bool
	// OriginalBytes is how long the answer was before it was cut.
	OriginalBytes int
}

// BatchEvidence is what one attempt at one batch produced.
type BatchEvidence struct {
	// RunID names the run the evidence belongs to.
	RunID string
	// Batch is which batch of the conversation this was, counting from one.
	Batch int
	// Attempt is which attempt at that batch, counting from one.
	Attempt int
	// Outcome is "generated" or "failed".
	Outcome string
	// Reason is why it failed, empty when it succeeded.
	Reason string
	// StartMessageID and EndMessageID are the turns the attempt covered.
	StartMessageID uint
	EndMessageID   uint
	// SegmentBody is the segment content the program made of the answer.
	SegmentBody string
	// Claims are the candidates the board refused, with the reason each one
	// was refused.
	Claims []agent.WhiteboardClaimCandidate
	// Calls are the model calls the attempt made, in order.
	Calls []CallEvidence
}

// BatchEvidenceSet is the evidence of one run, and whether the run was
// allowed to keep any.
//
// A run that was not allowed to keep the evidence has no set at all: the
// absence of the set is what says the switch was off, and an empty set would
// say the same thing as a run that kept nothing.
type BatchEvidenceSet struct {
	// Enabled says the deployment's summary segment debug dump was on.
	Enabled bool
	// RunID names the run the evidence belongs to.
	RunID string
	// Records is one entry per attempt, in the order the attempts happened.
	Records []BatchEvidence
}

// batchEvidenceFile is what one attempt file holds.
//
// The calls are listed with what each of them was and how it ended, and the
// raw body of each call is in its own file: the bodies are the big part, and
// a reader who wants one of them should not have to read the others.
type batchEvidenceFile struct {
	RunID          string               `json:"run_id"`
	Batch          int                  `json:"batch"`
	Attempt        int                  `json:"attempt"`
	Outcome        string               `json:"outcome"`
	Reason         string               `json:"reason"`
	StartMessageID uint                 `json:"start_message_id"`
	EndMessageID   uint                 `json:"end_message_id"`
	SegmentBody    string               `json:"segment_body"`
	Calls          []batchEvidenceCall  `json:"calls"`
	Claims         []batchEvidenceClaim `json:"claims"`
}

// batchEvidenceCall is one call as the attempt file lists it.
type batchEvidenceCall struct {
	Index         int    `json:"index"`
	Kind          string `json:"kind"`
	Outcome       string `json:"outcome"`
	Reason        string `json:"reason,omitempty"`
	Truncated     bool   `json:"truncated,omitempty"`
	OriginalBytes int    `json:"original_bytes,omitempty"`
	// File is the file the call's own body is in.
	File string `json:"file"`
}

// batchEvidenceCallFile is what one call file holds.
type batchEvidenceCallFile struct {
	RunID         string `json:"run_id"`
	Batch         int    `json:"batch"`
	Attempt       int    `json:"attempt"`
	CallIndex     int    `json:"call_index"`
	CallType      string `json:"call_type"`
	Outcome       string `json:"outcome"`
	Reason        string `json:"reason,omitempty"`
	RawOutput     string `json:"raw_output"`
	Truncated     bool   `json:"truncated,omitempty"`
	OriginalBytes int    `json:"original_bytes,omitempty"`
}

// batchEvidenceClaim is one refused candidate, as it is written down.
//
// The four fields the product gives it: the key the model gave the entry, the
// check that refused it, the message the claim rested on and the verdict the
// engine made of it. The learner's own words are not among them - they are in
// the raw answer, and a candidate that carried them would put the learner's
// text into a counter.
type batchEvidenceClaim struct {
	Key       string `json:"key"`
	Reason    string `json:"reason"`
	MessageID uint   `json:"message_id"`
	Verdict   string `json:"verdict"`
}

// WriteBatchEvidence writes the evidence of every attempt next to the run's
// other files.
//
// One file per attempt and one per model call, named after the batch, the
// attempt and the call so that the files sort in the order the calls
// happened. The directory is created if it is not there yet, and a failure to
// write is returned rather than swallowed: evidence that was not written is a
// run that cannot be read, and the reader has to be told.
func WriteBatchEvidence(dir string, records []BatchEvidence) error {
	if dir == "" {
		return fmt.Errorf("%w: the run has no output directory", errEvidenceNotExported)
	}
	evidenceDir := filepath.Join(dir, EvidenceDirName)
	if err := os.MkdirAll(evidenceDir, 0o700); err != nil {
		return fmt.Errorf("create the evidence directory: %w", err)
	}
	for _, record := range records {
		if err := writeBatchEvidenceAttempt(evidenceDir, record); err != nil {
			return err
		}
	}
	return nil
}

// writeBatchEvidenceAttempt writes the attempt's file and its call files.
func writeBatchEvidenceAttempt(dir string, record BatchEvidence) error {
	calls := make([]batchEvidenceCall, 0, len(record.Calls))
	for _, call := range record.Calls {
		name := fmt.Sprintf("batch-%02d-attempt-%02d-call-%02d.json",
			record.Batch, record.Attempt, call.Index)
		file := batchEvidenceCallFile{
			RunID: record.RunID, Batch: record.Batch, Attempt: record.Attempt,
			CallIndex: call.Index, CallType: call.Kind, Outcome: call.Outcome,
			Reason: call.Reason, RawOutput: call.RawOutput,
			Truncated: call.Truncated, OriginalBytes: call.OriginalBytes,
		}
		if err := writeEvidenceJSON(filepath.Join(dir, name), file); err != nil {
			return err
		}
		calls = append(calls, batchEvidenceCall{
			Index: call.Index, Kind: call.Kind, Outcome: call.Outcome, Reason: call.Reason,
			Truncated: call.Truncated, OriginalBytes: call.OriginalBytes, File: name,
		})
	}
	claims := make([]batchEvidenceClaim, 0, len(record.Claims))
	for _, claim := range record.Claims {
		claims = append(claims, batchEvidenceClaim{
			Key: claim.Key, Reason: claim.Reason, MessageID: claim.MessageID, Verdict: claim.Verdict,
		})
	}
	name := fmt.Sprintf("batch-%02d-attempt-%02d.json", record.Batch, record.Attempt)
	return writeEvidenceJSON(filepath.Join(dir, name), batchEvidenceFile{
		RunID: record.RunID, Batch: record.Batch, Attempt: record.Attempt,
		Outcome: record.Outcome, Reason: record.Reason,
		StartMessageID: record.StartMessageID, EndMessageID: record.EndMessageID,
		SegmentBody: record.SegmentBody, Calls: calls, Claims: claims,
	})
}

// writeEvidenceJSON writes one file.
func writeEvidenceJSON(path string, value any) error {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode the evidence of %s: %w", filepath.Base(path), err)
	}
	if err = os.WriteFile(path, append(encoded, '\n'), 0o600); err != nil {
		return fmt.Errorf("write the evidence of %s: %w", filepath.Base(path), err)
	}
	return nil
}

// finishRunWithEvidence writes the evidence of a run and only then lets the
// sandbox go.
//
// The order is the point. The sandbox holds the model's own answers, and they
// are the only copy of what the run produced; a run that tore the sandbox
// down before the evidence was written has thrown them away. So the teardown
// happens after the write, and a write that fails means the teardown does
// not happen at all: the caller is told, and the container is still there.
//
// The teardown is a parameter so that the order can be tested without a
// container.
func finishRunWithEvidence(outputDir string, records []BatchEvidence, enabled bool,
	teardown func() error) error {
	if enabled {
		if err := WriteBatchEvidence(outputDir, records); err != nil {
			return fmt.Errorf("%w: %v", errEvidenceNotExported, err)
		}
	}
	if teardown == nil {
		return nil
	}
	return teardown()
}

// newBatchEvidenceCollector says whether this deployment is allowed to keep
// the evidence of a run.
//
// The evidence is the learner's own text, so it is kept only when the
// deployment's summary segment debug dump is on. The collector is the switch:
// a run that was not allowed to keep the evidence keeps none of it, and the
// records it holds are empty rather than partial.
type batchEvidenceCollector struct {
	enabled bool
	runID   string
	records []BatchEvidence
}

// newBatchEvidenceCollector builds the collector of a run.
func newBatchEvidenceCollector(agentConfig config.AgentConfig, runID string) *batchEvidenceCollector {
	return &batchEvidenceCollector{enabled: agentConfig.SummarySegmentDebugDump, runID: runID}
}

// add keeps one attempt's evidence, when the run is allowed to keep any.
func (c *batchEvidenceCollector) add(record BatchEvidence) {
	if c == nil || !c.enabled {
		return
	}
	record.RunID = c.runID
	c.records = append(c.records, record)
}

// set is what the run kept, and whether it was allowed to keep it.
func (c *batchEvidenceCollector) set() *BatchEvidenceSet {
	if c == nil || !c.enabled {
		return nil
	}
	return &BatchEvidenceSet{Enabled: true, RunID: c.runID, Records: c.records}
}

// truncateAnswer bounds one answer that is kept.
//
// The cut is marked and the original length is recorded: a reader has to be
// able to tell an answer that was cut from an answer that was short.
func truncateAnswer(body string) (string, bool, int) {
	original := len(body)
	if original <= evidenceAnswerMaxBytes {
		return body, false, original
	}
	cut := evidenceAnswerMaxBytes
	for cut > 0 && !utf8.RuneStart(body[cut]) {
		cut--
	}
	return body[:cut] + "\n...[truncated]", true, original
}
