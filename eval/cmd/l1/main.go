// Command l1 evaluates retrieval quality (layer L1) of the GopherLearn RAG
// service against a private corpus and question set.
//
//	l1 check -config eval.toml.local     validate the inputs; offline, free
//
// Every mode refuses to start when a configured path sits inside a git working
// tree, and none of them runs a paid call unless a limit in the configuration
// allows it.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/xiaopang456/GopherLearn/eval/internal/config"
	"github.com/xiaopang456/GopherLearn/eval/internal/dataset"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

const usage = `usage: l1 <mode> -config <file>

modes:
  check   validate the configuration, corpus manifest and question file (offline)
`

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	mode, rest := args[0], args[1:]
	switch mode {
	case "check":
		return runCheck(rest, stdout, stderr)
	case "-h", "-help", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "l1: unknown mode %q\n\n%s", mode, usage)
		return 2
	}
}

// loadInputs reads the configuration and everything it points at, after the
// path guard has accepted every location.
func loadInputs(args []string, name string, stderr io.Writer) (*config.Config, *dataset.Corpus, *dataset.Dataset, bool) {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "", "evaluation configuration file (TOML)")
	if err := flags.Parse(args); err != nil {
		return nil, nil, nil, false
	}
	if *configPath == "" {
		fmt.Fprintf(stderr, "l1 %s: -config is required\n", name)
		return nil, nil, nil, false
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "l1 %s: %v\n", name, err)
		return nil, nil, nil, false
	}
	if err := cfg.CheckPaths(); err != nil {
		fmt.Fprintf(stderr, "l1 %s: %v\n", name, err)
		return nil, nil, nil, false
	}
	corpus, err := dataset.LoadCorpus(cfg.Paths.CorpusManifest)
	if err != nil {
		fmt.Fprintf(stderr, "l1 %s: %v\n", name, err)
		return nil, nil, nil, false
	}
	questions, err := dataset.LoadDataset(cfg.Paths.DatasetFile, corpus)
	if err != nil {
		fmt.Fprintf(stderr, "l1 %s: %v\n", name, err)
		return nil, nil, nil, false
	}
	return cfg, corpus, questions, true
}

func runCheck(args []string, stdout, stderr io.Writer) int {
	cfg, corpus, questions, ok := loadInputs(args, "check", stderr)
	if !ok {
		return 1
	}
	answerable := 0
	for _, q := range questions.Questions {
		if q.Answerable {
			answerable++
		}
	}
	lines := 0
	for _, doc := range corpus.Documents {
		lines += doc.Lines
	}
	fmt.Fprintf(stdout, "config digest   %s\n", cfg.Digest())
	fmt.Fprintf(stdout, "corpus          %s: %d documents, %d lines, hash %s\n", corpus.Name, len(corpus.Documents), lines, corpus.Hash())
	fmt.Fprintf(stdout, "questions       %d (%d dev, %d sealed); %d answerable, %d unanswerable; version %s\n",
		len(questions.Questions), len(questions.Filter(dataset.SplitDev)), len(questions.Filter(dataset.SplitSealed)),
		answerable, len(questions.Questions)-answerable, questions.Version)
	fmt.Fprintf(stdout, "matrix          %d methods x %d topK values = %d cells\n",
		len(cfg.Matrix.Methods), len(cfg.Matrix.TopK), len(cfg.Matrix.Methods)*len(cfg.Matrix.TopK))
	fmt.Fprintln(stdout, "ok: inputs are well formed; no service was contacted")
	return 0
}
