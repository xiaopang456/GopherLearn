// Command l4-read measures the read side of the layered summary.
//
// The writer's command replays a conversation and reports what was written.
// This one goes the rest of the way: the segments and the board are stored,
// the product's own context builder assembles the window a real turn would
// get, and the product's own model answers the question from that window. The
// report says what reached the model, whether the segment carrying the answer
// was one of them, whether an old value was marked as replaced, whether the
// ids the window authorised are exactly the ids it showed, and whether the
// answer named the current value rather than the superseded one.
//
// The model endpoint is the operator's counting proxy, so the credentials
// stay with the operator and every call is metered.
//
//	go run ./cmd/l4-read -repository .. -input <turns.json> -scenario <id> \
//	  -question-file <question.txt> -evidence-turns 30,45 \
//	  -accepted "每周六" -forbidden "每周日" \
//	  -proxy http://127.0.0.1:18200 -chat-model deepseek-flash -output-dir <dir>
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
	"strconv"
	"strings"

	"github.com/xiaopang456/GopherLearn/eval/internal/conversation"
	"github.com/xiaopang456/GopherLearn/eval/internal/pathguard"
)

// listFlag collects a flag that may be repeated.
//
// The values are answer-key phrases, and a phrase can carry a comma. Repeating
// the flag keeps the separator out of the value.
type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, "; ") }

func (l *listFlag) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("an answer-key phrase cannot be empty")
	}
	*l = append(*l, value)
	return nil
}

func run(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("l4-read", flag.ContinueOnError)
	repository := flags.String("repository", "..", "product checkout root")
	input := flags.String("input", "", "private turn array of one conversation")
	scenario := flags.String("scenario", "", "scenario identifier")
	question := flags.String("question", "", "the question the read side is measured against")
	questionFile := flags.String("question-file", "", "file holding the question (UTF-8)")
	evidenceTurns := flags.String("evidence-turns", "", "comma separated turn numbers the answer comes from")
	proxy := flags.String("proxy", "", "counting proxy base URL")
	chatModel := flags.String("chat-model", "", "chat model name")
	output := flags.String("output-dir", "", "new private output directory")
	capabilities := flags.String("tools", "read_history_message,browse_summary_segments",
		"comma separated tool names this turn is rendered against")
	stage := flags.String("stage", conversation.LayeredStageFull,
		"full (seed, read, answer) or batches (seed only)")
	scaledConfig := flags.String("scaled-config", "",
		"[agentConfig] TOML overlay for a scaled run; absent keeps the product defaults")
	timeout := flags.Duration("timeout", 0, "read deadline")
	checkConfig := flags.Bool("check-config", false,
		"load the product's configuration, report what it says, and exit without calling a model")
	expectMode := flags.String("expect-summary-segment-mode", "on",
		"the layered-summary mode the run needs (on, shadow or off)")
	expectAssistantContent := flags.String("expect-assistant-content", "on",
		"the assistant-content switch the run needs (on or off)")
	expectDebugDump := flags.String("expect-debug-dump", "on",
		"the debug-dump switch the run needs (on or off)")
	var accepted, forbidden listFlag
	flags.Var(&accepted, "accepted", "an answer the run passes on (repeatable)")
	flags.Var(&forbidden, "forbidden", "an answer the run fails on (repeatable)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("no positional arguments are accepted")
	}
	if *checkConfig {
		// The overlay goes with the check: the check has to read the same
		// configuration the run hands the product, or it verifies a run that
		// does not happen.
		return runConfigCheck([]string{
			"-repository", *repository,
			"-scaled-config", *scaledConfig,
			"-expect-summary-segment-mode", *expectMode,
			"-expect-assistant-content", *expectAssistantContent,
			"-expect-debug-dump", *expectDebugDump,
			"-chat-model", *chatModel,
		}, os.Stdout)
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
	if *questionFile != "" {
		raw, readErr := os.ReadFile(*questionFile)
		if readErr != nil {
			return readErr
		}
		*question = strings.TrimSpace(string(raw))
	}
	if strings.TrimSpace(*question) == "" {
		return fmt.Errorf("a question is required, by flag or by file")
	}
	turns, err := readTurns(*input)
	if err != nil {
		return err
	}
	evidence, err := parseTurns(*evidenceTurns)
	if err != nil {
		return err
	}
	toolNames := parseToolNames(*capabilities)
	interrupted, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	deadline := conversation.LayeredReadTimeout
	if *timeout > 0 {
		deadline = *timeout
	}
	ctx, cancel := context.WithTimeout(interrupted, deadline)
	defer cancel()
	if err = os.MkdirAll(*output, 0700); err != nil {
		return err
	}
	read, runErr := conversation.RunLayeredRead(ctx, *repository, *scenario, *question,
		turns, evidence, accepted, forbidden, toolNames, *stage, *proxy, *chatModel, *scaledConfig, *output)
	fmt.Fprintf(out, "scenario=%s seeded=%d/%d board_in_window=%t segments=%v evidence=%v verdict=%s\n",
		read.Scenario, read.SeededSegments, read.SeededBoard, read.WhiteboardInWindow,
		read.SegmentsInWindow, read.EvidenceSeqs, read.Verdict)
	if len(read.ToolChecks) > 0 {
		// Authorised is the product's grant set answering, not the tool
		// running: the row says so with Exercised. Pending rows are left out
		// of the tally entirely - the browse tool decides reachability inside
		// itself, and counting this harness's guess would put an opinion in a
		// column of product decisions.
		authorised, exercised, pending := 0, 0, 0
		for _, check := range read.ToolChecks {
			if check.Pending {
				pending++
				continue
			}
			if check.Authorised {
				authorised++
			}
			if check.Exercised {
				exercised++
			}
		}
		fmt.Fprintf(out, "authority_checks=%d authorised=%d pending=%d tools_exercised=%d grants=%d\n",
			len(read.ToolChecks), authorised, pending, exercised, read.Grants)
	}
	// The result and the ledger of the model calls of this run, written next
	// to each other. Every call carries the role it was made in and the
	// purpose it was made for, and a ledger that only exists inside the result
	// file cannot be read by anything that does not parse the whole result.
	if err = conversation.WriteLayeredReadFiles(*output, read); err != nil {
		return err
	}
	return runErr
}

// parseToolNames reads the tool table a run is rendered against.
//
// The names are what the model would see in its tool list, because that is
// what the product's renderer reads. A name nothing recognises contributes no
// capability and changes nothing, which is the same rule the product applies
// to a tool it has never heard of.
func parseToolNames(value string) []string {
	names := make([]string, 0, 4)
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			names = append(names, part)
		}
	}
	return names
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
		fmt.Fprintln(os.Stderr, "l4-read:", err)
		os.Exit(1)
	}
}
