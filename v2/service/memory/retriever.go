package memory

import (
	"GopherAI/common/memoryrecall"
	"GopherAI/common/topicmatch"
	learningdao "GopherAI/dao/learning"
	memorydao "GopherAI/dao/memory"
	"GopherAI/model"
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

type RecallRepository interface {
	ListAllActive(context.Context, int64, model.MemoryScope, float64, int) ([]model.LongTermMemory, error)
	ListByTopicKey(context.Context, int64, model.MemoryScope, string, float64, int) ([]model.LongTermMemory, error)
	ListTopicKeys(context.Context, int64, int) ([]memorydao.TopicKeyRow, error)
}

type ActiveLearningReader interface {
	GetActive(context.Context, int64, string) (*model.LearningSession, error)
}

type UsageRecorder interface {
	Record(int64, []string, time.Time) bool
}

type RetrieverConfig struct {
	// MaxResults bounds the retrieved `topic_note` results only.
	MaxResults     int
	CandidateLimit int
	MinConfidence  float64
	// GlobalLimit caps the always-injected `profile` set. The type promises
	// "inject everything", which needs a ceiling so that a long-lived account
	// cannot quietly consume the whole memory budget.
	GlobalLimit int
}

func DefaultRetrieverConfig() RetrieverConfig {
	return RetrieverConfig{MaxResults: 5, CandidateLimit: 50, MinConfidence: 0.6, GlobalLimit: 30}
}

func (c RetrieverConfig) Validate() error {
	if c.MaxResults < 1 || c.MaxResults > 5 || c.CandidateLimit < c.MaxResults || c.CandidateLimit > 100 || c.MinConfidence < 0 || c.MinConfidence > 1 {
		return fmt.Errorf("invalid memory retriever config")
	}
	if c.GlobalLimit < 1 || c.GlobalLimit > 100 {
		return fmt.Errorf("memory global limit must be between 1 and 100")
	}
	return nil
}

type RecallRequest struct {
	UserID          int64
	SessionID       string
	Question        string
	StrongTopicKeys []string
	// Cycle is the request-scoped coordination point shared with Episode
	// recall. It carries the one query vector this request is allowed to buy
	// and the TopicKeys of the episodes that already passed the MySQL reload.
	Cycle *memoryrecall.Cycle
}

type Retriever struct {
	repository RecallRepository
	learning   ActiveLearningReader
	usage      UsageRecorder
	resolver   *TopicResolver
	config     RetrieverConfig
	now        func() time.Time
}

// UseTopicResolver replaces the built-in name matching with the layered
// resolver. It is optional: without it the retriever keeps the pre-v3 lexical
// behaviour, which is what the focused tests and the offline comparison
// commands are built against.
func (r *Retriever) UseTopicResolver(resolver *TopicResolver) { r.resolver = resolver }

// TopicKeysFor exposes the resolution the retriever would use, so the context
// builder can hand the same keys to Episode recall before it runs. Resolving
// once and sharing the answer is what keeps a mixed question at one embedding
// call instead of two.
func (r *Retriever) TopicKeysFor(ctx context.Context, request RecallRequest) TopicResolution {
	if r == nil || r.resolver == nil {
		return TopicResolution{}
	}
	return r.resolver.Resolve(ctx, TopicResolveRequest{
		UserID: request.UserID, Question: request.Question,
		AuthoritativeKeys: request.StrongTopicKeys, Cycle: request.Cycle,
	})
}

func NewRetriever(repository RecallRepository, learning ActiveLearningReader, usage UsageRecorder, config RetrieverConfig) (*Retriever, error) {
	if repository == nil {
		return nil, fmt.Errorf("memory recall repository is required")
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &Retriever{repository: repository, learning: learning, usage: usage, config: config, now: time.Now}, nil
}

// RecallResult keeps the two memory types apart all the way to the caller.
//
// They were concatenated into one slice until AGENT-019 measured the cost:
// with the profile block emitted first and no overall cap, thirty globals
// arrived ahead of every retrieved note, so the token budget was spent before
// a single query-relevant memory was reached. Ordering alone cannot fix that,
// because whichever type goes first starves the other. Separate lists let the
// context builder give each its own budget.
type RecallResult struct {
	Global []string
	Topic  []string
}

func (r *Retriever) RelevantMemories(ctx context.Context, request RecallRequest) (RecallResult, error) {
	if request.UserID <= 0 || strings.TrimSpace(request.SessionID) == "" || strings.TrimSpace(request.Question) == "" {
		return RecallResult{}, fmt.Errorf("memory recall identity and question are required")
	}
	strongKeys := append([]string(nil), request.StrongTopicKeys...)
	if r.learning != nil {
		active, err := r.learning.GetActive(ctx, request.UserID, request.SessionID)
		if err != nil && !errors.Is(err, learningdao.ErrLearningSessionNotFound) {
			return RecallResult{}, fmt.Errorf("load memory learning context: %w", err)
		}
		if err == nil && active != nil {
			strongKeys = append(strongKeys, active.TopicKey)
		}
	}

	globalLines, globalIDs, err := r.GlobalMemories(ctx, request.UserID)
	if err != nil {
		return RecallResult{}, err
	}
	result := RecallResult{Global: globalLines}
	ids := make([]string, 0, r.config.GlobalLimit+r.config.MaxResults)
	ids = append(ids, globalIDs...)

	if r.resolver != nil {
		// The resolver owns every layer: the authoritative keys passed in, the
		// learner's own wording, and — only when neither covered the question —
		// one bounded semantic proposal. What it returns is still just keys.
		resolution := r.resolver.Resolve(ctx, TopicResolveRequest{
			UserID: request.UserID, Question: request.Question,
			AuthoritativeKeys: strongKeys, Cycle: request.Cycle,
		})
		strongKeys = normalizeStrongTopicKeys(resolution.Keys)
	} else {
		prepared := topicmatch.NewQuestion(request.Question)
		rows, err := r.repository.ListTopicKeys(ctx, request.UserID, 200)
		if err != nil {
			return RecallResult{}, fmt.Errorf("list user memory topics: %w", err)
		}
		for _, row := range rows {
			if matchesTopic(prepared, row.TopicKey, row.Topic) {
				strongKeys = append(strongKeys, row.TopicKey)
			}
		}
		strongKeys = normalizeStrongTopicKeys(strongKeys)
	}
	if len(strongKeys) > 0 {
		seen := make(map[string]struct{})
		for _, topicKey := range strongKeys {
			memories, err := r.repository.ListByTopicKey(ctx, request.UserID, model.MemoryScopeTopic, topicKey, r.config.MinConfidence, r.config.CandidateLimit)
			if err != nil {
				return RecallResult{}, fmt.Errorf("load topic memories: %w", err)
			}
			sort.SliceStable(memories, func(i, j int) bool {
				if memories[i].UserConfirmed != memories[j].UserConfirmed {
					return memories[i].UserConfirmed
				}
				if memories[i].Durability != memories[j].Durability {
					return memories[i].Durability == model.MemoryDurabilityStable
				}
				if memories[i].Importance != memories[j].Importance {
					return memories[i].Importance > memories[j].Importance
				}
				if !memories[i].UpdatedAt.Equal(memories[j].UpdatedAt) {
					return memories[i].UpdatedAt.After(memories[j].UpdatedAt)
				}
				return memories[i].ID < memories[j].ID
			})
			for _, memory := range memories {
				if len(result.Topic) == r.config.MaxResults {
					break
				}
				if _, duplicate := seen[memory.ID]; duplicate || !eligibleMemory(memory, request.UserID, model.MemoryScopeTopic, r.config.MinConfidence, r.now()) || memory.TopicKey != topicKey {
					continue
				}
				seen[memory.ID] = struct{}{}
				result.Topic = append(result.Topic, renderMemory(memory))
				ids = append(ids, memory.ID)
			}
			if len(result.Topic) == r.config.MaxResults {
				break
			}
		}
	}
	if len(ids) == 0 {
		return RecallResult{}, nil
	}
	if r.usage != nil {
		_ = r.usage.Record(request.UserID, ids, r.now())
	}
	return result, nil
}

// GlobalMemories is the always-injected half of long-term semantic memory.
//
// It is exported and it is the only implementation, called by RelevantMemories
// above as well as by the STABILIZE-032 planned read path. Global memory is
// explicitly out of that task's scope: its relevance does not correlate with the
// question, it already has its own 800-token ceiling, and letting a model decide
// whether to drop a stated teaching preference or an explicit prohibition is a
// different task with a different risk. Sharing one function is what makes
// "unchanged" a fact about the code rather than a claim about two copies.
func (r *Retriever) GlobalMemories(ctx context.Context, userID int64) ([]string, []string, error) {
	if r == nil || r.repository == nil {
		return nil, nil, fmt.Errorf("memory recall repository is required")
	}
	if userID <= 0 {
		return nil, nil, fmt.Errorf("memory recall identity is required")
	}
	globals, err := r.repository.ListAllActive(ctx, userID, model.MemoryScopeGlobal, r.config.MinConfidence, r.config.GlobalLimit)
	if err != nil {
		return nil, nil, fmt.Errorf("load global memories: %w", err)
	}
	lines := make([]string, 0, len(globals))
	ids := make([]string, 0, len(globals))
	for _, memory := range globals {
		if !eligibleMemory(memory, userID, model.MemoryScopeGlobal, r.config.MinConfidence, r.now()) {
			continue
		}
		lines = append(lines, renderMemory(memory))
		ids = append(ids, memory.ID)
	}
	return lines, ids, nil
}

func eligibleMemory(memory model.LongTermMemory, userID int64, scope model.MemoryScope, minConfidence float64, now time.Time) bool {
	return memory.UserID == userID && memory.Status == model.LongTermMemoryActive && memory.Scope == scope &&
		memory.Confidence >= minConfidence && !memory.ExpiredAt(now)
}

func normalizeStrongTopicKeys(keys []string) []string {
	seen := make(map[string]struct{}, len(keys))
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		key = model.NormalizeLearningTopicKey(key)
		if key == "" {
			continue
		}
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, key)
	}
	return result
}

// MatchesTopicByName reports whether a question names one of the learner's
// memory topics. It is exported so the same fixtures that score the mastery
// matcher can score this one; the two feed a single pool of topic keys, and
// comparing them by reading them is not comparing them.
func MatchesTopicByName(question, topicKey, topic string) bool {
	return matchesTopic(topicmatch.NewQuestion(question), topicKey, topic)
}

// matchesTopic asks whether the question reproduces enough of the topic's name.
//
// It used to require the whole name as a contiguous substring, which scored
// 0.294 against 0.941 for the coverage matcher on the same forty-six labelled
// cases — and caught nothing the coverage matcher missed, so its strictness
// bought no precision, only twenty-four extra misses. "TCP 粘包" failed to match
// the key `tcp粘包` over one space.
//
// What is worth keeping is not this rule but the candidates it runs over: the
// mastery matcher only ever sees topics the learner has been quizzed on, so a
// topic that holds memories and no quiz history is reachable from here alone.
// The pool stays; the rule is now the one that was measured.
func matchesTopic(question topicmatch.Question, topicKey, topic string) bool {
	if question.Empty() {
		return false
	}
	// `topic` is MIN(subject) over that topic's memories rather than a curated
	// name, so it is tried as well as the key: whichever of the two the learner
	// echoed is enough.
	for _, candidate := range []string{topicKey, topic} {
		if strings.TrimSpace(candidate) == "" {
			continue
		}
		if question.Names(candidate) {
			return true
		}
	}
	return false
}

type rankedMemory struct {
	memory       model.LongTermMemory
	tagHits      int
	topicMatch   bool
	typePriority int
}

func rankMemories(candidates []model.LongTermMemory, userID int64, tags []string, topicKey string, learningMode bool, minConfidence float64) []rankedMemory {
	queryTags := make(map[string]struct{}, len(tags))
	for _, tag := range tags {
		queryTags[tag] = struct{}{}
	}
	ranked := make([]rankedMemory, 0, len(candidates))
	for _, memory := range candidates {
		if memory.UserID != userID || memory.Status != model.LongTermMemoryActive || memory.Confidence < minConfidence || !allowedForMode(memory.Scope, learningMode) {
			continue
		}
		hits := 0
		for _, tag := range NormalizeMemoryTags(memory.Tags) {
			if _, exists := queryTags[tag]; exists {
				hits++
			}
		}
		topicMatch := strings.TrimSpace(topicKey) != "" && memory.TopicKey == topicKey
		if hits == 0 && !topicMatch {
			continue
		}
		ranked = append(ranked, rankedMemory{memory: memory, tagHits: hits, topicMatch: topicMatch, typePriority: memoryTypePriority(memory.Scope, learningMode)})
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		left, right := ranked[i], ranked[j]
		if left.topicMatch != right.topicMatch {
			return left.topicMatch
		}
		if left.tagHits != right.tagHits {
			return left.tagHits > right.tagHits
		}
		if left.typePriority != right.typePriority {
			return left.typePriority > right.typePriority
		}
		if left.memory.UserConfirmed != right.memory.UserConfirmed {
			return left.memory.UserConfirmed
		}
		if left.memory.Confidence != right.memory.Confidence {
			return left.memory.Confidence > right.memory.Confidence
		}
		if !sameTime(left.memory.LastUsedAt, right.memory.LastUsedAt) {
			return later(left.memory.LastUsedAt, right.memory.LastUsedAt)
		}
		if left.memory.Importance != right.memory.Importance {
			return left.memory.Importance > right.memory.Importance
		}
		return left.memory.ID < right.memory.ID
	})
	return ranked
}

// AGENT-018 left two types, and the old per-mode type sets no longer carry any
// meaning: both types are allowed in both modes. What differs is how they are
// selected, which is handled above rather than by filtering here.
//
// Retired types are absent from this set, which is what keeps pre-migration
// rows out of every answer without deleting them.
func recallMemoryTypes() []model.MemoryScope {
	return []model.MemoryScope{model.MemoryScopeGlobal, model.MemoryScopeTopic}
}

func allowedForMode(memoryType model.MemoryScope, _ bool) bool {
	return memoryType.Valid()
}

// A profile fact applies to every question, so it outranks a topic note that
// merely shares a tag with the current one.
func memoryTypePriority(memoryType model.MemoryScope, _ bool) int {
	switch memoryType {
	case model.MemoryScopeGlobal:
		return 2
	case model.MemoryScopeTopic:
		return 1
	}
	return 0
}

func renderMemory(memory model.LongTermMemory) string {
	return fmt.Sprintf("[%s; tags=%s] %s: %s", memory.Scope, strings.Join(memory.Tags, ","), strings.TrimSpace(memory.Subject), strings.TrimSpace(memory.Content))
}

// RenderMemoryLine exposes renderMemory so the M1/M2 paired harness can map
// an injected line back to its memory id without writing a second copy of
// the format. It adds no behavior: recall paths keep calling renderMemory.
func RenderMemoryLine(memory model.LongTermMemory) string { return renderMemory(memory) }

func hasTag(tags []string, target string) bool {
	for _, tag := range tags {
		if tag == target {
			return true
		}
	}
	return false
}

func sameTime(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Equal(*right)
}

func later(left, right *time.Time) bool {
	if left == nil {
		return false
	}
	if right == nil {
		return true
	}
	return left.After(*right)
}
