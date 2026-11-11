package conversation

import (
	"GopherAI/common/memoryrecall"
	"GopherAI/config"
	"GopherAI/entry"
	"GopherAI/model"
	agent "GopherAI/service/agent"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	modelcomponent "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"gorm.io/gorm"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const ProductScope = "完整产品服务；后台任务在被测回答之前不执行；之后产生的后台调用单独记账"

// ProductResult is offline wiring evidence, never a real-model quality result.
type ProductResult struct {
	Scope           string               `json:"scope"`
	Environment     string               `json:"env"`
	Answer          string               `json:"answer"`
	Response        json.RawMessage      `json:"response"`
	ResponseText    string               `json:"response_text,omitempty"`
	BeforeCounts    map[string]JobCounts `json:"before_counts"`
	AfterCounts     map[string]JobCounts `json:"after_counts"`
	StoppedCounts   map[string]JobCounts `json:"stopped_counts"`
	Events          []JobEvent           `json:"job_events"`
	Calls           []ModelCall          `json:"model_calls"`
	UsageByRole     map[string]RoleUsage `json:"usage_by_role"`
	Error           string               `json:"error,omitempty"`
	Entry           string               `json:"entry"`
	Provenance      Provenance           `json:"provenance"`
	ContextMessages []*schema.Message    `json:"context_messages"`
	Audit           BackgroundAudit      `json:"background_evidence"`
	// MemoryIndexEvidence is read from the shared derived memory indexes: once
	// before the answer is asked and once after the background window closed.
	// A real attempt without both readings is not decidable, because the second
	// one is the only evidence that the answer did not leave state behind for
	// the next repeat.
	MemoryIndexEvidence MemoryIndexEvidence `json:"memory_index_evidence"`
	BackgroundAudit     struct {
		Valid  bool   `json:"valid"`
		Reason string `json:"reason,omitempty"`
	} `json:"backgroundAudit"`
	QuietWindowSeconds   int   `json:"quiet_window_seconds"`
	RestoreMilliseconds  int64 `json:"restore_ms"`
	ShutdownComplete     bool  `json:"shutdown_complete"`
	QuestionMilliseconds int64 `json:"question_ms"`
	// Questions holds one entry per question the attempt asked, in the order
	// they were asked. It is written only when the attempt asked more than
	// one: the single-question attempt keeps its own fields, and a reader of
	// an old attempt finds the same shape it always did.
	Questions []QuestionResult `json:"questions,omitempty"`
	// SeededMemories says what the seeded profile memories became, and
	// SeededMemoryRows is what was read back from the database afterwards.
	// The write is checked by the read: a seed that cannot be read back is
	// not a seed.
	SeededMemories   SeedMemoryReport       `json:"seeded_memories"`
	SeededMemoryRows []model.LongTermMemory `json:"seeded_memory_rows,omitempty"`
}

// QuestionResult is one question of an attempt and what came back.
//
// The answer, the raw response and the steps are kept per question, so that
// a scenario with several questions has the evidence of each question next
// to its own answer and not in one pile.
type QuestionResult struct {
	Index                int               `json:"index"`
	Question             string            `json:"question"`
	Answer               string            `json:"answer"`
	RunID                string            `json:"run_id,omitempty"`
	Response             json.RawMessage   `json:"response,omitempty"`
	Steps                []model.AgentStep `json:"steps,omitempty"`
	ContextMessages      []*schema.Message `json:"context_messages,omitempty"`
	StartedAt            time.Time         `json:"started_at"`
	FinishedAt           time.Time         `json:"finished_at"`
	QuestionMilliseconds int64             `json:"question_ms"`
	Error                string            `json:"error,omitempty"`
}

// AttemptInput is what one attempt adds to the standard wiring.
//
// The memories are the profile memories the learner confirmed before this
// attempt. They are written into the evaluation database before the
// conversation is prepared and again before the question is asked, because
// the answer is asked in a fresh database.
//
// The questions are asked in order, in the same session. A scenario with one
// question has one entry; a scenario that asks a follow-up has one entry per
// question, and the earlier question and its answer stay in the session, so
// that the later question is answered in the context of the earlier one.
type AttemptInput struct {
	Memories  []SeedMemory
	Questions []string
}

// ProductSummaryConfig mirrors the production wiring, including its context
// summary output limit rather than the standalone summarizer test defaults.
func ProductSummaryConfig(c *config.Config) agent.SummaryConfig {
	a := c.AgentConfig
	return agent.SummaryConfig{TriggerMessages: a.SummaryTriggerMessages, TriggerTokens: a.SummaryTriggerTokens, RetainMessages: a.SummaryRetainMessages, MaxInputTokens: a.SummaryMaxInputTokens, MaxOutputTokens: a.ContextMaxSummaryTokens, FetchLimit: a.SummaryFetchLimit, Timeout: time.Duration(a.SummaryTimeoutSeconds) * time.Second, StageBudget: time.Duration(a.SummaryStageBudgetSeconds) * time.Second, MaxDeadlineShare: agent.DefaultSummaryConfig().MaxDeadlineShare, FailureCooldown: time.Duration(a.SummaryFailureCooldownSeconds) * time.Second, Budget: agent.SummaryBudgetOf(a.SummaryGroupItemLimit, a.SummaryOpenLoopsItemLimit, a.SummaryEvidenceIDLimit)}
}

func offlineSettings(repository, storage string) (*entry.Settings, error) {
	data, err := os.ReadFile(filepath.Join(repository, "tutorial", "v2-entry", "isolated.example.toml"))
	if err != nil {
		return nil, err
	}
	text := strings.ReplaceAll(string(data), `key = ""`, `key = "synthetic-offline-key-with-at-least-32-characters"`)
	text = strings.ReplaceAll(text, `chatKey = ""`, `chatKey = "synthetic-never-sent"`)
	text = strings.ReplaceAll(text, `embeddingApiKey = ""`, `embeddingApiKey = "synthetic-never-sent"`)
	dir, err := os.MkdirTemp("", "gl-offline-config-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "offline.toml")
	if err = os.WriteFile(path, []byte(text), 0600); err != nil {
		return nil, err
	}
	settings, err := entry.Load(path)
	if err != nil {
		return nil, err
	}
	settings.Config.KnowledgeConfig.StorageDir = storage
	settings.Config.AgentConfig.EpisodeQuietWindowSeconds = 86400
	settings.Config.AgentThinkingEnabled = true
	settings.Config.RagChatThinkingDialect = config.ChatThinkingDialectDeepSeek
	settings.Config.RagRerankEnabled = true
	settings.Config.RagRerankProvider = config.RerankProviderDashScope
	settings.Config.RagRerankEndpoint = "http://127.0.0.1:1/v1/rerank"
	settings.Config.RagRerankModel = "synthetic-rerank"
	settings.Config.RagRerankToken = "synthetic-rerank-token-at-least-32-characters"
	return settings, settings.Validate()
}

func closeSandbox(s *Sandbox) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return s.Close(ctx)
}

// PrepareOfflineProduct uses a deterministic fake model behind the real summary
// generator and rolling provider. There is no online/provider credential path.
type WarmupReport struct {
	Snapshot      Snapshot             `json:"-"`
	Calls         []ModelCall          `json:"model_calls"`
	Turns         []PreparedTurn       `json:"turns"`
	DegradedTurns int                  `json:"degraded_turns"`
	UsageByRole   map[string]RoleUsage `json:"usage_by_role"`
	// FailureCooldownSeconds is the product's own failure cooldown, and
	// CooldownWaits is how many times the replay waited it out before writing
	// the next turn. A replay that does not wait would spend the cooldown in
	// one breath and never compact again.
	FailureCooldownSeconds int `json:"failure_cooldown_seconds"`
	CooldownWaits          int `json:"cooldown_waits"`
	// SummaryFailureReasons counts the degraded turns by the step that
	// failed. The cooldown is one of them: it is counted on its own so that
	// the share of degraded turns still reads as the share of real failures.
	SummaryFailureReasons map[string]int `json:"summary_failure_reasons,omitempty"`
	// CooldownTurns is how many of the degraded turns were held back by the
	// cooldown rather than failed by themselves.
	CooldownTurns int `json:"cooldown_turns"`
	// SummaryTruncations is how many times the compression budget trimmed a
	// summary, and SummaryDroppedItems is how many items it cut.
	SummaryTruncations  int `json:"summary_truncations"`
	SummaryDroppedItems int `json:"summary_dropped_items"`
	// SeededMemories says what the seeded profile memories became. The
	// memories are written before the conversation is prepared, so that the
	// product sees them while the conversation is built.
	SeededMemories SeedMemoryReport `json:"seeded_memories"`
}

func PrepareOfflineProduct(ctx context.Context, repository string, turns []Turn, input AttemptInput) (Snapshot, []ModelCall, error) {
	report, err := PrepareOfflineProductReport(ctx, repository, turns, input)
	return report.Snapshot, report.Calls, err
}
func PrepareOfflineProductReport(ctx context.Context, repository string, turns []Turn, input AttemptInput) (report WarmupReport, resultErr error) {
	report.Snapshot, report.Calls, report.SeededMemories, resultErr = prepareOfflineProduct(ctx, repository, turns, input, func(prepared []PreparedTurn) {
		report.Turns = prepared
		SummarizeTurns(prepared, &report)
	})
	var usageErr error
	report.UsageByRole, usageErr = SummarizeCalls(report.Calls)
	return report, errors.Join(resultErr, usageErr)
}
func prepareOfflineProduct(ctx context.Context, repository string, turns []Turn, input AttemptInput, observe func([]PreparedTurn)) (snapshot Snapshot, calls []ModelCall, memories SeedMemoryReport, resultErr error) {
	if err := ctx.Err(); err != nil {
		return snapshot, nil, memories, err
	}
	storage, err := os.MkdirTemp("", "gl-offline-storage-")
	if err != nil {
		return snapshot, nil, memories, err
	}
	defer os.RemoveAll(storage)
	settings, err := offlineSettings(repository, storage)
	if err != nil {
		return snapshot, nil, memories, err
	}
	s, err := NewSandbox(ctx)
	if err != nil {
		return snapshot, nil, memories, err
	}
	defer func() { resultErr = errors.Join(resultErr, closeSandbox(s)) }()
	if err = s.InitProductSchema(ctx); err != nil {
		return snapshot, nil, memories, err
	}
	if err = s.SeedProduct(ctx, model.User{ID: 7, Username: "synthetic-learner"}, nil, storage); err != nil {
		return snapshot, nil, memories, err
	}
	// The seeded profile memories are written before the conversation is
	// prepared, so that the product sees them while the conversation is
	// built and not only when the question is asked.
	memories, _, err = seedAttemptMemories(ctx, s, 7, input.Memories)
	if err != nil {
		return snapshot, nil, memories, err
	}
	if err = (guardedStore{s}).with(ctx, func(tx *gorm.DB) error {
		return tx.Create(&model.Session{ID: "synthetic-evaluation", UserID: 7, UserName: "synthetic-learner", Mode: model.SessionModeAgent, Title: "Synthetic evaluation"}).Error
	}); err != nil {
		return snapshot, nil, memories, err
	}
	ledger := &ModelLedger{}
	defer func() {
		if !ledger.Complete() {
			resultErr = errors.Join(resultErr, fmt.Errorf("warmup model telemetry incomplete"))
		}
	}()
	roles, err := LoadRolePrompts(repository, memoryrecall.ReadMode(settings.Config.AgentConfig.MemoryReadMode))
	if err != nil {
		return snapshot, nil, memories, err
	}
	chat, err := WrapModel(&offlineChat{}, ledger, roles)
	if err != nil {
		return snapshot, nil, memories, err
	}
	policy := ProductSummaryConfig(settings.Config)
	generator, err := agent.NewModelSummaryGenerator(chat, policy.MaxInputTokens, policy.MaxOutputTokens, policy.Budget)
	if err != nil {
		return snapshot, nil, memories, err
	}
	prepared, err := s.Prepare(ctx, 7, "synthetic-evaluation", "synthetic-learner", turns, generator, policy)
	observe(prepared)
	if err != nil {
		return snapshot, ledger.Calls(), memories, err
	}
	snapshot, err = s.Capture(ctx, 7, "synthetic-evaluation")
	return snapshot, ledger.Calls(), memories, err
}

// RunOfflineProduct invokes the unchanged standard product entry. Its generated
// configuration points exclusively at freshly owned databases and a loopback
// fake model endpoint. There are no replacement product dependencies.
func RunOfflineProduct(ctx context.Context, repository string, snapshot Snapshot, input AttemptInput) (ProductResult, error) {
	return runOfflineEntry(ctx, repository, snapshot, input)
}

type offlineChat struct {
	mu sync.Mutex
	// answers holds the prompt of every answer the fake model gave, in the
	// order it gave them. An attempt that asks several questions keeps one
	// entry per question, so that the context of the later question can be
	// read and compared with the earlier one.
	answers [][]*schema.Message
}

func (m *offlineChat) WithTools([]*schema.ToolInfo) (modelcomponent.ToolCallingChatModel, error) {
	return m, nil
}
func (m *offlineChat) Generate(ctx context.Context, input []*schema.Message, _ ...modelcomponent.Option) (*schema.Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(input) > 0 && strings.HasPrefix(input[0].Content, "You compress an older portion") {
		return schema.AssistantMessage(`{"schema_version":2,"current_goal":"synthetic offline summary"}`, nil), nil
	}
	if len(input) > 0 && strings.HasPrefix(input[0].Content, "You are GopherAI") {
		m.mu.Lock()
		m.answers = append(m.answers, append([]*schema.Message(nil), input...))
		m.mu.Unlock()
		return schema.AssistantMessage("synthetic offline answer", nil), nil
	}
	return nil, fmt.Errorf("offline model has no response for this product role")
}
func (m *offlineChat) Stream(ctx context.Context, input []*schema.Message, opts ...modelcomponent.Option) (*schema.StreamReader[*schema.Message], error) {
	out, err := m.Generate(ctx, input, opts...)
	if err != nil {
		return nil, err
	}
	out.ResponseMeta = &schema.ResponseMeta{FinishReason: "stop"}
	return schema.StreamReaderFromArray([]*schema.Message{out}), nil
}
func (m *offlineChat) answerInput() []*schema.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.answers) == 0 {
		return nil
	}
	return append([]*schema.Message(nil), m.answers[len(m.answers)-1]...)
}

// answerInputs returns the prompt of every answer, in the order the answers
// were given.
func (m *offlineChat) answerInputs() [][]*schema.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([][]*schema.Message, 0, len(m.answers))
	for _, answer := range m.answers {
		out = append(out, append([]*schema.Message(nil), answer...))
	}
	return out
}
