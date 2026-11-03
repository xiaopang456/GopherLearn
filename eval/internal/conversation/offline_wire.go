package conversation

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/cloudwego/eino/schema"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"
)

// This provider speaks the real SDK wire protocol on loopback. It has no
// forwarding destination or credentials and cannot reach an upstream model.
func offlineWire(ledger *ModelLedger, roles RolePrompts, chat *offlineChat) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []*schema.Message `json:"messages"`
			Stream   bool              `json:"stream"`
			Input    json.RawMessage   `json:"input"`
		}
		err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&request)
		role := "unknown"
		prompt := ""
		if strings.HasSuffix(r.URL.Path, "/embeddings") {
			role = "embedding"
		} else if len(request.Messages) > 0 && request.Messages[0] != nil && request.Messages[0].Role == schema.System {
			prompt = request.Messages[0].Content
			if v, ok := roles[prompt]; ok {
				role = v
			}
		}
		id := ledger.begin(role, "http:"+r.URL.Path, request, prompt)
		defer func() { ledger.finish(id, err) }()
		if err != nil || r.Method != "POST" || role == "unknown" {
			err = fmt.Errorf("unclassified or malformed offline model request")
			http.Error(w, err.Error(), 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if role == "embedding" {
			var inputs []string
			if e := json.Unmarshal(request.Input, &inputs); e != nil {
				var text string
				if e = json.Unmarshal(request.Input, &text); e != nil {
					err = e
					http.Error(w, "invalid embedding input", 400)
					return
				}
				inputs = []string{text}
			}
			data := []any{}
			for i := range inputs {
				data = append(data, map[string]any{"object": "embedding", "index": i, "embedding": make([]float64, 1024)})
			}
			err = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data, "model": "synthetic-offline"})
			return
		}
		var message *schema.Message
		message, err = chat.Generate(r.Context(), request.Messages)
		if err != nil {
			http.Error(w, "unsupported offline role", 400)
			return
		}
		if request.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			chunk := map[string]any{"id": "offline", "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": "synthetic-offline", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": message.Content}, "finish_reason": "stop"}}}
			var bytes []byte
			bytes, err = json.Marshal(chunk)
			if err != nil {
				return
			}
			_, err = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", bytes)
			return
		}
		err = json.NewEncoder(w).Encode(map[string]any{"id": "offline", "object": "chat.completion", "created": time.Now().Unix(), "model": "synthetic-offline", "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": message.Content}, "finish_reason": "stop"}}})
	}))
}

type redisSandbox struct{ id, nonce, addr string }

func newRedisSandbox(ctx context.Context) (*redisSandbox, error) {
	nonce, err := randomHex(16)
	if err != nil {
		return nil, err
	}
	id, err := docker(ctx, "run", "-d", "--pull=never", "--name", "gl-eval-redis-"+nonce, "--label", sandboxLabel+"="+nonce, "-p", "127.0.0.1::6379", "redis/redis-stack-server:latest")
	if err != nil {
		return nil, err
	}
	if !containerIDPattern.MatchString(id) {
		return nil, fmt.Errorf("invalid Redis container id")
	}
	s := &redisSandbox{id: id, nonce: nonce}
	fail := func(err error) (*redisSandbox, error) {
		c, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if cleanup := s.Close(c); cleanup != nil {
			return nil, fmt.Errorf("%v; Redis cleanup: %w", err, cleanup)
		}
		return nil, err
	}
	if err = s.verify(ctx); err != nil {
		return fail(err)
	}
	port, err := docker(ctx, "port", id, "6379/tcp")
	if err != nil {
		return fail(err)
	}
	host, p, err := net.SplitHostPort(port)
	if err != nil || host != "127.0.0.1" || p == "" {
		return fail(fmt.Errorf("Redis is not loopback-only"))
	}
	s.addr = port
	for {
		pong, e := docker(ctx, "exec", id, "redis-cli", "ping")
		if e == nil && pong == "PONG" {
			return s, nil
		}
		select {
		case <-ctx.Done():
			return fail(ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}
func (s *redisSandbox) verify(ctx context.Context) error {
	if s == nil || !containerIDPattern.MatchString(s.id) || len(s.nonce) != 32 {
		return fmt.Errorf("invalid Redis identity")
	}
	label, err := docker(ctx, "inspect", "--format", `{{index .Config.Labels "org.gopherlearn.eval.nonce"}}`, s.id)
	if err != nil || label != s.nonce {
		return fmt.Errorf("Redis identity mismatch")
	}
	if s.addr != "" {
		port, err := docker(ctx, "port", s.id, "6379/tcp")
		if err != nil || port != s.addr {
			return fmt.Errorf("Redis port changed")
		}
	}
	return nil
}
func (s *redisSandbox) Close(ctx context.Context) error {
	if err := s.verify(ctx); err != nil {
		return err
	}
	_, err := docker(ctx, "rm", "-f", "-v", s.id)
	return err
}
