package memory

import (
	"GopherAI/common/memoryrecall"
	memorydao "GopherAI/dao/memory"
	"GopherAI/model"
	"context"
	"errors"
	"testing"
)

type stubCatalogue struct {
	rows  []memorydao.TopicKeyRow
	err   error
	calls int
}

func (s *stubCatalogue) ListTopicKeys(context.Context, int64, int) ([]memorydao.TopicKeyRow, error) {
	s.calls++
	return s.rows, s.err
}

type countingEmbedder struct {
	calls int
	err   error
}

func (c *countingEmbedder) EmbedQuery(context.Context, string) ([]float32, error) {
	c.calls++
	if c.err != nil {
		return nil, c.err
	}
	return make([]float32, 4), nil
}

func hit(id string, distance float64) memorydao.MemorySearchResult {
	return memorydao.MemorySearchResult{MemoryID: id, Distance: distance}
}

func topicMemory(id, key string) model.LongTermMemory {
	return model.LongTermMemory{
		ID: id, UserID: 7, TopicKey: key, Scope: model.MemoryScopeTopic,
		Status: model.LongTermMemoryActive, Confidence: 0.9,
	}
}

// resolverUnder builds the semantic resolver with the widest gate, so a test
// that is about layering is not also about threshold values.
func resolverUnder(t *testing.T, catalogue TopicKeyCatalogue, index MemoryVectorSearcher, embedder QueryEmbedder, loader MemoryLoader) *TopicResolver {
	t.Helper()
	config := DefaultTopicResolverConfig()
	config.MinStandout, config.MinMargin = 1.30, 0.06
	resolver, err := NewSemanticTopicResolver(catalogue, index, embedder, loader, config)
	if err != nil {
		t.Fatal(err)
	}
	return resolver
}

// The layering exists to keep the provider off the request path whenever the
// server or the learner's own wording already answered the question.
func TestAuthoritativeAndLexicalKeysNeverReachTheProvider(t *testing.T) {
	catalogue := &stubCatalogue{rows: []memorydao.TopicKeyRow{
		{TopicKey: "tcp粘包", Topic: "粘包的个人理解", Notes: 2},
		{TopicKey: "go并发", Topic: "channel 的类比", Notes: 2},
	}}
	embedder := &countingEmbedder{}
	resolver := resolverUnder(t, catalogue, &stubIndex{}, embedder, stubLoader{})

	authoritative := resolver.Resolve(context.Background(), TopicResolveRequest{
		UserID: 7, Question: "接着讲", AuthoritativeKeys: []string{"tcp粘包"},
	})
	if len(authoritative.Keys) != 1 || authoritative.Keys[0] != "tcp粘包" || embedder.calls != 0 {
		t.Fatalf("authoritative=%+v embedding calls=%d", authoritative, embedder.calls)
	}

	lexical := resolver.Resolve(context.Background(), TopicResolveRequest{
		UserID: 7, Question: "channel 的类比我当时是怎么说的？",
	})
	if len(lexical.Keys) != 1 || lexical.Keys[0] != "go并发" || embedder.calls != 0 {
		t.Fatalf("lexical=%+v embedding calls=%d", lexical, embedder.calls)
	}
}

// The TopicKeys of episodes that already passed the MySQL reload are treated as
// authoritative, which is what makes a mixed question cost nothing extra.
func TestEpisodeTopicKeysShortCircuitTheSemanticLayer(t *testing.T) {
	catalogue := &stubCatalogue{rows: []memorydao.TopicKeyRow{{TopicKey: "向量检索", Topic: "维度理解"}}}
	embedder := &countingEmbedder{}
	resolver := resolverUnder(t, catalogue, &stubIndex{}, embedder, stubLoader{})
	cycle := memoryrecall.New("上次那个决定还算数吗？")
	cycle.SetEpisodeTopicKeys([]string{"向量检索"})

	resolution := resolver.Resolve(context.Background(), TopicResolveRequest{
		UserID: 7, Question: "上次那个决定还算数吗？", Cycle: cycle,
	})
	if len(resolution.Keys) != 1 || resolution.Keys[0] != "向量检索" {
		t.Fatalf("resolution=%+v", resolution)
	}
	if embedder.calls != 0 {
		t.Fatalf("authoritative episode keys must not buy an embedding: %d", embedder.calls)
	}
}

// An implicit alias reaches the right topic only through the semantic layer,
// and what it produces is a key — never the row the index ranked.
func TestSemanticLayerProposesAKeyAndNeverInjectsTheIndexedRow(t *testing.T) {
	catalogue := &stubCatalogue{rows: []memorydao.TopicKeyRow{
		{TopicKey: "tcp粘包", Topic: "粘包的个人理解"},
		{TopicKey: "熔断限流", Topic: "令牌桶的类比"},
	}}
	index := &stubIndex{
		semantic: []memorydao.MemorySearchResult{hit("m-water", 0.20), hit("m-bucket", 0.55)},
		lexical:  []memorydao.MemorySearchResult{hit("m-water", 0)},
	}
	loader := stubLoader{rows: []model.LongTermMemory{
		topicMemory("m-water", "tcp粘包"), topicMemory("m-bucket", "熔断限流"),
	}}
	resolver := resolverUnder(t, catalogue, index, &countingEmbedder{}, loader)

	resolution := resolver.Resolve(context.Background(), TopicResolveRequest{
		UserID: 7, Question: "消息边界这件事，我平时是拿什么来打比方的？",
	})
	if resolution.SemanticKey != "tcp粘包" || len(resolution.Keys) != 1 || resolution.Keys[0] != "tcp粘包" {
		t.Fatalf("resolution=%+v", resolution)
	}
	for _, key := range resolution.Keys {
		if key == "m-water" {
			t.Fatalf("a memory id must never be returned as a topic key: %+v", resolution)
		}
	}
}

// Two candidates that neither channel separates are exactly the case the old
// hybrid answered anyway. The gate abstains instead.
func TestAmbiguousTopCandidatesAbstain(t *testing.T) {
	catalogue := &stubCatalogue{rows: []memorydao.TopicKeyRow{
		{TopicKey: "tcp粘包", Topic: "粘包的个人理解"},
		{TopicKey: "packetbuffer", Topic: "缓冲区复用的困惑"},
	}}
	index := &stubIndex{
		semantic: []memorydao.MemorySearchResult{hit("m-a", 0.40), hit("m-b", 0.41)},
	}
	loader := stubLoader{rows: []model.LongTermMemory{
		topicMemory("m-a", "tcp粘包"), topicMemory("m-b", "packetbuffer"),
	}}
	resolver := resolverUnder(t, catalogue, index, &countingEmbedder{}, loader)
	resolution := resolver.Resolve(context.Background(), TopicResolveRequest{
		UserID: 7, Question: "那个我一直没搞懂的地方到底是怎么回事？",
	})
	if len(resolution.Keys) != 0 || resolution.SemanticKey != "" {
		t.Fatalf("resolution=%+v", resolution)
	}
	if !containsCode(resolution.ReasonCodes, "topic_ambiguous_abstain") {
		t.Fatalf("reason codes=%v", resolution.ReasonCodes)
	}
}

// Redis is a candidate source, MySQL is the authority. A row belonging to
// another account, or one the catalogue no longer lists, proposes nothing.
func TestIndexCandidatesAreRefusedWhenMySQLDisagrees(t *testing.T) {
	index := &stubIndex{semantic: []memorydao.MemorySearchResult{hit("m-foreign", 0.05), hit("m-retired", 0.06)}}

	t.Run("foreign owner", func(t *testing.T) {
		catalogue := &stubCatalogue{rows: []memorydao.TopicKeyRow{{TopicKey: "tcp粘包", Topic: "粘包的个人理解"}}}
		loader := stubLoader{rows: []model.LongTermMemory{{
			ID: "m-foreign", UserID: 8, TopicKey: "tcp粘包", Scope: model.MemoryScopeTopic,
			Status: model.LongTermMemoryActive, Confidence: 0.9,
		}}}
		resolver := resolverUnder(t, catalogue, index, &countingEmbedder{}, loader)
		resolution := resolver.Resolve(context.Background(), TopicResolveRequest{UserID: 7, Question: "消息边界那个比喻是什么？"})
		if len(resolution.Keys) != 0 {
			t.Fatalf("resolution=%+v", resolution)
		}
	})

	t.Run("key absent from the active catalogue", func(t *testing.T) {
		catalogue := &stubCatalogue{rows: []memorydao.TopicKeyRow{{TopicKey: "别的主题", Topic: "别的主题"}}}
		loader := stubLoader{rows: []model.LongTermMemory{topicMemory("m-retired", "已归档主题")}}
		// Both channels name it, so the gate accepts and the catalogue is the
		// only thing left that can refuse it.
		agreeing := &stubIndex{
			semantic: []memorydao.MemorySearchResult{hit("m-retired", 0.06)},
			lexical:  []memorydao.MemorySearchResult{hit("m-retired", 0)},
		}
		resolver := resolverUnder(t, catalogue, agreeing, &countingEmbedder{}, loader)
		resolution := resolver.Resolve(context.Background(), TopicResolveRequest{UserID: 7, Question: "消息边界那个比喻是什么？"})
		if len(resolution.Keys) != 0 || resolution.SemanticKey != "" {
			t.Fatalf("resolution=%+v", resolution)
		}
		if !containsCode(resolution.ReasonCodes, "topic_not_in_catalogue") {
			t.Fatalf("reason codes=%v", resolution.ReasonCodes)
		}
	})
}

// Losing the provider costs implicit topics, not the answer.
func TestResolverDegradesToTheLexicalLayer(t *testing.T) {
	catalogue := &stubCatalogue{rows: []memorydao.TopicKeyRow{{TopicKey: "tcp粘包", Topic: "粘包的个人理解"}}}
	resolver := resolverUnder(t, catalogue, &stubIndex{}, &countingEmbedder{err: errors.New("down")}, stubLoader{})
	resolution := resolver.Resolve(context.Background(), TopicResolveRequest{UserID: 7, Question: "消息边界那个比喻是什么？"})
	if len(resolution.Keys) != 0 {
		t.Fatalf("resolution=%+v", resolution)
	}
	if !containsCode(resolution.ReasonCodes, "embedding_unavailable") {
		t.Fatalf("reason codes=%v", resolution.ReasonCodes)
	}
}

// One request, one embedding, however many readers ask for the vector.
func TestSecondResolutionInOneRequestReusesTheFirst(t *testing.T) {
	catalogue := &stubCatalogue{rows: []memorydao.TopicKeyRow{{TopicKey: "tcp粘包", Topic: "粘包的个人理解"}}}
	index := &stubIndex{
		semantic: []memorydao.MemorySearchResult{hit("m-water", 0.10), hit("m-other", 0.60)},
		lexical:  []memorydao.MemorySearchResult{hit("m-water", 0)},
	}
	loader := stubLoader{rows: []model.LongTermMemory{topicMemory("m-water", "tcp粘包"), topicMemory("m-other", "熔断限流")}}
	embedder := &countingEmbedder{}
	resolver := resolverUnder(t, catalogue, index, embedder, loader)
	question := "消息边界这件事我拿什么打比方？"
	cycle := memoryrecall.New(question)

	first := resolver.Resolve(context.Background(), TopicResolveRequest{UserID: 7, Question: question, Cycle: cycle})
	second := resolver.Resolve(context.Background(), TopicResolveRequest{UserID: 7, Question: question, Cycle: cycle})
	if len(first.Keys) != 1 || len(second.Keys) != 1 || first.Keys[0] != second.Keys[0] {
		t.Fatalf("first=%+v second=%+v", first, second)
	}
	if embedder.calls != 1 || cycle.EmbeddingCalls() != 1 {
		t.Fatalf("embedder calls=%d cycle=%d", embedder.calls, cycle.EmbeddingCalls())
	}
	if !containsCode(second.ReasonCodes, "semantic_topic_reused") {
		t.Fatalf("second reason codes=%v", second.ReasonCodes)
	}
}

// The frozen gate has two routes and both are relative quantities. This pins
// which one fired, so the offline sweep and the service cannot drift apart.
func TestAcceptSemanticTopicHasTwoRelativeRoutes(t *testing.T) {
	agreeing := TopicResolution{Candidates: []TopicKeyCandidate{{TopicKey: "a"}}, Agreement: true, Standout: 1.0, Margin: 0}
	if ok, reason := AcceptSemanticTopic(agreeing, 1.15, 0.04); !ok || reason != "semantic_topic_agreement" {
		t.Fatalf("agreement route ok=%v reason=%s", ok, reason)
	}
	two := []TopicKeyCandidate{{TopicKey: "a"}, {TopicKey: "b"}}
	standout := TopicResolution{Candidates: two, Standout: 1.20, Margin: 0.05}
	if ok, reason := AcceptSemanticTopic(standout, 1.15, 0.04); !ok || reason != "semantic_topic_standout" {
		t.Fatalf("standout route ok=%v reason=%s", ok, reason)
	}
	narrow := TopicResolution{Candidates: two, Standout: 1.20, Margin: 0.01}
	if ok, reason := AcceptSemanticTopic(narrow, 1.15, 0.04); ok || reason != "topic_ambiguous_abstain" {
		t.Fatalf("narrow margin ok=%v reason=%s", ok, reason)
	}
	flat := TopicResolution{Candidates: two, Standout: 1.02, Margin: 0.09}
	if ok, _ := AcceptSemanticTopic(flat, 1.15, 0.04); ok {
		t.Fatalf("a candidate no better than its batch must not pass")
	}
	// A lone candidate has nothing to be ambiguous with, so the margin is not
	// applicable rather than zero-and-failing.
	alone := TopicResolution{Candidates: []TopicKeyCandidate{{TopicKey: "a"}}, Standout: 1.20}
	if ok, reason := AcceptSemanticTopic(alone, 1.15, 0.04); !ok || reason != "semantic_topic_standout" {
		t.Fatalf("single candidate ok=%v reason=%s", ok, reason)
	}
}

func containsCode(codes []string, want string) bool {
	for _, code := range codes {
		if code == want {
			return true
		}
	}
	return false
}

// STABILIZE-031 5.2. The learner spelled out a name; a candidate topic that
// does not contain it is not that thing however high it ranks. This is a
// different kind of evidence from standout and margin, which is the point:
// tightening those numbers could not separate these cases, because none-08
// cleared both of them.
func TestIdentifierNotInTopicAbstains(t *testing.T) {
	catalogue := &stubCatalogue{rows: []memorydao.TopicKeyRow{{TopicKey: "索引优化", Topic: "最左前缀的踩坑"}}}
	index := &stubIndex{
		semantic: []memorydao.MemorySearchResult{hit("m-index", 0.20)},
		lexical:  []memorydao.MemorySearchResult{hit("m-index", 0)},
	}
	loader := stubLoader{rows: []model.LongTermMemory{topicMemory("m-index", "索引优化")}}
	resolver := resolverUnder(t, catalogue, index, &countingEmbedder{}, loader)

	// Without an identifier the same candidate is accepted: the gate is about
	// the identifier, not about this topic.
	accepted := resolver.Resolve(context.Background(), TopicResolveRequest{
		UserID: 7, Question: "我在索引上遇到过什么问题？",
		Signals: &TopicSignals{TopicTerms: []string{"索引"}},
	})
	if len(accepted.Keys) != 1 || accepted.Keys[0] != "索引优化" {
		t.Fatalf("without an identifier the candidate must stand: %+v", accepted)
	}

	refused := resolver.Resolve(context.Background(), TopicResolveRequest{
		UserID: 7, Question: "我在 PostgreSQL 上遇到过什么索引问题？",
		Signals: &TopicSignals{TopicTerms: []string{"索引"}, Identifiers: []string{"PostgreSQL"}},
	})
	if len(refused.Keys) != 0 {
		t.Fatalf("a named technology absent from the topic must abstain: %+v", refused)
	}
	if !containsCode(refused.ReasonCodes, "identifier_not_in_topic_abstain") {
		t.Fatalf("reason codes=%v", refused.ReasonCodes)
	}
}

// An identifier the topic does carry must not be blocked.
func TestIdentifierPresentInTopicStillAccepts(t *testing.T) {
	catalogue := &stubCatalogue{rows: []memorydao.TopicKeyRow{{TopicKey: "packetbuffer", Topic: "缓冲区复用的困惑"}}}
	index := &stubIndex{
		semantic: []memorydao.MemorySearchResult{hit("m-buffer", 0.20)},
		lexical:  []memorydao.MemorySearchResult{hit("m-buffer", 0)},
	}
	loader := stubLoader{rows: []model.LongTermMemory{topicMemory("m-buffer", "packetbuffer")}}
	resolver := resolverUnder(t, catalogue, index, &countingEmbedder{}, loader)
	resolution := resolver.Resolve(context.Background(), TopicResolveRequest{
		UserID: 7, Question: "PacketBuffer 取包之后要不要复制？",
		Signals: &TopicSignals{TopicTerms: []string{"取包"}, Identifiers: []string{"PacketBuffer"}},
	})
	if len(resolution.Keys) != 1 || resolution.Keys[0] != "packetbuffer" {
		t.Fatalf("resolution=%+v", resolution)
	}
}

// STABILIZE-031 5.3. A question that is all shell names no subject, so the
// semantic layer must not run — it would spend a call and might propose a topic
// the learner never mentioned.
func TestNoTopicAfterShellStripAbstainsBeforeTheProvider(t *testing.T) {
	catalogue := &stubCatalogue{rows: []memorydao.TopicKeyRow{{TopicKey: "熔断限流", Topic: "令牌桶的类比"}}}
	embedder := &countingEmbedder{}
	index := &stubIndex{semantic: []memorydao.MemorySearchResult{hit("m-bucket", 0.10)}}
	loader := stubLoader{rows: []model.LongTermMemory{topicMemory("m-bucket", "熔断限流")}}
	resolver := resolverUnder(t, catalogue, index, embedder, loader)

	resolution := resolver.Resolve(context.Background(), TopicResolveRequest{
		UserID: 7, Question: "上次聊了什么？", Signals: &TopicSignals{},
	})
	if len(resolution.Keys) != 0 {
		t.Fatalf("a shell-only question must resolve nothing: %+v", resolution)
	}
	if embedder.calls != 0 {
		t.Fatalf("a shell-only question must not buy an embedding: %d", embedder.calls)
	}
	if !containsCode(resolution.ReasonCodes, "no_topic_after_shell_strip") {
		t.Fatalf("reason codes=%v", resolution.ReasonCodes)
	}
}

// A caller that did not compute the signals must not have the two new rules
// applied to an empty guess.
func TestAbsentSignalsSkipTheNewRules(t *testing.T) {
	catalogue := &stubCatalogue{rows: []memorydao.TopicKeyRow{{TopicKey: "索引优化", Topic: "最左前缀的踩坑"}}}
	index := &stubIndex{
		semantic: []memorydao.MemorySearchResult{hit("m-index", 0.20)},
		lexical:  []memorydao.MemorySearchResult{hit("m-index", 0)},
	}
	loader := stubLoader{rows: []model.LongTermMemory{topicMemory("m-index", "索引优化")}}
	resolver := resolverUnder(t, catalogue, index, &countingEmbedder{}, loader)
	resolution := resolver.Resolve(context.Background(), TopicResolveRequest{
		UserID: 7, Question: "我在 PostgreSQL 上遇到过什么索引问题？",
	})
	if len(resolution.Keys) != 1 {
		t.Fatalf("without signals the pre-031 behaviour must stand: %+v", resolution)
	}
}

// The lexical constructor must not be able to reach a provider at all: the
// design asks for "not constructed", not "constructed and skipped".
func TestLexicalResolverHasNoSemanticLayer(t *testing.T) {
	catalogue := &stubCatalogue{rows: []memorydao.TopicKeyRow{{TopicKey: "tcp粘包", Topic: "粘包的个人理解"}}}
	resolver, err := NewLexicalTopicResolver(catalogue, DefaultTopicResolverConfig())
	if err != nil {
		t.Fatal(err)
	}
	lexical := resolver.Resolve(context.Background(), TopicResolveRequest{
		UserID: 7, Question: "粘包的个人理解是什么？", Signals: &TopicSignals{TopicTerms: []string{"粘包"}},
	})
	if len(lexical.Keys) != 1 || lexical.Keys[0] != "tcp粘包" {
		t.Fatalf("the lexical layer must still work: %+v", lexical)
	}
	implicit := resolver.Resolve(context.Background(), TopicResolveRequest{
		UserID: 7, Question: "消息边界这件事我拿什么打比方？", Signals: &TopicSignals{TopicTerms: []string{"消息边界"}},
	})
	if len(implicit.Keys) != 0 {
		t.Fatalf("a lexical-only resolver must not propose implicit topics: %+v", implicit)
	}
	if !containsCode(implicit.ReasonCodes, "semantic_topic_disabled") {
		t.Fatalf("reason codes=%v", implicit.ReasonCodes)
	}
}

// The semantic constructor demands every dependency and both gate numbers.
func TestSemanticResolverRefusesToBuildWithoutItsGate(t *testing.T) {
	catalogue := &stubCatalogue{}
	index, embedder, loader := &stubIndex{}, &countingEmbedder{}, stubLoader{}
	if _, err := NewSemanticTopicResolver(catalogue, index, embedder, loader, DefaultTopicResolverConfig()); err == nil {
		t.Fatal("the semantic resolver must refuse an unset gate")
	}
	if _, err := NewSemanticTopicResolver(catalogue, nil, embedder, loader, DefaultTopicResolverConfig()); err == nil {
		t.Fatal("the semantic resolver must refuse a missing index")
	}
	config := DefaultTopicResolverConfig()
	config.MinStandout, config.MinMargin = 1.30, 0.06
	if _, err := NewSemanticTopicResolver(catalogue, index, embedder, loader, config); err != nil {
		t.Fatalf("an explicit gate must build: %v", err)
	}
}

// The failed development gate must not survive as a default anywhere.
func TestDefaultResolverConfigCarriesNoGate(t *testing.T) {
	config := DefaultTopicResolverConfig()
	if config.MinStandout != 0 || config.MinMargin != 0 {
		t.Fatalf("the default config must carry no semantic gate: %+v", config)
	}
}
