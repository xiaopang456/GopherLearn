package topicconsent

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"

	"GopherAI/model"
)

// DescriptorPolicy decides what text of a topic is sent to the embedding
// provider.
//
// It is an explicit, frozen choice rather than an implementation detail,
// because it is the whole privacy surface of suggestion_v1.1: whatever this
// returns leaves the deployment. Neither policy may ever carry a mastery level,
// a weak point, a personal analogy or the text of a long-term memory.
type DescriptorPolicy string

const (
	// DescriptorKeyAndName sends only the topic key and its display name.
	//
	// It is the minimal policy: the key and name are already what the user is
	// shown in the confirmation strip, so this sends nothing the learner is not
	// looking at. It is also the weaker signal, because a topic name rarely
	// shares wording with the question that is about it - which is precisely
	// why the lexical method failed.
	DescriptorKeyAndName DescriptorPolicy = "key_and_name"
	// DescriptorWithLearningExamples adds the text of quiz questions the
	// learner has been asked on that topic.
	//
	// It is the descriptor STABILIZE-049 reached 40/40 with, and the extra
	// vocabulary is what let a question about 固定头 and 长度字段 match the topic
	// 粘包处理 at all. It is more revealing than the minimal policy: the question
	// text is system-authored rather than the learner's own writing, and it
	// carries no answer, no score and no grader remark, but it does show which
	// subjects this learner has been examined on.
	DescriptorWithLearningExamples DescriptorPolicy = "key_name_and_learning_examples"
)

func (p DescriptorPolicy) Valid() bool {
	return p == DescriptorKeyAndName || p == DescriptorWithLearningExamples
}

// SemanticTopic is one catalogue row plus the non-sensitive extra text a
// descriptor policy may use.
type SemanticTopic struct {
	Topic
	// LearningExamples are quiz question texts. They are carried separately
	// from Topic so that the minimal policy is structurally unable to include
	// them: it never reads this field.
	LearningExamples []string
}

// SemanticTopicLister is the catalogue read for the semantic stage.
type SemanticTopicLister interface {
	ListSemanticTopics(ctx context.Context, userID int64) ([]SemanticTopic, error)
}

type SemanticTopicListerFunc func(context.Context, int64) ([]SemanticTopic, error)

func (f SemanticTopicListerFunc) ListSemanticTopics(ctx context.Context, userID int64) ([]SemanticTopic, error) {
	return f(ctx, userID)
}

// Embedder turns text into vectors. It is an interface so the evaluation can
// count calls and so no provider client is constructed under a mode that does
// not use one.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float64, error)
}

// SemanticSuggester is suggestion_v1.1: lexical first, embedding for recall
// only, and the user still the only thing that authorises a read.
//
// What it deliberately does not do is decide. No model ranks the candidates
// into a Primary, because that is the decision four generations of selector
// failed at and the reason this whole line of work moved the choice to the
// learner. The embedding is here to make sure the right option is on the list,
// not to pick it.
type SemanticSuggester struct {
	lexical   *Suggester
	topics    SemanticTopicLister
	embedder  Embedder
	policy    DescriptorPolicy
	limit     int
	topK      int
	minCosine float64
	batchSize int
}

// SemanticSuggesterConfig is frozen before the first embedding call.
type SemanticSuggesterConfig struct {
	Policy DescriptorPolicy
	// Limit is the ceiling shown to the user.
	Limit int
	// TopK is how many semantic neighbours are considered before the floor and
	// the limit are applied.
	TopK int
	// MaxBatchInputs is the largest number of texts one embedding request may
	// carry.
	//
	// It exists because a provider limit is not a suggestion: qwen3.7-text-embedding
	// rejects a batch over twenty with a 400, and a suggester that sends the
	// question plus every descriptor in one request therefore fails outright for
	// any learner with twenty or more topics - which is the normal case, not an
	// edge one.
	MaxBatchInputs int
	// MinCosine is the absolute floor a semantic neighbour must clear.
	//
	// An absolute threshold is a known hazard - STABILIZE-030 recorded that it
	// drifts with pool size - and it is used here for one thing a relative rule
	// cannot do: decide that a question is about none of this learner's topics.
	// Without it a pure Top-K would hand back five unrelated subjects for a
	// question about the weather, which is exactly the "nearest available topic"
	// behaviour the confirmed mode exists to remove.
	MinCosine float64
}

// DefaultSemanticSuggesterConfig is the a-priori configuration. The floor was
// set before any per-case score was inspected and is frozen: it is not swept,
// and it is not retuned against the cases it is measured on.
func DefaultSemanticSuggesterConfig() SemanticSuggesterConfig {
	return SemanticSuggesterConfig{Policy: DescriptorKeyAndName, Limit: MaxCandidates, TopK: 5,
		MinCosine: 0.35, MaxBatchInputs: 20}
}

func (c SemanticSuggesterConfig) Validate() error {
	if !c.Policy.Valid() {
		return fmt.Errorf("descriptor policy %q must be key_and_name or key_name_and_learning_examples", c.Policy)
	}
	if c.Limit < 1 || c.Limit > MaxCandidates {
		return fmt.Errorf("limit must be between 1 and %d", MaxCandidates)
	}
	if c.TopK < 1 || c.TopK > 20 {
		return fmt.Errorf("topK must be between 1 and 20")
	}
	if c.MinCosine <= 0 || c.MinCosine >= 1 {
		return fmt.Errorf("minCosine must be between 0 and 1")
	}
	if c.MaxBatchInputs < 1 || c.MaxBatchInputs > 100 {
		return fmt.Errorf("maxBatchInputs must be between 1 and 100")
	}
	return nil
}

func NewSemanticSuggester(lexical *Suggester, topics SemanticTopicLister, embedder Embedder,
	config SemanticSuggesterConfig) (*SemanticSuggester, error) {
	if lexical == nil || topics == nil || embedder == nil {
		return nil, fmt.Errorf("semantic suggester requires a lexical stage, a catalogue and an embedder")
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &SemanticSuggester{lexical: lexical, topics: topics, embedder: embedder,
		policy: config.Policy, limit: config.Limit, topK: config.TopK, minCosine: config.MinCosine,
		batchSize: config.MaxBatchInputs}, nil
}

// Descriptor renders the text that will be sent to the embedding provider.
//
// It is exported because the authorisation request has to show exactly what
// leaves the deployment, and a description of the text is not the text.
func Descriptor(policy DescriptorPolicy, topic SemanticTopic) string {
	parts := []string{"topic_key: " + topic.Key, "topic_name: " + topic.Name}
	if policy == DescriptorWithLearningExamples && len(topic.LearningExamples) > 0 {
		parts = append(parts, "learning_examples: "+strings.Join(topic.LearningExamples, " | "))
	}
	return strings.Join(parts, "\n")
}

// SemanticResult carries the candidates plus the accounting the evaluation and
// the ledger need.
type SemanticResult struct {
	Candidates []Candidate
	// LexicalHits is how many candidates the zero-call stage already found, so
	// the value added by the embedding stage is attributable.
	LexicalHits int
	// EmbeddingCalls is the number of provider requests this suggestion made.
	EmbeddingCalls int
	// RejectedBelowFloor counts neighbours dropped by MinCosine, which is how
	// "this question is about none of your topics" is distinguished from "the
	// index returned nothing".
	RejectedBelowFloor int
	// OwnerRejected counts candidates dropped because the key was not in this
	// user's catalogue on re-check.
	OwnerRejected int
}

// Suggest runs the lexical stage, then fills remaining slots semantically.
func (s *SemanticSuggester) Suggest(ctx context.Context, userID int64, question string) (SemanticResult, error) {
	if s == nil {
		return SemanticResult{}, fmt.Errorf("semantic suggester is not configured")
	}
	if userID <= 0 {
		return SemanticResult{}, fmt.Errorf("topic suggestion requires a trusted user id")
	}
	if strings.TrimSpace(question) == "" {
		return SemanticResult{}, fmt.Errorf("topic suggestion requires a question")
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
		return SemanticResult{}, fmt.Errorf("list topics: %w", err)
	}
	// The owner set is rebuilt from this user's catalogue and every semantic
	// neighbour is checked against it. The catalogue read is already scoped by
	// user id; re-checking here is defence in depth, in the place where the
	// decision is actually made.
	owned := map[string]SemanticTopic{}
	for _, topic := range topics {
		key := model.NormalizeLearningTopicKey(topic.Key)
		if key == "" {
			continue
		}
		topic.Key = key
		owned[key] = topic
	}
	if len(owned) == 0 {
		return result, nil
	}

	ordered := make([]SemanticTopic, 0, len(owned))
	for _, key := range sortedKeys(owned) {
		ordered = append(ordered, owned[key])
	}
	texts := make([]string, 0, len(ordered)+1)
	texts = append(texts, strings.TrimSpace(question))
	for _, topic := range ordered {
		texts = append(texts, Descriptor(s.policy, topic))
	}
	vectors, calls, err := s.embedBatched(ctx, texts)
	result.EmbeddingCalls = calls
	if err != nil {
		// A failed embedding costs recall, never the turn: the lexical
		// candidates stand and the user simply sees a shorter list.
		return result, nil
	}
	if len(vectors) != len(texts) {
		return result, fmt.Errorf("embedder returned %d vectors for %d texts", len(vectors), len(texts))
	}

	type scored struct {
		topic SemanticTopic
		score float64
	}
	ranked := make([]scored, 0, len(ordered))
	for index, topic := range ordered {
		ranked = append(ranked, scored{topic: topic, score: cosine(vectors[0], vectors[index+1])})
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		return ranked[i].topic.Key < ranked[j].topic.Key
	})

	present := map[string]struct{}{}
	for _, candidate := range result.Candidates {
		present[candidate.TopicKey] = struct{}{}
	}
	considered := 0
	for _, item := range ranked {
		if considered >= s.topK || len(result.Candidates) >= s.limit {
			break
		}
		considered++
		if _, ok := present[item.topic.Key]; ok {
			continue
		}
		if item.score < s.minCosine {
			// Below the floor the honest answer is "none of your topics", not
			// the closest one available.
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
	return result, nil
}

// embedBatched splits the request so no single call exceeds the provider's
// batch limit, and reports how many calls it actually made so a budget can be
// reconciled against reality rather than against an assumption.
func (s *SemanticSuggester) embedBatched(ctx context.Context, texts []string) ([][]float64, int, error) {
	size := s.batchSize
	if size < 1 {
		size = 20
	}
	out := make([][]float64, 0, len(texts))
	calls := 0
	for start := 0; start < len(texts); start += size {
		end := start + size
		if end > len(texts) {
			end = len(texts)
		}
		calls++
		vectors, err := s.embedder.Embed(ctx, texts[start:end])
		if err != nil {
			return nil, calls, err
		}
		if len(vectors) != end-start {
			return nil, calls, fmt.Errorf("embedder returned %d vectors for %d texts", len(vectors), end-start)
		}
		out = append(out, vectors...)
	}
	return out, calls, nil
}

func cosine(a, b []float64) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += a[i] * b[i]
		normA += a[i] * a[i]
		normB += b[i] * b[i]
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}

func sortedKeys(items map[string]SemanticTopic) []string {
	out := make([]string, 0, len(items))
	for key := range items {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}
