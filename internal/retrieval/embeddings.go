package retrieval

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The embedding client is the first half of a real semantic index: text goes out
// as vectors and comes back as vectors, and those vectors are what the index stores
// so a query never has to embed the whole corpus again.
//
// The wire shape is the OpenAI-compatible one the original project used - POST
// {base}/embeddings with a batched "input" array and a "data" array whose items each
// carry an index - reproduced rather than reinvented, because an endpoint that
// already speaks that shape is the one any local BGE server, vLLM deployment or
// hosted service will also speak.
//
// Two rules here are load-bearing. Vectors are placed by the reported "index"
// instead of by response order, because a batched endpoint is allowed to answer out
// of order and silently mislabelling every chunk would corrupt the index permanently
// rather than fail once. And a dimension mismatch is a startup-shaped error, not a
// per-query surprise: a stored index and a live model that disagree about width
// produce confident nonsense, so the check runs on every vector, both directions.

const (
	// DefaultEmbeddingsTimeout bounds one embedding request. It is a whole-request
	// timeout, matching how the original deployment capped a retrieval-time embed.
	DefaultEmbeddingsTimeout = 30 * time.Second
	// DefaultEmbeddingsBatchSize mirrors the original project's chunk batch: a batch
	// large enough to amortise the round trip, small enough that one slow vector does
	// not hold up an entire document.
	DefaultEmbeddingsBatchSize = 10
	// maxEmbeddingsResponseBytes caps how much of an embedding body is read. A vector
	// response is numbers; a megabyte per ten texts is already generous, and an
	// unbounded read turns a broken endpoint into an out-of-memory process.
	maxEmbeddingsResponseBytes = 16 << 20
	// embeddingsProtocol identifies the wire contract in a fingerprint. Bumping it
	// invalidates every stored vector on purpose.
	embeddingsProtocol = "openai-compatible-embeddings-v1"
)

var (
	// ErrEmbeddingsConfig reports an embedder that cannot be built. Callers must fail
	// at startup on this rather than fall back to another embedder, because a silent
	// swap would keep serving from an index whose vectors were made by a different
	// model.
	ErrEmbeddingsConfig = errors.New("retrieval: embedding configuration is invalid")
	// ErrEmbeddingsUnavailable reports that the endpoint could not be reached or
	// answered with a server-side failure. It is the only embedding error a caller may
	// treat as transient.
	ErrEmbeddingsUnavailable = errors.New("retrieval: embedding endpoint unavailable")
	// ErrEmbeddingsShape reports an answer that does not match the contract: wrong
	// count, wrong width, a non-finite number, duplicate or out-of-range index. Those
	// are not transient, and retrying them would only produce the same bad vectors.
	ErrEmbeddingsShape = errors.New("retrieval: embedding response does not match the contract")
)

// EmbeddingsOptions configures an HTTPEmbedder. Zero values select the defaults
// except BaseURL, Model and Dimensions, which are required.
type EmbeddingsOptions struct {
	BaseURL string
	APIKey  string
	Model   string
	// Dimensions is the width every vector must have. It is not probed from the first
	// response: the caller states the index it is about to write, and a mismatch stops
	// the write.
	Dimensions int
	Timeout    time.Duration
	BatchSize  int
	// Client is optional; a nil Client uses a dedicated http.Client bounded by Timeout.
	Client *http.Client
}

// HTTPEmbedder implements Embedder against an OpenAI-compatible /embeddings endpoint.
//
// Unlike DeterministicEmbedder it never normalizes on the caller's behalf: models
// that publish unit vectors would be rescaled by a second normalization, and cosine
// over unit vectors is unchanged by the first one, so the values are stored exactly
// as the model returned them.
type HTTPEmbedder struct {
	endpoint   string
	apiKey     string
	model      string
	dimensions int
	client     *http.Client
	batchSize  int
}

var _ Embedder = (*HTTPEmbedder)(nil)

func NewHTTPEmbedder(options EmbeddingsOptions) (*HTTPEmbedder, error) {
	base := strings.TrimSpace(options.BaseURL)
	if base == "" {
		return nil, fmt.Errorf("%w: base url is required", ErrEmbeddingsConfig)
	}
	endpoint, err := embeddingsEndpoint(base)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrEmbeddingsConfig, err)
	}
	model := strings.TrimSpace(options.Model)
	if model == "" {
		return nil, fmt.Errorf("%w: model is required", ErrEmbeddingsConfig)
	}
	if options.Dimensions <= 0 {
		return nil, fmt.Errorf("%w: dimensions must be positive", ErrEmbeddingsConfig)
	}
	timeout := options.Timeout
	if timeout <= 0 {
		timeout = DefaultEmbeddingsTimeout
	}
	if timeout > 10*time.Minute {
		return nil, fmt.Errorf("%w: timeout %s is longer than 10m", ErrEmbeddingsConfig, timeout)
	}
	batch := options.BatchSize
	if batch <= 0 {
		batch = DefaultEmbeddingsBatchSize
	}
	if batch > 256 {
		return nil, fmt.Errorf("%w: batch size %d exceeds 256", ErrEmbeddingsConfig, batch)
	}
	client := options.Client
	if client == nil {
		client = &http.Client{Timeout: timeout}
	} else if client.Timeout <= 0 {
		// A borrowed client without its own deadline would let one hung embedding
		// request sit until the caller's context expires, which is a much longer and
		// less predictable wait than the configured one.
		copied := *client
		copied.Timeout = timeout
		client = &copied
	}
	return &HTTPEmbedder{
		endpoint: endpoint, apiKey: strings.TrimSpace(options.APIKey), model: model,
		dimensions: options.Dimensions, client: client, batchSize: batch,
	}, nil
}

// Dimensions implements Embedder. It reports the configured width, which is the
// width every returned vector was checked against.
func (e *HTTPEmbedder) Dimensions() int { return e.dimensions }

// Model implements Embedder. The dimension is appended because a persisted index must
// be able to tell "same model name, different output width" apart from a match; that
// mistake is otherwise invisible until queries start scoring nonsense.
func (e *HTTPEmbedder) Model() string {
	return e.model + ":" + strconv.Itoa(e.dimensions)
}

// Embed implements Embedder: one vector per text, in input order, batched.
func (e *HTTPEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if ctx == nil {
		return nil, errNoContext
	}
	if len(texts) == 0 {
		// An empty batch never reaches the network: a caller that embeds nothing must
		// not be able to spend a request, and a document with no chunks is normal.
		return [][]float32{}, nil
	}
	vectors := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += e.batchSize {
		end := min(start+e.batchSize, len(texts))
		batch, err := e.embedBatch(ctx, texts[start:end])
		if err != nil {
			return nil, err
		}
		if len(batch) != end-start {
			return nil, fmt.Errorf("%w: batch returned %d vectors for %d texts",
				ErrEmbeddingsShape, len(batch), end-start)
		}
		vectors = append(vectors, batch...)
	}
	return vectors, nil
}

func (e *HTTPEmbedder) embedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(map[string]any{
		"model":           e.model,
		"input":           texts,
		"encoding_format": "float",
	})
	if err != nil {
		return nil, fmt.Errorf("%w: request could not be encoded: %v", ErrEmbeddingsShape, err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("%w: request could not be built", ErrEmbeddingsShape)
	}
	request.Header.Set("content-type", "application/json")
	request.Header.Set("accept", "application/json")
	if e.apiKey != "" {
		request.Header.Set("authorization", "Bearer "+e.apiKey)
	}
	response, err := e.client.Do(request)
	if err != nil {
		// The transport error text can contain the URL, so it is classified and dropped
		// rather than wrapped: an embedding endpoint's address does not belong in a log
		// line next to a user's question.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("%w: transport failed", ErrEmbeddingsUnavailable)
	}
	defer func() { _ = response.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(response.Body, maxEmbeddingsResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("%w: response could not be read", ErrEmbeddingsUnavailable)
	}
	if response.StatusCode >= http.StatusInternalServerError {
		return nil, fmt.Errorf("%w: endpoint returned %d", ErrEmbeddingsUnavailable, response.StatusCode)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		// Only the status is kept. A provider-authored message can echo the request,
		// and the request contains the caller's document text.
		return nil, fmt.Errorf("%w: endpoint returned %d", ErrEmbeddingsShape, response.StatusCode)
	}
	if ctype := response.Header.Get("content-type"); ctype != "" && !strings.Contains(ctype, "application/json") {
		return nil, fmt.Errorf("%w: content type %q is not json", ErrEmbeddingsShape, ctype)
	}
	return decodeEmbeddings(body, e.dimensions)
}

// decodeEmbeddings validates the response against the contract and reorders it by the
// reported index.
//
// The envelope is read leniently on purpose: a compliant /embeddings answer also carries
// object, created, model and usage, and refusing unknown keys at that level would fail
// every working endpoint. What is refused instead is anything that makes a vector
// ambiguous - a missing index, a wrong width, a non-finite value, a duplicate or hole in
// the batch - and any content after the object, which would mean the answer was not the
// one JSON value that was asked for.
func decodeEmbeddings(body []byte, dimensions int) ([][]float32, error) {
	var decoded struct {
		Data []struct {
			Index     *int      `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&decoded); err != nil {
		return nil, fmt.Errorf("%w: body is not a valid embeddings object", ErrEmbeddingsShape)
	}
	if err := decoder.Decode(new(json.RawMessage)); err != io.EOF {
		return nil, fmt.Errorf("%w: content followed the embeddings object", ErrEmbeddingsShape)
	}
	if len(decoded.Data) == 0 {
		return nil, fmt.Errorf("%w: no vectors were returned", ErrEmbeddingsShape)
	}
	vectors := make([][]float32, len(decoded.Data))
	seen := make([]bool, len(decoded.Data))
	for position, item := range decoded.Data {
		if item.Index == nil {
			return nil, fmt.Errorf("%w: vector %d carries no index", ErrEmbeddingsShape, position)
		}
		index := *item.Index
		if index < 0 || index >= len(decoded.Data) {
			return nil, fmt.Errorf("%w: index %d is outside 0..%d", ErrEmbeddingsShape, index, len(decoded.Data)-1)
		}
		if seen[index] {
			return nil, fmt.Errorf("%w: index %d appeared twice", ErrEmbeddingsShape, index)
		}
		seen[index] = true
		if len(item.Embedding) != dimensions {
			return nil, fmt.Errorf("%w: vector %d has %d dimensions, index holds %d",
				ErrEmbeddingsShape, index, len(item.Embedding), dimensions)
		}
		for _, value := range item.Embedding {
			if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
				return nil, fmt.Errorf("%w: vector %d carries a non-finite value", ErrEmbeddingsShape, index)
			}
		}
		vectors[index] = item.Embedding
	}
	for index, present := range seen {
		if !present {
			return nil, fmt.Errorf("%w: vector %d is missing", ErrEmbeddingsShape, index)
		}
	}
	return vectors, nil
}

// embeddingsEndpoint resolves the absolute URL. A base that already names the
// endpoint is used as given, so an operator can point at a gateway path.
func embeddingsEndpoint(raw string) (string, error) {
	trimmed := strings.TrimRight(strings.TrimSpace(raw), "/")
	if strings.HasSuffix(trimmed, "/embeddings") {
		return validateEndpointURL(trimmed)
	}
	return validateEndpointURL(trimmed + "/embeddings")
}

func validateEndpointURL(candidate string) (string, error) {
	if !strings.HasPrefix(candidate, "http://") && !strings.HasPrefix(candidate, "https://") {
		return "", fmt.Errorf("base url must be http or https")
	}
	if strings.Contains(candidate, "://") && strings.Count(candidate, "://") > 1 {
		return "", fmt.Errorf("base url must contain one scheme")
	}
	rest := candidate[strings.Index(candidate, "://")+3:]
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		// Credentials in a URL are rejected outright: they would be logged by every
		// proxy between here and the endpoint.
		return "", fmt.Errorf("base url must not carry credentials")
	}
	if rest == "" || strings.ContainsAny(rest, " \t\n") {
		return "", fmt.Errorf("base url must name a host")
	}
	return candidate, nil
}

// VectorIdentity fingerprints everything that makes a stored vector comparable:
// the wire protocol, the model, the width and the version of the text that was
// embedded. The index keeps it beside each vector and refuses to score a query
// against vectors whose identity differs from the live embedder's, which turns a
// model swap from a silent accuracy collapse into an explicit re-index.
func VectorIdentity(embedder Embedder, contentVersion string) string {
	if embedder == nil {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{
		embeddingsProtocol, embedder.Model(), strconv.Itoa(embedder.Dimensions()), contentVersion,
	}, "\x1e")))
	return hex.EncodeToString(sum[:])
}
