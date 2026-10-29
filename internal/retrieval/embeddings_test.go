package retrieval

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// embeddingsRecorder is the fake embedding endpoint used by every case here. It counts
// requests and keeps the parsed body of the last one, so batching, request fields and
// "no request at all" are asserted rather than assumed.
type embeddingsRecorder struct {
	server   *httptest.Server
	requests int
	lastBody struct {
		Model          string   `json:"model"`
		Input          []string `json:"input"`
		EncodingFormat string   `json:"encoding_format"`
	}
}

func newEmbeddingsRecorder(t *testing.T, handler func(http.ResponseWriter, *embeddingsRecorder)) *embeddingsRecorder {
	t.Helper()
	recorder := &embeddingsRecorder{}
	recorder.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder.requests++
		recorder.lastBody.Model = ""
		recorder.lastBody.Input = nil
		recorder.lastBody.EncodingFormat = ""
		_ = json.NewDecoder(r.Body).Decode(&recorder.lastBody)
		handler(w, recorder)
	}))
	t.Cleanup(recorder.server.Close)
	return recorder
}

// vectorsOf answers a batch with one vector per input text, each vector distinctive to
// its position so a reordered or trusted-order parse is visible.
func vectorsOf(w http.ResponseWriter, count, dimensions int, reverse bool) {
	items := make([]map[string]any, 0, count)
	for index := 0; index < count; index++ {
		vector := make([]float64, dimensions)
		vector[index] = float64(index + 1)
		items = append(items, map[string]any{"object": "embedding", "index": index, "embedding": vector})
	}
	if reverse {
		for left, right := 0, len(items)-1; left < right; left, right = left+1, right-1 {
			items[left], items[right] = items[right], items[left]
		}
	}
	w.Header().Set("content-type", "application/json")
	encoded, _ := json.Marshal(map[string]any{"object": "list", "data": items})
	_, _ = w.Write(encoded)
}

func TestNewHTTPEmbedderRejectsUnusableConfig(t *testing.T) {
	cases := []struct {
		name    string
		options EmbeddingsOptions
	}{
		{"no base url", EmbeddingsOptions{APIKey: "k", Model: "m", Dimensions: 4}},
		{"schemeless base", EmbeddingsOptions{BaseURL: "127.0.0.1:9", Model: "m", Dimensions: 4}},
		{"credentials in base", EmbeddingsOptions{BaseURL: "https://user:pass@example.invalid", Model: "m", Dimensions: 4}},
		{"no model", EmbeddingsOptions{BaseURL: "http://127.0.0.1:9", Dimensions: 4}},
		{"zero dimensions", EmbeddingsOptions{BaseURL: "http://127.0.0.1:9", Model: "m"}},
		{"negative dimensions", EmbeddingsOptions{BaseURL: "http://127.0.0.1:9", Model: "m", Dimensions: -8}},
		{"batch too wide", EmbeddingsOptions{BaseURL: "http://127.0.0.1:9", Model: "m", Dimensions: 4, BatchSize: 5000}},
		{"timeout too long", EmbeddingsOptions{BaseURL: "http://127.0.0.1:9", Model: "m", Dimensions: 4, Timeout: time.Hour}},
	}
	for _, item := range cases {
		if _, err := NewHTTPEmbedder(item.options); !errors.Is(err, ErrEmbeddingsConfig) {
			t.Errorf("%s: error = %v, want ErrEmbeddingsConfig", item.name, err)
		}
	}
}

func TestEmbedUsesOpenAICompatibleShapeAndBatches(t *testing.T) {
	recorder := newEmbeddingsRecorder(t, func(w http.ResponseWriter, r *embeddingsRecorder) {
		vectorsOf(w, len(r.lastBody.Input), 4, false)
	})
	embedder, err := NewHTTPEmbedder(EmbeddingsOptions{
		BaseURL: recorder.server.URL, APIKey: "test-key", Model: "bge-test", Dimensions: 4, BatchSize: 2,
	})
	if err != nil {
		t.Fatalf("constructor: %v", err)
	}
	if embedder.Dimensions() != 4 {
		t.Errorf("Dimensions() = %d, want 4", embedder.Dimensions())
	}
	if embedder.Model() != "bge-test:4" {
		t.Errorf("Model() = %q, want the model name to carry the width", embedder.Model())
	}

	vectors, err := embedder.Embed(context.Background(), []string{"one", "two", "three"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if recorder.requests != 2 {
		t.Fatalf("endpoint saw %d requests, want 2 for three texts with a batch of 2", recorder.requests)
	}
	if len(vectors) != 3 {
		t.Fatalf("got %d vectors, want 3", len(vectors))
	}
	// Vectors are numbered inside their own batch, so the third text is position 0 of
	// the second batch. Asserting the batch-local pattern is what proves the two
	// answers were concatenated in request order rather than sorted by anything.
	expected := [][]float32{{1, 0, 0, 0}, {0, 2, 0, 0}, {1, 0, 0, 0}}
	for index, want := range expected {
		if len(vectors[index]) != 4 {
			t.Fatalf("vector %d has %d dimensions, want 4", index, len(vectors[index]))
		}
		for position := range want {
			if vectors[index][position] != want[position] {
				t.Fatalf("vector %d = %v, want %v", index, vectors[index], want)
			}
		}
	}
	if recorder.lastBody.EncodingFormat != "float" {
		t.Errorf("encoding_format = %q, want float", recorder.lastBody.EncodingFormat)
	}
	if recorder.lastBody.Model != "bge-test" {
		t.Errorf("model = %q, want bge-test", recorder.lastBody.Model)
	}
	if len(recorder.lastBody.Input) != 1 || recorder.lastBody.Input[0] != "three" {
		t.Errorf("last batch input = %v, want only the third text", recorder.lastBody.Input)
	}
}

func TestEmbedSendsBearerOnlyWhenConfigured(t *testing.T) {
	var seen []string
	recorder := newEmbeddingsRecorder(t, func(w http.ResponseWriter, r *embeddingsRecorder) {
		vectorsOf(w, 1, 4, false)
	})
	embedder, err := NewHTTPEmbedder(EmbeddingsOptions{BaseURL: recorder.server.URL, Model: "m", Dimensions: 4})
	if err != nil {
		t.Fatalf("constructor: %v", err)
	}
	// The recorder cannot see headers, so a second server that reports them is used
	// instead of widening the helper.
	headerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("authorization"))
		vectorsOfRaw(w, 1)
	}))
	defer headerServer.Close()
	noKey, err := NewHTTPEmbedder(EmbeddingsOptions{BaseURL: headerServer.URL, Model: "m", Dimensions: 4})
	if err != nil {
		t.Fatalf("constructor: %v", err)
	}
	if _, err := noKey.Embed(context.Background(), []string{"a"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if _, err := embedder.Embed(context.Background(), []string{"a"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(seen) != 1 || seen[0] != "" {
		t.Fatalf("authorization headers = %v, want one empty value when no key is configured", seen)
	}
}

func vectorsOfRaw(w http.ResponseWriter, count int) {
	items := make([]map[string]any, 0, count)
	for index := 0; index < count; index++ {
		items = append(items, map[string]any{"index": index, "embedding": []float64{1, 2, 3, 4}})
	}
	w.Header().Set("content-type", "application/json")
	encoded, _ := json.Marshal(map[string]any{"data": items})
	_, _ = w.Write(encoded)
}

func TestEmbedReordersAnswerByReportedIndex(t *testing.T) {
	// The batch is answered last-to-first on purpose. A client that trusts response
	// order labels every chunk with another chunk's vector, and the index stays wrong
	// forever without ever failing; placing by the reported index makes that
	// unrepresentable.
	recorder := newEmbeddingsRecorder(t, func(w http.ResponseWriter, r *embeddingsRecorder) {
		vectorsOf(w, len(r.lastBody.Input), 4, true)
	})
	embedder, err := NewHTTPEmbedder(EmbeddingsOptions{
		BaseURL: recorder.server.URL, Model: "bge-test", Dimensions: 4, BatchSize: 3,
	})
	if err != nil {
		t.Fatalf("constructor: %v", err)
	}
	vectors, err := embedder.Embed(context.Background(), []string{"a", "b", "c"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	for index := range vectors {
		if vectors[index][index] != float32(index+1) {
			t.Fatalf("vector %d = %v, want the endpoint's value for that index", index, vectors[index])
		}
	}
}

func TestEmbedClassifiesEveryBrokenAnswerAsShape(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"no data", `{"data":[]}`},
		{"wrong width", `{"data":[{"index":0,"embedding":[1,2]}]}`},
		{"missing index", `{"data":[{"embedding":[1,2,3,4]}]}`},
		{"duplicate index", `{"data":[{"index":0,"embedding":[1,2,3,4]},{"index":0,"embedding":[1,2,3,4]}]}`},
		{"out of range index", `{"data":[{"index":7,"embedding":[1,2,3,4]}]}`},
		{"hole in the batch", `{"data":[{"index":0,"embedding":[1,2,3,4]},{"index":2,"embedding":[1,2,3,4]}]}`},
		{"trailing content", `{"data":[{"index":0,"embedding":[1,2,3,4]}]} extra`},
		{"not json at all", `<html>gateway</html>`},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			recorder := newEmbeddingsRecorder(t, func(w http.ResponseWriter, r *embeddingsRecorder) {
				w.Header().Set("content-type", "application/json")
				_, _ = w.Write([]byte(item.body))
			})
			embedder, err := NewHTTPEmbedder(EmbeddingsOptions{
				BaseURL: recorder.server.URL, Model: "bge-test", Dimensions: 4,
			})
			if err != nil {
				t.Fatalf("constructor: %v", err)
			}
			if _, err := embedder.Embed(context.Background(), []string{"a"}); !errors.Is(err, ErrEmbeddingsShape) {
				t.Fatalf("error = %v, want ErrEmbeddingsShape", err)
			}
		})
	}
}

func TestEmbedDistinguishesUnavailableFromRefused(t *testing.T) {
	const echoedSecret = "SECRET-ECHO-DOCUMENT-TEXT"
	for _, item := range []struct {
		status int
		want   error
	}{
		{http.StatusInternalServerError, ErrEmbeddingsUnavailable},
		{http.StatusServiceUnavailable, ErrEmbeddingsUnavailable},
		{http.StatusBadRequest, ErrEmbeddingsShape},
		{http.StatusUnauthorized, ErrEmbeddingsShape},
	} {
		recorder := newEmbeddingsRecorder(t, func(w http.ResponseWriter, r *embeddingsRecorder) {
			w.Header().Set("content-type", "application/json")
			w.WriteHeader(item.status)
			// The body quotes text that must never reach an error string: a provider
			// message can echo the document it refused.
			_, _ = w.Write([]byte(`{"error":{"message":"` + echoedSecret + `"}}`))
		})
		embedder, err := NewHTTPEmbedder(EmbeddingsOptions{
			BaseURL: recorder.server.URL, Model: "bge-test", Dimensions: 4,
		})
		if err != nil {
			t.Fatalf("constructor: %v", err)
		}
		_, err = embedder.Embed(context.Background(), []string{"a"})
		if !errors.Is(err, item.want) {
			t.Fatalf("status %d: error = %v, want %v", item.status, err, item.want)
		}
		if strings.Contains(err.Error(), echoedSecret) {
			t.Fatalf("status %d: error leaked the response body: %v", item.status, err)
		}
	}
}

func TestEmbedRejectsNonJSONContentType(t *testing.T) {
	recorder := newEmbeddingsRecorder(t, func(w http.ResponseWriter, r *embeddingsRecorder) {
		w.Header().Set("content-type", "text/plain")
		_, _ = w.Write([]byte("ok"))
	})
	embedder, err := NewHTTPEmbedder(EmbeddingsOptions{BaseURL: recorder.server.URL, Model: "m", Dimensions: 4})
	if err != nil {
		t.Fatalf("constructor: %v", err)
	}
	if _, err := embedder.Embed(context.Background(), []string{"a"}); !errors.Is(err, ErrEmbeddingsShape) {
		t.Fatalf("error = %v, want ErrEmbeddingsShape", err)
	}
}

func TestEmbedMakesNoRequestForNoText(t *testing.T) {
	recorder := newEmbeddingsRecorder(t, func(w http.ResponseWriter, r *embeddingsRecorder) {
		t.Fatal("an empty batch must not reach the endpoint")
	})
	embedder, err := NewHTTPEmbedder(EmbeddingsOptions{BaseURL: recorder.server.URL, Model: "m", Dimensions: 4})
	if err != nil {
		t.Fatalf("constructor: %v", err)
	}
	vectors, err := embedder.Embed(context.Background(), nil)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vectors) != 0 || recorder.requests != 0 {
		t.Fatalf("vectors = %d, requests = %d, want neither", len(vectors), recorder.requests)
	}
}

func TestEmbedHonoursCancelledContext(t *testing.T) {
	recorder := newEmbeddingsRecorder(t, func(w http.ResponseWriter, r *embeddingsRecorder) {
		vectorsOf(w, 1, 4, false)
	})
	embedder, err := NewHTTPEmbedder(EmbeddingsOptions{BaseURL: recorder.server.URL, Model: "m", Dimensions: 4})
	if err != nil {
		t.Fatalf("constructor: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := embedder.Embed(ctx, []string{"a"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if recorder.requests != 0 {
		t.Fatalf("requests = %d, want none after the context was already cancelled", recorder.requests)
	}
}

func TestEndpointAcceptsAFullPathWithoutDoublingIt(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		vectorsOfRaw(w, 1)
	}))
	defer server.Close()
	embedder, err := NewHTTPEmbedder(EmbeddingsOptions{BaseURL: server.URL + "/embeddings", Model: "m", Dimensions: 4})
	if err != nil {
		t.Fatalf("constructor: %v", err)
	}
	if _, err := embedder.Embed(context.Background(), []string{"a"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(paths) != 1 || paths[0] != "/embeddings" {
		t.Fatalf("paths = %v, want exactly one request to /embeddings", paths)
	}
}

func TestVectorIdentityChangesWithModelWidthAndVersion(t *testing.T) {
	base := VectorIdentity(&positionalEmbedder{vectors: [][]float32{{1, 2}}}, "v1")
	if base == "" {
		t.Fatal("identity must not be empty for a real embedder")
	}
	if VectorIdentity(nil, "v1") != "" {
		t.Error("a nil embedder has no identity")
	}
	variants := []string{
		VectorIdentity(&positionalEmbedder{vectors: [][]float32{{1, 2}}}, "v2"),
		VectorIdentity(&positionalEmbedder{vectors: [][]float32{{1, 2, 3}}}, "v1"),
		VectorIdentity(&namedEmbedder{vectors: [][]float32{{1, 2}}, model: "other-model"}, "v1"),
	}
	for index, variant := range variants {
		if variant == base {
			t.Errorf("variant %d matched the base identity; the fingerprint does not cover that axis", index)
		}
	}
	if len(base) != 64 || strings.Contains(base, "positional-test-embedder") {
		t.Errorf("identity = %q, want a hex digest rather than a readable model string", base)
	}
}

// namedEmbedder differs from positionalEmbedder only in Model(), which is the axis the
// width and version cases cannot vary.
type namedEmbedder struct {
	vectors [][]float32
	model   string
}

func (e *namedEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	if len(texts) != len(e.vectors) {
		return nil, errors.New("namedEmbedder: arity " + strconv.Itoa(len(e.vectors)))
	}
	return e.vectors, nil
}

func (e *namedEmbedder) Dimensions() int { return len(e.vectors[0]) }
func (e *namedEmbedder) Model() string   { return e.model }
