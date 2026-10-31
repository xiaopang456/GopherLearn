package validate

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/xiaopang456/GopherLearn/eval/internal/dataset"
)

// RawQuestion is a question line before the split is assigned.
//
// The split is read as a plain string so a file that has not been split yet
// can still be read; the dataset loader would reject it, but the splitter's
// job is to fill that field in.
type RawQuestion struct {
	ID         string                `json:"id"`
	Question   string                `json:"question"`
	Answerable *bool                 `json:"answerable"`
	Evidence   []dataset.EvidenceSet `json:"evidence_sets"`
	Split      string                `json:"split"`
	Topic      string                `json:"topic,omitempty"`
	Type       string                `json:"type,omitempty"`
}

// OutQuestion is the shape written back to the question file.
//
// The field order matches the schema in the evaluation README, and
// evidence_sets is omitted when there is none, so an unanswerable question
// stays acceptable to the dataset loader.
type OutQuestion struct {
	ID         string                `json:"id"`
	Question   string                `json:"question"`
	Answerable bool                  `json:"answerable"`
	Evidence   []dataset.EvidenceSet `json:"evidence_sets,omitempty"`
	Split      string                `json:"split"`
	Topic      string                `json:"topic,omitempty"`
	Type       string                `json:"type,omitempty"`
}

// ReadRawQuestions reads a question file without validating it.
func ReadRawQuestions(path string) ([]RawQuestion, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	var out []RawQuestion
	line := 0
	for scanner.Scan() {
		line++
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}
		var q RawQuestion
		if err := json.Unmarshal([]byte(text), &q); err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, line, err)
		}
		out = append(out, q)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// WriteQuestions writes questions back out, one JSON object per line.
func WriteQuestions(path string, questions []OutQuestion) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()
	writer := bufio.NewWriter(file)
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(false)
	for _, q := range questions {
		if err := encoder.Encode(q); err != nil {
			return err
		}
	}
	return writer.Flush()
}

// WriteMeta writes the sidecar annotations back out.
func WriteMeta(path string, meta []Meta) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()
	writer := bufio.NewWriter(file)
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(false)
	for _, m := range meta {
		if err := encoder.Encode(m); err != nil {
			return err
		}
	}
	return writer.Flush()
}
