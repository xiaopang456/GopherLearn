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
	qid := flags.String("qid", "synthetic-offline", "scenario identifier for the scoring record")
	arm := flags.String("arm", "current", "configuration identifier")
	rep := flags.Int("rep", 0, "repeat number: 0, 1 or 2")
	condition := flags.String("condition", "main", "main, control or distractor")
	stopFile := flags.String("stop-file", "", "private stop-file path, watched during execution")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *env != "fake" {
		return fmt.Errorf("only explicit -env fake is supported")
	}
	if *phase != "prepare" && *phase != "answer" {
		return fmt.Errorf("phase must be prepare or answer")
	}
	if *rep < 0 || *rep > 2 || *qid == "" || *arm == "" || (*condition != "main" && *condition != "control" && *condition != "distractor") {
		return fmt.Errorf("invalid attempt identity")
	}
	if err := pathguard.CheckAll(map[string]string{"input": *input, "output": *output}); err != nil {
		return err
	}
	if *stopFile != "" {
		if err := pathguard.Check("stop-file", *stopFile); err != nil {
			return err
		}
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
		info, statErr := file.Stat()
		if statErr != nil {
			return statErr
		}
		if info.Size() > 8<<20 {
			return fmt.Errorf("turn file exceeds limit")
		}
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
	deadline, cancel := context.WithTimeout(interrupted, 3*time.Minute)
	defer cancel()
	ctx, cancelWatch := watchStopFile(deadline, *stopFile)
	defer cancelWatch()
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
		var warmup conversation.WarmupReport
		warmup, err = conversation.PrepareOfflineProductReport(ctx, *repository, turns)
		snapshot = warmup.Snapshot
		err = errors.Join(err, write("model-calls.json", warmup.Calls), write("warmup.json", warmup))
		if err == nil {
			err = conversation.WriteSnapshot(filepath.Join(*output, "snapshot.json"), snapshot)
		}
	} else {
		var result conversation.ProductResult
		result, err = conversation.RunOfflineProduct(ctx, *repository, snapshot, *question)
		if cause := context.Cause(ctx); cause != nil {
			err = errors.Join(err, cause)
			result.Error = err.Error()
			result.BackgroundAudit.Valid = false
			result.BackgroundAudit.Reason = result.Error
		}
		err = errors.Join(err, write("result.json", result))
		encoded, encodeResultErr := json.Marshal(result)
		if encodeResultErr != nil {
			return errors.Join(err, encodeResultErr)
		}
		var record map[string]any
		if decodeErr := json.Unmarshal(encoded, &record); decodeErr != nil {
			return decodeErr
		}
		record["qid"], record["arm"], record["rep"], record["condition"] = *qid, *arm, *rep, *condition
		data, encodeErr := json.Marshal(record)
		if encodeErr != nil {
			return encodeErr
		}
		file, fileErr := os.OpenFile(filepath.Join(*output, "record.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if fileErr == nil {
			_, fileErr = file.Write(append(data, '\n'))
			fileErr = errors.Join(fileErr, file.Close())
		}
		err = errors.Join(err, fileErr)
	}
	if cause := context.Cause(ctx); cause != nil {
		err = errors.Join(err, cause)
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
