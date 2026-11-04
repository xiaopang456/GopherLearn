// Command l4-product prepares conversation histories and asks the final
// question through the standard product entry. The offline mode uses only
// fake transports. The real mode reaches the provider models through the
// operator's counting proxy, which holds the credentials, enforces the call
// caps and writes the stop file.
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
	"strings"
	"time"

	"github.com/xiaopang456/GopherLearn/eval/internal/conversation"
	"github.com/xiaopang456/GopherLearn/eval/internal/pathguard"
)

func run(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("l4-product", flag.ContinueOnError)
	env := flags.String("env", "", "fake or real")
	phase := flags.String("phase", "", "prepare or answer")
	repository := flags.String("repository", "..", "product checkout root")
	input := flags.String("input", "", "private turn array or snapshot archive")
	output := flags.String("output-dir", "", "new private output directory")
	question := flags.String("question", "", "final question for answer phase")
	questionFile := flags.String("question-file", "", "file holding the final question (UTF-8)")
	runID := flags.String("run-id", "", "evaluation run identifier")
	qid := flags.String("qid", "synthetic-offline", "scenario identifier for the scoring record")
	arm := flags.String("arm", "current", "configuration identifier")
	rep := flags.Int("rep", 0, "repeat number: 0, 1 or 2")
	condition := flags.String("condition", "main", "main, control or distractor")
	stopFile := flags.String("stop-file", "", "private stop-file path, watched during execution")
	timeout := flags.Duration("timeout", 0, "attempt deadline; the default depends on the environment")
	proxy := flags.String("proxy", "", "counting proxy base URL (real mode)")
	redis := flags.String("redis", "", "shared evaluation index host:port (real mode)")
	redisPassword := flags.String("redis-password-env", "", "environment variable holding the evaluation index password")
	storage := flags.String("storage", "", "knowledge storage root of the shared evaluation index")
	knowledgeSeed := flags.String("knowledge-seed", "", "knowledge seed file of the shared evaluation index")
	chatModel := flags.String("chat-model", "", "chat model name (real mode)")
	embeddingModel := flags.String("embedding-model", "", "embedding model name (real mode)")
	rerankModel := flags.String("rerank-model", "", "rerank model name (real mode)")
	userID := flags.Int64("user-id", 0, "evaluation user identifier (real mode)")
	userName := flags.String("user-name", "", "evaluation user name (real mode)")
	sessionID := flags.String("session-id", "", "evaluation session identifier (real mode)")
	corpus := flags.String("corpus", "", "corpus directory of the shared evaluation index (index phase)")
	seedOut := flags.String("seed-out", "", "knowledge seed file to write (index phase)")
	storageOut := flags.String("storage-out", "", "storage directory of the shared evaluation index (index phase)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || (*env != "fake" && *env != "real") {
		return fmt.Errorf("only explicit -env fake or -env real is supported")
	}
	if *phase != "prepare" && *phase != "answer" && *phase != "index" {
		return fmt.Errorf("phase must be prepare, answer or index")
	}
	if *rep < 0 || *rep > 2 || *qid == "" || *arm == "" || (*condition != "main" && *condition != "control" && *condition != "distractor") {
		return fmt.Errorf("invalid attempt identity")
	}
	guarded := map[string]string{"output": *output}
	if *phase != "index" {
		guarded["input"] = *input
	}
	if err := pathguard.CheckAll(guarded); err != nil {
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
	inputDigest := ""
	if *phase != "index" {
		inputDigest, err = fileDigest(*input)
		if err != nil {
			return err
		}
	}
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
	} else if *phase == "answer" {
		if *questionFile != "" {
			raw, readErr := os.ReadFile(*questionFile)
			if readErr != nil {
				return readErr
			}
			*question = strings.TrimSpace(string(raw))
		}
		if *question == "" {
			return fmt.Errorf("question required")
		}
		snapshot, err = conversation.ReadSnapshot(*input)
		if err != nil {
			return err
		}
	}
	real := *env == "real"
	var environment conversation.RealEnvironment
	var seed *conversation.KnowledgeSeed
	if real {
		if *phase != "index" {
			explicit := map[string]bool{}
			flags.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
			if !explicit["run-id"] || !explicit["arm"] || !explicit["qid"] || !explicit["condition"] {
				return fmt.Errorf("scenario identity requires explicit run-id, arm, qid and condition")
			}
			if *knowledgeSeed != "" || *userID != 0 || *userName != "" {
				return fmt.Errorf("scenario runs require an empty knowledge base and derived identity")
			}
			*userID, *userName, err = scenarioIdentity(*runID, *arm, *qid, *condition)
			if err != nil {
				return err
			}
		}
		environment, err = realEnvironment(flags, proxy, redis, redisPassword, storage, knowledgeSeed, chatModel, embeddingModel, rerankModel, userID, userName, sessionID)
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
	deadline := 3 * time.Minute
	if real {
		deadline = 15 * time.Minute
	}
	if *timeout > 0 {
		deadline = *timeout
	}
	expired, cancel := context.WithTimeout(interrupted, deadline)
	defer cancel()
	ctx, cancelWatch := watchStopFile(expired, *stopFile)
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
	paidCalls := 0
	if *phase == "index" {
		var index conversation.IndexReport
		index, err = conversation.BuildSharedIndex(ctx, *repository, environment, *corpus, *seedOut, *storageOut)
		paidCalls = len(index.Calls)
		err = errors.Join(err, write("model-calls.json", index.Calls), write("index-report.json", index))
	} else if *phase == "prepare" {
		var warmup conversation.WarmupReport
		if real {
			warmup, err = conversation.PrepareRealProductReport(ctx, *repository, turns, environment)
		} else {
			warmup, err = conversation.PrepareOfflineProductReport(ctx, *repository, turns)
		}
		snapshot = warmup.Snapshot
		paidCalls = len(warmup.Calls)
		err = errors.Join(err, write("model-calls.json", warmup.Calls), write("warmup.json", warmup))
		if err == nil {
			err = conversation.WriteSnapshot(filepath.Join(*output, "snapshot.json"), snapshot)
		}
	} else {
		var result conversation.ProductResult
		if real {
			result, err = conversation.RunRealProduct(ctx, *repository, snapshot, *question, environment, seed)
		} else {
			result, err = conversation.RunOfflineProduct(ctx, *repository, snapshot, *question)
		}
		if cause := context.Cause(ctx); cause != nil {
			err = errors.Join(err, cause)
			result.Error = err.Error()
			result.BackgroundAudit.Valid = false
			result.BackgroundAudit.Reason = result.Error
		}
		paidCalls = len(result.Calls)
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
	manifest := map[string]any{"scope": conversation.ProductScope, "env": *env, "phase": *phase, "paid_model_calls": paidCalls, "quality_and_cost_verified": false, "success": err == nil}
	if real {
		manifest["proxy"] = environment.ProxyBaseURL
		manifest["evaluation_index"] = environment.RedisAddress
		manifest["chat_model"] = environment.ChatModel
		manifest["embedding_model"] = environment.EmbeddingModel
		manifest["rerank_model"] = environment.RerankModel
		manifest["user_id"] = environment.UserID
		manifest["user_name"] = environment.UserName
		manifest["run_id"] = *runID
		manifest["session_id"] = environment.SessionID
	}
	if inputDigest != "" {
		manifest["input_sha256"] = inputDigest
	}
	outputs := map[string]string{}
	for _, name := range []string{"snapshot.json", "model-calls.json", "warmup.json", "result.json", "record.jsonl", "index-report.json"} {
		hash, hashErr := fileDigest(filepath.Join(*output, name))
		if hashErr == nil {
			outputs[name] = hash
		} else if !os.IsNotExist(hashErr) {
			err = errors.Join(err, hashErr)
		}
	}
	manifest["outputs"] = outputs
	if inputDigest != "" {
		if current, hashErr := fileDigest(*input); hashErr != nil || current != inputDigest {
			err = errors.Join(err, fmt.Errorf("input changed during attempt"))
		}
	}
	manifest["success"] = err == nil
	if err != nil {
		manifest["error"] = err.Error()
	}
	err = errors.Join(err, write("manifest.json", manifest))
	if real {
		fmt.Fprintln(out, "REAL RUN: provider models reached through the counting proxy; costs are metered by the operator's ledger")
	} else {
		fmt.Fprintln(out, "OFFLINE ONLY: standard product entry, disposable MySQL/Redis, loopback fake model; no quality or cost conclusion")
	}
	return err
}

// realEnvironment collects the paid-run wiring. The index password is read
// from the process environment so that it never reaches a command line, an
// output file, or an error message.
func realEnvironment(flags *flag.FlagSet, proxy, redis, redisPassword, storage, knowledgeSeed, chatModel, embeddingModel, rerankModel *string, userID *int64, userName, sessionID *string) (conversation.RealEnvironment, error) {
	var environment conversation.RealEnvironment
	if *proxy == "" || *redis == "" || *storage == "" || *chatModel == "" || *embeddingModel == "" || *rerankModel == "" || *userID <= 0 || *userName == "" || *sessionID == "" {
		return environment, fmt.Errorf("the real mode needs the proxy, index, storage, models and evaluation identity")
	}
	if *redisPassword == "" {
		return environment, fmt.Errorf("the real mode needs the index password variable")
	}
	password, ok := os.LookupEnv(*redisPassword)
	if !ok || password == "" {
		return environment, fmt.Errorf("the index password variable is not set")
	}
	environment = conversation.RealEnvironment{
		ProxyBaseURL:   *proxy,
		RedisAddress:   *redis,
		RedisPassword:  password,
		StorageDir:     *storage,
		ChatModel:      *chatModel,
		EmbeddingModel: *embeddingModel,
		RerankModel:    *rerankModel,
		UserID:         *userID,
		UserName:       *userName,
		SessionID:      *sessionID,
	}
	return environment, environment.Validate()
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
