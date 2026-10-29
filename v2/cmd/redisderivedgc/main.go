// Command redisderivedgc audits and optionally removes Redis HASH records that
// no longer have an authoritative MySQL identity.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"GopherAI/common/mysql"
	redisCommon "GopherAI/common/redis"
	"GopherAI/config"
	"GopherAI/internal/derivedgc"

	"github.com/gin-gonic/gin"
)

func main() {
	configPath := flag.String("config", "config/config.toml", "GopherAI configuration file")
	apply := flag.Bool("apply", false, "delete audited orphan HASH keys")
	writersStopped := flag.Bool("writers-stopped", false, "assert that backend derived-view writers are stopped")
	confirm := flag.String("confirm", "", "required apply confirmation phrase")
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
	cleaner, err := derivedgc.New(db, redisCommon.Rdb)
	if err != nil {
		fatal("initialize derived gc: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	now := time.Now().UTC()
	if !*apply {
		report, auditErr := cleaner.Audit(ctx, now)
		printReport("DRY-RUN", report)
		if auditErr != nil {
			fatal("audit derived hashes: %v", auditErr)
		}
		if !report.Ready() {
			os.Exit(1)
		}
		return
	}
	report, applyErr := cleaner.Apply(ctx, now, *writersStopped, *confirm)
	printReport("APPLY", report)
	if applyErr != nil {
		fatal("apply derived hash gc: %v", applyErr)
	}
}

func printReport(mode string, report derivedgc.Report) {
	fmt.Printf("mode=%s families=%d ready=%t deleted=%d\n", mode, len(report.Families), report.Ready(), report.Deleted)
	for _, family := range report.Families {
		fmt.Printf("family=%s mysql_expected=%d redis_hashes=%d index_docs=%d keep=%d orphan=%d unexpected_types=%d ready=%t\n",
			family.Family, family.Expected, family.RedisHashes, family.IndexDocs, family.Kept, family.Orphans, family.UnexpectedTypes, family.Ready())
		ids := make([]int64, 0, len(family.OrphansByUser))
		for id := range family.OrphansByUser {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		for _, id := range ids {
			fmt.Printf("family=%s orphan_user=%d count=%d\n", family.Family, id, family.OrphansByUser[id])
		}
		for _, blocker := range family.Blockers {
			fmt.Printf("family=%s blocker=%q\n", family.Family, blocker)
		}
	}
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(2)
}
