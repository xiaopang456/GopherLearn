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

// TestLocalRerankerHealthAndStableOrdering drives the client against a service
// that answers like the standard cross-encoder server: an empty /health and a
// model identity at /info.
func TestLocalRerankerHealthAndStableOrdering(t *testing.T) {
	const token = "local-secret"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer "+token {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch request.URL.Path {
		case "/health":
			writer.WriteHeader(http.StatusOK)
		case "/info":
			_ = json.NewEncoder(writer).Encode(map[string]string{"model_id": RerankModel, "model_sha": RerankRevision})
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

// TestLocalRerankerHealthNamesTheFailingReason proves the health check says
// which fact was wrong: a listening service that serves another model must not
// pass as a healthy one.
func TestLocalRerankerHealthNamesTheFailingReason(t *testing.T) {
	for _, test := range []struct {
		name     string
		handler  http.HandlerFunc
		want     error
		contains string
	}{
		{
			name: "another model",
			handler: func(writer http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/health":
					writer.WriteHeader(http.StatusOK)
				case "/info":
					_ = json.NewEncoder(writer).Encode(map[string]string{"model_id": "BAAI/bge-reranker-base", "model_sha": RerankRevision})
				default:
					writer.WriteHeader(http.StatusNotFound)
				}
			},
			want:     ErrRerankIdentity,
			contains: "bge-reranker-base",
		},
		{
			name: "another revision",
			handler: func(writer http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/health":
					writer.WriteHeader(http.StatusOK)
				case "/info":
					_ = json.NewEncoder(writer).Encode(map[string]string{"model_id": RerankModel, "model_sha": "0000000000000000000000000000000000000000"})
				default:
					writer.WriteHeader(http.StatusNotFound)
				}
			},
			want:     ErrRerankIdentity,
			contains: shortRevision(RerankRevision),
		},
		{
			name: "no identity at all",
			handler: func(writer http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/health":
					writer.WriteHeader(http.StatusOK)
				default:
					writer.WriteHeader(http.StatusNotFound)
				}
			},
			want:     ErrRerankUnavailable,
			contains: "model identity",
		},
		{
			name: "health is not green",
			handler: func(writer http.ResponseWriter, request *http.Request) {
				writer.WriteHeader(http.StatusServiceUnavailable)
			},
			want:     ErrRerankUnavailable,
			contains: "health status 503",
		},
		{
			name: "the model itself is broken",
			handler: func(writer http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/health":
					writer.WriteHeader(http.StatusOK)
				case "/info":
					_ = json.NewEncoder(writer).Encode(map[string]string{"model_id": RerankModel, "model_sha": RerankRevision})
				default:
					writer.WriteHeader(http.StatusInternalServerError)
				}
			},
			want:     ErrRerankUnavailable,
			contains: "warmup",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(test.handler)
			defer server.Close()
			ranker, err := NewLocalReranker(strings.ReplaceAll(server.URL, "127.0.0.1", "localhost"), "token", time.Second)
			if err != nil {
				t.Fatal(err)
			}
			err = ranker.Health(context.Background())
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
			if !strings.Contains(err.Error(), test.contains) {
				t.Fatalf("error %q does not name the reason %q", err, test.contains)
			}
		})
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

// TestNewRerankerSelectsTheConfiguredProvider proves the factory routes on the
// configured provider name and refuses a name it does not implement.
func TestNewRerankerSelectsTheConfiguredProvider(t *testing.T) {
	local, err := NewReranker(RerankerOptions{Provider: "local", Endpoint: "http://127.0.0.1:18081", Token: "token", Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := local.(*LocalReranker); !ok {
		t.Fatalf("provider local built %T", local)
	}
	hosted, err := NewReranker(RerankerOptions{Provider: "dashscope", Endpoint: "https://rerank.example.invalid/v1/rerank", Model: "qwen3-rerank", Token: "token", Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := hosted.(*DashScopeReranker); !ok {
		t.Fatalf("provider dashscope built %T", hosted)
	}
	if _, err := NewReranker(RerankerOptions{Provider: "somethingelse", Endpoint: "http://127.0.0.1:18081", Token: "token", Timeout: time.Second}); !errors.Is(err, ErrRerankConfig) {
		t.Fatalf("unknown provider error = %v, want ErrRerankConfig", err)
	}
}
