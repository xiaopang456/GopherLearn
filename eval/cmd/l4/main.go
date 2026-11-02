// Command l4 prepares synthetic short-term-memory snapshots without a model.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"GopherAI/model"
	agent "GopherAI/service/agent"
	"github.com/xiaopang456/GopherLearn/eval/internal/conversation"
	"github.com/xiaopang456/GopherLearn/eval/internal/pathguard"
)

type stubGenerator struct{ Calls int }

func (s *stubGenerator) GenerateSummary(context.Context, string, []model.Message) (string, error) {
	s.Calls++
	return `{"schema_version":2,"current_goal":"synthetic offline preparation"}`, nil
}

func run(args []string, out io.Writer) (resultErr error) {
	flags := flag.NewFlagSet("l4", flag.ContinueOnError)
	env := flags.String("env", "", "must be fake")
	input := flags.String("turns", "", "JSON turn array outside repositories")
	output := flags.String("snapshot", "", "new snapshot file outside repositories")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *env != "fake" {
		return fmt.Errorf("only explicit -env fake is supported")
	}
	if err := pathguard.CheckAll(map[string]string{"turns": *input, "snapshot": *output}); err != nil {
		return err
	}
	if _, err := os.Stat(*output); err == nil {
		return fmt.Errorf("snapshot already exists")
	} else if !os.IsNotExist(err) {
		return err
	}
	f, err := os.Open(*input)
	if err != nil {
		return err
	}
	defer f.Close()
	decoder := json.NewDecoder(io.LimitReader(f, 8<<20))
	decoder.DisallowUnknownFields()
	var turns []conversation.Turn
	if err = decoder.Decode(&turns); err != nil {
		return err
	}
	var extra any
	if err = decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("turn file contains trailing data")
	}
	if len(turns) == 0 || len(turns) > 256 {
		return fmt.Errorf("turn count must be 1 through 256")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	sandbox, err := conversation.NewSandbox(ctx)
	if err != nil {
		return err
	}
	defer func() {
		stop, c := context.WithTimeout(context.Background(), 15*time.Second)
		defer c()
		if err := sandbox.Close(stop); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("sandbox cleanup failed: %w", err))
		}
	}()
	if err = sandbox.InitSynthetic(ctx); err != nil {
		return err
	}
	generator := &stubGenerator{}
	prepared, err := sandbox.Prepare(ctx, 7, "synthetic-evaluation", "synthetic-learner", turns, generator, agent.DefaultSummaryConfig())
	if err != nil {
		return err
	}
	snapshot, err := sandbox.Capture(ctx, 7, "synthetic-evaluation")
	if err != nil {
		return err
	}
	if err = conversation.WriteSnapshot(*output, snapshot); err != nil {
		return err
	}
	fmt.Fprintf(out, "OFFLINE STUB: no quality or cost conclusion\nturns=%d summary_calls=%d messages=%d\n", len(prepared), generator.Calls, len(snapshot.Messages))
	return nil
}
func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
