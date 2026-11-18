package conversation

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

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
				if known, ok := classifyPrompt(roles, prompt); ok {
					role = known
				}
			}
		}
		id := ledger.begin(role, "http:"+r.URL.Path, json.RawMessage(raw), prompt)
		// The card generation is the one call whose raw answer a reader
		// cannot reconstruct: the ledger holds hashes, and a hash says that
		// two bodies differ, not what either of them said. When the server
		// refuses a card, the question is what the model actually wrote.
		//
		// The request is recognised by the TOOL it binds - the card writer
		// is the only call that asks for publish_session_card - so the
		// capture does not depend on the prompt's wording.
		isCardGeneration := role == "background" && bytes.Contains(raw, []byte(cardToolName))
		ledger.mu.Lock()
		ledger.calls[id-1].Model = request.Model
		ledger.calls[id-1].RequestedMaxTokens = request.MaxTokens
		ledger.calls[id-1].RequestedMaxCompletionTokens = request.MaxCompletionTokens
		ledger.calls[id-1].InputSHA256 = digest(raw)
		// The request body is the record of what the model was shown, and
		// the product does not persist the cards it showed. The names of the
		// conversations those cards were about are read out of the body
		// here, so a paid run can say what the answer was given.
		if shown, sessions := ReadShownCards(request.Messages); shown {
			ledger.calls[id-1].CardSessions = sessions
		}
		// The cross-session memory the request carried is read out of the same
		// body: the product does not persist the memories it showed, so the
		// request is the only record of what the model was given. The kind
		// says whether it was a card or an Episode, and the text says what it
		// said.
		memory := ReadShownMemory(request.Messages)
		ledger.calls[id-1].ShownMemoryKind = memory.Kind
		ledger.calls[id-1].InjectedText = memory.Text
		ledger.calls[id-1].InjectedMessageIDs = memory.MessageIDs
		ledger.calls[id-1].InjectedSegmentSeqs = memory.SegmentSeqs
		// The tool table is in the same body. The two arms do not carry the
		// same table, so the attempt has to say which one it was given.
		if names := ToolNamesFromBody(raw); len(names) > 0 {
			ledger.calls[id-1].ToolTable = names
		}
		ledger.mu.Unlock()
		observed := &usageWriter{ResponseWriter: w, ledger: ledger, id: id,
			requestBody: string(raw), isCardGeneration: isCardGeneration}
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
	// requestBody is the body the model was sent, kept for the card
	// generation's capture.
	requestBody string
	// isCardGeneration says this call is the card writer's.
	isCardGeneration bool
	// cardBody is the body the model answered, kept for the card
	// generation's capture.
	//
	// It is kept apart from pending because a streamed answer is consumed
	// line by line: pending holds what is left of it, and the card's own
	// answer is what the reader needs whole.
	cardBody []byte
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
		if w.isCardGeneration {
			// The card's own answer is kept whole: it is the one answer a
			// reader cannot reconstruct, and the body says what the model
			// wrote.
			w.cardBody = append(w.cardBody, p...)
		}
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
	// The card generation's own exchange is kept whole.
	//
	// It is the one exchange a reader cannot reconstruct: the ledger holds
	// hashes, and a hash says that two bodies differ, not what either of
	// them said. When the server refuses a card, the question is what the
	// model actually wrote - and that is in this body.
	//
	// The bodies carry no credential: the handler that reads them never
	// reads a header, and the key the harness uses is a placeholder.
	if w.isCardGeneration && !w.oversized {
		w.ledger.CaptureCardExchange(CardExchange{
			Request:         w.requestBody,
			Response:        string(w.cardBody),
			HTTPStatus:      w.status,
			TargetMessageID: cardTargetMessageID(w.requestBody),
			At:              time.Now().UTC(),
		})
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

// cardTargetMessageID reads the target message id out of a card
// generation's request.
//
// The payload the card writer is shown names the message the card may
// reach to, and that id belongs to exactly one conversation. The
// conversation's own id is not in the body - the product writes the card
// for a session it holds in its own database - so the target is what ties
// a raw exchange to the conversation it belongs to.
//
// Zero means the body said nothing usable, and the exchange is then only
// known by its own order.
func cardTargetMessageID(body string) uint {
	var request struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(body), &request); err != nil {
		return 0
	}
	for _, message := range request.Messages {
		if message.Role != "user" {
			continue
		}
		var payload struct {
			TargetMessageID uint `json:"target_message_id"`
		}
		if err := json.Unmarshal([]byte(message.Content), &payload); err != nil {
			continue
		}
		if payload.TargetMessageID > 0 {
			return payload.TargetMessageID
		}
	}
	return 0
}
