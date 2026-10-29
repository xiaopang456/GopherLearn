package rag

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const hostedToken = "hosted-secret-token"

// hostedServer answers like a hosted rerank endpoint and records the request
// it received.
type hostedServer struct {
	server  *httptest.Server
	request map[string]any
	header  http.Header
}

func newHostedServer(t *testing.T, status int, body string) *hostedServer {
	t.Helper()
	recorder := &hostedServer{request: map[string]any{}}
	recorder.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		recorder.header = request.Header.Clone()
		payload, _ := io.ReadAll(io.LimitReader(request.Body, 1<<20))
		_ = json.Unmarshal(payload, &recorder.request)
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(status)
		_, _ = writer.Write([]byte(body))
	}))
	t.Cleanup(recorder.server.Close)
	return recorder
}

func TestDashScopeRerankerSendsTheDocumentedRequest(t *testing.T) {
	recorder := newHostedServer(t, http.StatusOK, `{"output":{"results":[{"index":1,"relevance_score":0.2},{"index":0,"relevance_score":0.9}]},"request_id":"req-1"}`)
	reranker, err := NewDashScopeReranker(recorder.server.URL, "qwen3-rerank", hostedToken, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	scores, err := reranker.Rerank(context.Background(), "查询", []string{"第一段", "第二段"})
	if err != nil {
		t.Fatal(err)
	}
	if scores[0].Index != 0 || scores[0].Score != 0.9 || scores[1].Index != 1 {
		t.Fatalf("unexpected order: %+v", scores)
	}
	if recorder.header.Get("Authorization") != "Bearer "+hostedToken {
		t.Fatalf("authorization header = %q", recorder.header.Get("Authorization"))
	}
	if recorder.header.Get("Content-Type") != "application/json" {
		t.Fatalf("content type = %q", recorder.header.Get("Content-Type"))
	}
	if recorder.request["model"] != "qwen3-rerank" {
		t.Fatalf("model = %v", recorder.request["model"])
	}
	input, ok := recorder.request["input"].(map[string]any)
	if !ok {
		t.Fatalf("input = %v", recorder.request["input"])
	}
	if input["query"] != "查询" {
		t.Fatalf("query = %v", input["query"])
	}
	documents, ok := input["documents"].([]any)
	if !ok || len(documents) != 2 || documents[0] != "第一段" {
		t.Fatalf("documents = %v", input["documents"])
	}
	parameters, ok := recorder.request["parameters"].(map[string]any)
	if !ok {
		t.Fatalf("parameters = %v", recorder.request["parameters"])
	}
	if parameters["top_n"] != float64(2) {
		t.Fatalf("top_n = %v", parameters["top_n"])
	}
	if parameters["return_documents"] != false {
		t.Fatalf("return_documents = %v", parameters["return_documents"])
	}
}

func TestDashScopeRerankerRefusesUnsafeConfiguration(t *testing.T) {
	for _, test := range []struct {
		endpoint, model, token string
	}{
		{"http://rerank.example.invalid/v1/rerank", "qwen3-rerank", hostedToken},
		{"https://rerank.example.invalid/v1/rerank", "", hostedToken},
		{"https://rerank.example.invalid/v1/rerank", "qwen3-rerank", ""},
		{"https://user:pass@rerank.example.invalid/v1/rerank", "qwen3-rerank", hostedToken},
		{"https://rerank.example.invalid/v1/rerank?key=1", "qwen3-rerank", hostedToken},
	} {
		if _, err := NewDashScopeReranker(test.endpoint, test.model, test.token, time.Second); !errors.Is(err, ErrRerankConfig) {
			t.Fatalf("NewDashScopeReranker(%q) accepted unsafe configuration: %v", test.endpoint, err)
		}
	}
}

func TestDashScopeRerankerNamesTheFailingReason(t *testing.T) {
	for _, test := range []struct {
		name     string
		status   int
		body     string
		want     error
		contains string
	}{
		{
			name:     "the credential is refused",
			status:   http.StatusUnauthorized,
			body:     `{"code":"InvalidApiKey","message":"Invalid API-key provided."}`,
			want:     ErrRerankRejected,
			contains: "Invalid API-key provided.",
		},
		{
			name:     "the service is throttling",
			status:   http.StatusTooManyRequests,
			body:     `{"code":"Throttling","message":"rate limited"}`,
			want:     ErrRerankRateLimited,
			contains: "rate limited",
		},
		{
			name:     "the request itself is wrong",
			status:   http.StatusBadRequest,
			body:     `{"code":"InvalidParameter","message":"model not found"}`,
			want:     ErrRerankConfig,
			contains: "model not found",
		},
		{
			name:     "the service failed",
			status:   http.StatusInternalServerError,
			body:     `{"code":"InternalError","message":"boom"}`,
			want:     ErrRerankUnavailable,
			contains: "boom",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := newHostedServer(t, test.status, test.body)
			reranker, err := NewDashScopeReranker(recorder.server.URL, "qwen3-rerank", hostedToken, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			_, err = reranker.Rerank(context.Background(), "q", []string{"a"})
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
			if !strings.Contains(err.Error(), test.contains) {
				t.Fatalf("error %q does not name the reason %q", err, test.contains)
			}
		})
	}
}

func TestDashScopeRerankerRefusesAnUnreadableAnswer(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
	}{
		{"missing scores", `{"output":{"results":[{"index":0}]}}`},
		{"missing index", `{"output":{"results":[{"relevance_score":0.5}]}}`},
		{"too few results", `{"output":{"results":[{"index":0,"relevance_score":0.5}]}}`},
		{"an error inside a 200", `{"code":"InvalidApiKey","message":"Invalid API-key provided."}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := newHostedServer(t, http.StatusOK, test.body)
			reranker, err := NewDashScopeReranker(recorder.server.URL, "qwen3-rerank", hostedToken, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := reranker.Rerank(context.Background(), "q", []string{"a", "b"}); !errors.Is(err, ErrRerankUnavailable) {
				t.Fatalf("error = %v, want ErrRerankUnavailable", err)
			}
		})
	}
}

func TestDashScopeRerankerReportsATimeoutSeparately(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	reranker, err := NewDashScopeReranker(server.URL, "qwen3-rerank", hostedToken, 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	_, err = reranker.Rerank(context.Background(), "q", []string{"a"})
	if !errors.Is(err, ErrRerankTimeout) {
		t.Fatalf("error = %v, want ErrRerankTimeout", err)
	}
}

// TestDashScopeHealthProvesTheCredential proves the startup check reaches the
// service: a refused credential must fail the check rather than let the
// deployment start with a reranker that cannot work.
func TestDashScopeHealthProvesTheCredential(t *testing.T) {
	recorder := newHostedServer(t, http.StatusUnauthorized, `{"code":"InvalidApiKey","message":"Invalid API-key provided."}`)
	reranker, err := NewDashScopeReranker(recorder.server.URL, "qwen3-rerank", hostedToken, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	err = reranker.Health(context.Background())
	if !errors.Is(err, ErrRerankRejected) {
		t.Fatalf("error = %v, want ErrRerankRejected", err)
	}
	if !errors.Is(err, ErrRerankUnavailable) {
		t.Fatalf("error = %v, want ErrRerankUnavailable as well", err)
	}
}

func TestDashScopeHealthPassesWhenTheServiceAnswers(t *testing.T) {
	recorder := newHostedServer(t, http.StatusOK, `{"output":{"results":[{"index":0,"relevance_score":0.7}]}}`)
	reranker, err := NewDashScopeReranker(recorder.server.URL, "qwen3-rerank", hostedToken, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := reranker.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
}
