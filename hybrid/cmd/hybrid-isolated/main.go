package main

import (
	"context"
	"flag"
	"fmt"
	"gopherlearn-v2-hybrid"
	"io"
	"os"
	"os/signal"
	"syscall"
)

func runCLI(ctx context.Context, args []string, stderr io.Writer) int {
	flags := flag.NewFlagSet("hybrid-isolated", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	path := flags.String("config", "", "required independent configuration")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: hybrid-isolated -config <independent-file>")
		return 2
	}
	if err := hybrid.Run(ctx, *path); err != nil {
		// Do not print configuration values or dependency errors containing them.
		fmt.Fprintln(stderr, "hybrid entry refused or failed; configuration and resource checks required")
		return 1
	}
	return 0
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := runCLI(ctx, os.Args[1:], os.Stderr)
	stop()
	// Run has completed bounded HTTP shutdown and resource cleanup before exit.
	os.Exit(code)
}
