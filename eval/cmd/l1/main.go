// Command l1 evaluates retrieval quality (layer L1) of the GopherLearn RAG
// service against a private corpus and question set.
//
//	l1 check -config eval.toml.local                  validate the inputs; offline, free
//	l1 plan  -config eval.toml.local                  forecast the work and paid calls; offline, free
//	l1 run   -config eval.toml.local -env fake        run on in-memory fakes; offline, free
//	l1 run   -config eval.toml.local -env real        run on the real services; spends money
//
// Every mode refuses to start when a configured path sits inside a git working
// tree, and a real run makes no paid call that a limit in the configuration does
// not allow.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"

	"GopherAI/common/rag"

	"github.com/xiaopang456/GopherLearn/eval/internal/config"
	"github.com/xiaopang456/GopherLearn/eval/internal/dataset"
	"github.com/xiaopang456/GopherLearn/eval/internal/fake"
	"github.com/xiaopang456/GopherLearn/eval/internal/realenv"
	"github.com/xiaopang456/GopherLearn/eval/internal/runner"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

const usage = `usage: l1 <mode> -config <file> [flags]

modes:
  check   validate the configuration, corpus manifest and question file (offline)
  plan    forecast chunks, embedding and rerank calls against the limits (offline)
  run     run the retrieval matrix; -env fake (offline) or -env real (paid)

run flags:
  -env fake|real   required; fake results prove plumbing only, never quality
  -split dev|sealed  questions to run (default dev; sealed is held back for the final report)
  -rebuild         replace an evaluation index in Redis whose fingerprint differs
`

// Settings of the offline environment.
const (
	fakeDimension      = 64
	fakeMaxSourceBytes = 8 << 20
)

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	mode, rest := args[0], args[1:]
	switch mode {
	case "check":
		return runCheck(rest, stdout, stderr)
	case "plan":
		return runPlan(ctx, rest, stdout, stderr)
	case "run":
		return runRun(ctx, rest, stdout, stderr)
	case "-h", "-help", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "l1: unknown mode %q\n\n%s", mode, usage)
		return 2
	}
}

// inputs is everything a mode needs from the files.
type inputs struct {
	cfg       *config.Config
	corpus    *dataset.Corpus
	questions *dataset.Dataset
}

// loadInputs reads the configuration and everything it points at, after the
// path guard has accepted every location.
func loadInputs(flags *flag.FlagSet, configPath string, stderr io.Writer) (*inputs, bool) {
	name := flags.Name()
	if configPath == "" {
		fmt.Fprintf(stderr, "l1 %s: -config is required\n", name)
		return nil, false
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintf(stderr, "l1 %s: %v\n", name, err)
		return nil, false
	}
	if err := cfg.CheckPaths(); err != nil {
		fmt.Fprintf(stderr, "l1 %s: %v\n", name, err)
		return nil, false
	}
	corpus, err := dataset.LoadCorpus(cfg.Paths.CorpusManifest)
	if err != nil {
		fmt.Fprintf(stderr, "l1 %s: %v\n", name, err)
		return nil, false
	}
	questions, err := dataset.LoadDataset(cfg.Paths.DatasetFile, corpus)
	if err != nil {
		fmt.Fprintf(stderr, "l1 %s: %v\n", name, err)
		return nil, false
	}
	return &inputs{cfg: cfg, corpus: corpus, questions: questions}, true
}

func newFlags(name string, stderr io.Writer) (*flag.FlagSet, *string) {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(stderr)
	return flags, flags.String("config", "", "evaluation configuration file (TOML)")
}

func runCheck(args []string, stdout, stderr io.Writer) int {
	flags, configPath := newFlags("check", stderr)
	if err := flags.Parse(args); err != nil {
		return 2
	}
	in, ok := loadInputs(flags, *configPath, stderr)
	if !ok {
		return 1
	}
	answerable := 0
	for _, q := range in.questions.Questions {
		if q.Answerable {
			answerable++
		}
	}
	lines := 0
	for _, doc := range in.corpus.Documents {
		lines += doc.Lines
	}
	fmt.Fprintf(stdout, "config digest   %s\n", in.cfg.Digest())
	fmt.Fprintf(stdout, "corpus          %s: %d documents, %d lines, hash %s\n", in.corpus.Name, len(in.corpus.Documents), lines, in.corpus.Hash())
	fmt.Fprintf(stdout, "questions       %d (%d dev, %d sealed); %d answerable, %d unanswerable; version %s\n",
		len(in.questions.Questions), len(in.questions.Filter(dataset.SplitDev)), len(in.questions.Filter(dataset.SplitSealed)),
		answerable, len(in.questions.Questions)-answerable, in.questions.Version)
	fmt.Fprintf(stdout, "matrix          %d methods x %d topK values\n", len(in.cfg.Matrix.Methods), len(in.cfg.Matrix.TopK))
	fmt.Fprintln(stdout, "ok: inputs are well formed; no service was contacted")
	return 0
}

func runPlan(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags, configPath := newFlags("plan", stderr)
	split := flags.String("split", dataset.SplitDev, "questions to plan for: dev or sealed")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	in, ok := loadInputs(flags, *configPath, stderr)
	if !ok {
		return 1
	}
	product, err := config.LoadProductOffline(in.cfg.Services.ProductConfig)
	if err != nil {
		fmt.Fprintf(stderr, "l1 plan: %v\n", err)
		return 1
	}
	questions := in.questions.Filter(*split)
	if len(questions) == 0 {
		fmt.Fprintf(stderr, "l1 plan: the %q split has no questions\n", *split)
		return 1
	}
	estimate, err := runner.MakePlan(ctx, runner.PlanInput{
		Config: in.cfg, Corpus: in.corpus, Questions: questions,
		Chunker: rag.DefaultChunkerConfig(), MaxSourceBytes: product.KnowledgeConfig.MaxFileSizeBytes,
	})
	if err != nil {
		fmt.Fprintf(stderr, "l1 plan: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "plan for the %q split (an upper bound: an index already in Redis costs no indexing calls)\n", *split)
	estimate.Write(stdout, in.cfg.Limits)
	return 0
}

func runRun(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags, configPath := newFlags("run", stderr)
	envName := flags.String("env", "", "environment: fake or real (required)")
	split := flags.String("split", dataset.SplitDev, "questions to run: dev or sealed")
	rebuild := flags.Bool("rebuild", false, "replace an evaluation index whose fingerprint differs")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *envName != runner.EnvFake && *envName != runner.EnvReal {
		fmt.Fprintf(stderr, "l1 run: -env must be %q or %q; there is no default, so a real run is never started by accident\n", runner.EnvFake, runner.EnvReal)
		return 2
	}
	in, ok := loadInputs(flags, *configPath, stderr)
	if !ok {
		return 1
	}

	var env runner.Env
	if *envName == runner.EnvFake {
		env = fakeEnv()
		fmt.Fprintln(stdout, "OFFLINE FAKE ENVIRONMENT: these results prove plumbing only. Never report them as retrieval quality.")
	} else {
		product, err := config.LoadProduct(in.cfg.Services.ProductConfig)
		if err != nil {
			fmt.Fprintf(stderr, "l1 run: %v\n", err)
			return 1
		}
		realEnv, closeEnv, err := realenv.New(ctx, product)
		if err != nil {
			fmt.Fprintf(stderr, "l1 run: %v\n", err)
			return 1
		}
		defer closeEnv()
		env = realEnv
	}

	result, err := runner.Execute(ctx, in.cfg, in.corpus, in.questions, env, runner.Options{Split: *split, Rebuild: *rebuild, Progress: stdout})
	if result != nil {
		fmt.Fprintf(stdout, "run directory   %s\n", result.Dir)
		fmt.Fprintf(stdout, "records         %d (%d with errors); complete: %v\n", result.Records, result.Errors, result.Complete)
		fmt.Fprintf(stdout, "paid calls      %d embedding, %d rerank (index: %s)\n", result.EmbeddingCalls, result.RerankCalls, result.IndexDecision)
	}
	if err != nil {
		fmt.Fprintf(stderr, "l1 run: %v\n", err)
		return 1
	}
	if !result.Complete {
		return 1
	}
	return 0
}

// fakeEnv is the offline environment: deterministic fakes and an in-memory index.
func fakeEnv() runner.Env {
	embedder := fake.NewEmbedder(fakeDimension)
	backend := fake.NewBackend(fakeDimension)
	return runner.Env{
		Kind: runner.EnvFake, Embedder: embedder, Repository: backend, Searcher: backend, Reranker: fake.NewReranker(),
		Dimension: fakeDimension, EmbeddingModel: "fake-hash-embedder", RerankModel: "fake/token-overlap",
		Chunker: rag.DefaultChunkerConfig(), MaxSourceBytes: fakeMaxSourceBytes,
	}
}
