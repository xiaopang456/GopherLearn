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
//   go run ./cmd/l4-read -check-config -repository <checkout> \
//     -scaled-config <overlay the run uses> \
//     -expect-summary-segment-mode on -expect-assistant-content on \
//     -expect-debug-dump on
//
// The overlay is the same file the run hands the product. A run that passes
// the overlay to the product and not to the check measures one thing and
// verifies another, and the check would pass while the run measures the
// closed arm.
//
// The check fails when the configuration does not say what the run needs it
// to say, and it prints what it found either way: the mode the product will
// run in and the budgets the window will really render with. The report
// carries switches and numbers, and no key of any kind.

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/xiaopang456/GopherLearn/eval/internal/conversation"
)

func runConfigCheck(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("l4-read -check-config", flag.ContinueOnError)
	repository := flags.String("repository", "..", "product checkout root")
	scaledConfig := flags.String("scaled-config", "",
		"the [agentConfig] overlay the run hands the product; absent keeps the product defaults")
	expectMode := flags.String("expect-summary-segment-mode", "on",
		"the layered-summary mode the run needs (on, shadow or off)")
	expectAssistantContent := flags.String("expect-assistant-content", "on",
		"the assistant-content switch the run needs (on or off)")
	expectDebugDump := flags.String("expect-debug-dump", "on",
		"the debug-dump switch the run needs (on or off)")
	chatModel := flags.String("chat-model", "config-check-no-model",
		"the model name the run would use; the check never calls a model")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("no positional arguments are accepted")
	}

	report, err := conversation.CheckProductConfiguration(conversation.ConfigurationCheck{
		Repository:   *repository,
		ScaledConfig: strings.TrimSpace(*scaledConfig),
		ChatModel:    strings.TrimSpace(*chatModel),
	})
	if err != nil {
		return err
	}
	report.Expected = conversation.ExpectedConfiguration{
		SummarySegmentMode:      strings.TrimSpace(*expectMode),
		SummaryAssistantContent: strings.TrimSpace(*expectAssistantContent),
		SummarySegmentDebugDump: strings.TrimSpace(*expectDebugDump) == "on",
	}
	report.Matches = report.MatchesExpected(report.Expected)

	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	fmt.Fprintln(out, string(encoded))
	if !report.Matches {
		return fmt.Errorf("the configuration does not say what this run needs: "+
			"the mode is %q (want %q), the assistant content is %q (want %q), "+
			"the debug dump is %v (want %v); the effective budgets are "+
			"a whiteboard of %d, a resident block of %d, a retrieved block of %d and one segment of %d",
			report.SummarySegmentMode, report.Expected.SummarySegmentMode,
			report.SummaryAssistantContent, report.Expected.SummaryAssistantContent,
			report.SummarySegmentDebugDump, report.Expected.SummarySegmentDebugDump,
			report.Budgets.WhiteboardTokens, report.Budgets.ResidentSegmentTokens,
			report.Budgets.RetrievedSegmentTokens, report.Budgets.SegmentTokens)
	}
	return nil
}
