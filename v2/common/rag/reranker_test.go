package rag

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLocalRerankerRejectsUnsafeConfiguration(t *testing.T) {
	for _, test := range []struct{ endpoint, token string }{
		{"https://example.com", "token"},
		{"http://example.com", "token"},
		{"http://127.0.0.1:18081?debug=1", "token"},
		{"http://127.0.0.1:18081", ""},
	} {
		if _, err := NewLocalReranker(test.endpoint, test.token, time.Second); err == nil {
			t.Fatalf("NewLocalReranker(%q) accepted unsafe configuration", test.endpoint)
		}
	}
}

func TestLocalRerankerHealthAndStableOrdering(t *testing.T) {
	const token = "local-secret"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer "+token {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch request.URL.Path {
		case "/health":
			_ = json.NewEncoder(writer).Encode(map[string]string{"status": "ready", "device": "cuda:0", "model": RerankModel, "revision": RerankRevision})
		case "/rerank":
			var payload struct {
				Texts []string `json:"texts"`
			}
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			if len(payload.Texts) == 1 {
				_ = json.NewEncoder(writer).Encode([]RerankScore{{Index: 0, Score: 1}})
			} else {
				_ = json.NewEncoder(writer).Encode([]RerankScore{{Index: 2, Score: 1}, {Index: 0, Score: 2}, {Index: 1, Score: 2}})
			}
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	ranker, err := NewLocalReranker(strings.ReplaceAll(server.URL, "127.0.0.1", "localhost"), token, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := ranker.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	scores, err := ranker.Rerank(context.Background(), "q", []string{"a", "b", "c"})
	if err != nil {
		t.Fatal(err)
	}
	if scores[0].Index != 0 || scores[1].Index != 1 || scores[2].Index != 2 {
		t.Fatalf("unexpected stable order: %+v", scores)
	}
}

func TestValidateRerankScoresRejectsProtocolViolations(t *testing.T) {
	for _, scores := range [][]RerankScore{
		{{Index: 0, Score: 1}},
		{{Index: 0, Score: 1}, {Index: 0, Score: 2}},
		{{Index: 0, Score: 1}, {Index: 2, Score: 2}},
		{{Index: 0, Score: 1}, {Index: 1, Score: math.NaN()}},
	} {
		if err := validateRerankScores(scores, 2); err == nil {
			t.Fatalf("accepted invalid scores: %+v", scores)
		}
	}
}

func TestLocalRerankerClassifiesTransportAndServerFailuresAsUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(http.StatusServiceUnavailable) }))
	endpoint := strings.ReplaceAll(server.URL, "127.0.0.1", "localhost")
	server.Close()
	ranker, err := NewLocalReranker(endpoint, "token", 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	_, err = ranker.Rerank(context.Background(), "q", []string{"a"})
	if !errors.Is(err, ErrRerankUnavailable) {
		t.Fatalf("error = %v, want ErrRerankUnavailable", err)
	}
}
