// Command l4-summary replays one conversation through the product's rolling
// summarizer and stops at the first compaction failure, keeping the whole
// error, the stage it came from, and the body the model returned.
//
// The model endpoint is the operator's counting proxy, so the credentials stay
// with the operator and every call is metered. Nothing in the product is
// replaced and no request body is rewritten.
//
//	go run ./cmd/l4-summary -repository .. -input <history.json> -scenario <id> \
//	  -proxy http://127.0.0.1:18150 -chat-model deepseek-flash -output-dir <dir>
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/xiaopang456/GopherLearn/eval/internal/conversation"
	"github.com/xiaopang456/GopherLearn/eval/internal/pathguard"
)

func run(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("l4-summary", flag.ContinueOnError)
	repository := flags.String("repository", "..", "product checkout root")
	input := flags.String("input", "", "private turn array of one conversation")
	scenario := flags.String("scenario", "", "scenario identifier")
	condition := flags.String("condition", "main", "condition label of the history")
	output := flags.String("output-dir", "", "new private output directory")
	proxy := flags.String("proxy", "", "counting proxy base URL")
	chatModel := flags.String("chat-model", "", "chat model name")
	timeout := flags.Duration("timeout", 0, "replay deadline")
	reclassify := flags.String("reclassify", "", "a saved diagnosis to read again, without a model call")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("no positional arguments are accepted")
	}
	if err := pathguard.CheckAll(map[string]string{"output": *output}); err != nil {
		return err
	}
	if _, err := os.Stat(*output); err == nil {
		return fmt.Errorf("output already exists")
	} else if !os.IsNotExist(err) {
		return err
	}
	if *reclassify != "" {
		return reclassifySaved(*reclassify, *output, out)
	}
	if strings.TrimSpace(*input) == "" || strings.TrimSpace(*scenario) == "" {
		return fmt.Errorf("input and scenario are required")
	}
	if strings.TrimSpace(*proxy) == "" || strings.TrimSpace(*chatModel) == "" {
		return fmt.Errorf("proxy and chat-model are required")
	}
	if err := pathguard.Check("input", *input); err != nil {
		return err
	}
	if _, err := os.Stat(*output); err == nil {
		return fmt.Errorf("output already exists")
	} else if !os.IsNotExist(err) {
		return err
	}
	turns, err := readTurns(*input)
	if err != nil {
		return err
	}
	interrupted, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	deadline := conversation.SummaryDiagnosisTimeout
	if *timeout > 0 {
		deadline = *timeout
	}
	ctx, cancel := context.WithTimeout(interrupted, deadline)
	defer cancel()
	diagnosis, err := conversation.DiagnoseSummaryFailure(ctx, *repository, *scenario, *condition, turns, *proxy, *chatModel)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*output, 0700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(diagnosis, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*output, "diagnosis.json"), append(body, '\n'), 0600); err != nil {
		return err
	}
	fmt.Fprintf(out, "scenario=%s stage=%s failure_turn=%d error=%s\n", diagnosis.Scenario, diagnosis.Stage, diagnosis.FailureTurn, diagnosis.ErrorText)
	return nil
}

// reclassifySaved reads a diagnosis again without a model call. The step the
// failure came from and the item counts of the body the model returned can be
// read from the saved file, so nothing has to be paid for twice.
func reclassifySaved(source, output string, out io.Writer) error {
	body, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	var diagnosis conversation.SummaryDiagnosis
	if err := json.Unmarshal(body, &diagnosis); err != nil {
		return err
	}
	diagnosis = conversation.ReclassifyDiagnosis(diagnosis)
	encoded, err := json.MarshalIndent(diagnosis, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(output, 0700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(output, "diagnosis.json"), append(encoded, '\n'), 0600); err != nil {
		return err
	}
	fmt.Fprintf(out, "scenario=%s stage=%s failure_turn=%d error=%s\n", diagnosis.Scenario, diagnosis.Stage, diagnosis.FailureTurn, diagnosis.ErrorText)
	return nil
}

func readTurns(path string) ([]conversation.Turn, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > 8<<20 {
		return nil, fmt.Errorf("turn file exceeds limit")
	}
	var turns []conversation.Turn
	decoder := json.NewDecoder(io.LimitReader(file, 8<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&turns); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("turn file contains trailing data")
	}
	if len(turns) < 1 || len(turns) > 256 {
		return nil, fmt.Errorf("turn count must be 1 through 256")
	}
	return turns, nil
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "l4-summary:", err)
		os.Exit(1)
	}
}

var _ = time.Second
