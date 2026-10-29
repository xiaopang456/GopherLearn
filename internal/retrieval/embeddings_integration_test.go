package retrieval

import (
	"context"
	"math"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// This is the only place the embedding client is pointed at a real service, and it is
// gated the way the MySQL and RediSearch suites are: an ordinary `go test ./...` exercises
// the client against an httptest fake and never opens a socket to anything outside the
// machine. Set GOPHERLEARN_EMBEDDING_BASE_URL plus GOPHERLEARN_EMBEDDING_MODEL (and
// GOPHERLEARN_EMBEDDING_API_KEY if the service needs one) and the same client is asked the
// questions a placeholder cannot answer.
//
// Nothing here prints a vector. The assertions are about shape, order and relative
// similarity, which is all a retrieval layer is entitled to assume from an embedding
// endpoint; whether the resulting ranking is *good* is a different measurement, on a
// different corpus, with a human deciding what counts as relevant.
func TestEmbedderAgainstARealEndpoint(t *testing.T) {
	base := strings.TrimSpace(os.Getenv("GOPHERLEARN_EMBEDDING_BASE_URL"))
	model := strings.TrimSpace(os.Getenv("GOPHERLEARN_EMBEDDING_MODEL"))
	rawDimensions := strings.TrimSpace(os.Getenv("GOPHERLEARN_EMBEDDING_DIMENSIONS"))
	if base == "" || model == "" || rawDimensions == "" {
		t.Skip("set GOPHERLEARN_EMBEDDING_BASE_URL, GOPHERLEARN_EMBEDDING_MODEL and GOPHERLEARN_EMBEDDING_DIMENSIONS to run the embedding suite")
	}
	// The width has to be declared, not discovered, because that is what the index is built
	// for: a service that answers with a different width than the deployment configured is a
	// startup problem, and the client refuses its vectors. This gate is where the declared
	// number is checked against the real one.
	dimensions, err := strconv.Atoi(rawDimensions)
	if err != nil || dimensions <= 0 {
		t.Fatalf("GOPHERLEARN_EMBEDDING_DIMENSIONS = %q, want a positive integer", rawDimensions)
	}
	embedder, err := NewHTTPEmbedder(EmbeddingsOptions{
		BaseURL: base, APIKey: os.Getenv("GOPHERLEARN_EMBEDDING_API_KEY"), Model: model,
		Dimensions: dimensions, Timeout: 60 * time.Second, BatchSize: 3,
	})
	if err != nil {
		t.Fatalf("NewHTTPEmbedder: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	texts := []string{
		"用 sync.WaitGroup 等待一组 goroutine 全部结束，Add 登记数量，Done 递减，Wait 阻塞到归零。",
		"Nginx 的 upstream 块把请求分发到多台后端实例，可以按权重或最少连接选择。",
		"learner 的笔记里如果只有标题和一句话，切片就不足以支撑出题。",
	}
	vectors, err := embedder.Embed(ctx, texts)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vectors) != len(texts) {
		t.Fatalf("Embed returned %d vectors for %d texts", len(vectors), len(texts))
	}
	width := len(vectors[0])
	if width == 0 {
		t.Fatal("the first vector is empty")
	}
	if width != dimensions {
		t.Fatalf("vectors are %d wide, but the deployment configured %d", width, dimensions)
	}
	for index, vector := range vectors {
		if len(vector) != width {
			t.Fatalf("vector %d is %d wide, vector 0 is %d wide: a mixed-width index cannot be compared",
				index, len(vector), width)
		}
		for _, value := range vector {
			if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
				t.Fatalf("vector %d carries a non-finite component", index)
			}
		}
	}

	// Order is the property the index depends on: vector i must describe text i, or every
	// stored chunk would be searchable under somebody else's meaning. The check is
	// statistical, not by id - a second embedding of the same text has to sit closer to its
	// own first version than to either of the other two topics.
	repeat, err := embedder.Embed(ctx, []string{texts[0]})
	if err != nil {
		t.Fatalf("Embed (repeat): %v", err)
	}
	if len(repeat) != 1 || len(repeat[0]) != width {
		t.Fatalf("repeat embedding returned %d vectors of width %d, want 1 of %d",
			len(repeat), len(repeat[0]), width)
	}
	sameTopic := CosineSimilarity(repeat[0], vectors[0])
	for other := range texts[1:] {
		otherTopic := CosineSimilarity(repeat[0], vectors[other+1])
		if !(sameTopic > otherTopic) {
			t.Fatalf("the same text is no closer to itself (%v) than to an unrelated text (%v, pair %d)",
				sameTopic, otherTopic, other)
		}
	}

	// The identity a batch is stored under has to change with the model, because an index
	// that mixes two models answers confidently and wrongly.
	if !strings.Contains(embedder.Model(), model) {
		t.Errorf("Model() = %q, want it to name the configured model %q", embedder.Model(), model)
	}
	if got := embedder.Dimensions(); got != width {
		t.Errorf("Dimensions() = %d, want the width the service actually returned (%d)", got, width)
	}
	if VectorIdentity(embedder, "document-chunks-v1") == "" {
		t.Error("vector identity is empty")
	}
}
