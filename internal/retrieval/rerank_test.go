package retrieval

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// rerankRecorder is the fake cross-encoder. It keeps the parsed request so the wire
// contract the original service expects is asserted field by field, and a counter so
// "refused before sending" is distinguishable from "sent and rejected".
type rerankRecorder struct {
	server      *httptest.Server
	requests    int
	paths       []string
	authHeaders []string
	lastRequest struct {
		Query      string   `json:"query"`
		Texts      []string `json:"texts"`
		RawScores  *bool    `json:"raw_scores"`
		ReturnText *bool    `json:"return_text"`
		Truncate   *bool    `json:"truncate"`
	}
}

func newRerankRecorder(t *testing.T, handler func(http.ResponseWriter, *rerankRecorder)) *rerankRecorder {
	t.Helper()
	recorder := &rerankRecorder{}
	recorder.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder.requests++
		recorder.paths = append(recorder.paths, r.URL.Path)
		recorder.authHeaders = append(recorder.authHeaders, r.Header.Get("authorization"))
		recorder.lastRequest.Query = ""
		recorder.lastRequest.Texts = nil
		recorder.lastRequest.RawScores = nil
		recorder.lastRequest.ReturnText = nil
		recorder.lastRequest.Truncate = nil
		_ = json.NewDecoder(r.Body).Decode(&recorder.lastRequest)
		handler(w, recorder)
	}))
	t.Cleanup(recorder.server.Close)
	return recorder
}

func scores(w http.ResponseWriter, pairs ...[2]any) {
	items := make([]map[string]any, 0, len(pairs))
	for _, pair := range pairs {
		items = append(items, map[string]any{"index": pair[0], "score": pair[1]})
	}
	w.Header().Set("content-type", "application/json")
	encoded, _ := json.Marshal(items)
	_, _ = w.Write(encoded)
}

func newTestReranker(t *testing.T, endpoint string, options RerankerOptions) *HTTPReranker {
	t.Helper()
	options.Endpoint = endpoint
	if options.AllowedHosts == nil {
		options.AllowedHosts = []string{"127.0.0.1", "localhost"}
	}
	reranker, err := NewHTTPReranker(options)
	if err != nil {
		t.Fatalf("constructor: %v", err)
	}
	return reranker
}

func TestNewHTTPRerankerRejectsUnusableConfig(t *testing.T) {
	long := strings.Repeat("a", minRerankAuthTokenLength)
	cases := []struct {
		name    string
		options RerankerOptions
	}{
		{"no endpoint", RerankerOptions{}},
		{"schemeless", RerankerOptions{Endpoint: "127.0.0.1:18081"}},
		{"credentials", RerankerOptions{Endpoint: "http://user:pw@127.0.0.1:18081"}},
		{"host outside the allowlist", RerankerOptions{Endpoint: "http://evil.example.invalid"}},
		{"short token", RerankerOptions{Endpoint: "http://127.0.0.1:18081", AuthToken: "tooshort"}},
		{"long timeout", RerankerOptions{Endpoint: "http://127.0.0.1:18081", Timeout: time.Minute}},
		{"too many passages", RerankerOptions{Endpoint: "http://127.0.0.1:18081", MaxPassages: 5000}},
	}
	for _, item := range cases {
		if _, err := NewHTTPReranker(item.options); !errors.Is(err, ErrRerankConfig) {
			t.Errorf("%s: error = %v, want ErrRerankConfig", item.name, err)
		}
	}
	if _, err := NewHTTPReranker(RerankerOptions{
		Endpoint: "http://127.0.0.1:18081", AuthToken: long, AllowedHosts: []string{"127.0.0.1"},
	}); err != nil {
		t.Errorf("a %d-byte token on an allowed host should be accepted: %v", len(long), err)
	}
	if _, err := NewHTTPReranker(RerankerOptions{Endpoint: "http://rerank.internal:8080", AllowedHosts: []string{"rerank.internal"}}); err != nil {
		t.Errorf("an operator-named host should be allowed once listed: %v", err)
	}
}

func TestRerankSendsTheContractedBodyAndOrdersByScore(t *testing.T) {
	recorder := newRerankRecorder(t, func(w http.ResponseWriter, r *rerankRecorder) {
		// Answered worst-to-best: the caller must still receive best-first hits.
		scores(w, [2]any{2, 0.1}, [2]any{1, 0.5}, [2]any{0, 0.9})
	})
	reranker := newTestReranker(t, recorder.server.URL, RerankerOptions{AuthToken: strings.Repeat("t", minRerankAuthTokenLength)})
	hits, err := reranker.Rerank(context.Background(), "问题", []string{"甲", "乙", "丙"})
	if err != nil {
		t.Fatalf("Rerank: %v", err)
	}
	if reranker.Model() != RerankModel {
		t.Errorf("Model() = %q, want %q", reranker.Model(), RerankModel)
	}
	if reranker.MaxPassages() != DefaultRerankCandidateLimit {
		t.Errorf("MaxPassages() = %d, want the default %d", reranker.MaxPassages(), DefaultRerankCandidateLimit)
	}
	if len(hits) != 3 || hits[0].Index != 0 || hits[1].Index != 1 || hits[2].Index != 2 {
		t.Fatalf("hits = %v, want positions ordered by descending score", hits)
	}
	if reranker.Model() == "" {
		t.Fatal("model name is required")
	}
	if got := recorder.lastRequest; got.Query != "问题" || len(got.Texts) != 3 || got.Texts[0] != "甲" {
		t.Fatalf("request = %+v, want the query and passages in caller order", got)
	}
	if got := recorder.lastRequest; got.RawScores == nil || !*got.RawScores {
		t.Error("raw_scores must be sent as true")
	}
	if got := recorder.lastRequest; got.ReturnText == nil || *got.ReturnText {
		t.Error("return_text must be sent as false: the service has no reason to echo passages back")
	}
	if got := recorder.lastRequest; got.Truncate == nil || !*got.Truncate {
		t.Error("truncate must be sent as true so an over-long passage is cut rather than refused")
	}
	if len(recorder.paths) != 1 || recorder.paths[0] != "/rerank" {
		t.Fatalf("paths = %v, want exactly one /rerank request", recorder.paths)
	}
	if recorder.authHeaders[0] != "Bearer "+strings.Repeat("t", minRerankAuthTokenLength) {
		t.Fatalf("authorization = %q, want the bearer form", recorder.authHeaders[0])
	}
}

func TestRerankRefusesToTruncateAnOversizedWindow(t *testing.T) {
	recorder := newRerankRecorder(t, func(w http.ResponseWriter, r *rerankRecorder) {
		t.Fatal("an over-cap request must not reach the service")
	})
	reranker := newTestReranker(t, recorder.server.URL, RerankerOptions{MaxPassages: 2})
	if _, err := reranker.Rerank(context.Background(), "q", []string{"a", "b", "c"}); !errors.Is(err, ErrRerankShape) {
		t.Fatalf("error = %v, want ErrRerankShape", err)
	}
	if recorder.requests != 0 {
		t.Fatalf("requests = %d, want none", recorder.requests)
	}
}

func TestRerankValidatesTheScoreArrayAgainstWhatWasSent(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"fewer scores than passages", `[{"index":0,"score":0.9}]`},
		{"more scores than passages", `[{"index":0,"score":0.9},{"index":1,"score":0.5},{"index":2,"score":0.1}]`},
		{"duplicate index", `[{"index":0,"score":0.9},{"index":0,"score":0.5}]`},
		{"index out of range", `[{"index":5,"score":0.9},{"index":1,"score":0.5}]`},
		{"missing score", `[{"index":0},{"index":1,"score":0.5}]`},
		{"null score", `[{"index":0,"score":null},{"index":1,"score":0.5}]`},
		{"object instead of array", `{"results":[{"index":0,"score":0.9}]}`},
		{"trailing content", `[{"index":0,"score":0.9},{"index":1,"score":0.5}] extra`},
		{"not json", `<html>502</html>`},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			recorder := newRerankRecorder(t, func(w http.ResponseWriter, r *rerankRecorder) {
				w.Header().Set("content-type", "application/json")
				_, _ = w.Write([]byte(item.body))
			})
			reranker := newTestReranker(t, recorder.server.URL, RerankerOptions{})
			_, err := reranker.Rerank(context.Background(), "q", []string{"a", "b"})
			if !errors.Is(err, ErrRerankShape) {
				t.Fatalf("error = %v, want ErrRerankShape", err)
			}
		})
	}
}

func TestRerankSeparatesUnavailableFromProtocolBreak(t *testing.T) {
	for _, item := range []struct {
		status int
		want   error
	}{
		{http.StatusInternalServerError, ErrRerankUnavailable},
		{http.StatusBadGateway, ErrRerankUnavailable},
		{http.StatusBadRequest, ErrRerankShape},
		{http.StatusUnauthorized, ErrRerankShape},
	} {
		recorder := newRerankRecorder(t, func(w http.ResponseWriter, r *rerankRecorder) {
			w.Header().Set("content-type", "text/plain")
			w.WriteHeader(item.status)
			_, _ = w.Write([]byte("passage echo that must not be quoted: " + r.lastRequest.Query))
		})
		reranker := newTestReranker(t, recorder.server.URL, RerankerOptions{})
		_, err := reranker.Rerank(context.Background(), "SECRET-QUERY-TEXT", []string{"a"})
		if !errors.Is(err, item.want) {
			t.Fatalf("status %d: error = %v, want %v", item.status, err, item.want)
		}
		if strings.Contains(err.Error(), "SECRET-QUERY-TEXT") {
			t.Fatalf("status %d: error echoed the request: %v", item.status, err)
		}
	}
}

func TestRerankReportsTransportFailureAsUnavailable(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	endpoint := dead.URL
	dead.Close()
	reranker := newTestReranker(t, endpoint, RerankerOptions{})
	if _, err := reranker.Rerank(context.Background(), "q", []string{"a"}); !errors.Is(err, ErrRerankUnavailable) {
		t.Fatalf("error = %v, want ErrRerankUnavailable", err)
	}
	if err := reranker.Health(context.Background()); !errors.Is(err, ErrRerankUnavailable) {
		t.Fatalf("health error = %v, want ErrRerankUnavailable", err)
	}
}

func TestRerankEmptyPassageListNeedsNoRequest(t *testing.T) {
	recorder := newRerankRecorder(t, func(w http.ResponseWriter, r *rerankRecorder) {
		t.Fatal("no passages means no request")
	})
	reranker := newTestReranker(t, recorder.server.URL, RerankerOptions{})
	hits, err := reranker.Rerank(context.Background(), "q", nil)
	if err != nil || len(hits) != 0 || recorder.requests != 0 {
		t.Fatalf("hits = %v, err = %v, requests = %d, want an empty answer and no request", hits, err, recorder.requests)
	}
}

func TestHealthReadsTheStatusOnly(t *testing.T) {
	recorder := newRerankRecorder(t, func(w http.ResponseWriter, r *rerankRecorder) {
		if r.paths[len(r.paths)-1] != "/health" {
			t.Errorf("path = %q, want /health", r.paths[len(r.paths)-1])
		}
		w.WriteHeader(http.StatusNoContent)
	})
	reranker := newTestReranker(t, recorder.server.URL, RerankerOptions{})
	if err := reranker.Health(context.Background()); err != nil {
		t.Fatalf("Health: %v", err)
	}

	failing := newRerankRecorder(t, func(w http.ResponseWriter, r *rerankRecorder) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	breaker := newTestReranker(t, failing.server.URL, RerankerOptions{})
	if err := breaker.Health(context.Background()); !errors.Is(err, ErrRerankUnavailable) {
		t.Fatalf("error = %v, want ErrRerankUnavailable", err)
	}
}
