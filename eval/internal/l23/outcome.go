// Package l23 reads the per-trial outcomes the L2/L3 scorer writes and turns them into
// success rates, pass^k and paired comparisons.
//
// The scorer (eval/l2l3, Node) judges one trial at a time with pure assertion functions
// and writes outcomes.jsonl; this package does the statistics, reusing the paired
// comparison code of the retrieval evaluation. The two sides share only that file, so its
// shape is pinned by a golden file both read.
package l23

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// OutcomeSchema is the schema string of an outcome line.
const OutcomeSchema = "outcome.v1"

// The four things an assertion or a layer can say.
const (
	Pass       = "pass"
	Fail       = "fail"
	NeedsJudge = "needs_judge"
	Error      = "error"
)

// Assertion is one check inside a layer.
type Assertion struct {
	Name          string `json:"name"`
	Verdict       string `json:"verdict"`
	Label         string `json:"label,omitempty"`
	Informational bool   `json:"informational,omitempty"`
	Reason        string `json:"reason"`
}

// Layer is a trial's verdict on one layer (L2 answer or L3 tool use).
type Layer struct {
	Verdict    string      `json:"verdict"`
	Class      string      `json:"class"`
	Coverage   *float64    `json:"coverage,omitempty"`
	Tier       string      `json:"tier,omitempty"`
	Assertions []Assertion `json:"assertions"`
}

// Cost is what the trial cost, as the run reported it.
type Cost struct {
	ModelCalls     *float64 `json:"modelCalls"`
	ToolCalls      *float64 `json:"toolCalls"`
	DurationMs     *float64 `json:"durationMs"`
	ProjectedInput *float64 `json:"projectedInput"`
}

// Outcome is one trial: one arm, one question, one repeat.
type Outcome struct {
	Schema   string `json:"schema"`
	Arm      string `json:"arm"`
	QID      string `json:"qid"`
	Rep      int    `json:"rep"`
	Env      string `json:"env"`
	Cluster  string `json:"cluster"`
	Category string `json:"category"`
	Layers   struct {
		L2 *Layer `json:"l2"`
		L3 *Layer `json:"l3"`
	} `json:"layers"`
	Cost Cost `json:"cost"`
}

// Layer returns the named layer ("l2" or "l3") or nil.
func (o Outcome) Layer(name string) *Layer {
	if name == "l2" {
		return o.Layers.L2
	}
	return o.Layers.L3
}

// Meta is outcomes.meta.json: what the outcomes were scored against.
type Meta struct {
	Schema        string   `json:"schema"`
	DatasetSHA256 *string  `json:"dataset_sha256"`
	LabelsSHA256  *string  `json:"labels_sha256"`
	L3OnlySHA256  *string  `json:"l3_only_sha256"`
	CorpusHash    string   `json:"corpus_hash"`
	Records       int      `json:"records"`
	Outcomes      int      `json:"outcomes"`
	Envs          []string `json:"envs"`
	Arms          []string `json:"arms"`
}

// ReadOutcomes parses outcomes.jsonl. Unknown fields and a wrong schema string are
// errors, so a change to the scorer's output cannot pass unnoticed.
func ReadOutcomes(r io.Reader) ([]Outcome, error) {
	var outcomes []Outcome
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for line := 1; scanner.Scan(); line++ {
		text := bytes.TrimSpace(scanner.Bytes())
		if len(text) == 0 {
			continue
		}
		decoder := json.NewDecoder(bytes.NewReader(text))
		decoder.DisallowUnknownFields()
		var outcome Outcome
		if err := decoder.Decode(&outcome); err != nil {
			return nil, fmt.Errorf("outcomes line %d: %w", line, err)
		}
		if outcome.Schema != OutcomeSchema {
			return nil, fmt.Errorf("outcomes line %d: schema is %q, want %q", line, outcome.Schema, OutcomeSchema)
		}
		outcomes = append(outcomes, outcome)
	}
	return outcomes, scanner.Err()
}

// ReadDir reads outcomes.jsonl and outcomes.meta.json from a directory.
func ReadDir(dir string) ([]Outcome, Meta, error) {
	file, err := os.Open(filepath.Join(dir, "outcomes.jsonl"))
	if err != nil {
		return nil, Meta{}, err
	}
	defer file.Close()
	outcomes, err := ReadOutcomes(file)
	if err != nil {
		return nil, Meta{}, err
	}
	raw, err := os.ReadFile(filepath.Join(dir, "outcomes.meta.json"))
	if err != nil {
		return nil, Meta{}, err
	}
	var meta Meta
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&meta); err != nil {
		return nil, Meta{}, fmt.Errorf("outcomes.meta.json: %w", err)
	}
	return outcomes, meta, nil
}
