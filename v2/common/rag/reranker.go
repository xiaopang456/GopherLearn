package rag

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	RerankModel      = "BAAI/bge-reranker-v2-m3"
	RerankRevision   = "953dc6f6f85a1b2dbfca4c34a2796e7dde08d41e"
	RerankCandidateK = 24
)

var ErrRerankUnavailable = errors.New("RAG reranker unavailable")

type RerankScore struct {
	Index int     `json:"index"`
	Score float64 `json:"score"`
}

type Reranker interface {
	Health(context.Context) error
	Rerank(context.Context, string, []string) ([]RerankScore, error)
}

type LocalReranker struct {
	baseURL string
	token   string
	client  *http.Client
}

func NewLocalReranker(endpoint, token string, timeout time.Duration) (*LocalReranker, error) {
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || parsed.Scheme != "http" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("rerank endpoint must be a local http URL without credentials, query, or fragment")
	}
	host := strings.ToLower(parsed.Hostname())
	if host != "127.0.0.1" && host != "localhost" && host != "host.docker.internal" && host != "reranker" {
		return nil, errors.New("rerank endpoint must remain local")
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("rerank bearer token is required")
	}
	if timeout <= 0 || timeout > 10*time.Second {
		return nil, errors.New("rerank timeout must be in (0,10s]")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return &LocalReranker{baseURL: parsed.String(), token: token, client: &http.Client{Timeout: timeout}}, nil
}

func (r *LocalReranker) Health(ctx context.Context) error {
	request, err := http.NewRequestWithContext(nonNilContext(ctx), http.MethodGet, r.baseURL+"/health", nil)
	if err != nil {
		return err
	}
	r.authorize(request)
	response, err := r.client.Do(request)
	if err != nil {
		return fmt.Errorf("%w: health request: %v", ErrRerankUnavailable, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: health status %d", ErrRerankUnavailable, response.StatusCode)
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 16<<10))
	decoder.DisallowUnknownFields()
	var health struct {
		Status   string `json:"status"`
		Device   string `json:"device"`
		Model    string `json:"model"`
		Revision string `json:"revision"`
	}
	if err := decoder.Decode(&health); err != nil {
		return fmt.Errorf("decode rerank health: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return fmt.Errorf("decode rerank health: %w", err)
	}
	if health.Status != "ready" || health.Model != RerankModel || health.Revision != RerankRevision || !strings.HasPrefix(strings.ToLower(health.Device), "cuda") {
		return fmt.Errorf("rerank health identity does not match the frozen CUDA model")
	}
	// A metadata-only health endpoint can be green while CUDA inference is not.
	// Warm one synthetic pair at startup so the first user's request neither
	// discovers a broken model nor pays framework initialization latency.
	if _, err := r.Rerank(ctx, "本地重排序服务健康检查", []string{"本地重排序服务已经就绪。"}); err != nil {
		return fmt.Errorf("rerank inference warmup: %w", err)
	}
	return nil
}

func (r *LocalReranker) Rerank(ctx context.Context, query string, passages []string) ([]RerankScore, error) {
	query = strings.TrimSpace(query)
	if query == "" || len(passages) == 0 || len(passages) > RerankCandidateK {
		return nil, errors.New("rerank requires a query and 1..24 passages")
	}
	for index, passage := range passages {
		if strings.TrimSpace(passage) == "" {
			return nil, fmt.Errorf("rerank passage %d is empty", index)
		}
	}
	body, err := json.Marshal(struct {
		Query      string   `json:"query"`
		Texts      []string `json:"texts"`
		RawScores  bool     `json:"raw_scores"`
		ReturnText bool     `json:"return_text"`
		Truncate   bool     `json:"truncate"`
	}{Query: query, Texts: passages, RawScores: true, ReturnText: false, Truncate: true})
	if err != nil {
		return nil, err
	}
	if len(body) > 2_000_000 {
		return nil, errors.New("rerank request exceeds 2000000 bytes")
	}
	request, err := http.NewRequestWithContext(nonNilContext(ctx), http.MethodPost, r.baseURL+"/rerank", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	r.authorize(request)
	response, err := r.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%w: request: %v", ErrRerankUnavailable, err)
	}
	defer response.Body.Close()
	limited := io.LimitReader(response.Body, 64<<10)
	if response.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(limited)
		if response.StatusCode >= http.StatusInternalServerError {
			return nil, fmt.Errorf("%w: status %d", ErrRerankUnavailable, response.StatusCode)
		}
		return nil, fmt.Errorf("rerank status %d: %s", response.StatusCode, strings.TrimSpace(string(message)))
	}
	decoder := json.NewDecoder(limited)
	decoder.DisallowUnknownFields()
	var scores []RerankScore
	if err := decoder.Decode(&scores); err != nil {
		return nil, fmt.Errorf("decode rerank response: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, fmt.Errorf("decode rerank response: %w", err)
	}
	if err := validateRerankScores(scores, len(passages)); err != nil {
		return nil, err
	}
	sort.SliceStable(scores, func(i, j int) bool {
		if scores[i].Score != scores[j].Score {
			return scores[i].Score > scores[j].Score
		}
		return scores[i].Index < scores[j].Index
	})
	return scores, nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("unexpected trailing JSON value")
		}
		return err
	}
	return nil
}

func (r *LocalReranker) authorize(request *http.Request) {
	request.Header.Set("Authorization", "Bearer "+r.token)
}

func validateRerankScores(scores []RerankScore, candidates int) error {
	if len(scores) != candidates {
		return fmt.Errorf("rerank returned %d scores, want %d", len(scores), candidates)
	}
	seen := make(map[int]struct{}, len(scores))
	for _, score := range scores {
		if score.Index < 0 || score.Index >= candidates || math.IsNaN(score.Score) || math.IsInf(score.Score, 0) {
			return errors.New("rerank returned an invalid index or score")
		}
		if _, duplicate := seen[score.Index]; duplicate {
			return errors.New("rerank returned a duplicate index")
		}
		seen[score.Index] = struct{}{}
	}
	return nil
}

func nonNilContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}
