package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/xiaopang456/GopherLearn/internal/app"
	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/knowledge"
	"github.com/xiaopang456/GopherLearn/internal/model"
	"github.com/xiaopang456/GopherLearn/internal/openai"
	"github.com/xiaopang456/GopherLearn/internal/repository"
	"github.com/xiaopang456/GopherLearn/internal/responder"
	"github.com/xiaopang456/GopherLearn/internal/service"
)

// The four prompts below are the whole workload. They are short on purpose: this
// run exists to prove connectivity and one retrieval-to-answer path, not answer
// quality, and every extra token is paid for.
const (
	smokeSystemPrompt = "你是 GopherLearn 的连通性检查助手。请用一句话回答，不要反问，不要解释流程。"

	completeQuestion = "用一句话说明 Redis 的 AOF 是什么。"
	streamQuestion   = "用一句话说明 Redis 的 RDB 是什么。"

	// knowledgeQuestion shares exact terms with knowledgeMarkdown so retrieval
	// admits it; the question is otherwise unimportant.
	knowledgeQuestion = "AOF 的 fsync 策略可以选哪些？"
	knowledgeMarkdown = "# AOF 持久化\n\nAOF 的 fsync 策略可以选 always、everysec 或 no。\n"

	// noEvidenceQuestion shares no term with the stored document, so retrieval
	// admits nothing and the answer must be produced without a provider call.
	noEvidenceQuestion = "光合作用中的卡尔文循环发生在哪个部位"

	// noEvidenceMarkdown is a corpus of its own rather than a copy of the knowledge
	// step's notes: ingestion refuses a second identical document for one user, which
	// is the per-user uniqueness v2 enforces. The step only needs a stored document
	// that cannot answer the question, and a note about image layers serves that just
	// as well as a note about AOF while remaining a different document.
	noEvidenceMarkdown = "# 镜像分层\n\n容器镜像由若干只读层叠加而成。\n"
)

// Step names, statuses and failure codes are closed vocabularies: a reader must be
// able to compare two recorded runs without parsing prose, and a provider's own
// error wording is never part of the record.
const (
	stepSetup      = "setup"
	stepComplete   = "complete"
	stepStream     = "stream"
	stepKnowledge  = "knowledge_qa"
	stepNoEvidence = "no_evidence"
)

const (
	statusOK     = "ok"
	statusFailed = "failed"
)

const (
	failProviderNotConfigured    = "provider_not_configured"
	failInvalidRequest           = "invalid_request"
	failProviderStatus           = "provider_status"
	failProviderUnavailable      = "provider_unavailable"
	failProviderTimeout          = "provider_timeout"
	failMalformedResponse        = "malformed_response"
	failEmptyCompletion          = "empty_completion"
	failRequestCap               = "request_cap_exceeded"
	failCanceled                 = "canceled"
	failDoneMarkerInStream       = "done_marker_in_stream"
	failNothingCited             = "nothing_cited"
	failForeignCitation          = "citation_outside_smoke_document"
	failAnsweredWithoutEvidence  = "answered_without_evidence"
	failCitedWithoutEvidence     = "cited_without_evidence"
	failRequestSpentWithoutProof = "request_spent_without_evidence"
	failUnclassified             = "unclassified_failure"
)

var (
	// ErrDoneMarkerInStream reports a provider that leaked its protocol terminator
	// into answer text.
	ErrDoneMarkerInStream = errors.New("model smoke: the stream delivered [DONE] as answer text")
	// ErrNothingCited reports a knowledge turn whose document was retrievable yet
	// cited nothing - the citation path is the thing under test.
	ErrNothingCited = errors.New("model smoke: the grounded answer cited nothing although its document was retrievable")
	// ErrForeignCitation reports a citation that does not point at the throwaway
	// document: it can only have been invented by the model or leaked from another
	// scope.
	ErrForeignCitation = errors.New("model smoke: a citation pointed outside the smoke document")
	// ErrAnsweredWithoutEvidence reports a grounded turn that answered an unrelated
	// question instead of saying it found nothing.
	ErrAnsweredWithoutEvidence = errors.New("model smoke: an unrelated question was answered from the knowledge base")
	// ErrCitedWithoutEvidence reports a grounded turn that produced citations for an
	// unrelated question.
	ErrCitedWithoutEvidence = errors.New("model smoke: an unrelated question cited a chunk")
	// ErrRequestSpentWithoutEvidence reports that the zero-call check consumed a
	// provider request, which makes it a fourth paid call.
	ErrRequestSpentWithoutEvidence = errors.New("model smoke: the no-evidence check spent a provider request")
)

// usageRecord mirrors what a provider reported for the non-streaming completion, or
// is null when it reported nothing.
type usageRecord struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// stepRecord is what one step of the run actually did. A failed step keeps its real
// counts and a null answer rather than disappearing, because the file's purpose is
// to say which part worked.
type stepRecord struct {
	Name         string `json:"name"`
	Status       string `json:"status"`
	RequestsUsed int    `json:"requests_used"`
	NonEmpty     bool   `json:"nonempty"`
	// AnswerSHA256 is the hash of that step's model answer, or null when the step
	// failed or produced no answer. It is enough to prove later that the same call
	// returned the same bytes, and not enough to read them back.
	AnswerSHA256 *string `json:"answer_sha256"`
	// FinishReason is the provider's own reason for the step, null when none came
	// back or the step is not a model call.
	FinishReason *string `json:"finish_reason"`
	// DeltaCount and SourceCount are only meaningful for the streamed and grounded
	// steps; they stay 0 elsewhere rather than claiming an unmeasured fact.
	DeltaCount  int `json:"delta_count"`
	SourceCount int `json:"source_count"`
	// FailureCode is from the closed set above and null on success. The raw error
	// goes to stderr only: it is the one place a provider's words may appear.
	FailureCode *string `json:"failure_code"`
	// ProviderStatus records the HTTP status behind a provider_status failure so an
	// operator can tell a bad key from a quota, without storing any response text.
	ProviderStatus *int `json:"provider_status"`
}

// report is the entire on-disk result. The field list is the contract: answers,
// prompts and the Authorization header are never recorded, only hashes, counts and
// machine-readable reasons.
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

	CompleteNonEmpty bool `json:"complete_nonempty"`

	StreamNonEmpty   bool `json:"stream_nonempty"`
	StreamDeltaCount int  `json:"stream_delta_count"`

	KnowledgeAnswerNonEmpty bool `json:"knowledge_answer_nonempty"`
	KnowledgeSourceCount    int  `json:"knowledge_source_count"`
	NoEvidenceSkipped       bool `json:"no_evidence_skipped_provider"`

	FinishReason    string       `json:"finish_reason"`
	UsageIfReturned *usageRecord `json:"usage_if_returned"`

	// Steps holds the per-step truth, including the three answer hashes. It is the
	// only place a step's facts are computed; the headline fields above are derived
	// from it so the two cannot disagree.
	Steps []stepRecord `json:"steps"`

	APIKeyRecorded bool `json:"api_key_recorded"`
	RealMySQL      bool `json:"real_mysql"`
	Redis          bool `json:"redis"`
	RabbitMQ       bool `json:"rabbitmq"`
	Docker         bool `json:"docker"`

	// requestMark is the meter reading at the end of the previous step. Steps run in
	// order, so each one's own request count is the difference - and the caller
	// cannot get the bookkeeping wrong by taking the reading in the wrong place. It
	// is bookkeeping, never recorded.
	requestMark int
}

func (r report) encode() ([]byte, error) {
	encoded, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}

// deriveHeadlineFacts recomputes the contract's flat fields from the step records.
func (r *report) deriveHeadlineFacts() {
	for _, step := range r.Steps {
		switch step.Name {
		case stepComplete:
			r.CompleteNonEmpty = step.NonEmpty
			r.FinishReason = ""
			if step.FinishReason != nil {
				r.FinishReason = *step.FinishReason
			}
		case stepStream:
			r.StreamNonEmpty = step.NonEmpty
			r.StreamDeltaCount = step.DeltaCount
		case stepKnowledge:
			r.KnowledgeAnswerNonEmpty = step.NonEmpty
			r.KnowledgeSourceCount = step.SourceCount
		case stepNoEvidence:
			r.NoEvidenceSkipped = step.Status == statusOK && step.RequestsUsed == 0
		}
	}
}

// execute runs the four steps in order against one metered client and returns the
// record of what happened. A step that fails does not stop the others and does not
// suppress the record: the point of the file is to say which part worked, and the
// request cap already bounds the damage of a provider that fails every time.
func execute(settings resolved, deps dependencies) (report, error) {
	result := report{
		SchemaVersion: schemaVersion,
		ExecutedAt:    deps.now().UTC().Format(timeLayout),
		Provider:      string(app.ProviderOpenAI),
		EndpointHost:  hostOf(settings.model.BaseURL),
		Model:         settings.model.Model,
		RequestCap:    requestCap,
		Retries:       0,
		// These flags are statements about this code path, not settings: the smoke
		// uses in-memory repositories and records no credential anywhere.
		RealMySQL: false,
		Redis:     false,
		RabbitMQ:  false,
		Docker:    false,
	}
	var failures []error

	inner, err := deps.newModel(settings.model)
	if err != nil {
		recordSetupFailure(&result, err)
		return result, summarize(result, settings.output, err)
	}
	meter := newMetered(inner, requestCap)

	knowledgeStore := knowledge.NewMemoryStore()
	repo := repository.NewInMemoryRepository()
	assistant, _, err := app.New(knowledgeStore, app.Config{
		Provider: app.ProviderOpenAI,
		Model:    settings.model,
		Context:  settings.budget,
	}, app.Options{
		// The meter is installed here rather than inside app so that the direct
		// steps and the production strategy share one counter.
		NewModel: func(openai.Config) (model.ChatModel, error) { return meter, nil },
	})
	if err != nil {
		recordSetupFailure(&result, err)
		return result, summarize(result, settings.output, err)
	}

	ctx := context.Background()
	// One throwaway user and one in-memory transcript for the whole run: nothing
	// here can reach a real database, and reusing the identity keeps the two
	// knowledge steps from colliding on a unique email.
	userID, err := smokeUser(ctx, repo)
	if err != nil {
		recordSetupFailure(&result, err)
		return result, summarize(result, settings.output, err)
	}

	complete, usage, err := runComplete(ctx, meter)
	finishStep(&result, &failures, meter, stepComplete, "complete step", complete, err)

	streamed, err := runStream(ctx, meter)
	finishStep(&result, &failures, meter, stepStream, "stream step", streamed, err)

	knowledged, err := runKnowledge(ctx, repo, knowledgeStore, assistant, settings, userID)
	finishStep(&result, &failures, meter, stepKnowledge, "knowledge step", knowledged, err)

	skipped, err := runNoEvidence(ctx, repo, knowledgeStore, assistant, settings, userID)
	finishStep(&result, &failures, meter, stepNoEvidence, "no-evidence step", skipped, err)

	result.UsageIfReturned = usage
	result.ActualRequests = meter.requests
	result.Failures = meter.failures
	result.APIKeyRecorded = false
	result.deriveHeadlineFacts()
	return result, summarize(result, settings.output, errors.Join(failures...))
}

// failure is the only shape a failed run leaves this process with. Its text is
// rebuilt from the closed-set codes, the numeric provider status and the result path,
// because the errors underneath carry a provider's own wording - and an operator
// reading stderr must not be the one who leaks it. errors.Is still reaches the real
// cause, so the tests can name what broke without printing it.
type failure struct {
	message string
	cause   error
}

func (f *failure) Error() string { return f.message }

func (f *failure) Unwrap() error { return f.cause }

// summarize turns a cause into the safe one-line report of which steps failed. A nil
// cause stays nil, so a good run reports nothing.
func summarize(record report, output string, cause error) error {
	if cause == nil {
		return nil
	}
	parts := make([]string, 0, len(record.Steps))
	for _, step := range record.Steps {
		if step.Status != statusFailed {
			continue
		}
		label := step.Name + "=" + codeText(step.FailureCode)
		if step.ProviderStatus != nil {
			label += "/" + strconv.Itoa(*step.ProviderStatus)
		}
		parts = append(parts, label)
	}
	if len(parts) == 0 {
		// Nothing is marked failed, so this is a setup-class error that never got as
		// far as a step record. Say so without repeating what it said.
		parts = append(parts, "run="+failUnclassified)
	}
	return &failure{
		message: fmt.Sprintf("execution failed: %s output=%s", strings.Join(parts, " "), output),
		cause:   cause,
	}
}

func codeText(code *string) string {
	if code == nil {
		return failUnclassified
	}
	return *code
}

// recordSetupFailure documents a run that died before any step could be judged, so
// the file still says why there is nothing in it.
func recordSetupFailure(result *report, err error) {
	code := failureCodeOf(err)
	result.Steps = append(result.Steps, stepRecord{
		Name:        stepSetup,
		Status:      statusFailed,
		FailureCode: &code,
	})
	result.APIKeyRecorded = false
	result.deriveHeadlineFacts()
}

// finishStep completes a step record with the requests it really consumed and its
// outcome, appends it to the report, and collects the failure without hiding the
// numbers behind it.
func finishStep(result *report, failures *[]error, meter *metered, name, label string, step stepRecord, err error) {
	step.Name = name
	step.RequestsUsed = meter.requests - result.requestMark
	result.requestMark = meter.requests
	if err != nil {
		step.Status = statusFailed
		// A failed step has no answer to point at, even when bytes arrived before it
		// broke: what did arrive stays as counts - delta_count, source_count,
		// requests_used - and never as an answer digest or a finish reason.
		step.AnswerSHA256 = nil
		step.NonEmpty = false
		step.FinishReason = nil
		code := failureCodeOf(err)
		step.FailureCode = &code
		if status, ok := openai.ProviderStatusOf(err); ok {
			followed := status
			step.ProviderStatus = &followed
		}
		*failures = append(*failures, fmt.Errorf("%s: %w", label, err))
	} else {
		step.Status = statusOK
	}
	result.Steps = append(result.Steps, step)
}

// failureCodeOf reduces any error to the closed vocabulary above. The error text is
// dropped on purpose: a provider can put anything in it, including a fragment of the
// prompt it was handed.
func failureCodeOf(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrRequestCapExceeded):
		return failRequestCap
	case errors.Is(err, ErrDoneMarkerInStream):
		return failDoneMarkerInStream
	case errors.Is(err, ErrNothingCited):
		return failNothingCited
	case errors.Is(err, ErrForeignCitation):
		return failForeignCitation
	case errors.Is(err, ErrAnsweredWithoutEvidence):
		return failAnsweredWithoutEvidence
	case errors.Is(err, ErrCitedWithoutEvidence):
		return failCitedWithoutEvidence
	case errors.Is(err, ErrRequestSpentWithoutEvidence):
		return failRequestSpentWithoutProof
	case errors.Is(err, model.ErrNotConfigured):
		return failProviderNotConfigured
	case errors.Is(err, model.ErrInvalidRequest):
		return failInvalidRequest
	case errors.Is(err, model.ErrTimeout):
		return failProviderTimeout
	case errors.Is(err, model.ErrProviderStatus):
		return failProviderStatus
	case errors.Is(err, model.ErrMalformedResponse):
		return failMalformedResponse
	case errors.Is(err, model.ErrEmptyCompletion):
		return failEmptyCompletion
	case errors.Is(err, model.ErrUnavailable):
		return failProviderUnavailable
	case errors.Is(err, context.Canceled):
		return failCanceled
	default:
		return failUnclassified
	}
}

// timeLayout is RFC3339 with a mandatory offset, which is what a later reader needs
// to line the run up with provider-side logs.
const timeLayout = "2006-01-02T15:04:05Z07:00"

func runComplete(ctx context.Context, chat model.ChatModel) (stepRecord, *usageRecord, error) {
	completion, err := chat.Complete(ctx, smokeRequest(completeQuestion))
	if err != nil {
		return stepRecord{}, nil, err
	}
	// Emptiness is judged after trimming; the digest is of the bytes as they came
	// back. The same split applies to all three answers, so a hash can be compared
	// against a provider log without guessing what this tool tidied up first.
	record := stepRecord{AnswerSHA256: hashAnswer(completion.Content)}
	record.NonEmpty = record.AnswerSHA256 != nil
	if !record.NonEmpty {
		return record, nil, model.ErrEmptyCompletion
	}
	record.FinishReason = reasonOf(completion.FinishReason)
	return record, usageOf(completion.Usage), nil
}

func runStream(ctx context.Context, chat model.ChatModel) (stepRecord, error) {
	var (
		deltas    int
		assembled strings.Builder
	)
	completion, err := chat.Stream(ctx, smokeRequest(streamQuestion), func(delta string) error {
		// The protocol's own end marker must never arrive as answer text. If it
		// does, this run reports it instead of writing a normal-looking result.
		if delta == "[DONE]" {
			return fmt.Errorf("%w: %w", ErrDoneMarkerInStream, model.ErrMalformedResponse)
		}
		deltas++
		assembled.WriteString(delta)
		return nil
	})
	record := stepRecord{DeltaCount: deltas, AnswerSHA256: hashAnswer(assembled.String())}
	record.NonEmpty = record.AnswerSHA256 != nil
	if err != nil {
		return record, err
	}
	if deltas == 0 {
		return record, fmt.Errorf("%w: the stream produced no content delta", model.ErrMalformedResponse)
	}
	if record.AnswerSHA256 == nil {
		return record, model.ErrEmptyCompletion
	}
	record.FinishReason = reasonOf(completion.FinishReason)
	return record, nil
}

// runKnowledge drives the real conversation stack: an in-memory repository and
// knowledge store, a throwaway user, collection, Markdown document and chat
// session, then MessageService.Send with grounding enabled. It is the only step
// that proves retrieval, admission and the model generator are wired together.
func runKnowledge(ctx context.Context, repo repository.Repository, store knowledge.Store, assistant responder.Responder, settings resolved, userID string) (stepRecord, error) {
	collection, err := store.CreateCollection(ctx, &domain.Collection{UserID: userID, Title: "冒烟手册"})
	if err != nil {
		return stepRecord{}, err
	}
	knowledgeService := service.NewKnowledgeService(store)
	document, err := knowledgeService.AddDocument(ctx, userID, collection.ID, service.DocumentInput{
		Title:   "aof.md",
		Format:  string(domain.DocumentFormatMarkdown),
		Content: knowledgeMarkdown,
	})
	if err != nil {
		return stepRecord{}, err
	}
	session, err := repo.CreateSession(ctx, &domain.Session{UserID: userID, Title: "冒烟会话", Mode: domain.SessionModeChat})
	if err != nil {
		return stepRecord{}, err
	}
	messages := app.NewMessageService(repo, assistant, knowledgeService, settings.budget)
	pair, err := messages.Send(ctx, userID, session.ID, knowledgeQuestion, domain.KnowledgeOptions{
		Enabled: true, CollectionIDs: []string{collection.ID},
	})
	if err != nil {
		return stepRecord{}, err
	}
	record := stepRecord{
		SourceCount: len(pair.Assistant.Sources),
	}
	record.AnswerSHA256 = hashAnswer(pair.Assistant.Content)
	record.NonEmpty = record.AnswerSHA256 != nil
	// A grounded answer with no citation, or with a citation from somewhere else,
	// is a failed step even when the model produced fluent text.
	if record.SourceCount == 0 {
		return record, ErrNothingCited
	}
	if record.AnswerSHA256 == nil {
		return record, model.ErrEmptyCompletion
	}
	if err := citationsOwned(pair.Assistant.Sources, document.ID, collection.ID); err != nil {
		return record, err
	}
	return record, nil
}

// citationsOwned is the server-side half of the citation promise: every source must
// point at the document this run created, and every source id must be derived from a
// chunk of it. Anything else can only have been invented by the model or leaked from
// another scope, and both are hard failures rather than cosmetic ones.
func citationsOwned(sources []domain.Source, documentID, collectionID string) error {
	for _, source := range sources {
		if source.DocumentID != documentID || source.CollectionID != collectionID {
			return fmt.Errorf("%w: %s/%s", ErrForeignCitation, source.CollectionID, source.DocumentID)
		}
		if source.SourceID != "src-"+source.ChunkID {
			return fmt.Errorf("%w: %q is not derived from a retrieved chunk", ErrForeignCitation, source.SourceID)
		}
	}
	return nil
}

// runNoEvidence proves the cheapest and most important property of the knowledge
// path: with nothing admitted, the answer is the fixed "nothing found" text and the
// provider was asked nothing. It consumes no budget, so the run stays at three
// requests, and its answer is never hashed - that text is produced by this server,
// not by the model, so a hash of it would record nothing about the provider.
func runNoEvidence(ctx context.Context, repo repository.Repository, store knowledge.Store, assistant responder.Responder, settings resolved, userID string) (stepRecord, error) {
	collection, err := store.CreateCollection(ctx, &domain.Collection{UserID: userID, Title: "无证据手册"})
	if err != nil {
		return stepRecord{}, err
	}
	knowledgeService := service.NewKnowledgeService(store)
	if _, err := knowledgeService.AddDocument(ctx, userID, collection.ID, service.DocumentInput{
		Title: "layers.md", Format: string(domain.DocumentFormatMarkdown), Content: noEvidenceMarkdown,
	}); err != nil {
		return stepRecord{}, err
	}
	session, err := repo.CreateSession(ctx, &domain.Session{UserID: userID, Title: "冒烟会话", Mode: domain.SessionModeChat})
	if err != nil {
		return stepRecord{}, err
	}
	messages := app.NewMessageService(repo, assistant, knowledgeService, settings.budget)
	pair, err := messages.Send(ctx, userID, session.ID, noEvidenceQuestion, domain.KnowledgeOptions{
		Enabled: true, CollectionIDs: []string{collection.ID},
	})
	if err != nil {
		return stepRecord{}, err
	}
	record := stepRecord{
		NonEmpty:    pair.Assistant.Content != "",
		SourceCount: len(pair.Assistant.Sources),
	}
	if pair.Assistant.Content != responder.NoEvidenceAnswer {
		return record, ErrAnsweredWithoutEvidence
	}
	if record.SourceCount != 0 {
		return record, ErrCitedWithoutEvidence
	}
	return record, nil
}

func smokeRequest(question string) model.Request {
	return model.Request{Messages: []model.Message{
		{Role: model.RoleSystem, Content: smokeSystemPrompt},
		{Role: model.RoleUser, Content: question},
	}}
}

func smokeUser(ctx context.Context, repo repository.Repository) (string, error) {
	user, err := repo.CreateUser(ctx, &domain.User{
		Email:        "smoke@localhost.invalid",
		DisplayName:  "model smoke",
		PasswordHash: "not-a-credential-this-step-never-authenticates",
	})
	if err != nil {
		return "", err
	}
	return user.ID, nil
}

func hashText(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// hashAnswer answers "what did the model say" without saying it, and returns nil for
// an answer that does not exist so the record shows null rather than the hash of an
// empty string.
func hashAnswer(text string) *string {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	hash := hashText(text)
	return &hash
}

func reasonOf(reason model.FinishReason) *string {
	text := strings.TrimSpace(string(reason))
	if text == "" {
		return nil
	}
	return &text
}

func usageOf(usage model.Usage) *usageRecord {
	if usage.PromptTokens == 0 && usage.CompletionTokens == 0 && usage.TotalTokens == 0 {
		// Providers that omit usage must produce null, not a row of zeros that a
		// reader could mistake for a real measurement.
		return nil
	}
	return &usageRecord{
		PromptTokens:     usage.PromptTokens,
		CompletionTokens: usage.CompletionTokens,
		TotalTokens:      usage.TotalTokens,
	}
}

// hostOf reduces the endpoint to a host for the record. The path can carry a
// deployment-specific prefix and is not needed to identify a run; the key is never
// in the URL in the first place, because openai.Config refuses credentials there.
func hostOf(baseURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Host == "" {
		return "unavailable"
	}
	return parsed.Host
}
