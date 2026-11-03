// Command l4-product checks complete product wiring with fake transports only.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/xiaopang456/GopherLearn/eval/internal/conversation"
	"github.com/xiaopang456/GopherLearn/eval/internal/pathguard"
)

func run(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("l4-product", flag.ContinueOnError)
	env := flags.String("env", "", "must be fake")
	phase := flags.String("phase", "", "prepare or answer")
	repository := flags.String("repository", "..", "product checkout root")
	input := flags.String("input", "", "private turn array or snapshot archive")
	output := flags.String("output-dir", "", "new private output directory")
	question := flags.String("question", "", "final question for answer phase")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *env != "fake" {
		return fmt.Errorf("only explicit -env fake is supported")
	}
	if *phase != "prepare" && *phase != "answer" {
		return fmt.Errorf("phase must be prepare or answer")
	}
	if err := pathguard.CheckAll(map[string]string{"input": *input, "output": *output}); err != nil {
		return err
	}
	if _, err := os.Stat(*output); err == nil {
		return fmt.Errorf("output already exists")
	} else if !os.IsNotExist(err) {
		return err
	}
	var turns []conversation.Turn
	var snapshot conversation.Snapshot
	var err error
	if *phase == "prepare" {
		file, e := os.Open(*input)
		if e != nil {
			return e
		}
		defer file.Close()
		decoder := json.NewDecoder(io.LimitReader(file, 8<<20))
		decoder.DisallowUnknownFields()
		if err = decoder.Decode(&turns); err != nil {
			return err
		}
		var extra any
		if err = decoder.Decode(&extra); err != io.EOF {
			return fmt.Errorf("turn file contains trailing data")
		}
		if len(turns) < 1 || len(turns) > 256 {
			return fmt.Errorf("turn count must be 1 through 256")
		}
	} else {
		if *question == "" {
			return fmt.Errorf("question required")
		}
		snapshot, err = conversation.ReadSnapshot(*input)
		if err != nil {
			return err
		}
	}
	// Reserve the directory exclusively; partial/failed attempts remain visible.
	if err = os.Mkdir(*output, 0700); err != nil {
		return err
	}
	interrupted, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(interrupted, 3*time.Minute)
	defer cancel()
	write := func(name string, value any) error {
		file, e := os.OpenFile(filepath.Join(*output, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			return e
		}
		encoder := json.NewEncoder(file)
		encoder.SetIndent("", "  ")
		e = encoder.Encode(value)
		return errors.Join(e, file.Close())
	}
	if *phase == "prepare" {
		var calls []conversation.ModelCall
		snapshot, calls, err = conversation.PrepareOfflineProduct(ctx, *repository, turns)
		err = errors.Join(err, write("model-calls.json", calls))
		if err == nil {
			err = conversation.WriteSnapshot(filepath.Join(*output, "snapshot.json"), snapshot)
		}
	} else {
		var result conversation.ProductResult
		result, err = conversation.RunOfflineProduct(ctx, *repository, snapshot, *question)
		err = errors.Join(err, write("result.json", result))
	}
	manifest := map[string]any{"scope": conversation.ProductScope, "env": "fake", "phase": *phase, "paid_model_calls": 0, "quality_and_cost_verified": false, "success": err == nil}
	if err != nil {
		manifest["error"] = err.Error()
	}
	err = errors.Join(err, write("manifest.json", manifest))
	fmt.Fprintln(out, "OFFLINE ONLY: complete product with fake model/vector transports; no quality or cost conclusion")
	return err
}
func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
