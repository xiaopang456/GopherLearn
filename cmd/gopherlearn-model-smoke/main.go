// Command gopherlearn-model-smoke is the prepared-but-unexecuted provider smoke
// check. It exists so that the first real call to a paid endpoint is a single,
// bounded, auditable act rather than something that happens by accident while
// someone is debugging.
//
// Nothing in this file is allowed to talk to a provider before every refusal has
// been checked: the exact authorization phrase, the provider mode, the three
// required settings, and the absence of the output file. Only then is a client
// constructed, the output directory created, and a request sent.
//
// The hard budget is three requests with no retries, counted inside this process.
// Four steps run in order - a non-streaming completion, a streamed completion, a
// full knowledge_qa turn, and a zero-call check that an irrelevant question asks
// the provider nothing at all - and only the first three consume budget.
//
// The recorded result is deliberately thin: hashes, counts and reasons. Never a
// prompt, an answer, an Authorization header, or anything derived from the key.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/app"
	"github.com/xiaopang456/GopherLearn/internal/model"
	"github.com/xiaopang456/GopherLearn/internal/openai"
	"github.com/xiaopang456/GopherLearn/internal/service"
)

const (
	// authorizationPhrase must match -authorize exactly. It is a three-word
	// literal rather than a boolean so that running the tool requires typing an
	// intention, and "model-smoke-3" names the request count that will be spent.
	authorizationPhrase = "model-smoke-3"
	// requestCap is the in-process hard ceiling shared by Complete and Stream.
	requestCap = 3
	// defaultOutputPath is the only file this command writes.
	defaultOutputPath = "spec/verification/provider-smoke-v1.json"
	// schemaVersion pins the result shape so a later reader can tell which rules
	// produced a recorded file. Version 2 splits the single answer hash into one per
	// step and records every step's status, because a partially failed run is still
	// evidence.
	schemaVersion = 2
)

var (
	// ErrNotAuthorized covers a missing and an incorrect phrase: the tool refuses,
	// and the reason string differs while the exit path does not.
	ErrNotAuthorized = errors.New("model smoke is not authorized")
	// ErrOutputExists reports that the result file is already there, whether the
	// preflight found it or it appeared during the run. Overwriting a previous smoke
	// record would destroy the evidence it holds.
	ErrOutputExists = errors.New("model smoke output file already exists")
	// ErrWrongProvider reports a process that is not configured for a real
	// provider. Running the smoke against the echo mode would record a successful
	// call that never happened.
	ErrWrongProvider = errors.New("model smoke requires GOPHERLEARN_AI_PROVIDER=openai_compatible")
	// ErrRequestCapExceeded refuses a request beyond the budget before it reaches
	// the network.
	ErrRequestCapExceeded = errors.New("model smoke request cap exceeded")
)

// dependencies are the effectful edges of the command, injected so the whole flow
// is testable without a provider, without the real authorization phrase, and
// without touching the filesystem.
type dependencies struct {
	// authorized decides whether a candidate phrase is accepted. Production passes
	// the exact-match rule; tests use their own phrase so the real one never
	// appears in a test run.
	authorized func(candidate string) bool
	newModel   func(config openai.Config) (model.ChatModel, error)
	stat       func(path string) (fs.FileInfo, error)
	mkdirAll   func(path string) error
	writeFile  func(path string, data []byte) error
	now        func() time.Time
	stdout     io.Writer
}

// productionDependencies wires the real side effects.
func productionDependencies(stdout io.Writer) dependencies {
	return dependencies{
		authorized: func(candidate string) bool { return candidate == authorizationPhrase },
		newModel:   func(config openai.Config) (model.ChatModel, error) { return openai.New(config) },
		stat:       os.Stat,
		mkdirAll:   func(path string) error { return os.MkdirAll(path, 0o750) },
		writeFile: func(path string, data []byte) error {
			// O_EXCL is the real guarantee, not the earlier stat: two runs started at
			// once both pass their preflight, and only the create can decide between
			// them. A write that fails part-way leaves its partial file in place on
			// purpose, because a half record is evidence to look at while a silent
			// replacement would destroy the previous run.
			file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				return err
			}
			if _, writeErr := file.Write(data); writeErr != nil {
				_ = file.Close()
				return writeErr
			}
			return file.Close()
		},
		now:    time.Now,
		stdout: stdout,
	}
}

// settings is what survived preflight.
type settings struct {
	authorize string
	output    string
}

func main() {
	code, err := run(os.Args[1:], os.Getenv, productionDependencies(os.Stdout))
	if err != nil {
		fmt.Fprintln(os.Stderr, "model smoke:", err)
	}
	os.Exit(code)
}

// run parses flags, refuses, and only then executes. It returns a process exit
// code: 0 accepted, 1 refused or failed, 2 a usage error such as an unknown flag.
func run(args []string, lookup func(string) string, deps dependencies) (int, error) {
	if lookup == nil {
		lookup = func(string) string { return "" }
	}
	if deps.authorized == nil {
		deps.authorized = productionDependencies(nil).authorized
	}
	if deps.stdout == nil {
		deps.stdout = io.Discard
	}

	flags := flag.NewFlagSet("gopherlearn-model-smoke", flag.ContinueOnError)
	flags.SetOutput(deps.stdout)
	var configured settings
	flags.StringVar(&configured.authorize, "authorize", "", "exact authorization phrase required")
	flags.StringVar(&configured.output, "out", defaultOutputPath, "result file to create; never overwritten")
	if err := flags.Parse(args); err != nil {
		return 2, err
	}

	plan, refusal := preflight(configured, lookup, deps)
	if refusal != nil {
		return 1, refusal
	}
	// A run that failed part-way still produces its record: the counts and the
	// per-step statuses are the reason the failure is worth reading. The raw errors
	// go to stderr, never into the file, because they may carry a provider's words.
	report, execution := execute(plan, deps)
	encoded, err := report.encode()
	if err != nil {
		return 1, errors.Join(execution, err)
	}
	if err := deps.mkdirAll(filepath.Dir(plan.output)); err != nil {
		return 1, errors.Join(execution, err)
	}
	if err := deps.writeFile(plan.output, encoded); err != nil {
		if errors.Is(err, os.ErrExist) {
			// The preflight found nothing and the exclusive create lost the race, or
			// another authorized run got here first. Either way the older bytes stay.
			err = fmt.Errorf("%w: %s appeared while this run was working", ErrOutputExists, plan.output)
		}
		return 1, errors.Join(execution, err)
	}
	outcome := "completed"
	if execution != nil {
		outcome = "incomplete"
	}
	fmt.Fprintf(deps.stdout, "model smoke %s: requests=%d/%d failures=%d steps=%d output=%s\n",
		outcome, report.ActualRequests, requestCap, report.Failures, len(report.Steps), plan.output)
	if execution != nil {
		return 1, execution
	}
	return 0, nil
}

// preflight applies every refusal in order and touches nothing but the
// environment and one stat of the output path. It returns the resolved
// configuration, or the reason the run is refused.
func preflight(configured settings, lookup func(string) string, deps dependencies) (resolved, error) {
	if configured.authorize == "" {
		return resolved{}, fmt.Errorf("%w: -authorize was not given", ErrNotAuthorized)
	}
	if !deps.authorized(configured.authorize) {
		// The expected phrase is not echoed: a mistyped value in a terminal is
		// already a record of the intent, and the accepted value must not be
		// reproduced by a refusal message.
		return resolved{}, fmt.Errorf("%w: -authorize does not match the required phrase", ErrNotAuthorized)
	}
	if provider := lookup(app.EnvProvider); provider != string(app.ProviderOpenAI) {
		return resolved{}, ErrWrongProvider
	}
	// app.ConfigFromEnvironment enforces base URL, API key and model, and reads the
	// context budget. The key comes from this process environment only: no .env
	// file is opened and no directory is searched for credentials.
	config, err := app.ConfigFromEnvironment(lookup)
	if err != nil {
		return resolved{}, err
	}
	if info, statErr := deps.stat(configured.output); statErr == nil && info != nil {
		return resolved{}, fmt.Errorf("%w: %s", ErrOutputExists, configured.output)
	} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		// A stat that fails for another reason is not evidence that the file is
		// absent, and guessing here could overwrite a previous result.
		return resolved{}, fmt.Errorf("model smoke cannot inspect the output path: %w", statErr)
	}
	return resolved{model: config.Model, budget: config.Context, output: configured.output}, nil
}

// resolved is the accepted plan.
type resolved struct {
	model  openai.Config
	budget service.ContextBudget
	output string
}
