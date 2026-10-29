// Command gopherlearn-integrated is the formal startup entry of the integrated
// application: the original V2 assembly (Agent = Eino ReAct, knowledge, memory,
// learning, Episode) kept as the running kernel, with GopherLearn's retained
// ordinary Chat mounted on the mainline contract paths.
//
// It adds no business implementation. Every collaborator arrives through
// hybrid.Run, which refuses a configuration that would migrate business data,
// read credentials from the environment, or share a schema with the baseline
// entry. cmd/gopherlearn remains the historical baseline entry and builds the
// mainline agent loop instead of this chain, which is why this process announces
// which entry it is before it opens anything.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"GopherAI/common/rag"
	"gopherlearn-v2-hybrid"
)

// entryLine names the routes this process serves: an Agent turn reaches
// v2/service/agent.Runner, and a chat turn reaches the retained DirectChat.
const entryLine = "gopherlearn integrated entry: agent=original V2 Eino ReAct, chat=retained DirectChat, business migration refused"

func runCLI(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("gopherlearn-integrated", flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("config", "", "explicit integrated configuration file (required)")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: gopherlearn-integrated -config <integrated-file.toml>")
		return 2
	}
	if strings.TrimSpace(*path) == "" {
		fmt.Fprintln(stderr, "usage: gopherlearn-integrated -config <integrated-file.toml>")
		return 2
	}
	fmt.Fprintln(stdout, entryLine)
	if err := hybrid.Run(ctx, *path); err != nil {
		// Dependency errors can quote a configuration value, so only the refusal
		// reaches the operator's terminal; the file and its cause stay unsent.
		// A rerank failure is the exception: its cause is a fixed sentence that
		// names what to fix and quotes no configuration value, and an operator
		// who cannot see it has nothing to act on.
		if message := rag.OperatorMessage(err); message != "" {
			fmt.Fprintln(stderr, message)
		} else {
			fmt.Fprintln(stderr, "integrated entry refused or failed; check the configuration requirements and the schema, Redis instance and storage directory it points at")
		}
		return 1
	}
	return 0
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := runCLI(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	// hybrid.Run completed its bounded HTTP shutdown and resource cleanup here.
	os.Exit(code)
}
