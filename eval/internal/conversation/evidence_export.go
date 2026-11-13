package conversation

// The evidence a run leaves behind when the sandbox that produced it is gone.
//
// The counters a run reports say how many batches failed and how many
// candidates the board refused. They cannot say which entry was refused, on
// what grounds, or what the model actually wrote: the program's own answer is
// dropped with the error, and a refused candidate is counted rather than
// kept. The three that can answer those questions - the model's own answer,
// the segment body the program made of it, and the refused candidates - are
// the learner's own words.
//
// So they are written into the private run directory and nowhere else: not
// into the result file, which is read by people who do not need them, not
// into a test's expectations, and not into anything that is committed. The
// switch is the deployment's summary segment debug dump: a run that nobody is
// going to read afterwards has no business keeping the learner's words.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"GopherAI/config"
	agent "GopherAI/service/agent"
)

// The outcomes one attempt at one batch can have.
const (
	// EvidenceOutcomeGenerated says the attempt produced a segment.
	EvidenceOutcomeGenerated = "generated"
	// EvidenceOutcomeFailed says the attempt produced nothing.
	EvidenceOutcomeFailed = "failed"
)

// EvidenceDirName is the directory the evidence is written into, under the
// run's own output directory.
const EvidenceDirName = "evidence"

// BatchEvidence is what one attempt at one batch produced, kept for a run
// that has to be read afterwards.
type BatchEvidence struct {
	Batch          int    // which batch of the conversation this was, 1 based
	Attempt        int    // which attempt at that batch, 1 based
	Outcome        string // "generated" or "failed"
	Reason         string // why it failed, "" when it succeeded
	StartMessageID uint
	EndMessageID   uint
	RawOutput      string                           // the model's own answer, verbatim, as far as it goes
	SegmentBody    string                           // the segment content the program made of it
	Claims         []agent.WhiteboardClaimCandidate // the refused candidates
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
	// Records is one entry per attempt, in the order the attempts happened.
	Records []BatchEvidence
}

// batchEvidenceFile is what one evidence file holds.
//
// The candidates are written with the four fields the product gives them: the
// key the model gave the entry, the check that refused it, the message the
// claim rested on and the verdict the engine made of it. The learner's own
// words are not among them - they are in the raw answer, and a candidate
// that carried them would put the learner's text into a counter.
type batchEvidenceFile struct {
	Batch          int                  `json:"batch"`
	Attempt        int                  `json:"attempt"`
	Outcome        string               `json:"outcome"`
	Reason         string               `json:"reason"`
	StartMessageID uint                 `json:"start_message_id"`
	EndMessageID   uint                 `json:"end_message_id"`
	RawOutput      string               `json:"raw_output"`
	SegmentBody    string               `json:"segment_body"`
	Claims         []batchEvidenceClaim `json:"claims"`
}

// batchEvidenceClaim is one refused candidate, as it is written down.
type batchEvidenceClaim struct {
	Key       string `json:"key"`
	Reason    string `json:"reason"`
	MessageID uint   `json:"message_id"`
	Verdict   string `json:"verdict"`
}

// batchEvidenceFileFrom turns one record into what is written down.
func batchEvidenceFileFrom(record BatchEvidence) batchEvidenceFile {
	claims := make([]batchEvidenceClaim, 0, len(record.Claims))
	for _, claim := range record.Claims {
		claims = append(claims, batchEvidenceClaim{
			Key:       claim.Key,
			Reason:    claim.Reason,
			MessageID: claim.MessageID,
			Verdict:   claim.Verdict,
		})
	}
	return batchEvidenceFile{
		Batch:          record.Batch,
		Attempt:        record.Attempt,
		Outcome:        record.Outcome,
		Reason:         record.Reason,
		StartMessageID: record.StartMessageID,
		EndMessageID:   record.EndMessageID,
		RawOutput:      record.RawOutput,
		SegmentBody:    record.SegmentBody,
		Claims:         claims,
	}
}

// WriteBatchEvidence writes the evidence of every attempt next to the run's
// other files.
//
// One file per attempt, named after the batch and the attempt so that the
// files sort in the order the attempts happened. The directory is created if
// it is not there yet, and a failure to write is returned rather than
// swallowed: evidence that was not written is a run that cannot be read, and
// the reader has to be told.
func WriteBatchEvidence(dir string, records []BatchEvidence) error {
	evidenceDir := filepath.Join(dir, EvidenceDirName)
	if err := os.MkdirAll(evidenceDir, 0o700); err != nil {
		return fmt.Errorf("create the evidence directory: %w", err)
	}
	for _, record := range records {
		encoded, err := json.MarshalIndent(batchEvidenceFileFrom(record), "", "  ")
		if err != nil {
			return fmt.Errorf("encode the evidence of batch %d attempt %d: %w",
				record.Batch, record.Attempt, err)
		}
		name := fmt.Sprintf("batch-%02d-attempt-%02d.json", record.Batch, record.Attempt)
		path := filepath.Join(evidenceDir, name)
		if err = os.WriteFile(path, append(encoded, '\n'), 0o600); err != nil {
			return fmt.Errorf("write the evidence of batch %d attempt %d: %w",
				record.Batch, record.Attempt, err)
		}
	}
	return nil
}

// batchEvidenceCollector keeps the per-attempt evidence of a run.
//
// The evidence is the learner's own text, so it is kept only when the
// deployment's summary segment debug dump is on. The collector is the switch:
// a run that was not allowed to keep the evidence keeps none of it, and the
// records it holds are empty rather than partial.
type batchEvidenceCollector struct {
	enabled bool
	records []BatchEvidence
}

// newBatchEvidenceCollector says whether this deployment is allowed to keep
// the evidence of a run.
func newBatchEvidenceCollector(agentConfig config.AgentConfig) *batchEvidenceCollector {
	return &batchEvidenceCollector{enabled: agentConfig.SummarySegmentDebugDump}
}

// add keeps one attempt's evidence, when the run is allowed to keep any.
func (c *batchEvidenceCollector) add(record BatchEvidence) {
	if c == nil || !c.enabled {
		return
	}
	c.records = append(c.records, record)
}

// set is what the run kept, and whether it was allowed to keep it.
func (c *batchEvidenceCollector) set() *BatchEvidenceSet {
	if c == nil || !c.enabled {
		return nil
	}
	return &BatchEvidenceSet{Enabled: true, Records: c.records}
}
