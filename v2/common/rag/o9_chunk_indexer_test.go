package rag

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/embedding"
)

type o9Embedder struct {
	active  atomic.Int32
	maximum atomic.Int32
	started atomic.Int32
	delay   time.Duration
	mu      sync.Mutex
}

func (e *o9Embedder) EmbedStrings(ctx context.Context, texts []string, _ ...embedding.Option) ([][]float64, error) {
	active := e.active.Add(1)
	for old := e.maximum.Load(); active > old && !e.maximum.CompareAndSwap(old, active); old = e.maximum.Load() {
	}
	e.started.Add(1)
	defer e.active.Add(-1)
	for _, text := range texts {
		if text == "o9-failing-batch" {
			// Let every sibling enter the provider before this batch fails.
			for e.started.Load() < 5 {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(time.Millisecond):
				}
			}
			return nil, errors.New("simulated batch error")
		}
	}
	if e.delay > 0 {
		timer := time.NewTimer(e.delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
		}
	} else if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make([][]float64, len(texts))
	for i, text := range texts {
		vector := deterministicVector(text, 4)
		out[i] = make([]float64, len(vector))
		for j := range vector {
			out[i][j] = float64(vector[j])
		}
	}
	return out, nil
}

func TestO9ConcurrentBatchesMatchSerial(t *testing.T) {
	chunks := testIndexChunks(t, "o9-order", 9)
	var serial [][]float32
	for _, concurrency := range []int{1, 4, 8} {
		embedder := &o9Embedder{}
		repository := &fakeChunkVectorRepository{}
		limiter, err := NewEmbeddingSemaphore(concurrency)
		if err != nil {
			t.Fatal(err)
		}
		config := testIndexConfig(4, 1)
		config.Limiter = limiter
		indexer := mustKnowledgeChunkIndexer(t, embedder, repository, config)
		if _, err = indexer.Index(context.Background(), ChunkIndexRequest{UserID: 42, Chunks: chunks}); err != nil {
			t.Fatalf("concurrency %d: Index() error: %v", concurrency, err)
		}
		vectors := make([][]float32, len(repository.stored))
		for i := range repository.stored {
			vectors[i] = append([]float32(nil), repository.stored[i].Vector...)
			if repository.stored[i].Chunk.Index != i {
				t.Fatalf("concurrency %d: stored row %d has chunk index %d", concurrency, i, repository.stored[i].Chunk.Index)
			}
		}
		if concurrency == 1 {
			serial = vectors
			continue
		}
		if !sameVectorBytes(vectors, serial) {
			t.Fatalf("concurrency %d changed vector bytes or order", concurrency)
		}
	}
}

func TestO9NodeSemaphoreBoundsInFlight(t *testing.T) {
	limiter, err := NewEmbeddingSemaphore(3)
	if err != nil {
		t.Fatal(err)
	}
	embedder := &o9Embedder{delay: 15 * time.Millisecond}
	var wg sync.WaitGroup
	for doc := 0; doc < 2; doc++ {
		doc := doc
		wg.Add(1)
		go func() {
			defer wg.Done()
			repository := &fakeChunkVectorRepository{}
			config := testIndexConfig(4, 1)
			config.Limiter = limiter
			indexer := mustKnowledgeChunkIndexer(t, embedder, repository, config)
			if _, indexErr := indexer.Index(context.Background(), ChunkIndexRequest{UserID: 42, Chunks: testIndexChunks(t, fmt.Sprintf("o9-doc-%d", doc), 5)}); indexErr != nil {
				t.Errorf("Index() error: %v", indexErr)
			}
		}()
	}
	wg.Wait()
	if got := embedder.maximum.Load(); got > 3 || got < 2 {
		t.Fatalf("node max in-flight=%d, want 2..3", got)
	}
}

func TestO9FailureCancelsSiblingsAndStoresNothing(t *testing.T) {
	chunks := testIndexChunks(t, "o9-cancel-siblings", 5)
	chunks[2].EmbeddingContent = "o9-failing-batch"
	limiter, err := NewEmbeddingSemaphore(5)
	if err != nil {
		t.Fatal(err)
	}
	embedder := &o9Embedder{}
	repository := &fakeChunkVectorRepository{}
	config := testIndexConfig(4, 1)
	config.Limiter = limiter
	indexer := mustKnowledgeChunkIndexer(t, embedder, repository, config)
	_, err = indexer.Index(context.Background(), ChunkIndexRequest{UserID: 42, Chunks: chunks})
	if !errors.Is(err, ErrEmbeddingBatch) || !bytes.Contains([]byte(err.Error()), []byte("2-2")) {
		t.Fatalf("Index() error=%v, want failed batch 2-2", err)
	}
	if repository.storeCalls != 0 || embedder.active.Load() != 0 || embedder.started.Load() != 5 {
		t.Fatalf("after return stores=%d active=%d started=%d", repository.storeCalls, embedder.active.Load(), embedder.started.Load())
	}
}

func TestO9ParentCancelStopsAllBatches(t *testing.T) {
	limiter, err := NewEmbeddingSemaphore(4)
	if err != nil {
		t.Fatal(err)
	}
	embedder := &o9ParentCancelEmbedder{entered: make(chan struct{}, 4)}
	repository := &fakeChunkVectorRepository{}
	config := testIndexConfig(4, 1)
	config.Limiter = limiter
	indexer := mustKnowledgeChunkIndexer(t, embedder, repository, config)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, indexErr := indexer.Index(ctx, ChunkIndexRequest{UserID: 42, Chunks: testIndexChunks(t, "o9-parent-cancel", 4)})
		done <- indexErr
	}()
	for i := 0; i < 4; i++ {
		select {
		case <-embedder.entered:
		case <-time.After(time.Second):
			t.Fatal("not all batches entered provider")
		}
	}
	cancel()
	select {
	case err = <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Index() error=%v, want context canceled", err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("Index() did not return promptly after parent cancellation")
	}
	if embedder.active.Load() != 0 || repository.storeCalls != 0 {
		t.Fatalf("after parent cancel active=%d store_calls=%d", embedder.active.Load(), repository.storeCalls)
	}
}

type o9ParentCancelEmbedder struct {
	entered chan struct{}
	active  atomic.Int32
}

func (e *o9ParentCancelEmbedder) EmbedStrings(ctx context.Context, _ []string, _ ...embedding.Option) ([][]float64, error) {
	e.active.Add(1)
	defer e.active.Add(-1)
	e.entered <- struct{}{}
	<-ctx.Done()
	return nil, ctx.Err()
}

func sameVectorBytes(left, right [][]float32) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if len(left[i]) != len(right[i]) {
			return false
		}
		for j := range left[i] {
			if math.Float32bits(left[i][j]) != math.Float32bits(right[i][j]) {
				return false
			}
		}
	}
	return true
}
