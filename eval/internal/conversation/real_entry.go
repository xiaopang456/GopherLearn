package conversation

// Real-mode evaluation. The measured product keeps its own behaviour: no
// request body is rewritten, no output limit is added, and no product
// dependency is replaced. The only difference from a local run is the address
// of the model endpoints, which point at the operator's counting proxy. That
// proxy injects the provider credentials, enforces the per-lane call caps,
// records the provider usage, and writes the stop file when a chat call is
// refused or the cost watchdog trips. The evaluation therefore never sees a
// provider key and never changes what the product asks for.
//
// The knowledge index is shared by every run of one evaluation and is read
// only during a run: the product's index worker never sees a pending
// document, so no chunk is written while an answer is being produced.

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
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"GopherAI/common/aihelper"
	"GopherAI/common/memoryrecall"
	"GopherAI/config"
	"GopherAI/entry"
	"GopherAI/model"
	agent "GopherAI/service/agent"
	"GopherAI/utils"
	"github.com/cloudwego/eino-ext/components/model/openai"
	"gorm.io/gorm"
)

// RealEnvironment describes where a paid evaluation run finds its model
// endpoints, its shared knowledge index, and its evaluation identity.
type RealEnvironment struct {
	// ProxyBaseURL is the operator's counting proxy, for example
	// http://127.0.0.1:18140. The proxy holds the credentials.
	ProxyBaseURL string
	// RedisAddress is the shared evaluation index, host:port.
	RedisAddress string
	// RedisPassword opens the shared evaluation index. It never appears in
	// an output file, a manifest, or an error message.
	RedisPassword string
	// StorageDir holds the seeded documents of the shared index.
	StorageDir string
	// ChatModel, EmbeddingModel and RerankModel are the provider model
	// names the product is measured with.
	ChatModel      string
	EmbeddingModel string
	RerankModel    string
	// UserID, UserName and SessionID identify the fictional evaluation
	// learner and the conversation under test.
	UserID    int64
	UserName  string
	SessionID string
}

// Validate reports an incomplete paid-run wiring before any resource opens.
func (e RealEnvironment) Validate() error {
	if err := loopbackAddress(e.ProxyBaseURL); err != nil {
		return fmt.Errorf("counting proxy: %w", err)
	}
	if err := loopbackAddress("http://" + e.RedisAddress); err != nil {
		return fmt.Errorf("evaluation index: %w", err)
	}
	if strings.TrimSpace(e.StorageDir) == "" {
		return fmt.Errorf("evaluation storage directory is required")
	}
	if strings.TrimSpace(e.ChatModel) == "" || strings.TrimSpace(e.EmbeddingModel) == "" || strings.TrimSpace(e.RerankModel) == "" {
		return fmt.Errorf("model names are required")
	}
	if e.UserID <= 0 || strings.TrimSpace(e.UserName) == "" || strings.TrimSpace(e.SessionID) == "" {
		return fmt.Errorf("evaluation identity is required")
	}
	return nil
}

// loopbackAddress keeps a paid run away from any host the operator uses.
func loopbackAddress(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "http" {
		return fmt.Errorf("address must be an http URL")
	}
	host, _, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		host = parsed.Host
	}
	if host != "127.0.0.1" && host != "localhost" && host != "::1" {
		return fmt.Errorf("address must be a loopback address")
	}
	return nil
}

// proxyLane binds one product model path to the counting proxy lane that
// carries it and to the upstream path that lane forwards to. The upstream
// paths are the provider paths the product itself uses; nothing is added to
// the request body.
type proxyLane struct {
	lane string
	path string
}

var proxyLanes = map[string]proxyLane{
	"/v1/chat/completions": {lane: "chat", path: "/v1/chat/completions"},
	"/v1/embeddings":       {lane: "embed", path: "/embeddings"},
	"/v1/rerank":           {lane: "rerank", path: "/api/v1/services/rerank/text-rerank/text-rerank"},
}

// proxyForwarder relays a model request to the counting proxy unchanged.
func proxyForwarder(base string) http.Handler {
	client := &http.Client{Timeout: 10 * time.Minute}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		spec, ok := proxyLanes[r.URL.Path]
		if !ok {
			http.Error(w, "unknown model path", http.StatusNotFound)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20+1))
		r.Body.Close()
		if err != nil || len(body) > 8<<20 {
			http.Error(w, "model request body is invalid", http.StatusBadRequest)
			return
		}
		target := strings.TrimSuffix(base, "/") + "/lane/" + spec.lane + spec.path
		request, err := http.NewRequestWithContext(r.Context(), http.MethodPost, target, bytes.NewReader(body))
		if err != nil {
			http.Error(w, "model request could not be built", http.StatusBadGateway)
			return
		}
		request.Header.Set("Content-Type", "application/json")
		if accept := r.Header.Get("Accept"); accept != "" {
			request.Header.Set("Accept", accept)
		}
		response, err := client.Do(request)
		if err != nil {
			http.Error(w, "the counting proxy is unreachable", http.StatusBadGateway)
			return
		}
		defer response.Body.Close()
		for key, values := range response.Header {
			switch strings.ToLower(key) {
			case "content-length", "content-encoding", "transfer-encoding", "connection":
				continue
			}
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(response.StatusCode)
		buffer := make([]byte, 32<<10)
		for {
			read, readErr := response.Body.Read(buffer)
			if read > 0 {
				if _, writeErr := w.Write(buffer[:read]); writeErr != nil {
					return
				}
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
			}
			if readErr != nil {
				return
			}
		}
	})
}

// realWire serves the product's model endpoints on loopback. It records every
// request at the HTTP boundary and forwards it to the counting proxy.
func realWire(ledger *ModelLedger, roles RolePrompts, proxyBase string) *httptest.Server {
	return httptest.NewServer(modelHTTPHandler(ledger, roles, proxyForwarder(proxyBase)))
}

// realSettings points the standard entry at the counting proxy and at the
// shared evaluation index. The product's own defaults are kept.
func realSettings(repository, storage string, env RealEnvironment) (*entry.Settings, error) {
	settings, err := offlineSettings(repository, storage)
	if err != nil {
		return nil, err
	}
	settings.Entry.ChatModel = env.ChatModel
	settings.Entry.ChatKey = "placeholder-proxy-injected"
	settings.Entry.EmbeddingKey = "placeholder-proxy-injected"
	settings.Config.RagModelConfig.RagEmbeddingModel = env.EmbeddingModel
	settings.Config.RagModelConfig.RagRerankModel = env.RerankModel
	settings.Config.RagModelConfig.RagChatModelName = env.ChatModel
	settings.Config.RagModelConfig.RagChatThinkingDialect = config.ChatThinkingDialectDeepSeek
	settings.Config.AgentThinkingEnabled = true
	settings.Config.AgentConfig.EpisodeQuietWindowSeconds = 86400
	settings.Config.RedisPassword = env.RedisPassword
	return settings, settings.Validate()
}

// PrepareRealProductReport warms one conversation with the product's rolling
// summarizer. The summary model is the provider model, reached through the
// counting proxy, so the summaries a run restores are the summaries the
// product would have produced.
func PrepareRealProductReport(ctx context.Context, repository string, turns []Turn, env RealEnvironment) (report WarmupReport, resultErr error) {
	if err := env.Validate(); err != nil {
		return report, err
	}
	if len(turns) == 0 {
		return report, fmt.Errorf("turns are required")
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	storage, err := os.MkdirTemp("", "gl-real-storage-")
	if err != nil {
		return report, err
	}
	defer os.RemoveAll(storage)
	settings, err := realSettings(repository, storage, env)
	if err != nil {
		return report, err
	}
	roles, err := LoadRolePrompts(repository, memoryrecall.ReadMode(settings.Config.MemoryReadMode))
	if err != nil {
		return report, err
	}
	ledger := &ModelLedger{}
	defer func() {
		if !ledger.Complete() {
			resultErr = errors.Join(resultErr, fmt.Errorf("warmup model telemetry incomplete"))
		}
	}()
	wire := realWire(ledger, roles, env.ProxyBaseURL)
	defer wire.Close()
	dialect, err := config.NormalizeChatThinkingDialect(settings.Config.RagModelConfig.RagChatThinkingDialect)
	if err != nil {
		return report, err
	}
	chat, err := openai.NewChatModel(ctx, &openai.ChatModelConfig{
		BaseURL:     wire.URL + "/v1",
		Model:       env.ChatModel,
		APIKey:      "placeholder-proxy-injected",
		ExtraFields: aihelper.ThinkingExtraFields(dialect, false),
	})
	if err != nil {
		return report, err
	}
	s, err := NewSandbox(ctx)
	if err != nil {
		return report, err
	}
	defer func() { resultErr = errors.Join(resultErr, closeSandbox(s)) }()
	if err = s.InitProductSchema(ctx); err != nil {
		return report, err
	}
	password, err := randomHex(24)
	if err != nil {
		return report, err
	}
	hash, err := utils.HashPassword(password)
	if err != nil {
		return report, err
	}
	if err = s.SeedProduct(ctx, model.User{ID: env.UserID, Username: env.UserName, Password: hash}, nil, env.StorageDir); err != nil {
		return report, err
	}
	if err = (guardedStore{s}).with(ctx, func(tx *gorm.DB) error {
		return tx.Create(&model.Session{ID: env.SessionID, UserID: env.UserID, UserName: env.UserName, Mode: model.SessionModeAgent, Title: "Evaluation conversation"}).Error
	}); err != nil {
		return report, err
	}
	policy := ProductSummaryConfig(settings.Config)
	generator, err := agent.NewModelSummaryGenerator(chat, policy.MaxInputTokens, policy.MaxOutputTokens)
	if err != nil {
		return report, err
	}
	prepared, err := s.Prepare(ctx, env.UserID, env.SessionID, env.UserName, turns, generator, policy)
	for _, turn := range prepared {
		if turn.SummaryDegraded {
			report.DegradedTurns++
		}
		if turn.CooldownWaitMilliseconds > 0 {
			report.CooldownWaits++
		}
	}
	report.FailureCooldownSeconds = int(policy.FailureCooldown / time.Second)
	report.Turns = prepared
	report.Calls = ledger.Calls()
	if err != nil {
		return report, err
	}
	report.Snapshot, err = s.Capture(ctx, env.UserID, env.SessionID)
	if err != nil {
		return report, err
	}
	report.UsageByRole, err = SummarizeCalls(report.Calls)
	return report, errors.Join(err, nil)
}

// RunRealProduct restores one prepared conversation into a fresh evaluation
// database and asks the unchanged standard product entry.
func RunRealProduct(ctx context.Context, repository string, snapshot Snapshot, question string, env RealEnvironment, seed *KnowledgeSeed) (ProductResult, error) {
	return runRealEntry(ctx, repository, snapshot, question, env, seed)
}

func runRealEntry(ctx context.Context, repository string, snapshot Snapshot, question string, env RealEnvironment, seed *KnowledgeSeed) (result ProductResult, resultErr error) {
	result.Scope = ProductScope
	result.Environment = "real"
	result.Entry = "gopherlearn-integrated:hybrid.Run"
	result.QuietWindowSeconds = 86400
	defer func() {
		if len(result.Response) > 0 && !json.Valid(result.Response) {
			result.ResponseText = string(result.Response)
			result.Response = nil
		}
		if resultErr != nil {
			result.BackgroundAudit.Valid = false
			result.BackgroundAudit.Reason = resultErr.Error()
			result.Error = resultErr.Error()
		}
	}()
	if err := env.Validate(); err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if strings.TrimSpace(question) == "" {
		return result, fmt.Errorf("question required")
	}
	if err := snapshot.Validate(); err != nil {
		return result, err
	}
	if snapshot.Session.UserID != env.UserID || snapshot.Session.UserName != env.UserName || snapshot.Session.ID != env.SessionID {
		return result, fmt.Errorf("the snapshot belongs to another evaluation identity")
	}
	dir, err := os.MkdirTemp("", "gl-real-entry-")
	if err != nil {
		return result, err
	}
	defer os.RemoveAll(dir)
	settings, err := realSettings(repository, env.StorageDir, env)
	if err != nil {
		return result, err
	}
	result.Provenance, err = collectProvenance(ctx, repository, settings, snapshot)
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
	password, err := randomHex(24)
	if err != nil {
		return result, err
	}
	hash, err := utils.HashPassword(password)
	if err != nil {
		return result, err
	}
	if err = s.SeedProduct(ctx, model.User{ID: env.UserID, Username: env.UserName, Password: hash}, seed.documents(), env.StorageDir); err != nil {
		return result, err
	}
	restoreStart := time.Now()
	if err = s.Restore(ctx, snapshot); err != nil {
		return result, err
	}
	result.RestoreMilliseconds = time.Since(restoreStart).Milliseconds()
	journal, err := s.StartJobJournal(ctx)
	if err != nil {
		return result, err
	}
	ledger := &ModelLedger{}
	roles, err := LoadRolePrompts(repository, memoryrecall.ReadMode(settings.Config.MemoryReadMode))
	if err != nil {
		return result, err
	}
	wire := realWire(ledger, roles, env.ProxyBaseURL)
	defer wire.Close()
	connection, err := s.ConnectionInfo(ctx)
	if err != nil {
		return result, err
	}
	settings.Config.MysqlHost = connection.Host
	settings.Config.MysqlPort, err = strconv.Atoi(connection.Port)
	if err != nil {
		return result, err
	}
	settings.Config.MysqlDatabaseName = connection.Database
	settings.Config.MysqlUser = connection.User
	settings.Config.MysqlPassword = connection.Password
	settings.Config.RedisHost, settings.Config.RedisPort, err = splitRedisAddress(env.RedisAddress)
	if err != nil {
		return result, err
	}
	settings.Entry.ChatBaseURL = wire.URL + "/v1"
	settings.Config.RagRerankEndpoint = wire.URL + "/v1/rerank"
	settings.Config.RagBaseUrl = wire.URL + "/v1"
	settings.Config.RagEmbeddingBaseURL = wire.URL + "/v1"
	settings.Config.JwtConfig.Key, err = randomHex(32)
	if err != nil {
		return result, err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return result, err
	}
	address := listener.Addr().(*net.TCPAddr)
	settings.Config.MainConfig.Host = "127.0.0.1"
	settings.Config.MainConfig.Port = address.Port
	listener.Close()
	path := filepath.Join(dir, "product.toml")
	oldToken, hadToken := os.LookupEnv("RAG_RERANK_AUTH_TOKEN")
	if err = os.Setenv("RAG_RERANK_AUTH_TOKEN", settings.Config.RagRerankToken); err != nil {
		return result, err
	}
	defer func() {
		if hadToken {
			os.Setenv("RAG_RERANK_AUTH_TOKEN", oldToken)
		} else {
			os.Unsetenv("RAG_RERANK_AUTH_TOKEN")
		}
	}()
	if err = writeEntryConfig(path, settings, wire.URL); err != nil {
		return result, err
	}
	loaded, err := entry.Load(path)
	if err != nil {
		return result, err
	}
	if loaded.Config.EpisodeQuietWindowSeconds != 86400 || loaded.Entry.Migrate || !loaded.Config.RagRerankEnabled || loaded.Config.RagRerankEndpoint != wire.URL+"/v1/rerank" {
		return result, fmt.Errorf("unexpected standard entry settings")
	}
	result.Audit.Started = time.Now().UTC()
	product := startStandardProduct(path)
	defer func() { resultErr = errors.Join(resultErr, product.stop()) }()
	client := &http.Client{Timeout: 5 * time.Minute, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	base := "http://" + settings.Address()
	token, err := loginStandardProduct(ctx, product, client, base, env.UserName, password)
	if err != nil {
		return result, err
	}
	result.Audit.Before, result.BeforeCounts, err = s.ObserveJobs(ctx)
	if err != nil {
		return result, err
	}
	body, _ := json.Marshal(map[string]any{"sessionId": snapshot.Session.ID, "requestId": "evaluation-final-question", "question": question, "knowledgePolicy": "knowledge_preferred", "enableThinking": true})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/v1/AI/agent/run", bytes.NewReader(body))
	if err != nil {
		return result, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	questionStart := time.Now()
	response, requestErr := client.Do(request)
	if requestErr == nil {
		result.Response, requestErr = io.ReadAll(io.LimitReader(response.Body, 4<<20+1))
		response.Body.Close()
		if len(result.Response) > 4<<20 {
			requestErr = fmt.Errorf("product response exceeds limit")
			result.Response = nil
		}
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
	result.QuestionMilliseconds = time.Since(questionStart).Milliseconds()
	collect, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	barrierErr := journal.AnswerBarrier(collect)
	result.Audit.After, result.AfterCounts, err = s.ObserveJobs(collect)
	stopErr := product.stop()
	result.ShutdownComplete = stopErr == nil
	wire.Close()
	var stoppedErr error
	_, result.StoppedCounts, stoppedErr = s.ObserveJobs(collect)
	result.Events, result.Audit.ExecutedBeforeAnswer, resultErr = journal.Finish(collect)
	result.Calls = AttributeCalls(ledger.Calls(), result.Audit.Answered)
	var usageErr error
	result.UsageByRole, usageErr = SummarizeCalls(result.Calls)
	for _, c := range result.Calls {
		result.Audit.Calls = append(result.Audit.Calls, BackgroundCall{Role: c.Role, Started: c.Started})
	}
	result.Audit.Complete = resultErr == nil && barrierErr == nil && stopErr == nil && stoppedErr == nil && err == nil && usageErr == nil && ledger.Complete()
	resultErr = errors.Join(resultErr, requestErr, barrierErr, err, stopErr, stoppedErr, usageErr, result.Audit.ValidateBackground())
	result.BackgroundAudit.Valid = resultErr == nil
	return result, resultErr
}

func splitRedisAddress(address string) (string, int, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", 0, err
	}
	number, err := strconv.Atoi(port)
	if err != nil {
		return "", 0, fmt.Errorf("evaluation index port is invalid")
	}
	return host, number, nil
}
