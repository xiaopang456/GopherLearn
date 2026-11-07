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
// database write. It never calls a model and needs no provider configuration.
//
//	preview a deployment        summaryconvert -config config/config.toml
//	write the conversions       summaryconvert -config config/config.toml -apply
//
// Conversion is mechanical and lossless. Prose becomes paragraph items of this
// schema's string groups; the old structured document keeps its groups and
// citations, and its legacy_text is carried into completed_items. A row that
// cannot be carried over without dropping something is refused and reported,
// never partially written.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"GopherAI/common/mysql"
	"GopherAI/config"
	"GopherAI/internal/summaryconvert"
	"GopherAI/model"

	"github.com/gin-gonic/gin"
)

func main() {
	configPath := flag.String("config", "config/config.toml", "GopherAI configuration file")
	apply := flag.Bool("apply", false, "write converted rows; without it this run only previews")
	limit := flag.Int("limit", 512, "maximum summary rows to read in one run")
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
		Apply:  *apply,
		Limit:  *limit,
		Budget: model.DefaultSummaryBudget(),
	}
	report, err := summaryconvert.NewRunner(summaryconvert.NewStore(db), options).Run(ctx)
	if err != nil {
		fatal("convert stored summaries: %v", err)
	}
	printReport(report)
}

func printReport(report summaryconvert.Report) {
	shapes := report.CountByShape()
	actions := report.CountByAction()
	var failures, refused int
	for _, outcome := range report.Outcomes {
		if outcome.Action == summaryconvert.ActionFailed {
			failures++
		}
		if outcome.Action == summaryconvert.ActionUnchanged &&
			(outcome.Shape == summaryconvert.ShapeDamaged ||
				outcome.Shape == summaryconvert.ShapeProse ||
				outcome.Shape == summaryconvert.ShapeLegacyJSON) {
			refused++
		}
		fmt.Printf("row=%s user=%d mode=%q shape=%s action=%s reason=%q\n",
			outcome.Row.SessionID, outcome.Row.UserID, string(outcome.Row.Mode), outcome.Shape, outcome.Action, outcome.Reason)
	}
	fmt.Printf("read=%d more=%t shapes: current=%d legacy_json=%d prose=%d empty=%d damaged=%d\n",
		report.RowsRead, report.RowsWithMore,
		shapes[summaryconvert.ShapeCurrent], shapes[summaryconvert.ShapeLegacyJSON],
		shapes[summaryconvert.ShapeProse], shapes[summaryconvert.ShapeEmpty], shapes[summaryconvert.ShapeDamaged])
	fmt.Printf("actions: converted=%d would_convert=%d unchanged=%d failed=%d refused=%d\n",
		actions[summaryconvert.ActionConverted], actions[summaryconvert.ActionWouldConvert],
		actions[summaryconvert.ActionUnchanged], failures, refused)
	if report.RowsWithMore {
		fmt.Printf("warning: more rows exist than this run read; raise -limit and run again\n")
	}
	if failures > 0 {
		fatal("%d rows were not written", failures)
	}
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(2)
}
