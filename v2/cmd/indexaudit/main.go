// Command indexaudit compares what the knowledge record says with what the
// knowledge index holds, and reports the difference.
//
// The command only reports. It never deletes a key, never re-indexes a
// document and never refuses to run: an operator who wants to see whether the
// index is short gets the answer without the tool touching anything.
//
// The startup audit runs the same comparison with the same judgement; this
// command is the manual way to ask the question between two starts.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"GopherAI/common/mysql"
	redisCommon "GopherAI/common/redis"
	"GopherAI/config"
	"GopherAI/internal/indexaudit"

	"github.com/gin-gonic/gin"
)

func main() {
	configPath := flag.String("config", "config/config.toml", "GopherAI configuration file")
	timeout := flag.Duration("timeout", 5*time.Minute, "overall timeout")
	flag.Parse()

	loaded, err := config.Load(*configPath)
	if err != nil {
		fatal("load configuration: %v", err)
	}
	if err = config.Use(loaded); err != nil {
		fatal("apply configuration: %v", err)
	}
	gin.SetMode(gin.ReleaseMode)
	db, err := mysql.OpenConfigured()
	if err != nil {
		fatal("connect mysql: %v", err)
	}
	redisCommon.Init()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	auditor := indexaudit.NewAuditor(
		indexaudit.MySQLSource{DB: db, ExcludeInFlight: true},
		indexaudit.RedisCounter{Client: redisCommon.Rdb},
		nil,
		func(format string, args ...any) { fmt.Printf(format+"\n", args...) },
	)
	report, auditErr := auditor.Audit(ctx)
	printReport(report)
	if auditErr != nil {
		fatal("audit knowledge index: %v", auditErr)
	}
}

func printReport(report indexaudit.Report) {
	fmt.Printf("compared=%d blocks=%d missing=%d extra=%d\n", report.Compared, report.Blocks, len(report.Missing), len(report.Extra))
	for _, blocker := range report.Blockers {
		fmt.Printf("blocker=%q\n", blocker)
	}
	for _, document := range report.Missing {
		fmt.Printf("document=%s user=%d version=%d expected=%d actual=%d rebuild_command=%q\n",
			document.DocumentID, document.UserID, document.Version, document.Expected, document.Actual, indexaudit.RebuildCommand(document.DocumentID))
	}
	for _, document := range report.Extra {
		fmt.Printf("document=%s user=%d version=%d expected=%d actual=%d note=%q\n",
			document.DocumentID, document.UserID, document.Version, document.Expected, document.Actual, "the index holds more blocks than the record says")
	}
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(2)
}
