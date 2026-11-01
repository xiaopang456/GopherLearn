// Command l23 turns the L2/L3 scorer's outcomes into the evaluation's tables.
//
//	l23 report -outcomes DIR -baseline ARM    success rates, pass^k and paired comparisons; offline
//
// The scorer (eval/l2l3/score.mjs) judges each trial and writes outcomes.jsonl; this
// program only reads that file. It calls no service, so it can be run again and gives the
// same bytes.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/xiaopang456/GopherLearn/eval/internal/l23"
	"github.com/xiaopang456/GopherLearn/eval/internal/pathguard"
)

const usage = `usage: l23 report -outcomes <dir> [-baseline <arm>] [-seed N] [-resamples N] [-out <dir>]

  -outcomes   directory holding outcomes.jsonl and outcomes.meta.json (from score.mjs)
  -baseline   the arm the others are compared with (required when there is more than one arm)
  -seed       bootstrap seed (default 20261030)
  -resamples  bootstrap resamples per comparison (default 2000)
  -out        where report.md and report.json go (default: the outcomes directory)
`

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "report" {
		fmt.Fprint(stderr, usage)
		return 2
	}
	flags := flag.NewFlagSet("report", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dir := flags.String("outcomes", "", "outcomes directory")
	baseline := flags.String("baseline", "", "baseline arm")
	seed := flags.Int64("seed", 20261030, "bootstrap seed")
	resamples := flags.Int("resamples", 2000, "bootstrap resamples")
	out := flags.String("out", "", "output directory")
	if err := flags.Parse(args[1:]); err != nil {
		return 2
	}
	if *dir == "" {
		fmt.Fprintln(stderr, "l23 report: -outcomes is required")
		return 2
	}
	if *resamples < 100 || *resamples > 100000 {
		fmt.Fprintln(stderr, "l23 report: -resamples must be in 100..100000")
		return 2
	}
	target := *out
	if target == "" {
		target = *dir
	}
	for name, path := range map[string]string{"-outcomes": *dir, "-out": target} {
		if err := pathguard.Check(name, path); err != nil {
			fmt.Fprintf(stderr, "l23 report: %v\n", err)
			return 1
		}
	}

	outcomes, meta, err := l23.ReadDir(*dir)
	if err != nil {
		fmt.Fprintf(stderr, "l23 report: %v\n", err)
		return 1
	}
	if meta.Outcomes != len(outcomes) {
		fmt.Fprintf(stderr, "l23 report: outcomes.meta.json says %d outcomes but outcomes.jsonl has %d\n", meta.Outcomes, len(outcomes))
		return 1
	}
	opts := l23.Options{Baseline: *baseline, Seed: *seed, Resamples: *resamples}
	stats, err := l23.Compute(outcomes, opts)
	if err != nil {
		fmt.Fprintf(stderr, "l23 report: %v\n", err)
		return 1
	}
	data, err := l23.JSON(stats)
	if err != nil {
		fmt.Fprintf(stderr, "l23 report: %v\n", err)
		return 1
	}
	if err := os.MkdirAll(target, 0o700); err != nil {
		fmt.Fprintf(stderr, "l23 report: %v\n", err)
		return 1
	}
	if err := os.WriteFile(filepath.Join(target, "report.md"), []byte(l23.Markdown(stats, meta, opts)), 0o600); err != nil {
		fmt.Fprintf(stderr, "l23 report: %v\n", err)
		return 1
	}
	if err := os.WriteFile(filepath.Join(target, "report.json"), data, 0o600); err != nil {
		fmt.Fprintf(stderr, "l23 report: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "reported %d outcomes from %d arm(s) -> %s\n", len(outcomes), len(stats.Arms), target)
	return 0
}
