package topicconsent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

	"GopherAI/model"
)

// The cached variant exists because the two halves of a suggestion have
// opposite lifetimes. A learner's topic catalogue is stable across a whole
// session; the question changes every turn. Vectorising the catalogue once per
// question, as the first implementation did, is not merely wasteful - it sent
// the same twenty descriptors forty times in one frozen run - it also makes the
// per-turn cost grow with how much the learner has studied, which is exactly
// backwards.

// TopicVectorCacheKey is the identity a cached vector belongs to.
//
// Every field is part of the key because every one of them changes what the
// vector means. A vector produced by another model, at another dimension, from
// another descriptor policy, or from a different catalogue revision is not a
// stale entry to be refreshed - it is a different quantity, and silently
// reusing it would compare embeddings that were never comparable.
type TopicVectorCacheKey struct {
	Model           string
	Dimension       int
	Policy          DescriptorPolicy
	CatalogueDigest string
}

func (k TopicVectorCacheKey) valid() error {
	if strings.TrimSpace(k.Model) == "" || k.Dimension <= 0 || !k.Policy.Valid() ||
		strings.TrimSpace(k.CatalogueDigest) == "" {
		return errors.New("topic vector cache key needs model, dimension, policy and catalogue digest")
	}
	return nil
}

// CatalogueDigest is the identity of one catalogue under one policy.
//
// It hashes the rendered descriptors rather than the raw rows, because the
// descriptor is what is actually embedded: a catalogue change that does not
// alter any descriptor cannot invalidate a vector, and one that does must.
func CatalogueDigest(policy DescriptorPolicy, topics []SemanticTopic) string {
	rendered := make([]string, 0, len(topics))
	for _, topic := range topics {
		rendered = append(rendered, DescriptorSHA256(Descriptor(policy, topic)))
	}
	sort.Strings(rendered)
	sum := sha256.Sum256([]byte(strings.Join(rendered, "\n")))
	return hex.EncodeToString(sum[:])
}

// DescriptorSHA256 is the per-descriptor cache key.
func DescriptorSHA256(descriptor string) string {
	sum := sha256.Sum256([]byte(descriptor))
	return hex.EncodeToString(sum[:])
}

// TopicVectorCache holds descriptor vectors for exactly one key.
type TopicVectorCache struct {
	key     TopicVectorCacheKey
	vectors map[string][]float64
	warmed  bool
	// Calls and Inputs record what warming actually cost, so a budget is
	// reconciled against requests made rather than requests assumed.
	Calls  int
	Inputs int
}

func NewTopicVectorCache(key TopicVectorCacheKey) (*TopicVectorCache, error) {
	if err := key.valid(); err != nil {
		return nil, err
	}
	return &TopicVectorCache{key: key, vectors: map[string][]float64{}}, nil
}

// Key exposes the identity so a caller can prove which one it warmed.
func (c *TopicVectorCache) Key() TopicVectorCacheKey { return c.key }

// Warmed reports whether Warm completed successfully.
func (c *TopicVectorCache) Warmed() bool { return c != nil && c.warmed }

// Warm vectorises every descriptor once, in batches within the provider limit.
//
// It returns an error rather than degrading. A run whose catalogue never got
// vectorised has no semantic stage at all, and continuing would produce a score
// that looks like a measurement of this method and is not one.
func (c *TopicVectorCache) Warm(ctx context.Context, embedder Embedder, topics []SemanticTopic, batchSize int) error {
	if c == nil {
		return errors.New("topic vector cache is not configured")
	}
	if embedder == nil {
		return errors.New("warming the topic vector cache needs an embedder")
	}
	if batchSize < 1 || batchSize > 100 {
		return fmt.Errorf("batch size %d must be between 1 and 100", batchSize)
	}
	if digest := CatalogueDigest(c.key.Policy, topics); digest != c.key.CatalogueDigest {
		return fmt.Errorf("catalogue digest %s does not match the cache key %s", digest, c.key.CatalogueDigest)
	}
	// Distinct descriptors only: two topics that render identically are one
	// vector, and the cache is keyed on the descriptor rather than the key.
	order := make([]string, 0, len(topics))
	seen := map[string]struct{}{}
	for _, topic := range topics {
		descriptor := Descriptor(c.key.Policy, topic)
		if _, ok := seen[descriptor]; ok {
			continue
		}
		seen[descriptor] = struct{}{}
		order = append(order, descriptor)
	}
	for start := 0; start < len(order); start += batchSize {
		end := start + batchSize
		if end > len(order) {
			end = len(order)
		}
		batch := order[start:end]
		c.Calls++
		c.Inputs += len(batch)
		vectors, err := embedder.Embed(ctx, batch)
		if err != nil {
			return fmt.Errorf("warm topic vectors: %w", err)
		}
		if len(vectors) != len(batch) {
			return fmt.Errorf("embedder returned %d vectors for %d descriptors", len(vectors), len(batch))
		}
		for index, vector := range vectors {
			if len(vector) != c.key.Dimension {
				return fmt.Errorf("descriptor vector has dimension %d, want %d", len(vector), c.key.Dimension)
			}
			c.vectors[DescriptorSHA256(batch[index])] = vector
		}
	}
	c.warmed = true
	return nil
}

// Vector returns a cached descriptor vector.
func (c *TopicVectorCache) Vector(descriptor string) ([]float64, bool) {
	if c == nil || !c.warmed {
		return nil, false
	}
	vector, ok := c.vectors[DescriptorSHA256(descriptor)]
	return vector, ok
}

// Size is how many distinct descriptors are cached.
func (c *TopicVectorCache) Size() int {
	if c == nil {
		return 0
	}
	return len(c.vectors)
}

// CachedSemanticSuggester is the production shape: the catalogue is vectorised
// once, and each question is vectorised once when it arrives.
//
// Ranking, TopK, the cosine floor and the owner re-check are unchanged from
// SemanticSuggester and are shared with it through rankTopicNeighbours, so this
// variant cannot drift into a different method while wearing the same
// thresholds.
type CachedSemanticSuggester struct {
	lexical   *Suggester
	topics    SemanticTopicLister
	embedder  Embedder
	cache     *TopicVectorCache
	policy    DescriptorPolicy
	limit     int
	topK      int
	minCosine float64
	// strict makes a question-embedding failure an error instead of a silent
	// fall back to lexical candidates. Production wants the fall back;
	// a measurement run must never report a score no configuration produced.
	strict bool
}

func NewCachedSemanticSuggester(lexical *Suggester, topics SemanticTopicLister, embedder Embedder,
	cache *TopicVectorCache, config SemanticSuggesterConfig, strict bool) (*CachedSemanticSuggester, error) {
	if lexical == nil || topics == nil || embedder == nil {
		return nil, errors.New("cached semantic suggester requires a lexical stage, a catalogue and an embedder")
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if cache == nil || !cache.Warmed() {
		return nil, errors.New("cached semantic suggester requires a warmed topic vector cache")
	}
	if cache.Key().Policy != config.Policy {
		return nil, fmt.Errorf("cache was warmed for policy %q but this run uses %q", cache.Key().Policy, config.Policy)
	}
	return &CachedSemanticSuggester{lexical: lexical, topics: topics, embedder: embedder, cache: cache,
		policy: config.Policy, limit: config.Limit, topK: config.TopK, minCosine: config.MinCosine,
		strict: strict}, nil
}

// Suggest runs the lexical stage, embeds the question once, and ranks it
// locally against the cached descriptor vectors.
func (s *CachedSemanticSuggester) Suggest(ctx context.Context, userID int64, question string) (SemanticResult, error) {
	if s == nil {
		return SemanticResult{}, errors.New("cached semantic suggester is not configured")
	}
	if userID <= 0 {
		return SemanticResult{}, errors.New("topic suggestion requires a trusted user id")
	}
	if strings.TrimSpace(question) == "" {
		return SemanticResult{}, errors.New("topic suggestion requires a question")
	}
	lexical, err := s.lexical.Suggest(ctx, userID, question)
	if err != nil {
		return SemanticResult{}, err
	}
	result := SemanticResult{Candidates: append([]Candidate{}, lexical...), LexicalHits: len(lexical)}
	if len(result.Candidates) >= s.limit {
		result.Candidates = result.Candidates[:s.limit]
		return result, nil
	}
	topics, err := s.topics.ListSemanticTopics(ctx, userID)
	if err != nil {
		return result, fmt.Errorf("list topics: %w", err)
	}
	owned := ownedTopics(topics)
	if len(owned) == 0 {
		return result, nil
	}

	// One request, one input: the question. The catalogue is already vectorised.
	result.EmbeddingCalls = 1
	vectors, err := s.embedder.Embed(ctx, []string{strings.TrimSpace(question)})
	if err != nil || len(vectors) != 1 {
		if err == nil {
			err = fmt.Errorf("embedder returned %d vectors for one question", len(vectors))
		}
		if s.strict {
			return result, fmt.Errorf("embed question: %w", err)
		}
		return result, nil
	}
	questionVector := vectors[0]

	scored := make([]scoredTopic, 0, len(owned))
	for _, key := range sortedOwnedKeys(owned) {
		topic := owned[key]
		vector, ok := s.cache.Vector(Descriptor(s.policy, topic))
		if !ok {
			// A descriptor the cache does not hold means the catalogue moved
			// under the run. Failing closed is the only honest option: ranking
			// against a partial catalogue would silently change the method.
			return result, fmt.Errorf("no cached vector for topic %q", key)
		}
		scored = append(scored, scoredTopic{topic: topic, score: cosine(questionVector, vector)})
	}
	rankTopicNeighbours(&result, scored, owned, s.topK, s.limit, s.minCosine)
	return result, nil
}

// scoredTopic is one candidate with its similarity.
type scoredTopic struct {
	topic SemanticTopic
	score float64
}

// rankTopicNeighbours is the shared selection rule.
//
// It exists so the cached and uncached variants cannot diverge on TopK, the
// floor, the owner re-check or the tie-break while both claiming the same
// frozen thresholds. An equivalence test pins them against each other.
func rankTopicNeighbours(result *SemanticResult, scored []scoredTopic, owned map[string]SemanticTopic,
	topK, limit int, minCosine float64) {
	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].score != scored[j].score {
			return scored[i].score > scored[j].score
		}
		return scored[i].topic.Key < scored[j].topic.Key
	})
	present := map[string]struct{}{}
	for _, candidate := range result.Candidates {
		present[candidate.TopicKey] = struct{}{}
	}
	considered := 0
	for _, item := range scored {
		if considered >= topK || len(result.Candidates) >= limit {
			break
		}
		considered++
		if _, ok := present[item.topic.Key]; ok {
			continue
		}
		if item.score < minCosine {
			result.RejectedBelowFloor++
			continue
		}
		if _, ok := owned[item.topic.Key]; !ok {
			result.OwnerRejected++
			continue
		}
		source := SourceLexicalMatch
		switch {
		case item.topic.Graded:
			source = SourceGradedTopic
		case item.topic.Notes > 0:
			source = SourceRecentNotes
		}
		name := strings.TrimSpace(item.topic.Name)
		if name == "" {
			name = item.topic.Key
		}
		result.Candidates = append(result.Candidates,
			Candidate{TopicKey: item.topic.Key, DisplayName: name, Source: source})
		present[item.topic.Key] = struct{}{}
	}
}

func ownedTopics(topics []SemanticTopic) map[string]SemanticTopic {
	owned := map[string]SemanticTopic{}
	for _, topic := range topics {
		key := model.NormalizeLearningTopicKey(topic.Key)
		if key == "" {
			continue
		}
		topic.Key = key
		owned[key] = topic
	}
	return owned
}

func sortedOwnedKeys(owned map[string]SemanticTopic) []string {
	out := make([]string, 0, len(owned))
	for key := range owned {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}
