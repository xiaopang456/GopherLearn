package topicconsent

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

const (
	testModel = "qwen3.7-text-embedding"
	testDim   = 3
)

// countingEmbedder records every batch so call counts, batch sizes and cache
// hits are observed rather than assumed.
type countingEmbedder struct {
	batches [][]string
	err     error
	vectors map[string][]float64
}

func (c *countingEmbedder) Embed(_ context.Context, texts []string) ([][]float64, error) {
	c.batches = append(c.batches, append([]string(nil), texts...))
	if c.err != nil {
		return nil, c.err
	}
	out := make([][]float64, 0, len(texts))
	for _, text := range texts {
		if vector, ok := c.vectors[text]; ok {
			out = append(out, vector)
			continue
		}
		out = append(out, []float64{0, 0, 1})
	}
	return out, nil
}

func (c *countingEmbedder) calls() int {
	return len(c.batches)
}

func (c *countingEmbedder) inputs() int {
	total := 0
	for _, batch := range c.batches {
		total += len(batch)
	}
	return total
}

func cachedTopics(n int) []SemanticTopic {
	out := make([]SemanticTopic, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, SemanticTopic{
			Topic:            Topic{Key: fmt.Sprintf("alpha-%02d", i), Name: fmt.Sprintf("Alpha %02d", i), Notes: 1},
			LearningExamples: []string{fmt.Sprintf("example question %02d", i)},
		})
	}
	return out
}

func warmedCache(t *testing.T, topics []SemanticTopic, embedder Embedder, policy DescriptorPolicy, batch int) *TopicVectorCache {
	t.Helper()
	cache, err := NewTopicVectorCache(TopicVectorCacheKey{
		Model: testModel, Dimension: testDim, Policy: policy,
		CatalogueDigest: CatalogueDigest(policy, topics),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.Warm(context.Background(), embedder, topics, batch); err != nil {
		t.Fatalf("warm: %v", err)
	}
	return cache
}

// The frozen catalogue has twenty topics, and the provider refuses a batch over
// twenty. Warming must therefore fit in one request; a catalogue of twenty-one
// must split rather than fail.
func TestWarmRespectsTheBatchBoundary(t *testing.T) {
	for _, spec := range []struct {
		topics    int
		wantCalls int
	}{{20, 1}, {21, 2}, {40, 2}, {41, 3}} {
		embedder := &countingEmbedder{}
		topics := cachedTopics(spec.topics)
		cache := warmedCache(t, topics, embedder, DescriptorWithLearningExamples, 20)
		if embedder.calls() != spec.wantCalls {
			t.Fatalf("%d topics: %d calls, want %d", spec.topics, embedder.calls(), spec.wantCalls)
		}
		for index, batch := range embedder.batches {
			if len(batch) > 20 {
				t.Fatalf("%d topics: batch %d carried %d inputs", spec.topics, index, len(batch))
			}
		}
		if embedder.inputs() != spec.topics {
			t.Fatalf("%d topics: embedded %d inputs", spec.topics, embedder.inputs())
		}
		if cache.Size() != spec.topics {
			t.Fatalf("%d topics: cached %d", spec.topics, cache.Size())
		}
		if cache.Calls != spec.wantCalls || cache.Inputs != spec.topics {
			t.Fatalf("cache accounting = %d calls %d inputs", cache.Calls, cache.Inputs)
		}
	}
}

// The whole point: forty questions must not re-vectorise the catalogue. One
// warm request, then exactly one request per question.
func TestCatalogueIsVectorisedOnceForTheWholeRun(t *testing.T) {
	topics := cachedTopics(20)
	warmEmbedder := &countingEmbedder{}
	cache := warmedCache(t, topics, warmEmbedder, DescriptorWithLearningExamples, 20)
	if warmEmbedder.calls() != 1 || warmEmbedder.inputs() != 20 {
		t.Fatalf("warming cost %d calls / %d inputs", warmEmbedder.calls(), warmEmbedder.inputs())
	}

	questionEmbedder := &countingEmbedder{}
	suggester := newCachedFixture(t, topics, questionEmbedder, cache, true)
	for i := 0; i < 40; i++ {
		if _, err := suggester.Suggest(context.Background(), 7, fmt.Sprintf("完全不沾边的提问 %02d", i)); err != nil {
			t.Fatalf("question %d: %v", i, err)
		}
	}
	if questionEmbedder.calls() != 40 || questionEmbedder.inputs() != 40 {
		t.Fatalf("questions cost %d calls / %d inputs, want 40/40",
			questionEmbedder.calls(), questionEmbedder.inputs())
	}
	// One warm plus forty questions is the whole run.
	if total := warmEmbedder.calls() + questionEmbedder.calls(); total != 41 {
		t.Fatalf("run cost %d requests, want 41", total)
	}
	for _, batch := range questionEmbedder.batches {
		if len(batch) != 1 {
			t.Fatalf("a question request carried %d inputs", len(batch))
		}
	}
}

func newCachedFixture(t *testing.T, topics []SemanticTopic, embedder Embedder,
	cache *TopicVectorCache, strict bool) *CachedSemanticSuggester {
	t.Helper()
	plain := make([]Topic, 0, len(topics))
	for _, topic := range topics {
		plain = append(plain, topic.Topic)
	}
	lexical, err := NewSuggester(TopicListerFunc(func(context.Context, int64) ([]Topic, error) {
		return plain, nil
	}), MaxCandidates)
	if err != nil {
		t.Fatal(err)
	}
	config := DefaultSemanticSuggesterConfig()
	config.Policy = cache.Key().Policy
	suggester, err := NewCachedSemanticSuggester(lexical, SemanticTopicListerFunc(
		func(_ context.Context, userID int64) ([]SemanticTopic, error) {
			if userID != 7 {
				return []SemanticTopic{{Topic: Topic{Key: "other-用户主题", Name: "别人的主题"}}}, nil
			}
			return topics, nil
		}), embedder, cache, config, strict)
	if err != nil {
		t.Fatal(err)
	}
	return suggester
}

// A cache is bound to a model, a dimension, a policy and a catalogue revision.
// None of them may be crossed.
func TestCacheNeverCrossesItsIdentity(t *testing.T) {
	topics := cachedTopics(3)
	policy := DescriptorWithLearningExamples
	digest := CatalogueDigest(policy, topics)

	// A changed catalogue changes the digest, so the old key cannot warm it.
	changed := append(append([]SemanticTopic{}, topics...),
		SemanticTopic{Topic: Topic{Key: "beta-01", Name: "Beta 01"}})
	if CatalogueDigest(policy, changed) == digest {
		t.Fatal("adding a topic must change the catalogue digest")
	}
	cache, err := NewTopicVectorCache(TopicVectorCacheKey{
		Model: testModel, Dimension: testDim, Policy: policy, CatalogueDigest: digest})
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.Warm(context.Background(), &countingEmbedder{}, changed, 20); err == nil {
		t.Fatal("warming a different catalogue against a stale digest must be refused")
	}

	// The policy is part of the digest too: the same rows under another policy
	// render different descriptors.
	if CatalogueDigest(DescriptorKeyAndName, topics) == digest {
		t.Fatal("a different descriptor policy must produce a different digest")
	}

	// A cache warmed for one policy may not serve a run configured for another.
	warmed := warmedCache(t, topics, &countingEmbedder{}, DescriptorKeyAndName, 20)
	plain := []Topic{topics[0].Topic}
	lexical, err := NewSuggester(TopicListerFunc(func(context.Context, int64) ([]Topic, error) { return plain, nil }), MaxCandidates)
	if err != nil {
		t.Fatal(err)
	}
	config := DefaultSemanticSuggesterConfig()
	config.Policy = DescriptorWithLearningExamples
	if _, err := NewCachedSemanticSuggester(lexical, SemanticTopicListerFunc(
		func(context.Context, int64) ([]SemanticTopic, error) { return topics, nil }),
		&countingEmbedder{}, warmed, config, true); err == nil {
		t.Fatal("a cache warmed under another policy must be refused")
	}

	// An unwarmed cache is not usable at all.
	cold, err := NewTopicVectorCache(TopicVectorCacheKey{
		Model: testModel, Dimension: testDim, Policy: policy, CatalogueDigest: digest})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewCachedSemanticSuggester(lexical, SemanticTopicListerFunc(
		func(context.Context, int64) ([]SemanticTopic, error) { return topics, nil }),
		&countingEmbedder{}, cold, config, true); err == nil {
		t.Fatal("an unwarmed cache must be refused")
	}
	// Key fields are validated rather than defaulted.
	for name, key := range map[string]TopicVectorCacheKey{
		"no model":   {Dimension: testDim, Policy: policy, CatalogueDigest: digest},
		"no dim":     {Model: testModel, Policy: policy, CatalogueDigest: digest},
		"bad policy": {Model: testModel, Dimension: testDim, Policy: "whatever", CatalogueDigest: digest},
		"no digest":  {Model: testModel, Dimension: testDim, Policy: policy},
	} {
		if _, err := NewTopicVectorCache(key); err == nil {
			t.Fatalf("%s must be refused", name)
		}
	}
}

func TestWarmRefusesAWrongDimension(t *testing.T) {
	topics := cachedTopics(2)
	policy := DescriptorWithLearningExamples
	cache, err := NewTopicVectorCache(TopicVectorCacheKey{Model: testModel, Dimension: 1024,
		Policy: policy, CatalogueDigest: CatalogueDigest(policy, topics)})
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.Warm(context.Background(), &countingEmbedder{}, topics, 20); err == nil {
		t.Fatal("a three-dimensional vector must not satisfy a 1024-dimensional key")
	}
	if cache.Warmed() {
		t.Fatal("a failed warm must not mark the cache usable")
	}
}

// A measurement run must fail rather than quietly report lexical candidates as
// if the semantic stage had run. This is the STABILIZE-051 void-run lesson.
func TestStrictModeRefusesToDegradeSilently(t *testing.T) {
	topics := cachedTopics(3)
	cache := warmedCache(t, topics, &countingEmbedder{}, DescriptorWithLearningExamples, 20)

	strict := newCachedFixture(t, topics, &countingEmbedder{err: errors.New("provider 400")}, cache, true)
	if _, err := strict.Suggest(context.Background(), 7, "完全不沾边的提问"); err == nil {
		t.Fatal("a strict run must surface an embedding failure")
	}

	// Production keeps the forgiving behaviour: recall is lost, the turn is not.
	lenient := newCachedFixture(t, topics, &countingEmbedder{err: errors.New("provider 400")}, cache, false)
	result, err := lenient.Suggest(context.Background(), 7, "alpha 00 怎么用")
	if err != nil {
		t.Fatalf("production mode must not fail the turn: %v", err)
	}
	if len(result.Candidates) == 0 {
		t.Fatal("lexical candidates must survive the outage in production mode")
	}
}

// The cached variant must select exactly what the uncached one selects: same
// TopK, same floor, same owner re-check, same tie-break.
func TestCachedAndUncachedRankIdentically(t *testing.T) {
	topics := cachedTopics(6)
	vectors := map[string][]float64{}
	for index, topic := range topics {
		descriptor := Descriptor(DescriptorWithLearningExamples, topic)
		// Descending similarity to the question vector {1,0,0}.
		vectors[descriptor] = []float64{1 - float64(index)*0.12, float64(index) * 0.05, 0}
	}
	question := "完全不沾边的提问"
	vectors[question] = []float64{1, 0, 0}

	config := DefaultSemanticSuggesterConfig()
	config.Policy = DescriptorWithLearningExamples

	plain := make([]Topic, 0, len(topics))
	for _, topic := range topics {
		plain = append(plain, topic.Topic)
	}
	lister := SemanticTopicListerFunc(func(context.Context, int64) ([]SemanticTopic, error) { return topics, nil })
	makeLexical := func() *Suggester {
		lexical, err := NewSuggester(TopicListerFunc(func(context.Context, int64) ([]Topic, error) {
			return plain, nil
		}), MaxCandidates)
		if err != nil {
			t.Fatal(err)
		}
		return lexical
	}

	uncached, err := NewSemanticSuggester(makeLexical(), lister, &countingEmbedder{vectors: vectors}, config)
	if err != nil {
		t.Fatal(err)
	}
	uncachedResult, err := uncached.Suggest(context.Background(), 7, question)
	if err != nil {
		t.Fatal(err)
	}

	cache := warmedCache(t, topics, &countingEmbedder{vectors: vectors}, DescriptorWithLearningExamples, 20)
	cached := newCachedFixture(t, topics, &countingEmbedder{vectors: vectors}, cache, true)
	cachedResult, err := cached.Suggest(context.Background(), 7, question)
	if err != nil {
		t.Fatal(err)
	}

	if len(cachedResult.Candidates) != len(uncachedResult.Candidates) {
		t.Fatalf("cached returned %d candidates, uncached %d",
			len(cachedResult.Candidates), len(uncachedResult.Candidates))
	}
	for index := range cachedResult.Candidates {
		if cachedResult.Candidates[index].TopicKey != uncachedResult.Candidates[index].TopicKey {
			t.Fatalf("rank %d: cached=%s uncached=%s", index,
				cachedResult.Candidates[index].TopicKey, uncachedResult.Candidates[index].TopicKey)
		}
	}
	if cachedResult.RejectedBelowFloor != uncachedResult.RejectedBelowFloor {
		t.Fatalf("floor rejections differ: cached=%d uncached=%d",
			cachedResult.RejectedBelowFloor, uncachedResult.RejectedBelowFloor)
	}
}

// A cache hit is what makes the second question cheap; assert it is a hit
// rather than trusting the call count alone.
func TestCacheServesDescriptorVectorsFromMemory(t *testing.T) {
	topics := cachedTopics(4)
	cache := warmedCache(t, topics, &countingEmbedder{}, DescriptorWithLearningExamples, 20)
	for _, topic := range topics {
		if _, ok := cache.Vector(Descriptor(DescriptorWithLearningExamples, topic)); !ok {
			t.Fatalf("no cached vector for %s", topic.Key)
		}
	}
	if _, ok := cache.Vector("a descriptor that was never warmed"); ok {
		t.Fatal("an unknown descriptor must miss")
	}
}
