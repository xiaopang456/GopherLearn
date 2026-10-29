package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/agent"
	"github.com/xiaopang456/GopherLearn/internal/app"
	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/knowledge"
	"github.com/xiaopang456/GopherLearn/internal/model"
	"github.com/xiaopang456/GopherLearn/internal/openai"
	"github.com/xiaopang456/GopherLearn/internal/repository"
	"github.com/xiaopang456/GopherLearn/internal/service"
)

// The workload is one question that cannot be answered without the tool, about one
// document that exists only in this process. Two tokens do different jobs. The marker is
// what both documents share, a latin string BM25 admits on a shared term; the facts are
// what only one of them carries, so "the model answered correctly" can be decided without
// reading the answer. The caller's document holds cycle-90d and the other user's holds
// cycle-7d, which turns a search that escaped its scope into a fact a record can state.
type workload struct {
	marker            string
	question          string
	expectedFact      string
	foreignFact       string
	ownDocument       string
	foreignDocument   string
	documentTitle     string
	foreignTitle      string
	collectionTitle   string
	foreignCollection string
}

func defaultWorkload() workload {
	marker := "sprocket-9q7x"
	return workload{
		marker:   marker,
		question: "知识库里带 " + marker + " 标记的齿轮箱，检修周期判别值是什么？只回答那个判别值。",
		// The facts are ASCII and neither contains the other, so a substring test
		// decides attribution without a lexer.
		expectedFact:      "cycle-90d",
		foreignFact:       "cycle-7d",
		ownDocument:       "# 检修周期\n\n带 " + marker + " 标记的齿轮箱，检修周期判别值是 cycle-90d。\n",
		foreignDocument:   "# 检修周期\n\n带 " + marker + " 标记的齿轮箱，检修周期判别值是 cycle-7d。\n",
		documentTitle:     "gearbox.md",
		foreignTitle:      "gearbox-other.md",
		collectionTitle:   "冒烟手册",
		foreignCollection: "别人的手册",
	}
}

const (
	// turnRequests is the fixed provider budget: the decision and the answer.
	turnRequests = 2
	// turnTools is how many tools that decision may run: one.
	turnTools = 1
	// turnDeadline bounds the whole turn so a stalled vendor cannot hold the process
	// open past the two requests it was allowed.
	turnDeadline = 90 * time.Second
)

// Failure codes are a closed set. Nothing from a provider response is ever written into
// the record or printed, because an error body can quote the prompt back. The reason codes
// below describe which rule *this server* applied, which is a fact about our own parser
// rather than vendor text, so naming them costs nothing in privacy and is the whole
// difference between "the run failed" and a diagnosis.
const (
	codeNoProviderAttempt       = "no_provider_attempt"
	codeAnsweredWithoutToolCall = "answered_without_tool_call"
	codeJSONInProseNotToolCall  = "json_in_prose_not_tool_call"
	codeUnknownToolName         = "unknown_tool_name"
	codeMissingToolCallID       = "missing_tool_call_id"
	codeToolCallIDMismatch      = "tool_call_id_mismatch"
	codeNoToolResultMessage     = "no_tool_result_message"

	// A tool call that ended the turn without an answer turn. v1 called this
	// "not fulfilled by budget" whenever the loop stopped, which blamed the budget for
	// every refusal; the generic code now says only what is known, and the reason codes
	// below say more when the error chain supports it.
	codeToolCallNotFulfilled = "tool_call_not_fulfilled"

	// Argument and scope refusals, one per rule.
	codeArgumentsNotObject           = "arguments_not_object"
	codeArgumentsMalformed           = "arguments_malformed_json"
	codeArgumentsTooLarge            = "arguments_too_large"
	codeArgumentsUnknownField        = "arguments_unknown_field"
	codeArgumentsQueryMissingOrBlank = "arguments_query_missing_or_blank"
	codeArgumentsQueryTooLong        = "arguments_query_too_long"
	codeForeignCollectionInScope     = "foreign_collection_in_scope"
	codeUnknownTool                  = "unknown_tool"
	codeToolExecutionFailed          = "tool_execution_failed"
	codeModelCallBudget              = "model_call_budget"
	codeToolCallBudget               = "tool_call_budget"
	codeStepBudget                   = "step_budget"
	codeDeadline                     = "deadline"

	codeToolReturnedNoEvidence = "tool_returned_no_evidence"

	// The attribution codes say which fact went where. They are the difference between
	// "the protocol worked" and "this answer came from the document it should have".
	codeToolResultMissingExpectedFact = "tool_result_missing_expected_fact"
	codeToolResultCarriesForeignFact  = "tool_result_carries_foreign_fact"
	codeAnswerMissingExpectedFact     = "answer_missing_expected_fact"

	codeNoCitations           = "no_citations"
	codeForeignCitation       = "citation_outside_temp_document"
	codeEmptyAnswer           = "empty_answer"
	codeSecondRequestMissing  = "second_request_missing"
	codeTooManyRequests       = "more_requests_than_allowed"
	codeProviderStatus        = "provider_status"
	codeProviderTimeout       = "provider_timeout"
	codeProviderUnavailable   = "provider_unavailable"
	codeProviderNotConfigured = "provider_not_configured"
	codeMalformedResponse     = "malformed_response"
	codeEmptyCompletion       = "empty_completion"
	codeInvalidRequest        = "invalid_request"
	codeRequestCapExceeded    = "request_cap_exceeded"
	codeToolsUnsupported      = "tools_unsupported"
	codeCanceled              = "canceled"
	codeWiringFailed          = "wiring_failed"
	codeTurnFailed            = "turn_failed"
	codeUnclassified          = "unclassified_failure"
)

// usageRecord is what the provider reported, or null when it reported nothing.
type usageRecord struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// callShape is what the first tool request looked like. Every field is a boolean or a
// count: which keys were used, how long the query was, how many collections were named.
// No key name, no query text and no collection id is recorded, because the payload is
// model-authored and a sealed artifact must not become a transcript.
type callShape struct {
	ToolCallsInFirstResponse int  `json:"tool_calls_in_first_response"`
	ArgumentsIsObject        bool `json:"arguments_is_single_json_object"`
	ArgumentsBytes           int  `json:"arguments_bytes"`

	QueryPresent         bool `json:"query_present"`
	QueryIsString        bool `json:"query_is_string"`
	QueryNonBlank        bool `json:"query_nonblank"`
	QueryRunes           *int `json:"query_runes"`
	CollectionIDsPresent bool `json:"collection_ids_present"`
	// CollectionIDsCount is null when the key was absent rather than reporting zero, so
	// "the model did not name any" and "there was nothing to count" stay distinct.
	CollectionIDsCount   *int `json:"collection_ids_count"`
	UndeclaredFieldCount *int `json:"undeclared_field_count"`
}

// report is the whole record. Every field states structure, a count or a hash: no
// question, no answer, no tool observation, no header, no key, no full endpoint URL.
type report struct {
	SchemaVersion int    `json:"schema_version"`
	ExecutedAt    string `json:"executed_at"`
	Provider      string `json:"provider"`
	EndpointHost  string `json:"endpoint_host"`
	Model         string `json:"model"`

	RequestCap     int `json:"request_cap"`
	ActualRequests int `json:"actual_requests"`
	Retries        int `json:"retries"`
	Failures       int `json:"failures"`

	HarnessMaxModelCalls int `json:"harness_max_model_calls"`
	HarnessMaxToolCalls  int `json:"harness_max_tool_calls"`
	HarnessMaxSteps      int `json:"harness_max_steps"`
	ProviderStatus       int `json:"provider_status"`

	// Three separate facts that v1 collapsed into one. The tool can be entered and still
	// refuse, a tool can produce a result that never reaches another request, and a result
	// can reach a later request without reaching the second one.
	ToolsExecuted                  int  `json:"tools_executed"`
	ToolResultsProduced            bool `json:"tool_results_produced"`
	ToolResultReachedSecondRequest bool `json:"tool_result_reached_second_request"`

	FirstResponseWasToolCall bool   `json:"first_response_was_tool_call"`
	ToolNameObserved         string `json:"tool_name_observed"`
	ToolCallIDPresent        bool   `json:"tool_call_id_present"`
	ToolCallIDConsistent     bool   `json:"tool_call_id_consistent"`
	ToolReturnedEvidence     bool   `json:"tool_returned_evidence"`

	// Attribution of the facts: the caller's discriminator must arrive through the tool,
	// the other account's must not arrive at all, and the answer must repeat what arrived.
	ToolResultHasExpectedFact bool `json:"tool_result_contains_expected_fact"`
	ToolResultHasForeignFact  bool `json:"tool_result_contains_foreign_fact"`
	AnswerHasExpectedFact     bool `json:"answer_contains_expected_fact"`

	FirstCall               *callShape   `json:"first_tool_call"`
	SourceCount             int          `json:"source_count"`
	CitationsOwnedByTempDoc bool         `json:"citations_owned_by_temp_document"`
	AnswerNonEmpty          bool         `json:"answer_nonempty"`
	AnswerSHA256            *string      `json:"answer_sha256"`
	UsageIfReturned         *usageRecord `json:"usage_if_returned"`
	FailureCodes            []string     `json:"failure_codes"`

	APIKeyRecorded bool `json:"api_key_recorded"`
	RealMySQL      bool `json:"real_mysql"`
	Redis          bool `json:"redis"`
	RabbitMQ       bool `json:"rabbitmq"`
	Docker         bool `json:"docker"`

	// output is where the record goes; it is bookkeeping for the summary line, never
	// serialized.
	output string
}

func (r report) encode() ([]byte, error) {
	encoded, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}

// failure is the only error this command prints. It names closed-set codes, the numeric
// provider status and the record path, and nothing else; errors.Is still reaches the real
// cause so a test can name what broke without printing it.
type failure struct {
	message string
	cause   error
}

func (f *failure) Error() string { return f.message }

func (f *failure) Unwrap() error { return f.cause }

// summarize turns a cause into the safe one-line report of a failed run.
func summarize(record report, cause error) error {
	if cause == nil {
		return nil
	}
	codes := record.FailureCodes
	if len(codes) == 0 {
		codes = []string{codeUnclassified}
	}
	status := ""
	if record.ProviderStatus != 0 {
		status = " provider_status=" + strconv.Itoa(record.ProviderStatus)
	}
	return &failure{
		message: fmt.Sprintf("execution failed: requests=%d/%d codes=%s%s output=%s",
			record.ActualRequests, requestCap, strings.Join(codes, ","), status, record.output),
		cause: cause,
	}
}

func joinErrors(left, right error) error { return errors.Join(left, right) }

// evidence is everything the checks read. Passing one struct instead of seven positional
// arguments keeps a new check from being able to borrow the wrong one of them.
type evidence struct {
	meter        *metered
	tool         *countedTool
	cause        error
	pair         service.MessagePair
	collectionID string
	documentID   string
	work         workload
}

// execute spends at most two provider requests and then verifies the protocol, whatever
// happened on the way. A run that failed still writes its record before exiting non-zero.
func execute(next plan, deps dependencies) (report, error) {
	record := report{
		SchemaVersion: schemaVersion,
		ExecutedAt:    deps.now().UTC().Format(timeLayout),
		Provider:      string(app.ProviderOpenAI),
		EndpointHost:  hostOf(next.model.BaseURL),
		Model:         next.model.Model,
		RequestCap:    requestCap,
		Retries:       0,
		output:        next.output,
	}
	budget := agent.Budget{
		MaxModelCalls: turnRequests,
		MaxToolCalls:  turnTools,
		MaxSteps:      turnRequests,
		Deadline:      turnDeadline,
	}.Normalize()
	record.HarnessMaxModelCalls = budget.MaxModelCalls
	record.HarnessMaxToolCalls = budget.MaxToolCalls
	record.HarnessMaxSteps = budget.MaxSteps

	ctx := context.Background()
	repo := repository.NewInMemoryRepository()
	store := knowledge.NewMemoryStore()
	ingest := service.NewKnowledgeService(store)
	work := next.work

	principal, foreign, err := seedUsers(ctx, repo)
	if err != nil {
		return finish(record, fmt.Errorf("agent smoke setup: %w", err))
	}
	tempCollection, tempDocument, err := seedKnowledge(ctx, ingest, principal, work.collectionTitle, work.documentTitle, work.ownDocument)
	if err != nil {
		return finish(record, fmt.Errorf("agent smoke setup: %w", err))
	}
	// A second user holding the same marker in their own collection, and their own
	// discriminator. If the search ever escaped the caller's scope, this is how the record
	// would show it: a fact that only exists in the other account appears in the tool
	// result the caller was handed. Nobody has to read an answer to find out.
	if _, _, err := seedKnowledge(ctx, ingest, foreign, work.foreignCollection, work.foreignTitle, work.foreignDocument); err != nil {
		return finish(record, fmt.Errorf("agent smoke setup: %w", err))
	}

	inner, err := deps.newModel(next.model)
	if err != nil {
		record.FailureCodes = []string{codeProviderNotConfigured}
		return finish(record, err)
	}
	meter := newMetered(inner, requestCap)
	counter := &countedTool{}
	config := app.Config{
		Provider: app.ProviderOpenAI,
		Model:    next.model,
		Context:  service.ContextBudget{}.Normalize(),
		Agent:    budget,
	}
	assistant, _, err := app.New(store, config, app.Options{
		NewModel:     func(openai.Config) (model.ChatModel, error) { return meter, nil },
		DecorateTool: counter.wrap,
	})
	if err != nil {
		record.FailureCodes = []string{codeWiringFailed}
		return finish(record, fmt.Errorf("agent smoke wiring: %w", err))
	}
	session, err := repo.CreateSession(ctx, &domain.Session{
		UserID: principal, Title: "工具冒烟", Mode: domain.SessionModeLearningAgent,
	})
	if err != nil {
		return finish(record, fmt.Errorf("agent smoke setup: %w", err))
	}
	// The mirror under the message service, not instead of it: the service still decides
	// what an HTTP caller sees, and this only keeps the chain it collapsed away.
	mirror := &recordedResponder{inner: assistant}
	messages := app.NewMessageService(repo, mirror, ingest, config.Context)

	pair, turnErr := messages.Send(ctx, principal, session.ID, work.question, domain.KnowledgeOptions{})
	record.ActualRequests = meter.requests
	record.Failures = meter.failures
	verify(evidence{
		meter:        meter,
		tool:         counter,
		cause:        mirror.chain(),
		pair:         pair,
		collectionID: tempCollection,
		documentID:   tempDocument,
		work:         work,
	}, &record)
	if failing, ok := meter.firstFailure(); ok {
		if status, has := openai.ProviderStatusOf(failing.err); has {
			record.ProviderStatus = status
		}
	}

	cause := turnErr
	if cause == nil && len(record.FailureCodes) > 0 {
		// The turn produced an answer and still failed the protocol check, which is the
		// case this smoke exists to catch.
		cause = errors.New("agent tool smoke checks failed")
	}
	return finish(record, cause)
}

// finish pins the flags that describe this code path rather than the run, and pairs the
// record with its printable summary.
func finish(record report, cause error) (report, error) {
	record.APIKeyRecorded = false
	record.RealMySQL = false
	record.Redis = false
	record.RabbitMQ = false
	record.Docker = false
	if record.FailureCodes == nil {
		record.FailureCodes = []string{}
	}
	return record, summarize(record, cause)
}

// verify turns the transcript into structural claims: the protocol shape first, then
// attribution - which fact reached the model through the tool, and which answer repeats
// it. A check that could not be made is recorded as a code rather than left absent,
// because absence is what a silent fallback looks like from the outside.
func verify(found evidence, record *report) {
	codes := map[string]bool{}
	add := func(code string) { codes[code] = true }
	meter := found.meter

	calls := meter.firstToolCallRound()
	call, wasToolCall := meter.firstToolCall()
	record.FirstResponseWasToolCall = wasToolCall
	record.FirstCall = shapeOf(calls, meter.firstRoundTools())
	if len(meter.attempts) == 0 {
		add(codeNoProviderAttempt)
	}
	if !wasToolCall {
		switch {
		case len(meter.attempts) == 0:
		case proseLooksLikeToolCall(meter.attempts[0].completion.Content):
			add(codeJSONInProseNotToolCall)
		default:
			add(codeAnsweredWithoutToolCall)
		}
	} else {
		record.ToolNameObserved = call.Name
		if call.Name != agent.KnowledgeSearchName {
			add(codeUnknownToolName)
		}
		record.ToolCallIDPresent = strings.TrimSpace(call.ID) != ""
		if !record.ToolCallIDPresent {
			add(codeMissingToolCallID)
		}
	}

	record.ToolsExecuted = found.tool.invocations
	record.ToolResultsProduced = found.tool.productions > 0
	result, reached := meter.toolResultInRequest(1)
	record.ToolResultReachedSecondRequest = reached
	if !reached {
		// The answer turn was asked to speak without ever being handed the tool's result.
		add(codeNoToolResultMessage)
	} else {
		record.ToolCallIDConsistent = record.ToolCallIDPresent && result.ToolCallID == call.ID
		if !record.ToolCallIDConsistent {
			add(codeToolCallIDMismatch)
		}
		record.ToolReturnedEvidence = observationCarriesEvidence(result.Content)
		if !record.ToolReturnedEvidence {
			add(codeToolReturnedNoEvidence)
		}
		// Attribution by fact, not by prose: the caller's discriminator must reach the
		// model through the tool, and the other account's must not arrive at all.
		record.ToolResultHasExpectedFact = strings.Contains(result.Content, found.work.expectedFact)
		if !record.ToolResultHasExpectedFact {
			add(codeToolResultMissingExpectedFact)
		}
		record.ToolResultHasForeignFact = strings.Contains(result.Content, found.work.foreignFact)
		if record.ToolResultHasForeignFact {
			add(codeToolResultCarriesForeignFact)
		}
	}

	if record.ToolResultsProduced && len(meter.attempts) < 2 {
		add(codeSecondRequestMissing)
	}
	// A round that asked for tools and never got its result back is a call this run did not
	// fulfil. Whether the ceiling or a refusal caused it is decided only by the error chain:
	// v1 blamed the budget whenever the loop stopped, which turned every parser refusal
	// into a story about limits.
	if meter.toolCallRounds() > meter.toolResultRounds() {
		add(codeToolCallNotFulfilled)
	}
	for _, reason := range reasonCodes(found.cause) {
		add(reason)
	}
	if meter.requests > requestCap {
		add(codeTooManyRequests)
	}

	if answer := strings.TrimSpace(found.pair.Assistant.Content); answer == "" {
		add(codeEmptyAnswer)
	} else {
		// TrimSpace decides whether there is an answer; the digest covers the bytes as
		// they were stored. Hashing the trimmed text would report the same digest for two
		// messages that differ only in padding, and a sealed record is a claim about the
		// bytes, not about how they read.
		record.AnswerNonEmpty = true
		hash := hashText(found.pair.Assistant.Content)
		record.AnswerSHA256 = &hash
		record.AnswerHasExpectedFact = strings.Contains(found.pair.Assistant.Content, found.work.expectedFact)
		if !record.AnswerHasExpectedFact {
			add(codeAnswerMissingExpectedFact)
		}
	}
	record.SourceCount = len(found.pair.Assistant.Sources)
	if record.SourceCount == 0 {
		add(codeNoCitations)
	} else {
		owned := true
		for _, source := range found.pair.Assistant.Sources {
			if source.CollectionID != found.collectionID || source.DocumentID != found.documentID ||
				source.SourceID != "src-"+source.ChunkID {
				owned = false
			}
		}
		record.CitationsOwnedByTempDoc = owned
		if !owned {
			add(codeForeignCitation)
		}
	}
	if failing, ok := meter.firstFailure(); ok {
		add(providerCode(failing.err))
	}
	record.UsageIfReturned = usageOf(meter)

	names := make([]string, 0, len(codes))
	for code := range codes {
		names = append(names, code)
	}
	sort.Strings(names)
	record.FailureCodes = names
}

// reasonCodes names, from the error chain the mirror kept, which of this server's own rules
// ended the turn. The chain arrives wrapped several times over - the registry wraps a tool's
// own refusal in ErrToolExecution - so the most specific question is asked first, and only
// one argument/scope reason is reported per event: a record full of codes that all describe
// the same refusal reads like several failures.
func reasonCodes(cause error) []string {
	if cause == nil {
		return nil
	}
	var codes []string
	switch {
	case errors.Is(cause, agent.ErrArgumentsNotObject):
		codes = append(codes, codeArgumentsNotObject)
	case errors.Is(cause, agent.ErrArgumentsMalformed):
		codes = append(codes, codeArgumentsMalformed)
	case errors.Is(cause, agent.ErrArgumentsTooLarge):
		codes = append(codes, codeArgumentsTooLarge)
	case errors.Is(cause, agent.ErrUnknownArgumentField):
		codes = append(codes, codeArgumentsUnknownField)
	case errors.Is(cause, agent.ErrQueryMissingOrBlank):
		codes = append(codes, codeArgumentsQueryMissingOrBlank)
	case errors.Is(cause, agent.ErrQueryTooLong):
		codes = append(codes, codeArgumentsQueryTooLong)
	case errors.Is(cause, agent.ErrForeignCollection):
		codes = append(codes, codeForeignCollectionInScope)
	case errors.Is(cause, agent.ErrUnknownTool):
		codes = append(codes, codeUnknownTool)
	case errors.Is(cause, agent.ErrToolExecution):
		// The tool was entered and failed without naming a rule of ours. This is the
		// honest floor: saying more than "execution failed" would be a guess.
		codes = append(codes, codeToolExecutionFailed)
	}
	switch {
	case errors.Is(cause, agent.ErrToolCallBudget):
		codes = append(codes, codeToolCallBudget)
	case errors.Is(cause, agent.ErrModelCallBudget):
		codes = append(codes, codeModelCallBudget)
	case errors.Is(cause, agent.ErrStepBudget):
		codes = append(codes, codeStepBudget)
	case errors.Is(cause, agent.ErrDeadline):
		codes = append(codes, codeDeadline)
	}
	return codes
}

// shapeOf reads the arguments payload the way a schema does: which keys, what type, how
// long. It never returns text the model wrote.
func shapeOf(calls []model.ToolCall, offered []model.ToolDefinition) *callShape {
	if len(calls) == 0 {
		return nil
	}
	shape := &callShape{ToolCallsInFirstResponse: len(calls)}
	raw := calls[0].Arguments
	shape.ArgumentsBytes = len(raw)

	var envelope map[string]json.RawMessage
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &envelope); err != nil {
		// Not an object, or not JSON at all. The counts stay nil rather than zero: nothing
		// here was readable enough to count.
		return shape
	}
	shape.ArgumentsIsObject = true

	declared := declaredProperties(offered, calls[0].Name)
	undeclared := 0
	for key := range envelope {
		if !declared[key] {
			undeclared++
		}
	}
	if declared != nil {
		shape.UndeclaredFieldCount = &undeclared
	}
	value, present := envelope["query"]
	shape.QueryPresent = present
	if present {
		var text string
		if err := json.Unmarshal(value, &text); err == nil {
			shape.QueryIsString = true
			count := len([]rune(strings.TrimSpace(text)))
			shape.QueryNonBlank = count > 0
			shape.QueryRunes = &count
		}
	}
	named, namedPresent := envelope["collection_ids"]
	shape.CollectionIDsPresent = namedPresent
	if namedPresent {
		var ids []json.RawMessage
		if err := json.Unmarshal(named, &ids); err == nil {
			count := len(ids)
			shape.CollectionIDsCount = &count
		}
	}
	return shape
}

// declaredProperties reads the property names this server offered for one tool, so
// "undeclared" means "not in the schema we ourselves published" rather than a list kept in
// two places. A nil answer means the schema was not offered, and the count is then left
// null instead of guessed.
func declaredProperties(offered []model.ToolDefinition, name string) map[string]bool {
	for _, tool := range offered {
		if tool.Name != name {
			continue
		}
		var schema struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(tool.Parameters, &schema); err != nil || schema.Properties == nil {
			return nil
		}
		declared := make(map[string]bool, len(schema.Properties))
		for property := range schema.Properties {
			declared[property] = true
		}
		return declared
	}
	return nil
}

// usageOf totals what the provider reported across the requests this turn made. One row
// for the whole turn is the useful number when a turn is two requests, and a vendor that
// reports nothing at all keeps the field null rather than a row of zeros that reads as a
// measurement.
func usageOf(meter *metered) *usageRecord {
	var total usageRecord
	reported := false
	for _, entry := range meter.attempts {
		if entry.err != nil {
			continue
		}
		usage := entry.completion.Usage
		if usage.PromptTokens == 0 && usage.CompletionTokens == 0 && usage.TotalTokens == 0 {
			continue
		}
		reported = true
		total.PromptTokens += usage.PromptTokens
		total.CompletionTokens += usage.CompletionTokens
		total.TotalTokens += usage.TotalTokens
	}
	if !reported {
		return nil
	}
	return &total
}

// proseLooksLikeToolCall names the failure mode the seam forbids: a model that writes a
// tool request as JSON in its answer text instead of using the structured field. It is
// only ever used to label the failure, never to act on it.
func proseLooksLikeToolCall(content string) bool {
	lowered := strings.ToLower(content)
	return strings.Contains(lowered, "tool_call") ||
		(strings.Contains(lowered, `"name"`) && strings.Contains(lowered, agent.KnowledgeSearchName))
}

// observationCarriesEvidence checks the tool result the model was handed without
// repeating it. The shape is the signal: the data label, and the first numbered passage
// actually present. Wording is not a signal here - a search that found material in only
// part of a user's knowledge base carries a scope note containing the words "没有找到",
// and treating that phrase as "nothing found" would report real evidence as none.
func observationCarriesEvidence(content string) bool {
	return strings.Contains(content, agent.ObservationLabel) && strings.Contains(content, "\n[1] ")
}

func providerCode(err error) string {
	switch {
	case err == nil:
		return codeUnclassified
	case errors.Is(err, ErrRequestCapExceeded):
		return codeRequestCapExceeded
	case errors.Is(err, model.ErrToolsUnsupported):
		return codeToolsUnsupported
	case errors.Is(err, model.ErrNotConfigured):
		return codeProviderNotConfigured
	case errors.Is(err, model.ErrInvalidRequest):
		return codeInvalidRequest
	case errors.Is(err, model.ErrTimeout):
		return codeProviderTimeout
	case errors.Is(err, model.ErrProviderStatus):
		return codeProviderStatus
	case errors.Is(err, model.ErrMalformedResponse):
		return codeMalformedResponse
	case errors.Is(err, model.ErrEmptyCompletion):
		return codeEmptyCompletion
	case errors.Is(err, model.ErrUnavailable):
		return codeProviderUnavailable
	case errors.Is(err, context.Canceled):
		return codeCanceled
	default:
		return codeUnclassified
	}
}

func seedUsers(ctx context.Context, repo repository.Repository) (string, string, error) {
	principal, err := repo.CreateUser(ctx, &domain.User{
		Email: "agent-smoke@localhost.invalid", DisplayName: "agent smoke",
		PasswordHash: "not-a-credential-this-run-never-authenticates",
	})
	if err != nil {
		return "", "", err
	}
	foreign, err := repo.CreateUser(ctx, &domain.User{
		Email: "other-smoke@localhost.invalid", DisplayName: "other smoke",
		PasswordHash: "not-a-credential-this-run-never-authenticates",
	})
	if err != nil {
		return "", "", err
	}
	return principal.ID, foreign.ID, nil
}

func seedKnowledge(ctx context.Context, ingest *service.KnowledgeService, userID, collectionTitle, documentTitle, content string) (string, string, error) {
	collection, err := ingest.CreateCollection(ctx, userID, collectionTitle)
	if err != nil {
		return "", "", err
	}
	document, err := ingest.AddDocument(ctx, userID, collection.ID, service.DocumentInput{
		Title: documentTitle, Format: string(domain.DocumentFormatMarkdown), Content: content,
	})
	if err != nil {
		return "", "", err
	}
	return collection.ID, document.ID, nil
}

// timeLayout is RFC3339 with a mandatory offset, so a later reader can line the run up
// against provider-side logs.
const timeLayout = "2006-01-02T15:04:05Z07:00"

func hashText(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

func hostOf(baseURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Host == "" {
		return "unavailable"
	}
	return parsed.Host
}
