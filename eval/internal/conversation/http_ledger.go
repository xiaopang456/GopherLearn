package conversation

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/cloudwego/eino/schema"
)

// modelHTTPHandler records the actual SDK request boundary and provider usage
// response without replacing any product collaborator. The supplied backend is
// always a no-upstream fake handler in the offline command.
func modelHTTPHandler(ledger *ModelLedger, roles RolePrompts, backend http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, readErr := io.ReadAll(io.LimitReader(r.Body, 8<<20+1))
		r.Body.Close()
		var request struct {
			Messages            []*schema.Message `json:"messages"`
			Model               string            `json:"model"`
			Input               json.RawMessage   `json:"input"`
			MaxTokens           *int              `json:"max_tokens"`
			MaxCompletionTokens *int              `json:"max_completion_tokens"`
		}
		decodeErr := json.Unmarshal(raw, &request)
		role, prompt := "unknown", ""
		switch r.URL.Path {
		case "/v1/embeddings":
			if len(request.Input) > 0 {
				role = "embedding"
			}
		case "/v1/rerank":
			if len(request.Input) > 0 {
				role = "rerank"
			}
		case "/v1/chat/completions":
			if len(request.Messages) > 0 && request.Messages[0] != nil && request.Messages[0].Role == schema.System {
				prompt = request.Messages[0].Content
				if known, ok := roles[prompt]; ok {
					role = known
				}
			}
		}
		id := ledger.begin(role, "http:"+r.URL.Path, json.RawMessage(raw), prompt)
		ledger.mu.Lock()
		ledger.calls[id-1].Model = request.Model
		ledger.calls[id-1].RequestedMaxTokens = request.MaxTokens
		ledger.calls[id-1].RequestedMaxCompletionTokens = request.MaxCompletionTokens
		ledger.calls[id-1].InputSHA256 = digest(raw)
		ledger.mu.Unlock()
		observed := &usageWriter{ResponseWriter: w, ledger: ledger, id: id}
		defer func() {
			if recover() != nil {
				observed.badTelemetry("model handler panicked")
				observed.writeErr = fmt.Errorf("model handler panicked")
				if observed.status == 0 {
					http.Error(observed, "model handler failed", 500)
				}
			}
			observed.end()
			// A client that cancels its request context after reading the
			// response is not a failed request. Only a request that never
			// produced a response is a cancellation.
			if observed.writeErr == nil && observed.status == 0 && r.Context().Err() != nil {
				observed.writeErr = fmt.Errorf("model request cancelled")
			}
			ledger.finish(id, observed.writeErr)
		}()
		if readErr != nil || decodeErr != nil || len(raw) > 8<<20 || r.Method != "POST" || role == "unknown" {
			observed.writeErr = fmt.Errorf("unclassified or malformed model request refused")
			http.Error(observed, "unclassified or malformed model request refused", 400)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		backend.ServeHTTP(observed, r)
	})
}

type usageWriter struct {
	http.ResponseWriter
	ledger    *ModelLedger
	id        int
	status    int
	streaming bool
	pending   []byte
	event     []byte
	oversized bool
	writeErr  error
}

func (w *usageWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.streaming = strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream")
	w.ledger.mu.Lock()
	w.ledger.calls[w.id-1].HTTPStatus = status
	w.ledger.mu.Unlock()
	w.ResponseWriter.WriteHeader(status)
}
func (w *usageWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	if !w.oversized {
		w.pending = append(w.pending, p...)
		if len(w.pending) > 8<<20 {
			w.badTelemetry("model usage frame exceeds limit")
			w.oversized = true
			w.pending = nil
		} else if w.streaming {
			for {
				newline := bytes.IndexByte(w.pending, '\n')
				if newline < 0 {
					break
				}
				line := bytes.TrimSuffix(w.pending[:newline], []byte{'\r'})
				w.line(line)
				w.pending = w.pending[newline+1:]
			}
		}
	}
	n, err := w.ResponseWriter.Write(p)
	if err != nil {
		w.writeErr = fmt.Errorf("model response write failed")
	}
	return n, err
}
func (w *usageWriter) Flush() {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	if err := http.NewResponseController(w.ResponseWriter).Flush(); err != nil {
		w.writeErr = fmt.Errorf("model response flush failed")
	}
}
func (w *usageWriter) line(line []byte) {
	if len(line) == 0 {
		if len(w.event) > 0 {
			w.parse(w.event)
			w.event = nil
		}
		return
	}
	if bytes.HasPrefix(line, []byte("data:")) {
		if len(w.event) > 0 {
			w.event = append(w.event, '\n')
		}
		w.event = append(w.event, bytes.TrimSpace(line[5:])...)
		if len(w.event) > 8<<20 {
			w.badTelemetry("model usage event exceeds limit")
			w.oversized = true
			w.event = nil
		}
	}
}
func (w *usageWriter) badTelemetry(reason string) {
	w.ledger.mu.Lock()
	defer w.ledger.mu.Unlock()
	w.ledger.calls[w.id-1].TelemetryError = reason
}
func (w *usageWriter) parse(raw []byte) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("[DONE]")) {
		return
	}
	var response struct {
		Usage json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		if w.status < 400 {
			w.badTelemetry("malformed model response telemetry")
		}
		return
	}
	if len(response.Usage) == 0 || bytes.Equal(response.Usage, []byte("null")) {
		return
	}
	var usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
		PromptDetails    struct {
			CachedTokens *int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		CacheHit          *int `json:"prompt_cache_hit_tokens"`
		CompletionDetails struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	}
	if err := json.Unmarshal(response.Usage, &usage); err != nil || usage.PromptTokens < 0 || usage.CompletionTokens < 0 || usage.TotalTokens < 0 || usage.CompletionDetails.ReasoningTokens < 0 {
		w.badTelemetry("invalid provider usage")
		return
	}
	cache := usage.PromptDetails.CachedTokens
	if cache == nil {
		cache = usage.CacheHit
	} else if usage.CacheHit != nil && *cache != *usage.CacheHit {
		w.badTelemetry("conflicting cache usage")
		return
	}
	if cache != nil && *cache < 0 {
		w.badTelemetry("invalid cached token usage")
		return
	}
	w.ledger.mu.Lock()
	defer w.ledger.mu.Unlock()
	c := &w.ledger.calls[w.id-1]
	c.RawUsageSamples = append(c.RawUsageSamples, append(json.RawMessage(nil), response.Usage...))
	u := schema.TokenUsage{PromptTokens: usage.PromptTokens, CompletionTokens: usage.CompletionTokens, TotalTokens: usage.TotalTokens, CompletionTokensDetails: schema.CompletionTokensDetails{ReasoningTokens: usage.CompletionDetails.ReasoningTokens}}
	if cache != nil {
		u.PromptTokenDetails.CachedTokens = *cache
	}
	c.Usage = &u
	c.UsageSamples = append(c.UsageSamples, u)
}
func (w *usageWriter) end() {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	if w.status >= 400 && w.writeErr == nil {
		w.writeErr = fmt.Errorf("model HTTP status %d", w.status)
	}
	if w.oversized {
		return
	}
	if w.streaming {
		if len(w.pending) > 0 {
			w.line(bytes.TrimSuffix(w.pending, []byte{'\r'}))
		}
		if len(w.event) > 0 {
			w.parse(w.event)
		}
	} else {
		w.parse(w.pending)
	}
}
