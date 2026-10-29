package memoryrecall

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestCycleBuysTheQueryVectorOnce(t *testing.T) {
	calls := 0
	embed := func(context.Context, string) ([]float32, error) {
		calls++
		return []float32{1, 2}, nil
	}
	cycle := New("上次关于缓冲区复用的决定是什么？")
	first, err := cycle.QueryVector(context.Background(), "上次关于缓冲区复用的决定是什么？", embed)
	if err != nil || len(first) != 2 {
		t.Fatalf("first=%v err=%v", first, err)
	}
	second, err := cycle.QueryVector(context.Background(), "上次关于缓冲区复用的决定是什么？", embed)
	if err != nil || len(second) != 2 {
		t.Fatalf("second=%v err=%v", second, err)
	}
	if calls != 1 || cycle.EmbeddingCalls() != 1 {
		t.Fatalf("provider calls=%d ledger=%d", calls, cycle.EmbeddingCalls())
	}
}

// A failure is memoised too. The provider that just refused this question will
// refuse it again inside the same turn, and retrying only spends budget.
func TestCycleMemoisesTheFailureAndCountsIt(t *testing.T) {
	calls := 0
	embed := func(context.Context, string) ([]float32, error) {
		calls++
		return nil, errors.New("provider down")
	}
	cycle := New("问题")
	if _, err := cycle.QueryVector(context.Background(), "问题", embed); err == nil {
		t.Fatal("expected the provider error")
	}
	if _, err := cycle.QueryVector(context.Background(), "问题", embed); err == nil {
		t.Fatal("expected the memoised error")
	}
	if calls != 1 || cycle.EmbeddingCalls() != 1 {
		t.Fatalf("provider calls=%d ledger=%d", calls, cycle.EmbeddingCalls())
	}
}

// The memoised vector belongs to one question. Serving it for different text
// would silently answer a query nobody asked.
func TestCycleDoesNotServeAnotherQuestionsVector(t *testing.T) {
	seen := []string{}
	embed := func(_ context.Context, text string) ([]float32, error) {
		seen = append(seen, text)
		return []float32{1}, nil
	}
	cycle := New("问题一")
	if _, err := cycle.QueryVector(context.Background(), "问题一", embed); err != nil {
		t.Fatal(err)
	}
	if _, err := cycle.QueryVector(context.Background(), "问题二", embed); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen[0] != "问题一" || seen[1] != "问题二" {
		t.Fatalf("provider saw %v", seen)
	}
}

// Callers built without a cycle keep working: the nil receiver falls straight
// through to the provider rather than panicking or silently returning nothing.
func TestNilCycleFallsThroughToTheProvider(t *testing.T) {
	var cycle *Cycle
	calls := 0
	vector, err := cycle.QueryVector(context.Background(), "问题", func(context.Context, string) ([]float32, error) {
		calls++
		return []float32{9}, nil
	})
	if err != nil || len(vector) != 1 || calls != 1 {
		t.Fatalf("vector=%v err=%v calls=%d", vector, err, calls)
	}
	cycle.SetEpisodeTopicKeys([]string{"x"})
	cycle.SetProposedTopicKeys([]string{"y"})
	cycle.Reason("z")
	if cycle.EpisodeTopicKeys() != nil || cycle.EmbeddingCalls() != 0 || cycle.Reasons() != nil {
		t.Fatal("a nil cycle must stay empty")
	}
	if keys, ok := cycle.ProposedTopicKeys(); ok || keys != nil {
		t.Fatal("a nil cycle must not report a proposal")
	}
}

// The two readers can run concurrently in a future revision; the ledger must
// still count one call.
func TestCycleIsSafeUnderConcurrentReaders(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	embed := func(context.Context, string) ([]float32, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return []float32{1}, nil
	}
	cycle := New("问题")
	var wait sync.WaitGroup
	for i := 0; i < 8; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, _ = cycle.QueryVector(context.Background(), "问题", embed)
		}()
	}
	wait.Wait()
	if calls != 1 || cycle.EmbeddingCalls() != 1 {
		t.Fatalf("provider calls=%d ledger=%d", calls, cycle.EmbeddingCalls())
	}
}

// Recorded keys are copies. A caller mutating what it passed in, or what it got
// back, must not reach into another reader's view of the same turn.
func TestCycleCopiesTheKeysItRecords(t *testing.T) {
	cycle := New("问题")
	keys := []string{"a", "b"}
	cycle.SetEpisodeTopicKeys(keys)
	keys[0] = "mutated"
	got := cycle.EpisodeTopicKeys()
	if got[0] != "a" {
		t.Fatalf("stored keys=%v", got)
	}
	got[1] = "mutated"
	if cycle.EpisodeTopicKeys()[1] != "b" {
		t.Fatalf("returned slice aliases the stored one")
	}
}
