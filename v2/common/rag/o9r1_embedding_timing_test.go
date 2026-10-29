package rag

import (
	"GopherAI/common/stagetiming"
	"context"
	"testing"
	"time"
)

type o9r1DelayedStoreRepository struct {
	*fakeChunkVectorRepository
	delay time.Duration
}

func (r *o9r1DelayedStoreRepository) Store(ctx context.Context, chunks []EmbeddedChunk) error {
	defer stagetiming.From(ctx).Start("write")()
	timer := time.NewTimer(r.delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return r.fakeChunkVectorRepository.Store(ctx, chunks)
	}
}

func TestO9R1EmbedTimingExcludesStore(t *testing.T) {
	timer := stagetiming.New()
	ctx := stagetiming.With(context.Background(), timer)
	embedder := &o9Embedder{delay: 10 * time.Millisecond}
	repository := &o9r1DelayedStoreRepository{
		fakeChunkVectorRepository: &fakeChunkVectorRepository{},
		delay:                     200 * time.Millisecond,
	}
	indexer := mustKnowledgeChunkIndexer(t, embedder, repository, testIndexConfig(4, 1))
	if _, err := indexer.Index(ctx, ChunkIndexRequest{UserID: 42, Chunks: testIndexChunks(t, "o9r1-embed-timing", 5)}); err != nil {
		t.Fatalf("Index() error: %v", err)
	}

	embedMS := timer.Millis("embed")
	writeMS := timer.Millis("write")
	if embedMS <= 0 {
		t.Fatalf("expected a recorded embed batch duration, got %.1fms", embedMS)
	}
	if writeMS < 180 {
		t.Fatalf("write stage=%.1fms, want delayed Store to contribute at least 180ms", writeMS)
	}
	if embedMS >= (embedMS+writeMS)/2 {
		t.Fatalf("embed stage=%.1fms is not clearly below embed+write=%.1fms", embedMS, embedMS+writeMS)
	}
	t.Logf("O9-R1 stage timings embed_ms=%.1f write_ms=%.1f embed_plus_write_ms=%.1f", embedMS, writeMS, embedMS+writeMS)
}
