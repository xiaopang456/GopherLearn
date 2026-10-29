package topicconsent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// stubEmbedder returns fixed vectors so every semantic assertion is
// deterministic and no provider is contacted.
type stubEmbedder struct {
	vectors map[string][]float64
	calls   int
	err     error
	// captured records exactly what text was sent, which is how the descriptor
	// policy is asserted rather than assumed.
	captured []string
}

func (s *stubEmbedder) Embed(_ context.Context, texts []string) ([][]float64, error) {
	s.calls++
	s.captured = append(s.captured, texts...)
	if s.err != nil {
		return nil, s.err
	}
	out := make([][]float64, 0, len(texts))
	for _, text := range texts {
		if vector, ok := s.vectors[text]; ok {
			out = append(out, vector)
			continue
		}
		matched := false
		for key, vector := range s.vectors {
			if strings.Contains(text, key) {
				out = append(out, vector)
				matched = true
				break
			}
		}
		if !matched {
			out = append(out, []float64{0, 0, 1})
		}
	}
	return out, nil
}

func semanticTopics() []SemanticTopic {
	return []SemanticTopic{
		{Topic: Topic{Key: "rpc-粘包处理", Name: "RPC 粘包处理", Graded: true}, LearningExamples: []string{"PacketBuffer 如何判断一条完整消息已经到达？"}},
		{Topic: Topic{Key: "redis-ttl", Name: "Redis TTL", Graded: true}, LearningExamples: []string{"惰性删除和定期删除分别在什么时候触发？"}},
		{Topic: Topic{Key: "jwt-鉴权", Name: "JWT 鉴权", Notes: 2}},
	}
}

func newSemanticFixture(t *testing.T, config SemanticSuggesterConfig, embedder *stubEmbedder) *SemanticSuggester {
	t.Helper()
	topics := semanticTopics()
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
	suggester, err := NewSemanticSuggester(lexical, SemanticTopicListerFunc(
		func(_ context.Context, userID int64) ([]SemanticTopic, error) {
			if userID != 7 {
				// Another learner's catalogue, deliberately overlapping in
				// wording so a leak would be observable.
				return []SemanticTopic{{Topic: Topic{Key: "other-用户主题", Name: "别人的主题"}}}, nil
			}
			return topics, nil
		}), embedder, config)
	if err != nil {
		t.Fatal(err)
	}
	return suggester
}

// The whole point of v1.1: a question that shares no wording with the topic
// still reaches the list, which is the case suggestion_v1 returned empty on.
func TestSemanticRecallFindsATopicLexicalMatchingMisses(t *testing.T) {
	embedder := &stubEmbedder{vectors: map[string][]float64{
		"固定头":       {1, 0, 0},
		"rpc-粘包处理":  {1, 0, 0},
		"redis-ttl": {0, 1, 0},
		"jwt-鉴权":    {0, 0.2, 0.9},
	}}
	suggester := newSemanticFixture(t, DefaultSemanticSuggesterConfig(), embedder)
	result, err := suggester.Suggest(context.Background(), 7, "固定头已经读完后，如何根据长度字段判断消息体是否收齐？")
	if err != nil {
		t.Fatal(err)
	}
	if result.LexicalHits != 0 {
		t.Fatalf("the lexical stage should find nothing here, got %d", result.LexicalHits)
	}
	if len(result.Candidates) == 0 || result.Candidates[0].TopicKey != "rpc-粘包处理" {
		t.Fatalf("candidates = %+v", result.Candidates)
	}
	if result.EmbeddingCalls != 1 {
		t.Fatalf("embedding calls = %d, want exactly one batched call", result.EmbeddingCalls)
	}
}

// A question about none of the learner's topics must offer nothing, rather than
// the nearest available subject.
func TestBelowTheFloorNothingIsOffered(t *testing.T) {
	embedder := &stubEmbedder{vectors: map[string][]float64{
		"今天天气":      {1, 0, 0},
		"rpc-粘包处理":  {0, 1, 0},
		"redis-ttl": {0, 1, 0},
		"jwt-鉴权":    {0, 1, 0},
	}}
	suggester := newSemanticFixture(t, DefaultSemanticSuggesterConfig(), embedder)
	result, err := suggester.Suggest(context.Background(), 7, "今天天气怎么样？")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Candidates) != 0 {
		t.Fatalf("an unrelated question must offer nothing, got %+v", result.Candidates)
	}
	if result.RejectedBelowFloor == 0 {
		t.Fatal("the floor must be what rejected them, and must be counted")
	}
}

// The descriptor policy is the entire privacy surface, so what is sent is
// asserted directly rather than described.
func TestMinimalDescriptorSendsOnlyKeyAndName(t *testing.T) {
	embedder := &stubEmbedder{vectors: map[string][]float64{}}
	suggester := newSemanticFixture(t, DefaultSemanticSuggesterConfig(), embedder)
	if _, err := suggester.Suggest(context.Background(), 7, "固定头怎么读"); err != nil {
		t.Fatal(err)
	}
	for _, sent := range embedder.captured {
		if strings.Contains(sent, "learning_examples") || strings.Contains(sent, "PacketBuffer") {
			t.Fatalf("the minimal policy leaked learning examples: %q", sent)
		}
	}
}

func TestLearningExamplePolicySendsExactlyThoseAndNothingElse(t *testing.T) {
	embedder := &stubEmbedder{vectors: map[string][]float64{}}
	config := DefaultSemanticSuggesterConfig()
	config.Policy = DescriptorWithLearningExamples
	suggester := newSemanticFixture(t, config, embedder)
	if _, err := suggester.Suggest(context.Background(), 7, "固定头怎么读"); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, sent := range embedder.captured {
		if strings.Contains(sent, "PacketBuffer") {
			found = true
		}
		// Never, under either policy: an assessment, a weak point, an analogy
		// or the text of a long-term memory.
		for _, banned := range []string{"掌握", "薄弱", "误区", "mastery", "weak_point", "note_content"} {
			if strings.Contains(sent, banned) {
				t.Fatalf("descriptor carried banned content %q: %s", banned, sent)
			}
		}
	}
	if !found {
		t.Fatal("the learning-example policy must actually send them")
	}
}

// Descriptor is what the authorisation request quotes, so it must be exactly
// what Suggest sends.
func TestDescriptorRendersWhatIsSent(t *testing.T) {
	topic := semanticTopics()[0]
	minimal := Descriptor(DescriptorKeyAndName, topic)
	if strings.Contains(minimal, "learning_examples") {
		t.Fatalf("minimal descriptor = %q", minimal)
	}
	if !strings.Contains(minimal, "topic_key: rpc-粘包处理") || !strings.Contains(minimal, "topic_name: RPC 粘包处理") {
		t.Fatalf("minimal descriptor = %q", minimal)
	}
	rich := Descriptor(DescriptorWithLearningExamples, topic)
	if !strings.Contains(rich, "learning_examples: PacketBuffer") {
		t.Fatalf("rich descriptor = %q", rich)
	}
}

// The embedding may only widen the list. It must never authorise a read or
// return a topic the current user does not own.
func TestSemanticStageNeverReturnsAnotherUsersTopic(t *testing.T) {
	embedder := &stubEmbedder{vectors: map[string][]float64{
		"别人的主题": {1, 0, 0},
		"固定头":   {1, 0, 0},
	}}
	suggester := newSemanticFixture(t, DefaultSemanticSuggesterConfig(), embedder)
	result, err := suggester.Suggest(context.Background(), 7, "固定头怎么读")
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range result.Candidates {
		if candidate.TopicKey == "other-用户主题" {
			t.Fatal("another learner's topic reached the candidate list")
		}
	}
}

func TestNeverExceedsTheCeiling(t *testing.T) {
	embedder := &stubEmbedder{vectors: map[string][]float64{}}
	config := DefaultSemanticSuggesterConfig()
	config.Limit = 2
	suggester := newSemanticFixture(t, config, embedder)
	result, err := suggester.Suggest(context.Background(), 7, "rpc 粘包 redis ttl jwt 鉴权")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Candidates) > 2 {
		t.Fatalf("returned %d candidates over a limit of 2", len(result.Candidates))
	}
}

// A provider outage costs recall, never the turn.
func TestEmbeddingFailureKeepsTheLexicalCandidates(t *testing.T) {
	embedder := &stubEmbedder{err: errors.New("provider down")}
	suggester := newSemanticFixture(t, DefaultSemanticSuggesterConfig(), embedder)
	result, err := suggester.Suggest(context.Background(), 7, "jwt 鉴权怎么做")
	if err != nil {
		t.Fatalf("an embedding outage must not fail the suggestion: %v", err)
	}
	if len(result.Candidates) == 0 {
		t.Fatal("the lexical candidates must survive the outage")
	}
}

func TestConfigurationIsValidatedBeforeUse(t *testing.T) {
	base := DefaultSemanticSuggesterConfig()
	if err := base.Validate(); err != nil {
		t.Fatalf("the frozen default must be valid: %v", err)
	}
	for name, mutate := range map[string]func(*SemanticSuggesterConfig){
		"unknown policy": func(c *SemanticSuggesterConfig) { c.Policy = "everything" },
		"limit over max": func(c *SemanticSuggesterConfig) { c.Limit = MaxCandidates + 1 },
		"zero floor":     func(c *SemanticSuggesterConfig) { c.MinCosine = 0 },
		"floor above 1":  func(c *SemanticSuggesterConfig) { c.MinCosine = 1 },
	} {
		config := DefaultSemanticSuggesterConfig()
		mutate(&config)
		if err := config.Validate(); err == nil {
			t.Fatalf("%s must be refused", name)
		}
	}
}

// The default is the minimal policy: the richer one is a deliberate, authorised
// choice, never something a caller gets by forgetting to set it.
func TestDefaultPolicyIsTheMinimalOne(t *testing.T) {
	if DefaultSemanticSuggesterConfig().Policy != DescriptorKeyAndName {
		t.Fatal("the default descriptor policy must send the least")
	}
}

// BUG-AGENT-201 regression. The provider rejects a batch over twenty inputs, so
// a suggester that sent the question plus every descriptor in one request
// failed outright for any learner with twenty or more topics - which is the
// ordinary case. All forty calls of the first authorised run returned HTTP 400
// and produced no vector at all.
func TestEmbeddingRequestsRespectTheProviderBatchLimit(t *testing.T) {
	topics := make([]SemanticTopic, 0, 40)
	plain := make([]Topic, 0, 40)
	for i := 0; i < 40; i++ {
		topic := Topic{Key: fmt.Sprintf("topic-%02d", i), Name: fmt.Sprintf("Alpha %02d", i), Notes: 1}
		topics = append(topics, SemanticTopic{Topic: topic})
		plain = append(plain, topic)
	}
	var sizes []int
	embedder := &recordingBatchEmbedder{onBatch: func(n int) { sizes = append(sizes, n) }}

	lexical, err := NewSuggester(TopicListerFunc(func(context.Context, int64) ([]Topic, error) {
		return plain, nil
	}), MaxCandidates)
	if err != nil {
		t.Fatal(err)
	}
	config := DefaultSemanticSuggesterConfig()
	suggester, err := NewSemanticSuggester(lexical, SemanticTopicListerFunc(
		func(context.Context, int64) ([]SemanticTopic, error) { return topics, nil }), embedder, config)
	if err != nil {
		t.Fatal(err)
	}
	result, err := suggester.Suggest(context.Background(), 7, "完全不沾边的提问内容")
	if err != nil {
		t.Fatalf("suggest: %v", err)
	}
	if len(sizes) == 0 {
		t.Fatal("no embedding request was made")
	}
	// The limit is written here as the provider's own documented ceiling rather
	// than read from the config, so this test compiles and fails against the
	// unfixed implementation instead of failing to build.
	const providerBatchLimit = 20
	for index, size := range sizes {
		if size > providerBatchLimit {
			t.Fatalf("request %d carried %d inputs, over the provider limit of %d", index, size, providerBatchLimit)
		}
	}
	// 41 texts at 20 per request is three requests, and the reported count must
	// match reality so a call budget can be reconciled against it.
	if result.EmbeddingCalls != len(sizes) {
		t.Fatalf("reported %d calls, made %d", result.EmbeddingCalls, len(sizes))
	}
	total := 0
	for _, size := range sizes {
		total += size
	}
	if total != len(topics)+1 {
		t.Fatalf("embedded %d texts, want %d", total, len(topics)+1)
	}
}

// recordingBatchEmbedder reports the size of every batch it is handed and
// returns well-formed vectors, so the test observes batching alone.
type recordingBatchEmbedder struct {
	onBatch func(int)
}

func (r *recordingBatchEmbedder) Embed(_ context.Context, texts []string) ([][]float64, error) {
	if r.onBatch != nil {
		r.onBatch(len(texts))
	}
	out := make([][]float64, 0, len(texts))
	for range texts {
		out = append(out, []float64{0, 0, 1})
	}
	return out, nil
}
