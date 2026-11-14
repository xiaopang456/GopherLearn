package main

// The check a run makes on its own configuration before it spends anything.
//
// The harness runs the product through its isolated entry: the entry is
// handed one explicit configuration file and reads nothing else, so a switch
// said in the environment never reaches it. A run that measured the wrong
// arm - or a run that meant to keep the per-batch evidence and did not - is
// not caught by a clean git tree or by a file hash: both of those say what
// the file is, not what the product will do with it.
//
// So the check loads the configuration the way the run does and reports what
// the product will actually do. It is a separate command because it has to
// answer that question without a model, without a database and without a
// proxy - so it can be run before the first paid call.
//
//   go run ./cmd/l4-read -check-config -repository <checkout>
//
// The check fails when the configuration does not say what the run needs it
// to say, and it prints what it found either way.

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/xiaopang456/GopherLearn/eval/internal/conversation"
)

func runConfigCheck(args []string, out *os.File) error {
	flags := flag.NewFlagSet("l4-read -check-config", flag.ContinueOnError)
	repository := flags.String("repository", "..", "product checkout root")
	expectAssistantContent := flags.String("expect-assistant-content", "on",
		"the assistant-content switch the run needs (on or off)")
	expectDebugDump := flags.String("expect-debug-dump", "on",
		"the debug-dump switch the run needs (on or off)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("no positional arguments are accepted")
	}

	report, err := conversation.CheckProductConfiguration(*repository)
	if err != nil {
		return err
	}
	report.Expected = conversation.ExpectedConfiguration{
		SummaryAssistantContent: strings.TrimSpace(*expectAssistantContent),
		SummarySegmentDebugDump: strings.TrimSpace(*expectDebugDump) == "on",
	}
	report.Matches = report.SummaryAssistantContent == report.Expected.SummaryAssistantContent &&
		report.SummarySegmentDebugDump == report.Expected.SummarySegmentDebugDump

	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	fmt.Fprintln(out, string(encoded))
	if !report.Matches {
		return fmt.Errorf("the configuration does not say what the run needs: "+
			"assistant content is %q (want %q), debug dump is %v (want %v)",
			report.SummaryAssistantContent, report.Expected.SummaryAssistantContent,
			report.SummarySegmentDebugDump, report.Expected.SummarySegmentDebugDump)
	}
	return nil
}
