package hybrid

import (
	"GopherAI/config"
	"GopherAI/entry"
	"GopherAI/model"
	"GopherAI/utils"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/cloudwego/eino/schema"
	redisclient "github.com/redis/go-redis/v9"
	"github.com/xiaopang456/GopherLearn/conversation"
	"gorm.io/gorm"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func entryConfigFile(t *testing.T, modify func(string) string) string {
	t.Helper()
	raw, err := os.ReadFile("../tutorial/v2-entry/isolated.example.toml")
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ReplaceAll(string(raw), `key = ""`, `key = "synthetic-isolated-test-key-at-least-32-characters"`)
	text = strings.ReplaceAll(text, `chatKey = ""`, `chatKey = "synthetic-provider-never-sent"`)
	text = strings.ReplaceAll(text, `embeddingKey = ""`, `embeddingKey = "synthetic-embedding-never-sent"`)
	text = strings.ReplaceAll(text, `storageDir = "data/v2-isolated/knowledge"`, fmt.Sprintf("storageDir = %q", filepath.ToSlash(t.TempDir())))
	text += `
[ordinaryChat]
GOPHERLEARN_AI_PROVIDER = "openai_compatible"
GOPHERLEARN_OPENAI_BASE_URL = "https://ordinary.invalid/v1"
GOPHERLEARN_OPENAI_API_KEY = "synthetic-ordinary-never-sent"
GOPHERLEARN_OPENAI_MODEL = "ordinary-model"
`
	if modify != nil {
		text = modify(text)
	}
	path := filepath.Join(t.TempDir(), "hybrid.toml")
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestHybridEntryInvalidConfigurationPrecedesFactories(t *testing.T) {
	for _, kind := range []string{"missing", "syntax", "migration", "ordinary-missing", "ordinary-invalid", "ordinary-agent-key"} {
		t.Run(kind, func(t *testing.T) {
			path := ""
			if kind != "missing" {
				path = entryConfigFile(t, func(text string) string {
					switch kind {
					case "syntax":
						return text + "\n[broken"
					case "migration":
						return strings.Replace(text, "migrate = false", "migrate = true", 1)
					case "ordinary-missing":
						return strings.Split(text, "[ordinaryChat]")[0]
					case "ordinary-invalid":
						return text + "\nGOPHERLEARN_OPENAI_TIMEOUT = \"invalid\""
					case "ordinary-agent-key":
						return text + "\nGOPHERLEARN_AGENT_MAX_STEPS = \"1\""
					}
					return text
				})
			}
			called := false
			r := entryRuntime{dependencies: func(*entry.Settings) entry.Dependencies { called = true; return entry.Dependencies{} }, client: func(conversation.ClientConfig) (conversation.Model, error) {
				called = true
				return &ordinaryModel{}, nil
			}, listen: func(string, string) (net.Listener, error) { called = true; return nil, nil }, server: entry.NewHTTPServer, shutdownTimeout: time.Second}
			if err := runEntry(context.Background(), path, r); err == nil || called {
				t.Fatalf("invalid config reached factory: %v called=%v", err, called)
			}
		})
	}
}

func entryRuntimeFixture(t *testing.T, chat conversation.Model) (entryRuntime, *gorm.DB, *scriptedChatModel, *int, *int) {
	t.Helper()
	db := sqliteDatabase(t)
	t.Cleanup(func() { sqlDB, _ := db.DB(); _ = sqlDB.Close() })
	agent := &scriptedChatModel{respond: func(context.Context, int) (*schema.Message, error) {
		return schema.AssistantMessage("agent response", nil), nil
	}}
	migrations, opens, redisOpens := 0, 0, 0
	deps := dependencies(t, db, agent, &testChunks{}, &migrations)
	openDB, openRedis := deps.Infrastructure.OpenDatabase, deps.Infrastructure.OpenRedis
	deps.Infrastructure.OpenDatabase = func(ctx context.Context, c *config.Config) (*gorm.DB, error) { opens++; return openDB(ctx, c) }
	deps.Infrastructure.OpenRedis = func(c *config.Config) (*redisclient.Client, error) { redisOpens++; return openRedis(c) }
	r := entryRuntime{dependencies: func(*entry.Settings) entry.Dependencies { return deps }, client: func(c conversation.ClientConfig) (conversation.Model, error) {
		if c.Model != "ordinary-model" || c.Timeout != 30*time.Second || c.MaxOutputTokens != 1024 || c.EnableThinking != nil {
			t.Errorf("ordinary client defaults drifted")
		}
		return chat, nil
	}, listen: net.Listen, server: entry.NewHTTPServer, shutdownTimeout: time.Second}
	return r, db, agent, &opens, &redisOpens
}

func TestHybridEntryFailuresReleaseOriginalResources(t *testing.T) {
	for _, stage := range []string{"model", "assembly", "listen"} {
		t.Run(stage, func(t *testing.T) {
			r, db, _, opens, _ := entryRuntimeFixture(t, &ordinaryModel{})
			failure := errors.New("synthetic entry failure")
			switch stage {
			case "model":
				r.client = func(conversation.ClientConfig) (conversation.Model, error) { return nil, failure }
			case "assembly":
				previous := r.dependencies
				r.dependencies = func(s *entry.Settings) entry.Dependencies {
					d := previous(s)
					d.Infrastructure.OpenRedis = func(*config.Config) (*redisclient.Client, error) { return nil, failure }
					return d
				}
			case "listen":
				r.listen = func(string, string) (net.Listener, error) { return nil, failure }
			}
			if err := runEntry(context.Background(), entryConfigFile(t, nil), r); !errors.Is(err, failure) {
				t.Fatalf("failure lost: %v", err)
			}
			if stage == "model" {
				if *opens != 0 {
					t.Fatal("model failure opened database")
				}
				return
			}
			if *opens != 1 {
				t.Fatal("duplicate database open")
			}
			sqlDB, err := db.DB()
			if err != nil {
				t.Fatal(err)
			}
			if err := sqlDB.PingContext(context.Background()); err == nil {
				t.Fatal("database left open after failure")
			}
		})
	}
}

type failingCloseListener struct {
	net.Listener
	failure error
}

func (l failingCloseListener) Close() error {
	return errors.Join(l.Listener.Close(), l.failure)
}

func TestHybridEntryShutdownPreservesCloseError(t *testing.T) {
	r, db, _, _, _ := entryRuntimeFixture(t, &ordinaryModel{})
	failure := errors.New("synthetic listener close failure")
	listening := make(chan string, 1)
	r.listen = func(network, address string) (net.Listener, error) {
		listener, err := net.Listen(network, "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		listening <- "http://" + listener.Addr().String()
		return failingCloseListener{listener, failure}, nil
	}
	path := entryConfigFile(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runEntry(ctx, path, r) }()
	var base string
	select {
	case base = <-listening:
	case err := <-done:
		t.Fatalf("entry stopped: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("no listener")
	}
	status, _, _ := socketJSON(t, base, "GET", "/unregistered-entry-check", "", "")
	if status != http.StatusNotFound {
		t.Fatal("actual handler not running")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, failure) {
			t.Fatalf("close error swallowed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("unbounded close failure")
	}
	sqlDB, _ := db.DB()
	if err := sqlDB.PingContext(context.Background()); err == nil {
		t.Fatal("close failure skipped original resource cleanup")
	}
}

func socketJSON(t *testing.T, base, method, path, body, token string) (int, map[string]any, string) {
	t.Helper()
	request, err := http.NewRequest(method, base+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string]any
	if strings.HasPrefix(response.Header.Get("Content-Type"), "application/json") {
		if err := json.Unmarshal(raw, &envelope); err != nil {
			t.Fatal(err)
		}
	}
	return response.StatusCode, envelope, string(raw)
}

func TestHybridEntrySocketUsesSameHandlerAndBoundedShutdown(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(fmt.Sprint("timeout=", timeout), func(t *testing.T) {
			blocked := &blockedHTTPModel{base: &ordinaryModel{}, entered: make(chan struct{}), release: make(chan struct{})}
			r, db, agent, opens, redisOpens := entryRuntimeFixture(t, blocked)
			r.shutdownTimeout = 100 * time.Millisecond
			listening := make(chan string, 1)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			original := r.listen
			r.listen = func(network, address string) (net.Listener, error) {
				l, err := original(network, "127.0.0.1:0")
				if err == nil {
					listening <- "http://" + l.Addr().String()
				}
				return l, err
			}
			serverReady := make(chan struct{})
			r.server = func(address string, h http.Handler) *http.Server {
				s := entry.NewHTTPServer(address, h)
				if s.WriteTimeout != 0 || s.ReadHeaderTimeout != 5*time.Second || s.ReadTimeout != 30*time.Second || s.IdleTimeout != 60*time.Second {
					t.Error("SSE server settings drifted")
				}
				close(serverReady)
				return s
			}
			finished := make(chan error, 1)
			path := entryConfigFile(t, nil)
			go func() { finished <- runEntry(ctx, path, r) }()
			var base string
			select {
			case base = <-listening:
			case err := <-finished:
				t.Fatalf("entry stopped: %v", err)
			case <-time.After(10 * time.Second):
				t.Fatal("entry did not listen")
			}
			<-serverReady
			hash, err := utils.HashPassword("synthetic-password")
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Create(&model.User{ID: 1, Username: "learner", Name: "learner", Email: "learner@invalid.example", Password: hash}).Error; err != nil {
				t.Fatal(err)
			}
			_, login, _ := socketJSON(t, base, "POST", "/api/v1/auth/login", `{"email":"learner@invalid.example","password":"synthetic-password"}`, "")
			token := login["data"].(map[string]any)["token"].(string)
			_, created, _ := socketJSON(t, base, "POST", "/api/v1/sessions", `{"mode":"chat","title":"entry"}`, token)
			sid := created["data"].(map[string]any)["id"].(string)
			status, sent, _ := socketJSON(t, base, "POST", "/api/v1/sessions/"+sid+"/messages", `{"content":"ordinary"}`, token)
			if status != 200 || sent["error"] != nil {
				t.Fatalf("ordinary Send %d %v", status, sent)
			}
			_, created, _ = socketJSON(t, base, "POST", "/api/v1/sessions", `{"mode":"learning_agent","title":"Agent","requestId":"entry-agent-create"}`, token)
			asid := created["data"].(map[string]any)["id"].(string)
			status, run, _ := socketJSON(t, base, "POST", "/api/v1/sessions/"+asid+"/messages", agentBody("entry-agent-run", "hello"), token)
			if status != 200 || run["error"] != nil || agent.CallCount() != 1 {
				t.Fatalf("Agent Run %d %v", status, run)
			}
			status, _, stream := socketJSON(t, base, "POST", "/api/v1/sessions/"+asid+"/stream", publicStreamBody("entry-agent-stream", "hello"), token)
			if status != 200 || !strings.Contains(stream, "event: message.completed") || !strings.Contains(stream, "agent response") || agent.CallCount() != 2 {
				t.Fatalf("Agent SSE %d %s", status, stream)
			}
			for _, path := range []string{nativeMemoryPath, "/api/v1/knowledge/documents"} {
				status, result, _ := socketJSON(t, base, "GET", path, "", token)
				if status != 200 || result["status_code"] != float64(1000) {
					t.Fatalf("native route %s: %d %v", path, status, result)
				}
			}
			status, _, raw := socketJSON(t, base, "POST", learningPath+"start-stream", learningJSON(map[string]string{"sessionId": asid, "topic": "Redis"}), token)
			if status != 404 || raw != "" || agent.CallCount() != 2 {
				t.Fatal("learning SSE exclusion lost")
			}
			var tasks int64
			if err := db.Model(&model.LearningSession{}).Count(&tasks).Error; err != nil || tasks != 0 {
				t.Fatal("excluded learning side effect")
			}
			status, current, _ := socketJSON(t, base, "GET", learningPath+"current?sessionId="+asid, "", token)
			if status != 200 || current["status_code"] == nil {
				t.Fatal("native route lost")
			}
			request, err := http.NewRequest("POST", base+"/api/v1/sessions/"+sid+"/stream", strings.NewReader(`{"content":"before-complete"}`))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", "Bearer "+token)
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if first := readUntilDelta(t, response); !strings.Contains(first, "回答：before-complete") {
				t.Fatal("ordinary SSE did not flush")
			}
			<-blocked.entered
			if !timeout {
				close(blocked.release)
				if _, err := io.ReadAll(response.Body); err != nil {
					t.Fatal(err)
				}
			}
			start := time.Now()
			cancel()
			select {
			case err := <-finished:
				if timeout && !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("shutdown timeout lost: %v", err)
				}
				if !timeout && err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("unbounded entry shutdown")
			}
			if time.Since(start) > 2*time.Second {
				t.Fatal("HTTP shutdown exceeded test bound")
			}
			if *opens != 1 || *redisOpens != 1 {
				t.Fatal("duplicate connection chain")
			}
			sqlDB, _ := db.DB()
			if err := sqlDB.PingContext(context.Background()); err == nil {
				t.Fatal("resources left open")
			}
		})
	}
}
