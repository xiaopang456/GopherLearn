package conversation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"GopherAI/common/memoryrecall"
	"GopherAI/common/rag"
	"GopherAI/config"
	"GopherAI/entry"
	"GopherAI/model"
	agent "GopherAI/service/agent"
	"GopherAI/utils/myjwt"
	"github.com/cloudwego/eino/components/embedding"
	modelcomponent "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	redisclient "github.com/redis/go-redis/v9"
	ordinary "github.com/xiaopang456/GopherLearn/conversation"
	hybrid "gopherlearn-v2-hybrid"
	"gorm.io/gorm"
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
	Events          []JobEvent           `json:"job_events"`
	Calls           []ModelCall          `json:"model_calls"`
	ContextMessages []*schema.Message    `json:"context_messages"`
	Audit           BackgroundAudit      `json:"background_evidence"`
	BackgroundAudit struct {
		Valid  bool   `json:"valid"`
		Reason string `json:"reason,omitempty"`
	} `json:"backgroundAudit"`
	QuietWindowSeconds  int   `json:"quiet_window_seconds"`
	RestoreMilliseconds int64 `json:"restore_ms"`
	ShutdownComplete    bool  `json:"shutdown_complete"`
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

// RunOfflineProduct restores into a new MySQL instance, starts the complete
// hybrid product (including its original workers), asks one authenticated HTTP
// question, and stops it before finalizing the transactional task/model audit.
// Only network model/vector transports are fake. It cannot call paid models.
func RunOfflineProduct(ctx context.Context, repository string, snapshot Snapshot, question string) (result ProductResult, resultErr error) {
	defer func() {
		if resultErr != nil {
			result.BackgroundAudit.Valid = false
			result.BackgroundAudit.Reason = resultErr.Error()
		}
	}()
	result.Scope = ProductScope
	result.Environment = "fake"
	result.QuietWindowSeconds = 86400
	if strings.TrimSpace(question) == "" {
		return result, fmt.Errorf("question required")
	}
	if err := snapshot.Validate(); err != nil {
		return result, err
	}
	storage, err := os.MkdirTemp("", "gl-offline-storage-")
	if err != nil {
		return result, err
	}
	defer os.RemoveAll(storage)
	settings, err := offlineSettings(repository, storage)
	if err != nil {
		return result, err
	}
	s, err := NewSandbox(ctx)
	if err != nil {
		return result, err
	}
	defer func() { resultErr = errors.Join(resultErr, closeSandbox(s)) }()
	if err = s.InitProductSchema(ctx); err != nil {
		return result, err
	}
	if err = s.SeedProduct(ctx, model.User{ID: snapshot.Session.UserID, Username: snapshot.Session.UserName}, nil, storage); err != nil {
		return result, err
	}
	start := time.Now()
	if err = s.Restore(ctx, snapshot); err != nil {
		return result, err
	}
	result.RestoreMilliseconds = time.Since(start).Milliseconds()
	journal, err := s.StartJobJournal(ctx)
	if err != nil {
		return result, err
	}
	ledger := &ModelLedger{}
	roles, err := LoadRolePrompts(repository, memoryrecall.ReadMode(settings.Config.AgentConfig.MemoryReadMode))
	if err != nil {
		return result, err
	}
	stub := &offlineChat{}
	chat, err := WrapModel(stub, ledger, roles)
	if err != nil {
		return result, err
	}
	c, err := s.ConnectionInfo(ctx)
	if err != nil {
		return result, err
	}
	// Populate only connection fields from the newly created owned instance.
	settings.Config.MysqlHost = c.Host
	fmt.Sscan(c.Port, &settings.Config.MysqlPort)
	settings.Config.MysqlDatabaseName = c.Database
	settings.Config.MysqlUser = c.User
	settings.Config.MysqlPassword = c.Password
	deps := entry.ProductionDependencies(settings)
	deps.Infrastructure.OpenDatabase = func(ctx context.Context, _ *config.Config) (*gorm.DB, error) { return s.openProductDB(ctx) }
	deps.Infrastructure.OpenRedis = func(*config.Config) (*redisclient.Client, error) {
		client := redisclient.NewClient(&redisclient.Options{Addr: "offline.invalid:6379", MaxRetries: -1})
		client.AddHook(offlineRedis{})
		return client, nil
	}
	deps.ChatModel = func(context.Context, *entry.Settings) (modelcomponent.ToolCallingChatModel, error) { return chat, nil }
	deps.Embedder = func(context.Context, *config.Config) (embedding.Embedder, error) { return offlineEmbedder{ledger}, nil }
	deps.Chunks = func(*redisclient.Client, int) (entry.ChunkStore, error) { return offlineChunks{}, nil }
	result.Audit.Started = time.Now().UTC()
	app, err := hybrid.Assemble(context.Background(), settings, deps, offlineOrdinary{ledger}, ordinary.Options{})
	if err != nil {
		return result, err
	}
	closed := false
	defer func() {
		if !closed {
			resultErr = errors.Join(resultErr, app.Close())
		}
	}()
	server := httptest.NewServer(app.Handler)
	defer server.Close()
	result.Audit.Before, result.BeforeCounts, err = s.ObserveJobs(ctx)
	if err != nil {
		return result, err
	}
	token, err := myjwt.GenerateToken(snapshot.Session.UserID, snapshot.Session.UserName)
	if err != nil {
		return result, err
	}
	body, _ := json.Marshal(map[string]any{"sessionId": snapshot.Session.ID, "requestId": "offline-final-question", "question": question, "knowledgePolicy": "knowledge_preferred"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/api/v1/AI/agent/run", bytes.NewReader(body))
	if err != nil {
		return result, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	response, requestErr := server.Client().Do(req)
	if requestErr == nil {
		result.Response, requestErr = io.ReadAll(io.LimitReader(response.Body, 4<<20))
		response.Body.Close()
		if requestErr == nil && response.StatusCode != 200 {
			requestErr = fmt.Errorf("product HTTP status %d", response.StatusCode)
		}
		var envelope struct {
			Answer string `json:"answer"`
		}
		if requestErr == nil {
			requestErr = json.Unmarshal(result.Response, &envelope)
			result.Answer = envelope.Answer
			if result.Answer == "" {
				requestErr = fmt.Errorf("product response has no answer")
			}
		}
	}
	result.Audit.Answered = time.Now().UTC()
	// Collection uses a fresh bounded context even after a request timeout.
	collect, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	barrierErr := journal.AnswerBarrier(collect)
	result.Audit.After, result.AfterCounts, err = s.ObserveJobs(collect)
	server.Close()
	stopErr := app.Close()
	closed = true
	result.ShutdownComplete = stopErr == nil
	result.Events, result.Audit.ExecutedBeforeAnswer, resultErr = journal.Finish(collect)
	result.Calls = ledger.Calls()
	result.ContextMessages = stub.answerInput()
	for _, call := range result.Calls {
		result.Audit.Calls = append(result.Audit.Calls, BackgroundCall{Role: call.Role, Started: call.Started})
	}
	result.Audit.Complete = resultErr == nil && barrierErr == nil && stopErr == nil && err == nil && ledger.Complete()
	auditErr := result.Audit.ValidateBackground()
	resultErr = errors.Join(resultErr, requestErr, barrierErr, err, stopErr, auditErr)
	result.BackgroundAudit.Valid = resultErr == nil
	if resultErr != nil {
		result.BackgroundAudit.Reason = resultErr.Error()
	}
	return result, resultErr
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

type offlineEmbedder struct{ ledger *ModelLedger }

func (e offlineEmbedder) EmbedStrings(ctx context.Context, input []string, _ ...embedding.Option) ([][]float64, error) {
	id := e.ledger.begin("embedding", "embed", input, "")
	defer func() { e.ledger.finish(id, ctx.Err()) }()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make([][]float64, len(input))
	for i := range out {
		out[i] = make([]float64, 1024)
	}
	return out, nil
}

type offlineOrdinary struct{ ledger *ModelLedger }

func (o offlineOrdinary) Complete(context.Context, ordinary.Request) (ordinary.Completion, error) {
	id := o.ledger.begin("unknown", "ordinary", nil, "")
	err := fmt.Errorf("ordinary model is outside this evaluation")
	o.ledger.finish(id, err)
	return ordinary.Completion{}, err
}
func (o offlineOrdinary) Stream(ctx context.Context, r ordinary.Request, _ ordinary.DeltaHandler) (ordinary.Completion, error) {
	return o.Complete(ctx, r)
}

type offlineChunks struct{}

func (offlineChunks) EnsureIndex(context.Context) error { return nil }
func (offlineChunks) Store(context.Context, []rag.EmbeddedChunk) error {
	return fmt.Errorf("offline vector writes refused")
}
func (offlineChunks) LoadDocumentVersionEmbeddings(context.Context, int64, string, int) ([]rag.StoredChunkEmbedding, error) {
	return nil, nil
}
func (offlineChunks) SearchActiveByUserVector(context.Context, int64, []float32, int, map[string]int) ([]rag.RedisChunkSearchResult, error) {
	return nil, nil
}
func (offlineChunks) SearchActiveByUserText(context.Context, int64, string, int, map[string]int) ([]rag.RedisChunkSearchResult, error) {
	return nil, nil
}
func (offlineChunks) DeleteDocumentVersion(context.Context, int64, string, int) error {
	return fmt.Errorf("offline vector deletion refused")
}

type offlineRedis struct{}

func (offlineRedis) DialHook(redisclient.DialHook) redisclient.DialHook {
	return func(context.Context, string, string) (net.Conn, error) {
		return nil, fmt.Errorf("offline Redis dialing refused")
	}
}
func (offlineRedis) ProcessHook(redisclient.ProcessHook) redisclient.ProcessHook {
	return func(_ context.Context, cmd redisclient.Cmder) error {
		if cmd.Name() == "ft.info" {
			err := fmt.Errorf("unknown index name")
			cmd.SetErr(err)
			return err
		}
		switch c := cmd.(type) {
		case *redisclient.StatusCmd:
			c.SetVal("OK")
		case *redisclient.Cmd:
			c.SetVal([]any{})
		case *redisclient.StringSliceCmd:
			c.SetVal([]string{})
		default:
			err := fmt.Errorf("unsupported offline Redis operation")
			cmd.SetErr(err)
			return err
		}
		return nil
	}
}
func (h offlineRedis) ProcessPipelineHook(redisclient.ProcessPipelineHook) redisclient.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redisclient.Cmder) error {
		for _, cmd := range cmds {
			if err := h.ProcessHook(nil)(ctx, cmd); err != nil {
				return err
			}
		}
		return nil
	}
}
