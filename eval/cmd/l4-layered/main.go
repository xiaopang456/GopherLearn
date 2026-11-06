// Command l4-layered replays one conversation through the layered summary and
// writes what the summary made of it: how far it reached, how often a
// generation failed, what the board marked as replaced, whether the segment
// that carries the answer is the one the preselector picked, and what the
// calls cost.
//
// The mode decides the arm:
//
//	off     the old rolling summary, which is what the product does today
//	shadow  the segments and the board are written, and the old summary still
//	        serves the window
//	on      the segments and the board, and the old summary is not read or
//	        written at all
//
// Running "all" replays the same conversation once per mode, so the two arms
// see the same turns and the difference between their numbers is the
// difference between the two structures.
//
// The model endpoint is the operator's counting proxy, so the credentials stay
// with the operator and every call is metered.
//
//	go run ./cmd/l4-layered -repository .. -input <history.json> -scenario <id> \
//	  -mode all -proxy http://127.0.0.1:18200 -chat-model deepseek-flash -output-dir <dir>
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
	"strconv"
	"strings"
	"time"

	"github.com/xiaopang456/GopherLearn/eval/internal/conversation"
	"github.com/xiaopang456/GopherLearn/eval/internal/pathguard"
)

func run(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("l4-layered", flag.ContinueOnError)
	repository := flags.String("repository", "..", "product checkout root")
	input := flags.String("input", "", "private turn array of one conversation")
	scenario := flags.String("scenario", "", "scenario identifier")
	condition := flags.String("condition", "main", "condition label of the history")
	question := flags.String("question", "", "the question the retrieval is measured against")
	questionFile := flags.String("question-file", "", "file holding the question (UTF-8)")
	evidenceTurns := flags.String("evidence-turns", "", "comma separated turn numbers the answer comes from")
	mode := flags.String("mode", "all", "off, shadow, on or all")
	proxy := flags.String("proxy", "", "counting proxy base URL")
	chatModel := flags.String("chat-model", "", "chat model name")
	output := flags.String("output-dir", "", "new private output directory")
	timeout := flags.Duration("timeout", 0, "replay deadline")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("no positional arguments are accepted")
	}
	if err := pathguard.CheckAll(map[string]string{"output": *output}); err != nil {
		return err
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
	modes, err := layeredModes(*mode)
	if err != nil {
		return err
	}
	if *questionFile != "" {
		raw, readErr := os.ReadFile(*questionFile)
		if readErr != nil {
			return readErr
		}
		*question = strings.TrimSpace(string(raw))
	}
	turns, err := readTurns(*input)
	if err != nil {
		return err
	}
	evidence, err := parseTurns(*evidenceTurns)
	if err != nil {
		return err
	}
	interrupted, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	deadline := conversation.LayeredRunTimeout
	if *timeout > 0 {
		deadline = *timeout
	}
	ctx, cancel := context.WithTimeout(interrupted, deadline)
	defer cancel()
	if err = os.MkdirAll(*output, 0700); err != nil {
		return err
	}
	runs := make([]conversation.LayeredRun, 0, len(modes))
	for _, current := range modes {
		run, err := conversation.RunLayeredSummary(ctx, *repository, *scenario, *condition, turns,
			*question, evidence, *proxy, *chatModel, current)
		if err != nil {
			return err
		}
		runs = append(runs, run)
		fmt.Fprintf(out, "mode=%s covered=%d/%d failures=%d superseded=%d calls=%d\n",
			run.Mode, run.Coverage.CoveredMessageID, run.Coverage.TotalMessages,
			run.Failures.Total, run.Board.SupersededMarks, run.ModelCalls)
	}
	body, err := json.MarshalIndent(runs, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(*output, "runs.json"), append(body, '\n'), 0600)
}

// layeredModes reads the mode flag.
//
// "all" is the three modes in the order a comparison reads them: the arm that
// exists today, the arm that writes without being read, and the arm that is
// read.
func layeredModes(value string) ([]string, error) {
	value = strings.TrimSpace(value)
	if value == "" || value == "all" {
		return []string{"off", "shadow", "on"}, nil
	}
	for _, candidate := range conversation.LayeredModes {
		if candidate == value {
			return []string{value}, nil
		}
	}
	return nil, fmt.Errorf("mode must be off, shadow, on or all")
}

// parseTurns reads the turn numbers the answer comes from.
func parseTurns(value string) ([]int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	var turns []int
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		turn, err := strconv.Atoi(part)
		if err != nil {
			return nil, fmt.Errorf("invalid turn %q", part)
		}
		if turn < 1 {
			return nil, fmt.Errorf("turn numbers start at 1")
		}
		turns = append(turns, turn)
	}
	return turns, nil
}

// readTurns reads a private turn array.
//
// The file is read with a limit and without unknown fields, because a turn
// array is written by hand and a hand-written file can carry a field the
// harness does not know.
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
		fmt.Fprintln(os.Stderr, "l4-layered:", err)
		os.Exit(1)
	}
}

var _ = time.Second
