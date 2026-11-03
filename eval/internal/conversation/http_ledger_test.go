package conversation

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPModelLedgerUsageAndClosedRoles(t *testing.T) {
	ledger := &ModelLedger{}
	backendCalls := 0
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendCalls++
		switch backendCalls {
		case 1:
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":4}}}`)
		case 2:
			w.Header().Set("Content-Type", "text/event-stream")
			for _, part := range []string{"da", "ta: {\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":1,\"total_tokens\":13}}\r\n\r\n", "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":3,\"total_tokens\":15,\"prompt_tokens_details\":{\"cached_tokens\":4}}}\n\ndata: [DONE]\n\n"} {
				io.WriteString(w, part)
				w.(http.Flusher).Flush()
			}
		case 3:
			io.WriteString(w, `{"choices":[]}`)
		case 4:
			http.Error(w, "provider failure without sensitive body logging", 429)
		}
	})
	handler := modelHTTPHandler(ledger, RolePrompts{"known": "answer"}, backend)
	for i := 0; i < 4; i++ {
		body := `{"model":"synthetic","messages":[{"role":"system","content":"known"}],"tools":[{"type":"function"}]}`
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(body)))
	}
	calls := ledger.Calls()
	if len(calls) != 4 || !ledger.Complete() {
		t.Fatal(calls)
	}
	for _, i := range []int{0, 1} {
		c := calls[i]
		if c.Model != "synthetic" || c.Usage == nil || c.Usage.PromptTokens != 12 || c.Usage.CompletionTokens != 3 || c.Usage.PromptTokenDetails.CachedTokens != 4 {
			t.Fatalf("usage %+v", c)
		}
	}
	if len(calls[1].RawUsageSamples) != 2 || len(calls[1].UsageSamples) != 2 {
		t.Fatal("stream snapshots lost")
	}
	if calls[2].Usage != nil || calls[3].HTTPStatus != 429 || calls[3].Error == "" {
		t.Fatal("missing usage/status invented")
	}
	calls[0].RawUsageSamples[0][0] = 'x'
	if !json.Valid(ledger.Calls()[0].RawUsageSamples[0]) {
		t.Fatal("mutable usage snapshot")
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(`{"messages":[{"role":"system","content":"unrecognized"}]}`)))
	if w.Code != 400 || backendCalls != 4 || ledger.Complete() {
		t.Fatal("unknown request reached backend")
	}
}

func TestHTTPModelLedgerRejectsTelemetryLoss(t *testing.T) {
	for _, body := range []string{`{"usage":{"prompt_tokens":-1}}`, `{"usage":"invalid"}`, `not json`} {
		ledger := &ModelLedger{}
		handler := modelHTTPHandler(ledger, RolePrompts{"known": "background"}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) }))
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(`{"messages":[{"role":"system","content":"known"}]}`)))
		if ledger.Complete() || ledger.Calls()[0].TelemetryError == "" {
			t.Fatalf("accepted %s", body)
		}
	}
}

func TestDeepSeekCacheAndReasoningUsage(t *testing.T) {
	ledger := &ModelLedger{}
	handler := modelHTTPHandler(ledger, RolePrompts{"known": "answer"}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"usage":{"prompt_tokens":12,"completion_tokens":9,"total_tokens":21,"prompt_cache_hit_tokens":4,"prompt_cache_miss_tokens":8,"completion_tokens_details":{"reasoning_tokens":6}}}`)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(`{"messages":[{"role":"system","content":"known"}]}`)))
	calls := ledger.Calls()
	totals, err := SummarizeCalls(calls)
	if err != nil || calls[0].Usage.PromptTokenDetails.CachedTokens != 4 || calls[0].Usage.CompletionTokensDetails.ReasoningTokens != 6 || totals["answer"].ReportedCachedInput != 4 || totals["answer"].ReportedReasoning != 6 || totals["answer"].ReportedOutput != 9 {
		t.Fatalf("tokens double counted or missing: %+v err=%v", totals, err)
	}
}
