// Command redisindexcleanup audits and optionally drops known obsolete
// RediSearch index metadata. It never deletes HASH documents.
package main

import (
	redisCommon "GopherAI/common/redis"
	"GopherAI/config"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"
)

func main() {
	configPath := flag.String("config", "config/config.toml", "GopherAI configuration file")
	apply := flag.Bool("apply", false, "drop known legacy index metadata after all safety checks pass")
	confirm := flag.String("confirm", "", "required with -apply: "+redisCommon.LegacyIndexCleanupConfirmation)
	timeout := flag.Duration("timeout", 2*time.Minute, "overall timeout")
	flag.Parse()

	loaded, err := config.Load(*configPath)
	if err != nil {
		fatal("load configuration: %v", err)
	}
	if err = config.Use(loaded); err != nil {
		fatal("apply configuration: %v", err)
	}
	redisCommon.Init()
	if redisCommon.Rdb == nil {
		fatal("redis client is not configured")
	}
	defer redisCommon.Rdb.Close()
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	report, err := redisCommon.AuditManagedSearchIndexes(ctx, redisCommon.Rdb)
	if err != nil {
		fatal("audit managed indexes: %v", err)
	}
	printReport(report)
	if !*apply {
		if !report.SafeToDrop() {
			fatal("dry-run found unsafe index state")
		}
		fmt.Printf("dry-run only; %d known legacy index(es) are eligible\n", len(report.LegacyIndexes()))
		return
	}
	if *confirm != redisCommon.LegacyIndexCleanupConfirmation {
		fatal("-apply requires -confirm %s", redisCommon.LegacyIndexCleanupConfirmation)
	}
	after, err := redisCommon.DropAuditedLegacySearchIndexes(ctx, redisCommon.Rdb, *confirm)
	if err != nil {
		printReport(after)
		fatal("cleanup legacy indexes: %v", err)
	}
	fmt.Printf("cleanup complete; dropped=%v\n", report.LegacyIndexes())
	printReport(after)
}

func printReport(report redisCommon.SearchIndexAuditReport) {
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		fatal("encode audit report: %v", err)
	}
	fmt.Println(string(encoded))
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(2)
}
