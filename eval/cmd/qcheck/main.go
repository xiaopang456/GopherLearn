// Command qcheck validates a generated question set against the rules in
// eval-design.md §5.2, and cuts the development and sealed splits.
//
//	qcheck validate -corpus corpus.json -dataset dataset.jsonl [-meta dataset.meta.jsonl] [-quota quota.json] [-report report.json]
//	qcheck split    -dataset dataset.jsonl -meta dataset.meta.jsonl -out dataset.split.jsonl [-seed 42]
//
// Both modes are offline and free: nothing here calls a model or a service.
//
// Every path is refused when it sits inside a git working tree, so a question
// set cannot be committed by accident.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/xiaopang456/GopherLearn/eval/internal/dataset"
	"github.com/xiaopang456/GopherLearn/eval/internal/pathguard"
	"github.com/xiaopang456/GopherLearn/eval/internal/validate"
)

const usage = `usage: qcheck <mode> [flags]

modes:
  validate   check a question set against the rules of eval-design.md §5.2 (offline)
  split      cut the question set into the dev and sealed splits (offline)

validate flags:
  -corpus <file>     corpus manifest (required)
  -dataset <file>    question file, JSONL (required)
  -meta <file>       sidecar annotations, JSONL (optional but recommended)
  -quota <file>      quota file, JSON (optional; without it only the per-question rules run)
  -report <file>     write the report as JSON (optional)
  -strict            treat warnings as errors

split flags:
  -dataset <file>    question file to split (required)
  -meta <file>       sidecar annotations, JSONL (required)
  -out <file>        question file to write with the split filled in (required)
  -seed <n>          shuffle seed (default 42)
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "validate":
		return runValidate(args[1:], stdout, stderr)
	case "split":
		return runSplit(args[1:], stdout, stderr)
	case "-h", "-help", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "qcheck: unknown mode %q\n\n%s", args[0], usage)
		return 2
	}
}

func runValidate(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("qcheck validate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	corpusPath := flags.String("corpus", "", "corpus manifest")
	datasetPath := flags.String("dataset", "", "question file")
	metaPath := flags.String("meta", "", "sidecar annotations")
	quotaPath := flags.String("quota", "", "quota file")
	reportPath := flags.String("report", "", "report output")
	strict := flags.Bool("strict", false, "treat warnings as errors")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *corpusPath == "" || *datasetPath == "" {
		fmt.Fprintln(stderr, "qcheck validate: -corpus and -dataset are required")
		return 2
	}
	paths := map[string]string{"corpus": *corpusPath, "dataset": *datasetPath}
	if *metaPath != "" {
		paths["meta"] = *metaPath
	}
	if *quotaPath != "" {
		paths["quota"] = *quotaPath
	}
	if *reportPath != "" {
		paths["report"] = *reportPath
	}
	if err := pathguard.CheckAll(paths); err != nil {
		fmt.Fprintf(stderr, "qcheck validate: %v\n", err)
		return 2
	}

	corpus, err := dataset.LoadCorpus(*corpusPath)
	if err != nil {
		fmt.Fprintf(stderr, "qcheck validate: %v\n", err)
		return 2
	}
	raw, err := os.ReadFile(*datasetPath)
	if err != nil {
		fmt.Fprintf(stderr, "qcheck validate: %v\n", err)
		return 2
	}
	// The structural rules come from the dataset loader; a failure there is
	// reported as part of the same run rather than as a separate step. The
	// loader is used in its lenient form: the questions that are structurally
	// fine still have to be checked against the content rules, otherwise one
	// broken question would hide every other problem in the file.
	parsed, issues := dataset.ParseDatasetLenient(raw, corpus)
	questions := parsed.Questions
	structural := map[string]string{}
	for _, issue := range issues {
		structural[fmt.Sprintf("%d:%s", issue.Line, issue.Message)] = issue.String()
	}

	var meta []validate.Meta
	if *metaPath != "" {
		meta, err = validate.LoadMeta(*metaPath)
		if err != nil {
			fmt.Fprintf(stderr, "qcheck validate: %v\n", err)
			return 2
		}
	}
	var quota validate.Quota
	if *quotaPath != "" {
		quota, err = validate.LoadQuota(*quotaPath)
		if err != nil {
			fmt.Fprintf(stderr, "qcheck validate: %v\n", err)
			return 2
		}
	}

	report, err := validate.Validate(corpus, questions, meta, quota, validate.DefaultParams())
	if err != nil {
		fmt.Fprintf(stderr, "qcheck validate: %v\n", err)
		return 2
	}
	if len(structural) > 0 {
		report.Findings = append(report.Findings, validate.Finding{
			Severity: validate.SeverityError, Rule: "dataset.structural",
			Detail: fmt.Sprintf("the dataset loader reported %d structural problem(s); %d question(s) could not be checked against the content rules",
				len(structural), len(issues))})
		for _, key := range sortedStructuralKeys(structural) {
			report.Findings = append(report.Findings, validate.Finding{
				Severity: validate.SeverityError, Rule: "dataset.structural",
				Detail: structural[key]})
		}
	}

	fmt.Fprint(stdout, report.Text())
	if *reportPath != "" {
		encoded, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			fmt.Fprintf(stderr, "qcheck validate: %v\n", err)
			return 2
		}
		if err := os.WriteFile(*reportPath, encoded, 0o600); err != nil {
			fmt.Fprintf(stderr, "qcheck validate: %v\n", err)
			return 2
		}
	}
	failed := len(report.Errors()) > 0
	if *strict {
		for _, f := range report.Findings {
			if f.Severity == validate.SeverityWarning {
				failed = true
			}
		}
	}
	if failed {
		return 1
	}
	return 0
}

func runSplit(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("qcheck split", flag.ContinueOnError)
	flags.SetOutput(stderr)
	datasetPath := flags.String("dataset", "", "question file")
	metaPath := flags.String("meta", "", "sidecar annotations")
	outPath := flags.String("out", "", "question file to write")
	seed := flags.Int64("seed", 42, "shuffle seed")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *datasetPath == "" || *metaPath == "" || *outPath == "" {
		fmt.Fprintln(stderr, "qcheck split: -dataset, -meta and -out are required")
		return 2
	}
	if err := pathguard.CheckAll(map[string]string{
		"dataset": *datasetPath, "meta": *metaPath, "out": *outPath,
	}); err != nil {
		fmt.Fprintf(stderr, "qcheck split: %v\n", err)
		return 2
	}
	raw, err := validate.ReadRawQuestions(*datasetPath)
	if err != nil {
		fmt.Fprintf(stderr, "qcheck split: %v\n", err)
		return 2
	}
	meta, err := validate.LoadMeta(*metaPath)
	if err != nil {
		fmt.Fprintf(stderr, "qcheck split: %v\n", err)
		return 2
	}
	questions := make([]dataset.Question, 0, len(raw))
	for _, q := range raw {
		answerable := false
		if q.Answerable != nil {
			answerable = *q.Answerable
		}
		questions = append(questions, dataset.Question{
			ID: q.ID, Question: q.Question, Answerable: answerable,
			Evidence: q.Evidence, Split: q.Split, Topic: q.Topic, Type: q.Type,
		})
	}
	result, err := validate.Split(questions, meta, *seed)
	if err != nil {
		fmt.Fprintf(stderr, "qcheck split: %v\n", err)
		return 2
	}
	dev := map[string]bool{}
	for _, id := range result.Dev {
		dev[id] = true
	}
	out := make([]validate.OutQuestion, 0, len(raw))
	for _, q := range raw {
		answerable := false
		if q.Answerable != nil {
			answerable = *q.Answerable
		}
		split := "sealed"
		if dev[q.ID] {
			split = "dev"
		}
		evidence := q.Evidence
		if !answerable {
			evidence = nil
		}
		out = append(out, validate.OutQuestion{
			ID: q.ID, Question: q.Question, Answerable: answerable,
			Evidence: evidence, Split: split, Topic: q.Topic, Type: q.Type,
		})
	}
	if err := validate.WriteQuestions(*outPath, out); err != nil {
		fmt.Fprintf(stderr, "qcheck split: %v\n", err)
		return 2
	}
	fmt.Fprintf(stdout, "split %d questions: dev=%d sealed=%d (seed %d)\n",
		len(out), len(result.Dev), len(result.Sealed), *seed)
	return 0
}

func sortedStructuralKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
