package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/knowledge"
	"github.com/xiaopang456/GopherLearn/internal/model"
	"github.com/xiaopang456/GopherLearn/internal/openai"
	"github.com/xiaopang456/GopherLearn/internal/repository"
	"github.com/xiaopang456/GopherLearn/internal/responder"
	"github.com/xiaopang456/GopherLearn/internal/service"
)

// testPhrase stands in for the real authorization phrase. It exists so the whole
// accepted path can be executed in tests without the production phrase - and
// without a provider - ever being typed or run.
const testPhrase = "local-test-phrase-not-the-real-one"

const testKey = "sk-smoke-test-key-must-never-be-recorded"

// scriptChat is a provider stand-in that answers the smoke's three shapes and
// records what it was asked.
type scriptChat struct {
	requests []model.Request
	failWith error
	// chunked is what Stream emits; Complete returns reply.
	reply  string
	chunks []string
}

func (c *scriptChat) Complete(_ context.Context, request model.Request) (model.Completion, error) {
	c.requests = append(c.requests, request)
	if c.failWith != nil {
		return model.Completion{}, c.failWith
	}
	return model.Completion{Content: c.reply, FinishReason: model.FinishStop, Model: "smoke-model"}, nil
}

func (c *scriptChat) Stream(_ context.Context, request model.Request, onDelta model.DeltaHandler) (model.Completion, error) {
	c.requests = append(c.requests, request)
	if c.failWith != nil {
		return model.Completion{}, c.failWith
	}
	var assembled strings.Builder
	for _, chunk := range c.chunks {
		if err := onDelta(chunk); err != nil {
			return model.Completion{}, err
		}
		assembled.WriteString(chunk)
	}
	return model.Completion{Content: assembled.String(), FinishReason: model.FinishStop}, nil
}

func defaultChat() *scriptChat {
	return &scriptChat{
		reply:  "AOF 是以追加写日志记录每条写命令的持久化方式。",
		chunks: []string{"RDB ", "是内存快照。"},
	}
}

// recorder captures the side effects a refused run must not have.
type recorder struct {
	statCalls  []string
	existing   map[string]bool
	mkdirCalls []string
	written    map[string][]byte
	stdout     bytes.Buffer
	nowValue   time.Time
	// beforeWrite runs between "the preflight said the path was free" and the
	// create itself, which is exactly the window a second concurrent run exploits.
	beforeWrite func(path string)
}

func newRecorder() *recorder {
	return &recorder{
		written:  map[string][]byte{},
		existing: map[string]bool{},
		nowValue: time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC),
	}
}

func (r *recorder) dependencies(chat model.ChatModel) dependencies {
	return dependencies{
		authorized: func(candidate string) bool { return candidate == testPhrase },
		newModel: func(openai.Config) (model.ChatModel, error) {
			if chat == nil {
				return nil, errors.New("no chat installed")
			}
			return chat, nil
		},
		stat: func(path string) (fs.FileInfo, error) {
			r.statCalls = append(r.statCalls, path)
			if r.existing[path] {
				return fakeFileInfo(path), nil
			}
			return nil, fs.ErrNotExist
		},
		mkdirAll: func(path string) error {
			r.mkdirCalls = append(r.mkdirCalls, path)
			return nil
		},
		writeFile: func(path string, data []byte) error {
			if r.beforeWrite != nil {
				r.beforeWrite(path)
			}
			// The same exclusive-create rule the production writer uses: an existing
			// path is refused, and a created one becomes existing.
			if r.existing[path] {
				return &fs.PathError{Op: "open", Path: path, Err: fs.ErrExist}
			}
			r.written[path] = append([]byte(nil), data...)
			r.existing[path] = true
			return nil
		},
		now:    func() time.Time { return r.nowValue },
		stdout: &r.stdout,
	}
}

// live swaps the provider stand-in for a real client, so a test can put an
// httptest server between the smoke and the wire and still count requests.
func (r *recorder) live() dependencies {
	deps := r.dependencies(nil)
	deps.newModel = func(config openai.Config) (model.ChatModel, error) { return openai.New(config) }
	return deps
}

func environment(overrides map[string]string) map[string]string {
	values := map[string]string{
		appProviderEnv():  "openai_compatible",
		openai.EnvBaseURL: "https://provider.invalid/v1",
		openai.EnvAPIKey:  testKey,
		openai.EnvModel:   "smoke-model",
	}
	for key, value := range overrides {
		if value == "" {
			delete(values, key)
			continue
		}
		values[key] = value
	}
	return values
}

func appProviderEnv() string { return "GOPHERLEARN_AI_PROVIDER" }

func lookupOf(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func authorizedArgs(output string) []string {
	return []string{"-authorize", testPhrase, "-out", output}
}

const smokeOutput = "out/provider-smoke-v1.json"

func TestRunRefusesWithoutAuthorization(t *testing.T) {
	recorder := newRecorder()
	chat := defaultChat()
	code, err := run([]string{"-out", smokeOutput}, lookupOf(environment(nil)), recorder.dependencies(chat))
	if code != 1 {
		t.Fatalf("exit code = %d, want refusal", code)
	}
	if !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("error = %v, want ErrNotAuthorized", err)
	}
	assertNothingReached(t, recorder, chat)
}

func TestRunRefusesAWrongAuthorizationPhrase(t *testing.T) {
	for _, phrase := range []string{"model-smoke-2", "Model-Smoke-3", " model-smoke-3", "model-smoke-3 ", "", "no"} {
		recorder := newRecorder()
		chat := defaultChat()
		args := []string{"-authorize", phrase, "-out", smokeOutput}
		if _, err := run(args, lookupOf(environment(nil)), recorder.dependencies(chat)); !errors.Is(err, ErrNotAuthorized) {
			t.Fatalf("phrase %q error = %v, want ErrNotAuthorized", phrase, err)
		}
		assertNothingReached(t, recorder, chat)
	}
	// The accepted phrase itself must never be reproduced in a refusal, or a
	// terminal scrollback becomes a record of how to authorize a paid call.
	recorder := newRecorder()
	if _, refusal := run([]string{"-authorize", "wrong", "-out", smokeOutput}, lookupOf(environment(nil)), recorder.dependencies(defaultChat())); refusal == nil || strings.Contains(recorder.stdout.String(), authorizationPhrase) {
		t.Fatalf("refusal printed the authorization phrase: %q", recorder.stdout.String())
	}
}

// TestProductionAuthorizerIsByteExact covers the rule the operator's own typed
// phrase is measured against - the other refusal tests inject their own phrase,
// so this closure is otherwise never exercised. A case-insensitive comparison
// here would turn a mistyped authorization into a paid call, which is exactly the
// mistake this command exists to prevent.
func TestProductionAuthorizerIsByteExact(t *testing.T) {
	accepts := productionDependencies(nil).authorized
	for _, candidate := range []string{
		strings.ToUpper(authorizationPhrase),
		strings.ToTitle(authorizationPhrase),
		"\t" + authorizationPhrase,
		authorizationPhrase + " ",
		authorizationPhrase[:len(authorizationPhrase)-1],
		"",
		testPhrase,
	} {
		if accepts(candidate) {
			t.Fatalf("the production authorizer accepted %q, want the exact phrase only", candidate)
		}
	}
}

func TestRunRefusesAConfigurationThatCannotReachAProvider(t *testing.T) {
	cases := []struct {
		name   string
		values map[string]string
		would  error
	}{
		{"echo provider", environment(map[string]string{appProviderEnv(): "echo"}), ErrWrongProvider},
		{"missing provider", environment(map[string]string{appProviderEnv(): ""}), ErrWrongProvider},
		{"unsupported provider", environment(map[string]string{appProviderEnv(): "ollama"}), ErrWrongProvider},
		{"missing base url", environment(map[string]string{openai.EnvBaseURL: ""}), openai.ErrMissingBaseURL},
		{"missing api key", environment(map[string]string{openai.EnvAPIKey: ""}), openai.ErrMissingAPIKey},
		{"blank api key", environment(map[string]string{openai.EnvAPIKey: "   "}), openai.ErrMissingAPIKey},
		{"missing model", environment(map[string]string{openai.EnvModel: ""}), openai.ErrMissingModel},
		{"credential-bearing url", environment(map[string]string{openai.EnvBaseURL: "https://sk-in-url@api.invalid/v1"}), openai.ErrInvalidBaseURL},
		{"bad timeout", environment(map[string]string{openai.EnvTimeout: "soon"}), openai.ErrInvalidTimeout},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := newRecorder()
			chat := defaultChat()
			code, err := run(authorizedArgs(smokeOutput), lookupOf(testCase.values), recorder.dependencies(chat))
			if code != 1 || !errors.Is(err, testCase.would) {
				t.Fatalf("error = %v (code %d), want %v", err, code, testCase.would)
			}
			assertNothingReached(t, recorder, chat)
		})
	}
}

func TestRunRefusesToOverwriteAnExistingReport(t *testing.T) {
	recorder := newRecorder()
	recorder.existing[smokeOutput] = true // an earlier authorized run left a record
	chat := defaultChat()
	_, err := run(authorizedArgs(smokeOutput), lookupOf(environment(nil)), recorder.dependencies(chat))
	if !errors.Is(err, ErrOutputExists) {
		t.Fatalf("error = %v, want ErrOutputExists", err)
	}
	// The output check is this refusal's reason, so the stat happens - but it is
	// the last thing to happen, and nothing is created, written or dialed.
	if len(recorder.statCalls) != 1 || recorder.statCalls[0] != smokeOutput {
		t.Fatalf("output checks = %v, want exactly one for %s", recorder.statCalls, smokeOutput)
	}
	if len(recorder.written) != 0 || len(recorder.mkdirCalls) != 0 {
		t.Fatalf("a refusal wrote %v or created %v", recorder.written, recorder.mkdirCalls)
	}
	if len(chat.requests) != 0 {
		t.Fatalf("a refusal asked the provider %d times", len(chat.requests))
	}
}

// TestPreflightTouchesNothingButTheOutputPath is the "no credential hunting"
// guarantee made observable: the only path inspected before a decision is the one
// the operator named as output.
func TestPreflightTouchesNothingButTheOutputPath(t *testing.T) {
	recorder := newRecorder()
	if _, err := preflight(settings{authorize: testPhrase, output: smokeOutput}, lookupOf(environment(nil)), recorder.dependencies(defaultChat())); err != nil {
		t.Fatal(err)
	}
	if len(recorder.statCalls) != 1 || recorder.statCalls[0] != smokeOutput {
		t.Fatalf("preflight inspected %v, want only the output path", recorder.statCalls)
	}
	if len(recorder.mkdirCalls) != 0 || len(recorder.written) != 0 {
		t.Fatalf("preflight created directories %v or files %v", recorder.mkdirCalls, recorder.written)
	}
}

func TestExecuteRunsThreeRequestsAndAsksNothingWithoutEvidence(t *testing.T) {
	recorder := newRecorder()
	chat := defaultChat()
	plan, refusal := preflight(settings{authorize: testPhrase, output: smokeOutput}, lookupOf(environment(nil)), recorder.dependencies(chat))
	if refusal != nil {
		t.Fatal(refusal)
	}
	report, err := execute(plan, recorder.dependencies(chat))
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}
	if len(chat.requests) != requestCap {
		t.Fatalf("provider requests = %d, want exactly %d", len(chat.requests), requestCap)
	}
	if report.ActualRequests != requestCap || report.RequestCap != requestCap || report.Failures != 0 {
		t.Fatalf("budget bookkeeping = %+v", report)
	}
	if !report.CompleteNonEmpty || !report.StreamNonEmpty || report.StreamDeltaCount != len(chat.chunks) {
		t.Fatalf("steps 1-2 = %+v", report)
	}
	if !report.KnowledgeAnswerNonEmpty || report.KnowledgeSourceCount == 0 {
		t.Fatalf("knowledge step did not cite its own document: %+v", report)
	}
	if !report.NoEvidenceSkipped {
		t.Fatal("the unrelated question did consume a provider request")
	}
	if report.Retries != 0 {
		t.Fatalf("retries = %d, want 0", report.Retries)
	}
	if report.FinishReason != string(model.FinishStop) {
		t.Fatalf("finish reason = %q", report.FinishReason)
	}
	if report.UsageIfReturned != nil {
		t.Fatalf("usage invented from nothing: %+v", report.UsageIfReturned)
	}
	// Each step carries its own facts, and the requests add up to the run's total
	// rather than each step claiming the whole counter.
	complete, streamed, grounded, skipped := stepOf(t, report, stepComplete), stepOf(t, report, stepStream), stepOf(t, report, stepKnowledge), stepOf(t, report, stepNoEvidence)
	for index, step := range []stepRecord{complete, streamed, grounded} {
		if step.Status != statusOK || step.FailureCode != nil {
			t.Fatalf("step %d = %+v, want ok with no failure code", index+1, step)
		}
		if step.RequestsUsed != 1 {
			t.Fatalf("step %q used %d requests, want exactly 1", step.Name, step.RequestsUsed)
		}
		if len(mustHash(t, step)) != 64 {
			t.Fatalf("step %q has no 64-character answer hash", step.Name)
		}
	}
	if skipped.Status != statusOK || skipped.RequestsUsed != 0 || skipped.AnswerSHA256 != nil {
		t.Fatalf("the zero-call step = %+v, want ok, no request and no answer hash", skipped)
	}
	if total := complete.RequestsUsed + streamed.RequestsUsed + grounded.RequestsUsed + skipped.RequestsUsed; total != report.ActualRequests {
		t.Fatalf("per-step requests sum to %d, want %d", total, report.ActualRequests)
	}
	// The knowledge turn must be the third request and must carry numbered
	// evidence plus the system policy, not a bare question.
	knowledgeRequest := chat.requests[2]
	question, ok := knowledgeRequest.LastUserContent()
	if !ok || !strings.Contains(question, knowledgeQuestion) || !strings.Contains(question, "[证据]") {
		t.Fatalf("knowledge prompt lost its evidence block: %q", question)
	}
	if knowledgeRequest.Messages[0].Content != responder.KnowledgeSystemPrompt {
		t.Fatal("the knowledge step used the wrong system policy")
	}
}

// TestThreeSuccessfulAnswersRecordThreeDigests is item two: each answer is hashed
// on its own, so a later reader can tell which step produced which bytes.
func TestThreeSuccessfulAnswersRecordThreeDigests(t *testing.T) {
	server, hits := providerEndpoint(t, streamSucceeds)
	recorder := newRecorder()
	code, err := run(authorizedArgs(smokeOutput), lookupOf(liveEnvironment(server.URL)), recorder.live())
	if code != 0 || err != nil {
		t.Fatalf("run against the test endpoint: code %d err %v", code, err)
	}
	if got := hits.Load(); got != requestCap {
		t.Fatalf("the test endpoint was dialed %d times, want %d", got, requestCap)
	}
	recorded := decodeReport(t, recorder)
	if recorded.UsageIfReturned == nil || recorded.UsageIfReturned.TotalTokens != 28 {
		t.Fatalf("usage from the endpoint was not recorded: %+v", recorded.UsageIfReturned)
	}
	seen := map[string]string{}
	for _, name := range []string{stepComplete, stepStream, stepKnowledge} {
		hash := mustHash(t, stepOf(t, recorded, name))
		if earlier, clash := seen[hash]; clash {
			t.Fatalf("steps %q and %q recorded the same answer hash %s", earlier, name, hash)
		}
		seen[hash] = name
	}
	// The answers themselves stay off the disk: only their digests are recorded.
	encoded := encodeReport(t, recorded)
	for _, leaked := range []string{completeAnswer, streamAnswerPartOne, streamAnswerPartTwo, knowledgeAnswer, testKey, "Authorization", "Bearer"} {
		if bytes.Contains(encoded, []byte(leaked)) {
			t.Fatalf("the report stored provider content %q:\n%s", leaked, encoded)
		}
	}
}

func TestReportCarriesNoCredential(t *testing.T) {
	recorder := newRecorder()
	chat := defaultChat()
	plan, refusal := preflight(settings{authorize: testPhrase, output: smokeOutput}, lookupOf(environment(nil)), recorder.dependencies(chat))
	if refusal != nil {
		t.Fatal(refusal)
	}
	report, err := execute(plan, recorder.dependencies(chat))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := report.encode()
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{testKey, "Authorization", "Bearer", knowledgeMarkdown, chat.reply, chat.chunks[0]} {
		if bytes.Contains(encoded, []byte(forbidden)) {
			t.Fatalf("the report recorded %q:\n%s", forbidden, encoded)
		}
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"schema_version", "executed_at", "provider", "endpoint_host", "model", "request_cap",
		"actual_requests", "retries", "failures", "complete_nonempty", "stream_nonempty", "stream_delta_count",
		"knowledge_answer_nonempty", "knowledge_source_count", "no_evidence_skipped_provider", "finish_reason",
		"usage_if_returned", "steps", "api_key_recorded", "real_mysql", "redis", "rabbitmq", "docker"} {
		if _, exists := decoded[field]; !exists {
			t.Fatalf("report is missing the contract field %q:\n%s", field, encoded)
		}
	}
	// One record per step, each with its own answer slot. A single shared
	// answer_sha256 at the top level would make the streamed and grounded answers
	// unidentifiable.
	steps, ok := decoded["steps"].([]any)
	if !ok || len(steps) != 4 {
		t.Fatalf("steps = %v, want four records", decoded["steps"])
	}
	for _, entry := range steps {
		record, isObject := entry.(map[string]any)
		if !isObject {
			t.Fatalf("step entry is not an object: %v", entry)
		}
		for _, field := range []string{"name", "status", "requests_used", "nonempty", "answer_sha256",
			"finish_reason", "delta_count", "source_count", "failure_code", "provider_status"} {
			if _, exists := record[field]; !exists {
				t.Fatalf("step %v is missing %q", record["name"], field)
			}
		}
	}
	if _, legacy := decoded["answer_sha256"]; legacy {
		t.Fatalf("the v1 top-level answer hash is back:\n%s", encoded)
	}
	if decoded["api_key_recorded"] != false || decoded["real_mysql"] != false || decoded["redis"] != false ||
		decoded["rabbitmq"] != false || decoded["docker"] != false {
		t.Fatalf("the report claims a side effect that did not happen:\n%s", encoded)
	}
	if decoded["endpoint_host"] != "provider.invalid" {
		t.Fatalf("endpoint_host = %v", decoded["endpoint_host"])
	}
	if decoded["schema_version"] != float64(schemaVersion) {
		t.Fatalf("schema_version = %v, want %d", decoded["schema_version"], schemaVersion)
	}
}

func TestRunWritesOnlyTheNamedReportAndPrintsNoCredential(t *testing.T) {
	recorder := newRecorder()
	chat := defaultChat()
	code, err := run(authorizedArgs(smokeOutput), lookupOf(environment(nil)), recorder.dependencies(chat))
	if err != nil || code != 0 {
		t.Fatalf("accepted run: code %d err %v", code, err)
	}
	if len(recorder.written) != 1 {
		t.Fatalf("files written = %v, want exactly one", recorder.written)
	}
	if _, exists := recorder.written[smokeOutput]; !exists {
		t.Fatalf("the report went to %v, want %s", recorder.written, smokeOutput)
	}
	if len(recorder.mkdirCalls) != 1 {
		t.Fatalf("directories created = %v", recorder.mkdirCalls)
	}
	line := recorder.stdout.String()
	for _, forbidden := range []string{testKey, authorizationPhrase, "Bearer"} {
		if strings.Contains(line, forbidden) {
			t.Fatalf("stdout leaked %q: %q", forbidden, line)
		}
	}
	if !strings.Contains(line, "model smoke completed") || !strings.Contains(line, "requests=3/3") ||
		!strings.Contains(line, "steps=4") || !strings.Contains(line, smokeOutput) {
		t.Fatalf("summary line = %q", line)
	}
}

func TestMeterRefusesTheFourthRequestBeforeTheProvider(t *testing.T) {
	var hits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"回答"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(server.Close)
	client, err := openai.New(openai.Config{BaseURL: server.URL, APIKey: testKey, Model: "m", Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	meter := newMetered(client, requestCap)
	request := model.Request{Messages: []model.Message{{Role: model.RoleUser, Content: "问题"}}}
	for attempt := 1; attempt <= requestCap; attempt++ {
		if _, err := meter.Complete(context.Background(), request); err != nil {
			t.Fatalf("attempt %d failed: %v", attempt, err)
		}
	}
	if _, err := meter.Stream(context.Background(), request, func(string) error { return nil }); !errors.Is(err, ErrRequestCapExceeded) {
		t.Fatalf("fourth attempt error = %v, want ErrRequestCapExceeded", err)
	}
	if meter.requests != requestCap {
		t.Fatalf("counted requests = %d, want %d", meter.requests, requestCap)
	}
	if hits.Load() != requestCap {
		t.Fatalf("the provider was dialed %d times, want %d", hits.Load(), requestCap)
	}
}

func TestMeterCountsFailuresWithoutRetrying(t *testing.T) {
	var hits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)
	client, err := openai.New(openai.Config{BaseURL: server.URL, APIKey: testKey, Model: "m", Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	meter := newMetered(client, requestCap)
	request := model.Request{Messages: []model.Message{{Role: model.RoleUser, Content: "问题"}}}
	for attempt := 0; attempt < requestCap; attempt++ {
		if _, err := meter.Complete(context.Background(), request); !errors.Is(err, model.ErrProviderStatus) {
			t.Fatalf("attempt %d error = %v, want ErrProviderStatus", attempt, err)
		}
	}
	if _, err := meter.Complete(context.Background(), request); !errors.Is(err, ErrRequestCapExceeded) {
		t.Fatalf("fourth attempt error = %v", err)
	}
	// A failed request is still a request: it was sent, and it was not resent.
	if hits.Load() != requestCap {
		t.Fatalf("the failing provider was dialed %d times, want %d with no retry", hits.Load(), requestCap)
	}
	if meter.failures != requestCap || meter.requests != requestCap {
		t.Fatalf("meter = %+v, want %d requests and %d failures", meter, requestCap, requestCap)
	}
}

func TestExecuteReportsStepFailuresAndStillRecordsTheBudget(t *testing.T) {
	recorder := newRecorder()
	chat := defaultChat()
	chat.failWith = model.ErrUnavailable
	plan, refusal := preflight(settings{authorize: testPhrase, output: smokeOutput}, lookupOf(environment(nil)), recorder.dependencies(chat))
	if refusal != nil {
		t.Fatal(refusal)
	}
	report, err := execute(plan, recorder.dependencies(chat))
	if err == nil {
		t.Fatal("an outage was reported as success")
	}
	if report.CompleteNonEmpty || report.StreamNonEmpty || report.KnowledgeAnswerNonEmpty {
		t.Fatalf("failed steps reported success: %+v", report)
	}
	if report.ActualRequests != requestCap || report.Failures != requestCap {
		t.Fatalf("budget bookkeeping = %+v, want %d requests and %d failures", report, requestCap, requestCap)
	}
	// Even with a broken provider the no-evidence path must not have asked anything
	// of it: three attempts were spent on the three real steps.
	if !report.NoEvidenceSkipped {
		t.Fatal("an unrelated question was attempted against the provider too")
	}
	// A failed step records a null answer instead of pretending it said something.
	for _, name := range []string{stepComplete, stepStream, stepKnowledge} {
		step := stepOf(t, report, name)
		if step.Status != statusFailed || step.FailureCode == nil || *step.FailureCode == "" {
			t.Fatalf("failed step %q is not marked with a code: %+v", name, step)
		}
		if step.AnswerSHA256 != nil {
			t.Fatalf("a failed step %q recorded an answer hash", name)
		}
	}
}

// TestRunArchivesAPartiallyFailedRun is item one with a real client on the wire:
// when only the streaming step fails, the file must still say how many requests
// were spent and which step failed - and it must not carry the endpoint's words.
func TestRunArchivesAPartiallyFailedRun(t *testing.T) {
	server, hits := providerEndpoint(t, streamRefused)
	recorder := newRecorder()
	code, err := run(authorizedArgs(smokeOutput), lookupOf(liveEnvironment(server.URL)), recorder.live())
	if code != 1 {
		t.Fatalf("exit code = %d with %v, want 1 for a partially failed run", code, err)
	}
	if err == nil || !errors.Is(err, model.ErrProviderStatus) {
		t.Fatalf("error = %v, want the provider status failure to reach the operator", err)
	}
	if got := hits.Load(); got != requestCap {
		t.Fatalf("the endpoint was dialed %d times, want %d", got, requestCap)
	}
	encoded, exists := recorder.written[smokeOutput]
	if !exists {
		t.Fatalf("a partially failed run left no record behind: %v", recorder.written)
	}
	report := decodeReport(t, recorder)
	if report.ActualRequests != requestCap || report.Failures != 1 || report.Retries != 0 {
		t.Fatalf("counts in the file do not match what happened: %+v", report)
	}
	failed := stepOf(t, report, stepStream)
	if failed.Status != statusFailed || failed.RequestsUsed != 1 || failed.AnswerSHA256 != nil ||
		failed.FailureCode == nil || *failed.FailureCode != failProviderStatus {
		t.Fatalf("the stream step = %+v, want one failed request with a null answer", failed)
	}
	if failed.ProviderStatus == nil || *failed.ProviderStatus != http.StatusServiceUnavailable {
		t.Fatalf("provider_status = %v, want %d", failed.ProviderStatus, http.StatusServiceUnavailable)
	}
	for _, name := range []string{stepComplete, stepKnowledge} {
		if step := stepOf(t, report, name); step.Status != statusOK || step.ProviderStatus != nil {
			t.Fatalf("step %q = %+v, want ok beside the failure", name, step)
		}
	}
	if stepOf(t, report, stepNoEvidence).RequestsUsed != 0 || !report.NoEvidenceSkipped {
		t.Fatalf("the zero-call step spent a request during a partial failure: %+v", report.Steps)
	}
	// The endpoint's error body, its answers and the key are all absent: the file
	// carries codes and numbers, not text from the other side.
	for _, forbidden := range []string{providerFailureText, completeAnswer, knowledgeAnswer, testKey, "Authorization", "Bearer"} {
		if bytes.Contains(encoded, []byte(forbidden)) {
			t.Fatalf("the record kept %q from the provider:\n%s", forbidden, encoded)
		}
	}
	if !strings.Contains(recorder.stdout.String(), "model smoke incomplete") {
		t.Fatalf("summary line = %q, want it to say the run is incomplete", recorder.stdout.String())
	}
	// Item three: the text main() prints is this error's, so it may carry only the
	// closed-set code, the numeric status and the path.
	spoken := err.Error()
	if !strings.Contains(spoken, "stream="+failProviderStatus+"/"+strconv.Itoa(http.StatusServiceUnavailable)) ||
		!strings.Contains(spoken, smokeOutput) {
		t.Fatalf("failure summary = %q, want the code, the status and the output path", spoken)
	}
	for _, forbidden := range []string{providerFailureText, "type ", "code ", "message", testKey, completeAnswer, "Authorization", "Bearer"} {
		if strings.Contains(spoken, forbidden) {
			t.Fatalf("the operator's terminal would show %q: %q", forbidden, spoken)
		}
	}
}

// TestRunRefusesWhenTheReportAppearsAfterPreflight covers the window the preflight
// stat cannot see: another authorized run creates the file while this one is
// working. The exclusive create decides, and the older bytes survive.
func TestRunRefusesWhenTheReportAppearsAfterPreflight(t *testing.T) {
	recorder := newRecorder()
	competitor := []byte(`{"schema_version":1,"note":"an earlier authorized run"}`)
	recorder.beforeWrite = func(path string) {
		if path == smokeOutput && !recorder.existing[path] {
			recorder.existing[path] = true
			recorder.written[path] = append([]byte(nil), competitor...)
		}
	}
	chat := defaultChat()
	code, err := run(authorizedArgs(smokeOutput), lookupOf(environment(nil)), recorder.dependencies(chat))
	if code != 1 || !errors.Is(err, ErrOutputExists) {
		t.Fatalf("run = code %d err %v, want a refusal naming the existing output", code, err)
	}
	if !bytes.Equal(recorder.written[smokeOutput], competitor) {
		t.Fatalf("the older report was replaced with %q", recorder.written[smokeOutput])
	}
	if len(chat.requests) != requestCap {
		t.Fatalf("the steps ran %d times, want the three real requests", len(chat.requests))
	}
}

// TestProductionWriterNeverReplacesExistingBytes checks the real os.OpenFile flags
// on a real file, because everything above goes through a stand-in.
func TestProductionWriterNeverReplacesExistingBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "provider-smoke-v1.json")
	original := []byte("{\"sentinel\":\"do-not-touch\"}\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	err := productionDependencies(nil).writeFile(path, []byte(`{"schema_version":2}`))
	if !errors.Is(err, os.ErrExist) {
		t.Fatalf("write over an existing file = %v, want an ErrExist refusal", err)
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(after, original) {
		t.Fatalf("the existing file changed:\n%s", after)
	}
}

// TestMain guards every artifact a paid run can leave behind: this command's own provider
// record and both agent tool-calling records. It says nothing about whether such a run has
// ever happened - a checkout either has the files or does not, and the digests are computed
// rather than stored here, so no specific run's SHA becomes a test constant. What it does
// assert is that this suite cannot touch any of them: a record that was absent must not
// appear, and one that was there must come out byte-identical.
func TestMain(m *testing.M) {
	before := sealedState()
	code := m.Run()
	if problem := sealedGuardProblem(before, sealedState()); problem != "" {
		fmt.Fprintf(os.Stderr, "sealed record guard: %s\n", problem)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

// sealedRecord is what the guard knows about one artifact.
type sealedRecord struct {
	path   string
	exists bool
	digest string
}

// sealedRelativePaths are the read-only artifacts. The agent records belong to another
// command, and a run of this suite has no business creating or editing them either: every
// sealed path is guarded in every package that could reach it.
var sealedRelativePaths = []string{
	defaultOutputPath,
	"spec/verification/agent-tool-smoke-v1.json",
	"spec/verification/agent-tool-smoke-v2.json",
}

func sealedState() (state []sealedRecord) {
	root := mustRepoRoot()
	for _, relative := range sealedRelativePaths {
		entry := sealedRecord{path: filepath.Join(root, filepath.FromSlash(relative))}
		if raw, err := os.ReadFile(entry.path); err == nil {
			entry.exists = true
			entry.digest = hashText(string(raw))
		}
		state = append(state, entry)
	}
	return state
}

// sealedGuardProblem compares two observations and returns "" when the suite left every
// record alone.
func sealedGuardProblem(before, after []sealedRecord) string {
	for index := range before {
		if index >= len(after) {
			return "the guard lost track of a sealed record"
		}
		earlier, later := before[index], after[index]
		switch {
		case !earlier.exists && !later.exists:
		case !earlier.exists:
			return fmt.Sprintf("%s appeared while the suite ran: no test may write a sealed record", earlier.path)
		case !later.exists:
			return fmt.Sprintf("%s disappeared while the suite ran: a paid run's record is evidence and is never deleted", earlier.path)
		case earlier.digest != later.digest:
			return fmt.Sprintf("%s changed while the suite ran: %s -> %s",
				later.path, shortDigest(earlier.digest), shortDigest(later.digest))
		}
	}
	return ""
}

// shortDigest keeps a guard message readable without pasting a whole record hash into
// test output. It never assumes the caller handed it a full one.
func shortDigest(digest string) string {
	if len(digest) <= 12 {
		return digest
	}
	return digest[:12]
}

// TestSealedGuardAcceptsOnlyUntouchedRecords checks the guard itself, because in a normal
// run its failure branch cannot be reached - the suite really does leave the files alone.
func TestSealedGuardAcceptsOnlyUntouchedRecords(t *testing.T) {
	present := sealedRecord{path: "a", exists: true, digest: "aaaa"}
	other := sealedRecord{path: "a", exists: true, digest: "bbbb"}
	absent := sealedRecord{path: "a"}
	cases := []struct {
		name   string
		before sealedRecord
		after  sealedRecord
		wantOK bool
	}{
		{"absent and still absent", absent, absent, true},
		{"present and unchanged", present, present, true},
		{"created by the suite", absent, present, false},
		{"deleted by the suite", present, absent, false},
		{"rewritten by the suite", present, other, false},
	}
	for _, testCase := range cases {
		problem := sealedGuardProblem([]sealedRecord{testCase.before}, []sealedRecord{testCase.after})
		if (problem == "") != testCase.wantOK {
			t.Fatalf("%s: problem = %q, wantOK %v", testCase.name, problem, testCase.wantOK)
		}
	}
	// All three artifacts are watched, in this order, and one of them is another
	// command's file: touching it from here would be a silent rewrite of paid evidence.
	watched := sealedState()
	if len(watched) != len(sealedRelativePaths) || len(watched) != 3 {
		t.Fatalf("the guard watches %d records, want the provider record and both agent records", len(watched))
	}
	for index, want := range sealedRelativePaths {
		if filepath.ToSlash(watched[index].path) == filepath.ToSlash(want) {
			t.Fatalf("watched[%d] = %s, want it joined onto the module root", index, watched[index].path)
		}
		if filepath.Base(watched[index].path) != filepath.Base(want) {
			t.Fatalf("watched[%d] = %s, want %s", index, watched[index].path, want)
		}
	}
}

// TestTheSealedArtifactsAreFoundWhereTheGuardLooks confirms the guard is pointed at the
// module root rather than at this package's own directory, where a relative path would
// resolve to nothing and pass for the wrong reason.
func TestTheSealedArtifactsAreFoundWhereTheGuardLooks(t *testing.T) {
	root := repoRoot(t)
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("guard root %s is not the module root: %v", root, err)
	}
	// A fresh checkout has no records at all; an audited one has all three. Either is
	// fine, and neither is a test constant - but a present record must carry a full
	// digest, which is what proves the guard read a real file rather than a path it made up.
	for _, entry := range sealedState() {
		if entry.exists && len(entry.digest) != 64 {
			t.Fatalf("%s: present with a partial digest %q", entry.path, entry.digest)
		}
	}
}

// repoRoot walks up from the test's working directory to the module root, the only
// place the default output path is meaningful.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := findRepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func mustRepoRoot() string {
	root, err := findRepoRoot()
	if err != nil {
		panic(err)
	}
	return root
}

func findRepoRoot() (string, error) {
	directory, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for current := directory; ; current = filepath.Dir(current) {
		if info, statErr := os.Stat(filepath.Join(current, "go.mod")); statErr == nil && !info.IsDir() {
			return current, nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("no go.mod above %s: cannot locate the default output path", directory)
		}
	}
}

// fakeFileInfo is a stat result that proves only "this path exists".
type fakeFileInfo string

func (f fakeFileInfo) Name() string { return string(f) }

func (f fakeFileInfo) Size() int64 { return int64(len(string(f))) }

func (f fakeFileInfo) Mode() fs.FileMode { return 0o600 }

func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }

func (f fakeFileInfo) IsDir() bool { return false }

func (f fakeFileInfo) Sys() any { return nil }

// assertNothingReached is the "0 requests, 0 files" half of every refusal: not
// only no answer and no report, but no side effect at all - the output path is not
// even inspected until the configuration has been accepted.
func assertNothingReached(t *testing.T, recorder *recorder, chat *scriptChat) {
	t.Helper()
	if len(recorder.statCalls) != 0 {
		t.Fatalf("a refusal inspected the output path %v before checking its reason", recorder.statCalls)
	}
	if len(recorder.written) != 0 {
		t.Fatalf("a refused run wrote %v", recorder.written)
	}
	if len(recorder.mkdirCalls) != 0 {
		t.Fatalf("a refused run created directories %v", recorder.mkdirCalls)
	}
	if len(chat.requests) != 0 {
		t.Fatalf("a refused run asked the provider %d times", len(chat.requests))
	}
}

// TestRunArchivesASetupFailureKeepsTheCountsAtZero covers the other half of item
// one: a run that dies before any step could be judged still writes a record, and
// that record says nothing was spent rather than implying a run happened.
func TestRunArchivesASetupFailureKeepsTheCountsAtZero(t *testing.T) {
	recorder := newRecorder()
	code, err := run(authorizedArgs(smokeOutput), lookupOf(environment(nil)), recorder.dependencies(nil))
	if code != 1 || err == nil {
		t.Fatalf("setup failure = code %d err %v, want a non-zero exit with a reason", code, err)
	}
	recorded := decodeReport(t, recorder)
	if len(recorded.Steps) != 1 {
		t.Fatalf("steps = %+v, want only the setup record", recorded.Steps)
	}
	setup := stepOf(t, recorded, stepSetup)
	if setup.Status != statusFailed || setup.RequestsUsed != 0 || setup.FailureCode == nil ||
		*setup.FailureCode != failUnclassified {
		t.Fatalf("setup record = %+v", setup)
	}
	if recorded.ActualRequests != 0 || recorded.Failures != 0 || recorded.Retries != 0 {
		t.Fatalf("a run that never reached the provider recorded %+v", recorded)
	}
	// None of the headline facts may claim success, and no_evidence_skipped_provider
	// stays false because the check never ran to confirm it.
	if recorded.CompleteNonEmpty || recorded.StreamNonEmpty || recorded.KnowledgeAnswerNonEmpty ||
		recorded.NoEvidenceSkipped || filledHashes(recorded) != 0 {
		t.Fatalf("unverified facts recorded as success: %+v", recorded)
	}
}

// filledHashes counts the answer slots a report actually filled.
func filledHashes(recorded report) int {
	var filled int
	for _, step := range recorded.Steps {
		if step.AnswerSHA256 != nil {
			filled++
		}
	}
	return filled
}

// foreignCitation answers fluently but cites a document this run never created, which
// is what a model that invented a source - or a scope leak - looks like.
type foreignCitation struct{}

func (foreignCitation) Respond(context.Context, responder.Request) (responder.Answer, error) {
	return responder.Answer{
		Content: foreignAnswer,
		Sources: []domain.Source{{
			SourceID:     "src-invented",
			DocumentID:   "someone-elses-document",
			CollectionID: "someone-elses-collection",
			ChunkID:      "someone-elses-chunk",
			Title:        "别的手册",
			Snippet:      "与本题无关的一段话",
		}},
	}, nil
}

func (f foreignCitation) Stream(ctx context.Context, request responder.Request, emit func(string) error) (responder.Answer, error) {
	answer, err := f.Respond(ctx, request)
	if err == nil && emit != nil {
		err = emit(answer.Content)
	}
	return answer, err
}

// TestKnowledgeStepRefusesAForeignCitation checks the citation-ownership rule
// directly, because nothing else in this file can see it: the real strategy only
// ever derives citations from retrieved chunks, so a wrong answer there arrives only
// through a broken or invented source.
func TestKnowledgeStepRefusesAForeignCitation(t *testing.T) {
	ctx := context.Background()
	repo := repository.NewInMemoryRepository()
	store := knowledge.NewMemoryStore()
	userID, err := smokeUser(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	record, err := runKnowledge(ctx, repo, store, foreignCitation{}, resolved{budget: service.ContextBudget{}.Normalize()}, userID)
	if !errors.Is(err, ErrForeignCitation) {
		t.Fatalf("error = %v, want the foreign citation refused", err)
	}
	if record.SourceCount != 1 {
		t.Fatalf("the step should still report the citation it got: %+v", record)
	}
	// Item two: the digest is of the bytes as they arrived, not of a tidied copy.
	if record.AnswerSHA256 == nil || *record.AnswerSHA256 != hashText(foreignAnswer) {
		t.Fatalf("answer digest = %v, want the hash of the untrimmed bytes", record.AnswerSHA256)
	}
	if *record.AnswerSHA256 == hashText(strings.TrimSpace(foreignAnswer)) {
		t.Fatal("the digest was taken after trimming, which is a different byte string")
	}
}

// TestHashAnswerJudgesEmptinessByTrimmingButDigestsRawBytes is item two in one place:
// all three steps share this function, so the two rules must not be conflated.
func TestHashAnswerJudgesEmptinessByTrimmingButDigestsRawBytes(t *testing.T) {
	padded := "   带空白的回答\t"
	hashed := hashAnswer(padded)
	if hashed == nil {
		t.Fatal("an answer with content was treated as empty")
	}
	if *hashed != hashText(padded) {
		t.Fatalf("digest = %s, want the hash of %q as given", *hashed, padded)
	}
	for _, blank := range []string{"", "   ", "\t\n "} {
		if hashAnswer(blank) != nil {
			t.Fatalf("whitespace-only answer %q produced a digest instead of null", blank)
		}
	}
}

// TestCompleteStepDigestsTheRawAnswer covers item two at the step that receives a
// whole non-streaming reply.
func TestCompleteStepDigestsTheRawAnswer(t *testing.T) {
	chat := defaultChat()
	chat.reply = "  两侧都有空格的回答  "
	record, usage, err := runComplete(context.Background(), chat)
	if err != nil || usage != nil {
		t.Fatalf("runComplete = %v / %v", record, err)
	}
	if record.AnswerSHA256 == nil || *record.AnswerSHA256 != hashText(chat.reply) {
		t.Fatalf("digest = %v, want the hash of the untrimmed reply", record.AnswerSHA256)
	}
	if !record.NonEmpty {
		t.Fatalf("a padded but real answer was recorded as empty: %+v", record)
	}
}

// silentAnswer answers fluently but cites nothing at all.
type silentAnswer struct{}

func (silentAnswer) Respond(context.Context, responder.Request) (responder.Answer, error) {
	return responder.Answer{Content: "模型自己有话要说，但没有来源。"}, nil
}

func (s silentAnswer) Stream(ctx context.Context, request responder.Request, emit func(string) error) (responder.Answer, error) {
	answer, err := s.Respond(ctx, request)
	if err == nil && emit != nil {
		err = emit(answer.Content)
	}
	return answer, err
}

// TestKnowledgeStepRefusesAnAnswerThatCitesNothing keeps the grounded turn honest: an
// answer with no citation is a failure even though it arrived with text, because a
// knowledge run that cited nothing proved nothing about retrieval.
func TestKnowledgeStepRefusesAnAnswerThatCitesNothing(t *testing.T) {
	ctx := context.Background()
	repo := repository.NewInMemoryRepository()
	store := knowledge.NewMemoryStore()
	userID, err := smokeUser(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	record, err := runKnowledge(ctx, repo, store, silentAnswer{}, resolved{budget: service.ContextBudget{}.Normalize()}, userID)
	if !errors.Is(err, ErrNothingCited) {
		t.Fatalf("error = %v, want an uncited grounded answer to fail", err)
	}
	if record.SourceCount != 0 || record.AnswerSHA256 == nil {
		t.Fatalf("the fluent answer should still be recorded as it stood: %+v", record)
	}
}

// citedBlank answers with an empty body but a citation, which is the shape the step
// must reject: the brief's requirement is a non-empty grounded answer, not merely a
// referenced one.
type citedBlank struct{}

func (citedBlank) Respond(context.Context, responder.Request) (responder.Answer, error) {
	return responder.Answer{Content: "   ", Sources: []domain.Source{{
		SourceID: "src-chunk", DocumentID: "document-theirs", CollectionID: "collection-theirs", ChunkID: "chunk",
	}}}, nil
}

func (s citedBlank) Stream(ctx context.Context, request responder.Request, emit func(string) error) (responder.Answer, error) {
	return s.Respond(ctx, request)
}

func TestKnowledgeStepRefusesABlankAnswer(t *testing.T) {
	ctx := context.Background()
	repo := repository.NewInMemoryRepository()
	store := knowledge.NewMemoryStore()
	userID, err := smokeUser(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	record, err := runKnowledge(ctx, repo, store, citedBlank{}, resolved{budget: service.ContextBudget{}.Normalize()}, userID)
	if !errors.Is(err, model.ErrEmptyCompletion) {
		t.Fatalf("error = %v, want a whitespace-only grounded answer to fail as empty", err)
	}
	if record.AnswerSHA256 != nil || record.NonEmpty {
		t.Fatalf("a blank answer was recorded as an answer: %+v", record)
	}
}

// TestCitationsOwnedChecksBothRulesSeparately exists because the foreign citation
// above breaks both ownership rules at once, which cannot tell them apart. Each
// clause gets its own case.
func TestCitationsOwnedChecksBothRulesSeparately(t *testing.T) {
	const documentID = "document-mine"
	const collectionID = "collection-mine"
	own := domain.Source{
		SourceID:     "src-chunk-mine",
		DocumentID:   documentID,
		CollectionID: collectionID,
		ChunkID:      "chunk-mine",
	}
	if err := citationsOwned([]domain.Source{own}, documentID, collectionID); err != nil {
		t.Fatalf("a citation to this run's own chunk was refused: %v", err)
	}
	otherDocument := own
	otherDocument.DocumentID = "document-theirs"
	if err := citationsOwned([]domain.Source{otherDocument}, documentID, collectionID); !errors.Is(err, ErrForeignCitation) {
		t.Fatalf("a citation to another document = %v, want refused", err)
	}
	otherCollection := own
	otherCollection.CollectionID = "collection-theirs"
	if err := citationsOwned([]domain.Source{otherCollection}, documentID, collectionID); !errors.Is(err, ErrForeignCitation) {
		t.Fatalf("a citation to another collection = %v, want refused", err)
	}
	invented := own
	invented.SourceID = "src-invented"
	if err := citationsOwned([]domain.Source{invented}, documentID, collectionID); !errors.Is(err, ErrForeignCitation) {
		t.Fatalf("a source id that is not derived from its chunk = %v, want refused", err)
	}
	// An empty list is this predicate's business only in the trivial sense: the step
	// that produced it fails on its own "cited nothing" rule.
	if err := citationsOwned(nil, documentID, collectionID); err != nil {
		t.Fatalf("no citations must not fail here, that is ErrNothingCited's job: %v", err)
	}
}

// TestNoEvidenceSkippedIsNotClaimedWithoutProof pins the meaning of the one field
// that reports a negative: no_evidence_skipped_provider may say true only when the
// check itself ran, succeeded and spent nothing.
func TestNoEvidenceSkippedIsNotClaimedWithoutProof(t *testing.T) {
	ctx := context.Background()
	repo := repository.NewInMemoryRepository()
	store := knowledge.NewMemoryStore()
	userID, err := smokeUser(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	// A responder that answers from itself instead of admitting it found nothing
	// must fail the check, not satisfy it.
	record, err := runNoEvidence(ctx, repo, store, foreignCitation{}, resolved{budget: service.ContextBudget{}.Normalize()}, userID)
	if !errors.Is(err, ErrAnsweredWithoutEvidence) {
		t.Fatalf("error = %v, want an answer without evidence to fail the check", err)
	}
	// The stand-in answered fluently and cited something, so the failure is not
	// "nothing came back" - it is that the knowledge path let a model-authored
	// answer through where it must have said it found nothing.
	if !record.NonEmpty || record.SourceCount == 0 {
		t.Fatalf("the check recorded the wrong shape of failure: %+v", record)
	}

	cases := []struct {
		name   string
		status string
		used   int
		want   bool
	}{
		{"failed check", statusFailed, 0, false},
		{"spent a request", statusOK, 1, false},
		{"succeeded with no request", statusOK, 0, true},
	}
	for _, testCase := range cases {
		step := stepRecord{Name: stepNoEvidence, Status: testCase.status, RequestsUsed: testCase.used}
		var summary report
		summary.Steps = []stepRecord{step}
		summary.deriveHeadlineFacts()
		if summary.NoEvidenceSkipped != testCase.want {
			t.Fatalf("%s: no_evidence_skipped_provider = %v, want %v", testCase.name, summary.NoEvidenceSkipped, testCase.want)
		}
	}
}

// TestStreamStepRefusesADoneMarkerInTheBody covers the protocol rule of step two:
// the end marker is not answer text, and a provider that ships it inside a delta is
// reported as broken rather than archived as a normal run.
func TestStreamStepRefusesADoneMarkerInTheBody(t *testing.T) {
	chat := defaultChat()
	chat.chunks = []string{"正文的一半。", "[DONE]", "不该再有一半。"}
	record, err := runStream(context.Background(), chat)
	if !errors.Is(err, ErrDoneMarkerInStream) {
		t.Fatalf("error = %v, want the [DONE] marker refused", err)
	}
	if record.DeltaCount != 1 {
		t.Fatalf("delta count = %d, want only the content before the marker", record.DeltaCount)
	}
	if record.AnswerSHA256 == nil || *record.AnswerSHA256 != hashText("正文的一半。") {
		t.Fatalf("the marker leaked into the recorded answer: %+v", record)
	}
	// Text really did arrive before the marker, so the step function reports what it
	// gathered; the archived record is what must not call it an answer.
	if !record.NonEmpty {
		t.Fatalf("the step should still carry what arrived before the marker: %+v", record)
	}
	archived := archiveStep(t, newMetered(chat, requestCap), stepStream, record, err)
	if archived.AnswerSHA256 != nil || archived.NonEmpty || archived.FinishReason != nil {
		t.Fatalf("a failed step was archived with an answer: %+v", archived)
	}
	if archived.DeltaCount != 1 || archived.FailureCode == nil || *archived.FailureCode != failDoneMarkerInStream {
		t.Fatalf("the archived failure lost the counts it should keep: %+v", archived)
	}
}

// archiveStep runs one raw step record through the same finishing pass execute() uses,
// so a test can look at the shape that would reach the file.
func archiveStep(t *testing.T, meter *metered, name string, step stepRecord, err error) stepRecord {
	t.Helper()
	var (
		recorded report
		failures []error
	)
	finishStep(&recorded, &failures, meter, name, name, step, err)
	if len(recorded.Steps) != 1 {
		t.Fatalf("finishing produced %d records", len(recorded.Steps))
	}
	return recorded.Steps[0]
}

// TestAFailedStepNeverKeepsAnAnswerDigest is item one: bytes that arrived before an
// upstream failure stay as counts, and never as a digest or a claim of non-emptiness.
func TestAFailedStepNeverKeepsAnAnswerDigest(t *testing.T) {
	hash := hashText("半截回答")
	reason := "stop"
	partial := stepRecord{
		NonEmpty:     true,
		AnswerSHA256: &hash,
		FinishReason: &reason,
		DeltaCount:   3,
		SourceCount:  2,
	}
	recorded := archiveStep(t, newMetered(defaultChat(), requestCap), stepStream, partial, model.ErrTimeout)
	if recorded.AnswerSHA256 != nil || recorded.NonEmpty || recorded.FinishReason != nil {
		t.Fatalf("a failed step kept an answer digest or a success flag: %+v", recorded)
	}
	if recorded.Status != statusFailed || recorded.DeltaCount != 3 || recorded.SourceCount != 2 {
		t.Fatalf("the counts that were true are gone: %+v", recorded)
	}
	if recorded.FailureCode == nil || *recorded.FailureCode != failProviderTimeout {
		t.Fatalf("failure code = %v, want %s", recorded.FailureCode, failProviderTimeout)
	}

	// A step that succeeded keeps everything.
	succeeded := archiveStep(t, newMetered(defaultChat(), requestCap), stepStream, partial, nil)
	if succeeded.Status != statusOK || succeeded.AnswerSHA256 == nil || !succeeded.NonEmpty ||
		succeeded.FailureCode != nil || succeeded.FinishReason == nil {
		t.Fatalf("a successful step was altered by the finishing pass: %+v", succeeded)
	}
}

// TestRunArchivesAStreamThatBrokeAfterContent is item one on a real socket: the
// endpoint sends one delta and then fails, so the record must show the delta it got
// and no answer at all.
func TestRunArchivesAStreamThatBrokeAfterContent(t *testing.T) {
	server, hits := providerEndpoint(t, streamBreaksMidway)
	recorder := newRecorder()
	code, err := run(authorizedArgs(smokeOutput), lookupOf(liveEnvironment(server.URL)), recorder.live())
	if code != 1 || err == nil {
		t.Fatalf("run = code %d err %v, want a non-zero exit", code, err)
	}
	if got := hits.Load(); got != requestCap {
		t.Fatalf("the endpoint was dialed %d times, want %d", got, requestCap)
	}
	recorded := decodeReport(t, recorder)
	stream := stepOf(t, recorded, stepStream)
	if stream.Status != statusFailed || stream.DeltaCount != 1 || stream.RequestsUsed != 1 {
		t.Fatalf("the partial stream is not recorded as it happened: %+v", stream)
	}
	if stream.AnswerSHA256 != nil || stream.NonEmpty {
		t.Fatalf("a broken stream recorded an answer: %+v", stream)
	}
	// The transport answered 200 and the error arrived inside the stream, so the
	// recorded status says exactly that rather than inventing a 5xx.
	if stream.FailureCode == nil || *stream.FailureCode != failProviderStatus ||
		stream.ProviderStatus == nil || *stream.ProviderStatus != http.StatusOK {
		t.Fatalf("broken-stream failure = %v / %v, want provider_status/200", stream.FailureCode, stream.ProviderStatus)
	}
	if spoken := err.Error(); !strings.Contains(spoken, "stream="+failProviderStatus+"/200") {
		t.Fatalf("failure summary = %q", spoken)
	}
	if recorded.StreamNonEmpty || recorded.StreamDeltaCount != 1 {
		t.Fatalf("the headline facts disagree with the step: %+v", recorded)
	}
	// The other two steps really did answer, so their digests stay.
	for _, name := range []string{stepComplete, stepKnowledge} {
		if step := stepOf(t, recorded, name); step.Status != statusOK || step.AnswerSHA256 == nil {
			t.Fatalf("step %q = %+v, want a completed answer beside the broken stream", name, step)
		}
	}
	if recorded.ActualRequests != requestCap || recorded.Failures != 1 {
		t.Fatalf("counts = %d requests / %d failures, want %d / 1",
			recorded.ActualRequests, recorded.Failures, requestCap)
	}
}

// The provider-answer texts are what the stand-in endpoint says for each step. The
// three direct answers differ on purpose so their recorded digests can be told apart,
// and none of them may appear in the report. completeAnswer carries surrounding
// whitespace because the digest must cover the bytes as returned.
const (
	completeAnswer      = "  provider-complete-answer  "
	streamAnswerPartOne = "provider-stream-answer-one "
	streamAnswerPartTwo = "provider-stream-answer-two"
	knowledgeAnswer     = "provider-knowledge-answer"
	// foreignAnswer is what the stand-in grounded responder says: padded, so a
	// trimmed digest would be observably different from the bytes it was given.
	foreignAnswer = "  这句话看起来有据可依。  "
	// providerFailureText is planted in the error body to prove a provider's own
	// words never reach the record, nor the operator's terminal.
	providerFailureText = "provider-side-text-that-must-not-be-archived"
)

// streamBehaviour is how the stand-in endpoint treats the streaming request. The
// first two steps and the grounded step always succeed unless named here.
type streamBehaviour int

const (
	streamSucceeds streamBehaviour = iota
	// streamRefused answers with a non-2xx status before any content.
	streamRefused
	// streamBreaksMidway sends one content delta and then an error frame, which is
	// the case where bytes really did arrive before the upstream failed.
	streamBreaksMidway
)

// providerEndpoint is a stand-in OpenAI-compatible server for the smoke's three
// shapes, so a test can put a real client on a real socket between the smoke and an
// answer without ever reaching a provider.
func providerEndpoint(t *testing.T, behaviour streamBehaviour) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		raw, readErr := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if readErr != nil {
			t.Errorf("the request body could not be read: %v", readErr)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var envelope struct {
			Stream bool `json:"stream"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil {
			t.Errorf("the client sent a body that is not JSON: %v", err)
		}
		switch {
		case envelope.Stream && behaviour == streamRefused:
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = fmt.Fprintf(w, `{"error":{"code":"%s","type":"%s","message":"%s"}}`,
				providerFailureText, providerFailureText, providerFailureText)
		case envelope.Stream && behaviour == streamBreaksMidway:
			w.Header().Set("Content-Type", "text/event-stream")
			writeSSE(t, w, `{"choices":[{"delta":{"content":%q}}]}`, streamAnswerPartOne)
			writeSSE(t, w, `{"error":{"code":%q,"type":"server_error"}}`, providerFailureText)
		case envelope.Stream:
			w.Header().Set("Content-Type", "text/event-stream")
			writeSSE(t, w, `{"choices":[{"delta":{"content":%q}}]}`, streamAnswerPartOne)
			writeSSE(t, w, `{"choices":[{"delta":{"content":%q},"finish_reason":"stop"}]}`, streamAnswerPartTwo)
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		case strings.Contains(string(raw), "fsync"):
			writeCompletionJSON(t, w, knowledgeAnswer)
		default:
			writeCompletionJSON(t, w, completeAnswer)
		}
	}))
	t.Cleanup(server.Close)
	return server, &hits
}

func writeSSE(t *testing.T, w io.Writer, shape, content string) {
	t.Helper()
	if _, err := fmt.Fprintf(w, "data: "+shape+"\n\n", content); err != nil {
		t.Errorf("the stream could not be written: %v", err)
	}
}

func writeCompletionJSON(t *testing.T, w http.ResponseWriter, content string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	body := fmt.Sprintf(`{"model":"smoke-model","choices":[{"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],`+
		`"usage":{"prompt_tokens":21,"completion_tokens":7,"total_tokens":28}}`, content)
	if _, err := io.WriteString(w, body); err != nil {
		t.Errorf("the completion could not be written: %v", err)
	}
}

func liveEnvironment(baseURL string) map[string]string {
	return environment(map[string]string{openai.EnvBaseURL: baseURL})
}

func decodeReport(t *testing.T, recorder *recorder) report {
	t.Helper()
	raw, exists := recorder.written[smokeOutput]
	if !exists {
		t.Fatalf("no report was written: %v", recorder.written)
	}
	var recorded report
	if err := json.Unmarshal(raw, &recorded); err != nil {
		t.Fatalf("the report is not the shape this tool writes: %v\n%s", err, raw)
	}
	return recorded
}

func encodeReport(t *testing.T, recorded report) []byte {
	t.Helper()
	encoded, err := recorded.encode()
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func stepOf(t *testing.T, recorded report, name string) stepRecord {
	t.Helper()
	for _, step := range recorded.Steps {
		if step.Name == name {
			return step
		}
	}
	t.Fatalf("the report has no %q step: %+v", name, recorded.Steps)
	return stepRecord{}
}

func mustHash(t *testing.T, step stepRecord) string {
	t.Helper()
	if step.AnswerSHA256 == nil {
		t.Fatalf("step %q recorded a null answer hash", step.Name)
	}
	return *step.AnswerSHA256
}
