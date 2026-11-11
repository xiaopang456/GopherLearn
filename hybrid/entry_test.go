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
	"gorm.io/gorm"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	text = strings.ReplaceAll(text, `embeddingApiKey = ""`, `embeddingApiKey = "synthetic-embedding-never-sent"`)
	text = strings.ReplaceAll(text, `storageDir = "data/v2-isolated/knowledge"`, fmt.Sprintf("storageDir = %q", filepath.ToSlash(t.TempDir())))
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
	for _, kind := range []string{"missing", "syntax", "migration"} {
		t.Run(kind, func(t *testing.T) {
			path := ""
			if kind != "missing" {
				path = entryConfigFile(t, func(text string) string {
					switch kind {
					case "syntax":
						return text + "\n[broken"
					case "migration":
						return strings.Replace(text, "migrate = false", "migrate = true", 1)
					}
					return text
				})
			}
			called := false
			r := entryRuntime{dependencies: func(*entry.Settings) entry.Dependencies { called = true; return entry.Dependencies{} }, listen: func(string, string) (net.Listener, error) { called = true; return nil, nil }, server: entry.NewHTTPServer, shutdownTimeout: time.Second}
			if err := runEntry(context.Background(), path, r); err == nil || called {
				t.Fatalf("invalid config reached factory: %v called=%v", err, called)
			}
		})
	}
}

// [ordinaryChat] is refused by name, not skipped: a file that still carries a section
// nothing builds a client from reads as though it still configured one, and the operator
// would be changing a section with no effect. The refusal has to reach the log, because
// the entry otherwise reports only that it refused to start.
func TestHybridEntryRefusesTheRetiredOrdinaryChatSectionByName(t *testing.T) {
	for _, shape := range []string{"populated", "empty"} {
		t.Run(shape, func(t *testing.T) {
			section := "[ordinaryChat]\nGOPHERLEARN_OPENAI_MODEL = \"ordinary-model\"\n"
			if shape == "empty" {
				// A section written and left blank is still a section the operator
				// believes is in force.
				section = "[ordinaryChat]\n"
			}
			path := entryConfigFile(t, func(text string) string { return text + "\n" + section })
			called := false
			r := entryRuntime{dependencies: func(*entry.Settings) entry.Dependencies { called = true; return entry.Dependencies{} }, listen: func(string, string) (net.Listener, error) { called = true; return nil, nil }, server: entry.NewHTTPServer, shutdownTimeout: time.Second}
			err := runEntry(context.Background(), path, r)
			if err == nil || called {
				t.Fatalf("a retired section was accepted, or a factory still ran: %v called=%v", err, called)
			}
			if !strings.Contains(err.Error(), "ordinaryChat") {
				t.Fatalf("the refusal does not name the section: %v", err)
			}
			// Printable: this is a key-naming refusal with no value in it.
			printed := config.SafeRefusalMessage(err)
			if printed == "" || !strings.Contains(printed, "[ordinaryChat]") || strings.Contains(printed, "ordinary-model") {
				t.Fatalf("refusal is not printable as written: %q", printed)
			}
		})
	}
	// Positive control, so the case above is not "everything is refused": the same file
	// without the section loads, and nothing in it mentions an ordinary client.
	settings, err := loadEntry(entryConfigFile(t, nil))
	if err != nil {
		t.Fatalf("a configuration with no [ordinaryChat] was refused: %v", err)
	}
	if settings == nil || settings.v2 == nil {
		t.Fatal("the settings the entry assembles from are missing")
	}
}

func entryRuntimeFixture(t *testing.T) (entryRuntime, *gorm.DB, *scriptedChatModel, *int, *int) {
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
	r := entryRuntime{dependencies: func(*entry.Settings) entry.Dependencies { return deps }, listen: net.Listen, server: entry.NewHTTPServer, shutdownTimeout: time.Second}
	return r, db, agent, &opens, &redisOpens
}

func TestHybridEntryFailuresReleaseOriginalResources(t *testing.T) {
	for _, stage := range []string{"assembly", "listen"} {
		t.Run(stage, func(t *testing.T) {
			r, db, _, opens, _ := entryRuntimeFixture(t)
			failure := errors.New("synthetic entry failure")
			switch stage {
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
	r, db, _, _, _ := entryRuntimeFixture(t)
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
			r, db, agent, opens, redisOpens := entryRuntimeFixture(t)
			// The in-flight request that shutdown has to drain is a conversation turn,
			// and a conversation turn is generated by the V2 model. The gate keeps the
			// earlier turns in this test fast and blocks only the last one.
			gate := make(chan struct{})
			release := make(chan struct{})
			var releaseOnce sync.Once
			releaseNow := func() { releaseOnce.Do(func() { close(release) }) }
			defer releaseNow()
			agent.setRespond(func(ctx context.Context, _ int) (*schema.Message, error) {
				select {
				case <-gate:
					select {
					case <-release:
						return schema.AssistantMessage("回答：before-complete", nil), nil
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				default:
					return schema.AssistantMessage("agent response", nil), nil
				}
			})
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
			streamDone := make(chan struct{})
			r.server = func(address string, h http.Handler) *http.Server {
				wrapped := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					if req.Header.Get("X-Test-Shutdown-Stream") == "yes" {
						defer close(streamDone)
					}
					h.ServeHTTP(w, req)
				})
				s := entry.NewHTTPServer(address, wrapped)
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
			// A conversation turn is one generation on the shared path and nothing on the
			// retired ordinary model, which is what makes the counts below what they are.
			if agent.CallCount() != 1 {
				t.Fatalf("ordinary Send generated %d times, want 1", agent.CallCount())
			}
			_, created, _ = socketJSON(t, base, "POST", "/api/v1/sessions", `{"mode":"learning_agent","title":"Agent","requestId":"entry-agent-create"}`, token)
			asid := created["data"].(map[string]any)["id"].(string)
			status, run, _ := socketJSON(t, base, "POST", "/api/v1/sessions/"+asid+"/messages", agentBody("entry-agent-run", "hello"), token)
			if status != 200 || run["error"] != nil || agent.CallCount() != 2 {
				t.Fatalf("Agent Run %d %v", status, run)
			}
			status, _, stream := socketJSON(t, base, "POST", "/api/v1/sessions/"+asid+"/stream", publicStreamBody("entry-agent-stream", "hello"), token)
			if status != 200 || !strings.Contains(stream, "event: message.completed") || !strings.Contains(stream, "agent response") || agent.CallCount() != 3 {
				t.Fatalf("Agent SSE %d %s", status, stream)
			}
			for _, path := range []string{nativeMemoryPath, "/api/v1/knowledge/documents"} {
				status, result, _ := socketJSON(t, base, "GET", path, "", token)
				if status != 200 || result["status_code"] != float64(1000) {
					t.Fatalf("native route %s: %d %v", path, status, result)
				}
			}
			status, _, raw := socketJSON(t, base, "POST", learningPath+"start-stream", learningJSON(map[string]string{"sessionId": asid, "topic": "Redis"}), token)
			if status != 404 || raw != "" || agent.CallCount() != 3 {
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
			request.Header.Set("X-Test-Shutdown-Stream", "yes")
			close(gate)
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			frames := collectSSEFrames(response.Body)
			// The frame saying the turn has started is flushed while generation is still
			// blocked, which is what leaves this request in flight for shutdown to drain.
			// The answer is one validated snapshot and cannot arrive before the model
			// returns, so it is not what this test waits for.
			if first := frames.next(t, 10*time.Second); !strings.Contains(first, "event: message.started") {
				t.Fatalf("ordinary SSE did not flush the run starting: %s", first)
			}
			if timeout {
				go frames.drain()
			} else {
				releaseNow()
				if rest := frames.rest(); !strings.Contains(rest, `"content":"回答：before-complete"`) || !strings.Contains(rest, "event: message.completed") {
					t.Fatalf("ordinary SSE did not finish: %s", rest)
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
			// A forced HTTP close does not wait for the handler. Let the model
			// finish, then wait for its persistence and response cleanup before
			// the test removes the database directory.
			releaseNow()
			select {
			case <-streamDone:
			case <-time.After(5 * time.Second):
				t.Fatal("stream handler did not exit")
			}
		})
	}
}
