package rag

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// maxRemoteErrorBytes bounds how much of a failed response is quoted back.
const maxRemoteErrorBytes = 512

// DashScopeReranker calls a hosted rerank endpoint that follows the DashScope
// request and response shapes.
//
// The request carries a model name, a query and the candidate documents; the
// answer carries one relevance score per candidate, addressed by the position
// the candidate had in the request. The credential is a bearer token, so it
// travels in the Authorization header and never in the body.
//
// Unlike the local service this endpoint has no health probe, so the startup
// check is one synthetic rerank call. That call costs one request against the
// deployment's quota and proves the credential, the model and the network path
// in one step.
type DashScopeReranker struct {
	endpoint string
	model    string
	token    string
	client   *http.Client
}

// NewDashScopeReranker builds a client for a hosted rerank endpoint.
//
// The endpoint must be an https URL: the credential leaves the process
// inside the Authorization header, and plain http would put it on the wire in
// clear text. A loopback address is the one exception, for a service the
// deployment runs itself. The URL is the rerank URL itself, not a service
// root.
func NewDashScopeReranker(endpoint, model, token string, timeout time.Duration) (*DashScopeReranker, error) {
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("%w: rerank endpoint must be an https URL without credentials, query, or fragment", ErrRerankConfig)
	}
	host := strings.ToLower(parsed.Hostname())
	loopback := host == "127.0.0.1" || host == "localhost" || host == "::1"
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && loopback) {
		return nil, fmt.Errorf("%w: rerank endpoint must be an https URL; plain http would carry the credential in clear text", ErrRerankConfig)
	}
	if strings.TrimSpace(model) == "" {
		return nil, fmt.Errorf("%w: rerank model is required for a hosted rerank endpoint", ErrRerankConfig)
	}
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("%w: rerank bearer token is required", ErrRerankConfig)
	}
	if timeout <= 0 || timeout > MaxRerankTimeout {
		return nil, fmt.Errorf("%w: rerank timeout must be in (0,%s]", ErrRerankConfig, MaxRerankTimeout)
	}
	return &DashScopeReranker{
		endpoint: strings.TrimRight(parsed.String(), "/"),
		model:    strings.TrimSpace(model),
		token:    token,
		client:   &http.Client{Timeout: timeout},
	}, nil
}

// Health proves the service answers with this deployment's credential and
// model. A hosted endpoint has no health probe, so one synthetic rerank call
// stands in for it.
func (r *DashScopeReranker) Health(ctx context.Context) error {
	if _, err := r.Rerank(ctx, "重排服务健康检查", []string{"重排服务已经就绪。"}); err != nil {
		return fmt.Errorf("%w: rerank startup check: %w", ErrRerankUnavailable, err)
	}
	return nil
}

func (r *DashScopeReranker) Rerank(ctx context.Context, query string, passages []string) ([]RerankScore, error) {
	query = strings.TrimSpace(query)
	if query == "" || len(passages) == 0 || len(passages) > RerankCandidateK {
		return nil, errors.New("rerank requires a query and 1..24 passages")
	}
	for index, passage := range passages {
		if strings.TrimSpace(passage) == "" {
			return nil, fmt.Errorf("rerank passage %d is empty", index)
		}
	}
	body, err := json.Marshal(map[string]any{
		"model": r.model,
		"input": map[string]any{
			"query":     query,
			"documents": passages,
		},
		"parameters": map[string]any{
			"return_documents": false,
			"top_n":            len(passages),
		},
	})
	if err != nil {
		return nil, err
	}
	if len(body) > 2_000_000 {
		return nil, errors.New("rerank request exceeds 2000000 bytes")
	}
	request, err := http.NewRequestWithContext(nonNilContext(ctx), http.MethodPost, r.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+r.token)
	response, err := r.client.Do(request)
	if err != nil {
		return nil, r.transportError(err)
	}
	defer response.Body.Close()
	limited := io.LimitReader(response.Body, 64<<10)
	if response.StatusCode != http.StatusOK {
		return nil, r.statusError(response.StatusCode, limited)
	}
	var envelope struct {
		Output struct {
			Results []struct {
				Index *int     `json:"index"`
				Score *float64 `json:"relevance_score"`
			} `json:"results"`
		} `json:"output"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(limited).Decode(&envelope); err != nil {
		return nil, fmt.Errorf("%w: the rerank service answered with an unreadable body: %v", ErrRerankUnavailable, err)
	}
	if strings.TrimSpace(envelope.Code) != "" {
		return nil, fmt.Errorf("%w: the rerank service reported %s: %s", ErrRerankUnavailable, envelope.Code, boundText(envelope.Message))
	}
	scores := make([]RerankScore, 0, len(envelope.Output.Results))
	for _, result := range envelope.Output.Results {
		if result.Index == nil || result.Score == nil {
			return nil, fmt.Errorf("%w: the rerank service answered a result without an index or a score", ErrRerankUnavailable)
		}
		scores = append(scores, RerankScore{Index: *result.Index, Score: *result.Score})
	}
	if err := validateRerankScores(scores, len(passages)); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRerankUnavailable, err)
	}
	return sortRerankScores(scores), nil
}

// transportError separates a call that ran out of time from a call that never
// reached the service. The two need different answers from an operator.
func (r *DashScopeReranker) transportError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return fmt.Errorf("%w: the rerank service did not answer inside the configured timeout: %v", ErrRerankTimeout, err)
	}
	return fmt.Errorf("%w: request: %v", ErrRerankUnavailable, err)
}

// statusError maps a refused request to the reason an operator needs. The
// service's own code and message are quoted, bounded, because they name the
// actual problem - a wrong model name, an exhausted quota - and never carry
// the credential.
func (r *DashScopeReranker) statusError(status int, body io.Reader) error {
	payload, _ := io.ReadAll(io.LimitReader(body, maxRemoteErrorBytes))
	var failure struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(payload, &failure)
	detail := boundText(strings.TrimSpace(failure.Code + " " + failure.Message))
	switch {
	case status == http.StatusBadRequest:
		return fmt.Errorf("%w: the rerank service refused the request (status %d): %s; check the rerank model name and the endpoint path", ErrRerankConfig, status, detail)
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return fmt.Errorf("%w: the rerank service refused the credential (status %d): %s", ErrRerankRejected, status, detail)
	case status == http.StatusTooManyRequests:
		return fmt.Errorf("%w: the rerank service is throttling this deployment (status %d): %s", ErrRerankRateLimited, status, detail)
	case status >= http.StatusInternalServerError:
		return fmt.Errorf("%w: the rerank service failed (status %d): %s", ErrRerankUnavailable, status, detail)
	default:
		return fmt.Errorf("%w: the rerank service answered with status %d: %s", ErrRerankUnavailable, status, detail)
	}
}

// boundText keeps a quoted service message short enough for a terminal.
func boundText(text string) string {
	text = strings.TrimSpace(text)
	if len(text) > 200 {
		return text[:200]
	}
	return text
}
