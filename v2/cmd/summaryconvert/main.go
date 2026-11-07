// Command summaryconvert reports what shape each stored session summary has,
// and converts the shapes this build cannot read into the one it can.
//
// It reads the summary table of one deployment. Three writers have put content
// into that table: the rolling summary of this build, the ordinary conversation
// chain that stored the model's prose answer, and the structured summarizer that
// predates this schema and wrote a document whose citations are strings. Only the
// first is readable here, and an unreadable row is not an error at runtime - the
// rolling summary reports it as unusable and leaves that session without a
// compacted history until it is converted.
//
// The command does nothing unless told to. With no flags it is a preview: it
// reads, classifies, and says what a writing run would do, and it makes no
// database write and no provider call.
//
//	preview a deployment                summaryconvert -config config/config.toml
//	write the mechanical conversions    summaryconvert -config config/config.toml -apply
//	also re-express prose summaries     summaryconvert -config config/config.toml -apply -with-model
//
// The prose arm is the one that costs something: a prose summary has no groups
// to carry over, so it takes one model call per row to say the same thing in this
// schema's shape. Without -with-model those rows are listed with that reason and
// left exactly as they are. A row that cannot be converted is never rewritten and
// never truncated: it is reported.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"GopherAI/common/aihelper"
	"GopherAI/common/mysql"
	"GopherAI/config"
	"GopherAI/internal/summaryconvert"
	"GopherAI/model"

	"github.com/gin-gonic/gin"
)

func main() {
	configPath := flag.String("config", "config/config.toml", "GopherAI configuration file")
	apply := flag.Bool("apply", false, "write converted rows; without it this run only previews")
	withModel := flag.Bool("with-model", false, "re-express prose summaries with one model call per row; needs -apply")
	limit := flag.Int("limit", 512, "maximum summary rows to read in one run")
	messageLimit := flag.Int("message-limit", 512, "maximum covered messages a prose conversion may read")
	maxInputTokens := flag.Int("max-input-tokens", 8000, "maximum input tokens for one prose conversion")
	timeout := flag.Duration("timeout", 10*time.Minute, "overall timeout")
	flag.Parse()

	loaded, err := config.Load(*configPath)
	if err != nil {
		fatal("load configuration: %v", err)
	}
	if err := config.Use(loaded); err != nil {
		fatal("apply configuration: %v", err)
	}
	gin.SetMode(gin.ReleaseMode)
	db, err := mysql.OpenConfigured()
	if err != nil {
		fatal("connect mysql: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	options := summaryconvert.Options{
		Apply:        *apply,
		Limit:        *limit,
		MessageLimit: *messageLimit,
		Budget:       model.DefaultSummaryBudget(),
	}
	if *withModel && !*apply {
		fmt.Println("note: -with-model without -apply spends no call; prose rows are previewed only")
	}
	if *withModel && *apply {
		chatModel, err := aihelper.NewToolCallingChatModel(ctx)
		if err != nil {
			fatal("create the model for prose conversion: %v", err)
		}
		generator, err := summaryconvert.NewModelProseGenerator(chatModel, *maxInputTokens)
		if err != nil {
			fatal("prepare prose conversion: %v", err)
		}
		options.Prose = generator
	}

	report, err := summaryconvert.NewRunner(summaryconvert.NewStore(db), db, options).Run(ctx)
	if err != nil {
		fatal("convert stored summaries: %v", err)
	}
	printReport(report)
}

func printReport(report summaryconvert.Report) {
	shapes := report.CountByShape()
	actions := report.CountByAction()
	var calls, failures int
	for _, outcome := range report.Outcomes {
		calls += outcome.ModelCalls
		if outcome.Action == summaryconvert.ActionFailed {
			failures++
		}
		fmt.Printf("row=%s user=%d mode=%q shape=%s action=%s calls=%d dropped_ids=%d legacy_text_runes=%d reason=%q\n",
			outcome.Row.SessionID, outcome.Row.UserID, string(outcome.Row.Mode), outcome.Shape, outcome.Action,
			outcome.ModelCalls, len(outcome.DroppedEvidenceIDs), outcome.LegacyTextRunes, outcome.Reason)
	}
	fmt.Printf("read=%d more=%t shapes: current=%d legacy_json=%d prose=%d empty=%d damaged=%d\n",
		report.RowsRead, report.RowsWithMore,
		shapes[summaryconvert.ShapeCurrent], shapes[summaryconvert.ShapeLegacyJSON],
		shapes[summaryconvert.ShapeProse], shapes[summaryconvert.ShapeEmpty], shapes[summaryconvert.ShapeDamaged])
	fmt.Printf("actions: converted=%d would_convert=%d unchanged=%d failed=%d model_calls=%d\n",
		actions[summaryconvert.ActionConverted], actions[summaryconvert.ActionWouldConvert],
		actions[summaryconvert.ActionUnchanged], failures, calls)
	if report.RowsWithMore {
		fmt.Printf("warning: more rows exist than this run read; raise -limit and run again\n")
	}
	if failures > 0 {
		fatal("%d rows were not converted", failures)
	}
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(2)
}
