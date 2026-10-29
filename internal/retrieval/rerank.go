package retrieval

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

// The reranker is the second stage a hybrid search hands its fused candidates to:
// a cross-encoder reads the query and one passage together, so it can judge
// relevance the way two independent vector spaces cannot. The original deployment
// served BAAI/bge-reranker-v2-m3 locally behind this exact shape, and the shape is
// kept as it was found - including the bare-array response, which is unusual enough
// that a silently "tolerant" parser would be the likeliest place to lose a bug.
//
// What is deliberately not copied is the failure policy ambiguity. Transport faults
// and 5xx answers are reported as ErrRerankUnavailable, meaning "the ranking we
// already have is still valid"; everything else - a 4xx, a body that is not the
// contract, a count that disagrees with the passages sent - is ErrRerankShape,
// meaning "this reranker is misconfigured and falling back would hide it".

const (
	// RerankModel names the cross-encoder the migrated deployment runs.
	RerankModel = "BAAI/bge-reranker-v2-m3"
	// DefaultRerankTimeout is short on purpose: a reranker that answers slowly is
	// worse than no reranker, because the fused order is already usable.
	DefaultRerankTimeout = 5 * time.Second
	// MaxRerankTimeout is the ceiling the original service accepted; beyond it the
	// request would be cancelled by the caller's own retrieval deadline anyway.
	MaxRerankTimeout = 10 * time.Second
	// DefaultRerankCandidateLimit is how many fused candidates are sent for a second
	// ordering. It is larger than the final top-k by design: reranking five passages
	// that BM25 and cosine already agreed on adds nothing.
	DefaultRerankCandidateLimit = 24
	// minRerankAuthTokenLength is the original guard: a short token is a typo, and a
	// typo in an authorization header looks like working configuration.
	minRerankAuthTokenLength = 32
	// maxRerankResponseBytes bounds the score array.
	maxRerankResponseBytes = 1 << 20
)

// DefaultRerankHosts are the addresses a reranker endpoint may point at. The service
// is a local model server, so a public host here is far more likely to be a
// misconfiguration that starts sending document text somewhere unexpected than it is
// to be intentional; RerankerOptions.AllowedHosts widens the list explicitly.
var DefaultRerankHosts = []string{"127.0.0.1", "localhost", "host.docker.internal", "reranker"}

var (
	// ErrRerankConfig reports a reranker that cannot be built.
	ErrRerankConfig = errors.New("retrieval: reranker configuration is invalid")
	// ErrRerankUnavailable reports a transport fault or a server-side failure. A
	// caller may keep the ranking it already has.
	ErrRerankUnavailable = errors.New("retrieval: reranker unavailable")
	// ErrRerankShape reports an answer that is not the agreed contract, or a request
	// the contract forbids. Falling back over it would keep a broken deployment live.
	ErrRerankShape = errors.New("retrieval: reranker response does not match the contract")
)

// RerankHit is one passage's second-stage verdict, addressed by the position it had
// in the passages that were sent. Scores are reported as the model produced them:
// they order passages within one call and mean nothing across calls, so they never
// leave the server.
type RerankHit struct {
	Index int
	Score float64
}

// Reranker reorders the positions of the passages it is handed.
type Reranker interface {
	// Rerank returns exactly one hit per passage. Returning a subset would silently
	// drop evidence the first stage had already admitted.
	Rerank(ctx context.Context, query string, passages []string) ([]RerankHit, error)
	// Model names the ranking, so a result set can say which second stage produced it.
	Model() string
}

// RerankerOptions configures an HTTPReranker.
type RerankerOptions struct {
	Endpoint    string
	AuthToken   string
	Timeout     time.Duration
	MaxPassages int
	// AllowedHosts replaces the default local-only allowlist. Empty selects
	// DefaultRerankHosts.
	AllowedHosts []string
	Client       *http.Client
}

// HTTPReranker talks to the local cross-encoder service.
type HTTPReranker struct {
	endpoint       string
	healthEndpoint string
	authToken      string
	client         *http.Client
	maxPassages    int
	model          string
}

var _ Reranker = (*HTTPReranker)(nil)

func NewHTTPReranker(options RerankerOptions) (*HTTPReranker, error) {
	base := strings.TrimRight(strings.TrimSpace(options.Endpoint), "/")
	if base == "" {
		return nil, fmt.Errorf("%w: endpoint is required", ErrRerankConfig)
	}
	parsed, err := url.Parse(base)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" {
		return nil, fmt.Errorf("%w: endpoint must be an absolute http url", ErrRerankConfig)
	}
	if parsed.User != nil {
		return nil, fmt.Errorf("%w: endpoint must not carry credentials", ErrRerankConfig)
	}
	allowed := options.AllowedHosts
	if len(allowed) == 0 {
		allowed = DefaultRerankHosts
	}
	if !containsFold(allowed, parsed.Hostname()) {
		return nil, fmt.Errorf("%w: host %q is not in the allowed list", ErrRerankConfig, parsed.Hostname())
	}
	token := strings.TrimSpace(options.AuthToken)
	if token != "" && len(token) < minRerankAuthTokenLength {
		return nil, fmt.Errorf("%w: auth token must be at least %d bytes", ErrRerankConfig, minRerankAuthTokenLength)
	}
	timeout := options.Timeout
	if timeout <= 0 {
		timeout = DefaultRerankTimeout
	}
	if timeout > MaxRerankTimeout {
		return nil, fmt.Errorf("%w: timeout %s exceeds %s", ErrRerankConfig, timeout, MaxRerankTimeout)
	}
	maxPassages := options.MaxPassages
	if maxPassages <= 0 {
		maxPassages = DefaultRerankCandidateLimit
	}
	if maxPassages > 100 {
		return nil, fmt.Errorf("%w: max passages %d exceeds 100", ErrRerankConfig, maxPassages)
	}
	client := options.Client
	if client == nil {
		client = &http.Client{Timeout: timeout}
	} else if client.Timeout <= 0 {
		copied := *client
		copied.Timeout = timeout
		client = &copied
	}
	return &HTTPReranker{
		endpoint:       base + "/rerank",
		healthEndpoint: base + "/health",
		authToken:      token, client: client, maxPassages: maxPassages,
		model: RerankModel,
	}, nil
}

// Model implements Reranker.
func (r *HTTPReranker) Model() string { return r.model }

// MaxPassages reports how many candidates one call may carry, so the pipeline can ask
// for exactly as many fused candidates as the second stage will accept.
func (r *HTTPReranker) MaxPassages() int { return r.maxPassages }

// Rerank implements Reranker. Passages are sent in the caller's order and every
// returned index is a position in that same slice.
func (r *HTTPReranker) Rerank(ctx context.Context, query string, passages []string) ([]RerankHit, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: context is required", ErrRerankShape)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(passages) == 0 {
		return []RerankHit{}, nil
	}
	if len(passages) > r.maxPassages {
		// Refusing beats truncating: a silent cut would drop passages the first stage
		// admitted and make the missing ones unexplainable later.
		return nil, fmt.Errorf("%w: %d passages exceeds the %d limit", ErrRerankShape, len(passages), r.maxPassages)
	}
	payload, err := json.Marshal(map[string]any{
		"query":       query,
		"texts":       passages,
		"raw_scores":  true,
		"return_text": false,
		"truncate":    true,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: request could not be encoded", ErrRerankShape)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, r.endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("%w: request could not be built", ErrRerankShape)
	}
	request.Header.Set("content-type", "application/json")
	request.Header.Set("accept", "application/json")
	if r.authToken != "" {
		request.Header.Set("authorization", "Bearer "+r.authToken)
	}
	response, err := r.client.Do(request)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("%w: transport failed", ErrRerankUnavailable)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxRerankResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("%w: response could not be read", ErrRerankUnavailable)
	}
	if response.StatusCode >= http.StatusInternalServerError {
		return nil, fmt.Errorf("%w: service returned %d", ErrRerankUnavailable, response.StatusCode)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("%w: service returned %d", ErrRerankShape, response.StatusCode)
	}
	if ctype := response.Header.Get("content-type"); ctype != "" && !strings.Contains(ctype, "application/json") {
		return nil, fmt.Errorf("%w: content type %q is not json", ErrRerankShape, ctype)
	}
	return decodeRerank(body, len(passages))
}

// Health asks the service whether it is up. The pipeline does not call it on every
// turn - a failing rerank is already reported as unavailable - but startup wiring and
// an operator can, and doing it over HTTP rather than a TCP probe means a process
// listening on the wrong path is caught before the first learner question.
func (r *HTTPReranker) Health(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("%w: context is required", ErrRerankShape)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, r.healthEndpoint, nil)
	if err != nil {
		return fmt.Errorf("%w: request could not be built", ErrRerankShape)
	}
	if r.authToken != "" {
		request.Header.Set("authorization", "Bearer "+r.authToken)
	}
	response, err := r.client.Do(request)
	if err != nil {
		return fmt.Errorf("%w: transport failed", ErrRerankUnavailable)
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode >= http.StatusInternalServerError {
		return fmt.Errorf("%w: health returned %d", ErrRerankUnavailable, response.StatusCode)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("%w: health returned %d", ErrRerankShape, response.StatusCode)
	}
	return nil
}

// decodeRerank reads the bare score array and validates it against how many passages
// were sent. The strictness is the point: an off-by-one in the service's index
// reporting would otherwise reorder evidence rather than fail.
func decodeRerank(body []byte, sent int) ([]RerankHit, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var raw []struct {
		// Both fields are pointers because a null or an absent score is a different
		// fact from a zero one: the service answered without judging the passage, and
		// reading that as 0.0 would rank it last instead of refusing.
		Index *int     `json:"index"`
		Score *float64 `json:"score"`
	}
	if err := decoder.Decode(&raw); err != nil {
		return nil, fmt.Errorf("%w: body is not a score array", ErrRerankShape)
	}
	if err := decoder.Decode(new(json.RawMessage)); err != io.EOF {
		return nil, fmt.Errorf("%w: content followed the score array", ErrRerankShape)
	}
	if len(raw) != sent {
		return nil, fmt.Errorf("%w: %d scores for %d passages", ErrRerankShape, len(raw), sent)
	}
	hits := make([]RerankHit, 0, len(raw))
	seen := make([]bool, sent)
	for position, item := range raw {
		if item.Index == nil || item.Score == nil {
			return nil, fmt.Errorf("%w: entry %d is missing an index or a score", ErrRerankShape, position)
		}
		index, score := *item.Index, *item.Score
		if index < 0 || index >= sent {
			return nil, fmt.Errorf("%w: index %d is outside 0..%d", ErrRerankShape, index, sent-1)
		}
		if seen[index] {
			return nil, fmt.Errorf("%w: index %d appeared twice", ErrRerankShape, index)
		}
		seen[index] = true
		if math.IsNaN(score) || math.IsInf(score, 0) {
			return nil, fmt.Errorf("%w: index %d carries a non-finite score", ErrRerankShape, index)
		}
		hits = append(hits, RerankHit{Index: index, Score: score})
	}
	for index, present := range seen {
		if !present {
			return nil, fmt.Errorf("%w: score for index %d is missing", ErrRerankShape, index)
		}
	}
	// Best score first, ties broken by the original position so the ordering is a
	// function of the input alone.
	sort.SliceStable(hits, func(left, right int) bool {
		if hits[left].Score != hits[right].Score {
			return hits[left].Score > hits[right].Score
		}
		return hits[left].Index < hits[right].Index
	})
	return hits, nil
}

func containsFold(values []string, candidate string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), candidate) {
			return true
		}
	}
	return false
}
