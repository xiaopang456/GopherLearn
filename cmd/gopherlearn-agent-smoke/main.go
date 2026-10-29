// Command gopherlearn-agent-smoke is the prepared-but-unexecuted check of one thing
// the offline suite cannot prove: that the configured model actually speaks the tool
// calling protocol end to end - tools offered, a structured tool_calls response, a
// role=tool result fed back, and a final answer grounded in what retrieval returned.
//
// It is deliberately separate from the provider smoke check. That run happened before
// tool calling existed and proves completion, streaming and the grounded chat path;
// nothing in it says a vendor will answer a tool request. Reusing its record file would
// also destroy a one-time piece of evidence.
//
// The v1 run of this command happened on 2026-09-25 and is sealed at
// agent-tool-smoke-v1.json: the vendor did answer with a structured tool call, and this
// server refused the arguments without being able to say which rule fired. v2 is the same
// two-request check with that diagnosis added, and it never opens the v1 file for writing.
//
// The budget is two provider requests, no retries, one execution per process. The
// first request must come back as a knowledge_search tool call and the second must
// produce the answer from the tool result, so a model that ignores the tool offer fails
// instead of quietly passing. The seeded documents carry ASCII discriminators that exist
// in only one account each, so the record can state attribution - the caller's fact
// reached the answer turn, the other user's did not - without storing a single word of
// the question, the answer or the tool output. Every check that fails is written down
// before the process exits non-zero, because the record is the point of spending the
// requests.
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
)

const (
	// authorizationPhrase is a different literal from the provider smoke check and from
	// the v1 tool-calling check on purpose: authorizing one paid run must not authorize
	// another. The v1 run happened and is sealed; this unlocks only v2.
	authorizationPhrase = "agent-tool-smoke-3"
	// requestCap is the whole point of this tool: two provider calls, the decision and
	// the answer, and never a third.
	requestCap = 2
	// defaultOutputPath is the only file this command writes. The v1 record stays exactly
	// where it was written and is never opened for writing by anything here.
	defaultOutputPath = "spec/verification/agent-tool-smoke-v2.json"
	// schemaVersion pins this record's shape. v1 reported "tools_executed" and a budget
	// blame that the evidence did not support, so the shape had to change rather than be
	// reinterpreted.
	schemaVersion = 2
)

var (
	// ErrNotAuthorized covers a missing and an incorrect phrase.
	ErrNotAuthorized = errors.New("agent tool smoke is not authorized")
	// ErrOutputExists reports an already-recorded run, whether preflight or the
	// exclusive create found it.
	ErrOutputExists = errors.New("agent tool smoke output file already exists")
	// ErrWrongProvider reports a process that is not on a real provider.
	ErrWrongProvider = errors.New("agent tool smoke requires GOPHERLEARN_AI_PROVIDER=openai_compatible")
	// ErrRequestCapExceeded refuses a request beyond two before it reaches a socket.
	ErrRequestCapExceeded = errors.New("agent tool smoke request cap exceeded")
)

// dependencies are the effectful edges, injected so the entire accepted path can run in
// tests against a substitute model and a local HTTP test server, without the real
// phrase, a provider, or the filesystem.
type dependencies struct {
	authorized func(candidate string) bool
	// newModel builds the chat model. Production returns the real client; tests
	// install a scripted one. Either way the harness only runs if the result can carry
	// tool calls, which is the seam this smoke exists to exercise.
	newModel  func(config openai.Config) (model.ChatModel, error)
	stat      func(path string) (fs.FileInfo, error)
	mkdirAll  func(path string) error
	writeFile func(path string, data []byte) error
	now       func() time.Time
	stdout    io.Writer
}

func productionDependencies(stdout io.Writer) dependencies {
	return dependencies{
		authorized: func(candidate string) bool { return candidate == authorizationPhrase },
		newModel:   func(config openai.Config) (model.ChatModel, error) { return openai.New(config) },
		stat:       os.Stat,
		mkdirAll:   func(path string) error { return os.MkdirAll(path, 0o750) },
		writeFile: func(path string, data []byte) error {
			// O_EXCL is the real guarantee: the preflight stat can be overtaken by a
			// second run, and only the create can decide between them.
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

type settings struct {
	authorize string
	output    string
}

// plan is what survived preflight.
type plan struct {
	model  openai.Config
	output string
	// work is the seeded question and documents. It is part of the plan rather than a
	// package constant so a test can run the whole accepted path against a variant - a
	// document that carries the wrong discriminator, say - without touching production
	// behaviour, which always gets defaultWorkload().
	work workload
}

func main() {
	code, err := run(os.Args[1:], os.Getenv, productionDependencies(os.Stdout))
	if err != nil {
		// run returns only safe summaries: closed-set codes, the numeric provider
		// status and the record path. A provider's own error text can echo a prompt,
		// so it never reaches this line.
		fmt.Fprintln(os.Stderr, "agent tool smoke:", err)
	}
	os.Exit(code)
}

// run parses flags, refuses, executes once, and archives the record whether or not the
// execution succeeded. Exit codes: 0 accepted, 1 refused or failed, 2 a usage error.
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

	flags := flag.NewFlagSet("gopherlearn-agent-smoke", flag.ContinueOnError)
	flags.SetOutput(deps.stdout)
	var configured settings
	flags.StringVar(&configured.authorize, "authorize", "", "exact authorization phrase required")
	flags.StringVar(&configured.output, "out", defaultOutputPath, "result file to create; never overwritten")
	if err := flags.Parse(args); err != nil {
		return 2, err
	}

	next, refusal := preflight(configured, lookup, deps)
	if refusal != nil {
		return 1, refusal
	}
	record, execution := execute(next, deps)
	encoded, err := record.encode()
	if err != nil {
		return 1, joinErrors(execution, err)
	}
	if err := deps.mkdirAll(filepath.Dir(next.output)); err != nil {
		return 1, joinErrors(execution, err)
	}
	if err := deps.writeFile(next.output, encoded); err != nil {
		if errors.Is(err, os.ErrExist) {
			err = fmt.Errorf("%w: %s appeared while this run was working", ErrOutputExists, next.output)
		}
		return 1, joinErrors(execution, err)
	}
	outcome := "completed"
	if execution != nil {
		outcome = "incomplete"
	}
	fmt.Fprintf(deps.stdout, "agent tool smoke %s: requests=%d/%d tools=%d sources=%d codes=%d output=%s\n",
		outcome, record.ActualRequests, requestCap, record.ToolsExecuted, record.SourceCount, len(record.FailureCodes), next.output)
	if execution != nil {
		return 1, execution
	}
	return 0, nil
}

// preflight applies every refusal in order and touches nothing but the environment and
// one stat of the output path.
func preflight(configured settings, lookup func(string) string, deps dependencies) (plan, error) {
	if configured.authorize == "" {
		return plan{}, fmt.Errorf("%w: -authorize was not given", ErrNotAuthorized)
	}
	if !deps.authorized(configured.authorize) {
		return plan{}, fmt.Errorf("%w: -authorize does not match the required phrase", ErrNotAuthorized)
	}
	if provider := lookup(app.EnvProvider); provider != string(app.ProviderOpenAI) {
		return plan{}, ErrWrongProvider
	}
	config, err := app.ConfigFromEnvironment(lookup)
	if err != nil {
		return plan{}, err
	}
	if info, statErr := deps.stat(configured.output); statErr == nil && info != nil {
		return plan{}, fmt.Errorf("%w: %s", ErrOutputExists, configured.output)
	} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return plan{}, fmt.Errorf("agent tool smoke cannot inspect the output path: %w", statErr)
	}
	return plan{model: config.Model, output: configured.output, work: defaultWorkload()}, nil
}
