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

	"GopherAI/config"
)

const (
	RerankModel      = "BAAI/bge-reranker-v2-m3"
	RerankRevision   = "953dc6f6f85a1b2dbfca4c34a2796e7dde08d41e"
	RerankCandidateK = 24
	// MaxRerankTimeout bounds one rerank call. The bound is generous because
	// a cross-encoder running on CPU answers a candidate batch in seconds, not
	// milliseconds; a shorter bound would turn a working service into a
	// silent degradation.
	MaxRerankTimeout = 120 * time.Second
)

var (
	// ErrRerankUnavailable marks a rerank service that could not answer.
	ErrRerankUnavailable = errors.New("RAG reranker unavailable")
	// ErrRerankConfig marks a rerank configuration that cannot be used.
	ErrRerankConfig = errors.New("rerank configuration is invalid")
	// ErrRerankRejected marks a credential the service refused.
	ErrRerankRejected = errors.New("rerank credential rejected")
	// ErrRerankRateLimited marks a service that is throttling this deployment.
	ErrRerankRateLimited = errors.New("rerank request rate limited")
	// ErrRerankTimeout marks a call that did not finish inside its bound.
	ErrRerankTimeout = errors.New("rerank request timed out")
	// ErrRerankIdentity marks a service that serves another model than the
	// one this deployment expects.
	ErrRerankIdentity = errors.New("rerank service serves a different model")
)

type RerankScore struct {
	Index int     `json:"index"`
	Score float64 `json:"score"`
}

type Reranker interface {
	Health(context.Context) error
	Rerank(context.Context, string, []string) ([]RerankScore, error)
}

// RerankerOptions describes the rerank service to build.
//
// Provider selects the service shape; an empty provider means the local one.
// Endpoint is the service address, Token is the bearer credential and Model
// names the model a hosted provider should serve.
type RerankerOptions struct {
	Provider string
	Endpoint string
	Model    string
	Token    string
	Timeout  time.Duration
}

// NewReranker builds the rerank client the configuration asks for.
//
// The error names the supported providers and never carries the endpoint or
// the token, because those are configuration values and a startup error
// reaches terminals and logs.
func NewReranker(options RerankerOptions) (Reranker, error) {
	switch strings.TrimSpace(options.Provider) {
	case "", config.RerankProviderLocal:
		return NewLocalReranker(options.Endpoint, options.Token, options.Timeout)
	case config.RerankProviderDashScope:
		return NewDashScopeReranker(options.Endpoint, options.Model, options.Token, options.Timeout)
	default:
		return nil, fmt.Errorf("%w: unsupported rerank provider; supported providers are %s and %s", ErrRerankConfig, config.RerankProviderLocal, config.RerankProviderDashScope)
	}
}

type LocalReranker struct {
	baseURL string
	token   string
	client  *http.Client
}

// NewLocalReranker builds a client for a locally served cross-encoder.
//
// The address must stay local: the service holds the model and the passages
// never have to leave the machine through it.
func NewLocalReranker(endpoint, token string, timeout time.Duration) (*LocalReranker, error) {
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || parsed.Scheme != "http" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("%w: rerank endpoint must be a local http URL without credentials, query, or fragment", ErrRerankConfig)
	}
	host := strings.ToLower(parsed.Hostname())
	if host != "127.0.0.1" && host != "localhost" && host != "host.docker.internal" && host != "reranker" {
		return nil, fmt.Errorf("%w: rerank endpoint must remain local", ErrRerankConfig)
	}
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("%w: rerank bearer token is required", ErrRerankConfig)
	}
	if timeout <= 0 || timeout > MaxRerankTimeout {
		return nil, fmt.Errorf("%w: rerank timeout must be in (0,%s]", ErrRerankConfig, MaxRerankTimeout)
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return &LocalReranker{baseURL: parsed.String(), token: token, client: &http.Client{Timeout: timeout}}, nil
}

// Health checks that the local service is up and serves the frozen model.
//
// The service reports its identity at /info; /health only says that the
// process is listening. Both are checked because a listening process with the
// wrong weights answers every request and orders the evidence wrongly.
func (r *LocalReranker) Health(ctx context.Context) error {
	if err := r.checkHealth(ctx); err != nil {
		return err
	}
	if err := r.checkIdentity(ctx); err != nil {
		return err
	}
	// A metadata-only health endpoint can be green while the model itself is
	// not loaded. Warm one synthetic pair at startup so the first user's
	// request neither discovers a broken model nor pays framework
	// initialization latency.
	if _, err := r.Rerank(ctx, "本地重排序服务健康检查", []string{"本地重排序服务已经就绪。"}); err != nil {
		return fmt.Errorf("%w: rerank inference warmup: %w", ErrRerankUnavailable, err)
	}
	return nil
}

// checkHealth requires the service to answer /health with a success status.
// The body is deliberately ignored: the standard cross-encoder server answers
// it with an empty body, and a wrapper that answers with a JSON envelope is
// accepted as well.
func (r *LocalReranker) checkHealth(ctx context.Context) error {
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
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: health status %d", ErrRerankUnavailable, response.StatusCode)
	}
	return nil
}

// checkIdentity requires the service to report the frozen model and revision.
// The device is not part of this check: the same weights run on CPU, and a
// deployment that answers slowly is a deployment problem, not a wrong model.
func (r *LocalReranker) checkIdentity(ctx context.Context) error {
	request, err := http.NewRequestWithContext(nonNilContext(ctx), http.MethodGet, r.baseURL+"/info", nil)
	if err != nil {
		return err
	}
	r.authorize(request)
	response, err := r.client.Do(request)
	if err != nil {
		return fmt.Errorf("%w: model identity request: %v", ErrRerankUnavailable, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: the rerank service did not report its model identity (status %d)", ErrRerankUnavailable, response.StatusCode)
	}
	var info struct {
		ModelID  string `json:"model_id"`
		ModelSHA string `json:"model_sha"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 16<<10)).Decode(&info); err != nil {
		return fmt.Errorf("%w: the rerank service did not report its model identity: %v", ErrRerankUnavailable, err)
	}
	if info.ModelID != RerankModel || info.ModelSHA != RerankRevision {
		return fmt.Errorf("%w: the rerank service serves %s at revision %s, but this deployment expects %s at revision %s",
			ErrRerankIdentity, info.ModelID, shortRevision(info.ModelSHA), RerankModel, shortRevision(RerankRevision))
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
	return sortRerankScores(scores), nil
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

// sortRerankScores puts the best score first and breaks ties by the original
// position, so the order is a function of the input alone.
func sortRerankScores(scores []RerankScore) []RerankScore {
	sort.SliceStable(scores, func(i, j int) bool {
		if scores[i].Score != scores[j].Score {
			return scores[i].Score > scores[j].Score
		}
		return scores[i].Index < scores[j].Index
	})
	return scores
}

// shortRevision shortens a revision for an error message. A full commit hash
// is unreadable in a terminal and says nothing an operator did not already
// know.
func shortRevision(revision string) string {
	if len(revision) > 12 {
		return revision[:12]
	}
	return revision
}

func nonNilContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}
