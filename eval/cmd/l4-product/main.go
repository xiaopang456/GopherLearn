// Command l4-product prepares conversation histories and asks the final
// question through the standard product entry. The offline mode uses only
// fake transports. The real mode reaches the provider models through the
// operator's counting proxy, which holds the credentials, enforces the call
// caps and writes the stop file.
package main

import (
	"bytes"
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
	questionsFile := flags.String("questions", "", "file holding the questions of the attempt, in order (JSON array of strings)")
	seedMemory := flags.String("seed-memory", "", "file holding the profile memories the learner confirmed earlier (JSON array)")
	runID := flags.String("run-id", "", "evaluation run identifier")
	qid := flags.String("qid", "synthetic-offline", "scenario identifier for the scoring record")
	arm := flags.String("arm", "current", "configuration identifier")
	rep := flags.Int("rep", 0, "repeat number: 0, 1 or 2")
	condition := flags.String("condition", "main", "main, control or distractor")
	mode := flags.String("mode", "off", "the arm to measure: off, shadow or on")
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
	if *seedMemory != "" {
		guarded["seed-memory"] = *seedMemory
	}
	if *questionsFile != "" {
		guarded["questions"] = *questionsFile
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
	var attempt conversation.AttemptInput
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
		if *questionsFile != "" && (*question != "" || *questionFile != "") {
			return fmt.Errorf("the questions of an attempt come either from -questions or from -question")
		}
		if *questionsFile != "" {
			asked, readErr := readQuestions(*questionsFile)
			if readErr != nil {
				return readErr
			}
			attempt.Questions = asked
		} else {
			if *question == "" {
				return fmt.Errorf("question required")
			}
			attempt.Questions = []conversation.Question{{Text: *question}}
		}
		snapshot, err = conversation.ReadSnapshot(*input)
		if err != nil {
			return err
		}
	}
	if *seedMemory != "" {
		memories, readErr := readSeedMemories(*seedMemory)
		if readErr != nil {
			return readErr
		}
		attempt.Memories = memories
	}
	real := *env == "real"
	var environment conversation.RealEnvironment
	var seed *conversation.KnowledgeSeed
	if real {
		explicit := map[string]bool{}
		flags.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
		id, name, err := deriveScenarioIdentity(explicit, identityInput{
			phase: *phase, run: *runID, arm: *arm, qid: *qid, condition: *condition,
			rep: *rep, knowledgeSeed: *knowledgeSeed, userID: *userID, userName: *userName,
		})
		if err != nil {
			return err
		}
		*userID, *userName = id, name
		environment, err = realEnvironment(flags, proxy, redis, redisPassword, storage, knowledgeSeed, chatModel, embeddingModel, rerankModel, userID, userName, sessionID, *mode)
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
			warmup, err = conversation.PrepareRealProductReport(ctx, *repository, turns, environment, attempt)
		} else {
			warmup, err = conversation.PrepareOfflineProductReport(ctx, *repository, turns, attempt)
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
			result, err = conversation.RunRealProduct(ctx, *repository, snapshot, environment, seed, attempt)
		} else {
			result, err = conversation.RunOfflineProduct(ctx, *repository, snapshot, attempt)
		}
		if cause := context.Cause(ctx); cause != nil {
			err = errors.Join(err, cause)
			result.Error = err.Error()
			result.BackgroundAudit.Valid = false
			result.BackgroundAudit.Reason = result.Error
		}
		paidCalls = len(result.Calls)
		err = errors.Join(err, write("result.json", result))
		// The model ledger of this attempt is written next to the result.
		// Every call carries the role it was made in and the purpose it was
		// made for, and a ledger that is only inside the result file cannot
		// be read by anything that does not parse the whole result.
		err = errors.Join(err, write("model-calls.json", result.Calls))
		// One record per question. An attempt that asks one question writes
		// one record, and an attempt that asks several questions in one
		// session writes one record per question, each with its own answer
		// and its own question number.
		encoded, encodeResultErr := json.Marshal(result)
		if encodeResultErr != nil {
			return errors.Join(err, encodeResultErr)
		}
		var records bytes.Buffer
		for index, asked := range result.Questions {
			var record map[string]any
			if decodeErr := json.Unmarshal(encoded, &record); decodeErr != nil {
				return decodeErr
			}
			record["qid"], record["arm"], record["rep"], record["condition"] = attemptQID(*qid, index, len(result.Questions)), *arm, *rep, *condition
			record["question"], record["answer"] = asked.Question, asked.Answer
			// The question index of a record is the round of the conversation the
			// question was asked at, when the attempt says which round that is.
			// The turn says where in the conversation the answer has to come
			// from; the position in the list says only where in the file the
			// question was written down.
			record["question_index"] = index
			if asked.Turn > 0 {
				record["question_index"] = asked.Turn
			}
			if len(result.Questions) > 1 {
				record["response"], record["run_id"], record["steps"] = asked.Response, asked.RunID, asked.Steps
			}
			data, encodeErr := json.Marshal(record)
			if encodeErr != nil {
				return encodeErr
			}
			records.Write(append(data, '\n'))
		}
		file, fileErr := os.OpenFile(filepath.Join(*output, "record.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if fileErr == nil {
			_, fileErr = file.Write(records.Bytes())
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
		manifest["summary_segment_mode"] = environment.SummarySegmentMode
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
func realEnvironment(flags *flag.FlagSet, proxy, redis, redisPassword, storage, knowledgeSeed, chatModel, embeddingModel, rerankModel *string, userID *int64, userName, sessionID *string, mode string) (conversation.RealEnvironment, error) {
	var environment conversation.RealEnvironment
	if *proxy == "" || *redis == "" || *storage == "" || *chatModel == "" || *embeddingModel == "" || *rerankModel == "" || *userID <= 0 || *userName == "" || *sessionID == "" {
		return environment, fmt.Errorf("the real mode needs the proxy, index, storage, models and evaluation identity")
	}
	if *redisPassword == "" {
		return environment, fmt.Errorf("the real mode needs the index password variable")
	}
	if !conversation.SummarySegmentModeKnown(mode) {
		return environment, fmt.Errorf("the mode must be off, shadow or on")
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
		// The arm is part of the environment rather than of the run label:
		// the product reads the mode once, when it starts, and a run whose
		// label does not match the mode it booted with cannot be compared
		// with anything.
		SummarySegmentMode: mode,
	}
	return environment, environment.Validate()
}

// attemptQID names one attempt of one scenario.
//
// An attempt that asked one question is the scenario. An attempt that asked
// several questions in one session is one attempt per question, and the
// question's number is on the identifier, so that the scorer can tell which
// answer key the attempt belongs to.
func attemptQID(qid string, index, total int) string {
	if total <= 1 {
		return qid
	}
	return fmt.Sprintf("%s#q%d", qid, index+1)
}

// readQuestions reads the questions of one attempt, in the order they are
// asked.
//
// The file is a JSON array, and each entry is either the text of a question
// or an object that says which round of the conversation the question was
// asked at:
//
//	["第一问", {"turn": 45, "question": "那第二项呢？"}]
//
// The turn is what the scorer reads the attempt by, and a question that names
// no turn is asked at the end of the conversation.
//
// The file is read with a limit and without unknown fields, because a
// hand-written file can carry a field the command does not know.
func readQuestions(path string) ([]conversation.Question, error) {
	raw, err := readLimitedFile(path)
	if err != nil {
		return nil, err
	}
	var entries []json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&entries); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("questions file contains trailing data")
	}
	if len(entries) < 1 || len(entries) > 3 {
		return nil, fmt.Errorf("an attempt asks one to three questions")
	}
	questions := make([]conversation.Question, 0, len(entries))
	for _, entry := range entries {
		text := ""
		if err := json.Unmarshal(entry, &text); err == nil {
			questions = append(questions, conversation.Question{Text: text})
			continue
		}
		var asked struct {
			Turn     int    `json:"turn"`
			Question string `json:"question"`
		}
		if err := json.Unmarshal(entry, &asked); err != nil {
			return nil, fmt.Errorf("a question is neither a string nor an object")
		}
		if asked.Turn < 0 {
			return nil, fmt.Errorf("a question names a turn that is not a round of the conversation")
		}
		questions = append(questions, conversation.Question{Text: asked.Question, Turn: asked.Turn})
	}
	for _, question := range questions {
		if strings.TrimSpace(question.Text) == "" {
			return nil, fmt.Errorf("a question cannot be empty")
		}
	}
	return questions, nil
}

// readSeedMemories reads the profile memories one attempt seeds.
//
// The file is a JSON array of the memories, and it is read with a limit and
// without unknown fields, for the same reason the questions file is.
func readSeedMemories(path string) ([]conversation.SeedMemory, error) {
	raw, err := readLimitedFile(path)
	if err != nil {
		return nil, err
	}
	var memories []conversation.SeedMemory
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&memories); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("seed memory file contains trailing data")
	}
	if len(memories) < 1 || len(memories) > 8 {
		return nil, fmt.Errorf("an attempt seeds one to eight profile memories")
	}
	return memories, nil
}

// readLimitedFile reads a private input file with a size limit.
func readLimitedFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > 1<<20 {
		return nil, fmt.Errorf("input file exceeds limit")
	}
	return io.ReadAll(io.LimitReader(file, 1<<20))
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
