package conversation

// Diagnosis of a rolling-summary failure. The product's own log prints the
// type of the error but not its text, so a failed compaction cannot be
// explained from a run's logs. This replays one conversation through the same
// summarizer, with the same wiring, and keeps the whole error, the stage the
// error came from, and the body the model returned.
//
// The command never rewrites a request and never replaces a product
// dependency: the model endpoint is the operator's counting proxy and the
// summary policy is the product's own configuration.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"GopherAI/common/aihelper"
	"GopherAI/common/memoryrecall"
	"GopherAI/common/rag"
	"GopherAI/config"
	"GopherAI/entry"
	"GopherAI/model"
	agent "GopherAI/service/agent"
	"GopherAI/utils"
	"github.com/cloudwego/eino-ext/components/model/openai"
	modelcomponent "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"gorm.io/gorm"
)

// recordingChat keeps the last body the model returned, so a failed parse can
// be read next to the text that caused it.
type recordingChat struct {
	inner modelcomponent.ToolCallingChatModel
	mu    sync.Mutex
	last  string
}

func (r *recordingChat) Generate(ctx context.Context, input []*schema.Message, opts ...modelcomponent.Option) (*schema.Message, error) {
	out, err := r.inner.Generate(ctx, input, opts...)
	if out != nil {
		r.mu.Lock()
		r.last = out.Content
		r.mu.Unlock()
	}
	return out, err
}

func (r *recordingChat) Stream(ctx context.Context, input []*schema.Message, opts ...modelcomponent.Option) (*schema.StreamReader[*schema.Message], error) {
	return r.inner.Stream(ctx, input, opts...)
}

func (r *recordingChat) WithTools(tools []*schema.ToolInfo) (modelcomponent.ToolCallingChatModel, error) {
	bound, err := r.inner.WithTools(tools)
	if err != nil {
		return nil, err
	}
	return &recordingChat{inner: bound}, nil
}

func (r *recordingChat) body() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last
}

// SummaryDiagnosis is what one failed compaction looked like.
type SummaryDiagnosis struct {
	Scenario    string   `json:"scenario"`
	Condition   string   `json:"condition"`
	Turns       int      `json:"turns"`
	FailureTurn int      `json:"failure_turn"`
	Messages    int      `json:"messages_at_failure"`
	ErrorType   string   `json:"error_type"`
	ErrorText   string   `json:"error_text"`
	ErrorChain  []string `json:"error_chain"`
	Stage       string   `json:"stage"`
	RawResponse string   `json:"raw_response,omitempty"`
	ModelCalls  int      `json:"model_calls"`
	// RawEvidenceCounts is how many items the model put in each group of the
	// summary it returned, and RawItemLimit is the product's own limit per
	// group. A model that returns more items than the limit has is the whole
	// reason a compaction can fail on a document that parses.
	RawEvidenceCounts map[string]int `json:"raw_evidence_counts,omitempty"`
	RawItemLimit      int            `json:"raw_item_limit,omitempty"`
}

// The phrases that name the step a failure came from. The product's decoder
// reports a document that breaks a limit with the same prefix it uses for a
// body it could not read, so the phrases of the innermost error decide.
//
// The phrases of the item limits are kept even though the product does not
// refuse a document for breaking one any more: a document that outgrew its
// budget is trimmed, and a saved diagnosis from a run before that change can
// still carry the old error and has to be readable.
var summaryFailurePhrases = []struct {
	phrase string
	stage  string
}{
	{"generate rolling summary", "model_call"},
	{"token limit", "length_check"},
	{"invalid session summary goal", "length_check"},
	{"too many evidence items", "structure_validation"},
	{"unsupported session summary schema version", "structure_validation"},
	{"session summary evidence:", "structure_validation"},
	{"rolling summary evidence:", "structure_validation"},
	{"cited non-user or unavailable message", "structure_validation"},
	{"created evidence without a message id", "structure_validation"},
	{"must use schema version", "structure_validation"},
	{"invalid character", "output_parse"},
	{"unexpected end of JSON", "output_parse"},
	{"cannot unmarshal", "output_parse"},
	{"decode rolling summary", "output_parse"},
	{"summary model returned no durable session state", "empty_output"},
	{"rolling summary", "structure_validation"},
}

// summaryStage names the step the failure came from.
func summaryStage(text string) string {
	for _, phrase := range summaryFailurePhrases {
		if strings.Contains(text, phrase.phrase) {
			return phrase.stage
		}
	}
	return "unknown"
}

// summaryStageOfChain names the step from the whole error, from the outermost
// wrap to the innermost cause.
func summaryStageOfChain(chain []string) string {
	for i := len(chain) - 1; i >= 0; i-- {
		if stage := summaryStage(chain[i]); stage != "unknown" {
			return stage
		}
	}
	return "unknown"
}

// rawEvidenceCounts reads the groups of the body the model returned, so a
// document that breaks a limit can be read next to the limit it broke.
func rawEvidenceCounts(body string) map[string]int {
	var document struct {
		ConfirmedDecisions []json.RawMessage `json:"confirmed_decisions"`
		OpenLoops          []json.RawMessage `json:"open_loops"`
		UserCorrections    []json.RawMessage `json:"user_corrections"`
		CompletedItems     []json.RawMessage `json:"completed_items"`
		KeyErrors          []json.RawMessage `json:"key_errors"`
		References         []json.RawMessage `json:"references"`
	}
	if err := json.Unmarshal([]byte(body), &document); err != nil {
		return nil
	}
	return map[string]int{
		"confirmed_decisions": len(document.ConfirmedDecisions),
		"open_loops":          len(document.OpenLoops),
		"user_corrections":    len(document.UserCorrections),
		"completed_items":     len(document.CompletedItems),
		"key_errors":          len(document.KeyErrors),
		"references":          len(document.References),
	}
}

// errorChain is the whole error, from the outermost wrap to the innermost cause.
func errorChain(err error) []string {
	var chain []string
	for err != nil {
		chain = append(chain, err.Error())
		err = errors.Unwrap(err)
	}
	return chain
}

// summarySettings is the product's configuration with the chat model pointed
// at the counting proxy. The knowledge index is not part of a summary, so the
// example configuration's own placeholders stay where they are.
func summarySettings(repository, storage, chatModel string) (*entry.Settings, error) {
	settings, err := offlineSettings(repository, storage)
	if err != nil {
		return nil, err
	}
	settings.Entry.ChatModel = chatModel
	settings.Entry.ChatKey = "placeholder-proxy-injected"
	settings.Config.RagModelConfig.RagEmbeddingAPIKey = "placeholder-proxy-injected"
	settings.Config.RagModelConfig.RagChatThinkingDialect = config.ChatThinkingDialectDeepSeek
	settings.Config.AgentThinkingEnabled = true
	return settings, settings.Validate()
}

// DiagnoseSummaryFailure replays one conversation and stops at the first
// compaction failure, keeping the whole error and the body that produced it.
func DiagnoseSummaryFailure(ctx context.Context, repository, scenario, condition string, turns []Turn, proxyBase, chatModel string) (diagnosis SummaryDiagnosis, resultErr error) {
	diagnosis = SummaryDiagnosis{Scenario: scenario, Condition: condition, Turns: len(turns), Stage: "no_failure"}
	if len(turns) == 0 {
		return diagnosis, fmt.Errorf("turns are required")
	}
	if err := ctx.Err(); err != nil {
		return diagnosis, err
	}
	storage, err := os.MkdirTemp("", "gl-summary-storage-")
	if err != nil {
		return diagnosis, err
	}
	defer os.RemoveAll(storage)
	settings, err := summarySettings(repository, storage, chatModel)
	if err != nil {
		return diagnosis, err
	}
	roles, err := LoadRolePrompts(repository, memoryrecall.ReadMode(settings.Config.MemoryReadMode))
	if err != nil {
		return diagnosis, err
	}
	ledger := &ModelLedger{}
	defer func() {
		if !ledger.Complete() {
			resultErr = errors.Join(resultErr, fmt.Errorf("summary diagnosis model telemetry incomplete"))
		}
	}()
	wire := realWire(ledger, roles, proxyBase)
	defer wire.Close()
	dialect, err := config.NormalizeChatThinkingDialect(settings.Config.RagModelConfig.RagChatThinkingDialect)
	if err != nil {
		return diagnosis, err
	}
	chat, err := openai.NewChatModel(ctx, &openai.ChatModelConfig{
		BaseURL:     wire.URL + "/v1",
		Model:       chatModel,
		APIKey:      "placeholder-proxy-injected",
		ExtraFields: aihelper.ThinkingExtraFields(dialect, false),
	})
	if err != nil {
		return diagnosis, err
	}
	recorder := &recordingChat{inner: chat}
	wrapped, err := WrapModel(recorder, ledger, roles)
	if err != nil {
		return diagnosis, err
	}
	policy := ProductSummaryConfig(settings.Config)
	generator, err := agent.NewModelSummaryGenerator(wrapped, policy.MaxInputTokens, policy.MaxOutputTokens, policy.Budget)
	if err != nil {
		return diagnosis, err
	}
	s, err := NewSandbox(ctx)
	if err != nil {
		return diagnosis, err
	}
	defer func() { resultErr = errors.Join(resultErr, closeSandbox(s)) }()
	if err = s.InitProductSchema(ctx); err != nil {
		return diagnosis, err
	}
	password, err := randomHex(24)
	if err != nil {
		return diagnosis, err
	}
	hash, err := utils.HashPassword(password)
	if err != nil {
		return diagnosis, err
	}
	if err = s.SeedProduct(ctx, model.User{ID: 7001, Username: "l4-eval-learner", Password: hash}, nil, ""); err != nil {
		return diagnosis, err
	}
	sessionID := "l4-summary-" + scenario
	if err = (guardedStore{s}).with(ctx, func(tx *gorm.DB) error {
		return tx.Create(&model.Session{ID: sessionID, UserID: 7001, UserName: "l4-eval-learner", Mode: model.SessionModeAgent, Title: "Summary diagnosis"}).Error
	}); err != nil {
		return diagnosis, err
	}
	guard := guardedStore{sandbox: s}
	provider, err := agent.NewRollingSummaryProvider(guard, guard, generator, policy)
	if err != nil {
		return diagnosis, err
	}
	for i, turn := range turns {
		if _, err = guard.Create(ctx, &model.Message{SessionID: sessionID, UserName: "l4-eval-learner", Content: turn.User, IsUser: true, CreatedAt: turn.UserAt.UTC(), AnswerBasis: rag.AnswerBasisConversation, KnowledgeStatus: rag.KnowledgeStatusNotRequired, KnowledgeReason: rag.KnowledgeReasonNone}); err != nil {
			return diagnosis, err
		}
		_, summaryErr := provider.SessionSummary(ctx, agent.RunContext{UserID: 7001, UserName: "l4-eval-learner", SessionID: sessionID, Question: turn.User})
		if summaryErr != nil {
			diagnosis.FailureTurn = i + 1
			diagnosis.Messages = (i + 1) * 2
			diagnosis.ErrorType = fmt.Sprintf("%T", summaryErr)
			diagnosis.ErrorText = summaryErr.Error()
			diagnosis.ErrorChain = errorChain(summaryErr)
			diagnosis.Stage = summaryStageOfChain(diagnosis.ErrorChain)
			diagnosis.RawResponse = recorder.body()
			diagnosis.RawEvidenceCounts = rawEvidenceCounts(diagnosis.RawResponse)
			diagnosis.RawItemLimit = model.SessionSummaryItemLimit
			diagnosis.ModelCalls = len(ledger.Calls())
			return diagnosis, nil
		}
		if _, err = guard.Create(ctx, &model.Message{SessionID: sessionID, UserName: "l4-eval-learner", Content: turn.Assistant, CreatedAt: turn.AssistantAt.UTC()}); err != nil {
			return diagnosis, err
		}
	}
	diagnosis.ModelCalls = len(ledger.Calls())
	return diagnosis, nil
}

// SummaryDiagnosisTimeout is the deadline the command gives one replay.
const SummaryDiagnosisTimeout = 20 * time.Minute

// ReclassifyDiagnosis reads a saved diagnosis and recomputes what can be read
// from it without a model call: the step the failure came from, and how many
// items the body the model returned put in each group.
func ReclassifyDiagnosis(diagnosis SummaryDiagnosis) SummaryDiagnosis {
	diagnosis.Stage = summaryStageOfChain(diagnosis.ErrorChain)
	diagnosis.RawEvidenceCounts = rawEvidenceCounts(diagnosis.RawResponse)
	diagnosis.RawItemLimit = model.SessionSummaryItemLimit
	return diagnosis
}
