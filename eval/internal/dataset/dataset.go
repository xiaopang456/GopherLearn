package dataset

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// Question splits.
const (
	// SplitDev questions are used while the pipeline is being tuned.
	SplitDev = "dev"
	// SplitSealed questions are held back for the final report.
	SplitSealed = "sealed"
)

// maxReportedIssues bounds how many problems one validation error lists.
const maxReportedIssues = 50

// Span is a closed, 1-based line interval of one corpus document.
type Span struct {
	File  string `json:"file"`
	Start int    `json:"start"`
	End   int    `json:"end"`
}

// EvidenceSet is one complete way to answer a question: every span in it is
// needed. A question lists alternative sets, and a retrieval that covers any one
// of them has found the evidence.
type EvidenceSet []Span

// LineRef names one line of one document.
type LineRef struct {
	File string
	Line int
}

// Lines returns the distinct lines the set covers. Overlapping or repeated spans
// count a line once.
func (s EvidenceSet) Lines() map[LineRef]struct{} {
	lines := make(map[LineRef]struct{})
	for _, span := range s {
		for n := span.Start; n <= span.End; n++ {
			lines[LineRef{File: span.File, Line: n}] = struct{}{}
		}
	}
	return lines
}

// Question is one line of the question file.
type Question struct {
	ID         string        `json:"id"`
	Question   string        `json:"question"`
	Answerable bool          `json:"answerable"`
	Evidence   []EvidenceSet `json:"evidence_sets"`
	Split      string        `json:"split"`
	Topic      string        `json:"topic,omitempty"`
	Type       string        `json:"type,omitempty"`
}

// Dataset is a validated question file.
type Dataset struct {
	Questions []Question
	// Version identifies the file's bytes (sha256, lowercase hex). A run records
	// it so results can be tied to the exact question set that produced them.
	Version string
}

// Filter returns the questions of one split, in file order.
func (d *Dataset) Filter(split string) []Question {
	var out []Question
	for _, q := range d.Questions {
		if q.Split == split {
			out = append(out, q)
		}
	}
	return out
}

// Issue is one structural problem in the question file.
type Issue struct {
	// Line is the 1-based line of the file, or 0 for a problem of the whole file.
	Line    int
	ID      string
	Message string
}

func (i Issue) String() string {
	switch {
	case i.Line > 0 && i.ID != "":
		return fmt.Sprintf("line %d (%s): %s", i.Line, i.ID, i.Message)
	case i.Line > 0:
		return fmt.Sprintf("line %d: %s", i.Line, i.Message)
	default:
		return i.Message
	}
}

// ValidationError lists every structural problem found, so an author fixes the
// whole file in one pass instead of one error at a time.
type ValidationError struct {
	Issues []Issue
}

func (e *ValidationError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "question file has %d problem(s):", len(e.Issues))
	for i, issue := range e.Issues {
		if i == maxReportedIssues {
			fmt.Fprintf(&b, "\n  … and %d more", len(e.Issues)-maxReportedIssues)
			break
		}
		b.WriteString("\n  ")
		b.WriteString(issue.String())
	}
	return b.String()
}

// rawQuestion is the wire shape. Answerable is a pointer so that a missing value
// is an error instead of a silent "false".
type rawQuestion struct {
	ID         *string       `json:"id"`
	Question   *string       `json:"question"`
	Answerable *bool         `json:"answerable"`
	Evidence   []EvidenceSet `json:"evidence_sets"`
	Split      *string       `json:"split"`
	Topic      string        `json:"topic"`
	Type       string        `json:"type"`
}

// LoadDataset reads and validates a question file against its corpus.
func LoadDataset(path string, corpus *Corpus) (*Dataset, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read question file: %w", err)
	}
	return ParseDataset(raw, corpus)
}

// ParseDataset validates question-file bytes against a corpus.
//
// The checks are structural only: it cannot know whether a span really answers a
// question, but it can and does reject spans that point outside the corpus, an
// answerable question without evidence, an unanswerable one with evidence,
// duplicate ids, unknown fields and unknown splits.
func ParseDataset(raw []byte, corpus *Corpus) (*Dataset, error) {
	sum := sha256.Sum256(raw)
	data := &Dataset{Version: hex.EncodeToString(sum[:])}
	var issues []Issue
	seen := map[string]int{}

	reader := bufio.NewReader(bytes.NewReader(raw))
	for lineNo := 1; ; lineNo++ {
		line, readErr := reader.ReadBytes('\n')
		text := bytes.TrimSpace(line)
		if len(text) > 0 {
			question, problems := parseQuestion(text, lineNo, corpus, seen)
			issues = append(issues, problems...)
			if len(problems) == 0 {
				data.Questions = append(data.Questions, question)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, fmt.Errorf("read question file: %w", readErr)
		}
	}
	if len(data.Questions) == 0 && len(issues) == 0 {
		issues = append(issues, Issue{Message: "no questions"})
	}
	if len(issues) > 0 {
		return nil, &ValidationError{Issues: issues}
	}
	return data, nil
}

func parseQuestion(text []byte, lineNo int, corpus *Corpus, seen map[string]int) (Question, []Issue) {
	var raw rawQuestion
	decoder := json.NewDecoder(bytes.NewReader(text))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&raw); err != nil {
		return Question{}, []Issue{{Line: lineNo, Message: "not a valid question object: " + err.Error()}}
	}
	if decoder.More() {
		return Question{}, []Issue{{Line: lineNo, Message: "more than one JSON value on the line"}}
	}

	id := ""
	if raw.ID != nil {
		id = *raw.ID
	}
	var issues []Issue
	add := func(format string, args ...any) {
		issues = append(issues, Issue{Line: lineNo, ID: id, Message: fmt.Sprintf(format, args...)})
	}

	switch {
	case raw.ID == nil || strings.TrimSpace(id) == "":
		add("id is required")
	case seen[id] > 0:
		add("id is already used on line %d", seen[id])
	default:
		seen[id] = lineNo
	}
	if raw.Question == nil || strings.TrimSpace(*raw.Question) == "" {
		add("question is required")
	}
	if raw.Answerable == nil {
		add("answerable is required (true or false)")
	}
	switch {
	case raw.Split == nil:
		add("split is required (%q or %q)", SplitDev, SplitSealed)
	case *raw.Split != SplitDev && *raw.Split != SplitSealed:
		add("split %q is not %q or %q", *raw.Split, SplitDev, SplitSealed)
	}

	if raw.Answerable != nil {
		switch {
		case *raw.Answerable && len(raw.Evidence) == 0:
			add("an answerable question needs at least one evidence set")
		case !*raw.Answerable && len(raw.Evidence) > 0:
			add("an unanswerable question must have no evidence_sets")
		}
	}
	for si, set := range raw.Evidence {
		if len(set) == 0 {
			add("evidence_sets[%d] is empty", si)
			continue
		}
		for pi, span := range set {
			for _, problem := range checkSpan(span, corpus) {
				add("evidence_sets[%d][%d]: %s", si, pi, problem)
			}
		}
	}
	if len(issues) > 0 {
		return Question{}, issues
	}
	return Question{
		ID:         id,
		Question:   *raw.Question,
		Answerable: *raw.Answerable,
		Evidence:   raw.Evidence,
		Split:      *raw.Split,
		Topic:      raw.Topic,
		Type:       raw.Type,
	}, nil
}

func checkSpan(span Span, corpus *Corpus) []string {
	doc, ok := corpus.Lookup(span.File)
	if !ok {
		return []string{fmt.Sprintf("file %q is not in the corpus manifest", span.File)}
	}
	var problems []string
	if span.Start < 1 {
		problems = append(problems, fmt.Sprintf("start %d must be at least 1 (lines are 1-based)", span.Start))
	}
	if span.End < span.Start {
		problems = append(problems, fmt.Sprintf("end %d is before start %d", span.End, span.Start))
	}
	if span.End > doc.Lines {
		problems = append(problems, fmt.Sprintf("end %d is past the last line of %q (%d lines)", span.End, span.File, doc.Lines))
	}
	return problems
}
