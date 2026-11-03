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
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"GopherAI/common/memoryrecall"
	"GopherAI/config"
	"GopherAI/entry"
	"GopherAI/model"
	"GopherAI/utils"
	"github.com/BurntSushi/toml"
	hybrid "gopherlearn-v2-hybrid"
)

func writeEntryConfig(path string, s *entry.Settings, endpoint string) error {
	envelope := struct {
		*config.Config
		Entry    any               `toml:"isolatedEntry"`
		Ordinary map[string]string `toml:"ordinaryChat"`
	}{s.Config, s.Entry, map[string]string{
		"GOPHERLEARN_AI_PROVIDER":     "openai_compatible",
		"GOPHERLEARN_OPENAI_BASE_URL": endpoint + "/v1",
		"GOPHERLEARN_OPENAI_API_KEY":  "synthetic-never-sent-upstream",
		"GOPHERLEARN_OPENAI_MODEL":    "synthetic-offline",
	}}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	return errors.Join(toml.NewEncoder(file).Encode(envelope), file.Close())
}

// serveStandardProduct calls exactly the Run used by cmd/gopherlearn-integrated.
// It adds neither a custom product assembly nor alternate worker configuration.
type standardProduct struct {
	cancel  context.CancelFunc
	done    chan error
	stopped bool
}

func startStandardProduct(path string) *standardProduct {
	ctx, cancel := context.WithCancel(context.Background())
	p := &standardProduct{cancel: cancel, done: make(chan error, 1)}
	go func() { p.done <- hybrid.Run(ctx, path) }()
	return p
}
func (p *standardProduct) stop() error {
	if p.stopped {
		return nil
	}
	p.stopped = true
	p.cancel()
	select {
	case err := <-p.done:
		if err != nil {
			return fmt.Errorf("standard product entry failed")
		}
		return nil
	case <-time.After(35 * time.Second):
		return fmt.Errorf("standard product shutdown incomplete")
	}
}

func loginStandardProduct(ctx context.Context, p *standardProduct, client *http.Client, base, user, password string) (string, error) {
	body, _ := json.Marshal(map[string]string{"username": user, "password": password})
	for {
		select {
		case <-p.done:
			p.cancel()
			p.stopped = true
			return "", fmt.Errorf("standard product failed before readiness")
		default:
		}
		attempt, cancel := context.WithTimeout(ctx, time.Second)
		request, err := http.NewRequestWithContext(attempt, http.MethodPost, base+"/api/v1/user/login", bytes.NewReader(body))
		if err != nil {
			cancel()
			return "", err
		}
		request.Header.Set("Content-Type", "application/json")
		response, err := client.Do(request)
		var token string
		if err == nil {
			var result struct {
				Token string `json:"token"`
			}
			if response.StatusCode == 200 && json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result) == nil {
				token = result.Token
			}
			response.Body.Close()
		}
		cancel()
		if token != "" {
			return token, nil
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("standard product readiness timed out")
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func runOfflineEntry(ctx context.Context, repository string, snapshot Snapshot, question string) (result ProductResult, resultErr error) {
	result.Scope = ProductScope
	result.Environment = "fake"
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
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if strings.TrimSpace(question) == "" {
		return result, fmt.Errorf("question required")
	}
	if err := snapshot.Validate(); err != nil {
		return result, err
	}
	dir, err := os.MkdirTemp("", "gl-standard-offline-")
	if err != nil {
		return result, err
	}
	defer os.RemoveAll(dir)
	settings, err := offlineSettings(repository, filepath.Join(dir, "knowledge"))
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
	if err = s.SeedProduct(ctx, model.User{ID: snapshot.Session.UserID, Username: snapshot.Session.UserName, Password: hash}, nil, settings.Config.StorageDir); err != nil {
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
	redis, err := newRedisSandbox(ctx)
	if err != nil {
		return result, err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		resultErr = errors.Join(resultErr, redis.Close(cleanup))
	}()
	ledger := &ModelLedger{}
	roles, err := LoadRolePrompts(repository, memoryrecall.ReadMode(settings.Config.MemoryReadMode))
	if err != nil {
		return result, err
	}
	stub := &offlineChat{}
	wire := offlineWire(ledger, roles, stub)
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
	settings.Config.RedisHost, connection.Port, err = net.SplitHostPort(redis.addr)
	if err != nil {
		return result, err
	}
	settings.Config.RedisPort, err = strconv.Atoi(connection.Port)
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
	// The existing configuration intentionally reads this one credential only
	// from the process environment. Use a fake token for the loopback service.
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
	// Verify serialized settings before reaching any production resource factory.
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
	client := &http.Client{Timeout: time.Minute, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	base := "http://" + settings.Address()
	token, err := loginStandardProduct(ctx, product, client, base, snapshot.Session.UserName, password)
	if err != nil {
		return result, err
	}
	result.Audit.Before, result.BeforeCounts, err = s.ObserveJobs(ctx)
	if err != nil {
		return result, err
	}
	body, _ := json.Marshal(map[string]any{"sessionId": snapshot.Session.ID, "requestId": "offline-final-question", "question": question, "knowledgePolicy": "knowledge_preferred", "enableThinking": true})
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
	result.ContextMessages = stub.answerInput()
	for _, c := range result.Calls {
		result.Audit.Calls = append(result.Audit.Calls, BackgroundCall{Role: c.Role, Started: c.Started})
	}
	result.Audit.Complete = resultErr == nil && barrierErr == nil && stopErr == nil && stoppedErr == nil && err == nil && usageErr == nil && ledger.Complete()
	resultErr = errors.Join(resultErr, requestErr, barrierErr, err, stopErr, stoppedErr, usageErr, result.Audit.ValidateBackground())
	result.BackgroundAudit.Valid = resultErr == nil
	return result, resultErr
}
