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
	BeforeCounts    map[string]JobCounts `json:"before_counts"`
	AfterCounts     map[string]JobCounts `json:"after_counts"`
	StoppedCounts   map[string]JobCounts `json:"stopped_counts"`
	Events          []JobEvent           `json:"job_events"`
	Calls           []ModelCall          `json:"model_calls"`
	UsageByRole     map[string]RoleUsage `json:"usage_by_role"`
	Error           string               `json:"error,omitempty"`
	Entry           string               `json:"entry"`
	ContextMessages []*schema.Message    `json:"context_messages"`
	Audit           BackgroundAudit      `json:"background_evidence"`
	BackgroundAudit struct {
		Valid  bool   `json:"valid"`
		Reason string `json:"reason,omitempty"`
	} `json:"backgroundAudit"`
	QuietWindowSeconds   int   `json:"quiet_window_seconds"`
	RestoreMilliseconds  int64 `json:"restore_ms"`
	ShutdownComplete     bool  `json:"shutdown_complete"`
	QuestionMilliseconds int64 `json:"question_ms"`
}

// ProductSummaryConfig mirrors the production wiring, including its context
// summary output limit rather than the standalone summarizer test defaults.
func ProductSummaryConfig(c *config.Config) agent.SummaryConfig {
	a := c.AgentConfig
	return agent.SummaryConfig{TriggerMessages: a.SummaryTriggerMessages, TriggerTokens: a.SummaryTriggerTokens, RetainMessages: a.SummaryRetainMessages, MaxInputTokens: a.SummaryMaxInputTokens, MaxOutputTokens: a.ContextMaxSummaryTokens, FetchLimit: a.SummaryFetchLimit, Timeout: time.Duration(a.SummaryTimeoutSeconds) * time.Second, StageBudget: time.Duration(a.SummaryStageBudgetSeconds) * time.Second, MaxDeadlineShare: agent.DefaultSummaryConfig().MaxDeadlineShare, FailureCooldown: time.Duration(a.SummaryFailureCooldownSeconds) * time.Second}
}

func offlineSettings(repository, storage string) (*entry.Settings, error) {
	data, err := os.ReadFile(filepath.Join(repository, "tutorial", "v2-entry", "isolated.example.toml"))
	if err != nil {
		return nil, err
	}
	text := strings.ReplaceAll(string(data), `key = ""`, `key = "synthetic-offline-key-with-at-least-32-characters"`)
	text = strings.ReplaceAll(text, `chatKey = ""`, `chatKey = "synthetic-never-sent"`)
	text = strings.ReplaceAll(text, `embeddingKey = ""`, `embeddingKey = "synthetic-never-sent"`)
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
	// Reranking has its own HTTP transport, outside the assembly dependency seam.
	// Offline mode explicitly refuses it rather than leaking a provider request.
	if settings.Config.RagModelConfig.RagRerankEnabled {
		return nil, fmt.Errorf("offline runner requires reranking disabled")
	}
	return settings, settings.Validate()
}

func closeSandbox(s *Sandbox) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return s.Close(ctx)
}

// PrepareOfflineProduct uses a deterministic fake model behind the real summary
// generator and rolling provider. There is no online/provider credential path.
func PrepareOfflineProduct(ctx context.Context, repository string, turns []Turn) (snapshot Snapshot, calls []ModelCall, resultErr error) {
	storage, err := os.MkdirTemp("", "gl-offline-storage-")
	if err != nil {
		return snapshot, nil, err
	}
	defer os.RemoveAll(storage)
	settings, err := offlineSettings(repository, storage)
	if err != nil {
		return snapshot, nil, err
	}
	s, err := NewSandbox(ctx)
	if err != nil {
		return snapshot, nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, closeSandbox(s)) }()
	if err = s.InitProductSchema(ctx); err != nil {
		return snapshot, nil, err
	}
	if err = s.SeedProduct(ctx, model.User{ID: 7, Username: "synthetic-learner"}, nil, storage); err != nil {
		return snapshot, nil, err
	}
	if err = (guardedStore{s}).with(ctx, func(tx *gorm.DB) error {
		return tx.Create(&model.Session{ID: "synthetic-evaluation", UserID: 7, UserName: "synthetic-learner", Mode: model.SessionModeAgent, Title: "Synthetic evaluation"}).Error
	}); err != nil {
		return snapshot, nil, err
	}
	ledger := &ModelLedger{}
	roles, err := LoadRolePrompts(repository, memoryrecall.ReadMode(settings.Config.AgentConfig.MemoryReadMode))
	if err != nil {
		return snapshot, nil, err
	}
	chat, err := WrapModel(&offlineChat{}, ledger, roles)
	if err != nil {
		return snapshot, nil, err
	}
	policy := ProductSummaryConfig(settings.Config)
	generator, err := agent.NewModelSummaryGenerator(chat, policy.MaxInputTokens, policy.MaxOutputTokens)
	if err != nil {
		return snapshot, nil, err
	}
	if _, err = s.Prepare(ctx, 7, "synthetic-evaluation", "synthetic-learner", turns, generator, policy); err != nil {
		return snapshot, ledger.Calls(), err
	}
	snapshot, err = s.Capture(ctx, 7, "synthetic-evaluation")
	return snapshot, ledger.Calls(), err
}

// RunOfflineProduct invokes the unchanged standard product entry. Its generated
// configuration points exclusively at freshly owned databases and a loopback
// fake model endpoint. There are no replacement product dependencies.
func RunOfflineProduct(ctx context.Context, repository string, snapshot Snapshot, question string) (ProductResult, error) {
	return runOfflineEntry(ctx, repository, snapshot, question)
}

type offlineChat struct {
	mu     sync.Mutex
	answer []*schema.Message
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
		m.answer = append([]*schema.Message(nil), input...)
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
	return append([]*schema.Message(nil), m.answer...)
}
