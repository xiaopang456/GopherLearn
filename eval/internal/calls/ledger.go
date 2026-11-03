package calls

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// CallSchema is the schema string of a call ledger line. A ledger that says
// anything else is refused: a call ledger that changed shape must not be read
// as if it had not.
const CallSchema = "call.v1"

// ReadCalls reads a call ledger: one model call per line, in the call.v1
// format. The identity fields are optional; a call without them is read as
// un-attributable rather than dropped.
func ReadCalls(r io.Reader) ([]Call, error) {
	var out []Call
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 1<<20), 8<<20)
	for line := 1; scanner.Scan(); line++ {
		text := bytes.TrimSpace(scanner.Bytes())
		if len(text) == 0 {
			continue
		}
		var entry struct {
			Schema  string `json:"schema"`
			At      string `json:"at"`
			Lane    string `json:"lane"`
			Model   string `json:"model"`
			Stream  bool   `json:"stream"`
			Run     string `json:"run"`
			Session string `json:"session"`
			Phase   string `json:"phase"`
			Usage   *struct {
				Prompt     *int64 `json:"prompt"`
				Completion *int64 `json:"completion"`
				Total      *int64 `json:"total"`
				Cached     *int64 `json:"cached"`
				Reasoning  *int64 `json:"reasoning"`
			} `json:"usage"`
		}
		decoder := json.NewDecoder(bytes.NewReader(text))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&entry); err != nil {
			return nil, fmt.Errorf("call ledger line %d: %w", line, err)
		}
		if entry.Schema != CallSchema {
			return nil, fmt.Errorf("call ledger line %d: schema is %q, want %q", line, entry.Schema, CallSchema)
		}
		call := Call{Lane: entry.Lane, Model: entry.Model, Stream: entry.Stream, RunID: entry.Run, SessionID: entry.Session, Phase: entry.Phase}
		if entry.At != "" {
			at, err := time.Parse(time.RFC3339Nano, entry.At)
			if err != nil {
				return nil, fmt.Errorf("call ledger line %d: at %q is not a time: %w", line, entry.At, err)
			}
			call.At = at
		}
		if entry.Usage != nil {
			call.Usage = &Usage{
				Prompt:     entry.Usage.Prompt,
				Completion: entry.Usage.Completion,
				Total:      entry.Usage.Total,
				Cached:     entry.Usage.Cached,
				Reasoning:  entry.Usage.Reasoning,
			}
		}
		out = append(out, call)
	}
	return out, scanner.Err()
}

// ReadCallFile reads a call ledger from a file.
func ReadCallFile(path string) ([]Call, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	calls, err := ReadCalls(file)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return calls, nil
}

// WriteCalls writes a call ledger: one model call per line, in the call.v1
// format. The same calls always give the same bytes.
func WriteCalls(w io.Writer, list []Call) error {
	encoder := bufio.NewWriter(w)
	for _, call := range list {
		entry := struct {
			Schema  string `json:"schema"`
			At      string `json:"at"`
			Lane    string `json:"lane"`
			Model   string `json:"model"`
			Stream  bool   `json:"stream"`
			Run     string `json:"run,omitempty"`
			Session string `json:"session,omitempty"`
			Phase   string `json:"phase,omitempty"`
			Usage   *struct {
				Prompt     *int64 `json:"prompt"`
				Completion *int64 `json:"completion"`
				Total      *int64 `json:"total"`
				Cached     *int64 `json:"cached"`
				Reasoning  *int64 `json:"reasoning"`
			} `json:"usage,omitempty"`
		}{Schema: CallSchema, Lane: call.Lane, Model: call.Model, Stream: call.Stream, Run: call.RunID, Session: call.SessionID, Phase: call.Phase}
		if !call.At.IsZero() {
			entry.At = call.At.UTC().Format(time.RFC3339Nano)
		}
		if call.Usage != nil {
			entry.Usage = &struct {
				Prompt     *int64 `json:"prompt"`
				Completion *int64 `json:"completion"`
				Total      *int64 `json:"total"`
				Cached     *int64 `json:"cached"`
				Reasoning  *int64 `json:"reasoning"`
			}{Prompt: call.Usage.Prompt, Completion: call.Usage.Completion, Total: call.Usage.Total, Cached: call.Usage.Cached, Reasoning: call.Usage.Reasoning}
		}
		data, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		if _, err := encoder.Write(append(data, '\n')); err != nil {
			return err
		}
	}
	return encoder.Flush()
}

// WriteCallFile writes a call ledger into a directory, refusing to overwrite.
func WriteCallFile(dir, name string, list []Call) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, name)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	if err := WriteCalls(file, list); err != nil {
		file.Close()
		return "", err
	}
	return path, file.Close()
}

// ReadRuns reads a provider run ledger: one line per event of a trial. The
// lines of a trial are put back together by their arm, question and repeat,
// so a run's identity comes out of the ledger rather than out of a time
// window.
//
// The events read are:
//   - "run": the trial started; it names the session.
//   - "result": the answer came back; it names the run and the time.
//   - "steps": the run view was read; it names the run when the result did not.
//
// The other events of a ledger (a refusal, an exception, a cleanup) say
// nothing about a run's cost and are ignored.
func ReadRuns(r io.Reader) ([]Run, error) {
	type key struct {
		arm, qid string
		rep      int
	}
	order := []key{}
	runs := map[key]*Run{}
	decoder := json.NewDecoder(bufio.NewReaderSize(r, 1<<20))
	for {
		var entry map[string]json.RawMessage
		if err := decoder.Decode(&entry); err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("run ledger: %w", err)
		}
		phase := rawString(entry["phase"])
		switch phase {
		case "run", "result", "steps":
		default:
			continue
		}
		arm, qid, ok := rawIdentity(entry)
		if !ok {
			return nil, fmt.Errorf("run ledger: a %q event has no arm, question or repeat", phase)
		}
		rep := 0
		if raw, ok := entry["rep"]; ok {
			value, err := strconv.Atoi(string(raw))
			if err != nil {
				return nil, fmt.Errorf("run ledger: rep %s is not a number", string(raw))
			}
			rep = value
		}
		k := key{arm: arm, qid: qid, rep: rep}
		run, exists := runs[k]
		if !exists {
			run = &Run{Arm: arm, QID: qid, Rep: rep}
			runs[k] = run
			order = append(order, k)
		}
		switch phase {
		case "run":
			if session := rawString(entry["sessionId"]); session != "" {
				run.SessionID = session
			}
		case "result":
			if id := rawString(entry["runId"]); id != "" {
				run.RunID = id
			}
			if at := rawTime(entry["answeredAt"]); !at.IsZero() {
				run.AnsweredAt = at
			} else if at := rawTime(entry["at"]); !at.IsZero() {
				run.AnsweredAt = at
			}
			if raw, ok := entry["modelCalls"]; ok {
				var calls int
				if err := json.Unmarshal(raw, &calls); err == nil {
					run.ReportedModelCalls = &calls
				}
			}
		case "steps":
			if id := rawString(entry["runId"]); id != "" && run.RunID == "" {
				run.RunID = id
			}
		}
	}
	out := make([]Run, 0, len(order))
	for _, k := range order {
		out = append(out, *runs[k])
	}
	return out, nil
}

// ReadRunFile reads a provider run ledger from a file.
func ReadRunFile(path string) ([]Run, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	runs, err := ReadRuns(file)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return runs, nil
}

func rawString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return ""
	}
	return value
}

func rawTime(raw json.RawMessage) time.Time {
	if len(raw) == 0 {
		return time.Time{}
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return time.Time{}
	}
	at, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}
	}
	return at
}

func rawIdentity(entry map[string]json.RawMessage) (string, string, bool) {
	arm, qid := rawString(entry["arm"]), rawString(entry["qid"])
	if arm == "" || qid == "" {
		return "", "", false
	}
	return arm, qid, true
}
