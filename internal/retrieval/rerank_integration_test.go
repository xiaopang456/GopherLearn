package retrieval

import (
	"context"
	"errors"
	"math"
	"os"
	"strings"
	"testing"
	"time"
)

// The reranker is gated the way the RediSearch suite is: an ordinary `go test ./...`
// exercises the protocol against a local httptest server and never needs a model. Point
// GOPHERLEARN_RERANK_ENDPOINT at a running cross-encoder - a local text-embeddings-inference
// container serving BAAI/bge-reranker-v2-m3 is what this was written against - and the same
// client is asked to do the thing it exists for: tell a passage that answers the question
// from passages that do not.
//
// This file is deliberately small. Nothing here claims the reranker improves recall; it
// checks that a real model, reached over the real protocol, orders a real Chinese question
// the way the shape of the code assumes, and that the response it sends is one this client
// can read.
func TestRerankerAgainstARealModel(t *testing.T) {
	endpoint := strings.TrimSpace(os.Getenv("GOPHERLEARN_RERANK_ENDPOINT"))
	if endpoint == "" {
		t.Skip("set GOPHERLEARN_RERANK_ENDPOINT to run the cross-encoder suite")
	}
	reranker, err := NewHTTPReranker(RerankerOptions{
		Endpoint: endpoint, AuthToken: tokenForTest(t),
		// The widest timeout the client allows. This host served the model on CPU, where
		// three short passages measured about 3s end to end; the production default (5s)
		// is therefore not generous, and a full 24-candidate re-rank on CPU exceeds any
		// timeout this client permits. That is a deployment fact, not a reason to widen the
		// guard: a second stage that cannot answer inside the turn's budget is what the
		// fused order is there for.
		Timeout: MaxRerankTimeout,
	})
	if err != nil {
		t.Fatalf("NewHTTPReranker: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if err := reranker.Health(ctx); err != nil {
		t.Fatalf("Health: %v", err)
	}
	if reranker.Model() != RerankModel {
		t.Fatalf("Model = %q, want the cross-encoder this client names: %q", reranker.Model(), RerankModel)
	}

	query := "怎么用 goroutine 等一组任务全部做完"
	passages := []string{
		"sync.WaitGroup 用于等待一组 goroutine 结束。调用 Add 登记要等待的任务数，每个任务结束时调用 Done，" +
			"主协程调用 Wait 阻塞直到计数归零。Add 必须在启动 goroutine 之前调用，否则 Wait 可能提前返回。",
		"Redis 的 RDB 快照由 SAVE 配置触发，AOF 则按 everysec 策略刷盘。两者可以同时开启，" +
			"重启时优先使用 AOF 重建数据集，因为它的丢失窗口更小。",
		"CSS 的 flex 布局里 justify-content 控制主轴对齐，align-items 控制交叉轴对齐。" +
			"嵌套 flex 容器要注意 min-width 默认为 auto 造成的溢出问题。",
	}
	ranked, err := reranker.Rerank(ctx, query, passages)
	if err != nil {
		t.Fatalf("Rerank: %v", err)
	}
	if len(ranked) != len(passages) {
		t.Fatalf("Rerank returned %d hits, want one per passage", len(ranked))
	}
	seen := make(map[int]struct{}, len(ranked))
	for _, hit := range ranked {
		if hit.Index < 0 || hit.Index >= len(passages) {
			t.Fatalf("hit index %d is outside the passage list", hit.Index)
		}
		if _, duplicate := seen[hit.Index]; duplicate {
			t.Fatalf("passage %d reported twice", hit.Index)
		}
		seen[hit.Index] = struct{}{}
		if math.IsNaN(hit.Score) || math.IsInf(hit.Score, 0) {
			t.Fatalf("passage %d scored %v, which cannot be ranked", hit.Index, hit.Score)
		}
	}
	if ranked[0].Index != 0 {
		t.Fatalf("best passage = %d (%q...), want the WaitGroup passage", ranked[0].Index,
			truncateRunes(passages[ranked[0].Index], 24))
	}
	if ranked[0].Score <= ranked[1].Score || ranked[1].Score <= ranked[2].Score {
		t.Fatalf("scores are not in descending order: %+v", ranked)
	}
	// A query with no language overlap either must still get a complete, ordered answer
	// rather than a partial one: the client's contract is one score per passage.
	english, err := reranker.Rerank(ctx, "how do I read a file line by line in Go", []string{
		"Use bufio.NewScanner on the opened file and loop over Scan, then read Text.",
		"Nginx upstream blocks distribute requests across backend instances.",
	})
	if err != nil {
		t.Fatalf("Rerank (english): %v", err)
	}
	if len(english) != 2 || english[0].Index != 0 {
		t.Fatalf("english ranking = %+v, want the scanner passage first", english)
	}
}

// TestRerankerReportsAnUnavailableModel is the other half of the gate: a deployment that
// points at an address with nothing behind it must produce ErrRerankUnavailable, which is
// what lets the hybrid retriever keep its fused order instead of failing the turn.
func TestRerankerReportsAnUnavailableModel(t *testing.T) {
	endpoint := strings.TrimSpace(os.Getenv("GOPHERLEARN_RERANK_ENDPOINT"))
	if endpoint == "" {
		t.Skip("set GOPHERLEARN_RERANK_ENDPOINT to run the cross-encoder suite")
	}
	reranker, err := NewHTTPReranker(RerankerOptions{Endpoint: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatalf("NewHTTPReranker: %v", err)
	}
	_, err = reranker.Rerank(context.Background(), "q", []string{"a"})
	if !errors.Is(err, ErrRerankUnavailable) {
		t.Fatalf("error = %v, want ErrRerankUnavailable", err)
	}
}

// tokenForTest returns the bearer token to send. A local text-embeddings-inference server
// ignores the header, so this is about the client's own rule - a token shorter than
// minRerankAuthTokenLength is treated as a typo and refused - rather than about secrecy.
func tokenForTest(t *testing.T) string {
	t.Helper()
	token := strings.TrimSpace(os.Getenv("GOPHERLEARN_RERANK_AUTH_TOKEN"))
	if token == "" {
		return strings.Repeat("local-tei-needs-no-auth", 2)[:minRerankAuthTokenLength]
	}
	return token
}

func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}
